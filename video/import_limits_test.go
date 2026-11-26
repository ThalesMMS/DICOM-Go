package video

import (
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"testing"
	"time"
)

func TestInspectSourceMP4LimitsAndMetadataForgeries(t *testing.T) {
	original := importAVCFixture(t)
	for _, limit := range []ImportLimits{{MaxBytes: int64(len(original) - 1)}, {MaxPixels: 6143}, {MaxDecodedBytes: 18431}, {MaxFrames: -1}, {MaxBytes: 1<<30 + 1}} {
		if r, e := InspectSource(context.Background(), original, limit); e == nil || r.Rows != 0 {
			t.Fatalf("limit accepted: %+v", limit)
		}
	}
	for _, tc := range []struct {
		name   string
		mutate func([]byte) []byte
	}{
		{"truncated", func(b []byte) []byte { return b[:len(b)-1] }},
		{"sample-offset", func(b []byte) []byte {
			at := bytes.Index(b, []byte("stco"))
			binary.BigEndian.PutUint32(b[at+12:], 0)
			return b
		}},
		{"rotation", func(b []byte) []byte {
			at := bytes.Index(b, []byte("tkhd"))
			binary.BigEndian.PutUint32(b[at+4+40:], 0)
			return b
		}},
		{"frame-budget", func(b []byte) []byte {
			at := bytes.Index(b, []byte("stsz"))
			binary.BigEndian.PutUint32(b[at+12:], 0xffffffff)
			return b
		}},
		{"variable-timing", func(b []byte) []byte {
			at := bytes.Index(b, []byte("stts"))
			binary.BigEndian.PutUint32(b[at+16:], 0)
			return b
		}},
		{"fragmented", func(b []byte) []byte { return append(b, importBoxBytes("moof", nil)...) }},
		{"bad-NAL", func(b []byte) []byte {
			at := bytes.Index(b, []byte("mdat"))
			binary.BigEndian.PutUint32(b[at+4:], 0xffffffff)
			return b
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			b := tc.mutate(bytes.Clone(original))
			if r, e := InspectSource(context.Background(), b, ImportLimits{}); e == nil || r.Rows != 0 {
				t.Fatal("forged container accepted")
			}
		})
	}
}
func TestInspectSourceCancelledAndUnsupportedContainer(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, e := InspectSource(ctx, importMPEGFixture(), ImportLimits{}); !errors.Is(e, context.Canceled) {
		t.Fatalf("cancel: %v", e)
	}
	for _, b := range [][]byte{nil, []byte("not media"), {0, 0, 1, 0x67}} {
		if _, e := InspectSource(nil, b, ImportLimits{}); e == nil {
			t.Fatal("unsupported input accepted")
		}
	}
}

