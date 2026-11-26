package encapdoc

import (
	"bytes"
	"encoding/binary"
	"encoding/xml"
	"errors"
	"fmt"
	"io"
	"strings"
	"unicode/utf8"

	"github.com/ThalesMMS/dicom-go/core"
	"github.com/ThalesMMS/dicom-go/dictionary/std"
	"github.com/ThalesMMS/dicom-go/internal/iccprofile"
	"github.com/ThalesMMS/dicom-go/object"
	"github.com/ThalesMMS/dicom-go/transfer"
)

// Storage SOP Classes of the Encapsulated Document IODs (PS3.4 B.5).
const (
	EncapsulatedCDAStorage = "1.2.840.10008.5.1.4.1.1.104.2"
	EncapsulatedSTLStorage = "1.2.840.10008.5.1.4.1.1.104.3"
	EncapsulatedOBJStorage = "1.2.840.10008.5.1.4.1.1.104.4"
	EncapsulatedMTLStorage = "1.2.840.10008.5.1.4.1.1.104.5"
)

// MIME Types of Encapsulated Document (0042,0012) for each IOD (PS3.3
// A.45.1.4.1, A.45.2.4.1, A.85).
const (
	MIMETypePDF = "application/pdf"
	MIMETypeCDA = "text/XML"
	MIMETypeSTL = "model/stl"
	MIMETypeOBJ = "model/obj"
	MIMETypeMTL = "model/mtl"
)

var (
	// ErrInvalidDocument reports content that does not match its format: a
	// CDA that is not XML, an STL that is neither binary nor ASCII STL, an
	// OBJ or MTL that is not text.
	ErrInvalidDocument = errors.New("encapdoc: invalid encapsulated document")
	// ErrMissingHL7InstanceIdentifier reports a CDA without HL7 Instance
	// Identifier (0040,E001), given or read from its ClinicalDocument id.
	ErrMissingHL7InstanceIdentifier = errors.New("encapdoc: CDA needs an HL7 Instance Identifier")
	// ErrMissingFrameOfReference reports an STL or OBJ model without a Frame
	// of Reference UID, given or inherited from its source.
	ErrMissingFrameOfReference = errors.New("encapdoc: 3D model needs a Frame of Reference UID")
	// ErrUnsupportedSOPClass reports a data set that is not an Encapsulated
	// Document.
	ErrUnsupportedSOPClass = errors.New("encapdoc: not an encapsulated document")
)

var (
	tagHL7InstanceIdentifier      = core.NewTag(0x0040, 0xE001)
	tagFrameOfReferenceUID        = core.NewTag(0x0020, 0x0052)
	tagMeasurementUnitsCodeSeq    = core.NewTag(0x0040, 0x08EA)
	tagCodeValue                  = core.NewTag(0x0008, 0x0100)
	tagCodingSchemeDesignator     = core.NewTag(0x0008, 0x0102)
	tagCodeMeaning                = core.NewTag(0x0008, 0x0104)
	tagPositionReferenceIndicator = core.NewTag(0x0020, 0x1040)
	tagICCProfile                 = core.NewTag(0x0028, 0x2000)
	tagColorSpace                 = core.NewTag(0x0028, 0x2002)
)

// DocumentOptions are the attributes common to every Encapsulated Document
// IOD. PDFOptions is the same type.
type DocumentOptions struct {
	SOPInstanceUID      string
	StudyInstanceUID    string
	SeriesInstanceUID   string
	PatientName         string
	PatientID           string
	SeriesDescription   string
	SeriesNumber        string
	InstanceNumber      string
	ContentDate         string
	ContentTime         string
	AcquisitionDateTime string
	// BurnedInAnnotation accepts YES or NO. Empty defaults to YES for PDF and
	// CDA, which may contain identifying text that DICOM treats as burned
	// in, and to NO for 3D models.
	BurnedInAnnotation string
	DocumentTitle      string
	Data               []byte
	// Common fills the Patient, Study and equipment modules (and the Frame of
	// Reference of 3D models) from a source instance of the same patient
	// (object.InheritCommonModules); the explicit fields above win over it.
	Common object.DerivedCommon
}

// CDAOptions describe an Encapsulated CDA (PS3.3 A.45.2).
type CDAOptions struct {
	DocumentOptions
	// HL7InstanceIdentifier is HL7 Instance Identifier (0040,E001), root and
	// optional extension joined by '^'. Empty reads the ClinicalDocument id.
	// A supplied value must agree with the XML id when it is available.
	HL7InstanceIdentifier string
}

