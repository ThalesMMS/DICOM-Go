package imagefilter

// This harness is identical in isolated baseline and candidate checkouts.
// It depends only on public APIs so the baseline uses its original scheduler.

import (
	"context"
	"crypto/sha256"
	"encoding/binary"
	"fmt"
	"math"
	"os"
	"runtime"
	"strconv"
	"testing"

	"github.com/ThalesMMS/dicom-go/internal/parallel"
)

var gaussianBenchmarkSink []*Volume

func gaussianBenchmarkFixture(dims [3]int) *Volume {
	v := NewVolume(dims, [3]float64{0.7, 1.3, 2.1})
	for z := 0; z < dims[2]; z++ {
		for y := 0; y < dims[1]; y++ {
			for x := 0; x < dims[0]; x++ {
				v.Data[v.Index(x, y, z)] = float32(35*math.Sin(float64(x)*0.09) + 7*math.Cos(float64(y)*0.13) + 11*math.Sin(float64(z)*0.17) + float64((x+3*y+5*z)%19))
			}
		}
	}
	return v
}

func gaussianBenchmarkHash(volumes []*Volume) string {
	h := sha256.New()
	var word [8]byte
	for _, v := range volumes {
		for _, dim := range v.Dims {
			binary.LittleEndian.PutUint64(word[:], uint64(dim))
			_, _ = h.Write(word[:])
		}
		for _, spacing := range v.SpacingMM {
			binary.LittleEndian.PutUint64(word[:], math.Float64bits(spacing))
			_, _ = h.Write(word[:])
		}
		for _, sample := range v.Data {
			binary.LittleEndian.PutUint32(word[:4], math.Float32bits(sample))
			_, _ = h.Write(word[:4])
		}
	}
	return fmt.Sprintf("%x", h.Sum(nil))
}

func gaussianBenchmarkAPI(ctx context.Context, src *Volume, opts RecursiveGaussianOptions, api string) ([]*Volume, error) {
	switch api {
	case "gradient":
		v, err := GradientRecursiveGaussian(ctx, src, opts)
		if err != nil {
			return nil, err
		}
		return v[:], nil
	case "hessian":
		v, err := HessianRecursiveGaussian(ctx, src, opts)
		if err != nil {
			return nil, err
		}
		return []*Volume{v.XX, v.XY, v.XZ, v.YY, v.YZ, v.ZZ}, nil
	case "vesselness":
		v, err := Vesselness(ctx, src, VesselnessOptions{SigmasMM: []float64{opts.SigmaMM[0]}, Gamma: 10, MaxWorkingBytes: 4 << 20, Workers: opts.Workers})
		if err != nil {
			return nil, err
		}
		return []*Volume{v.Response}, nil
	}
	var v *Volume
	var err error
	switch api {
	case "smooth":
		v, err = SmoothRecursiveGaussian(ctx, src, opts)
	case "magnitude":
		v, err = GradientMagnitudeRecursiveGaussian(ctx, src, opts)
	case "laplacian":
		v, err = LaplacianRecursiveGaussian(ctx, src, opts)
	default:
		return nil, fmt.Errorf("unknown Gaussian benchmark API %q", api)
	}
	if err != nil {
		return nil, err
	}
	return []*Volume{v}, nil
}

func BenchmarkRecursiveGaussianReuse(b *testing.B) {
	for _, api := range []string{"smooth", "gradient", "magnitude", "hessian", "laplacian", "vesselness"} {
		for _, sigma := range []float64{1, 8} {
			b.Run(fmt.Sprintf("%s/sigma=%g", api, sigma), func(b *testing.B) {
				src := gaussianBenchmarkFixture([3]int{96, 80, 48})
				opts := RecursiveGaussianOptions{SigmaMM: IsotropicSigma(sigma)}
				inputHash := gaussianBenchmarkHash([]*Volume{src})
				probe, err := gaussianBenchmarkAPI(context.Background(), src, opts, api)
				if err != nil {
					b.Fatal(err)
				}
				outputHash := gaussianBenchmarkHash(probe)
				probe = nil
				b.ReportAllocs()
				b.ResetTimer()
				for i := 0; i < b.N; i++ {
					gaussianBenchmarkSink = nil
					gaussianBenchmarkSink, err = gaussianBenchmarkAPI(context.Background(), src, opts, api)
					if err != nil {
						b.Fatal(err)
					}
				}
				b.StopTimer()
				if gaussianBenchmarkHash([]*Volume{src}) != inputHash || gaussianBenchmarkHash(gaussianBenchmarkSink) != outputHash {
					b.Fatal("benchmark source/output hash changed")
				}
				b.Logf("dims=%v spacing=%v sigma=%g runtime=%s GOMAXPROCS=%d helperBudget=%d input=%s output=%s", src.Dims, src.SpacingMM, sigma, runtime.Version(), runtime.GOMAXPROCS(0), parallel.Budget(), inputHash, outputHash)
				gaussianBenchmarkSink = nil
			})
		}
	}
}

// Run an already compiled test binary under /usr/bin/time -l with an explicit
// API. Each process prepares one input, performs one API call and hashes all
// outputs; its RSS includes input, runtime, hashes and the whole API request.
func TestRecursiveGaussianReuseProcess(t *testing.T) {
	api := os.Getenv("DICOM_GAUSSIAN_PROCESS_API")
	if api == "" {
		t.Skip("isolated process measurement requires DICOM_GAUSSIAN_PROCESS_API")
	}
	dim, sigma, workers := 256, 1.0, 0
	var err error
	if s := os.Getenv("DICOM_GAUSSIAN_PROCESS_DIM"); s != "" {
		dim, err = strconv.Atoi(s)
		if err != nil || dim <= 0 {
			t.Fatal("invalid process dimension")
		}
	}
	if s := os.Getenv("DICOM_GAUSSIAN_PROCESS_SIGMA"); s != "" {
		sigma, err = strconv.ParseFloat(s, 64)
		if err != nil || sigma <= 0 {
			t.Fatal("invalid process sigma")
		}
	}
	if s := os.Getenv("DICOM_GAUSSIAN_PROCESS_WORKERS"); s != "" {
		workers, err = strconv.Atoi(s)
		if err != nil || workers < 0 {
			t.Fatal("invalid process workers")
		}
	}
	src := gaussianBenchmarkFixture([3]int{dim, dim, dim})
	inputHash := gaussianBenchmarkHash([]*Volume{src})
	var before, after runtime.MemStats
	runtime.ReadMemStats(&before)
	out, err := gaussianBenchmarkAPI(context.Background(), src, RecursiveGaussianOptions{SigmaMM: IsotropicSigma(sigma), Workers: workers}, api)
	if err != nil {
		t.Fatal(err)
	}
	runtime.ReadMemStats(&after)
	outputHash := gaussianBenchmarkHash(out)
	if gaussianBenchmarkHash([]*Volume{src}) != inputHash {
		t.Fatal("process source changed")
	}
	t.Logf("api=%s dims=%v spacing=%v sigma=%g workers=%d runtime=%s GOMAXPROCS=%d helperBudget=%d input=%s output=%s API_TotalAlloc=%d API_Mallocs=%d", api, src.Dims, src.SpacingMM, sigma, workers, runtime.Version(), runtime.GOMAXPROCS(0), parallel.Budget(), inputHash, outputHash, after.TotalAlloc-before.TotalAlloc, after.Mallocs-before.Mallocs)
	runtime.KeepAlive(src)
	runtime.KeepAlive(out)
}
