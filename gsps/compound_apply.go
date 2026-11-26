package gsps

import (
	"fmt"
	"slices"
)

// selectGraphicAnnotations resolves the alternative encoded for each
// Compound Graphic Instance ID. The identifier is local to this presentation
// state. Layer, annotation item, group and target image scope guard the link.
func selectGraphicAnnotations(state *State, target ImageState) []GraphicAnnotation {
	counts := make(map[uint32]int)
	applicable := make([]GraphicAnnotation, 0, len(state.GraphicAnnotations))
	for _, item := range state.GraphicAnnotations {
		if item.Compound != nil && item.compoundGraphicIDPresent() {
			counts[item.CompoundGraphicID]++
		}
		if len(item.ReferencedImages) == 0 || referencesApply(item.ReferencedImages, target) {
			applicable = append(applicable, item)
		}
	}
	primary := make(map[uint32]GraphicAnnotation, len(counts))
	for _, item := range applicable {
		if item.Compound != nil && !item.malformedCompoundLink && item.compoundGraphicIDPresent() && counts[item.CompoundGraphicID] == 1 &&
			compoundFitsTarget(item.Compound, target) {
			primary[item.CompoundGraphicID] = item
		}
	}
	for _, item := range applicable {
		if item.Compound != nil || !item.compoundGraphicIDPresent() {
			continue
		}
		if compound, exists := primary[item.CompoundGraphicID]; exists &&
			(item.malformedCompoundLink || !sameCompoundScope(item, compound)) {
			// A same-ID alternative whose scope is inconsistent cannot
			// safely be hidden. Keep the simple appearance in that case.
			delete(primary, item.CompoundGraphicID)
		}
	}
	selected := applicable[:0]
	for _, item := range applicable {
		compound, hasPrimary := primary[item.CompoundGraphicID]
		switch {
		case item.Compound != nil:
			if hasPrimary && item.Compound == compound.Compound {
				selected = append(selected, item)
			}
		case !item.malformedCompoundLink && item.compoundGraphicIDPresent() && hasPrimary && sameCompoundScope(item, compound):
			// The state supplies this object as the alternative appearance.
		default:
			selected = append(selected, item)
		}
	}
	return selected
}

func sameCompoundScope(a, b GraphicAnnotation) bool {
	return a.GroupIndex == b.GroupIndex && a.sameGraphicGroup(b) &&
		graphicLayerName(a.LayerName) == graphicLayerName(b.LayerName) &&
		referencedImagesEqual(a.ReferencedImages, b.ReferencedImages)
}

func compoundFitsTarget(graphic *CompoundGraphic, target ImageState) bool {
	if validateCompoundGraphic(graphic) != nil || !standardCompoundType(graphic.Type) ||
		target.Rows <= 0 || target.Columns <= 0 {
		return false
	}
	if normalizedAnnotationUnits(graphic.Units) == AnnotationUnitsPixel {
		inside := func(p Point2D) bool {
			return p.X >= 0 && p.X <= float64(target.Columns) && p.Y >= 0 && p.Y <= float64(target.Rows)
		}
		for _, p := range graphic.Points {
			if !inside(p) {
				return false
			}
		}
		if graphic.RotationPoint != nil && !inside(*graphic.RotationPoint) {
			return false
		}
	}
	return true
}

// CaptureAnnotationState detaches the graphics, geometry and image references
// used by RenderAnnotationOverlay. It admits at most 2^20 list entries across
// the snapshot, including points, nested frame bindings and pattern bytes.
// Pixel, LUT, shutter and patient data are outside the annotation snapshot.
func CaptureAnnotationState(state *State) (*State, error) {
	if state == nil {
		return nil, fmt.Errorf("%w: annotation state is nil", ErrInvalidObject)
	}
	const limit = 1 << 20
	remaining := limit
	reserve := func(n int) bool {
		if n > remaining {
			return false
		}
		remaining -= n
		return true
	}
	referenceBudget := func(refs []ReferencedImage) bool {
		if !reserve(len(refs)) {
			return false
		}
		for _, ref := range refs {
			if !reserve(len(ref.Frames)) {
				return false
			}
		}
		return true
	}
	overBudget := fmt.Errorf("%w: annotation snapshot exceeds %d entries", ErrInvalidObject, limit)
	if !reserve(len(state.GraphicLayers)) || !reserve(len(state.GraphicAnnotations)) ||
		!reserve(len(state.DisplayedAreas)) || !referenceBudget(state.ReferencedImages) ||
		!referenceBudget(state.DisplayedArea.ReferencedImages) {
		return nil, overBudget
	}
	for _, item := range state.GraphicAnnotations {
		if !reserve(len(item.Polyline)) || !referenceBudget(item.ReferencedImages) {
			return nil, overBudget
		}
		if graphic := item.Compound; graphic != nil {
			if !reserve(len(graphic.Points)) || !reserve(len(graphic.MajorTicks)) ||
				(graphic.FillStyle != nil && !reserve(len(graphic.FillStyle.Pattern))) {
				return nil, overBudget
			}
		}
	}
	areaBudget := func(area DisplayedArea) bool {
		return reserve(len(area.PixelSpacing)) && reserve(len(area.PixelAspectRatio))
	}
	if !areaBudget(state.DisplayedArea) {
		return nil, overBudget
	}
	for _, area := range state.DisplayedAreas {
		if !referenceBudget(area.ReferencedImages) || !areaBudget(area) {
			return nil, overBudget
		}
	}
	snapshot := &State{
		SOPClassUID: state.SOPClassUID, SOPInstanceUID: state.SOPInstanceUID,
		SpatialTransform: state.SpatialTransform, ReferencedImages: cloneReferences(state.ReferencedImages),
		DisplayedArea: cloneAnnotationArea(state.DisplayedArea), DisplayedAreas: slices.Clone(state.DisplayedAreas),
		GraphicLayers: slices.Clone(state.GraphicLayers), GraphicAnnotations: slices.Clone(state.GraphicAnnotations),
	}
	for i := range snapshot.DisplayedAreas {
		snapshot.DisplayedAreas[i] = cloneAnnotationArea(snapshot.DisplayedAreas[i])
	}
	for i := range snapshot.GraphicLayers {
		layer := &snapshot.GraphicLayers[i]
		layer.RecommendedGrayscale = cloneAnnotationValue(layer.RecommendedGrayscale)
		layer.RecommendedCIELab = cloneAnnotationValue(layer.RecommendedCIELab)
	}
	for i := range snapshot.GraphicAnnotations {
		item := &snapshot.GraphicAnnotations[i]
		item.Polyline = slices.Clone(item.Polyline)
		item.BoundingBox = cloneAnnotationValue(item.BoundingBox)
		item.ReferencedImages = cloneReferences(item.ReferencedImages)
		item.Compound = cloneCompoundGraphic(item.Compound)
	}
	return snapshot, nil
}

