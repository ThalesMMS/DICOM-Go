package spectroscopy

import (
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"math"

	"github.com/ThalesMMS/dicom-go/core"
	"github.com/ThalesMMS/dicom-go/object"
)

var (
	ErrInvalidMetadata = errors.New("dicom/spectroscopy: invalid metadata")
	ErrInvalidData     = errors.New("dicom/spectroscopy: invalid spectroscopy data")
	ErrDataLength      = errors.New("dicom/spectroscopy: spectroscopy data length mismatch")
	ErrLimitExceeded   = errors.New("dicom/spectroscopy: resource limit exceeded")
	ErrInvalidOptions  = errors.New("dicom/spectroscopy: invalid options")
	ErrIndex           = errors.New("dicom/spectroscopy: index out of range")
	ErrAxisUnavailable = errors.New("dicom/spectroscopy: axis calibration unavailable")
)

var (
	tagData           = core.NewTag(0x5600, 0x0020)
	tagFrames         = core.NewTag(0x0028, 0x0008)
	tagVoxelRows      = core.NewTag(0x0028, 0x0010)
	tagVoxelColumns   = core.NewTag(0x0028, 0x0011)
	tagPointRows      = core.NewTag(0x0028, 0x9001)
	tagPointColumns   = core.NewTag(0x0028, 0x9002)
	tagRepresentation = core.NewTag(0x0028, 0x9108)
	tagDomainColumns  = core.NewTag(0x0028, 0x9003)
	tagDomainRows     = core.NewTag(0x0028, 0x9235)
	tagSpectralWidth  = core.NewTag(0x0018, 0x9052)
	tagTransmitter    = core.NewTag(0x0018, 0x9098)
	tagChemicalShift  = core.NewTag(0x0018, 0x9053)
)

// Representation describes the stored components at each spectral point.
type Representation string

const (
	Complex   Representation = "COMPLEX" // interleaved real, imaginary
	Real      Representation = "REAL"
	Imaginary Representation = "IMAGINARY"
	Magnitude Representation = "MAGNITUDE"
)

func (r Representation) components() (int, error) {
	switch r {
	case Complex:
		return 2, nil
	case Real, Imaginary, Magnitude:
		return 1, nil
	default:
		return 0, fmt.Errorf("%w: unsupported data representation", ErrInvalidMetadata)
	}
}

type Domain string

const (
	Time      Domain = "TIME"
	Frequency Domain = "FREQUENCY"
)

// Dimensions describes the outer frame/voxel grid and each voxel's spectrum.
// Both the voxel grid and spectral grid are row-major; the entire spectrum of
// one voxel precedes the next voxel. Frames are the outermost dimension.
type Dimensions struct {
	Frames, VoxelRows, VoxelColumns int
	PointRows, PointColumns         int
}

// Axis describes a spectral axis. Columns vary along a data point row (the
// sampling axis); rows vary along a data point column (the evolution axis).
// Optional calibration values are nil when absent from the dataset. They are
// owned metadata copies, not references to the object's numeric values.
type Axis struct {
	Points                    int
	Domain                    Domain
	SpectralWidthHz           *float64
	TransmitterFrequencyMHz   *float64
	ChemicalShiftReferencePPM *float64
}

// FrequencyOffsetHz uses a uniform width/Points bin convention and high-to-low
// ordering. referenceIndex is supplied by the caller and can be fractional;
// it identifies zero relative frequency. DICOM does not specify that bin here.
// No absolute bin center, endpoint or Fourier-shift convention is inferred.
func (a Axis) FrequencyOffsetHz(index int, referenceIndex float64) (float64, error) {
	if a.Domain != Frequency {
		return 0, ErrAxisUnavailable
	}
	step, err := a.step(index, referenceIndex)
	if err != nil {
		return 0, err
	}
	return finiteCoordinate((referenceIndex - float64(index)) / float64(a.Points) * step)
}

