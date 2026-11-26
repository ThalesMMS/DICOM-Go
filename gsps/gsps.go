package gsps

import (
	"encoding/binary"
	"errors"
	"fmt"
	"math"
	"strings"
	"time"

	"github.com/ThalesMMS/dicom-go/core"
	"github.com/ThalesMMS/dicom-go/internal/derivedio"
	"github.com/ThalesMMS/dicom-go/object"
	"github.com/ThalesMMS/dicom-go/pixeldata/display"
)

const (
	GrayscaleSoftcopyPresentationStateStorage = "1.2.840.10008.5.1.4.1.1.11.1"
	PresentationLUTIdentity                   = "IDENTITY"
	PresentationLUTInverse                    = "INVERSE"

	GraphicTypePoint        = "POINT"
	GraphicTypePolyline     = "POLYLINE"
	GraphicTypeCircle       = "CIRCLE"
	GraphicTypeEllipse      = "ELLIPSE"
	GraphicTypeInterpolated = "INTERPOLATED"

	AnnotationUnitsPixel   = "PIXEL"
	AnnotationUnitsDisplay = "DISPLAY"

	PresentationSizeScaleToFit = "SCALE TO FIT"
	PresentationSizeTrueSize   = "TRUE SIZE"
	PresentationSizeMagnify    = "MAGNIFY"
)

var (
	ErrUnsupportedSOPClass = errors.New("dicom/gsps: unsupported SOP class")
	ErrUnsupportedUnits    = errors.New("dicom/gsps: unsupported annotation units")
	ErrInvalidObject       = errors.New("dicom/gsps: invalid object")
	ErrMissingReference    = errors.New("dicom/gsps: missing reference")
)

var (
	tagReferencedSeriesSequence     = core.NewTag(0x0008, 0x1115)
	tagReferencedImageSequence      = core.NewTag(0x0008, 0x1140)
	tagDisplayedAreaSelectionSeq    = core.NewTag(0x0070, 0x005A)
	tagDisplayedAreaTopLeft         = core.NewTag(0x0070, 0x0052)
	tagDisplayedAreaBottomRight     = core.NewTag(0x0070, 0x0053)
	tagPresentationSizeMode         = core.NewTag(0x0070, 0x0100)
	tagPresentationPixelSpacing     = core.NewTag(0x0070, 0x0101)
	tagPresentationPixelAspectRatio = core.NewTag(0x0070, 0x0102)
	tagPresentationPixelMagnify     = core.NewTag(0x0070, 0x0103)
	tagImageRotation                = core.NewTag(0x0070, 0x0042)
	tagImageHorizontalFlip          = core.NewTag(0x0070, 0x0041)
	tagShutterShape                 = core.NewTag(0x0018, 0x1600)
	tagShutterLeft                  = core.NewTag(0x0018, 0x1602)
	tagShutterRight                 = core.NewTag(0x0018, 0x1604)
	tagShutterUpper                 = core.NewTag(0x0018, 0x1606)
	tagShutterLower                 = core.NewTag(0x0018, 0x1608)
	tagShutterCircleCenter          = core.NewTag(0x0018, 0x1610)
	tagShutterCircleRadius          = core.NewTag(0x0018, 0x1612)
	tagShutterPolygonVertices       = core.NewTag(0x0018, 0x1620)
	tagShutterPresentationValue     = core.NewTag(0x0018, 0x1622)
	tagShutterOverlayGroup          = core.NewTag(0x0018, 0x1623)
	tagShutterPresentationColor     = core.NewTag(0x0018, 0x1624)
	tagSoftcopyVOILUTSequence       = core.NewTag(0x0028, 0x3110)
	tagWindowCenter                 = core.NewTag(0x0028, 0x1050)
	tagWindowWidth                  = core.NewTag(0x0028, 0x1051)
	tagWindowCenterWidthExplanation = core.NewTag(0x0028, 0x1055)
	tagVOILUTFunction               = core.NewTag(0x0028, 0x1056)
	tagVOILUTSequence               = core.NewTag(0x0028, 0x3010)
	tagLUTDescriptor                = core.NewTag(0x0028, 0x3002)
	tagLUTExplanation               = core.NewTag(0x0028, 0x3003)
	tagLUTData                      = core.NewTag(0x0028, 0x3006)
	tagPresentationLUTShape         = core.NewTag(0x2050, 0x0020)
	tagGraphicAnnotationSequence    = core.NewTag(0x0070, 0x0001)
	tagGraphicLayer                 = core.NewTag(0x0070, 0x0002)
	tagAnchorPointAnnotationUnits   = core.NewTag(0x0070, 0x0004)
	tagGraphicAnnotationUnits       = core.NewTag(0x0070, 0x0005)
	tagTextObjectSequence           = core.NewTag(0x0070, 0x0008)
	tagUnformattedTextValue         = core.NewTag(0x0070, 0x0006)
	tagAnchorPoint                  = core.NewTag(0x0070, 0x0014)
	tagAnchorPointVisibility        = core.NewTag(0x0070, 0x0015)
	tagGraphicObjectSequence        = core.NewTag(0x0070, 0x0009)
	tagGraphicDimensions            = core.NewTag(0x0070, 0x0020)
	tagGraphicData                  = core.NewTag(0x0070, 0x0022)
	tagGraphicType                  = core.NewTag(0x0070, 0x0023)
	tagGraphicFilled                = core.NewTag(0x0070, 0x0024)
	tagNumberOfGraphicPoints        = core.NewTag(0x0070, 0x0021)
	tagGraphicLayerSequence         = core.NewTag(0x0070, 0x0060)
	tagGraphicLayerOrder            = core.NewTag(0x0070, 0x0062)
)

type ReferencedImage struct {
	SeriesInstanceUID string
	SOPClassUID       string
	SOPInstanceUID    string
	Frames            []int
}

type DisplayedArea struct {
	// Defined distinguishes an explicitly encoded all-zero area from an absent
	// Displayed Area Selection Sequence. Non-zero coordinates imply presence for
	// backward compatibility with existing callers.
	Defined      bool
	TopLeftX     int
	TopLeftY     int
	BottomRightX int
	BottomRightY int
	// PresentationSizeMode is SCALE TO FIT, TRUE SIZE, or MAGNIFY. Empty means
	// SCALE TO FIT for backward compatibility.
	PresentationSizeMode string
	PixelSpacing         []float64
	PixelAspectRatio     []int
	MagnificationRatio   float64
	ReferencedImages     []ReferencedImage
}

type SpatialTransform struct {
	RotationDegrees int
	FlipHorizontal  bool
}

type SoftcopyVOI struct {
	WindowCenter     float64
	WindowWidth      float64
	Explanation      string
	Function         display.VOIFunction
	LUT              *display.LUT
	ReferencedImages []ReferencedImage
}

type Point2D struct {
	X float64
	Y float64
}

type GraphicAnnotation struct {
	LayerName string
	// GroupIndex identifies objects that came from the same Graphic Annotation
	// Sequence item. A zero value keeps programmatically-created annotations as
	// independent items for backward compatibility.
	GroupIndex   int
	Text         string
	Anchor       Point2D
	AnchorUnits  string
	GraphicType  string
	GraphicUnits string
	Polyline     []Point2D
	// BoundingBox places the text in a box; a text object needs a bounding
	// box, an anchor or both. With a bounding box, the anchor is written
	// only when it is visible or not at the origin (see hasAnchor).
	BoundingBox *BoundingBox
	// AnchorVisible is Anchor Point Visibility: whether the anchor is drawn
	// and joined to the text.
	AnchorVisible bool
	// Filled is Graphic Filled, allowed for closed graphics only (circles,
	// ellipses and closed polylines).
	Filled bool
	// Compound holds one geometry and its style from the Compound Graphic
	// Sequence. The alternatives are separate annotations in the same group.
	Compound *CompoundGraphic
	// CompoundGraphicID links a Compound Graphic and its text or graphic
	// alternatives within this presentation state. HasCompoundGraphicID
	// distinguishes an explicit UL zero from an absent identifier.
	CompoundGraphicID    uint32
	HasCompoundGraphicID bool
	GraphicGroupID       uint32
	HasGraphicGroupID    bool
	// ReferencedImages scopes this Graphic Annotation Sequence item to images
	// or frames. Empty means the annotation applies to every referenced image.
	ReferencedImages      []ReferencedImage
	malformedCompoundLink bool
}

