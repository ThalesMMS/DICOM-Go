// Package nifti exports scalar DICOM image series as geometry-aware NIfTI-1
// volumes and reads NIfTI-1/NIfTI-2 volumes back into DICOM LPS geometry. It
// deliberately excludes presentation transforms: VOI, windowing, MONOCHROME1
// inversion, GSPS, and display state never alter quantitative voxel values.
//
// The reader (Read, ReadPair, ReadFile) resolves the qform/sform precedence
// documented in docs/NIFTI_IMPORT.md, applies scl_slope/scl_inter on access,
// preserves float NaN, enforces ReadLimits before reading voxels, and can
// convert an integer labelmap lying on a source series grid into a DICOM
// Segmentation (Volume.SEGDocument). A decoded Volume is immutable and safe
// for concurrent readers.
package nifti

import (
	"errors"
	"fmt"
	"math"
	"strings"

	"github.com/ThalesMMS/dicom-go/object"
	"github.com/ThalesMMS/dicom-go/render"
)

const (
	DatatypeUint8   int16 = 2
	DatatypeInt16   int16 = 4
	DatatypeInt32   int16 = 8
	DatatypeFloat32 int16 = 16
	DatatypeFloat64 int16 = 64
	DatatypeInt8    int16 = 256
	DatatypeUint16  int16 = 512
	DatatypeUint32  int16 = 768
)

var (
	ErrInvalidSource       = errors.New("dicom/nifti: invalid source")
	ErrInvalidGeometry     = errors.New("dicom/nifti: invalid or unsupported geometry")
	ErrMixedIdentity       = errors.New("dicom/nifti: mixed series or frame of reference")
	ErrUnsupportedPixels   = errors.New("dicom/nifti: unsupported pixel representation")
	ErrUnsupportedScaling  = errors.New("dicom/nifti: unsupported scaling")
	ErrUnsupportedTemporal = errors.New("dicom/nifti: unsupported temporal dimension")
	ErrLimitExceeded       = errors.New("dicom/nifti: resource limit exceeded")
	ErrNIfTI2Required      = errors.New("dicom/nifti: NIfTI-2 required")
)

// ErrorCode is a stable, PHI-free failure classification.
type ErrorCode string

const (
	CodeSource              ErrorCode = "source"
	CodeIdentity            ErrorCode = "identity"
	CodeGeometry            ErrorCode = "geometry"
	CodeTemporal            ErrorCode = "temporal"
	CodePixels              ErrorCode = "pixels"
	CodeScaling             ErrorCode = "scaling"
	CodeLimit               ErrorCode = "limit"
	CodeNIfTI2              ErrorCode = "nifti2-required"
	CodeWrite               ErrorCode = "write"
	CodeCanceled            ErrorCode = "canceled"
	CodeQuadruped           ErrorCode = "quadruped-coordinate-system"
	CodeMultipleStacks      ErrorCode = "multiple-stacks"
	CodeIrregularTime       ErrorCode = "irregular-time"
	CodeMissingTiming       ErrorCode = "missing-time-spacing"
	CodeSingleSlice         ErrorCode = "single-slice"
	CodeUnsupportedModality ErrorCode = "unsupported-modality-transform"
)

// ExportError identifies a source/frame ordinal without echoing DICOM values,
// paths, UIDs, or arbitrary decoder text in Error(). The wrapped error remains
// available through errors.Is/errors.As for trusted callers.
type ExportError struct {
	Code        ErrorCode
	SourceIndex int // zero-based; -1 when not source-specific
	FrameIndex  int // zero-based; -1 when not frame-specific
	// GeometryIssue is a stable, non-PHI render guardrail reason when Code is
	// CodeGeometry. It is empty for non-geometric failures.
	GeometryIssue render.GeometryIssue
	Err           error
}