// PPM uses referenceIndex as the bin at the transmitter frequency, whose
// chemical shift is ChemicalShiftReferencePPM. The caller must establish that
// reference index externally; the package does not assume a centered spectrum.
func (a Axis) PPM(index int, referenceIndex float64) (float64, error) {
	offset, err := a.FrequencyOffsetHz(index, referenceIndex)
	if err != nil {
		return 0, err
	}
	if a.TransmitterFrequencyMHz == nil || !positiveFinite(*a.TransmitterFrequencyMHz) || a.ChemicalShiftReferencePPM == nil || !finite(*a.ChemicalShiftReferencePPM) {
		return 0, ErrAxisUnavailable
	}
	return finiteCoordinate(*a.ChemicalShiftReferencePPM + offset / *a.TransmitterFrequencyMHz)
}

// TimeOffsetSeconds uses sampling interval 1/SpectralWidthHz and increasing
// time, relative to an explicit referenceIndex. It does not infer acquisition
// start time or perform frequency/time-domain conversion.
func (a Axis) TimeOffsetSeconds(index int, referenceIndex float64) (float64, error) {
	if a.Domain != Time {
		return 0, ErrAxisUnavailable
	}
	width, err := a.step(index, referenceIndex)
	if err != nil {
		return 0, err
	}
	return finiteCoordinate((float64(index) - referenceIndex) / width)
}

func (a Axis) step(index int, referenceIndex float64) (float64, error) {
	if a.Points <= 0 || index < 0 || index >= a.Points {
		return 0, ErrIndex
	}
	if !finite(referenceIndex) || a.SpectralWidthHz == nil || !positiveFinite(*a.SpectralWidthHz) {
		return 0, ErrAxisUnavailable
	}
	return *a.SpectralWidthHz, nil
}

func finiteCoordinate(value float64) (float64, error) {
	if !finite(value) {
		return 0, ErrAxisUnavailable
	}
	return value, nil
}

func finite(value float64) bool         { return !math.IsNaN(value) && !math.IsInf(value, 0) }
func positiveFinite(value float64) bool { return value > 0 && finite(value) }

// Data owns Values in encoded frame/voxel/spectral/component order. COMPLEX
// stores real/imaginary pairs; other representations store one float per point.
// IEEE-754 values (including NaN payloads, infinities and signed zero) are kept.
// Values does not alias the source object. Spectrum and Point return views of
// this owned buffer; mutation of those views mutates Data, never the object.
// Data and the source object must not be mutated concurrently with access/read.
type Data struct {
	Dimensions     Dimensions
	Representation Representation
	ColumnAxis     Axis
	RowAxis        *Axis
	Values         []float32
}

// Spectrum returns all components of one voxel's complete row-major spectrum.
func (d *Data) Spectrum(frame, voxelRow, voxelColumn int) ([]float32, error) {
	components, err := d.validLayout()
	if err != nil {
		return nil, err
	}
	shape := d.Dimensions
	if frame < 0 || frame >= shape.Frames || voxelRow < 0 || voxelRow >= shape.VoxelRows || voxelColumn < 0 || voxelColumn >= shape.VoxelColumns {
		return nil, ErrIndex
	}
	count := shape.PointRows * shape.PointColumns * components
	first := ((frame*shape.VoxelRows+voxelRow)*shape.VoxelColumns + voxelColumn) * count
	return d.Values[first : first+count : first+count], nil
}

// Point returns the stored component(s) at one spectral grid position.
func (d *Data) Point(frame, voxelRow, voxelColumn, pointRow, pointColumn int) ([]float32, error) {
	spectrum, err := d.Spectrum(frame, voxelRow, voxelColumn)
	if err != nil {
		return nil, err
	}
	shape := d.Dimensions
	if pointRow < 0 || pointRow >= shape.PointRows || pointColumn < 0 || pointColumn >= shape.PointColumns {
		return nil, ErrIndex
	}
	components, _ := d.Representation.components()
	first := (pointRow*shape.PointColumns + pointColumn) * components
	return spectrum[first : first+components : first+components], nil
}

