package render

import (
	"errors"
	"fmt"
	"math"
)

// Two-dimensional transfer function (#1232), after VTK's
// vtkVolumeProperty::SetTransferFunction2D and
// vtkOpenGLVolumeTransferFunction2D (written from their documented behaviour;
// no VTK code is copied). A DVR sample is classified by an RGBA table over
// its modality value (columns) and its covariant gradient magnitude in
// modality units per mm (rows), the same magnitude the gradient opacity
// function reads (#1010). Edges and homogeneous regions of equal value so
// receive different colours and opacities, which a 1D function times a
// separable gradient factor cannot express. While set, the table replaces
// the preset's scalar transfer function and its gradient opacity in DVR;
// shading, labels, clipping, fusion and the opacity step correction apply as
// before. Projections ignore it.

const (
	// DefaultVRTransfer2DWidth and DefaultVRTransfer2DHeight are the editor's
	// table size: 256 modality values by 128 gradient magnitudes.
	DefaultVRTransfer2DWidth  = 256
	DefaultVRTransfer2DHeight = 128
	// MaxVRTransfer2DSide bounds each axis of a table.
	MaxVRTransfer2DSide = 1024
	// MaxVRTransfer2DRegions bounds the regions of BakeVRTransfer2DRegions.
	MaxVRTransfer2DRegions = 16
)

// ErrInvalidVRTransfer2D reports a malformed 2D transfer function or region.
var ErrInvalidVRTransfer2D = errors.New("render: invalid VR 2D transfer function")

// VRTransfer2D is an immutable RGBA table over modality value and gradient
// magnitude. Column c is the value IntensityMin + c/(Width-1)·(IntensityMax −
// IntensityMin), row r the magnitude r/(Height-1)·GradientMax; Lookup
// interpolates bilinearly and clamps outside the table. Entries are linear
// light, non-premultiplied, with the opacity of OpacityUnitDistanceMM of the
// preset's transfer function.
type VRTransfer2D struct {
	width, height              int
	intensityMin, intensityMax float64
	gradientMax                float64
	table                      []RGBA
	// columnMaxAlpha is each column's largest opacity, a bound of every
	// sample at that value whatever its gradient.
	columnMaxAlpha []float64
}

// NewVRTransfer2D validates and copies a row-major table (row r, column c at
// r·width+c). Width and height are 2..MaxVRTransfer2DSide, the intensity range
// finite and increasing, gradientMax finite and positive, and every channel
// in [0, 1].
func NewVRTransfer2D(width, height int, intensityMin, intensityMax, gradientMax float64, table []RGBA) (*VRTransfer2D, error) {
	if width < 2 || height < 2 || width > MaxVRTransfer2DSide || height > MaxVRTransfer2DSide {
		return nil, fmt.Errorf("%w: size %dx%d", ErrInvalidVRTransfer2D, width, height)
	}
	if !finite(intensityMin) || !finite(intensityMax) || intensityMax <= intensityMin {
		return nil, fmt.Errorf("%w: intensity range %v..%v", ErrInvalidVRTransfer2D, intensityMin, intensityMax)
	}
	if !finite(gradientMax) || gradientMax <= 0 {
		return nil, fmt.Errorf("%w: gradient maximum %v", ErrInvalidVRTransfer2D, gradientMax)
	}
	if len(table) != width*height {
		return nil, fmt.Errorf("%w: %d entries for %dx%d", ErrInvalidVRTransfer2D, len(table), width, height)
	}
	for index, entry := range table {
		if !unitChannel(entry.R) || !unitChannel(entry.G) || !unitChannel(entry.B) || !unitChannel(entry.A) {
			return nil, fmt.Errorf("%w: entry %d %+v", ErrInvalidVRTransfer2D, index, entry)
		}
	}
	return newVRTransfer2D(width, height, intensityMin, intensityMax, gradientMax, append([]RGBA(nil), table...)), nil
}

func unitChannel(value float64) bool { return finite(value) && value >= 0 && value <= 1 }

func newVRTransfer2D(width, height int, intensityMin, intensityMax, gradientMax float64, table []RGBA) *VRTransfer2D {
	transfer := &VRTransfer2D{
		width: width, height: height,
		intensityMin: intensityMin, intensityMax: intensityMax, gradientMax: gradientMax,
		table: table, columnMaxAlpha: make([]float64, width),
	}
	for row := 0; row < height; row++ {
		for column, entry := range table[row*width : (row+1)*width] {
			transfer.columnMaxAlpha[column] = math.Max(transfer.columnMaxAlpha[column], entry.A)
		}
	}
	return transfer
}

