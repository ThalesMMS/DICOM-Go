package render

import (
	"context"
	"math"
)

// Volumetric shadows and scattering (#1064), after VTK's
// vtkGPUVolumeRayCastMapper (VolumetricScatteringBlending,
// GlobalIlluminationReach and vtkVolumeProperty's ScatteringAnisotropy,
// vtkVolumeShaderComposer's volumeShadow; written from their behaviour, no
// VTK code is copied). A shaded DVR sample also receives the light that
// reaches it through the volume: the transmittance of a shadow ray toward the
// light, weighted by a Henyey–Greenstein phase function, scales the sample's
// diffuse colour. The result blends with the surface shading by a coefficient
// that favours the surface model where the gradient is strong and the sample
// opaque, and the volumetric model in homogeneous tissue.

// VRScattering configures volumetric scattering. The zero value disables it.
type VRScattering struct {
	// Blend is VTK's VolumetricScatteringBlending in (0,2]: up to 1 it mixes
	// the surface and volumetric models, above 1 it favours the volumetric
	// one everywhere. Zero disables scattering.
	Blend float64
	// Reach bounds the shadow rays to this fraction of the volume diagonal,
	// VTK's GlobalIlluminationReach, in (0,1]; zero means 1.
	Reach float64
	// Anisotropy is the Henyey–Greenstein g in (-1,1): positive values
	// scatter forward (toward a light behind the tissue), zero is isotropic.
	Anisotropy float64
	// LightAzimuth and LightElevation place a key light relative to the
	// camera, in radians: 0,0 is a headlight, whose shadow rays run back
	// along the view ray and so cast no visible shadow; positive azimuth
	// moves the light to the viewer's left, positive elevation up.
	LightAzimuth, LightElevation float64
}

// Enabled reports whether the settings request scattering.
func (s VRScattering) Enabled() bool {
	return finite(s.Blend) && s.Blend > 0
}

// Normalized clamps the settings to their domains.
func (s VRScattering) Normalized() VRScattering {
	s.Blend = math.Min(2, math.Max(0, s.Blend))
	if !finite(s.Reach) || s.Reach <= 0 || s.Reach > 1 {
		s.Reach = 1
	}
	if !finite(s.Anisotropy) {
		s.Anisotropy = 0
	}
	s.Anisotropy = math.Max(-0.99, math.Min(0.99, s.Anisotropy))
	if !finite(s.LightAzimuth) {
		s.LightAzimuth = 0
	}
	if !finite(s.LightElevation) {
		s.LightElevation = 0
	}
	return s
}

// VRScatteringLight returns the unit direction toward the key light for a
// camera with the given forward, right and up vectors.
func VRScatteringLight(forward, right, up Vec3, azimuth, elevation float64) Vec3 {
	ce := math.Cos(elevation)
	return forward.Scale(-ce * math.Cos(azimuth)).
		Add(up.Scale(math.Sin(elevation))).
		Add(right.Scale(-ce * math.Sin(azimuth))).
		Normalize()
}

// HenyeyGreenstein is VTK's phase function: (1-g²)/(1+g²-2g·cos)^1.5, which
// is 1 for g = 0 (VTK does not divide by 4π).
func HenyeyGreenstein(g, cosAngle float64) float64 {
	if math.Abs(g) < 0.01 {
		return 1
	}
	g2 := g * g
	d := 1 + g2 - 2*g*cosAngle
	return (1 - g2) / (d * math.Sqrt(d))
}

// VRScatteringCoefficient is VTK's blend of the volumetric model into the
// surface model for blend b, gradient magnitude gradient (in normalized
// scalar units per voxel) and sample opacity alpha.
func VRScatteringCoefficient(b, gradient, alpha float64) float64 {
	e := math.Exp(-2 * b * gradient * alpha)
	if b < 1 {
		return 2 * b * e
	}
	return 2*(1-b)*e + 2*b - 1
}

// vrScatterFrame is the per-frame scattering state of the CPU renderer.
type vrScatterFrame struct {
	settings VRScattering
	toLight  Vec3 // box-centred world direction toward the light
	shadow   vrShadowVolume
	// gradientScale turns a central-difference index gradient into units of
	// the transfer-function domain per voxel.
	gradientScale float64
}

// vrShadowVolume is the transmittance toward the light at a coarse lattice
// of texture points (the CPU's precomputed light attenuation volume).
type vrShadowVolume struct {
	nx, ny, nz int
	t          []float32
}

// maxVRShadowLattice bounds the shadow volume per axis: shadows are soft, and
// 64³ shadow rays keep the settled frame's cost in the tens of milliseconds.
const maxVRShadowLattice = 64

// maxVRShadowSteps bounds the samples of one shadow ray.
const maxVRShadowSteps = 64

// vrShadowStep is the shadow-ray step for a primary step and reach (mm).
func vrShadowStep(stepLen, reachMM float64) float64 {
	return math.Max(stepLen, reachMM/maxVRShadowSteps)
}

