package gsps

import (
	"fmt"
	"image"
	"image/color"
	"image/draw"
	"math"

	"github.com/ThalesMMS/dicom-go/internal/dicomcolor"
)

const (
	compoundTileSide = 32
	compoundTileArea = compoundTileSide * compoundTileSide
	// Keep distance, clipping and float-to-integer arithmetic reliable even
	// when a zoomed shape extends far outside the destination.
	compoundCoordinateLimit = 1 << 30
)

// paintCompoundPaths composites the shadow, fill and stroke in that order.
// The paths are already in destination pixel coordinates. Coverage costs are
// charged to the caller's shared render budget, including off-screen work.
func paintCompoundPaths(dst draw.Image, paths [][]Point2D, closed bool, graphic *CompoundGraphic, fallback color.NRGBA, scale float64, budget *int64) error {
	if graphic == nil {
		return fmt.Errorf("%w: missing Compound Graphic", ErrInvalidObject)
	}
	if !validCompoundPaintNumber(scale) || scale <= 0 || budget == nil {
		return fmt.Errorf("%w: invalid Compound Graphic paint scale or budget", ErrInvalidObject)
	}
	for _, path := range paths {
		if err := spendCompoundPaint(budget, int64(len(path))); err != nil {
			return err
		}
		for _, point := range path {
			if !validCompoundPaintNumber(point.X) || !validCompoundPaintNumber(point.Y) {
				return fmt.Errorf("%w: Compound Graphic projection is out of range", ErrInvalidObject)
			}
		}
	}
	stroke := compoundStroke{on: fallback, radius: .5}
	if line := graphic.LineStyle; line != nil {
		stroke.on = compoundColor(line.OnColor, line.OnOpacity, fallback)
		stroke.off = compoundColor(line.OffColor, line.OffOpacity, color.NRGBA{})
		if line.Thickness != nil {
			width := *line.Thickness * scale
			if !validCompoundPaintNumber(width) || width <= 0 {
				return fmt.Errorf("%w: invalid Compound Graphic line thickness", ErrInvalidObject)
			}
			stroke.radius = max(.5, width/2)
		}
		if line.Dashing == "DASHED" {
			if line.Pattern == nil {
				return fmt.Errorf("%w: DASHED Compound Graphic requires Line Pattern", ErrInvalidObject)
			}
			stroke.pattern = line.Pattern
		}
		if line.Shadow == "NORMAL" || line.Shadow == "OUTLINED" {
			shadow, err := stroke.shadow(line, scale)
			if err != nil {
				return err
			}
			if err := paintCompoundStroke(dst, paths, shadow, budget); err != nil {
				return err
			}
		}
	}
	if closed && graphic.Filled != nil && *graphic.Filled {
		if graphic.FillStyle == nil {
			return fmt.Errorf("%w: filled Compound Graphic requires Fill Style", ErrInvalidObject)
		}
		for _, path := range paths {
			if err := paintCompoundFill(dst, path, graphic.FillStyle, fallback, budget); err != nil {
				return err
			}
		}
	}
	return paintCompoundStroke(dst, paths, stroke, budget)
}

func validCompoundPaintNumber(v float64) bool {
	return math.Abs(v) <= compoundCoordinateLimit
}

func spendCompoundPaint(budget *int64, cost int64) error {
	if cost > *budget {
		return fmt.Errorf("%w: Compound Graphic paint work exceeds limit", ErrInvalidObject)
	}
	*budget -= cost
	return nil
}

func compoundColor(encoded *[3]uint16, opacity *float64, fallback color.NRGBA) color.NRGBA {
	ink := fallback
	if encoded != nil {
		rgb := dicomcolor.CIELabToRGB(*encoded)
		ink = color.NRGBA{R: uint8(rgb[0]), G: uint8(rgb[1]), B: uint8(rgb[2]), A: 255}
	}
	if opacity != nil {
		ink.A = uint8(math.Round(255 * max(0, min(1, *opacity))))
	}
	return ink
}

