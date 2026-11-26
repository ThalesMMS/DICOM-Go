package roi

import (
	"context"
	"errors"
	"fmt"
	"github.com/ThalesMMS/dicom-go/internal/parallel"
	"github.com/ThalesMMS/dicom-go/render"
	"math"
	"sync/atomic"
)

var ErrBinaryAntiAlias = errors.New("roi: invalid binary anti-alias request")
var ErrBinaryAntiAliasLimit = errors.New("roi: binary anti-alias resource limit")

// BinaryAntiAliasOptions specifies an offline estimated surface; it never
// replaces the binary source. Zero values choose documented finite defaults.
type BinaryAntiAliasOptions struct {
	MaxIterations                                              int
	MaximumRMSChangeMM, TimeStepMM2, BandWidthMM, SignMarginMM float64
	MaxVoxels, MaxVoxelIterations, MaxBytes                    uint64
	Workers                                                    int
	Progress                                                   func(BinaryAntiAliasIteration)
}
type BinaryAntiAliasIteration struct {
	Iteration     int
	RMSChangeMM   float64
	ClampedVoxels int
}
type BinaryAntiAliasReport struct {
	Options                  BinaryAntiAliasOptions
	Iterations               int
	Converged                bool
	StopReason               string
	LogicalBytes             uint64
	BandVoxels, InsideVoxels int
	History                  []BinaryAntiAliasIteration
}

// BinaryAntiAliasResult owns an estimated implicit field in mm (positive
// inside), its initial field and an unchanged snapshot of the original mask.
// It structurally implements mesh.Field without introducing a package cycle.
// Callers keep it stable during sampling/extraction. This is not a distance
// function after evolution and does not promise surface volume/topology.
type BinaryAntiAliasResult struct {
	Size            [3]int
	Phi, InitialPhi []float32
	Original        *VolumeMask
	Report          BinaryAntiAliasReport
	affine          render.GeometryAffine
}

func (r *BinaryAntiAliasResult) Dims() [3]int                          { return r.Size }
func (r *BinaryAntiAliasResult) IndexToPatient() render.GeometryAffine { return r.affine }
func (r *BinaryAntiAliasResult) ReadSlice(ctx context.Context, z int, dst []float32) error {
	if ctx == nil {
		ctx = context.Background()
	}
	if e := ctx.Err(); e != nil {
		return e
	}
	if r == nil || z < 0 || z >= r.Size[2] || len(dst) < r.Size[0]*r.Size[1] || len(r.Phi) != r.Size[0]*r.Size[1]*r.Size[2] {
		return ErrBinaryAntiAlias
	}
	plane := r.Size[0] * r.Size[1]
	copy(dst, r.Phi[z*plane:(z+1)*plane])
	return ctx.Err()
}

