package dicomweb

import (
	"context"
	"encoding/base64"
	"encoding/binary"
	"encoding/xml"
	"testing"

	"github.com/ThalesMMS/dicom-go/core"
	"github.com/ThalesMMS/dicom-go/dicomxml"
	"github.com/ThalesMMS/dicom-go/dictionary/std"
	"github.com/ThalesMMS/dicom-go/internal/valueencode"
	"github.com/ThalesMMS/dicom-go/object"
	"github.com/ThalesMMS/dicom-go/transfer"
)

var xmlByteOrderLittle = []byte{1, 2, 3, 4, 5, 6, 7, 8, 9, 10, 11, 12, 13, 14, 15, 16}

// TestDatasetFromDICOMXMLKeepsLittleEndianInlineBinary checks that XML
// metadata whose transfer syntax is Big Endian becomes JSON metadata with the
// same Little Endian OW and OF InlineBinary, as for a Little Endian syntax.
func TestDatasetFromDICOMXMLKeepsLittleEndianInlineBinary(t *testing.T) {
	inline := base64.StdEncoding.EncodeToString(xmlByteOrderLittle)
	document := `<?xml version="1.0" encoding="UTF-8"?><NativeDicomModel xmlns="` + dicomxml.Namespace + `" xml:space="preserve">` +
		`<DicomAttribute tag="00281201" vr="OW" keyword="RedPaletteColorLookupTableData"><InlineBinary>` + inline + `</InlineBinary></DicomAttribute>` +
		`<DicomAttribute tag="00660016" vr="OF" keyword="PointCoordinatesData"><InlineBinary>` + inline + `</InlineBinary></DicomAttribute>` +
		`</NativeDicomModel>`
	for _, syntax := range []transfer.Syntax{transfer.ExplicitVRBigEndian, transfer.ExplicitVRLittleEndian} {
		dataset, err := (Client{}).datasetFromDICOMXML(context.Background(), []byte(document), 1<<20, syntax)
		if err != nil {
			t.Fatalf("%s: %v", syntax.Name, err)
		}
		for _, tag := range []string{"00281201", "00660016"} {
			if got := dataset[tag].InlineBinary; got != inline {
				t.Errorf("%s: %s InlineBinary %q, want %q", syntax.Name, tag, got, inline)
			}
		}
	}
}

// TestEncodeStoreMetadataWritesLittleEndianXMLInlineBinary checks that the
// STOW-RS XML metadata of a Big Endian instance carries Little Endian OW and
// OF InlineBinary.
func TestEncodeStoreMetadataWritesLittleEndianXMLInlineBinary(t *testing.T) {
	instance := object.FromElements([]core.Element{
		core.NewRawElement(core.NewTag(0x0028, 0x1201), core.VROW, valueencode.ReverseFixedWidth(xmlByteOrderLittle, 2)),
		core.NewRawElement(core.NewTag(0x0066, 0x0016), core.VROF, valueencode.ReverseFixedWidth(xmlByteOrderLittle, 4)),
	}, std.Dictionary)
	instance.SetValueByteOrder(binary.BigEndian)
	encoded, err := encodeStoreMetadata(context.Background(), MetadataMediaTypeDICOMXML, []*object.Object{instance})
	if err != nil {
		t.Fatal(err)
	}
	var model struct {
		Attributes []struct {
			Tag    string `xml:"tag,attr"`
			Inline string `xml:"InlineBinary"`
		} `xml:"DicomAttribute"`
	}
	if len(encoded) != 1 {
		t.Fatalf("%d metadata parts", len(encoded))
	}
	if err := xml.Unmarshal(encoded[0], &model); err != nil {
		t.Fatal(err)
	}
	want := base64.StdEncoding.EncodeToString(xmlByteOrderLittle)
	if len(model.Attributes) != 2 {
		t.Fatalf("attributes %+v", model.Attributes)
	}
	for _, attr := range model.Attributes {
		if attr.Inline != want {
			t.Errorf("%s InlineBinary %q, want %q", attr.Tag, attr.Inline, want)
		}
	}
}
