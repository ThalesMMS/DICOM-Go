package render

import "math"

// HU / texture-space accessors on the shared Volume for the 3D volume
// renderer (3d-toolbar-plan.md §5.1). ValueAt already returns rescaled HU (slope/
// intercept applied in PixelValueAt) and TrilinearAt samples it; this adds the
// dataset HU range, a recommended window, a SampleHU alias, and the physical
// extent the camera/raycaster use so anisotropic spacing is not squashed.

// SampleHU returns the trilinearly-interpolated HU value at fractional voxel
// coordinates. ok is false only when fully outside the volume.
func (v *Volume) SampleHU(p Vec3) (float64, bool) { return v.TrilinearAt(p) }

// HURange returns the dataset HU min/max. It is computed once (strided for large
// volumes to bound cost) and cached. ok is false for an empty volume.
func (v *Volume) HURange() (huMin, huMax float64, ok bool) {
	return v.huRangeFromSampler(nil)
}

func (v *Volume) huRangeFromSampler(sampler *volumeSampler) (huMin, huMax float64, ok bool) {
	if v == nil || v.Depth == 0 {
		return 0, 0, false
	}
	v.huMu.Lock()
	defer v.huMu.Unlock()
	if v.huRangeOK {
		return v.huMin, v.huMax, true
	}
	ownsSampler := false
	if sampler == nil || sampler.vol != v {
		var samplerOK bool
		sampler, samplerOK = newVolumeSampler(v)
		if !samplerOK {
			return 0, 0, false
		}
		ownsSampler = true
	}
	if ownsSampler {
		defer sampler.Close()
	}
	const target = 2_000_000 // cap samples scanned
	total := sampler.cols * sampler.rows * sampler.depth
	stride := 1
	if total > target {
		stride = total / target
	}
	huMin, huMax = math.Inf(1), math.Inf(-1)
	found := false
	// Visit exactly the previous flattened sample indices. Iterating every
	// skipped voxel made the supposedly bounded range scan linear in the entire
	// acquisition, even when only ~target samples were actually inspected.
	for i := 0; i < total; {
		x := i % sampler.cols
		plane := i / sampler.cols
		y, z := plane%sampler.rows, plane/sampler.rows
		if val, valid := sampler.valueAt(x, y, z); valid {
			if val < huMin {
				huMin = val
			}
			if val > huMax {
				huMax = val
			}
			found = true
		}
		if stride >= total-i {
			break
		}
		i += stride
	}
	if !found {
		return 0, 0, false
	}
	if huMax <= huMin {
		huMin--
		huMax++
	}
	v.huMin, v.huMax, v.huRangeOK = huMin, huMax, true
	return huMin, huMax, true
}

// RecommendedWindow returns a sensible default window: the series default when
// valid, otherwise spanning the dataset HU range.
func (v *Volume) RecommendedWindow() WindowLevel {
	if v != nil && len(v.slices) > 0 {
		dw := v.slices[0].DefaultWindow
		if dw.Width > 0 {
			return dw
		}
	}
	if lo, hi, ok := v.HURange(); ok {
		return WindowLevel{Center: (lo + hi) / 2, Width: hi - lo}
	}
	return WindowLevel{Center: 40, Width: 400}
}

// PhysicalSize is the volume's extent in mm along (X=col, Y=row, Z=slice),
// measured between the outermost voxel centres.
func (v *Volume) PhysicalSize() Vec3 {
	if v == nil {
		return Vec3{}
	}
	return Vec3{
		X: float64(v.Cols-1) * v.ColSpacing,
		Y: float64(v.Rows-1) * v.RowSpacing,
		Z: float64(v.Depth-1) * v.SliceSpacing,
	}
}

// BoundingRadiusMM is half the physical diagonal — the camera fit distance basis.
func (v *Volume) BoundingRadiusMM() float64 {
	s := v.PhysicalSize()
	return 0.5 * s.Length()
}

// FrameRadiusMM is the largest physical half-extent of the volume box (half its
// longest side). Fitting the camera to this — instead of the circumscribing
// BoundingRadiusMM — makes the anatomy fill most of the viewport (reference 16);
// the box corners may extend past the frame on oblique orbits, which matches a
// clinical VR view rather than the looser bounding-sphere fit.
func (v *Volume) FrameRadiusMM() float64 {
	if v == nil {
		return 1
	}
	s := v.PhysicalSize()
	m := s.X
	if s.Y > m {
		m = s.Y
	}
	if s.Z > m {
		m = s.Z
	}
	return 0.5 * m
}

// TextureToVoxel maps a [0,1]³ texture coordinate to fractional voxel coordinates
// (component i → i*(dim-1)).
func (v *Volume) TextureToVoxel(tex Vec3) Vec3 {
	return Vec3{
		X: tex.X * float64(v.Cols-1),
		Y: tex.Y * float64(v.Rows-1),
		Z: tex.Z * float64(v.Depth-1),
	}
}

