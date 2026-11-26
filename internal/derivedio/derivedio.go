package derivedio

import (
	"encoding/binary"
	"fmt"
	"math"
	"sort"
	"strings"
	"unicode"

	"github.com/ThalesMMS/dicom-go/core"
	"github.com/ThalesMMS/dicom-go/dictionary/std"
	"github.com/ThalesMMS/dicom-go/object"
	"github.com/ThalesMMS/dicom-go/transfer"
)

var (
	TagMediaStorageSOPClassUID    = core.NewTag(0x0002, 0x0002)
	TagMediaStorageSOPInstanceUID = core.NewTag(0x0002, 0x0003)
	TagTransferSyntaxUID          = core.NewTag(0x0002, 0x0010)

	TagSOPClassUID       = core.NewTag(0x0008, 0x0016)
	TagSOPInstanceUID    = core.NewTag(0x0008, 0x0018)
	TagContentDate       = core.NewTag(0x0008, 0x0023)
	TagContentTime       = core.NewTag(0x0008, 0x0033)
	TagModality          = core.NewTag(0x0008, 0x0060)
	TagSeriesDescr       = core.NewTag(0x0008, 0x103E)
	TagRefSOPClassUID    = core.NewTag(0x0008, 0x1150)
	TagRefSOPInstanceUID = core.NewTag(0x0008, 0x1155)
	TagRefFrameNumber    = core.NewTag(0x0008, 0x1160)

	TagStudyInstanceUID    = core.NewTag(0x0020, 0x000D)
	TagSeriesInstanceUID   = core.NewTag(0x0020, 0x000E)
	TagFrameOfReferenceUID = core.NewTag(0x0020, 0x0052)

	TagSamplesPerPixel           = core.NewTag(0x0028, 0x0002)
	TagPhotometricInterpretation = core.NewTag(0x0028, 0x0004)
	TagNumberOfFrames            = core.NewTag(0x0028, 0x0008)
	TagRows                      = core.NewTag(0x0028, 0x0010)
	TagColumns                   = core.NewTag(0x0028, 0x0011)
	TagBitsAllocated             = core.NewTag(0x0028, 0x0100)
	TagBitsStored                = core.NewTag(0x0028, 0x0101)
	TagHighBit                   = core.NewTag(0x0028, 0x0102)
	TagPixelRepresentation       = core.NewTag(0x0028, 0x0103)
	TagPixelData                 = core.NewTag(0x7FE0, 0x0010)
)

// Object builds a DICOM object from the given elements using the standard dictionary.
func Object(elements ...core.Element) *object.Object {
	return object.FromElements(elements, std.Dictionary)
}

// File creates a DICOM file object with the specified SOP class UID, instance UID,
// and dataset using explicit VR little-endian transfer syntax.
// It returns object.ErrMissingSOPClassUID if sopClassUID is empty,
// object.ErrMissingSOPInstanceUID if sopInstanceUID is empty, or an error if dataset is nil.
func File(sopClassUID string, sopInstanceUID string, dataset *object.Object) (*object.File, error) {
	if strings.TrimSpace(sopClassUID) == "" {
		return nil, object.ErrMissingSOPClassUID
	}
	if strings.TrimSpace(sopInstanceUID) == "" {
		return nil, object.ErrMissingSOPInstanceUID
	}
	if dataset == nil {
		return nil, fmt.Errorf("dicom derived: dataset is required")
	}
	return &object.File{
		Meta: Object(
			UI(TagMediaStorageSOPClassUID, sopClassUID),
			UI(TagMediaStorageSOPInstanceUID, sopInstanceUID),
			UI(TagTransferSyntaxUID, transfer.ExplicitVRLittleEndian.UID),
		),
		Dataset:        dataset,
		TransferSyntax: transfer.ExplicitVRLittleEndian,
	}, nil
}

// Str constructs a string element with the specified tag, VR, and value.
func Str(tag core.Tag, vr core.VR, value string) core.Element {
	return core.Element{Header: core.ElementHeader{Tag: tag, VR: vr}, Value: core.StringValue{value}}
}

// Strings constructs a DICOM element containing multiple string values.
func Strings(tag core.Tag, vr core.VR, values []string) core.Element {
	return core.Element{Header: core.ElementHeader{Tag: tag, VR: vr}, Value: core.StringValue(append([]string(nil), values...))}
}

// UI builds a DICOM element for unique identifiers.
func UI(tag core.Tag, value string) core.Element {
	return Str(tag, core.VRUI, value)
}

// CS constructs a Code String element.
func CS(tag core.Tag, value string) core.Element {
	return Str(tag, core.VRCS, value)
}

