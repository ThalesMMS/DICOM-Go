package render

import (
	"context"
	"encoding/binary"
	"errors"
	"math"
	"testing"
)

func TestScalarValidityFrameCancelledDecode(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if samples, err := DecodeScalarValidityFrameContext(ctx, FrameOutput{}); !errors.Is(err, context.Canceled) || len(samples.Values) != 0 {
		t.Fatal("cancelled readback allocated samples", err)
	}
}

func TestScalarValidityViewContractRejectsDisplayAndOversizedRequests(t *testing.T) {
	valid := ViewState{ContractVersion: ViewStateContractVersionV19, StructSize: ViewStateHeaderSizeV19, Kind: ViewKindMPR, VolumeGeneration: 1, ViewGeneration: 1, OutputWidth: 2, OutputHeight: 2, MPR: MPRViewState{PixelStepULPS: [3]float64{1, 0, 0}, PixelStepVLPS: [3]float64{0, 1, 0}, Interpolation: InterpolationLinear, ScalarValidity: true}}
	if err := ValidateViewState(valid); err != nil {
		t.Fatal(err)
	}
	for name, change := range map[string]func(*ViewState){
		"old-version": func(v *ViewState) { v.ContractVersion = ViewStateContractVersionV18 },
		"display":     func(v *ViewState) { v.MPR.Presentation.Mode = MPRPresentationGray },
		"filter":      func(v *ViewState) { v.VolumeFilter = VolumeFilterState{Mode: VolumeFilterGaussian, SigmaMM: 1} },
		"averaged":    func(v *ViewState) { v.MPR.Antialias2x = true },
		"oversized":   func(v *ViewState) { v.OutputWidth = 8192; v.OutputHeight = 8192 },
	} {
		t.Run(name, func(t *testing.T) {
			view := valid
			change(&view)
			if !errors.Is(ValidateViewState(view), ErrInvalidViewState) {
				t.Fatal("unsupported quantitative view admitted")
			}
		})
	}
}

func TestScalarValidityV29PreservesModalityFilterDomain(t *testing.T) {
	view := ViewState{ContractVersion: ViewStateContractVersionV29, StructSize: ViewStateHeaderSizeV29,
		Kind: ViewKindMPR, VolumeGeneration: 1, ViewGeneration: 1, OutputWidth: 2, OutputHeight: 2,
		MPR: MPRViewState{PixelStepULPS: [3]float64{1, 0, 0}, PixelStepVLPS: [3]float64{0, 1, 0},
			Interpolation: InterpolationLinear, ScalarValidity: true}}
	for _, filter := range []VolumeFilterState{
		{Mode: VolumeFilterGaussian, SigmaMM: 1.5},
		{Mode: VolumeFilterBasicSmooth5x5},
		{Mode: VolumeFilterMedian3x3x3},
	} {
		view.VolumeFilter = filter
		if err := ValidateViewState(view); err != nil {
			t.Fatalf("modality filter %v refused: %v", filter, err)
		}
		old := view
		old.ContractVersion = ViewStateContractVersionV28
		if !errors.Is(ValidateViewState(old), ErrInvalidViewState) {
			t.Fatal("older scalar contract admitted filtered values")
		}
	}
	view.VolumeFilter = VolumeFilterState{Mode: VolumeFilterVesselEnhance}
	if !errors.Is(ValidateViewState(view), ErrInvalidViewState) {
		t.Fatal("display vessel response admitted as modality values")
	}
}

func TestScalarValidityFrameOwnedPaddedRowsAndSentinelCollision(t *testing.T) {
	frame := FrameOutput{ContractVersion: FrameOutputContractVersion, StructSize: FrameOutputHeaderSizeV1, Format: FrameFormatScalarValidityF32TopLeft, Width: 2, Height: 2, StrideBytes: 24, CapacityBytes: 40, WrittenBytes: 40, VolumeGeneration: 1, ViewGeneration: 1, BackendID: "webgpu", Pixels: make([]byte, 40)}
	for _, at := range []int{0, 8, 24, 32} {
		binary.LittleEndian.PutUint32(frame.Pixels[at:], math.Float32bits(123))
	}
	binary.LittleEndian.PutUint32(frame.Pixels[4:], math.Float32bits(1))
	binary.LittleEndian.PutUint32(frame.Pixels[36:], math.Float32bits(1))
	samples, err := DecodeScalarValidityFrame(frame)
	if err != nil {
		t.Fatal(err)
	}
	for i, want := range []bool{true, false, false, true} {
		if samples.Valid[i] != want || samples.Values[i] != 123 {
			t.Fatal("validity inferred from values or row padding", samples)
		}
	}
	frame.Pixels[0] = 0
	if samples.Values[0] != 123 {
		t.Fatal("samples alias mutable readback")
	}
}

func TestScalarValidityFrameRejectsMalformedFlagsAndBytes(t *testing.T) {
	base := FrameOutput{ContractVersion: FrameOutputContractVersion, StructSize: FrameOutputHeaderSizeV1, Format: FrameFormatScalarValidityF32TopLeft, Width: 1, Height: 1, StrideBytes: 8, CapacityBytes: 8, WrittenBytes: 8, VolumeGeneration: 1, ViewGeneration: 1, BackendID: "webgpu", Pixels: make([]byte, 8)}
	for name, change := range map[string]func(*FrameOutput){
		"partial":        func(f *FrameOutput) { f.Pixels = f.Pixels[:7] },
		"stride":         func(f *FrameOutput) { f.StrideBytes = 4 },
		"flag":           func(f *FrameOutput) { binary.LittleEndian.PutUint32(f.Pixels[4:], math.Float32bits(2)) },
		"nan":            func(f *FrameOutput) { binary.LittleEndian.PutUint32(f.Pixels, math.Float32bits(float32(math.NaN()))) },
		"infinite":       func(f *FrameOutput) { binary.LittleEndian.PutUint32(f.Pixels, math.Float32bits(float32(math.Inf(1)))) },
		"display-format": func(f *FrameOutput) { f.Format = FrameFormatRGBA8NRGBATopLeft; f.StrideBytes = 4; f.WrittenBytes = 4 },
	} {
		t.Run(name, func(t *testing.T) {
			frame := base
			frame.Pixels = append([]byte(nil), base.Pixels...)
			change(&frame)
			samples, err := DecodeScalarValidityFrame(frame)
			if !errors.Is(err, ErrInvalidFrameOutput) || len(samples.Values) != 0 {
				t.Fatal("malformed quantitative frame admitted", err)
			}
		})
	}
}