// SampleHUTexture samples HU at a [0,1]³ texture coordinate.
func (v *Volume) SampleHUTexture(tex Vec3) (float64, bool) {
	return v.TrilinearAt(v.TextureToVoxel(tex))
}

// gradientAt returns the central-difference HU gradient at texture coordinate tex
// (six trilinear samples one voxel apart). It drives both Blinn-Phong shading and
// gradient-opacity modulation, so the renderer computes it once per opaque sample.
func (v *Volume) gradientAt(tex Vec3) Vec3 {
	dx := 1.0 / float64(maxInt(v.Cols, 2))
	dy := 1.0 / float64(maxInt(v.Rows, 2))
	dz := 1.0 / float64(maxInt(v.Depth, 2))
	hx1, _ := v.SampleHUTexture(Vec3{tex.X + dx, tex.Y, tex.Z})
	hx0, _ := v.SampleHUTexture(Vec3{tex.X - dx, tex.Y, tex.Z})
	hy1, _ := v.SampleHUTexture(Vec3{tex.X, tex.Y + dy, tex.Z})
	hy0, _ := v.SampleHUTexture(Vec3{tex.X, tex.Y - dy, tex.Z})
	hz1, _ := v.SampleHUTexture(Vec3{tex.X, tex.Y, tex.Z + dz})
	hz0, _ := v.SampleHUTexture(Vec3{tex.X, tex.Y, tex.Z - dz})
	return Vec3{X: hx1 - hx0, Y: hy1 - hy0, Z: hz1 - hz0}
}

// shadeWithGradient is the surface shading of a sample with the given HU
// gradient viewed along viewDir under a headlight, in the model of
// headlightShading with the legacy surface material: the shaded colour is
// colour*lit + specular. Flat regions (tiny gradient) are ambient only.
func (v *Volume) shadeWithGradient(grad, viewDir Vec3) (lit, specular float64) {
	material := legacyVRLightingMaterial()
	g := grad.Length()
	if g < 1e-6 {
		return material.Ambient, 0
	}
	n := grad.Scale(-1 / g) // outward (toward lower density)
	return headlightShading(n, viewDir.Scale(-1).Normalize(), material)
}

// vrOpacityThreshold ends a ray once its accumulated opacity leaves less
// than one 8-bit step of the background visible, VTK's
// g_opacityThreshold (the former 0.95 let up to 5% of the background leak
// through opaque tissue, #1063).
const vrOpacityThreshold = 1 - 1.0/255

// headlightShading is VTK's default headlight model for volumes
// (vtkVolumeShaderComposer, two-sided lighting as vtkRenderer's default,
// #1063): with light = eye and unit normal n, the shaded colour is
// colour*(ambient + diffuse*|n·l|) plus a white specular highlight
// specular*max(r·v, 0)^power added on top, where r reflects the light about
// n, so r·v = 2(n·l)²-1. Specular is not tinted by the tissue colour.
func headlightShading(normal, eye Vec3, material VRLightingMaterial) (lit, specular float64) {
	nDotL := normal.Dot(eye)
	rDotV := 2*nDotL*nDotL - 1
	nDotL = math.Abs(nDotL)
	if !(nDotL > 0) {
		return material.Ambient, 0
	}
	lit = material.Ambient + material.Diffuse*nDotL
	if rDotV > 0 {
		specular = material.Specular * math.Pow(rDotV, material.SpecularPower)
	} else if material.SpecularPower == 0 {
		specular = material.Specular
	}
	return lit, specular
}

// shadeVRWithGradient implements the shared CPU/WebGPU DVR convention: the
// shaded colour is colour*lit + specular (headlightShading). The scalar
// central difference arrives in index space without the 0.5 divisor;
// transforming the covector by transpose(PatientLPSToIndex) produces a
// physical patient-LPS normal.
func (v *Volume) shadeVRWithGradient(indexGradient, viewDir Vec3, material VRLightingMaterial) (lit, specular float64) {
	if v == nil || !material.valid() {
		return 0, 0
	}
	physical := Vec3{}
	if v.ColSpacing > 0 {
		physical = physical.Add(v.AxisX.Scale(0.5 * indexGradient.X / v.ColSpacing))
	}
	if v.RowSpacing > 0 {
		physical = physical.Add(v.AxisY.Scale(0.5 * indexGradient.Y / v.RowSpacing))
	}
	if v.SliceSpacing > 0 {
		physical = physical.Add(v.Normal.Scale(0.5 * indexGradient.Z / v.SliceSpacing))
	}
	magnitude := physical.Length()
	if magnitude <= 1e-6 {
		return material.Ambient, 0
	}
	normal := physical.Scale(-1 / magnitude)
	patientViewDirection := v.AxisX.Scale(viewDir.X).
		Add(v.AxisY.Scale(viewDir.Y)).
		Add(v.Normal.Scale(viewDir.Z))
	return headlightShading(normal, patientViewDirection.Scale(-1).Normalize(), material)
}

func maxInt(a, b int) int {
	if a > b {
		return a
	}
	return b
}
