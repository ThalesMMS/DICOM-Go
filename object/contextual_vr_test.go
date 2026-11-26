package object_test

import (
	"bytes"
	"encoding/binary"
	"errors"
	"math"
	"strings"
	"testing"

	"github.com/ThalesMMS/dicom-go/core"
	"github.com/ThalesMMS/dicom-go/dicomjson"
	"github.com/ThalesMMS/dicom-go/dicomxml"
	"github.com/ThalesMMS/dicom-go/dictionary/std"
	"github.com/ThalesMMS/dicom-go/internal/dicomtest"
	"github.com/ThalesMMS/dicom-go/object"
	"github.com/ThalesMMS/dicom-go/transfer"
)

var (
	ctxTagRows                  = core.NewTag(0x0028, 0x0010)
	ctxTagColumns               = core.NewTag(0x0028, 0x0011)
	ctxTagBitsAllocated         = core.NewTag(0x0028, 0x0100)
	ctxTagPixelRepresentation   = core.NewTag(0x0028, 0x0103)
	ctxTagSmallestPixel         = core.NewTag(0x0028, 0x0106)
	ctxTagPixelPadding          = core.NewTag(0x0028, 0x0120)
	ctxTagZeroVelocity          = core.NewTag(0x0018, 0x9810)
	ctxTagModalityLUTSequence   = core.NewTag(0x0028, 0x3000)
	ctxTagLUTDescriptor         = core.NewTag(0x0028, 0x3002)
	ctxTagLUTData               = core.NewTag(0x0028, 0x3006)
	ctxTagRealWorldMapping      = core.NewTag(0x0040, 0x9096)
	ctxTagRealWorldFirstMapped  = core.NewTag(0x0040, 0x9216)
	ctxTagRealWorldLastMapped   = core.NewTag(0x0040, 0x9211)
	ctxTagIconImageSequence     = core.NewTag(0x0088, 0x0200)
	ctxTagRedPaletteDescriptor  = core.NewTag(0x0028, 0x1101)
	ctxTagSOPClassUID           = core.NewTag(0x0008, 0x0016)
	ctxTagSOPInstanceUID        = core.NewTag(0x0008, 0x0018)
	ctxTagFrameOfReferenceUID   = core.NewTag(0x0020, 0x0052)
	ctxTagInstanceNumber        = core.NewTag(0x0020, 0x0013)
	ctxTagSliceThickness        = core.NewTag(0x0018, 0x0050)
	ctxTagFrameIncrementPointer = core.NewTag(0x0028, 0x0009)
)

func ctxWithSOP(elements ...core.Element) []core.Element {
	return append([]core.Element{
		ctxElement(ctxTagSOPClassUID, core.VRUI, core.StringValue{"1.2.840.10008.5.1.4.1.1.7"}),
		ctxElement(ctxTagSOPInstanceUID, core.VRUI, core.StringValue{"1.2.826.0.1.3680043.10.543.938.3"}),
	}, elements...)
}

func ctxElement(tag core.Tag, vr core.VR, value core.Value) core.Element {
	return core.Element{Header: core.ElementHeader{Tag: tag, VR: vr}, Value: value}
}

func ctxItems(items ...[]core.Element) core.SequenceValue {
	value := core.SequenceValue{}
	for _, item := range items {
		value.Items = append(value.Items, core.DataSet{Elements: item})
	}
	return value
}

