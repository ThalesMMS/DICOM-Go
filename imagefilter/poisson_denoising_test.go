package imagefilter

import (
	"context"
	"errors"
	"math"
	"math/rand"
	"os"
	"reflect"
	"sync"
	"testing"
)

func poissonTestVolume(d [3]int, values []float32) *Volume {
	return &Volume{Dims: d, SpacingMM: [3]float64{1, 1, 1}, Data: values}
}
func poissonTestOptions() PoissonDenoisingOptions {
	return PoissonDenoisingOptions{CountScale: 1, Lambda: .02, Iterations: 40, SourceReferences: []string{"synthetic:1503"}}
}
func poissonRequire(t *testing.T, v *Volume, o PoissonDenoisingOptions) *PoissonDenoisingResult {
	t.Helper()
	r, e := DenoisePoisson(context.Background(), v, o)
	if e != nil {
		t.Fatal(e)
	}
	return r
}

func TestPoissonCoordinateIndependentRoot(t *testing.T) {
	for _, c := range [][3]float64{{4, .5, 3}, {0, 1, 2}, {0, 1, .5}, {1e-40, 1, 0}, {1e38, 1e6, 1e44}} {
		y, a, s := c[0], c[1], c[2]
		got := poissonCoordinate(y, a, s)
		// Independent bisection of derivative, including constrained zero minimizer.
		lo, hi := 0.0, math.Max(1, math.Max(y, s/a)+1)
		for j := 0; j < 300; j++ {
			mid := (lo + hi) / 2
			derivative := 1 - s + a*mid
			if y > 0 {
				derivative -= y / mid
			}
			if derivative > 0 {
				hi = mid
			} else {
				lo = mid
			}
		}
		want := (lo + hi) / 2
		if math.Abs(got-want) > 1e-12*math.Max(want, 1e-50) {
			t.Fatalf("root %v got%g want%g", c, got, want)
		}
	}
}

func TestPoissonAnalyticPairObjective(t *testing.T) {
	v := poissonTestVolume([3]int{2, 1, 1}, []float32{0, 2})
	o := poissonTestOptions()
	o.Lambda = 1
	o.Iterations = 128
	r := poissonRequire(t, v, o)
	// Constrained optimum is exactly (0,1): positive coordinate derivative 1-2/1+1=0;
	// zero coordinate right derivative 1-1=0. Independent objective is 2*log(2)-0.5.
	if math.Abs(float64(r.Volume.Data[0])) > 1e-3 || math.Abs(float64(r.Volume.Data[1])-1) > 1e-3 {
		t.Fatal(r.Volume.Data)
	}
	want := 2*math.Log(2) - .5 // independent likelihood plus one edge penalty
	if math.Abs(r.Metadata.FinalObjective-want) > 1e-5 || r.Metadata.FinalObjective > r.Metadata.InitialObjective {
		t.Fatal(r.Metadata, want)
	}
}

func TestPoissonIdentityConstantAndFloor(t *testing.T) {
	for _, data := range [][]float32{{0, 0, 0}, {7, 7, 7}, {math.MaxFloat32, math.SmallestNonzeroFloat32, 1, 0, math.Float32frombits(0x80000000)}} {
		for _, scale := range []float64{1e-12, .7, 1, 1e12} {
			o := poissonTestOptions()
			o.CountScale = scale
			o.Lambda = 0
			v := poissonTestVolume([3]int{len(data), 1, 1}, append([]float32(nil), data...))
			r := poissonRequire(t, v, o)
			for i, input := range data {
				if math.Float32bits(input) != math.Float32bits(r.Volume.Data[i]) {
					t.Fatalf("identity gain=%g index=%d bits %08x -> %08x", scale, i, math.Float32bits(input), math.Float32bits(r.Volume.Data[i]))
				}
			}
			if r.Metadata.IterationsCompleted != 0 || r.Metadata.FinalPoissonDeviance != 0 || r.Metadata.InitialObjective != 0 || r.Metadata.FinalObjective != 0 || r.Metadata.FinalPenalty != 0 {
				t.Fatal(r)
			}
		}
	}
	for _, level := range []float32{0, .001, 1, 1000} {
		data := make([]float32, 27)
		for i := range data {
			data[i] = level
		}
		r := poissonRequire(t, poissonTestVolume([3]int{3, 3, 3}, data), poissonTestOptions())
		for _, v := range r.Volume.Data {
			if math.Abs(float64(v-level)) > 1e-6*math.Max(1, float64(level)) {
				t.Fatal(level, v)
			}
		}
	}
}