func (e *ExportError) Error() string {
	if e == nil {
		return "dicom/nifti: export failed"
	}
	location := ""
	if e.SourceIndex >= 0 {
		location = fmt.Sprintf(" at source %d", e.SourceIndex+1)
		if e.FrameIndex >= 0 {
			location += fmt.Sprintf(" frame %d", e.FrameIndex+1)
		}
	}
	code := strings.TrimSpace(string(e.Code))
	if code == "" {
		code = "export"
	}
	if e.GeometryIssue != render.GeometryIssueNone {
		code += " (" + string(e.GeometryIssue) + ")"
	}
	return "dicom/nifti: " + code + location
}

func (e *ExportError) Unwrap() error {
	if e == nil {
		return nil
	}
	return e.Err
}

type Compression uint8

const (
	CompressionNone Compression = iota
	CompressionGZIP
)

func (c Compression) String() string {
	if c == CompressionGZIP {
		return "gzip"
	}
	return "none"
}

type ScalingPolicy uint8

const (
	// ScalingPreserveUniform writes normalized stored integers and preserves one
	// uniform linear transform in scl_slope/scl_inter.
	ScalingPreserveUniform ScalingPolicy = iota
	// ScalingApplyFloat32 applies every frame's slope/intercept and writes F32.
	ScalingApplyFloat32
	// ScalingApplyFloat64 applies every frame's slope/intercept and writes F64.
	ScalingApplyFloat64
)

func (p ScalingPolicy) String() string {
	switch p {
	case ScalingApplyFloat32:
		return "apply-float32"
	case ScalingApplyFloat64:
		return "apply-float64"
	default:
		return "preserve-uniform"
	}
}

type GeometryPolicy uint8

const (
	GeometryStrict GeometryPolicy = iota
	// GeometryResampleLinear permits only geometry classified as regularizable
	// by render. Unsupported/mixed geometry still fails closed.
	GeometryResampleLinear
	// GeometryResampleCubicBSpline permits the same geometry as
	// GeometryResampleLinear and resamples it with the cubic B-spline
	// interpolant of the source voxels (#1190, render.RegularGridCubicBSpline):
	// exact at the voxels and smoother between them than trilinear, at the
	// cost of a coefficient prefilter of the whole source volume. Regular
	// geometry is written unresampled under either policy.
	GeometryResampleCubicBSpline
)

// resamples reports whether the policy permits resampling.
func (p GeometryPolicy) resamples() bool {
	return p == GeometryResampleLinear || p == GeometryResampleCubicBSpline
}

// interpolation is the render kernel and the Report.Interpolation name of
// a resampling policy.
func (p GeometryPolicy) interpolation() (render.RegularGridInterpolation, string) {
	if p == GeometryResampleCubicBSpline {
		return render.RegularGridCubicBSpline, "cubic-bspline"
	}
	return render.RegularGridLinear, "linear"
}

// SingleSlicePolicy selects whether one frame exports as a 2D (nz = 1)
// NIfTI image. Its through-plane pixdim[3] is object.ResolveSliceSpacing of
// the frame: Spacing Between Slices, then Slice Thickness, then 1 mm, with
// the source reported in Report.SliceSpacingSource and the sidecar.
type SingleSlicePolicy uint8

const (
	// SingleSliceReject keeps the fail-closed default: one frame is
	// CodeSingleSlice.
	SingleSliceReject SingleSlicePolicy = iota
	// SingleSlicePatient exports one frame that has Image Position,
	// Image Orientation (Patient) and Pixel Spacing; the third affine column
	// is the slice normal scaled by the resolved spacing, with scanner
	// qform/sform codes.
	SingleSlicePatient
	// SingleSliceImageGrid additionally accepts one frame without patient
	// geometry (DX, CR, US, secondary capture): it writes qform_code =
	// sform_code = 0 (NIfTI method 1), pixdim from object.PixelSpacingAt
	// (1 mm when none resolves) and the resolved slice spacing. Such a grid
	// claims no patient position or orientation; the reader needs
	// ReadOptions.AllowMissingTransform.
	SingleSliceImageGrid
)

type Limits struct {
	MaxInstances         int
	MaxFrames            int
	MaxVoxels            uint64
	MaxUncompressedBytes uint64
	MaxSpoolBytes        uint64
	MaxResampleVoxels    uint64
	// MaxInMemorySourceBytes bounds the fallback used by sources/codecs that
	// cannot stream one decoded frame at a time. Native path sources use a
	// FrameSink and remain O(one frame); oversized compressed sources fail
	// explicitly instead of materializing without a ceiling.
	MaxInMemorySourceBytes uint64
}

