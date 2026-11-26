package dicomxml

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/binary"
	"encoding/xml"
	"fmt"
	"io"
	"runtime"
	"strings"
	"testing"

	"github.com/ThalesMMS/dicom-go/core"
	"github.com/ThalesMMS/dicom-go/dictionary/std"
	"github.com/ThalesMMS/dicom-go/internal/valueencode"
	"github.com/ThalesMMS/dicom-go/object"
	"github.com/ThalesMMS/dicom-go/transfer"
)

// byteOrderLittle is the Little Endian Value Field of every byte order test
// value; a Big Endian object holds it reversed at the unit width.
var byteOrderLittle = []byte{1, 2, 3, 4, 5, 6, 7, 8, 9, 10, 11, 12, 13, 14, 15, 16}

var byteOrderValues = []struct {
	tag   core.Tag
	vr    core.VR
	width int
}{
	{core.NewTag(0x0028, 0x1201), core.VROW, 2},
	{core.NewTag(0x0066, 0x0016), core.VROF, 4},
	{core.NewTag(0x0066, 0x0022), core.VROD, 8},
	{core.NewTag(0x0066, 0x0040), core.VROL, 4},
	{core.NewTag(0x7FE0, 0x0001), core.VROV, 8},
	{core.NewTag(0x0042, 0x0011), core.VROB, 1},
	{core.NewTag(0x0018, 0x1000), core.VRUN, 1},
}

var byteOrderIcon = core.NewTag(0x0088, 0x0200)

func inOrder(order binary.ByteOrder, width int) []byte {
	if order == binary.BigEndian {
		return valueencode.ReverseFixedWidth(byteOrderLittle, width)
	}
	return bytes.Clone(byteOrderLittle)
}

func uint16Raw(order binary.ByteOrder, value uint16) []byte {
	raw := make([]byte, 2)
	order.PutUint16(raw, value)
	return raw
}

// byteOrderObject holds every test value in order, an icon item with OW
// Pixel Data and Bits Allocated 32, and an OW value of typed words whose
// Little Endian Value Field is byteOrderLittle.
func byteOrderObject(order binary.ByteOrder) *object.Object {
	elements := []core.Element{
		{Header: core.ElementHeader{Tag: byteOrderIcon, VR: core.VRSQ}, Value: core.SequenceValue{Items: []core.DataSet{{Elements: []core.Element{
			core.NewRawElement(valueencode.BitsAllocatedTag, core.VRUS, uint16Raw(order, 32)),
			core.NewRawElement(core.TagPixelData, core.VROW, inOrder(order, 4)),
		}}}}},
		{Header: core.ElementHeader{Tag: core.NewTag(0x0028, 0x1202), VR: core.VROW}, Value: core.Uint16Value{0x0201, 0x0403, 0x0605, 0x0807, 0x0A09, 0x0C0B, 0x0E0D, 0x100F}},
	}
	for _, value := range byteOrderValues {
		elements = append(elements, core.NewRawElement(value.tag, value.vr, inOrder(order, value.width)))
	}
	obj := object.FromDataSet(core.DataSet{Elements: elements}, std.Dictionary)
	obj.SetValueByteOrder(order)
	return obj
}

type byteOrderModel struct {
	Attributes []byteOrderAttribute `xml:"DicomAttribute"`
}

type byteOrderAttribute struct {
	Tag    string `xml:"tag,attr"`
	Inline string `xml:"InlineBinary"`
	Bulk   *struct {
		URI string `xml:"uri,attr"`
	} `xml:"BulkData"`
	Items []byteOrderModel `xml:"Item"`
}

func (m byteOrderModel) attribute(t *testing.T, tag core.Tag) byteOrderAttribute {
	t.Helper()
	for _, attr := range m.Attributes {
		if attr.Tag == tag.HexString() {
			return attr
		}
	}
	t.Fatalf("no attribute %s", tag.HexString())
	return byteOrderAttribute{}
}

