package display

import (
	"errors"
	"image"
	"image/color"
	"image/draw"
	mathbits "math/bits"
)

var (
	// ErrUnsupportedPresentationLUT reports Presentation LUT content outside the
	// supported Sequence and Shape forms.
	ErrUnsupportedPresentationLUT = errors.New("dicom/display: unsupported Presentation LUT")
	// ErrUnsupportedGSPS reports Grayscale Softcopy Presentation State content
	// that the pipeline does not interpret.
	ErrUnsupportedGSPS = errors.New("dicom/display: Grayscale Softcopy Presentation State is not supported")
)

// Presentation is the presentation-stage transform applied after the VOI
// transform has produced 8-bit MONOCHROME2 display values. It is modeled after
// ModalityLUT and VOILUT so callers can reason about and test each stage
// independently. The stages run in a fixed order:
//
//	VOI output -> Presentation LUT/Shape -> Shutter -> Overlays
//
// GSPS interpretation is an explicit non-goal of this stage (see
// ErrUnsupportedGSPS).
type Presentation struct {
	// LUT maps VOI output values through a Presentation LUT Sequence, or the
	// LIN OD shape's table (LinOD).
	LUT *LUT
	// LinOD reports Presentation LUT Shape LIN OD (see
	// PrintDensity.LinODPresentationLUT).
	LinOD bool
	// Invert inverts the display value (MONOCHROME1, or Presentation LUT Shape
	// INVERSE) after VOI mapping.
	Invert bool
	// Shutter, when set, masks pixels outside the display shutter region.
	Shutter *Shutter
	// Overlays are 1-bit overlay planes burned onto the image last.
	Overlays []Overlay
}

// Apply runs the presentation stages in place within the bounds of an 8-bit
// grayscale image, including when it shares backing pixels with a larger image.
func (p Presentation) Apply(img *image.Gray) {
	if img == nil {
		return
	}
	bounds := img.Bounds()
	if bounds.Empty() {
		return
	}
	if p.LUT != nil || p.Invert {
		width := bounds.Dx()
		for y := 0; y < bounds.Dy(); y++ {
			start := y * img.Stride
			row := img.Pix[start : start+width]
			if p.LUT != nil {
				for x, pixel := range row {
					row[x] = p.LUT.entryByte(p.LUT.Lookup(int(pixel)))
				}
			}
			if p.Invert {
				for x, pixel := range row {
					row[x] = 255 - pixel
				}
			}
		}
	}
	if p.Shutter != nil {
		p.Shutter.Apply(img)
	}
	for i := range p.Overlays {
		p.Overlays[i].Composite(img, overlayBurnValue)
	}
}

// overlayBurnValue is the grayscale value written where an overlay bit is set.
const overlayBurnValue = 255

// Shutter masks the region outside a display shutter (PS3.3 C.7.6.11). When
// more than one shape is present a pixel is visible only if it lies inside every
// present shape (the shutter region is their intersection); pixels outside the
// region are set to PresentationValue.
type Shutter struct {
	// PresentationValue is the 8-bit grayscale value written to masked-out
	// pixels (derived from Shutter Presentation Value (0018,1622)).
	PresentationValue uint8
	// PresentationPValue preserves the original 16-bit DICOM P-Value when it
	// was explicitly encoded. PresentationValue remains the rendering value.
	PresentationPValue        uint16
	PresentationPValueDefined bool
	// PresentationColorCIELab preserves the optional shutter presentation
	// color. Grayscale rendering continues to use PresentationValue.
	PresentationColorCIELab        [3]uint16
	PresentationColorCIELabDefined bool
	Rectangle                      *RectShutter
	Circle                         *CircleShutter
	Polygon                        *PolygonShutter
	Bitmap                         *BitmapShutter
}