func DefaultLimits() Limits {
	return Limits{
		MaxInstances:           100_000,
		MaxFrames:              1_000_000,
		MaxVoxels:              1 << 32,
		MaxUncompressedBytes:   16 << 30,
		MaxSpoolBytes:          16 << 30,
		MaxResampleVoxels:      256 << 20,
		MaxInMemorySourceBytes: 512 << 20,
	}
}

// StrictGeometryTolerances are intentionally tighter than the viewer's
// display-oriented defaults. NIfTI encodes one regular affine grid.
func StrictGeometryTolerances() render.GeometryTolerances {
	return render.GeometryTolerances{
		SpacingRel:      0.001,
		SpacingAbs:      0.001,
		OrientationCos:  math.Cos(0.1 * math.Pi / 180),
		TiltCos:         math.Cos(0.1 * math.Pi / 180),
		PositionAbs:     1e-6,
		ShearAbs:        math.Tan(0.1 * math.Pi / 180),
		AffineRoundTrip: 1e-9,
	}
}

type Options struct {
	Compression Compression
	Scaling     ScalingPolicy
	Geometry    GeometryPolicy
	// TemporalSpacingSeconds explicitly supplies uniform spacing for a resolved
	// temporal dimension whose DICOM metadata identifies order but not timing.
	// It is ignored for 3D exports and must be finite and positive when used.
	TemporalSpacingSeconds float64
	// SingleSlice opts in to exporting one frame (see SingleSlicePolicy).
	SingleSlice SingleSlicePolicy
	Tolerances  render.GeometryTolerances
	Limits      Limits
	TempDir     string
}

func DefaultOptions() Options {
	return Options{
		Tolerances: StrictGeometryTolerances(),
		Limits:     DefaultLimits(),
	}
}

// Stable, PHI-free warning codes reported in Report.Warnings and the sidecar
// when a Parametric Map export carries voxels without a quantitative value.
const (
	// WarningParametricPaddingAsNaN: Pixel Padding voxels were written as
	// NaN, because NIfTI has no padding attribute.
	WarningParametricPaddingAsNaN = "parametric-map-padding-written-as-nan"
	// WarningParametricNaNPreserved: stored NaN voxels were written as NaN.
	WarningParametricNaNPreserved = "parametric-map-nan-preserved"
	// WarningParametricInfinitePreserved: stored ±Inf voxels were written as
	// their linearly mapped infinity.
	WarningParametricInfinitePreserved = "parametric-map-infinite-preserved"
	// WarningParametricOutsideMappingAsNaN: stored voxels outside the
	// applicable Real World Value Mapping domains were written as NaN.
	WarningParametricOutsideMappingAsNaN = "parametric-map-outside-mapping-written-as-nan"
	// WarningParametricMappingOverflowAsNaN: voxels whose calibrated
	// values overflowed were written as NaN.
	WarningParametricMappingOverflowAsNaN = "parametric-map-mapping-overflow-written-as-nan"
)

// Stable, PHI-free warning codes of a single-slice export.
const (
	// WarningSliceSpacingFromThickness: no Spacing Between Slices; the
	// through-plane pixdim is Slice Thickness.
	WarningSliceSpacingFromThickness = "slice-spacing-from-slice-thickness"
	// WarningSliceSpacingDefault: neither Spacing Between Slices nor Slice
	// Thickness; the through-plane pixdim is 1 mm.
	WarningSliceSpacingDefault = "slice-spacing-default-1mm"
	// WarningNoPatientGeometry: SingleSliceImageGrid wrote a frame without
	// Image Position/Orientation (Patient) with no spatial transform.
	WarningNoPatientGeometry = "no-patient-geometry"
	// WarningPixelSpacingDefault: the image-grid frame has no resolvable
	// pixel spacing; the in-plane pixdim is 1 mm.
	WarningPixelSpacingDefault = "pixel-spacing-default-1mm"
)

