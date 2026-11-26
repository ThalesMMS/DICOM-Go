package render

import (
	"fmt"
	"math"
)

const MaxVRDirectionalLights = 3
const VRSpecularTableSamples = 4097

type VRToneMapping uint32

const (
	VRToneMappingClamp VRToneMapping = iota
	VRToneMappingReinhard
)

// VRDirectionalLight points towards an infinitely distant white source in
// camera coordinates (right, up, towards viewer). Direction must be unit length.
type VRDirectionalLight struct {
	DirectionCamera [3]float64
	Intensity       float64
}

// VRIlluminationState is a V14 DVR presentation contract. The zero value keeps
// the legacy two-sided headlight and clamp. Fixed arrays bound request size and
// make freezing independent of caller-owned slices. This is not a CPU raycaster.
type VRIlluminationState struct {
	LightCount     uint32
	Lights         [MaxVRDirectionalLights]VRDirectionalLight
	ToneMapping    VRToneMapping
	SpecularLookup bool
}

// VRVascularIllumination is an own neutral-white key/fill/back profile.
// It moves with the camera, applies Reinhard to scene radiance before adding
// the authored background. Specular lookup is a separate opt-in: the qualified
// Intel Vulkan profile currently evaluates pow faster than table reads.
func VRVascularIllumination() VRIlluminationState {
	unit := func(x, y, z float64) [3]float64 {
		n := math.Sqrt(x*x + y*y + z*z)
		return [3]float64{x / n, y / n, z / n}
	}
	return VRIlluminationState{LightCount: 3, ToneMapping: VRToneMappingReinhard,
		Lights: [MaxVRDirectionalLights]VRDirectionalLight{
			{DirectionCamera: unit(-0.5, 0.7, 1), Intensity: 0.7},
			{DirectionCamera: unit(0.7, -0.2, 1), Intensity: 0.2},
			{DirectionCamera: unit(0, 0.3, -1), Intensity: 0.1},
		}}
}

// VRSpecularTable returns samples of x^power on [0,1]. Linear interpolation's
// worst-case analytic error is <=128*127/(8*4096^2) for the admitted powers.
// Zero power is exactly one; fractional powers below one are refused because
// their endpoint derivative invalidates this error bound.
func VRSpecularTable(power float64) ([VRSpecularTableSamples]float32, error) {
	var values [VRSpecularTableSamples]float32
	if !finite(power) || power < 0 || power > 128 || (power > 0 && power < 1) {
		return values, fmt.Errorf("%w: specular table power must be zero or 1..128", ErrInvalidViewState)
	}
	for i := range values {
		values[i] = float32(math.Pow(float64(i)/float64(VRSpecularTableSamples-1), power))
	}
	return values, nil
}

func validateVRIllumination(s VRIlluminationState, material VRLightingMaterial) error {
	if s.LightCount > MaxVRDirectionalLights || s.ToneMapping > VRToneMappingReinhard {
		return fmt.Errorf("invalid illumination count/tone mapping")
	}
	total := 0.0
	for i, l := range s.Lights {
		if i >= int(s.LightCount) {
			if l != (VRDirectionalLight{}) {
				return fmt.Errorf("inactive light carries state")
			}
			continue
		}
		n := math.Hypot(math.Hypot(l.DirectionCamera[0], l.DirectionCamera[1]), l.DirectionCamera[2])
		if !finite(n) || math.Abs(n-1) > 1e-6 || !finite(l.Intensity) || l.Intensity <= 0 || l.Intensity > 4 {
			return fmt.Errorf("invalid directional light")
		}
		total += l.Intensity
	}
	if total > 4 || !material.valid() || material.Ambient > 16 || material.Diffuse > 16 || material.Specular > 16 {
		return fmt.Errorf("illumination material/intensity exceeds profile")
	}
	if s.SpecularLookup && (!finite(material.SpecularPower) || material.SpecularPower > 128 || (material.SpecularPower > 0 && material.SpecularPower < 1)) {
		return fmt.Errorf("unsupported specular lookup power")
	}
	return nil
}
