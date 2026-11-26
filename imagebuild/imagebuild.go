// Package imagebuild creates DICOM image instances from buffers of modality
// values (Hounsfield units, MR signal, or any real-world quantity) with their
// patient-space geometry: one classic CT, MR or Secondary Capture instance
// per slice, with the inverse rescale to 16-bit stored values, the Image
// Plane module, the modules each IOD requires, the derivation, and the
// patient and study modules inherited from a source instance.
package imagebuild

import (
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"math"
	"strings"
	"time"

	"github.com/ThalesMMS/dicom-go/core"
	"github.com/ThalesMMS/dicom-go/derivation"
	"github.com/ThalesMMS/dicom-go/internal/derivedio"
	"github.com/ThalesMMS/dicom-go/object"
)

// ErrInvalid reports geometry, values or options that do not make a valid
// image.
var ErrInvalid = errors.New("dicom/imagebuild: invalid image")

// IOD selects the Information Object Definition of the instances.
type IOD int

const (
	// CT builds CT Image Storage instances (PS3.3 A.3).
	CT IOD = iota + 1
	// MR builds MR Image Storage instances (A.4).
	MR
	// SecondaryCapture builds Secondary Capture Image Storage instances
	// (A.8.1), omitting the Image Plane unless requested.
	SecondaryCapture
)

const (
	ctImageStorage               = "1.2.840.10008.5.1.4.1.1.2"
	mrImageStorage               = "1.2.840.10008.5.1.4.1.1.4"
	secondaryCaptureImageStorage = "1.2.840.10008.5.1.4.1.1.7"
)

// Geometry is the Image Plane shared by every slice.
type Geometry struct {
	Rows, Columns int
	// PixelSpacing is Pixel Spacing (0028,0030): the distance between rows,
	// then between columns, in mm.
	PixelSpacing [2]float64
	// RowDirection and ColumnDirection are Image Orientation (Patient): the
	// unit direction along a row (increasing column) and along a column.
	RowDirection, ColumnDirection [3]float64
	// SliceThickness in mm; zero writes it empty (Type 2).
	SliceThickness float64
	// FrameOfReferenceUID; empty inherits the source's (Options.Common) or,
	// without one, generates a new frame.
	FrameOfReferenceUID string
}

// Slice is one image: Rows x Columns modality values in row-major order and
// Image Position (Patient), the centre of its first pixel.
type Slice struct {
	Values   []float64
	Position [3]float64
	// Valid, when provided to Build, marks samples that exist in the source
	// domain. False samples use one stored padding code outside the valid
	// range. A nil mask treats every sample as valid.
	Valid []bool
	// Sources are added to Options.Derivation's sources in this instance
	// only, such as the source images a resampled slice was read from.
	Sources []derivation.Source
}

// Rescale maps stored values to modality values: v = Slope*stored + Intercept.
type Rescale struct {
	Slope, Intercept float64
}

// Window is a VOI window written as Window Center and Width.
type Window struct {
	Center, Width float64
}

