package encapdoc

import (
	"bytes"
	"encoding/binary"
	"errors"
	"strings"
	"testing"

	"github.com/ThalesMMS/dicom-go/core"
	"github.com/ThalesMMS/dicom-go/internal/commontest"
	"github.com/ThalesMMS/dicom-go/object"
)

const syntheticCDA = `<?xml version="1.0" encoding="UTF-8"?>
<ClinicalDocument xmlns="urn:hl7-org:v3">
  <typeId root="2.16.840.1.113883.1.3" extension="POCD_HD000040"/>
  <id root="2.25.1051001" extension="doc-7"/>
  <title>Synthetic report.</title>
</ClinicalDocument>
`

// binarySTL is a binary STL of n triangles.
func binarySTL(n int) []byte {
	data := make([]byte, 84+50*n)
	copy(data, "binary STL written for the dicom-go tests")
	binary.LittleEndian.PutUint32(data[80:], uint32(n))
	for i := 84; i < len(data); i++ {
		data[i] = byte(i * 7)
	}
	return data
}

const asciiSTL = "solid cube\n facet normal 0 0 -1\n  outer loop\n   vertex 0 0 0\n   vertex 1 0 0\n   vertex 0 1 0\n  endloop\n endfacet\nendsolid cube\n"

const objModel = "mtllib model.mtl\nv 0 0 0\nv 1 0 0\nv 0 1 0\nf 1 2 3\n"

const mtlMaterial = "newmtl bone\nKd 0.9 0.9 0.8\n"

// roundTrip writes f as Part 10 and reads its data set back.
func roundTrip(t *testing.T, f *object.File) *object.Object {
	t.Helper()
	sopClass, _ := f.Dataset.GetUID(tagSOPClassUID)
	sopInstance, _ := f.Dataset.GetUID(tagSOPInstanceUID)
	f.Meta = object.New(nil)
	f.Meta.Put(core.NewRawElement(core.NewTag(0x0002, 0x0002), core.VRUI, []byte(sopClass)))
	f.Meta.Put(core.NewRawElement(core.NewTag(0x0002, 0x0003), core.VRUI, []byte(sopInstance)))
	if err := f.RebuildFileMeta(); err != nil {
		t.Fatal(err)
	}
	var encoded bytes.Buffer
	if err := object.WriteFile(&encoded, f); err != nil {
		t.Fatal(err)
	}
	back, err := object.ReadFile(&encoded)
	if err != nil {
		t.Fatal(err)
	}
	return back.Dataset
}

func documentOptions(uid string, data []byte) DocumentOptions {
	return DocumentOptions{SOPInstanceUID: uid, SeriesInstanceUID: uid + ".1", DocumentTitle: "Synthetic", Data: data, Common: object.DerivedCommon{Source: commontest.Source()}}
}

// TestEncapsulatedDocumentsRoundTrip builds every Encapsulated Document IOD
// (#1051), writes it, reads it back and extracts the exact payload, with and
// without Encapsulated Document Length (whose absence leaves the format's
// padding rules).
func TestEncapsulatedDocumentsRoundTrip(t *testing.T) {
	model := func(uid string, data []byte) ModelOptions {
		return ModelOptions{DocumentOptions: documentOptions(uid, data), FrameOfReferenceUID: "2.25.1051099"}
	}
	for _, tc := range []struct {
		name     string
		build    func() (*object.File, error)
		data     []byte
		sopClass string
		mime     string
		modality string
	}{
		{"pdf", func() (*object.File, error) {
			return NewPDF(documentOptions("2.25.1051010", []byte("%PDF-1.4\n%%EOF\n")))
		}, []byte("%PDF-1.4\n%%EOF\n"), EncapsulatedPDFStorage, MIMETypePDF, "DOC"},
		{"cda", func() (*object.File, error) {
			return NewCDA(CDAOptions{DocumentOptions: documentOptions("2.25.1051011", []byte(syntheticCDA))})
		}, []byte(syntheticCDA), EncapsulatedCDAStorage, MIMETypeCDA, "DOC"},
		{"binary stl", func() (*object.File, error) { return NewSTL(model("2.25.1051012", binarySTL(3))) }, binarySTL(3), EncapsulatedSTLStorage, MIMETypeSTL, "M3D"},
		{"ascii stl", func() (*object.File, error) { return NewSTL(model("2.25.1051013", []byte(asciiSTL))) }, []byte(asciiSTL), EncapsulatedSTLStorage, MIMETypeSTL, "M3D"},
		{"obj", func() (*object.File, error) { return NewOBJ(model("2.25.1051014", []byte(objModel))) }, []byte(objModel), EncapsulatedOBJStorage, MIMETypeOBJ, "M3D"},
		{"mtl", func() (*object.File, error) {
			return NewMTL(ModelOptions{DocumentOptions: documentOptions("2.25.1051015", []byte(mtlMaterial))})
		}, []byte(mtlMaterial), EncapsulatedMTLStorage, MIMETypeMTL, "M3D"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if len(tc.data)%2 == 0 && tc.name != "binary stl" { // 84 + 50n bytes is always even
				t.Fatalf("fixture of even length %d does not exercise the padding", len(tc.data))
			}
			f, err := tc.build()
			if err != nil {
				t.Fatal(err)
			}
			back := roundTrip(t, f)
			if modality, _ := back.GetString(tagModality); strings.TrimSpace(modality) != tc.modality {
				t.Fatalf("Modality %q", modality)
			}
			doc, err := Extract(back)
			if err != nil {
				t.Fatal(err)
			}
			if !bytes.Equal(doc.Data, tc.data) || doc.SOPClassUID != tc.sopClass || doc.MIMEType != tc.mime || !doc.DeclaredLength {
				t.Fatalf("extracted %q (%s %s declared %v)", doc.Data, doc.SOPClassUID, doc.MIMEType, doc.DeclaredLength)
			}
			if raw, _ := back.GetRaw(tagEncapsulatedDocument); len(raw) != len(tc.data)+len(tc.data)%2 || len(tc.data)%2 == 1 && raw[len(raw)-1] != 0 {
				t.Fatalf("stored value of %d bytes for a %d-byte document", len(raw), len(tc.data))
			}
			// Without the declared length, the format's padding rule applies.
			back.Remove(tagEncapsulatedDocumentLength)
			doc, err = Extract(back)
			if err != nil || !bytes.Equal(doc.Data, tc.data) || doc.DeclaredLength {
				t.Fatalf("without length: %q %v", doc.Data, err)
			}
		})
	}
}

