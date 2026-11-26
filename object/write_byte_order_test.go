package object

import (
	"bytes"
	"context"
	"encoding/binary"
	"fmt"
	"strings"
	"testing"

	"github.com/ThalesMMS/dicom-go/core"
	"github.com/ThalesMMS/dicom-go/dictionary/std"
	"github.com/ThalesMMS/dicom-go/internal/valueencode"
	"github.com/ThalesMMS/dicom-go/parser"
	"github.com/ThalesMMS/dicom-go/transfer"
	"github.com/ThalesMMS/dicom-go/validation"
)

var (
	byteOrderTestRows           = core.NewTag(0x0028, 0x0010)
	byteOrderTestColumns        = core.NewTag(0x0028, 0x0011)
	byteOrderTestBitsAllocated  = core.NewTag(0x0028, 0x0100)
	byteOrderTestPrivateCreator = core.NewTag(0x0011, 0x0010)
	byteOrderTestSequence       = core.NewTag(0x0011, 0x1050)
	byteOrderTestNested         = core.NewTag(0x0011, 0x1051)
	// Every VR whose bytes a change of byte order reverses, plus the byte
	// streams OB and UN that keep them.
	byteOrderTestVRs = []core.VR{
		core.VRUS, core.VRSS, core.VRUL, core.VRSL, core.VRFL, core.VRFD, core.VRAT,
		core.VROW, core.VROL, core.VROD, core.VROF, core.VROV, core.VRSV, core.VRUV,
		core.VROB, core.VRUN,
	}
)

// byteOrderTestValue is four units of a recognisable value of vr in order.
func byteOrderTestValue(vr core.VR, order binary.ByteOrder) []byte {
	width := valueencode.ByteOrderWidth(vr)
	le := make([]byte, 4*max(width, 2))
	for i := range le {
		le[i] = byte(0x11*(i%width+1)) + byte(i/width)
	}
	if order == binary.BigEndian {
		return valueencode.ReverseFixedWidth(le, width)
	}
	return le
}

// byteOrderTestElements has a value of every numeric VR at the top level,
// in a sequence item and in an item nested in it, all in order.
func byteOrderTestElements(order binary.ByteOrder) []core.Element {
	numeric := func(group uint16) []core.Element {
		var elements []core.Element
		for index, vr := range byteOrderTestVRs {
			elements = append(elements, core.NewRawElement(core.NewTag(group, uint16(0x1000+index)), vr, byteOrderTestValue(vr, order)))
		}
		return elements
	}
	us := func(tag core.Tag, value uint16) core.Element {
		raw := make([]byte, 2)
		order.PutUint16(raw, value)
		return core.NewRawElement(tag, core.VRUS, raw)
	}
	inner := append([]core.Element{{Header: core.ElementHeader{Tag: byteOrderTestPrivateCreator, VR: core.VRLO}, Value: core.StringValue{"BYTE ORDER TEST"}}}, numeric(0x0011)...)
	item := append(append([]core.Element(nil), inner...), core.Element{
		Header: core.ElementHeader{Tag: byteOrderTestNested, VR: core.VRSQ},
		Value:  core.SequenceValue{Items: []core.DataSet{{Elements: inner}}},
	})
	elements := []core.Element{
		{Header: core.ElementHeader{Tag: core.NewTag(0x0008, 0x0016), VR: core.VRUI}, Value: core.StringValue{"1.2.840.10008.5.1.4.1.1.7"}},
		{Header: core.ElementHeader{Tag: core.NewTag(0x0008, 0x0018), VR: core.VRUI}, Value: core.StringValue{"1.2.826.0.1.3680043.10.543.1967.1"}},
		us(byteOrderTestRows, 2),
		us(byteOrderTestColumns, 3),
	}
	elements = append(elements, inner...)
	return append(elements, core.Element{
		Header: core.ElementHeader{Tag: byteOrderTestSequence, VR: core.VRSQ},
		Value:  core.SequenceValue{Items: []core.DataSet{{Elements: item}}},
	})
}

