package parser

import (
	"bytes"
	"encoding/binary"
	"errors"
	"testing"

	"github.com/ThalesMMS/dicom-go/core"
	"github.com/ThalesMMS/dicom-go/dictionary/std"
	"github.com/ThalesMMS/dicom-go/transfer"
)

// Explicit VR Little Endian builders for container-boundary fixtures.

func bElement(group, element uint16, vr string, value []byte) []byte {
	var b bytes.Buffer
	_ = binary.Write(&b, binary.LittleEndian, group)
	_ = binary.Write(&b, binary.LittleEndian, element)
	b.WriteString(vr)
	switch vr {
	case "OB", "OW", "SQ", "UN", "UT":
		b.Write([]byte{0, 0})
		_ = binary.Write(&b, binary.LittleEndian, uint32(len(value)))
	default:
		_ = binary.Write(&b, binary.LittleEndian, uint16(len(value)))
	}
	b.Write(value)
	return b.Bytes()
}

func bControl(element uint16, length uint32) []byte {
	var b bytes.Buffer
	_ = binary.Write(&b, binary.LittleEndian, uint16(0xFFFE))
	_ = binary.Write(&b, binary.LittleEndian, element)
	_ = binary.Write(&b, binary.LittleEndian, length)
	return b.Bytes()
}

// bItem frames body as a defined-length item that declares overrun extra
// bytes.
func bItem(body []byte, overrun uint32) []byte {
	return append(bControl(0xE000, uint32(len(body))+overrun), body...)
}

func bSequence(group, element uint16, length uint32, content []byte) []byte {
	var b bytes.Buffer
	_ = binary.Write(&b, binary.LittleEndian, group)
	_ = binary.Write(&b, binary.LittleEndian, element)
	b.WriteString("SQ")
	b.Write([]byte{0, 0})
	_ = binary.Write(&b, binary.LittleEndian, length)
	b.Write(content)
	return b.Bytes()
}

var (
	code01   = bElement(0x0008, 0x0100, "SH", []byte("CODE01  "))
	code02   = bElement(0x0008, 0x0100, "SH", []byte("CODE02  "))
	sentinel = bElement(0x0010, 0x0010, "PN", []byte("SENTINEL"))
)

func readBoundary(t *testing.T, stream []byte, recover bool) (core.DataSet, []BoundaryRecovery, error) {
	t.Helper()
	reader := NewReader(bytes.NewReader(stream), transfer.ExplicitVRLittleEndian, ReaderOptions{Dictionary: std.Dictionary, RecoverContainerOverruns: recover})
	ds, err := reader.ReadDataSet()
	recoveries, _ := reader.BoundaryRecoveries()
	return ds, recoveries, err
}

func sequenceItems(t *testing.T, ds core.DataSet, tag core.Tag) []core.DataSet {
	t.Helper()
	for _, element := range ds.Elements {
		if element.Tag() == tag {
			sequence, ok := element.Value.(core.SequenceValue)
			if !ok {
				t.Fatalf("%s is %T", tag, element.Value)
			}
			return sequence.Items
		}
	}
	t.Fatalf("%s missing", tag)
	return nil
}

func lastString(ds core.DataSet) string {
	raw, _ := ds.Elements[len(ds.Elements)-1].RawBytes()
	return string(raw)
}

