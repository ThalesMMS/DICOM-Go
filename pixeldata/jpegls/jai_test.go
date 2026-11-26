package jpegls

import (
	"bytes"
	"context"
	"os"
	"slices"
	"testing"

	"github.com/ThalesMMS/dicom-go/internal/dcmtkoracle"
	"github.com/ThalesMMS/dicom-go/pixeldata"
)

// jaiFrame is a textured little endian frame of bits-bit samples: a ramp
// plus pseudo-random noise, so contexts span the threshold ranges.
func jaiFrame(rows, columns, bits int) []byte {
	frame := make([]byte, 0, rows*columns*2)
	state := uint32(12345)
	maxval := (1 << bits) - 1
	for y := 0; y < rows; y++ {
		for x := 0; x < columns; x++ {
			state = state*1664525 + 1013904223
			noise := int(state>>16) % 2000
			value := (x*maxval/columns + y*37 + noise) % (maxval + 1)
			frame = append(frame, byte(value), byte(value>>8))
		}
	}
	return frame
}

// jaiStream encodes frame as the JAI ImageIO encoder did: with its
// thresholds and without an LSE segment declaring them.
func jaiStream(t *testing.T, frame []byte, metadata pixeldata.Metadata) []byte {
	t.Helper()
	params, err := validateEncoderInput(metadata)
	if err != nil {
		t.Fatal(err)
	}
	samples, err := nativeToSamples(frame, metadata, params)
	if err != nil {
		t.Fatal(err)
	}
	preset, err := withJAIThresholds(presetCodingParameters{maxval: params.maxval, reset: basicReset}, int(metadata.BitsStored))
	if err != nil {
		t.Fatal(err)
	}
	stream, err := encodeFrameWithPreset(context.Background(), samples, metadata, params, preset)
	if err != nil {
		t.Fatal(err)
	}
	return stream
}

// withLSE inserts after the one-component SOF55 the LSE segment GDCM
// FixJAIBugJPEGLS writes: MAXVAL, the JAI thresholds and RESET 64.
func withLSE(t *testing.T, stream []byte, precision int) []byte {
	t.Helper()
	maxval := (1 << precision) - 1
	preset, err := withJAIThresholds(presetCodingParameters{maxval: maxval}, precision)
	if err != nil {
		t.Fatal(err)
	}
	lse := []byte{0xFF, 0xF8, 0x00, 0x0D, 0x01}
	for _, v := range []int{maxval, preset.t1, preset.t2, preset.t3, basicReset} {
		lse = append(lse, byte(v>>8), byte(v))
	}
	const afterSOF = 2 + 2 + 11 // SOI, SOF55 marker and its 11-byte segment
	return slices.Concat(stream[:afterSOF], lse, stream[afterSOF:])
}

func TestJAIThresholdsMatchGDCMTable(t *testing.T) {
	for _, tc := range []struct{ precision, t1, t2, t3 int }{
		// FixJAIBugJPEGLS writes these for 13 and 14 bits and T1, T2 for 15.
		{13, 34, 131, 548}, {14, 66, 259, 1092}, {15, 130, 515, 2180}, {16, 258, 1027, 4356},
	} {
		preset, err := withJAIThresholds(presetCodingParameters{maxval: (1 << tc.precision) - 1}, tc.precision)
		if err != nil || preset.t1 != tc.t1 || preset.t2 != tc.t2 || preset.t3 != tc.t3 {
			t.Errorf("%d bits: %d %d %d, %v", tc.precision, preset.t1, preset.t2, preset.t3, err)
		}
		// The standard clamps FACTOR at MAXVAL 4095: the defaults differ.
		if t1, t2, t3 := defaultThresholds((1 << tc.precision) - 1); t1 != 18 || t2 != 67 || t3 != 276 {
			t.Errorf("%d bits: standard thresholds %d %d %d", tc.precision, t1, t2, t3)
		}
	}
	// A custom MAXVAL too small for the thresholds is refused.
	if _, err := withJAIThresholds(presetCodingParameters{maxval: 1000}, 16); err == nil {
		t.Fatal("accepted T3 above MAXVAL")
	}
}

func decodeJAI(t *testing.T, stream []byte, metadata pixeldata.Metadata, recover bool) ([]byte, []string) {
	t.Helper()
	decoded, diagnostics, err := tryDecodeJAI(t, stream, metadata, recover)
	if err != nil {
		t.Fatal(err)
	}
	return decoded, diagnostics
}