func TestEncapsulatedDocumentRules(t *testing.T) {
	cda, err := NewCDA(CDAOptions{DocumentOptions: documentOptions("2.25.1051020", []byte(syntheticCDA))})
	if err != nil {
		t.Fatal(err)
	}
	if id, _ := cda.Dataset.GetString(tagHL7InstanceIdentifier); strings.TrimSpace(id) != "2.25.1051001^doc-7" {
		t.Fatalf("HL7 Instance Identifier %q", id)
	}
	if doc, _ := Extract(cda.Dataset); doc.HL7InstanceIdentifier != "2.25.1051001^doc-7" {
		t.Fatalf("extracted identifier %q", doc.HL7InstanceIdentifier)
	}
	stl, err := NewSTL(ModelOptions{DocumentOptions: documentOptions("2.25.1051021", binarySTL(1))})
	if err != nil {
		t.Fatalf("STL inheriting its Frame of Reference: %v", err)
	}
	if units, ok := stl.Dataset.GetSequence(tagMeasurementUnitsCodeSeq); !ok || len(units) != 1 {
		t.Fatal("no Measurement Units Code Sequence")
	} else if value, _ := units[0].GetString(tagCodeValue); strings.TrimSpace(value) != "mm" {
		t.Fatalf("units %q", value)
	}
	if burned, _ := stl.Dataset.GetString(tagBurnedInAnnotation); strings.TrimSpace(burned) != BurnedInAnnotationNo {
		t.Fatalf("model Burned In Annotation %q", burned)
	}
	noSource := documentOptions("2.25.1051022", binarySTL(1))
	noSource.Common = object.DerivedCommon{}
	for _, tc := range []struct {
		name string
		err  error
		call func() error
	}{
		{"CDA not XML", ErrInvalidDocument, func() error {
			_, err := NewCDA(CDAOptions{DocumentOptions: documentOptions("2.25.1", []byte("not <xml")), HL7InstanceIdentifier: "2.25.2"})
			return err
		}},
		{"CDA without id", ErrMissingHL7InstanceIdentifier, func() error {
			_, err := NewCDA(CDAOptions{DocumentOptions: documentOptions("2.25.1", []byte("<ClinicalDocument/>"))})
			return err
		}},
		{"STL of wrong length", ErrInvalidDocument, func() error {
			_, err := NewSTL(ModelOptions{DocumentOptions: documentOptions("2.25.1", binarySTL(2)[:150]), FrameOfReferenceUID: "2.25.3"})
			return err
		}},
		{"STL without Frame of Reference", ErrMissingFrameOfReference, func() error {
			_, err := NewSTL(ModelOptions{DocumentOptions: noSource})
			return err
		}},
		{"OBJ with NUL", ErrInvalidDocument, func() error {
			_, err := NewOBJ(ModelOptions{DocumentOptions: documentOptions("2.25.1", []byte("v 0\x000 0\n")), FrameOfReferenceUID: "2.25.3"})
			return err
		}},
		{"not a document", ErrUnsupportedSOPClass, func() error {
			_, err := Extract(object.New(nil))
			return err
		}},
		{"length beyond value", ErrInvalidDocument, func() error {
			f, err := NewPDF(documentOptions("2.25.1051023", []byte("%PDF-1.4\n%%EOF\n")))
			if err != nil {
				return err
			}
			f.Dataset.SetUL(tagEncapsulatedDocumentLength, 99)
			_, err = Extract(f.Dataset)
			return err
		}},
	} {
		if err := tc.call(); !errors.Is(err, tc.err) {
			t.Errorf("%s: %v, want %v", tc.name, err, tc.err)
		}
	}
}
