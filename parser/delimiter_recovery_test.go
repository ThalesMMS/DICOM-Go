package parser

import (
	"bytes"
	"encoding/binary"
	"errors"
	"io"
	"testing"

	"github.com/ThalesMMS/dicom-go/core"
	"github.com/ThalesMMS/dicom-go/dictionary/std"
	"github.com/ThalesMMS/dicom-go/transfer"
)

func le32(v uint32) []byte { b := make([]byte, 4); binary.LittleEndian.PutUint32(b, v); return b }

func control(g, e uint16, length uint32) []byte {
	b := make([]byte, 4)
	binary.LittleEndian.PutUint16(b[0:], g)
	binary.LittleEndian.PutUint16(b[2:], e)
	return append(b, le32(length)...)
}

func sqHeader(g, e uint16, length uint32) []byte {
	b := make([]byte, 8)
	binary.LittleEndian.PutUint16(b[0:], g)
	binary.LittleEndian.PutUint16(b[2:], e)
	copy(b[4:], "SQ")
	return append(b, le32(length)...)
}

// explicitBigShort encodes an Explicit VR Big Endian short-length element.
func explicitBigShort(g, e uint16, vr string, value []byte) []byte {
	b := make([]byte, 8, 8+len(value))
	binary.BigEndian.PutUint16(b[0:], g)
	binary.BigEndian.PutUint16(b[2:], e)
	copy(b[4:], vr)
	binary.BigEndian.PutUint16(b[6:], uint16(len(value)))
	return append(b, value...)
}

func readDelims(t *testing.T, data []byte, opts ReaderOptions) ([]core.Element, []BoundaryRecovery, error) {
	t.Helper()
	opts.Dictionary = std.Dictionary
	r := NewReader(bytes.NewReader(data), transfer.ExplicitVRLittleEndian, opts)
	elements, err := r.ReadAll()
	recs, _ := r.BoundaryRecoveries()
	return elements, recs, err
}

func reasons(recs []BoundaryRecovery) []BoundaryReason {
	out := make([]BoundaryReason, len(recs))
	for i, r := range recs {
		out[i] = r.Reason
	}
	return out
}

func TestSwappedItemTagsWithBigEndianItem(t *testing.T) {
	var data []byte
	data = append(data, sqHeader(0x0008, 0x1140, 0xffffffff)...)
	data = append(data, 0xff, 0xfe, 0xe0, 0x00, 0xff, 0xff, 0xff, 0xff) // (FEFF,00E0) undefined
	data = append(data, explicitBigShort(0x0008, 0x1150, "UI", []byte("1.2.3\x00"))...)
	data = append(data, explicitBigShort(0x0028, 0x0010, "US", []byte{0x01, 0x02})...)
	data = append(data, 0xff, 0xfe, 0xe0, 0x0d, 0, 0, 0, 0) // item delimiter, big endian
	data = append(data, 0xff, 0xfe, 0xe0, 0xdd, 0, 0, 0, 0) // swapped sequence delimiter
	data = append(data, explicitShort(0x0010, 0x0020, "LO", []byte("ID12"))...)

	if _, _, err := readDelims(t, data, ReaderOptions{}); err == nil {
		t.Fatal("strict read accepted swapped item tags")
	}
	elements, recs, err := readDelims(t, data, ReaderOptions{RecoverDelimiterAnomalies: true})
	if err != nil {
		t.Fatal(err)
	}
	// ReadAll returns the element tokens in stream order (sequences flattened).
	if len(elements) != 3 || elements[2].Header.Tag != core.NewTag(0x0010, 0x0020) {
		t.Fatalf("elements = %d", len(elements))
	}
	if raw, _ := elements[0].RawBytes(); !bytes.Equal(raw, append([]byte("1.2.3"), 0)) {
		t.Fatalf("UI = %q", raw)
	}
	// Big Endian numeric values are stored in the stream's byte order.
	if raw, _ := elements[1].RawBytes(); !bytes.Equal(raw, []byte{0x02, 0x01}) {
		t.Fatalf("US raw = % x", raw)
	}
	want := []BoundaryReason{BoundarySwappedItemTag, BoundarySwappedItemTag}
	if got := reasons(recs); len(got) != 2 || got[0] != want[0] || got[1] != want[1] {
		t.Fatalf("recoveries = %v", got)
	}
}

