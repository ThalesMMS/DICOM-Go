// Package parametricmap parses and lazily decodes DICOM Parametric Map
// instances. Quantitative values retain their Real World Value Mapping,
// quantity, and units and are never routed through ordinary image VOI.
package parametricmap

import (
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"math"
	"sort"
	"strconv"
	"strings"
	"sync"

	"github.com/ThalesMMS/dicom-go/core"
	"github.com/ThalesMMS/dicom-go/internal/derivedio"
	"github.com/ThalesMMS/dicom-go/object"
	"github.com/ThalesMMS/dicom-go/pixeldata"
	"github.com/ThalesMMS/dicom-go/render"
	"github.com/ThalesMMS/dicom-go/roi"
)

const ParametricMapStorage = "1.2.840.10008.5.1.4.1.1.30"

var (
	ErrUnsupportedSOPClass   = errors.New("dicom/parametricmap: unsupported SOP class")
	ErrInvalidObject         = errors.New("dicom/parametricmap: invalid object")
	ErrMissingMapping        = errors.New("dicom/parametricmap: missing real world value mapping")
	ErrAmbiguousMapping      = errors.New("dicom/parametricmap: ambiguous real world value mapping")
	ErrOutsideMapping        = errors.New("dicom/parametricmap: outside real world value mapping domain")
	ErrUnsupportedDimensions = errors.New("dicom/parametricmap: unsupported dimensions")
	ErrGeometryMismatch      = errors.New("dicom/parametricmap: geometry mismatch")
	ErrNonFinite             = errors.New("dicom/parametricmap: non-finite quantitative value")
	ErrMemoryLimit           = errors.New("dicom/parametricmap: memory limit exceeded")
	ErrPayloadUnavailable    = errors.New("dicom/parametricmap: pixel payload was not retained")
	// ErrInvalidPadding reports inconsistent Pixel Padding attributes for the
	// active payload, such as a missing required endpoint, wrong VR or value
	// count, or a value outside the stored sample range.
	ErrInvalidPadding = errors.New("dicom/parametricmap: invalid pixel padding")
)

var errMappingOverflow = fmt.Errorf("%w: mapping overflow", ErrNonFinite)

var (
	tagFloatPixelData           = core.NewTag(0x7FE0, 0x0008)
	tagDoubleFloatPixelData     = core.NewTag(0x7FE0, 0x0009)
	tagSharedFunctionalGroups   = core.NewTag(0x5200, 0x9229)
	tagPerFrameFunctionalGroups = core.NewTag(0x5200, 0x9230)
	tagRealWorldValueMappingSeq = core.NewTag(0x0040, 0x9096)
	tagRWVFirstMapped           = core.NewTag(0x0040, 0x9216)
	tagRWVLastMapped            = core.NewTag(0x0040, 0x9211)
	tagDoubleRWVFirstMapped     = core.NewTag(0x0040, 0x9214)
	tagDoubleRWVLastMapped      = core.NewTag(0x0040, 0x9213)
	tagRWVLUTData               = core.NewTag(0x0040, 0x9212)
	tagRWVIntercept             = core.NewTag(0x0040, 0x9224)
	tagRWVSlope                 = core.NewTag(0x0040, 0x9225)
	tagMeasurementUnitsCodeSeq  = core.NewTag(0x0040, 0x08EA)
	tagQuantityDefinitionSeq    = core.NewTag(0x0040, 0x9220)
	tagConceptNameCodeSeq       = core.NewTag(0x0040, 0xA043)
	tagConceptCodeSeq           = core.NewTag(0x0040, 0xA168)
	tagCodeValue                = core.NewTag(0x0008, 0x0100)
	tagCodingSchemeDesignator   = core.NewTag(0x0008, 0x0102)
	tagCodingSchemeVersion      = core.NewTag(0x0008, 0x0103)
	tagCodeMeaning              = core.NewTag(0x0008, 0x0104)
	tagDimensionIndexSeq        = core.NewTag(0x0020, 0x9222)
	tagDimensionIndexPointer    = core.NewTag(0x0020, 0x9165)
	tagDimensionIndexValues     = core.NewTag(0x0020, 0x9157)
	tagFrameContentSeq          = core.NewTag(0x0020, 0x9111)
	tagInStackPositionNumber    = core.NewTag(0x0020, 0x9057)
	tagImagePositionPatient     = core.NewTag(0x0020, 0x0032)
	tagReferencedSOPClassUID    = core.NewTag(0x0008, 0x1150)
	tagReferencedSOPInstanceUID = core.NewTag(0x0008, 0x1155)

	// General Equipment and Image Pixel Modules (PS3.3 C.7.5.1 and C.7.6.3):
	// US or SS determined by Pixel Representation.
	tagIntegerPixelPaddingValue      = core.NewTag(0x0028, 0x0120)
	tagIntegerPixelPaddingRangeLimit = core.NewTag(0x0028, 0x0121)
	// Floating Point Image Pixel Module (PS3.3 C.7.6.24): FL pair.
	tagFloatPixelPaddingValue      = core.NewTag(0x0028, 0x0122)
	tagFloatPixelPaddingRangeLimit = core.NewTag(0x0028, 0x0124)
	// Double Floating Point Image Pixel Module (PS3.3 C.7.6.25): FD pair.
	tagDoubleFloatPixelPaddingValue      = core.NewTag(0x0028, 0x0123)
	tagDoubleFloatPixelPaddingRangeLimit = core.NewTag(0x0028, 0x0125)
)

type PayloadKind string

const (
	PayloadFloat32 PayloadKind = "float32"
	PayloadFloat64 PayloadKind = "float64"
	PayloadInteger PayloadKind = "integer"
)

// Code is a coded DICOM concept.
type Code struct {
	Value   string
	Scheme  string
	Meaning string
}

func (c Code) String() string {
	if strings.TrimSpace(c.Meaning) != "" {
		return strings.TrimSpace(c.Meaning)
	}
	if c.Value == "" {
		return ""
	}
	if c.Scheme == "" {
		return c.Value
	}
	return c.Scheme + ":" + c.Value
}