// TestMarshalLittleEndianInlineBinary checks that the InlineBinary of Big
// Endian OW, OF, OD, OL and OV values, typed OW words included, is Little
// Endian at the VR width, and OW Pixel Data with Bits Allocated 32 at the
// sample width, as is the data given to BulkDataURIFunc; and that
// SourceByteOrderInlineBinary, or a Little Endian object, keeps the bytes.
func TestMarshalLittleEndianInlineBinary(t *testing.T) {
	for _, order := range []binary.ByteOrder{binary.BigEndian, binary.LittleEndian} {
		for _, source := range []bool{false, true} {
			for _, withBulk := range []bool{false, true} {
				name := fmt.Sprintf("%v source=%t bulk=%t", order, source, withBulk)
				want := func(width int) []byte {
					if source {
						return inOrder(order, width)
					}
					return byteOrderLittle
				}
				opts := Options{SourceByteOrderInlineBinary: source}
				bulk := map[core.Tag][]byte{}
				if withBulk {
					opts.BulkDataURIFunc = func(tag core.Tag, _ core.VR, data []byte) string {
						bulk[tag] = bytes.Clone(data)
						if tag == core.NewTag(0x0066, 0x0016) {
							return "https://example.invalid/bulk/OF"
						}
						return ""
					}
				}
				obj := byteOrderObject(order)
				encoded, err := Marshal(obj, opts)
				if err != nil {
					t.Fatalf("%s: %v", name, err)
				}
				var model byteOrderModel
				if err := xml.Unmarshal(encoded, &model); err != nil {
					t.Fatalf("%s: %v", name, err)
				}
				check := func(attr byteOrderAttribute, label string, width int) {
					t.Helper()
					if attr.Bulk != nil {
						if got := bulk[core.NewTag(0x0066, 0x0016)]; !bytes.Equal(got, want(width)) {
							t.Errorf("%s: %s BulkDataURIFunc data % X, want % X", name, label, got, want(width))
						}
						return
					}
					got, err := base64.StdEncoding.DecodeString(attr.Inline)
					if err != nil {
						t.Fatalf("%s: %s: %v", name, label, err)
					}
					if !bytes.Equal(got, want(width)) {
						t.Errorf("%s: %s InlineBinary % X, want % X", name, label, got, want(width))
					}
				}
				for _, value := range byteOrderValues {
					check(model.attribute(t, value.tag), value.tag.HexString()+" "+value.vr.String(), value.width)
				}
				check(model.attribute(t, core.NewTag(0x0028, 0x1202)), "typed OW", 2)
				icon := model.attribute(t, byteOrderIcon)
				if len(icon.Items) != 1 {
					t.Fatalf("%s: icon items %+v", name, icon.Items)
				}
				check(icon.Items[0].attribute(t, core.TagPixelData), "icon Pixel Data", 4)
				if withBulk {
					if bulk[core.NewTag(0x0066, 0x0016)] == nil || model.attribute(t, core.NewTag(0x0066, 0x0016)).Bulk == nil {
						t.Fatalf("%s: OF value was not offloaded", name)
					}
					if got := bulk[core.NewTag(0x0066, 0x0022)]; !bytes.Equal(got, want(8)) {
						t.Errorf("%s: OD BulkDataURIFunc data % X, want % X", name, got, want(8))
					}
				}
				// The source object is never changed by the reversal.
				if raw, _ := obj.GetRaw(core.NewTag(0x0066, 0x0022)); !bytes.Equal(raw, inOrder(order, 8)) {
					t.Fatalf("%s: source OD changed to % X", name, raw)
				}
			}
		}
	}
}

