package imagefilter

import (
	"context"
	"errors"
	"math/rand"
	"sort"
	"testing"
)

// bruteMedian is the definition: the element of rank n/2 of the sorted box
// of each sample, borders clamped.
func bruteMedian(src *Volume, r [3]int) *Volume {
	out := NewVolume(src.Dims, src.SpacingMM)
	nx, ny, nz := src.Dims[0], src.Dims[1], src.Dims[2]
	var box []float32
	for z := 0; z < nz; z++ {
		for y := 0; y < ny; y++ {
			for x := 0; x < nx; x++ {
				box = box[:0]
				for dz := -r[2]; dz <= r[2]; dz++ {
					for dy := -r[1]; dy <= r[1]; dy++ {
						for dx := -r[0]; dx <= r[0]; dx++ {
							box = append(box, src.At(clampAxis(x+dx, nx), clampAxis(y+dy, ny), clampAxis(z+dz, nz)))
						}
					}
				}
				sort.Slice(box, func(i, j int) bool { return box[i] < box[j] })
				out.Data[out.Index(x, y, z)] = box[len(box)/2]
			}
		}
	}
	return out
}

// randomVolume fills dims with integers in [lo, lo+span) or, when
// fractional, with arbitrary values, plus runs of repeated values so ties
// are common.
func randomVolume(dims [3]int, spacing [3]float64, lo, span int, fractional bool, seed int64) *Volume {
	v := NewVolume(dims, spacing)
	rng := rand.New(rand.NewSource(seed))
	for i := range v.Data {
		if i > 0 && rng.Intn(5) == 0 {
			v.Data[i] = v.Data[i-1]
			continue
		}
		value := float32(lo + rng.Intn(span))
		if fractional {
			value = float32(lo) + float32(span)*rng.Float32()
		}
		v.Data[i] = value
	}
	return v
}

func TestMedianEqualsBruteForceOnRandomInputs(t *testing.T) {
	cases := []struct {
		name       string
		dims       [3]int
		spacing    [3]float64
		radiusMM   [3]float64
		lo, span   int
		fractional bool
	}{
		{"2-D 3x3 CT", [3]int{37, 29, 1}, [3]float64{1, 1, 0}, [3]float64{1, 1, 0}, -1024, 4096, false},
		{"2-D anisotropic", [3]int{41, 23, 1}, [3]float64{0.5, 1, 0}, [3]float64{1, 1, 0}, 0, 4096, false},
		{"2-D wide span", [3]int{31, 31, 1}, [3]float64{1, 1, 0}, [3]float64{2, 2, 0}, -32768, 65536, false},
		{"2-D few values", [3]int{33, 17, 1}, [3]float64{1, 1, 0}, [3]float64{3, 1, 0}, 7, 3, false},
		{"3-D box", [3]int{19, 17, 13}, [3]float64{0.7, 0.7, 1.5}, [3]float64{0.7, 1.4, 1.5}, -1000, 2000, false},
		{"3-D slice-wise", [3]int{16, 15, 6}, [3]float64{1, 1, 1}, [3]float64{1, 1, 0}, 0, 255, false},
		{"radius beyond axis", [3]int{5, 4, 3}, [3]float64{1, 1, 1}, [3]float64{9, 9, 9}, 0, 100, false},
		{"fractional 2-D", [3]int{35, 27, 1}, [3]float64{1, 1, 0}, [3]float64{2, 1, 0}, -50, 100, true},
		{"fractional 3-D", [3]int{14, 13, 9}, [3]float64{1, 1, 2}, [3]float64{1, 1, 2}, -3, 7, true},
		{"span over 65536", [3]int{21, 19, 1}, [3]float64{1, 1, 0}, [3]float64{1, 1, 0}, -40000, 80000, false},
	}
	for i, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			src := randomVolume(c.dims, c.spacing, c.lo, c.span, c.fractional, int64(1185+i))
			opts := MedianOptions{RadiusMM: c.radiusMM}
			r, err := MedianRadiusSamples(opts, c.dims, c.spacing)
			if err != nil {
				t.Fatal(err)
			}
			want := bruteMedian(src, r)
			for _, workers := range []int{1, 3, 0} {
				opts.Workers = workers
				got, err := Median(context.Background(), src, opts)
				if err != nil {
					t.Fatal(err)
				}
				for j := range want.Data {
					if got.Data[j] != want.Data[j] {
						t.Fatalf("workers %d, radius %v: sample %d is %v, want %v", workers, r, j, got.Data[j], want.Data[j])
					}
				}
			}
		})
	}
}