// Options describes the series the instances form.
type Options struct {
	IOD IOD
	// SecondaryCaptureImagePlane includes the optional Image Plane and
	// Frame of Reference modules for a spatially calibrated single-frame SC
	// (DICOM PS3.3 Table A.8-1). It has no effect on CT or MR.
	SecondaryCaptureImagePlane bool
	// Common supplies the patient, study, Frame of Reference and equipment
	// (object.DerivedCommon); for CT and MR a source of the same modality
	// also supplies the acquisition attributes the IOD requires.
	Common            object.DerivedCommon
	SeriesInstanceUID string // generated when empty
	// SOPInstanceUIDs optionally fixes one identity per slice, for reviewed
	// provenance or deterministic output. Empty retains generated identities.
	SOPInstanceUIDs   []string
	SeriesNumber      int
	SeriesDescription string
	// ImageType is Image Type (0008,0008). Empty is DERIVED\SECONDARY with
	// the third value CT and MR require: AXIAL for CT (every cross-section,
	// reformats included), OTHER for MR (MPR suits a reformat); a two-value
	// type gets that third value.
	ImageType []string
	// Derivation is recorded in every instance (derivation.Apply), for
	// example derivation.MultiplanarReformatting from the source images.
	Derivation derivation.Derivation
	// ReferencedInstances are written as the Referenced Instance Sequence
	// (0008,114A) of every instance: other instances significantly related
	// to the images, such as the Spatial Registration they were resampled
	// through. Each needs a Purpose of Reference.
	ReferencedInstances []derivation.Source
	// Rescale fixes the stored values; nil stores integral values as they
	// are when they fit 16 bits, and otherwise spans the value range with a
	// slope over unsigned 16-bit values.
	Rescale *Rescale
	// RescaleType is Rescale Type (0028,1054), such as HU; empty omits it,
	// except for Secondary Capture, which requires it (US, unspecified).
	// MR has no Modality LUT module: an identity rescale is not written.
	RescaleType string
	Window      *Window
	// ContentTime is Content Date and Time; zero uses the current time.
	ContentTime time.Time
}

// Build returns one instance per slice, numbered from 1, all in one new
// series.
func Build(geometry Geometry, slices []Slice, opts Options) ([]*object.File, error) {
	if err := validate(geometry, slices, opts); err != nil {
		return nil, err
	}
	rescale, signed, padding, err := chooseRescale(slices, opts)
	if err != nil {
		return nil, err
	}
	seriesUID := strings.TrimSpace(opts.SeriesInstanceUID)
	if seriesUID == "" {
		if seriesUID, err = newUID(); err != nil {
			return nil, err
		}
	}
	frameUID := strings.TrimSpace(geometry.FrameOfReferenceUID)
	if frameUID == "" && opts.hasImagePlane() && (opts.Common.Source == nil || !opts.Common.Source.Has(tagFrameOfReferenceUID)) {
		if frameUID, err = newUID(); err != nil {
			return nil, err
		}
	}
	// Without a source study every instance would get its own generated
	// Study Instance UID; the series shares one.
	studyUID := ""
	if opts.Common.Source == nil || !opts.Common.Source.Has(tagStudyInstanceUID) {
		if studyUID, err = newUID(); err != nil {
			return nil, err
		}
	}
	now := opts.ContentTime
	if now.IsZero() {
		now = time.Now()
	}
	files := make([]*object.File, 0, len(slices))
	for i, slice := range slices {
		file, err := buildInstancePixels(context.Background(), geometry, slice, i+1, opts, rescale, signed, seriesUID, studyUID, frameUID, now, nil, padding)
		if err != nil {
			return nil, fmt.Errorf("slice %d: %w", i+1, err)
		}
		files = append(files, file)
	}
	return files, nil
}

func (o Options) hasImagePlane() bool {
	return o.IOD != SecondaryCapture || o.SecondaryCaptureImagePlane
}