// amrshadid/go-dicom#122: the last item of a defined-length sequence declares
// more bytes than the sequence holds, while its elements are complete.
func TestItemOverrunningItsSequence(t *testing.T) {
	items := append(bItem(code01, 0), bItem(code02, 24)...)
	stream := bytes.Join([][]byte{
		bElement(0x0008, 0x0060, "CS", []byte("MR")),
		bSequence(0x0004, 0x1220, uint32(len(items)), items),
		sentinel,
	}, nil)
	sequenceEnd := len(stream) - len(sentinel)

	_, _, err := readBoundary(t, stream, false)
	var parseErr *ParseError
	if !errors.Is(err, ErrContainerOverrun) || !errors.As(err, &parseErr) || parseErr.Offset > int64(sequenceEnd) {
		t.Fatalf("strict: %v", err)
	}

	ds, recoveries, err := readBoundary(t, stream, true)
	if err != nil {
		t.Fatal(err)
	}
	if len(ds.Elements) != 3 {
		t.Fatalf("%d top-level elements", len(ds.Elements))
	}
	records := sequenceItems(t, ds, core.NewTag(0x0004, 0x1220))
	if len(records) != 2 || lastString(records[1]) != "CODE02  " {
		t.Fatalf("items %d", len(records))
	}
	if got := lastString(ds); got != "SENTINEL" {
		t.Fatalf("element after the sequence read %q", got)
	}
	want := BoundaryRecovery{Path: "(0004,1220)[1]", Offset: int64(sequenceEnd - len(code02)), DeclaredLength: int64(len(code02) + 24), AvailableLength: int64(len(code02)), Reason: BoundaryItemOverrun}
	if len(recoveries) != 1 || recoveries[0] != want {
		t.Fatalf("recoveries %+v, want %+v", recoveries, want)
	}
}

// An item header whose body lies outside the sequence is not an item.
func TestItemHeaderAtTheSequenceEndIsNotAnItem(t *testing.T) {
	items := append(bItem(code01, 0), bControl(0xE000, 16)...)
	stream := append(bSequence(0x0004, 0x1220, uint32(len(items)), items), sentinel...)
	if _, _, err := readBoundary(t, stream, false); !errors.Is(err, ErrContainerOverrun) {
		t.Fatalf("strict: %v", err)
	}
	ds, recoveries, err := readBoundary(t, stream, true)
	if err != nil {
		t.Fatal(err)
	}
	if records := sequenceItems(t, ds, core.NewTag(0x0004, 0x1220)); len(records) != 1 {
		t.Fatalf("%d items: a phantom item was recorded", len(records))
	}
	if lastString(ds) != "SENTINEL" || len(recoveries) != 1 || recoveries[0].Reason != BoundaryItemHeaderAtEnd || recoveries[0].Path != "(0004,1220)[1]" {
		t.Fatalf("recoveries %+v", recoveries)
	}
}

// Values, fragments and headers that cross a container end are refused in
// every mode, and nothing outside the container is read as their content.
func TestTruncatedContentIsNeverRecovered(t *testing.T) {
	// The second element's value claims 32 bytes but only 8 remain.
	truncatedValue := append(append([]byte(nil), code01...), bElement(0x0008, 0x0104, "LO", make([]byte, 8))...)
	binary.LittleEndian.PutUint16(truncatedValue[len(code01)+6:], 32)
	// Four bytes of a header, then the sequence ends.
	partialHeader := append(append([]byte(nil), code01...), 0x08, 0x00, 0x04, 0x01)
	for name, body := range map[string][]byte{"truncated value": truncatedValue, "partial header": partialHeader} {
		item := bItem(body, 0)
		stream := append(bSequence(0x0008, 0x1115, uint32(len(item)), item), sentinel...)
		for _, recover := range []bool{false, true} {
			_, _, err := readBoundary(t, stream, recover)
			if !errors.Is(err, ErrContainerOverrun) {
				t.Fatalf("%s (recover=%v): %v", name, recover, err)
			}
		}
	}
	// An overrunning item whose clamped content ends inside an element is
	// ambiguous: the element is refused, nothing is filled in.
	short := bItem(append(append([]byte(nil), code01...), bElement(0x0008, 0x0104, "LO", []byte("ABCDEFGH"))[:12]...), 20)
	stream := append(bSequence(0x0008, 0x1115, uint32(len(short)), short), sentinel...)
	if _, _, err := readBoundary(t, stream, true); !errors.Is(err, ErrContainerOverrun) {
		t.Fatalf("ambiguous boundary: %v", err)
	}
}

