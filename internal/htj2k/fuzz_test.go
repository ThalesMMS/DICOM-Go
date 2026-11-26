package htj2k

import (
	"errors"
	"testing"
)

// FuzzDecodeCleanup feeds arbitrary segments to the decoder, which must
// either decode them or report ErrMalformed/ErrUnsupported, never panic.
func FuzzDecodeCleanup(f *testing.F) {
	for _, block := range [][]int32{{1}, {-5, 0, 7, 1 << 20}, {0, 0, 0, 3, 0, 0, 0, 0, -1}} {
		w := 1
		switch len(block) {
		case 4:
			w = 2
		case 9:
			w = 3
		}
		segment, err := EncodeCleanup(block, w, len(block)/w)
		if err != nil {
			f.Fatal(err)
		}
		f.Add(segment, uint8(w), uint8(len(block)/w))
	}
	f.Add([]byte{0xFF, 0xFF, 0xFF, 0x02}, uint8(64), uint8(64))
	f.Fuzz(func(t *testing.T, segment []byte, w, h uint8) {
		width, height := int(w%64)+1, int(h%64)+1
		out := make([]int32, width*height)
		err := DecodeCleanup(segment, width, height, out, 31)
		if err != nil && !errors.Is(err, ErrMalformed) && !errors.Is(err, ErrUnsupported) {
			t.Fatalf("untyped error %v", err)
		}
	})
}