// byteOrderTestPixelElements describes a 2x3 frame of bits samples stored
// with vr, in order.
func byteOrderTestPixelElements(order binary.ByteOrder, vr core.VR, bits uint16) []core.Element {
	us := func(tag core.Tag, value uint16) core.Element {
		raw := make([]byte, 2)
		order.PutUint16(raw, value)
		return core.NewRawElement(tag, core.VRUS, raw)
	}
	samples := []uint64{1, 256, 4095, 65535, 42, 1024}
	width := int(bits / 8)
	pixels := make([]byte, 0, len(samples)*width)
	for _, sample := range samples {
		unit := make([]byte, 8)
		order.PutUint64(unit, sample)
		if order == binary.BigEndian {
			unit = unit[8-width:]
		} else {
			unit = unit[:width]
		}
		pixels = append(pixels, unit...)
	}
	return []core.Element{
		{Header: core.ElementHeader{Tag: core.NewTag(0x0008, 0x0016), VR: core.VRUI}, Value: core.StringValue{"1.2.840.10008.5.1.4.1.1.7"}},
		{Header: core.ElementHeader{Tag: core.NewTag(0x0008, 0x0018), VR: core.VRUI}, Value: core.StringValue{"1.2.826.0.1.3680043.10.543.1967.2"}},
		us(core.NewTag(0x0028, 0x0002), 1),
		{Header: core.ElementHeader{Tag: core.NewTag(0x0028, 0x0004), VR: core.VRCS}, Value: core.StringValue{"MONOCHROME2"}},
		us(byteOrderTestRows, 2),
		us(byteOrderTestColumns, 3),
		us(byteOrderTestBitsAllocated, bits),
		us(core.NewTag(0x0028, 0x0101), bits),
		us(core.NewTag(0x0028, 0x0102), bits-1),
		us(core.NewTag(0x0028, 0x0103), 0),
		core.NewRawElement(core.TagPixelData, vr, pixels),
	}
}

// byteOrderTestPart10 writes elements, whose raw values are in syntax's byte
// order, as a Part 10 file in syntax.
func byteOrderTestPart10(t *testing.T, elements []core.Element, syntax transfer.Syntax) []byte {
	t.Helper()
	dataset := FromElements(elements, std.Dictionary)
	dataset.SetValueByteOrder(syntax.ByteOrder)
	var out bytes.Buffer
	if err := WriteFile(&out, &File{Dataset: dataset, TransferSyntax: syntax}); err != nil {
		t.Fatalf("write %s source: %v", syntax.Name, err)
	}
	return out.Bytes()
}

// byteOrderTestConvert reads source with opts and writes it in target.
func byteOrderTestConvert(t *testing.T, source []byte, opts ReadFileOptions, target transfer.Syntax) []byte {
	t.Helper()
	file, err := ReadFileWithOptions(bytes.NewReader(source), opts)
	if err != nil {
		t.Fatalf("read source: %v", err)
	}
	file.TransferSyntax = target
	var out bytes.Buffer
	if err := WriteFile(&out, file); err != nil {
		t.Fatalf("write %s: %v", target.Name, err)
	}
	return out.Bytes()
}

func byteOrderTestRead(t *testing.T, data []byte, want transfer.Syntax) *File {
	t.Helper()
	file, err := ReadFile(bytes.NewReader(data))
	if err != nil {
		t.Fatalf("read converted file: %v", err)
	}
	if file.TransferSyntax.UID != want.UID {
		t.Fatalf("transfer syntax = %s, want %s", file.TransferSyntax.UID, want.UID)
	}
	if file.Dataset.ValueByteOrder() != want.ByteOrder {
		t.Fatalf("value byte order = %v, want %v", file.Dataset.ValueByteOrder(), want.ByteOrder)
	}
	return file
}

// assertByteOrderTestValues checks every numeric value of dataset, at each
// level, against the value written in the dataset's own byte order.
func assertByteOrderTestValues(t *testing.T, dataset *Object, level string) {
	t.Helper()
	order := dataset.ValueByteOrder()
	for index, vr := range byteOrderTestVRs {
		tag := core.NewTag(0x0011, uint16(0x1000+index))
		element, ok := dataset.Get(tag)
		if !ok {
			t.Fatalf("%s: %s (%s) missing", level, tag, vr)
		}
		raw, ok := element.RawBytes()
		if !ok {
			t.Fatalf("%s: %s (%s) value %T is not raw", level, tag, vr, element.Value)
		}
		if want := byteOrderTestValue(vr, order); !bytes.Equal(raw, want) {
			t.Fatalf("%s: %s (%s) = % X, want % X", level, tag, vr, raw, want)
		}
	}
}

