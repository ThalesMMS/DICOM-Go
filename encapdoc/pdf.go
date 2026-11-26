// Package encapdoc builds DICOM Encapsulated Document objects.
package encapdoc

import (
	"encoding/binary"
	"errors"
	"fmt"
	"math"
	"strings"

	"github.com/ThalesMMS/dicom-go/core"
	"github.com/ThalesMMS/dicom-go/object"
)

const (
	EncapsulatedPDFStorage = "1.2.840.10008.5.1.4.1.1.104.1"
	BurnedInAnnotationYes  = "YES"
	BurnedInAnnotationNo   = "NO"
)

var (
	ErrEncapsulatedDocumentTooLarge = errors.New("encapdoc: encapsulated document is too large")
	ErrInvalidBurnedInAnnotation    = errors.New("encapdoc: Burned In Annotation must be YES or NO")
)

var (
	tagSpecificCharacterSet       = core.NewTag(0x0008, 0x0005)
	tagContentDate                = core.NewTag(0x0008, 0x0023)
	tagAcquisitionDateTime        = core.NewTag(0x0008, 0x002A)
	tagContentTime                = core.NewTag(0x0008, 0x0033)
	tagModality                   = core.NewTag(0x0008, 0x0060)
	tagPatientName                = core.NewTag(0x0010, 0x0010)
	tagPatientID                  = core.NewTag(0x0010, 0x0020)
	tagStudyInstanceUID           = core.NewTag(0x0020, 0x000D)
	tagSeriesInstanceUID          = core.NewTag(0x0020, 0x000E)
	tagSeriesNumber               = core.NewTag(0x0020, 0x0011)
	tagInstanceNumber             = core.NewTag(0x0020, 0x0013)
	tagBurnedInAnnotation         = core.NewTag(0x0028, 0x0301)
	tagConceptNameCodeSequence    = core.NewTag(0x0040, 0xA043)
	tagDocumentTitle              = core.NewTag(0x0042, 0x0010)
	tagEncapsulatedDocument       = core.NewTag(0x0042, 0x0011)
	tagMIMETypeOfEncapsulatedDoc  = core.NewTag(0x0042, 0x0012)
	tagEncapsulatedDocumentLength = core.NewTag(0x0042, 0x0015)
	tagConversionType             = core.NewTag(0x0008, 0x0064)
	tagSeriesDescription          = core.NewTag(0x0008, 0x103E)
	tagSOPClassUID                = core.NewTag(0x0008, 0x0016)
	tagSOPInstanceUID             = core.NewTag(0x0008, 0x0018)
)

// PDFOptions describe an Encapsulated PDF (PS3.3 A.45.1).
type PDFOptions = DocumentOptions

// NewPDF builds a DICOM Encapsulated PDF object using Explicit VR Little Endian.
// Empty SeriesNumber and InstanceNumber default to "1"; Type 2 document
// attributes are emitted even when their values are unknown.
func NewPDF(opts PDFOptions) (*object.File, error) {
	return build(kindPDF, opts, nil)
}

func defaultString(value, fallback string) string {
	if value = strings.TrimSpace(value); value != "" {
		return value
	}
	return fallback
}

func documentLengthElement(length int) (core.Element, error) {
	if length > math.MaxUint32 {
		return core.Element{}, fmt.Errorf("%w: %d bytes", ErrEncapsulatedDocumentTooLarge, length)
	}
	return object.ULElement(tagEncapsulatedDocumentLength, binary.LittleEndian, uint32(length)), nil
}

func stringElement(tag core.Tag, vr core.VR, value string) core.Element {
	return core.Element{
		Header: core.ElementHeader{Tag: tag, VR: vr},
		Value:  core.StringValue{strings.TrimSpace(value)},
	}
}
