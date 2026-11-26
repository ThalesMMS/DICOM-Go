package render

import (
	"bytes"
	"context"
	"encoding/binary"
	"encoding/json"
	"errors"
	"io"
	"math"
	"testing"
)

func TestFreezeViewStateOwnsVariableMembers(t *testing.T) {
	state := validVRViewState()
	state.ContractVersion = ViewStateContractVersionV2
	state.StructSize = ViewStateHeaderSizeV2
	state.VR.CutMask = VoxelMask{
		Dimensions: [3]uint32{2, 2, 1},
		Words:      []uint32{0b1001},
	}
	frozen, err := FreezeViewState(state)
	if err != nil {
		t.Fatal(err)
	}
	state.VR.ClippingPlanesLPS[0][0] = 9
	state.VR.TransferLUT.Samples[0].R = 1
	state.VR.CutMask.Words[0] = 0
	if frozen.VR.ClippingPlanesLPS[0][0] != 1 {
		t.Fatal("frozen clipping planes alias caller memory")
	}
	if frozen.VR.TransferLUT.Samples[0].R != 0 {
		t.Fatal("frozen transfer LUT aliases caller memory")
	}
	if frozen.VR.CutMask.Words[0] != 0b1001 {
		t.Fatal("frozen cut mask aliases caller memory")
	}
}

func TestValidateViewStateV2AcceptsEndoscopyCropAndCut(t *testing.T) {
	state := validVRViewState()
	state.ContractVersion = ViewStateContractVersionV2
	state.StructSize = ViewStateHeaderSizeV2
	state.VR.Projection = ProjectionEndoscopy
	state.VR.Crop = NormalizedCropBox{
		Enabled: 1,
		Min:     [3]float64{0.1, 0.2, 0.3},
		Max:     [3]float64{0.9, 0.8, 0.7},
	}
	state.VR.CutMask = VoxelMask{
		Dimensions: [3]uint32{2, 2, 1},
		Words:      []uint32{0b1001},
	}
	if err := ValidateViewState(state); err != nil {
		t.Fatalf("valid V2 VR state rejected: %v", err)
	}
}

func TestValidateViewStateV3RequiresPhysicalOpacityUnitDistance(t *testing.T) {
	state := validVRViewState()
	state.ContractVersion = ViewStateContractVersionV3
	state.StructSize = ViewStateHeaderSizeV3
	state.VR.OpacityUnitDistanceMM = 1
	if err := ValidateViewState(state); err != nil {
		t.Fatalf("valid V3 VR state rejected: %v", err)
	}
	state.VR.OpacityUnitDistanceMM = 0
	if err := ValidateViewState(state); !errors.Is(err, ErrInvalidViewState) {
		t.Fatalf("V3 zero opacity unit error = %v", err)
	}

	legacy := validVRViewState()
	if err := ValidateViewState(legacy); err != nil {
		t.Fatalf("V1 zero opacity unit compatibility rejected: %v", err)
	}
	if got := legacy.VR.EffectiveOpacityUnitDistanceMM(); got != DefaultVROpacityUnitDistanceMM {
		t.Fatalf("legacy opacity unit fallback = %v", got)
	}
}

func TestValidateViewStateV4RequiresFrozenOpaqueBackground(t *testing.T) {
	state := validVRViewState()
	state.ContractVersion = ViewStateContractVersionV4
	state.StructSize = ViewStateHeaderSizeV4
	state.VR.OpacityUnitDistanceMM = 1
	state.VR.BackgroundRGBA = [4]float64{0.1, 0.2, 0.3, 1}
	if err := ValidateViewState(state); err != nil {
		t.Fatalf("valid V4 VR state rejected: %v", err)
	}

	for _, background := range [][4]float64{
		{},
		{0, 0, 0, 0.5},
		{-0.1, 0, 0, 1},
		{0, 0, 1.1, 1},
		{math.NaN(), 0, 0, 1},
	} {
		state.VR.BackgroundRGBA = background
		if err := ValidateViewState(state); !errors.Is(err, ErrInvalidViewState) {
			t.Fatalf("V4 background %v error = %v", background, err)
		}
	}

	legacy := validVRViewState()
	legacy.VR.BackgroundRGBA = [4]float64{0, 0, 0, 1}
	if err := ValidateViewState(legacy); !errors.Is(err, ErrInvalidViewState) {
		t.Fatalf("V1 accepted unversioned background: %v", err)
	}
}

