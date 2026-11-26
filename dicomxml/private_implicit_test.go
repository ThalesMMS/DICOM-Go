package dicomxml

import (
	"bytes"
	"encoding/binary"
	"errors"
	"strings"
	"testing"

	"github.com/ThalesMMS/dicom-go/core"
	"github.com/ThalesMMS/dicom-go/dicomjson"
	"github.com/ThalesMMS/dicom-go/dictionary/std"
	"github.com/ThalesMMS/dicom-go/object"
	"github.com/ThalesMMS/dicom-go/transfer"
)

// implicitElement encodes one Implicit VR Little Endian element; a nil value
// with undefined length opens a sequence closed by the items that follow.
func implicitElement(group, element uint16, value []byte) []byte {
	out := binary.LittleEndian.AppendUint16(nil, group)
	out = binary.LittleEndian.AppendUint16(out, element)
	out = binary.LittleEndian.AppendUint32(out, uint32(len(value)))
	return append(out, value...)
}

// implicitSequence encodes an undefined-length sequence of defined-length
// items, as a private SQ appears in Implicit VR.
func implicitSequence(group, element uint16, items ...[]byte) []byte {
	out := binary.LittleEndian.AppendUint16(nil, group)
	out = binary.LittleEndian.AppendUint16(out, element)
	out = binary.LittleEndian.AppendUint32(out, 0xffffffff)
	for _, item := range items {
		out = append(out, 0xfe, 0xff, 0x00, 0xe0)
		out = binary.LittleEndian.AppendUint32(out, uint32(len(item)))
		out = append(out, item...)
	}
	return append(out, 0xfe, 0xff, 0xdd, 0xe0, 0, 0, 0, 0)
}

func readImplicit(t *testing.T, parts ...[]byte) *object.Object {
	t.Helper()
	obj, err := object.ReadDataSet(bytes.NewReader(bytes.Join(parts, nil)), transfer.ImplicitVRLittleEndian)
	if err != nil {
		t.Fatal(err)
	}
	return obj
}

// privateImplicitObject holds, in Implicit VR, what the gdcmData Philips and
// Siemens files do: creators and a private sequence read as UN, an element in
// the illegal odd group 0003, and a private element at block offset 00.
func privateImplicitObject(t *testing.T) *object.Object {
	return readImplicit(t,
		implicitElement(0x0003, 0x3000, []byte("1.2.3.4\x00")),
		implicitElement(0x0008, 0x0060, []byte("MR")),
		implicitElement(0x2001, 0x0010, []byte("Philips Imaging DD 001")),
		implicitElement(0x2001, 0x1000, []byte("AB")),
		implicitSequence(0x2001, 0x105f,
			bytes.Join([][]byte{implicitElement(0x0020, 0x0013, []byte("1 ")), implicitElement(0x2005, 0x0010, []byte("Philips MR Imaging DD 001 ")), implicitElement(0x2005, 0x1071, []byte{0, 0, 0x80, 0x3f})}, nil),
			implicitElement(0x0020, 0x0013, []byte("2 ")),
		),
	)
}

