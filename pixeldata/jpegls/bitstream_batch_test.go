package jpegls

import (
	"errors"
	"io"
	"testing"
)

// The bit-at-a-time reference retains the former readBits semantics, including
// the position after stuffed bytes, an invalid marker and truncated input.
func referenceReadBits(reader *bitReader, n int) (uint32, error) {
	var value uint32
	for i := 0; i < n; i++ {
		bit, err := reader.readBit()
		if err != nil {
			return 0, err
		}
		value = (value << 1) | bit
	}
	return value, nil
}

// consumedBits is the number of entropy bits a reader has consumed: the
// bits of the bytes it loaded (7 for a byte after 0xFF, 8 otherwise) less
// those still pending.
func consumedBits(r *bitReader) int {
	loaded := 0
	afterFF := false
	for _, b := range r.buf[:r.pos] {
		if afterFF {
			loaded += 7
		} else {
			loaded += 8
		}
		afterFF = !afterFF && b == 0xff
	}
	return loaded - r.count
}

func checkBatchedReadBits(t testing.TB, raw []byte, widths []byte) {
	t.Helper()
	actual := bitReader{buf: raw}
	reference := bitReader{buf: raw}
	for _, width := range widths {
		n := int(width % 41)
		got, gotErr := actual.readBits(n)
		want, wantErr := referenceReadBits(&reference, n)
		if got != want || (gotErr == nil) != (wantErr == nil) || errors.Is(gotErr, io.ErrUnexpectedEOF) != errors.Is(wantErr, io.ErrUnexpectedEOF) || errors.Is(gotErr, ErrInvalidCodestream) != errors.Is(wantErr, ErrInvalidCodestream) {
			t.Fatalf("readBits(%d): result/error differs", n)
		}
		// The readers may have loaded different amounts ahead; what must
		// agree is how many entropy bits have been consumed.
		if consumedBits(&actual) != consumedBits(&reference) {
			t.Fatalf("readBits(%d): consumed %d bits, reference %d", n, consumedBits(&actual), consumedBits(&reference))
		}
	}
}

func TestReadBitsBatchPreservesStuffingAndFailureState(t *testing.T) {
	for _, raw := range [][]byte{nil, {0}, {0xff}, {0xff, 0x7f, 0xff, 0, 0x12, 0x34}, {0xff, 0x80}, {0x55, 0xaa, 0x81, 0x00, 0xfe, 0xff, 0x7f}} {
		for prefix := byte(0); prefix < 9; prefix++ {
			for width := byte(0); width <= 40; width++ {
				checkBatchedReadBits(t, raw, []byte{prefix, width, 7, 1, 8, 32, 0})
			}
		}
	}
}

func FuzzReadBitsBatchEquivalent(f *testing.F) {
	f.Add([]byte{0xff, 0x7f, 0x55, 0xaa}, []byte{3, 8, 1, 16})
	f.Add([]byte{0xff, 0x80}, []byte{7, 2, 8})
	f.Fuzz(func(t *testing.T, raw, widths []byte) {
		if len(raw) > 4096 || len(widths) > 256 {
			return
		}
		checkBatchedReadBits(t, raw, widths)
	})
}
