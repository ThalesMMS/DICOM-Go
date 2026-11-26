package encapsulated

import (
	"context"
	"encoding/binary"
	"errors"
	"testing"
)

func botTable(offsets ...uint32) []byte {
	b := make([]byte, 4*len(offsets))
	for i, o := range offsets {
		binary.LittleEndian.PutUint32(b[4*i:], o)
	}
	return b
}

// four one-Item frames, each an even-length codestream (13 bytes + pad).
func recoveryItems(n int) [][]byte {
	items := make([][]byte, n)
	for i := range items {
		items[i] = append(append([]byte{}, paddingCodestream...), 0)
	}
	return items
}

func TestInconsistentBasicOffsetTableRecovery(t *testing.T) {
	items := recoveryItems(4)
	// Five entries for four frames, as in gdcmData 00191113.dcm.
	bot := botTable(0, 22, 44, 66, 88)
	if _, err := New(context.Background(), Memory(items), Tables{Basic: bot}, 4, JPEG, Limits{}); !errors.Is(err, ErrLayout) {
		t.Fatalf("strict error = %v, want ErrLayout", err)
	}
	ctx := WithRecovery(context.Background(), Recovery{IgnoreInconsistentBasicOffsetTable: true})
	p, err := New(ctx, Memory(items), Tables{Basic: bot}, 4, JPEG, Limits{})
	if err != nil {
		t.Fatal(err)
	}
	r := p.Recovery()
	if !r.BasicOffsetTableIgnored || r.BasicOffsetTableEntries != 5 || r.Frames != 4 || r.Reason == "" {
		t.Fatalf("report = %+v", r)
	}
	if p.Len() != 4 {
		t.Fatalf("frames = %d", p.Len())
	}
	// A consistent table never reports a recovery.
	p, err = New(ctx, Memory(items), Tables{Basic: botTable(0, 22, 44, 66)}, 4, JPEG, Limits{})
	if err != nil || p.Recovery().BasicOffsetTableIgnored {
		t.Fatalf("consistent table: %v %+v", err, p.Recovery())
	}
}

func TestBasicOffsetTableRecoveryNeverInventsFrames(t *testing.T) {
	ctx := WithRecovery(context.Background(), Recovery{IgnoreInconsistentBasicOffsetTable: true})
	// Three codestreams cannot satisfy four declared frames.
	_, err := New(ctx, Memory(recoveryItems(3)), Tables{Basic: botTable(0, 1, 2, 3)}, 4, JPEG, Limits{})
	if !errors.Is(err, ErrLayout) {
		t.Fatalf("error = %v, want ErrLayout", err)
	}
	// Five codestreams are more than four frames.
	_, err = New(ctx, Memory(recoveryItems(5)), Tables{Basic: botTable(0, 1, 2)}, 4, JPEG, Limits{})
	if !errors.Is(err, ErrLayout) {
		t.Fatalf("error = %v, want ErrLayout", err)
	}
}

// FuzzBasicOffsetTableRecovery mutates BOT entries (out of order, duplicated,
// beyond the value) and checks that recovery either fails or yields exactly
// the declared frames, each a whole codestream.
func FuzzBasicOffsetTableRecovery(f *testing.F) {
	f.Add(uint32(0), uint32(22), uint32(44), uint32(66), uint8(4))
	f.Add(uint32(0), uint32(44), uint32(22), uint32(66), uint8(4))
	f.Add(uint32(0), uint32(22), uint32(22), uint32(66), uint8(4))
	f.Add(uint32(0), uint32(22), uint32(44), uint32(9999), uint8(4))
	f.Add(uint32(5), uint32(0), uint32(0), uint32(0), uint8(3))
	f.Fuzz(func(t *testing.T, a, b, c, d uint32, entries uint8) {
		n := int(entries%6) + 1
		bot := botTable(a, b, c, d, a+b, c^d)[:4*n]
		items := recoveryItems(4)
		for _, recover := range []bool{false, true} {
			ctx := WithRecovery(context.Background(), Recovery{IgnoreInconsistentBasicOffsetTable: recover})
			p, err := New(ctx, Memory(items), Tables{Basic: bot}, 4, JPEG, Limits{})
			if err != nil {
				continue
			}
			if p.Len() != 4 {
				t.Fatalf("recover=%v frames = %d", recover, p.Len())
			}
			for i := 0; i < 4; i++ {
				v, err := p.Frame(context.Background(), i)
				if err != nil {
					t.Fatal(err)
				}
				if err := ValidateFrame(context.Background(), v.Data, JPEG); err != nil {
					t.Fatalf("recover=%v frame %d invalid: %v", recover, i, err)
				}
			}
			if !recover && p.Recovery().BasicOffsetTableIgnored {
				t.Fatal("strict plan reported a recovery")
			}
		}
	})
}