// TestMarshalLittleEndianInlineBinaryStreamsLargeValues checks the reversal of
// values written in many chunks: typed words and doubles past the 4 KiB work
// buffer and a raw OW value past the 32 KiB chunk, without SourceByteOrder.
func TestMarshalLittleEndianInlineBinaryStreamsLargeValues(t *testing.T) {
	words := make(core.Uint16Value, 5001)
	doubles := make(core.Float64Value, 1001)
	for i := range words {
		words[i] = uint16(i*7 + 1)
	}
	for i := range doubles {
		doubles[i] = float64(i) + 0.5
	}
	rawLittle := make([]byte, 70001*2)
	for i := range rawLittle {
		rawLittle[i] = byte(i * 13)
	}
	obj := object.FromElements([]core.Element{
		{Header: core.ElementHeader{Tag: core.NewTag(0x0028, 0x1201), VR: core.VROW}, Value: words},
		{Header: core.ElementHeader{Tag: core.NewTag(0x0066, 0x0022), VR: core.VROD}, Value: doubles},
		core.NewRawElement(core.NewTag(0x0028, 0x1202), core.VROW, valueencode.ReverseFixedWidth(rawLittle, 2)),
	}, std.Dictionary)
	obj.SetValueByteOrder(binary.BigEndian)
	encoded, err := Marshal(obj, Options{Limits: Limits{MaxInlineBinaryBytes: 1 << 20, MaxTotalBinaryBytes: 1 << 20}})
	if err != nil {
		t.Fatal(err)
	}
	var model byteOrderModel
	if err := xml.Unmarshal(encoded, &model); err != nil {
		t.Fatal(err)
	}
	wantWords := make([]byte, 2*len(words))
	for i, v := range words {
		binary.LittleEndian.PutUint16(wantWords[2*i:], v)
	}
	wantDoubles, _, err := valueencode.Numeric(doubles, binary.LittleEndian)
	if err != nil {
		t.Fatal(err)
	}
	for tag, want := range map[core.Tag][]byte{core.NewTag(0x0028, 0x1201): wantWords, core.NewTag(0x0066, 0x0022): wantDoubles, core.NewTag(0x0028, 0x1202): rawLittle} {
		got, err := base64.StdEncoding.DecodeString(model.attribute(t, tag).Inline)
		if err != nil {
			t.Fatal(err)
		}
		if !bytes.Equal(got, want) {
			t.Errorf("%s: InlineBinary differs from the Little Endian value (len %d, want %d)", tag, len(got), len(want))
		}
	}
}

// TestMarshalLittleEndianInlineBinaryOfDeferredPixelData checks OW Pixel
// Data with Bits Allocated 32 read from an Explicit VR Big Endian stream and
// left on the source, which is copied through the reversal at the sample
// width.
func TestMarshalLittleEndianInlineBinaryOfDeferredPixelData(t *testing.T) {
	samples := make([]byte, 4*40000)
	for i := range samples {
		samples[i] = byte(i * 7)
	}
	little := object.FromElements([]core.Element{
		core.NewRawElement(valueencode.BitsAllocatedTag, core.VRUS, uint16Raw(binary.LittleEndian, 32)),
		core.NewRawElement(core.TagPixelData, core.VROW, samples),
	}, std.Dictionary)
	var wire bytes.Buffer
	if err := object.WriteDataSet(&wire, little, transfer.ExplicitVRBigEndian); err != nil {
		t.Fatal(err)
	}
	if !bytes.Contains(wire.Bytes(), valueencode.ReverseFixedWidth(samples[:16], 4)) {
		t.Fatal("the Big Endian stream does not hold the samples reversed at their width")
	}
	obj, err := object.ReadDataSetWithOptions(bytes.NewReader(wire.Bytes()), transfer.ExplicitVRBigEndian, object.ReadFileOptions{DeferPixelData: true})
	if err != nil {
		t.Fatal(err)
	}
	if pixel, _ := obj.Get(core.TagPixelData); pixel.Value != nil {
		t.Fatalf("Pixel Data was read into memory (%T)", pixel.Value)
	}
	encoded, err := Marshal(obj, Options{Limits: Limits{MaxInlineBinaryBytes: 1 << 20, MaxTotalBinaryBytes: 1 << 20}})
	if err != nil {
		t.Fatal(err)
	}
	var model byteOrderModel
	if err := xml.Unmarshal(encoded, &model); err != nil {
		t.Fatal(err)
	}
	got, err := base64.StdEncoding.DecodeString(model.attribute(t, core.TagPixelData).Inline)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, samples) {
		t.Fatalf("Pixel Data InlineBinary is not the Little Endian samples (len %d)", len(got))
	}
}

