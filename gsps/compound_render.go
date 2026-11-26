package gsps

import (
	"fmt"
	"image"
	"image/color"
	"image/draw"
	"math"
	"strings"
)

// RenderAnnotationOverlay renders an image/frame's GSPS graphic and text
// objects to a transparent color raster, independently of its source pixels.
// pixelToCanvas is the viewport's affine transform from continuous DICOM
// source pixel edges to positions in bounds. Image-edge coordinate (0,0)
// denotes the top-left corner of the first pixel, not its centre.
func RenderAnnotationOverlay(state *State, target ImageState, bounds image.Rectangle, pixelToCanvas func(Point2D) Point2D) (*image.NRGBA, error) {
	const maxOverlayPixels = 1 << 24
	width, height := bounds.Dx(), bounds.Dy()
	if state == nil || pixelToCanvas == nil || target.Rows <= 0 || target.Columns <= 0 ||
		width <= 0 || height <= 0 || width > maxOverlayPixels/height ||
		bounds.Min.X < -compoundCoordinateLimit || bounds.Min.Y < -compoundCoordinateLimit ||
		bounds.Max.X > compoundCoordinateLimit || bounds.Max.Y > compoundCoordinateLimit {
		return nil, fmt.Errorf("%w: invalid annotation viewport", ErrInvalidObject)
	}
	if len(state.ReferencedImages) != 0 && !referencesApply(state.ReferencedImages, target) {
		return nil, fmt.Errorf("%w: presentation state does not reference the displayed image", ErrMissingReference)
	}
	switch state.SpatialTransform.RotationDegrees {
	case 0, 90, 180, 270:
	default:
		return nil, fmt.Errorf("%w: noncardinal GSPS image rotation", ErrInvalidObject)
	}
	layers, err := resolvedGraphicLayers(state)
	if err != nil {
		return nil, err
	}
	area := selectDisplayedArea(state, target)
	place := func(p Point2D, units string) (Point2D, error) {
		pixel, err := annotationPointInPixelUnits(p, units, area, target, state.SpatialTransform)
		if err != nil {
			return Point2D{}, err
		}
		mapped := pixelToCanvas(pixel)
		if !safeAnnotationPoint(mapped) {
			return Point2D{}, fmt.Errorf("%w: nonfinite or oversized annotation projection", ErrInvalidObject)
		}
		return mapped, nil
	}
	out := image.NewNRGBA(bounds)
	targetCanvas := &canvas{overlay: out}
	if err := drawSelectedAnnotations(targetCanvas, state, target, layers, place, BasicFont(min(8, 1+min(width, height)/512))); err != nil {
		return nil, err
	}
	return out, nil
}

func drawSelectedAnnotations(c *canvas, state *State, target ImageState, layers []GraphicLayer, place annotationProjector, font Font) error {
	byLayer := make(map[string][]GraphicAnnotation, len(layers))
	for _, item := range selectGraphicAnnotations(state, target) {
		name := graphicLayerName(item.LayerName)
		byLayer[name] = append(byLayer[name], item)
	}
	budget := int64(128 << 20)
	if c.overlay != nil {
		c.budget = &budget
	}
	for _, layer := range layers {
		value := uint8(255)
		if layer.RecommendedGrayscale != nil {
			value = uint8(*layer.RecommendedGrayscale >> 8)
		}
		base := color.NRGBA{R: value, G: value, B: value, A: 255}
		if c.overlay != nil {
			base = compoundColor(layer.RecommendedCIELab, nil, base)
		}
		c.ink = base
		for _, item := range byLayer[layer.Name] {
			if item.Compound == nil {
				if !c.reserve(int64(len(item.Polyline)) + int64(len(item.Text))) {
					return c.exhausted
				}
				if err := drawSimpleAnnotation(c, item, value, font, place); err != nil {
					return err
				}
				if c.exhausted != nil {
					return c.exhausted
				}
				continue
			}
			graphic, err := projectCompound(item.Compound, place, c.bounds())
			if err != nil {
				return err
			}
			if err := paintCompoundPaths(c.destination(), graphic.paths, graphic.closed, item.Compound, base, graphic.scale, &budget); err != nil {
				return err
			}
			for _, label := range graphic.labels {
				if err := drawCompoundTickLabel(c.destination(), label, item.Compound.TextStyle, font, base, graphic.scale, &budget); err != nil {
					return err
				}
			}
		}
	}
	return nil
}

