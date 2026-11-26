// Package colorbar builds calibrated colour legends (scalar bars) for
// false-colour presentations of DICOM images: pseudocolour palettes applied
// after VOI windowing, PET/SPECT fusion overlays, and banded overlays such as
// Parametric Map value bands or RT Dose isodose levels.
//
// A Legend is presentation-neutral data. It carries the calibrated value
// range, the unit, labelled ticks at "nice" values and an exact value-to-colour
// mapping, so a user interface only draws what the legend describes and tests
// can check the legend against the colours actually painted in the image.
//
// Orientation convention: position 0 is the bottom of the bar (Min) and
// position 1 is the top (Max). Raster renders the top row first.
package colorbar

import (
	"errors"
	"fmt"
	"image"
	"image/color"
	"math"
	"strings"

	"github.com/ThalesMMS/dicom-go/pixeldata/display"
	"github.com/ThalesMMS/dicom-go/render"
)

// Kind distinguishes a continuous ramp from a set of discrete value bands.
type Kind int

const (
	// Continuous legends map every value in [Min, Max] through a colour
	// function, e.g. a windowed pseudocolour palette.
	Continuous Kind = iota
	// Discrete legends list contiguous value bands, e.g. isodose levels.
	// Bands are drawn with equal height regardless of their value width so
	// narrow bands stay legible; tick positions follow the band boundaries.
	Discrete
)

// DefaultMaxTicks is the tick budget used when a spec leaves MaxTicks unset.
const DefaultMaxTicks = 6

// minimumRasterHeight is the smallest bar raster that can show a gradient.
const minimumRasterHeight = 2

var (
	// ErrInvalidRange reports a missing, non-finite or empty value range.
	ErrInvalidRange = errors.New("colorbar: invalid value range")
	// ErrNonAffineCalibration reports a calibration that is not an increasing
	// affine transform, so tick positions could not be derived linearly.
	ErrNonAffineCalibration = errors.New("colorbar: calibration must be an increasing affine transform")
	// ErrNoBands reports a discrete legend without usable bands.
	ErrNoBands = errors.New("colorbar: no usable bands")
)

// Tick is one labelled value on the legend axis.
type Tick struct {
	// Value is the calibrated value in Legend.Unit.
	Value float64
	// Position is the normalized bar position: 0 bottom (Min), 1 top (Max).
	Position float64
	// Label is Value formatted with the precision implied by the tick step.
	Label string
}

// Band is one contiguous value interval of a discrete legend.
type Band struct {
	// Low and High bound the calibrated band; a value v belongs to the band
	// when Low <= v < High, and the top band also includes High.
	Low, High float64
	// Color is the display colour the overlay paints for the band.
	Color color.NRGBA
	// Label optionally names the band (for example "95%").
	Label string
}

// Legend is a calibrated colour legend ready to draw.
type Legend struct {
	// Title names the mapping, for example "Pseudocolour · Jet" or "RT Dose".
	Title string
	// Unit is the calibrated unit of every value, for example HU, SUVbw, Gy.
	// It is empty only when the source values have no defined unit.
	Unit string
	// Kind selects continuous or discrete presentation.
	Kind Kind
	// Min and Max are the calibrated values at the bottom and top of the bar.
	Min, Max float64
	// Ticks lists labelled axis values in increasing order.
	Ticks []Tick
	// Bands lists discrete bands bottom-to-top; nil for continuous legends.
	Bands []Band

	colorAt func(float64) color.NRGBA
}

// Valid reports whether the legend has a finite, non-empty value range and a
// colour source.
func (l Legend) Valid() bool {
	if !finite(l.Min) || !finite(l.Max) || l.Max <= l.Min {
		return false
	}
	if l.Kind == Discrete {
		return len(l.Bands) > 0
	}
	return l.colorAt != nil
}

// Header returns the title followed by the bracketed unit, e.g.
// "PET · Hot Iron [SUVbw]".
func (l Legend) Header() string {
	title := strings.TrimSpace(l.Title)
	unit := strings.TrimSpace(l.Unit)
	switch {
	case title == "":
		if unit == "" {
			return ""
		}
		return "[" + unit + "]"
	case unit == "":
		return title
	default:
		return title + " [" + unit + "]"
	}
}