// Code is a coded entry: Code Value, Coding Scheme Designator and Code
// Meaning.
type Code struct {
	Value, Scheme, Meaning string
}

// Millimetre is the UCUM code of the default model units.
var Millimetre = Code{Value: "mm", Scheme: "UCUM", Meaning: "millimeter"}

// ModelOptions describe an Encapsulated STL, OBJ or MTL (PS3.3 A.85).
type ModelOptions struct {
	DocumentOptions
	// FrameOfReferenceUID places an STL or OBJ model in patient space; empty
	// inherits it from Common's source. MTL files have no Frame of Reference.
	FrameOfReferenceUID string
	// MeasurementUnits are the units of the model coordinates (Manufacturing
	// 3D Model Module); zero is Millimetre.
	MeasurementUnits Code
	// ICCProfile is ICC Profile (0028,2000) of the colours of an OBJ or MTL;
	// nil writes an sRGB profile with Color Space (0028,2002) SRGB. PS3.3
	// 2026d lists the ICC Profile Module as User optional there, but
	// dciodvfy requires it, so it is always written. STL has no colours and
	// ignores it.
	ICCProfile []byte
}

// kind is the per-IOD part of a build.
type kind struct {
	sopClassUID     string
	mimeType        string
	modality        string
	burnedInDefault string
	secondaryCap    bool // SC Equipment Module (Conversion Type)
	frameOfRef      bool
	model           bool
	iccProfile      bool
	validate        func([]byte) error
}

var (
	kindPDF = kind{sopClassUID: EncapsulatedPDFStorage, mimeType: MIMETypePDF, modality: "DOC", burnedInDefault: BurnedInAnnotationYes, secondaryCap: true}
	kindCDA = kind{sopClassUID: EncapsulatedCDAStorage, mimeType: MIMETypeCDA, modality: "DOC", burnedInDefault: BurnedInAnnotationYes, secondaryCap: true}
	kindSTL = kind{sopClassUID: EncapsulatedSTLStorage, mimeType: MIMETypeSTL, modality: "M3D", burnedInDefault: BurnedInAnnotationNo, frameOfRef: true, model: true, validate: validateSTL}
	kindOBJ = kind{sopClassUID: EncapsulatedOBJStorage, mimeType: MIMETypeOBJ, modality: "M3D", burnedInDefault: BurnedInAnnotationNo, frameOfRef: true, model: true, iccProfile: true, validate: validateText}
	kindMTL = kind{sopClassUID: EncapsulatedMTLStorage, mimeType: MIMETypeMTL, modality: "M3D", burnedInDefault: BurnedInAnnotationNo, model: true, iccProfile: true, validate: validateText}
)

// NewCDA builds an Encapsulated CDA object. The document must be XML; its
// HL7 Instance Identifier is taken from the options or from the id of the
// ClinicalDocument. If both are present they must agree.
func NewCDA(opts CDAOptions) (*object.File, error) {
	sourceIdentifier, err := cdaInstanceIdentifier(opts.Data)
	if err != nil {
		return nil, err
	}
	identifier := strings.TrimSpace(opts.HL7InstanceIdentifier)
	if identifier == "" {
		identifier = sourceIdentifier
	}
	if sourceIdentifier != "" {
		if err := compareCDAIdentifiers(identifier, sourceIdentifier); err != nil {
			return nil, err
		}
	}
	if identifier == "" {
		return nil, ErrMissingHL7InstanceIdentifier
	}
	return build(kindCDA, opts.DocumentOptions, func(dataset *object.Object) error {
		dataset.Put(stringElement(tagHL7InstanceIdentifier, core.VRST, identifier))
		return nil
	})
}

// NewSTL builds an Encapsulated STL object from a binary or ASCII STL file.
func NewSTL(opts ModelOptions) (*object.File, error) { return buildModel(kindSTL, opts) }

// NewOBJ builds an Encapsulated OBJ object from a Wavefront OBJ file.
func NewOBJ(opts ModelOptions) (*object.File, error) { return buildModel(kindOBJ, opts) }

// NewMTL builds an Encapsulated MTL object from a Wavefront material file.
func NewMTL(opts ModelOptions) (*object.File, error) { return buildModel(kindMTL, opts) }