func TestValidateViewStateV5Jitter(t *testing.T) {
	state := validVRViewState()
	state.ContractVersion = ViewStateContractVersionV5
	state.StructSize = ViewStateHeaderSizeV5
	state.VR.OpacityUnitDistanceMM = 1
	state.VR.BackgroundRGBA = [4]float64{0, 0, 0, 1}
	for _, jitter := range []uint32{0, 1} {
		state.VR.JitterEnabled = jitter
		if err := ValidateViewState(state); err != nil {
			t.Fatalf("valid V5 VR state (jitter %d) rejected: %v", jitter, err)
		}
	}
	state.VR.JitterEnabled = 2
	if err := ValidateViewState(state); !errors.Is(err, ErrInvalidViewState) {
		t.Fatalf("V5 jitter flag 2 error = %v", err)
	}
	state.StructSize = ViewStateHeaderSizeV4
	state.VR.JitterEnabled = 0
	if err := ValidateViewState(state); !errors.Is(err, ErrInvalidViewState) {
		t.Fatalf("V5 with a V4 header size error = %v", err)
	}

	v4 := state
	v4.ContractVersion, v4.StructSize = ViewStateContractVersionV4, ViewStateHeaderSizeV4
	v4.VR.JitterEnabled = 1
	if err := ValidateViewState(v4); !errors.Is(err, ErrInvalidViewState) {
		t.Fatalf("V4 accepted jitter: %v", err)
	}
}

func TestValidateViewStateV1RejectsV2Fields(t *testing.T) {
	state := validVRViewState()
	state.ContractVersion = ViewStateContractVersionV1
	state.StructSize = ViewStateHeaderSizeV1
	state.VR.Projection = ProjectionEndoscopy
	if err := ValidateViewState(state); !errors.Is(err, ErrInvalidViewState) {
		t.Fatalf("V1 endoscopy error = %v", err)
	}
}

func TestValidateViewStateV2RejectsMalformedCropAndCut(t *testing.T) {
	state := validVRViewState()
	state.ContractVersion = ViewStateContractVersionV2
	state.StructSize = ViewStateHeaderSizeV2
	state.VR.Crop = NormalizedCropBox{Enabled: 1, Min: [3]float64{0.8, 0, 0}, Max: [3]float64{0.2, 1, 1}}
	if err := ValidateViewState(state); !errors.Is(err, ErrInvalidViewState) {
		t.Fatalf("inverted crop error = %v", err)
	}

	state.VR.Crop = NormalizedCropBox{}
	state.VR.CutMask = VoxelMask{Dimensions: [3]uint32{65, 1, 1}, Words: []uint32{1}}
	if err := ValidateViewState(state); !errors.Is(err, ErrInvalidViewState) {
		t.Fatalf("short cut mask error = %v", err)
	}

	state.VR.CutMask = VoxelMask{Dimensions: [3]uint32{math.MaxUint32, math.MaxUint32, 1}}
	if err := ValidateViewState(state); !errors.Is(err, ErrInvalidViewState) {
		t.Fatalf("boundary cut mask error = %v", err)
	}
}

func TestValidateViewStateBoundsAndCubic(t *testing.T) {
	state := validMPRViewState()
	state.MPR.Interpolation = InterpolationCubic
	if err := ValidateViewState(state); err != nil {
		t.Fatalf("cubic MPR rejected: %v", err)
	}
	state.OutputWidth = MaxViewOutputDimension + 1
	if err := ValidateViewState(state); !errors.Is(err, ErrInvalidViewState) {
		t.Fatalf("oversized output error = %v", err)
	}
}

func TestValidateVRGradientOpacityScale(t *testing.T) {
	state := validVRViewState()
	state.VR.GradientOpacityScale = 220
	if err := ValidateViewState(state); err != nil {
		t.Fatalf("gradient-opacity scale rejected: %v", err)
	}
	for _, invalid := range []float64{-1, math.NaN(), math.Inf(1)} {
		state.VR.GradientOpacityScale = invalid
		if err := ValidateViewState(state); !errors.Is(err, ErrInvalidViewState) {
			t.Fatalf("gradient-opacity scale %v error = %v", invalid, err)
		}
	}
}

