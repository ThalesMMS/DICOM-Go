package calcium

import (
	"context"
	"errors"
	"math"
	"testing"
)

// The phantom is synthetic: a 64×64 stack of 0.5 mm pixels (0.25 mm² each)
// at -50 HU over 12 mm along z, with plaques of known area, attenuation and
// z extent. Scores of a plaque do not depend on the reconstruction: a plaque
// 6 mm long is two 3 mm slices, four 1.5 mm slices or twelve 0.5 mm slices,
// and the increment normalization makes every Agatston score equal.

const (
	phantomSize    = 64
	phantomSpacing = 0.5
	background     = -50
)

type plaque struct {
	name           string
	x, y, w, h     int     // pixel rectangle
	zStart, zEnd   float64 // mm, [zStart, zEnd)
	hu             float32 // every pixel
	peakHU         float32 // the first pixel, when non-zero
	peakX, peakY   int
	wantVessel     Vessel
	clickX, clickY int
}

var phantomPlaques = []plaque{
	// 4×4 pixels = 4 mm², 250 HU with one 350 HU pixel: weight 3,
	// mean 256.25 HU, 6 mm long.
	{name: "LAD", x: 10, y: 10, w: 4, h: 4, zStart: 0, zEnd: 6, hu: 250, peakHU: 350, peakX: 11, peakY: 11, wantVessel: LAD, clickX: 12, clickY: 12},
	// 6×4 pixels = 6 mm², 300 HU with one 450 HU pixel: weight 4,
	// mean 306.25 HU, 3 mm long.
	{name: "RCA", x: 30, y: 30, w: 6, h: 4, zStart: 6, zEnd: 9, hu: 300, peakHU: 450, peakX: 30, peakY: 30, wantVessel: RCA, clickX: 33, clickY: 31},
	// 2×2 pixels = exactly the 1 mm² minimum, 150 HU: weight 1, 3 mm long.
	{name: "LM", x: 50, y: 10, w: 2, h: 2, zStart: 9, zEnd: 12, hu: 150, wantVessel: LM, clickX: 50, clickY: 11},
	// Exactly at the threshold: 130 HU counts, 2×2 pixels, weight 1, other
	// coronary, 3 mm long.
	{name: "Other", x: 50, y: 50, w: 2, h: 2, zStart: 0, zEnd: 3, hu: 130, wantVessel: Other, clickX: 51, clickY: 51},
	// Bone: 10×10 pixels at 700 HU over the whole stack, left unassigned.
	{name: "bone", x: 20, y: 45, w: 10, h: 10, zStart: 0, zEnd: 12, hu: 700, wantVessel: Unassigned},
}

// phantom builds the stack for one increment; every slice also holds noise
// that is not a lesion: a single 1000 HU pixel (0.25 mm²), a 3-pixel line
// (0.75 mm²) and a 2×2 square at 129.9 HU (below the threshold).
func phantom(increment float64) (Geometry, [][]float32) {
	count := int(math.Round(12 / increment))
	g := Geometry{Columns: phantomSize, Rows: phantomSize, PixelSpacing: [2]float64{phantomSpacing, phantomSpacing}, SliceThicknessMM: 3}
	slices := make([][]float32, count)
	for z := range slices {
		position := float64(z) * increment
		g.SlicePositions = append(g.SlicePositions, position)
		values := make([]float32, phantomSize*phantomSize)
		for i := range values {
			values[i] = background
		}
		set := func(x, y int, hu float32) { values[y*phantomSize+x] = hu }
		set(2, 60, 1000)
		set(60, 2, 400)
		set(61, 2, 400)
		set(62, 2, 400)
		for _, p := range [][2]int{{40, 2}, {41, 2}, {40, 3}, {41, 3}} {
			set(p[0], p[1], 129.9)
		}
		for _, p := range phantomPlaques {
			if position < p.zStart || position >= p.zEnd {
				continue
			}
			for y := p.y; y < p.y+p.h; y++ {
				for x := p.x; x < p.x+p.w; x++ {
					set(x, y, p.hu)
				}
			}
			if p.peakHU != 0 {
				set(p.peakX, p.peakY, p.peakHU)
			}
		}
		slices[z] = values
	}
	return g, slices
}

func near(a, b float64) bool { return math.Abs(a-b) <= 1e-9*math.Max(1, math.Abs(b)) }

