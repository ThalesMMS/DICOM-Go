package rle

import (
	"bytes"
	"context"
	"errors"
	"image"
	"testing"

	"github.com/ThalesMMS/dicom-go/pixeldata"
)

func TestRLECallerBufferMatchesAllocated(t *testing.T) {
	for _, bits := range []uint16{8, 16} {
		for _, samples := range []uint16{1, 3} {
			m := pixeldata.Metadata{Rows: 9, Columns: 13, SamplesPerPixel: samples, BitsAllocated: bits, BitsStored: bits, HighBit: bits - 1, NumberOfFrames: 1, PhotometricInterpretation: "MONOCHROME2", PlanarConfigurationPresent: samples == 3}
			if samples == 3 {
				m.PhotometricInterpretation = "RGB"
			}
			frame := make([]byte, int(m.FrameSize()))
			for at := range frame {
				frame[at] = byte(at * 31)
			}
			encoded, err := NewEncoder().EncodeFrame(context.Background(), frame, m)
			if err != nil {
				t.Fatal(err)
			}
			for _, rect := range []image.Rectangle{image.Rect(0, 0, 13, 9), image.Rect(1, 2, 5, 7), image.Rect(10, 8, 13, 9)} {
				want, err := New().DecodeFrameRegion(context.Background(), encoded.Data, m, rect)
				if err != nil {
					t.Fatal(err)
				}
				out, err := New().BufferMetadata(nil, m, pixeldata.FrameBufferRequest{Region: rect})
				if err != nil {
					t.Fatal(err)
				}
				dst := bytes.Repeat([]byte{0xcf}, len(want.Data[0])+9)
				n, diagnostics, err := New().DecodeFrameInto(nil, encoded.Data, m, pixeldata.FrameBufferRequest{Region: rect}, dst)
				if err != nil || n != len(want.Data[0]) || out.FrameSize() != int64(n) || out.PlanarConfiguration != 0 || len(diagnostics) != 0 || !bytes.Equal(dst[:n], want.Data[0]) || !bytes.Equal(dst[n:], bytes.Repeat([]byte{0xcf}, 9)) {
					t.Fatalf("bits%d samples%d rect%v n%d err%v", bits, samples, rect, n, err)
				}
				dst[0] = 0xff
				fresh := make([]byte, n)
				if _, _, err := New().DecodeFrameInto(nil, encoded.Data, m, pixeldata.FrameBufferRequest{Region: rect}, fresh); err != nil || !bytes.Equal(fresh, want.Data[0]) {
					t.Fatal("retained caller destination")
				}
			}
		}
	}
}
func TestRLECallerBufferPreflightAndSparseError(t *testing.T) {
	m := pixeldata.Metadata{Rows: 4, Columns: 4, SamplesPerPixel: 1, BitsAllocated: 16, BitsStored: 16, HighBit: 15, NumberOfFrames: 1, PhotometricInterpretation: "MONOCHROME2"}
	raw := make([]byte, 32)
	for at := range raw {
		raw[at] = byte(at)
	}
	encoded, err := NewEncoder().EncodeFrame(context.Background(), raw, m)
	if err != nil {
		t.Fatal(err)
	}
	for _, kind := range []string{"short", "cancel", "bad-header"} {
		dst := bytes.Repeat([]byte{0x99}, 40)
		before := bytes.Clone(dst)
		stream := encoded.Data
		ctx := context.Background()
		expected := pixeldata.ErrBufferTooSmall
		switch kind {
		case "short":
			dst = dst[:31]
		case "cancel":
			var cancel context.CancelFunc
			ctx, cancel = context.WithCancel(ctx)
			cancel()
			expected = context.Canceled
		case "bad-header":
			stream = []byte{0}
			expected = ErrInvalidHeader
		}
		n, _, err := New().DecodeFrameInto(ctx, stream, m, pixeldata.FrameBufferRequest{Region: image.Rect(0, 0, 4, 4)}, dst)
		if !errors.Is(err, expected) || n != 0 || !bytes.Equal(dst, before[:len(dst)]) {
			t.Fatalf("%s n%d %v", kind, n, err)
		}
	}
	// Two independent segments: the first repeats 16 bytes and touches odd
	// destination positions; the second declares a 16-byte literal with only
	// one payload byte. Preflight offsets/expansion pass, parsing must fail.
	header := make([]byte, 64)
	header[0] = 2
	header[4] = 64
	header[8] = 66
	stream := append(header, 0xf1, 7, 15, 0x80)
	dst := bytes.Repeat([]byte{0xc4}, 40)
	before := bytes.Clone(dst)
	n, _, err := New().DecodeFrameInto(nil, stream, m, pixeldata.FrameBufferRequest{Region: image.Rect(0, 0, 4, 4)}, dst)
	if !errors.Is(err, ErrSegmentDecodeFailed) || n != 32 || !bytes.Equal(dst[n:], before[n:]) {
		t.Fatalf("partial n%d %v", n, err)
	}

	ctx := pixeldata.DecodeContextWithRecovery(context.Background(), pixeldata.DecodeRecovery{RLESegmentOverrun: true})
	tolerant := bytes.Repeat([]byte{0xc4}, 40)
	n, diagnostics, err := New().DecodeFrameInto(ctx, stream, m, pixeldata.FrameBufferRequest{Region: image.Rect(0, 0, 4, 4)}, tolerant)
	want, expectedErr := New().DecodeFrameRegion(ctx, stream, m, image.Rect(0, 0, 4, 4))
	if err != nil || expectedErr != nil || n != 32 || len(diagnostics) != 1 || diagnostics[0] != pixeldata.DiagnosticRLESegmentOverrun || !bytes.Equal(tolerant[:n], want.Data[0]) || !bytes.Equal(tolerant[n:], bytes.Repeat([]byte{0xc4}, 8)) {
		t.Fatalf("recovery n%d diagnostics%v %v / %v", n, diagnostics, err, expectedErr)
	}
}
