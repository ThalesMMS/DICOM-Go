package gsps

import (
	"fmt"
	"math"
	"strings"
	"unicode/utf8"

	"github.com/ThalesMMS/dicom-go/core"
	"github.com/ThalesMMS/dicom-go/internal/derivedio"
	"github.com/ThalesMMS/dicom-go/object"
)

const (
	CompoundTypeMultiline    = "MULTILINE"
	CompoundTypeInfiniteLine = "INFINITELINE"
	CompoundTypeCutline      = "CUTLINE"
	CompoundTypeRangeLine    = "RANGELINE"
	CompoundTypeRuler        = "RULER"
	CompoundTypeAxis         = "AXIS"
	CompoundTypeCrosshair    = "CROSSHAIR"
	CompoundTypeArrow        = "ARROW"
	CompoundTypeRectangle    = "RECTANGLE"
	CompoundTypeEllipse      = "ELLIPSE"

	// The writer uses Explicit VR. Each FL point is eight bytes, and the
	// Graphic Data element has a 16-bit value length in that syntax.
	maxWrittenCompoundPoints = (1<<16 - 2) / 8
)

var (
	tagCompoundGraphicSequence   = core.NewTag(0x0070, 0x0209)
	tagCompoundGraphicInstanceID = core.NewTag(0x0070, 0x0226)
	tagCompoundRotationAngle     = core.NewTag(0x0070, 0x0230)
	tagGraphicGroupSequence      = core.NewTag(0x0070, 0x0234)
	tagGraphicGroupLabel         = core.NewTag(0x0070, 0x0207)
	tagGraphicGroupDescription   = core.NewTag(0x0070, 0x0208)
	tagCompoundGapLength         = core.NewTag(0x0070, 0x0261)
	tagDiameterOfVisibility      = core.NewTag(0x0070, 0x0262)
	tagCompoundRotationPoint     = core.NewTag(0x0070, 0x0273)
	tagTickAlignment             = core.NewTag(0x0070, 0x0274)
	tagShowTickLabel             = core.NewTag(0x0070, 0x0278)
	tagTickLabelAlignment        = core.NewTag(0x0070, 0x0279)
	tagCompoundGraphicUnits      = core.NewTag(0x0070, 0x0282)
	tagMajorTicksSequence        = core.NewTag(0x0070, 0x0287)
	tagTickPosition              = core.NewTag(0x0070, 0x0288)
	tagTickLabel                 = core.NewTag(0x0070, 0x0289)
	tagCompoundGraphicType       = core.NewTag(0x0070, 0x0294)
	tagGraphicGroupID            = core.NewTag(0x0070, 0x0295)
)

// CompoundGraphic is an item of the Compound Graphic Sequence (PS3.3 C.10.5).
// The containing GraphicAnnotation holds its local numeric link, layer and
// image references. A nil conditional attribute represents absence on the wire.
type CompoundGraphic struct {
	Type, Units          string
	Points               []Point2D
	Filled               *bool
	RotationAngle        *float64
	RotationPoint        *Point2D
	GapLength            *float64
	DiameterOfVisibility *float64
	TickAlignment        string
	TickLabelAlignment   string
	ShowTickLabel        *bool
	MajorTicks           []CompoundTick
	LineStyle            *CompoundLineStyle
	FillStyle            *CompoundFillStyle
	TextStyle            *CompoundTextStyle
	malformed            bool
}

// CompoundTick describes a labeled axis tick between the two endpoints.
type CompoundTick struct {
	Position float64
	Label    string
}

// GraphicGroup describes an association of annotations (PS3.3 C.10.11).
type GraphicGroup struct {
	ID          uint32
	Label       string
	Description string
}

func standardCompoundType(value string) bool {
	switch value {
	case CompoundTypeMultiline, CompoundTypeInfiniteLine, CompoundTypeCutline,
		CompoundTypeRangeLine, CompoundTypeRuler, CompoundTypeAxis, CompoundTypeCrosshair,
		CompoundTypeArrow, CompoundTypeRectangle, CompoundTypeEllipse:
		return true
	default:
		return false
	}
}

func (a GraphicAnnotation) compoundGraphicIDPresent() bool {
	return a.HasCompoundGraphicID || a.CompoundGraphicID != 0
}