// ColorAt returns the colour the legend assigns to a calibrated value. For a
// continuous legend the mapping is exact (the same function used to build the
// bar) and values outside [Min, Max] return the colour the source mapping
// gives them, which for a VOI window is the clamped end colour. For a discrete
// legend it returns false when the value lies outside every band, i.e. where
// the overlay paints nothing.
func (l Legend) ColorAt(value float64) (color.NRGBA, bool) {
	if math.IsNaN(value) {
		return color.NRGBA{}, false
	}
	if l.Kind == Discrete {
		index := l.bandIndex(value)
		if index < 0 {
			return color.NRGBA{}, false
		}
		return l.Bands[index].Color, true
	}
	if l.colorAt == nil {
		return color.NRGBA{}, false
	}
	return l.colorAt(value), true
}

// Position maps a calibrated value to its normalized bar position, clamped to
// [0, 1]. Discrete legends use piecewise-linear positions inside equal-height
// bands.
func (l Legend) Position(value float64) float64 {
	if !l.Valid() || math.IsNaN(value) {
		return 0
	}
	if l.Kind == Discrete {
		n := len(l.Bands)
		if value <= l.Bands[0].Low {
			return 0
		}
		if value >= l.Bands[n-1].High {
			return 1
		}
		for index, band := range l.Bands {
			if value < band.High || index == n-1 {
				width := band.High - band.Low
				fraction := 0.0
				if width > 0 {
					fraction = (value - band.Low) / width
				}
				return clamp01((float64(index) + clamp01(fraction)) / float64(n))
			}
		}
		return 1
	}
	return clamp01((value - l.Min) / (l.Max - l.Min))
}

// ValueAtPosition is the inverse of Position for continuous legends and
// returns the band midpoint for discrete legends.
func (l Legend) ValueAtPosition(position float64) float64 {
	position = clamp01(position)
	if l.Kind == Discrete && len(l.Bands) > 0 {
		index := int(math.Floor(position * float64(len(l.Bands))))
		if index >= len(l.Bands) {
			index = len(l.Bands) - 1
		}
		band := l.Bands[index]
		return (band.Low + band.High) / 2
	}
	return l.Min + position*(l.Max-l.Min)
}

// Raster draws the bar as a width×height image, top row = Max. Each row is
// sampled at its centre, so a 256-row bar of an 8-bit palette shows every
// display level. A zero-size or invalid legend returns nil.
func (l Legend) Raster(width, height int) *image.NRGBA {
	if width <= 0 || height < minimumRasterHeight || !l.Valid() {
		return nil
	}
	out := image.NewNRGBA(image.Rect(0, 0, width, height))
	for y := 0; y < height; y++ {
		position := 1 - (float64(y)+0.5)/float64(height)
		var c color.NRGBA
		if l.Kind == Discrete {
			index := int(math.Floor(position * float64(len(l.Bands))))
			if index >= len(l.Bands) {
				index = len(l.Bands) - 1
			}
			c = l.Bands[index].Color
		} else {
			c = l.colorAt(l.ValueAtPosition(position))
		}
		row := out.Pix[y*out.Stride : y*out.Stride+width*4]
		for x := 0; x < width; x++ {
			row[x*4+0], row[x*4+1], row[x*4+2], row[x*4+3] = c.R, c.G, c.B, c.A
		}
	}
	return out
}

// Signature is a compact, deterministic description of everything a legend
// draws. User interfaces use it to skip redundant redraws.
func (l Legend) Signature() string {
	var b strings.Builder
	fmt.Fprintf(&b, "%d|%s|%s|%g|%g", l.Kind, l.Title, l.Unit, l.Min, l.Max)
	for _, tick := range l.Ticks {
		fmt.Fprintf(&b, "|t%s@%.4f", tick.Label, tick.Position)
	}
	for _, band := range l.Bands {
		fmt.Fprintf(&b, "|b%g:%g:%02x%02x%02x%02x", band.Low, band.High, band.Color.R, band.Color.G, band.Color.B, band.Color.A)
	}
	if l.Kind == Continuous && l.colorAt != nil && l.Valid() {
		for step := 0; step <= 16; step++ {
			c := l.colorAt(l.ValueAtPosition(float64(step) / 16))
			fmt.Fprintf(&b, "|c%02x%02x%02x%02x", c.R, c.G, c.B, c.A)
		}
	}
	return b.String()
}

