package gsps

import (
	"encoding/binary"
	"fmt"
	"math"
	"sort"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/ThalesMMS/dicom-go/core"
	"github.com/ThalesMMS/dicom-go/internal/derivedio"
	"github.com/ThalesMMS/dicom-go/object"
	"github.com/ThalesMMS/dicom-go/pixeldata/display"
)

// This file holds the Presentation State Identification and Presentation
// Series attributes, the Graphic Layer module, text bounding boxes and the
// Modality LUT and Presentation LUT Sequence of a presentation state.

const (
	// Bounding Box Text Horizontal Justification (0070,0012) values.
	JustifyLeft   = "LEFT"
	JustifyRight  = "RIGHT"
	JustifyCenter = "CENTER"

	// DefaultContentLabel is the Content Label Write uses when the state
	// has none.
	DefaultContentLabel = "UNNAMED"

	// maxTextLength is the length limit of Unformatted Text Value (ST).
	maxTextLength = 1024
)

var (
	tagSeriesNumber                     = core.NewTag(0x0020, 0x0011)
	tagInstanceNumber                   = core.NewTag(0x0020, 0x0013)
	tagLaterality                       = core.NewTag(0x0020, 0x0060)
	tagContentLabel                     = core.NewTag(0x0070, 0x0080)
	tagContentDescription               = core.NewTag(0x0070, 0x0081)
	tagPresentationCreationDate         = core.NewTag(0x0070, 0x0082)
	tagPresentationCreationTime         = core.NewTag(0x0070, 0x0083)
	tagContentCreatorName               = core.NewTag(0x0070, 0x0084)
	tagBoundingBoxAnnotationUnits       = core.NewTag(0x0070, 0x0003)
	tagBoundingBoxTopLeft               = core.NewTag(0x0070, 0x0010)
	tagBoundingBoxBottomRight           = core.NewTag(0x0070, 0x0011)
	tagBoundingBoxJustification         = core.NewTag(0x0070, 0x0012)
	tagGraphicLayerRecommendedGrayscale = core.NewTag(0x0070, 0x0066)
	tagGraphicLayerDescription          = core.NewTag(0x0070, 0x0068)
	tagGraphicLayerRecommendedCIELab    = core.NewTag(0x0070, 0x0401)
	tagPresentationLUTSequence          = core.NewTag(0x2050, 0x0010)
	tagRescaleIntercept                 = core.NewTag(0x0028, 0x1052)
	tagRescaleSlope                     = core.NewTag(0x0028, 0x1053)
	tagRescaleType                      = core.NewTag(0x0028, 0x1054)
	tagModalityLUTSequence              = core.NewTag(0x0028, 0x3000)
	tagModalityLUTType                  = core.NewTag(0x0028, 0x3004)
)

// GraphicLayer is an item of the Graphic Layer Sequence (PS3.3 C.10.7).
type GraphicLayer struct {
	// Name is the Graphic Layer (CS: at most 16 upper-case letters, digits,
	// spaces or underscores).
	Name string
	// Order is the Graphic Layer Order: layers are drawn from the lowest
	// order up. Zero places the layer after every ordered layer, in the
	// order the layers are first used.
	Order int
	// RecommendedGrayscale is the Graphic Layer Recommended Display
	// Grayscale Value, a 16-bit P-Value.
	RecommendedGrayscale *uint16
	// RecommendedCIELab is the Graphic Layer Recommended Display CIELab
	// Value.
	RecommendedCIELab *[3]uint16
	// Description is the Graphic Layer Description (LO).
	Description string
}

// BoundingBox is the bounding box of a text object: its corners, their
// units and how the text is justified in it.
type BoundingBox struct {
	TopLeft     Point2D
	BottomRight Point2D
	// Units is PIXEL (empty) or DISPLAY.
	Units string
	// Justification is LEFT (empty), RIGHT or CENTER.
	Justification string
}

// ModalityLUT is the Modality LUT module of a presentation state, which
// replaces the modality transform of the referenced images. It is either a
// rescale (Slope, Intercept and RescaleType) or a LUT (LUT, LUTType and
// Explanation).
type ModalityLUT struct {
	Slope     float64
	Intercept float64
	// RescaleType is Rescale Type (LO); empty writes "US" (unspecified).
	RescaleType string
	LUT         *display.LUT
	// LUTType is Modality LUT Type (LO); empty writes "US".
	LUTType     string
	Explanation string
}

