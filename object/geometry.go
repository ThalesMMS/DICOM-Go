package object

import (
	"math"

	"github.com/ThalesMMS/dicom-go/core"
)

// ImageOrientationTolerance bounds how far Image Orientation (Patient) may
// depart from orthonormal cosines (PS3.3 C.7.6.2.1.1) before geometry refuses
// it: the deviation of each cosine's length from 1 and the absolute dot
// product of the two, at most 1e-3. That is GDCM's DirectionCosines::IsValid
// bound, which ITK's reader and writer apply, and it admits cosines written
// with four or more decimals. Render, the NIfTI exporter and the classic
// multi-frame converter share it; within it, the row is normalized and the
// column made orthogonal to it.
const ImageOrientationTolerance = 1e-3

// ImageOrientationDeviation is the largest of |‖row‖−1|, |‖column‖−1| and
// |row·column| of the six Image Orientation (Patient) values; ok is false
// unless there are six finite values.
func ImageOrientationDeviation(iop []float64) (deviation float64, ok bool) {
	if len(iop) < 6 {
		return 0, false
	}
	for _, value := range iop[:6] {
		if math.IsNaN(value) || math.IsInf(value, 0) {
			return 0, false
		}
	}
	row := math.Sqrt(iop[0]*iop[0] + iop[1]*iop[1] + iop[2]*iop[2])
	column := math.Sqrt(iop[3]*iop[3] + iop[4]*iop[4] + iop[5]*iop[5])
	dot := iop[0]*iop[3] + iop[1]*iop[4] + iop[2]*iop[5]
	return math.Max(math.Abs(row-1), math.Max(math.Abs(column-1), math.Abs(dot))), true
}

// FrameGeometry is a typed accessor for the per-frame spatial geometry tags a
// volume builder needs, so callers do not hand-roll the tag reads. Missing tags
// leave the corresponding field at its zero value (and the Has* flags false).
type FrameGeometry struct {
	// ImagePositionPatient is (0020,0032): the (x,y,z) mm position of the first
	// voxel. Length 3 when present.
	ImagePositionPatient []float64
	// ImageOrientationPatient is (0020,0037): the row/column direction cosines.
	// Length 6 when present.
	ImageOrientationPatient []float64
	// PixelSpacing is (0028,0030): [rowSpacing, colSpacing] in mm, read by
	// SpacingPair (one value is isotropic). Length 2 when usable.
	PixelSpacing []float64
	// PixelSpacingValues tells how Pixel Spacing was read, and
	// PixelSpacingErr why a present one is unusable (ErrMalformedSpacing).
	// A malformed per-frame value leaves the top-level or shared one, as
	// PixelSpacingAt does, and still sets PixelSpacingErr.
	PixelSpacingValues SpacingValues
	PixelSpacingErr    error
	// SliceThickness is (0018,0050) in mm. HasSliceThickness reports presence.
	SliceThickness    float64
	HasSliceThickness bool
	// SpacingBetweenSlices is (0018,0088) in mm. HasSpacingBetweenSlices reports
	// presence.
	SpacingBetweenSlices    float64
	HasSpacingBetweenSlices bool

	// PositionSource and OrientationSource report where
	// ImagePositionPatient and ImageOrientationPatient came from (see
	// GeometrySource); they are empty when the value is absent.
	PositionSource    GeometrySource
	OrientationSource GeometrySource
	// PositionDerivation is set when ImagePositionPatient was computed from
	// a slice index and spacing rather than read (NM reconstructions and the
	// opt-in legacy multi-frame recovery); nil when it was read.
	PositionDerivation *FramePositionDerivation
	// Findings are stable, PHI-free codes explaining a position that could
	// not be derived or that needed a noted interpretation.
	Findings []FrameGeometryFinding
}

var (
	tagImagePositionPatient     = core.NewTag(0x0020, 0x0032)
	tagImageOrientationPatient  = core.NewTag(0x0020, 0x0037)
	tagPixelSpacing             = core.NewTag(0x0028, 0x0030)
	tagSliceThickness           = core.NewTag(0x0018, 0x0050)
	tagSpacingBetweenSlices     = core.NewTag(0x0018, 0x0088)
	tagSharedFunctionalGroups   = core.NewTag(0x5200, 0x9229)
	tagPerFrameFunctionalGroups = core.NewTag(0x5200, 0x9230)
	tagPlanePositionSequence    = core.NewTag(0x0020, 0x9113)
	tagPlaneOrientationSequence = core.NewTag(0x0020, 0x9116)
	tagPixelMeasuresSequence    = core.NewTag(0x0028, 0x9110)
)

// FrameGeometry reads the spatial geometry tags from the object's dataset.
func (o *Object) FrameGeometry() FrameGeometry {
	if o == nil {
		return FrameGeometry{}
	}
	return buildFrameGeometry(o)
}

// FrameGeometry reads the spatial geometry tags from the file (routing to the
// dataset). It is the convenience the viewer's volume model consumes.
func (f *File) FrameGeometry() FrameGeometry {
	if f == nil {
		return FrameGeometry{}
	}
	return f.Dataset.FrameGeometry()
}

