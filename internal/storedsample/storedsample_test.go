package storedsample

import (
	"encoding/binary"
	"errors"
	"testing"
)

func TestReaderInterpretsStoredSamples(t *testing.T) {
	le, be := binary.LittleEndian, binary.BigEndian
	for _, tc := range []struct {
		name                     string
		allocated, stored, high  int
		signed                   bool
		order                    binary.ByteOrder
		data                     []byte
		want                     int64
		wantMinimum, wantMaximum int64
	}{
		{"13-in-16 signed", 16, 13, 12, true, le, []byte{0x30, 0x18}, -2000, -4096, 4095},
		{"13-in-16 signed ignores bits above HighBit", 16, 13, 12, true, le, []byte{0x30, 0xF8}, -2000, -4096, 4095},
		{"12-in-16 signed", 16, 12, 11, true, le, []byte{0x00, 0x08}, -2048, -2048, 2047},
		{"12-in-16 unsigned at HighBit 15", 16, 12, 15, false, le, []byte{0x00, 0xF0}, 0xF00, 0, 4095},
		{"12-in-16 signed at HighBit 15", 16, 12, 15, true, le, []byte{0x00, 0x80}, -2048, -2048, 2047},
		{"16 signed big-endian", 16, 16, 15, true, be, []byte{0x80, 0x00}, -32768, -32768, 32767},
		{"16 unsigned maximum", 16, 16, 15, false, le, []byte{0xFF, 0xFF}, 65535, 0, 65535},
		{"8 signed minimum", 8, 8, 7, true, le, []byte{0x80}, -128, -128, 127},
		{"8 unsigned maximum", 8, 8, 7, false, le, []byte{0xFF}, 255, 0, 255},
		{"32 signed minimum", 32, 32, 31, true, le, []byte{0, 0, 0, 0x80}, -1 << 31, -1 << 31, 1<<31 - 1},
		{"32 unsigned maximum", 32, 32, 31, false, be, []byte{0xFF, 0xFF, 0xFF, 0xFF}, 1<<32 - 1, 0, 1<<32 - 1},
		{"24-in-32 signed", 32, 24, 23, true, le, []byte{0xFF, 0xFF, 0xFF, 0x00}, -1, -1 << 23, 1<<23 - 1},
		{"1-bit field", 16, 1, 0, true, le, []byte{0x01, 0x00}, -1, -1, 0},
	} {
		r, err := New(tc.allocated, tc.stored, tc.high, tc.signed, tc.order)
		if err != nil {
			t.Fatalf("%s: %v", tc.name, err)
		}
		if got := r.Value(tc.data); got != tc.want {
			t.Errorf("%s: Value = %d, want %d", tc.name, got, tc.want)
		}
		if lo, hi := r.Range(); lo != tc.wantMinimum || hi != tc.wantMaximum {
			t.Errorf("%s: Range = %d..%d, want %d..%d", tc.name, lo, hi, tc.wantMinimum, tc.wantMaximum)
		}
		if r.Width() != tc.allocated/8 || r.Signed() != tc.signed {
			t.Errorf("%s: width %d signed %v", tc.name, r.Width(), r.Signed())
		}
	}
}

// Interpretation is idempotent for values already sign-extended in their
// container, so a decoded frame read again yields the same value, and a
// value extended from BitsAllocated instead of BitsStored is not produced.
func TestReaderSignExtendsOnceFromBitsStored(t *testing.T) {
	r, err := New(16, 13, 12, true, nil)
	if err != nil {
		t.Fatal(err)
	}
	// 0x1830 is the 13-bit pattern, 0xF830 the same value already
	// sign-extended to 16 bits (int16 -2000), 0x3830 junk above HighBit.
	for _, raw := range []uint16{0x1830, 0xF830, 0x3830} {
		data := binary.LittleEndian.AppendUint16(nil, raw)
		if got := r.Value(data); got != -2000 {
			t.Fatalf("raw %#04x = %d, want -2000", raw, got)
		}
	}
}

func TestNewRefusesInvalidFormats(t *testing.T) {
	for _, tc := range []struct {
		allocated, stored, high int
		want                    error
	}{
		{1, 1, 0, ErrBitsAllocated},
		{12, 12, 11, ErrBitsAllocated},
		{64, 64, 63, ErrBitsAllocated},
		{16, 0, 15, ErrBitsStored},
		{16, 17, 16, ErrBitsStored},
		{16, 12, 10, ErrHighBit},
		{16, 12, 16, ErrHighBit},
	} {
		if _, err := New(tc.allocated, tc.stored, tc.high, false, nil); !errors.Is(err, tc.want) {
			t.Errorf("%+v: %v, want %v", tc, err, tc.want)
		}
	}
}

func TestLegacyKeepsHistoricalRepair(t *testing.T) {
	// BitsStored 0 means the whole container; an impossible HighBit means
	// BitsStored-1; an unsupported container reads 0.
	if got := Legacy(16, 0, 0, true, nil); got.Value([]byte{0xFF, 0xFF}) != -1 {
		t.Fatal("BitsStored 0 is not the whole container")
	}
	if got := Legacy(16, 12, 3, true, nil); got.Value([]byte{0x00, 0x08}) != -2048 {
		t.Fatal("impossible HighBit is not BitsStored-1")
	}
	if got := Legacy(12, 12, 11, false, nil); got.Width() != 0 || got.Value([]byte{1, 2}) != 0 {
		t.Fatal("unsupported container is readable")
	}
}