// Clone returns an independent shutter value, including polygon vertices and
// bitmap overlay bytes. It is safe to retain while a caller edits live state.
func (s *Shutter) Clone() *Shutter {
	if s == nil {
		return nil
	}
	out := *s
	if s.Rectangle != nil {
		value := *s.Rectangle
		out.Rectangle = &value
	}
	if s.Circle != nil {
		value := *s.Circle
		out.Circle = &value
	}
	if s.Polygon != nil {
		value := *s.Polygon
		value.Vertices = append([]ShutterPoint(nil), s.Polygon.Vertices...)
		out.Polygon = &value
	}
	if s.Bitmap != nil {
		value := *s.Bitmap
		value.Overlay.Data = append([]byte(nil), s.Bitmap.Overlay.Data...)
		out.Bitmap = &value
	}
	return &out
}

// RectShutter is a rectangular shutter. Edges are 1-based image coordinates as
// stored in DICOM (Shutter Left/Right Vertical Edge = columns, Upper/Lower
// Horizontal Edge = rows); the visible region is inclusive of the edges.
type RectShutter struct {
	Left, Right  int // vertical edges (columns)
	Upper, Lower int // horizontal edges (rows)
}

func (r RectShutter) inside(row, col int) bool {
	c := col + 1 // image column to 1-based DICOM column
	rw := row + 1
	return c >= r.Left && c <= r.Right && rw >= r.Upper && rw <= r.Lower
}

// CircleShutter is a circular shutter. Center is a 1-based (row, column) pair
// (Center of Circular Shutter) and Radius is in pixels.
type CircleShutter struct {
	CenterRow, CenterCol int
	Radius               int
}

// inside compares the squared distance with the squared radius exactly in
// 128 bits, so centers and radii far outside the image (Center of Circular
// Shutter and Radius are IS values of up to 12 digits) cannot overflow into
// a wrong answer. A negative Radius acts as its magnitude.
func (c CircleShutter) inside(row, col int) bool {
	dr := absDiff(row+1, c.CenterRow)
	dc := absDiff(col+1, c.CenterCol)
	radius := absDiff(c.Radius, 0)
	if dr > radius || dc > radius {
		return false
	}
	// dr, dc <= radius <= 2^63, so each square is below 2^127 and the sum
	// fits in 128 bits.
	radiusHi, radiusLo := mathbits.Mul64(radius, radius)
	rowHi, rowLo := mathbits.Mul64(dr, dr)
	colHi, colLo := mathbits.Mul64(dc, dc)
	lo, carry := mathbits.Add64(rowLo, colLo, 0)
	hi, _ := mathbits.Add64(rowHi, colHi, carry)
	return hi < radiusHi || (hi == radiusHi && lo <= radiusLo)
}

// absDiff is |a-b| without overflow.
func absDiff(a, b int) uint64 {
	if a >= b {
		return uint64(a) - uint64(b)
	}
	return uint64(b) - uint64(a)
}

// PolygonShutter is a polygonal shutter. Vertices are 1-based (row, column)
// points (Vertices of the Polygonal Shutter). The polygon is implicitly closed.
type PolygonShutter struct {
	Vertices []ShutterPoint
}

// ShutterPoint is a 1-based (row, column) vertex.
type ShutterPoint struct {
	Row, Col int
}

// inside reports whether an image pixel lies within the polygon using the
// even-odd ray-casting rule.
func (p PolygonShutter) inside(row, col int) bool {
	if len(p.Vertices) < 3 {
		return false
	}
	r := float64(row + 1)
	c := float64(col + 1)
	in := false
	n := len(p.Vertices)
	j := n - 1
	for i := 0; i < n; i++ {
		ri := float64(p.Vertices[i].Row)
		ci := float64(p.Vertices[i].Col)
		rj := float64(p.Vertices[j].Row)
		cj := float64(p.Vertices[j].Col)
		if (ri > r) != (rj > r) {
			crossCol := (cj-ci)*(r-ri)/(rj-ri) + ci
			if c < crossCol {
				in = !in
			}
		}
		j = i
	}
	return in
}