func TestValidateViewStateRejectsFiniteInputsWithNonFiniteDerivedGeometry(t *testing.T) {
	mpr := validMPRViewState()
	mpr.MPR.PixelStepULPS = [3]float64{math.MaxFloat64, math.MaxFloat64, 0}
	mpr.MPR.PixelStepVLPS = [3]float64{math.MaxFloat64, -math.MaxFloat64, 0}
	mpr.OutputWidth, mpr.OutputHeight = 1, 1
	if err := ValidateViewState(mpr); err != nil {
		t.Fatalf("robustly normalizable single-pixel MPR basis rejected: %v", err)
	}
	mpr.OutputWidth, mpr.OutputHeight = 2, 2
	if err := ValidateViewState(mpr); !errors.Is(err, ErrInvalidViewState) {
		t.Fatalf("overflowing MPR derived extent error = %v", err)
	}

	mpr = validMPRViewState()
	mpr.OutputWidth, mpr.OutputHeight = 3, 1
	mpr.MPR.PixelOriginLPS[0] = math.Pow(2, 53) - 1
	if err := ValidateViewState(mpr); !errors.Is(err, ErrInvalidViewState) {
		t.Fatalf("last-pair float64 MPR collapse error = %v", err)
	}

	vr := validVRViewState()
	vr.VR.PositionLPS = [3]float64{-math.MaxFloat64, 0, 0}
	vr.VR.FocalPointLPS = [3]float64{math.MaxFloat64, 0, 0}
	if err := ValidateViewState(vr); !errors.Is(err, ErrInvalidViewState) {
		t.Fatalf("overflowing camera subtraction error = %v", err)
	}

	vr = validVRViewState()
	vr.VR.PositionLPS = [3]float64{}
	vr.VR.FocalPointLPS = [3]float64{1, 1, 1}
	vr.VR.ViewUpLPS = [3]float64{math.MaxFloat64, -math.MaxFloat64, math.MaxFloat64}
	if err := ValidateViewState(vr); err != nil {
		t.Fatalf("robustly normalizable huge view-up rejected: %v", err)
	}
}

func TestValidateAffinePairRejectsNonFiniteIntermediateProducts(t *testing.T) {
	const huge = math.MaxFloat64
	forward := GeometryAffine{
		huge, huge, huge, 0,
		huge, huge, huge, 0,
		huge, huge, huge, 0,
		0, 0, 0, 1,
	}
	inverse := GeometryAffine{
		huge, -huge, huge, 0,
		huge, -huge, huge, 0,
		huge, -huge, huge, 0,
		0, 0, 0, 1,
	}
	if err := validateAffinePair(forward, inverse); err == nil {
		t.Fatal("affine pair with non-finite intermediate products was accepted")
	}
}

func TestValidateFrameOutputRejectsCapacityBeyondSliceAndEmptyBackend(t *testing.T) {
	state := validMPRViewState()
	output := FrameOutput{
		ContractVersion:        FrameOutputContractVersion,
		StructSize:             FrameOutputHeaderSizeV1,
		Format:                 FrameFormatGrayF32TopLeft,
		Width:                  2,
		Height:                 2,
		StrideBytes:            8,
		CapacityBytes:          16,
		WrittenBytes:           16,
		VolumeGeneration:       state.VolumeGeneration,
		ViewGeneration:         state.ViewGeneration,
		PresentationGeneration: state.PresentationGeneration,
		BackendID:              "cpu-test",
		Pixels:                 make([]byte, 16),
	}
	if err := ValidateFrameOutput(output, state); err != nil {
		t.Fatalf("valid output rejected: %v", err)
	}
	output.CapacityBytes = 17
	if err := ValidateFrameOutput(output, state); err == nil {
		t.Fatal("capacity larger than slice accepted")
	}
	output.CapacityBytes = 16
	output.BackendID = ""
	if err := ValidateFrameOutput(output, state); err == nil {
		t.Fatal("empty backend accepted")
	}
	output.BackendID = "cpu-test"
	output.Width = MaxViewOutputDimension + 1
	if err := ValidateFrameOutput(output, ViewState{}); err == nil {
		t.Fatal("oversized backend-neutral frame accepted")
	}
	output.Width = 2
	output.BackendID = string(make([]byte, MaxFrameBackendIDBytes+1))
	if err := ValidateFrameOutput(output, ViewState{}); err == nil {
		t.Fatal("oversized backend id accepted")
	}
}