func poissonSample(r *rand.Rand, mean float64) float32 {
	// Knuth product generator, mathematical Poisson law; fixed-seed synthetic counts.
	threshold := math.Exp(-mean)
	p := 1.0
	k := 0
	for p > threshold {
		k++
		p *= r.Float64()
	}
	if mean == 0 {
		return 0
	}
	return float32(k - 1)
}
func poissonMoments(data []float32, truth float64) (mean, variance, mse float64) {
	for _, v := range data {
		mean += float64(v)
		delta := float64(v) - truth
		mse += delta * delta
	}
	mean /= float64(len(data))
	mse /= float64(len(data))
	for _, v := range data {
		d := float64(v) - mean
		variance += d * d
	}
	variance /= float64(len(data))
	return
}

func TestPoissonFixedSeedQuantitativePhantoms(t *testing.T) {
	for _, dims := range [][3]int{{64, 64, 1}, {16, 16, 16}} {
		for _, mean := range []float64{0, .25, 4, 40} {
			t.Run(stringNamePoisson(mean), func(t *testing.T) {
				rng := rand.New(rand.NewSource(1503))
				data := make([]float32, 4096)
				for i := range data {
					data[i] = poissonSample(rng, mean)
				}
				before := append([]float32(nil), data...)
				o := poissonTestOptions()
				r := poissonRequire(t, poissonTestVolume(dims, data), o)
				im, iv, ie := poissonMoments(data, mean)
				om, ov, oe := poissonMoments(r.Volume.Data, mean)
				if mean == 0 {
					if om != 0 || ov != 0 || r.Metadata.FinalPoissonDeviance != 0 {
						t.Fatal(r)
					}
					return
				}
				// Qualification bounds intentionally include estimator bias at the low-count floor.
				if math.Abs(om-mean) > .12*mean+.04 || ov >= iv || oe >= ie {
					t.Fatalf("mean%g observed%g var%g MSE%g estimate%g var%g MSE%g", mean, im, iv, ie, om, ov, oe)
				}
				if r.Metadata.FinalObjective > r.Metadata.InitialObjective*(1+1e-12) || r.Metadata.FinalPoissonDeviance <= 0 {
					t.Fatal(r.Metadata)
				}
				if !reflect.DeepEqual(before, data) {
					t.Fatal("source mutation")
				}
				// Independent residual from published float32 values, not the solver's reported loop.
				var dev float64
				for i, v := range r.Volume.Data {
					y := float64(data[i])
					x := float64(v)
					if y == 0 {
						dev += 2 * x
					} else {
						if x <= 0 {
							t.Fatal("positive count lost")
						}
						dev += 2 * (x - y + y*math.Log(y/x))
					}
				}
				if math.Abs(dev-r.Metadata.FinalPoissonDeviance) > 1e-4*math.Max(1, dev) {
					t.Fatal(dev, r.Metadata.FinalPoissonDeviance)
				}
				t.Logf("truth=%g observed(mean=%g variance=%g mse=%g) restored(mean=%g variance=%g mse=%g) deviance=%g", mean, im, iv, ie, om, ov, oe, dev)
			})
		}
	}
}

func stringNamePoisson(v float64) string {
	switch v {
	case 0:
		return "zero"
	case .25:
		return "floor"
	case 4:
		return "four"
	default:
		return "forty"
	}
}

func TestPoissonBordersAndAnisotropy(t *testing.T) {
	// One sweep from (0,4): clipped border has exactly one neighbor, not duplicated/mirrored.
	o := poissonTestOptions()
	o.Lambda = .5
	o.Iterations = 1
	v := poissonTestVolume([3]int{2, 1, 1}, []float32{0, 4})
	r := poissonRequire(t, v, o)
	if r.Volume.Data[0] != 2 {
		t.Fatal(r.Volume.Data)
	} // (.5*4-1)/.5
	v.SpacingMM[0] = 2
	r = poissonRequire(t, v, o)
	if r.Volume.Data[0] != 0 {
		t.Fatal("physical inverse-square weighting", r.Volume.Data)
	}
	// Swapped x/y spacing and geometry produce equal axial independent update for source0.
	v = poissonTestVolume([3]int{1, 2, 1}, []float32{0, 4})
	v.SpacingMM = [3]float64{0, 1, 0}
	r = poissonRequire(t, v, o)
	if r.Volume.Data[0] != 2 {
		t.Fatal(r.Volume.Data)
	}
}

