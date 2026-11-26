package encapdoc

import (
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"testing"
)

func payloadSTLFixture() []byte {
	v := make([]byte, 134)
	copy(v, "Synthetic STL")
	binary.LittleEndian.PutUint32(v[80:], 1)
	return v
}
func TestInspectPayloadDocumentFamiliesPreserveBytes(t *testing.T) {
	for _, c := range []struct {
		mime, sop string
		data      []byte
		id        string
	}{
		{MIMETypePDF, EncapsulatedPDFStorage, []byte("%PDF-1.7\n1 0 obj <<>> endobj\n%%EOF\n"), ""},
		{MIMETypeCDA, EncapsulatedCDAStorage, []byte(`<ClinicalDocument xmlns="urn:hl7-org:v3"><id root="1.2.3" extension="synthetic"/></ClinicalDocument>`), "1.2.3^synthetic"},
		{MIMETypeSTL, EncapsulatedSTLStorage, payloadSTLFixture(), ""},
		{MIMETypeSTL, EncapsulatedSTLStorage, []byte("solid synthetic\nfacet normal 0 0 1\nouter loop\nvertex 0 0 0\nvertex 1 0 0\nvertex 0 1 0\nendloop\nendfacet\nendsolid synthetic\n"), ""},
		{MIMETypeOBJ, EncapsulatedOBJStorage, []byte("mtllib https://invalid.example/not-fetched.mtl\nv 0 0 0\nv 1 0 0\nv 0 1 0\nf 1 2 3\n"), ""},
		{MIMETypeMTL, EncapsulatedMTLStorage, []byte("newmtl synthetic\nKd 1 0 0\nmap_Kd https://invalid.example/not-fetched.png\n"), ""},
	} {
		t.Run(c.mime+"/"+c.sop, func(t *testing.T) {
			original := bytes.Clone(c.data)
			r, e := InspectPayload(context.Background(), c.mime, c.data, PayloadLimits{})
			if e != nil || r.SOPClassUID != c.sop || r.HL7InstanceIdentifier != c.id || !bytes.Equal(original, c.data) {
				t.Fatalf("result=%+v error=%v", r, e)
			}
		})
	}
}
func TestInspectPayloadRejectsContradictoryMalformedFamilies(t *testing.T) {
	for _, c := range []struct {
		mime string
		data []byte
	}{
		{MIMETypePDF, []byte("%PDF-2.7\n%%EOF")}, {MIMETypePDF, []byte("not PDF %%EOF")}, {MIMETypePDF, []byte("%PDF-1.7 missing EOF")},
		{MIMETypeCDA, []byte(`<ClinicalDocument xmlns="urn:hl7-org:v3"><id root="1.2.3"/><id root="1.2.4"/></ClinicalDocument>`)},
		{MIMETypeCDA, []byte(`<wrong xmlns="urn:hl7-org:v3"><id root="1.2.3"/></wrong>`)},
		{MIMETypeCDA, []byte(`<!DOCTYPE ClinicalDocument SYSTEM "https://invalid.example/never-fetch"><ClinicalDocument xmlns="urn:hl7-org:v3"><id root="1.2.3"/></ClinicalDocument>`)},
		{MIMETypeCDA, []byte(`<ClinicalDocument><id root="1.2.3"/></ClinicalDocument>`)},
		{MIMETypeSTL, []byte("solid bogus\nendsolid bogus\n")},
		{MIMETypeOBJ, []byte("v NaN 0 0\nf 1 1 1\n")}, {MIMETypeOBJ, []byte("not an OBJ")}, {MIMETypeOBJ, []byte("v 0 0 0\nf 1 2 3\n")}, {MIMETypeOBJ, []byte("v 0 0 0\nf /1 /1 /1\n")},
		{MIMETypeMTL, []byte("newmtl x\nKd Inf 0 0")}, {MIMETypeMTL, []byte("Kd 1 0 0")},
	} {
		t.Run(c.mime+"/"+string(c.data[:minPayloadTest(12, len(c.data))]), func(t *testing.T) {
			if _, e := InspectPayload(context.Background(), c.mime, c.data, PayloadLimits{}); e == nil {
				t.Fatal("malformed payload accepted")
			}
		})
	}
	v := payloadSTLFixture()
	binary.LittleEndian.PutUint32(v[84:], 0x7fc00000)
	if _, e := InspectPayload(context.Background(), MIMETypeSTL, v, PayloadLimits{}); !errors.Is(e, ErrInvalidDocument) {
		t.Fatal("nonfinite STL accepted", e)
	}
}
func minPayloadTest(a, b int) int {
	if a < b {
		return a
	}
	return b
}
func TestInspectPayloadLimitsAndCancellation(t *testing.T) {
	data := []byte(`<ClinicalDocument xmlns="urn:hl7-org:v3"><id root="1.2.3"/><component><nested/></component></ClinicalDocument>`)
	for _, limits := range []PayloadLimits{{MaxBytes: int64(len(data) - 1)}, {MaxDepth: 2}, {MaxRecords: 2}} {
		if _, e := InspectPayload(context.Background(), MIMETypeCDA, data, limits); !errors.Is(e, ErrEncapsulatedDocumentTooLarge) {
			t.Fatal("limit not enforced", e)
		}
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, e := InspectPayload(ctx, MIMETypeCDA, data, PayloadLimits{}); !errors.Is(e, context.Canceled) {
		t.Fatal("cancellation ignored", e)
	}
	if _, e := InspectPayload(context.Background(), MIMETypeOBJ, []byte("v 0 0 0\nf 1 1 1\n"), PayloadLimits{MaxRecords: -1}); e == nil {
		t.Fatal("negative limit accepted")
	}
}
