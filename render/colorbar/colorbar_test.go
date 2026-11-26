package colorbar

import (
	"encoding/binary"
	"errors"
	"image/color"
	"math"
	"reflect"
	"testing"

	"github.com/ThalesMMS/dicom-go/pixeldata"
	"github.com/ThalesMMS/dicom-go/pixeldata/display"
	"github.com/ThalesMMS/dicom-go/render"
)

// syntheticFrame is a PHI-free signed 16-bit CT-like ramp with a HU rescale.
func syntheticFrame(stored []int16, slope, intercept float64) *render.Frame {
	data := make([]byte, 2*len(stored))
	for i, value := range stored {
		binary.LittleEndian.PutUint16(data[2*i:], uint16(value))
	}
	return &render.Frame{
		Metadata: pixeldata.Metadata{
			Rows: 1, Columns: uint16(len(stored)), SamplesPerPixel: 1,
			BitsAllocated: 16, BitsStored: 16, HighBit: 15, PixelRepresentation: 1,
			PhotometricInterpretation: "MONOCHROME2",
		},
		ByteOrder:  binary.LittleEndian,
		PixelBytes: data,
		Rescale:    render.Rescale{Slope: slope, Intercept: intercept},
	}
}

func hotIron(gray uint8) color.NRGBA {
	r, g, b := display.HotIronLUT().At(int(gray))
	return color.NRGBA{R: r, G: g, B: b, A: 255}
}

// The acceptance criterion: legend colours equal the colours the renderer
// paints after the same window and palette, for every sampled value.
func TestForWindowMatchesRenderedPaletteColours(t *testing.T) {
	stored := make([]int16, 0, 401)
	for value := -200; value <= 200; value++ {
		stored = append(stored, int16(value))
	}
	const slope, intercept = 2.0, -1000.0
	frame := syntheticFrame(stored, slope, intercept)
	for _, window := range []render.WindowLevel{
		{Center: -600, Width: 500},
		{Center: -1000, Width: 801, Function: display.VOILinearExact},
		{Center: -900, Width: 300, Function: display.VOISigmoid},
	} {
		img, err := render.RenderFrame(frame, window)
		if err != nil {
			t.Fatalf("RenderFrame: %v", err)
		}
		for _, invert := range []bool{false, true} {
			legend, err := ForWindow(WindowSpec{Title: "Pseudocolour", Unit: "HU", Window: window, Palette: hotIron, Invert: invert})
			if err != nil {
				t.Fatalf("ForWindow(%+v): %v", window, err)
			}
			for x, value := range stored {
				r, _, _, _ := img.At(x, 0).RGBA()
				gray := uint8(r >> 8)
				if invert {
					gray = 255 - gray
				}
				want := hotIron(gray)
				got, ok := legend.ColorAt(float64(value)*slope + intercept)
				if !ok || got != want {
					t.Fatalf("window %+v invert=%v value %v HU: legend %v, rendered %v", window, invert, float64(value)*slope+intercept, got, want)
				}
			}
		}
	}
}

func TestForWindowRangeTicksAndRaster(t *testing.T) {
	legend, err := ForWindow(WindowSpec{Title: "CT", Unit: "HU", Window: render.WindowLevel{Center: 40, Width: 401}, Palette: hotIron})
	if err != nil {
		t.Fatal(err)
	}
	if legend.Min != -160.5 || legend.Max != 239.5 {
		t.Fatalf("range = [%v, %v], want PS3.3 LINEAR [-160.5, 239.5]", legend.Min, legend.Max)
	}
	var labels []string
	for _, tick := range legend.Ticks {
		labels = append(labels, tick.Label)
		if tick.Value < legend.Min || tick.Value > legend.Max {
			t.Fatalf("tick %v outside range", tick)
		}
		if math.Abs(tick.Position-(tick.Value-legend.Min)/(legend.Max-legend.Min)) > 1e-12 {
			t.Fatalf("tick %+v position mismatch", tick)
		}
	}
	if want := []string{"-100", "0", "100", "200"}; !reflect.DeepEqual(labels, want) {
		t.Fatalf("tick labels = %v, want %v", labels, want)
	}
	if got := legend.Header(); got != "CT [HU]" {
		t.Fatalf("header = %q", got)
	}
	raster := legend.Raster(4, 256)
	if raster == nil {
		t.Fatal("nil raster")
	}
	top, bottom := raster.NRGBAAt(0, 0), raster.NRGBAAt(3, 255)
	if top != hotIron(255) || bottom != hotIron(0) {
		t.Fatalf("raster ends top=%v bottom=%v, want white-hot top and black bottom", top, bottom)
	}
	if legend.Raster(0, 10) != nil || (Legend{}).Raster(4, 4) != nil {
		t.Fatal("degenerate raster should be nil")
	}
}