// BitmapShutter uses a referenced overlay plane whose set bits mask pixels
// (DICOM PS3.3 C.7.6.15).
type BitmapShutter struct {
	Group   uint16
	Overlay Overlay
}

func (b BitmapShutter) masks(row, col int) bool {
	overlayRow := row - (b.Overlay.OriginRow - 1)
	overlayCol := col - (b.Overlay.OriginCol - 1)
	if overlayRow < 0 || overlayRow >= b.Overlay.Rows || overlayCol < 0 || overlayCol >= b.Overlay.Columns {
		return false
	}
	return b.Overlay.isSet(overlayRow, overlayCol)
}

// visible reports whether a pixel lies inside every present shutter shape.
func (s *Shutter) visible(row, col int) bool {
	if s.Rectangle != nil && !s.Rectangle.inside(row, col) {
		return false
	}
	if s.Circle != nil && !s.Circle.inside(row, col) {
		return false
	}
	if s.Polygon != nil && !s.Polygon.inside(row, col) {
		return false
	}
	if s.Bitmap != nil && s.Bitmap.masks(row, col) {
		return false
	}
	return true
}

// VisibleSourcePixel evaluates a shutter in the original zero-based image
// coordinates, independent of any regional texture origin or display reduction.
func (s *Shutter) VisibleSourcePixel(x, y int) bool { return s == nil || s.visible(y, x) }

// Apply masks every pixel outside the shutter region in place.
func (s *Shutter) Apply(img *image.Gray) {
	if s == nil || img == nil {
		return
	}
	// A shutter with no shapes masks nothing.
	if s.Rectangle == nil && s.Circle == nil && s.Polygon == nil && s.Bitmap == nil {
		return
	}
	bounds := img.Bounds()
	for row := 0; row < bounds.Dy(); row++ {
		for col := 0; col < bounds.Dx(); col++ {
			if !s.visible(row, col) {
				img.Pix[row*img.Stride+col] = s.PresentationValue
			}
		}
	}
}

// ApplyTo returns a shuttered copy of src without mutating render caches owned
// by the caller. Grayscale inputs remain grayscale; other image types are
// copied to RGBA and masked with an equivalent neutral gray.
func (s *Shutter) ApplyTo(src image.Image) image.Image {
	if s == nil || src == nil ||
		(s.Rectangle == nil && s.Circle == nil && s.Polygon == nil && s.Bitmap == nil) {
		return src
	}
	bounds := src.Bounds()
	if gray, ok := src.(*image.Gray); ok {
		out := image.NewGray(bounds)
		draw.Draw(out, bounds, gray, bounds.Min, draw.Src)
		s.Apply(out)
		return out
	}
	out := image.NewRGBA(bounds)
	draw.Draw(out, bounds, src, bounds.Min, draw.Src)
	masked := color.Gray{Y: s.PresentationValue}
	for row := 0; row < bounds.Dy(); row++ {
		for col := 0; col < bounds.Dx(); col++ {
			if !s.visible(row, col) {
				out.Set(bounds.Min.X+col, bounds.Min.Y+row, masked)
			}
		}
	}
	return out
}

