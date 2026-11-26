package render

import (
	"errors"
	"fmt"
	"math"
)

// Gradient opacity as a transfer function (#1010), after VTK's
// vtkVolumeProperty gradient opacity: a piecewise-linear function of the
// gradient magnitude, clamped outside its points, multiplies the scalar
// transfer function's opacity. The magnitude is covariant, in modality units
// per millimetre (HU/mm for CT): the central difference in index space is
// converted by the volume's spacing and orientation, as shading already does,
// so the same function reads the same on thin and thick slices. Written from
// VTK's documented behaviour; no VTK code is copied.

// MaxVRGradientOpacityPoints bounds a gradient opacity function.
const MaxVRGradientOpacityPoints = 32

// ErrInvalidVRGradientOpacity reports a malformed gradient opacity function.
var ErrInvalidVRGradientOpacity = errors.New("render: invalid VR gradient opacity function")

// VRGradientOpacityPoint maps a gradient magnitude, in modality units per mm,
// to an opacity factor in [0, 1].
type VRGradientOpacityPoint struct {
	Magnitude float64
	Opacity   float64
}

// VRGradientOpacity is a piecewise-linear gradient opacity function with
// strictly increasing magnitudes; nil disables it.
type VRGradientOpacity []VRGradientOpacityPoint

// Validate checks the points: at most MaxVRGradientOpacityPoints, finite
// non-negative magnitudes in strictly increasing order and opacities in
// [0, 1].
func (f VRGradientOpacity) Validate() error {
	if len(f) > MaxVRGradientOpacityPoints {
		return fmt.Errorf("%w: %d points, at most %d", ErrInvalidVRGradientOpacity, len(f), MaxVRGradientOpacityPoints)
	}
	for i, point := range f {
		if !finite(point.Magnitude) || point.Magnitude < 0 || !finite(point.Opacity) || point.Opacity < 0 || point.Opacity > 1 {
			return fmt.Errorf("%w: point %d %+v", ErrInvalidVRGradientOpacity, i, point)
		}
		if i > 0 && point.Magnitude <= f[i-1].Magnitude {
			return fmt.Errorf("%w: magnitudes not increasing at point %d", ErrInvalidVRGradientOpacity, i)
		}
	}
	return nil
}

// At evaluates the function at a gradient magnitude: linear between points,
// the first and last opacities outside them. An empty function is 1.
func (f VRGradientOpacity) At(magnitude float64) float64 {
	if len(f) == 0 {
		return 1
	}
	if magnitude <= f[0].Magnitude || math.IsNaN(magnitude) {
		return f[0].Opacity
	}
	for i := 1; i < len(f); i++ {
		if magnitude <= f[i].Magnitude {
			previous := f[i-1]
			t := (magnitude - previous.Magnitude) / (f[i].Magnitude - previous.Magnitude)
			return previous.Opacity + t*(f[i].Opacity-previous.Opacity)
		}
	}
	return f[len(f)-1].Opacity
}

// VRGradientOpacityRamp is the usual edge-enhancing function: transparent up
// to low HU/mm, opaque from high HU/mm, linear between.
func VRGradientOpacityRamp(low, high float64) VRGradientOpacity {
	return VRGradientOpacity{{Magnitude: low, Opacity: 0}, {Magnitude: high, Opacity: 1}}
}

// physicalGradientMagnitude converts a central difference taken one voxel
// either side in index space (not yet halved) to the covariant magnitude in
// modality units per mm.
func (v *Volume) physicalGradientMagnitude(indexGradient Vec3) float64 {
	if v == nil {
		return 0
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
	return physical.Length()
}

// usesGradientOpacity reports whether the preset modulates opacity by the
// gradient, by its function or its legacy scale.
func (preset VRPreset) usesGradientOpacity() bool {
	return len(preset.GradientOpacity) != 0 || preset.GradientOpacityScale > 0
}

// gradientOpacityFactor is the opacity factor of a sample's index gradient:
// the function of the covariant magnitude when the preset has one, else the
// legacy scale's clamp(|g| / scale) in its historical units (which keep the
// clinical presets rendering as before).
func (preset VRPreset) gradientOpacityFactor(sampler *volumeSampler, indexGradient Vec3) float64 {
	if len(preset.GradientOpacity) != 0 {
		return preset.GradientOpacity.At(sampler.vol.physicalGradientMagnitude(indexGradient))
	}
	if preset.GradientOpacityScale > 0 {
		return clampUnit(sampler.gradientOpacityVector(indexGradient).Length() / preset.GradientOpacityScale)
	}
	return 1
}