// LO constructs a string element with Long String (LO) value representation.
func LO(tag core.Tag, value string) core.Element {
	return Str(tag, core.VRLO, value)
}

// SH constructs a string element with SH (Short Header) VR.
func SH(tag core.Tag, value string) core.Element {
	return Str(tag, core.VRSH, value)
}

// IS builds an Integer String element (object.FormatIS). A value outside
// the signed 32-bit range cannot be encoded and is left empty.
func IS(tag core.Tag, values ...int) core.Element {
	out := make([]string, len(values))
	for i, value := range values {
		out[i], _ = object.FormatIS(int64(value))
	}
	return Strings(tag, core.VRIS, out)
}

// DS builds a Decimal String element (object.FormatDS). NaN and infinities
// cannot be encoded and are left empty.
func DS(tag core.Tag, values ...float64) core.Element {
	out := make([]string, len(values))
	for i, value := range values {
		out[i], _ = object.FormatDS(value)
	}
	return Strings(tag, core.VRDS, out)
}

// US constructs an Unsigned Short element in Little Endian, the byte order
// of the Explicit VR Little Endian objects this package builds.
func US(tag core.Tag, values ...uint16) core.Element {
	return object.USElement(tag, binary.LittleEndian, values...)
}

// UL constructs an Unsigned Long element in Little Endian.
func UL(tag core.Tag, values ...uint32) core.Element {
	return object.ULElement(tag, binary.LittleEndian, values...)
}

// SL constructs a Signed Long element in Little Endian.
func SL(tag core.Tag, values ...int32) core.Element {
	return object.SLElement(tag, binary.LittleEndian, values...)
}

// FL constructs a Floating Point Single element in Little Endian.
func FL(tag core.Tag, values ...float64) core.Element {
	single := make([]float32, len(values))
	for i, value := range values {
		single[i] = float32(value)
	}
	return object.FLElement(tag, binary.LittleEndian, single...)
}

// FD constructs a Floating Point Double element in Little Endian.
func FD(tag core.Tag, values ...float64) core.Element {
	return object.FDElement(tag, binary.LittleEndian, values...)
}

// Raw constructs a DICOM element with the specified tag, value representation, and raw binary data.
func Raw(tag core.Tag, vr core.VR, data []byte) core.Element {
	return core.NewRawElement(tag, vr, data)
}

// Seq creates a DICOM sequence element with the specified tag and items.
func Seq(tag core.Tag, items ...core.DataSet) core.Element {
	return core.Element{
		Header: core.ElementHeader{Tag: tag, VR: core.VRSQ, Length: core.UndefinedLength, LengthSet: true},
		Value:  core.SequenceValue{Items: items},
	}
}

// DataSet returns a DataSet containing the provided elements in ascending
// tag order, as PS3.5 7.1 requires of every data set, sequence items
// included.
func DataSet(elements ...core.Element) core.DataSet {
	sort.SliceStable(elements, func(i, j int) bool {
		a, b := elements[i].Tag(), elements[j].Tag()
		if a.Group != b.Group {
			return a.Group < b.Group
		}
		return a.Element < b.Element
	})
	return core.DataSet{Elements: elements}
}

// CleanString retrieves the string value for a tag and returns it with leading and trailing whitespace and null bytes removed. Returns an empty string if the tag is missing.
func CleanString(obj *object.Object, tag core.Tag) string {
	value, ok := obj.GetString(tag)
	if !ok {
		return ""
	}
	return strings.TrimFunc(value, func(r rune) bool {
		return r == 0 || unicode.IsSpace(r)
	})
}

// CleanUID retrieves a UID from obj using tag and returns it with surrounding whitespace trimmed, or an empty string if the tag is missing.
func CleanUID(obj *object.Object, tag core.Tag) string {
	value, ok := obj.GetUID(tag)
	if !ok {
		return ""
	}
	return strings.TrimSpace(value)
}

// Int retrieves an integer value from a DICOM object by tag.
// If the tag is not found or an error occurs, it returns 0.
func Int(obj *object.Object, tag core.Tag) int {
	values := Ints(obj, tag)
	if len(values) == 0 {
		return 0
	}
	return int(values[0])
}

// Ints retrieves integer values from either IS strings or the standard binary
// integer VRs. It returns nil when the element is missing or malformed.
func Ints(obj *object.Object, tag core.Tag) []int64 {
	values, err := LookupInts(obj, tag)
	if err != nil {
		return nil
	}
	return values
}

// LookupInts retrieves integer values from IS strings or standard binary
// integer VRs and reports missing, malformed, or unsupported values.
func LookupInts(obj *object.Object, tag core.Tag) ([]int64, error) {
	if obj == nil {
		return nil, fmt.Errorf("dicom derived: nil object for %s", tag)
	}
	return obj.GetInts(tag)
}