func validate(geometry Geometry, slices []Slice, opts Options) error {
	switch {
	case opts.IOD != CT && opts.IOD != MR && opts.IOD != SecondaryCapture:
		return fmt.Errorf("%w: unknown IOD %d", ErrInvalid, opts.IOD)
	case geometry.Rows < 1 || geometry.Columns < 1 || geometry.Rows > math.MaxUint16 || geometry.Columns > math.MaxUint16:
		return fmt.Errorf("%w: %dx%d pixels", ErrInvalid, geometry.Columns, geometry.Rows)
	case len(slices) == 0:
		return fmt.Errorf("%w: no slices", ErrInvalid)
	}
	if err := validateInstanceUIDs(opts.SOPInstanceUIDs, len(slices)); err != nil {
		return err
	}
	if opts.hasImagePlane() {
		if !(geometry.PixelSpacing[0] > 0) || !(geometry.PixelSpacing[1] > 0) || math.IsInf(geometry.PixelSpacing[0], 0) || math.IsInf(geometry.PixelSpacing[1], 0) {
			return fmt.Errorf("%w: pixel spacing %v", ErrInvalid, geometry.PixelSpacing)
		}
		row, column := geometry.RowDirection, geometry.ColumnDirection
		if math.Abs(norm(row)-1) > 1e-3 || math.Abs(norm(column)-1) > 1e-3 || math.Abs(dot(row, column)) > 1e-3 {
			return fmt.Errorf("%w: orientation is not two orthogonal unit vectors", ErrInvalid)
		}
	}
	for i, slice := range slices {
		if len(slice.Values) != geometry.Rows*geometry.Columns {
			return fmt.Errorf("%w: slice %d has %d values for %dx%d pixels", ErrInvalid, i+1, len(slice.Values), geometry.Columns, geometry.Rows)
		}
		if slice.Valid != nil && len(slice.Valid) != len(slice.Values) {
			return fmt.Errorf("%w: slice %d has %d validity flags for %d values", ErrInvalid, i+1, len(slice.Valid), len(slice.Values))
		}
		for _, v := range slice.Position {
			if math.IsNaN(v) || math.IsInf(v, 0) {
				return fmt.Errorf("%w: slice %d position %v", ErrInvalid, i+1, slice.Position)
			}
		}
	}
	return nil
}

func norm(v [3]float64) float64   { return math.Sqrt(dot(v, v)) }
func dot(a, b [3]float64) float64 { return a[0]*b[0] + a[1]*b[1] + a[2]*b[2] }

// chooseRescale picks the rescale and the sign of the stored values.
func chooseRescale(slices []Slice, opts Options) (Rescale, bool, *int32, error) {
	low, high := math.Inf(1), math.Inf(-1)
	integral, padded := true, false
	for _, slice := range slices {
		for i, v := range slice.Values {
			if slice.Valid != nil && !slice.Valid[i] {
				padded = true
				continue
			}
			if math.IsNaN(v) || math.IsInf(v, 0) {
				return Rescale{}, false, nil, fmt.Errorf("%w: value %v", ErrInvalid, v)
			}
			low, high = math.Min(low, v), math.Max(high, v)
			integral = integral && math.Abs(v-math.Round(v)) < 1e-6
		}
	}
	if low > high {
		return Rescale{}, false, nil, fmt.Errorf("%w: no valid pixels", ErrInvalid)
	}
	rescale, signed, err := chooseRescaleRange(low, high, integral, opts.Rescale)
	if err != nil || !padded {
		return rescale, signed, nil, err
	}
	identity := Rescale{Slope: 1}
	if opts.IOD == MR && rescale != identity {
		return Rescale{}, false, nil, fmt.Errorf("%w: padded classic MR requires integral values with identity rescale", ErrInvalid)
	}
	if opts.Rescale != nil || rescale == identity {
		if padding, ok := spareStoredCode(low, high, rescale, signed); ok {
			return rescale, signed, &padding, nil
		}
		if opts.Rescale != nil || opts.IOD == MR {
			return Rescale{}, false, nil, fmt.Errorf("%w: no unused padding code in the selected rescale", ErrInvalid)
		}
	}
	// The ordinary unsigned quantizer has 65,536 values. Leave one endpoint
	// out of the valid range. Constant values already map to stored zero.
	padding := int32(math.MaxUint16)
	if high > low {
		rescale = Rescale{Slope: (high - low) / (math.MaxUint16 - 1), Intercept: low}
		// Prefer a black padding code where shifting the mapping does not
		// consume the available precision at this magnitude.
		dark := Rescale{Slope: rescale.Slope, Intercept: low - rescale.Slope}
		if code, ok := spareStoredCode(low, high, dark, false); ok && code == 0 {
			rescale, padding = dark, code
		}
	}
	if rescale.Slope <= 0 || math.IsInf(rescale.Slope, 0) || math.IsInf(rescale.Intercept, 0) {
		return Rescale{}, false, nil, fmt.Errorf("%w: padded pixel range cannot be represented", ErrInvalid)
	}
	return rescale, false, &padding, nil
}

