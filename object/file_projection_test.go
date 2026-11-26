package object_test

import (
	"bytes"
	"context"
	"errors"
	"reflect"
	"testing"

	"github.com/ThalesMMS/dicom-go/core"
	"github.com/ThalesMMS/dicom-go/internal/dicomtest"
	"github.com/ThalesMMS/dicom-go/object"
	"github.com/ThalesMMS/dicom-go/parser"
	"github.com/ThalesMMS/dicom-go/transfer"
	"github.com/ThalesMMS/dicom-go/validation"
)

func TestProjectionPreservesLateCharsetAndImplicitContext(t *testing.T) {
	var elements []core.Element
	for _, element := range ctxSignedImage() {
		if element.Tag() != ctxTagPixelRepresentation {
			elements = append(elements, element)
		}
	}
	name := core.NewTag(0x0010, 0x0010)
	elements = append(elements, core.NewRawElement(name, core.VRPN, []byte("JOS\xc9^EXAMPLE")),
		dicomtest.NewStringElement(core.NewTag(0x0008, 0x0005), core.VRCS, "ISO_IR 100"),
		dicomtest.NewUShortElement(ctxTagPixelRepresentation, 1))
	var encoded bytes.Buffer
	writer := parser.NewWriter(&encoded, transfer.ImplicitVRLittleEndian)
	for _, element := range elements {
		if err := writer.WriteElement(element); err != nil {
			t.Fatal(err)
		}
	}
	wire := encoded.Bytes()
	full, err := object.ReadDataSetWithOptions(bytes.NewReader(wire), transfer.ImplicitVRLittleEndian, object.ReadFileOptions{SkipPixelData: true})
	if err != nil {
		t.Fatal(err)
	}
	defer full.Close()
	tags := []core.Tag{ctxTagRealWorldMapping, ctxTagSmallestPixel, name}
	projected, err := object.ReadDataSetWithOptions(bytes.NewReader(wire), transfer.ImplicitVRLittleEndian, object.ReadFileOptions{SkipPixelData: true, DataSetTags: tags})
	if err != nil {
		t.Fatal(err)
	}
	defer projected.Close()
	for _, tag := range tags {
		got, _ := projected.Get(tag)
		want, _ := full.Get(tag)
		if !reflect.DeepEqual(got, want) {
			t.Fatalf("selected %s changed: %#v / %#v", tag, got, want)
		}
	}
	if got, ok := projected.GetString(name); !ok || got != "JOSÉ^EXAMPLE" {
		t.Fatalf("late charset: %q %v", got, ok)
	}
	if value, _ := projected.Get(ctxTagSmallestPixel); value.VR() != core.VRSS {
		t.Fatal("late root Pixel Representation was lost")
	}
	if !errors.Is(object.WriteDataSet(&bytes.Buffer{}, projected, transfer.ImplicitVRLittleEndian), core.ErrDiscardedValue) {
		t.Fatal("projected object was serializable")
	}
}

func TestProjectionRecoveryReportsMatchCompleteMetadata(t *testing.T) {
	minimal := dicomtest.EncodeElements(transfer.ExplicitVRLittleEndian, dicomtest.MinimalDataset()...)
	for _, wire := range [][]byte{overrunFile(t), noGroupLengthPart10(overrunStream(t)), missingSyntaxPart10(minimal)} {
		path := writeLadderFile(t, wire)
		full, wantReport, err := object.OpenFileWithRecoveryLadder(path, object.ReadFileOptions{SkipPixelData: true}, object.NonConformantReadRecoveries())
		if err != nil {
			t.Fatal(err)
		}
		defer full.Close()
		projected, gotReport, err := object.OpenFileWithRecoveryLadder(path, object.ReadFileOptions{SkipPixelData: true, DataSetTags: []core.Tag{ctxTagSOPClassUID, ctxTagSOPInstanceUID}}, object.NonConformantReadRecoveries())
		if err != nil {
			t.Fatal(err)
		}
		defer projected.Close()
		if !reflect.DeepEqual(gotReport, wantReport) {
			t.Fatalf("recovery classification changed: %+v / %+v", gotReport, wantReport)
		}
		got, gotTruncated := projected.Dataset.BoundaryRecoveries()
		want, wantTruncated := full.Dataset.BoundaryRecoveries()
		if !reflect.DeepEqual(got, want) || gotTruncated != wantTruncated {
			t.Fatal("omitted values lost boundary recovery evidence")
		}
	}
}

func TestProjectionRefusesIncompatibleModes(t *testing.T) {
	for _, opts := range []object.ReadFileOptions{
		{DataSetTags: []core.Tag{}},
		{DataSetTags: []core.Tag{}, SkipPixelData: true, AllowPartialRead: true},
		{DataSetTags: []core.Tag{}, SkipPixelData: true, DeferPixelData: true},
		{DataSetTags: []core.Tag{}, SkipPixelData: true, InlineValueBytesThreshold: 100},
		{DataSetTags: []core.Tag{core.NewTag(0xfffe, 0xe000)}, SkipPixelData: true},
		{SkipPixelData: true, SeekUnselectedSequences: true},
	} {
		if _, err := object.ReadDataSetWithOptions(bytes.NewReader(nil), transfer.ExplicitVRLittleEndian, opts); !errors.Is(err, parser.ErrDataSetProjectionOptions) {
			t.Fatalf("incompatible mode accepted: %v", err)
		}
	}
	if _, _, err := object.ReadDataSetWithValidation(context.Background(), bytes.NewReader(nil), transfer.ExplicitVRLittleEndian, object.ReadFileOptions{SkipPixelData: true, DataSetTags: []core.Tag{}}, validation.Options{}); !errors.Is(err, parser.ErrDataSetProjectionOptions) {
		t.Fatalf("uniform validation projected: %v", err)
	}
}