// Floats retrieves values from DS strings or FL/FD binary elements. It returns
// nil if the tag is missing or cannot be parsed.
func Floats(obj *object.Object, tag core.Tag) []float64 {
	values, err := LookupFloats(obj, tag)
	if err != nil {
		return nil
	}
	return values
}

// LookupFloats retrieves values from DS strings or FL/FD binary elements and
// reports missing, malformed, or unsupported values.
func LookupFloats(obj *object.Object, tag core.Tag) ([]float64, error) {
	if obj == nil {
		return nil, fmt.Errorf("dicom derived: nil object for %s", tag)
	}
	return obj.GetFloats(tag)
}

// Sequence retrieves a sequence element from the object by tag. It returns nil if the element is missing, otherwise returns the sequence items.
func Sequence(obj *object.Object, tag core.Tag) []*object.Object {
	items, ok := obj.GetSequence(tag)
	if !ok {
		return nil
	}
	return items
}

// Uint16Bytes encodes the uint16 values into a byte slice using little-endian byte order.
func Uint16Bytes(values []uint16) []byte {
	out := make([]byte, len(values)*2)
	for i, value := range values {
		binary.LittleEndian.PutUint16(out[i*2:], value)
	}
	return out
}

// Uint16s converts a byte slice to uint16 values using little-endian byte order.
func Uint16s(data []byte) []uint16 {
	out := make([]uint16, len(data)/2)
	for i := range out {
		out[i] = binary.LittleEndian.Uint16(data[i*2:])
	}
	return out
}

// SamePosition reports whether a and b are within 0.001 of each other.
func SamePosition(a, b float64) bool {
	return math.Abs(a-b) <= 1e-3
}

// DerivedUID returns a deterministic UID under the 2.25 root (PS3.5 B.2)
// for an object derived from parts, such as a Dimension Organization UID of
// an instance: core.NameUID of the parts.
func DerivedUID(parts ...string) string {
	return core.NameUID(parts...)
}

var (
	TagSeriesNumber       = core.NewTag(0x0020, 0x0011)
	TagInstanceNumber     = core.NewTag(0x0020, 0x0013)
	TagContentLabel       = core.NewTag(0x0070, 0x0080)
	TagContentDescription = core.NewTag(0x0070, 0x0081)
	TagContentCreatorName = core.NewTag(0x0070, 0x0084)
)

// Identification holds the Series Number, Instance Number and Content
// Identification Macro (PS3.3 10.9) of a derived object.
type Identification struct {
	SeriesNumber       int
	InstanceNumber     int
	ContentLabel       string
	ContentDescription string
	ContentCreatorName string
}

// Elements encodes the identification: Series and Instance Number default
// to 1 and Content Label to defaultLabel; Content Description and Content
// Creator's Name are written empty when unset (Type 2).
func (id Identification) Elements(defaultLabel string) ([]core.Element, error) {
	label := id.ContentLabel
	if label == "" {
		label = defaultLabel
	}
	if !validCodeString(label) {
		return nil, fmt.Errorf("dicom derived: Content Label %q is not a CS value", label)
	}
	if id.SeriesNumber < 0 || id.InstanceNumber < 0 {
		return nil, fmt.Errorf("dicom derived: negative Series or Instance Number")
	}
	series, instance := max(id.SeriesNumber, 1), max(id.InstanceNumber, 1)
	return []core.Element{
		IS(TagSeriesNumber, series),
		IS(TagInstanceNumber, instance),
		CS(TagContentLabel, label),
		LO(TagContentDescription, id.ContentDescription),
		Str(TagContentCreatorName, core.VRPN, id.ContentCreatorName),
	}, nil
}

// ReadIdentification reads the fields Identification.Elements writes.
func ReadIdentification(obj *object.Object) Identification {
	return Identification{
		SeriesNumber:       Int(obj, TagSeriesNumber),
		InstanceNumber:     Int(obj, TagInstanceNumber),
		ContentLabel:       CleanString(obj, TagContentLabel),
		ContentDescription: CleanString(obj, TagContentDescription),
		ContentCreatorName: CleanString(obj, TagContentCreatorName),
	}
}

// validCodeString reports a single CS value: 1 to 16 upper-case letters,
// digits, spaces or underscores.
func validCodeString(value string) bool {
	if value == "" || len(value) > 16 || strings.TrimSpace(value) == "" {
		return false
	}
	for _, r := range value {
		if !(r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' || r == ' ' || r == '_') {
			return false
		}
	}
	return true
}