type Reference struct {
	SOPClassUID    string
	SOPInstanceUID string
	// Frames are optional one-based Referenced Frame Numbers. Empty means the
	// whole source instance, not implicitly its first frame.
	Frames []int
}

// Mapping converts a stored sample to its calibrated real-world value.
type Mapping struct {
	FirstValue float64
	LastValue  float64
	// HasRange makes FirstValue and LastValue inclusive stored-value bounds.
	// Parsed mappings with an explicit range set it, including [0, 0].
	HasRange     bool
	Label        string
	Slope        float64
	Intercept    float64
	LUT          []float64
	Units        Code
	Quantity     Code
	unitsVersion string
}

// Apply maps one stored sample to its real-world value.
//
// Float Pixel Data and Double Float Pixel Data are IEEE 754 streams (PS3.5
// 6.2, VR OF/OD), so a stored sample may be NaN or ±Inf; producers use them
// for voxels without a measurement (for example outside a fitting mask). A
// stored NaN maps to NaN and a stored ±Inf maps through the linear transform;
// neither is an error. A LUT mapping has no entry for ±Inf and reports
// ErrMissingMapping. A finite stored value whose mapping is non-finite (an
// overflowing transform or a non-finite LUT entry) is invalid and reports
// ErrNonFinite. A finite sample outside an explicit range reports
// ErrOutsideMapping.
func (m Mapping) Apply(stored float64) (float64, error) {
	if math.IsNaN(stored) {
		return math.NaN(), nil
	}
	if m.HasRange && finite(stored) && (stored < m.FirstValue || stored > m.LastValue) {
		return 0, ErrOutsideMapping
	}
	var value float64
	if len(m.LUT) > 0 {
		// Canonical nearest entry (#1115): math.Round sent an offset of
		// exactly -0.5 to -1 and failed a value the range accepts.
		index, ok := render.NearestIndexInside(stored-m.FirstValue, len(m.LUT))
		if !ok {
			return 0, fmt.Errorf("%w: stored value %.8g outside mapping [%.8g, %.8g]", ErrMissingMapping, stored, m.FirstValue, m.LastValue)
		}
		value = m.LUT[index]
	} else {
		value = stored*m.Slope + m.Intercept
		if math.IsInf(stored, 0) {
			return value, nil
		}
	}
	if !finite(value) {
		if len(m.LUT) == 0 && finite(m.Slope) && finite(m.Intercept) {
			return 0, fmt.Errorf("%w: mapped value %.8g", errMappingOverflow, value)
		}
		return 0, fmt.Errorf("%w: mapped value %.8g", ErrNonFinite, value)
	}
	return value, nil
}

// Padding is the closed stored-value range marked by the payload's Pixel
// Padding Value and Pixel Padding Range Limit (PS3.3 C.7.5.1.1.2, C.7.6.24
// and C.7.6.25). A single integer padding value uses equal endpoints.
// A NaN pair designates stored NaN samples.
type Padding struct {
	Value      float64
	RangeLimit float64
}

// Contains reports whether a stored sample is padding.
func (p Padding) Contains(stored float64) bool {
	if math.IsNaN(p.Value) {
		return math.IsNaN(stored)
	}
	return stored >= min(p.Value, p.RangeLimit) && stored <= max(p.Value, p.RangeLimit)
}

// VoxelCounts classifies the voxels of decoded frames. Only Valid voxels carry
// a quantitative value. Padding voxels decode to NaN, stored NaN voxels stay
// NaN, and stored ±Inf voxels decode to the linearly mapped infinity; none of
// them contributes to MaskStatistics, Resample or SamplePatient. Finite
// samples outside all applicable mapping domains and samples whose calibrated
// values overflow are counted separately and also decode to NaN.
type VoxelCounts struct {
	Valid           int
	Padding         int
	NaN             int
	Infinite        int
	OutsideMapping  int
	MappingOverflow int
}

// Unmeasured returns the number of voxels without a quantitative value.
func (c VoxelCounts) Unmeasured() int {
	return c.Padding + c.NaN + c.Infinite + c.OutsideMapping + c.MappingOverflow
}

// Add accumulates other into c.
func (c *VoxelCounts) Add(other VoxelCounts) {
	c.Valid += other.Valid
	c.Padding += other.Padding
	c.NaN += other.NaN
	c.Infinite += other.Infinite
	c.OutsideMapping += other.OutsideMapping
	c.MappingOverflow += other.MappingOverflow
}

// Measured reports whether a decoded calibrated value is a quantitative
// measurement rather than padding, NaN or ±Inf.
func Measured(value float64) bool { return finite(value) }

type Frame struct {
	Geometry             render.SliceGeometry
	DimensionIndexValues []int
	Mapping              Mapping
	// AdditionalMappings, when present, follow Mapping in ascending order
	// of the disjoint stored-value intervals for this scalar frame.
	AdditionalMappings []Mapping
}

// Map retains encoded frames and decodes calibrated values only when requested.
// The small LRU cache is bounded and can be evicted by an application memory
// budget through SetCacheLimitBytes/EvictBytes.
type Map struct {
	SOPClassUID         string
	SOPInstanceUID      string
	StudyInstanceUID    string
	SeriesInstanceUID   string
	FrameOfReferenceUID string
	Rows                int
	Columns             int
	NumberOfFrames      int
	Payload             PayloadKind
	Frames              []Frame
	References          []Reference
	Units               Code
	Quantity            Code
	// Padding is the stored-value padding range of the active pixel payload,
	// or nil when the object declares none. Padding voxels decode to NaN;
	// DecodeFrameInto can report them as an explicit mask.
	Padding *Padding

	order           binary.ByteOrder
	floatRaw        []byte
	native          *pixeldata.NativeFrames
	cacheMu         sync.Mutex
	cache           map[int][]float64
	cacheOrder      []int
	cacheBytes      int64
	cacheLimit      int64
	payloadRetained bool
}

// ReadOptions selects the real-world representation of a scalar map.
type ReadOptions struct {
	// MappingLabel selects exactly one LUT Label (0040,9210) per frame,
	// with case-sensitive matching. An empty label admits compatible,
	// disjoint domains automatically. The selected units and quantity must
	// still agree across all frames.
	MappingLabel string
}

