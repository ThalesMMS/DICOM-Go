package rle

import (
	"context"
	"errors"
	"image"
	"runtime"
	"testing"

	"github.com/ThalesMMS/dicom-go/internal/decoderecovery"
)

// allocatedBytes returns the bytes fn allocates on the heap.
func allocatedBytes(fn func()) uint64 {
	var before, after runtime.MemStats
	runtime.GC()
	runtime.ReadMemStats(&before)
	fn()
	runtime.ReadMemStats(&after)
	return after.TotalAlloc - before.TotalAlloc
}

// A segment of n bytes decodes to at most 64n bytes (a two-byte replicate
// run yields 128), so a strict decode refuses a frame its segments cannot
// fill before allocating anything the size of the declared frame.
func TestStrictDecodeRefusesUnfillableFrameBeforeAllocating(t *testing.T) {
	const rows, cols = 4096, 4096 // 16 MiB per segment, 32 MiB frame
	fragment := rleFragmentEncoded([]byte{0x81, 1}, []byte{0x81, 2})
	var err error
	allocated := allocatedBytes(func() {
		_, _, err = decodeFrame(context.Background(), fragment, rows, cols, 1, 16, false)
	})
	if !errors.Is(err, ErrSegmentDecodeFailed) {
		t.Fatalf("error %v, want ErrSegmentDecodeFailed", err)
	}
	if allocated > 1<<16 {
		t.Fatalf("refusing a 68-byte frame allocated %d bytes", allocated)
	}

	metadata := encoderMetadata(rows, cols, 1, 16, "MONOCHROME2")
	allocated = allocatedBytes(func() {
		_, err = New().DecodeFrameRegion(context.Background(), fragment, metadata, image.Rect(0, 0, cols, rows))
	})
	if !errors.Is(err, ErrSegmentDecodeFailed) {
		t.Fatalf("region error %v, want ErrSegmentDecodeFailed", err)
	}
	if allocated > 1<<16 {
		t.Fatalf("refusing a 68-byte region allocated %d bytes", allocated)
	}

	// The opt-in recovery zero-fills the frame, so it allocates it once.
	ctx := decoderecovery.With(context.Background(), decoderecovery.Options{RLESegmentOverrun: true})
	var recovered bool
	allocated = allocatedBytes(func() {
		_, recovered, err = decodeFrame(ctx, fragment, rows, cols, 1, 16, true)
	})
	if err != nil || !recovered {
		t.Fatalf("recovery: %v, recovered %v", err, recovered)
	}
	if frame := uint64(rows * cols * 2); allocated > frame+1<<16 {
		t.Fatalf("recovered decode allocated %d bytes for a %d-byte frame", allocated, frame)
	}
}

func BenchmarkDecodeFrame(b *testing.B) {
	for _, c := range []struct {
		name          string
		samples, bits uint16
		photometric   string
	}{
		{"mono16", 1, 16, "MONOCHROME2"},
		{"rgb8", 3, 8, "RGB"},
	} {
		b.Run(c.name, func(b *testing.B) {
			const rows, cols = 512, 512
			frame := make([]byte, rows*cols*int(c.samples)*int(c.bits)/8)
			state := uint32(1)
			for i := range frame {
				if i%64 < 40 {
					frame[i] = byte(i / 512) // runs
				} else {
					state = state*1664525 + 1013904223
					frame[i] = byte(state >> 24) // literals
				}
			}
			encoded, err := NewEncoder().EncodeFrame(context.Background(), frame, encoderMetadata(rows, cols, c.samples, c.bits, c.photometric))
			if err != nil {
				b.Fatal(err)
			}
			b.SetBytes(int64(len(frame)))
			b.ReportAllocs()
			for range b.N {
				if _, _, err := decodeFrame(context.Background(), encoded.Data, rows, cols, int(c.samples), int(c.bits), false); err != nil {
					b.Fatal(err)
				}
			}
		})
	}
}

// A decoded frame is written in place: decoding allocates the frame once,
// not a second copy of every segment.
func TestDecodeAllocatesTheFrameOnce(t *testing.T) {
	const rows, cols = 1024, 1024
	frame := make([]byte, rows*cols*3)
	for i := range frame {
		frame[i] = byte(i / 4096)
	}
	encoded, err := NewEncoder().EncodeFrame(context.Background(), frame, encoderMetadata(rows, cols, 3, 8, "RGB"))
	if err != nil {
		t.Fatal(err)
	}
	allocated := allocatedBytes(func() {
		_, _, err = decodeFrame(context.Background(), encoded.Data, rows, cols, 3, 8, false)
	})
	if err != nil {
		t.Fatal(err)
	}
	if allocated > uint64(len(frame))+1<<16 {
		t.Fatalf("decoding a %d-byte frame allocated %d bytes", len(frame), allocated)
	}
}