// Transform returns the modality transform the display pipeline applies.
func (m *ModalityLUT) Transform() display.ModalityLUT {
	if m == nil {
		return display.ModalityLUT{Slope: 1}
	}
	if m.LUT != nil {
		return display.ModalityLUT{LUT: m.LUT}
	}
	return display.ModalityLUT{Slope: m.Slope, Intercept: m.Intercept}
}

// identificationElements encodes the Presentation Series and Presentation
// State Identification attributes, defaulting the Instance Number to 1, the
// Content Label to UNNAMED and the creation date and time to now.
func identificationElements(state *State, now time.Time) ([]core.Element, error) {
	label := state.ContentLabel
	if label == "" {
		label = DefaultContentLabel
	}
	if !validCS(label) {
		return nil, fmt.Errorf("%w: Content Label %q is not a CS value", ErrInvalidObject, label)
	}
	if state.InstanceNumber < 0 || state.SeriesNumber < 0 {
		return nil, fmt.Errorf("%w: negative Series or Instance Number", ErrInvalidObject)
	}
	if state.Laterality != "" && state.Laterality != "L" && state.Laterality != "R" {
		return nil, fmt.Errorf("%w: Laterality %q is neither L nor R", ErrInvalidObject, state.Laterality)
	}
	instance := state.InstanceNumber
	if instance == 0 {
		instance = 1
	}
	date, clock := state.CreationDate, state.CreationTime
	if date == "" {
		date = now.Format("20060102")
	}
	if clock == "" {
		clock = now.Format("150405")
	}
	series := derivedio.IS(tagSeriesNumber)
	if state.SeriesNumber > 0 {
		series = derivedio.IS(tagSeriesNumber, state.SeriesNumber)
	}
	elements := []core.Element{
		series,
		derivedio.IS(tagInstanceNumber, instance),
		derivedio.CS(tagContentLabel, label),
		derivedio.LO(tagContentDescription, state.ContentDescription),
		derivedio.Str(tagPresentationCreationDate, core.VRDA, date),
		derivedio.Str(tagPresentationCreationTime, core.VRTM, clock),
		derivedio.Str(tagContentCreatorName, core.VRPN, state.ContentCreatorName),
	}
	if state.Laterality != "" {
		elements = append(elements, derivedio.CS(tagLaterality, state.Laterality))
	}
	return elements, nil
}

func readIdentification(obj *object.Object, state *State) {
	state.SeriesNumber = derivedio.Int(obj, tagSeriesNumber)
	state.InstanceNumber = derivedio.Int(obj, tagInstanceNumber)
	state.Laterality = derivedio.CleanString(obj, tagLaterality)
	state.ContentLabel = derivedio.CleanString(obj, tagContentLabel)
	state.ContentDescription = derivedio.CleanString(obj, tagContentDescription)
	state.CreationDate = derivedio.CleanString(obj, tagPresentationCreationDate)
	state.CreationTime = derivedio.CleanString(obj, tagPresentationCreationTime)
	state.ContentCreatorName = derivedio.CleanString(obj, tagContentCreatorName)
}