func (a GraphicAnnotation) graphicGroupIDPresent() bool {
	return a.HasGraphicGroupID || a.GraphicGroupID != 0
}

func (a GraphicAnnotation) sameGraphicGroup(other GraphicAnnotation) bool {
	return a.graphicGroupIDPresent() == other.graphicGroupIDPresent() && a.GraphicGroupID == other.GraphicGroupID
}

func readCompoundGraphics(item *object.Object, layer string) []GraphicAnnotation {
	var out []GraphicAnnotation
	for _, obj := range derivedio.Sequence(item, tagCompoundGraphicSequence) {
		annotation := GraphicAnnotation{LayerName: layer, Compound: &CompoundGraphic{}}
		c := annotation.Compound
		linkOK := readCompoundLink(obj, &annotation)
		if !linkOK || !annotation.compoundGraphicIDPresent() || item.HasDuplicateParsedTag(tagCompoundGraphicSequence) {
			c.malformed = true
		}
		var ok bool
		c.Type, ok = readCompoundString(obj, tagCompoundGraphicType, core.VRCS)
		c.malformed = c.malformed || !ok
		c.Units, ok = readCompoundString(obj, tagCompoundGraphicUnits, core.VRCS)
		c.malformed = c.malformed || !ok
		c.Points, ok = readCompoundPoints(obj)
		c.malformed = c.malformed || !ok
		c.Filled, ok = readCompoundBool(obj, tagGraphicFilled)
		c.malformed = c.malformed || !ok
		c.RotationAngle, ok = readCompoundFloat(obj, tagCompoundRotationAngle, core.VRFD)
		c.malformed = c.malformed || !ok
		c.RotationPoint, ok = readCompoundRotationPoint(obj)
		c.malformed = c.malformed || !ok
		c.GapLength, ok = readCompoundFloat(obj, tagCompoundGapLength, core.VRFL)
		c.malformed = c.malformed || !ok
		c.DiameterOfVisibility, ok = readCompoundFloat(obj, tagDiameterOfVisibility, core.VRFL)
		c.malformed = c.malformed || !ok
		c.TickAlignment, ok = readCompoundString(obj, tagTickAlignment, core.VRCS)
		c.malformed = c.malformed || !ok
		c.TickLabelAlignment, ok = readCompoundString(obj, tagTickLabelAlignment, core.VRCS)
		c.malformed = c.malformed || !ok
		c.ShowTickLabel, ok = readCompoundBool(obj, tagShowTickLabel)
		c.malformed = c.malformed || !ok
		c.MajorTicks, ok = readCompoundTicks(obj)
		c.malformed = c.malformed || !ok
		if !readCompoundStyles(obj, c) {
			c.malformed = true
		}
		out = append(out, annotation)
	}
	return out
}

func readCompoundRotationPoint(obj *object.Object) (*Point2D, bool) {
	e, present := obj.Get(tagCompoundRotationPoint)
	if !present {
		return nil, true
	}
	if e.VR() != core.VRFL || e.Length() != 8 || obj.HasDuplicateParsedTag(tagCompoundRotationPoint) {
		return nil, false
	}
	xy := derivedio.Floats(obj, tagCompoundRotationPoint)
	if len(xy) != 2 {
		return nil, false
	}
	return &Point2D{X: xy[0], Y: xy[1]}, true
}

func readCompoundPoints(obj *object.Object) ([]Point2D, bool) {
	dimensions, dimensionsOK := readCompoundInt(obj, tagGraphicDimensions, core.VRUS)
	count, countOK := readCompoundInt(obj, tagNumberOfGraphicPoints, core.VRUS)
	e, present := obj.Get(tagGraphicData)
	if !dimensionsOK || dimensions == nil || *dimensions != 2 || !countOK || count == nil || *count <= 0 ||
		!present || e.VR() != core.VRFL || obj.HasDuplicateParsedTag(tagGraphicData) || e.Length() != core.Length(*count*8) {
		return nil, false
	}
	raw := derivedio.Floats(obj, tagGraphicData)
	if int64(len(raw)) != *count*2 {
		return nil, false
	}
	points := make([]Point2D, int(*count))
	for i := range points {
		points[i] = Point2D{X: raw[i*2], Y: raw[i*2+1]}
	}
	return points, true
}

