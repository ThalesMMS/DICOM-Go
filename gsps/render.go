package gsps

import (
	"fmt"
	"image"
	"image/color"
	"image/draw"
	"math"
	"sort"
	"strings"

	"github.com/ThalesMMS/dicom-go/pixeldata/display"
)

// maxRenderPixels bounds the output of Render, whose size follows the
// displayed area rather than the image.
const maxRenderPixels = 1 << 26

// RenderOptions tunes Render.
type RenderOptions struct {
	// WholeImage renders the whole image, rotated and flipped, instead of
	// the displayed area only; DCMTK dcmp2pgm renders this way.
	WholeImage bool
	// NoAnnotations leaves out the graphic and text objects.
	NoAnnotations bool
	// ImageOverlays are the image's own overlay planes, which the state may
	// activate (see ActiveOverlays).
	ImageOverlays []display.Overlay
	// Font draws text objects; nil selects BasicFont scaled to the output.
	Font Font
}

// Render burns a presentation state into one grayscale frame, headless, for
// DICOMweb rendered responses, printing or export. frame holds the stored
// pixels; its Modality transform is replaced by the state's Modality LUT
// when the state has one, and its VOI by the state's Softcopy VOI for the
// image (no VOI transform, a full-range window, when the state has none).
// target identifies the image and frame for the image-specific items and
// defaults its size to the frame's.
//
// The stages follow PS3.4 N.2: modality, VOI, the Presentation LUT (shape
// or table) giving P-Values, the activated overlays, the display shutter
// (which covers them), then the rotation (clockwise) and horizontal flip,
// the displayed area (unless opts.WholeImage; its part outside the image
// takes the shutter presentation value, or black without a shutter, as
// DCMTK prints it) and the graphic and text objects, layer by layer
// in Graphic Layer Order, in each layer's recommended grayscale (white by
// default). Compound Graphic style colors are converted to grayscale for
// this output. The result is 8-bit P-Values, one output pixel per image pixel:
// Presentation Size Mode, pixel spacing and aspect ratio, which need the
// display device, are left to the caller. INTERPOLATED graphics are drawn
// as Catmull-Rom curves through their points and text is drawn upright
// with a contrasting outline. PIXEL annotations turn with the image; DISPLAY
// annotations are placed on the displayed area and do not.
func Render(frame display.Frame, state *State, target ImageState, opts RenderOptions) (*image.Gray, error) {
	if state == nil {
		return nil, fmt.Errorf("%w: state is nil", ErrInvalidObject)
	}
	if target.Rows == 0 && target.Columns == 0 {
		target.Rows, target.Columns = frame.Rows, frame.Columns
	}
	if target.Rows != frame.Rows || target.Columns != frame.Columns {
		return nil, fmt.Errorf("%w: image is %dx%d but the frame is %dx%d", ErrInvalidObject, target.Columns, target.Rows, frame.Columns, frame.Rows)
	}
	if target.SOPInstanceUID != "" && len(state.ReferencedImages) > 0 && !referencesApply(state.ReferencedImages, target) {
		return nil, fmt.Errorf("%w: image %s", ErrMissingReference, target.SOPInstanceUID)
	}
	layers, err := resolvedGraphicLayers(state)
	if err != nil {
		return nil, err
	}
	if err := validatePresentationLUT(state.PresentationLUT); err != nil {
		return nil, err
	}
	geometry, err := newRenderGeometry(frame.Columns, frame.Rows, state, target, opts.WholeImage)
	if err != nil {
		return nil, err
	}

	if state.ModalityLUT != nil {
		frame.Modality = state.ModalityLUT.Transform()
	}
	frame.VOI = display.VOILUT{}
	if voi := selectSoftcopyVOI(state, target); softcopyVOIDefined(voi) {
		frame.VOI = display.VOILUT{Center: voi.WindowCenter, Width: voi.WindowWidth, Function: voi.Function, LUT: voi.LUT}
	}
	pvalues, err := presentationValues(frame, state)
	if err != nil {
		return nil, err
	}

	layerValues := make(map[string]uint8, len(layers))
	for _, layer := range layers {
		layerValues[layer.Name] = 255
		if layer.RecommendedGrayscale != nil {
			layerValues[layer.Name] = uint8(*layer.RecommendedGrayscale >> 8)
		}
	}
	frameNumber := max(target.FrameNumber, 1)
	activeLayer := make(map[uint16]string, len(state.OverlayActivations))
	for _, activation := range state.OverlayActivations {
		activeLayer[activation.Group] = graphicLayerName(activation.Layer)
	}
	for _, overlay := range ActiveOverlays(state, opts.ImageOverlays) {
		if plane, ok := overlay.ForImageFrame(frameNumber - 1); ok {
			plane.Composite(pvalues, layerValues[activeLayer[overlay.Group]])
		}
	}
	if state.Shutter != nil {
		state.Shutter.Apply(pvalues)
	}

	pad := uint8(0)
	if state.Shutter != nil {
		pad = state.Shutter.PresentationValue
	}
	out := geometry.transform(pvalues, pad)
	if opts.NoAnnotations {
		return out, nil
	}
	font := opts.Font
	if font == nil {
		font = BasicFont(1 + min(out.Rect.Dx(), out.Rect.Dy())/512)
	}
	canvas := &canvas{img: out}
	place := func(p Point2D, units string) (Point2D, error) { return geometry.place(p, units), nil }
	if err := drawSelectedAnnotations(canvas, state, target, layers, place, font); err != nil {
		return nil, err
	}
	return out, nil
}

