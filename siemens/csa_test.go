package siemens

import (
	"bytes"
	"encoding/binary"
	"errors"
	"reflect"
	"strings"
	"testing"
)

type testElement struct {
	name    string
	vm      int32
	vr      string
	syngoDT int32
	items   []string // "" writes an empty item
	extra   int      // empty items appended after the values
}

// buildCSA writes a CSA blob in the documented layout; signature selects
// SV10 or NOSV.
func buildCSA(sv10 bool, elements []testElement) []byte {
	var b bytes.Buffer
	u32 := func(v uint32) { _ = binary.Write(&b, binary.LittleEndian, v) }
	if sv10 {
		b.WriteString("SV10")
		b.Write([]byte{4, 3, 2, 1})
	}
	u32(uint32(len(elements)))
	u32(77)
	var lengthOffset uint32
	if !sv10 && len(elements) > 0 {
		lengthOffset = uint32(len(elements[0].items) + elements[0].extra)
	}
	for _, e := range elements {
		name := make([]byte, 64)
		copy(name, e.name)
		b.Write(name)
		u32(uint32(e.vm))
		vr := make([]byte, 4)
		copy(vr, e.vr)
		b.Write(vr)
		u32(uint32(e.syngoDT))
		u32(uint32(len(e.items) + e.extra))
		u32(77)
		for _, item := range append(append([]string(nil), e.items...), make([]string, e.extra)...) {
			length := uint32(len(item))
			u32(length + lengthOffset)
			u32(length)
			u32(77)
			u32(length)
			b.WriteString(item)
			b.Write(make([]byte, (4-len(item)%4)%4))
		}
	}
	return b.Bytes()
}

func TestParseCSASV10AndNOSV(t *testing.T) {
	elements := []testElement{
		{name: "NumberOfImagesInMosaic", vm: 1, vr: "US", syngoDT: 10, items: []string{"18      \x00"}, extra: 5},
		{name: "SliceNormalVector", vm: 3, vr: "FD", syngoDT: 4, items: []string{"-0.03737142\x00", "-0.31458821\x00", "0.94849229\x00"}, extra: 3},
		{name: "Empty", vm: 1, vr: "DS", syngoDT: 3},
	}
	for _, sv10 := range []bool{true, false} {
		header, err := ParseCSA(buildCSA(sv10, elements))
		if err != nil {
			t.Fatal(err)
		}
		want := CSAFormatNOSV
		if sv10 {
			want = CSAFormatSV10
		}
		if header.Format != want || len(header.Elements) != 3 {
			t.Fatalf("format %s, %d elements", header.Format, len(header.Elements))
		}
		count, _ := header.Element("NumberOfImagesInMosaic")
		if n, err := count.Int(); err != nil || n != 18 || count.Items != 6 || count.VR != "US" || count.SyngoDT != 10 {
			t.Fatalf("NumberOfImagesInMosaic = %+v (%d, %v)", count, n, err)
		}
		normal, _ := header.Element("SliceNormalVector")
		values, err := normal.Float64s()
		if err != nil || len(values) != 3 || values[2] != 0.94849229 {
			t.Fatalf("SliceNormalVector = %v, %v", values, err)
		}
		if empty, ok := header.Element("Empty"); !ok || len(empty.Values) != 0 {
			t.Fatalf("Empty = %+v", empty)
		}
	}
}

func TestParseCSANOSVItemLengths(t *testing.T) {
	blob := buildCSA(false, []testElement{
		{name: "First", vm: 2, vr: "IS", items: []string{"12", "34"}},
		{name: "Second", vm: 1, vr: "IS", items: []string{"567"}},
	})
	// NOSV uses x0 minus the first tag's item count, regardless of x1/x3.
	const firstItem = 8 + csaElementHeaderBytes
	binary.LittleEndian.PutUint32(blob[firstItem+4:], 1234)
	binary.LittleEndian.PutUint32(blob[firstItem+12:], 5678)
	header, err := ParseCSA(blob)
	if err != nil {
		t.Fatal(err)
	}
	if header.Format != CSAFormatNOSV || len(header.Elements) != 2 ||
		strings.Join(header.Elements[0].Values, ",") != "12,34" ||
		strings.Join(header.Elements[1].Values, ",") != "567" {
		t.Fatalf("parsed NOSV header = %+v", header)
	}
	binary.LittleEndian.PutUint32(blob[firstItem:], 1)
	if _, err := ParseCSA(blob); !errors.Is(err, ErrInvalidCSA) {
		t.Fatalf("negative NOSV item length: %v", err)
	}
}

func TestParseCSAByteSwappedZeroedInterfileAndDataset(t *testing.T) {
	blob := buildCSA(true, []testElement{{name: "A", vm: 1, vr: "IS", syngoDT: 6, items: []string{"7"}}})
	swapped := make([]byte, len(blob))
	for i := 0; i+1 < len(blob); i += 2 {
		swapped[i], swapped[i+1] = blob[i+1], blob[i]
	}
	header, err := ParseCSA(swapped)
	if err != nil || !header.ByteSwapped || header.Format != CSAFormatSV10 {
		t.Fatalf("byte-swapped: %+v, %v", header, err)
	}
	if header, err := ParseCSA(make([]byte, 32)); err != nil || header.Format != CSAFormatZeroed {
		t.Fatalf("zeroed: %+v, %v", header, err)
	}
	if header, err := ParseCSA([]byte("!INTERFILE :=\r\n\x00\x00")); err != nil || header.Format != CSAFormatInterfile || !strings.HasPrefix(header.Text, "!INTERFILE") {
		t.Fatalf("interfile: %+v, %v", header, err)
	}
	if header, err := ParseCSA([]byte("<pds><com>gm:1094</com></pds>\x00")); err != nil || header.Format != CSAFormatXML || header.Text != "<pds><com>gm:1094</com></pds>" {
		t.Fatalf("xml: %+v, %v", header, err)
	}
	// (0000,0000) UL 4 bytes, then (FFFF,FFFF) CS "END!".
	dataset := []byte{0, 0, 0, 0, 'U', 'L', 4, 0, 1, 0, 0, 0, 0xff, 0xff, 0xff, 0xff, 'C', 'S', 4, 0, 'E', 'N', 'D', '!'}
	if header, err := ParseCSA(dataset); err != nil || header.Format != CSAFormatDataset || header.Dataset == nil {
		t.Fatalf("dataset: %+v, %v", header, err)
	}
}

