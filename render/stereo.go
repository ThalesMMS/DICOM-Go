package render

import (
	"fmt"
	"math"
)

type StereoMode uint32

const (
	StereoMono StereoMode = iota
	StereoSideBySide
	StereoRedCyan
)

// StereoPresentationState selects a binocular presentation. Distances are mm.
// Mono retains optional settings but never changes the central camera.
type StereoPresentationState struct {
	Mode            StereoMode
	EyeSeparationMM float64
	ConvergenceMM   float64
}

func (s StereoPresentationState) Validate() error {
	invalid := func() error { return fmt.Errorf("%w: invalid stereo presentation", ErrInvalidViewState) }
	if s.Mode > StereoRedCyan || !finiteScalar(s.EyeSeparationMM) || !finiteScalar(s.ConvergenceMM) || s.EyeSeparationMM < 0 || s.EyeSeparationMM > 1000 || s.ConvergenceMM < 0 || s.ConvergenceMM > 1000000 {
		return invalid()
	}
	if s.Mode != StereoMono && (s.ConvergenceMM == 0 || s.EyeSeparationMM > s.ConvergenceMM) {
		return invalid()
	}
	return nil
}

// StereoEyeViews contains immutable eye cameras for one central generation.
// The optical axes meet at ConvergenceLPS (symmetric toe-in). This geometry-only
// function does not sample volumes or compose images. It preserves clipping,
// physical sampling, lighting, labels and the central camera itself.
type StereoEyeViews struct {
	Left, Right    ViewState
	ConvergenceLPS [3]float64
}

func DeriveStereoEyeViews(center ViewState) (StereoEyeViews, error) {
	if err := ValidateViewState(center); err != nil {
		return StereoEyeViews{}, err
	}
	if center.Kind != ViewKindVR || center.VR.Stereo.Mode == StereoMono {
		return StereoEyeViews{}, fmt.Errorf("%w: stereo eye views require active VR stereo", ErrInvalidViewState)
	}
	config := center.VR.Stereo
	pos := Vec3{X: center.VR.PositionLPS[0], Y: center.VR.PositionLPS[1], Z: center.VR.PositionLPS[2]}
	focus := Vec3{X: center.VR.FocalPointLPS[0], Y: center.VR.FocalPointLPS[1], Z: center.VR.FocalPointLPS[2]}
	up := Vec3{X: center.VR.ViewUpLPS[0], Y: center.VR.ViewUpLPS[1], Z: center.VR.ViewUpLPS[2]}
	forward := focus.Sub(pos).Normalize()
	right := forward.Cross(up).Normalize()
	convergence := pos.Add(forward.Scale(config.ConvergenceMM))
	array := func(v Vec3) [3]float64 { return [3]float64{v.X, v.Y, v.Z} }
	result := StereoEyeViews{Left: center, Right: center, ConvergenceLPS: array(convergence)}
	if config.Mode == StereoSideBySide {
		if center.OutputWidth < 2 {
			return StereoEyeViews{}, fmt.Errorf("%w: stereo surface must be at least two pixels wide", ErrInvalidViewState)
		}
		result.Left.OutputWidth = (center.OutputWidth + 1) / 2
		result.Right.OutputWidth = result.Left.OutputWidth
	}
	result.Left.VR.Stereo = StereoPresentationState{}
	result.Right.VR.Stereo = StereoPresentationState{}
	if config.EyeSeparationMM != 0 {
		result.Left.VR.PositionLPS = array(pos.Add(right.Scale(-config.EyeSeparationMM / 2)))
		result.Right.VR.PositionLPS = array(pos.Add(right.Scale(config.EyeSeparationMM / 2)))
		result.Left.VR.FocalPointLPS = array(convergence)
		result.Right.VR.FocalPointLPS = array(convergence)
	}
	for _, eye := range []ViewState{result.Left, result.Right} {
		if err := ValidateViewState(eye); err != nil {
			return StereoEyeViews{}, err
		}
	}
	for _, p := range result.ConvergenceLPS {
		if math.IsNaN(p) || math.IsInf(p, 0) {
			return StereoEyeViews{}, fmt.Errorf("%w: stereo convergence overflow", ErrInvalidViewState)
		}
	}
	return result, nil
}