// Read parses metadata and encoded frame boundaries without decoding the quantitative payload.
func Read(file *object.File) (*Map, error) {
	return ReadWithOptions(file, ReadOptions{})
}

// ReadWithOptions permits explicit selection of a mapping alternative.
func ReadWithOptions(file *object.File, options ReadOptions) (*Map, error) {
	if file == nil || file.Dataset == nil {
		return nil, fmt.Errorf("%w: dataset is nil", ErrInvalidObject)
	}
	return ReadDatasetWithOptions(file.Dataset, options)
}

func ReadDataset(obj *object.Object) (*Map, error) {
	return ReadDatasetWithOptions(obj, ReadOptions{})
}

// ReadDatasetWithOptions interprets the dataset using the selected mapping policy.
func ReadDatasetWithOptions(obj *object.Object, options ReadOptions) (*Map, error) {
	return readDataset(obj, true, options)
}

// ReadMetadata parses geometry, dimensions, mappings, units, and encoded
// payload boundaries without requiring the Pixel Data value to be retained.
// FrameValues returns ErrPayloadUnavailable on the resulting Map. This lets
// two-pass consumers plan large exports with DeferPixelData enabled.
func ReadMetadata(file *object.File) (*Map, error) {
	return ReadMetadataWithOptions(file, ReadOptions{})
}

// ReadMetadataWithOptions uses the same mapping selection as ReadWithOptions
// and leaves the source payload untouched.
func ReadMetadataWithOptions(file *object.File, options ReadOptions) (*Map, error) {
	if file == nil || file.Dataset == nil {
		return nil, fmt.Errorf("%w: dataset is nil", ErrInvalidObject)
	}
	return readDataset(file.Dataset, false, options)
}

func readDataset(obj *object.Object, retainPayload bool, options ReadOptions) (*Map, error) {
	if obj == nil {
		return nil, fmt.Errorf("%w: dataset is nil", ErrInvalidObject)
	}
	if sop := derivedio.CleanUID(obj, derivedio.TagSOPClassUID); sop != ParametricMapStorage {
		return nil, fmt.Errorf("%w: %s", ErrUnsupportedSOPClass, sop)
	}
	rows, columns := derivedio.Int(obj, derivedio.TagRows), derivedio.Int(obj, derivedio.TagColumns)
	frames := derivedio.Int(obj, derivedio.TagNumberOfFrames)
	if frames == 0 {
		frames = 1
	}
	if rows <= 0 || columns <= 0 || frames <= 0 {
		return nil, fmt.Errorf("%w: invalid dimensions %dx%dx%d", ErrInvalidObject, columns, rows, frames)
	}
	voxels, ok := checkedProduct(rows, columns, frames)
	if !ok || voxels > 64*1024*1024 {
		return nil, fmt.Errorf("%w: %dx%dx%d samples", ErrMemoryLimit, columns, rows, frames)
	}
	out := &Map{
		SOPClassUID:         ParametricMapStorage,
		SOPInstanceUID:      derivedio.CleanUID(obj, derivedio.TagSOPInstanceUID),
		StudyInstanceUID:    derivedio.CleanUID(obj, derivedio.TagStudyInstanceUID),
		SeriesInstanceUID:   derivedio.CleanUID(obj, derivedio.TagSeriesInstanceUID),
		FrameOfReferenceUID: derivedio.CleanUID(obj, derivedio.TagFrameOfReferenceUID),
		Rows:                rows,
		Columns:             columns,
		NumberOfFrames:      frames,
		order:               obj.ValueByteOrder(),
		cache:               make(map[int][]float64),
		cacheLimit:          32 << 20,
		payloadRetained:     retainPayload,
	}
	if out.SOPInstanceUID == "" || out.StudyInstanceUID == "" || out.SeriesInstanceUID == "" || out.FrameOfReferenceUID == "" {
		return nil, fmt.Errorf("%w: missing identity or FrameOfReferenceUID", ErrInvalidObject)
	}

	if element, found := obj.Get(tagFloatPixelData); found {
		length := int(element.Length())
		if length != voxels*4 {
			return nil, fmt.Errorf("%w: Float Pixel Data length %d, want %d", ErrInvalidObject, length, voxels*4)
		}
		out.Payload = PayloadFloat32
		if retainPayload {
			var err error
			out.floatRaw, err = readPixelPayload(obj, element, length)
			if err != nil {
				return nil, err
			}
		}
	} else if element, found := obj.Get(tagDoubleFloatPixelData); found {
		length := int(element.Length())
		if length != voxels*8 {
			return nil, fmt.Errorf("%w: Double Float Pixel Data length %d, want %d", ErrInvalidObject, length, voxels*8)
		}
		out.Payload = PayloadFloat64
		if retainPayload {
			var err error
			out.floatRaw, err = readPixelPayload(obj, element, length)
			if err != nil {
				return nil, err
			}
		}
	} else {
		metadata, err := pixeldata.ExtractMetadata(obj)
		if err != nil {
			return nil, fmt.Errorf("%w: integer Pixel Data metadata: %v", ErrInvalidObject, err)
		}
		if metadata.SamplesPerPixel != 1 || metadata.PixelRepresentation > 1 ||
			(metadata.BitsAllocated != 8 && metadata.BitsAllocated != 16 && metadata.BitsAllocated != 32) {
			return nil, fmt.Errorf("%w: unsupported integer pixel metadata %+v", ErrInvalidObject, metadata)
		}
		if metadata.BitsStored == 0 || metadata.BitsStored > metadata.BitsAllocated ||
			metadata.HighBit >= metadata.BitsAllocated ||
			int(metadata.HighBit)+1 < int(metadata.BitsStored) {
			return nil, fmt.Errorf("%w: unsupported integer bit layout %+v", ErrInvalidObject, metadata)
		}
		element, found := obj.Get(core.TagPixelData)
		if !found {
			return nil, fmt.Errorf("%w: integer Pixel Data is missing", ErrInvalidObject)
		}
		frameBytes := rows * columns * int(metadata.BitsAllocated/8)
		wantBytes := frameBytes * frames
		length := int(element.Length())
		if length != wantBytes && !(wantBytes%2 == 1 && length == wantBytes+1) {
			return nil, fmt.Errorf("%w: integer Pixel Data length %d, want %d", ErrInvalidObject, length, wantBytes)
		}
		out.Payload = PayloadInteger
		out.Padding, err = readIntegerPadding(obj, metadata.SampleFormat(out.order))
		if err != nil {
			return nil, err
		}
		if retainPayload {
			native, nativeErr := readNativeFrames(obj, element, length)
			if nativeErr != nil {
				return nil, fmt.Errorf("%w: integer Pixel Data: %w", ErrInvalidObject, nativeErr)
			}
			if len(native.Data) != frames {
				return nil, fmt.Errorf("%w: %d encoded frames for %d frames", ErrInvalidObject, len(native.Data), frames)
			}
			for index, frame := range native.Data {
				if len(frame) != frameBytes {
					return nil, fmt.Errorf("%w: frame %d has %d bytes, want %d", ErrInvalidObject, index+1, len(frame), frameBytes)
				}
			}
			out.native = native
		}
	}

	if out.Payload != PayloadInteger {
		padding, err := readFloatPadding(obj, out.Payload)
		if err != nil {
			return nil, err
		}
		out.Padding = padding
	}

	perFrame := sequence(obj, tagPerFrameFunctionalGroups)
	if len(perFrame) != 0 && len(perFrame) != frames {
		return nil, fmt.Errorf("%w: %d per-frame groups for %d frames", ErrInvalidObject, len(perFrame), frames)
	}
	out.Frames = make([]Frame, frames)
	if err := readFrameMappings(obj, perFrame, out, options); err != nil {
		return nil, err
	}
	for index := 0; index < frames; index++ {
		geometry, err := frameGeometry(obj.FrameGeometryAt(index), rows, columns)
		if err != nil {
			return nil, fmt.Errorf("frame %d: %w", index+1, err)
		}
		out.Frames[index].Geometry = geometry
	}
	if err := readDimensions(obj, perFrame, out.Frames); err != nil {
		return nil, err
	}
	for index := 1; index < len(out.Frames); index++ {
		if out.Frames[index].Geometry.RowDir.Dot(out.Frames[0].Geometry.RowDir) <
			render.DefaultGeometryTolerances().OrientationCos ||
			out.Frames[index].Geometry.ColDir.Dot(out.Frames[0].Geometry.ColDir) <
				render.DefaultGeometryTolerances().OrientationCos {
			return nil, fmt.Errorf("%w: frame orientation varies", ErrUnsupportedDimensions)
		}
	}
	references, err := readReferences(obj)
	if err != nil {
		return nil, err
	}
	out.References = references
	return out, nil
}