type State struct {
	// Common fills the Patient, Study and equipment modules from a source
	// instance of the same patient (object.InheritCommonModules).
	Common            object.DerivedCommon
	SOPClassUID       string
	SOPInstanceUID    string
	StudyInstanceUID  string
	SeriesInstanceUID string
	// Presentation Series and Presentation State Identification. Write
	// defaults InstanceNumber to 1, ContentLabel to UNNAMED and the
	// creation date and time to now; SeriesNumber 0 writes an empty
	// Series Number and Laterality is written when set.
	SeriesNumber         int
	Laterality           string
	InstanceNumber       int
	ContentLabel         string
	ContentDescription   string
	ContentCreatorName   string
	CreationDate         string
	CreationTime         string
	ReferencedImages     []ReferencedImage
	DisplayedArea        DisplayedArea
	DisplayedAreas       []DisplayedArea
	Shutter              *display.Shutter
	SpatialTransform     SpatialTransform
	SoftcopyVOI          SoftcopyVOI
	SoftcopyVOIs         []SoftcopyVOI
	PresentationLUTShape string
	// PresentationLUT is a Presentation LUT Sequence table, written instead
	// of PresentationLUTShape, with PresentationLUTExplanation.
	PresentationLUT            *display.LUT
	PresentationLUTExplanation string
	// ModalityLUT replaces the referenced images' modality transform; nil
	// keeps theirs.
	ModalityLUT *ModalityLUT
	// GraphicLayers defines layers with their order, recommended display
	// values and descriptions. Layers only named by GraphicAnnotations or
	// OverlayActivations are added after them by Write.
	GraphicLayers      []GraphicLayer
	GraphicGroups      []GraphicGroup
	GraphicAnnotations []GraphicAnnotation
	// OverlayActivations lists the overlay groups the state displays and
	// Overlays the planes it defines itself; see ActiveOverlays.
	OverlayActivations []OverlayActivation
	Overlays           []display.Overlay
}

type ImageState struct {
	SOPInstanceUID string
	FrameNumber    int
	Rows           int
	Columns        int
}

type AppliedState struct {
	DisplayedArea   DisplayedArea
	Shutter         *display.Shutter
	RotationDegrees int
	FlipHorizontal  bool
	WindowCenter    float64
	WindowWidth     float64
	VOI             display.VOILUT
	PresentationLUT string
	// PresentationLUTTable is the Presentation LUT Sequence table, which
	// takes precedence over the PresentationLUT shape.
	PresentationLUTTable *display.LUT
	// ModalityLUT is the state's modality transform; nil keeps the image's.
	ModalityLUT        *display.ModalityLUT
	GraphicLayers      []GraphicLayer
	GraphicAnnotations []GraphicAnnotation
}

// Write creates a DICOM file containing a grayscale softcopy presentation state.
// If the state's SOPClassUID is not specified, it defaults to the standard GSPS
// SOP class UID. Returns ErrInvalidObject if state is nil, or ErrUnsupportedSOPClass
// if the SOP class UID does not match the supported GSPS class.
func Write(state *State) (*object.File, error) {
	if state == nil {
		return nil, fmt.Errorf("%w: state is nil", ErrInvalidObject)
	}
	sopClassUID := state.SOPClassUID
	if sopClassUID == "" {
		sopClassUID = GrayscaleSoftcopyPresentationStateStorage
	}
	if sopClassUID != GrayscaleSoftcopyPresentationStateStorage {
		return nil, fmt.Errorf("%w: %s", ErrUnsupportedSOPClass, sopClassUID)
	}
	if err := validateReferencedSeries(state.ReferencedImages); err != nil {
		return nil, err
	}
	if err := validateDisplayedAreas(state); err != nil {
		return nil, err
	}
	if err := display.ValidateShutter(state.Shutter); err != nil {
		return nil, err
	}
	if err := validateSoftcopyVOIs(state); err != nil {
		return nil, err
	}
	if err := validateGraphicAnnotations(state.GraphicAnnotations); err != nil {
		return nil, err
	}
	if err := validateCompoundLinks(state.GraphicAnnotations); err != nil {
		return nil, err
	}
	switch state.SpatialTransform.RotationDegrees {
	case 0, 90, 180, 270:
	default:
		return nil, fmt.Errorf("%w: image rotation %d is not 0, 90, 180 or 270", ErrInvalidObject, state.SpatialTransform.RotationDegrees)
	}
	if err := validateModalityLUT(state.ModalityLUT); err != nil {
		return nil, err
	}
	if err := validatePresentationLUT(state.PresentationLUT); err != nil {
		return nil, err
	}
	layers, err := resolvedGraphicLayers(state)
	if err != nil {
		return nil, err
	}
	identification, err := identificationElements(state, time.Now())
	if err != nil {
		return nil, err
	}
	elements := []core.Element{
		derivedio.UI(derivedio.TagSOPClassUID, sopClassUID),
		derivedio.UI(derivedio.TagSOPInstanceUID, state.SOPInstanceUID),
		derivedio.CS(derivedio.TagModality, "PR"),
		derivedio.UI(derivedio.TagStudyInstanceUID, state.StudyInstanceUID),
		derivedio.UI(derivedio.TagSeriesInstanceUID, state.SeriesInstanceUID),
		referencedSeriesSequence(state.ReferencedImages),
		displayedAreaSequence(state),
		derivedio.US(tagImageRotation, uint16(state.SpatialTransform.RotationDegrees)),
		derivedio.CS(tagImageHorizontalFlip, flipValue(state.SpatialTransform.FlipHorizontal)),
	}
	elements = append(elements, identification...)
	elements = append(elements, modalityLUTElements(state.ModalityLUT)...)
	elements = append(elements, shutterElements(state.Shutter)...)
	if voiSequence, ok := softcopyVOISequence(state); ok {
		elements = append(elements, voiSequence)
	}
	if state.PresentationLUT != nil {
		elements = append(elements, presentationLUTSequence(state.PresentationLUT, state.PresentationLUTExplanation))
	} else {
		elements = append(elements, derivedio.CS(tagPresentationLUTShape, presentationLUTShape(state.PresentationLUTShape)))
	}
	elements = append(elements, graphicLayerSequence(layers))
	if groups, ok, err := graphicGroupSequence(state); err != nil {
		return nil, err
	} else if ok {
		elements = append(elements, groups)
	}
	if len(state.GraphicAnnotations) > 0 {
		elements = append(elements, graphicAnnotationSequence(state.GraphicAnnotations))
	}
	overlayElements, err := overlayActivationElements(state)
	if err != nil {
		return nil, err
	}
	elements = append(elements, overlayElements...)
	dataset := derivedio.Object(elements...)
	if err := state.Common.Apply(dataset, object.InheritWithoutFrameOfReference); err != nil {
		return nil, err
	}
	return derivedio.File(sopClassUID, state.SOPInstanceUID, dataset)
}