func readCompoundTicks(obj *object.Object) ([]CompoundTick, bool) {
	e, present := obj.Get(tagMajorTicksSequence)
	if !present {
		return nil, true
	}
	items := derivedio.Sequence(obj, tagMajorTicksSequence)
	if e.VR() != core.VRSQ || obj.HasDuplicateParsedTag(tagMajorTicksSequence) || len(items) == 0 {
		return nil, false
	}
	var ticks []CompoundTick
	ok := true
	for _, tick := range items {
		position, positionOK := readCompoundFloat(tick, tagTickPosition, core.VRFL)
		label, labelOK := readCompoundString(tick, tagTickLabel, core.VRSH)
		if !positionOK || position == nil || !labelOK || label == "" {
			ok = false
			continue
		}
		ticks = append(ticks, CompoundTick{Position: *position, Label: label})
	}
	return ticks, ok
}

func readCompoundLink(obj *object.Object, a *GraphicAnnotation) bool {
	id, idOK := readCompoundUint32(obj, tagCompoundGraphicInstanceID)
	group, groupOK := readCompoundUint32(obj, tagGraphicGroupID)
	if id != nil {
		a.CompoundGraphicID = *id
		a.HasCompoundGraphicID = true
	}
	if group != nil {
		a.GraphicGroupID = *group
		a.HasGraphicGroupID = true
	}
	return idOK && groupOK
}

func readCompoundString(obj *object.Object, tag core.Tag, vr core.VR) (string, bool) {
	e, present := obj.Get(tag)
	if !present {
		return "", true
	}
	var maxBytes core.Length
	switch vr {
	case core.VRCS:
		maxBytes = 16
	case core.VRSH:
		maxBytes = 16 * 8
	case core.VRLO:
		maxBytes = 64 * 8
	case core.VRST:
		maxBytes = maxTextLength * 8
	}
	if e.VR() != vr || e.Length() > maxBytes || obj.HasDuplicateParsedTag(tag) {
		return "", false
	}
	values, err := obj.LookupStrings(tag)
	if err != nil || len(values) != 1 {
		return "", false
	}
	if vr == core.VRCS {
		return strings.TrimSpace(values[0]), true
	}
	return values[0], true
}

func readCompoundInt(obj *object.Object, tag core.Tag, vr core.VR) (*int64, bool) {
	e, present := obj.Get(tag)
	if !present {
		return nil, true
	}
	var width core.Length = 4
	if vr == core.VRUS {
		width = 2
	}
	if e.VR() != vr || e.Length() != width || obj.HasDuplicateParsedTag(tag) {
		return nil, false
	}
	values, err := obj.GetInts(tag)
	if err != nil || len(values) != 1 {
		return nil, false
	}
	return &values[0], true
}

func readCompoundUint32(obj *object.Object, tag core.Tag) (*uint32, bool) {
	value, ok := readCompoundInt(obj, tag, core.VRUL)
	if value == nil {
		return nil, ok
	}
	if *value < 0 || *value > math.MaxUint32 {
		return nil, false
	}
	word := uint32(*value)
	return &word, true
}

func readCompoundFloat(obj *object.Object, tag core.Tag, vr core.VR) (*float64, bool) {
	e, present := obj.Get(tag)
	if !present {
		return nil, true
	}
	var width core.Length = 4
	if vr == core.VRFD {
		width = 8
	}
	if e.VR() != vr || e.Length() != width || obj.HasDuplicateParsedTag(tag) {
		return nil, false
	}
	values, err := obj.GetFloats(tag)
	if err != nil || len(values) != 1 {
		return nil, false
	}
	return &values[0], true
}

func readCompoundBool(obj *object.Object, tag core.Tag) (*bool, bool) {
	if !obj.Has(tag) {
		return nil, true
	}
	value, ok := readCompoundString(obj, tag, core.VRCS)
	if !ok || (value != "Y" && value != "N") {
		return nil, false
	}
	yes := value == "Y"
	return &yes, true
}