func assertByteOrderTestDataSet(t *testing.T, file *File) {
	t.Helper()
	for tag, want := range map[core.Tag]int64{byteOrderTestRows: 2, byteOrderTestColumns: 3} {
		if got, err := file.Dataset.GetInts(tag); err != nil || len(got) != 1 || got[0] != want {
			t.Fatalf("%s = %v (%v), want %d", tag, got, err, want)
		}
	}
	assertByteOrderTestValues(t, file.Dataset, "top level")
	items, ok := file.Dataset.GetSequence(byteOrderTestSequence)
	if !ok || len(items) != 1 {
		t.Fatalf("sequence items = %d (%t), want 1", len(items), ok)
	}
	assertByteOrderTestValues(t, items[0], "item")
	nested, ok := items[0].GetSequence(byteOrderTestNested)
	if !ok || len(nested) != 1 {
		t.Fatalf("nested sequence items = %d (%t), want 1", len(nested), ok)
	}
	assertByteOrderTestValues(t, nested[0], "nested item")
}

func assertByteOrderTestPixels(t *testing.T, file *File, bits int) {
	t.Helper()
	order := file.Dataset.ValueByteOrder()
	pixels, ok := file.Dataset.GetRaw(core.TagPixelData)
	width := bits / 8
	if !ok || len(pixels) != 6*width {
		t.Fatalf("Pixel Data length = %d (%t), want %d", len(pixels), ok, 6*width)
	}
	for index, want := range []uint64{1, 256, 4095, 65535, 42, 1024} {
		unit := pixels[index*width : (index+1)*width]
		var got uint64
		switch width {
		case 1:
			got = uint64(unit[0])
			want &= 0xff
		case 2:
			got = uint64(order.Uint16(unit))
		case 4:
			got = uint64(order.Uint32(unit))
		}
		if got != want {
			t.Fatalf("pixel %d = %d, want %d", index, got, want)
		}
	}
}

var byteOrderTestDirections = []struct {
	name           string
	source, target transfer.Syntax
}{
	{"BigToExplicitLittle", transfer.ExplicitVRBigEndian, transfer.ExplicitVRLittleEndian},
	{"ExplicitLittleToBig", transfer.ExplicitVRLittleEndian, transfer.ExplicitVRBigEndian},
	{"BigToDeflatedLittle", transfer.ExplicitVRBigEndian, transfer.DeflatedExplicitVRLittleEndian},
}

// byteOrderTestReadModes reads values into memory or leaves the numeric
// values and Pixel Data on the seekable source, at the top level and in
// sequence items.
var byteOrderTestReadModes = []struct {
	name     string
	deferred bool
}{
	{"InMemory", false},
	{"Deferred", true},
}

// byteOrderTestReadOptions defers values when deferred and syntax keeps
// source offsets (a Deflated data set does not).
func byteOrderTestReadOptions(deferred bool, syntax transfer.Syntax) ReadFileOptions {
	if !deferred || syntax.Deflated {
		return ReadFileOptions{}
	}
	// The bulk data descriptor leaves every binary value longer than one
	// byte on the source, at any depth, as DICOMweb origins read them.
	return ReadFileOptions{DeferPixelData: true, BulkDataDescriptor: &parser.BulkDataDescriptor{Rules: []parser.BulkDataRule{{Threshold: 1}}}}
}

// byteOrderTestDeferredDepths counts the elements left on the source at the
// top level and inside sequence items.
func byteOrderTestDeferredDepths(elements []core.Element, nested bool, top, items *int) {
	for _, element := range elements {
		if element.Value == nil && element.VR() != core.VRSQ {
			if nested {
				*items++
			} else {
				*top++
			}
		}
		if sequence, ok := element.Value.(core.SequenceValue); ok {
			for _, item := range sequence.Items {
				byteOrderTestDeferredDepths(item.Elements, true, top, items)
			}
		}
	}
}

