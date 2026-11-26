package jpegls

import (
	"bytes"
	"context"
	"encoding/binary"
	"slices"
	"testing"

	"github.com/ThalesMMS/dicom-go/pixeldata"
)

// TestSignedNearLosslessBoundsTheCodes decodes a signed .81 stream: the
// codestream holds the BitsStored-wide unsigned codes, the decoder returns
// them, and NEAR bounds their error. Values away from the code midpoint
// 2^(BitsStored-1) keep the bound in the signed domain too; across it a
// code error changes the sign (see JPEGLS_NEAR_LOSSLESS.md).
func TestSignedNearLosslessBoundsTheCodes(t *testing.T) {
	const rows, columns, near = 32, 48, 3
	for _, bits := range []uint16{12, 16} {
		metadata := encoderMetadata(rows, columns, 1, 16, "MONOCHROME2")
		metadata.BitsStored, metadata.HighBit = bits, bits-1
		frame := make([]byte, rows*columns*2)
		state := uint32(7)
		for i := range rows * columns {
			state = state*1664525 + 1013904223
			// CT-like values around zero: -1000..999 plus noise.
			value := int16(i%2000-1000) + int16(state>>28)
			binary.LittleEndian.PutUint16(frame[2*i:], uint16(value))
		}
		params, err := validateEncoderInput(metadata)
		if err != nil {
			t.Fatal(err)
		}
		codes, err := nativeToSamples(frame, metadata, params)
		if err != nil {
			t.Fatal(err)
		}
		stream, err := encodeFrameWithNear(context.Background(), codes, metadata, params, near, InterleaveNone)
		if err != nil {
			t.Fatal(err)
		}
		metadata.PixelRepresentation = 1
		obj, pixel := jpeglsObjectWithFragment(t, metadata, stream)
		decoded, err := NewNearLossless().Decode(pixel, obj)
		if err != nil {
			t.Fatalf("%d bits: %v", bits, err)
		}
		mask := 1<<bits - 1
		signBit := 1 << (bits - 1)
		for i, code := range codes {
			got := int(binary.LittleEndian.Uint16(decoded.Data[0][2*i:]))
			if got > mask || abs(got-code) > near {
				t.Fatalf("%d bits sample %d: code %d decoded %d", bits, i, code, got)
			}
			// Sign-extend both codes to compare the signed values.
			signed := func(c int) int { return (c ^ signBit) - signBit }
			if abs(signed(got)-signed(code)) > near {
				t.Fatalf("%d bits sample %d: value %d decoded %d", bits, i, signed(code), signed(got))
			}
		}
	}
}

func abs(x int) int {
	if x < 0 {
		return -x
	}
	return x
}

// TestDecodeAcceptsAnyItemPadByte decodes frames whose Item ends with a pad
// byte after EOI: PS3.5 A.4 does not fix its value, so 0xFF is accepted and
// reported as encapsulated-nonzero-frame-pad.
func TestDecodeAcceptsAnyItemPadByte(t *testing.T) {
	metadata := encoderMetadata(3, 5, 1, 8, "MONOCHROME2")
	for seed := byte(0); seed < 64; seed++ {
		frame := make([]byte, 15)
		for i := range frame {
			frame[i] = seed*7 + byte(i*i)
		}
		encoded, err := NewEncoder().EncodeFrame(context.Background(), frame, metadata)
		if err != nil {
			t.Fatal(err)
		}
		if len(encoded.Data)%2 == 0 {
			continue // the Item needs no pad
		}
		for _, pad := range []byte{0x00, 0xFF} {
			obj, pixel := jpeglsObjectWithFragment(t, metadata, append(bytes.Clone(encoded.Data), pad))
			decoded, err := New().Decode(pixel, obj)
			if err != nil || !bytes.Equal(decoded.Data[0], frame) {
				t.Fatalf("pad %#x: %v", pad, err)
			}
			if reported := slices.Contains(decoded.Diagnostics, pixeldata.DiagnosticNonZeroFramePad); reported != (pad != 0) {
				t.Fatalf("pad %#x: diagnostics %v", pad, decoded.Diagnostics)
			}
		}
		return
	}
	t.Fatal("no odd-length codestream found")
}