func spareStoredCode(low, high float64, rescale Rescale, signed bool) (int32, bool) {
	a, b := math.Round((low-rescale.Intercept)/rescale.Slope), math.Round((high-rescale.Intercept)/rescale.Slope)
	minimum, maximum := 0.0, float64(math.MaxUint16)
	if signed {
		minimum, maximum = math.MinInt16, math.MaxInt16
	}
	lo, hi := min(a, b), max(a, b)
	if !(lo >= minimum && hi <= maximum) {
		return 0, false
	}
	if lo > minimum {
		return int32(minimum), true
	}
	if hi < maximum {
		return int32(maximum), true
	}
	return 0, false
}

func chooseRescaleRange(low, high float64, integral bool, fixed *Rescale) (Rescale, bool, error) {
	if fixed != nil {
		if !(fixed.Slope != 0) || math.IsNaN(fixed.Slope) || math.IsInf(fixed.Slope, 0) || math.IsNaN(fixed.Intercept) || math.IsInf(fixed.Intercept, 0) {
			return Rescale{}, false, fmt.Errorf("%w: rescale %+v", ErrInvalid, *fixed)
		}
		a, b := math.Round((low-fixed.Intercept)/fixed.Slope), math.Round((high-fixed.Intercept)/fixed.Slope)
		lo, hi := math.Min(a, b), math.Max(a, b)
		switch {
		case lo >= 0 && hi <= math.MaxUint16:
			return *fixed, false, nil
		case lo >= math.MinInt16 && hi <= math.MaxInt16:
			return *fixed, true, nil
		}
		return Rescale{}, false, fmt.Errorf("%w: values %v to %v do not fit 16 bits with %+v", ErrInvalid, low, high, *fixed)
	}
	switch {
	case integral && low >= 0 && high <= math.MaxUint16:
		return Rescale{Slope: 1}, false, nil
	case integral && low >= math.MinInt16 && high <= math.MaxInt16:
		return Rescale{Slope: 1}, true, nil
	case high == low:
		return Rescale{Slope: 1, Intercept: low}, false, nil
	}
	return Rescale{Slope: (high - low) / math.MaxUint16, Intercept: low}, false, nil
}

var (
	tagImageType           = core.NewTag(0x0008, 0x0008)
	tagContentDate         = core.NewTag(0x0008, 0x0023)
	tagContentTime         = core.NewTag(0x0008, 0x0033)
	tagConversionType      = core.NewTag(0x0008, 0x0064)
	tagSeriesDescription   = core.NewTag(0x0008, 0x103e)
	tagSliceThickness      = core.NewTag(0x0018, 0x0050)
	tagStudyInstanceUID    = core.NewTag(0x0020, 0x000d)
	tagSeriesNumber        = core.NewTag(0x0020, 0x0011)
	tagInstanceNumber      = core.NewTag(0x0020, 0x0013)
	tagPatientOrientation  = core.NewTag(0x0020, 0x0020)
	tagImagePosition       = core.NewTag(0x0020, 0x0032)
	tagImageOrientation    = core.NewTag(0x0020, 0x0037)
	tagFrameOfReferenceUID = core.NewTag(0x0020, 0x0052)
	tagLaterality          = core.NewTag(0x0020, 0x0060)
	tagPixelSpacing        = core.NewTag(0x0028, 0x0030)
	tagPixelPaddingValue   = core.NewTag(0x0028, 0x0120)
	tagWindowCenter        = core.NewTag(0x0028, 0x1050)
	tagWindowWidth         = core.NewTag(0x0028, 0x1051)
	tagRescaleIntercept    = core.NewTag(0x0028, 0x1052)
	tagRescaleSlope        = core.NewTag(0x0028, 0x1053)
	tagRescaleType         = core.NewTag(0x0028, 0x1054)
	tagModality            = core.NewTag(0x0008, 0x0060)
	tagPatientPosition     = core.NewTag(0x0018, 0x5100)
	tagBodyPartExamined    = core.NewTag(0x0018, 0x0015)

	tagReferencedInstanceSequence = core.NewTag(0x0008, 0x114A)

	// Acquisition attributes copied from a source of the same modality;
	// the MR ones the IOD requires default to RM and NONE.
	ctAcquisition = []core.Tag{core.NewTag(0x0018, 0x0060), core.NewTag(0x0020, 0x0012), tagPatientPosition}
	mrAcquisition = []core.Tag{
		core.NewTag(0x0018, 0x0020), core.NewTag(0x0018, 0x0021), core.NewTag(0x0018, 0x0022), core.NewTag(0x0018, 0x0023),
		core.NewTag(0x0018, 0x0080), core.NewTag(0x0018, 0x0081), core.NewTag(0x0018, 0x0082), core.NewTag(0x0018, 0x0091),
		core.NewTag(0x0018, 0x0087), core.NewTag(0x0018, 0x0085), core.NewTag(0x0020, 0x0012), tagPatientPosition,
	}
)