// BinaryAntiAlias applies projected mean-curvature flow on a fixed physical
// band. The signed constraint at every voxel is retained, with an explicit
// strictly positive margin, so zero ties cannot relabel outside samples.
// Reflecting domain faces use the existing level-set stencil. No Pixel Data,
// topology repair, automatic extraction, viewer or 3D rendering is performed.
func BinaryAntiAlias(ctx context.Context, src *VolumeMask, o BinaryAntiAliasOptions) (*BinaryAntiAliasResult, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	if e := ctx.Err(); e != nil {
		return nil, e
	}
	size, h, origin, n, e := antiAliasGeometry(ctx, src)
	if e != nil {
		return nil, e
	}
	hmin, hmax := math.Min(h[0], h[1]), math.Max(h[0], h[1])
	dims := 2
	if size[2] > 1 {
		dims = 3
		hmin = math.Min(hmin, h[2])
		hmax = math.Max(hmax, h[2])
	}
	if o.MaxIterations == 0 {
		o.MaxIterations = 100
	}
	if o.MaxVoxels == 0 {
		o.MaxVoxels = 16 << 20
	}
	if o.MaxBytes == 0 {
		o.MaxBytes = 512 << 20
	}
	if o.MaxVoxelIterations == 0 {
		o.MaxVoxelIterations = 1 << 31
	}
	if o.MaximumRMSChangeMM == 0 {
		o.MaximumRMSChangeMM = .005 * hmin
	}
	if o.BandWidthMM == 0 {
		o.BandWidthMM = 3 * hmax
	}
	if o.SignMarginMM == 0 {
		o.SignMarginMM = 1e-4 * hmin
	}
	maxDT := hmin * hmin / (4 * float64(dims))
	if o.TimeStepMM2 == 0 {
		o.TimeStepMM2 = maxDT
	}
	if o.MaxIterations < 1 || o.MaxIterations > 100000 || o.Workers < 0 || o.Workers > 64 {
		return nil, ErrBinaryAntiAlias
	}
	for _, v := range []float64{o.MaximumRMSChangeMM, o.TimeStepMM2, o.BandWidthMM, o.SignMarginMM, maxDT} {
		if !antiAliasFinite(v) || v <= 0 {
			return nil, ErrBinaryAntiAlias
		}
	}
	if o.TimeStepMM2 > maxDT || o.SignMarginMM >= .5*hmin || o.BandWidthMM < hmax || float32(o.SignMarginMM) <= 0 || math.IsInf(float64(float32(o.SignMarginMM)), 0) {
		return nil, ErrBinaryAntiAlias
	}
	if uint64(n) > o.MaxVoxels || uint64(n) > math.MaxInt32 || uint64(n) > o.MaxVoxelIterations/uint64(o.MaxIterations) {
		return nil, ErrBinaryAntiAliasLimit
	}
	// Borrowed input + snapshot, three float fields, EDT, int32 fixed band,
	// owned/source axes, sequential EDT line scratch, and bounded trace.
	bytes := uint64(0)
	maxAxis := max(size[0], size[1], size[2])
	axisCount := uint64(size[0]) + uint64(size[1]) + uint64(size[2])
	for _, v := range []uint64{uint64(n) * 26, axisCount * 16, uint64(maxAxis)*40 + 8, uint64(o.MaxIterations) * 24} {
		if !antiAliasReserve(&bytes, v, o.MaxBytes) {
			return nil, ErrBinaryAntiAliasLimit
		}
	}
	inside := 0
	for i, v := range src.Voxels {
		if i&4095 == 0 {
			if e := ctx.Err(); e != nil {
				return nil, e
			}
		}
		if v {
			inside++
		}
	}
	if inside == 0 || inside == n {
		return nil, fmt.Errorf("%w: mask needs inside and outside samples", ErrBinaryAntiAlias)
	}
	original := &VolumeMask{Columns: size[0], Rows: size[1], Slices: size[2], X: append([]float64(nil), src.X...), Y: append([]float64(nil), src.Y...), Z: append([]float64(nil), src.Z...), Voxels: append([]bool(nil), src.Voxels...)}
	phi, e := antiAliasInitial(ctx, original, size, h, hmin)
	if e != nil {
		return nil, e
	}
	initial := append([]float32(nil), phi...)
	next := append([]float32(nil), phi...)
	band := make([]int32, 0, n)
	for i, v := range phi {
		if i&4095 == 0 {
			if e := ctx.Err(); e != nil {
				return nil, e
			}
		}
		if math.Abs(float64(v)) <= o.BandWidthMM {
			band = append(band, int32(i))
		}
	}
	if len(band) == 0 {
		return nil, ErrBinaryAntiAlias
	}
	g := &levelSetGrid{ctx: ctx, nx: size[0], ny: size[1], nz: size[2], n: n, size: size, stride: [3]int{1, size[0], size[0] * size[1]}, h: h, hmin: hmin, hmax: hmax, dims: dims, bounded: true}
	state := &levelSetState{levelSetGrid: g, phi: phi}
	terms := levelSetTerms{curvature: 1}
	r := &BinaryAntiAliasResult{Size: size, Original: original, InitialPhi: initial, Report: BinaryAntiAliasReport{Options: o, LogicalBytes: bytes, BandVoxels: len(band), InsideVoxels: inside, StopReason: "iteration-limit", History: make([]BinaryAntiAliasIteration, 0, o.MaxIterations)}}
	r.Report.Options.Progress = nil
	r.affine = render.GeometryAffine{h[0], 0, 0, origin[0], 0, h[1], 0, origin[1], 0, 0, h[2], origin[2], 0, 0, 0, 1}
	workers := min(64, parallel.DefaultWorkers())
	if o.Workers > 0 {
		workers = min(workers, o.Workers)
	}
	for iteration := 1; iteration <= o.MaxIterations; iteration++ {
		var invalid atomic.Bool
		if e := parallel.Run(ctx, len(band), workers, 256, func() func(int) {
			return func(k int) {
				if k&63 == 0 && ctx.Err() != nil {
					return
				}
				i := int(band[k])
				rate, _, _ := state.ratePotentialRegularized(i, terms, false, 0, 0)
				if !antiAliasFinite(float64(rate)) {
					invalid.Store(true)
					return
				}
				v := float64(phi[i]) + o.TimeStepMM2*float64(rate)
				if original.Voxels[i] {
					v = math.Max(v, o.SignMarginMM)
				} else {
					v = math.Min(v, -o.SignMarginMM)
				}
				if !antiAliasFinite(v) || math.IsInf(float64(float32(v)), 0) {
					invalid.Store(true)
					return
				}
				next[i] = float32(v)
			}
		}); e != nil {
			return nil, e
		}
		if invalid.Load() {
			return nil, fmt.Errorf("%w: nonfinite evolution", ErrBinaryAntiAlias)
		}
		squared := 0.
		clamped := 0
		for k, j := range band {
			if k&4095 == 0 {
				if e := ctx.Err(); e != nil {
					return nil, e
				}
			}
			i := int(j)
			d := float64(next[i]) - float64(phi[i])
			squared += d * d
			if math.Abs(float64(next[i])) <= o.SignMarginMM*(1+1e-6) {
				clamped++
			}
		}
		rms := math.Sqrt(squared / float64(len(band)))
		if !antiAliasFinite(rms) {
			return nil, ErrBinaryAntiAlias
		}
		phi, next = next, phi
		state.phi = phi
		r.Report.Iterations = iteration
		p := BinaryAntiAliasIteration{iteration, rms, clamped}
		r.Report.History = append(r.Report.History, p)
		if o.Progress != nil {
			o.Progress(p)
		}
		if e := ctx.Err(); e != nil {
			return nil, e
		}
		if rms <= o.MaximumRMSChangeMM {
			r.Report.Converged = true
			r.Report.StopReason = "rms-tolerance"
			break
		}
	}
	r.Phi = phi
	if e := ctx.Err(); e != nil {
		return nil, e
	}
	return r, nil
}
func antiAliasFinite(v float64) bool { return !math.IsNaN(v) && !math.IsInf(v, 0) }
func antiAliasReserve(total *uint64, v, limit uint64) bool {
	if *total > limit || v > limit-*total {
		return false
	}
	*total += v
	return true
}
