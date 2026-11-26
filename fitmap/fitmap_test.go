package fitmap

import (
	"context"
	"errors"
	"math"
	"math/rand"
	"testing"

	"github.com/ThalesMMS/dicom-go/core"
	"github.com/ThalesMMS/dicom-go/internal/derivedio"
	"github.com/ThalesMMS/dicom-go/mrdiffusion"
)

// decayPhantom is a 32 × 32 phantom of four quadrants with known rates:
// signal S0·exp(−x·rate) + offset + Gaussian noise of sigma at each
// parameter x. Background (S0 = 0) surrounds the quadrants.
func decayPhantom(parameters []float64, rate func(quadrant int) float64, s0, offset, sigma float64, seed int64) (images [][]float32, truth []float64) {
	const n = 32
	random := rand.New(rand.NewSource(seed))
	truth = make([]float64, n*n)
	images = make([][]float32, len(parameters))
	for k := range images {
		images[k] = make([]float32, n*n)
	}
	for y := 0; y < n; y++ {
		for x := 0; x < n; x++ {
			i := y*n + x
			truth[i] = math.NaN()
			inside := x >= 2 && x < n-2 && y >= 2 && y < n-2
			quadrant := (y/(n/2))*2 + x/(n/2)
			for k, p := range parameters {
				s := offset + sigma*random.NormFloat64()
				if inside {
					truth[i] = rate(quadrant)
					s += s0 * math.Exp(-p*truth[i])
				}
				images[k][i] = float32(s)
			}
		}
	}
	return images, truth
}

var t2Quadrants = []float64{40, 80, 150, 300} // ms

func t2Rate(q int) float64 { return 1 / t2Quadrants[q] }

func TestFitRecoversKnownT2(t *testing.T) {
	echoes := []float64{10, 20, 30, 40, 50, 60, 70, 80}
	images, truth := decayPhantom(echoes, t2Rate, 1000, 0, 0, 1)
	result, err := Fit(context.Background(), T2, echoes, images, Options{Threshold: 1})
	if err != nil {
		t.Fatal(err)
	}
	if result.Fitted != 28*28 || result.Skipped != 32*32-28*28 || len(result.Parameters) != 8 {
		t.Fatalf("fitted %d, skipped %d, parameters %v", result.Fitted, result.Skipped, result.Parameters)
	}
	for i, want := range truth {
		got := result.Values[i]
		if math.IsNaN(want) {
			if !math.IsNaN(got) {
				t.Fatalf("background voxel %d fitted to %v", i, got)
			}
			continue
		}
		if math.Abs(got-1/want) > 1e-6*(1/want) || math.Abs(result.S0[i]-1000) > 1e-5*1000 || math.Abs(result.RSquared[i]-1) > 1e-9 {
			t.Fatalf("voxel %d: T2 %v (want %v), S0 %v, R² %v", i, got, 1/want, result.S0[i], result.RSquared[i])
		}
	}
}

// TestFitWithNoiseAndBackground: a noise floor added to every signal biases
// the plain fit, and subtracting it as the background removes the bias; on
// noisy echoes the weighted fit stays within 3% of the true T2.
func TestFitWithNoiseAndBackground(t *testing.T) {
	echoes := []float64{8, 16, 24, 32, 48, 64, 96}
	images, truth := decayPhantom(echoes, t2Rate, 1000, 60, 0, 2)
	biased, err := Fit(context.Background(), T2, echoes, images, Options{Threshold: 100})
	if err != nil {
		t.Fatal(err)
	}
	corrected, err := Fit(context.Background(), T2, echoes, images, Options{Threshold: 100, Background: 60})
	if err != nil {
		t.Fatal(err)
	}
	quadrantMean := func(values []float64, q int) float64 {
		var sum, n float64
		for i, v := range values {
			x, y := i%32, i/32
			if !math.IsNaN(truth[i]) && (y/16)*2+x/16 == q {
				sum += v
				n++
			}
		}
		return sum / n
	}
	for q, want := range t2Quadrants {
		if got := quadrantMean(corrected.Values, q); math.Abs(got-want) > 1e-6*want {
			t.Fatalf("quadrant %d: corrected T2 %v, want %v", q, got, want)
		}
		if got := quadrantMean(biased.Values, q); !(got > want*1.05) {
			t.Fatalf("quadrant %d: the noise floor did not bias the T2 up (%v for %v)", q, got, want)
		}
	}

	noisy, _ := decayPhantom(echoes, t2Rate, 1000, 0, 8, 3)
	weighted, err := Fit(context.Background(), T2, echoes, noisy, Options{Threshold: 100, Weighted: true})
	if err != nil {
		t.Fatal(err)
	}
	for q, want := range t2Quadrants {
		if got := quadrantMean(weighted.Values, q); math.Abs(got-want) > 0.03*want {
			t.Fatalf("quadrant %d: weighted T2 %v on noisy echoes, want %v ± 3%%", q, got, want)
		}
	}
}