// Read extracts GSPS presentation state from a DICOM object.
// It returns ErrInvalidObject if obj is nil, or ErrUnsupportedSOPClass if the
// object's SOP Class UID does not match the supported GSPS SOP class.
func Read(obj *object.Object) (*State, error) {
	if obj == nil {
		return nil, fmt.Errorf("%w: dataset is nil", ErrInvalidObject)
	}
	sopClassUID := derivedio.CleanUID(obj, derivedio.TagSOPClassUID)
	if sopClassUID != GrayscaleSoftcopyPresentationStateStorage {
		return nil, fmt.Errorf("%w: %s", ErrUnsupportedSOPClass, sopClassUID)
	}
	vois, err := readSoftcopyVOIs(obj)
	if err != nil {
		return nil, err
	}
	displayedAreas, err := readDisplayedAreas(obj)
	if err != nil {
		return nil, err
	}
	shutter, err := display.ShutterFromObject(obj)
	if err != nil {
		return nil, fmt.Errorf("dicom/gsps: %w", err)
	}
	modality, err := readModalityLUT(obj)
	if err != nil {
		return nil, err
	}
	plut, plutExplanation, err := readPresentationLUT(obj)
	if err != nil {
		return nil, err
	}
	state := &State{
		SOPClassUID:          sopClassUID,
		SOPInstanceUID:       derivedio.CleanUID(obj, derivedio.TagSOPInstanceUID),
		StudyInstanceUID:     derivedio.CleanUID(obj, derivedio.TagStudyInstanceUID),
		SeriesInstanceUID:    derivedio.CleanUID(obj, derivedio.TagSeriesInstanceUID),
		ReferencedImages:     readReferences(obj),
		DisplayedAreas:       displayedAreas,
		Shutter:              shutter,
		SpatialTransform:     readSpatialTransform(obj),
		SoftcopyVOIs:         vois,
		PresentationLUTShape: derivedio.CleanString(obj, tagPresentationLUTShape),
		PresentationLUT:      plut,
		ModalityLUT:          modality,
		GraphicLayers:        readGraphicLayers(obj),
		GraphicGroups:        readGraphicGroups(obj),
		GraphicAnnotations:   readGraphicAnnotations(obj),
		OverlayActivations:   readOverlayActivations(obj),
		Overlays:             readStateOverlays(obj, shutter),
	}
	state.PresentationLUTExplanation = plutExplanation
	readIdentification(obj, state)
	if len(displayedAreas) > 0 {
		state.DisplayedArea = displayedAreas[0]
	}
	if len(vois) > 0 {
		state.SoftcopyVOI = vois[0]
		for _, voi := range vois {
			if len(voi.ReferencedImages) == 0 {
				state.SoftcopyVOI = voi
				break
			}
		}
	}
	return state, nil
}

// Apply derives image-specific display parameters from a presentation state,
// optionally verifying that the image appears in the state's referenced images.
// Returns AppliedState with the derived parameters, ErrInvalidObject if state is nil,
// or ErrMissingReference if the image is not found in references.
func Apply(state *State, image ImageState) (AppliedState, error) {
	if state == nil {
		return AppliedState{}, fmt.Errorf("%w: state is nil", ErrInvalidObject)
	}
	if image.SOPInstanceUID != "" && len(state.ReferencedImages) > 0 {
		if !referencesApply(state.ReferencedImages, image) {
			return AppliedState{}, fmt.Errorf("%w: image %s", ErrMissingReference, image.SOPInstanceUID)
		}
	}
	displayedArea := selectDisplayedArea(state, image)
	annotations, err := applyGraphicAnnotations(state, displayedArea, image)
	if err != nil {
		return AppliedState{}, err
	}
	softcopyVOI := selectSoftcopyVOI(state, image)
	voi := display.VOILUT{
		Center: softcopyVOI.WindowCenter, Width: softcopyVOI.WindowWidth,
		Function: softcopyVOI.Function, LUT: softcopyVOI.LUT,
	}
	layers, err := resolvedGraphicLayers(state)
	if err != nil {
		return AppliedState{}, err
	}
	var modality *display.ModalityLUT
	if state.ModalityLUT != nil {
		transform := state.ModalityLUT.Transform()
		modality = &transform
	}
	return AppliedState{
		PresentationLUTTable: state.PresentationLUT,
		ModalityLUT:          modality,
		GraphicLayers:        layers,
		DisplayedArea:        displayedArea,
		Shutter:              cloneShutter(state.Shutter),
		RotationDegrees:      state.SpatialTransform.RotationDegrees,
		FlipHorizontal:       state.SpatialTransform.FlipHorizontal,
		WindowCenter:         softcopyVOI.WindowCenter,
		WindowWidth:          softcopyVOI.WindowWidth,
		VOI:                  voi,
		PresentationLUT:      presentationLUTShape(state.PresentationLUTShape),
		GraphicAnnotations:   annotations,
	}, nil
}

func selectSoftcopyVOI(state *State, image ImageState) SoftcopyVOI {
	items := state.SoftcopyVOIs
	if len(items) == 0 {
		return state.SoftcopyVOI
	}
	defaultIndex := -1
	for i, item := range items {
		if len(item.ReferencedImages) == 0 {
			if defaultIndex < 0 {
				defaultIndex = i
			}
			continue
		}
		if softcopyVOIApplies(item, image) {
			return item
		}
	}
	if defaultIndex >= 0 {
		return items[defaultIndex]
	}
	return SoftcopyVOI{}
}

func selectDisplayedArea(state *State, image ImageState) DisplayedArea {
	areas := configuredDisplayedAreas(state)
	defaultIndex := -1
	for i, area := range areas {
		if len(area.ReferencedImages) == 0 {
			if defaultIndex < 0 {
				defaultIndex = i
			}
			continue
		}
		if referencesApply(area.ReferencedImages, image) {
			return area
		}
	}
	if defaultIndex >= 0 {
		return areas[defaultIndex]
	}
	return DisplayedArea{}
}

func softcopyVOIApplies(item SoftcopyVOI, image ImageState) bool {
	return referencesApply(item.ReferencedImages, image)
}

func referencesApply(refs []ReferencedImage, image ImageState) bool {
	for _, ref := range refs {
		if ref.SOPInstanceUID != image.SOPInstanceUID {
			continue
		}
		if len(ref.Frames) == 0 {
			return true
		}
		if image.FrameNumber <= 0 {
			continue
		}
		for _, frame := range ref.Frames {
			if frame == image.FrameNumber {
				return true
			}
		}
	}
	return false
}

func applyGraphicAnnotations(state *State, displayedArea DisplayedArea, image ImageState) ([]GraphicAnnotation, error) {
	selected := selectGraphicAnnotations(state, image)
	annotations := make([]GraphicAnnotation, 0, len(selected))
	for i, source := range selected {
		annotation := source
		annotation.Polyline = append([]Point2D(nil), source.Polyline...)
		annotation.Compound = cloneCompoundGraphic(source.Compound)
		if box := source.BoundingBox; box != nil {
			converted := *box
			var err error
			if converted.TopLeft, err = annotationPointInPixelUnits(box.TopLeft, box.Units, displayedArea, image, state.SpatialTransform); err != nil {
				return nil, fmt.Errorf("annotation %d bounding box: %w", i, err)
			}
			if converted.BottomRight, err = annotationPointInPixelUnits(box.BottomRight, box.Units, displayedArea, image, state.SpatialTransform); err != nil {
				return nil, fmt.Errorf("annotation %d bounding box: %w", i, err)
			}
			converted.Units = AnnotationUnitsPixel
			annotation.BoundingBox = &converted
		}
		if source.Text != "" && hasAnchor(source) {
			anchor, err := annotationPointInPixelUnits(source.Anchor, source.AnchorUnits, displayedArea, image, state.SpatialTransform)
			if err != nil {
				return nil, fmt.Errorf("annotation %d anchor: %w", i, err)
			}
			annotation.Anchor = anchor
			annotation.AnchorUnits = AnnotationUnitsPixel
		}
		if len(source.Polyline) > 0 {
			for pointIndex, point := range source.Polyline {
				converted, err := annotationPointInPixelUnits(point, source.GraphicUnits, displayedArea, image, state.SpatialTransform)
				if err != nil {
					return nil, fmt.Errorf("annotation %d graphic point %d: %w", i, pointIndex, err)
				}
				annotation.Polyline[pointIndex] = converted
			}
			annotation.GraphicUnits = AnnotationUnitsPixel
		}
		annotations = append(annotations, annotation)
	}
	return annotations, nil
}