// presentationValues renders the frame through modality and VOI and maps
// the result through the Presentation LUT table or shape.
func presentationValues(frame display.Frame, state *State) (*image.Gray, error) {
	lut := state.PresentationLUT
	if lut == nil {
		out, err := display.RenderGray(frame)
		if err != nil {
			return nil, err
		}
		if strings.EqualFold(strings.TrimSpace(state.PresentationLUTShape), PresentationLUTInverse) {
			for i := range out.Pix {
				out.Pix[i] = 255 - out.Pix[i]
			}
		}
		return out, nil
	}
	// The table's input spans the VOI output range: render it at 16 bits
	// and scale it onto the table's entries.
	wide, err := display.RenderGray16(frame)
	if err != nil {
		return nil, err
	}
	out := image.NewGray(wide.Rect)
	last := float64(len(lut.Entries) - 1)
	top := math.Exp2(float64(lut.BitsPerEntry)) - 1
	for i := range out.Pix {
		value := float64(uint16(wide.Pix[2*i])<<8 | uint16(wide.Pix[2*i+1]))
		entry := float64(lut.Entries[int(math.Floor(value*last/65535))])
		out.Pix[i] = uint8(math.Round(math.Min(entry, top) * 255 / top))
	}
	return out, nil
}

// renderGeometry maps image coordinates to the rendered output: rotation,
// flip and the displayed area crop.
type renderGeometry struct {
	width, height int // image size
	rotation      int
	flip          bool
	rotatedWidth  int
	crop          image.Rectangle // output region, in rotated coordinates
	area          [2]Point2D      // displayed area corners, in output coordinates
}

// newSpatialGeometry is the rotation and flip of a width x height image.
func newSpatialGeometry(width, height int, transform SpatialTransform) renderGeometry {
	g := renderGeometry{width: width, height: height, rotation: transform.RotationDegrees, flip: transform.FlipHorizontal, rotatedWidth: width}
	if g.rotation == 90 || g.rotation == 270 {
		g.rotatedWidth = height
	}
	return g
}

func newRenderGeometry(width, height int, state *State, target ImageState, whole bool) (renderGeometry, error) {
	switch state.SpatialTransform.RotationDegrees {
	case 0, 90, 180, 270:
	default:
		return renderGeometry{}, fmt.Errorf("%w: image rotation %d is not 0, 90, 180 or 270", ErrInvalidObject, state.SpatialTransform.RotationDegrees)
	}
	g := newSpatialGeometry(width, height, state.SpatialTransform)
	left, top, right, bottom := 0.0, 0.0, float64(width), float64(height)
	if area := selectDisplayedArea(state, target); displayedAreaDefined(area) {
		var err error
		if left, top, right, bottom, err = displayedAreaPixelEdges(area, target); err != nil {
			return g, err
		}
	}
	a, b := g.rotate(Point2D{X: left, Y: top}), g.rotate(Point2D{X: right, Y: bottom})
	areaRect := image.Rect(int(math.Min(a.X, b.X)), int(math.Min(a.Y, b.Y)), int(math.Max(a.X, b.X)), int(math.Max(a.Y, b.Y)))
	g.crop = areaRect
	if whole {
		if g.rotation == 90 || g.rotation == 270 {
			g.crop = image.Rect(0, 0, height, width)
		} else {
			g.crop = image.Rect(0, 0, width, height)
		}
	}
	if g.crop.Empty() || g.crop.Dx()*g.crop.Dy() > maxRenderPixels || g.crop.Dx() > maxRenderPixels || g.crop.Dy() > maxRenderPixels {
		return g, fmt.Errorf("%w: rendered size %dx%d", ErrInvalidObject, g.crop.Dx(), g.crop.Dy())
	}
	origin := Point2D{X: float64(g.crop.Min.X), Y: float64(g.crop.Min.Y)}
	g.area = [2]Point2D{
		{X: float64(areaRect.Min.X) - origin.X, Y: float64(areaRect.Min.Y) - origin.Y},
		{X: float64(areaRect.Max.X) - origin.X, Y: float64(areaRect.Max.Y) - origin.Y},
	}
	return g, nil
}

