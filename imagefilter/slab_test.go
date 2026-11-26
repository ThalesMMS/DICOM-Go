package imagefilter

import (
	"context"
	"errors"
	"fmt"
	"runtime"
	"testing"
)

// countingReader serves a volume's slices and counts how often each is read.
type countingReader struct {
	v     *Volume
	reads []int
	fail  int // slice whose read fails, -1 for none
}

func (r *countingReader) ReadSlices(ctx context.Context, z0, z1 int, dst []float32) error {
	for z := z0; z < z1; z++ {
		if z == r.fail {
			return fmt.Errorf("slice %d unreadable", z)
		}
		r.reads[z]++
	}
	return r.v.ReadSlices(ctx, z0, z1, dst)
}

func newCountingReader(v *Volume) *countingReader {
	return &countingReader{v: v, reads: make([]int, v.Dims[2]), fail: -1}
}

func equalVolumes(t *testing.T, label string, got, want *Volume) {
	t.Helper()
	for i := range want.Data {
		if got.Data[i] != want.Data[i] {
			t.Fatalf("%s: sample %d is %v, want %v", label, i, got.Data[i], want.Data[i])
		}
	}
}

func TestFilterSlabsIsIdenticalToTheWholeVolume(t *testing.T) {
	dims, spacing := [3]int{23, 19, 17}, [3]float64{0.8, 0.9, 1.3}
	integer := ctPhantom(dims, spacing, 1189)
	fractional := randomVolume(dims, spacing, -40, 90, true, 1189)
	filters := []struct {
		name   string
		src    *Volume
		filter SlabFilter
		whole  func(*Volume) (*Volume, error)
	}{
		{"median histogram", integer, MedianOptions{RadiusMM: [3]float64{0.8, 0.9, 2.6}}, func(v *Volume) (*Volume, error) {
			return Median(context.Background(), v, MedianOptions{RadiusMM: [3]float64{0.8, 0.9, 2.6}})
		}},
		{"median quickselect", fractional, MedianOptions{RadiusMM: [3]float64{1.6, 0, 1.3}, Workers: 3}, func(v *Volume) (*Volume, error) {
			return Median(context.Background(), v, MedianOptions{RadiusMM: [3]float64{1.6, 0, 1.3}})
		}},
		{"bilateral", integer, BilateralOptions{SigmaMM: [3]float64{0.8, 0.8, 1.2}, RangeSigma: 40}, func(v *Volume) (*Volume, error) {
			return Bilateral(context.Background(), v, BilateralOptions{SigmaMM: [3]float64{0.8, 0.8, 1.2}, RangeSigma: 40, Workers: 2})
		}},
		{"unsharp in plane", fractional, UnsharpMaskOptions{SigmaMM: [3]float64{1, 1.5, 0}, Amount: 0.7, Threshold: 2}, func(v *Volume) (*Volume, error) {
			return UnsharpMask(context.Background(), v, UnsharpMaskOptions{SigmaMM: [3]float64{1, 1.5, 0}, Amount: 0.7, Threshold: 2})
		}},
	}
	sliceBytes := int64(dims[0] * dims[1] * 4)
	for _, f := range filters {
		want, err := f.whole(f.src)
		if err != nil {
			t.Fatal(err)
		}
		for _, budget := range []int64{1, 9 * sliceBytes, 20 * sliceBytes, 0} {
			t.Run(fmt.Sprintf("%s/%d", f.name, budget), func(t *testing.T) {
				reader := newCountingReader(f.src)
				got := NewVolume(dims, spacing)
				res, err := FilterSlabs(context.Background(), f.filter, reader, dims, spacing, got, SlabOptions{MaxWorkingBytes: budget})
				if err != nil {
					t.Fatal(err)
				}
				equalVolumes(t, f.name, got, want)
				if res.Slabs != (dims[2]+res.SlabSlices-1)/res.SlabSlices {
					t.Fatalf("%+v", res)
				}
				if budget > 0 && res.SlabSlices > 1 && res.WorkingBytes > budget {
					t.Fatalf("working %d bytes over the bound %d: %+v", res.WorkingBytes, budget, res)
				}
				for z, n := range reader.reads {
					if n != 1 {
						t.Fatalf("slice %d read %d times (%+v)", z, n, res)
					}
				}
			})
		}
	}
}

func TestFilterSlabsBoundsTheMemory(t *testing.T) {
	// 4 MiB of input; the filter sees at most 1 MiB of it at once.
	dims, spacing := [3]int{128, 128, 64}, [3]float64{1, 1, 1}
	src := ctPhantom(dims, spacing, 3)
	dst := NewVolume(dims, spacing)
	const budget = 1 << 20
	for _, f := range []SlabFilter{
		MedianOptions{RadiusMM: [3]float64{1, 1, 1}, Workers: 2},
		BilateralOptions{SigmaMM: [3]float64{0.4, 0.4, 0.4}, RangeSigma: 50, Workers: 2},
		UnsharpMaskOptions{SigmaMM: [3]float64{1, 1, 0}, Workers: 2},
		NonLocalMeansOptions{NoiseSigma: 20, PatchRadiusMM: [3]float64{1, 1, 0}, SearchRadiusMM: [3]float64{1, 1, 1}, Workers: 2},
	} {
		runtime.GC()
		var before, after runtime.MemStats
		runtime.ReadMemStats(&before)
		res, err := FilterSlabs(context.Background(), f, src, dims, spacing, dst, SlabOptions{MaxWorkingBytes: budget})
		runtime.ReadMemStats(&after)
		if err != nil {
			t.Fatal(err)
		}
		allocated := int64(after.TotalAlloc - before.TotalAlloc)
		t.Logf("%T: allocated %d bytes for a %d-byte volume, bound %d: %+v", f, allocated, len(src.Data)*4, int64(budget), res)
		if res.WorkingBytes > budget || allocated > res.WorkingBytes+256<<10 || res.SlabSlices >= dims[2] {
			t.Errorf("%T: allocated %d bytes, working %d, bound %d: %+v", f, allocated, res.WorkingBytes, budget, res)
		}
	}
}

