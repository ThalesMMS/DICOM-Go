package display

import (
	"encoding/binary"
	"errors"
	"fmt"
	"image"
	"math"
	"strings"

	"github.com/ThalesMMS/dicom-go/core"
	"github.com/ThalesMMS/dicom-go/object"
)

// ErrInvalidCurve reports a retired Curve module group (50xx) whose
// attributes or Curve Data cannot be decoded.
var ErrInvalidCurve = errors.New("dicom: invalid curve")

// Curve groups and attributes of the retired Curve module (PS3.3-2004
// C.10.2). The module was retired in favour of Waveforms and Graphic
// Annotation; legacy US, NM and XA images still carry it.
const (
	curveGroupFirst = uint16(0x5000)
	curveGroupLast  = uint16(0x501E)

	tagCurveDimensions        = uint16(0x0005)
	tagCurveNumberOfPoints    = uint16(0x0010)
	tagCurveTypeOfData        = uint16(0x0020)
	tagCurveDescription       = uint16(0x0022)
	tagCurveAxisUnits         = uint16(0x0030)
	tagCurveAxisLabels        = uint16(0x0040)
	tagCurveDataValueRep      = uint16(0x0103)
	tagCurveDataDescriptor    = uint16(0x0110)
	tagCurveCoordinateStart   = uint16(0x0112)
	tagCurveCoordinateStep    = uint16(0x0114)
	tagCurveLabel             = uint16(0x2500)
	tagCurveData              = uint16(0x3000)
	maxCurveDimensions        = 16
	curveDescriptorByInterval = 0
)

// Data Value Representation (50xx,0103) enumerated values.
const (
	CurveValuesUS = 0 // unsigned 16-bit
	CurveValuesSS = 1 // signed 16-bit
	CurveValuesFL = 2 // 32-bit float
	CurveValuesFD = 3 // 64-bit float
	CurveValuesSL = 4 // signed 32-bit
)

// Curve is one decoded group of the retired Curve module.
type Curve struct {
	// Group is the curve group, 0x5000-0x501E.
	Group uint16
	// Dimensions is Curve Dimensions (50xx,0005), the size of each tuple.
	Dimensions int
	// TypeOfData is Type of Data (50xx,0020), upper case: TAC, PROF, HIST,
	// ROI, TABL, FILT, POLY, ECG, PRESSURE, FLOW, PHYSIO or RESP.
	TypeOfData  string
	Description string
	Label       string
	// AxisUnits and AxisLabels hold one value per dimension when present;
	// PIXL marks image pixel coordinates.
	AxisUnits  []string
	AxisLabels []string
	// ValueRepresentation is Data Value Representation (50xx,0103), one of
	// the CurveValues constants.
	ValueRepresentation int
	// Points holds Number of Points (50xx,0010) tuples of Dimensions values,
	// in Curve Data order. Dimensions described by interval spacing (Curve
	// Data Descriptor 0000H) are filled from Coordinate Start Value and
	// Coordinate Step Value.
	Points [][]float64
}

// CurvePoint is a point in image pixel coordinates: X is the column and Y the
// row, zero-based, with integers at pixel centres.
type CurvePoint struct {
	X, Y float64
}

// IsImageGraphic reports whether the curve is a graphic drawn on the image:
// Type of Data POLY (poly line) or ROI (region of interest outline) with at
// least two dimensions, whose first two axes are in pixels (PIXL) or carry no
// Axis Units.
func (c Curve) IsImageGraphic() bool {
	if c.TypeOfData != "POLY" && c.TypeOfData != "ROI" || c.Dimensions < 2 || len(c.Points) == 0 {
		return false
	}
	for axis := 0; axis < 2 && axis < len(c.AxisUnits); axis++ {
		if unit := strings.ToUpper(strings.TrimSpace(c.AxisUnits[axis])); unit != "" && unit != "PIXL" {
			return false
		}
	}
	return true
}

// Closed reports whether the graphic is a closed outline: ROI curves enclose
// a region, POLY curves are open poly lines.
func (c Curve) Closed() bool { return c.TypeOfData == "ROI" }

// ImagePoints returns the vertices of an image graphic curve in zero-based
// image coordinates, or false when the curve is not one (see
// IsImageGraphic). Curve Data stores (x, y) = (column, row) tuples in pixels
// with the top-left pixel at (1,1), the convention of Overlay Origin; the
// result subtracts one so that integers are pixel centres. Dimensions beyond
// the second are ignored.
func (c Curve) ImagePoints() ([]CurvePoint, bool) {
	if !c.IsImageGraphic() {
		return nil, false
	}
	out := make([]CurvePoint, 0, len(c.Points))
	for _, tuple := range c.Points {
		if len(tuple) < 2 || !finite(tuple[0]) || !finite(tuple[1]) {
			continue
		}
		out = append(out, CurvePoint{X: tuple[0] - 1, Y: tuple[1] - 1})
	}
	return out, len(out) > 0
}

