package rle

import (
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"image"
	"testing"

	"github.com/ThalesMMS/dicom-go/pixeldata"
)

// TestDecodeFrameRegionMatchesFullDecode encodes frames whose runs cross
// row boundaries and checks every region against the cropped full decode.
func TestDecodeFrameRegionMatchesFullDecode(t *testing.T) {
	for _, c := range []struct {
		name        string
		samples     uint16
		bits        uint16
		photometric string
	}{
		{"mono8", 1, 8, "MONOCHROME2"},
		{"mono16", 1, 16, "MONOCHROME2"},
		{"rgb8", 3, 8, "RGB"},
		{"rgb16", 3, 16, "RGB"},
	} {
		t.Run(c.name, func(t *testing.T) {
			const rows, cols = 9, 13
			m := pixeldata.Metadata{Rows: rows, Columns: cols, SamplesPerPixel: c.samples, BitsAllocated: c.bits, BitsStored: c.bits, HighBit: c.bits - 1,
				PhotometricInterpretation: c.photometric, NumberOfFrames: 1, PlanarConfigurationPresent: c.samples == 3}
			step := int(c.samples) * int(c.bits) / 8
			frame := make([]byte, rows*cols*step)
			for i := range frame {
				// Long constant stretches (repeat runs across rows) and noise.
				if i%97 < 60 {
					frame[i] = 7
				} else {
					frame[i] = byte(i * 31)
				}
			}
			encoded, err := NewEncoder().EncodeFrame(context.Background(), frame, m)
			if err != nil {
				t.Fatal(err)
			}
			full, _, err := decodeFrame(context.Background(), encoded.Data, rows, cols, int(c.samples), int(c.bits), false)
			if err != nil {
				t.Fatal(err)
			}
			for y0 := 0; y0 < rows; y0 += 2 {
				for x0 := 0; x0 < cols; x0 += 3 {
					region := image.Rect(x0, y0, min(cols, x0+5), min(rows, y0+4))
					got, err := New().DecodeFrameRegion(context.Background(), encoded.Data, m, region)
					if err != nil {
						t.Fatalf("region %v: %v", region, err)
					}
					var want []byte
					for y := region.Min.Y; y < region.Max.Y; y++ {
						want = append(want, full[(y*cols+region.Min.X)*step:(y*cols+region.Max.X)*step]...)
					}
					if got.Rows != region.Dy() || got.Columns != region.Dx() || !bytes.Equal(got.Data[0], want) {
						t.Fatalf("region %v differs from the cropped full decode", region)
					}
				}
			}
		})
	}
}

func TestDecodeFrameRegionRejectsTruncatedSegments(t *testing.T) {
	m := pixeldata.Metadata{Rows: 4, Columns: 4, SamplesPerPixel: 1, BitsAllocated: 8, BitsStored: 8, HighBit: 7, PhotometricInterpretation: "MONOCHROME2", NumberOfFrames: 1}
	encoded, err := NewEncoder().EncodeFrame(context.Background(), bytes.Repeat([]byte{1, 2, 3, 4}, 4), m)
	if err != nil {
		t.Fatal(err)
	}
	truncated := encoded.Data[:len(encoded.Data)-4]
	if _, err := New().DecodeFrameRegion(context.Background(), truncated, m, image.Rect(0, 3, 4, 4)); !errors.Is(err, ErrSegmentDecodeFailed) {
		t.Fatalf("truncated segment: %v", err)
	}
	// The rows above the damage still decode.
	if _, err := New().DecodeFrameRegion(context.Background(), truncated, m, image.Rect(0, 0, 4, 1)); err != nil {
		t.Fatalf("region above the truncation: %v", err)
	}
}

// TestDecodeFrameRegionCutsRunsAcrossRows decodes a segment whose single
// repeat run and literal run span several rows, as other encoders write.
func TestDecodeFrameRegionCutsRunsAcrossRows(t *testing.T) {
	m := pixeldata.Metadata{Rows: 4, Columns: 4, SamplesPerPixel: 1, BitsAllocated: 8, BitsStored: 8, HighBit: 7, PhotometricInterpretation: "MONOCHROME2", NumberOfFrames: 1}
	header := make([]byte, 64)
	binary.LittleEndian.PutUint32(header[0:], 1)
	binary.LittleEndian.PutUint32(header[4:], 64)
	// Ten 5s (repeat run of 10), then a literal run of six bytes 0..5.
	codestream := append(header, 0xf7, 5, 5, 0, 1, 2, 3, 4, 5)
	got, err := New().DecodeFrameRegion(context.Background(), codestream, m, image.Rect(1, 2, 4, 4))
	if err != nil {
		t.Fatal(err)
	}
	if want := []byte{5, 0, 1, 3, 4, 5}; !bytes.Equal(got.Data[0], want) {
		t.Fatalf("region % x, want % x", got.Data[0], want)
	}
}
