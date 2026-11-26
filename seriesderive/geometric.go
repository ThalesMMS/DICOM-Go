package seriesderive

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/ThalesMMS/dicom-go/core"
	"github.com/ThalesMMS/dicom-go/derivation"
	"github.com/ThalesMMS/dicom-go/internal/derivedio"
	"github.com/ThalesMMS/dicom-go/object"
	"github.com/ThalesMMS/dicom-go/pixeldata"
	"github.com/ThalesMMS/dicom-go/validation/iod"
)

var (
	ErrGeometricProfile    = errors.New("dicom/seriesderive: unsupported geometric image profile")
	ErrGeometricOptions    = errors.New("dicom/seriesderive: invalid geometric options")
	ErrGeometricLimit      = errors.New("dicom/seriesderive: geometric resource limit exceeded")
	ErrGeometricValidation = errors.New("dicom/seriesderive: geometric image validation failed")
)

// Interpolation selects resampling in stored sample units, before any modality
// transform, VOI or MONOCHROME1 inversion. Neither method provides antialiasing.
type Interpolation uint8

const (
	Nearest Interpolation = iota
	Bilinear
)

// CropRegion is an integer rectangle in the source's zero-based pixel grid.
// It must lie entirely inside the image; clipping outside the image is refused.
type CropRegion struct {
	Column, Row, Width, Height int
}

// GeometricLimits bounds a single operation. Zero fields select finite defaults.
// Clone bounds apply to both Part 10 round trips, including nested metadata.
// They bound payloads and structure, not process RSS or a batch of operations.
type GeometricLimits struct {
	MaxInputPixels, MaxOutputPixels int64
	Clone                           object.CloneFileOptions
}

// DefaultGeometricLimits permits 64 Mi pixels in each grid and 256 MiB in each
// encoded file. Numeric limits are checked before cloning and pixel allocation.
func DefaultGeometricLimits() GeometricLimits {
	c := object.DefaultCloneFileOptions()
	c.MaxElementBytes = 256 << 20
	c.MaxPixelDataBytes = 128 << 20
	c.MaxTotalBytes = 256 << 20
	return GeometricLimits{MaxInputPixels: 64 << 20, MaxOutputPixels: 64 << 20, Clone: c}
}

// GeometricOptions composes crop, resize, flips, then clockwise rotation.
// Rows/Columns describe the grid before rotation. Both zero retain the crop
// size; one zero derives that axis from the physical aspect ratio (rounded to
// the nearest positive integer). Two nonzero axes allow anisotropic spacing.
// Resize preserves the crop's physical pixel edges, including singleton axes.
type GeometricOptions struct {
	Crop                         *CropRegion
	Rows, Columns                int
	Interpolation                Interpolation
	FlipHorizontal, FlipVertical bool
	RotateClockwise              int // 0, 90, 180 or 270 degrees
	SeriesInstanceUID            string
	SOPInstanceUID               string
	// UIDGenerator is called for each unspecified output UID after admission
	// and source validation. Nil uses core.NewUID. It must return unique UIDs.
	UIDGenerator          func() (string, error)
	SeriesDescription     string
	DerivationDescription string
	Limits                GeometricLimits
}

// GeometricGeometry follows the DICOM Image Plane convention: position is the
// first pixel's centre; orientation contains the row then column direction;
// spacing is distance between rows then between columns, in millimetres.
type GeometricGeometry struct {
	Position    [3]float64
	Orientation [6]float64
	Spacing     [2]float64
}

// GeometricReport is returned only with a complete, independent output. The
// inverse affine maps output (column,row,1) to continuous source (column,row).
// IOD reports retain informational/manual-condition findings of the scoped
// library validator; they are not a claim of validation of every IOD condition.
type GeometricReport struct {
	SourceRows, SourceColumns         int
	Rows, Columns                     int
	Crop                              CropRegion
	Interpolation                     Interpolation
	Source, Output                    GeometricGeometry
	SourceFromOutput                  [2][3]float64
	SeriesInstanceUID, SOPInstanceUID string
	InputIOD, OutputIOD               iod.Report
}