// TestMarshalReadsBitsAllocatedInTheEffectiveByteOrder checks an object whose
// raw values are Big Endian while its own value byte order is the Little
// Endian default: with Options.ByteOrder Big Endian, Bits Allocated 32 is read
// in that order, not as 8192, so OW Pixel Data is reversed by sample.
func TestMarshalReadsBitsAllocatedInTheEffectiveByteOrder(t *testing.T) {
	obj := object.FromElements([]core.Element{
		core.NewRawElement(valueencode.BitsAllocatedTag, core.VRUS, uint16Raw(binary.BigEndian, 32)),
		core.NewRawElement(core.TagPixelData, core.VROW, inOrder(binary.BigEndian, 4)),
	}, std.Dictionary)
	encoded, err := Marshal(obj, Options{ByteOrder: binary.BigEndian})
	if err != nil {
		t.Fatal(err)
	}
	var model byteOrderModel
	if err := xml.Unmarshal(encoded, &model); err != nil {
		t.Fatal(err)
	}
	got, err := base64.StdEncoding.DecodeString(model.attribute(t, core.TagPixelData).Inline)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, byteOrderLittle) {
		t.Fatalf("Pixel Data InlineBinary % X, want % X", got, byteOrderLittle)
	}
	decoded, err := UnmarshalWithOptions(encoded, std.Dictionary, UnmarshalOptions{ByteOrder: binary.BigEndian})
	if err != nil {
		t.Fatal(err)
	}
	if got, want := objectRaw(t, decoded, core.TagPixelData), inOrder(binary.BigEndian, 4); !bytes.Equal(got, want) {
		t.Fatalf("round trip Pixel Data % X, want % X", got, want)
	}
}

// TestBulkDataURIFuncReversesADeferredValueInItsOwnBuffer checks that a Big
// Endian value read from the source for BulkDataURIFunc is reversed in the
// buffer it was read into: the default allocates no second copy of it beyond
// what SourceByteOrderInlineBinary allocates, and the callback receives the
// Little Endian bytes.
func TestBulkDataURIFuncReversesADeferredValueInItsOwnBuffer(t *testing.T) {
	samples := make([]byte, 8<<20)
	for i := range samples {
		samples[i] = byte(i * 5)
	}
	little := object.FromElements([]core.Element{
		core.NewRawElement(valueencode.BitsAllocatedTag, core.VRUS, uint16Raw(binary.LittleEndian, 16)),
		core.NewRawElement(core.TagPixelData, core.VROW, samples),
	}, std.Dictionary)
	var wire bytes.Buffer
	if err := object.WriteDataSet(&wire, little, transfer.ExplicitVRBigEndian); err != nil {
		t.Fatal(err)
	}
	allocated := func(source bool) (uint64, []byte) {
		obj, err := object.ReadDataSetWithOptions(bytes.NewReader(wire.Bytes()), transfer.ExplicitVRBigEndian, object.ReadFileOptions{DeferPixelData: true})
		if err != nil {
			t.Fatal(err)
		}
		var received []byte
		opts := Options{SourceByteOrderInlineBinary: source, BulkDataURIFunc: func(tag core.Tag, _ core.VR, data []byte) string {
			if tag == core.TagPixelData {
				received = data[:16:16]
				return "https://example.invalid/pixel"
			}
			return ""
		}}
		runtime.GC()
		var before, after runtime.MemStats
		runtime.ReadMemStats(&before)
		if _, err := Marshal(obj, opts); err != nil {
			t.Fatal(err)
		}
		runtime.ReadMemStats(&after)
		return after.TotalAlloc - before.TotalAlloc, bytes.Clone(received)
	}
	kept, keptHead := allocated(true)
	reversed, reversedHead := allocated(false)
	if !bytes.Equal(reversedHead, samples[:16]) || !bytes.Equal(keptHead, valueencode.ReverseFixedWidth(samples[:16], 2)) {
		t.Fatalf("callback data % X (kept % X), want % X", reversedHead, keptHead, samples[:16])
	}
	if reversed > kept+uint64(len(samples))/2 {
		t.Fatalf("reversal allocated %d bytes over the %d of the source order, a copy of the %d byte value", reversed-kept, kept, len(samples))
	}
}