func cloneAnnotationArea(area DisplayedArea) DisplayedArea {
	area.ReferencedImages = cloneReferences(area.ReferencedImages)
	area.PixelSpacing = slices.Clone(area.PixelSpacing)
	area.PixelAspectRatio = slices.Clone(area.PixelAspectRatio)
	return area
}

func cloneAnnotationValue[T any](value *T) *T {
	if value == nil {
		return nil
	}
	copy := *value
	return &copy
}

func cloneCompoundGraphic(value *CompoundGraphic) *CompoundGraphic {
	graphic := cloneAnnotationValue(value)
	if graphic == nil {
		return nil
	}
	graphic.Points = slices.Clone(graphic.Points)
	graphic.Filled = cloneAnnotationValue(graphic.Filled)
	graphic.RotationAngle = cloneAnnotationValue(graphic.RotationAngle)
	graphic.RotationPoint = cloneAnnotationValue(graphic.RotationPoint)
	graphic.GapLength = cloneAnnotationValue(graphic.GapLength)
	graphic.DiameterOfVisibility = cloneAnnotationValue(graphic.DiameterOfVisibility)
	graphic.ShowTickLabel = cloneAnnotationValue(graphic.ShowTickLabel)
	graphic.MajorTicks = slices.Clone(graphic.MajorTicks)
	if style := cloneAnnotationValue(graphic.LineStyle); style != nil {
		style.OnColor = cloneAnnotationValue(style.OnColor)
		style.OffColor = cloneAnnotationValue(style.OffColor)
		style.ShadowColor = cloneAnnotationValue(style.ShadowColor)
		style.OnOpacity = cloneAnnotationValue(style.OnOpacity)
		style.OffOpacity = cloneAnnotationValue(style.OffOpacity)
		style.Thickness = cloneAnnotationValue(style.Thickness)
		style.ShadowOffsetX = cloneAnnotationValue(style.ShadowOffsetX)
		style.ShadowOffsetY = cloneAnnotationValue(style.ShadowOffsetY)
		style.ShadowOpacity = cloneAnnotationValue(style.ShadowOpacity)
		style.Pattern = cloneAnnotationValue(style.Pattern)
		graphic.LineStyle = style
	}
	if style := cloneAnnotationValue(graphic.FillStyle); style != nil {
		style.OnColor = cloneAnnotationValue(style.OnColor)
		style.OffColor = cloneAnnotationValue(style.OffColor)
		style.OnOpacity = cloneAnnotationValue(style.OnOpacity)
		style.OffOpacity = cloneAnnotationValue(style.OffOpacity)
		style.Pattern = slices.Clone(style.Pattern)
		graphic.FillStyle = style
	}
	if style := cloneAnnotationValue(graphic.TextStyle); style != nil {
		style.Color = cloneAnnotationValue(style.Color)
		style.ShadowColor = cloneAnnotationValue(style.ShadowColor)
		style.ShadowOffsetX = cloneAnnotationValue(style.ShadowOffsetX)
		style.ShadowOffsetY = cloneAnnotationValue(style.ShadowOffsetY)
		style.ShadowOpacity = cloneAnnotationValue(style.ShadowOpacity)
		style.Underlined = cloneAnnotationValue(style.Underlined)
		style.Bold = cloneAnnotationValue(style.Bold)
		style.Italic = cloneAnnotationValue(style.Italic)
		graphic.TextStyle = style
	}
	return graphic
}