// byteOrderTestConvertMode converts source like byteOrderTestConvert and,
// for a deferred mode, checks that values were left on the source.
func byteOrderTestConvertMode(t *testing.T, source []byte, sourceSyntax transfer.Syntax, deferred, wantNested bool, target transfer.Syntax) []byte {
	t.Helper()
	opts := byteOrderTestReadOptions(deferred, sourceSyntax)
	if opts.BulkDataDescriptor != nil {
		file, err := ReadFileWithOptions(bytes.NewReader(source), opts)
		if err != nil {
			t.Fatalf("read deferred source: %v", err)
		}
		var top, items int
		byteOrderTestDeferredDepths(file.Dataset.Elements(), false, &top, &items)
		if top == 0 || wantNested && items == 0 {
			t.Fatalf("deferred values: top level %d, items %d", top, items)
		}
	}
	return byteOrderTestConvert(t, source, opts, target)
}

func TestWriteFileReversesNumericValuesAcrossByteOrders(t *testing.T) {
	for _, direction := range byteOrderTestDirections {
		for _, mode := range byteOrderTestReadModes {
			t.Run(direction.name+"/"+mode.name, func(t *testing.T) {
				source := byteOrderTestPart10(t, byteOrderTestElements(direction.source.ByteOrder), direction.source)
				assertByteOrderTestDataSet(t, byteOrderTestRead(t, source, direction.source))
				converted := byteOrderTestConvertMode(t, source, direction.source, mode.deferred, true, direction.target)
				assertByteOrderTestDataSet(t, byteOrderTestRead(t, converted, direction.target))
				back := byteOrderTestConvertMode(t, converted, direction.target, mode.deferred, true, direction.source)
				assertByteOrderTestDataSet(t, byteOrderTestRead(t, back, direction.source))
			})
		}
	}
}

func TestWriteFileReversesPixelDataAcrossByteOrders(t *testing.T) {
	for _, pixel := range []struct {
		name string
		vr   core.VR
		bits uint16
	}{
		{"OW16", core.VROW, 16},
		{"OW32", core.VROW, 32},
		{"OB8", core.VROB, 8},
	} {
		for _, direction := range byteOrderTestDirections {
			for _, mode := range byteOrderTestReadModes {
				t.Run(pixel.name+"/"+direction.name+"/"+mode.name, func(t *testing.T) {
					source := byteOrderTestPart10(t, byteOrderTestPixelElements(direction.source.ByteOrder, pixel.vr, pixel.bits), direction.source)
					assertByteOrderTestPixels(t, byteOrderTestRead(t, source, direction.source), int(pixel.bits))
					converted := byteOrderTestConvertMode(t, source, direction.source, mode.deferred, false, direction.target)
					file := byteOrderTestRead(t, converted, direction.target)
					assertByteOrderTestPixels(t, file, int(pixel.bits))
					if element, _ := file.Dataset.Get(core.TagPixelData); element.VR() != pixel.vr {
						t.Fatalf("Pixel Data VR = %s, want %s", element.VR(), pixel.vr)
					}
					back := byteOrderTestConvertMode(t, converted, direction.target, mode.deferred, false, direction.source)
					assertByteOrderTestPixels(t, byteOrderTestRead(t, back, direction.source), int(pixel.bits))
				})
			}
		}
	}
}

func TestWriteFileReversesImplicitLittleEndianToBigEndian(t *testing.T) {
	// Implicit VR takes the VRs from the dictionary: Rows, Columns and the
	// OW Pixel Data are reversed.
	elements := byteOrderTestPixelElements(binary.LittleEndian, core.VROW, 16)
	source := byteOrderTestPart10(t, elements, transfer.ImplicitVRLittleEndian)
	converted := byteOrderTestConvert(t, source, ReadFileOptions{}, transfer.ExplicitVRBigEndian)
	file := byteOrderTestRead(t, converted, transfer.ExplicitVRBigEndian)
	assertByteOrderTestPixels(t, file, 16)
	back := byteOrderTestConvert(t, converted, ReadFileOptions{}, transfer.ImplicitVRLittleEndian)
	assertByteOrderTestPixels(t, byteOrderTestRead(t, back, transfer.ImplicitVRLittleEndian), 16)
}