// ctxSignedImage has signed root pixels, an unsigned icon item, one mapping
// item with its own unsigned Pixel Representation and a sibling inheriting the
// signed root. The typed VRs are the expected concrete VRs.
func ctxSignedImage() []core.Element {
	pixels := make([]byte, 2*2*3)
	for i, v := range []int16{-2000, -1, 0, 1, 32767, -32768} {
		binary.LittleEndian.PutUint16(pixels[i*2:], uint16(v))
	}
	lut := make([]byte, 2*4)
	for i, v := range []uint16{0, 100, 200, 65535} {
		binary.LittleEndian.PutUint16(lut[i*2:], v)
	}
	return []core.Element{
		ctxElement(ctxTagSOPClassUID, core.VRUI, core.StringValue{"1.2.840.10008.5.1.4.1.1.2"}),
		ctxElement(ctxTagSOPInstanceUID, core.VRUI, core.StringValue{"1.2.826.0.1.3680043.10.543.938.1"}),
		ctxElement(ctxTagZeroVelocity, core.VRSS, core.Int16Value{-7}),
		ctxElement(core.NewTag(0x0028, 0x0002), core.VRUS, core.Uint16Value{1}),
		ctxElement(core.NewTag(0x0028, 0x0004), core.VRCS, core.StringValue{"MONOCHROME2"}),
		ctxElement(ctxTagRows, core.VRUS, core.Uint16Value{2}),
		ctxElement(ctxTagColumns, core.VRUS, core.Uint16Value{3}),
		ctxElement(ctxTagBitsAllocated, core.VRUS, core.Uint16Value{16}),
		ctxElement(core.NewTag(0x0028, 0x0101), core.VRUS, core.Uint16Value{16}),
		ctxElement(core.NewTag(0x0028, 0x0102), core.VRUS, core.Uint16Value{15}),
		ctxElement(ctxTagPixelRepresentation, core.VRUS, core.Uint16Value{1}),
		ctxElement(ctxTagSmallestPixel, core.VRSS, core.Int16Value{-2000}),
		ctxElement(ctxTagPixelPadding, core.VRSS, core.Int16Value{-32768}),
		ctxElement(ctxTagModalityLUTSequence, core.VRSQ, ctxItems([]core.Element{
			ctxElement(ctxTagLUTDescriptor, core.VRSS, core.Int16Value{4, -2000, 16}),
			ctxElement(ctxTagLUTData, core.VROW, core.RawValue(lut)),
		})),
		ctxElement(ctxTagRealWorldMapping, core.VRSQ, ctxItems(
			[]core.Element{
				ctxElement(ctxTagPixelRepresentation, core.VRUS, core.Uint16Value{0}),
				ctxElement(ctxTagRealWorldFirstMapped, core.VRUS, core.Uint16Value{65535}),
			},
			[]core.Element{
				ctxElement(ctxTagRealWorldLastMapped, core.VRSS, core.Int16Value{-5}),
			},
		)),
		ctxElement(ctxTagIconImageSequence, core.VRSQ, ctxItems([]core.Element{
			ctxElement(ctxTagBitsAllocated, core.VRUS, core.Uint16Value{8}),
			ctxElement(ctxTagPixelRepresentation, core.VRUS, core.Uint16Value{0}),
			ctxElement(ctxTagSmallestPixel, core.VRUS, core.Uint16Value{65535}),
			// OW is valid for 8-bit Pixel Data in explicit syntaxes and is the
			// only Pixel Data VR Implicit VR can carry (PS3.5 A.1).
			core.NewRawElement(core.TagPixelData, core.VROW, []byte{1, 2, 3, 4}),
		})),
		core.NewRawElement(core.TagPixelData, core.VROW, pixels),
	}
}

func ctxSemantic(t *testing.T, obj *object.Object) []dicomtest.SemanticElement {
	t.Helper()
	semantic, err := dicomtest.SemanticDataSet(obj.ToDataSet(), obj.ValueByteOrder())
	if err != nil {
		t.Fatal(err)
	}
	return semantic
}

func ctxWrite(t *testing.T, obj *object.Object, syntax transfer.Syntax) []byte {
	t.Helper()
	var buf bytes.Buffer
	if err := object.WriteFile(&buf, &object.File{Dataset: obj, TransferSyntax: syntax}); err != nil {
		t.Fatalf("write %s: %v", syntax.Name, err)
	}
	return buf.Bytes()
}

func ctxRead(t *testing.T, data []byte) *object.File {
	t.Helper()
	file, err := object.ReadFile(bytes.NewReader(data))
	if err != nil {
		t.Fatal(err)
	}
	return file
}

