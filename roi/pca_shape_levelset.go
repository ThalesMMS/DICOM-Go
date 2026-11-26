package roi

import (
	"context"
	"fmt"
	"math"
)

// PCAShapePriorOptions defines a fixed research target and its reaction weight
// (inverse pseudotime). Pose/coefficients are caller-selected, never optimized
// from a patient image. Geometry must match both the volume and canonical grid.
type PCAShapePriorOptions struct {
	Model          *PCAShapeModel
	Geometry       PCAShapeGeometry
	CoefficientsSD []float64
	Pose           PCAShapePose
	Outside        PCAShapeOutsidePolicy
	Weight         float64
	// Thresholds only disclose model/initial-field mismatch; no confidence is
	// inferred. Defaults: 3 standard deviations and 1 mm clipped-field RMS.
	MaxCoefficientSD, MaxResidualMM float64
	Limits                          PCAShapeLimits
}

type PCAShapePriorDiagnostics struct {
	ModelID, Provenance                      string
	Weight                                   float64
	CoefficientsSD                           []float64
	Pose                                     PCAShapePose
	InitialFieldRMSMM, FinalFieldRMSMM       float64
	InitialMeanPotential, FinalMeanPotential float64
	EvaluatedVoxels, ExtrapolatedVoxels      int
	OutsideDistribution                      bool
	// This bounded dense-band profile uses a fixed, clipped approximate SDF
	// target. Diagnostics are numerical mismatch, never anatomical confidence.
	Limitation string
}
type PCAShapePriorResult struct {
	Segmentation *LevelSetResult
	Prior        PCAShapePriorDiagnostics
}

type pcaShapeEvolution struct {
	options     PCAShapePriorOptions
	target      []float32
	diagnostics PCAShapePriorDiagnostics
}

