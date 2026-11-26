package encapsulated

import (
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"testing"
)

func incompleteOpaqueFrame() []byte {
	return []byte{0xff, 0xd8, 0xff, 0xe0, 0, 6, 0xff, 0xd9, 0xff, 0xd8, 0xff, 0xda, 0, 2, 0x11, 0xff, 0, 0xff, 0xd9, 0}
}
func TestIncompletePrefixFramingAndOriginalQuotas(t *testing.T) {
	frame := incompleteOpaqueFrame()
	fragments := [][]byte{frame[:18], frame[18:], frame, []byte{0xff, 0xd8, 0xff, 0xda, 0, 2, 0x11, 0x22}}
	plan, report, err := CompletePrefix(context.Background(), Memory(fragments), Tables{}, 3, JPEG, Limits{})
	if err != nil || plan.Len() != 2 || report.RetainedFragments != 3 || report.DiscardedFragments != 1 || report.DiscardedBytes != 8 {
		t.Fatalf("%+v %v", report, err)
	}
	for i := 0; i < 2; i++ {
		view, err := plan.Frame(context.Background(), i)
		if err != nil || !bytes.Equal(view.Data, frame) {
			t.Fatalf("frame%d %x %v", i, view.Data, err)
		}
	}
	if _, _, err := CompletePrefix(context.Background(), Memory(fragments), Tables{}, 3, JPEG, Limits{MaxFrames: 2}); !errors.Is(err, ErrResourceLimit) {
		t.Fatalf("original frame quota %v", err)
	}
	if _, _, err := CompletePrefix(context.Background(), Memory(fragments), Tables{}, 3, JPEG, Limits{MaxFragments: 3}); !errors.Is(err, ErrResourceLimit) {
		t.Fatalf("fragment quota %v", err)
	}
	if _, _, err := CompletePrefix(context.Background(), Memory(fragments), Tables{}, 3, JPEG, Limits{MaxBytes: 47}); !errors.Is(err, ErrResourceLimit) {
		t.Fatalf("input quota %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, _, err := CompletePrefix(ctx, Memory(fragments), Tables{}, 3, JPEG, Limits{}); !errors.Is(err, context.Canceled) {
		t.Fatalf("cancel %v", err)
	}
	// Valid populated BOT describes all three retained source Item starts before
	// the independently proved two-frame prefix trims its table.
	bot := make([]byte, 12)
	binary.LittleEndian.PutUint32(bot[4:], 36)
	binary.LittleEndian.PutUint32(bot[8:], 64)
	plan, report, err = CompletePrefix(context.Background(), Memory(fragments), Tables{Basic: bot}, 3, JPEG, Limits{})
	if err != nil || plan.Len() != 2 || report.RetainedFrames != 2 {
		t.Fatalf("BOT %+v %v", report, err)
	}
	bad := bytes.Clone(bot)
	binary.LittleEndian.PutUint32(bad[4:], 26)
	if _, _, err := CompletePrefix(context.Background(), Memory(fragments), Tables{Basic: bad}, 3, JPEG, Limits{}); !errors.Is(err, ErrLayout) {
		t.Fatalf("conflicting BOT %v", err)
	}
	for _, table := range []Tables{{Basic: bot[:8]}, {ExtendedPresent: true}, {LengthsPresent: true}, {Extended: []uint64{0}}} {
		if _, _, err := CompletePrefix(context.Background(), Memory(fragments), table, 3, JPEG, Limits{}); !errors.Is(err, ErrLayout) {
			t.Fatalf("table %v", err)
		}
	}
}
func TestIncompletePrefixNeverInventsEndMarkers(t *testing.T) {
	frame := incompleteOpaqueFrame()
	cases := []struct {
		name      string
		fragments [][]byte
		frames    int
		want      error
	}{
		{"opaque EOI only", [][]byte{frame[:10]}, 1, ErrFrameCount},
		{"missing EOI", [][]byte{frame[:18]}, 1, ErrFrameCount},
		{"nested SOI", [][]byte{frame, []byte{0xff, 0xd8, 0xff, 0xd8}}, 2, ErrLayout},
		{"invalid marker length", [][]byte{frame, []byte{0xff, 0xd8, 0xff, 0xe0, 0, 1}}, 2, ErrLayout},
		{"garbage middle", [][]byte{frame, []byte{1, 2}, frame}, 3, ErrLayout},
		{"extra complete frame", [][]byte{frame, frame}, 1, ErrFrameCount},
		{"odd Item", [][]byte{frame[:19]}, 1, ErrLayout},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			plan, _, err := CompletePrefix(context.Background(), Memory(tc.fragments), Tables{}, tc.frames, JPEG, Limits{})
			if plan != nil || !errors.Is(err, tc.want) {
				t.Fatalf("%v", err)
			}
		})
	}
	for _, format := range []Format{Format(0), JPEG2000, Format(4)} {
		if _, _, err := CompletePrefix(context.Background(), Memory([][]byte{frame}), Tables{}, 1, format, Limits{}); !errors.Is(err, ErrLayout) {
			t.Fatalf("format%v %v", format, err)
		}
	}
	// JPEG-LS treats FF followed by a byte below 80 as entropy, not a marker.
	ls := []byte{0xff, 0xd8, 0xff, 0xda, 0, 2, 0xff, 0x20, 0xff, 0xd9}
	p, _, err := CompletePrefix(context.Background(), Memory([][]byte{ls}), Tables{}, 1, JPEGLS, Limits{})
	if err != nil || p.Len() != 1 {
		t.Fatalf("JPEG-LS %v", err)
	}
}