func buildModel(k kind, opts ModelOptions) (*object.File, error) {
	units := opts.MeasurementUnits
	if units == (Code{}) {
		units = Millimetre
	}
	if strings.TrimSpace(units.Value) == "" || strings.TrimSpace(units.Scheme) == "" || strings.TrimSpace(units.Meaning) == "" {
		return nil, fmt.Errorf("%w: incomplete measurement units %+v", ErrInvalidDocument, units)
	}
	return build(k, opts.DocumentOptions, func(dataset *object.Object) error {
		dataset.Put(object.SequenceElement(tagMeasurementUnitsCodeSeq, object.FromElements([]core.Element{
			stringElement(tagCodeValue, core.VRSH, units.Value),
			stringElement(tagCodingSchemeDesignator, core.VRSH, units.Scheme),
			stringElement(tagCodeMeaning, core.VRLO, units.Meaning),
		}, std.Dictionary)))
		if k.iccProfile {
			profile := opts.ICCProfile
			if profile == nil {
				profile = iccprofile.SRGB()
				dataset.Put(stringElement(tagColorSpace, core.VRCS, "SRGB"))
			}
			dataset.Put(core.NewRawElement(tagICCProfile, core.VROB, append([]byte(nil), profile...)))
		}
		if !k.frameOfRef {
			return nil
		}
		if uid := strings.TrimSpace(opts.FrameOfReferenceUID); uid != "" {
			dataset.Put(stringElement(tagFrameOfReferenceUID, core.VRUI, uid))
			if _, ok := dataset.Get(tagPositionReferenceIndicator); !ok {
				dataset.Put(stringElement(tagPositionReferenceIndicator, core.VRLO, ""))
			}
		}
		if uid, ok := dataset.GetUID(tagFrameOfReferenceUID); !ok || strings.TrimSpace(uid) == "" {
			return ErrMissingFrameOfReference
		}
		return nil
	})
}

// build writes the modules every Encapsulated Document IOD shares, then
// extra for the IOD's own attributes.
func build(k kind, opts DocumentOptions, extra func(*object.Object) error) (*object.File, error) {
	if strings.TrimSpace(opts.SOPInstanceUID) == "" {
		return nil, object.ErrMissingSOPInstanceUID
	}
	burnedInAnnotation := strings.ToUpper(strings.TrimSpace(opts.BurnedInAnnotation))
	if burnedInAnnotation == "" {
		burnedInAnnotation = k.burnedInDefault
	}
	if burnedInAnnotation != BurnedInAnnotationYes && burnedInAnnotation != BurnedInAnnotationNo {
		return nil, fmt.Errorf("%w: %q", ErrInvalidBurnedInAnnotation, opts.BurnedInAnnotation)
	}
	if k.validate != nil {
		if err := k.validate(opts.Data); err != nil {
			return nil, err
		}
	}
	lengthElement, err := documentLengthElement(len(opts.Data))
	if err != nil {
		return nil, err
	}
	elements := []core.Element{
		stringElement(tagSpecificCharacterSet, core.VRCS, "ISO_IR 192"),
		stringElement(tagSOPClassUID, core.VRUI, k.sopClassUID),
		stringElement(tagSOPInstanceUID, core.VRUI, opts.SOPInstanceUID),
		stringElement(tagContentDate, core.VRDA, opts.ContentDate),
		stringElement(tagAcquisitionDateTime, core.VRDT, opts.AcquisitionDateTime),
		stringElement(tagContentTime, core.VRTM, opts.ContentTime),
		stringElement(tagModality, core.VRCS, k.modality),
		stringElement(tagSeriesDescription, core.VRLO, opts.SeriesDescription),
		stringElement(tagStudyInstanceUID, core.VRUI, opts.StudyInstanceUID),
		stringElement(tagSeriesInstanceUID, core.VRUI, opts.SeriesInstanceUID),
		stringElement(tagSeriesNumber, core.VRIS, defaultString(opts.SeriesNumber, "1")),
		stringElement(tagInstanceNumber, core.VRIS, defaultString(opts.InstanceNumber, "1")),
		stringElement(tagPatientName, core.VRPN, opts.PatientName),
		stringElement(tagPatientID, core.VRLO, opts.PatientID),
		stringElement(tagBurnedInAnnotation, core.VRCS, burnedInAnnotation),
		object.SequenceElement(tagConceptNameCodeSequence),
		stringElement(tagDocumentTitle, core.VRST, opts.DocumentTitle),
		stringElement(tagMIMETypeOfEncapsulatedDoc, core.VRLO, k.mimeType),
		core.NewRawElement(tagEncapsulatedDocument, core.VROB, append([]byte(nil), opts.Data...)),
		lengthElement,
	}
	if k.secondaryCap {
		elements = append(elements, stringElement(tagConversionType, core.VRCS, "WSD"))
	}
	dataset := object.FromElements(elements, std.Dictionary)
	// Unset patient fields are left to Common, which writes them empty
	// (Type 2) when its source lacks them too.
	for tag, value := range map[core.Tag]string{tagPatientName: opts.PatientName, tagPatientID: opts.PatientID, tagStudyInstanceUID: opts.StudyInstanceUID} {
		if strings.TrimSpace(value) == "" {
			dataset.Remove(tag)
		}
	}
	modules := object.InheritWithoutFrameOfReference
	if k.frameOfRef {
		modules = object.InheritAll
	}
	if err := opts.Common.Apply(dataset, modules); err != nil {
		return nil, err
	}
	if extra != nil {
		if err := extra(dataset); err != nil {
			return nil, err
		}
	}
	return &object.File{Dataset: dataset, TransferSyntax: transfer.ExplicitVRLittleEndian}, nil
}