func readIntegerPadding(obj *object.Object, format pixeldata.SampleFormat) (*Padding, error) {
	padding, err := pixeldata.PixelPaddingFromObject(obj, format)
	if err != nil {
		return nil, fmt.Errorf("%w: %w", ErrInvalidPadding, err)
	}
	if padding == nil {
		if _, hasLimit := obj.Get(tagIntegerPixelPaddingRangeLimit); hasLimit {
			return nil, fmt.Errorf("%w: %s present without %s", ErrInvalidPadding, tagIntegerPixelPaddingRangeLimit, tagIntegerPixelPaddingValue)
		}
		return nil, nil
	}
	limit := padding.Value
	if padding.HasRange {
		limit = padding.RangeLimit
	}
	return &Padding{Value: float64(padding.Value), RangeLimit: float64(limit)}, nil
}

// readFloatPadding reads the padding pair of the floating point pixel module that
// matches payload: the FL pair of the Floating Point Image Pixel Module for
// Float Pixel Data and the FD pair of the Double Floating Point Image Pixel
// Module for Double Float Pixel Data. Range Limit is Type 1C (required when
// the value is present), so a half-specified pair returns ErrInvalidPadding.
func readFloatPadding(obj *object.Object, payload PayloadKind) (*Padding, error) {
	var valueTag, limitTag core.Tag
	var vr core.VR
	switch payload {
	case PayloadFloat32:
		valueTag, limitTag, vr = tagFloatPixelPaddingValue, tagFloatPixelPaddingRangeLimit, core.VRFL
	case PayloadFloat64:
		valueTag, limitTag, vr = tagDoubleFloatPixelPaddingValue, tagDoubleFloatPixelPaddingRangeLimit, core.VRFD
	default:
		return nil, nil
	}
	value, valueOK, err := paddingFloat(obj, valueTag, vr)
	if err != nil {
		return nil, err
	}
	limit, limitOK, err := paddingFloat(obj, limitTag, vr)
	if err != nil {
		return nil, err
	}
	switch {
	case !valueOK && !limitOK:
		return nil, nil
	case !valueOK:
		return nil, fmt.Errorf("%w: %s present without %s", ErrInvalidPadding, limitTag, valueTag)
	case !limitOK:
		return nil, fmt.Errorf("%w: %s present without the Type 1C %s", ErrInvalidPadding, valueTag, limitTag)
	case math.IsNaN(value) != math.IsNaN(limit):
		return nil, fmt.Errorf("%w: NaN %s paired with a number", ErrInvalidPadding, valueTag)
	}
	return &Padding{Value: value, RangeLimit: limit}, nil
}

func paddingFloat(obj *object.Object, tag core.Tag, vr core.VR) (float64, bool, error) {
	element, found := obj.Get(tag)
	if !found {
		return 0, false, nil
	}
	if element.VR() != vr {
		return 0, false, fmt.Errorf("%w: %s has VR %s, want %s", ErrInvalidPadding, tag, element.VR(), vr)
	}
	values, err := derivedio.LookupFloats(obj, tag)
	if err != nil || len(values) != 1 {
		return 0, false, fmt.Errorf("%w: %s must hold one value", ErrInvalidPadding, tag)
	}
	return values[0], true, nil
}