// annotationPointInPixelUnits converts a point to image pixel coordinates.
// A DISPLAY point is placed on the displayed area as shown, after the
// rotation and flip, and mapped back to the image pixel under it, so that it
// keeps its place on the screen once the image is transformed.
func annotationPointInPixelUnits(point Point2D, units string, area DisplayedArea, image ImageState, transform SpatialTransform) (Point2D, error) {
	switch normalizedAnnotationUnits(units) {
	case AnnotationUnitsPixel:
		return point, nil
	case AnnotationUnitsDisplay:
		if !unitInterval(point.X) || !unitInterval(point.Y) {
			return Point2D{}, fmt.Errorf("%w: DISPLAY point %g\\%g is outside 0..1", ErrInvalidObject, point.X, point.Y)
		}
		left, top, right, bottom, err := displayedAreaPixelEdges(area, image)
		if err != nil {
			return Point2D{}, err
		}
		g := newSpatialGeometry(image.Columns, image.Rows, transform)
		a, b := g.rotate(Point2D{X: left, Y: top}), g.rotate(Point2D{X: right, Y: bottom})
		shown := Point2D{
			X: math.Min(a.X, b.X) + point.X*math.Abs(b.X-a.X),
			Y: math.Min(a.Y, b.Y) + point.Y*math.Abs(b.Y-a.Y),
		}
		return g.unrotate(shown), nil
	default:
		return Point2D{}, fmt.Errorf("%w: %q", ErrUnsupportedUnits, units)
	}
}

func displayedAreaPixelEdges(area DisplayedArea, image ImageState) (left, top, right, bottom float64, err error) {
	if !displayedAreaDefined(area) {
		if image.Columns <= 0 || image.Rows <= 0 {
			return 0, 0, 0, 0, fmt.Errorf("%w: DISPLAY units require positive image dimensions", ErrInvalidObject)
		}
		return 0, 0, float64(image.Columns), float64(image.Rows), nil
	}
	l, t, r, b := area.ImageRegion()
	return float64(l - 1), float64(t - 1), float64(r), float64(b), nil
}

// ImageRegion returns the image region the displayed area selects, as
// 1-based inclusive pixel columns left..right and rows top..bottom. The
// corners are the pixels shown at the top left and bottom right after the
// spatial transformation (PS3.3 C.10.4), so a rotated or flipped state may
// list them in either order.
func (a DisplayedArea) ImageRegion() (left, top, right, bottom int) {
	return min(a.TopLeftX, a.BottomRightX), min(a.TopLeftY, a.BottomRightY), max(a.TopLeftX, a.BottomRightX), max(a.TopLeftY, a.BottomRightY)
}

// Corners returns the Displayed Area corners that select the 1-based
// inclusive image region left..right, top..bottom under the transform: the
// top left hand corner is the image pixel displayed at the top left after
// the rotation and flip, and the bottom right hand corner the one displayed
// at the bottom right (PS3.3 C.10.4).
func (t SpatialTransform) Corners(left, top, right, bottom int) (topLeftX, topLeftY, bottomRightX, bottomRightY int) {
	left, right = min(left, right), max(left, right)
	top, bottom = min(top, bottom), max(top, bottom)
	// Where each corner lands, up to a translation.
	place := func(x, y int) (int, int) {
		switch t.RotationDegrees {
		case 90:
			x, y = -y, x
		case 180:
			x, y = -x, -y
		case 270:
			x, y = y, -x
		}
		if t.FlipHorizontal {
			x = -x
		}
		return x, y
	}
	corners := [4][2]int{{left, top}, {right, top}, {left, bottom}, {right, bottom}}
	best := 0
	for i, corner := range corners {
		x, y := place(corner[0], corner[1])
		bx, by := place(corners[best][0], corners[best][1])
		if x+y < bx+by {
			best = i
		}
	}
	opposite := corners[3-best]
	return corners[best][0], corners[best][1], opposite[0], opposite[1]
}

func unitInterval(value float64) bool {
	return value >= 0 && value <= 1 && !math.IsNaN(value) && !math.IsInf(value, 0)
}

// referencedSeriesSequence creates a DICOM ReferencedSeriesSequence element containing referenced images grouped by series.
func referencedSeriesSequence(refs []ReferencedImage) core.Element {
	type seriesGroup struct {
		seriesUID string
		images    []core.DataSet
	}
	var groups []seriesGroup
	index := map[string]int{}
	for _, ref := range refs {
		elements := []core.Element{
			derivedio.UI(derivedio.TagRefSOPClassUID, ref.SOPClassUID),
			derivedio.UI(derivedio.TagRefSOPInstanceUID, ref.SOPInstanceUID),
		}
		if len(ref.Frames) > 0 {
			elements = append(elements, derivedio.IS(derivedio.TagRefFrameNumber, ref.Frames...))
		}
		ds := derivedio.DataSet(elements...)
		if i, ok := index[ref.SeriesInstanceUID]; ok {
			groups[i].images = append(groups[i].images, ds)
			continue
		}
		index[ref.SeriesInstanceUID] = len(groups)
		groups = append(groups, seriesGroup{seriesUID: ref.SeriesInstanceUID, images: []core.DataSet{ds}})
	}
	items := make([]core.DataSet, 0, len(groups))
	for _, group := range groups {
		items = append(items, derivedio.DataSet(
			derivedio.UI(derivedio.TagSeriesInstanceUID, group.seriesUID),
			derivedio.Seq(tagReferencedImageSequence, group.images...),
		))
	}
	return derivedio.Seq(tagReferencedSeriesSequence, items...)
}

func validateReferencedSeries(refs []ReferencedImage) error {
	for _, ref := range refs {
		if strings.TrimSpace(ref.SeriesInstanceUID) == "" ||
			strings.TrimSpace(ref.SOPClassUID) == "" ||
			strings.TrimSpace(ref.SOPInstanceUID) == "" {
			return fmt.Errorf("%w: referenced image requires series, SOP class, and SOP instance UIDs", ErrMissingReference)
		}
	}
	return nil
}

func validateDisplayedArea(area DisplayedArea) error {
	if !displayedAreaDefined(area) {
		return fmt.Errorf("%w: Displayed Area is required", ErrInvalidObject)
	}
	for _, value := range []int{area.TopLeftX, area.TopLeftY, area.BottomRightX, area.BottomRightY} {
		if int64(value) < -(1<<31) || int64(value) > 1<<31-1 {
			return fmt.Errorf("%w: Displayed Area coordinate %d is outside SL", ErrInvalidObject, value)
		}
	}
	mode := normalizedPresentationSizeMode(area.PresentationSizeMode)
	switch mode {
	case PresentationSizeScaleToFit:
	case PresentationSizeTrueSize:
		if len(area.PixelSpacing) != 2 {
			return fmt.Errorf("%w: TRUE SIZE Displayed Area requires Presentation Pixel Spacing", ErrInvalidObject)
		}
	case PresentationSizeMagnify:
		if area.MagnificationRatio <= 0 || math.IsNaN(area.MagnificationRatio) || math.IsInf(area.MagnificationRatio, 0) {
			return fmt.Errorf("%w: MAGNIFY Displayed Area requires a positive magnification ratio", ErrInvalidObject)
		}
	default:
		return fmt.Errorf("%w: unsupported Presentation Size Mode %q", ErrInvalidObject, area.PresentationSizeMode)
	}
	if area.MagnificationRatio != 0 &&
		(area.MagnificationRatio < 0 || math.IsNaN(area.MagnificationRatio) || math.IsInf(area.MagnificationRatio, 0)) {
		return fmt.Errorf("%w: invalid magnification ratio %g", ErrInvalidObject, area.MagnificationRatio)
	}
	if len(area.PixelSpacing) != 0 && len(area.PixelSpacing) != 2 {
		return fmt.Errorf("%w: Presentation Pixel Spacing requires two values", ErrInvalidObject)
	}
	for _, value := range area.PixelSpacing {
		if value <= 0 || math.IsNaN(value) || math.IsInf(value, 0) {
			return fmt.Errorf("%w: Presentation Pixel Spacing contains invalid value %g", ErrInvalidObject, value)
		}
	}
	if len(area.PixelAspectRatio) != 0 && len(area.PixelAspectRatio) != 2 {
		return fmt.Errorf("%w: Presentation Pixel Aspect Ratio requires two values", ErrInvalidObject)
	}
	if len(area.PixelSpacing) > 0 && len(area.PixelAspectRatio) > 0 {
		return fmt.Errorf("%w: Displayed Area cannot encode both Presentation Pixel Spacing and Aspect Ratio", ErrInvalidObject)
	}
	for _, value := range area.PixelAspectRatio {
		if value <= 0 {
			return fmt.Errorf("%w: Presentation Pixel Aspect Ratio contains invalid value %d", ErrInvalidObject, value)
		}
	}
	for _, ref := range area.ReferencedImages {
		if strings.TrimSpace(ref.SOPClassUID) == "" || strings.TrimSpace(ref.SOPInstanceUID) == "" {
			return fmt.Errorf("%w: Displayed Area reference requires SOP class and instance UIDs", ErrMissingReference)
		}
	}
	return nil
}