func buildInstancePixels(ctx context.Context, geometry Geometry, slice Slice, number int, opts Options, rescale Rescale, signed bool, seriesUID, studyUID, frameUID string, now time.Time, pixels []byte, padding *int32) (*object.File, error) {
	var sopUID string
	if len(opts.SOPInstanceUIDs) > 0 {
		sopUID = opts.SOPInstanceUIDs[number-1]
	} else {
		var err error
		sopUID, err = newUID()
		if err != nil {
			return nil, err
		}
	}
	sopClass, modality := ctImageStorage, "CT"
	switch opts.IOD {
	case MR:
		sopClass, modality = mrImageStorage, "MR"
	case SecondaryCapture:
		sopClass, modality = secondaryCaptureImageStorage, "OT"
	}
	imageType := append([]string(nil), opts.ImageType...)
	if len(imageType) == 0 {
		imageType = []string{"DERIVED", "SECONDARY"}
	}
	if len(imageType) == 2 {
		switch opts.IOD {
		case CT:
			imageType = append(imageType, "AXIAL")
		case MR:
			imageType = append(imageType, "OTHER")
		}
	}
	representation := uint16(0)
	if signed {
		representation = 1
	}
	if pixels == nil {
		pixels = make([]byte, 2*len(slice.Values))
	}
	padded := false
	for i, v := range slice.Values {
		if i%4096 == 0 {
			if err := ctx.Err(); err != nil {
				return nil, err
			}
		}
		if padding != nil && slice.Valid != nil && !slice.Valid[i] {
			putUint16(pixels[2*i:], uint16(*padding))
			padded = true
			continue
		}
		stored := math.Round((v - rescale.Intercept) / rescale.Slope)
		if padding != nil {
			low, high := 0.0, float64(math.MaxUint16)
			if signed {
				low, high = math.MinInt16, math.MaxInt16
			}
			if !(stored >= low && stored <= high) || stored == float64(*padding) {
				return nil, fmt.Errorf("%w: valid pixel %d does not fit the reserved-padding rescale", ErrInvalid, i+1)
			}
		}
		if signed {
			stored = math.Max(math.MinInt16, math.Min(math.MaxInt16, stored))
			putUint16(pixels[2*i:], uint16(int16(stored)))
		} else {
			stored = math.Max(0, math.Min(math.MaxUint16, stored))
			putUint16(pixels[2*i:], uint16(stored))
		}
	}
	elements := []core.Element{
		derivedio.UI(derivedio.TagSOPClassUID, sopClass),
		derivedio.UI(derivedio.TagSOPInstanceUID, sopUID),
		derivedio.Strings(tagImageType, core.VRCS, imageType),
		derivedio.Str(tagContentDate, core.VRDA, now.Format("20060102")),
		derivedio.Str(tagContentTime, core.VRTM, now.Format("150405")),
		derivedio.CS(tagModality, modality),
		derivedio.UI(derivedio.TagSeriesInstanceUID, seriesUID),
		derivedio.IS(tagInstanceNumber, number),
		derivedio.US(derivedio.TagSamplesPerPixel, 1),
		derivedio.CS(derivedio.TagPhotometricInterpretation, "MONOCHROME2"),
		derivedio.US(derivedio.TagRows, uint16(geometry.Rows)),
		derivedio.US(derivedio.TagColumns, uint16(geometry.Columns)),
		derivedio.US(derivedio.TagBitsAllocated, 16),
		derivedio.US(derivedio.TagBitsStored, 16),
		derivedio.US(derivedio.TagHighBit, 15),
		derivedio.US(derivedio.TagPixelRepresentation, representation),
		// pixels is owned by Build, or borrowed only through WriteSeries' serial
		// callback. Do not clone a whole plane during incremental serialization.
		{Header: core.ElementHeader{Tag: core.TagPixelData, VR: core.VROW, Length: core.Length(len(pixels)), LengthSet: true}, Value: core.RawValue(pixels)},
	}
	if padded {
		if signed {
			elements = append(elements, object.SSElement(tagPixelPaddingValue, binary.LittleEndian, int16(*padding)))
		} else {
			elements = append(elements, derivedio.US(tagPixelPaddingValue, uint16(*padding)))
		}
	}
	if opts.IOD != MR || rescale != (Rescale{Slope: 1}) {
		elements = append(elements, derivedio.DS(tagRescaleIntercept, rescale.Intercept), derivedio.DS(tagRescaleSlope, rescale.Slope))
	}
	if opts.SeriesNumber > 0 {
		elements = append(elements, derivedio.IS(tagSeriesNumber, opts.SeriesNumber))
	} else {
		elements = append(elements, core.NewRawElement(tagSeriesNumber, core.VRIS, nil))
	}
	if description := strings.TrimSpace(opts.SeriesDescription); description != "" {
		elements = append(elements, derivedio.LO(tagSeriesDescription, description))
	}
	rescaleType := strings.TrimSpace(opts.RescaleType)
	if rescaleType == "" && opts.IOD == SecondaryCapture {
		rescaleType = "US"
	}
	if rescaleType != "" {
		elements = append(elements, derivedio.LO(tagRescaleType, rescaleType))
	}
	if opts.Window != nil && opts.Window.Width > 0 {
		elements = append(elements, derivedio.DS(tagWindowCenter, opts.Window.Center), derivedio.DS(tagWindowWidth, opts.Window.Width))
	}
	if opts.IOD == SecondaryCapture {
		elements = append(elements,
			derivedio.CS(tagConversionType, "WSD"),
			core.NewRawElement(tagPatientOrientation, core.VRCS, nil),
		)
	}
	if opts.hasImagePlane() {
		row, column := geometry.RowDirection, geometry.ColumnDirection
		elements = append(elements,
			derivedio.DS(tagImagePosition, slice.Position[0], slice.Position[1], slice.Position[2]),
			derivedio.DS(tagImageOrientation, row[0], row[1], row[2], column[0], column[1], column[2]),
			derivedio.DS(tagPixelSpacing, geometry.PixelSpacing[0], geometry.PixelSpacing[1]),
		)
		if geometry.SliceThickness > 0 {
			elements = append(elements, derivedio.DS(tagSliceThickness, geometry.SliceThickness))
		} else {
			elements = append(elements, core.NewRawElement(tagSliceThickness, core.VRDS, nil))
		}
		if frameUID != "" {
			elements = append(elements, derivedio.UI(tagFrameOfReferenceUID, frameUID))
		}
	}
	if studyUID != "" {
		elements = append(elements, derivedio.UI(tagStudyInstanceUID, studyUID))
	}
	dataset := derivedio.Object(elements...)
	if err := copyAcquisition(dataset, opts, modality); err != nil {
		return nil, err
	}
	modules := object.InheritAll
	if !opts.hasImagePlane() {
		modules = object.InheritWithoutFrameOfReference
	}
	if err := opts.Common.Apply(dataset, modules); err != nil {
		return nil, err
	}
	step := opts.Derivation
	if len(slice.Sources) > 0 {
		step.Sources = append(append([]derivation.Source(nil), step.Sources...), slice.Sources...)
	}
	if step.Description != "" || len(step.Codes) > 0 || len(step.Sources) > 0 {
		if err := derivation.Apply(dataset, step); err != nil {
			return nil, err
		}
	}
	if len(opts.ReferencedInstances) > 0 {
		items := make([]core.DataSet, 0, len(opts.ReferencedInstances))
		for _, reference := range opts.ReferencedInstances {
			if reference.Purpose == nil {
				return nil, fmt.Errorf("%w: referenced instance %s needs a purpose of reference", ErrInvalid, reference.SOPInstanceUID)
			}
			item, err := reference.Item()
			if err != nil {
				return nil, err
			}
			items = append(items, item)
		}
		dataset.Put(derivedio.Seq(tagReferencedInstanceSequence, items...))
	}
	return derivedio.File(sopClass, sopUID, dataset)
}