// TestFitADCMatchesMRDiffusion: the ADC is the decay rate over b-values in
// µm²/s, and equals mrdiffusion.FitADC's unweighted fit.
func TestFitADCMatchesMRDiffusion(t *testing.T) {
	adc := []float64{0.7e-3, 1.1e-3, 1.8e-3, 3.0e-3} // mm²/s
	bValues := []float64{0, 500, 1000, 1000, 1500}
	images, truth := decayPhantom(bValues, func(q int) float64 { return adc[q] }, 800, 0, 0.5, 4)
	result, err := Fit(context.Background(), ADC, bValues, images, Options{Threshold: 50})
	if err != nil {
		t.Fatal(err)
	}
	reference, err := mrdiffusion.FitADC(bValues, images, mrdiffusion.ADCOptions{MinSignal: 0})
	if err != nil {
		t.Fatal(err)
	}
	if len(result.Parameters) != 4 {
		t.Fatalf("distinct b-values %v", result.Parameters)
	}
	for i, want := range truth {
		if math.IsNaN(want) {
			continue
		}
		if got := result.Values[i]; math.Abs(got-reference.Values[i]*1e6) > 1e-6*got || math.Abs(got-want*1e6) > 0.1*want*1e6 {
			t.Fatalf("voxel %d: ADC %v µm²/s, mrdiffusion %v, truth %v", i, got, reference.Values[i]*1e6, want*1e6)
		}
	}
	if ADC.Units().Value != "um2/s" || ADC.Quantity().Value != "113041" || T2.Quantity().Value != "113065" || T2Star.Quantity().Value != "113064" ||
		T2.Units().Value != "ms" || T2Star.String() != "T2*" {
		t.Fatal("quantity or unit codes")
	}
}

func TestFitClassifiesVoxels(t *testing.T) {
	echoes := []float64{10, 30, 10}
	images := [][]float32{
		{0, 500, 500, 500, 500, 500},
		{0, 300, 600, 500, 100, 499.99},
		{0, 500, 500, 500, -5, 500},
	}
	mask := []bool{true, true, true, true, true, false}
	result, err := Fit(context.Background(), T2Star, echoes, images, Options{Mask: mask, MaxValue: 1000, Background: 101})
	if err != nil {
		t.Fatal(err)
	}
	// 0: below the threshold; 1: fitted; 2: rising signal; 3: flat signal
	// (no decay); 4: one echo above the background; 5: masked.
	if result.Skipped != 2 || result.Fitted != 1 || result.NonDecaying != 2 || result.Insufficient != 1 || result.OutOfRange != 0 {
		t.Fatalf("classification %+v", result)
	}
	slow := [][]float32{{1000}, {999}}
	if r, _ := Fit(context.Background(), T2, []float64{10, 20}, slow, Options{MaxValue: 1000}); r.OutOfRange != 1 || !math.IsNaN(r.Values[0]) {
		t.Fatalf("slow decay: %+v", r)
	}
}

func TestFitIsIndependentOfWorkers(t *testing.T) {
	echoes := []float64{10, 20, 40, 80}
	images, _ := decayPhantom(echoes, t2Rate, 900, 0, 10, 5)
	// Tile the phantom to span several chunks.
	for k := range images {
		for len(images[k]) < 5*fitChunk {
			images[k] = append(images[k], images[k]...)
		}
	}
	want, err := Fit(context.Background(), T2, echoes, images, Options{Threshold: 100, Workers: 1})
	if err != nil {
		t.Fatal(err)
	}
	for _, workers := range []int{0, 3} {
		got, err := Fit(context.Background(), T2, echoes, images, Options{Threshold: 100, Workers: workers})
		if err != nil {
			t.Fatal(err)
		}
		if got.Fitted != want.Fitted {
			t.Fatalf("workers %d: %d fitted, want %d", workers, got.Fitted, want.Fitted)
		}
		for i := range want.Values {
			if math.Float64bits(got.Values[i]) != math.Float64bits(want.Values[i]) {
				t.Fatalf("workers %d: voxel %d differs", workers, i)
			}
		}
	}
}