func TestImplicitReadResolvesUSOrSSFromScopedPixelRepresentation(t *testing.T) {
	source := object.FromElements(ctxSignedImage(), std.Dictionary)
	want := ctxSemantic(t, source)

	implicit := ctxRead(t, ctxWrite(t, source, transfer.ImplicitVRLittleEndian))
	if diff := dicomtest.DiffSemantic(ctxSemantic(t, implicit.Dataset), want); diff != "" {
		t.Fatalf("implicit read lost contextual VR or value: %s", diff)
	}
	if element, _ := implicit.Dataset.Get(ctxTagSmallestPixel); element.VR() != core.VRSS || int16(binary.LittleEndian.Uint16(mustRaw(t, implicit.Dataset, ctxTagSmallestPixel))) != -2000 {
		t.Fatalf("signed Smallest Image Pixel Value read as %s", element.VR())
	}

	// Implicit -> Explicit keeps every attribute, VR and pixel byte; the
	// independent explicit reader sees the resolved VRs on the wire.
	explicitBytes := ctxWrite(t, implicit.Dataset, transfer.ExplicitVRLittleEndian)
	explicit := ctxRead(t, explicitBytes)
	if diff := dicomtest.DiffSemantic(ctxSemantic(t, explicit.Dataset), want); diff != "" {
		t.Fatalf("implicit -> explicit changed the data set: %s", diff)
	}
	for _, tc := range []struct {
		tag core.Tag
		vr  string
	}{{ctxTagSmallestPixel, "SS"}, {ctxTagPixelPadding, "SS"}, {ctxTagZeroVelocity, "SS"}} {
		header := append(append([]byte{}, dicomtest.ExplicitLongHeaderBytes(binary.LittleEndian, tc.tag, core.VR(tc.vr), 0)[:4]...), tc.vr...)
		if !bytes.Contains(explicitBytes, header) {
			t.Fatalf("explicit output lacks %s %s", tc.tag, tc.vr)
		}
	}
	if got, _ := explicit.Dataset.GetRaw(core.TagPixelData); !bytes.Equal(got, mustRaw(t, source, core.TagPixelData)) {
		t.Fatal("Pixel Data changed")
	}
	if element, _ := explicit.Dataset.Get(core.TagPixelData); element.VR() != core.VROW {
		t.Fatalf("Pixel Data VR = %s", element.VR())
	}
}

func mustRaw(t *testing.T, obj *object.Object, tag core.Tag) []byte {
	t.Helper()
	raw, ok := obj.GetRaw(tag)
	if !ok {
		t.Fatalf("%s missing", tag)
	}
	return raw
}

func TestImplicitReadKeepsDefaultWithoutContextAndNeverOverridesExplicitVR(t *testing.T) {
	// No Pixel Representation anywhere: the dictionary default stays and the
	// value keeps its bytes; nothing is guessed.
	unscoped := object.FromElements(ctxWithSOP(
		ctxElement(ctxTagSmallestPixel, core.VRUS, core.Uint16Value{63536}),
	), std.Dictionary)
	read := ctxRead(t, ctxWrite(t, unscoped, transfer.ImplicitVRLittleEndian))
	if element, _ := read.Dataset.Get(ctxTagSmallestPixel); element.VR() != core.VRUS {
		t.Fatalf("unscoped VR = %s, want dictionary default US", element.VR())
	}

	// An explicit stream that declares US in a signed image is preserved as
	// declared; only validation may report it.
	declared := object.FromElements(ctxWithSOP(
		ctxElement(ctxTagPixelRepresentation, core.VRUS, core.Uint16Value{1}),
		ctxElement(ctxTagSmallestPixel, core.VRUS, core.Uint16Value{63536}),
	), std.Dictionary)
	read = ctxRead(t, ctxWrite(t, declared, transfer.ExplicitVRLittleEndian))
	if element, _ := read.Dataset.Get(ctxTagSmallestPixel); element.VR() != core.VRUS {
		t.Fatalf("explicit VR overwritten with %s", element.VR())
	}
}

