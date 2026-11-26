package imagefilter

import (
	"context"
	"errors"
	"math"
	"math/rand"
	"testing"
	"time"

	"github.com/ThalesMMS/dicom-go/internal/parallel"
)

// n4Phantom is a head-like MR phantom: white matter (300) inside grey
// matter (200) with CSF pockets (80), zero outside, multiplied by field
// and with Gaussian noise of sigma noise.
func n4Phantom(dims [3]int, field func(x, y, z float64) float64, noise float64) (in *Volume, truth []float64, class []int) {
	in = NewVolume(dims, [3]float64{1.2, 1.2, 1.5})
	truth = make([]float64, len(in.Data))
	class = make([]int, len(in.Data))
	random := rand.New(rand.NewSource(1104))
	for z := 0; z < dims[2]; z++ {
		for y := 0; y < dims[1]; y++ {
			for x := 0; x < dims[0]; x++ {
				i := in.Index(x, y, z)
				u, v, w := 2*float64(x)/float64(dims[0]-1)-1, 2*float64(y)/float64(dims[1]-1)-1, 0.0
				if dims[2] > 1 {
					w = 2*float64(z)/float64(dims[2]-1) - 1
				}
				r := math.Sqrt(u*u/0.85 + v*v + w*w/0.8)
				value := 0.0
				switch {
				case r < 0.55:
					value, class[i] = 300, 1
				case r < 0.9:
					value, class[i] = 200, 2
				}
				if class[i] != 0 && (math.Hypot(u-0.3, v+0.2) < 0.12 || math.Hypot(u+0.35, v-0.25) < 0.1) {
					value, class[i] = 80, 3
				}
				truth[i] = field(float64(x)/float64(dims[0]-1), float64(y)/float64(dims[1]-1), (w+1)/2)
				if value > 0 {
					in.Data[i] = float32(math.Max(1, value*truth[i]+noise*random.NormFloat64()))
				}
			}
		}
	}
	return in, truth, class
}

// n4SurfaceCoil is a smooth field with a surface-coil-like fall-off: about
// 1.5 near one side, 0.7 on the other.
func n4SurfaceCoil(x, y, z float64) float64 {
	return math.Exp(0.45*(0.5-x) + 0.25*(y-0.5)*(y-0.5) - 0.15*z + 0.1*math.Sin(2*y))
}

func classCV(data []float32, class []int, want int) float64 {
	var sum, sum2, n float64
	for i, c := range class {
		if c == want {
			v := float64(data[i])
			sum += v
			sum2 += v * v
			n++
		}
	}
	mean := sum / n
	return math.Sqrt(sum2/n-mean*mean) / mean
}

// TestN4RecoversASyntheticBiasField: the estimated field matches the true
// one up to a constant factor, and correction makes each tissue class
// nearly uniform again.
func TestN4RecoversASyntheticBiasField(t *testing.T) {
	in, truth, class := n4Phantom([3]int{64, 64, 40}, n4SurfaceCoil, 4)
	var progressed int
	started := time.Now()
	result, err := N4BiasFieldCorrection(context.Background(), in, N4Options{ShrinkFactor: 2, Progress: func(N4Progress) { progressed++ }})
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("64 × 64 × 40 in %v: levels %+v, shrunken %v, %d samples", time.Since(started).Round(time.Millisecond), result.Levels, result.ShrunkenDims, result.Samples)
	if len(result.Levels) != 4 || progressed == 0 || result.ShrunkenDims != [3]int{32, 32, 20} {
		t.Fatalf("levels %+v, %d progress calls, shrunken %v", result.Levels, progressed, result.ShrunkenDims)
	}
	// The ratio of the true to the estimated field is constant in the head.
	var ratios []float32
	for i, c := range class {
		if c != 0 {
			ratios = append(ratios, float32(truth[i]/math.Exp(float64(result.LogBiasField.Data[i]))))
		}
	}
	ones := make([]int, len(ratios))
	for i := range ones {
		ones[i] = 1
	}
	fieldError := classCV(ratios, ones, 1)
	for c, name := range map[int]string{1: "white matter", 2: "grey matter"} {
		before, after := classCV(in.Data, class, c), classCV(result.Corrected.Data, class, c)
		t.Logf("%s: coefficient of variation %.3f before, %.3f after", name, before, after)
		if after > before/3 || after > 0.04 {
			t.Errorf("%s: coefficient of variation %.3f before, %.3f after", name, before, after)
		}
	}
	t.Logf("true/estimated field ratio varies by %.2f%%", 100*fieldError)
	if fieldError > 0.03 {
		t.Fatalf("true/estimated field ratio varies by %.2f%%", 100*fieldError)
	}
}

// TestN4MaskAndTwoDimensions: a mask restricts the estimate, a 2-D image
// is corrected, and a volume without a uniform class keeps its field flat.
func TestN4MaskAndTwoDimensions(t *testing.T) {
	in, _, class := n4Phantom([3]int{96, 96, 1}, func(x, y, _ float64) float64 { return math.Exp(0.4 * (x - y)) }, 2)
	mask := make([]bool, len(in.Data))
	for i, c := range class {
		mask[i] = c == 1 || c == 2
	}
	result, err := N4BiasFieldCorrection(context.Background(), in, N4Options{Mask: mask, ShrinkFactor: 2, Iterations: []int{50, 50, 50}})
	if err != nil {
		t.Fatal(err)
	}
	if before, after := classCV(in.Data, class, 1), classCV(result.Corrected.Data, class, 1); after > before/3 {
		t.Fatalf("2-D white matter: %.3f before, %.3f after", before, after)
	}
	// Outside the head the input stays zero.
	if result.Corrected.Data[0] != 0 {
		t.Fatalf("background corrected to %v", result.Corrected.Data[0])
	}
}