func frameGeometry(g object.FrameGeometry, rows, columns int) (render.SliceGeometry, error) {
	if len(g.ImagePositionPatient) != 3 || len(g.ImageOrientationPatient) != 6 || len(g.PixelSpacing) != 2 ||
		!positiveFinite(g.PixelSpacing[0]) || !positiveFinite(g.PixelSpacing[1]) {
		return render.SliceGeometry{}, fmt.Errorf("%w: incomplete patient geometry", ErrGeometryMismatch)
	}
	rowRaw := render.Vec3{X: g.ImageOrientationPatient[0], Y: g.ImageOrientationPatient[1], Z: g.ImageOrientationPatient[2]}
	colRaw := render.Vec3{X: g.ImageOrientationPatient[3], Y: g.ImageOrientationPatient[4], Z: g.ImageOrientationPatient[5]}
	origin := render.Vec3{X: g.ImagePositionPatient[0], Y: g.ImagePositionPatient[1], Z: g.ImagePositionPatient[2]}
	if !finiteVec(origin) || !finiteVec(rowRaw) || !finiteVec(colRaw) ||
		math.Abs(rowRaw.Length()-1) > 1e-4 || math.Abs(colRaw.Length()-1) > 1e-4 || math.Abs(rowRaw.Dot(colRaw)) > 1e-4 {
		return render.SliceGeometry{}, fmt.Errorf("%w: invalid orientation", ErrGeometryMismatch)
	}
	normal := rowRaw.Cross(colRaw).Normalize()
	if normal == (render.Vec3{}) {
		return render.SliceGeometry{}, fmt.Errorf("%w: degenerate orientation", ErrGeometryMismatch)
	}
	return render.SliceGeometry{
		Origin: origin,
		RowDir: rowRaw, ColDir: colRaw, Normal: normal,
		RowSpacing: g.PixelSpacing[0], ColSpacing: g.PixelSpacing[1],
		Rows: rows, Columns: columns,
	}, nil
}

// FrameValues returns a defensive copy of calibrated values for one frame.
func (m *Map) FrameValues(index int) ([]float64, error) {
	if m == nil || index < 0 || index >= m.NumberOfFrames {
		return nil, fmt.Errorf("%w: frame %d", ErrInvalidObject, index)
	}
	if !m.payloadRetained {
		return nil, ErrPayloadUnavailable
	}
	m.cacheMu.Lock()
	if values, ok := m.cache[index]; ok {
		m.touchLocked(index)
		out := append([]float64(nil), values...)
		m.cacheMu.Unlock()
		return out, nil
	}
	m.cacheMu.Unlock()
	values, err := m.decodeFrame(index)
	if err != nil {
		return nil, err
	}
	m.cacheMu.Lock()
	if existing, ok := m.cache[index]; ok {
		values = existing
	} else {
		m.cache[index] = values
		m.cacheOrder = append(m.cacheOrder, index)
		m.cacheBytes += int64(len(values) * 8)
		m.trimLocked()
	}
	out := append([]float64(nil), values...)
	m.cacheMu.Unlock()
	return out, nil
}

// FrameValuesInto decodes one calibrated frame into destination without
// populating the Map cache or allocating a defensive copy. It is intended for
// bounded streaming consumers that own their per-frame buffer. Padding,
// stored NaN, out-of-domain voxels and mapping overflows decode to NaN.
// Stored ±Inf voxels decode to ±Inf. Use DecodeFrameInto for their counts
// and an explicit padding mask.
func (m *Map) FrameValuesInto(index int, destination []float64) error {
	_, err := m.DecodeFrameInto(index, destination, nil)
	return err
}

// DecodeFrameInto decodes one calibrated frame into values and classifies
// every voxel. When padding is non-nil it must hold Rows*Columns entries and
// receives true exactly for pixel padding voxels, which lets a consumer
// distinguish padding from stored NaN although both decode to NaN.
func (m *Map) DecodeFrameInto(index int, values []float64, padding []bool) (VoxelCounts, error) {
	if m == nil || index < 0 || index >= m.NumberOfFrames {
		return VoxelCounts{}, fmt.Errorf("%w: frame %d", ErrInvalidObject, index)
	}
	if !m.payloadRetained {
		return VoxelCounts{}, ErrPayloadUnavailable
	}
	count := m.Rows * m.Columns
	if len(values) < count {
		return VoxelCounts{}, fmt.Errorf("%w: destination has %d values, want %d", ErrInvalidObject, len(values), count)
	}
	if padding != nil && len(padding) < count {
		return VoxelCounts{}, fmt.Errorf("%w: padding mask has %d entries, want %d", ErrInvalidObject, len(padding), count)
	}
	values = values[:count]
	frame := m.Frames[index]
	var counts VoxelCounts
	for pixel := 0; pixel < count; pixel++ {
		stored, err := m.storedValue(index, pixel)
		if err != nil {
			return VoxelCounts{}, err
		}
		isPadding := m.Padding != nil && m.Padding.Contains(stored)
		if padding != nil {
			padding[pixel] = isPadding
		}
		switch {
		case isPadding:
			values[pixel] = math.NaN()
			counts.Padding++
			continue
		case math.IsNaN(stored):
			values[pixel] = math.NaN()
			counts.NaN++
			continue
		}
		value, err := frame.applyMapping(stored)
		if err != nil {
			switch {
			case errors.Is(err, ErrOutsideMapping):
				counts.OutsideMapping++
			case errors.Is(err, errMappingOverflow):
				counts.MappingOverflow++
			default:
				return VoxelCounts{}, fmt.Errorf("frame %d pixel %d: %w", index+1, pixel, err)
			}
			values[pixel] = math.NaN()
			continue
		}
		values[pixel] = value
		if math.IsInf(stored, 0) {
			counts.Infinite++
		} else {
			counts.Valid++
		}
	}
	return counts, nil
}

func (m *Map) decodeFrame(index int) ([]float64, error) {
	count := m.Rows * m.Columns
	out := make([]float64, count)
	if err := m.FrameValuesInto(index, out); err != nil {
		return nil, err
	}
	return out, nil
}