// Composite draws an image graphic curve onto img as one-pixel lines through
// its vertices, closing ROI outlines, writing value at every pixel the lines
// cross inside the image. A single-point curve marks its pixel. Segments are
// clipped to the image first, so the work is bounded by the image size and
// the point count whatever the coordinates. Curves that are not image
// graphics draw nothing.
func (c Curve) Composite(img *image.Gray, value uint8) {
	points, ok := c.ImagePoints()
	if !ok || img == nil {
		return
	}
	bounds := img.Bounds()
	if bounds.Dx() <= 0 || bounds.Dy() <= 0 {
		return
	}
	set := func(x, y int) {
		if x >= 0 && y >= 0 && x < bounds.Dx() && y < bounds.Dy() {
			img.Pix[img.PixOffset(bounds.Min.X+x, bounds.Min.Y+y)] = value
		}
	}
	if len(points) == 1 {
		set(int(math.Round(points[0].X)), int(math.Round(points[0].Y)))
		return
	}
	last := len(points) - 1
	if c.Closed() && points[0] != points[last] {
		points = append(points, points[0])
		last++
	}
	maxX, maxY := float64(bounds.Dx())-0.5, float64(bounds.Dy())-0.5
	for i := 0; i < last; i++ {
		a, b, inside := clipSegment(points[i], points[i+1], -0.5, -0.5, maxX, maxY)
		if !inside {
			continue
		}
		drawLine(int(math.Round(a.X)), int(math.Round(a.Y)), int(math.Round(b.X)), int(math.Round(b.Y)), set)
	}
}

// drawLine visits the pixels of a Bresenham line from (x0,y0) to (x1,y1).
func drawLine(x0, y0, x1, y1 int, set func(x, y int)) {
	dx, dy := absInt(x1-x0), -absInt(y1-y0)
	sx, sy := 1, 1
	if x0 > x1 {
		sx = -1
	}
	if y0 > y1 {
		sy = -1
	}
	err := dx + dy
	for {
		set(x0, y0)
		if x0 == x1 && y0 == y1 {
			return
		}
		e2 := 2 * err
		if e2 >= dy {
			err += dy
			x0 += sx
		}
		if e2 <= dx {
			err += dx
			y0 += sy
		}
	}
}

// clipSegment clips the segment a-b to the rectangle [minX,maxX]x[minY,maxY]
// (Liang-Barsky) and reports false when no part of it lies inside.
func clipSegment(a, b CurvePoint, minX, minY, maxX, maxY float64) (CurvePoint, CurvePoint, bool) {
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
			if r > t1 {
				return a, b, false
			}
			if r > t0 {
				t0 = r
			}
		} else {
			if r < t0 {
				return a, b, false
			}
			if r < t1 {
				t1 = r
			}
		}
	}
	return CurvePoint{X: a.X + t0*dx, Y: a.Y + t0*dy}, CurvePoint{X: a.X + t1*dx, Y: a.Y + t1*dy}, true
}

func absInt(v int) int {
	if v < 0 {
		return -v
	}
	return v
}

func finite(v float64) bool { return !math.IsNaN(v) && !math.IsInf(v, 0) }

// CurvesFromObject decodes every retired Curve module group (5000-501E,
// even) that has Curve Data (50xx,3000). Groups that cannot be decoded are
// skipped and reported, joined, in an error wrapping ErrInvalidCurve; the
// valid curves are returned beside it. Audio curves (Audio Sample Data
// without Curve Data) are not curves here.
func CurvesFromObject(obj *object.Object) ([]Curve, error) {
	if obj == nil {
		return nil, nil
	}
	var curves []Curve
	var errs []error
	for group := curveGroupFirst; group <= curveGroupLast; group += 2 {
		data, ok := obj.GetRaw(core.NewTag(group, tagCurveData))
		if !ok {
			continue
		}
		curve, err := decodeCurve(obj, group, data)
		if err != nil {
			errs = append(errs, err)
			continue
		}
		curves = append(curves, curve)
	}
	return curves, errors.Join(errs...)
}