func validateDisplayedAreas(state *State) error {
	areas := configuredDisplayedAreas(state)
	if len(areas) == 0 {
		return fmt.Errorf("%w: Displayed Area is required", ErrInvalidObject)
	}
	for i, area := range areas {
		if err := validateDisplayedArea(area); err != nil {
			return fmt.Errorf("dicom/gsps: Displayed Area item %d: %w", i, err)
		}
	}
	return nil
}

func configuredDisplayedAreas(state *State) []DisplayedArea {
	if state != nil && len(state.DisplayedAreas) > 0 {
		return state.DisplayedAreas
	}
	if state != nil && displayedAreaDefined(state.DisplayedArea) {
		return []DisplayedArea{state.DisplayedArea}
	}
	return nil
}

func normalizedPresentationSizeMode(mode string) string {
	mode = strings.ToUpper(strings.TrimSpace(mode))
	if mode == "" {
		return PresentationSizeScaleToFit
	}
	return mode
}

func shutterElements(shutter *display.Shutter) []core.Element {
	if shutter == nil {
		return nil
	}
	shapes := make([]string, 0, 4)
	elements := make([]core.Element, 0, 16)
	if rect := shutter.Rectangle; rect != nil {
		shapes = append(shapes, "RECTANGULAR")
		elements = append(elements,
			derivedio.IS(tagShutterLeft, rect.Left),
			derivedio.IS(tagShutterRight, rect.Right),
			derivedio.IS(tagShutterUpper, rect.Upper),
			derivedio.IS(tagShutterLower, rect.Lower),
		)
	}
	if circle := shutter.Circle; circle != nil {
		shapes = append(shapes, "CIRCULAR")
		elements = append(elements,
			derivedio.IS(tagShutterCircleCenter, circle.CenterRow, circle.CenterCol),
			derivedio.IS(tagShutterCircleRadius, circle.Radius),
		)
	}
	if polygon := shutter.Polygon; polygon != nil {
		shapes = append(shapes, "POLYGONAL")
		values := make([]int, 0, len(polygon.Vertices)*2)
		for _, point := range polygon.Vertices {
			values = append(values, point.Row, point.Col)
		}
		elements = append(elements, derivedio.IS(tagShutterPolygonVertices, values...))
	}
	if bitmap := shutter.Bitmap; bitmap != nil {
		shapes = append(shapes, "BITMAP")
		group := bitmap.Group
		overlay := bitmap.Overlay
		elements = append(elements,
			derivedio.US(tagShutterOverlayGroup, group),
			derivedio.US(core.NewTag(group, 0x0010), uint16(overlay.Rows)),
			derivedio.US(core.NewTag(group, 0x0011), uint16(overlay.Columns)),
			derivedio.CS(core.NewTag(group, 0x0040), "G"),
			signedShorts(core.NewTag(group, 0x0050), int16(overlay.OriginRow), int16(overlay.OriginCol)),
			derivedio.US(core.NewTag(group, 0x0100), 1),
			derivedio.US(core.NewTag(group, 0x0102), 0),
			derivedio.Raw(core.NewTag(group, 0x3000), core.VROW, append([]byte(nil), overlay.Data...)),
		)
	}
	pvalue := uint16(shutter.PresentationValue) * 257
	if shutter.PresentationPValueDefined {
		pvalue = shutter.PresentationPValue
	}
	head := []core.Element{
		derivedio.Strings(tagShutterShape, core.VRCS, shapes),
		derivedio.US(tagShutterPresentationValue, pvalue),
	}
	if shutter.PresentationColorCIELabDefined {
		color := shutter.PresentationColorCIELab
		head = append(head, derivedio.US(tagShutterPresentationColor, color[0], color[1], color[2]))
	}
	return append(head, elements...)
}

func signedShorts(tag core.Tag, values ...int16) core.Element {
	return object.SSElement(tag, binary.LittleEndian, values...)
}

func cloneShutter(source *display.Shutter) *display.Shutter {
	return source.Clone()
}

func validateGraphicAnnotations(items []GraphicAnnotation) error {
	type groupKey struct {
		index int
		layer string
	}
	groupRefs := map[groupKey][]ReferencedImage{}
	for i, item := range items {
		for _, ref := range item.ReferencedImages {
			if strings.TrimSpace(ref.SOPClassUID) == "" || strings.TrimSpace(ref.SOPInstanceUID) == "" {
				return fmt.Errorf("%w: annotation %d reference requires SOP class and instance UIDs", ErrMissingReference, i)
			}
		}
		if item.GroupIndex > 0 {
			key := groupKey{index: item.GroupIndex, layer: graphicLayerName(item.LayerName)}
			if previous, ok := groupRefs[key]; ok && !referencedImagesEqual(previous, item.ReferencedImages) {
				return fmt.Errorf("%w: annotation group %d on layer %q has inconsistent references", ErrInvalidObject, item.GroupIndex, item.LayerName)
			}
			groupRefs[key] = item.ReferencedImages
		}
		if !validCS(graphicLayerName(item.LayerName)) {
			return fmt.Errorf("%w: annotation %d layer %q is not a CS value", ErrInvalidObject, i, item.LayerName)
		}
		if item.Compound != nil {
			if item.Text != "" || item.BoundingBox != nil || item.AnchorVisible || len(item.Polyline) != 0 || item.Filled {
				return fmt.Errorf("%w: annotation %d mixes Compound Graphic and simple object attributes", ErrInvalidObject, i)
			}
			if len(item.Compound.Points) > maxWrittenCompoundPoints {
				return fmt.Errorf("%w: annotation %d Compound Graphic exceeds Explicit VR Graphic Data length", ErrInvalidObject, i)
			}
			if err := validateCompoundGraphic(item.Compound); err != nil {
				return fmt.Errorf("dicom/gsps: annotation %d: %w", i, err)
			}
			continue
		}
		if item.Text != "" {
			if err := validateTextObject(item); err != nil {
				return fmt.Errorf("dicom/gsps: annotation %d text: %w", i, err)
			}
		} else if item.BoundingBox != nil || item.AnchorVisible {
			return fmt.Errorf("%w: annotation %d has a bounding box or visible anchor without text", ErrInvalidObject, i)
		}
		if item.Filled && !graphicNeedsFilledAttribute(normalizedGraphicTypeOrPolyline(item.GraphicType), item.Polyline) {
			return fmt.Errorf("%w: annotation %d is filled but not a closed graphic", ErrInvalidObject, i)
		}
		if len(item.Polyline) > 1<<16-1 {
			return fmt.Errorf("%w: annotation %d has %d graphic points, maximum is 65535", ErrInvalidObject, i, len(item.Polyline))
		}
		for pointIndex, point := range item.Polyline {
			if err := validateAnnotationPoint(point, item.GraphicUnits); err != nil {
				return fmt.Errorf("dicom/gsps: annotation %d graphic point %d: %w", i, pointIndex, err)
			}
		}
	}
	return nil
}