func TestInspectSourceAVCAspectMetadataRefusals(t *testing.T) {
	original := importAVCFixture(t)
	// Import fixture SPS is real. Build a local SPS metadata view at a known VUI
	// bit position to exercise present/absent/truncated/SAR cases without changing
	// or copying the existing SPS parser.
	for _, tc := range []struct {
		name string
		bits []byte
		want bool
	}{
		{"absent", []byte{0x40}, true},            // VUI absent, rbsp_stop_one_bit
		{"square", []byte{0xc0, 0x60}, true},      // VUI+aspect present, idc1, trailing one
		{"non-square", []byte{0xc0, 0xa0}, false}, // idc2
		{"unspecified-idc", []byte{0xc0, 0x20}, false},
		{"extended-square-still-not-idc1", []byte{0xff, 0xe0}, false},
		{"truncated", []byte{0xc0}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := &h264SPS{raw: append([]byte{0x67}, tc.bits...)}
			e := inspectImportAVCAspect(s)
			if (e == nil) != tc.want {
				t.Fatalf("aspect: %v", e)
			}
		})
	}
	// A declared non-square pixel-aspect box must not override square SPS.
	for _, ratio := range [][2]uint32{{2, 1}, {0, 0}} {
		b := bytes.Clone(original)
		at := bytes.Index(b, []byte("avcC"))
		n := int(binary.BigEndian.Uint32(b[at-4:]))
		end := at - 4 + n
		pasp := make([]byte, 8)
		binary.BigEndian.PutUint32(pasp, ratio[0])
		binary.BigEndian.PutUint32(pasp[4:], ratio[1])
		box := importBoxBytes("pasp", pasp)
		// All ancestor sizes grow with the inserted sibling box.
		for _, kind := range []string{"moov", "trak", "mdia", "minf", "stbl", "stsd", "avc1"} {
			pos := bytes.Index(b, []byte(kind)) - 4
			binary.BigEndian.PutUint32(b[pos:], binary.BigEndian.Uint32(b[pos:])+uint32(len(box)))
		}
		b = append(append(append([]byte(nil), b[:end]...), box...), b[end:]...)
		if _, e := InspectSource(context.Background(), b, ImportLimits{}); e == nil {
			t.Fatal("non-square MP4 pasp accepted")
		}
	}
	b := bytes.Clone(original)
	at := bytes.Index(b, []byte("avcC"))
	b[at+4+3] = 41
	if _, e := InspectSource(context.Background(), b, ImportLimits{}); e == nil {
		t.Fatal("conflicting avcC/SPS level accepted")
	}
}
func TestInspectSourceMPEGRefusesOutsideDICOMProfile(t *testing.T) {
	original := importMPEGFixture()
	for _, change := range []func([]byte){
		func(b []byte) { at := bytes.Index(b, []byte{0, 0, 1, 0xb3}); b[at+7] = 0x12 }, //24fps
		func(b []byte) { at := bytes.Index(b, []byte{0, 0, 1, 0xb3}); b[at+4] = 0x40 }, //width1024
		func(b []byte) { at := bytes.Index(b, []byte{0, 0, 1, 0xb5}); b[at+5] = 0x6a }, //H14
		func(b []byte) { b[6] = 0 }, //invalid pack marker
	} {
		b := bytes.Clone(original)
		change(b)
		if _, e := InspectSource(context.Background(), b, ImportLimits{}); e == nil {
			t.Fatal("non-DICOM MPEG profile accepted")
		}
	}
}

type importScanContext struct{ calls, cancelAt int }

func (c *importScanContext) Deadline() (time.Time, bool) { return time.Time{}, false }
func (c *importScanContext) Done() <-chan struct{}       { return nil }
func (c *importScanContext) Value(any) any               { return nil }
func (c *importScanContext) Err() error {
	c.calls++
	if c.calls >= c.cancelAt {
		return context.Canceled
	}
	return nil
}
func TestInspectSourceMPEGSystemScanCancellation(t *testing.T) {
	b := bytes.Repeat([]byte{0, 0, 1, 0}, 4096)
	b = append(b, 0, 0, 1, 0xb9)
	ctx := &importScanContext{cancelAt: 32}
	_, e := nextSystemStart(ctx, b, 0)
	if !errors.Is(e, context.Canceled) {
		t.Fatalf("system scanner cancellation not observed: %v (checkpoints %d)", e, ctx.calls)
	}
	if ctx.calls > 32 {
		t.Fatal("continued after cancellation")
	}
}

func TestInspectSourceMPEGStartCodesAtChunkBoundaries(t *testing.T) {
	for _, at := range []int{0, 4093, 4094, 4095, 4096, 8191} {
		b := make([]byte, at+4)
		copy(b[at:], []byte{0, 0, 1, 0xb9})
		n, e := nextSystemStart(context.Background(), b, 0)
		if e != nil || n != at {
			t.Fatalf("at %d got %d %v", at, n, e)
		}
	}
	b := append(bytes.Repeat([]byte{7}, 4096), 0, 0, 1)
	if n, e := nextSystemStart(context.Background(), b, 0); e != nil || n != len(b) {
		t.Fatalf("truncated prefix: %d %v", n, e)
	}
}
