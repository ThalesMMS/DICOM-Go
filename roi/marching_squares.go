package roi

// Sub-pixel iso-contours by marching squares (#1075), written from the
// algorithm (Lorensen and Cline's marching cubes in two dimensions, the
// behaviour of VTK's vtkMarchingSquares and vtkFlyingEdges2D): the field is
// sampled at pixel centres, every cell of four neighbouring samples is
// classified by which corners lie at or above the level, and the crossings
// are placed on the cell edges by linear interpolation. Saddle cells are
// resolved by the mean of the four corners. Samples outside the grid (and
// NaN) count as below the level, so every contour is closed; a contour
// leaving the grid runs along the outer edge of the border pixels.

import (
	"errors"
	"fmt"
	"math"
	"sort"
)

// ErrIsoContour reports a field or options that cannot be contoured.
var ErrIsoContour = errors.New("roi: invalid iso-contour input")

// IsoContour is one closed iso-line in pixel coordinates: X is the column and
// Y the row of the pixel centres, so (0, 0) is the centre of the first
// pixel. The region at or above the level lies on the right of the direction
// of travel in image coordinates (y down): outer boundaries have a positive
// and holes a negative ClosedPolygonArea.
type IsoContour struct {
	Points []Point2D
}

// maxIsoContourSamples bounds the grid of one call (a 16k × 16k slice).
const maxIsoContourSamples = 1 << 28

func validIsoContourGrid(width, height int) bool {
	return width > 0 && height > 0 && width <= maxIsoContourSamples/height
}

// IsoContours traces the closed iso-lines of a width × height field sampled
// by value at level, in a deterministic order.
func IsoContours(width, height int, value func(x, y int) float64, level float64) ([]IsoContour, error) {
	if !validIsoContourGrid(width, height) || value == nil || math.IsNaN(level) || math.IsInf(level, 0) {
		return nil, fmt.Errorf("%w: %d×%d field at level %v", ErrIsoContour, width, height, level)
	}
	// The padded grid has a ring of below-level samples.
	pw, ph := width+2, height+2
	sample := make([]float64, pw*ph)
	for i := range sample {
		sample[i] = math.Inf(-1)
	}
	for y := 0; y < height; y++ {
		for x := 0; x < width; x++ {
			if v := value(x, y); !math.IsNaN(v) {
				sample[(y+1)*pw+x+1] = v
			}
		}
	}
	return traceIsoContours(sample, pw, ph, level), nil
}

type isoSegment struct {
	from, to   int // edge keys
	start, end Point2D
}

// traceIsoContours contours a padded grid whose border is below level.
func traceIsoContours(sample []float64, pw, ph int, level float64) []IsoContour {
	at := func(i, j int) float64 { return sample[j*pw+i] }
	// Edge keys: 2·(j·pw+i) for the edge from (i,j) to (i+1,j), plus one for
	// the edge from (i,j) to (i,j+1).
	crossing := func(ax, ay, bx, by int) Point2D {
		va, vb := at(ax, ay), at(bx, by)
		// Beyond the grid (the padding) the crossing is half-way, on the
		// edge of the last pixel.
		t := 0.5
		if !math.IsInf(va, 0) && !math.IsInf(vb, 0) && va != vb {
			t = (level - va) / (vb - va)
		}
		return Point2D{X: float64(ax) + t*float64(bx-ax) - 1, Y: float64(ay) + t*float64(by-ay) - 1}
	}
	var segments []isoSegment
	for j := 0; j+1 < ph; j++ {
		for i := 0; i+1 < pw; i++ {
			// Corners clockwise on screen: top-left, top-right, bottom-right,
			// bottom-left; edge k joins corner k to corner k+1.
			cx := [4]int{i, i + 1, i + 1, i}
			cy := [4]int{j, j, j + 1, j + 1}
			var inside [4]bool
			count := 0
			for k := 0; k < 4; k++ {
				inside[k] = at(cx[k], cy[k]) >= level
				if inside[k] {
					count++
				}
			}
			if count == 0 || count == 4 {
				continue
			}
			edgeKey := [4]int{2 * (j*pw + i), 2*(j*pw+i+1) + 1, 2 * ((j+1)*pw + i), 2*(j*pw+i) + 1}
			type cross struct {
				key  int
				p    Point2D
				exit bool
			}
			var crosses []cross
			for k := 0; k < 4; k++ {
				n := (k + 1) % 4
				if inside[k] == inside[n] {
					continue
				}
				crosses = append(crosses, cross{key: edgeKey[k], p: crossing(cx[k], cy[k], cx[n], cy[n]), exit: inside[k]})
			}
			connected := true // the inside corners of a saddle are joined
			if len(crosses) == 4 {
				centre := 0.0
				for k := 0; k < 4; k++ {
					centre += at(cx[k], cy[k])
				}
				connected = centre/4 >= level
			}
			// Each exit pairs with the next entry along the loop when the
			// inside is connected, else with the previous one; the segment
			// runs from the exit to the entry, keeping the inside on the right.
			for c, x := range crosses {
				if !x.exit {
					continue
				}
				step := 1
				if !connected {
					step = -1
				}
				e := (c + step + len(crosses)) % len(crosses)
				for crosses[e].exit {
					e = (e + step + len(crosses)) % len(crosses)
				}
				segments = append(segments, isoSegment{from: x.key, to: crosses[e].key, start: x.p, end: crosses[e].p})
			}
		}
	}
	byStart := make(map[int]int, len(segments))
	for s, segment := range segments {
		byStart[segment.from] = s
	}
	used := make([]bool, len(segments))
	var contours []IsoContour
	for s := range segments {
		if used[s] {
			continue
		}
		var points []Point2D
		for cur := s; !used[cur]; {
			used[cur] = true
			points = appendDistinct(points, segments[cur].start)
			next, ok := byStart[segments[cur].to]
			if !ok {
				break
			}
			cur = next
		}
		if len(points) > 1 && points[0] == points[len(points)-1] {
			points = points[:len(points)-1]
		}
		if len(points) >= 3 {
			contours = append(contours, IsoContour{Points: points})
		}
	}
	return contours
}

