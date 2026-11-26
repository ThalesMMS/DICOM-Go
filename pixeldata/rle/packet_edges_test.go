package rle

import (
	"bytes"
	"context"
	"errors"
	"image"
	"testing"

	"github.com/ThalesMMS/dicom-go/pixeldata"
)

// Hand-authored PackBits segments exercise the decoder independently of
// the encoder's preferred row and packet partitioning.
func TestRLEWirePacketExtremesReachWholeFrameAndRegion(t *testing.T) {
	const columns = 43
	const size = 3 * columns
	literal := make([]byte, 128)
	for i := range literal {
		literal[i] = byte(i * 7)
	}
	for _, tc := range []struct {
		name    string
		segment []byte
		want    []byte
	}{
		{"128-literals-with-no-ops", append(append([]byte{0x80, 0x7f}, literal...), 0x80, 0x00, 0x44), append(bytes.Clone(literal), 0x44)},
		{"128-repeated-then-literal", []byte{0x81, 0x71, 0x00, 0x83}, append(bytes.Repeat([]byte{0x71}, 128), 0x83)},
		{"one-literal-then-128-repeated", []byte{0x00, 0xfa, 0x80, 0x81, 0x29, 0x80}, append([]byte{0xfa}, bytes.Repeat([]byte{0x29}, 128)...)},
		{"max-run-cut-at-segment-end", append([]byte{0x00, 0x32, 0x7f}, literal[:127]...), nil},
		{"max-run-exceeds-the-plane", []byte{0x01, 1, 2, 0x81, 0x70}, nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			stream := rleFragmentEncoded(tc.segment)
			obj, pixel := rleObjectWithFragment(t, 3, columns, 1, 8, "MONOCHROME1", stream)
			decoder := New()
			got, err := decoder.Decode(pixel, obj)
			if tc.want == nil {
				if !errors.Is(err, ErrSegmentDecodeFailed) || len(got.Data) != 0 {
					t.Fatalf("malformed segment produced %+v, %v", got, err)
				}
				return
			}
			if err != nil || len(got.Data) != 1 || !bytes.Equal(got.Data[0], tc.want) || len(got.Diagnostics) != 0 {
				t.Fatalf("body returned %+v, %v", got, err)
			}
			metadata, err := pixeldata.ExtractMetadata(obj)
			if err != nil {
				t.Fatal(err)
			}
			region := image.Rect(columns-4, 1, columns, 3)
			wantRegion := append(bytes.Clone(tc.want[2*columns-4:2*columns]), tc.want[size-4:]...)
			dst := bytes.Repeat([]byte{0xec}, len(wantRegion)+2)
			written, diagnostics, err := decoder.DecodeFrameInto(context.Background(), stream, metadata, pixeldata.FrameBufferRequest{Region: region}, dst)
			if err != nil || written != len(wantRegion) || len(diagnostics) != 0 ||
				!bytes.Equal(dst[:written], wantRegion) || !bytes.Equal(dst[written:], []byte{0xec, 0xec}) {
				t.Fatalf("region wrote %x, %d, %v, %v, want %x", dst, written, diagnostics, err, wantRegion)
			}
		})
	}
}
