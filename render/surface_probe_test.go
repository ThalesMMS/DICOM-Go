package render

import (
	"errors"
	"math"
	"testing"
)

func testSurfaceProbeState() SurfaceProbeState {
	identity := GeometryAffine{1, 0, 0, 0, 0, 1, 0, 0, 0, 0, 1, 0, 0, 0, 0, 1}
	return SurfaceProbeState{SourceGeneration: 7, SourceDimensions: [3]uint32{5, 6, 7},
		SourceID: "analytic-ramp", SourceFrameOfReference: "1.2.3", SurfaceFrameOfReference: "1.2.3",
		SurfaceToSourceLPS: identity, PatientToSourceIndex: identity, Unit: "Bq/ml", Scale: 1,
		Interpolation: InterpolationLinear, Transfer: TransferLUT{DomainMin: 0, DomainMax: 100,
			Samples: []TransferSample{{B: 1, A: 1}, {R: 1, A: 1}}}, InvalidColor: [3]float64{1, 0, 1}}
}

func TestSurfaceProbeContractBoundsRegistrationAndOwnership(t *testing.T) {
	good := testSurfaceProbeState()
	if err := good.Validate(); err != nil {
		t.Fatal(err)
	}
	copy := good.Clone()
	copy.Transfer.Samples[0].B = .2
	if good.Transfer.Samples[0].B != 1 {
		t.Fatal("probe transfer aliases caller")
	}
	for name, mutate := range map[string]func(*SurfaceProbeState){
		"missing-source":    func(s *SurfaceProbeState) { s.SourceID = "" },
		"missing-unit":      func(s *SurfaceProbeState) { s.Unit = " " },
		"unknown-frame":     func(s *SurfaceProbeState) { s.SourceFrameOfReference = "" },
		"mismatched-frame":  func(s *SurfaceProbeState) { s.SurfaceFrameOfReference = "other" },
		"hidden-transform":  func(s *SurfaceProbeState) { s.SurfaceToSourceLPS[3] = 1 },
		"singular":          func(s *SurfaceProbeState) { s.PatientToSourceIndex[0] = 0 },
		"nonfinite":         func(s *SurfaceProbeState) { s.Offset = math.NaN() },
		"zero-scale":        func(s *SurfaceProbeState) { s.Scale = 0 },
		"point-band":        func(s *SurfaceProbeState) { s.EndMM = 1 },
		"cubic":             func(s *SurfaceProbeState) { s.Interpolation = Interpolation(999) },
		"translucent-table": func(s *SurfaceProbeState) { s.Transfer.Samples[0].A = .5 },
		"invalid-color":     func(s *SurfaceProbeState) { s.InvalidColor[2] = math.Inf(1) },
		"no-generation":     func(s *SurfaceProbeState) { s.SourceGeneration = 0 },
	} {
		t.Run(name, func(t *testing.T) {
			bad := good.Clone()
			mutate(&bad)
			if !errors.Is(bad.Validate(), ErrInvalidViewState) {
				t.Fatal("invalid probe admitted")
			}
		})
	}
	registered := good.Clone()
	registered.Registration = SurfaceProbeExplicitAffine
	registered.SurfaceFrameOfReference = "other"
	registered.SurfaceToSourceLPS[3] = 42
	registered.Scale = -2
	if err := registered.Validate(); err != nil {
		t.Fatal(err)
	}
}

func TestSurfaceProbeBandEndpointRuleAndLimits(t *testing.T) {
	s := testSurfaceProbeState()
	s.Operator = SurfaceProbeMean
	s.StartMM, s.EndMM, s.MaximumStepMM = -1, 2, .8
	count, err := s.SampleCount()
	if err != nil || count != 5 {
		t.Fatal(count, err)
	}
	for _, op := range []SurfaceProbeOperator{SurfaceProbeMinimum, SurfaceProbeMaximum, SurfaceProbeMean} {
		s.Operator = op
		if err := s.Validate(); err != nil {
			t.Fatal(err)
		}
	}
	s.EndMM = 510
	s.StartMM = 0
	s.MaximumStepMM = 1
	if count, err = s.SampleCount(); err != nil || count != 511 {
		t.Fatal(count, err)
	}
	s.EndMM = 511
	if count, err = s.SampleCount(); err != nil || count != 512 {
		t.Fatal(count, err)
	}
	s.EndMM = 512
	if _, err = s.SampleCount(); err == nil {
		t.Fatal("unbounded band admitted")
	}
	s.StartMM = -math.MaxFloat64
	s.EndMM = math.MaxFloat64
	if _, err = s.SampleCount(); err == nil {
		t.Fatal("overflowing extent admitted")
	}
}