func TestSwappedDefinedLengthItem(t *testing.T) {
	big := explicitBigShort(0x0028, 0x0010, "US", []byte{0x01, 0x02})
	little := explicitShort(0x0028, 0x0011, "US", []byte{0x04, 0x03})
	data := sqHeader(0x0008, 0x1140, 0xffffffff)
	data = append(data, 0xff, 0xfe, 0xe0, 0x00)
	data = binary.BigEndian.AppendUint32(data, uint32(len(big)))
	data = append(data, big...)
	data = append(data, control(0xfffe, 0xe000, uint32(len(little)))...)
	data = append(data, little...)
	data = append(data, control(0xfffe, 0xe0dd, 0)...)
	data = append(data, explicitShort(0x0010, 0x0020, "LO", []byte("ID12"))...)
	r := NewReader(bytes.NewReader(data), transfer.ExplicitVRLittleEndian, ReaderOptions{RecoverDelimiterAnomalies: true})
	want := []TokenKind{TokenStartSequence, TokenStartItem, TokenElement, TokenEndItem,
		TokenStartItem, TokenElement, TokenEndItem, TokenEndSequence, TokenElement}
	for i, kind := range want {
		tok, err := r.Next()
		if err != nil || tok.Kind != kind {
			t.Fatalf("token %d = %+v, %v; want %v", i, tok, err, kind)
		}
		if kind == TokenStartItem && tok.Header.Length != core.Length(len(big)) {
			t.Fatalf("item length = %d, want %d", tok.Header.Length, len(big))
		}
		if i == 2 || i == 5 {
			wantTag, wantRaw := core.NewTag(0x0028, 0x0010), []byte{0x02, 0x01}
			if i == 5 {
				wantTag, wantRaw = core.NewTag(0x0028, 0x0011), []byte{0x04, 0x03}
			}
			if raw, _ := tok.Element.RawBytes(); tok.Header.Tag != wantTag || !bytes.Equal(raw, wantRaw) {
				t.Fatalf("element %d = %+v, raw % x", i, tok, raw)
			}
		}
		if i == 8 && tok.Header.Tag != core.NewTag(0x0010, 0x0020) {
			t.Fatalf("trailing element = %+v", tok)
		}
	}
	if _, err := r.Next(); !errors.Is(err, io.EOF) {
		t.Fatalf("end of stream = %v", err)
	}
}

func TestSwappedItemStateDoesNotLeak(t *testing.T) {
	swappedEmpty := []byte{0xff, 0xfe, 0xe0, 0x00, 0, 0, 0, 0}
	pixels := []byte{0xe0, 0x7f, 0x10, 0, 'O', 'B', 0, 0, 0xff, 0xff, 0xff, 0xff}
	pixels = append(pixels, swappedEmpty...) // Basic Offset Table
	pixels = append(pixels, control(0xfffe, 0xe000, 2)...)
	pixels = append(pixels, 1, 2)
	pixels = append(pixels, control(0xfffe, 0xe0dd, 0)...)
	pixels = append(pixels, sqHeader(0x0008, 0x1140, 0xffffffff)...)
	orphan := append(sqHeader(0x0008, 0x1140, 0xffffffff), control(0xfffe, 0xe000, 8)...)
	orphan = append(orphan, swappedEmpty...)
	for name, prefix := range map[string][]byte{"pixel offset table": pixels, "skipped nested item": orphan} {
		t.Run(name, func(t *testing.T) {
			item := explicitShort(0x0028, 0x0010, "US", []byte{0x02, 0x01})
			data := append(append([]byte(nil), prefix...), control(0xfffe, 0xe000, uint32(len(item)))...)
			data = append(data, item...)
			data = append(data, control(0xfffe, 0xe0dd, 0)...)
			elements, _, err := readDelims(t, data, ReaderOptions{RecoverDelimiterAnomalies: true})
			if err != nil || len(elements) == 0 {
				t.Fatalf("read = %v, elements = %v", err, elements)
			}
			last := elements[len(elements)-1]
			if raw, _ := last.RawBytes(); last.Header.Tag != core.NewTag(0x0028, 0x0010) || !bytes.Equal(raw, []byte{0x02, 0x01}) {
				t.Fatalf("following little-endian element = %+v, raw % x", last, raw)
			}
		})
	}
}