// copyAcquisition copies the acquisition attributes of a source of the same
// modality, writing the Type 1 and Type 2 ones the IOD requires when the
// source has none.
func copyAcquisition(dataset *object.Object, opts Options, modality string) error {
	source := opts.Common.Source
	var tags []core.Tag
	switch opts.IOD {
	case CT:
		tags = ctAcquisition
	case MR:
		tags = mrAcquisition
	}
	sameModality := false
	if source != nil {
		m, _ := source.LookupString(tagModality)
		sameModality = strings.TrimSpace(m) == modality
	}
	if source != nil {
		// Laterality and Body Part Examined describe the anatomy, whatever
		// the modality; the acquisition attributes only a source of the same
		// modality.
		anatomy := []core.Tag{tagLaterality, tagBodyPartExamined}
		for i, tag := range append(anatomy, tags...) {
			if i >= len(anatomy) && !sameModality {
				continue
			}
			if values, err := source.LookupStrings(tag); err == nil {
				element, _ := source.Get(tag)
				dataset.Put(core.Element{Header: core.ElementHeader{Tag: tag, VR: element.VR()}, Value: core.StringValue(object.TrimNULPadding(values))})
			}
		}
	}
	// Patient Position (Type 2C for CT and MR) is written empty when
	// unknown. Laterality is not: it must be absent for an unpaired body
	// part, so it is only copied.
	defaults := map[core.Tag]core.Element{}
	if opts.IOD != SecondaryCapture {
		defaults[tagPatientPosition] = core.NewRawElement(tagPatientPosition, core.VRCS, nil)
	}
	switch opts.IOD {
	case CT:
		defaults[core.NewTag(0x0018, 0x0060)] = core.NewRawElement(core.NewTag(0x0018, 0x0060), core.VRDS, nil) // KVP
		defaults[core.NewTag(0x0020, 0x0012)] = core.NewRawElement(core.NewTag(0x0020, 0x0012), core.VRIS, nil) // Acquisition Number
	case MR:
		defaults[core.NewTag(0x0018, 0x0020)] = derivedio.CS(core.NewTag(0x0018, 0x0020), "RM")   // Scanning Sequence: research mode
		defaults[core.NewTag(0x0018, 0x0021)] = derivedio.CS(core.NewTag(0x0018, 0x0021), "NONE") // Sequence Variant
		for _, t := range []struct {
			tag core.Tag
			vr  core.VR
		}{{core.NewTag(0x0018, 0x0022), core.VRCS}, {core.NewTag(0x0018, 0x0023), core.VRCS}, {core.NewTag(0x0018, 0x0080), core.VRDS},
			{core.NewTag(0x0018, 0x0081), core.VRDS}, {core.NewTag(0x0018, 0x0091), core.VRIS}} {
			defaults[t.tag] = core.NewRawElement(t.tag, t.vr, nil)
		}
	}
	for tag, element := range defaults {
		if !dataset.Has(tag) {
			dataset.Put(element)
		}
	}
	return nil
}

func putUint16(b []byte, v uint16) { b[0], b[1] = byte(v), byte(v>>8) }

func newUID() (string, error) {
	return core.NewUID()
}
