package render

import (
	"context"
	"math"
)

// A callback is not known affine: corner samples cannot bound its interior.
// Such registration retains full marching. Identity patient registration
// still supports distinct orientations, spacing, origins and dimensions.
func (l *vrSpaceLeap) limitFusion(ctx context.Context, primary *volumeSampler, f *vrFusionFrame) error {
	if f == nil {
		return nil
	}
	secondary := f.reader.sampler
	if primary.vol.tilt.Applied || secondary.vol.tilt.Applied {
		for i := range l.empty {
			l.empty[i] = false
		}
		return nil
	}
	spacing := math.Min(secondary.vol.ColSpacing, math.Min(secondary.vol.RowSpacing, secondary.vol.effectiveSliceSpacing()))
	if !finite(spacing) || spacing <= 0 {
		for i := range l.empty {
			l.empty[i] = false
		}
		return nil
	}
	grid, err := buildVRBlockGrid(ctx, secondary)
	if err != nil {
		return err
	}
	overlay := newVRSpaceLeap(grid, secondary)
	overlay.forDVR(f.tf, f.lut, f.window, f.dataMin, f.dataMax)
	if f.opacity == 0 {
		for i := range overlay.empty {
			overlay.empty[i] = true
		}
	}
	return parallelRowsContext(ctx, l.grid.nz, func(bz int) {
		for by := 0; by < l.grid.ny; by++ {
			for bx := 0; bx < l.grid.nx; bx++ {
				if ctx.Err() != nil {
					return
				}
				index := (bz*l.grid.ny+by)*l.grid.nx + bx
				if !l.empty[index] {
					continue
				}
				lo, hi := Vec3{X: math.Inf(1), Y: math.Inf(1), Z: math.Inf(1)}, Vec3{X: math.Inf(-1), Y: math.Inf(-1), Z: math.Inf(-1)}
				valid := true
				largestPatientCoordinate := math.Max(math.Abs(secondary.vol.Origin.X), math.Max(math.Abs(secondary.vol.Origin.Y), math.Abs(secondary.vol.Origin.Z)))
				for z := 0; z < 2; z++ {
					for y := 0; y < 2; y++ {
						for x := 0; x < 2; x++ {
							point := Vec3{X: float64(min(primary.cols-1, (bx+x)*vrBlockSize)), Y: float64(min(primary.rows-1, (by+y)*vrBlockSize)), Z: float64(min(primary.depth-1, (bz+z)*vrBlockSize))}
							patient := primary.vol.VoxelToPatient(point)
							largestPatientCoordinate = math.Max(largestPatientCoordinate, math.Max(math.Abs(patient.X), math.Max(math.Abs(patient.Y), math.Abs(patient.Z))))
							point = secondary.vol.PatientToVoxel(patient)
							if !finiteMPRVec3(point) {
								valid = false
								continue
							}
							lo.X, lo.Y, lo.Z = math.Min(lo.X, point.X), math.Min(lo.Y, point.Y), math.Min(lo.Z, point.Z)
							hi.X, hi.Y, hi.Z = math.Max(hi.X, point.X), math.Max(hi.Y, point.Y), math.Max(hi.Z, point.Z)
						}
					}
				}
				if !valid {
					l.empty[index] = false
					continue
				}
				// Enclose affine floating point rounding and interpolation support.
				margin := 1e-8 + 1e-12*(1+largestPatientCoordinate)/spacing
				if !finite(margin) {
					l.empty[index] = false
					continue
				}
				lo = lo.Sub(Vec3{X: margin, Y: margin, Z: margin})
				hi = hi.Add(Vec3{X: margin, Y: margin, Z: margin})
				maximum := Vec3{X: float64(secondary.cols - 1), Y: float64(secondary.rows - 1), Z: float64(secondary.depth - 1)}
				if hi.X < 0 || hi.Y < 0 || hi.Z < 0 || lo.X > maximum.X || lo.Y > maximum.Y || lo.Z > maximum.Z {
					continue
				}
				// Mixed inside/outside blocks retain sampling and its overlap diagnostics.
				if lo.X < 0 || lo.Y < 0 || lo.Z < 0 || hi.X > maximum.X || hi.Y > maximum.Y || hi.Z > maximum.Z {
					l.empty[index] = false
					continue
				}
				x0, y0, z0 := int(math.Floor(lo.X))/vrBlockSize, int(math.Floor(lo.Y))/vrBlockSize, int(math.Floor(lo.Z))/vrBlockSize
				x1, y1, z1 := int(math.Floor(hi.X))/vrBlockSize, int(math.Floor(hi.Y))/vrBlockSize, int(math.Floor(hi.Z))/vrBlockSize
				empty := true
				for z := z0; z <= z1 && empty; z++ {
					for y := y0; y <= y1 && empty; y++ {
						for x := x0; x <= x1; x++ {
							if !overlay.empty[(z*grid.ny+y)*grid.nx+x] {
								empty = false
								break
							}
						}
					}
				}
				l.empty[index] = empty
			}
		}
	})
}

// The first retained logical sample establishes the original overlap result.
// Checking clip/label metadata also covers a skipped block whose first sample
// is cut away but whose later samples are retained. Outside or fully inside
// secondary blocks are the only blocks this optimization skips.
func (w *vrFusionWorker) probeSkipped(ctx context.Context, o, d, boxMin, size Vec3, first, last, step float64, clip *VRClip, labels *VRLabelMap) {
	for t, index := first, 0; t <= last; t, index = t+step, index+1 {
		if index&63 == 0 && ctx.Err() != nil {
			return
		}
		point := o.Add(d.Scale(t))
		tex := Vec3{X: (point.X - boxMin.X) / size.X, Y: (point.Y - boxMin.Y) / size.Y, Z: (point.Z - boxMin.Z) / size.Z}
		if clip.keep(tex) && labels.keeps(tex) {
			_ = w.sample(tex, step)
			return
		}
	}
}