// ShapePriorDetectionLevelSet adds -weight*(phi-target) in the dense band to
// ShapeDetectionLevelSet. The per-voxel potential is weight/2*(phi-target)^2.
// Target and phi share the base band's +/-3*max-spacing clipping convention.
// There is no automatic training, pose fitting or remote/anatomical model.
// Weight=0 delegates directly to the unchanged base solver and its limits.
func ShapePriorDetectionLevelSet(ctx context.Context, v LevelSetVolume, init LevelSetInitialization, p ShapeDetectionParams, o LevelSetOptions, prior PCAShapePriorOptions) (*PCAShapePriorResult, error) {
	ctx = pcaContext(ctx)
	if e := ctx.Err(); e != nil {
		return nil, fmt.Errorf("%w: %w", ErrLevelSetCancelled, e)
	}
	if !finiteNonNegative(prior.Weight) || prior.Weight > 1e4 {
		return nil, ErrPCAShapeInvalid
	}
	if !finiteNonNegative(prior.MaxCoefficientSD) || prior.MaxCoefficientSD > 100 || !finiteNonNegative(prior.MaxResidualMM) || prior.MaxResidualMM > 1e6 || prior.Outside > PCANearestBoundary {
		return nil, ErrPCAShapeInvalid
	}
	if prior.Weight == 0 && prior.Model == nil {
		if len(prior.CoefficientsSD) != 0 {
			return nil, ErrPCAShapeInvalid
		}
		r, e := ShapeDetectionLevelSet(ctx, v, init, p, o)
		if e != nil {
			return nil, e
		}
		return &PCAShapePriorResult{Segmentation: r, Prior: PCAShapePriorDiagnostics{Limitation: "prior disabled; base solver result"}}, nil
	}
	if e := prior.Model.parameters(prior.CoefficientsSD, prior.Pose, prior.Outside); e != nil {
		return nil, e
	}
	l, e := prior.Limits.normalize()
	if e != nil {
		return nil, e
	}
	n, e := prior.Geometry.count(l)
	if e != nil {
		return nil, e
	}
	if prior.Geometry != prior.Model.data.Geometry || prior.Geometry.Dimensions != [3]int{v.Columns, v.Rows, v.Slices} || prior.Geometry.SpacingMM != v.SpacingMM {
		return nil, fmt.Errorf("%w: explicit grid/spacing mismatch", ErrPCAShapeInvalid)
	}
	if len(prior.CoefficientsSD) > l.MaxComponents || int64(n)*int64(len(prior.CoefficientsSD)+1)*8 > l.MaxWork {
		return nil, ErrPCAShapeLimit
	}
	if prior.MaxCoefficientSD == 0 {
		prior.MaxCoefficientSD = 3
	}
	if prior.MaxResidualMM == 0 {
		prior.MaxResidualMM = 1
	}
	if !(prior.MaxCoefficientSD > 0 && prior.MaxCoefficientSD <= 100) || !pcaFinite(prior.MaxCoefficientSD) || !(prior.MaxResidualMM > 0 && prior.MaxResidualMM <= 1e6) || !pcaFinite(prior.MaxResidualMM) {
		return nil, ErrPCAShapeInvalid
	}
	prior.CoefficientsSD = append([]float64(nil), prior.CoefficientsSD...)
	d := PCAShapePriorDiagnostics{ModelID: prior.Model.data.ID, Provenance: prior.Model.data.Provenance, Weight: prior.Weight, CoefficientsSD: append([]float64(nil), prior.CoefficientsSD...), Pose: prior.Pose, Limitation: "fixed synthetic/research approximate SDF; clipped-field residual is not anatomical confidence; evolution differs from ITK's sparse-field/MAP optimizer"}
	for _, c := range prior.CoefficientsSD {
		d.OutsideDistribution = d.OutsideDistribution || math.Abs(c) > prior.MaxCoefficientSD
	}
	if prior.Weight == 0 {
		r, e := ShapeDetectionLevelSet(ctx, v, init, p, o)
		if e != nil {
			return nil, e
		}
		return &PCAShapePriorResult{Segmentation: r, Prior: d}, nil
	}
	if p.Evolution != DenseBand {
		return nil, fmt.Errorf("%w: shape prior currently requires DenseBand", ErrPCAShapeInvalid)
	}
	// Reject an uncovered posed full domain before reading the source. With
	// explicit nearest-boundary policy the evaluated crop count is disclosed.
	if prior.Outside == PCARejectOutside {
		for _, x := range []int{0, v.Columns - 1} {
			for _, y := range []int{0, v.Rows - 1} {
				for _, z := range []int{0, v.Slices - 1} {
					if _, _, e := prior.Model.value(prior.CoefficientsSD, prior.Pose, prior.Outside, prior.Geometry.point(x, y, z)); e != nil {
						return nil, e
					}
				}
			}
		}
	}
	baseLimits, e := o.Limits.normalize()
	if e != nil {
		return nil, e
	}
	reserve := int64(n)*4 + int64(len(prior.CoefficientsSD))*16 + 8192
	budget := min(baseLimits.MaxBytes, l.MaxBytes)
	if reserve >= budget {
		return nil, ErrPCAShapeLimit
	}
	o.Limits = baseLimits
	o.Limits.MaxBytes = budget - reserve
	state := &pcaShapeEvolution{options: prior, diagnostics: d}
	r, e := shapeDetectionLevelSet(ctx, v, init, p, o, state)
	if e != nil {
		return nil, e
	}
	if e = ctx.Err(); e != nil {
		return nil, fmt.Errorf("%w: %w", ErrLevelSetCancelled, e)
	}
	state.diagnostics.OutsideDistribution = state.diagnostics.OutsideDistribution || state.diagnostics.InitialFieldRMSMM > prior.MaxResidualMM
	return &PCAShapePriorResult{Segmentation: r, Prior: state.diagnostics}, nil
}

func (p *pcaShapeEvolution) prepare(g *levelSetGrid) error {
	p.target = make([]float32, g.n)
	width := float64(levelSetBandVoxels) * g.hmax
	for i := range p.target {
		if i%1024 == 0 {
			if e := levelSetContextError(g.ctx); e != nil {
				return e
			}
		}
		x, y, z := g.coords(i)
		point := p.options.Geometry.point(x+g.origin[0], y+g.origin[1], z+g.origin[2])
		v, out, e := p.options.Model.value(p.options.CoefficientsSD, p.options.Pose, p.options.Outside, point)
		if e != nil {
			return e
		}
		if out {
			p.diagnostics.ExtrapolatedVoxels++
		}
		p.target[i] = float32(math.Max(-width, math.Min(width, v)))
	}
	p.diagnostics.EvaluatedVoxels = g.n
	return levelSetContextError(g.ctx)
}
func (p *pcaShapeEvolution) measure(ctx context.Context, phi []float32, initial bool) error {
	sum := 0.0
	for i, v := range phi {
		if i%4096 == 0 {
			if e := levelSetContextError(ctx); e != nil {
				return e
			}
		}
		delta := float64(v) - float64(p.target[i])
		sum += delta * delta
	}
	mean := sum / float64(len(phi))
	rms, energy := math.Sqrt(mean), p.options.Weight*mean/2
	if initial {
		p.diagnostics.InitialFieldRMSMM = rms
		p.diagnostics.InitialMeanPotential = energy
	} else {
		p.diagnostics.FinalFieldRMSMM = rms
		p.diagnostics.FinalMeanPotential = energy
	}
	return levelSetContextError(ctx)
}