func referencedImagesEqual(left, right []ReferencedImage) bool {
	if len(left) != len(right) {
		return false
	}
	for i := range left {
		if left[i].SeriesInstanceUID != right[i].SeriesInstanceUID ||
			left[i].SOPClassUID != right[i].SOPClassUID ||
			left[i].SOPInstanceUID != right[i].SOPInstanceUID ||
			len(left[i].Frames) != len(right[i].Frames) {
			return false
		}
		for frameIndex := range left[i].Frames {
			if left[i].Frames[frameIndex] != right[i].Frames[frameIndex] {
				return false
			}
		}
	}
	return true
}

func validateAnnotationPoint(point Point2D, units string) error {
	units = normalizedAnnotationUnits(units)
	if units != AnnotationUnitsPixel && units != AnnotationUnitsDisplay {
		return fmt.Errorf("%w: %q", ErrUnsupportedUnits, units)
	}
	if math.IsNaN(point.X) || math.IsInf(point.X, 0) || math.IsNaN(point.Y) || math.IsInf(point.Y, 0) {
		return fmt.Errorf("%w: annotation point contains a non-finite value", ErrInvalidObject)
	}
	if units == AnnotationUnitsDisplay && (!unitInterval(point.X) || !unitInterval(point.Y)) {
		return fmt.Errorf("%w: DISPLAY point %g\\%g is outside 0..1", ErrInvalidObject, point.X, point.Y)
	}
	return nil
}

func validateSoftcopyVOIs(state *State) error {
	items := configuredSoftcopyVOIs(state)
	for i, item := range items {
		for _, ref := range item.ReferencedImages {
			if strings.TrimSpace(ref.SOPClassUID) == "" || strings.TrimSpace(ref.SOPInstanceUID) == "" {
				return fmt.Errorf("%w: Softcopy VOI item %d reference requires SOP class and instance UIDs", ErrMissingReference, i)
			}
		}
		if item.LUT == nil {
			if math.IsNaN(item.WindowCenter) || math.IsInf(item.WindowCenter, 0) ||
				math.IsNaN(item.WindowWidth) || math.IsInf(item.WindowWidth, 0) {
				return fmt.Errorf("%w: Softcopy VOI item %d window contains a non-finite value", ErrInvalidObject, i)
			}
			if item.Function == display.VOILinear && item.WindowWidth < 1 {
				return fmt.Errorf("%w: Softcopy VOI item %d LINEAR window width %g is below 1", ErrInvalidObject, i, item.WindowWidth)
			}
			if item.Function != display.VOILinear && item.WindowWidth <= 0 {
				return fmt.Errorf("%w: Softcopy VOI item %d window width %g is not positive", ErrInvalidObject, i, item.WindowWidth)
			}
			continue
		}
		if item.LUT.BitsPerEntry != 8 && item.LUT.BitsPerEntry != 16 {
			return fmt.Errorf("%w: Softcopy VOI item %d LUT has %d bits per entry, not 8 or 16", ErrInvalidObject, i, item.LUT.BitsPerEntry)
		}
		if item.LUT.FirstMapped < -(1<<15) || item.LUT.FirstMapped > 1<<16-1 {
			return fmt.Errorf("%w: Softcopy VOI item %d LUT first mapped value %d is outside SS/US", ErrInvalidObject, i, item.LUT.FirstMapped)
		}
		if len(item.LUT.Entries) > 1<<16 {
			return fmt.Errorf("%w: Softcopy VOI item %d LUT has %d entries, maximum is 65536", ErrInvalidObject, i, len(item.LUT.Entries))
		}
		encodedCount := len(item.LUT.Entries)
		if encodedCount == 1<<16 {
			encodedCount = 0
		}
		if _, err := display.NewLUT([]int{encodedCount, item.LUT.FirstMapped, item.LUT.BitsPerEntry}, item.LUT.Entries); err != nil {
			return fmt.Errorf("dicom/gsps: Softcopy VOI item %d: %w", i, err)
		}
	}
	return nil
}

func displayedAreaDefined(area DisplayedArea) bool {
	return area.Defined || area.TopLeftX != 0 || area.TopLeftY != 0 || area.BottomRightX != 0 || area.BottomRightY != 0
}

func configuredSoftcopyVOIs(state *State) []SoftcopyVOI {
	if len(state.SoftcopyVOIs) > 0 {
		return state.SoftcopyVOIs
	}
	if softcopyVOIDefined(state.SoftcopyVOI) {
		return []SoftcopyVOI{state.SoftcopyVOI}
	}
	return nil
}

func softcopyVOIDefined(voi SoftcopyVOI) bool {
	return voi.WindowCenter != 0 || voi.WindowWidth != 0 || voi.Explanation != "" ||
		voi.Function != display.VOILinear || voi.LUT != nil || len(voi.ReferencedImages) > 0
}

// displayedAreaSequence creates image-specific or default Displayed Area items.
func displayedAreaSequence(state *State) core.Element {
	areas := configuredDisplayedAreas(state)
	items := make([]core.DataSet, 0, len(areas))
	for _, area := range areas {
		elements := []core.Element{
			derivedio.SL(tagDisplayedAreaTopLeft, int32(area.TopLeftX), int32(area.TopLeftY)),
			derivedio.SL(tagDisplayedAreaBottomRight, int32(area.BottomRightX), int32(area.BottomRightY)),
			derivedio.CS(tagPresentationSizeMode, normalizedPresentationSizeMode(area.PresentationSizeMode)),
		}
		if len(area.ReferencedImages) > 0 {
			elements = append(elements, voiReferencedImageSequence(area.ReferencedImages))
		}
		if len(area.PixelSpacing) == 2 {
			elements = append(elements, derivedio.DS(tagPresentationPixelSpacing, area.PixelSpacing...))
		}
		if len(area.PixelAspectRatio) == 2 {
			elements = append(elements, derivedio.IS(tagPresentationPixelAspectRatio, area.PixelAspectRatio...))
		} else if len(area.PixelSpacing) != 2 {
			// Presentation Pixel Aspect Ratio is required without a spacing.
			elements = append(elements, derivedio.IS(tagPresentationPixelAspectRatio, 1, 1))
		}
		if area.MagnificationRatio != 0 {
			elements = append(elements, derivedio.DS(tagPresentationPixelMagnify, area.MagnificationRatio))
		}
		items = append(items, derivedio.DataSet(elements...))
	}
	return derivedio.Seq(tagDisplayedAreaSelectionSeq, items...)
}

// softcopyVOISequence creates image-specific or default Softcopy VOI items.
func softcopyVOISequence(state *State) (core.Element, bool) {
	items := configuredSoftcopyVOIs(state)
	if len(items) == 0 {
		return core.Element{}, false
	}
	sets := make([]core.DataSet, 0, len(items))
	for _, voi := range items {
		elements := make([]core.Element, 0, 5)
		if len(voi.ReferencedImages) > 0 {
			elements = append(elements, voiReferencedImageSequence(voi.ReferencedImages))
		}
		if voi.LUT != nil {
			elements = append(elements, voiLUTSequence(voi))
		} else {
			elements = append(elements,
				derivedio.DS(tagWindowCenter, voi.WindowCenter),
				derivedio.DS(tagWindowWidth, voi.WindowWidth),
				derivedio.LO(tagWindowCenterWidthExplanation, voi.Explanation),
				derivedio.CS(tagVOILUTFunction, voiFunctionValue(voi.Function)),
			)
		}
		sets = append(sets, derivedio.DataSet(elements...))
	}
	return derivedio.Seq(tagSoftcopyVOILUTSequence, sets...), true
}