func compoundLinkElements(a GraphicAnnotation) []core.Element {
	var elements []core.Element
	if a.compoundGraphicIDPresent() {
		elements = append(elements, derivedio.UL(tagCompoundGraphicInstanceID, a.CompoundGraphicID))
	}
	if a.graphicGroupIDPresent() {
		elements = append(elements, derivedio.UL(tagGraphicGroupID, a.GraphicGroupID))
	}
	return elements
}

func compoundGraphicItem(a GraphicAnnotation) core.DataSet {
	c := a.Compound
	elements := append(compoundLinkElements(a),
		derivedio.CS(tagCompoundGraphicType, c.Type),
		derivedio.CS(tagCompoundGraphicUnits, c.Units),
		derivedio.US(tagGraphicDimensions, 2),
		derivedio.US(tagNumberOfGraphicPoints, uint16(len(c.Points))),
		derivedio.FL(tagGraphicData, flattenPoints(c.Points)...),
	)
	if c.RotationAngle != nil {
		elements = append(elements, derivedio.FD(tagCompoundRotationAngle, *c.RotationAngle))
	}
	if p := c.RotationPoint; p != nil {
		elements = append(elements, derivedio.FL(tagCompoundRotationPoint, p.X, p.Y))
	}
	for _, attribute := range []struct {
		tag   core.Tag
		value *float64
	}{
		{tagCompoundGapLength, c.GapLength},
		{tagDiameterOfVisibility, c.DiameterOfVisibility},
	} {
		if attribute.value != nil {
			elements = append(elements, derivedio.FL(attribute.tag, *attribute.value))
		}
	}
	if c.Filled != nil {
		elements = append(elements, derivedio.CS(tagGraphicFilled, flipValue(*c.Filled)))
	}
	if c.ShowTickLabel != nil {
		elements = append(elements, derivedio.CS(tagShowTickLabel, flipValue(*c.ShowTickLabel)))
	}
	if c.TickAlignment != "" {
		elements = append(elements, derivedio.CS(tagTickAlignment, c.TickAlignment))
	}
	if c.TickLabelAlignment != "" {
		elements = append(elements, derivedio.CS(tagTickLabelAlignment, c.TickLabelAlignment))
	}
	if len(c.MajorTicks) > 0 {
		ticks := make([]core.DataSet, 0, len(c.MajorTicks))
		for _, tick := range c.MajorTicks {
			ticks = append(ticks, derivedio.DataSet(derivedio.FL(tagTickPosition, tick.Position),
				derivedio.Str(tagTickLabel, core.VRSH, tick.Label)))
		}
		elements = append(elements, derivedio.Seq(tagMajorTicksSequence, ticks...))
	}
	return derivedio.DataSet(append(elements, compoundStyleElements(c)...)...)
}