func (d *Data) validLayout() (int, error) {
	if d == nil {
		return 0, ErrInvalidMetadata
	}
	components, err := d.Representation.components()
	if err != nil {
		return 0, err
	}
	count, err := valueCount(d.Dimensions, components)
	if err != nil {
		return 0, err
	}
	if count != int64(len(d.Values)) {
		return 0, ErrDataLength
	}
	return components, nil
}

const (
	DefaultMaxBytes  int64 = 64 << 20
	DefaultMaxValues int64 = DefaultMaxBytes / 4
)

// Options bounds the output allocation. MaxValues counts stored float32
// components, not complex pairs. Zero uses the finite default; negative limits
// are invalid. Header/value sizes, dimensions and count are checked before
// allocation. Parser/file input limits are configured separately by the caller.
type Options struct {
	MaxBytes  int64
	MaxValues int64
}

func Read(obj *object.Object, opts Options) (*Data, error) {
	return ReadContext(context.Background(), obj, opts)
}

// ReadContext reads a materialized or deferred OF value using the object's
// byte order. Deferred values stream through CopyValueTo into the sole owned
// float buffer; no full byte clone or second data-sized buffer is constructed.
// It does not close the object/file. Context is checked at copy/write/conversion
// boundaries; an arbitrary blocked value provider must cooperate separately.
func ReadContext(ctx context.Context, obj *object.Object, opts Options) (*Data, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if opts.MaxBytes < 0 || opts.MaxValues < 0 {
		return nil, ErrInvalidOptions
	}
	if opts.MaxBytes == 0 {
		opts.MaxBytes = DefaultMaxBytes
	}
	if opts.MaxValues == 0 {
		opts.MaxValues = DefaultMaxValues
	}
	if obj == nil {
		return nil, ErrInvalidMetadata
	}
	data, err := readMetadata(obj)
	if err != nil {
		return nil, err
	}
	components, _ := data.Representation.components()
	count, err := valueCount(data.Dimensions, components)
	if err != nil {
		return nil, err
	}
	// valueCount also ensures byte count and indexing fit the host's int.
	bytes := count * 4
	if count > opts.MaxValues || bytes > opts.MaxBytes {
		return nil, ErrLimitExceeded
	}
	element, ok := obj.Get(tagData)
	if !ok || element.VR() != core.VROF {
		return nil, fmt.Errorf("%w: (5600,0020) must be OF", ErrInvalidData)
	}
	switch value := element.Value.(type) {
	case nil:
	case core.RawValue:
		if int64(len(value)) != bytes {
			return nil, ErrDataLength
		}
	case core.Float32Value:
		if int64(len(value)) != count {
			return nil, ErrDataLength
		}
	default:
		return nil, fmt.Errorf("%w: unsupported OF value kind", ErrInvalidData)
	}
	if element.Length() == core.UndefinedLength || int64(element.Length()) != bytes {
		return nil, ErrDataLength
	}
	if element.Value != nil {
		length, known := element.CalculatedLength()
		if !known || int64(length) != bytes {
			return nil, ErrDataLength
		}
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	data.Values = make([]float32, int(count))
	if typed, ok := element.Value.(core.Float32Value); ok {
		for first := 0; first < len(typed); first += 1024 {
			if err := ctx.Err(); err != nil {
				return nil, err
			}
			last := first + 1024
			if last > len(typed) {
				last = len(typed)
			}
			copy(data.Values[first:last], typed[first:last])
		}
	} else {
		sink := floatSink{ctx: ctx, order: obj.ValueByteOrder(), values: data.Values}
		copied, err := obj.CopyValueTo(tagData, &sink)
		if err != nil {
			return nil, fmt.Errorf("dicom/spectroscopy: copy OF: %w", err)
		}
		if copied != bytes || sink.written != bytes || sink.index != len(data.Values) || sink.buffered != 0 {
			return nil, ErrDataLength
		}
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	return data, nil
}

func readMetadata(obj *object.Object) (*Data, error) {
	fields := []struct {
		tag core.Tag
		vr  core.VR
	}{
		{tagFrames, core.VRIS}, {tagVoxelRows, core.VRUS}, {tagVoxelColumns, core.VRUS},
		{tagPointRows, core.VRUL}, {tagPointColumns, core.VRUL},
	}
	var dims [5]int
	for index, field := range fields {
		limit := 4
		if field.vr == core.VRUS {
			limit = 2
		}
		if field.vr == core.VRIS {
			limit = 12
		}
		if err := checkMetadataElement(obj, field.tag, field.vr, limit); err != nil {
			return nil, err
		}
		values, err := obj.GetInts(field.tag)
		if err != nil || len(values) != 1 || values[0] <= 0 || uint64(values[0]) > uint64(maxInt()) {
			return nil, metadataError(field.tag)
		}
		dims[index] = int(values[0])
	}
	data := &Data{Dimensions: Dimensions{Frames: dims[0], VoxelRows: dims[1], VoxelColumns: dims[2], PointRows: dims[3], PointColumns: dims[4]}}
	representation, err := codeValue(obj, tagRepresentation)
	if err != nil {
		return nil, err
	}
	data.Representation = Representation(representation)
	if _, err := data.Representation.components(); err != nil {
		return nil, err
	}
	columnDomain, err := signalDomain(obj, tagDomainColumns)
	if err != nil {
		return nil, err
	}
	data.ColumnAxis = Axis{Points: data.Dimensions.PointColumns, Domain: columnDomain}
	if data.Dimensions.PointRows > 1 || obj.Has(tagDomainRows) {
		rowDomain, err := signalDomain(obj, tagDomainRows)
		if err != nil {
			return nil, err
		}
		data.RowAxis = &Axis{Points: data.Dimensions.PointRows, Domain: rowDomain}
	}
	rank := 1
	if data.Dimensions.PointRows > 1 {
		rank = 2
	}
	for _, tag := range []core.Tag{tagSpectralWidth, tagTransmitter, tagChemicalShift} {
		if !obj.Has(tag) {
			continue
		}
		if err := checkMetadataElement(obj, tag, core.VRFD, rank*8); err != nil {
			return nil, err
		}
		values, err := obj.GetFloats(tag)
		if err != nil || len(values) == 0 || len(values) > rank {
			return nil, metadataError(tag)
		}
		for index, value := range values {
			if !finite(value) || tag != tagChemicalShift && value <= 0 {
				return nil, metadataError(tag)
			}
			axis := &data.ColumnAxis
			if index == 1 {
				axis = data.RowAxis
			}
			owned := value
			switch tag {
			case tagSpectralWidth:
				axis.SpectralWidthHz = &owned
			case tagTransmitter:
				axis.TransmitterFrequencyMHz = &owned
			case tagChemicalShift:
				axis.ChemicalShiftReferencePPM = &owned
			}
		}
	}
	return data, nil
}

func metadataError(tag core.Tag) error {
	return fmt.Errorf("%w: attribute %s", ErrInvalidMetadata, tag)
}

func checkMetadataElement(obj *object.Object, tag core.Tag, vr core.VR, maximumBytes int) error {
	element, ok := obj.Get(tag)
	if !ok || element.VR() != vr {
		return metadataError(tag)
	}
	if !metadataValueWithinBounds(element.Value, vr, maximumBytes) {
		return metadataError(tag)
	}
	if element.Length() == core.UndefinedLength || element.Length() == 0 || uint64(element.Length()) > uint64(maximumBytes) {
		return metadataError(tag)
	}
	if element.Value != nil {
		if raw, ok := element.Value.(core.RawValue); ok && uint64(len(raw)) != uint64(element.Length()) {
			return metadataError(tag)
		}
		length, known := element.CalculatedLength()
		if !known || length != element.Length() {
			return metadataError(tag)
		}
	}
	width := 1
	switch vr {
	case core.VRUS:
		width = 2
	case core.VRUL:
		width = 4
	case core.VRFD:
		width = 8
	}
	if uint64(element.Length())%uint64(width) != 0 {
		return metadataError(tag)
	}
	return nil
}

// Preflight actual counts before any serialized uint32 Length calculation or
// numeric accessor. Division avoids overflowing count*width for hostile values.
func countWithinBytes(count, width, maximum int) bool {
	return count > 0 && width > 0 && maximum >= 0 && count <= maximum/width
}

func metadataValueWithinBounds(value core.Value, vr core.VR, maximum int) bool {
	switch value := value.(type) {
	case nil:
		return true // The object accessor reports an unavailable deferred scalar.
	case core.RawValue:
		return len(value) <= maximum
	case core.Uint16Value:
		return vr == core.VRUS && countWithinBytes(len(value), 2, maximum)
	case core.Uint32Value:
		return vr == core.VRUL && countWithinBytes(len(value), 4, maximum)
	case core.Float64Value:
		return vr == core.VRFD && countWithinBytes(len(value), 8, maximum)
	case core.StringValue:
		return (vr == core.VRIS || vr == core.VRCS) && len(value) == 1 && len(value[0]) <= maximum
	default:
		return false
	}
}

func codeValue(obj *object.Object, tag core.Tag) (string, error) {
	if err := checkMetadataElement(obj, tag, core.VRCS, 16); err != nil {
		return "", err
	}
	values, err := obj.LookupStrings(tag)
	if err != nil || len(values) != 1 || values[0] == "" {
		return "", metadataError(tag)
	}
	return values[0], nil
}

func signalDomain(obj *object.Object, tag core.Tag) (Domain, error) {
	value, err := codeValue(obj, tag)
	if err != nil {
		return "", err
	}
	if value != string(Time) && value != string(Frequency) {
		return "", metadataError(tag)
	}
	return Domain(value), nil
}

func maxInt() int { return int(^uint(0) >> 1) }

func valueCount(shape Dimensions, components int) (int64, error) {
	count := int64(1)
	for _, dim := range []int{shape.Frames, shape.VoxelRows, shape.VoxelColumns, shape.PointRows, shape.PointColumns, components, 4} {
		if dim <= 0 {
			return 0, ErrInvalidMetadata
		}
		if count > int64(maxInt())/int64(dim) {
			return 0, ErrLimitExceeded
		}
		count *= int64(dim)
	}
	return count / 4, nil
}

// floatSink converts arbitrarily chunked little/big-endian writes directly to
// output values, with at most a partial four-byte word as temporary storage.
type floatSink struct {
	ctx      context.Context
	order    binary.ByteOrder
	values   []float32
	index    int
	written  int64
	pending  [4]byte
	buffered int
}

func (s *floatSink) Write(data []byte) (int, error) {
	if err := s.ctx.Err(); err != nil {
		return 0, err
	}
	if int64(len(data)) > int64(len(s.values))*4-s.written {
		return 0, ErrDataLength
	}
	consumed := 0
	if s.buffered > 0 {
		n := copy(s.pending[s.buffered:], data)
		s.buffered += n
		data, consumed = data[n:], n
		if s.buffered == 4 {
			s.values[s.index] = math.Float32frombits(s.order.Uint32(s.pending[:]))
			s.index++
			s.buffered = 0
		}
	}
	for len(data) >= 4 {
		if s.index%1024 == 0 {
			if err := s.ctx.Err(); err != nil {
				s.written += int64(consumed)
				return consumed, err
			}
		}
		s.values[s.index] = math.Float32frombits(s.order.Uint32(data[:4]))
		s.index++
		data = data[4:]
		consumed += 4
	}
	if len(data) > 0 {
		s.buffered = copy(s.pending[:], data)
		consumed += len(data)
	}
	s.written += int64(consumed)
	return consumed, nil
}
