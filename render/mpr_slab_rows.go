package render

import (
	"context"
	"math"
)

// scalarSlabRow composes into one disjoint output row. Only Average needs a
// denominator scratch row; it belongs to one admitted worker and is reused.
type scalarSlabRow struct {
	values, weights []float64
	valid           []byte
	mode            SlabMode
	trapezoid       bool
}

func newScalarSlabRow(width, samples int, mode SlabMode, trapezoid bool) scalarSlabRow {
	row := scalarSlabRow{mode: mode, trapezoid: trapezoid}
	if mode == SlabAverage && samples > 1 {
		row.weights = make([]float64, width)
	}
	return row
}

func (r *scalarSlabRow) bind(plane *ScalarPlane, y int) {
	start := y * plane.width
	r.values = plane.values[start : start+plane.width]
	r.valid = plane.valid[start : start+plane.width]
	clear(r.weights)
}

func (r *scalarSlabRow) add(x, k, samples int, value float64, ok bool) {
	if !ok {
		return
	}
	switch r.mode {
	case SlabMinIP:
		if r.valid[x] == 0 || value < r.values[x] {
			r.values[x] = value
		}
	case SlabAverage, SlabSum:
		weight := 1.0
		if r.trapezoid {
			if samples > 1 && (k == 0 || k == samples-1) {
				weight = 0.5
			}
			// Keep the baseline multiply/add, including interior weights of 1.
			r.values[x] += weight * value
		} else {
			r.values[x] += value
		}
		if len(r.weights) != 0 {
			r.weights[x] += weight
		}
	default:
		if r.valid[x] == 0 || value > r.values[x] {
			r.values[x] = value
		}
	}
	r.valid[x] = 1
}

func (r *scalarSlabRow) finish(ctx context.Context) {
	if r.mode != SlabAverage || len(r.weights) == 0 {
		return
	}
	for x, weight := range r.weights {
		if ctx.Err() != nil {
			return
		}
		if weight != 0 {
			r.values[x] /= weight
		}
	}
}

func scalarSlabCoordinate(start, end, samples, k int) float64 {
	coordinate := float64(start)
	if samples > 1 {
		coordinate += float64(k) * float64(end-start) / float64(samples-1)
		if k == samples-1 {
			coordinate = float64(end)
		}
	}
	return coordinate
}

func renderCachedScalarSlabRowsContext(ctx context.Context, scalar *ScalarPlane, sampler *volumeSampler,
	plane MPRPlane, start, end, samples int, mode SlabMode, interpolation Interpolation, trapezoid bool,
) error {
	return parallelRowsWorkerContext(ctx, scalar.height, func() func(int) {
		row := newScalarSlabRow(scalar.width, samples, mode, trapezoid)
		return func(y int) {
			row.bind(scalar, y)
			z := orthogonalDisplaySliceIndex(y, scalar.height, sampler.depth)
			for k := 0; k < samples; k++ {
				if ctx.Err() != nil {
					return
				}
				coordinate := scalarSlabCoordinate(start, end, samples, k)
				integer := coordinate == math.Trunc(coordinate)
				for x := 0; x < scalar.width; x++ {
					if ctx.Err() != nil {
						return
					}
					position := Vec3{X: float64(x), Y: float64(y), Z: coordinate}
					switch plane {
					case MPRPlaneCoronal:
						position.Y, position.Z = coordinate, float64(z)
					case MPRPlaneSagittal:
						position.X, position.Y, position.Z = coordinate, float64(x), float64(z)
					}
					var value float64
					var ok bool
					if integer {
						value, ok = sampler.valueAt(int(position.X), int(position.Y), int(position.Z))
					} else {
						value, ok = sampler.sampleAt(position, interpolation)
					}
					row.add(x, k, samples, value, ok)
				}
			}
			row.finish(ctx)
		}
	})
}

// Affine rows retain 24 bytes per pixel of original ray origins, plus 8 for
// Average. The existing admission reserves 8*(samples-1) extra bytes per
// output pixel. Workers never exceed output rows, so this eligibility check
// covers the whole scratch without relaxing a limit or adding an allocation
// for a thin request. Non-affine/irregular and separable cubic rays retain the
// reference path (including #1256's precomputed ray taps).
func scalarSlabRowsEligible(sampler *volumeSampler, normal Vec3, samples int, mode SlabMode, interpolation Interpolation) bool {
	vol := sampler.vol
	if vol.tilt.Applied || (len(vol.positions) != 0 && vol.uniformSliceStep() <= 0) {
		return false
	}
	scratch := 24
	if mode == SlabAverage && samples > 1 {
		scratch += 8
	}
	if samples <= 1 || samples-1 < scratch/8 {
		return false
	}
	if interpolation == InterpolationCubic {
		if _, separable := vol.voxelLine(vol.Origin, normal).separableAxis(); separable {
			return false
		}
	}
	return true
}

func resliceScalarSlabRowsContext(ctx context.Context, sampler *volumeSampler, plane Plane, normal Vec3,
	width, height int, firstOffset float64, samples int, step float64, mode SlabMode,
	defaultVOI WindowLevel, limits MPRLimits, interpolation Interpolation, trapezoid bool,
) (*ScalarPlane, bool, error) {
	if !scalarSlabRowsEligible(sampler, normal, samples, mode, interpolation) {
		return nil, false, nil
	}
	scalar, err := newScalarPlane(width, height, samples, sampler.photometric, defaultVOI, limits)
	if err != nil {
		return nil, true, err
	}
	denomW := float64(maxInt(width-1, 1))
	denomH := float64(maxInt(height-1, 1))
	direction := sampler.vol.voxelLine(plane.Origin, normal)
	err = parallelRowsWorkerContext(ctx, height, func() func(int) {
		origins := make([]Vec3, width)
		row := newScalarSlabRow(width, samples, mode, trapezoid)
		return func(y int) {
			row.bind(scalar, y)
			t := float64(y) / denomH
			for x := range origins {
				if ctx.Err() != nil {
					return
				}
				// Preserve Plane.At and voxelLine's original pixel arithmetic.
				ray := sampler.vol.voxelLine(plane.At(float64(x)/denomW, t), normal)
				origins[x] = Vec3{X: ray.x0, Y: ray.y0, Z: ray.pos0}
			}
			for k := 0; k < samples; k++ {
				if ctx.Err() != nil {
					return
				}
				distance := (firstOffset + float64(k)) * step
				for x, origin := range origins {
					if ctx.Err() != nil {
						return
					}
					position := Vec3{
						X: origin.X + distance*direction.dx,
						Y: origin.Y + distance*direction.dy,
						Z: sampler.vol.positionToSlice(origin.Z + distance*direction.dpos),
					}
					value, ok := sampler.sampleAt(position, interpolation)
					row.add(x, k, samples, value, ok)
				}
			}
			row.finish(ctx)
		}
	})
	if err != nil {
		return nil, true, err
	}
	return scalar, true, nil
}