func (m *Map) storedValue(frame, pixel int) (float64, error) {
	count := m.Rows * m.Columns
	switch m.Payload {
	case PayloadFloat32:
		offset := (frame*count + pixel) * 4
		return float64(math.Float32frombits(m.order.Uint32(m.floatRaw[offset:]))), nil
	case PayloadFloat64:
		offset := (frame*count + pixel) * 8
		return math.Float64frombits(m.order.Uint64(m.floatRaw[offset:])), nil
	case PayloadInteger:
		meta := m.native.Metadata
		width := int(meta.BitsAllocated / 8)
		raw := m.native.Data[frame][pixel*width:]
		var unsigned uint64
		switch width {
		case 1:
			unsigned = uint64(raw[0])
		case 2:
			unsigned = uint64(m.order.Uint16(raw))
		case 4:
			unsigned = uint64(m.order.Uint32(raw))
		default:
			return 0, fmt.Errorf("%w: integer width %d", ErrInvalidObject, width)
		}
		shift := meta.HighBit + 1 - meta.BitsStored
		unsigned >>= shift
		mask := uint64(1)<<meta.BitsStored - 1
		unsigned &= mask
		if meta.PixelRepresentation == 1 && unsigned&(uint64(1)<<(meta.BitsStored-1)) != 0 {
			return float64(int64(unsigned | ^mask)), nil
		}
		return float64(unsigned), nil
	default:
		return 0, ErrInvalidObject
	}
}

// SamplePatient returns a calibrated bilinear sample from the nearest matching
// frame. It never extrapolates beyond a frame.
func (m *Map) SamplePatient(point render.Vec3) (float64, bool, error) {
	if m == nil || !finiteVec(point) {
		return 0, false, ErrInvalidObject
	}
	best, distance := -1, math.Inf(1)
	for index, frame := range m.Frames {
		d := math.Abs(point.Sub(frame.Geometry.Origin).Dot(frame.Geometry.Normal))
		if d < distance {
			best, distance = index, d
		}
	}
	tolerance := 0.5
	if len(m.Frames) > 1 {
		positions := make([]float64, len(m.Frames))
		for index, frame := range m.Frames {
			positions[index] = frame.Geometry.PositionAlong(frame.Geometry.Normal)
		}
		sort.Float64s(positions)
		minSpacing := math.Inf(1)
		for index := 1; index < len(positions); index++ {
			if spacing := positions[index] - positions[index-1]; spacing > 0 && spacing < minSpacing {
				minSpacing = spacing
			}
		}
		if finite(minSpacing) {
			tolerance = minSpacing/2 + 1e-5
		}
	}
	if best < 0 || distance > tolerance {
		return 0, false, nil
	}
	g := m.Frames[best].Geometry
	delta := point.Sub(g.Origin)
	// Pixel boxes (#1115): the outer half pixel repeats the edge pixels.
	column, okColumn := render.ClampContinuousIndex(delta.Dot(g.RowDir)/g.ColSpacing, m.Columns, 0)
	row, okRow := render.ClampContinuousIndex(delta.Dot(g.ColDir)/g.RowSpacing, m.Rows, 0)
	if !okColumn || !okRow {
		return 0, false, nil
	}
	values, err := m.FrameValues(best)
	if err != nil {
		return 0, false, err
	}
	value, ok := bilinear(values, m.Columns, m.Rows, column, row)
	if !ok {
		return 0, false, nil
	}
	return value, true, nil
}

// bilinear interpolates values at fractional pixel (x, y) inside the frame.
// It returns false when a neighbour with non-zero weight carries no
// quantitative value (padding, NaN or ±Inf), so unmeasured voxels never bleed
// into measured ones; a neighbour with zero weight is ignored.
func bilinear(values []float64, columns, rows int, x, y float64) (float64, bool) {
	x0, y0 := int(math.Floor(x)), int(math.Floor(y))
	x1, y1 := min(x0+1, columns-1), min(y0+1, rows-1)
	fx, fy := x-float64(x0), y-float64(y0)
	if fx == 0 {
		x1 = x0
	}
	if fy == 0 {
		y1 = y0
	}
	a, b := values[y0*columns+x0], values[y0*columns+x1]
	c, d := values[y1*columns+x0], values[y1*columns+x1]
	if !finite(a) || !finite(b) || !finite(c) || !finite(d) {
		return 0, false
	}
	top := interpolateFinite(a, b, fx)
	bottom := interpolateFinite(c, d, fx)
	return interpolateFinite(top, bottom, fy), true
}

// Opposite-sign finite values can have an infinite difference. Their weighted
// sum stays finite because each term has a weight in [0, 1].
func interpolateFinite(a, b, weight float64) float64 {
	delta := b - a
	if math.IsInf(delta, 0) {
		return (1-weight)*a + weight*b
	}
	return a + delta*weight
}

type Statistics struct {
	Count  int
	Min    float64
	Max    float64
	Mean   float64
	StdDev float64
	// Unmeasured counts mask pixels excluded because the map has no
	// quantitative value there (padding, NaN or ±Inf).
	Unmeasured int
}