// blendCompoundPixel applies straight-alpha source-over to the destination.
// The headless grayscale renderer receives a single display-gray sample.
func blendCompoundPixel(dst draw.Image, x, y int, src color.NRGBA) {
	if src.A == 0 {
		return
	}
	if gray, ok := dst.(*image.Gray); ok {
		old := gray.GrayAt(x, y).Y
		foreground := color.GrayModel.Convert(color.RGBA{R: src.R, G: src.G, B: src.B, A: 255}).(color.Gray).Y
		alpha := int(src.A)
		result := (int(foreground)*alpha + int(old)*(255-alpha) + 127) / 255
		gray.SetGray(x, y, color.Gray{Y: uint8(result)})
		return
	}
	if src.A == 255 {
		dst.Set(x, y, src)
		return
	}
	base := color.NRGBAModel.Convert(dst.At(x, y)).(color.NRGBA)
	srcWeight := int(src.A) * 255
	baseWeight := int(base.A) * (255 - int(src.A))
	alpha := srcWeight + baseWeight
	channel := func(a, b uint8) uint8 {
		return uint8((int(a)*srcWeight + int(b)*baseWeight + alpha/2) / alpha)
	}
	dst.Set(x, y, color.NRGBA{
		R: channel(src.R, base.R), G: channel(src.G, base.G), B: channel(src.B, base.B),
		A: uint8((alpha + 127) / 255),
	})
}

type compoundStroke struct {
	on, off color.NRGBA
	pattern *uint32
	radius  float64
	offset  Point2D // Local X follows each segment, local Y is its clockwise normal.
}

func (s compoundStroke) shadow(line *CompoundLineStyle, scale float64) (compoundStroke, error) {
	var offset Point2D
	if line.ShadowOffsetX != nil {
		offset.X = *line.ShadowOffsetX * scale
	}
	if line.ShadowOffsetY != nil {
		offset.Y = *line.ShadowOffsetY * scale
	}
	if !validCompoundPaintNumber(offset.X) || !validCompoundPaintNumber(offset.Y) {
		return compoundStroke{}, fmt.Errorf("%w: invalid Compound Graphic shadow offset", ErrInvalidObject)
	}
	out := compoundStroke{
		on:     compoundColor(line.ShadowColor, line.ShadowOpacity, color.NRGBA{A: 255}),
		radius: s.radius,
		offset: offset,
	}
	if line.Shadow == "OUTLINED" {
		out.radius += math.Hypot(offset.X, offset.Y)
		out.offset = Point2D{}
	}
	return out, nil
}

// Stroke coverage is collected by bounded tiles. A join or a repeated edge
// must not apply opacity twice within a single stroke. Foreground wins when
// opposite dash phases meet at a join.
type compoundCoverage map[image.Point]*[compoundTileArea]uint8

func (c compoundCoverage) cover(x, y int, value uint8, budget *int64) error {
	origin := image.Pt(x&^(compoundTileSide-1), y&^(compoundTileSide-1))
	tile := c[origin]
	if tile == nil {
		// Charge both the allocated bytes and the eventual scan of the tile.
		if err := spendCompoundPaint(budget, 2*compoundTileArea); err != nil {
			return err
		}
		tile = new([compoundTileArea]uint8)
		c[origin] = tile
	}
	index := (y-origin.Y)*compoundTileSide + (x - origin.X)
	tile[index] = max(tile[index], value)
	return nil
}

func paintCompoundStroke(dst draw.Image, paths [][]Point2D, stroke compoundStroke, budget *int64) error {
	if stroke.on.A == 0 && (stroke.pattern == nil || stroke.off.A == 0) {
		return nil
	}
	coverage := make(compoundCoverage)
	for _, path := range paths {
		phase := 0.0
		for i := 1; i < len(path); i++ {
			if err := spendCompoundPaint(budget, 1); err != nil {
				return err
			}
			from, to := path[i-1], path[i]
			length := math.Hypot(to.X-from.X, to.Y-from.Y)
			if err := coverCompoundSegment(coverage, dst.Bounds(), from, to, length, phase, stroke, budget); err != nil {
				return err
			}
			phase = math.Mod(phase+length, 32)
		}
	}
	inks := [3]color.NRGBA{{}, stroke.off, stroke.on}
	for origin, tile := range coverage {
		for index, value := range tile {
			if value != 0 {
				blendCompoundPixel(dst, origin.X+index%compoundTileSide, origin.Y+index/compoundTileSide, inks[value])
			}
		}
	}
	return nil
}