// rotate maps an image point to rotated and flipped coordinates.
func (g renderGeometry) rotate(p Point2D) Point2D {
	w, h := float64(g.width), float64(g.height)
	switch g.rotation {
	case 90:
		p = Point2D{X: h - p.Y, Y: p.X}
	case 180:
		p = Point2D{X: w - p.X, Y: h - p.Y}
	case 270:
		p = Point2D{X: p.Y, Y: w - p.X}
	}
	if g.flip {
		p.X = float64(g.rotatedWidth) - p.X
	}
	return p
}

// unrotate is the inverse of rotate.
func (g renderGeometry) unrotate(p Point2D) Point2D {
	w, h := float64(g.width), float64(g.height)
	if g.flip {
		p.X = float64(g.rotatedWidth) - p.X
	}
	switch g.rotation {
	case 90:
		p = Point2D{X: p.Y, Y: h - p.X}
	case 180:
		p = Point2D{X: w - p.X, Y: h - p.Y}
	case 270:
		p = Point2D{X: w - p.Y, Y: p.X}
	}
	return p
}

// place maps an annotation point in the given units to output coordinates.
func (g renderGeometry) place(p Point2D, units string) Point2D {
	if normalizedAnnotationUnits(units) == AnnotationUnitsDisplay {
		return Point2D{X: g.area[0].X + p.X*(g.area[1].X-g.area[0].X), Y: g.area[0].Y + p.Y*(g.area[1].Y-g.area[0].Y)}
	}
	p = g.rotate(p)
	return Point2D{X: p.X - float64(g.crop.Min.X), Y: p.Y - float64(g.crop.Min.Y)}
}

// transform rotates, flips and crops the P-Values; output pixels outside the
// image get pad.
func (g renderGeometry) transform(src *image.Gray, pad uint8) *image.Gray {
	out := image.NewGray(image.Rect(0, 0, g.crop.Dx(), g.crop.Dy()))
	if pad != 0 {
		for i := range out.Pix {
			out.Pix[i] = pad
		}
	}
	for row := 0; row < g.height; row++ {
		for col := 0; col < g.width; col++ {
			p := g.rotate(Point2D{X: float64(col) + 0.5, Y: float64(row) + 0.5})
			x, y := int(math.Floor(p.X))-g.crop.Min.X, int(math.Floor(p.Y))-g.crop.Min.Y
			if x >= 0 && y >= 0 && x < out.Rect.Dx() && y < out.Rect.Dy() {
				out.Pix[y*out.Stride+x] = src.Pix[row*src.Stride+col]
			}
		}
	}
	return out
}