// Resample aligns the nearest compatible map frame to a target slice geometry
// and returns calibrated values plus a validity mask. A target pixel is valid
// only when it lies inside the frame and every interpolation neighbour with
// non-zero weight is measured; padding, NaN and ±Inf voxels are invalid and
// therefore transparent to overlays and excluded from statistics.
// Unsupported orientation or non-overlapping planes fail explicitly.
func (m *Map) Resample(target render.SliceGeometry) ([]float64, []bool, error) {
	if m == nil || target.Rows <= 0 || target.Columns <= 0 ||
		!finiteVec(target.Origin) || !finiteVec(target.RowDir) || !finiteVec(target.ColDir) ||
		!positiveFinite(target.RowSpacing) || !positiveFinite(target.ColSpacing) ||
		math.Abs(target.RowDir.Length()-1) > 1e-4 || math.Abs(target.ColDir.Length()-1) > 1e-4 ||
		math.Abs(target.RowDir.Dot(target.ColDir)) > 1e-4 {
		return nil, nil, fmt.Errorf("%w: invalid target geometry", ErrGeometryMismatch)
	}
	best, distance := -1, math.Inf(1)
	for index, frame := range m.Frames {
		source := frame.Geometry
		if math.Abs(source.RowDir.Dot(target.RowDir)) < render.DefaultGeometryTolerances().OrientationCos ||
			math.Abs(source.ColDir.Dot(target.ColDir)) < render.DefaultGeometryTolerances().OrientationCos {
			continue
		}
		d := math.Abs(target.Origin.Sub(source.Origin).Dot(source.Normal))
		if d < distance {
			best, distance = index, d
		}
	}
	tolerance := 0.5
	if len(m.Frames) > 1 {
		positions := make([]float64, len(m.Frames))
		for index, frame := range m.Frames {
			positions[index] = frame.Geometry.PositionAlong(frame.Geometry.Normal)
		}
		sort.Float64s(positions)
		for index := 1; index < len(positions); index++ {
			if spacing := positions[index] - positions[index-1]; spacing > 0 && (tolerance == 0.5 || spacing/2 < tolerance) {
				tolerance = spacing/2 + 1e-5
			}
		}
	}
	if best < 0 || distance > tolerance {
		return nil, nil, fmt.Errorf("%w: target plane does not overlap the map", ErrGeometryMismatch)
	}
	source := m.Frames[best].Geometry
	sourceValues, err := m.FrameValues(best)
	if err != nil {
		return nil, nil, err
	}
	count, ok := checkedProduct(target.Rows, target.Columns)
	if !ok || count > 64*1024*1024 {
		return nil, nil, ErrMemoryLimit
	}
	values := make([]float64, count)
	valid := make([]bool, count)
	for row := 0; row < target.Rows; row++ {
		for column := 0; column < target.Columns; column++ {
			point := target.Origin.
				Add(target.RowDir.Scale(float64(column) * target.ColSpacing)).
				Add(target.ColDir.Scale(float64(row) * target.RowSpacing))
			delta := point.Sub(source.Origin)
			x, okX := render.ClampContinuousIndex(delta.Dot(source.RowDir)/source.ColSpacing, m.Columns, 0)
			y, okY := render.ClampContinuousIndex(delta.Dot(source.ColDir)/source.RowSpacing, m.Rows, 0)
			if !okX || !okY {
				continue
			}
			value, ok := bilinear(sourceValues, m.Columns, m.Rows, x, y)
			if !ok {
				continue
			}
			index := row*target.Columns + column
			values[index] = value
			valid[index] = true
		}
	}
	return values, valid, nil
}

// MaskStatistics computes calibrated ROI statistics without VOI transforms.
// Padding, NaN and ±Inf voxels are excluded and counted in Unmeasured.
func (m *Map) MaskStatistics(frame int, mask *roi.RasterMask) (Statistics, error) {
	if mask == nil || mask.Rows != m.Rows || mask.Columns != m.Columns {
		return Statistics{}, fmt.Errorf("%w: ROI mask dimensions", ErrGeometryMismatch)
	}
	values, err := m.FrameValues(frame)
	if err != nil {
		return Statistics{}, err
	}
	unmeasured := 0
	stats := roi.Stats2D(mask, func(x, y int) (float64, bool) {
		value := values[y*m.Columns+x]
		if !finite(value) {
			unmeasured++
			return 0, false
		}
		return value, true
	})
	return Statistics{Count: stats.Count, Min: stats.Min, Max: stats.Max, Mean: stats.Mean, StdDev: stats.StdDev, Unmeasured: unmeasured}, nil
}

func (m *Map) SetCacheLimitBytes(limit int64) {
	if m == nil {
		return
	}
	m.cacheMu.Lock()
	m.cacheLimit = max(0, limit)
	m.trimLocked()
	m.cacheMu.Unlock()
}

func (m *Map) CachedBytes() int64 {
	if m == nil {
		return 0
	}
	m.cacheMu.Lock()
	defer m.cacheMu.Unlock()
	return m.cacheBytes
}

func (m *Map) EvictBytes(target int64) int64 {
	if m == nil || target <= 0 {
		return 0
	}
	m.cacheMu.Lock()
	defer m.cacheMu.Unlock()
	before := m.cacheBytes
	for m.cacheBytes > 0 && before-m.cacheBytes < target && len(m.cacheOrder) > 0 {
		m.evictOldestLocked()
	}
	return before - m.cacheBytes
}

func (m *Map) touchLocked(index int) {
	for i, value := range m.cacheOrder {
		if value == index {
			copy(m.cacheOrder[i:], m.cacheOrder[i+1:])
			m.cacheOrder[len(m.cacheOrder)-1] = index
			return
		}
	}
}

func (m *Map) trimLocked() {
	for m.cacheBytes > m.cacheLimit && len(m.cacheOrder) > 0 {
		m.evictOldestLocked()
	}
}

func (m *Map) evictOldestLocked() {
	index := m.cacheOrder[0]
	m.cacheOrder = m.cacheOrder[1:]
	m.cacheBytes -= int64(len(m.cache[index]) * 8)
	delete(m.cache, index)
}

func readDimensions(obj *object.Object, perFrame []*object.Object, frames []Frame) error {
	dimensions := sequence(obj, tagDimensionIndexSeq)
	if len(dimensions) == 0 {
		return nil
	}
	pointers := make([]core.Tag, len(dimensions))
	for index, item := range dimensions {
		tag, ok := readTag(item, tagDimensionIndexPointer)
		if !ok {
			return fmt.Errorf("%w: invalid DimensionIndexPointer", ErrUnsupportedDimensions)
		}
		pointers[index] = tag
	}
	for index := range frames {
		if index >= len(perFrame) {
			return fmt.Errorf("%w: dimensions require per-frame groups", ErrUnsupportedDimensions)
		}
		content := firstItem(perFrame[index], tagFrameContentSeq)
		values64 := derivedio.Ints(content, tagDimensionIndexValues)
		if len(values64) != len(pointers) {
			return fmt.Errorf("%w: frame %d dimension value count", ErrUnsupportedDimensions, index+1)
		}
		frames[index].DimensionIndexValues = make([]int, len(values64))
		for i, value := range values64 {
			frames[index].DimensionIndexValues[i] = int(value)
		}
	}
	for dimension, pointer := range pointers {
		if pointer == tagImagePositionPatient || pointer == tagInStackPositionNumber {
			continue
		}
		first := frames[0].DimensionIndexValues[dimension]
		for frame := 1; frame < len(frames); frame++ {
			if frames[frame].DimensionIndexValues[dimension] != first {
				return fmt.Errorf("%w: varying dimension %s", ErrUnsupportedDimensions, pointer)
			}
		}
	}
	return nil
}