func coverCompoundSegment(coverage compoundCoverage, bounds image.Rectangle, from, to Point2D, length, phase float64, stroke compoundStroke, budget *int64) error {
	dx, dy := to.X-from.X, to.Y-from.Y
	if stroke.offset != (Point2D{}) && length > 0 {
		shiftX := (dx*stroke.offset.X - dy*stroke.offset.Y) / length
		shiftY := (dy*stroke.offset.X + dx*stroke.offset.Y) / length
		from.X += shiftX
		from.Y += shiftY
		to.X += shiftX
		to.Y += shiftY
	}
	// Scan strips perpendicular to the dominant axis, avoiding a whole
	// bounding-box scan for a thin diagonal segment.
	swap := math.Abs(dy) > math.Abs(dx)
	if swap {
		from.X, from.Y = from.Y, from.X
		to.X, to.Y = to.Y, to.X
		dx, dy = dy, dx
		bounds.Min.X, bounds.Min.Y = bounds.Min.Y, bounds.Min.X
		bounds.Max.X, bounds.Max.Y = bounds.Max.Y, bounds.Max.X
	}
	slope := 0.0
	if dx != 0 {
		slope = dy / dx
	}
	radius := stroke.radius
	span := radius * math.Hypot(1, slope)
	a, b := compoundPaintRange(min(from.X, to.X)-radius, max(from.X, to.X)+radius, bounds.Min.X, bounds.Max.X)
	radiusSquared := radius * radius
	for major := a; major < b; major++ {
		centerX := float64(major) + .5
		middleY := from.Y + (centerX-from.X)*slope
		c, d := compoundPaintRange(middleY-span, middleY+span, bounds.Min.Y, bounds.Max.Y)
		if err := spendCompoundPaint(budget, int64(d-c)+1); err != nil {
			return err
		}
		for minor := c; minor < d; minor++ {
			centerY := float64(minor) + .5
			t := 0.0
			if length > 0 {
				t = min(1, max(0, ((centerX-from.X)*dx+(centerY-from.Y)*dy)/(length*length)))
			}
			nx, ny := centerX-from.X-t*dx, centerY-from.Y-t*dy
			distanceSquared := nx*nx + ny*ny
			if distanceSquared > radiusSquared || (distanceSquared == radiusSquared && (ny < 0 || (ny == 0 && nx < 0))) {
				continue
			}
			value := uint8(2)
			if stroke.pattern != nil {
				bit := uint(math.Mod(phase+t*length, 32))
				if *stroke.pattern&(1<<(31-bit)) == 0 {
					value = 1
				}
			}
			x, y := major, minor
			if swap {
				x, y = y, x
			}
			if err := coverage.cover(x, y, value, budget); err != nil {
				return err
			}
		}
	}
	return nil
}

func compoundPaintRange(lo, hi float64, minPixel, maxPixel int) (int, int) {
	// Clamp while still in floating point. Projected positions may lie far
	// outside the destination even when its actual dimensions are small.
	first := min(float64(maxPixel), max(float64(minPixel), math.Ceil(lo-.5)))
	end := min(float64(maxPixel), max(first, math.Floor(hi-.5)+1))
	return int(first), int(end)
}

func paintCompoundFill(dst draw.Image, polygon []Point2D, style *CompoundFillStyle, fallback color.NRGBA, budget *int64) error {
	if len(polygon) < 3 {
		return nil
	}
	on := compoundColor(style.OnColor, style.OnOpacity, fallback)
	off := compoundColor(style.OffColor, style.OffOpacity, color.NRGBA{})
	stipple := style.Mode == "STIPPELED"
	if stipple && len(style.Pattern) != 128 {
		return fmt.Errorf("%w: STIPPELED Compound Graphic requires a 32x32 Fill Pattern", ErrInvalidObject)
	}
	if on.A == 0 && (!stipple || off.A == 0) {
		return nil
	}
	ymin, ymax := polygon[0].Y, polygon[0].Y
	for _, p := range polygon[1:] {
		ymin = min(ymin, p.Y)
		ymax = max(ymax, p.Y)
	}
	bounds := dst.Bounds()
	first, end := compoundPaintRange(ymin, ymax, bounds.Min.Y, bounds.Max.Y)
	for y := first; y < end; y++ {
		if err := spendCompoundPaint(budget, int64(len(polygon))); err != nil {
			return err
		}
		cy := float64(y) + .5
		left, right := math.Inf(1), math.Inf(-1)
		// Rectangle and ellipse outlines are convex, so the two extrema
		// determine the complete interior interval on this scanline.
		for i, a := range polygon {
			b := polygon[(i+1)%len(polygon)]
			if (a.Y <= cy) != (b.Y <= cy) {
				x := a.X + (cy-a.Y)*(b.X-a.X)/(b.Y-a.Y)
				left = min(left, x)
				right = max(right, x)
			}
		}
		x0, x1 := compoundPaintRange(left, right, bounds.Min.X, bounds.Max.X)
		if err := spendCompoundPaint(budget, int64(x1-x0)); err != nil {
			return err
		}
		for x := x0; x < x1; x++ {
			ink := on
			if stipple {
				row, column := (y-bounds.Min.Y)&31, (x-bounds.Min.X)&31
				if style.Pattern[row*4+column/8]&(0x80>>uint(column&7)) == 0 {
					ink = off
				}
			}
			blendCompoundPixel(dst, x, y, ink)
		}
	}
	return nil
}