// TransformImage derives a classic, single-frame CT/MR image with native 16-bit
// monochrome pixels. It preserves the native transfer syntax, Study and Frame
// of Reference, and creates new Series/SOP identities with source provenance.
// It validates the admitted input and final Part 10 output with strict VR/VM
// and the CT/MR IOD validator. Source data is never modified or retained.
// On every error both the file and report are nil. The caller owns the successful
// result and must Close it. The source must remain open and immutable during
// this call. Cancellation is checked between round trips and during sampling;
// the existing contextless clone and IOD routines cannot be interrupted inside.
func TransformImage(ctx context.Context, src *object.File, opts GeometricOptions) (*object.File, *GeometricReport, error) {
	if ctx == nil {
		return nil, nil, fmt.Errorf("%w: nil context", ErrGeometricOptions)
	}
	if err := ctx.Err(); err != nil {
		return nil, nil, err
	}
	limits, err := geometricLimits(opts.Limits)
	if err != nil {
		return nil, nil, err
	}
	if err := geometricOptions(opts); err != nil {
		return nil, nil, err
	}
	profile, err := geometricProfile(src, limits)
	if err != nil {
		return nil, nil, err
	}
	grid, err := geometricGrid(profile, opts, limits)
	if err != nil {
		return nil, nil, err
	}
	// Validate the original representation too: the ordinary writer rebuilds
	// file meta, so validating only its round trip could conceal a mismatch or
	// malformed encoded length/padding in the supplied source.
	inputIOD, err := validateGeometricFile(ctx, src, limits)
	if err != nil {
		return nil, nil, err
	}
	// CloneFileWithOptions admits the complete nested structure before its
	// streaming round trip. Pixels, sequences, text and deferred values become
	// independent of the source without a whole-file serialization buffer.
	work, err := object.CloneFileWithOptions(src, limits.Clone)
	if err != nil {
		return nil, nil, fmt.Errorf("%w: source clone: %w", ErrGeometricLimit, err)
	}
	defer work.Close()
	if err := ctx.Err(); err != nil {
		return nil, nil, err
	}
	_, err = validateGeometricFile(ctx, work, limits)
	if err != nil {
		return nil, nil, err
	}
	// Recheck the reparsed representation before interpreting pixels. Admission
	// must hold for the clone, including data materialized from deferred values.
	if _, err = geometricProfile(work, limits); err != nil {
		return nil, nil, err
	}
	frames, err := pixeldata.ExtractNativeFramesView(work.Dataset)
	if err != nil || len(frames.Data) != 1 || len(frames.Data[0]) != profile.rows*profile.columns*2 {
		return nil, nil, fmt.Errorf("%w: native frame layout", ErrGeometricProfile)
	}
	format := frames.Metadata.SampleFormat(work.Dataset.ValueByteOrder())
	padding, err := pixeldata.PixelPaddingFromObject(work.Dataset, format)
	if err != nil || work.Dataset.Has(core.NewTag(0x0028, 0x0121)) && padding == nil {
		return nil, nil, fmt.Errorf("%w: pixel padding", ErrGeometricProfile)
	}
	if padding != nil && opts.Interpolation == Bilinear {
		return nil, nil, fmt.Errorf("%w: bilinear interpolation with pixel padding", ErrGeometricProfile)
	}
	seriesUID, sopUID, err := geometricUIDs(ctx, work.Dataset, profile, opts)
	if err != nil {
		return nil, nil, err
	}
	if err := ctx.Err(); err != nil {
		return nil, nil, err
	}
	pixels, err := resampleGeometric(ctx, frames.Data[0], format, grid, opts.Interpolation)
	if err != nil {
		return nil, nil, err
	}
	dataset := work.Dataset
	dataset.Put(object.USElement(derivedio.TagRows, dataset.ValueByteOrder(), uint16(grid.rows)))
	dataset.Put(object.USElement(derivedio.TagColumns, dataset.ValueByteOrder(), uint16(grid.columns)))
	dataset.Put(core.Element{Header: core.ElementHeader{Tag: core.TagPixelData, VR: core.VROW}, Value: core.RawValue(pixels)})
	dataset.Put(derivedio.DS(tagGeometricPosition, grid.geometry.Position[:]...))
	dataset.Put(derivedio.DS(tagGeometricOrientation, grid.geometry.Orientation[:]...))
	dataset.Put(derivedio.DS(tagGeometricSpacing, grid.geometry.Spacing[:]...))
	dataset.Put(derivedio.UI(derivedio.TagSeriesInstanceUID, seriesUID))
	dataset.Put(derivedio.UI(derivedio.TagSOPInstanceUID, sopUID))
	dataset.Put(derivedio.Strings(tagImageType, core.VRCS, derivedImageType(dataset)))
	clearGeometricGridMetadata(dataset)
	if opts.SeriesDescription != "" {
		dataset.Put(derivedio.LO(derivedio.TagSeriesDescr, opts.SeriesDescription))
	}
	description := fmt.Sprintf("Geometric derivation: crop=%d,%d,%d,%d; resize=%dx%d; interpolation=%s; flip=%t,%t; clockwise=%d; pixel-edge FOV",
		grid.crop.Column, grid.crop.Row, grid.crop.Width, grid.crop.Height, grid.resizeColumns, grid.resizeRows,
		interpolationName(opts.Interpolation), opts.FlipHorizontal, opts.FlipVertical, opts.RotateClockwise)
	if opts.DerivationDescription != "" {
		description += "; " + opts.DerivationDescription
	}
	if err := derivation.Apply(dataset, derivation.Derivation{
		Description: description,
		Codes:       []derivation.Code{derivation.SpatialResampling},
		Sources:     []derivation.Source{{SOPClassUID: profile.classUID, SOPInstanceUID: profile.sopUID, Purpose: &derivation.SourceImageForProcessing}},
	}); err != nil {
		return nil, nil, err
	}
	if err := work.RebuildFileMeta(); err != nil {
		return nil, nil, err
	}
	if err := ctx.Err(); err != nil {
		return nil, nil, err
	}
	// A second bounded Part 10 round trip verifies the actual encoded result,
	// resolves VRs for implicit syntax and gives callers exclusively owned data.
	out, err := object.CloneFileWithOptions(work, limits.Clone)
	if err != nil {
		return nil, nil, fmt.Errorf("%w: output clone: %w", ErrGeometricLimit, err)
	}
	outputIOD, err := validateGeometricFile(ctx, out, limits)
	if err != nil {
		_ = out.Close()
		return nil, nil, err
	}
	outputLimits := limits
	outputLimits.MaxInputPixels = limits.MaxOutputPixels
	outputProfile, err := geometricProfile(out, outputLimits)
	if err != nil {
		_ = out.Close()
		return nil, nil, err
	}
	return out, &GeometricReport{
		SourceRows: profile.rows, SourceColumns: profile.columns, Rows: grid.rows, Columns: grid.columns,
		Crop: grid.crop, Interpolation: opts.Interpolation, Source: profile.geometry, Output: outputProfile.geometry,
		SourceFromOutput: grid.inverse, SeriesInstanceUID: seriesUID, SOPInstanceUID: sopUID, InputIOD: inputIOD, OutputIOD: outputIOD,
	}, nil
}