func TestForWindowCalibrationSUV(t *testing.T) {
	const factor = 0.0025 // SUVbw per Bq/ml for this synthetic study
	suv := func(value float64) (float64, bool) { return value * factor, true }
	window := render.WindowLevel{Center: 2000, Width: 4000, Function: display.VOILinearExact}
	legend, err := ForWindow(WindowSpec{Title: "PET", Unit: "SUVbw", Window: window, Palette: hotIron, Calibration: suv})
	if err != nil {
		t.Fatal(err)
	}
	if legend.Min != 0 || legend.Max != 10 {
		t.Fatalf("SUV range = [%v, %v], want [0, 10]", legend.Min, legend.Max)
	}
	for _, bqml := range []float64{0, 500, 1999, 3000, 4000, 6000} {
		got, _ := legend.ColorAt(bqml * factor)
		if want := hotIron(render.WindowedGray(bqml, window)); got != want {
			t.Fatalf("SUV %v colour %v, want %v", bqml*factor, got, want)
		}
	}
	nonAffine := func(value float64) (float64, bool) { return math.Sqrt(math.Max(0, value)), true }
	if _, err := ForWindow(WindowSpec{Window: window, Calibration: nonAffine}); !errors.Is(err, ErrNonAffineCalibration) {
		t.Fatalf("non-affine calibration err = %v", err)
	}
	unavailable := func(float64) (float64, bool) { return 0, false }
	if _, err := ForWindow(WindowSpec{Window: window, Calibration: unavailable}); !errors.Is(err, ErrNonAffineCalibration) {
		t.Fatalf("unavailable calibration err = %v", err)
	}
}

func TestWindowRange(t *testing.T) {
	lut := &display.LUT{FirstMapped: -100, BitsPerEntry: 8, Entries: make([]uint16, 201)}
	cases := []struct {
		name      string
		window    render.WindowLevel
		low, high float64
		ok        bool
	}{
		{"linear", render.WindowLevel{Center: 100, Width: 101}, 49.5, 149.5, true},
		{"linear width one", render.WindowLevel{Center: 10, Width: 1}, 9, 10, true},
		{"linear exact", render.WindowLevel{Center: 100, Width: 100, Function: display.VOILinearExact}, 50, 150, true},
		{"sigmoid", render.WindowLevel{Center: 0, Width: 10, Function: display.VOISigmoid}, -5, 5, true},
		{"voi lut", render.WindowLevel{LUT: lut}, -100, 100, true},
		{"zero width", render.WindowLevel{Center: 1}, 0, 0, false},
		{"nan", render.WindowLevel{Center: math.NaN(), Width: 10}, 0, 0, false},
		{"linear sub-unit width", render.WindowLevel{Center: 1, Width: 0.5}, 0, 0, false},
	}
	for _, tc := range cases {
		low, high, ok := WindowRange(tc.window)
		if ok != tc.ok || (ok && (low != tc.low || high != tc.high)) {
			t.Fatalf("%s: WindowRange = %v, %v, %v; want %v, %v, %v", tc.name, low, high, ok, tc.low, tc.high, tc.ok)
		}
	}
	if _, err := ForWindow(WindowSpec{Window: render.WindowLevel{}}); !errors.Is(err, ErrInvalidRange) {
		t.Fatalf("invalid window err = %v", err)
	}
}

