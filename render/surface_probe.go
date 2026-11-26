package render

import (
	"fmt"
	"math"
	"strings"
)

type SurfaceProbeOperator uint32

const (
	SurfaceProbePoint SurfaceProbeOperator = iota
	SurfaceProbeMinimum
	SurfaceProbeMaximum
	SurfaceProbeMean
)

type SurfaceProbeRegistration uint32

const (
	SurfaceProbeSameFrame SurfaceProbeRegistration = iota
	SurfaceProbeExplicitAffine
)

const MaxSurfaceProbeSamples = 512
const MaxSurfaceProbeWork = 16_000_000

// SurfaceProbeState binds a surface in patient LPS to an independently owned
// scalar source. A band is measured in surface-patient millimetres BEFORE the
// registration affine. Reversing normals reverses signed offsets, not winding.
// Only source voxel centres and their closed interpolation domain are valid;
// outside samples are omitted. No valid sample is distinct from scalar zero.
type SurfaceProbeState struct {
	SourceGeneration                                          uint64
	SourceDimensions                                          [3]uint32
	SourceID, SourceFrameOfReference, SurfaceFrameOfReference string
	Registration                                              SurfaceProbeRegistration
	SurfaceToSourceLPS                                        GeometryAffine
	PatientToSourceIndex                                      GeometryAffine
	Unit                                                      string
	Scale, Offset                                             float64
	Operator                                                  SurfaceProbeOperator
	StartMM, EndMM, MaximumStepMM                             float64
	ReverseNormals                                            bool
	Interpolation                                             Interpolation
	Transfer                                                  TransferLUT
	InvalidColor                                              [3]float64
}

func (s SurfaceProbeState) Enabled() bool { return s.SourceGeneration != 0 }

func (s SurfaceProbeState) Clone() SurfaceProbeState {
	s.Transfer.Samples = append([]TransferSample(nil), s.Transfer.Samples...)
	return s
}

// SampleCount uses equally spaced samples INCLUDING both band endpoints.
// MaximumStepMM is an upper bound; the actual step is extent/(count-1).
func (s SurfaceProbeState) SampleCount() (uint32, error) {
	if s.Operator == SurfaceProbePoint {
		if s.StartMM != 0 || s.EndMM != 0 || s.MaximumStepMM != 0 {
			return 0, fmt.Errorf("%w: point probe carries band offsets", ErrInvalidViewState)
		}
		return 1, nil
	}
	if s.Operator > SurfaceProbeMean || !finite(s.StartMM) || !finite(s.EndMM) ||
		!finite(s.MaximumStepMM) || s.EndMM <= s.StartMM || s.MaximumStepMM <= 0 {
		return 0, fmt.Errorf("%w: invalid surface probe band", ErrInvalidViewState)
	}
	intervals := math.Ceil((s.EndMM - s.StartMM) / s.MaximumStepMM)
	if !finite(intervals) || intervals < 1 || intervals >= MaxSurfaceProbeSamples {
		return 0, fmt.Errorf("%w: surface probe sample limit", ErrInvalidViewState)
	}
	return uint32(intervals) + 1, nil
}