func validateCompoundGraphic(c *CompoundGraphic) error {
	if c == nil || c.malformed || !validCS(c.Type) ||
		(c.Units != AnnotationUnitsPixel && c.Units != AnnotationUnitsDisplay) || len(c.Points) == 0 {
		return fmt.Errorf("%w: incomplete Compound Graphic", ErrInvalidObject)
	}
	for _, point := range c.Points {
		if !validCompoundPoint(point, c.Units) {
			return fmt.Errorf("%w: invalid Compound Graphic point", ErrInvalidObject)
		}
	}
	switch c.Type {
	case CompoundTypeMultiline:
		if len(c.Points)%2 != 0 {
			return fmt.Errorf("%w: MULTILINE requires pairs of points", ErrInvalidObject)
		}
	case CompoundTypeCrosshair:
		if len(c.Points) != 1 {
			return fmt.Errorf("%w: CROSSHAIR requires one point", ErrInvalidObject)
		}
	case CompoundTypeRectangle, CompoundTypeEllipse, CompoundTypeInfiniteLine, CompoundTypeCutline,
		CompoundTypeRangeLine, CompoundTypeRuler, CompoundTypeAxis, CompoundTypeArrow:
		if len(c.Points) != 2 {
			return fmt.Errorf("%w: Compound Graphic requires two points", ErrInvalidObject)
		}
		if float32(c.Points[0].X) == float32(c.Points[1].X) && float32(c.Points[0].Y) == float32(c.Points[1].Y) {
			return fmt.Errorf("%w: Compound Graphic endpoints must differ", ErrInvalidObject)
		}
	}
	if c.Type == CompoundTypeRectangle || c.Type == CompoundTypeEllipse {
		if c.Filled == nil || (*c.Filled && c.FillStyle == nil) {
			return fmt.Errorf("%w: rectangle or ellipse is missing its fill attributes", ErrInvalidObject)
		}
		if c.Points[1].X <= c.Points[0].X || c.Points[1].Y <= c.Points[0].Y {
			return fmt.Errorf("%w: invalid Compound Graphic bounding rectangle", ErrInvalidObject)
		}
	}
	gapLine := c.Type == CompoundTypeCutline || c.Type == CompoundTypeInfiniteLine
	if c.RotationAngle != nil && (!isFinite(*c.RotationAngle) || *c.RotationAngle < 0 || *c.RotationAngle > 360) {
		return fmt.Errorf("%w: invalid Compound Graphic rotation", ErrInvalidObject)
	}
	if ((c.RotationAngle != nil || gapLine) && c.RotationPoint == nil) ||
		(c.RotationPoint != nil && !validCompoundPoint(*c.RotationPoint, c.Units)) {
		return fmt.Errorf("%w: missing or invalid Compound Graphic rotation point", ErrInvalidObject)
	}
	if ((gapLine || c.Type == CompoundTypeCrosshair) && c.GapLength == nil) ||
		(c.GapLength != nil && (!validCompoundFL(*c.GapLength) || *c.GapLength < 0)) {
		return fmt.Errorf("%w: missing or invalid Compound Graphic gap", ErrInvalidObject)
	}
	if (c.Type == CompoundTypeCrosshair && c.DiameterOfVisibility == nil) ||
		(c.DiameterOfVisibility != nil && (!validCompoundFL(*c.DiameterOfVisibility) || *c.DiameterOfVisibility <= 0)) {
		return fmt.Errorf("%w: missing or invalid CROSSHAIR diameter", ErrInvalidObject)
	}
	if err := validateCompoundTicks(c); err != nil {
		return err
	}
	return validateCompoundStyles(c)
}

func validCompoundPoint(point Point2D, units string) bool {
	return validCompoundFL(point.X) && validCompoundFL(point.Y) && point.X >= 0 && point.Y >= 0 &&
		(units != AnnotationUnitsDisplay || (point.X <= 1 && point.Y <= 1))
}

func isFinite(value float64) bool {
	return !math.IsInf(value, 0) && !math.IsNaN(value)
}

func validCompoundFL(value float64) bool {
	return isFinite(value) && math.Abs(value) <= math.MaxFloat32
}

func validateCompoundTicks(c *CompoundGraphic) error {
	if c.Type == CompoundTypeAxis || c.Type == CompoundTypeCrosshair || c.Type == CompoundTypeRuler {
		if c.ShowTickLabel == nil || (c.TickAlignment != "TOP" && c.TickAlignment != "BOTTOM" && c.TickAlignment != "CENTER") ||
			(c.TickLabelAlignment != "TOP" && c.TickLabelAlignment != "BOTTOM") ||
			(c.Type == CompoundTypeCrosshair && c.TickAlignment != "CENTER") {
			return fmt.Errorf("%w: invalid Compound Graphic tick alignment or visibility", ErrInvalidObject)
		}
	}
	if c.Type == CompoundTypeAxis && len(c.MajorTicks) < 2 {
		return fmt.Errorf("%w: AXIS requires at least two major ticks", ErrInvalidObject)
	}
	for _, tick := range c.MajorTicks {
		if !unitInterval(tick.Position) || strings.TrimSpace(tick.Label) == "" ||
			!utf8.ValidString(tick.Label) || utf8.RuneCountInString(tick.Label) > 16 || strings.ContainsRune(tick.Label, '\\') {
			return fmt.Errorf("%w: invalid Compound Graphic major tick", ErrInvalidObject)
		}
	}
	return nil
}

// compoundIndexByID is called after the writer has established uniqueness.
func compoundIndexByID(items []GraphicAnnotation) map[uint32]int {
	indexes := make(map[uint32]int)
	for i, a := range items {
		if a.Compound != nil {
			indexes[a.CompoundGraphicID] = i
		}
	}
	return indexes
}