// TestXMLReverseWriterSplitsUnitsAcrossWrites checks units split across
// writes and a trailing partial unit, which is written unchanged.
func TestXMLReverseWriterSplitsUnitsAcrossWrites(t *testing.T) {
	data := make([]byte, 100003)
	for i := range data {
		data[i] = byte(i)
	}
	for _, width := range []int{2, 4, 8} {
		for _, step := range []int{1, 3, 7, 4096, 40000} {
			var out bytes.Buffer
			w := &xmlReverseWriter{writer: &out, width: width}
			for rest := data; len(rest) > 0; {
				n := min(step, len(rest))
				if written, err := w.Write(rest[:n]); err != nil || written != n {
					t.Fatalf("width %d step %d: wrote %d, %v", width, step, written, err)
				}
				rest = rest[n:]
			}
			if err := w.Close(); err != nil {
				t.Fatal(err)
			}
			if want := valueencode.ReverseFixedWidth(data, width); !bytes.Equal(out.Bytes(), want) {
				t.Fatalf("width %d step %d: output differs", width, step)
			}
		}
	}
}

func byteOrderInline(tag core.Tag, vr core.VR) string {
	return fmt.Sprintf(`<DicomAttribute tag="%s" vr="%s"><InlineBinary>%s</InlineBinary></DicomAttribute>`, tag.HexString(), vr, base64.StdEncoding.EncodeToString(byteOrderLittle))
}

func byteOrderBulk(tag core.Tag, vr core.VR) string {
	return fmt.Sprintf(`<DicomAttribute tag="%s" vr="%s"><BulkData uri="bulk:%s"/></DicomAttribute>`, tag.HexString(), vr, tag.HexString())
}

func byteOrderUS(tag core.Tag, value int) string {
	return fmt.Sprintf(`<DicomAttribute tag="%s" vr="US"><Value number="1">%d</Value></DicomAttribute>`, tag.HexString(), value)
}

// byteOrderDocument has Bits Allocated 64 and OW Pixel Data, every test value,
// and an icon item whose OW Pixel Data precedes its Bits Allocated 32; value
// writes each binary attribute as InlineBinary or BulkData.
func byteOrderDocument(value func(core.Tag, core.VR) string) string {
	var document strings.Builder
	document.WriteString(streamRoot)
	document.WriteString(byteOrderUS(valueencode.BitsAllocatedTag, 64))
	for _, v := range byteOrderValues {
		document.WriteString(value(v.tag, v.vr))
	}
	document.WriteString(`<DicomAttribute tag="` + byteOrderIcon.HexString() + `" vr="SQ"><Item number="1">`)
	document.WriteString(value(core.TagPixelData, core.VROW))
	document.WriteString(byteOrderUS(valueencode.BitsAllocatedTag, 32))
	document.WriteString(`</Item></DicomAttribute>`)
	document.WriteString(value(core.TagPixelData, core.VROW))
	document.WriteString(streamEnd)
	return document.String()
}