func TestFrameOutputV1HeaderSizeIncludesWarningBits(t *testing.T) {
	const frozenFrameOutputV1Bytes uint32 = 96
	if FrameOutputHeaderSizeV1 != frozenFrameOutputV1Bytes {
		t.Fatalf(
			"FrameOutputHeaderSizeV1 = %d, want frozen C++/Go V1 size %d",
			FrameOutputHeaderSizeV1,
			frozenFrameOutputV1Bytes,
		)
	}

	state := validMPRViewState()
	output := FrameOutput{
		ContractVersion:        FrameOutputContractVersion,
		StructSize:             FrameOutputHeaderSizeV1 - 1,
		Format:                 FrameFormatGrayF32TopLeft,
		Width:                  state.OutputWidth,
		Height:                 state.OutputHeight,
		StrideBytes:            uint64(state.OutputWidth) * 4,
		CapacityBytes:          uint64(state.OutputWidth) * uint64(state.OutputHeight) * 4,
		WrittenBytes:           uint64(state.OutputWidth) * uint64(state.OutputHeight) * 4,
		VolumeGeneration:       state.VolumeGeneration,
		ViewGeneration:         state.ViewGeneration,
		PresentationGeneration: state.PresentationGeneration,
		WarningBits:            FrameWarningHostRenderTiming,
		BackendID:              "header-size-regression",
		Pixels:                 make([]byte, int(state.OutputWidth*state.OutputHeight*4)),
	}
	if err := ValidateFrameOutput(output, state); err == nil {
		t.Fatal("FrameOutput accepted a V1 header that ends before WarningBits")
	}
}

func TestWriteModalityF32ContextStripsPaddingAndRescales(t *testing.T) {
	descriptor := testVolumeDescriptor(2, 2, 1)
	descriptor.ScalarFormat = VolumeScalarI16StoredLE
	descriptor.SampleDomain = VolumeSampleDomainStored
	descriptor.RescaleSlope = 2
	descriptor.RescaleIntercept = 10
	descriptor.RowStrideBytes = 8
	descriptor.SliceStrideBytes = 16
	descriptor.ByteLength = 16
	payload := make([]byte, 16)
	binary.LittleEndian.PutUint16(payload[0:], 1)
	binary.LittleEndian.PutUint16(payload[2:], 2)
	binary.LittleEndian.PutUint16(payload[8:], 3)
	binary.LittleEndian.PutUint16(payload[10:], 4)
	store := NewVolumeStore()
	t.Cleanup(func() { _ = store.Close() })
	if _, err := store.Replace(VolumeInput{Descriptor: descriptor, Payload: payload}); err != nil {
		t.Fatal(err)
	}
	lease, err := store.AcquireCurrent()
	if err != nil {
		t.Fatal(err)
	}
	defer lease.Release() //nolint:errcheck
	snapshot, err := lease.Snapshot()
	if err != nil {
		t.Fatal(err)
	}
	var output byteCollector
	if err := snapshot.WriteModalityF32Context(context.Background(), &output); err != nil {
		t.Fatal(err)
	}
	if len(output.data) != 16 {
		t.Fatalf("packed bytes = %d, want 16", len(output.data))
	}
	for index, want := range []float32{12, 14, 16, 18} {
		got := math.Float32frombits(binary.LittleEndian.Uint32(output.data[index*4:]))
		if got != want {
			t.Fatalf("sample %d = %v, want %v", index, got, want)
		}
	}
}