// Size returns the table's columns (values) and rows (gradient magnitudes).
func (t *VRTransfer2D) Size() (width, height int) {
	if t == nil {
		return 0, 0
	}
	return t.width, t.height
}

// IntensityRange returns the modality values of the first and last columns.
func (t *VRTransfer2D) IntensityRange() (minimum, maximum float64) {
	if t == nil {
		return 0, 0
	}
	return t.intensityMin, t.intensityMax
}

// GradientMax returns the gradient magnitude of the last row, in modality
// units per mm.
func (t *VRTransfer2D) GradientMax() float64 {
	if t == nil {
		return 0
	}
	return t.gradientMax
}

// At returns the entry of a column and row, zero outside the table.
func (t *VRTransfer2D) At(column, row int) RGBA {
	if t == nil || column < 0 || row < 0 || column >= t.width || row >= t.height {
		return RGBA{}
	}
	return t.table[row*t.width+column]
}

// Table returns a copy of the row-major entries.
func (t *VRTransfer2D) Table() []RGBA {
	if t == nil {
		return nil
	}
	return append([]RGBA(nil), t.table...)
}

// columnPosition is the fractional column of a modality value, clamped.
func (t *VRTransfer2D) columnPosition(value float64) float64 {
	if !finite(value) {
		return 0
	}
	return clampUnit((value-t.intensityMin)/(t.intensityMax-t.intensityMin)) * float64(t.width-1)
}

// Lookup interpolates the table bilinearly at a modality value and gradient
// magnitude (modality units per mm), clamped to the table.
func (t *VRTransfer2D) Lookup(value, gradientMagnitude float64) RGBA {
	if t == nil {
		return RGBA{}
	}
	x := t.columnPosition(value)
	y := 0.0
	if finite(gradientMagnitude) {
		y = clampUnit(gradientMagnitude/t.gradientMax) * float64(t.height-1)
	}
	x0, y0 := min(int(x), t.width-2), min(int(y), t.height-2)
	fx, fy := x-float64(x0), y-float64(y0)
	row0, row1 := t.table[y0*t.width:], t.table[(y0+1)*t.width:]
	mix := func(a, b RGBA, f float64) RGBA {
		return RGBA{R: lerp(a.R, b.R, f), G: lerp(a.G, b.G, f), B: lerp(a.B, b.B, f), A: lerp(a.A, b.A, f)}
	}
	return mix(mix(row0[x0], row0[x0+1], fx), mix(row1[x0], row1[x0+1], fx), fy)
}

// maxAlphaAt bounds the opacity Lookup returns at a value for any gradient:
// zero means every gradient classifies the sample transparent, so the ray
// caster skips it without computing the gradient.
func (t *VRTransfer2D) maxAlphaAt(value float64) float64 {
	x := t.columnPosition(value)
	x0 := min(int(x), t.width-2)
	return lerp(t.columnMaxAlpha[x0], t.columnMaxAlpha[x0+1], x-float64(x0))
}

// VRTransfer2DFromTransferFunction samples a 1D transfer function, as DVR
// classifies a sample with it under window, into a table constant along the
// gradient axis: rendering with the table reproduces the 1D frame up to the
// table's sampling of the value axis. HU-domain functions are baked over
// [intensityMin, intensityMax], which DVR uses as the volume's value range.
func VRTransfer2DFromTransferFunction(tf VRTransferFunction, window WindowLevel, width, height int, intensityMin, intensityMax, gradientMax float64) (*VRTransfer2D, error) {
	if _, err := NewVRTransfer2D(width, height, intensityMin, intensityMax, gradientMax, make([]RGBA, max(width*height, 0))); err != nil {
		return nil, err
	}
	var lut VRLUT
	if tf.Domain() == VRTransferDomainHU {
		lut = tf.BakeLUT(intensityMin, intensityMax, tf.PreferredLUTSize())
	} else {
		lut = tf.BakeWindowLUT(window, tf.PreferredLUTSize())
	}
	table := make([]RGBA, width*height)
	for column := 0; column < width; column++ {
		value := intensityMin + float64(column)/float64(width-1)*(intensityMax-intensityMin)
		entry := dvrTransferSample(tf, lut, value, window, intensityMin, intensityMax)
		for row := 0; row < height; row++ {
			table[row*width+column] = entry
		}
	}
	return newVRTransfer2D(width, height, intensityMin, intensityMax, gradientMax, table), nil
}