func TestWriteFileKeepsBytesWhenByteOrderMatches(t *testing.T) {
	for _, syntax := range []transfer.Syntax{transfer.ExplicitVRBigEndian, transfer.ExplicitVRLittleEndian} {
		t.Run(syntax.Name, func(t *testing.T) {
			source := byteOrderTestPart10(t, byteOrderTestElements(syntax.ByteOrder), syntax)
			if rewritten := byteOrderTestConvert(t, source, ReadFileOptions{}, syntax); !bytes.Equal(rewritten, source) {
				t.Fatal("rewriting in the same syntax changed the file")
			}
		})
	}
}

func TestWriteDataSetReversesWithoutChangingSource(t *testing.T) {
	elements := byteOrderTestElements(binary.BigEndian)
	dataset := FromElements(elements, std.Dictionary)
	dataset.SetValueByteOrder(binary.BigEndian)
	var out bytes.Buffer
	if err := WriteDataSet(&out, dataset, transfer.ExplicitVRLittleEndian); err != nil {
		t.Fatal(err)
	}
	read, err := ReadDataSet(bytes.NewReader(out.Bytes()), transfer.ExplicitVRLittleEndian)
	if err != nil {
		t.Fatal(err)
	}
	assertByteOrderTestDataSet(t, &File{Dataset: read})
	// The written object keeps its own Big Endian values.
	if dataset.ValueByteOrder() != binary.BigEndian {
		t.Fatal("source byte order changed")
	}
	assertByteOrderTestDataSet(t, &File{Dataset: dataset})
}

func TestWriteDataSetRefusesPartialNumericUnitAcrossByteOrders(t *testing.T) {
	dataset := FromElements([]core.Element{core.NewRawElement(core.NewTag(0x0011, 0x1002), core.VRUL, []byte{1, 2, 3, 4, 5, 6})}, std.Dictionary)
	dataset.SetValueByteOrder(binary.BigEndian)
	var out bytes.Buffer
	err := WriteDataSet(&out, dataset, transfer.ExplicitVRLittleEndian)
	if err == nil || !strings.Contains(err.Error(), "not a multiple of 4") {
		t.Fatalf("WriteDataSet error = %v, want a partial unit refusal", err)
	}
	// Written in its own order, the value is kept as it is.
	out.Reset()
	if err := WriteDataSet(&out, dataset, transfer.ExplicitVRBigEndian); err != nil {
		t.Fatal(err)
	}
}