func TestAnisotropicDiffusionSlabsAreIdentical(t *testing.T) {
	src := ctPhantom([3]int{21, 18, 13}, [3]float64{0.7, 0.7, 1.5}, 7)
	sliceBytes := int64(21 * 18 * 4)
	for name, diffuse := range map[string]func(context.Context, *Volume, AnisotropicDiffusionOptions) (*Volume, error){
		"gradient":  GradientAnisotropicDiffusion,
		"curvature": CurvatureAnisotropicDiffusion,
	} {
		want, err := diffuse(context.Background(), src, AnisotropicDiffusionOptions{Iterations: 4})
		if err != nil {
			t.Fatal(err)
		}
		for _, budget := range []int64{1, 2 * sliceBytes, 7 * sliceBytes, 12 * sliceBytes} {
			got, err := diffuse(context.Background(), src, AnisotropicDiffusionOptions{Iterations: 4, MaxWorkingBytes: budget, Workers: 3})
			if err != nil {
				t.Fatal(err)
			}
			equalVolumes(t, fmt.Sprintf("%s, %d bytes", name, budget), got, want)
		}
	}
	if _, err := GradientAnisotropicDiffusion(context.Background(), src, AnisotropicDiffusionOptions{MaxWorkingBytes: -1}); !errors.Is(err, ErrInvalidVolume) {
		t.Fatalf("negative bound: %v", err)
	}

	// Beyond the output, a 1 MiB volume diffuses within a 256 KiB bound (two slabs of two slices).
	big := ctPhantom([3]int{128, 128, 16}, [3]float64{1, 1, 1}, 9)
	runtime.GC()
	var before, after runtime.MemStats
	runtime.ReadMemStats(&before)
	if _, err := GradientAnisotropicDiffusion(context.Background(), big, AnisotropicDiffusionOptions{Iterations: 3, MaxWorkingBytes: 256 << 10, Workers: 2}); err != nil {
		t.Fatal(err)
	}
	runtime.ReadMemStats(&after)
	if allocated := int64(after.TotalAlloc - before.TotalAlloc); allocated > int64(len(big.Data))*4+256<<10+64<<10 {
		t.Fatalf("allocated %d bytes for a %d-byte volume", allocated, len(big.Data)*4)
	}
}

func TestFilterSlabsErrors(t *testing.T) {
	dims, spacing := [3]int{16, 16, 12}, [3]float64{1, 1, 1}
	src := ctPhantom(dims, spacing, 5)
	dst := NewVolume(dims, spacing)
	median := MedianOptions{RadiusMM: [3]float64{1, 1, 1}}
	if _, err := FilterSlabs(context.Background(), UnsharpMaskOptions{SigmaMM: [3]float64{1, 1, 1}}, src, dims, spacing, dst, SlabOptions{}); !errors.Is(err, ErrInvalidVolume) {
		t.Fatalf("unsharp along z: %v", err)
	}
	if _, err := FilterSlabs(context.Background(), median, src, dims, spacing, dst, SlabOptions{MaxWorkingBytes: -1}); !errors.Is(err, ErrInvalidVolume) {
		t.Fatalf("negative bound: %v", err)
	}
	if _, err := FilterSlabs(context.Background(), median, src, [3]int{16, 0, 12}, spacing, dst, SlabOptions{}); !errors.Is(err, ErrInvalidVolume) {
		t.Fatalf("empty dims: %v", err)
	}
	if _, err := FilterSlabs(context.Background(), median, src, dims, [3]float64{1, 0, 1}, dst, SlabOptions{}); !errors.Is(err, ErrInvalidVolume) {
		t.Fatalf("zero spacing: %v", err)
	}
	reader := newCountingReader(src)
	reader.fail = 7
	if _, err := FilterSlabs(context.Background(), median, reader, dims, spacing, dst, SlabOptions{MaxWorkingBytes: 1}); err == nil {
		t.Fatal("a read error was ignored")
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := FilterSlabs(ctx, median, src, dims, spacing, dst, SlabOptions{}); !errors.Is(err, context.Canceled) {
		t.Fatalf("cancelled: %v", err)
	}
}

func BenchmarkFilterSlabsMedian3D(b *testing.B) {
	dims := [3]int{256, 256, 64}
	src := ctPhantom(dims, [3]float64{1, 1, 1}, 1)
	dst := NewVolume(dims, src.SpacingMM)
	for _, budget := range []int64{4 << 20, 0} {
		b.Run(fmt.Sprintf("bound%dMiB", budget>>20), func(b *testing.B) {
			b.SetBytes(int64(len(src.Data) * 4))
			for i := 0; i < b.N; i++ {
				if _, err := FilterSlabs(context.Background(), MedianOptions{RadiusMM: [3]float64{1, 1, 1}}, src, dims, src.SpacingMM, dst, SlabOptions{MaxWorkingBytes: budget}); err != nil {
					b.Fatal(err)
				}
			}
		})
	}
}