func TestSurfaceProbeNormalsRejectMissingDegenerateAndMixedOrientation(t *testing.T) {
	v := []Vec3{{0, 0, 0}, {1, 0, 0}, {0, 1, 0}}
	triangles := [][3]uint32{{0, 1, 2}}
	n := []Vec3{{0, 0, 1}, {0, 0, 1}, {0, 0, 1}}
	if err := ValidateSurfaceProbeNormals(v, n, triangles); err != nil {
		t.Fatal(err)
	}
	for i := range n {
		n[i].Z = -1
	}
	if err := ValidateSurfaceProbeNormals(v, n, triangles); err != nil {
		t.Fatal("consistent inverted normals", err)
	}
	n[1].Z = 1
	if err := ValidateSurfaceProbeNormals(v, n, triangles); err == nil {
		t.Fatal("mixed orientation admitted")
	}
	if err := ValidateSurfaceProbeNormals(v, nil, triangles); err == nil {
		t.Fatal("missing normals admitted")
	}
	n[1].Z = -1
	n[0].Z = -2
	if err := ValidateSurfaceProbeNormals(v, n, triangles); err == nil {
		t.Fatal("nonunit normal admitted")
	}
	n[0].Z = -1
	v[2] = v[1]
	if err := ValidateSurfaceProbeNormals(v, n, triangles); err == nil {
		t.Fatal("degenerate triangle admitted")
	}
}

func TestSurfaceProbeViewVersionFreezeAndSourceTransform(t *testing.T) {
	d := testVolumeDescriptor(2, 2, 2)
	s := testSurfaceProbeState()
	s.SourceGeneration = d.VolumeGeneration
	s.SourceDimensions = d.Dimensions
	s.PatientToSourceIndex = d.PatientLPSToIndex
	if err := ValidateSurfaceProbeSource(s, d); err != nil {
		t.Fatal(err)
	}
	bad := s.Clone()
	bad.PatientToSourceIndex[3] += 1
	if err := ValidateSurfaceProbeSource(bad, d); err == nil {
		t.Fatal("misregistered source admitted")
	}
	v := validVRViewState()
	v.VR.OpacityUnitDistanceMM = 1
	v.VR.BackgroundRGBA = [4]float64{0, 0, 0, 1}
	v.VR.ImageSampleDistance = 1
	v.ContractVersion, v.StructSize = ViewStateContractVersionV27, ViewStateHeaderSizeV27
	v.VR.SurfaceProbe = s
	if err := ValidateViewState(v); err != nil {
		t.Fatal(err)
	}
	frozen, err := FreezeViewState(v)
	if err != nil {
		t.Fatal(err)
	}
	v.VR.SurfaceProbe.Transfer.Samples[0].B = .2
	if frozen.VR.SurfaceProbe.Transfer.Samples[0].B != 1 {
		t.Fatal("probe table crossed request boundary by alias")
	}
	if generation, dimensions := frozen.OverlaySource(); generation != d.VolumeGeneration || dimensions != d.Dimensions {
		t.Fatal("secondary identity lost", generation, dimensions)
	}
	v.ContractVersion, v.StructSize = ViewStateContractVersionV26, ViewStateHeaderSizeV26
	if err := ValidateViewState(v); err == nil {
		t.Fatal("old view version admitted probe")
	}
	v = frozen
	v.Kind = ViewKindMPR
	if err := ValidateViewState(v); err == nil {
		t.Fatal("probe accepted outside VR")
	}
}