func TestParseCSARejectsMalformedAndOversized(t *testing.T) {
	valid := buildCSA(true, []testElement{{name: "A", vm: 1, vr: "IS", syngoDT: 6, items: []string{"7"}}})
	cases := map[string][]byte{
		"short":        {1, 2},
		"truncated":    valid[:len(valid)-6],
		"bad-marker":   append(append([]byte(nil), valid[:12]...), append([]byte{78, 0, 0, 0}, valid[16:]...)...),
		"huge-count":   append(append([]byte("SV10\x04\x03\x02\x01"), 0xff, 0xff, 0, 0), 77, 0, 0, 0),
		"count-no-fit": append(append([]byte("SV10\x04\x03\x02\x01"), 10, 0, 0, 0), 77, 0, 0, 0),
	}
	for name, data := range cases {
		if _, err := ParseCSA(data); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
	if _, err := ParseCSAWithLimits(valid, CSALimits{MaxBytes: 16}); !errors.Is(err, ErrCSALimit) {
		t.Errorf("MaxBytes: %v", err)
	}
	if _, err := ParseCSAWithLimits(valid, CSALimits{MaxElements: 0, MaxItems: 0}); err != nil {
		t.Errorf("defaults: %v", err)
	}
	many := buildCSA(true, []testElement{{name: "A", vm: 1, vr: "IS", syngoDT: 6, items: []string{"1", "2", "3"}}})
	if _, err := ParseCSAWithLimits(many, CSALimits{MaxItems: 2}); !errors.Is(err, ErrCSALimit) {
		t.Errorf("MaxItems: %v", err)
	}
}

func FuzzParseCSA(f *testing.F) {
	f.Add(buildCSA(true, []testElement{{name: "NumberOfImagesInMosaic", vm: 1, vr: "US", syngoDT: 10, items: []string{"18"}, extra: 5}}))
	f.Add(buildCSA(false, []testElement{{name: "A", vm: 3, vr: "FD", syngoDT: 4, items: []string{"1", "2", "3"}}}))
	f.Add([]byte("!INTERFILE"))
	f.Add([]byte{0, 0, 0, 0, 'U', 'L', 4, 0, 1, 0, 0, 0})
	f.Fuzz(func(t *testing.T, data []byte) {
		header, err := ParseCSAWithLimits(data, CSALimits{MaxBytes: 1 << 20})
		if err != nil {
			return
		}
		// Every item costs at least 16 bytes, so values cannot outnumber
		// the input.
		values := 0
		for _, element := range header.Elements {
			values += len(element.Values)
		}
		if values*16 > len(data) {
			t.Fatalf("%d values from %d bytes", values, len(data))
		}
		// A scrubbed header keeps its length and reads the same, and a
		// second scrub changes nothing.
		scrubbed, _, err := ScrubCSA(data, CSALimits{MaxBytes: 1 << 20})
		if err != nil {
			return
		}
		again, err := ParseCSAWithLimits(scrubbed, CSALimits{MaxBytes: 1 << 20})
		if len(scrubbed) != len(data) || err != nil || !reflect.DeepEqual(again.Elements, header.Elements) {
			t.Fatalf("scrubbed header differs: %v", err)
		}
		if twice, _, _ := ScrubCSA(scrubbed, CSALimits{MaxBytes: 1 << 20}); !bytes.Equal(twice, scrubbed) {
			t.Fatal("scrub is not idempotent")
		}
	})
}

func TestEncodeCSARoundTrip(t *testing.T) {
	elements := []CSAElement{
		{Name: "NumberOfImagesInMosaic", VM: 1, VR: "US", SyngoDT: 10, Items: 6, Values: []string{"18"}},
		{Name: "SliceNormalVector", VM: 3, VR: "FD", SyngoDT: 4, Items: 6, Values: []string{"0", "0.5", "1"}},
	}
	blob, err := EncodeCSA(elements)
	if err != nil {
		t.Fatal(err)
	}
	header, err := ParseCSA(blob)
	if err != nil {
		t.Fatal(err)
	}
	if header.Format != CSAFormatSV10 || len(header.Elements) != 2 {
		t.Fatalf("%+v", header)
	}
	for i, want := range elements {
		got := header.Elements[i]
		if got.Name != want.Name || got.VM != want.VM || got.VR != want.VR || got.SyngoDT != want.SyngoDT || got.Items != want.Items || strings.Join(got.Values, ",") != strings.Join(want.Values, ",") {
			t.Fatalf("element %d = %+v, want %+v", i, got, want)
		}
	}
	if _, err := EncodeCSA([]CSAElement{{Name: strings.Repeat("x", 64)}}); err == nil {
		t.Fatal("encoded a 64-byte name")
	}
}