// Nested containers: an overrunning item inside an item of an outer
// sequence, a nested sequence overrunning its item, and an undefined-length
// item left open at the end of a defined-length sequence.
func TestNestedAndUndefinedLengthBoundaries(t *testing.T) {
	inner := bItem(code02, 40)
	innerSequence := bSequence(0x0040, 0xA730, uint32(len(inner)), inner)
	outerItem := bItem(append(append([]byte(nil), code01...), innerSequence...), 0)
	stream := append(bSequence(0x0040, 0xA730, uint32(len(outerItem)), outerItem), sentinel...)
	ds, recoveries, err := readBoundary(t, stream, true)
	if err != nil || lastString(ds) != "SENTINEL" || len(recoveries) != 1 || recoveries[0].Path != "(0040,A730)[0].(0040,A730)[0]" {
		t.Fatalf("nested item: %+v %v", recoveries, err)
	}

	// The nested sequence itself declares more than its item holds.
	overSequence := bSequence(0x0040, 0xA730, uint32(len(bItem(code02, 0)))+16, bItem(code02, 0))
	outerItem = bItem(append(append([]byte(nil), code01...), overSequence...), 0)
	stream = append(bSequence(0x0040, 0xA730, uint32(len(outerItem)), outerItem), sentinel...)
	if _, _, err := readBoundary(t, stream, false); !errors.Is(err, ErrContainerOverrun) {
		t.Fatalf("strict nested sequence: %v", err)
	}
	ds, recoveries, err = readBoundary(t, stream, true)
	if err != nil || lastString(ds) != "SENTINEL" || len(recoveries) != 1 || recoveries[0].Reason != BoundarySequenceOverrun || recoveries[0].Path != "(0040,A730)[0].(0040,A730)" {
		t.Fatalf("nested sequence: %+v %v", recoveries, err)
	}

	// Undefined-length item without its delimiter inside a defined sequence.
	open := append(bControl(0xE000, 0xFFFFFFFF), code01...)
	stream = append(bSequence(0x0008, 0x1115, uint32(len(open)), open), sentinel...)
	if _, _, err := readBoundary(t, stream, false); !errors.Is(err, ErrContainerOverrun) {
		t.Fatalf("strict open item: %v", err)
	}
	ds, recoveries, err = readBoundary(t, stream, true)
	if err != nil || lastString(ds) != "SENTINEL" || len(sequenceItems(t, ds, core.NewTag(0x0008, 0x1115))) != 1 ||
		len(recoveries) != 1 || recoveries[0].Reason != BoundaryMissingDelimiter || recoveries[0].DeclaredLength != -1 {
		t.Fatalf("open item: %+v %v", recoveries, err)
	}

	// Well-formed data produces no recoveries in either mode.
	good := append(bItem(code01, 0), bItem(code02, 0)...)
	stream = append(bSequence(0x0008, 0x1115, uint32(len(good)), good), sentinel...)
	for _, recover := range []bool{false, true} {
		if _, recoveries, err := readBoundary(t, stream, recover); err != nil || len(recoveries) != 0 {
			t.Fatalf("clean (recover=%v): %+v %v", recover, recoveries, err)
		}
	}
}