func TestMedianRadiusRoundsToSamples(t *testing.T) {
	dims := [3]int{64, 64, 1}
	for _, c := range []struct {
		radiusMM [3]float64
		spacing  [3]float64
		want     [3]int
	}{
		{[3]float64{0.5, 0.5, 0.5}, [3]float64{1, 1, 0}, [3]int{1, 1, 0}},
		{[3]float64{0.49, 1, 0}, [3]float64{1, 0.3, 0}, [3]int{0, 3, 0}},
		{[3]float64{0.5, 0.5, 0}, [3]float64{0.15, 0.15, 0}, [3]int{3, 3, 0}},
	} {
		got, err := MedianRadiusSamples(MedianOptions{RadiusMM: c.radiusMM}, dims, c.spacing)
		if err != nil || got != c.want {
			t.Errorf("radius %v mm at %v mm: %v, %v; want %v", c.radiusMM, c.spacing, got, err, c.want)
		}
	}
}

func TestMedianRemovesImpulsesAndKeepsTheStep(t *testing.T) {
	// A step from 100 to 900 with 5% salt-and-pepper impulses (0 or 4095),
	// the dead-pixel and speckle noise of CR and XA.
	const size = 64
	src := NewVolume([3]int{size, size, 1}, [3]float64{0.2, 0.2, 0})
	clean := NewVolume(src.Dims, src.SpacingMM)
	rng := rand.New(rand.NewSource(1185))
	for y := 0; y < size; y++ {
		for x := 0; x < size; x++ {
			v := float32(100)
			if x >= size/2 {
				v = 900
			}
			clean.Data[clean.Index(x, y, 0)] = v
			switch p := rng.Float64(); {
			case p < 0.025:
				v = 0
			case p < 0.05:
				v = 4095
			}
			src.Data[src.Index(x, y, 0)] = v
		}
	}
	out, err := Median(context.Background(), src, MedianOptions{RadiusMM: [3]float64{0.2, 0.2, 0}})
	if err != nil {
		t.Fatal(err)
	}
	wrong := 0
	for i := range out.Data {
		if out.Data[i] != clean.Data[i] {
			wrong++
		}
	}
	// Impulses are isolated at 5%: a 3×3 box rarely holds five of them.
	if wrong > size*size/200 {
		t.Fatalf("%d of %d samples differ from the clean step", wrong, size*size)
	}
	// The step is not blurred: the columns either side keep their values.
	for y := 2; y < size-2; y++ {
		if out.At(size/2-1, y, 0) != 100 && src.At(size/2-1, y, 0) == 100 {
			t.Fatalf("row %d: step blurred to %v", y, out.At(size/2-1, y, 0))
		}
	}
}

func TestMedianCancellationAndInvalidOptions(t *testing.T) {
	src := randomVolume([3]int{64, 64, 8}, [3]float64{1, 1, 1}, 0, 1000, false, 1)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := Median(ctx, src, MedianOptions{RadiusMM: [3]float64{1, 1, 1}}); !errors.Is(err, context.Canceled) {
		t.Fatalf("cancelled: %v", err)
	}
	for _, opts := range []MedianOptions{
		{RadiusMM: [3]float64{-1, 1, 1}},
		{RadiusMM: [3]float64{1, 1, 1}, MaxKernelSamples: 26},
		{RadiusMM: [3]float64{1, 1, 1}, MaxKernelSamples: -1},
	} {
		if _, err := Median(context.Background(), src, opts); !errors.Is(err, ErrInvalidVolume) {
			t.Errorf("%+v: %v", opts, err)
		}
	}
	// A zero radius is the identity.
	same, err := Median(context.Background(), src, MedianOptions{})
	if err != nil {
		t.Fatal(err)
	}
	for i := range same.Data {
		if same.Data[i] != src.Data[i] {
			t.Fatal("a zero radius changed the volume")
		}
	}
}