// FrameGeometryAt reads the spatial geometry for a zero-based multi-frame
// frame. Top-level Image Plane attributes are used as a compatibility fallback,
// then Shared Functional Groups are applied, followed by the matching item in
// Per-Frame Functional Groups. An invalid frame index simply omits the
// per-frame overlay while retaining top-level and shared geometry.
//
// A frame without a Plane Position (Patient) functional group that belongs to
// an NM tomographic reconstruction is then placed as PS3.3 C.8.4 defines it:
// orientation and first-frame position from the Detector Information
// Sequence (0054,0022), slice number from Slice Vector (0054,0080), and
// Spacing Between Slices (0018,0088) as the signed step along the slice
// normal. PositionSource, OrientationSource, PositionDerivation and Findings
// report the provenance. It is FrameGeometryAtWithOptions with no recovery.
func (o *Object) FrameGeometryAt(frameIndex int) FrameGeometry {
	return o.FrameGeometryAtWithOptions(frameIndex, FrameGeometryOptions{})
}

// FrameGeometryAtWithOptions is FrameGeometryAt with the opt-in recoveries
// of options; every value they produce is marked Recovered in
// PositionDerivation and named in Findings.
func (o *Object) FrameGeometryAtWithOptions(frameIndex int, options FrameGeometryOptions) FrameGeometry {
	geometry := o.FrameGeometry()
	if o == nil {
		return geometry
	}
	for _, macro := range []core.Tag{tagPlanePositionSequence, tagPlaneOrientationSequence, tagPixelMeasuresSequence} {
		if item, source, _ := o.FunctionalGroupItem(frameIndex, macro); item != nil {
			overlayFrameGeometry(&geometry, item.FrameGeometry(), functionalGroupGeometrySource(source))
		}
	}
	if geometry.PositionSource != GeometrySourceSharedFunctionalGroups &&
		geometry.PositionSource != GeometrySourcePerFrameFunctionalGroups {
		deriveFramePosition(o, frameIndex, &geometry, options)
	}
	return geometry
}

// FrameGeometryAt reads spatial geometry for a zero-based frame from the file's
// dataset. Like Object.FrameGeometryAt, it starts with top-level Image Plane
// attributes as a compatibility fallback, overlays Shared Functional Groups,
// then overlays the matching Per-Frame Functional Groups item when the index is
// valid, and places NM reconstructed slices.
func (f *File) FrameGeometryAt(frameIndex int) FrameGeometry {
	if f == nil || f.Dataset == nil {
		return FrameGeometry{}
	}
	return f.Dataset.FrameGeometryAt(frameIndex)
}

// FrameGeometryAtWithOptions is Object.FrameGeometryAtWithOptions on the
// file's dataset.
func (f *File) FrameGeometryAtWithOptions(frameIndex int, options FrameGeometryOptions) FrameGeometry {
	if f == nil || f.Dataset == nil {
		return FrameGeometry{}
	}
	return f.Dataset.FrameGeometryAtWithOptions(frameIndex, options)
}

func functionalGroupGeometrySource(source FunctionalGroupSource) GeometrySource {
	if source == FunctionalGroupPerFrame {
		return GeometrySourcePerFrameFunctionalGroups
	}
	return GeometrySourceSharedFunctionalGroups
}

func overlayFrameGeometry(dst *FrameGeometry, src FrameGeometry, source GeometrySource) {
	if len(src.ImagePositionPatient) >= 3 {
		dst.ImagePositionPatient = src.ImagePositionPatient
		dst.PositionSource = source
	}
	if len(src.ImageOrientationPatient) >= 6 {
		dst.ImageOrientationPatient = src.ImageOrientationPatient
		dst.OrientationSource = source
	}
	if len(src.PixelSpacing) >= 2 {
		dst.PixelSpacing = src.PixelSpacing
		dst.PixelSpacingValues, dst.PixelSpacingErr = src.PixelSpacingValues, nil
	} else if src.PixelSpacingErr != nil {
		dst.PixelSpacingErr = src.PixelSpacingErr
	}
	if src.HasSliceThickness {
		dst.SliceThickness = src.SliceThickness
		dst.HasSliceThickness = true
	}
	if src.HasSpacingBetweenSlices {
		dst.SpacingBetweenSlices = src.SpacingBetweenSlices
		dst.HasSpacingBetweenSlices = true
	}
}

func firstSequenceItem(obj *Object, tag core.Tag) *Object {
	return obj.sequenceItemAt(tag, 0)
}

func buildFrameGeometry(o *Object) FrameGeometry {
	getFloats, getFloat := o.GetFloats, o.GetFloat
	g := FrameGeometry{}
	if v, err := getFloats(tagImagePositionPatient); err == nil {
		g.ImagePositionPatient = v
		g.PositionSource = GeometrySourceImagePlane
	}
	if v, err := getFloats(tagImageOrientationPatient); err == nil {
		g.ImageOrientationPatient = v
		g.OrientationSource = GeometrySourceImagePlane
	}
	if row, column, values, err := o.SpacingPair(tagPixelSpacing); err != nil {
		g.PixelSpacingErr = err
	} else if values != SpacingValuesAbsent {
		g.PixelSpacing, g.PixelSpacingValues = []float64{row, column}, values
	}
	if v, err := getFloat(tagSliceThickness); err == nil {
		g.SliceThickness = v
		g.HasSliceThickness = true
	}
	if v, err := getFloat(tagSpacingBetweenSlices); err == nil {
		g.SpacingBetweenSlices = v
		g.HasSpacingBetweenSlices = true
	}
	return g
}