func TestDelimiterLengthOrphanAndGroupLengthRecovery(t *testing.T) {
	item := explicitShort(0x0008, 0x1150, "UI", []byte("1.2\x00"))
	var data []byte
	// Undefined-length sequence and item closed by delimiters with 0xFFFFFFFF lengths.
	data = append(data, sqHeader(0x0008, 0x1140, 0xffffffff)...)
	data = append(data, control(0xfffe, 0x0000, 0)[:4]...)
	data = append(data, 'U', 'L', 4, 0, 8, 0, 0, 0) // (FFFE,0000) UL 8
	data = append(data, control(0xfffe, 0xe000, 0xffffffff)...)
	data = append(data, item...)
	data = append(data, control(0xfffe, 0xe00d, 0xffffffff)...)
	data = append(data, control(0xfffe, 0xe0dd, 0)...)
	// A defined-length sequence followed by an orphan sequence delimiter.
	body := append(control(0xfffe, 0xe000, uint32(len(item))), item...)
	data = append(data, sqHeader(0x0008, 0x1115, uint32(len(body)))...)
	data = append(data, body...)
	data = append(data, control(0xfffe, 0xe0dd, 0)...)
	data = append(data, explicitShort(0x0010, 0x0020, "LO", []byte("ID12"))...)

	if _, _, err := readDelims(t, data, ReaderOptions{}); err == nil {
		t.Fatal("strict read accepted delimiter anomalies")
	}
	elements, recs, err := readDelims(t, data, ReaderOptions{RecoverDelimiterAnomalies: true})
	if err != nil {
		t.Fatal(err)
	}
	if len(elements) != 3 {
		t.Fatalf("elements = %d", len(elements))
	}
	got := reasons(recs)
	want := []BoundaryReason{BoundaryItemGroupLength, BoundaryDelimiterLengthIgnored, BoundaryOrphanDelimiter}
	if len(got) != len(want) {
		t.Fatalf("recoveries = %v", got)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("recoveries = %v, want %v", got, want)
		}
	}
	if _, _, err := readDelims(t, data, ReaderOptions{RecoverDelimiterAnomalies: true, MaxDelimiterRecoveries: 2}); !errors.Is(err, ErrMaxDelimiterRecoveries) {
		t.Fatalf("limit error = %v", err)
	}
}

func TestNestedItemHeaderAtItemEnd(t *testing.T) {
	inner := append(explicitShort(0x0008, 0x0000, "UL", le32(12)), control(0xfffe, 0xe000, 44)...)
	item := append(control(0xfffe, 0xe000, uint32(len(inner))), inner...)
	data := append(sqHeader(0x0029, 0x2a06, uint32(len(item))), item...)
	data = append(data, explicitShort(0x0010, 0x0020, "LO", []byte("ID12"))...)
	if _, _, err := readDelims(t, data, ReaderOptions{}); err == nil {
		t.Fatal("strict read accepted an item nested in an item")
	}
	elements, recs, err := readDelims(t, data, ReaderOptions{RecoverDelimiterAnomalies: true})
	if err != nil {
		t.Fatal(err)
	}
	if len(elements) != 2 || len(recs) != 1 || recs[0].Reason != BoundaryOrphanDelimiter {
		t.Fatalf("elements %d recoveries %+v", len(elements), recs)
	}
}

// FuzzDelimiterRecovery mutates sequences with delimiter anomalies: reads
// never panic and never exceed the depth, element and recovery limits.
func FuzzDelimiterRecovery(f *testing.F) {
	seed := append(sqHeader(0x0008, 0x1140, 0xffffffff), 0xff, 0xfe, 0xe0, 0x00, 0xff, 0xff, 0xff, 0xff)
	seed = append(seed, 0xff, 0xfe, 0xe0, 0x0d, 0, 0, 0, 0, 0xfe, 0xff, 0xdd, 0xe0, 0, 0, 0, 0)
	f.Add(seed)
	f.Add(append(sqHeader(0x0008, 0x1140, 16), control(0xfffe, 0xe0dd, 0xffffffff)...))
	f.Fuzz(func(t *testing.T, data []byte) {
		r := NewReader(bytes.NewReader(data), transfer.ExplicitVRLittleEndian, ReaderOptions{
			Dictionary: std.Dictionary, RecoverDelimiterAnomalies: true, MaxDelimiterRecoveries: 16,
			MaxSequenceDepth: 16, MaxElements: 4096, MaxElementBytes: 1 << 16, MaxTotalBytes: 1 << 16,
		})
		_, _ = r.ReadAll()
		recs, _ := r.BoundaryRecoveries()
		if len(recs) > 16 {
			t.Fatalf("recoveries = %d", len(recs))
		}
	})
}