func drawSimpleAnnotation(c *canvas, item GraphicAnnotation, value uint8, font Font, place annotationProjector) error {
	if len(item.Polyline) > 0 {
		points := make([]Point2D, len(item.Polyline))
		for i, point := range item.Polyline {
			if err := validateAnnotationPoint(point, item.GraphicUnits); err != nil {
				return err
			}
			converted, err := place(point, item.GraphicUnits)
			if err != nil {
				return err
			}
			points[i] = converted
		}
		c.drawGraphic(normalizedGraphicTypeOrPolyline(item.GraphicType), points, item.Filled, value)
	}
	if item.Text == "" {
		return nil
	}
	if err := validateTextObject(item); err != nil {
		return err
	}
	lines := strings.Split(strings.ReplaceAll(item.Text, "\r\n", "\n"), "\n")
	var anchor *Point2D
	if hasAnchor(item) {
		p, err := place(item.Anchor, item.AnchorUnits)
		if err != nil {
			return err
		}
		anchor = &p
	}
	var left, top, right float64
	justification := JustifyLeft
	if box := item.BoundingBox; box != nil {
		a, err := place(box.TopLeft, box.Units)
		if err != nil {
			return err
		}
		b, err := place(box.BottomRight, box.Units)
		if err != nil {
			return err
		}
		left, top, right = math.Min(a.X, b.X), math.Min(a.Y, b.Y), math.Max(a.X, b.X)
		justification = normalizedJustification(box.Justification)
		if anchor != nil && item.AnchorVisible {
			bottom := math.Max(a.Y, b.Y)
			nearest := Point2D{X: math.Max(left, math.Min(anchor.X, right)), Y: math.Max(top, math.Min(anchor.Y, bottom))}
			c.line(*anchor, nearest, value)
		}
	} else {
		// Without a bounding box the text starts just below and right of
		// the anchor.
		left, top = anchor.X+2, anchor.Y+2
		right = left
		if item.AnchorVisible {
			c.cross(*anchor, 2, value)
		}
	}
	y := int(math.Floor(top))
	for _, line := range lines {
		line = printable(line)
		width := font.Width(line)
		x := int(math.Floor(left))
		switch justification {
		case JustifyRight:
			x = int(math.Floor(right)) - width
		case JustifyCenter:
			x = int(math.Floor((left+right)/2)) - width/2
		}
		c.text(font, x, y, line, value)
		y += font.LineHeight()
	}
	return nil
}

// printable replaces control characters, which fonts cannot draw.
func printable(line string) string {
	return strings.Map(func(r rune) rune {
		if r < 0x20 || r == 0x7F || r >= 0x80 && r < 0xA0 {
			return ' '
		}
		return r
	}, line)
}

// canvas draws on the rendered P-Values or on a transparent viewport.
type canvas struct {
	img       *image.Gray
	overlay   *image.NRGBA
	ink       color.NRGBA
	budget    *int64
	exhausted error
}

func (c *canvas) destination() draw.Image {
	if c.overlay != nil {
		return c.overlay
	}
	return c.img
}

func (c *canvas) bounds() image.Rectangle { return c.destination().Bounds() }

func (c *canvas) reserve(cost int64) bool {
	if c.budget == nil {
		return true
	}
	if c.exhausted == nil {
		c.exhausted = spendCompoundPaint(c.budget, cost)
	}
	return c.exhausted == nil
}

func (c *canvas) set(x, y int, value uint8) {
	if image.Pt(x, y).In(c.bounds()) {
		if c.overlay != nil {
			blendCompoundPixel(c.overlay, x, y, c.ink)
		} else {
			c.img.Pix[c.img.PixOffset(x, y)] = value
		}
	}
}

func (c *canvas) drawGraphic(graphicType string, points []Point2D, filled bool, value uint8) {
	var outline []Point2D
	closed := false
	switch graphicType {
	case GraphicTypePoint:
		for _, p := range points {
			c.cross(p, 2, value)
		}
		return
	case GraphicTypeCircle:
		if len(points) < 2 {
			return
		}
		r := math.Hypot(points[1].X-points[0].X, points[1].Y-points[0].Y)
		outline, closed = ellipsePoints(points[0], r, r, 0), true
	case GraphicTypeEllipse:
		if len(points) < 4 {
			return
		}
		center := Point2D{X: (points[0].X + points[1].X) / 2, Y: (points[0].Y + points[1].Y) / 2}
		a := math.Hypot(points[1].X-points[0].X, points[1].Y-points[0].Y) / 2
		b := math.Hypot(points[3].X-points[2].X, points[3].Y-points[2].Y) / 2
		outline, closed = ellipsePoints(center, a, b, math.Atan2(points[1].Y-points[0].Y, points[1].X-points[0].X)), true
	case GraphicTypeInterpolated:
		outline, closed = catmullRom(points), closedPolyline(points)
	default:
		outline, closed = points, closedPolyline(points)
	}
	if filled && closed {
		c.fill(outline, value)
	}
	for i := 1; i < len(outline); i++ {
		c.line(outline[i-1], outline[i], value)
	}
	if len(outline) == 1 {
		c.set(int(math.Floor(outline[0].X)), int(math.Floor(outline[0].Y)), value)
	}
}

