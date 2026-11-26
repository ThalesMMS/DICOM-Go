package render

import "math"

// Interpolation modes of the CPU sampler (#1006), sharing the view-state
// Interpolation values: nearest for labelmaps and segmentations, trilinear
// (the default, and the interactive mode) and Catmull-Rom cubic (Keys,
// a = -0.5) for settled MPR/CPR frames. All use the border policy of
// voxelSample: within half a voxel of the grid positions and kernel taps are
// clamped to it; farther out the sample is outside.
//
// Windowed-sinc (Lanczos, Kaiser) and B-spline kernels are deliberately not
// offered here: they cost 6³–8³ taps per sample in Go, sinc kernels ring at
// CT edges, and B-splines need a coefficient prefilter of the whole volume.
// The cubic B-spline exists only for non-interactive exports (#1190,
// volume_bspline.go: ResampleRegularGridContext and registration.Resample).

// sampleAt samples voxel coordinates p with the interpolation mode;
// InterpolationUnknown is trilinear.
func (s *volumeSampler) sampleAt(p Vec3, mode Interpolation) (float64, bool) {
	switch mode {
	case InterpolationNearest:
		return s.nearestAt(p)
	case InterpolationCubic:
		return s.cubicAt(p)
	}
	return s.trilinearAt(p)
}

// nearestAt returns the voxel nearest to p, rounding halves up
// (floor(v+0.5), as VTK's nearest-neighbour interpolation).
func (s *volumeSampler) nearestAt(p Vec3) (float64, bool) {
	if s == nil {
		return 0, false
	}
	var ok bool
	if p.X, ok = clampToGrid(p.X, s.cols); !ok {
		return 0, false
	}
	if p.Y, ok = clampToGrid(p.Y, s.rows); !ok {
		return 0, false
	}
	if p.Z, ok = clampToGrid(p.Z, s.depth); !ok {
		return 0, false
	}
	nearest := func(v float64, n int) int { return min(NearestIndex(v), n-1) }
	return s.valueAt(nearest(p.X, s.cols), nearest(p.Y, s.rows), nearest(p.Z, s.depth))
}

// cubicTaps returns the voxel indices and Catmull-Rom weights of one axis:
// a single tap on a voxel (fraction 0), otherwise four taps clamped to the
// n voxels of the axis.
func cubicTaps(v float64, n int) (index [4]int, weight [4]float64, taps int) {
	base := int(math.Floor(v))
	f := v - float64(base)
	if f == 0 {
		index[0], weight[0] = base, 1
		return index, weight, 1
	}
	f2, f3 := f*f, f*f*f
	weight = [4]float64{
		(-f3 + 2*f2 - f) / 2,
		(3*f3 - 5*f2 + 2) / 2,
		(-3*f3 + 4*f2 + f) / 2,
		(f3 - f2) / 2,
	}
	for i := range index {
		index[i] = max(0, min(base-1+i, n-1))
	}
	return index, weight, 4
}

// cubicAt is the tricubic Catmull-Rom sample at p. Its weights sum to 1, so
// constant regions stay constant; like any interpolating cubic it may
// overshoot the neighbouring voxels at sharp edges.
func (s *volumeSampler) cubicAt(p Vec3) (float64, bool) {
	if s == nil {
		return 0, false
	}
	var ok bool
	if p.X, ok = clampToGrid(p.X, s.cols); !ok {
		return 0, false
	}
	if p.Y, ok = clampToGrid(p.Y, s.rows); !ok {
		return 0, false
	}
	if p.Z, ok = clampToGrid(p.Z, s.depth); !ok {
		return 0, false
	}
	xi, xw, xn := cubicTaps(p.X, s.cols)
	yi, yw, yn := cubicTaps(p.Y, s.rows)
	zi, zw, zn := cubicTaps(p.Z, s.depth)
	sum := 0.0
	for k := 0; k < zn; k++ {
		plane := 0.0
		for j := 0; j < yn; j++ {
			row := 0.0
			for i := 0; i < xn; i++ {
				row += xw[i] * s.valueOrZero(xi[i], yi[j], zi[k])
			}
			plane += yw[j] * row
		}
		sum += zw[k] * plane
	}
	return sum, true
}

// SampleAt samples fractional voxel coordinates of the source Volume with
// the interpolation mode (InterpolationUnknown is trilinear): nearest for
// labelmaps, cubic for settled MPR/CPR frames.
func (r *VolumeReader) SampleAt(p Vec3, mode Interpolation) (float64, bool) {
	if r == nil || r.source == nil || r.sampler == nil {
		return 0, false
	}
	if r.sampler.vol != r.source {
		p = r.sampler.vol.PatientToVoxel(r.source.VoxelToPatient(p))
	}
	return r.sampler.sampleAt(p, mode)
}

// SamplePatientAt samples a point in DICOM patient LPS coordinates (mm) with
// the interpolation mode (InterpolationUnknown is trilinear), under the same
// half-voxel border policy as SampleAt. It is the patient-space counterpart
// used by physical line profiles (roi.LineProfile).
func (r *VolumeReader) SamplePatientAt(patient Vec3, mode Interpolation) (float64, bool) {
	if r == nil || r.sampler == nil || r.sampler.vol == nil {
		return 0, false
	}
	return r.sampler.sampleAt(r.sampler.vol.PatientToVoxel(patient), mode)
}