// newVRScatterFrame precomputes the shadow volume, or returns nil when
// scattering is off for this frame.
func newVRScatterFrame(ctx context.Context, sampler *volumeSampler, lut VRLUT, preset VRPreset, window WindowLevel, dataMin, dataMax float64, shading bool, clip *VRClip, quality VRQuality, cam VRCamera, boxMin, size Vec3, stepLen float64) (*vrScatterFrame, error) {
	settings := preset.Scattering.Normalized()
	// The light attenuation follows the scalar transfer function, so a 2D
	// transfer function (#1232) casts no volumetric shadows.
	if !quality.Scattering || !shading || preset.Mode != VRModeDVR || !settings.Enabled() || preset.Transfer2D != nil {
		return nil, nil
	}
	right, up, forward := cam.basis()
	frame := &vrScatterFrame{
		settings: settings,
		toLight:  VRScatteringLight(forward, right, up, settings.LightAzimuth, settings.LightElevation),
	}
	// Gradients are measured in the transfer function's domain, the
	// normalized scalar of VTK: the window for normalized functions, the
	// value range for HU ones (the GPU LUT domain in both cases).
	lower, upper := dataMin, dataMax
	if preset.TF.Domain() == VRTransferDomainNormalized {
		lower, upper = VRWindowBounds(window)
	}
	if upper > lower {
		frame.gradientScale = 0.5 / (upper - lower)
	}
	vol := sampler.vol
	lattice := func(n int) int { return max(2, min(n, maxVRShadowLattice)) }
	s := &frame.shadow
	s.nx, s.ny, s.nz = lattice(vol.Cols), lattice(vol.Rows), lattice(vol.Depth)
	s.t = make([]float32, s.nx*s.ny*s.nz)
	reach := settings.Reach * size.Length()
	step := vrShadowStep(stepLen, reach)
	unit := preset.TF.OpacityUnitDistanceMM()
	labels := clip.activeLabels(vol)
	err := parallelRowsContext(ctx, s.nz, func(z int) {
		for y := 0; y < s.ny; y++ {
			for x := 0; x < s.nx; x++ {
				tex := Vec3{X: float64(x) / float64(s.nx-1), Y: float64(y) / float64(s.ny-1), Z: float64(z) / float64(s.nz-1)}
				origin := boxMin.Add(Vec3{X: tex.X * size.X, Y: tex.Y * size.Y, Z: tex.Z * size.Z})
				s.t[(z*s.ny+y)*s.nx+x] = float32(vrShadowTransmittance(sampler, lut, preset, window, dataMin, dataMax, clip, labels, origin, frame.toLight, step, reach, unit, boxMin, size))
			}
		}
	})
	return frame, err
}

// vrShadowTransmittance marches from origin toward the light, starting one
// step out, and multiplies (1 - opacity) of the transfer function's samples
// until the reach, the box or full occlusion. Clipped samples and, in
// only-inside mode, unlabelled ones do not occlude.
func vrShadowTransmittance(sampler *volumeSampler, lut VRLUT, preset VRPreset, window WindowLevel, dataMin, dataMax float64, clip *VRClip, labels *VRLabelMap, origin, toLight Vec3, step, reach, unit float64, boxMin, size Vec3) float64 {
	transmittance := 1.0
	for distance := step; distance <= reach; distance += step {
		point := origin.Add(toLight.Scale(distance))
		tex := Vec3{X: (point.X - boxMin.X) / size.X, Y: (point.Y - boxMin.Y) / size.Y, Z: (point.Z - boxMin.Z) / size.Z}
		if tex.X < 0 || tex.Y < 0 || tex.Z < 0 || tex.X > 1 || tex.Y > 1 || tex.Z > 1 {
			break
		}
		if !clip.keep(tex) || !labels.keeps(tex) {
			continue
		}
		value, ok := sampler.textureAt(tex)
		if !ok {
			continue
		}
		alpha := dvrTransferSample(preset.TF, lut, value, window, dataMin, dataMax).A
		transmittance *= 1 - CorrectVRAlpha(alpha, 1, step, unit)
		if transmittance < 1.0/255 {
			return 0
		}
	}
	return transmittance
}

// at is the trilinear transmittance at texture coordinate tex.
func (s *vrShadowVolume) at(tex Vec3) float64 {
	fx := clampUnit(tex.X) * float64(s.nx-1)
	fy := clampUnit(tex.Y) * float64(s.ny-1)
	fz := clampUnit(tex.Z) * float64(s.nz-1)
	x0, y0, z0 := min(int(fx), s.nx-2), min(int(fy), s.ny-2), min(int(fz), s.nz-2)
	ax, ay, az := fx-float64(x0), fy-float64(y0), fz-float64(z0)
	v := func(x, y, z int) float64 { return float64(s.t[(z*s.ny+y)*s.nx+x]) }
	lerp := func(a, b, t float64) float64 { return a + (b-a)*t }
	c00 := lerp(v(x0, y0, z0), v(x0+1, y0, z0), ax)
	c10 := lerp(v(x0, y0+1, z0), v(x0+1, y0+1, z0), ax)
	c01 := lerp(v(x0, y0, z0+1), v(x0+1, y0, z0+1), ax)
	c11 := lerp(v(x0, y0+1, z0+1), v(x0+1, y0+1, z0+1), ax)
	return lerp(lerp(c00, c10, ay), lerp(c01, c11, ay), az)
}

// shade blends the volumetric model into a surface-shaded sample: the
// secondary term is shadow·phase·colour·diffuse (the headlight's ambient
// colour is VTK's default black), weighted by VRScatteringCoefficient.
func (f *vrScatterFrame) shade(surfaceR, surfaceG, surfaceB, colorR, colorG, colorB, alpha float64, tex, rayDir, indexGradient Vec3, diffuse float64) (float64, float64, float64) {
	secondary := f.shadow.at(tex) * HenyeyGreenstein(f.settings.Anisotropy, f.toLight.Dot(rayDir.Normalize())) * diffuse
	coef := VRScatteringCoefficient(f.settings.Blend, indexGradient.Length()*f.gradientScale, alpha)
	keep := 1 - coef
	return keep*surfaceR + coef*secondary*colorR, keep*surfaceG + coef*secondary*colorG, keep*surfaceB + coef*secondary*colorB
}