func TestSyntheticPhantomReproducesExpectedScores(t *testing.T) {
	const calibration = 0.8 // mg/cm³ per HU
	// Expected scores per vessel, independent of the reconstruction.
	want := map[Vessel]VesselScore{
		// 6 mm / 3 mm × 4 mm² × 3; 4 mm² × 6 mm; 0.8 × 0.024 cm³ × 256.25 HU.
		LAD: {Agatston: 24, VolumeMM3: 24, MassMG: 0.8 * 0.024 * 256.25},
		// 3 mm / 3 mm × 6 mm² × 4; 6 mm² × 3 mm; 0.8 × 0.018 cm³ × 306.25 HU.
		RCA: {Agatston: 24, VolumeMM3: 18, MassMG: 0.8 * 0.018 * 306.25},
		// 1 mm² × 1; 1 mm² × 3 mm; 0.8 × 0.003 cm³ × 150 HU.
		LM:    {Agatston: 1, VolumeMM3: 3, MassMG: 0.8 * 0.003 * 150},
		Other: {Agatston: 1, VolumeMM3: 3, MassMG: 0.8 * 0.003 * 130},
		LCX:   {},
	}
	wantTotal := VesselScore{Agatston: 50, VolumeMM3: 48, MassMG: want[LAD].MassMG + want[RCA].MassMG + want[LM].MassMG + want[Other].MassMG}
	for _, tc := range []struct {
		name          string
		increment     float64
		normalization float64
		// lesions per plaque: slices it covers
		ladSlices, rcaSlices int
	}{
		{name: "3 mm contiguous", increment: 3, normalization: 1, ladSlices: 2, rcaSlices: 1},
		{name: "1.5 mm increment", increment: 1.5, normalization: 0.5, ladSlices: 4, rcaSlices: 2},
		{name: "0.5 mm thin slices", increment: 0.5, normalization: 0.5 / 3, ladSlices: 12, rcaSlices: 6},
	} {
		t.Run(tc.name, func(t *testing.T) {
			g, slices := phantom(tc.increment)
			analysis, err := Detect(context.Background(), g, SliceValues(slices), Options{MassCalibration: calibration})
			if err != nil {
				t.Fatal(err)
			}
			if !near(analysis.IncrementMM, tc.increment) || !near(analysis.NormalizationFactor, tc.normalization) {
				t.Fatalf("increment %v normalization %v, want %v and %v", analysis.IncrementMM, analysis.NormalizationFactor, tc.increment, tc.normalization)
			}
			// Noise is not a lesion: each slice has exactly the plaques it crosses.
			wantLesions := 0
			for _, position := range g.SlicePositions {
				for _, p := range phantomPlaques {
					if position >= p.zStart && position < p.zEnd {
						wantLesions++
					}
				}
			}
			if len(analysis.Lesions) != wantLesions {
				t.Fatalf("lesions = %d, want %d", len(analysis.Lesions), wantLesions)
			}
			// Assign by clicking each plaque once on its first slice; the
			// 3D group carries the click to every slice of the plaque.
			for _, p := range phantomPlaques {
				if p.wantVessel == Unassigned {
					continue
				}
				z := int(math.Round(p.zStart / tc.increment))
				id, ok := analysis.LesionAt(z, p.clickX, p.clickY)
				if !ok {
					t.Fatalf("%s: no lesion at (%d, %d) of slice %d", p.name, p.clickX, p.clickY, z)
				}
				count, err := analysis.AssignGroup(id, p.wantVessel)
				if err != nil {
					t.Fatal(err)
				}
				wantCount := int(math.Round((p.zEnd - p.zStart) / tc.increment))
				if count != wantCount {
					t.Fatalf("%s: assigned %d lesions, want %d", p.name, count, wantCount)
				}
			}
			score := analysis.Score()
			for _, vessel := range Vessels() {
				got, expected := score.Vessel(vessel), want[vessel]
				if !near(got.Agatston, expected.Agatston) || !near(got.VolumeMM3, expected.VolumeMM3) || !near(got.MassMG, expected.MassMG) {
					t.Errorf("%s = %+v, want Agatston %v volume %v mass %v", vessel, got, expected.Agatston, expected.VolumeMM3, expected.MassMG)
				}
			}
			if got := score.Vessel(LAD).Lesions; got != tc.ladSlices {
				t.Errorf("LAD lesions = %d, want %d", got, tc.ladSlices)
			}
			if got := score.Vessel(RCA).Lesions; got != tc.rcaSlices {
				t.Errorf("RCA lesions = %d, want %d", got, tc.rcaSlices)
			}
			if !near(score.Total.Agatston, wantTotal.Agatston) || !near(score.Total.VolumeMM3, wantTotal.VolumeMM3) || !near(score.Total.MassMG, wantTotal.MassMG) {
				t.Errorf("total = %+v, want %+v", score.Total, wantTotal)
			}
			// The bone stays out of the total: 100 pixels = 25 mm², weight 4,
			// 12 mm long.
			if !near(score.Unassigned.Agatston, 12.0/3*25*4) || !near(score.Unassigned.VolumeMM3, 25*12) {
				t.Errorf("unassigned = %+v", score.Unassigned)
			}
			if !score.MassCalibrated || score.ThresholdHU != 130 || score.Slices != len(slices) {
				t.Errorf("score parameters = %+v", score)
			}
		})
	}
}