func TestFitRejectsAndCancels(t *testing.T) {
	two := [][]float32{{1}, {2}}
	for name, c := range map[string]struct {
		kind       Kind
		parameters []float64
		images     [][]float32
		opts       Options
	}{
		"kind":          {Kind(9), []float64{1, 2}, two, Options{}},
		"count":         {T2, []float64{1}, two, Options{}},
		"one image":     {T2, []float64{1}, two[:1], Options{}},
		"sizes":         {T2, []float64{1, 2}, [][]float32{{1}, {1, 2}}, Options{}},
		"negative":      {T2, []float64{-1, 2}, two, Options{}},
		"not distinct":  {T2, []float64{5, 5}, two, Options{}},
		"mask":          {T2, []float64{1, 2}, two, Options{Mask: []bool{true, false}}},
		"nan threshold": {T2, []float64{1, 2}, two, Options{Threshold: math.NaN()}},
	} {
		if _, err := Fit(context.Background(), c.kind, c.parameters, c.images, c.opts); !errors.Is(err, ErrFit) {
			t.Errorf("%s: %v", name, err)
		}
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := Fit(ctx, T2, []float64{1, 2}, two, Options{}); !errors.Is(err, context.Canceled) {
		t.Errorf("canceled: %v", err)
	}
}

func TestGroupSortsEchoesByPosition(t *testing.T) {
	var frames []Frame
	index := 0
	for _, position := range []float64{12, 0, 6, 18} {
		for _, te := range []float64{30, 10.0004, 20} {
			if position == 18 && te == 20 {
				continue // the last slice misses an echo
			}
			frames = append(frames, Frame{Parameter: te, Position: position + 0.001*float64(index%2), Index: index})
			index++
		}
	}
	frames = append(frames, Frame{Parameter: 10, Position: 6, Index: 99}) // a repeat
	g, err := Group(frames, 0.01, 0.01)
	if err != nil {
		t.Fatal(err)
	}
	if len(g.Parameters) != 3 || g.Parameters[0] != 10 || len(g.Positions) != 3 || g.Positions[0] > 0.01 || g.Positions[2] < 11.99 || g.Dropped != 3 {
		t.Fatalf("grouping %+v", g)
	}
	for p, row := range g.Images {
		for s, i := range row {
			var frame Frame
			for _, f := range frames {
				if f.Index == i {
					frame = f
				}
			}
			if math.Abs(frame.Parameter-g.Parameters[p]) > 0.01 || math.Abs(frame.Position-g.Positions[s]) > 0.01 {
				t.Fatalf("image [%d][%d] is frame %+v", p, s, frame)
			}
		}
	}
	if _, err := Group([]Frame{{Parameter: 10}, {Parameter: 10.001, Position: 1}}, 0.01, 0.01); !errors.Is(err, ErrFit) {
		t.Fatalf("one echo: %v", err)
	}
	if _, err := Group([]Frame{{Parameter: 10}, {Parameter: 20, Position: 5}}, 0.01, 0.01); !errors.Is(err, ErrFit) {
		t.Fatalf("no common position: %v", err)
	}
}

func TestEchoTimeReadsClassicAndEnhancedObjects(t *testing.T) {
	classic := derivedio.Object(derivedio.DS(tagEchoTime, 22.5))
	if te, ok := EchoTime(classic, 0); !ok || te != 22.5 {
		t.Fatalf("classic echo time %v %v", te, ok)
	}
	perFrame := func(te float64) core.DataSet {
		return derivedio.DataSet(derivedio.Seq(tagMREchoSequence, derivedio.DataSet(derivedio.FD(tagEffectiveEchoTime, te))))
	}
	enhanced := derivedio.Object(
		derivedio.Seq(core.NewTag(0x5200, 0x9230), perFrame(10), perFrame(20)),
		derivedio.DS(tagEchoTime, 99),
	)
	for frame, want := range []float64{10, 20} {
		if te, ok := EchoTime(enhanced, frame); !ok || te != want {
			t.Fatalf("frame %d echo time %v %v", frame, te, ok)
		}
	}
	if _, ok := EchoTime(derivedio.Object(), 0); ok {
		t.Fatal("echo time without the attribute")
	}
}

// BenchmarkFitT2 fits a 256 × 256 × 20 eight-echo series.
func BenchmarkFitT2(b *testing.B) {
	echoes := []float64{10, 20, 30, 40, 50, 60, 70, 80}
	tile, _ := decayPhantom(echoes, t2Rate, 1000, 0, 10, 6)
	images := make([][]float32, len(echoes))
	for k := range images {
		for len(images[k]) < 256*256*20 {
			images[k] = append(images[k], tile[k]...)
		}
	}
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if _, err := Fit(context.Background(), T2, echoes, images, Options{Threshold: 100}); err != nil {
			b.Fatal(err)
		}
	}
}
