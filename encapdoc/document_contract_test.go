package encapdoc

import (
	"bytes"
	"context"
	"errors"
	"strconv"
	"testing"
	"time"

	"github.com/ThalesMMS/dicom-go/core"
	"github.com/ThalesMMS/dicom-go/internal/commontest"
	"github.com/ThalesMMS/dicom-go/object"
)

func TestEncapsulatedDocumentAttachmentsShareStudyAndKeepDistinctIdentity(t *testing.T) {
	const sourceStudyUID = "1.2.826.0.1.3680043.10.1.1047.1"
	const acquiredUID = "2.25.817390"
	source := commontest.Source()
	source.Put(stringElement(tagSOPInstanceUID, core.VRUI, acquiredUID))
	source.Put(stringElement(tagSeriesInstanceUID, core.VRUI, acquiredUID+".1"))
	now := time.Date(2026, time.January, 13, 12, 45, 0, 0, time.UTC)

	for index, tc := range []struct {
		name       string
		data       []byte
		build      func(DocumentOptions) (*object.File, error)
		frameOfRef bool
	}{
		{"pdf", []byte("%PDF-2.0\n%%EOF\r\n"), NewPDF, false},
		{"cda", []byte(syntheticCDA), func(opts DocumentOptions) (*object.File, error) {
			return NewCDA(CDAOptions{DocumentOptions: opts})
		}, false},
		{"stl", binarySTL(2), func(opts DocumentOptions) (*object.File, error) {
			return NewSTL(ModelOptions{DocumentOptions: opts})
		}, true},
		{"obj", []byte(objModel), func(opts DocumentOptions) (*object.File, error) {
			return NewOBJ(ModelOptions{DocumentOptions: opts})
		}, true},
		{"mtl", []byte(mtlMaterial), func(opts DocumentOptions) (*object.File, error) {
			return NewMTL(ModelOptions{DocumentOptions: opts})
		}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			uid := "2.25.817391." + strconv.Itoa(index+1)
			options := DocumentOptions{
				SOPInstanceUID:    uid,
				SeriesInstanceUID: uid + ".1",
				DocumentTitle:     "Pièce jointe",
				Data:              bytes.Clone(tc.data),
				Common:            object.DerivedCommon{Source: source, Now: now},
			}
			first, err := tc.build(options)
			if err != nil {
				t.Fatal(err)
			}
			options.SOPInstanceUID, options.SeriesInstanceUID = uid+".2", uid+".3"
			second, err := tc.build(options)
			if err != nil {
				t.Fatal(err)
			}
			options.Data[0] ^= 0xff
			for _, attachment := range []struct {
				file                   *object.File
				instanceUID, seriesUID string
			}{
				{first, uid, uid + ".1"}, {second, uid + ".2", uid + ".3"},
			} {
				back := roundTrip(t, attachment.file)
				for tag, want := range map[core.Tag]string{
					tagSOPInstanceUID:    attachment.instanceUID,
					tagSeriesInstanceUID: attachment.seriesUID,
					tagStudyInstanceUID:  sourceStudyUID,
					tagPatientID:         "SYN-1047",
					tagPatientName:       "Synthétique^Ana",
					tagDocumentTitle:     "Pièce jointe",
				} {
					got, err := back.LookupString(tag)
					if err != nil || got != want {
						t.Errorf("%v = %q, %v; want %q", tag, got, err, want)
					}
				}
				uid, exists := back.GetUID(tagFrameOfReferenceUID)
				if exists != tc.frameOfRef || exists && uid != commontest.FrameOfReferenceUID {
					t.Errorf("Frame of Reference UID = %q, present=%t; want presence %t", uid, exists, tc.frameOfRef)
				}
				document, err := Extract(back)
				if err != nil || !bytes.Equal(document.Data, tc.data) {
					t.Errorf("extract from attachment %s: %v, matching bytes=%t", attachment.instanceUID, err, bytes.Equal(document.Data, tc.data))
				}
			}
		})
	}
	assertString(t, source, tagSOPInstanceUID, acquiredUID)
	assertString(t, source, tagSeriesInstanceUID, acquiredUID+".1")
}