// TestUnmarshalLittleEndianInlineBinary checks that Unmarshal and Walk with a
// Big Endian ByteOrder reverse Little Endian InlineBinary and resolved
// BulkData of OW, OF, OD, OL and OV values, OW Pixel Data at the sample width
// of the Bits Allocated of its data set or item, and that a Little Endian
// ByteOrder or SourceByteOrderInlineBinary keeps the bytes.
func TestUnmarshalLittleEndianInlineBinary(t *testing.T) {
	resolver := func(_ context.Context, ref BulkDataReference) (io.ReadCloser, error) {
		return io.NopCloser(bytes.NewReader(byteOrderLittle)), nil
	}
	for _, bulk := range []bool{false, true} {
		value, policy := byteOrderInline, PreserveBulkData
		if bulk {
			value, policy = byteOrderBulk, ResolveBulkData
		}
		document := byteOrderDocument(value)
		for _, order := range []binary.ByteOrder{binary.BigEndian, binary.LittleEndian} {
			for _, source := range []bool{false, true} {
				name := fmt.Sprintf("%v source=%t bulk=%t", order, source, bulk)
				want := func(width int) []byte {
					if source {
						return byteOrderLittle
					}
					return inOrder(order, width)
				}
				opts := UnmarshalOptions{AllowMissingKeyword: true, ByteOrder: order, SourceByteOrderInlineBinary: source, BulkDataPolicy: policy, BulkDataResolver: resolver}
				obj, err := UnmarshalWithOptions([]byte(document), std.Dictionary, opts)
				if err != nil {
					t.Fatalf("%s: %v", name, err)
				}
				check := func(dataset *object.Object, tag core.Tag, label string, width int) {
					t.Helper()
					got, _ := dataset.GetRaw(tag)
					if !bytes.Equal(got, want(width)) {
						t.Errorf("%s: %s % X, want % X", name, label, got, want(width))
					}
				}
				for _, v := range byteOrderValues {
					check(obj, v.tag, v.tag.HexString()+" "+v.vr.String(), v.width)
				}
				check(obj, core.TagPixelData, "Pixel Data", 8)
				items, ok := obj.GetSequence(byteOrderIcon)
				if !ok || len(items) != 1 {
					t.Fatalf("%s: icon items %v", name, items)
				}
				check(items[0], core.TagPixelData, "icon Pixel Data", 4)

				walked := map[core.Tag][]byte{}
				var icon []core.DataSet
				err = Walk(strings.NewReader(document), std.Dictionary, opts, func(attr Attribute) error {
					if raw, ok := attr.Element.RawBytes(); ok {
						walked[attr.Element.Tag()] = raw
					}
					if seq, ok := attr.Element.Value.(core.SequenceValue); ok {
						icon = seq.Items
					}
					return nil
				})
				if err != nil {
					t.Fatalf("%s: Walk: %v", name, err)
				}
				for _, v := range byteOrderValues {
					if got := walked[v.tag]; !bytes.Equal(got, want(v.width)) {
						t.Errorf("%s: Walk %s % X, want % X", name, v.tag.HexString(), got, want(v.width))
					}
				}
				if got := walked[core.TagPixelData]; !bytes.Equal(got, want(8)) {
					t.Errorf("%s: Walk Pixel Data % X, want % X", name, got, want(8))
				}
				if len(icon) != 1 {
					t.Fatalf("%s: Walk icon items %v", name, icon)
				}
				for _, elem := range icon[0].Elements {
					if elem.Tag() == core.TagPixelData {
						if got, _ := elem.RawBytes(); !bytes.Equal(got, want(4)) {
							t.Errorf("%s: Walk icon Pixel Data % X, want % X", name, got, want(4))
						}
					}
				}
			}
		}
	}
}

// TestBigEndianObjectRoundTripsThroughLittleEndianXML checks that a Big
// Endian object marshaled with the default options and unmarshaled with a Big
// Endian ByteOrder keeps every Value Field.
func TestBigEndianObjectRoundTripsThroughLittleEndianXML(t *testing.T) {
	source := byteOrderObject(binary.BigEndian)
	encoded, err := Marshal(source, Options{})
	if err != nil {
		t.Fatal(err)
	}
	decoded, err := UnmarshalWithOptions(encoded, std.Dictionary, UnmarshalOptions{ByteOrder: binary.BigEndian})
	if err != nil {
		t.Fatal(err)
	}
	for _, v := range byteOrderValues {
		if got, want := objectRaw(t, decoded, v.tag), inOrder(binary.BigEndian, v.width); !bytes.Equal(got, want) {
			t.Errorf("%s: % X, want % X", v.tag.HexString(), got, want)
		}
	}
	if got, want := objectRaw(t, decoded, core.NewTag(0x0028, 0x1202)), valueencode.ReverseFixedWidth(byteOrderLittle, 2); !bytes.Equal(got, want) {
		t.Errorf("typed OW: % X, want % X", got, want)
	}
	items, _ := decoded.GetSequence(byteOrderIcon)
	if len(items) != 1 {
		t.Fatalf("icon items %v", items)
	}
	if got, want := objectRaw(t, items[0], core.TagPixelData), inOrder(binary.BigEndian, 4); !bytes.Equal(got, want) {
		t.Errorf("icon Pixel Data % X, want % X", got, want)
	}
}

func objectRaw(t *testing.T, obj *object.Object, tag core.Tag) []byte {
	t.Helper()
	raw, ok := obj.GetRaw(tag)
	if !ok {
		t.Fatalf("%s has no raw value", tag.HexString())
	}
	return raw
}