func tryDecodeJAI(t *testing.T, stream []byte, metadata pixeldata.Metadata, recover bool) ([]byte, []string, error) {
	t.Helper()
	obj, pixel := jpeglsObjectWithFragment(t, metadata, stream)
	ctx := pixeldata.DecodeContextWithRecovery(context.Background(), pixeldata.DecodeRecovery{JPEGLSJAIThresholds: recover})
	frames, err := New().DecodeContext(ctx, pixel, obj)
	if err != nil {
		return nil, nil, err
	}
	return frames.Data[0], frames.Diagnostics, nil
}

func TestDecodeJAIStreamOptIn(t *testing.T) {
	for _, bits := range []uint16{13, 16} {
		metadata := encoderMetadata(24, 40, 1, 16, "MONOCHROME2")
		metadata.BitsStored, metadata.HighBit = bits, bits-1
		frame := jaiFrame(24, 40, int(bits))
		stream := jaiStream(t, frame, metadata)

		// Strictly, the stream fails or decodes to other samples.
		if strict, _, err := tryDecodeJAI(t, stream, metadata, false); err == nil && bytes.Equal(strict, frame) {
			t.Fatalf("%d bits: the synthetic stream does not reproduce the bug", bits)
		}
		recovered, diagnostics := decodeJAI(t, stream, metadata, true)
		if !bytes.Equal(recovered, frame) || !slices.Equal(diagnostics, []string{pixeldata.DiagnosticJPEGLSJAIThresholds}) {
			t.Fatalf("%d bits: recovered equal %v, diagnostics %v", bits, bytes.Equal(recovered, frame), diagnostics)
		}
		// The LSE segment GDCM inserts gives the same samples without the
		// option, and the option leaves explicit thresholds alone.
		for _, recover := range []bool{false, true} {
			fixed, diagnostics := decodeJAI(t, withLSE(t, stream, int(bits)), metadata, recover)
			if !bytes.Equal(fixed, frame) || len(diagnostics) != 0 {
				t.Fatalf("%d bits with LSE (option %v): equal %v, diagnostics %v", bits, recover, bytes.Equal(fixed, frame), diagnostics)
			}
		}
	}
	// Streams of 12 bits or less are never changed.
	metadata := encoderMetadata(24, 40, 1, 16, "MONOCHROME2")
	metadata.BitsStored, metadata.HighBit = 12, 11
	frame := jaiFrame(24, 40, 12)
	encoded, err := NewEncoder().EncodeFrame(context.Background(), frame, metadata)
	if err != nil {
		t.Fatal(err)
	}
	if decoded, diagnostics := decodeJAI(t, encoded.Data, metadata, true); !bytes.Equal(decoded, frame) || len(diagnostics) != 0 {
		t.Fatalf("12 bits: equal %v, diagnostics %v", bytes.Equal(decoded, frame), diagnostics)
	}
}

// TestJAIStreamWithCharLS decodes the synthetic JAI stream with CharLS
// (through DCMTK dcmdjpls): with the LSE segment GDCM inserts it gives the
// source samples, as the opt-in decoder here does without the segment, and
// without the segment CharLS does not give them either. Enable with
// DICOM_GO_JPEGLS_INDEPENDENT=1 and DICOMGO_DCMTK_INTEGRATION=1.
func TestJAIStreamWithCharLS(t *testing.T) {
	if os.Getenv("DICOM_GO_JPEGLS_INDEPENDENT") != "1" {
		t.Skip("set DICOM_GO_JPEGLS_INDEPENDENT=1 to compare with CharLS")
	}
	dcmdjpls := dcmtkoracle.Tool(t, "dcmdjpls")
	metadata := encoderMetadata(24, 40, 1, 16, "MONOCHROME2")
	frame := jaiFrame(24, 40, 16)
	stream := jaiStream(t, frame, metadata)
	if recovered, _ := decodeJAI(t, stream, metadata, true); !bytes.Equal(recovered, frame) {
		t.Fatal("the opt-in decoder does not give the source samples")
	}
	if err := decodeWithDCMTK(t, dcmtkoracle.Exact, dcmdjpls, withLSE(t, stream, 16), frame, metadata); err != nil {
		t.Fatalf("CharLS with the LSE segment: %v", err)
	}
	if err := decodeWithDCMTK(t, "must not give the source samples without the LSE segment", dcmdjpls, stream, frame, metadata); err == nil {
		t.Fatal("CharLS decodes the JAI stream without the LSE segment: the vector does not reproduce the bug")
	}
}