func TestForBandsIsodoseLevels(t *testing.T) {
	blue := color.NRGBA{B: 255, A: 255}
	cyan := color.NRGBA{G: 220, B: 210, A: 255}
	yellow := color.NRGBA{R: 255, G: 210, A: 255}
	red := color.NRGBA{R: 255, A: 255}
	maximum := 52.0
	legend, err := ForBands("RT Dose", "Gy", []Band{
		{Low: 0.2 * maximum, High: 0.5 * maximum, Color: blue},
		{Low: 0.5 * maximum, High: 0.8 * maximum, Color: cyan},
		{Low: 0.8 * maximum, High: 0.95 * maximum, Color: yellow},
		{Low: 0.95 * maximum, High: maximum, Color: red},
	})
	if err != nil {
		t.Fatal(err)
	}
	var labels []string
	for _, tick := range legend.Ticks {
		labels = append(labels, tick.Label)
	}
	if want := []string{"10.4", "26.0", "41.6", "49.4", "52.0"}; !reflect.DeepEqual(labels, want) {
		t.Fatalf("labels = %v, want %v", labels, want)
	}
	checks := []struct {
		value float64
		want  color.NRGBA
		ok    bool
	}{
		{5, color.NRGBA{}, false},
		{10.4, blue, true},
		{30, cyan, true},
		{49.39, yellow, true},
		{49.4, red, true},
		{52, red, true},
		{52.1, color.NRGBA{}, false},
	}
	for _, check := range checks {
		got, ok := legend.ColorAt(check.value)
		if ok != check.ok || got != check.want {
			t.Fatalf("ColorAt(%v) = %v, %v; want %v, %v", check.value, got, ok, check.want, check.ok)
		}
	}
	if got := legend.Position(26); math.Abs(got-0.25) > 1e-12 {
		t.Fatalf("Position(26) = %v, want 0.25", got)
	}
	if got := legend.Position(50.7); math.Abs(got-0.875) > 1e-12 {
		t.Fatalf("Position(50.7) = %v, want 0.875", got)
	}
	raster := legend.Raster(2, 8)
	if raster.NRGBAAt(0, 0) != red || raster.NRGBAAt(0, 7) != blue || raster.NRGBAAt(1, 3) != yellow {
		t.Fatalf("discrete raster band order wrong")
	}
	if _, err := ForBands("x", "", []Band{{Low: 0, High: 1}, {Low: 2, High: 3}}); !errors.Is(err, ErrNoBands) {
		t.Fatalf("gap err = %v", err)
	}
	if _, err := ForBands("x", "", nil); !errors.Is(err, ErrNoBands) {
		t.Fatalf("empty err = %v", err)
	}
}

func TestNiceTicksAndFormatting(t *testing.T) {
	cases := []struct {
		low, high float64
		max       int
		want      []string
	}{
		{0, 10, 6, []string{"0", "2", "4", "6", "8", "10"}},
		{-1024, 3071, 6, []string{"-1000", "0", "1000", "2000", "3000"}},
		// Five ticks: 0.2 would need six, so the step coarsens to 0.5.
		{0, 1, 5, []string{"0.0", "0.5", "1.0"}},
		{0.0012, 0.0031, 6, []string{"0.0015", "0.0020", "0.0025", "0.0030"}},
		{-0.3, 0.3, 4, []string{"-0.2", "0.0", "0.2"}},
		{100.01, 100.02, 6, []string{"100.010", "100.012", "100.014", "100.016", "100.018", "100.020"}},
	}
	for _, tc := range cases {
		values, step := NiceTicks(tc.low, tc.high, tc.max)
		labels := make([]string, len(values))
		for i, value := range values {
			labels[i] = FormatValue(value, step)
		}
		if !reflect.DeepEqual(labels, tc.want) {
			t.Fatalf("NiceTicks(%v, %v, %d) = %v (step %v), want %v", tc.low, tc.high, tc.max, labels, step, tc.want)
		}
		if len(values) > max(tc.max, 2) {
			t.Fatalf("NiceTicks(%v, %v) returned %d ticks > %d", tc.low, tc.high, len(values), tc.max)
		}
	}
	if values, _ := NiceTicks(5, 5, 6); values != nil {
		t.Fatalf("empty range ticks = %v", values)
	}
	if got := FormatValue(-0.0001, 0.1); got != "0.0" {
		t.Fatalf("negative zero label = %q", got)
	}
}

func TestPETUnitsLabel(t *testing.T) {
	for code, want := range map[string]string{"BQML": "Bq/ml", " gml ": "g/ml", "CNTS": "counts", "NONE": "", "bogus": "", "UMOLMINML": "µmol/min/ml"} {
		if got := PETUnitsLabel(code); got != want {
			t.Fatalf("PETUnitsLabel(%q) = %q, want %q", code, got, want)
		}
	}
}

func TestSignatureTracksDrawnContent(t *testing.T) {
	a, _ := ForWindow(WindowSpec{Title: "t", Unit: "HU", Window: render.WindowLevel{Center: 40, Width: 400}, Palette: hotIron})
	b, _ := ForWindow(WindowSpec{Title: "t", Unit: "HU", Window: render.WindowLevel{Center: 40, Width: 400}, Palette: hotIron})
	c, _ := ForWindow(WindowSpec{Title: "t", Unit: "HU", Window: render.WindowLevel{Center: 40, Width: 400}})
	if a.Signature() != b.Signature() || a.Signature() == c.Signature() {
		t.Fatal("signature must be stable and change with the palette")
	}
	fn, err := ForFunction("f", "u", 0, 1, func(v float64) color.NRGBA { return color.NRGBA{R: uint8(255 * v), A: 255} }, 3)
	if err != nil || !fn.Valid() {
		t.Fatalf("ForFunction: %v", err)
	}
	if _, err := ForFunction("f", "u", 1, 1, nil, 3); !errors.Is(err, ErrInvalidRange) {
		t.Fatalf("ForFunction empty range err = %v", err)
	}
}