func TestImplicitPrivateSequenceAndCreatorsRoundTrip(t *testing.T) {
	obj := privateImplicitObject(t)
	if e, _ := obj.Get(core.NewTag(0x2001, 0x105f)); e.VR() != core.VRUN {
		t.Fatalf("fixture: the private sequence was read as %s, want UN", e.VR())
	}
	data, err := Marshal(obj, DefaultOptions())
	if err != nil {
		t.Fatal(err)
	}
	text := string(data)
	for _, want := range []string{
		`tag="00033000" vr="UN"`,
		`tag="20010010" vr="LO"`,
		`privateCreator="Philips Imaging DD 001" tag="20010000"`,
		`privateCreator="Philips Imaging DD 001" tag="2001005F" vr="SQ"`,
		`privateCreator="Philips MR Imaging DD 001" tag="20050071"`,
	} {
		if !strings.Contains(text, want) {
			t.Fatalf("XML lacks %s:\n%s", want, text)
		}
	}
	back, err := Unmarshal(data, std.Dictionary)
	if err != nil {
		t.Fatal(err)
	}
	if v, _ := back.GetString(core.NewTag(0x2001, 0x0010)); v != "Philips Imaging DD 001" {
		t.Fatalf("creator %q", v)
	}
	if raw, _ := back.GetRaw(core.NewTag(0x2001, 0x1000)); string(raw) != "AB" {
		t.Fatalf("offset-00 private element %q", raw)
	}
	items, ok := back.GetSequence(core.NewTag(0x2001, 0x105f))
	if !ok || len(items) != 2 {
		t.Fatalf("private sequence: %d items, %v", len(items), ok)
	}
	if v, _ := items[1].GetString(core.NewTag(0x0020, 0x0013)); v != "2" {
		t.Fatalf("item 2 Instance Number %q", v)
	}
	if raw, _ := items[0].GetRaw(core.NewTag(0x2005, 0x1071)); !bytes.Equal(raw, []byte{0, 0, 0x80, 0x3f}) {
		t.Fatalf("nested private element %v", raw)
	}
	var written bytes.Buffer
	if err := object.WriteDataSet(&written, back, transfer.ExplicitVRLittleEndian); err != nil {
		t.Fatal(err)
	}
	again, err := object.ReadDataSet(&written, transfer.ExplicitVRLittleEndian)
	if err != nil {
		t.Fatal(err)
	}
	if items, _ := again.GetSequence(core.NewTag(0x2001, 0x105f)); len(items) != 2 {
		t.Fatalf("written DICOM holds %d items", len(items))
	}
}

func TestPrivateRecoveryIsOptInAndReported(t *testing.T) {
	obj := readImplicit(t,
		implicitElement(0x0009, 0x0010, []byte("GEIIS\x00")),
		implicitElement(0x0009, 0x1010, []byte("ab")),
		implicitElement(0x0019, 0x1214, []byte("cd")),
		implicitElement(0x0021, 0x0010, []byte("DUP ")),
		implicitElement(0x0021, 0x0011, []byte("DUP ")),
		implicitElement(0x0021, 0x1001, []byte("e1")),
		implicitElement(0x0021, 0x1101, []byte("e2")),
		implicitElement(0x0021, 0x1102, []byte("e3")),
	)
	if _, err := Marshal(obj, DefaultOptions()); !errors.Is(err, ErrInvalidXML) || !strings.Contains(err.Error(), "00191214']") {
		// The creator's trailing NUL is padding since #2032; the element
		// without a creator still fails strict export.
		t.Fatalf("strict export of an element without a creator: %v", err)
	}
	opts := DefaultOptions()
	opts.RecoverPrivate = true
	if _, err := Marshal(obj, opts); !errors.Is(err, ErrInvalidXML) {
		t.Fatalf("RecoverPrivate without a report: %v", err)
	}
	var got []PrivateRecovery
	opts.PrivateRecovered = func(r PrivateRecovery) { got = append(got, r) }
	data, err := Marshal(obj, opts)
	if err != nil {
		t.Fatal(err)
	}
	want := []PrivateRecovery{
		{Kind: PrivateElementOmitted, Path: "/NativeDicomModel/DicomAttribute[@tag='00191214']", Tag: core.NewTag(0x0019, 0x1214)},
		{Kind: PrivateElementOmitted, Path: "/NativeDicomModel/DicomAttribute[@tag='00211101']", Tag: core.NewTag(0x0021, 0x1101), Creator: "DUP"},
		{Kind: PrivateElementMerged, Path: "/NativeDicomModel/DicomAttribute[@tag='00211102']", Tag: core.NewTag(0x0021, 0x1102), Creator: "DUP"},
	}
	if len(got) != len(want) {
		t.Fatalf("recoveries %+v", got)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("recovery %d: %+v, want %+v", i, got[i], want[i])
		}
	}
	back, err := Unmarshal(data, std.Dictionary)
	if err != nil {
		t.Fatal(err)
	}
	if v, _ := back.GetString(core.NewTag(0x0009, 0x0010)); v != "GEIIS" {
		t.Fatalf("recovered creator %q", v)
	}
	for tag, value := range map[core.Tag]string{core.NewTag(0x0009, 0x1010): "ab", core.NewTag(0x0021, 0x1001): "e1", core.NewTag(0x0021, 0x1002): "e3"} {
		if raw, _ := back.GetRaw(tag); string(raw) != value {
			t.Fatalf("%s: %q, want %q", tag, raw, value)
		}
	}
	if back.Has(core.NewTag(0x0019, 0x1214)) || back.Has(core.NewTag(0x0021, 0x1101)) {
		t.Fatal("an omitted element was written")
	}

	strict := readImplicit(t,
		implicitElement(0x0021, 0x0010, []byte("DUP ")),
		implicitElement(0x0021, 0x0011, []byte("DUP ")),
		implicitElement(0x0021, 0x1102, []byte("e3")),
	)
	if _, err := Marshal(strict, DefaultOptions()); !errors.Is(err, ErrInvalidXML) || !strings.Contains(err.Error(), "cannot tell apart") {
		t.Fatalf("strict export of a repeated creator: %v", err)
	}
}