// validCS reports whether value is a single Code String value: at most 16
// upper-case letters, digits, spaces or underscores, not blank.
func validCS(value string) bool {
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

// resolvedGraphicLayers returns every layer of the state, ordered for
// drawing: the defined layers, then those only named by annotations or
// overlay activations. Layers without an order follow the ordered ones.
func resolvedGraphicLayers(state *State) ([]GraphicLayer, error) {
	layers := make([]GraphicLayer, 0, len(state.GraphicLayers)+1)
	index := map[string]int{}
	for i, layer := range state.GraphicLayers {
		if !validCS(layer.Name) {
			return nil, fmt.Errorf("%w: graphic layer %d name %q is not a CS value", ErrInvalidObject, i, layer.Name)
		}
		if _, ok := index[layer.Name]; ok {
			return nil, fmt.Errorf("%w: graphic layer %q is defined twice", ErrInvalidObject, layer.Name)
		}
		if layer.Order < 0 {
			return nil, fmt.Errorf("%w: graphic layer %q has a negative order", ErrInvalidObject, layer.Name)
		}
		index[layer.Name] = len(layers)
		layers = append(layers, layer)
	}
	used := func(name string) error {
		name = graphicLayerName(name)
		if !validCS(name) {
			return fmt.Errorf("%w: graphic layer name %q is not a CS value", ErrInvalidObject, name)
		}
		if _, ok := index[name]; !ok {
			index[name] = len(layers)
			layers = append(layers, GraphicLayer{Name: name})
		}
		return nil
	}
	for _, item := range state.GraphicAnnotations {
		if err := used(item.LayerName); err != nil {
			return nil, err
		}
	}
	for _, activation := range state.OverlayActivations {
		if err := used(activation.Layer); err != nil {
			return nil, err
		}
	}
	next := 0
	for _, layer := range layers {
		next = max(next, layer.Order)
	}
	for i := range layers {
		if layers[i].Order == 0 {
			next++
			layers[i].Order = next
		}
	}
	sort.SliceStable(layers, func(i, j int) bool { return layers[i].Order < layers[j].Order })
	return layers, nil
}

func graphicLayerSequence(layers []GraphicLayer) core.Element {
	items := make([]core.DataSet, 0, len(layers))
	for _, layer := range layers {
		elements := []core.Element{
			derivedio.CS(tagGraphicLayer, layer.Name),
			derivedio.IS(tagGraphicLayerOrder, layer.Order),
		}
		if layer.RecommendedGrayscale != nil {
			elements = append(elements, derivedio.US(tagGraphicLayerRecommendedGrayscale, *layer.RecommendedGrayscale))
		}
		if lab := layer.RecommendedCIELab; lab != nil {
			elements = append(elements, derivedio.US(tagGraphicLayerRecommendedCIELab, lab[0], lab[1], lab[2]))
		}
		if layer.Description != "" {
			elements = append(elements, derivedio.LO(tagGraphicLayerDescription, layer.Description))
		}
		items = append(items, derivedio.DataSet(elements...))
	}
	return derivedio.Seq(tagGraphicLayerSequence, items...)
}

func readGraphicLayers(obj *object.Object) []GraphicLayer {
	var out []GraphicLayer
	for _, item := range derivedio.Sequence(obj, tagGraphicLayerSequence) {
		layer := GraphicLayer{
			Name:        derivedio.CleanString(item, tagGraphicLayer),
			Order:       derivedio.Int(item, tagGraphicLayerOrder),
			Description: derivedio.CleanString(item, tagGraphicLayerDescription),
		}
		if values := derivedio.Ints(item, tagGraphicLayerRecommendedGrayscale); len(values) > 0 {
			value := uint16(values[0])
			layer.RecommendedGrayscale = &value
		}
		if values := derivedio.Ints(item, tagGraphicLayerRecommendedCIELab); len(values) == 3 {
			layer.RecommendedCIELab = &[3]uint16{uint16(values[0]), uint16(values[1]), uint16(values[2])}
		}
		out = append(out, layer)
	}
	return out
}

func validateTextObject(item GraphicAnnotation) error {
	if utf8.RuneCountInString(item.Text) > maxTextLength {
		return fmt.Errorf("%w: text of %d characters exceeds the ST limit of %d", ErrInvalidObject, utf8.RuneCountInString(item.Text), maxTextLength)
	}
	if box := item.BoundingBox; box != nil {
		for _, point := range []Point2D{box.TopLeft, box.BottomRight} {
			if err := validateAnnotationPoint(point, box.Units); err != nil {
				return fmt.Errorf("bounding box: %w", err)
			}
		}
		switch normalizedJustification(box.Justification) {
		case JustifyLeft, JustifyRight, JustifyCenter:
		default:
			return fmt.Errorf("%w: text justification %q", ErrInvalidObject, box.Justification)
		}
	}
	if hasAnchor(item) {
		if err := validateAnnotationPoint(item.Anchor, item.AnchorUnits); err != nil {
			return fmt.Errorf("anchor: %w", err)
		}
	}
	return nil
}

// hasAnchor reports whether a text object has an anchor point: always
// without a bounding box, and with one when the anchor is visible or not
// at the origin.
func hasAnchor(item GraphicAnnotation) bool {
	return item.BoundingBox == nil || item.AnchorVisible || item.Anchor != (Point2D{})
}

func normalizedJustification(value string) string {
	value = strings.ToUpper(strings.TrimSpace(value))
	if value == "" {
		return JustifyLeft
	}
	return value
}

func textObjectItem(item GraphicAnnotation) core.DataSet {
	elements := append(compoundLinkElements(item), derivedio.Str(tagUnformattedTextValue, core.VRST, item.Text))
	if box := item.BoundingBox; box != nil {
		elements = append(elements,
			derivedio.CS(tagBoundingBoxAnnotationUnits, normalizedAnnotationUnits(box.Units)),
			derivedio.FL(tagBoundingBoxTopLeft, box.TopLeft.X, box.TopLeft.Y),
			derivedio.FL(tagBoundingBoxBottomRight, box.BottomRight.X, box.BottomRight.Y),
			derivedio.CS(tagBoundingBoxJustification, normalizedJustification(box.Justification)),
		)
	}
	if hasAnchor(item) {
		elements = append(elements,
			derivedio.CS(tagAnchorPointAnnotationUnits, normalizedAnnotationUnits(item.AnchorUnits)),
			derivedio.FL(tagAnchorPoint, item.Anchor.X, item.Anchor.Y),
			derivedio.CS(tagAnchorPointVisibility, flipValue(item.AnchorVisible)),
		)
	}
	return derivedio.DataSet(elements...)
}

func readBoundingBox(item *object.Object) *BoundingBox {
	topLeft := derivedio.Floats(item, tagBoundingBoxTopLeft)
	bottomRight := derivedio.Floats(item, tagBoundingBoxBottomRight)
	if len(topLeft) < 2 || len(bottomRight) < 2 {
		return nil
	}
	return &BoundingBox{
		TopLeft:       Point2D{X: topLeft[0], Y: topLeft[1]},
		BottomRight:   Point2D{X: bottomRight[0], Y: bottomRight[1]},
		Units:         normalizedAnnotationUnits(derivedio.CleanString(item, tagBoundingBoxAnnotationUnits)),
		Justification: normalizedJustification(derivedio.CleanString(item, tagBoundingBoxJustification)),
	}
}

func validateModalityLUT(m *ModalityLUT) error {
	if m == nil {
		return nil
	}
	if m.LUT != nil {
		if m.LUT.BitsPerEntry != 8 && m.LUT.BitsPerEntry != 16 {
			return fmt.Errorf("%w: Modality LUT has %d bits per entry, not 8 or 16", ErrInvalidObject, m.LUT.BitsPerEntry)
		}
		return validateLUT("Modality LUT", m.LUT, -(1 << 15))
	}
	if m.Slope == 0 || math.IsNaN(m.Slope) || math.IsInf(m.Slope, 0) || math.IsNaN(m.Intercept) || math.IsInf(m.Intercept, 0) {
		return fmt.Errorf("%w: Modality LUT rescale %g/%g", ErrInvalidObject, m.Slope, m.Intercept)
	}
	return nil
}

func validateLUT(name string, lut *display.LUT, minFirstMapped int) error {
	if lut.FirstMapped < minFirstMapped || lut.FirstMapped > 1<<16-1 {
		return fmt.Errorf("%w: %s first mapped value %d", ErrInvalidObject, name, lut.FirstMapped)
	}
	if len(lut.Entries) == 0 || len(lut.Entries) > 1<<16 {
		return fmt.Errorf("%w: %s has %d entries", ErrInvalidObject, name, len(lut.Entries))
	}
	count := len(lut.Entries)
	if count == 1<<16 {
		count = 0
	}
	if _, err := display.NewLUT([]int{count, lut.FirstMapped, lut.BitsPerEntry}, lut.Entries); err != nil {
		return fmt.Errorf("dicom/gsps: %s: %w", name, err)
	}
	return nil
}

func modalityLUTElements(m *ModalityLUT) []core.Element {
	if m == nil {
		return nil
	}
	if m.LUT != nil {
		lutType := m.LUTType
		if lutType == "" {
			lutType = "US"
		}
		item := lutItemElements(m.LUT, m.Explanation)
		item = append(item, derivedio.LO(tagModalityLUTType, lutType))
		return []core.Element{derivedio.Seq(tagModalityLUTSequence, derivedio.DataSet(item...))}
	}
	rescaleType := m.RescaleType
	if rescaleType == "" {
		rescaleType = "US"
	}
	return []core.Element{
		derivedio.DS(tagRescaleIntercept, m.Intercept),
		derivedio.DS(tagRescaleSlope, m.Slope),
		derivedio.LO(tagRescaleType, rescaleType),
	}
}

func readModalityLUT(obj *object.Object) (*ModalityLUT, error) {
	_, hasSlope := obj.Get(tagRescaleSlope)
	_, hasIntercept := obj.Get(tagRescaleIntercept)
	items := derivedio.Sequence(obj, tagModalityLUTSequence)
	if !hasSlope && !hasIntercept && len(items) == 0 {
		return nil, nil
	}
	transform, err := display.ModalityLUTFromObject(obj)
	if err != nil {
		return nil, fmt.Errorf("dicom/gsps: Modality LUT: %w", err)
	}
	out := &ModalityLUT{Slope: transform.Slope, Intercept: transform.Intercept, RescaleType: derivedio.CleanString(obj, tagRescaleType), LUT: transform.LUT}
	if len(items) > 0 {
		out.LUTType = derivedio.CleanString(items[0], tagModalityLUTType)
		out.Explanation = derivedio.CleanString(items[0], tagLUTExplanation)
	}
	return out, nil
}

// validatePresentationLUT checks a Presentation LUT Sequence table: its
// input starts at 0 and its entries have 10 to 16 bits (PS3.3 C.11.6.1.1).
func validatePresentationLUT(lut *display.LUT) error {
	if lut == nil {
		return nil
	}
	if lut.FirstMapped != 0 {
		return fmt.Errorf("%w: Presentation LUT first mapped value %d is not 0", ErrInvalidObject, lut.FirstMapped)
	}
	if lut.BitsPerEntry < 10 || lut.BitsPerEntry > 16 {
		return fmt.Errorf("%w: Presentation LUT has %d bits per entry, not 10 to 16", ErrInvalidObject, lut.BitsPerEntry)
	}
	return validateLUT("Presentation LUT", lut, 0)
}

func presentationLUTSequence(lut *display.LUT, explanation string) core.Element {
	return derivedio.Seq(tagPresentationLUTSequence, derivedio.DataSet(lutItemElements(lut, explanation)...))
}

func readPresentationLUT(obj *object.Object) (*display.LUT, string, error) {
	items := derivedio.Sequence(obj, tagPresentationLUTSequence)
	if len(items) == 0 {
		return nil, "", nil
	}
	lut, err := display.LUTFromItem(items[0], false)
	if err != nil {
		return nil, "", fmt.Errorf("dicom/gsps: Presentation LUT: %w", err)
	}
	return lut, derivedio.CleanString(items[0], tagLUTExplanation), nil
}

// lutItemElements encodes a LUT Descriptor, LUT Explanation and LUT Data;
// the descriptor is SS when the first mapped value is negative.
func lutItemElements(lut *display.LUT, explanation string) []core.Element {
	count := len(lut.Entries)
	if count == 1<<16 {
		count = 0
	}
	descriptorVR := core.VRUS
	if lut.FirstMapped < 0 {
		descriptorVR = core.VRSS
	}
	var descriptor [6]byte
	binary.LittleEndian.PutUint16(descriptor[0:2], uint16(count))
	binary.LittleEndian.PutUint16(descriptor[2:4], uint16(lut.FirstMapped))
	binary.LittleEndian.PutUint16(descriptor[4:6], uint16(lut.BitsPerEntry))
	elements := []core.Element{core.NewRawElement(tagLUTDescriptor, descriptorVR, descriptor[:])}
	if explanation != "" {
		elements = append(elements, derivedio.LO(tagLUTExplanation, explanation))
	}
	return append(elements, derivedio.Raw(tagLUTData, core.VROW, display.EncodeLUTData(lut.Entries, lut.BitsPerEntry)))
}