func TestResolveContextualVRsForAuthoredElements(t *testing.T) {
	fragments := core.FragmentSequence{Fragments: [][]byte{{0xFF, 0xD8, 0xFF, 0xD9}}}
	for _, tc := range []struct {
		name     string
		elements []core.Element
		want     map[core.Tag]core.VR
		err      bool
	}{
		{
			name: "signed root and unsigned item",
			elements: []core.Element{
				ctxElement(ctxTagPixelRepresentation, core.VRUS, core.Uint16Value{1}),
				ctxElement(ctxTagBitsAllocated, core.VRUS, core.Uint16Value{16}),
				ctxElement(ctxTagSmallestPixel, "", core.RawValue{0x30, 0xF8}),
				ctxElement(ctxTagPixelPadding, core.VRUS, core.Uint16Value{0}), // already set: kept
				core.NewRawElement(core.TagPixelData, "", []byte{1, 2, 3, 4}),
			},
			want: map[core.Tag]core.VR{ctxTagSmallestPixel: core.VRSS, ctxTagPixelPadding: core.VRUS, core.TagPixelData: core.VROW},
		},
		{
			name: "8-bit native and single-entry LUT",
			elements: []core.Element{
				ctxElement(ctxTagPixelRepresentation, core.VRUS, core.Uint16Value{0}),
				ctxElement(ctxTagBitsAllocated, core.VRUS, core.Uint16Value{8}),
				ctxElement(ctxTagRedPaletteDescriptor, "", core.RawValue{0, 1, 0, 0, 16, 0}),
				ctxElement(ctxTagLUTDescriptor, core.VRUS, core.Uint16Value{1, 0, 16}),
				ctxElement(ctxTagLUTData, "", core.RawValue{7, 0}),
				core.NewRawElement(core.TagPixelData, "", []byte{1, 2}),
			},
			want: map[core.Tag]core.VR{ctxTagRedPaletteDescriptor: core.VRUS, ctxTagLUTData: core.VRUS, core.TagPixelData: core.VROB},
		},
		{
			name: "encapsulated pixel data and multi-entry LUT",
			elements: []core.Element{
				ctxElement(ctxTagLUTDescriptor, core.VRUS, core.Uint16Value{2, 0, 16}),
				ctxElement(ctxTagLUTData, "", core.RawValue{1, 0, 2, 0}),
				ctxElement(core.TagPixelData, "", fragments),
			},
			want: map[core.Tag]core.VR{ctxTagLUTData: core.VROW, core.TagPixelData: core.VROB},
		},
		{name: "missing Pixel Representation", elements: []core.Element{ctxElement(ctxTagSmallestPixel, "", core.RawValue{1, 0})}, err: true},
		{name: "invalid Pixel Representation", elements: []core.Element{
			ctxElement(ctxTagPixelRepresentation, core.VRUS, core.Uint16Value{2}),
			ctxElement(ctxTagSmallestPixel, "", core.RawValue{1, 0}),
		}, err: true},
		{name: "native Pixel Data without Bits Allocated", elements: []core.Element{core.NewRawElement(core.TagPixelData, "", []byte{1, 2})}, err: true},
		{name: "LUT Data without descriptor", elements: []core.Element{ctxElement(ctxTagLUTData, "", core.RawValue{1, 0})}, err: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			obj := object.FromElements(tc.elements, std.Dictionary)
			before := obj.Elements()
			err := obj.ResolveContextualVRs()
			if tc.err {
				if !errors.Is(err, object.ErrAmbiguousVR) {
					t.Fatalf("error = %v, want ErrAmbiguousVR", err)
				}
				for _, element := range before {
					if got, _ := obj.Get(element.Tag()); got.VR() != element.VR() {
						t.Fatalf("failed resolution changed %s", element.Tag())
					}
				}
				// Serializers never guess: an unresolved empty VR is written as
				// opaque UN, not as one of the dictionary alternatives.
				var buf bytes.Buffer
				if err := object.WriteDataSet(&buf, obj, transfer.ExplicitVRLittleEndian); err != nil {
					t.Fatal(err)
				}
				read, err := object.ReadDataSet(bytes.NewReader(buf.Bytes()), transfer.ExplicitVRLittleEndian)
				if err != nil {
					t.Fatal(err)
				}
				for _, element := range before {
					if element.VR() != "" {
						continue
					}
					if got, _ := read.Get(element.Tag()); got.VR() != core.VRUN {
						t.Fatalf("%s written as %s, want UN", element.Tag(), got.VR())
					}
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			for tag, vr := range tc.want {
				if got, _ := obj.Get(tag); got.VR() != vr {
					t.Fatalf("%s VR = %q, want %s", tag, got.VR(), vr)
				}
			}
			syntax := transfer.ExplicitVRLittleEndian
			if element, _ := obj.Get(core.TagPixelData); element.Value != nil {
				if _, ok := element.Value.(core.FragmentSequence); ok {
					syntax = transfer.JPEGBaseline
				}
			}
			var buf bytes.Buffer
			if err := object.WriteDataSet(&buf, obj, syntax); err != nil {
				t.Fatal(err)
			}
		})
	}
}

type ctxOutput struct {
	name  string
	write func(*object.Object) error
}

func ctxOutputs() []ctxOutput {
	return []ctxOutput{
		{"part10", func(obj *object.Object) error {
			return object.WriteFile(&bytes.Buffer{}, &object.File{Dataset: obj, TransferSyntax: transfer.ExplicitVRLittleEndian})
		}},
		{"raw-implicit", func(obj *object.Object) error {
			return object.WriteDataSet(&bytes.Buffer{}, obj, transfer.ImplicitVRLittleEndian)
		}},
		{"raw-big-endian", func(obj *object.Object) error {
			return object.WriteDataSet(&bytes.Buffer{}, obj, transfer.ExplicitVRBigEndian)
		}},
		{"json", func(obj *object.Object) error { _, err := dicomjson.Marshal(obj, dicomjson.Options{}); return err }},
		{"xml", func(obj *object.Object) error { _, err := dicomxml.Marshal(obj, dicomxml.Options{}); return err }},
	}
}

func TestTypedAuthoringRejectsUnrepresentableValuesOnEveryOutput(t *testing.T) {
	for _, tc := range []struct {
		name    string
		element core.Element
	}{
		{"US 70000 as uint32", ctxElement(ctxTagRows, core.VRUS, core.Uint32Value{70000})},
		{"US 70000 as int64", ctxElement(ctxTagRows, core.VRUS, core.Int64Value{70000})},
		{"US negative", ctxElement(ctxTagRows, core.VRUS, core.Int16Value{-1})},
		{"US text", ctxElement(ctxTagRows, core.VRUS, core.StringValue{"12"})},
		{"SS unsigned type", ctxElement(ctxTagSmallestPixel, core.VRSS, core.Uint16Value{65535})},
		{"OW signed type", ctxElement(ctxTagLUTData, core.VROW, core.Int16Value{-1})},
		{"AT as integer", ctxElement(ctxTagFrameIncrementPointer, core.VRAT, core.Uint32Value{0x00181063})},
		{"PN as integer", ctxElement(core.NewTag(0x0010, 0x0010), core.VRPN, core.Int64Value{42})},
		{"IS fraction", ctxElement(ctxTagInstanceNumber, core.VRIS, core.StringValue{"1.5"})},
		{"IS above int32", ctxElement(ctxTagInstanceNumber, core.VRIS, core.StringValue{"2147483648"})},
		{"IS below int32", ctxElement(ctxTagInstanceNumber, core.VRIS, core.StringValue{"-2147483649"})},
		{"IS longer than 12 bytes", ctxElement(ctxTagInstanceNumber, core.VRIS, core.StringValue{"+000000000001"})},
		{"DS text", ctxElement(ctxTagSliceThickness, core.VRDS, core.StringValue{"abc"})},
		{"DS longer than 16 bytes", ctxElement(ctxTagSliceThickness, core.VRDS, core.StringValue{"1.00000000000000001"})},
		{"DS second value", ctxElement(ctxTagSliceThickness, core.VRDS, core.StringValue{"1.5", "NaN"})},
		{"DS float type", ctxElement(ctxTagSliceThickness, core.VRDS, core.Float64Value{1.5})},
		{"UI letters", ctxElement(ctxTagFrameOfReferenceUID, core.VRUI, core.StringValue{"1.2.826.0.1.3680043.100.RTPLAN"})},
		{"UI leading zero", ctxElement(ctxTagFrameOfReferenceUID, core.VRUI, core.StringValue{"1.2.826.01.1"})},
		{"UI empty component", ctxElement(ctxTagFrameOfReferenceUID, core.VRUI, core.StringValue{"1.2..3"})},
		{"UI longer than 64 bytes", ctxElement(ctxTagFrameOfReferenceUID, core.VRUI, core.StringValue{"1.2." + strings.Repeat("9", 61)})},
		{"UI second value", ctxElement(ctxTagFrameOfReferenceUID, core.VRUI, core.StringValue{"1.2.3", "1.2.x"})},
		{"UI joined second value", ctxElement(ctxTagFrameOfReferenceUID, core.VRUI, core.StringValue{`1.2.3\4.5`})},
	} {
		for _, output := range ctxOutputs() {
			t.Run(tc.name+"/"+output.name, func(t *testing.T) {
				obj := object.FromElements(ctxWithSOP(tc.element), std.Dictionary)
				err := output.write(obj)
				if err == nil {
					t.Fatal("unrepresentable value serialized")
				}
				if !errors.Is(err, core.ErrValueNotRepresentable) && !strings.Contains(err.Error(), "unsupported") {
					t.Fatalf("error %v is not a typed representability error", err)
				}
			})
		}
	}
}

func TestTypedAuthoringPreservesRepresentableValuesExactly(t *testing.T) {
	elements := []core.Element{
		ctxElement(ctxTagSOPClassUID, core.VRUI, core.StringValue{"1.2.840.10008.5.1.4.1.1.7"}),
		ctxElement(ctxTagSOPInstanceUID, core.VRUI, core.StringValue{"1.2.826.0.1.3680043.10.543.938.2"}),
		ctxElement(ctxTagRows, core.VRUS, core.Uint16Value{65535}),
		ctxElement(ctxTagPixelRepresentation, core.VRUS, core.Uint16Value{1}),
		ctxElement(ctxTagSmallestPixel, core.VRSS, core.Int16Value{-32768, 32767}),
		ctxElement(ctxTagInstanceNumber, core.VRIS, core.StringValue{" -12 ", "+7", "2147483647", ""}),
		ctxElement(ctxTagSliceThickness, core.VRDS, core.StringValue{"1.5e-3", " 12.0 ", "-.5"}),
		ctxElement(ctxTagFrameIncrementPointer, core.VRAT, core.TagValue{core.NewTag(0x0018, 0x1063)}),
		ctxElement(core.NewTag(0x0072, 0x0082), core.VRSV, core.Int64Value{math.MaxInt64, math.MinInt64, 1<<53 + 1}),
		ctxElement(core.NewTag(0x0072, 0x0083), core.VRUV, core.Uint64Value{math.MaxUint64, 1<<53 + 1}),
		ctxElement(core.NewTag(0x0018, 0x1149), core.VRIS, core.StringValue{"3", "4"}),
		ctxElement(core.NewTag(0x0010, 0x0010), core.VRPN, core.StringValue{"DOE^JANE", "SMITH^JOHN^A^DR^III"}),
		ctxElement(core.NewTag(0x0028, 0x1052), core.VRDS, core.StringValue{"-1024"}),
		ctxElement(core.NewTag(0x0018, 0x9311), core.VRFD, core.Float64Value{0.1, math.MaxFloat64}),
		ctxElement(core.NewTag(0x0018, 0x9087), core.VRFD, core.Float64Value{-0.0}),
		ctxElement(core.NewTag(0x0010, 0x9431), core.VRFL, core.Float32Value{math.MaxFloat32}),
	}
	source := object.FromElements(elements, std.Dictionary)
	want := ctxSemantic(t, source)
	for _, syntax := range []transfer.Syntax{transfer.ExplicitVRLittleEndian, transfer.ExplicitVRBigEndian, transfer.ImplicitVRLittleEndian} {
		read := ctxRead(t, ctxWrite(t, source, syntax))
		// Implicit VR relabels from the dictionary and resolves SS from
		// Pixel Representation, so every VR is expected to survive.
		got := ctxSemantic(t, read.Dataset)
		if diff := dicomtest.DiffSemantic(got, want); diff != "" {
			t.Fatalf("%s changed values: %s", syntax.Name, diff)
		}
	}
	document, err := dicomjson.Marshal(source, dicomjson.Options{})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(document), "9223372036854775807") || !strings.Contains(string(document), "18446744073709551615") || !strings.Contains(string(document), "9007199254740993") {
		t.Fatal("JSON 64-bit integers were rounded through float64")
	}
	fromJSON, err := dicomjson.UnmarshalWithOptions(document, std.Dictionary, dicomjson.DefaultUnmarshalOptions())
	if err != nil {
		t.Fatal(err)
	}
	if diff := dicomtest.DiffSemantic(ctxSemantic(t, fromJSON), want); diff != "" {
		t.Fatalf("JSON changed values: %s", diff)
	}
	document, err = dicomxml.Marshal(source, dicomxml.Options{})
	if err != nil {
		t.Fatal(err)
	}
	fromXML, err := dicomxml.UnmarshalWithOptions(document, std.Dictionary, dicomxml.DefaultUnmarshalOptions())
	if err != nil {
		t.Fatal(err)
	}
	if diff := dicomtest.DiffSemantic(ctxSemantic(t, fromXML), want); diff != "" {
		t.Fatalf("XML changed values: %s", diff)
	}
}