func TestN4RejectsAndCancels(t *testing.T) {
	in, _, _ := n4Phantom([3]int{16, 16, 8}, n4SurfaceCoil, 0)
	for name, opts := range map[string]N4Options{
		"mask size":      {Mask: make([]bool, 3)},
		"control points": {ControlPoints: 3},
		"iterations":     {Iterations: []int{10, 0}},
		"shrink":         {ShrinkFactor: -1},
		"workers":        {Workers: -1},
		"empty mask":     {Mask: make([]bool, len(in.Data))},
	} {
		if _, err := N4BiasFieldCorrection(context.Background(), in, opts); !errors.Is(err, ErrInvalidVolume) {
			t.Errorf("%s: %v", name, err)
		}
	}
	ctx, cancel := context.WithCancel(context.Background())
	_, err := N4BiasFieldCorrection(ctx, in, N4Options{Progress: func(N4Progress) { cancel() }})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("cancelled: %v", err)
	}
}

// TestN4HonorsWorkers: N4 runs on at most Workers goroutines from the
// shared concurrency budget (#1188), and its result does not depend on how
// many it gets.
func TestN4HonorsWorkers(t *testing.T) {
	previous := parallel.SetBudget(8)
	defer parallel.SetBudget(previous)
	in, _, _ := n4Phantom([3]int{48, 48, 24}, n4SurfaceCoil, 2)
	run := func(workers int) (*N4Result, int) {
		parallel.ResetPeak()
		result, err := N4BiasFieldCorrection(context.Background(), in, N4Options{ShrinkFactor: 1, Iterations: []int{5, 5}, Workers: workers})
		if err != nil {
			t.Fatal(err)
		}
		return result, parallel.Peak()
	}
	serial, serialHelpers := run(1)
	if serialHelpers != 0 {
		t.Fatalf("Workers 1 ran %d helper goroutines", serialHelpers)
	}
	for _, workers := range []int{2, 3, 0} {
		got, helpers := run(workers)
		if limit := parallel.Workers(workers) - 1; helpers > limit {
			t.Fatalf("Workers %d ran %d helper goroutines", workers, helpers)
		}
		for i := range got.Corrected.Data {
			if got.Corrected.Data[i] != serial.Corrected.Data[i] || got.LogBiasField.Data[i] != serial.LogBiasField.Data[i] {
				t.Fatalf("Workers %d: voxel %d differs from the serial result", workers, i)
			}
		}
	}
	parallel.SetBudget(0)
	if _, helpers := run(0); helpers != 0 {
		t.Fatalf("an empty budget ran %d helper goroutines", helpers)
	}
}

// TestBSplineRefinementKeepsTheSpline: refining the lattice, as between N4
// levels, leaves the evaluated spline unchanged.
func TestBSplineRefinementKeepsTheSpline(t *testing.T) {
	random := rand.New(rand.NewSource(7))
	lattice := &bsplineLattice{n: [3]int{5, 4, 6}, phi: make([]float64, 5*4*6)}
	for i := range lattice.phi {
		lattice.phi[i] = random.NormFloat64()
	}
	grid := &n4Grid{dims: [3]int{17, 9, 13}}
	axes := grid.axes()
	before := lattice.evaluateGrid(axes, 0)
	refined := lattice.refine([3]int{100, 100, 100})
	if refined.n != [3]int{7, 5, 9} {
		t.Fatalf("refined lattice %v", refined.n)
	}
	after := refined.evaluateGrid(axes, 0)
	for i := range before {
		if math.Abs(before[i]-after[i]) > 1e-12 {
			t.Fatalf("voxel %d: %v before refinement, %v after", i, before[i], after[i])
		}
	}
	// An axis is not refined beyond half its samples.
	if capped := lattice.refine([3]int{100, 8, 100}); capped.n != [3]int{7, 4, 9} {
		t.Fatalf("capped refinement %v", capped.n)
	}
}

// TestSharpenN4KeepsADegenerateHistogram: a constant image is left as is.
func TestSharpenN4KeepsADegenerateHistogram(t *testing.T) {
	values := []float64{2, 2, 2}
	out := make([]float64, 3)
	opts, _ := N4Options{}.normalized(&Volume{Data: make([]float32, 3)})
	sharpenN4(values, out, opts)
	for i := range out {
		if out[i] != 2 {
			t.Fatalf("sharpened %v", out)
		}
	}
}

// BenchmarkN4TypicalMR corrects a 256 × 256 × 160 volume, a typical 3-D T1
// acquisition, with the default options (shrink 4, four levels).
func BenchmarkN4TypicalMR(b *testing.B) {
	in, _, _ := n4Phantom([3]int{256, 256, 160}, n4SurfaceCoil, 4)
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if _, err := N4BiasFieldCorrection(context.Background(), in, N4Options{}); err != nil {
			b.Fatal(err)
		}
	}
}
