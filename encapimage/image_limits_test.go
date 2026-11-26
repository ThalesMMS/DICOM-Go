package encapimage

import (
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"math"
	"testing"
	"time"
)

func TestFromImagesBudgetsCancelAndOptionsFailClosed(t *testing.T) {
	data := grayInput(t)
	cases := []ImageOptions{
		{Limits: ImportLimits{MaxInputBytes: int64(len(data) - 1)}}, {Limits: ImportLimits{MaxPixels: 3}}, {Limits: ImportLimits{MaxDecodedBytes: 3}}, {Limits: ImportLimits{MaxFrames: -1}}, {Limits: ImportLimits{MaxFrames: 100001}}, {Limits: ImportLimits{MaxInputBytes: 1<<30 + 1}},
		{Format: PNG, J2KLossless: true}, {Format: PNG, Raw: RawOptions{Rows: 1}}, {JPEGOptions: JPEGOptions{StudyInstanceUID: "invalid"}}, {JPEGOptions: JPEGOptions{BurnedInAnnotation: "UNKNOWN"}},
	}
	for _, o := range cases {
		if f, r, e := FromImages(context.Background(), [][]byte{data}, o); e == nil || f != nil || r.Frames != 0 {
			t.Fatalf("option accepted: %+v", o)
		}
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if f, _, e := FromImages(ctx, [][]byte{data}, ImageOptions{}); !errors.Is(e, context.Canceled) || f != nil {
		t.Fatalf("cancel: %v", e)
	}
	if f, _, e := FromImages(context.Background(), [][]byte{data, data}, ImageOptions{Limits: ImportLimits{MaxFrames: 1}}); !errors.Is(e, ErrImportLimit) || f != nil {
		t.Fatalf("frame budget: %v", e)
	}
	if f, _, e := FromImages(context.Background(), [][]byte{[]byte{1, 2}}, ImageOptions{Format: Raw, Raw: RawOptions{Rows: 65535, Columns: 65535, Frames: 100000, BitsAllocated: 16, Samples: 3}}); e == nil || f != nil {
		t.Fatal("oversized raw accepted")
	}
}
func TestFromImagesFrameLayoutsMustAgree(t *testing.T) {
	if f, _, e := FromImages(context.Background(), [][]byte{[]byte("P5 1 1 255\n\x01"), []byte("P5 2 1 255\n\x01\x02")}, ImageOptions{}); !errors.Is(e, ErrUnsupportedImage) || f != nil {
		t.Fatalf("different frames: %v", e)
	}
}
func TestFromImagesSignedCanonicalCodesAndNativeVLOverflow(t *testing.T) {
	for _, data := range [][]byte{{0x00, 0x08}, {0x00, 0xf0}} {
		o := ImageOptions{Format: Raw, Raw: RawOptions{Rows: 1, Columns: 1, BitsAllocated: 16, BitsStored: 12, Signed: true}}
		if f, _, e := FromImages(context.Background(), [][]byte{data}, o); e == nil || f != nil {
			t.Fatal("invalid sign extension accepted")
		}
	}
	limits := ImportLimits{MaxPixels: 1 << 40, MaxDecodedBytes: 1 << 40}
	if _, e := admitImage(imageLayout{rows: 65535, columns: 65535, samples: 3, bits: 16}, limits); !errors.Is(e, ErrImportLimit) {
		t.Fatalf("DICOM VL overflow: %v", e)
	}
}

// scriptedContext deterministically cancels after a bounded number of marker
// checkpoints. It proves in-parser cancellation without timing assumptions.
type scriptedContext struct {
	calls    int
	cancelAt int
}

func (c *scriptedContext) Deadline() (time.Time, bool) { return time.Time{}, false }
func (c *scriptedContext) Done() <-chan struct{}       { return nil }
func (c *scriptedContext) Value(any) any               { return nil }
func (c *scriptedContext) Err() error {
	c.calls++
	if c.calls >= c.cancelAt {
		return context.Canceled
	}
	return nil
}
func TestFromImagesJPEGMarkerFanoutAndDeterministicCancellation(t *testing.T) {
	data := readFixture(t, "baseline_gray.jpg")
	many := append([]byte{255, 216}, bytes.Repeat([]byte{255, 0xfe, 0, 2}, 4097)...)
	many = append(many, data[2:]...)
	if f, _, e := FromImages(context.Background(), [][]byte{many}, ImageOptions{}); !errors.Is(e, ErrImportLimit) || f != nil {
		t.Fatalf("fanout: %v", e)
	}
	ctx := &scriptedContext{cancelAt: 10}
	if f, _, e := FromImages(ctx, [][]byte{many}, ImageOptions{}); !errors.Is(e, context.Canceled) || f != nil {
		t.Fatalf("in-parser cancel: %v", e)
	}
	if ctx.calls > 11 {
		t.Fatalf("continued after cancellation: %d", ctx.calls)
	}
}

func TestFromImagesPageVectorLengthAdmissionWithoutAllocatingFrames(t *testing.T) {
	if pageVectorBytes(1024) > 65534 || pageVectorBytes(100000) <= 65534 {
		t.Fatal("page vector projection")
	}
	f, _, e := FromImages(context.Background(), [][]byte{{}}, ImageOptions{Format: Raw, Limits: ImportLimits{MaxFrames: 100000}, Raw: RawOptions{Rows: 1, Columns: 1, Frames: 100000, BitsAllocated: 8}})
	if !errors.Is(e, ErrImportLimit) || f != nil {
		t.Fatalf("IS vector limit: %v", e)
	}
}
func TestFromImagesSyntaxAndSourceInstanceRefusal(t *testing.T) {
	for _, tc := range []struct {
		name string
		o    ImageOptions
	}{{"date", ImageOptions{JPEGOptions: JPEGOptions{StudyDate: "20269999"}}}, {"single-value-delimiter", ImageOptions{JPEGOptions: JPEGOptions{PatientID: "a\\b"}}}} {
		t.Run(tc.name, func(t *testing.T) {
			if f, _, e := FromImages(context.Background(), [][]byte{grayInput(t)}, tc.o); e == nil || f != nil {
				t.Fatal("invalid metadata syntax accepted")
			}
		})
	}
}

func TestFromImagesReadStageCancellation(t *testing.T) {
	t.Run("PNG-decode-read", func(t *testing.T) {
		data := grayInput(t)
		probe := &scriptedContext{cancelAt: math.MaxInt}
		limits := ImportLimits{MaxPixels: 1024, MaxDecodedBytes: 1}
		if _, e := readRaster(probe, data, PNG, limits); !errors.Is(e, ErrImportLimit) {
			t.Fatal(e)
		}
		bad := append([]byte(nil), data...)
		at := bytes.Index(bad, []byte("IDAT"))
		if at < 0 {
			t.Fatal("missing IDAT")
		}
		bad[at+4] ^= 255
		ctx := &scriptedContext{cancelAt: probe.calls + 3}
		limits.MaxDecodedBytes = 1024
		f, e := readRaster(ctx, bad, PNG, limits)
		if !errors.Is(e, context.Canceled) || len(f.data) != 0 {
			t.Fatalf("decode-stage cancel replaced by %v (checkpoints %d)", e, ctx.calls)
		}
		if ctx.calls > ctx.cancelAt+1 {
			t.Fatal("continued after cancel")
		}
	})
	t.Run("JPEG-entropy-scan", func(t *testing.T) {
		data := readFixture(t, "baseline_gray.jpg")
		original, e := parseJPEGStream(data)
		if e != nil {
			t.Fatal(e)
		}
		prefix := -1
		for _, s := range original.segments {
			if s.marker == markerSOS {
				prefix = s.start + 2 + int(binary.BigEndian.Uint16(data[s.start+2:]))
				break
			}
		}
		if prefix < 0 {
			t.Fatal("missing SOS")
		}
		own := append([]byte(nil), data[:prefix]...)
		own = append(own, bytes.Repeat([]byte{255, 0}, 4096)...)
		own = append(own, 255, markerEOI)
		if _, e := parseJPEGStream(own); e != nil {
			t.Fatal(e)
		}
		ctx := &scriptedContext{cancelAt: 32}
		parsed, e := parseJPEGStreamLimited(ctx, own, 4096)
		if !errors.Is(e, context.Canceled) || parsed.eoi != 0 {
			t.Fatalf("entropy-stage cancellation not observed: %v (checkpoints %d)", e, ctx.calls)
		}
		if ctx.calls > 33 {
			t.Fatal("continued after cancellation")
		}
	})
}
func TestFromImagesPortableReaderCancellationPriority(t *testing.T) {
	data := append([]byte("P2 1 1 255\n"), bytes.Repeat([]byte{' '}, 8192)...)
	data = append(data, '7')
	for cancelAt := 1; cancelAt <= 16; cancelAt++ {
		ctx := &scriptedContext{cancelAt: cancelAt}
		f, _, e := FromImages(ctx, [][]byte{data}, ImageOptions{})
		if ctx.calls >= cancelAt {
			if !errors.Is(e, context.Canceled) || f != nil {
				t.Fatalf("checkpoint %d cancellation mapped to %v", cancelAt, e)
			}
		} else if e != nil || f == nil {
			t.Fatal(e)
		}
	}
}

func TestFromImagesNativeAggregateDefinedLength(t *testing.T) {
	max := int64(4294967294)
	if int64(math.MaxInt)-1 < max {
		max = int64(math.MaxInt) - 1
	}
	for _, tc := range []struct {
		n    int64
		want int
		ok   bool
	}{{0, 0, true}, {1, 2, true}, {max, int(max), true}, {max - 1, int(max), true}, {1 << 32, 0, false}, {-1, 0, false}, {max + 1, 0, false}, {math.MaxInt64, 0, false}} {
		got, e := nativePixelDataLength(tc.n)
		if tc.ok {
			if e != nil || got != tc.want {
				t.Fatalf("n=%d got=%d e=%v", tc.n, got, e)
			}
		} else if !errors.Is(e, ErrImportLimit) {
			t.Fatalf("native aggregate %d accepted as padded length %d, err=%v", tc.n, got, e)
		}
	}
}