// ellipsePoints returns a closed outline of an ellipse with semi-axes a and
// b, the first turned by angle radians.
func ellipsePoints(center Point2D, a, b, angle float64) []Point2D {
	steps := int(math.Min(1024, math.Max(16, 2*math.Pi*math.Max(a, b)/2)))
	if math.IsNaN(a) || math.IsNaN(b) {
		steps = 16
	}
	sin, cos := math.Sincos(angle)
	out := make([]Point2D, 0, steps+1)
	for i := 0; i <= steps; i++ {
		t := 2 * math.Pi * float64(i%steps) / float64(steps)
		x, y := a*math.Cos(t), b*math.Sin(t)
		out = append(out, Point2D{X: center.X + x*cos - y*sin, Y: center.Y + x*sin + y*cos})
	}
	return out
}

// catmullRom samples a Catmull-Rom spline through points.
func catmullRom(points []Point2D) []Point2D {
	if len(points) < 3 {
		return points
	}
	const steps = 8
	at := func(i int) Point2D { return points[max(0, min(len(points)-1, i))] }
	out := make([]Point2D, 0, (len(points)-1)*steps+1)
	for i := 0; i+1 < len(points); i++ {
		p0, p1, p2, p3 := at(i-1), at(i), at(i+1), at(i+2)
		for s := 0; s < steps; s++ {
			t := float64(s) / steps
			t2, t3 := t*t, t*t*t
			blend := func(a, b, c, d float64) float64 {
				return 0.5 * (2*b + (c-a)*t + (2*a-5*b+4*c-d)*t2 + (3*b-a-3*c+d)*t3)
			}
			out = append(out, Point2D{X: blend(p0.X, p1.X, p2.X, p3.X), Y: blend(p0.Y, p1.Y, p2.Y, p3.Y)})
		}
	}
	return append(out, points[len(points)-1])
}

// line draws a segment clipped to the canvas.
func (c *canvas) line(from, to Point2D, value uint8) {
	bounds := c.bounds()
	a, b, ok := clipSegment(from, to, float64(bounds.Min.X)-1, float64(bounds.Min.Y)-1, float64(bounds.Max.X)+1, float64(bounds.Max.Y)+1)
	if !ok {
		return
	}
	x0, y0 := int(math.Floor(a.X)), int(math.Floor(a.Y))
	x1, y1 := int(math.Floor(b.X)), int(math.Floor(b.Y))
	dx, dy := abs(x1-x0), -abs(y1-y0)
	if !c.reserve(int64(max(dx, -dy)) + 1) {
		return
	}
	sx, sy := sign(x1-x0), sign(y1-y0)
	e := dx + dy
	for {
		c.set(x0, y0, value)
		if x0 == x1 && y0 == y1 {
			return
		}
		if e2 := 2 * e; e2 >= dy {
			e += dy
			x0 += sx
		} else {
			e += dx
			y0 += sy
		}
	}
}

// clipSegment clips a segment to a rectangle (Liang-Barsky).
func clipSegment(a, b Point2D, minX, minY, maxX, maxY float64) (Point2D, Point2D, bool) {
	if math.IsNaN(a.X+a.Y+b.X+b.Y) || math.IsInf(a.X+a.Y+b.X+b.Y, 0) {
		return a, b, false
	}
	t0, t1 := 0.0, 1.0
	dx, dy := b.X-a.X, b.Y-a.Y
	for _, edge := range [4][2]float64{{-dx, a.X - minX}, {dx, maxX - a.X}, {-dy, a.Y - minY}, {dy, maxY - a.Y}} {
		p, q := edge[0], edge[1]
		if p == 0 {
			if q < 0 {
				return a, b, false
			}
			continue
		}
		r := q / p
		if p < 0 {
			t0 = math.Max(t0, r)
		} else {
			t1 = math.Min(t1, r)
		}
		if t0 > t1 {
			return a, b, false
		}
	}
	return Point2D{X: a.X + t0*dx, Y: a.Y + t0*dy}, Point2D{X: a.X + t1*dx, Y: a.Y + t1*dy}, true
}

func (c *canvas) cross(p Point2D, arm int, value uint8) {
	x, y := p.X, p.Y
	c.line(Point2D{X: x - float64(arm), Y: y}, Point2D{X: x + float64(arm), Y: y}, value)
	c.line(Point2D{X: x, Y: y - float64(arm)}, Point2D{X: x, Y: y + float64(arm)}, value)
}