func interpolationName(i Interpolation) string {
	if i == Bilinear {
		return "bilinear"
	}
	return "nearest"
}

func clearGeometricGridMetadata(dataset *object.Object) {
	// These describe the old stored grid or its statistics, rather than the
	// acquisition/reconstruction plane or a transform of stored sample values.
	for _, tag := range []core.Tag{
		core.NewTag(0x0088, 0x0200),                              // Icon Image Sequence
		core.NewTag(0x0018, 0x1050),                              // Spatial Resolution: no new resolution claim
		core.NewTag(0x0028, 0x0031), core.NewTag(0x0028, 0x0032), // retired zoom factor/centre
		core.NewTag(0x0028, 0x0034),                              // Pixel Aspect Ratio: physical spacing is present
		core.NewTag(0x0028, 0x0106), core.NewTag(0x0028, 0x0107), // image extrema
		core.NewTag(0x0028, 0x0108), core.NewTag(0x0028, 0x0109), // series extrema
		core.NewTag(0x0028, 0x0110), core.NewTag(0x0028, 0x0111), // retired plane extrema
		core.NewTag(0x0028, 0x9099), // retired Largest Monochrome Pixel Value
	} {
		dataset.Remove(tag)
	}
	patientOrientation := core.NewTag(0x0020, 0x0020)
	if dataset.Has(patientOrientation) {
		dataset.Put(derivedio.Strings(patientOrientation, core.VRCS, nil))
	}
}

func geometricOptions(opts GeometricOptions) error {
	if opts.Interpolation > Bilinear || opts.Rows < 0 || opts.Columns < 0 ||
		(opts.RotateClockwise != 0 && opts.RotateClockwise != 90 && opts.RotateClockwise != 180 && opts.RotateClockwise != 270) {
		return ErrGeometricOptions
	}
	if len(opts.SeriesDescription) > 64 || strings.ContainsAny(opts.SeriesDescription, "\\\x00\r\n\f") ||
		len(opts.DerivationDescription) > 768 || strings.ContainsRune(opts.DerivationDescription, '\x00') {
		return fmt.Errorf("%w: descriptions", ErrGeometricOptions)
	}
	for _, uid := range []string{opts.SeriesInstanceUID, opts.SOPInstanceUID} {
		if uid != "" && !core.IsValidUID(uid) {
			return fmt.Errorf("%w: output UID syntax", ErrGeometricOptions)
		}
	}
	return nil
}
