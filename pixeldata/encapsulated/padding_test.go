package encapsulated

import (
	"context"
	"errors"
	"testing"
)

// minimal JPEG: SOI, DQT-like segment, SOS with one entropy byte, EOI.
var paddingCodestream = []byte{0xff, 0xd8, 0xff, 0xdb, 0x00, 0x02, 0xff, 0xda, 0x00, 0x02, 0x11, 0xff, 0xd9}

func TestValidateFrameTrailingPad(t *testing.T) {
	base := paddingCodestream // 13 bytes: one pad byte makes it even
	cases := []struct {
		name string
		data []byte
		ok   bool
	}{
		{"no pad (odd)", base, true},
		{"zero pad", append(append([]byte{}, base...), 0x00), true},
		{"0xFF fill pad", append(append([]byte{}, base...), 0xff), true},
		{"0x20 pad", append(append([]byte{}, base...), 0x20), true},
		{"two bytes", append(append([]byte{}, base...), 0x20, 0x20), false},
		{"nonzero pad on odd length", append(append([]byte{0xff, 0xd8, 0xff, 0xdb, 0x00, 0x03, 0x00}, base[6:]...), 0x20), false},
		{"zero pad on odd length", append(append([]byte{0xff, 0xd8, 0xff, 0xdb, 0x00, 0x03, 0x00}, base[6:]...), 0x00), true},
		{"byte between segments", append(append([]byte{}, base[:6]...), append([]byte{0x20}, base[6:]...)...), false},
		{"marker bytes after EOI", append(append([]byte{}, base...), 0xff, 0xd9, 0x00), false},
	}
	for _, c := range cases {
		for _, format := range []Format{JPEG, JPEGLS} {
			err := ValidateFrame(context.Background(), c.data, format)
			if c.ok && err != nil {
				t.Errorf("%s (format %d): %v", c.name, format, err)
			}
			if !c.ok && !errors.Is(err, ErrLayout) {
				t.Errorf("%s (format %d): error = %v, want ErrLayout", c.name, format, err)
			}
		}
	}
}

func TestPlanReportsPadAcrossFragments(t *testing.T) {
	first := paddingCodestream[:6]
	last := append(append([]byte{}, paddingCodestream[6:]...), 0xff) // FF D9 FF
	frames := [][]byte{first, last, append(append([]byte{}, paddingCodestream...), 0x20)}
	p, err := New(context.Background(), Memory(frames), Tables{}, 2, JPEG, Limits{})
	if err != nil {
		t.Fatal(err)
	}
	v, err := p.Frame(context.Background(), 0)
	if err != nil {
		t.Fatal(err)
	}
	if !v.Padded || v.PadByte != 0xff || len(v.Data) != len(paddingCodestream)+1 {
		t.Fatalf("frame 0 view = padded %v byte %#x len %d", v.Padded, v.PadByte, len(v.Data))
	}
	if err := ValidateFrame(context.Background(), v.Data, JPEG); err != nil {
		t.Fatal(err)
	}
	v, _ = p.Frame(context.Background(), 1)
	if !v.Padded || v.PadByte != 0x20 {
		t.Fatalf("frame 1 view = padded %v byte %#x", v.Padded, v.PadByte)
	}
}

func TestPlanRejectsDataAfterPad(t *testing.T) {
	frame := append(append([]byte{}, paddingCodestream...), 0x20, 0x20, 0x20)
	if _, err := New(context.Background(), Memory([][]byte{paddingCodestream[:6], frame[6:]}), Tables{}, 1, JPEG, Limits{}); !errors.Is(err, ErrLayout) {
		t.Fatalf("error = %v, want ErrLayout", err)
	}
}

func TestTrailingPad(t *testing.T) {
	if _, ok := TrailingPad(paddingCodestream); ok {
		t.Fatal("unpadded codestream reported a pad")
	}
	if b, ok := TrailingPad(append(append([]byte{}, paddingCodestream...), 0x20)); !ok || b != 0x20 {
		t.Fatalf("pad = %#x %v", b, ok)
	}
}

// FuzzFramePadding splits a codestream into Items at arbitrary even points and
// appends an arbitrary suffix. Only a single pad byte that makes the frame
// even (or a zero byte) may be accepted, and the codestream must be recovered
// unchanged apart from that byte.
func FuzzFramePadding(f *testing.F) {
	f.Add(uint8(0), []byte{})
	f.Add(uint8(3), []byte{0xff})
	f.Add(uint8(1), []byte{0x20, 0x20})
	f.Add(uint8(2), []byte{0x00})
	f.Fuzz(func(t *testing.T, split uint8, suffix []byte) {
		if len(suffix) > 8 {
			suffix = suffix[:8]
		}
		frame := append(append([]byte{}, paddingCodestream...), suffix...)
		accept := len(suffix) == 0 || len(suffix) == 1 && (len(frame)%2 == 0 || suffix[0] == 0)
		err := ValidateFrame(context.Background(), frame, JPEG)
		if accept != (err == nil) {
			t.Fatalf("suffix % x: accept=%v err=%v", suffix, accept, err)
		}
		cut := 2 * (int(split)%(len(paddingCodestream)/2) + 1)
		if cut >= len(frame) {
			return
		}
		items := [][]byte{frame[:cut], frame[cut:]}
		p, err := New(context.Background(), Memory(items), Tables{}, 1, JPEG, Limits{})
		if err != nil {
			return
		}
		v, err := p.Frame(context.Background(), 0)
		if err != nil {
			t.Fatal(err)
		}
		if string(v.Data[:len(paddingCodestream)]) != string(paddingCodestream) {
			t.Fatalf("codestream changed: % x", v.Data)
		}
	})
}