func voiReferencedImageSequence(refs []ReferencedImage) core.Element {
	items := make([]core.DataSet, 0, len(refs))
	for _, ref := range refs {
		elements := []core.Element{
			derivedio.UI(derivedio.TagRefSOPClassUID, ref.SOPClassUID),
			derivedio.UI(derivedio.TagRefSOPInstanceUID, ref.SOPInstanceUID),
		}
		if len(ref.Frames) > 0 {
			elements = append(elements, derivedio.IS(derivedio.TagRefFrameNumber, ref.Frames...))
		}
		items = append(items, derivedio.DataSet(elements...))
	}
	return derivedio.Seq(tagReferencedImageSequence, items...)
}

func voiLUTSequence(voi SoftcopyVOI) core.Element {
	return derivedio.Seq(tagVOILUTSequence, derivedio.DataSet(lutItemElements(voi.LUT, voi.Explanation)...))
}

func voiFunctionValue(function display.VOIFunction) string {
	switch function {
	case display.VOILinearExact:
		return "LINEAR_EXACT"
	case display.VOISigmoid:
		return "SIGMOID"
	default:
		return "LINEAR"
	}
}

// graphicAnnotationSequence creates a DICOM GraphicAnnotationSequence element
// while preserving objects that were grouped in the same source item.
func graphicAnnotationSequence(items []GraphicAnnotation) core.Element {
	type groupKey struct {
		index    int
		layer    string
		compound bool
	}
	type annotationGroup struct {
		layer string
		items []GraphicAnnotation
		refs  []ReferencedImage
	}

	groups := make([]annotationGroup, 0, len(items))
	groupIndexes := map[groupKey]int{}
	compoundIndexes := compoundIndexByID(items)
	for _, item := range items {
		layer := graphicLayerName(item.LayerName)
		var key groupKey
		switch {
		case item.GroupIndex > 0:
			key = groupKey{index: item.GroupIndex, layer: layer}
		case item.compoundGraphicIDPresent():
			key = groupKey{index: compoundIndexes[item.CompoundGraphicID], layer: layer, compound: true}
		default:
			groups = append(groups, annotationGroup{layer: layer, items: []GraphicAnnotation{item}, refs: item.ReferencedImages})
			continue
		}
		if groupIndex, ok := groupIndexes[key]; ok {
			groups[groupIndex].items = append(groups[groupIndex].items, item)
			continue
		}
		groupIndexes[key] = len(groups)
		groups = append(groups, annotationGroup{layer: layer, items: []GraphicAnnotation{item}, refs: item.ReferencedImages})
	}

	sets := make([]core.DataSet, 0, len(groups))
	for _, group := range groups {
		elements := []core.Element{derivedio.CS(tagGraphicLayer, group.layer)}
		if len(group.refs) > 0 {
			elements = append(elements, voiReferencedImageSequence(group.refs))
		}
		texts := make([]core.DataSet, 0, len(group.items))
		graphics := make([]core.DataSet, 0, len(group.items))
		var compounds []core.DataSet
		for _, item := range group.items {
			if item.Compound != nil {
				compounds = append(compounds, compoundGraphicItem(item))
				continue
			}
			if item.Text != "" {
				texts = append(texts, textObjectItem(item))
			}
			if len(item.Polyline) > 0 {
				graphicType := normalizedGraphicTypeOrPolyline(item.GraphicType)
				graphicElements := []core.Element{
					derivedio.CS(tagGraphicAnnotationUnits, normalizedAnnotationUnits(item.GraphicUnits)),
					derivedio.US(tagGraphicDimensions, 2),
					derivedio.US(tagNumberOfGraphicPoints, uint16(len(item.Polyline))),
					derivedio.FL(tagGraphicData, flattenPoints(item.Polyline)...),
					derivedio.CS(tagGraphicType, graphicType),
				}
				if graphicNeedsFilledAttribute(graphicType, item.Polyline) {
					graphicElements = append(graphicElements, derivedio.CS(tagGraphicFilled, flipValue(item.Filled)))
				}
				graphics = append(graphics, derivedio.DataSet(append(graphicElements, compoundLinkElements(item)...)...))
			}
		}
		if len(texts) > 0 {
			elements = append(elements, derivedio.Seq(tagTextObjectSequence, texts...))
		}
		if len(graphics) > 0 {
			elements = append(elements, derivedio.Seq(tagGraphicObjectSequence, graphics...))
		}
		if len(compounds) > 0 {
			elements = append(elements, derivedio.Seq(tagCompoundGraphicSequence, compounds...))
		}
		sets = append(sets, derivedio.DataSet(elements...))
	}
	return derivedio.Seq(tagGraphicAnnotationSequence, sets...)
}

// readReferences extracts referenced image identifiers and frame numbers from a DICOM object.
func readReferences(obj *object.Object) []ReferencedImage {
	var out []ReferencedImage
	for _, series := range derivedio.Sequence(obj, tagReferencedSeriesSequence) {
		seriesUID := derivedio.CleanUID(series, derivedio.TagSeriesInstanceUID)
		for _, refObj := range derivedio.Sequence(series, tagReferencedImageSequence) {
			ref := ReferencedImage{
				SeriesInstanceUID: seriesUID,
				SOPClassUID:       derivedio.CleanUID(refObj, derivedio.TagRefSOPClassUID),
				SOPInstanceUID:    derivedio.CleanUID(refObj, derivedio.TagRefSOPInstanceUID),
			}
			if frames, err := refObj.GetInts(derivedio.TagRefFrameNumber); err == nil {
				for _, frame := range frames {
					ref.Frames = append(ref.Frames, int(frame))
				}
			}
			out = append(out, ref)
		}
	}
	return out
}

func readDirectImageReferences(obj *object.Object) []ReferencedImage {
	var out []ReferencedImage
	for _, refObj := range derivedio.Sequence(obj, tagReferencedImageSequence) {
		ref := ReferencedImage{
			SOPClassUID:    derivedio.CleanUID(refObj, derivedio.TagRefSOPClassUID),
			SOPInstanceUID: derivedio.CleanUID(refObj, derivedio.TagRefSOPInstanceUID),
		}
		if frames, err := refObj.GetInts(derivedio.TagRefFrameNumber); err == nil {
			for _, frame := range frames {
				ref.Frames = append(ref.Frames, int(frame))
			}
		}
		out = append(out, ref)
	}
	return out
}

// readDisplayedAreas extracts every image-specific or default displayed area.
func readDisplayedAreas(obj *object.Object) ([]DisplayedArea, error) {
	items := derivedio.Sequence(obj, tagDisplayedAreaSelectionSeq)
	if len(items) == 0 {
		return nil, nil
	}
	areas := make([]DisplayedArea, 0, len(items))
	for i, item := range items {
		tl := derivedio.Ints(item, tagDisplayedAreaTopLeft)
		br := derivedio.Ints(item, tagDisplayedAreaBottomRight)
		if len(tl) != 2 || len(br) != 2 {
			return nil, fmt.Errorf("%w: Displayed Area item %d requires two corners", ErrInvalidObject, i)
		}
		area := DisplayedArea{
			Defined:              true,
			TopLeftX:             int(tl[0]),
			TopLeftY:             int(tl[1]),
			BottomRightX:         int(br[0]),
			BottomRightY:         int(br[1]),
			PresentationSizeMode: derivedio.CleanString(item, tagPresentationSizeMode),
			PixelSpacing:         derivedio.Floats(item, tagPresentationPixelSpacing),
			ReferencedImages:     readDirectImageReferences(item),
		}
		for _, value := range derivedio.Ints(item, tagPresentationPixelAspectRatio) {
			area.PixelAspectRatio = append(area.PixelAspectRatio, int(value))
		}
		if values := derivedio.Floats(item, tagPresentationPixelMagnify); len(values) > 0 {
			area.MagnificationRatio = values[0]
		}
		if err := validateDisplayedArea(area); err != nil {
			return nil, fmt.Errorf("dicom/gsps: Displayed Area item %d: %w", i, err)
		}
		areas = append(areas, area)
	}
	return areas, nil
}