func appendDistinct(points []Point2D, p Point2D) []Point2D {
	if n := len(points); n > 0 && points[n-1] == p {
		return points
	}
	return append(points, p)
}

// SubpixelContourOptions configures RasterMask.SubpixelContours.
type SubpixelContourOptions struct {
	// SmoothSigma, when positive, blurs the mask with a Gaussian of this σ in
	// pixels before contouring at one half, rounding the staircase of the
	// pixel edges; zero contours the mask itself.
	SmoothSigma float64
}

// SubpixelContours returns the boundaries of the mask as closed sub-pixel
// contours at the half-way level between the pixel centres inside and
// outside: outer boundaries turn clockwise on screen (positive
// ClosedPolygonArea) and holes the other way, so the set is exact under the
// even-odd (XOR) rule, the convention of CLOSEDPLANAR_XOR RT Structure Set
// contours. Without smoothing the contours pass half-way between pixel
// centres, so filling them by pixel centre gives the mask back exactly.
func (m *RasterMask) SubpixelContours(opts SubpixelContourOptions) ([]IsoContour, error) {
	if m == nil {
		return nil, fmt.Errorf("%w: empty mask", ErrIsoContour)
	}
	if !validIsoContourGrid(m.Columns, m.Rows) {
		return nil, fmt.Errorf("%w: %d×%d mask", ErrIsoContour, m.Columns, m.Rows)
	}
	if opts.SmoothSigma < 0 || math.IsNaN(opts.SmoothSigma) || opts.SmoothSigma > 64 {
		return nil, fmt.Errorf("%w: smoothing σ %v", ErrIsoContour, opts.SmoothSigma)
	}
	if m.Empty() {
		return nil, nil
	}
	field := make([]float64, m.Columns*m.Rows)
	for y := 0; y < m.Rows; y++ {
		for _, run := range m.runs[y] {
			for x := run.Start; x < run.End; x++ {
				field[y*m.Columns+x] = 1
			}
		}
	}
	if opts.SmoothSigma > 0 {
		field = gaussianBlur2D(field, m.Columns, m.Rows, opts.SmoothSigma)
	}
	return IsoContours(m.Columns, m.Rows, func(x, y int) float64 { return field[y*m.Columns+x] }, 0.5)
}

// gaussianBlur2D is a separable, truncated (3σ) Gaussian with zero outside
// the image.
func gaussianBlur2D(field []float64, w, h int, sigma float64) []float64 {
	radius := int(math.Ceil(3 * sigma))
	kernel := make([]float64, 2*radius+1)
	sum := 0.0
	for i := range kernel {
		d := float64(i - radius)
		kernel[i] = math.Exp(-d * d / (2 * sigma * sigma))
		sum += kernel[i]
	}
	for i := range kernel {
		kernel[i] /= sum
	}
	tmp := make([]float64, len(field))
	for y := 0; y < h; y++ {
		for x := 0; x < w; x++ {
			v := 0.0
			for k, weight := range kernel {
				if sx := x + k - radius; sx >= 0 && sx < w {
					v += weight * field[y*w+sx]
				}
			}
			tmp[y*w+x] = v
		}
	}
	out := make([]float64, len(field))
	for y := 0; y < h; y++ {
		for x := 0; x < w; x++ {
			v := 0.0
			for k, weight := range kernel {
				if sy := y + k - radius; sy >= 0 && sy < h {
					v += weight * tmp[sy*w+x]
				}
			}
			out[y*w+x] = v
		}
	}
	return out
}

// LabelContours is the boundary set of one label of a label field.
type LabelContours struct {
	Label    int
	Contours []IsoContour
}

// LabelIsoContours contours every non-zero label of a width × height label
// field (as vtkDiscreteFlyingEdges2D does), in ascending label order. Each
// label is contoured half-way between its pixel centres and those of any
// other label, with the orientation of SubpixelContours, so the set of each
// label is exact under the even-odd rule.
func LabelIsoContours(width, height int, label func(x, y int) int) ([]LabelContours, error) {
	if !validIsoContourGrid(width, height) || label == nil {
		return nil, fmt.Errorf("%w: %d×%d label field", ErrIsoContour, width, height)
	}
	labels := make([]int, width*height)
	seen := map[int]bool{}
	for y := 0; y < height; y++ {
		for x := 0; x < width; x++ {
			value := label(x, y)
			labels[y*width+x] = value
			if value != 0 {
				seen[value] = true
			}
		}
	}
	ordered := make([]int, 0, len(seen))
	for value := range seen {
		ordered = append(ordered, value)
	}
	sort.Ints(ordered)
	out := make([]LabelContours, 0, len(ordered))
	for _, value := range ordered {
		contours, err := IsoContours(width, height, func(x, y int) float64 {
			if labels[y*width+x] == value {
				return 1
			}
			return 0
		}, 0.5)
		if err != nil {
			return nil, err
		}
		out = append(out, LabelContours{Label: value, Contours: contours})
	}
	return out, nil
}