func TestUnmarshalKeepsCreatorSlotsAndFirstBlock(t *testing.T) {
	// A creator slot kept as UN is occupied, and a creator repeated in the
	// group resolves to its first block.
	doc := `<?xml version="1.0" encoding="UTF-8"?>
<NativeDicomModel xmlns="http://dicom.nema.org/PS3.19/models/NativeDICOM" xml:space="preserve">
<DicomAttribute tag="00090010" vr="UN"><InlineBinary>AAEC</InlineBinary></DicomAttribute>
<DicomAttribute tag="00090011" vr="LO"><Value number="1">X</Value></DicomAttribute>
<DicomAttribute tag="00090012" vr="LO"><Value number="1">X</Value></DicomAttribute>
<DicomAttribute tag="00090001" vr="LO" privateCreator="NEW"><Value number="1">n</Value></DicomAttribute>
<DicomAttribute tag="00090002" vr="LO" privateCreator="X"><Value number="1">x</Value></DicomAttribute>
</NativeDicomModel>`
	back, err := Unmarshal([]byte(doc), std.Dictionary)
	if err != nil {
		t.Fatal(err)
	}
	if v, _ := back.GetString(core.NewTag(0x0009, 0x1102)); v != "x" {
		t.Fatalf("repeated creator resolved elsewhere: %q", v)
	}
	if v, _ := back.GetString(core.NewTag(0x0009, 0x0013)); v != "NEW" {
		t.Fatalf("new creator allocated over an occupied slot: %q", v)
	}
	if raw, _ := back.GetRaw(core.NewTag(0x0009, 0x0010)); !bytes.Equal(raw, []byte{0, 1, 2, 0}) && !bytes.Equal(raw, []byte{0, 1, 2}) {
		t.Fatalf("UN creator slot %v", raw)
	}
}

func TestDICOMJSONExportsImplicitPrivateSequenceAsSQ(t *testing.T) {
	data, err := dicomjson.Marshal(privateImplicitObject(t), dicomjson.DefaultOptions())
	if err != nil {
		t.Fatal(err)
	}
	back, err := dicomjson.Unmarshal(data, std.Dictionary)
	if err != nil {
		t.Fatal(err)
	}
	e, _ := back.Get(core.NewTag(0x2001, 0x105f))
	items, _ := back.GetSequence(core.NewTag(0x2001, 0x105f))
	if e.VR() != core.VRSQ || len(items) != 2 {
		t.Fatalf("JSON round trip: %s with %d items", e.VR(), len(items))
	}
}
