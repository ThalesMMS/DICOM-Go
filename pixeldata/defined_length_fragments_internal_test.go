package pixeldata

import (
	"bytes"
	"encoding/binary"
	"testing"
)

func definedLengthItem(element uint16, value []byte) []byte {
	out := binary.LittleEndian.AppendUint16(nil, 0xFFFE)
	out = binary.LittleEndian.AppendUint16(out, element)
	out = binary.LittleEndian.AppendUint32(out, uint32(len(value)))
	return append(out, value...)
}

func TestSplitDefinedLengthItems(t *testing.T) {
	bot := definedLengthItem(0xE000, nil)
	fragment := definedLengthItem(0xE000, []byte{0xFF, 0xD8, 1, 2})
	delimiter := definedLengthItem(0xE0DD, nil)
	undefined := append(binary.LittleEndian.AppendUint32([]byte{0xFE, 0xFF, 0x00, 0xE0}, 0xFFFFFFFF), 1, 2)
	for _, tc := range []struct {
		name      string
		value     []byte
		ok        bool
		fragments int
	}{
		{"BOT and fragment", bytes.Join([][]byte{bot, fragment}, nil), true, 1},
		{"with delimiter", bytes.Join([][]byte{bot, fragment, fragment, delimiter}, nil), true, 2},
		{"BOT with offsets", bytes.Join([][]byte{definedLengthItem(0xE000, make([]byte, 4)), fragment}, nil), true, 1},
		{"bytes left over", bytes.Join([][]byte{bot, fragment, {0}}, nil), false, 0},
		{"truncated item", bytes.Join([][]byte{bot, fragment[:len(fragment)-1]}, nil), false, 0},
		{"undefined item length", bytes.Join([][]byte{bot, undefined}, nil), false, 0},
		{"delimiter not last", bytes.Join([][]byte{bot, delimiter, fragment}, nil), false, 0},
		{"other tag", bytes.Join([][]byte{bot, definedLengthItem(0xE00D, nil)}, nil), false, 0},
		{"only a BOT", bot, false, 0},
		{"native pixels", []byte{1, 2, 3, 4, 5, 6, 7, 8, 9, 10}, false, 0},
		{"empty", nil, false, 0},
	} {
		sequence, ok := splitDefinedLengthItems(tc.value, 10)
		if ok != tc.ok || len(sequence.Fragments) != tc.fragments {
			t.Errorf("%s: ok %v, %d fragments", tc.name, ok, len(sequence.Fragments))
		}
	}
	many := bot
	for range 4 {
		many = append(many, fragment...)
	}
	if _, ok := splitDefinedLengthItems(many, 3); ok {
		t.Error("accepted more Items than the limit")
	}
}

func FuzzSplitDefinedLengthItems(f *testing.F) {
	f.Add(bytes.Join([][]byte{definedLengthItem(0xE000, nil), definedLengthItem(0xE000, []byte{1, 2}), definedLengthItem(0xE0DD, nil)}, nil))
	f.Add([]byte{0xFE, 0xFF, 0x00, 0xE0, 0xFF, 0xFF, 0xFF, 0xFF})
	f.Fuzz(func(t *testing.T, value []byte) {
		sequence, ok := splitDefinedLengthItems(value, 1000)
		if !ok {
			return
		}
		// The Items re-encode to the value, less a final delimiter.
		encoded := definedLengthItem(0xE000, sequence.OffsetTable)
		for _, fragment := range sequence.Fragments {
			encoded = append(encoded, definedLengthItem(0xE000, fragment)...)
		}
		if !bytes.Equal(encoded, value) && !bytes.Equal(append(encoded, definedLengthItem(0xE0DD, nil)...), value) {
			t.Fatalf("split %d fragments do not re-encode the value", len(sequence.Fragments))
		}
	})
}