func TestEncapsulatedDocumentExtractionUsesOriginalLengthAndOwnsBytes(t *testing.T) {
	const revisedPDF = "%PDF-1.7\r\n%%EOF\r\nupdate\r\n%%EOF\r\n"
	binaryModel := payloadSTLFixture() // Its facet ends in genuine zero bytes.
	for _, tc := range []struct {
		name   string
		sop    string
		value  []byte
		length uint32 // Zero omits the optional attribute for these fixtures.
		want   []byte
	}{
		{"explicit-even-value", EncapsulatedSTLStorage, binaryModel, uint32(len(binaryModel)), binaryModel},
		{"inferred-binary-stl-keeps-final-zeros", EncapsulatedSTLStorage, binaryModel, 0, binaryModel},
		{"legacy-pdf-with-incremental-eof", EncapsulatedPDFStorage, []byte(revisedPDF + "\x00"), 0, []byte(revisedPDF)},
		{"explicit-odd-value-keeps-document-whitespace", EncapsulatedPDFStorage, []byte("%PDF-2.0\n%%EOF \x00"), 15, []byte("%PDF-2.0\n%%EOF ")},
		{"legacy-cda-preserves-xml-whitespace", EncapsulatedCDAStorage, []byte(syntheticCDA + " \x00"), 0, []byte(syntheticCDA + " ")},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dataset := object.New(nil)
			dataset.Put(stringElement(tagSOPClassUID, core.VRUI, tc.sop))
			dataset.Put(core.NewRawElement(tagEncapsulatedDocument, core.VROB, bytes.Clone(tc.value)))
			if tc.length != 0 {
				dataset.SetUL(tagEncapsulatedDocumentLength, tc.length)
			}
			document, err := Extract(dataset)
			if err != nil || !bytes.Equal(document.Data, tc.want) || document.DeclaredLength != (tc.length != 0) {
				t.Fatalf("Extract length=%d, declared=%t, error=%v; want length %d, declared=%t", len(document.Data), document.DeclaredLength, err, len(tc.want), tc.length != 0)
			}
			document.Data[0] ^= 0xff
			if stored, _ := dataset.GetRaw(tagEncapsulatedDocument); !bytes.Equal(stored, tc.value) {
				t.Fatal("editing the extracted payload changed the encoded DICOM value")
			}
			again, err := Extract(dataset)
			if err != nil || !bytes.Equal(again.Data, tc.want) {
				t.Fatalf("a second extraction did not return the original payload: %v", err)
			}
		})
	}
}

func TestEncapsulatedDocumentExtractionAndPayloadInspectionAreSeparate(t *testing.T) {
	for _, tc := range []struct {
		name, sop, storedMIME, wantMIME, contents string
		inspectErr                                error
	}{
		{"pdf-without-mime", EncapsulatedPDFStorage, "", MIMETypePDF, "%PDF-2.0\n%%EOF\n", nil},
		{"pdf-with-corrupt-body", EncapsulatedPDFStorage, MIMETypePDF, MIMETypePDF, "unrecognized document", ErrInvalidDocument},
		{"pdf-tagged-as-xml", EncapsulatedPDFStorage, MIMETypeCDA, MIMETypeCDA, "%PDF-2.0\n%%EOF\n", ErrInvalidDocument},
		{"xml-without-cda-namespace", EncapsulatedCDAStorage, MIMETypeCDA, MIMETypeCDA, `<ClinicalDocument><id root="2.25.817397"/></ClinicalDocument>`, ErrInvalidDocument},
		{"unrecognized-mime-type", EncapsulatedCDAStorage, "application/octet-stream", "application/octet-stream", syntheticCDA, ErrUnsupportedSOPClass},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dataset := object.New(nil)
			dataset.Put(stringElement(tagSOPClassUID, core.VRUI, tc.sop))
			if tc.storedMIME != "" {
				dataset.Put(stringElement(tagMIMETypeOfEncapsulatedDoc, core.VRLO, tc.storedMIME))
			}
			dataset.Put(core.NewRawElement(tagEncapsulatedDocument, core.VROB, []byte(tc.contents)))
			dataset.SetUL(tagEncapsulatedDocumentLength, uint32(len(tc.contents)))
			document, err := Extract(dataset)
			if err != nil || document.SOPClassUID != tc.sop || document.MIMEType != tc.wantMIME || string(document.Data) != tc.contents {
				t.Fatalf("Extract = %+v, error=%v", document, err)
			}
			info, err := InspectPayload(context.Background(), document.MIMEType, document.Data, PayloadLimits{})
			if !errors.Is(err, tc.inspectErr) {
				t.Fatalf("InspectPayload error=%v, want %v", err, tc.inspectErr)
			}
			if tc.inspectErr != nil && info != (PayloadInfo{}) || tc.inspectErr == nil && info.SOPClassUID != tc.sop {
				t.Fatalf("InspectPayload returned unexpected information %+v", info)
			}
		})
	}
}