func TestLesionMeasuresPeakMeanAndWeight(t *testing.T) {
	g, slices := phantom(3)
	analysis, err := Detect(context.Background(), g, SliceValues(slices), Options{})
	if err != nil {
		t.Fatal(err)
	}
	id, ok := analysis.LesionAt(0, 10, 10)
	if !ok {
		t.Fatal("no LAD lesion")
	}
	lesion := analysis.Lesions[id]
	if lesion.AreaMM2 != 4 || lesion.PeakHU != 350 || lesion.MeanHU != 256.25 || lesion.Weight != 3 || lesion.Agatston != 12 || lesion.VolumeMM3 != 12 {
		t.Fatalf("LAD lesion = %+v", lesion)
	}
	if lesion.CentroidX != 11.5 || lesion.CentroidY != 11.5 || len(lesion.Pixels) != 16 {
		t.Fatalf("LAD centroid (%v, %v), %d pixels", lesion.CentroidX, lesion.CentroidY, len(lesion.Pixels))
	}
	if _, ok := analysis.LesionAt(0, 2, 60); ok {
		t.Fatal("a 0.25 mm² pixel became a lesion")
	}
	if _, ok := analysis.LesionAt(0, 40, 2); ok {
		t.Fatal("pixels below the threshold became a lesion")
	}
	if got := analysis.Score(); got.MassCalibrated || got.Total.MassMG != 0 || got.Total.Lesions != 0 {
		t.Fatalf("score without assignment or calibration = %+v", got)
	}
	if err := analysis.SetMassCalibration(1); err != nil {
		t.Fatal(err)
	}
	if err := analysis.Assign(id, LAD); err != nil {
		t.Fatal(err)
	}
	if got := analysis.Score().Vessel(LAD); got.Lesions != 1 || got.MassMG != 0.012*256.25 {
		t.Fatalf("one LAD slice = %+v", got)
	}
	if err := analysis.Assign(id, Unassigned); err != nil {
		t.Fatal(err)
	}
	if got := analysis.Score().Total; got.Lesions != 0 {
		t.Fatalf("total after unassigning = %+v", got)
	}
}

func TestDensityWeightBands(t *testing.T) {
	for _, tc := range []struct {
		peak float64
		want int
	}{{100, 1}, {130, 1}, {199.9, 1}, {200, 2}, {299.9, 2}, {300, 3}, {399.9, 3}, {400, 4}, {3000, 4}} {
		if got := DensityWeight(tc.peak); got != tc.want {
			t.Errorf("DensityWeight(%v) = %d, want %d", tc.peak, got, tc.want)
		}
	}
}