func validateCompoundLinks(items []GraphicAnnotation) error {
	indexes := make(map[uint32]int)
	for i, a := range items {
		if a.malformedCompoundLink {
			return fmt.Errorf("%w: annotation %d has invalid graphic linkage", ErrInvalidObject, i)
		}
		if a.Compound == nil {
			continue
		}
		if !a.compoundGraphicIDPresent() {
			return fmt.Errorf("%w: Compound Graphic requires its instance ID", ErrInvalidObject)
		}
		if _, exists := indexes[a.CompoundGraphicID]; exists {
			return fmt.Errorf("%w: duplicate Compound Graphic instance ID %d", ErrInvalidObject, a.CompoundGraphicID)
		}
		indexes[a.CompoundGraphicID] = i
	}

	alternatives := make(map[uint32]bool, len(indexes))
	for i, a := range items {
		if a.Compound != nil || !a.compoundGraphicIDPresent() {
			continue
		}
		compoundIndex, exists := indexes[a.CompoundGraphicID]
		if !exists {
			return fmt.Errorf("%w: annotation %d has no corresponding Compound Graphic", ErrInvalidObject, i)
		}
		compound := items[compoundIndex]
		if graphicLayerName(a.LayerName) != graphicLayerName(compound.LayerName) ||
			max(a.GroupIndex, 0) != max(compound.GroupIndex, 0) ||
			!a.sameGraphicGroup(compound) || !referencedImagesEqual(a.ReferencedImages, compound.ReferencedImages) {
			return fmt.Errorf("%w: annotation %d alternative does not share its compound's scope", ErrInvalidObject, i)
		}
		if a.Text != "" || len(a.Polyline) > 0 {
			alternatives[a.CompoundGraphicID] = true
		}
	}
	for _, a := range items {
		if a.Compound != nil && !alternatives[a.CompoundGraphicID] {
			return fmt.Errorf("%w: Compound Graphic %d requires a text or graphic alternative", ErrInvalidObject, a.CompoundGraphicID)
		}
	}
	return nil
}

func readGraphicGroups(obj *object.Object) []GraphicGroup {
	var groups []GraphicGroup
	for _, item := range derivedio.Sequence(obj, tagGraphicGroupSequence) {
		id, ok := readCompoundUint32(item, tagGraphicGroupID)
		label, labelOK := readCompoundString(item, tagGraphicGroupLabel, core.VRLO)
		description, descriptionOK := readCompoundString(item, tagGraphicGroupDescription, core.VRST)
		if ok && id != nil && labelOK && label != "" && descriptionOK {
			groups = append(groups, GraphicGroup{ID: *id, Label: label, Description: description})
		}
	}
	return groups
}

func graphicGroupSequence(state *State) (core.Element, bool, error) {
	groups := make(map[uint32]bool, len(state.GraphicGroups))
	var sets []core.DataSet
	for _, group := range state.GraphicGroups {
		if strings.TrimSpace(group.Label) == "" || !utf8.ValidString(group.Label) ||
			utf8.RuneCountInString(group.Label) > 64 || strings.ContainsRune(group.Label, '\\') ||
			!utf8.ValidString(group.Description) || utf8.RuneCountInString(group.Description) > maxTextLength {
			return core.Element{}, false, fmt.Errorf("%w: invalid Graphic Group label or description", ErrInvalidObject)
		}
		if groups[group.ID] {
			return core.Element{}, false, fmt.Errorf("%w: duplicate Graphic Group ID %d", ErrInvalidObject, group.ID)
		}
		groups[group.ID] = true
		attrs := []core.Element{derivedio.UL(tagGraphicGroupID, group.ID), derivedio.LO(tagGraphicGroupLabel, group.Label)}
		if group.Description != "" {
			attrs = append(attrs, derivedio.Str(tagGraphicGroupDescription, core.VRST, group.Description))
		}
		sets = append(sets, derivedio.DataSet(attrs...))
	}
	for _, a := range state.GraphicAnnotations {
		if a.graphicGroupIDPresent() && !groups[a.GraphicGroupID] {
			return core.Element{}, false, fmt.Errorf("%w: undefined Graphic Group ID %d", ErrInvalidObject, a.GraphicGroupID)
		}
	}
	return derivedio.Seq(tagGraphicGroupSequence, sets...), len(sets) != 0, nil
}