func TestSelectRankMatchesSort(t *testing.T) {
	rng := rand.New(rand.NewSource(7))
	for trial := 0; trial < 500; trial++ {
		n := 1 + rng.Intn(60)
		v := make([]float32, n)
		for i := range v {
			v[i] = float32(rng.Intn(1 + rng.Intn(10)))
		}
		sorted := append([]float32(nil), v...)
		sort.Slice(sorted, func(i, j int) bool { return sorted[i] < sorted[j] })
		rank := rng.Intn(n)
		if got := selectRank(v, rank); got != sorted[rank] {
			t.Fatalf("rank %d of %v: %v, want %v", rank, sorted, got, sorted[rank])
		}
	}
}

// ctPhantom is a CT-like volume of whole HU: air, a soft-tissue disc and a
// bone core, with Gaussian noise of 20 HU.
func ctPhantom(dims [3]int, spacing [3]float64, seed int64) *Volume {
	v := NewVolume(dims, spacing)
	rng := rand.New(rand.NewSource(seed))
	for z := 0; z < dims[2]; z++ {
		for y := 0; y < dims[1]; y++ {
			for x := 0; x < dims[0]; x++ {
				dx, dy := float64(x-dims[0]/2), float64(y-dims[1]/2)
				hu := -1000.0
				if r2 := dx*dx + dy*dy; r2 < float64(dims[0]*dims[0])/40 {
					hu = 700
				} else if r2 < float64(dims[0]*dims[0])/5 {
					hu = 40
				}
				v.Data[v.Index(x, y, z)] = float32(int(hu + 20*rng.NormFloat64()))
			}
		}
	}
	return v
}

func benchmarkMedian(b *testing.B, src *Volume, radiusMM [3]float64) {
	b.SetBytes(int64(len(src.Data) * 4))
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if _, err := Median(context.Background(), src, MedianOptions{RadiusMM: radiusMM}); err != nil {
			b.Fatal(err)
		}
	}
}

func BenchmarkMedian2D512Radius1(b *testing.B) {
	benchmarkMedian(b, ctPhantom([3]int{512, 512, 1}, [3]float64{1, 1, 0}, 1), [3]float64{1, 1, 0})
}

func BenchmarkMedian2D512Radius5(b *testing.B) {
	benchmarkMedian(b, ctPhantom([3]int{512, 512, 1}, [3]float64{1, 1, 0}, 1), [3]float64{5, 5, 0})
}

// BenchmarkMedian2D512Radius1Fractional takes the quickselect path.
func BenchmarkMedian2D512Radius1Fractional(b *testing.B) {
	src := ctPhantom([3]int{512, 512, 1}, [3]float64{1, 1, 0}, 1)
	for i := range src.Data {
		src.Data[i] *= 0.37
	}
	benchmarkMedian(b, src, [3]float64{1, 1, 0})
}

// BenchmarkMedian2D512Radius1UniformNoise is the histogram path's worst
// case: uniform noise over 4096 values moves the median far at every step.
func BenchmarkMedian2D512Radius1UniformNoise(b *testing.B) {
	benchmarkMedian(b, randomVolume([3]int{512, 512, 1}, [3]float64{1, 1, 0}, -1024, 4096, false, 1), [3]float64{1, 1, 0})
}

func BenchmarkMedian3D256x256x64Radius1(b *testing.B) {
	benchmarkMedian(b, ctPhantom([3]int{256, 256, 64}, [3]float64{1, 1, 1}, 1), [3]float64{1, 1, 1})
}
