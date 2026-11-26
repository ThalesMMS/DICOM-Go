package tractography

import (
	"errors"

	"github.com/ThalesMMS/dicom-go/codes"
	"github.com/ThalesMMS/dicom-go/object"
)

// Storage is the Tractography Results Storage SOP Class.
const Storage = "1.2.840.10008.5.1.4.1.1.66.6"

var (
	ErrInvalidOptions   = errors.New("dicom/tractography: invalid options")
	ErrInvalidData      = errors.New("dicom/tractography: invalid data")
	ErrInvalidReference = errors.New("dicom/tractography: invalid reference")
	ErrUnsupportedValue = errors.New("dicom/tractography: materialized value required")
	ErrLimitExceeded    = errors.New("dicom/tractography: limit exceeded")
)

// SourceOrigin states how the result was created, not a fact inferred from a
// missing reference sequence. Write requires DICOMImages or OtherSource.
type SourceOrigin uint8

const (
	UnknownOrigin SourceOrigin = iota
	DICOMImages
	OtherSource
)

// CodeKind chooses the wire code-value attribute; its zero value is SH.
type CodeKind uint8

const (
	ShortCode CodeKind = iota
	LongCode
	URNCode
)

// Code supports basic and enhanced Code Sequence Macro encoding. Equivalent
// entries may have Context but cannot themselves have Equivalent entries.
type Code struct {
	codes.Code
	Kind          CodeKind
	SchemeVersion string
	Context       *CodeContext
	Equivalent    []Code
}

// CodeContext holds the optional enhanced encoding attributes of PS3.3 8.8.
type CodeContext struct {
	Identifier, UID, MappingResource, MappingResourceUID, MappingResourceName string
	Version, ExtensionFlag, LocalVersion, ExtensionCreatorUID                 string
}

// FromCode makes a short, basic coded entry from the terminology registry.
func FromCode(c codes.Code) Code { return Code{Code: c} }

type Algorithm struct {
	Family                            Code
	NameCode                          *Code
	Name, Version, Parameters, Source string
}

type Track struct {
	// Points are xyz patient coordinates in mm; at least two are required.
	// IEEE NaNs/infinities and signed zeros are preserved, never computed.
	Points      [][3]float32
	Color       *[3]uint16
	PointColors [][3]uint16
}

type MeasurementValues struct {
	Values []float32
	// PointIndices are one-based. Nil means every point, in track order.
	// A present empty list is invalid; arbitrary valid index order is retained.
	PointIndices []uint32
}

type Measurement struct {
	Quantity, Units Code
	Tracks          []MeasurementValues
}

type TrackStatistic struct {
	Quantity, Modifier, Units Code
	Values                    []float32
}

type TrackSetStatistic struct {
	Quantity, Modifier, Units Code
	Value                     float64
}

type TrackSet struct {
	Number             uint32
	Label, Description string
	Anatomy            Code
	AnatomyModifiers   []Code
	Color              *[3]uint16
	LineThickness      *float32
	Tracks             []Track
	Measurements       []Measurement
	TrackStatistics    []TrackStatistic
	Statistics         []TrackSetStatistic
	Acquisition        *Code
	Model              Code
	Algorithms         []Algorithm
}

type SOPReference struct{ SOPClassUID, SOPInstanceUID string }

type ImageReference struct {
	SOPReference
	StudyInstanceUID, SeriesInstanceUID string
	Frames                              []int32
	Segments                            []uint16
}

// Results owns the buffers returned by Read. Metadata is an independent,
// bounded Object containing supported common/optional root IOD attributes.
// It cannot override the explicitly modeled root fields. Unknown/private
// extensions and root image bulk are outside this typed round-trip contract.
type Results struct {
	SOPInstanceUID, StudyInstanceUID, SeriesInstanceUID, FrameOfReferenceUID string
	SeriesNumber, InstanceNumber                                             int32
	ContentDate, ContentTime, Label, Description                             string
	Origin                                                                   SourceOrigin
	TrackSets                                                                []TrackSet
	References                                                               []ImageReference
	PerformedProcedureStep                                                   *SOPReference
	Metadata                                                                 *object.Object
}

type ReadOptions struct {
	Limits Limits
	// RequireDICOMReferences is for callers that independently know creation
	// used DICOM images. Otherwise absent references yield UnknownOrigin.
	RequireDICOMReferences bool
}

type WriteOptions struct {
	Limits Limits
	// Source is used only for common modules; image bulk is not copied.
	// Equipment overrides retained equipment when explicitly supplied.
	Common object.DerivedCommon
}
