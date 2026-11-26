package pixeldata

import (
	"encoding/binary"
	"slices"
	"testing"

	"github.com/ThalesMMS/dicom-go/core"
	"github.com/ThalesMMS/dicom-go/object"
	"github.com/ThalesMMS/dicom-go/transfer"
)

// jpegHeaderOnly is SOI, an APP0 segment and a SOFn frame header.
func jpegHeaderOnly(marker byte, precision, rows, columns, components int) []byte {
	b := []byte{0xff, 0xd8, 0xff, 0xe0, 0x00, 0x04, 0x00, 0x00, 0xff, 0xff, 0xff, marker}
	b = binary.BigEndian.AppendUint16(b, uint16(8+3*components))
	b = append(b, byte(precision))
	b = binary.BigEndian.AppendUint16(b, uint16(rows))
	b = binary.BigEndian.AppendUint16(b, uint16(columns))
	b = append(b, byte(components))
	for i := 0; i < components; i++ {
		b = append(b, byte(i+1), 0x11, 0)
	}
	return append(b, 0xff, 0xda)
}

func encapsulatedObject(fragment []byte, elements ...core.Element) *object.Object {
	pixel := core.Element{
		Header: core.ElementHeader{Tag: core.TagPixelData, VR: core.VROB, Length: core.UndefinedLength},
		Value:  core.FragmentSequence{Fragments: [][]byte{fragment}},
	}
	obj := object.FromElements(append(elements, uint16RawElement(tagPixelRepresentation, 0), pixel), nil)
	obj.SetValueByteOrder(binary.LittleEndian)
	return obj
}

func TestReconcileWithCodestream(t *testing.T) {
	obj := encapsulatedObject(jpegHeaderOnly(0xc3, 12, 64, 32, 1),
		uint16RawElement(tagRows, 64), uint16RawElement(tagBitsAllocated, 12),
		uint16RawElement(tagBitsStored, 16), uint16RawElement(tagHighBit, 15))
	out, recs := reconcileWithCodestream(obj, transfer.JPEGLosslessSV1)
	got := make([]string, len(recs))
	for i, r := range recs {
		got[i] = r.Attribute + ":" + r.Declared + ">" + r.Used
	}
	want := []string{"Columns:>32", "SamplesPerPixel:>1", "PhotometricInterpretation:>MONOCHROME2",
		"BitsAllocated:12>16", "BitsStored:16>12", "HighBit:15>11"}
	if !slices.Equal(got, want) {
		t.Fatalf("reconciliations %v", got)
	}
	metadata, err := ExtractMetadata(out)
	if err != nil {
		t.Fatal(err)
	}
	if metadata.Columns != 32 || metadata.BitsAllocated != 16 || metadata.BitsStored != 12 || metadata.HighBit != 11 {
		t.Fatalf("metadata %+v", metadata)
	}
	if _, ok := getUint16(obj, tagColumns); ok {
		t.Fatal("the input data set was mutated")
	}
	// A consistent data set, or a non-JPEG syntax, is returned unchanged.
	if same, recs := reconcileWithCodestream(out, transfer.JPEGLosslessSV1); same != out || recs != nil {
		t.Fatalf("consistent data set reconciled: %v", recs)
	}
	if same, recs := reconcileWithCodestream(obj, transfer.JPEGLSLossless); same != obj || recs != nil {
		t.Fatalf("JPEG-LS reconciled: %v", recs)
	}
}

func TestReconcileColorBaseline(t *testing.T) {
	obj := encapsulatedObject(jpegHeaderOnly(0xc0, 8, 16, 16, 3),
		uint16RawElement(tagRows, 19789), uint16RawElement(tagColumns, 19789), uint16RawElement(tagSamplesPerPixel, 1),
		photometricInterpretationElement("MONOCHROME1"), uint16RawElement(tagBitsAllocated, 8),
		uint16RawElement(tagBitsStored, 8), uint16RawElement(tagHighBit, 7))
	out, recs := reconcileWithCodestream(obj, transfer.JPEGBaseline)
	if len(recs) != 5 {
		t.Fatalf("reconciliations %+v", recs)
	}
	metadata, err := ExtractMetadata(out)
	if err != nil {
		t.Fatal(err)
	}
	if metadata.Rows != 16 || metadata.SamplesPerPixel != 3 || normalizedPhotometric(metadata.PhotometricInterpretation) != "YBR_FULL_422" ||
		!metadata.PlanarConfigurationPresent || metadata.PlanarConfiguration != 0 {
		t.Fatalf("metadata %+v", metadata)
	}
}

func FuzzParseJPEGFrameHeader(f *testing.F) {
	f.Add(jpegHeaderOnly(0xc3, 16, 512, 512, 1))
	f.Add([]byte{0xff, 0xd8, 0xff, 0xc0, 0x00, 0x02})
	f.Fuzz(func(t *testing.T, data []byte) {
		header, ok := parseJPEGFrameHeader(data)
		if ok && (header.precision < 0 || header.components < 0) {
			t.Fatalf("header %+v", header)
		}
	})
}
