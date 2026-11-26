package render

import "math"

// First-hit picking for the 3D renderer (3d-toolbar-plan.md §5.11). PickVR casts
// a ray through screen pixel (sx,sy) using the same step / clip / window gate as
// the renderer and returns the patient-space (mm) position where the composited
// alpha reaches the threshold, refined by bisection between the last sample
// below it and the first at or above it (#1004). ROI length is the Euclidean
// distance between two picked points.

const vrPickAlphaThreshold = 0.5

// PickVR returns the patient-space surface point under pixel (sx,sy), or ok=false
// if the ray misses any opaque sample.
func PickVR(vol *Volume, cam VRCamera, preset VRPreset, window WindowLevel, clip *VRClip, sx, sy, w, h int) (WorldPoint, bool) {
	if vol == nil || w <= 0 || h <= 0 {
		return WorldPoint{}, false
	}
	size := vol.PhysicalSize()
	if size.X <= 0 || size.Y <= 0 || size.Z <= 0 {
		return WorldPoint{}, false
	}
	boxMin := size.Scale(-0.5)
	boxMax := size.Scale(0.5)
	o, d := cam.Ray(sx, sy, w, h)
	t0, t1, hit := intersectAABB(o, d, boxMin, boxMax)
	if !hit {
		return WorldPoint{}, false
	}

	dataMin, dataMax, ok := vol.HURange()
	if !ok {
		dataMin, dataMax = 0, 1
	}
	window = normalizeWindow(window, vol.RecommendedWindow())
	var lut VRLUT
	if preset.TF.Domain() == VRTransferDomainHU {
		lut = preset.TF.BakeLUT(dataMin, dataMax, preset.TF.PreferredLUTSize())
	} else {
		lut = preset.TF.BakeWindowLUT(window, preset.TF.PreferredLUTSize())
	}
	steps := 256
	stepLen := size.Length() / float64(steps)

	texAt := func(t float64) Vec3 {
		pw := o.Add(d.Scale(t))
		return Vec3{
			X: (pw.X - boxMin.X) / size.X,
			Y: (pw.Y - boxMin.Y) / size.Y,
			Z: (pw.Z - boxMin.Z) / size.Z,
		}
	}
	// alphaAt is the transfer opacity of the sample at t (0 when clipped).
	alphaAt := func(t float64) float64 {
		tex := texAt(t)
		if !clip.keep(tex) {
			return 0
		}
		hu, ok := vol.SampleHUTexture(tex)
		if !ok {
			return 0
		}
		if preset.TF.Domain() == VRTransferDomainNormalized {
			return lut.Lookup(preset.TF.windowCoordinate(hu, window)).A
		}
		return lut.Lookup(normalizedVRDataCoordinate(hu, dataMin, dataMax)).A * windowedUnit(hu, window)
	}
	unit := preset.TF.OpacityUnitDistanceMM()
	accA := 0.0
	first := true
	for t := t0; t <= t1; t += stepLen {
		alpha := alphaAt(t)
		next := accA + (1-accA)*CorrectVRAlpha(alpha, 1, stepLen, unit)
		if next >= vrPickAlphaThreshold {
			if !first {
				// Opacity accumulated over the part of the step up to t'
				// with the sample at t'.
				before, start := accA, t-stepLen
				t = bisectVRRay(start, t, func(tt float64) bool {
					return before+(1-before)*CorrectVRAlpha(alphaAt(tt), 1, tt-start, unit) >= vrPickAlphaThreshold
				})
			}
			patient := vol.VoxelToPatient(vol.TextureToVoxel(texAt(t)))
			return WorldPoint{X: patient.X, Y: patient.Y, Z: patient.Z}, true
		}
		accA = next
		first = false
	}
	return WorldPoint{}, false
}

// VRLengthMM returns the Euclidean distance (mm) between two picked world points.
func VRLengthMM(a, b WorldPoint) float64 {
	dx := a.X - b.X
	dy := a.Y - b.Y
	dz := a.Z - b.Z
	return math.Sqrt(dx*dx + dy*dy + dz*dz)
}
