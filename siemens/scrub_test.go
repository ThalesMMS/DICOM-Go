package siemens

import (
	"bytes"
	"encoding/binary"
	"errors"
	"reflect"
	"testing"
)

func TestScrubCSAClearsUninitializedBytes(t *testing.T) {
	elements := []CSAElement{
		// Encoded as the 7-byte item "ab\x00\x00\x00\x00\x00" and one pad byte.
		{Name: "PatientLeftover", VM: 1, VR: "LO", SyngoDT: 19, Items: 2, Values: []string{"ab\x00\x00\x00\x00"}},
		{Name: "SliceNormalVector", VM: 3, VR: "FD", SyngoDT: 4, Items: 3, Values: []string{"0", "0.5", "1"}},
	}
	clean, err := EncodeCSA(elements)
	if err != nil {
		t.Fatal(err)
	}
	dirty := bytes.Clone(clean)
	// Leftover memory after the name NUL, in the fourth VR byte, after the
	// value NUL and in the padding of the first element: the signature and
	// count take 16 bytes, the element header 84 and the item header 16.
	copy(dirty[16+len("PatientLeftover")+1:], "Doe^Jane")
	dirty[16+64+4+3] = 'X'
	value := 16 + 84 + 16
	copy(dirty[value+3:], "Jane")
	dirty[value+7] = 'J'
	if header, err := ParseCSA(dirty); err != nil || !reflect.DeepEqual(header.Elements, mustParse(t, clean).Elements) {
		t.Fatalf("dirty header reads differently: %v", err)
	}
	for _, tc := range []struct {
		name string
		data []byte
		swap bool
	}{{"SV10", dirty, false}, {"VS01", swapped(dirty), true}} {
		got, report, err := ScrubCSA(tc.data, CSALimits{})
		if err != nil {
			t.Fatalf("%s: %v", tc.name, err)
		}
		want := clean
		if tc.swap {
			want = swapped(clean)
		}
		if !bytes.Equal(got, want) || report.Cleared != len("Doe^Jane")+1+len("Jane")+1 || report.ByteSwapped != tc.swap || report.Format != CSAFormatSV10 {
			t.Fatalf("%s: report %+v, equal %v", tc.name, report, bytes.Equal(got, want))
		}
		if bytes.Equal(tc.data, got) {
			t.Fatalf("%s: ScrubCSA modified its input", tc.name)
		}
	}
	if got, report, err := ScrubCSA(make([]byte, 32), CSALimits{}); err != nil || report.Format != CSAFormatZeroed || !bytes.Equal(got, make([]byte, 32)) {
		t.Fatalf("zeroed: %+v %v", report, err)
	}
	for _, refused := range [][]byte{[]byte("!INTERFILE\n"), []byte("SV10\x04\x03\x02\x01\x05")} {
		if _, _, err := ScrubCSA(refused, CSALimits{}); !errors.Is(err, ErrInvalidCSA) {
			t.Fatalf("%q: err = %v", refused, err)
		}
	}
}

func TestScrubCSANOSVItemLengths(t *testing.T) {
	clean := buildCSA(false, []testElement{
		{name: "First", vm: 2, vr: "LO", items: []string{"ab\x00\x00\x00\x00\x00", "c\x00"}},
		{name: "Second", vm: 1, vr: "IS", items: []string{"567\x00"}},
	})
	// NOSV ignores x1/x3 and derives each length from x0 and the first
	// element's item count, both when parsing and when scrubbing.
	const firstItem = 8 + csaElementHeaderBytes
	binary.LittleEndian.PutUint32(clean[firstItem+4:], 1234)
	binary.LittleEndian.PutUint32(clean[firstItem+12:], 5678)
	dirty := bytes.Clone(clean)
	copy(dirty[8+len("First")+1:], "leftover")
	dirty[8+64+4+3] = 'X'
	value := firstItem + csaItemHeaderBytes
	copy(dirty[value+3:], "data")
	dirty[value+7] = 'P'
	original := bytes.Clone(dirty)

	got, report, err := ScrubCSA(dirty, CSALimits{})
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, clean) || report.Format != CSAFormatNOSV || report.Cleared != len("leftover")+1+len("data")+1 {
		t.Fatalf("report %+v, equal %v", report, bytes.Equal(got, clean))
	}
	if !reflect.DeepEqual(mustParse(t, got).Elements, mustParse(t, original).Elements) {
		t.Fatal("scrubbing changed the NOSV values")
	}
	if !bytes.Equal(dirty, original) {
		t.Fatal("ScrubCSA modified its input")
	}
}

func mustParse(t *testing.T, data []byte) *CSAHeader {
	t.Helper()
	header, err := ParseCSA(data)
	if err != nil {
		t.Fatal(err)
	}
	return header
}

func swapped(data []byte) []byte {
	out := bytes.Clone(data)
	swapWords(out)
	return out
}