func TestConnectivityAndOptions(t *testing.T) {
	// A diagonal line of four 0.5 mm pixels: 1 mm² when 8-connected, four
	// 0.25 mm² specks when 4-connected.
	g := Geometry{Columns: 8, Rows: 8, PixelSpacing: [2]float64{0.5, 0.5}, SlicePositions: []float64{0}, SliceThicknessMM: 3}
	values := make([]float32, 64)
	for i := 0; i < 4; i++ {
		values[(i+1)*8+i+1] = 500
	}
	for _, tc := range []struct {
		name    string
		options Options
		want    int
	}{
		{"8-connected", Options{}, 1},
		{"4-connected", Options{FourConnected: true}, 0},
		{"higher threshold", Options{ThresholdHU: 600}, 0},
		{"smaller minimum area", Options{FourConnected: true, MinLesionAreaMM2: 0.25}, 4},
	} {
		t.Run(tc.name, func(t *testing.T) {
			analysis, err := Detect(context.Background(), g, SliceValues([][]float32{values}), tc.options)
			if err != nil {
				t.Fatal(err)
			}
			if len(analysis.Lesions) != tc.want {
				t.Fatalf("lesions = %d, want %d", len(analysis.Lesions), tc.want)
			}
		})
	}
	// A single slice is normalized by its thickness: 3 mm → 1.
	analysis, err := Detect(context.Background(), g, SliceValues([][]float32{values}), Options{})
	if err != nil {
		t.Fatal(err)
	}
	if analysis.IncrementMM != 3 || analysis.Lesions[0].Agatston != 4 || analysis.Lesions[0].VolumeMM3 != 3 {
		t.Fatalf("single slice = increment %v, lesion %+v", analysis.IncrementMM, analysis.Lesions[0])
	}
}

func TestIncrementRefusesUnevenStacks(t *testing.T) {
	for _, tc := range []struct {
		name      string
		positions []float64
		thickness float64
		want      float64
		err       error
	}{
		{name: "ascending", positions: []float64{0, 2.5, 5, 7.5}, want: 2.5},
		{name: "descending", positions: []float64{9, 6, 3, 0}, want: 3},
		{name: "rounded positions", positions: []float64{0, 2.999, 6.001, 9}, want: 3},
		{name: "single slice", positions: []float64{4}, thickness: 2.5, want: 2.5},
		{name: "single slice without thickness", positions: []float64{4}, err: ErrInvalidGeometry},
		{name: "missing slice", positions: []float64{0, 3, 6, 12}, err: ErrIrregularSpacing},
		{name: "duplicate position", positions: []float64{0, 3, 3, 6}, err: ErrInvalidGeometry},
		{name: "not monotonic", positions: []float64{0, 3, 1.5}, err: ErrInvalidGeometry},
		{name: "no slices", err: ErrInvalidGeometry},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, err := Increment(Geometry{SlicePositions: tc.positions, SliceThicknessMM: tc.thickness})
			if tc.err != nil {
				if !errors.Is(err, tc.err) {
					t.Fatalf("err = %v, want %v", err, tc.err)
				}
				return
			}
			if err != nil || !near(got, tc.want) {
				t.Fatalf("Increment = %v, %v; want %v", got, err, tc.want)
			}
		})
	}
}

func TestDetectRefusesBadInputAndHonoursCancellation(t *testing.T) {
	g, slices := phantom(3)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := Detect(ctx, g, SliceValues(slices), Options{}); !errors.Is(err, context.Canceled) {
		t.Fatalf("cancelled Detect err = %v", err)
	}
	bad := g
	bad.PixelSpacing = [2]float64{0, 0.5}
	if _, err := Detect(context.Background(), bad, SliceValues(slices), Options{}); !errors.Is(err, ErrInvalidGeometry) {
		t.Fatalf("no pixel spacing err = %v", err)
	}
	if _, err := Detect(context.Background(), g, SliceValues(slices[:2]), Options{}); err == nil {
		t.Fatal("a missing slice was read")
	}
	if _, err := Detect(context.Background(), g, SliceValues(slices), Options{MassCalibration: -1}); !errors.Is(err, ErrInvalidOptions) {
		t.Fatalf("negative calibration err = %v", err)
	}
	analysis, err := Detect(context.Background(), g, SliceValues(slices), Options{})
	if err != nil {
		t.Fatal(err)
	}
	if err := analysis.Assign(len(analysis.Lesions), LAD); !errors.Is(err, ErrUnknownLesion) {
		t.Fatalf("unknown lesion err = %v", err)
	}
	if _, err := analysis.AssignGroup(0, Total); !errors.Is(err, ErrUnknownVessel) {
		t.Fatalf("Total as a vessel err = %v", err)
	}
	if err := analysis.SetMassCalibration(math.NaN()); !errors.Is(err, ErrInvalidOptions) {
		t.Fatalf("NaN calibration err = %v", err)
	}
}