// A directory-record-like sequence whose last IMAGE record overruns keeps
// every record under recovery; limits and the diagnostic cap still apply.
func TestDirectoryRecordOverrunKeepsImageRecordWithinLimits(t *testing.T) {
	record := func(kind string, overrun uint32) []byte {
		body := append(bElement(0x0004, 0x1430, "CS", []byte(kind)), bElement(0x0004, 0x1500, "CS", []byte("IMG01 "))...)
		return bItem(body, overrun)
	}
	var records []byte
	for i := 0; i < 5; i++ {
		records = append(records, record("SERIES", 0)...)
	}
	records = append(records, record("IMAGE ", 64)...)
	stream := append(bSequence(0x0004, 0x1220, uint32(len(records)), records), sentinel...)
	ds, recoveries, err := readBoundary(t, stream, true)
	if err != nil {
		t.Fatal(err)
	}
	items := sequenceItems(t, ds, core.NewTag(0x0004, 0x1220))
	if len(items) != 6 {
		t.Fatalf("%d records", len(items))
	}
	if raw, _ := items[5].Elements[0].RawBytes(); string(raw) != "IMAGE " || len(recoveries) != 1 {
		t.Fatalf("IMAGE record lost: %q %+v", raw, recoveries)
	}
	// Recovery does not lift limits.
	reader := NewReader(bytes.NewReader(stream), transfer.ExplicitVRLittleEndian, ReaderOptions{Dictionary: std.Dictionary, RecoverContainerOverruns: true, MaxElements: 5})
	if _, err := reader.ReadDataSet(); !errors.Is(err, ErrMaxElementsExceeded) {
		t.Fatalf("element limit: %v", err)
	}
	// The diagnostic list is capped and says so.
	var capped []byte
	for i := 0; i < 3; i++ {
		item := bItem(code01, 8)
		capped = append(capped, bSequence(0x0008, uint16(0x1110+2*i), uint32(len(item)), item)...)
	}
	reader = NewReader(bytes.NewReader(capped), transfer.ExplicitVRLittleEndian, ReaderOptions{Dictionary: std.Dictionary, RecoverContainerOverruns: true, MaxBoundaryRecoveries: 2})
	if _, err := reader.ReadDataSet(); err != nil {
		t.Fatal(err)
	}
	if list, truncated := reader.BoundaryRecoveries(); len(list) != 2 || !truncated {
		t.Fatalf("cap: %d %v", len(list), truncated)
	}
}

// FuzzContainerBoundaryRecovery: recovery never panics, never changes the
// result of input the strict reader accepts, and only reports anomalies whose
// declared length exceeds what the container had.
func FuzzContainerBoundaryRecovery(f *testing.F) {
	items := append(bItem(code01, 0), bItem(code02, 24)...)
	f.Add(append(bSequence(0x0004, 0x1220, uint32(len(items)), items), sentinel...))
	header := append(bItem(code01, 0), bControl(0xE000, 16)...)
	f.Add(append(bSequence(0x0004, 0x1220, uint32(len(header)), header), sentinel...))
	open := append(bControl(0xE000, 0xFFFFFFFF), code01...)
	f.Add(append(bSequence(0x0008, 0x1115, uint32(len(open)), open), sentinel...))
	good := append(bItem(code01, 0), bItem(code02, 0)...)
	f.Add(append(bSequence(0x0008, 0x1115, uint32(len(good)), good), sentinel...))
	// Deep nesting inside one defined-length container (#1020): within and
	// beyond the depth limit below.
	f.Add(deepNestingStream(15, true))
	f.Add(deepNestingStream(40, true))
	f.Fuzz(func(t *testing.T, stream []byte) {
		if len(stream) > 1<<16 {
			return
		}
		read := func(recover bool) (core.DataSet, []BoundaryRecovery, error) {
			reader := NewReader(bytes.NewReader(stream), transfer.ExplicitVRLittleEndian, ReaderOptions{
				Dictionary: std.Dictionary, RecoverContainerOverruns: recover,
				MaxElements: 4096, MaxSequenceDepth: 32, MaxElementBytes: 1 << 16,
			})
			ds, err := reader.ReadDataSet()
			recoveries, _ := reader.BoundaryRecoveries()
			return ds, recoveries, err
		}
		strict, strictRecoveries, strictErr := read(false)
		recovered, recoveries, recoveredErr := read(true)
		if len(strictRecoveries) != 0 {
			t.Fatal("strict reader recorded recoveries")
		}
		if strictErr == nil {
			if recoveredErr != nil || len(recoveries) != 0 || len(strict.Elements) != len(recovered.Elements) {
				t.Fatalf("recovery changed a strict read: %v %+v", recoveredErr, recoveries)
			}
		}
		for _, r := range recoveries {
			if r.DeclaredLength != -1 && r.DeclaredLength <= r.AvailableLength || r.Offset < 0 || r.Offset > int64(len(stream)) {
				t.Fatalf("inconsistent recovery %+v", r)
			}
		}
	})
}
