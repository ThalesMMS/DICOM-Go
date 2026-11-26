package nifti_test

import (
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"math"
	"testing"

	"github.com/ThalesMMS/dicom-go/nifti"
	"github.com/ThalesMMS/dicom-go/pixeldata/display"
	"github.com/ThalesMMS/dicom-go/render"
)

func TestScalarImageInspectRequiresCompleteReadBeforeDisplay(t *testing.T) {
	spec := defaultSynth()
	spec.qformCode = 1
	spec.datatype = nifti.DatatypeFloat64
	spec.dims = []int64{64, 64, 8}
	data := synthFile(spec, nil)
	for _, input := range [][]byte{data, gzipBytes(t, data)} {
		info, err := nifti.Inspect(context.Background(), bytes.NewReader(input), nifti.ReadOptions{})
		if err != nil || info.PayloadBytes != 64*64*8*8 || info.Dimensions != [4]int{64, 64, 8, 1} {
			t.Fatalf("header admission: %+v %v", info, err)
		}
		if _, err = nifti.Read(context.Background(), bytes.NewReader(input), nifti.ReadOptions{}); !errors.Is(err, nifti.ErrTruncatedData) {
			t.Fatalf("incomplete payload accepted: %v", err)
		}
	}
	_, err := nifti.Inspect(context.Background(), bytes.NewReader(data), nifti.ReadOptions{Limits: nifti.ReadLimits{MaxPayloadBytes: 16}})
	if !errors.Is(err, nifti.ErrLimitExceeded) {
		t.Fatalf("header limit: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err = nifti.Inspect(ctx, bytes.NewReader(data), nifti.ReadOptions{}); !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
}

func TestScalarImageGPUInputPreservesFloatSourceAndGeometry(t *testing.T) {
	spec := defaultSynth()
	spec.datatype = nifti.DatatypeFloat64
	spec.qformCode = 1
	spec.dims = []int64{2, 2, 2}
	spec.pixdim = [8]float64{-1, .7, 1.3, 4, 1, 1, 1, 1}
	values := []float64{.1234567890123, 2.25, -3.5, 4, 5, 6, 7, 8}
	volume := mustRead(t, synthFile(spec, values), nifti.ReadOptions{})
	input, err := volume.GPUInput(context.Background(), 0, 7, 32)
	if err != nil {
		t.Fatal(err)
	}
	if input.Descriptor.ScalarFormat != render.VolumeScalarF32ModalityLE || input.Descriptor.RescaleSlope != 1 || input.Descriptor.RescaleIntercept != 0 || input.Descriptor.IndexToPatientLPS != volume.Geometry.IndexToLPS {
		t.Fatalf("descriptor: %+v", input.Descriptor)
	}
	for i, want := range values {
		if volume.Stored(i) != want || volume.Value(i) != want {
			t.Fatalf("source changed at %d", i)
		}
		got := math.Float32frombits(binary.LittleEndian.Uint32(input.Payload[i*4:]))
		if got != float32(want) {
			t.Fatalf("transport sample %d: %g", i, got)
		}
	}
	p := render.Vec3{X: .2, Y: 1, Z: .7}
	got := input.Descriptor.PatientLPSToIndex.TransformPoint(input.Descriptor.IndexToPatientLPS.TransformPoint(p))
	if got.Sub(p).Length() > 1e-12 {
		t.Fatalf("affine roundtrip: %v", got)
	}
	for _, tc := range []struct{ generation, cap uint64 }{{7, 31}, {0, 32}} {
		if input, err = volume.GPUInput(context.Background(), 0, tc.generation, tc.cap); err == nil || len(input.Payload) != 0 {
			t.Fatalf("invalid admission accepted: %v", err)
		}
	}
	volume.Dimensions[0] = math.MaxInt
	if _, err = volume.SliceImage(context.Background(), 0, 0, render.WindowLevel{Width: 10}); err == nil {
		t.Fatal("mutated oversized layout accepted")
	}
}

func TestScalarImageNonfinitePolicyRetainsOriginalSamples(t *testing.T) {
	for _, bad := range []float64{math.NaN(), math.Inf(1), math.Inf(-1), math.MaxFloat64} {
		spec := defaultSynth()
		spec.qformCode = 1
		spec.datatype = nifti.DatatypeFloat64
		spec.dims = []int64{2, 2, 1}
		volume := mustRead(t, synthFile(spec, []float64{0, bad, .25, .5}), nifti.ReadOptions{})
		input, err := volume.GPUInput(context.Background(), 0, 1, 16)
		if !errors.Is(err, nifti.ErrGPUScalarProfile) || len(input.Payload) != 0 {
			t.Fatalf("nonrepresentable input accepted: %v", err)
		}
		img, err := volume.SliceImage(context.Background(), 0, 0, render.WindowLevel{Center: .5, Width: 1, Function: display.VOILinearExact})
		if err != nil {
			t.Fatal(err)
		}
		if math.IsNaN(bad) || math.IsInf(bad, 0) {
			if img.Pix[1] != 0 {
				t.Fatal("nonfinite display is not background")
			}
		}
		if !equalValues(volumeValues(volume), []float64{0, bad, .25, .5}) {
			t.Fatal("display changed source")
		}
		if img.Pix[2] == img.Pix[0] {
			t.Fatal("fractional float intensity was truncated before windowing")
		}
	}
}