// KeepPartialByteOrderUnits writes a value that is not a whole number of
// units, at the top level and in an item, in memory or deferred on the
// source: its whole units are reversed and the trailing partial unit kept.
// Without it the same Big Endian file is refused.
func TestWriteFileKeepsPartialByteOrderUnitsWhenAsked(t *testing.T) {
	top, nested := core.NewTag(0x0011, 0x1002), core.NewTag(0x0011, 0x1003)
	source := byteOrderTestPart10(t, []core.Element{
		newStringElement(tagSOPClassUID, core.VRUI, "1.2.840.10008.5.1.4.1.1.7"),
		newStringElement(tagSOPInstanceUID, core.VRUI, "1.2.826.0.1.3680043.10.543.1967.1"),
		core.NewRawElement(core.NewTag(0x0011, 0x0010), core.VRLO, []byte("PARTIAL UNIT")),
		core.NewRawElement(top, core.VRUL, []byte{1, 2, 3, 4, 5, 6}),
		{Header: core.ElementHeader{Tag: core.NewTag(0x0011, 0x1004), VR: core.VRSQ}, Value: core.SequenceValue{Items: []core.DataSet{{Elements: []core.Element{
			core.NewRawElement(nested, core.VRFD, []byte{1, 2, 3, 4, 5, 6, 7, 8, 9, 10, 11, 12}),
		}}}}},
	}, transfer.ExplicitVRBigEndian)
	for _, mode := range byteOrderTestReadModes {
		t.Run(mode.name, func(t *testing.T) {
			read := func() *File {
				file, err := ReadFileWithOptions(bytes.NewReader(source), byteOrderTestReadOptions(mode.deferred, transfer.ExplicitVRBigEndian))
				if err != nil {
					t.Fatal(err)
				}
				file.TransferSyntax = transfer.ExplicitVRLittleEndian
				return file
			}
			var out bytes.Buffer
			if err := WriteFile(&out, read()); err == nil || !strings.Contains(err.Error(), "not a multiple of") {
				t.Fatalf("WriteFile error = %v, want a partial unit refusal", err)
			}
			out.Reset()
			if err := WriteFileWithOptions(&out, read(), WriteFileOptions{KeepPartialByteOrderUnits: true}); err != nil {
				t.Fatal(err)
			}
			written := byteOrderTestRead(t, out.Bytes(), transfer.ExplicitVRLittleEndian)
			if got, _ := written.Dataset.GetRaw(top); !bytes.Equal(got, []byte{4, 3, 2, 1, 5, 6}) {
				t.Fatalf("UL = % X", got)
			}
			items, ok := written.Dataset.GetSequence(core.NewTag(0x0011, 0x1004))
			if !ok || len(items) != 1 {
				t.Fatalf("sequence items = %d (%t)", len(items), ok)
			}
			if got, _ := items[0].GetRaw(nested); !bytes.Equal(got, []byte{8, 7, 6, 5, 4, 3, 2, 1, 9, 10, 11, 12}) {
				t.Fatalf("FD in item = % X", got)
			}
		})
	}
}

func TestWriteDataSetWithValidationReversesAcrossByteOrders(t *testing.T) {
	dataset := FromElements(byteOrderTestElements(binary.BigEndian), std.Dictionary)
	dataset.SetValueByteOrder(binary.BigEndian)
	var out bytes.Buffer
	if _, err := WriteDataSetWithValidation(context.Background(), &out, dataset, transfer.ExplicitVRLittleEndian, validation.Options{}); err != nil {
		t.Fatal(err)
	}
	read, err := ReadDataSet(bytes.NewReader(out.Bytes()), transfer.ExplicitVRLittleEndian)
	if err != nil {
		t.Fatal(err)
	}
	assertByteOrderTestDataSet(t, &File{Dataset: read})
}

func TestWriteFileFramesFromBigEndianDataSet(t *testing.T) {
	native := [][]byte{{1, 0, 2, 0, 3, 0}, {4, 0, 5, 0, 6, 0}, {7, 0, 8, 0, 9, 0}}
	fragments := [][]byte{{0xff, 0xd8, 1, 0xff, 0xd9}, {0xff, 0xd8, 2, 3, 0xff, 0xd9}, {0xff, 0xd8, 0xff, 0xd9}}
	for _, c := range []struct {
		name   string
		bits   uint16
		syntax transfer.Syntax
		frames [][]byte
		policy OffsetTablePolicy
		width  int
	}{
		{"NativeOW", 16, transfer.ExplicitVRLittleEndian, native, OffsetTableAuto, 2},
		{"EncapsulatedEOT", 8, transfer.JPEGBaseline, fragments, OffsetTableExtended, 1},
	} {
		t.Run(c.name, func(t *testing.T) {
			elements := frameTestDataSet(len(c.frames), c.bits)
			var want bytes.Buffer
			little := FromElements(elements, nil)
			if err := WriteFileFrames(&want, &File{Dataset: little, TransferSyntax: c.syntax}, &memoryFrames{frames: c.frames}, FrameWriteOptions{OffsetTable: c.policy}); err != nil {
				t.Fatal(err)
			}
			// The same data set held in Big Endian writes the same file with
			// the same frames: the FrameSource produces them in the order of
			// the written syntax, and only the data set's values are reversed.
			reversed, err := reverseElementsByteOrder(elements, binary.LittleEndian, false)
			if err != nil {
				t.Fatal(err)
			}
			big := FromElements(reversed, nil)
			big.SetValueByteOrder(binary.BigEndian)
			var got bytes.Buffer
			if err := WriteFileFrames(&got, &File{Dataset: big, TransferSyntax: c.syntax}, &memoryFrames{frames: c.frames}, FrameWriteOptions{OffsetTable: c.policy}); err != nil {
				t.Fatal(err)
			}
			if !bytes.Equal(got.Bytes(), want.Bytes()) {
				t.Fatalf("Big Endian data set wrote\n%x\nwant\n%x", got.Bytes(), want.Bytes())
			}
		})
	}
}