// cdaInstanceIdentifier checks the XML and derives root^extension from a
// direct id of the ClinicalDocument. The compatibility constructor also
// accepts legacy XML with an explicit envelope identifier.
func cdaInstanceIdentifier(data []byte) (string, error) {
	decoder := xml.NewDecoder(bytes.NewReader(data))
	depth := 0
	hasRoot, inDocument, hasIdentifier := false, false, false
	var namespace, identifier string
	for {
		token, err := decoder.Token()
		if err == io.EOF {
			break
		}
		if err != nil {
			return "", fmt.Errorf("%w: CDA is not XML: %v", ErrInvalidDocument, err)
		}
		switch element := token.(type) {
		case xml.StartElement:
			depth++
			if depth == 1 {
				hasRoot = true
				inDocument = element.Name.Local == "ClinicalDocument"
				namespace = element.Name.Space
			}
			if inDocument && depth == 2 && element.Name.Local == "id" && element.Name.Space == namespace {
				if hasIdentifier {
					return "", ErrInvalidDocument
				}
				hasIdentifier = true
				root, extension, err := cdaIdentifierAttributes(element.Attr)
				if err != nil {
					return "", err
				}
				identifier = strings.TrimSpace(root)
				if identifier != "" && extension != "" {
					identifier += "^" + extension
				}
			}
		case xml.EndElement:
			depth--
		}
	}
	if !hasRoot {
		return "", fmt.Errorf("%w: CDA has no root element", ErrInvalidDocument)
	}
	return identifier, nil
}

// binarySTLLength is the length a binary STL declares: an 80-byte header, a
// 32-bit triangle count and 50 bytes per triangle. ok is false when data is
// too short to hold the header.
func binarySTLLength(data []byte) (int64, bool) {
	if len(data) < 84 {
		return 0, false
	}
	count := int64(binary.LittleEndian.Uint32(data[80:84]))
	return 84 + 50*count, true
}

// validateSTL accepts a binary STL whose triangle count matches its length
// (one padding byte allowed) or an ASCII STL from "solid" to "endsolid".
func validateSTL(data []byte) error {
	if want, ok := binarySTLLength(data); ok && (int64(len(data)) == want || int64(len(data)) == want+1 && data[want] == 0) {
		return nil
	}
	text := bytes.TrimSpace(bytes.TrimRight(data, "\x00"))
	if bytes.HasPrefix(text, []byte("solid")) && bytes.Contains(text, []byte("endsolid")) && utf8.Valid(text) {
		return nil
	}
	return fmt.Errorf("%w: neither a binary STL nor an ASCII STL", ErrInvalidDocument)
}

func lastLine(text []byte) []byte {
	if i := bytes.LastIndexByte(text, '\n'); i >= 0 {
		return text[i+1:]
	}
	return text
}

// validateText accepts UTF-8 text without NUL bytes (padding excepted).
func validateText(data []byte) error {
	text := bytes.TrimRight(data, "\x00")
	if !utf8.Valid(text) || bytes.IndexByte(text, 0) >= 0 {
		return fmt.Errorf("%w: not text", ErrInvalidDocument)
	}
	return nil
}