func TestPoissonOutliersAndExtremes(t *testing.T) {
	for _, data := range [][]float32{{0, 1, 1000000, 1, 0}, {math.SmallestNonzeroFloat32, 1, math.MaxFloat32}} {
		r := poissonRequire(t, poissonTestVolume([3]int{len(data), 1, 1}, data), poissonTestOptions())
		var hi float32
		for _, v := range data {
			if v > hi {
				hi = v
			}
		}
		for _, v := range r.Volume.Data {
			if v < 0 || v > hi || math.IsNaN(float64(v)) || math.IsInf(float64(v), 0) {
				t.Fatal(r.Volume.Data)
			}
		}
		if r.Metadata.FinalObjective > r.Metadata.InitialObjective*(1+1e-12) {
			t.Fatal(r.Metadata)
		}
	}
}

func TestPoissonDomainRejections(t *testing.T) {
	v := poissonTestVolume([3]int{3, 1, 1}, []float32{0, 1, 2})
	for _, mutate := range []func(*Volume, *PoissonDenoisingOptions){
		func(v *Volume, o *PoissonDenoisingOptions) { v.Data[0] = -1 },
		func(v *Volume, o *PoissonDenoisingOptions) { v.Data[0] = float32(math.NaN()) },
		func(v *Volume, o *PoissonDenoisingOptions) { v.Data[0] = float32(math.Inf(1)) },
		func(v *Volume, o *PoissonDenoisingOptions) { o.CountScale = 0 },
		func(v *Volume, o *PoissonDenoisingOptions) { o.CountScale = math.Inf(1) },
		func(v *Volume, o *PoissonDenoisingOptions) { o.Lambda = -1 },
		func(v *Volume, o *PoissonDenoisingOptions) { o.Lambda = math.NaN() },
		func(v *Volume, o *PoissonDenoisingOptions) { o.RelativeChange = math.NaN() },
		func(v *Volume, o *PoissonDenoisingOptions) { v.SpacingMM[0] = 1e-10 },
		func(v *Volume, o *PoissonDenoisingOptions) { o.SourceReferences = nil },
		func(v *Volume, o *PoissonDenoisingOptions) { o.SourceReferences = []string{""} },
	} {
		copy := v.Clone()
		o := poissonTestOptions()
		mutate(copy, &o)
		before := append([]float32(nil), copy.Data...)
		r, e := DenoisePoisson(context.Background(), copy, o)
		if r != nil || !errors.Is(e, ErrPoissonDenoisingInput) {
			t.Fatal(r, e)
		}
		for i := range before {
			if math.Float32bits(before[i]) != math.Float32bits(copy.Data[i]) {
				t.Fatal("mutation")
			}
		}
	}
}

func TestPoissonBudgetAdmissions(t *testing.T) {
	v := poissonTestVolume([3]int{3, 1, 1}, []float32{0, 1, 2})
	for _, lim := range []PoissonDenoisingLimits{{MaxVoxels: 2}, {MaxIterations: 1}, {MaxScratchBytes: 23}, {MaxWorkingBytes: 35}, {MaxNeighborVisits: 1}, {MaxReferenceBytes: 1}} {
		o := poissonTestOptions()
		o.Limits = lim
		r, e := DenoisePoisson(context.Background(), v, o)
		if r != nil || !errors.Is(e, ErrPoissonDenoisingLimit) {
			t.Fatal(lim, r, e)
		}
	}
	o := poissonTestOptions()
	o.Limits.MaxSourceReferences = 1
	o.SourceReferences = []string{"a", "b"}
	if r, e := DenoisePoisson(context.Background(), v, o); r != nil || !errors.Is(e, ErrPoissonDenoisingLimit) {
		t.Fatal(r, e)
	}
	o = poissonTestOptions()
	o.Limits.MaxVoxels = -1
	if r, e := DenoisePoisson(context.Background(), v, o); r != nil || !errors.Is(e, ErrPoissonDenoisingInput) {
		t.Fatal(r, e)
	}
	r := poissonRequire(t, v, poissonTestOptions())
	if r.Metadata.ScratchBytes != 24 || r.Metadata.WorkingBytes != 36+16+int64(len("synthetic:1503")) || r.Metadata.NeighborVisitBound != 3*(6*40+24) {
		t.Fatal(r.Metadata)
	}
}