func readReferences(obj *object.Object) ([]Reference, error) {
	seen := map[string]Reference{}
	var failure error
	var walk func(*object.Object)
	walk = func(current *object.Object) {
		if current == nil {
			return
		}
		if uid := derivedio.CleanUID(current, tagReferencedSOPInstanceUID); uid != "" {
			ref := Reference{
				SOPClassUID:    derivedio.CleanUID(current, tagReferencedSOPClassUID),
				SOPInstanceUID: uid,
			}
			frameTag := core.NewTag(0x0008, 0x1160)
			if current.Has(frameTag) {
				parts, ok := current.GetStrings(frameTag)
				if !ok || len(parts) == 0 {
					failure = fmt.Errorf("%w: invalid referenced frame number", ErrInvalidObject)
					return
				}
				for _, part := range parts {
					n, err := strconv.Atoi(strings.TrimSpace(part))
					if err != nil || n <= 0 {
						failure = fmt.Errorf("%w: invalid referenced frame number", ErrInvalidObject)
						return
					}
					ref.Frames = append(ref.Frames, n)
				}
			}
			key := fmt.Sprintf("%s|%v", uid, ref.Frames)
			seen[key] = ref
		}
		for _, element := range current.Elements() {
			if element.VR() != core.VRSQ {
				continue
			}
			if items, ok := current.GetSequence(element.Tag()); ok {
				for _, item := range items {
					walk(item)
				}
			}
		}
	}
	walk(obj)
	out := make([]Reference, 0, len(seen))
	for _, ref := range seen {
		out = append(out, ref)
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].SOPInstanceUID != out[j].SOPInstanceUID {
			return out[i].SOPInstanceUID < out[j].SOPInstanceUID
		}
		return fmt.Sprint(out[i].Frames) < fmt.Sprint(out[j].Frames)
	})
	return out, failure
}

func readNativeFrames(obj *object.Object, element core.Element, length int) (*pixeldata.NativeFrames, error) {
	if _, incomplete := obj.IncompletePixelData(); incomplete {
		return nil, object.ErrPartialFile
	}
	if element.Value == nil {
		raw, err := readPixelPayload(obj, element, length)
		if err != nil {
			return nil, err
		}
		// Keep the caller's element deferred. The temporary object shares
		// metadata element storage and owns the admitted pixel buffer.
		view := object.FromElements(obj.Elements(), nil)
		view.SetValueByteOrder(obj.ValueByteOrder())
		element.Value = core.RawValue(raw)
		view.Put(element)
		obj = view
	}
	return pixeldata.ExtractNativeFramesView(obj)
}

// Dimensions and encoded length must be admitted before calling this helper.
// CopyValueTo replays deferred bytes in the object's normalized value order.
// The fixed writer prevents a malformed provider from growing the reservation.
func readPixelPayload(obj *object.Object, element core.Element, length int) ([]byte, error) {
	if raw, ok := element.RawBytes(); ok {
		if len(raw) != length {
			return nil, fmt.Errorf("%w: inconsistent pixel payload length", ErrInvalidObject)
		}
		return raw, nil
	}
	raw := make([]byte, length)
	w := &pixelPayloadWriter{remaining: raw}
	n, err := obj.CopyValueTo(element.Tag(), w)
	if err != nil || n != int64(length) || len(w.remaining) != 0 {
		return nil, fmt.Errorf("%w: read deferred pixel payload: copied %d of %d bytes: %v", ErrInvalidObject, n, length, err)
	}
	return raw, nil
}

type pixelPayloadWriter struct{ remaining []byte }

func (w *pixelPayloadWriter) Write(p []byte) (int, error) {
	n := copy(w.remaining, p)
	w.remaining = w.remaining[n:]
	if n != len(p) {
		return n, io.ErrShortWrite
	}
	return n, nil
}

func readQuantity(item *object.Object) Code {
	for _, quantityItem := range sequence(item, tagQuantityDefinitionSeq) {
		for _, tag := range []core.Tag{tagConceptCodeSeq, tagConceptNameCodeSeq} {
			if code := readCode(firstItem(quantityItem, tag)); code.String() != "" {
				return code
			}
		}
	}
	return Code{}
}

func readCode(item *object.Object) Code {
	if item == nil {
		return Code{}
	}
	return Code{
		Value:   derivedio.CleanString(item, tagCodeValue),
		Scheme:  derivedio.CleanString(item, tagCodingSchemeDesignator),
		Meaning: derivedio.CleanString(item, tagCodeMeaning),
	}
}

func firstItem(obj *object.Object, tag core.Tag) *object.Object {
	items := sequence(obj, tag)
	if len(items) == 0 {
		return nil
	}
	return items[0]
}

func sequence(obj *object.Object, tag core.Tag) []*object.Object {
	if obj == nil {
		return nil
	}
	return derivedio.Sequence(obj, tag)
}

func readTag(obj *object.Object, tag core.Tag) (core.Tag, bool) {
	if obj == nil {
		return core.Tag{}, false
	}
	element, ok := obj.Get(tag)
	if !ok {
		return core.Tag{}, false
	}
	raw, ok := element.RawBytes()
	if !ok || len(raw) < 4 {
		return core.Tag{}, false
	}
	order := obj.ValueByteOrder()
	return core.NewTag(order.Uint16(raw), order.Uint16(raw[2:])), true
}

func finite(value float64) bool         { return !math.IsNaN(value) && !math.IsInf(value, 0) }
func positiveFinite(value float64) bool { return value > 0 && finite(value) }
func finiteVec(value render.Vec3) bool  { return finite(value.X) && finite(value.Y) && finite(value.Z) }

func checkedProduct(values ...int) (int, bool) {
	product := 1
	maxInt := int(^uint(0) >> 1)
	for _, value := range values {
		if value <= 0 || product > maxInt/value {
			return 0, false
		}
		product *= value
	}
	return product, true
}
