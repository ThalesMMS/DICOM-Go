package imagefilter

import (
	"context"
	"errors"
	"math"
	"math/rand/v2"
	"slices"
	"testing"
)

// mrPhantom builds a synthetic MR-like volume: a noisy dark background
// around nested ellipsoids of "fat", "white matter", "grey matter" and
// "CSF" intensities. labels holds the class of every voxel (0 background).
func mrPhantom(seed uint64, dims [3]int, classes [5]float64, noise float64) (*Volume, []uint8) {
	rng := rand.New(rand.NewPCG(seed, seed^0x9e3779b97f4a7c15))
	v := NewVolume(dims, [3]float64{1, 1, 3})
	labels := make([]uint8, len(v.Data))
	cx, cy, cz := float64(dims[0]-1)/2, float64(dims[1]-1)/2, float64(dims[2]-1)/2
	for z := 0; z < dims[2]; z++ {
		for y := 0; y < dims[1]; y++ {
			for x := 0; x < dims[0]; x++ {
				dx, dy := (float64(x)-cx)/cx, (float64(y)-cy)/cy
				dz := 0.0
				if cz > 0 {
					dz = 0.4 * (float64(z) - cz) / cz
				}
				r := math.Sqrt(dx*dx + dy*dy + dz*dz)
				var label uint8
				switch {
				case r < 0.25:
					label = 4 // CSF
				case r < 0.5:
					label = 2 // white matter
				case r < 0.7:
					label = 3 // grey matter
				case r < 0.8:
					label = 1 // fat
				}
				i := v.Index(x, y, z)
				labels[i] = label
				value := classes[label] + noise*rng.NormFloat64()
				if label == 0 {
					// Magnitude noise: a Rayleigh-like dark background.
					value = math.Abs(classes[0] + noise*rng.NormFloat64())
				}
				v.Data[i] = float32(value)
			}
		}
	}
	return v, labels
}

func classMeans(v *Volume, labels []uint8) [5]float64 {
	var sum, n [5]float64
	for i, l := range labels {
		sum[l] += float64(v.Data[i])
		n[l]++
	}
	var out [5]float64
	for i := range out {
		out[i] = sum[i] / n[i]
	}
	return out
}

func foregroundQuantiles(v *Volume, labels []uint8, ps []float64) []float64 {
	var fg []float64
	for i, l := range labels {
		if l != 0 {
			fg = append(fg, float64(v.Data[i]))
		}
	}
	slices.Sort(fg)
	out := make([]float64, len(ps))
	for i, p := range ps {
		out[i] = fg[int(p*float64(len(fg)-1))]
	}
	return out
}