func (l Legend) bandIndex(value float64) int {
	n := len(l.Bands)
	for index, band := range l.Bands {
		if value >= band.Low && (value < band.High || (index == n-1 && value <= band.High)) {
			return index
		}
	}
	return -1
}

// WindowSpec describes a palette applied to VOI-windowed modality values.
type WindowSpec struct {
	// Title and Unit label the legend. Unit is the unit of the calibrated
	// values (after Calibration), e.g. "HU" or "SUVbw".
	Title, Unit string
	// Window is the VOI window or VOI LUT in rescaled modality units, exactly
	// as passed to render.RenderFrame.
	Window render.WindowLevel
	// Palette maps the 8-bit windowed display level to a colour. Nil draws
	// the plain grayscale ramp.
	Palette func(gray uint8) color.NRGBA
	// Invert reports a presentation inversion applied to the windowed gray
	// level before the palette (MONOCHROME1 display or a user "invert").
	Invert bool
	// Calibration converts rescaled modality values to the legend unit, for
	// example PET Bq/ml to SUVbw. It must be an increasing affine transform;
	// nil means the rescaled values are already in Unit.
	Calibration func(float64) (float64, bool)
	// MaxTicks bounds the number of ticks; zero uses DefaultMaxTicks.
	MaxTicks int
}

// WindowRange returns the rescaled modality value interval that a VOI window
// spreads over the display range. LINEAR follows PS3.3 C.11.2.1.2.1
// (c - 0.5 ± (w - 1)/2), LINEAR_EXACT follows C.11.2.1.3.2 (c ± w/2) and
// SIGMOID uses c ± w/2, where its output passes about 12% and 88%. A VOI LUT
// uses its mapped input domain.
func WindowRange(window render.WindowLevel) (low, high float64, ok bool) {
	if window.LUT != nil {
		entries := len(window.LUT.Entries)
		if entries < 2 {
			return 0, 0, false
		}
		low = float64(window.LUT.FirstMapped)
		return low, low + float64(entries-1), true
	}
	if !finite(window.Center) || !finite(window.Width) || window.Width <= 0 {
		return 0, 0, false
	}
	switch window.Function {
	case display.VOILinearExact, display.VOISigmoid:
		low, high = window.Center-window.Width/2, window.Center+window.Width/2
	default:
		if window.Width < 1 {
			return 0, 0, false
		}
		low = window.Center - 0.5 - (window.Width-1)/2
		high = window.Center - 0.5 + (window.Width-1)/2
		if high <= low {
			// A LINEAR window of width 1 is a threshold at c - 0.5; give the
			// legend a unit-wide interval centred on it.
			low, high = window.Center-1, window.Center
		}
	}
	return low, high, high > low
}

// ForWindow builds a continuous legend for a palette applied after VOI
// windowing. Colours are computed with render.WindowedGray, the function the
// renderer uses, so every colour on the bar equals the colour the image shows
// for that value.
func ForWindow(spec WindowSpec) (Legend, error) {
	low, high, ok := WindowRange(spec.Window)
	if !ok {
		return Legend{}, ErrInvalidRange
	}
	displayLow, displayHigh := low, high
	if spec.Calibration != nil {
		var okLow, okHigh bool
		displayLow, okLow = spec.Calibration(low)
		displayHigh, okHigh = spec.Calibration(high)
		mid, okMid := spec.Calibration((low + high) / 2)
		if !okLow || !okHigh || !okMid || !finite(displayLow) || !finite(displayHigh) || displayHigh <= displayLow {
			return Legend{}, ErrNonAffineCalibration
		}
		want := (displayLow + displayHigh) / 2
		if math.Abs(mid-want) > 1e-9*math.Max(1, math.Abs(displayHigh-displayLow)) {
			return Legend{}, ErrNonAffineCalibration
		}
	}
	window := spec.Window
	palette := spec.Palette
	invert := spec.Invert
	colorAt := func(display float64) color.NRGBA {
		raw := display
		if spec.Calibration != nil {
			raw = low + (display-displayLow)/(displayHigh-displayLow)*(high-low)
		}
		gray := render.WindowedGray(raw, window)
		if invert {
			gray = 255 - gray
		}
		if palette == nil {
			return color.NRGBA{R: gray, G: gray, B: gray, A: 255}
		}
		return palette(gray)
	}
	legend := Legend{
		Title:   spec.Title,
		Unit:    spec.Unit,
		Kind:    Continuous,
		Min:     displayLow,
		Max:     displayHigh,
		colorAt: colorAt,
	}
	legend.Ticks = continuousTicks(displayLow, displayHigh, spec.MaxTicks)
	return legend, nil
}

