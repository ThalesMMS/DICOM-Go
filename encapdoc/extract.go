package encapdoc

import (
	"bytes"
	"context"
	"fmt"
	"strings"

	"github.com/ThalesMMS/dicom-go/object"
)

const pdfEOF = "%%EOF"

// Document is the payload of an Encapsulated Document object.
type Document struct {
	SOPClassUID string
	// MIMEType is MIME Type of Encapsulated Document (0042,0012), or the
	// IOD's type when the attribute is empty.
	MIMEType string
	// Data is the document without the padding of its Encapsulated Document
	// value.
	Data []byte
	// HL7InstanceIdentifier is HL7 Instance Identifier (0040,E001) of a CDA.
	HL7InstanceIdentifier string
	// DeclaredLength reports whether Encapsulated Document Length (0042,0015)
	// gave the length; otherwise the padding was removed by the format's
	// rules (see Extract).
	DeclaredLength bool
}

// Extract returns the document of an Encapsulated PDF, CDA, STL, OBJ or MTL
// object. Encapsulated Document (0042,0011) is padded to an even length;
// Encapsulated Document Length (0042,0015), when present, gives the exact
// length. A contradiction returns a DocumentLengthError before any extracted
// data is returned. Use Object.CopyValueTo for the complete stored value.
// Without it, the padding is removed by format: a PDF ends after its last
// %%EOF line, text formats (CDA, OBJ, MTL, ASCII STL) lose their trailing
// NUL bytes, and a binary STL keeps the length its triangle count declares.
func Extract(obj *object.Object) (Document, error) {
	k, err := documentKind(obj)
	if err != nil {
		return Document{}, err
	}
	element, ok := obj.Get(tagEncapsulatedDocument)
	if !ok {
		return Document{}, fmt.Errorf("%w: Encapsulated Document is missing", ErrInvalidDocument)
	}
	raw, ok := element.RawBytes()
	if !ok {
		return Document{}, fmt.Errorf("%w: Encapsulated Document is not a byte value", ErrInvalidDocument)
	}
	doc := Document{SOPClassUID: k.sopClassUID, MIMEType: k.mimeType}
	if mime, ok := obj.GetString(tagMIMETypeOfEncapsulatedDoc); ok && strings.TrimSpace(mime) != "" {
		doc.MIMEType = strings.TrimSpace(mime)
	}
	if identifier, ok := obj.GetString(tagHL7InstanceIdentifier); ok {
		doc.HL7InstanceIdentifier = strings.TrimSpace(identifier)
	}
	length, err := InspectDocumentLength(context.Background(), obj)
	if err != nil {
		return Document{}, err
	}
	if length.HasDeclaration {
		doc.Data, doc.DeclaredLength = append([]byte(nil), raw[:length.DeclaredBytes]...), true
		return doc, nil
	}
	doc.Data = append([]byte(nil), unpad(k, raw)...)
	return doc, nil
}

func documentKind(obj *object.Object) (kind, error) {
	if obj == nil {
		return kind{}, fmt.Errorf("%w: nil data set", ErrUnsupportedSOPClass)
	}
	sop, _ := obj.GetUID(tagSOPClassUID)
	sop = strings.TrimSpace(sop)
	switch sop {
	case EncapsulatedPDFStorage:
		return kindPDF, nil
	case EncapsulatedCDAStorage:
		return kindCDA, nil
	case EncapsulatedSTLStorage:
		return kindSTL, nil
	case EncapsulatedOBJStorage:
		return kindOBJ, nil
	case EncapsulatedMTLStorage:
		return kindMTL, nil
	default:
		return kind{}, fmt.Errorf("%w: SOP Class %q", ErrUnsupportedSOPClass, sop)
	}
}

// unpad removes the padding of a document without a declared length.
func unpad(k kind, raw []byte) []byte {
	switch k.sopClassUID {
	case EncapsulatedPDFStorage:
		if end := bytes.LastIndex(raw, []byte(pdfEOF)); end >= 0 {
			end += len(pdfEOF)
			for end < len(raw) && (raw[end] == '\r' || raw[end] == '\n') {
				end++
			}
			return raw[:end]
		}
		return bytes.TrimRight(raw, "\x00")
	case EncapsulatedSTLStorage:
		if want, ok := binarySTLLength(raw); ok && want <= int64(len(raw)) && int64(len(raw))-want <= 1 {
			return raw[:want]
		}
		return bytes.TrimRight(raw, "\x00")
	default:
		return bytes.TrimRight(raw, "\x00")
	}
}