func drawCompoundTickLabel(dst draw.Image, label projectedTickLabel, style *CompoundTextStyle, font Font, fallback color.NRGBA, scale float64, budget *int64) error {
	ink := fallback
	var mask *image.Gray
	if style == nil {
		mask = glyphMask(font, label.text, false, false, false)
	} else {
		ink = compoundColor(style.Color, nil, fallback)
		mask = glyphMask(font, label.text, enabledStyle(style.Bold), enabledStyle(style.Italic), enabledStyle(style.Underlined))
	}
	if mask.Rect.Empty() {
		return nil
	}
	x := int(math.Floor(label.anchor.X)) - mask.Rect.Dx()/2
	y := int(math.Floor(label.anchor.Y))
	if !label.below {
		y -= mask.Rect.Dy()
	}
	if err := spendCompoundPaint(budget, int64(mask.Rect.Dx())*int64(mask.Rect.Dy())); err != nil {
		return err
	}
	if style != nil && (style.Shadow == "NORMAL" || style.Shadow == "OUTLINED") {
		shadow := compoundColor(style.ShadowColor, style.ShadowOpacity, color.NRGBA{A: 255})
		dx, dy := *style.ShadowOffsetX*scale, *style.ShadowOffsetY*scale
		if !validCompoundPaintNumber(dx) || !validCompoundPaintNumber(dy) {
			return fmt.Errorf("%w: invalid Compound Graphic tick shadow offset", ErrInvalidObject)
		}
		if style.Shadow == "NORMAL" {
			if err := spendCompoundPaint(budget, int64(mask.Rect.Dx())*int64(mask.Rect.Dy())); err != nil {
				return err
			}
			blendGlyph(dst, mask, x+int(math.Round(dx)), y+int(math.Round(dy)), shadow)
		} else if err := blendOutlineGlyph(dst, mask, x, y, math.Hypot(dx, dy), shadow, budget); err != nil {
			return err
		}
	}
	blendGlyph(dst, mask, x, y, ink)
	return nil
}

func enabledStyle(value *bool) bool { return value != nil && *value }

// glyphMask adapts the existing Font API, which renders into Gray, to color
// and alpha compositing while preserving the requested tick-label emphasis.
func glyphMask(font Font, text string, bold, italic, underline bool) *image.Gray {
	const maxGlyphPixels = 1 << 20
	line := printable(strings.ReplaceAll(text, "\r\n", " "))
	width, height := font.Width(line), font.LineHeight()
	if width <= 0 || height <= 0 || width > maxGlyphPixels/height {
		return &image.Gray{}
	}
	base := image.NewGray(image.Rect(0, 0, width, height))
	font.Draw(base, 0, 0, line, 255)
	if !bold && !italic && !underline {
		return base
	}
	extra := 0
	if italic {
		extra = height / 4
	}
	if bold {
		extra++
	}
	if width+extra > maxGlyphPixels/height {
		return &image.Gray{}
	}
	mask := image.NewGray(image.Rect(0, 0, width+extra, height))
	for row := 0; row < height; row++ {
		shift := 0
		if italic {
			shift = (height - row - 1) / 4
		}
		for col := 0; col < width; col++ {
			if base.Pix[row*base.Stride+col] == 0 {
				continue
			}
			offset := row*mask.Stride + col + shift
			mask.Pix[offset] = 255
			if bold {
				mask.Pix[offset+1] = 255
			}
		}
	}
	if underline {
		for col := 0; col < width; col++ {
			mask.Pix[(height-1)*mask.Stride+col] = 255
		}
	}
	return mask
}

func blendGlyph(dst draw.Image, mask *image.Gray, x, y int, ink color.NRGBA) {
	origin := image.Pt(x, y)
	visible := mask.Rect.Add(origin).Intersect(dst.Bounds())
	for py := visible.Min.Y; py < visible.Max.Y; py++ {
		for px := visible.Min.X; px < visible.Max.X; px++ {
			if mask.GrayAt(px-x, py-y).Y != 0 {
				blendCompoundPixel(dst, px, py, ink)
			}
		}
	}
}

func blendOutlineGlyph(dst draw.Image, mask *image.Gray, x, y int, radius float64, ink color.NRGBA, budget *int64) error {
	if radius <= 0 || ink.A == 0 {
		return nil
	}
	// Bound the footprint independently of the authored offsets and leave
	// the output unpublished if the dilation cannot be completed.
	const maxOutline = 64
	if radius > maxOutline {
		return fmt.Errorf("%w: Compound Graphic tick outline exceeds limit", ErrInvalidObject)
	}
	r := int(math.Ceil(radius))
	expanded := mask.Rect.Inset(-r)
	pixels := int64(expanded.Dx()) * int64(expanded.Dy())
	if err := spendCompoundPaint(budget, pixels); err != nil {
		return err
	}
	dilated := image.NewGray(expanded)
	for py := 0; py < mask.Rect.Dy(); py++ {
		for px := 0; px < mask.Rect.Dx(); px++ {
			if mask.GrayAt(px, py).Y == 0 {
				continue
			}
			if err := spendCompoundPaint(budget, int64(2*r+1)*int64(2*r+1)); err != nil {
				return err
			}
			for dy := -r; dy <= r; dy++ {
				for dx := -r; dx <= r; dx++ {
					if float64(dx*dx+dy*dy) <= radius*radius {
						dilated.SetGray(px+dx, py+dy, color.Gray{Y: 255})
					}
				}
			}
		}
	}
	blendGlyph(dst, dilated, x, y, ink)
	return nil
}