// TestHistogramMatchingAlignsSyntheticMRHistograms is the acceptance test of
// the follow-up use case: a "prior" acquired with a different, non-linear
// intensity response and independent noise is matched to the "current"
// examination, after which the tissue classes and the foreground quantiles
// line up, so one window fits both.
func TestHistogramMatchingAlignsSyntheticMRHistograms(t *testing.T) {
	dims := [3]int{96, 96, 6}
	current, labels := mrPhantom(1, dims, [5]float64{15, 1100, 800, 600, 300}, 20)
	prior, priorLabels := mrPhantom(2, dims, [5]float64{15, 1100, 800, 600, 300}, 20)
	if !slices.Equal(labels, priorLabels) {
		t.Fatal("phantom labels differ between seeds")
	}
	// Different scanner gain and a non-linear response.
	for i, x := range prior.Data {
		prior.Data[i] = float32(150 + 1.6*math.Pow(float64(x), 0.95))
	}
	priorBefore := prior.Clone()

	out, match, err := MatchHistograms(context.Background(), prior, current, HistogramMatchOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(prior.Data, priorBefore.Data) {
		t.Fatal("MatchHistograms modified the source volume")
	}
	if out.Dims != prior.Dims || out.SpacingMM != prior.SpacingMM {
		t.Fatalf("output grid %v %v, want %v %v", out.Dims, out.SpacingMM, prior.Dims, prior.SpacingMM)
	}
	if match.MatchPoints() != DefaultHistogramMatchPoints {
		t.Fatalf("match points = %d, want %d", match.MatchPoints(), DefaultHistogramMatchPoints)
	}

	span := 1100.0 - 15.0
	want := classMeans(current, labels)
	before := classMeans(prior, labels)
	got := classMeans(out, labels)
	t.Logf("class means: current %.1f, prior %.1f, matched prior %.1f", want[1:], before[1:], got[1:])
	for class := 1; class < 5; class++ {
		if math.Abs(before[class]-want[class]) < 0.1*span {
			t.Fatalf("class %d: the unmatched prior (%v) is already close to %v; the test does not exercise matching", class, before[class], want[class])
		}
		if d := math.Abs(got[class] - want[class]); d > 0.02*span {
			t.Errorf("class %d mean: matched %.1f, current %.1f (|diff| %.1f > 2%% of the range)", class, got[class], want[class], d)
		}
	}
	ps := []float64{0.05, 0.1, 0.25, 0.5, 0.75, 0.9, 0.95}
	wantQ := foregroundQuantiles(current, labels, ps)
	gotQ := foregroundQuantiles(out, labels, ps)
	for i, p := range ps {
		if d := math.Abs(gotQ[i] - wantQ[i]); d > 0.03*span {
			t.Errorf("foreground quantile %.2f: matched %.1f, current %.1f (|diff| %.1f > 3%% of the range)", p, gotQ[i], wantQ[i], d)
		}
	}
	// The landmarks are ordered and end at the thresholds and maxima.
	landmarks := match.Landmarks()
	for i := 1; i < len(landmarks); i++ {
		if landmarks[i].Source < landmarks[i-1].Source || landmarks[i].Reference < landmarks[i-1].Reference || landmarks[i].Fraction <= landmarks[i-1].Fraction {
			t.Fatalf("landmarks not increasing at %d: %+v", i, landmarks)
		}
	}
	src, ref := match.SourceHistogram(), match.ReferenceHistogram()
	if landmarks[0].Source != src.Threshold || landmarks[len(landmarks)-1].Source != src.Max ||
		landmarks[0].Reference != ref.Threshold || landmarks[len(landmarks)-1].Reference != ref.Max {
		t.Fatalf("landmark ends %+v, want thresholds %v/%v and maxima %v/%v", landmarks, src.Threshold, ref.Threshold, src.Max, ref.Max)
	}
}

func TestHistogramMatchingRecoversAffineTransform(t *testing.T) {
	rng := rand.New(rand.NewPCG(7, 11))
	reference := make([]float32, 20000)
	for i := range reference {
		reference[i] = float32(500 + 150*rng.NormFloat64())
	}
	source := make([]float32, len(reference))
	for i, x := range reference {
		source[i] = 3*x + 100
	}
	for _, background := range []bool{false, true} {
		opts := HistogramMatchOptions{HistogramOptions: HistogramOptions{Levels: 4096, IncludeBackground: background}, MatchPoints: 31}
		match, err := NewHistogramMatchFromSamples(source, reference, opts)
		if err != nil {
			t.Fatal(err)
		}
		bin := (match.ReferenceHistogram().Max - match.ReferenceHistogram().Threshold) / 4096
		for i, x := range source {
			if d := math.Abs(match.Map(float64(x)) - float64(reference[i])); d > 4*bin+1e-3 {
				t.Fatalf("background=%v: Map(%v) = %v, want %v (|diff| %v, bin %v)", background, x, match.Map(float64(x)), reference[i], d, bin)
			}
		}
	}
}

func TestHistogramMatchingSelfIsIdentity(t *testing.T) {
	v, _ := mrPhantom(3, [3]int{64, 64, 2}, [5]float64{10, 900, 700, 500, 250}, 15)
	match, err := NewHistogramMatchFromSamples(v.Data, v.Data, HistogramMatchOptions{})
	if err != nil {
		t.Fatal(err)
	}
	for _, x := range []float64{0, 3, 100, 250, 500, 777, 900, 1200} {
		if got := match.Map(x); math.Abs(got-math.Min(x, match.ReferenceHistogram().Max)) > 1e-9*math.Max(1, x) {
			t.Errorf("self match Map(%v) = %v", x, got)
		}
	}
}

func TestHistogramMatchMapSegments(t *testing.T) {
	src := &IntensityHistogram{Min: 0, Max: 100, Threshold: 20, Counts: []uint64{1, 1, 1, 1}}
	ref := &IntensityHistogram{Min: 10, Max: 410, Threshold: 50, Counts: []uint64{1, 1, 1, 1}}
	match, err := NewHistogramMatch(src, ref, 3)
	if err != nil {
		t.Fatal(err)
	}
	// Uniform histograms: the landmarks are the quartiles of each range.
	wantLandmarks := []HistogramLandmark{{0, 20, 50}, {0.25, 40, 140}, {0.5, 60, 230}, {0.75, 80, 320}, {1, 100, 410}}
	for i, l := range match.Landmarks() {
		w := wantLandmarks[i]
		if math.Abs(l.Fraction-w.Fraction) > 1e-12 || math.Abs(l.Source-w.Source) > 1e-9 || math.Abs(l.Reference-w.Reference) > 1e-9 {
			t.Fatalf("landmark %d = %+v, want %+v", i, l, w)
		}
	}
	for _, tc := range []struct{ in, want float64 }{
		{-10, -10}, // below the minimum: the lower segment extends linearly
		{0, 10},    // source minimum -> reference minimum
		{10, 30},   // background, between minimum and threshold
		{20, 50},   // threshold -> threshold
		{50, 185},  // between landmarks
		{100, 410}, // maximum -> maximum
		{250, 410}, // above the maximum: flat, as in ITK
		{math.NaN(), math.NaN()},
		{math.Inf(1), math.Inf(1)},
	} {
		got := match.Map(tc.in)
		if !(got == tc.want || (math.IsNaN(got) && math.IsNaN(tc.want))) && math.Abs(got-tc.want) > 1e-9 {
			t.Errorf("Map(%v) = %v, want %v", tc.in, got, tc.want)
		}
	}
	// Monotonic everywhere.
	prev := math.Inf(-1)
	for x := -20.0; x <= 150; x += 0.25 {
		y := match.Map(x)
		if y < prev {
			t.Fatalf("Map not monotonic at %v: %v < %v", x, y, prev)
		}
		prev = y
	}
}

func TestIntensityHistogramQuantile(t *testing.T) {
	samples := make([]float32, 1000)
	for i := range samples {
		samples[i] = float32(i) // uniform 0..999
	}
	h, err := NewIntensityHistogram(samples, HistogramOptions{Levels: 100, IncludeBackground: true})
	if err != nil {
		t.Fatal(err)
	}
	if h.Samples != 1000 || h.Counted != 1000 || h.Threshold != 0 || h.Max != 999 {
		t.Fatalf("histogram %+v", h)
	}
	for _, p := range []float64{0, 0.1, 0.25, 0.49, 0.5, 0.75, 0.9, 1} {
		if got := h.Quantile(p); math.Abs(got-999*p) > 10 {
			t.Errorf("Quantile(%v) = %v, want about %v", p, got, 999*p)
		}
	}
	// The default threshold at the mean drops the lower half.
	h, err = NewIntensityHistogram(samples, HistogramOptions{Levels: 100})
	if err != nil {
		t.Fatal(err)
	}
	if h.Threshold != 499.5 || h.Counted != 500 {
		t.Fatalf("threshold %v counted %d, want 499.5 and 500", h.Threshold, h.Counted)
	}
}

func TestHistogramMatchingRejectsDegenerateInput(t *testing.T) {
	good := []float32{0, 1, 2, 3, 4}
	for _, tc := range []struct {
		name              string
		source, reference []float32
		opts              HistogramMatchOptions
	}{
		{"constant source", []float32{7, 7, 7}, good, HistogramMatchOptions{}},
		{"constant reference", good, []float32{2, 2}, HistogramMatchOptions{}},
		{"empty source", nil, good, HistogramMatchOptions{}},
		{"only NaN", []float32{float32(math.NaN()), float32(math.Inf(1))}, good, HistogramMatchOptions{}},
		{"negative levels", good, good, HistogramMatchOptions{HistogramOptions: HistogramOptions{Levels: -1}}},
		{"negative match points", good, good, HistogramMatchOptions{MatchPoints: -2}},
		{"too many match points", good, good, HistogramMatchOptions{MatchPoints: MaxHistogramMatchPoints + 1}},
	} {
		if _, err := NewHistogramMatchFromSamples(tc.source, tc.reference, tc.opts); !errors.Is(err, ErrInvalidHistogramMatch) {
			t.Errorf("%s: err = %v, want ErrInvalidHistogramMatch", tc.name, err)
		}
	}
	bad := &IntensityHistogram{Min: 0, Max: 10, Threshold: 20, Counts: []uint64{1}}
	ok, _ := NewIntensityHistogram(good, HistogramOptions{})
	if _, err := NewHistogramMatch(ok, bad, 0); !errors.Is(err, ErrInvalidHistogramMatch) {
		t.Errorf("threshold above max: err = %v", err)
	}
	if _, err := NewHistogramMatch(ok, &IntensityHistogram{Min: 0, Max: 10, Threshold: 0, Counts: []uint64{0, 0}}, 0); !errors.Is(err, ErrInvalidHistogramMatch) {
		t.Errorf("empty histogram: err = %v", err)
	}
}

func TestHistogramMatchingIgnoresNonFiniteSamples(t *testing.T) {
	nan := float32(math.NaN())
	source := []float32{0, 10, nan, 20, 30, float32(math.Inf(-1)), 40}
	reference := []float32{100, 200, 300, 400, 500}
	match, err := NewHistogramMatchFromSamples(source, reference, HistogramMatchOptions{HistogramOptions: HistogramOptions{IncludeBackground: true}})
	if err != nil {
		t.Fatal(err)
	}
	if h := match.SourceHistogram(); h.Samples != 5 || h.Min != 0 || h.Max != 40 {
		t.Fatalf("source histogram %+v", h)
	}
	out := make([]float32, len(source))
	if err := match.Apply(context.Background(), out, source, 1); err != nil {
		t.Fatal(err)
	}
	if !math.IsNaN(float64(out[2])) || !math.IsInf(float64(out[5]), -1) {
		t.Fatalf("non-finite samples not passed through: %v", out)
	}
	if out[0] != 100 || out[6] != 500 {
		t.Fatalf("extremes map to %v and %v, want 100 and 500", out[0], out[6])
	}
}

func TestHistogramMatchApplyWorkersInPlaceAndCancel(t *testing.T) {
	v, _ := mrPhantom(5, [3]int{128, 128, 12}, [5]float64{15, 1100, 800, 600, 300}, 20)
	ref, _ := mrPhantom(6, [3]int{64, 64, 4}, [5]float64{30, 2000, 1500, 1000, 500}, 30)
	match, err := NewHistogramMatchFromSamples(v.Data, ref.Data, HistogramMatchOptions{})
	if err != nil {
		t.Fatal(err)
	}
	one := make([]float32, len(v.Data))
	if err := match.Apply(context.Background(), one, v.Data, 1); err != nil {
		t.Fatal(err)
	}
	many := v.Clone()
	if err := match.Apply(context.Background(), many.Data, many.Data, 8); err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(one, many.Data) {
		t.Fatal("in-place multi-worker result differs from the single-worker result")
	}
	if err := match.Apply(context.Background(), one[:10], v.Data, 1); !errors.Is(err, ErrInvalidHistogramMatch) {
		t.Fatalf("length mismatch: err = %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := match.Apply(ctx, one, v.Data, 4); !errors.Is(err, context.Canceled) {
		t.Fatalf("cancelled Apply: err = %v", err)
	}
	if _, _, err := MatchHistograms(ctx, v, ref, HistogramMatchOptions{}); !errors.Is(err, context.Canceled) {
		t.Fatalf("cancelled MatchHistograms: err = %v", err)
	}
	if s := match.String(); s == "" {
		t.Fatal("empty summary")
	}
}

func BenchmarkHistogramMatchApply(b *testing.B) {
	v, _ := mrPhantom(8, [3]int{256, 256, 32}, [5]float64{15, 1100, 800, 600, 300}, 20)
	ref, _ := mrPhantom(9, [3]int{256, 256, 32}, [5]float64{30, 2000, 1500, 1000, 500}, 30)
	match, err := NewHistogramMatchFromSamples(v.Data, ref.Data, HistogramMatchOptions{})
	if err != nil {
		b.Fatal(err)
	}
	out := make([]float32, len(v.Data))
	b.SetBytes(int64(4 * len(v.Data)))
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if err := match.Apply(context.Background(), out, v.Data, 0); err != nil {
			b.Fatal(err)
		}
	}
}