// The comparator used above must detect the corruptions this issue names.
func TestContextualComparatorDetectsOmissionSignAndTruncation(t *testing.T) {
	base := ctxSignedImage()
	want := ctxSemantic(t, object.FromElements(base, std.Dictionary))
	for _, tc := range []struct {
		name   string
		mutate func([]core.Element) []core.Element
	}{
		{"omitted Rows", func(e []core.Element) []core.Element {
			out := e[:0:0]
			for _, element := range e {
				if element.Tag() != ctxTagRows && element.Tag() != ctxTagColumns {
					out = append(out, element)
				}
			}
			return out
		}},
		{"sign change by VR", func(e []core.Element) []core.Element {
			for i := range e {
				if e[i].Tag() == ctxTagSmallestPixel {
					e[i] = core.NewRawElement(ctxTagSmallestPixel, core.VRUS, []byte{0x30, 0xF8})
				}
			}
			return e
		}},
		{"numeric truncation", func(e []core.Element) []core.Element {
			for i := range e {
				if e[i].Tag() == ctxTagRows {
					e[i] = ctxElement(ctxTagRows, core.VRUS, core.Uint16Value{uint16(70000 & 0xFFFF)})
				}
			}
			return e
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			mutated := tc.mutate(append([]core.Element(nil), base...))
			if diff := dicomtest.DiffSemantic(ctxSemantic(t, object.FromElements(mutated, std.Dictionary)), want); diff == "" {
				t.Fatal("comparator accepted the corruption")
			}
		})
	}
}