// VRTransfer2DRegion is a rectangle of the value × gradient plane with a
// colour and an opacity, the authoring primitive of the 2D transfer editor.
// Softness in [0, 1] ramps the opacity from zero at the rectangle's edges to
// full over that fraction of its half-extent on each axis; edges on or beyond
// the table's domain are not softened, so a region from gradient zero keeps
// its opacity there.
type VRTransfer2DRegion struct {
	// IntensityMin and IntensityMax bound the modality values.
	IntensityMin, IntensityMax float64
	// GradientMin and GradientMax bound the gradient magnitude, in
	// modality units per mm.
	GradientMin, GradientMax float64
	// R, G and B are the sRGB colour in [0, 1].
	R, G, B float64
	// Opacity in [0, 1] is the opacity inside the rectangle.
	Opacity  float64
	Softness float64
}

// Validate checks a region: finite increasing ranges, non-negative
// gradients, and colour, opacity and softness in [0, 1].
func (region VRTransfer2DRegion) Validate() error {
	if !finite(region.IntensityMin) || !finite(region.IntensityMax) || region.IntensityMax <= region.IntensityMin ||
		!finite(region.GradientMin) || !finite(region.GradientMax) || region.GradientMin < 0 || region.GradientMax <= region.GradientMin {
		return fmt.Errorf("%w: region ranges %+v", ErrInvalidVRTransfer2D, region)
	}
	for _, channel := range []float64{region.R, region.G, region.B, region.Opacity, region.Softness} {
		if !unitChannel(channel) {
			return fmt.Errorf("%w: region channels %+v", ErrInvalidVRTransfer2D, region)
		}
	}
	return nil
}

// ValidateVRTransfer2DRegions checks 1..MaxVRTransfer2DRegions valid regions.
func ValidateVRTransfer2DRegions(regions []VRTransfer2DRegion) error {
	if len(regions) == 0 || len(regions) > MaxVRTransfer2DRegions {
		return fmt.Errorf("%w: %d regions, 1 to %d", ErrInvalidVRTransfer2D, len(regions), MaxVRTransfer2DRegions)
	}
	for index, region := range regions {
		if err := region.Validate(); err != nil {
			return fmt.Errorf("region %d: %w", index, err)
		}
	}
	return nil
}

// BakeVRTransfer2DRegions rasterizes regions into a width × height table
// over [intensityMin, intensityMax] × [0, gradientMax]. Where regions
// overlap their opacities combine as independent layers (1 − Π(1 − αᵢ)) and
// their colours mix in linear light weighted by opacity, so the order of the
// regions does not matter.
func BakeVRTransfer2DRegions(regions []VRTransfer2DRegion, width, height int, intensityMin, intensityMax, gradientMax float64) (*VRTransfer2D, error) {
	if err := ValidateVRTransfer2DRegions(regions); err != nil {
		return nil, err
	}
	if _, err := NewVRTransfer2D(width, height, intensityMin, intensityMax, gradientMax, make([]RGBA, max(width*height, 0))); err != nil {
		return nil, err
	}
	type linearRegion struct {
		VRTransfer2DRegion
		r, g, b float64
	}
	linear := make([]linearRegion, len(regions))
	for index, region := range regions {
		linear[index] = linearRegion{region, srgbToLinear(region.R), srgbToLinear(region.G), srgbToLinear(region.B)}
	}
	// ramp is a region's opacity profile along one axis at value v.
	ramp := func(v, lower, upper, domainLower, domainUpper, softness float64) float64 {
		if v < lower || v > upper {
			return 0
		}
		feather := softness * (upper - lower) / 2
		if feather <= 0 {
			return 1
		}
		weight := 1.0
		if lower > domainLower {
			weight = math.Min(weight, (v-lower)/feather)
		}
		if upper < domainUpper {
			weight = math.Min(weight, (upper-v)/feather)
		}
		return clampUnit(weight)
	}
	table := make([]RGBA, width*height)
	for row := 0; row < height; row++ {
		gradient := float64(row) / float64(height-1) * gradientMax
		for column := 0; column < width; column++ {
			value := intensityMin + float64(column)/float64(width-1)*(intensityMax-intensityMin)
			transparency, weight := 1.0, 0.0
			var r, g, b float64
			for _, region := range linear {
				alpha := region.Opacity *
					ramp(value, region.IntensityMin, region.IntensityMax, intensityMin, intensityMax, region.Softness) *
					ramp(gradient, region.GradientMin, region.GradientMax, 0, gradientMax, region.Softness)
				if alpha <= 0 {
					continue
				}
				transparency *= 1 - alpha
				weight += alpha
				r, g, b = r+alpha*region.r, g+alpha*region.g, b+alpha*region.b
			}
			if weight > 0 {
				table[row*width+column] = RGBA{R: clampUnit(r / weight), G: clampUnit(g / weight), B: clampUnit(b / weight), A: clampUnit(1 - transparency)}
			}
		}
	}
	return newVRTransfer2D(width, height, intensityMin, intensityMax, gradientMax, table), nil
}