func TestPoissonDerivedOwnership(t *testing.T) {
	v := poissonTestVolume([3]int{3, 1, 1}, []float32{0, 1, 4})
	o := poissonTestOptions()
	r := poissonRequire(t, v, o)
	if !r.Metadata.Derived || r.Metadata.Method != "Poisson-quadratic-MAP-coordinate" || r.Metadata.NoiseModel != "independent-Poisson" || len(r.Metadata.SourceSHA256) != 64 || r.Metadata.CountScale != 1 || r.Metadata.Lambda != .02 {
		t.Fatal(r.Metadata)
	}
	hash := r.Metadata.SourceSHA256
	v.Data[1] = 100
	o.SourceReferences[0] = "mutated"
	r.Volume.Data[0] = 80
	if r.Metadata.SourceReferences[0] != "synthetic:1503" || r.Metadata.SourceSHA256 != hash || v.Data[0] != 0 {
		t.Fatal("alias")
	}
}

type poissonCancelChecks struct {
	context.Context
	calls, at int
}

func (c *poissonCancelChecks) Err() error {
	c.calls++
	if c.calls >= c.at {
		return context.Canceled
	}
	return nil
}
func TestPoissonCancelNoPartial(t *testing.T) {
	data := make([]float32, 1024)
	for i := range data {
		data[i] = float32(i % 5)
	}
	v := poissonTestVolume([3]int{32, 32, 1}, data)
	before := append([]float32(nil), data...)
	for _, at := range []int{1, 5, 10, 30, 160} {
		ctx := &poissonCancelChecks{Context: context.Background(), at: at}
		r, e := DenoisePoisson(ctx, v, poissonTestOptions())
		if r != nil || !errors.Is(e, context.Canceled) {
			t.Fatal(at, r, e)
		}
	}
	if !reflect.DeepEqual(before, data) {
		t.Fatal("cancel mutated source")
	}
}

func TestPoissonConcurrentCalls(t *testing.T) {
	v := poissonTestVolume([3]int{4, 4, 1}, []float32{0, 1, 4, 0, 3, 4, 2, 0, 5, 6, 7, 8, 0, 1, 2, 1})
	o := poissonTestOptions()
	want := poissonRequire(t, v, o).Volume.Data
	var wg sync.WaitGroup
	for i := 0; i < 4; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			r, e := DenoisePoisson(context.Background(), v, o)
			if e != nil {
				t.Error(e)
				return
			}
			if !reflect.DeepEqual(want, r.Volume.Data) {
				t.Error("nondeterministic")
			}
		}()
	}
	wg.Wait()
}

func TestPoissonRelativeStopping(t *testing.T) {
	v := poissonTestVolume([3]int{3, 1, 1}, []float32{4, 4, 4})
	o := poissonTestOptions()
	o.RelativeChange = 1e-8
	r := poissonRequire(t, v, o)
	if r.Metadata.StoppingReason != "relative-change" || r.Metadata.IterationsCompleted != 1 {
		t.Fatal(r.Metadata)
	}
}

func TestPoissonDenoisingRSSChild(t *testing.T) {
	cell := os.Getenv("DICOMGO_POISSON_RSS_CELL")
	if cell == "" {
		t.Skip("opt-in isolated /usr/bin/time -l RSS process only")
	}
	dims, ok := poissonCostDims(cell)
	if !ok {
		t.Fatal("unknown RSS cell")
	}
	n, _ := voxelCount(dims)
	data := make([]float32, n)
	for i := range data {
		data[i] = float32(1 + i%13)
	}
	o := poissonTestOptions()
	o.Lambda = .1
	o.Iterations = 10
	v := poissonTestVolume(dims, data)
	r := poissonRequire(t, v, o)
	if len(r.Volume.Data) != n || r.Metadata.IterationsCompleted != 10 {
		t.Fatal(r)
	}
	t.Logf("cell=%s voxels=%d scratch_payload=%d working_payload=%d", cell, n, r.Metadata.ScratchBytes, r.Metadata.WorkingBytes)
}

func TestPoissonCountScaleEquivalence(t *testing.T) {
	v := poissonTestVolume([3]int{4, 1, 1}, []float32{0, 1, 4, 8})
	o := poissonTestOptions()
	a := poissonRequire(t, v, o)
	o.CountScale = 10
	o.Lambda /= 10
	b := poissonRequire(t, v, o)
	for i, x := range a.Volume.Data {
		if math.Abs(float64(x-b.Volume.Data[i])) > 1e-6*math.Max(1, float64(x)) {
			t.Fatal(a.Volume.Data, b.Volume.Data)
		}
	}
	o.CountScale = .5
	o.Lambda = .04
	c := poissonRequire(t, v, o)
	if c.Metadata.FractionalCountSamples != 1 {
		t.Fatal(c.Metadata)
	}
}