// ForFunction builds a continuous legend from an arbitrary calibrated
// value-to-colour function over [min, max].
func ForFunction(title, unit string, min, max float64, colorAt func(float64) color.NRGBA, maxTicks int) (Legend, error) {
	if !finite(min) || !finite(max) || max <= min {
		return Legend{}, ErrInvalidRange
	}
	if colorAt == nil {
		return Legend{}, errors.New("colorbar: nil colour function")
	}
	return Legend{
		Title: title, Unit: unit, Kind: Continuous, Min: min, Max: max,
		Ticks: continuousTicks(min, max, maxTicks), colorAt: colorAt,
	}, nil
}

// ForBands builds a discrete legend. Bands are sorted by the caller from low
// to high and must be contiguous and non-empty (Low < High, each Low equal to
// the previous High within a relative tolerance); one tick is emitted per
// boundary.
func ForBands(title, unit string, bands []Band) (Legend, error) {
	if len(bands) == 0 {
		return Legend{}, ErrNoBands
	}
	out := make([]Band, len(bands))
	copy(out, bands)
	for index, band := range out {
		if !finite(band.Low) || !finite(band.High) || band.High <= band.Low {
			return Legend{}, fmt.Errorf("%w: band %d [%g, %g]", ErrNoBands, index, band.Low, band.High)
		}
		if index > 0 {
			previous := out[index-1].High
			if math.Abs(band.Low-previous) > 1e-9*math.Max(1, math.Abs(previous)) {
				return Legend{}, fmt.Errorf("%w: band %d does not start at %g", ErrNoBands, index, previous)
			}
			out[index].Low = previous
		}
	}
	legend := Legend{
		Title: title, Unit: unit, Kind: Discrete,
		Min: out[0].Low, Max: out[len(out)-1].High, Bands: out,
	}
	boundaries := make([]float64, 0, len(out)+1)
	boundaries = append(boundaries, out[0].Low)
	for _, band := range out {
		boundaries = append(boundaries, band.High)
	}
	decimals := boundaryDecimals(boundaries)
	legend.Ticks = make([]Tick, len(boundaries))
	for index, value := range boundaries {
		legend.Ticks[index] = Tick{
			Value:    value,
			Position: float64(index) / float64(len(out)),
			Label:    formatDecimals(value, decimals),
		}
	}
	return legend, nil
}

func continuousTicks(low, high float64, maxTicks int) []Tick {
	values, step := NiceTicks(low, high, maxTicks)
	ticks := make([]Tick, 0, len(values))
	for _, value := range values {
		ticks = append(ticks, Tick{
			Value:    value,
			Position: clamp01((value - low) / (high - low)),
			Label:    FormatValue(value, step),
		})
	}
	return ticks
}

// boundaryDecimals chooses one fixed precision for all band boundaries: at
// least three significant digits of the largest magnitude, and enough digits
// to tell the two closest boundaries apart (e.g. 49.4 and 52.0 Gy).
func boundaryDecimals(boundaries []float64) int {
	largest, closest := 0.0, math.Inf(1)
	for index, value := range boundaries {
		largest = math.Max(largest, math.Abs(value))
		if index > 0 {
			closest = math.Min(closest, boundaries[index]-boundaries[index-1])
		}
	}
	decimals := 0
	if largest > 0 {
		decimals = 2 - int(math.Floor(math.Log10(largest)))
	}
	if finite(closest) && closest > 0 {
		decimals = max(decimals, decimalsForStep(closest))
	}
	return max(0, min(maxDecimals, decimals))
}

func finite(value float64) bool {
	return !math.IsNaN(value) && !math.IsInf(value, 0)
}

func clamp01(value float64) float64 {
	return math.Max(0, math.Min(1, value))
}