// UnmeasuredVoxels counts exported Parametric Map voxels that carry no
// quantitative value. Padding, NaN, OutsideMapping and MappingOverflow voxels
// are written as NaN. Infinite voxels are written as ±Inf. Read preserves both
// floating-point representations.
type UnmeasuredVoxels struct {
	Padding         int64
	NaN             int64
	Infinite        int64
	OutsideMapping  int64
	MappingOverflow int64
}

type Report struct {
	Dimensions       [4]int
	Datatype         int16
	BitPix           int16
	VoxelOffset      int64
	IndexToRAS       render.GeometryAffine
	ScalingPolicy    ScalingPolicy
	ScalingSlope     float64
	ScalingIntercept float64
	Compression      Compression
	InputReordered   bool
	Resampled        bool
	Interpolation    string
	Warnings         []string
	BytesWritten     int64
	Sidecar          Sidecar
	// UnmeasuredVoxels is non-zero only for Parametric Map exports whose
	// payload holds unmeasured voxels. Warnings names each kind.
	UnmeasuredVoxels UnmeasuredVoxels
	// SingleSlice reports a one-frame export (Options.SingleSlice), and
	// SliceSpacingSource where its through-plane pixdim came from; a volume
	// reports object.SliceSpacingFromPositions.
	SingleSlice        bool
	SliceSpacingSource object.SliceSpacingSource
	// NoSpatialTransform reports an image-grid export: qform_code and
	// sform_code are 0 and IndexToRAS is only diag(pixdim).
	// PixelSpacingSemantic tells what its in-plane pixdim measures
	// (object.PixelSpacingAt), empty when it defaulted to 1 mm.
	NoSpatialTransform   bool
	PixelSpacingSemantic object.PixelSpacingSemantic
}

func normalizedOptions(options Options) (Options, error) {
	defaults := DefaultOptions()
	if options.Compression > CompressionGZIP {
		return Options{}, &ExportError{Code: CodeWrite, SourceIndex: -1, FrameIndex: -1, Err: ErrInvalidSource}
	}
	if options.Scaling > ScalingApplyFloat64 {
		return Options{}, &ExportError{Code: CodeScaling, SourceIndex: -1, FrameIndex: -1, Err: ErrUnsupportedScaling}
	}
	if options.Geometry > GeometryResampleCubicBSpline || options.SingleSlice > SingleSliceImageGrid {
		return Options{}, &ExportError{Code: CodeGeometry, SourceIndex: -1, FrameIndex: -1, Err: ErrInvalidGeometry}
	}
	if options.Tolerances == (render.GeometryTolerances{}) {
		options.Tolerances = defaults.Tolerances
	}
	if options.Limits.MaxInstances == 0 {
		options.Limits.MaxInstances = defaults.Limits.MaxInstances
	}
	if options.Limits.MaxFrames == 0 {
		options.Limits.MaxFrames = defaults.Limits.MaxFrames
	}
	if options.Limits.MaxVoxels == 0 {
		options.Limits.MaxVoxels = defaults.Limits.MaxVoxels
	}
	if options.Limits.MaxUncompressedBytes == 0 {
		options.Limits.MaxUncompressedBytes = defaults.Limits.MaxUncompressedBytes
	}
	if options.Limits.MaxSpoolBytes == 0 {
		options.Limits.MaxSpoolBytes = defaults.Limits.MaxSpoolBytes
	}
	if options.Limits.MaxResampleVoxels == 0 {
		options.Limits.MaxResampleVoxels = defaults.Limits.MaxResampleVoxels
	}
	if options.Limits.MaxInMemorySourceBytes == 0 {
		options.Limits.MaxInMemorySourceBytes = defaults.Limits.MaxInMemorySourceBytes
	}
	if options.Limits.MaxInstances < 0 || options.Limits.MaxFrames < 0 {
		return Options{}, &ExportError{Code: CodeLimit, SourceIndex: -1, FrameIndex: -1, Err: ErrLimitExceeded}
	}
	return options, nil
}
