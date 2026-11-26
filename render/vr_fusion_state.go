package render

import "fmt"

// VRFusionState (V23) classifies a separately resident secondary volume in
// DVR. PatientToOverlayIndex includes registration. Samples are linear-light,
// non-premultiplied RGBA and travel beside the header, never as JSON arrays.
// The secondary opacity is scaled before its own physical step correction.
type VRFusionState struct {
	OverlayGeneration              uint64
	OverlayDimensions              [3]uint32
	PatientToOverlayIndex          GeometryAffine
	DomainMin, DomainMax           float64
	Opacity, OpacityUnitDistanceMM float64
	SampleCount                    uint32
	Samples                        []TransferSample `json:"-"`
}

func (s VRFusionState) Enabled() bool { return s.OverlayGeneration != 0 }

func (s VRFusionState) validateMetadata() error {
	fail := func(message string) error { return fmt.Errorf("%w: VR fusion %s", ErrInvalidViewState, message) }
	if !s.Enabled() {
		if s.OverlayDimensions != [3]uint32{} || s.PatientToOverlayIndex != (GeometryAffine{}) || s.DomainMin != 0 || s.DomainMax != 0 || s.Opacity != 0 || s.OpacityUnitDistanceMM != 0 || s.SampleCount != 0 {
			return fail("metadata without overlay")
		}
		return nil
	}
	for _, dimension := range s.OverlayDimensions {
		if dimension == 0 {
			return fail("empty overlay")
		}
	}
	for _, value := range s.PatientToOverlayIndex {
		if !finite(value) {
			return fail("non-finite registration")
		}
	}
	a := s.PatientToOverlayIndex
	if a[12] != 0 || a[13] != 0 || a[14] != 0 || a[15] != 1 {
		return fail("non-affine registration")
	}
	det := a[0]*(a[5]*a[10]-a[6]*a[9]) - a[1]*(a[4]*a[10]-a[6]*a[8]) + a[2]*(a[4]*a[9]-a[5]*a[8])
	if !finite(det) || det == 0 {
		return fail("singular registration")
	}
	if !finite(s.Opacity) || s.Opacity <= 0 || s.Opacity > 1 || !finite(s.OpacityUnitDistanceMM) || s.OpacityUnitDistanceMM <= 0 {
		return fail("invalid opacity or physical unit")
	}
	if !finite(s.DomainMin) || !finite(s.DomainMax) || s.DomainMax <= s.DomainMin || s.SampleCount < 2 || s.SampleCount > MaxTransferLUTSamples {
		return fail("invalid transfer domain or table")
	}
	return nil
}

func (s VRFusionState) PayloadBytes() (uint64, error) {
	if err := s.validateMetadata(); err != nil {
		return 0, err
	}
	return uint64(s.SampleCount) * 16, nil
}

func (s VRFusionState) Validate() error {
	if err := s.validateMetadata(); err != nil {
		return err
	}
	if len(s.Samples) != int(s.SampleCount) {
		return fmt.Errorf("%w: incomplete VR fusion transfer table", ErrInvalidViewState)
	}
	for _, sample := range s.Samples {
		for _, value := range [4]float32{sample.R, sample.G, sample.B, sample.A} {
			if !finite(float64(value)) || value < 0 || value > 1 {
				return fmt.Errorf("%w: invalid VR fusion transfer channel", ErrInvalidViewState)
			}
		}
	}
	return nil
}

// OverlaySource identifies the secondary source required by this view. It is
// shared by transports and budget planners for MPR and volume fusion.
func (s ViewState) OverlaySource() (uint64, [3]uint32) {
	if s.Kind == ViewKindVR && s.VR.SurfaceProbe.Enabled() {
		return s.VR.SurfaceProbe.SourceGeneration, s.VR.SurfaceProbe.SourceDimensions
	}
	if s.Kind == ViewKindVR && s.VR.Fusion.Enabled() {
		return s.VR.Fusion.OverlayGeneration, s.VR.Fusion.OverlayDimensions
	}
	if s.Kind == ViewKindMPR && s.MPR.Presentation.Mode == MPRPresentationFusion {
		return s.MPR.Presentation.Fusion.OverlayGeneration, s.MPR.Presentation.Fusion.OverlayDimensions
	}
	return 0, [3]uint32{}
}