// Overlay is a 1-bit DICOM overlay plane (repeating group 0x6000-0x601E,
// PS3.3 C.9 / PS3.5 8.1.2). Data is bit-packed row-major, least-significant bit
// first. Origin is the 1-based (row, column) position of the overlay's top-left
// pixel within the image (Overlay Origin (60xx,0050)). Composite and isSet use
// the first frame; Frame and ForImageFrame select others. Validate checks the
// attributes against the Overlay Plane module and the length of Data.
type Overlay struct {
	Rows, Columns int
	OriginRow     int
	OriginCol     int
	// Data holds Frames planes of Rows*Columns bits, packed back to back.
	Data []byte

	// Group is the repeating group (0x6000-0x601E) the overlay came from.
	Group uint16
	// Type is Overlay Type (60xx,0040): "G" graphics or "R" ROI.
	Type        string
	Subtype     string
	Label       string
	Description string
	// Frames is Number of Frames in Overlay (60xx,0015) and FrameOrigin the
	// 1-based Image Frame Origin (60xx,0051); see ForImageFrame.
	Frames      int
	FrameOrigin int
	// Embedded marks an overlay taken from unused bits of the image samples
	// at BitPosition (EmbeddedOverlays).
	Embedded    bool
	BitPosition int
	// ROI Area (60xx,1301), Mean (60xx,1302) and Standard Deviation
	// (60xx,1303), when HasROIStatistics.
	HasROIStatistics     bool
	ROIArea              int
	ROIMean              float64
	ROIStandardDeviation float64
}

// isSet reports whether the overlay bit at (row, col) within the overlay plane
// is set. Bits beyond Data read as unset.
func (o Overlay) isSet(row, col int) bool {
	if row < 0 || col < 0 || col >= o.Columns || row >= o.dataRows() {
		return false
	}
	// row < dataRows keeps the product within the data's bit count.
	idx := int64(row)*int64(o.Columns) + int64(col)
	if idx>>3 >= int64(len(o.Data)) {
		return false
	}
	return o.Data[idx>>3]&(1<<uint(idx&7)) != 0
}

// dataRows returns how many rows of the first plane Data reaches, counting a
// partly covered last row, capped at Rows.
func (o Overlay) dataRows() int {
	if o.Rows <= 0 || o.Columns <= 0 {
		return 0
	}
	columns := uint64(o.Columns)
	rows := (uint64(len(o.Data))*8 + columns - 1) / columns
	if rows < uint64(o.Rows) {
		return int(rows)
	}
	return o.Rows
}

// overlaySpan returns the overlay indexes [lo, hi) along one axis that land
// inside an image axis of size pixels, when overlay index 0 sits at the
// 1-based image position origin. It never overflows, whatever the inputs.
func overlaySpan(origin, extent, size int) (lo, hi int) {
	if extent <= 0 || size <= 0 || origin > size {
		return 0, 0
	}
	if origin >= 1 {
		return 0, min(extent, size-(origin-1))
	}
	// The first 1-origin overlay indexes lie before the image.
	if origin <= 1-extent {
		return 0, 0
	}
	lo = 1 - origin
	if size >= extent-lo {
		return lo, extent
	}
	return lo, lo + size
}

// Composite burns the first overlay plane onto img, writing value at every set
// overlay bit that falls within the image bounds. Only the part of the plane
// that overlaps both the image (after Overlay Origin) and Data is visited, so
// the work is bounded by the image size whatever Rows and Columns declare;
// bits beyond Data read as unset.
func (o Overlay) Composite(img *image.Gray, value uint8) {
	if img == nil {
		return
	}
	bounds := img.Bounds()
	width, height := bounds.Dx(), bounds.Dy()
	if width <= 0 || height <= 0 || img.Stride < width || len(img.Pix) < (height-1)*img.Stride+width {
		return
	}
	rowLo, rowHi := overlaySpan(o.OriginRow, o.dataRows(), height)
	colLo, colHi := overlaySpan(o.OriginCol, o.Columns, width)
	if rowLo >= rowHi || colLo >= colHi {
		return
	}
	// Within the spans, origin-1+index is an image coordinate in range.
	dataBits := int64(len(o.Data)) * 8
	for r := rowLo; r < rowHi; r++ {
		pix := img.Pix[(o.OriginRow-1+r)*img.Stride:]
		rowBit := int64(r) * int64(o.Columns)
		for c := colLo; c < colHi; c++ {
			idx := rowBit + int64(c)
			if idx >= dataBits {
				break
			}
			if o.Data[idx>>3]&(1<<uint(idx&7)) != 0 {
				pix[o.OriginCol-1+c] = value
			}
		}
	}
}