func decodeCurve(obj *object.Object, group uint16, data []byte) (Curve, error) {
	invalid := func(format string, args ...any) error {
		return fmt.Errorf("%w: group %#04x: %s", ErrInvalidCurve, group, fmt.Sprintf(format, args...))
	}
	curve := Curve{
		Group:       group,
		TypeOfData:  strings.ToUpper(overlayString(obj, group, tagCurveTypeOfData)),
		Description: overlayString(obj, group, tagCurveDescription),
		Label:       overlayString(obj, group, tagCurveLabel),
		AxisUnits:   curveStrings(obj, group, tagCurveAxisUnits),
		AxisLabels:  curveStrings(obj, group, tagCurveAxisLabels),
	}
	dims, ok := overlayUint16(obj, group, tagCurveDimensions)
	if !ok || dims < 1 || dims > maxCurveDimensions {
		return Curve{}, invalid("Curve Dimensions %d missing or outside 1-%d", dims, maxCurveDimensions)
	}
	curve.Dimensions = int(dims)
	points, ok := overlayUint16(obj, group, tagCurveNumberOfPoints)
	if !ok || points < 1 {
		return Curve{}, invalid("Number of Points missing or zero")
	}
	rep, ok := overlayUint16(obj, group, tagCurveDataValueRep)
	if !ok {
		return Curve{}, invalid("Data Value Representation missing")
	}
	size := curveValueSize(int(rep))
	if size == 0 {
		return Curve{}, invalid("Data Value Representation %d is not 0-4", rep)
	}
	curve.ValueRepresentation = int(rep)

	// Curve Data Descriptor: one value per dimension, 0 for a dimension
	// computed from Coordinate Start/Step and absent from Curve Data.
	order := obj.ValueByteOrder()
	descriptor := curveUint16s(obj, group, tagCurveDataDescriptor, order)
	if len(descriptor) != 0 && len(descriptor) != curve.Dimensions {
		return Curve{}, invalid("Curve Data Descriptor has %d values for %d dimensions", len(descriptor), curve.Dimensions)
	}
	starts := curveUint16s(obj, group, tagCurveCoordinateStart, order)
	steps := curveUint16s(obj, group, tagCurveCoordinateStep, order)
	stored := curve.Dimensions
	interval := make([]bool, curve.Dimensions)
	intervalStart := make([]float64, curve.Dimensions)
	intervalStep := make([]float64, curve.Dimensions)
	for axis, value := range descriptor {
		if value != curveDescriptorByInterval {
			continue
		}
		k := curve.Dimensions - stored // index among interval dimensions
		start, okStart := nthOrFirst(starts, k)
		step, okStep := nthOrFirst(steps, k)
		if !okStart || !okStep {
			return Curve{}, invalid("dimension %d uses interval spacing without Coordinate Start/Step Value", axis+1)
		}
		interval[axis], intervalStart[axis], intervalStep[axis] = true, float64(start), float64(step)
		stored--
	}
	need := uint64(points) * uint64(stored) * uint64(size)
	if uint64(len(data)) < need {
		return Curve{}, invalid("Curve Data has %d bytes, too few for %d point(s) of %d stored value(s) of %d bytes", len(data), points, stored, size)
	}
	curve.Points = make([][]float64, points)
	values := make([]float64, int(points)*curve.Dimensions)
	offset := 0
	for i := range curve.Points {
		tuple := values[i*curve.Dimensions : (i+1)*curve.Dimensions : (i+1)*curve.Dimensions]
		for axis := range tuple {
			if interval[axis] {
				tuple[axis] = intervalStart[axis] + intervalStep[axis]*float64(i)
				continue
			}
			tuple[axis] = curveValue(data[offset:offset+size], curve.ValueRepresentation, order)
			offset += size
		}
		curve.Points[i] = tuple
	}
	return curve, nil
}

func curveValueSize(rep int) int {
	switch rep {
	case CurveValuesUS, CurveValuesSS:
		return 2
	case CurveValuesFL, CurveValuesSL:
		return 4
	case CurveValuesFD:
		return 8
	}
	return 0
}

// curveValue decodes one Curve Data value in the data set byte order.
func curveValue(raw []byte, rep int, order binary.ByteOrder) float64 {
	switch rep {
	case CurveValuesUS:
		return float64(order.Uint16(raw))
	case CurveValuesSS:
		return float64(int16(order.Uint16(raw)))
	case CurveValuesFL:
		return float64(math.Float32frombits(order.Uint32(raw)))
	case CurveValuesFD:
		return math.Float64frombits(order.Uint64(raw))
	case CurveValuesSL:
		return float64(int32(order.Uint32(raw)))
	}
	return 0
}

func curveUint16s(obj *object.Object, group, element uint16, order binary.ByteOrder) []uint16 {
	raw, ok := obj.GetRaw(core.NewTag(group, element))
	if !ok {
		return nil
	}
	out := make([]uint16, 0, len(raw)/2)
	for i := 0; i+1 < len(raw); i += 2 {
		out = append(out, order.Uint16(raw[i:i+2]))
	}
	return out
}

func curveStrings(obj *object.Object, group, element uint16) []string {
	values, ok := obj.GetStrings(core.NewTag(group, element))
	if !ok {
		return nil
	}
	out := make([]string, len(values))
	for i, value := range values {
		out[i] = strings.TrimSpace(value)
	}
	return out
}

func nthOrFirst(values []uint16, n int) (uint16, bool) {
	if n < len(values) {
		return values[n], true
	}
	if len(values) > 0 {
		return values[0], true
	}
	return 0, false
}