func TestWriteModalityF32ContextShortWriterAndCancellation(t *testing.T) {
	store := NewVolumeStore()
	t.Cleanup(func() { _ = store.Close() })
	descriptor := testVolumeDescriptor(8, 4, 2)
	if _, err := store.ReplaceFloat32(descriptor, make([]float32, 8*4*2)); err != nil {
		t.Fatal(err)
	}
	lease, err := store.AcquireCurrent()
	if err != nil {
		t.Fatal(err)
	}
	defer lease.Release() //nolint:errcheck
	snapshot, _ := lease.Snapshot()
	if err := snapshot.WriteModalityF32To(shortWriter{}); !errors.Is(err, io.ErrShortWrite) {
		t.Fatalf("short writer error = %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	writer := &cancelWriter{cancel: cancel}
	if err := snapshot.WriteModalityF32Context(ctx, writer); !errors.Is(err, context.Canceled) {
		t.Fatalf("cancelled stream error = %v", err)
	}
	if writer.writes != 1 {
		t.Fatalf("writes after cancellation = %d, want 1", writer.writes)
	}
}

func TestValidateModalityF32ConversionRejectsStoredRescaleOverflow(t *testing.T) {
	descriptor := testVolumeDescriptor(1, 1, 1)
	descriptor.ScalarFormat = VolumeScalarI16StoredLE
	descriptor.SampleDomain = VolumeSampleDomainStored
	descriptor.RowStrideBytes = 2
	descriptor.SliceStrideBytes = 2
	descriptor.ByteLength = 2
	descriptor.RescaleSlope = math.MaxFloat64
	if err := ValidateModalityF32Conversion(descriptor); !errors.Is(err, ErrInvalidVolumeSnapshot) {
		t.Fatalf("overflow conversion error = %v", err)
	}
}

func TestWriteModalityF32StreamsRawNonFiniteF32ForWorkerValidation(t *testing.T) {
	for _, test := range []struct {
		name    string
		replace func(*VolumeStore, VolumeDescriptor) error
	}{
		{
			name: "float32-payload",
			replace: func(store *VolumeStore, descriptor VolumeDescriptor) error {
				_, err := store.ReplaceFloat32(descriptor, []float32{float32(math.NaN())})
				return err
			},
		},
		{
			name: "byte-payload",
			replace: func(store *VolumeStore, descriptor VolumeDescriptor) error {
				payload := make([]byte, 4)
				binary.LittleEndian.PutUint32(payload, math.Float32bits(float32(math.Inf(1))))
				_, err := store.Replace(VolumeInput{Descriptor: descriptor, Payload: payload})
				return err
			},
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			store := NewVolumeStore()
			t.Cleanup(func() { _ = store.Close() })
			descriptor := testVolumeDescriptor(1, 1, 1)
			if err := test.replace(store, descriptor); err != nil {
				t.Fatal(err)
			}
			lease, err := store.AcquireCurrent()
			if err != nil {
				t.Fatal(err)
			}
			defer lease.Release() //nolint:errcheck
			snapshot, _ := lease.Snapshot()
			var output byteCollector
			if err := snapshot.WriteModalityF32To(&output); err != nil {
				t.Fatal(err)
			}
			value := math.Float32frombits(binary.LittleEndian.Uint32(output.data))
			if !math.IsNaN(float64(value)) && !math.IsInf(float64(value), 0) {
				t.Fatalf("raw non-finite payload became %v", value)
			}
		})
	}
}

func BenchmarkWriteModalityF32Streaming512x512x32(b *testing.B) {
	store := NewVolumeStore()
	defer store.Close() //nolint:errcheck
	descriptor := testVolumeDescriptor(512, 512, 32)
	if _, err := store.ReplaceFloat32(descriptor, make([]float32, 512*512*32)); err != nil {
		b.Fatal(err)
	}
	lease, _ := store.AcquireCurrent()
	defer lease.Release() //nolint:errcheck
	snapshot, _ := lease.Snapshot()
	b.SetBytes(512 * 512 * 32 * 4)
	b.ReportAllocs()
	b.ResetTimer()
	for iteration := 0; iteration < b.N; iteration++ {
		if err := snapshot.WriteModalityF32To(io.Discard); err != nil {
			b.Fatal(err)
		}
	}
}

type byteCollector struct{ data []byte }

func (w *byteCollector) Write(data []byte) (int, error) {
	w.data = append(w.data, data...)
	return len(data), nil
}

type shortWriter struct{}

func (shortWriter) Write(data []byte) (int, error) { return len(data) - 1, nil }

type cancelWriter struct {
	cancel context.CancelFunc
	writes int
}

func (w *cancelWriter) Write(data []byte) (int, error) {
	w.writes++
	w.cancel()
	return len(data), nil
}

func validMPRViewState() ViewState {
	return ViewState{
		ContractVersion:        ViewStateContractVersion,
		StructSize:             ViewStateHeaderSizeV1,
		Kind:                   ViewKindMPR,
		VolumeGeneration:       1,
		ViewGeneration:         1,
		PresentationGeneration: 1,
		OutputWidth:            2,
		OutputHeight:           2,
		MPR: MPRViewState{
			PixelStepULPS: [3]float64{1, 0, 0},
			PixelStepVLPS: [3]float64{0, 1, 0},
			Interpolation: InterpolationLinear,
			SlabMode:      SlabNone,
		},
	}
}

func validVRViewState() ViewState {
	return ViewState{
		ContractVersion:        ViewStateContractVersion,
		StructSize:             ViewStateHeaderSizeV1,
		Kind:                   ViewKindVR,
		VolumeGeneration:       1,
		ViewGeneration:         1,
		PresentationGeneration: 1,
		OutputWidth:            2,
		OutputHeight:           2,
		VR: VRViewState{
			PositionLPS:         [3]float64{0, 0, -10},
			FocalPointLPS:       [3]float64{0, 0, 0},
			ViewUpLPS:           [3]float64{0, 1, 0},
			Projection:          ProjectionPerspective,
			VerticalFOVRadians:  math.Pi / 4,
			NearMM:              0.1,
			FarMM:               100,
			Mode:                VRModeDVR,
			SampleDistanceMM:    1,
			ImageSampleDistance: 1,
			Ambient:             0.2,
			Diffuse:             0.7,
			Specular:            0.1,
			SpecularPower:       8,
			ClippingPlanesLPS:   []ClippingPlaneLPS{{1, 0, 0, 0}},
			TransferLUT: TransferLUT{
				DomainMin: -1000,
				DomainMax: 3000,
				Samples:   []TransferSample{{}, {R: 1, G: 1, B: 1, A: 1}},
			},
		},
	}
}

func TestValidateViewStateV6Labels(t *testing.T) {
	m, err := NewVRLabelMap(2, 2, 2, []uint16{0, 1, 2, 0, 0, 0, 300, 0},
		[]VRLabelStyle{{}, {Color: RGBA{R: 1}, Opacity: 1, Blend: 1, Visible: true}}, true)
	if err != nil {
		t.Fatal(err)
	}
	state := validVRViewState()
	state.ContractVersion = ViewStateContractVersionV6
	state.StructSize = ViewStateHeaderSizeV6
	state.VR.OpacityUnitDistanceMM = 1
	state.VR.BackgroundRGBA = [4]float64{0, 0, 0, 1}
	state.VR.Labels = VRLabelStateFromMap(m, 7)
	if state.VR.Labels.Bits != 16 || state.VR.Labels.OnlyInside != 1 || len(state.VR.Labels.Styles) != 2 || state.VR.Labels.Styles[1].Visible != 1 {
		t.Fatalf("label state = %+v", state.VR.Labels)
	}
	frozen, err := FreezeViewState(state)
	if err != nil {
		t.Fatalf("valid V6 labels rejected: %v", err)
	}
	state.VR.Labels.Styles[1].Opacity = 0.5
	if frozen.VR.Labels.Styles[1].Opacity != 1 {
		t.Fatal("FreezeViewState shares the label styles")
	}
	for name, mutate := range map[string]func(*ViewState){
		"V5": func(s *ViewState) {
			s.ContractVersion, s.StructSize = ViewStateContractVersionV5, ViewStateHeaderSizeV5
		},
		"bits":       func(s *ViewState) { s.VR.Labels.Bits = 12 },
		"8-bit data": func(s *ViewState) { s.VR.Labels.Bits = 8 },
		"dims":       func(s *ViewState) { s.VR.Labels.Dimensions[2] = 3 },
		"opacity":    func(s *ViewState) { s.VR.Labels.Styles[1].Opacity = 2 },
		"visible":    func(s *ViewState) { s.VR.Labels.Styles[1].Visible = 2 },
		"styles":     func(s *ViewState) { s.VR.Labels.Styles = make([]VRLabelStyleState, MaxVRLabelStyles+1) },
		"no gen":     func(s *ViewState) { s.VR.Labels.Generation = 0 },
		"huge":       func(s *ViewState) { s.VR.Labels.Volume = nil; s.VR.Labels.Dimensions = [3]uint32{4096, 4096, 4096} },
		"overflow": func(s *ViewState) {
			s.VR.Labels.Volume = nil
			s.VR.Labels.Dimensions = [3]uint32{1 << 22, 1 << 21, 1 << 21}
		},
	} {
		bad := state
		bad.VR.Labels.Styles = append([]VRLabelStyleState(nil), state.VR.Labels.Styles...)
		mutate(&bad)
		if err := ValidateViewState(bad); !errors.Is(err, ErrInvalidViewState) {
			t.Fatalf("%s: error = %v, want ErrInvalidViewState", name, err)
		}
	}
	if got := m.LabelBytes(); len(got) != 16 || got[2] != 1 || got[12] != 300&0xff || got[13] != 1 {
		t.Fatalf("16-bit label bytes = %v", got)
	}
	small, _ := NewVRLabelMap(1, 1, 2, []uint16{0, 5}, nil, false)
	if small.StorageBits() != 8 || string(small.LabelBytes()) != "\x00\x05" {
		t.Fatalf("8-bit label bytes = %v", small.LabelBytes())
	}
}

// TestVRViewStateOmitsZeroLabelsFromJSON keeps label-free requests
// byte-identical to V5 so frozen workloads keep their identity.
func TestVRViewStateOmitsZeroLabelsFromJSON(t *testing.T) {
	plain, err := json.Marshal(validVRViewState())
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(plain, []byte(`"Labels"`)) {
		t.Fatalf("label-free view state serializes Labels: %s", plain)
	}
	labelled := validVRViewState()
	labelled.VR.Labels = VRLabelState{Generation: 1, Dimensions: [3]uint32{1, 1, 1}, Bits: 8}
	encoded, err := json.Marshal(labelled)
	if err != nil || !bytes.Contains(encoded, []byte(`"Labels"`)) {
		t.Fatalf("labelled view state = %s, %v", encoded, err)
	}
}

func TestValidateViewStateV7Scattering(t *testing.T) {
	state := validVRViewState()
	state.ContractVersion, state.StructSize = ViewStateContractVersionV7, ViewStateHeaderSizeV7
	state.VR.OpacityUnitDistanceMM = 1
	state.VR.BackgroundRGBA = [4]float64{0, 0, 0, 1}
	state.VR.Scattering = VRScatteringState{Blend: 1, ReachMM: 50, Anisotropy: 0.3, LightDirectionLPS: [3]float64{0, 0, 1}}
	if err := ValidateViewState(state); err != nil {
		t.Fatalf("V7 scattering rejected: %v", err)
	}
	for name, mutate := range map[string]func(*ViewState){
		"V6": func(s *ViewState) {
			s.ContractVersion, s.StructSize = ViewStateContractVersionV6, ViewStateHeaderSizeV6
		},
		"blend":          func(s *ViewState) { s.VR.Scattering.Blend = 2.5 },
		"reach":          func(s *ViewState) { s.VR.Scattering.ReachMM = 0 },
		"anisotropy":     func(s *ViewState) { s.VR.Scattering.Anisotropy = 1 },
		"light":          func(s *ViewState) { s.VR.Scattering.LightDirectionLPS = [3]float64{0, 0, 2} },
		"header":         func(s *ViewState) { s.StructSize = ViewStateHeaderSizeV6 },
		"blend disabled": func(s *ViewState) { s.VR.Scattering.Blend = 0 },
	} {
		mutated := state
		mutate(&mutated)
		if err := ValidateViewState(mutated); !errors.Is(err, ErrInvalidViewState) {
			t.Errorf("%s: err = %v", name, err)
		}
	}
	if encoded, err := json.Marshal(validVRViewState()); err != nil || bytes.Contains(encoded, []byte(`"Scattering"`)) {
		t.Fatalf("scattering-free view state serializes Scattering: %s, %v", encoded, err)
	}
}

func TestValidateViewStateV8CutPlanes(t *testing.T) {
	state := validVRViewState()
	state.ContractVersion, state.StructSize = ViewStateContractVersionV8, ViewStateHeaderSizeV8
	state.VR.OpacityUnitDistanceMM = 1
	state.VR.BackgroundRGBA = [4]float64{0, 0, 0, 1}
	state.VR.ClippingPlanesLPS = []ClippingPlaneLPS{{0, 0, 1, -2}, {1, 0, 0, 0}}
	state.VR.Cut = VRCutPlaneState{Planes: 0b10, ClippedVoxelIntensity: -1024, CapOpacity: 1, CapWindow: [2]float64{-160, 240}}
	if err := ValidateViewState(state); err != nil {
		t.Fatalf("V8 cut planes rejected: %v", err)
	}
	noCap := state
	noCap.VR.Cut.CapOpacity, noCap.VR.Cut.CapWindow = 0, [2]float64{}
	if err := ValidateViewState(noCap); err != nil {
		t.Fatalf("V8 cut planes without cap rejected: %v", err)
	}
	for name, mutate := range map[string]func(*ViewState){
		"V7": func(s *ViewState) {
			s.ContractVersion, s.StructSize = ViewStateContractVersionV7, ViewStateHeaderSizeV7
		},
		"header":         func(s *ViewState) { s.StructSize = ViewStateHeaderSizeV7 },
		"no plane":       func(s *ViewState) { s.VR.Cut.Planes = 0 },
		"missing plane":  func(s *ViewState) { s.VR.Cut.Planes = 0b100 },
		"intensity":      func(s *ViewState) { s.VR.Cut.ClippedVoxelIntensity = math.Inf(-1) },
		"opacity":        func(s *ViewState) { s.VR.Cut.CapOpacity = 1.5 },
		"empty window":   func(s *ViewState) { s.VR.Cut.CapWindow = [2]float64{10, 10} },
		"reverse window": func(s *ViewState) { s.VR.Cut.CapWindow = [2]float64{240, -160} },
	} {
		mutated := state
		mutate(&mutated)
		if err := ValidateViewState(mutated); !errors.Is(err, ErrInvalidViewState) {
			t.Errorf("%s: err = %v", name, err)
		}
	}
	if encoded, err := json.Marshal(validVRViewState()); err != nil || bytes.Contains(encoded, []byte(`"Cut"`)) {
		t.Fatalf("cut-free view state serializes Cut: %s, %v", encoded, err)
	}
}

func TestValidateViewStateV9Geometry(t *testing.T) {
	state := validVRViewState()
	state.ContractVersion, state.StructSize = ViewStateContractVersionV9, ViewStateHeaderSizeV9
	state.VR.OpacityUnitDistanceMM = 1
	state.VR.BackgroundRGBA = [4]float64{0, 0, 0, 1}
	pixels := int(state.OutputWidth * state.OutputHeight)
	depth := &VRDepthImage{Width: int(state.OutputWidth), Height: int(state.OutputHeight),
		T: make([]float32, pixels), R: make([]float32, pixels), G: make([]float32, pixels), B: make([]float32, pixels)}
	for i := range depth.T {
		depth.T[i] = float32(math.Inf(1))
	}
	depth.T[0], depth.R[0] = 12.5, 1
	state.VR.Geometry = VRGeometryStateFromDepth(depth)
	if state.VR.Geometry.Color[0] != 0xff0000ff || state.VR.Geometry.Color[1] != 0xff000000 {
		t.Fatalf("geometry colours %#x %#x", state.VR.Geometry.Color[0], state.VR.Geometry.Color[1])
	}
	if err := ValidateViewState(state); err != nil {
		t.Fatalf("V9 geometry rejected: %v", err)
	}
	frozen, err := FreezeViewState(state)
	if err != nil || &frozen.VR.Geometry.Depth[0] == &state.VR.Geometry.Depth[0] {
		t.Fatalf("frozen geometry shares its depth: %v", err)
	}
	for name, mutate := range map[string]func(*ViewState){
		"V8": func(s *ViewState) {
			s.ContractVersion, s.StructSize = ViewStateContractVersionV8, ViewStateHeaderSizeV8
		},
		"flag":   func(s *ViewState) { s.VR.Geometry.Enabled = 2 },
		"size":   func(s *ViewState) { s.VR.Geometry.Depth = s.VR.Geometry.Depth[1:] },
		"colour": func(s *ViewState) { s.VR.Geometry.Color = nil },
		"nan":    func(s *ViewState) { s.VR.Geometry.Depth = []float32{float32(math.NaN())} },
		"depth":  func(s *ViewState) { s.VR.Geometry.Depth = append([]float32{0}, s.VR.Geometry.Depth[1:]...) },
	} {
		mutated := state
		mutated.VR.Geometry.Depth = append([]float32(nil), state.VR.Geometry.Depth...)
		mutate(&mutated)
		if err := ValidateViewState(mutated); !errors.Is(err, ErrInvalidViewState) {
			t.Errorf("%s: err = %v", name, err)
		}
	}
	if encoded, err := json.Marshal(state); err != nil || bytes.Contains(encoded, []byte(`"Depth"`)) || !bytes.Contains(encoded, []byte(`"Geometry"`)) {
		t.Fatalf("geometry JSON = %s, %v", encoded, err)
	}
	if encoded, err := json.Marshal(validVRViewState()); err != nil || bytes.Contains(encoded, []byte(`"Geometry"`)) {
		t.Fatalf("geometry-free view state serializes Geometry: %s, %v", encoded, err)
	}
}