func (s SurfaceProbeState) Validate() error {
	fail := func(reason string) error { return fmt.Errorf("%w: surface probe: %s", ErrInvalidViewState, reason) }
	if !s.Enabled() {
		if s.SourceDimensions != [3]uint32{} || s.SourceID != "" || s.SourceFrameOfReference != "" || s.SurfaceFrameOfReference != "" ||
			s.Registration != 0 || s.SurfaceToSourceLPS != (GeometryAffine{}) || s.PatientToSourceIndex != (GeometryAffine{}) || s.Unit != "" || s.Scale != 0 || s.Offset != 0 || s.Operator != 0 || s.StartMM != 0 || s.EndMM != 0 || s.MaximumStepMM != 0 || s.ReverseNormals || s.Interpolation != 0 || s.Transfer.DomainMin != 0 || s.Transfer.DomainMax != 0 || len(s.Transfer.Samples) != 0 || s.InvalidColor != [3]float64{} {
			return fail("parameters without source generation")
		}
		return nil
	}
	for _, value := range []string{s.SourceID, s.SourceFrameOfReference, s.SurfaceFrameOfReference, s.Unit} {
		if strings.TrimSpace(value) == "" || len(value) > 256 || strings.ContainsAny(value, "\r\n\x00") {
			return fail("missing or invalid source identity, frame or unit")
		}
	}
	for _, dimension := range s.SourceDimensions {
		if dimension == 0 {
			return fail("empty source")
		}
	}
	if !finite(s.Scale) || s.Scale == 0 || !finite(s.Offset) {
		return fail("invalid quantitative scale")
	}
	identity := GeometryAffine{1, 0, 0, 0, 0, 1, 0, 0, 0, 0, 1, 0, 0, 0, 0, 1}
	if s.Registration == SurfaceProbeSameFrame {
		if s.SourceFrameOfReference != s.SurfaceFrameOfReference || s.SurfaceToSourceLPS != identity {
			return fail("same-frame registration requires matching frames and identity")
		}
	} else if s.Registration != SurfaceProbeExplicitAffine {
		return fail("unknown registration")
	}
	if _, ok := s.SurfaceToSourceLPS.Inverse(); !ok {
		return fail("invalid registration affine")
	}
	if _, ok := s.PatientToSourceIndex.Inverse(); !ok {
		return fail("invalid source index affine")
	}
	if s.Interpolation != InterpolationNearest && s.Interpolation != InterpolationLinear {
		return fail("unsupported interpolation")
	}
	if _, err := s.SampleCount(); err != nil {
		return err
	}
	if !finite(s.Transfer.DomainMin) || !finite(s.Transfer.DomainMax) || s.Transfer.DomainMin >= s.Transfer.DomainMax || len(s.Transfer.Samples) < 2 || len(s.Transfer.Samples) > MaxTransferLUTSamples {
		return fail("invalid color transfer")
	}
	for _, sample := range s.Transfer.Samples {
		if sample.A != 1 {
			return fail("surface probe color table must be opaque")
		}
		for _, value := range []float32{sample.R, sample.G, sample.B} {
			if !finite(float64(value)) || value < 0 || value > 1 {
				return fail("invalid color sample")
			}
		}
	}
	for _, value := range s.InvalidColor {
		if !finite(value) || value < 0 || value > 1 {
			return fail("invalid missing-sample color")
		}
	}
	return nil
}

// ValidateSurfaceProbeSource prevents a material from reinterpreting a different
// source generation, scalar layout, lattice or registration as the admitted one.
func ValidateSurfaceProbeSource(s SurfaceProbeState, d VolumeDescriptor) error {
	if err := s.Validate(); err != nil {
		return err
	}
	if err := ValidateVolumeDescriptor(d); err != nil {
		return err
	}
	if !s.Enabled() || d.Components != 1 || d.VolumeGeneration != s.SourceGeneration || d.Dimensions != s.SourceDimensions {
		return fmt.Errorf("%w: surface probe source identity/layout mismatch", ErrInvalidViewState)
	}
	for row := 0; row < 4; row++ {
		for col := 0; col < 4; col++ {
			want := 0.0
			for k := 0; k < 4; k++ {
				want += d.PatientLPSToIndex[row*4+k] * s.SurfaceToSourceLPS[k*4+col]
			}
			got := s.PatientToSourceIndex[row*4+col]
			if !finite(want) || math.Abs(got-want) > 1e-10*math.Max(1, math.Abs(want)) {
				return fmt.Errorf("%w: surface probe index transform does not match source and registration", ErrInvalidViewState)
			}
		}
	}
	return nil
}

// ValidateSurfaceProbeNormals admits band sampling only with complete unit
// normals and consistent orientation within every nondegenerate triangle.
// Uniformly inverted normals are valid and retain their signed interpretation.
func ValidateSurfaceProbeNormals(vertices, normals []Vec3, triangles [][3]uint32) error {
	fail := func() error {
		return fmt.Errorf("%w: surface probe requires complete consistent unit normals", ErrInvalidViewState)
	}
	if len(vertices) == 0 || len(normals) != len(vertices) || len(triangles) == 0 {
		return fail()
	}
	for i, n := range normals {
		v := vertices[i]
		if !finite(v.X) || !finite(v.Y) || !finite(v.Z) || !finite(n.X) || !finite(n.Y) || !finite(n.Z) || math.Abs(n.Dot(n)-1) > 1e-3 {
			return fail()
		}
	}
	for _, triangle := range triangles {
		for _, index := range triangle {
			if uint64(index) >= uint64(len(vertices)) {
				return fail()
			}
		}
		face := vertices[triangle[1]].Sub(vertices[triangle[0]]).Cross(vertices[triangle[2]].Sub(vertices[triangle[0]]))
		length := face.Length()
		if !finite(length) || length <= 1e-12 {
			return fail()
		}
		face = face.Scale(1 / length)
		sign := normals[triangle[0]].Dot(face)
		if math.Abs(sign) < 1e-4 {
			return fail()
		}
		for _, index := range triangle[1:] {
			projection := normals[index].Dot(face)
			if math.Abs(projection) < 1e-4 || projection*sign <= 0 {
				return fail()
			}
		}
	}
	return nil
}