// fill paints the pixels whose centres lie inside the polygon (even-odd).
func (c *canvas) fill(polygon []Point2D, value uint8) {
	minY, maxY := math.Inf(1), math.Inf(-1)
	for _, p := range polygon {
		minY, maxY = math.Min(minY, p.Y), math.Max(maxY, p.Y)
	}
	bounds := c.bounds()
	first := max(bounds.Min.Y, int(math.Floor(minY)))
	last := min(bounds.Max.Y-1, int(math.Ceil(maxY)))
	var xs []float64
	for y := first; y <= last; y++ {
		if !c.reserve(int64(len(polygon))) {
			return
		}
		cy := float64(y) + 0.5
		xs = xs[:0]
		for i := range polygon {
			a, b := polygon[i], polygon[(i+1)%len(polygon)]
			if (a.Y <= cy) != (b.Y <= cy) {
				xs = append(xs, a.X+(cy-a.Y)*(b.X-a.X)/(b.Y-a.Y))
			}
		}
		sort.Float64s(xs)
		for i := 0; i+1 < len(xs); i += 2 {
			from := max(bounds.Min.X, int(math.Ceil(xs[i]-0.5)))
			to := min(bounds.Max.X-1, int(math.Floor(xs[i+1]-0.5)))
			if !c.reserve(int64(max(0, to-from+1))) {
				return
			}
			for x := from; x <= to; x++ {
				c.set(x, y, value)
			}
		}
	}
}

// text draws a line with a one-pixel outline in the contrasting value.
func (c *canvas) text(font Font, x, y int, line string, value uint8) {
	if c.overlay != nil {
		mask := glyphMask(font, line, false, false, false)
		ink := c.ink
		if !mask.Rect.Empty() {
			if !c.reserve(9 * int64(mask.Rect.Dx()) * int64(mask.Rect.Dy())) {
				return
			}
			for _, d := range [8][2]int{{-1, -1}, {0, -1}, {1, -1}, {-1, 0}, {1, 0}, {-1, 1}, {0, 1}, {1, 1}} {
				blendGlyph(c.overlay, mask, x+d[0], y+d[1], color.NRGBA{A: 255})
			}
			blendGlyph(c.overlay, mask, x, y, ink)
		}
		return
	}
	outline := uint8(0)
	if value < 128 {
		outline = 255
	}
	for _, d := range [8][2]int{{-1, -1}, {0, -1}, {1, -1}, {-1, 0}, {1, 0}, {-1, 1}, {0, 1}, {1, 1}} {
		font.Draw(c.img, x+d[0], y+d[1], line, outline)
	}
	font.Draw(c.img, x, y, line, value)
}

func abs(v int) int {
	if v < 0 {
		return -v
	}
	return v
}

func sign(v int) int {
	switch {
	case v < 0:
		return -1
	case v > 0:
		return 1
	}
	return 0
}

// Font draws the text objects of Render.
type Font interface {
	// LineHeight is the distance between the tops of two lines, in pixels.
	LineHeight() int
	// Width is the width of a line of text, in pixels.
	Width(line string) int
	// Draw sets the pixels of a line of text, with its top-left corner at
	// (x, y), to value. Pixels outside dst are skipped.
	Draw(dst *image.Gray, x, y int, line string, value uint8)
}

// BasicFont returns the built-in 5x7 bitmap font for printable ASCII,
// enlarged scale times; other characters are drawn as '?'.
func BasicFont(scale int) Font {
	return basicFont{scale: max(scale, 1)}
}

type basicFont struct{ scale int }

func (f basicFont) LineHeight() int { return 9 * f.scale }

func (f basicFont) Width(line string) int {
	n := len([]rune(line))
	if n == 0 {
		return 0
	}
	return (6*n - 1) * f.scale
}

func (f basicFont) Draw(dst *image.Gray, x, y int, line string, value uint8) {
	bounds := dst.Rect
	for _, r := range line {
		if r < 0x20 || r > 0x7E {
			r = '?'
		}
		glyph := basicGlyphs[r-0x20]
		for row, bits := range glyph {
			for col := 0; col < 5; col++ {
				if bits&(0x10>>col) == 0 {
					continue
				}
				for dy := 0; dy < f.scale; dy++ {
					for dx := 0; dx < f.scale; dx++ {
						px, py := x+col*f.scale+dx, y+row*f.scale+dy
						if image.Pt(px, py).In(bounds) {
							dst.Pix[(py-bounds.Min.Y)*dst.Stride+px-bounds.Min.X] = value
						}
					}
				}
			}
		}
		x += 6 * f.scale
	}
}