// byteOrderTestFrameElements is the native 2x3 image of
// byteOrderTestPixelElements without its Pixel Data, with the numeric
// values of byteOrderTestElements at every level, in order.
func byteOrderTestFrameElements(order binary.ByteOrder, bits uint16) (elements []core.Element, pixels []byte) {
	for _, element := range byteOrderTestPixelElements(order, core.VROW, bits) {
		if element.Tag() == core.TagPixelData {
			pixels, _ = element.RawBytes()
			continue
		}
		elements = append(elements, element)
	}
	for _, element := range byteOrderTestElements(order) {
		if element.Tag().Group == 0x0011 {
			elements = append(elements, element)
		}
	}
	return elements, pixels
}

func TestWriteFileFramesWritesSourceFramesInDestinationOrder(t *testing.T) {
	for _, bits := range []uint16{16, 32} {
		for _, direction := range byteOrderTestDirections {
			for _, mode := range byteOrderTestReadModes {
				t.Run(fmt.Sprintf("OW%d/%s/%s", bits, direction.name, mode.name), func(t *testing.T) {
					// A data set read from a Part 10 file in the source
					// syntax holds its values in that order.
					elements, _ := byteOrderTestFrameElements(direction.source.ByteOrder, bits)
					source := byteOrderTestPart10(t, elements, direction.source)
					file, err := ReadFileWithOptions(bytes.NewReader(source), byteOrderTestReadOptions(mode.deferred, direction.source))
					if err != nil {
						t.Fatal(err)
					}
					if file.Dataset.ValueByteOrder() != direction.source.ByteOrder {
						t.Fatalf("source value byte order = %v, want %v", file.Dataset.ValueByteOrder(), direction.source.ByteOrder)
					}
					// The frame is produced in the byte order of the written
					// syntax and must reach the file unchanged.
					_, frame := byteOrderTestFrameElements(direction.target.ByteOrder, bits)
					file.TransferSyntax = direction.target
					var out bytes.Buffer
					if err := WriteFileFrames(&out, file, &memoryFrames{frames: [][]byte{frame}}, FrameWriteOptions{}); err != nil {
						t.Fatal(err)
					}
					written := byteOrderTestRead(t, out.Bytes(), direction.target)
					assertByteOrderTestPixels(t, written, int(bits))
					assertByteOrderTestDataSet(t, written)
				})
			}
		}
	}
}

func TestWriteFileFramesStreamsValuesDeferredInItems(t *testing.T) {
	for _, syntax := range []transfer.Syntax{transfer.ExplicitVRLittleEndian, transfer.ExplicitVRBigEndian} {
		t.Run(syntax.Name, func(t *testing.T) {
			// Values left on the source inside sequence items are copied
			// from it, not written empty, when the frames replace Pixel Data.
			elements, frame := byteOrderTestFrameElements(syntax.ByteOrder, 16)
			source := byteOrderTestPart10(t, elements, syntax)
			opts := byteOrderTestReadOptions(true, syntax)
			file, err := ReadFileWithOptions(bytes.NewReader(source), opts)
			if err != nil {
				t.Fatal(err)
			}
			var top, items int
			byteOrderTestDeferredDepths(file.Dataset.Elements(), false, &top, &items)
			if items == 0 {
				t.Fatal("no value was deferred in a sequence item")
			}
			var out bytes.Buffer
			if err := WriteFileFrames(&out, file, &memoryFrames{frames: [][]byte{frame}}, FrameWriteOptions{}); err != nil {
				t.Fatal(err)
			}
			written := byteOrderTestRead(t, out.Bytes(), syntax)
			assertByteOrderTestPixels(t, written, 16)
			assertByteOrderTestDataSet(t, written)
		})
	}
}