// Typed O* values are encoded at their VR width in the target byte order.
func TestTypedBinaryVRsEncodeAtTheirWidth(t *testing.T) {
	type typed struct {
		tag   core.Tag
		vr    core.VR
		value core.Value
		le    []byte
	}
	cases := []typed{
		{core.NewTag(0x0028, 0x1201), core.VROW, core.Uint16Value{0x0102, 0xFFFF}, []byte{0x02, 0x01, 0xFF, 0xFF}},
		{core.NewTag(0x0066, 0x0040), core.VROL, core.Uint32Value{0x01020304}, []byte{0x04, 0x03, 0x02, 0x01}},
		{core.NewTag(0x0066, 0x0129), core.VROV, core.Uint64Value{0x0102030405060708}, []byte{8, 7, 6, 5, 4, 3, 2, 1}},
		{core.NewTag(0x0064, 0x0009), core.VROF, core.Float32Value{1.5}, []byte{0x00, 0x00, 0xC0, 0x3F}},
		{core.NewTag(0x0066, 0x0022), core.VROD, core.Float64Value{-2}, []byte{0, 0, 0, 0, 0, 0, 0, 0xC0}},
	}
	elements := ctxWithSOP()
	for _, c := range cases {
		elements = append(elements, ctxElement(c.tag, c.vr, c.value))
	}
	source := object.FromElements(elements, std.Dictionary)
	for _, syntax := range []transfer.Syntax{transfer.ExplicitVRLittleEndian, transfer.ExplicitVRBigEndian} {
		read := ctxRead(t, ctxWrite(t, source, syntax))
		for _, c := range cases {
			want := append([]byte(nil), c.le...)
			if syntax.ByteOrder == binary.BigEndian {
				width := map[core.VR]int{core.VROW: 2, core.VROL: 4, core.VROF: 4, core.VROV: 8, core.VROD: 8}[c.vr]
				for i := 0; i < len(want); i += width {
					for l, r := i, i+width-1; l < r; l, r = l+1, r-1 {
						want[l], want[r] = want[r], want[l]
					}
				}
			}
			element, ok := read.Dataset.Get(c.tag)
			if !ok || element.VR() != c.vr {
				t.Fatalf("%s %s: element %v VR %s", syntax.Name, c.tag, ok, element.VR())
			}
			if got, _ := read.Dataset.GetRaw(c.tag); !bytes.Equal(got, want) {
				t.Fatalf("%s %s = % x, want % x", syntax.Name, c.vr, got, want)
			}
		}
	}
}