// readSpatialTransform extracts spatial transform settings from the given DICOM object.
func readSpatialTransform(obj *object.Object) SpatialTransform {
	return SpatialTransform{
		RotationDegrees: derivedio.Int(obj, tagImageRotation),
		FlipHorizontal:  derivedio.CleanString(obj, tagImageHorizontalFlip) == "Y",
	}
}

// readSoftcopyVOIs extracts every image-specific or default Softcopy VOI item.
func readSoftcopyVOIs(obj *object.Object) ([]SoftcopyVOI, error) {
	items := derivedio.Sequence(obj, tagSoftcopyVOILUTSequence)
	out := make([]SoftcopyVOI, 0, len(items))
	for i, item := range items {
		transform, err := display.VOIFromObject(item)
		if err != nil {
			return nil, fmt.Errorf("dicom/gsps: Softcopy VOI item %d: %w", i, err)
		}
		explanation := derivedio.CleanString(item, tagWindowCenterWidthExplanation)
		if transform.LUT != nil {
			if lutItems := derivedio.Sequence(item, tagVOILUTSequence); len(lutItems) > 0 {
				explanation = derivedio.CleanString(lutItems[0], tagLUTExplanation)
			}
		}
		out = append(out, SoftcopyVOI{
			WindowCenter:     transform.Center,
			WindowWidth:      transform.Width,
			Explanation:      explanation,
			Function:         transform.Function,
			LUT:              transform.LUT,
			ReferencedImages: readVOIReferences(item),
		})
	}
	return out, nil
}

func readVOIReferences(item *object.Object) []ReferencedImage {
	var out []ReferencedImage
	for _, refObj := range derivedio.Sequence(item, tagReferencedImageSequence) {
		ref := ReferencedImage{
			SOPClassUID:    derivedio.CleanUID(refObj, derivedio.TagRefSOPClassUID),
			SOPInstanceUID: derivedio.CleanUID(refObj, derivedio.TagRefSOPInstanceUID),
		}
		if frames, err := refObj.GetInts(derivedio.TagRefFrameNumber); err == nil {
			for _, frame := range frames {
				ref.Frames = append(ref.Frames, int(frame))
			}
		}
		out = append(out, ref)
	}
	return out
}

// readGraphicAnnotations extracts graphic annotations from a DICOM object's GraphicAnnotationSequence.
func readGraphicAnnotations(obj *object.Object) []GraphicAnnotation {
	var out []GraphicAnnotation
	for groupIndex, item := range derivedio.Sequence(obj, tagGraphicAnnotationSequence) {
		layer := derivedio.CleanString(item, tagGraphicLayer)
		references := readDirectImageReferences(item)
		texts := readTextObjects(item, layer)
		graphics := readGraphicObjects(item, layer)
		compounds := readCompoundGraphics(item, layer)
		for _, annotations := range [][]GraphicAnnotation{texts, graphics, compounds} {
			for i := range annotations {
				annotations[i].GroupIndex = groupIndex + 1
				annotations[i].ReferencedImages = cloneReferences(references)
			}
			out = append(out, annotations...)
		}
		if len(texts) == 0 && len(graphics) == 0 && len(compounds) == 0 {
			out = append(out, GraphicAnnotation{
				LayerName: layer, GroupIndex: groupIndex + 1,
				ReferencedImages: cloneReferences(references),
			})
		}
	}
	return out
}

func cloneReferences(source []ReferencedImage) []ReferencedImage {
	out := make([]ReferencedImage, len(source))
	for i := range source {
		out[i] = source[i]
		out[i].Frames = append([]int(nil), source[i].Frames...)
	}
	return out
}

func readTextObjects(item *object.Object, layer string) []GraphicAnnotation {
	var out []GraphicAnnotation
	for _, textItem := range derivedio.Sequence(item, tagTextObjectSequence) {
		annotation := GraphicAnnotation{
			LayerName:     layer,
			Text:          derivedio.CleanString(textItem, tagUnformattedTextValue),
			AnchorUnits:   normalizedAnnotationUnits(derivedio.CleanString(textItem, tagAnchorPointAnnotationUnits)),
			BoundingBox:   readBoundingBox(textItem),
			AnchorVisible: derivedio.CleanString(textItem, tagAnchorPointVisibility) == "Y",
		}
		annotation.malformedCompoundLink = !readCompoundLink(textItem, &annotation)
		anchor := derivedio.Floats(textItem, tagAnchorPoint)
		if len(anchor) >= 2 {
			annotation.Anchor = Point2D{X: anchor[0], Y: anchor[1]}
		}
		out = append(out, annotation)
	}
	return out
}

func readGraphicObjects(item *object.Object, layer string) []GraphicAnnotation {
	var out []GraphicAnnotation
	for _, graphicItem := range derivedio.Sequence(item, tagGraphicObjectSequence) {
		annotation := GraphicAnnotation{
			LayerName:    layer,
			GraphicType:  normalizedGraphicTypeOrPolyline(derivedio.CleanString(graphicItem, tagGraphicType)),
			GraphicUnits: normalizedAnnotationUnits(derivedio.CleanString(graphicItem, tagGraphicAnnotationUnits)),
			Filled:       derivedio.CleanString(graphicItem, tagGraphicFilled) == "Y",
		}
		annotation.malformedCompoundLink = !readCompoundLink(graphicItem, &annotation)
		values := derivedio.Floats(graphicItem, tagGraphicData)
		for i := 0; i+1 < len(values); i += 2 {
			annotation.Polyline = append(annotation.Polyline, Point2D{X: values[i], Y: values[i+1]})
		}
		out = append(out, annotation)
	}
	return out
}

func normalizedGraphicType(value string) string {
	return strings.ToUpper(strings.TrimSpace(value))
}

func normalizedGraphicTypeOrPolyline(value string) string {
	if value = normalizedGraphicType(value); value == "" {
		return GraphicTypePolyline
	}
	return value
}

func normalizedAnnotationUnits(value string) string {
	value = strings.ToUpper(strings.TrimSpace(value))
	if value == "" {
		return AnnotationUnitsPixel
	}
	return value
}

// flattenPoints converts a sequence of 2D points into a flat float64 slice with alternating X and Y coordinates.
func flattenPoints(points []Point2D) []float64 {
	values := make([]float64, 0, len(points)*2)
	for _, point := range points {
		values = append(values, point.X, point.Y)
	}
	return values
}

func graphicLayerName(value string) string {
	if value == "" {
		return "ANNOTATIONS"
	}
	return value
}

func graphicNeedsFilledAttribute(graphicType string, points []Point2D) bool {
	switch graphicType {
	case GraphicTypeCircle, GraphicTypeEllipse:
		return len(points) > 0
	case GraphicTypePolyline, GraphicTypeInterpolated:
		return closedPolyline(points)
	default:
		return false
	}
}

func closedPolyline(points []Point2D) bool {
	if len(points) < 2 {
		return false
	}
	first := points[0]
	last := points[len(points)-1]
	return first.X == last.X && first.Y == last.Y
}

// flipValue returns "Y" if enabled is true, otherwise "N".
func flipValue(enabled bool) string {
	if enabled {
		return "Y"
	}
	return "N"
}

// presentationLUTShape returns value if non-empty, otherwise returns the identity presentation LUT shape.
func presentationLUTShape(value string) string {
	if value == "" {
		return PresentationLUTIdentity
	}
	return value
}
