package imagefilter

import (
	"context"
	"errors"
	"fmt"
	"math"
	"math/rand"
	"testing"
)

// shapesPhantom is a piecewise-constant phantom of modality values in
// [0, 1000]: a background, a disc, a bright square, a thin bar and a
// staircase, per slice shifted so 3-D neighbours differ. clean holds the
// noise-free values and noisy adds Gaussian noise of sigma.
func shapesPhantom(dims [3]int, spacing [3]float64, sigma float64, seed int64) (clean, noisy *Volume) {
	clean, noisy = NewVolume(dims, spacing), NewVolume(dims, spacing)
	rng := rand.New(rand.NewSource(seed))
	nx, ny := float64(dims[0]), float64(dims[1])
	for z := 0; z < dims[2]; z++ {
		for y := 0; y < dims[1]; y++ {
			for x := 0; x < dims[0]; x++ {
				fx, fy := float64(x)/nx, float64(y)/ny
				v := 200.0
				if dx, dy := fx-0.35, fy-0.4; dx*dx+dy*dy < 0.04+0.0004*float64(z) {
					v = 600
				}
				if fx > 0.6 && fx < 0.85 && fy > 0.15 && fy < 0.4 {
					v = 1000
				}
				if fx > 0.1 && fx < 0.9 && fy > 0.72 && fy < 0.76 {
					v = 800
				}
				if fx > 0.6 && fy > 0.5 && fy < 0.65 {
					v = 300 + 100*math.Floor(fx*10-6)
				}
				i := clean.Index(x, y, z)
				clean.Data[i] = float32(v)
				noisy.Data[i] = float32(v + sigma*rng.NormFloat64())
			}
		}
	}
	return clean, noisy
}

// psnr is the peak signal-to-noise ratio of v against clean for a peak of
// 1000, in dB.
func psnr(v, clean *Volume) float64 {
	sum := 0.0
	for i := range v.Data {
		d := float64(v.Data[i] - clean.Data[i])
		sum += d * d
	}
	return 10 * math.Log10(1000*1000/(sum/float64(len(v.Data))))
}

func TestNonLocalMeansImprovesPSNR(t *testing.T) {
	const sigma = 40 // a PSNR of 28 dB for the peak of 1000
	for _, c := range []struct {
		name    string
		dims    [3]int
		spacing [3]float64
		opts    NonLocalMeansOptions
		minGain float64
	}{
		{"2-D", [3]int{128, 128, 1}, [3]float64{0.7, 0.7, 0}, NonLocalMeansOptions{NoiseSigma: sigma}, 10},
		{"3-D", [3]int{64, 64, 16}, [3]float64{0.8, 0.8, 1}, NonLocalMeansOptions{NoiseSigma: sigma, SearchRadiusMM: [3]float64{2.4, 2.4, 2}}, 10},
		{"3-D slice by slice", [3]int{64, 64, 6}, [3]float64{0.8, 0.8, 1}, NonLocalMeansOptions{NoiseSigma: sigma, PatchRadiusMM: [3]float64{0.8, 0.8, 0}, SearchRadiusMM: [3]float64{4, 4, 0}}, 8},
	} {
		clean, noisy := shapesPhantom(c.dims, c.spacing, sigma, 1186)
		out, err := NonLocalMeans(context.Background(), noisy, c.opts)
		if err != nil {
			t.Fatal(err)
		}
		before, after := psnr(noisy, clean), psnr(out, clean)
		// The bilateral filter at the display settings is the baseline.
		bilateral, err := Bilateral(context.Background(), noisy, BilateralOptions{SigmaMM: [3]float64{1, 1, 0}, RangeSigma: 3 * sigma})
		if err != nil {
			t.Fatal(err)
		}
		t.Logf("%s: PSNR %.2f dB noisy, %.2f dB non-local means (+%.2f), %.2f dB bilateral", c.name, before, after, after-before, psnr(bilateral, clean))
		if after-before < c.minGain || after < psnr(bilateral, clean)+1 {
			t.Errorf("%s: PSNR %.2f → %.2f dB, want a gain of %.0f dB and 1 dB over the bilateral filter", c.name, before, after, c.minGain)
		}
	}
}

func TestNonLocalMeansKeepsTheStep(t *testing.T) {
	src := noisyStep(64, 0.7, 10, 1186)
	out, err := NonLocalMeans(context.Background(), src, NonLocalMeansOptions{NoiseSigma: 10})
	if err != nil {
		t.Fatal(err)
	}
	if noise, width := flatNoise(out), edgeWidth(out); noise > flatNoise(src)/3 || width > edgeWidth(src)+0.3 {
		t.Fatalf("flat noise %.2f of %.2f, edge width %.2f of %.2f samples", noise, flatNoise(src), width, edgeWidth(src))
	}
}

// referenceNLM is the blockwise definition computed directly in float64:
// every block estimate pushed onto the samples it covers.
func referenceNLM(src *Volume, patch, search [3]int, step int, sigma, beta float64) *Volume {
	dims := src.Dims
	at := func(x, y, z int) float64 {
		return float64(src.At(clampAxis(x, dims[0]), clampAxis(y, dims[1]), clampAxis(z, dims[2])))
	}
	var centres [3][]int
	for axis := range centres {
		s := min(step, 2*patch[axis]+1)
		for c := 0; ; c += s {
			if c >= dims[axis]-1 {
				centres[axis] = append(centres[axis], dims[axis]-1)
				break
			}
			centres[axis] = append(centres[axis], c)
		}
	}
	h2 := 2 * beta * sigma * sigma
	n := float64((2*patch[0] + 1) * (2*patch[1] + 1) * (2*patch[2] + 1))
	sum := make([]float64, len(src.Data))
	count := make([]float64, len(src.Data))
	for _, cz := range centres[2] {
		for _, cy := range centres[1] {
			for _, cx := range centres[0] {
				type cand struct {
					x, y, z int
					w       float64
				}
				var cands []cand
				best := 0.0
				for qz := max(0, cz-search[2]); qz <= min(dims[2]-1, cz+search[2]); qz++ {
					for qy := max(0, cy-search[1]); qy <= min(dims[1]-1, cy+search[1]); qy++ {
						for qx := max(0, cx-search[0]); qx <= min(dims[0]-1, cx+search[0]); qx++ {
							if qx == cx && qy == cy && qz == cz {
								continue
							}
							d := 0.0
							for dz := -patch[2]; dz <= patch[2]; dz++ {
								for dy := -patch[1]; dy <= patch[1]; dy++ {
									for dx := -patch[0]; dx <= patch[0]; dx++ {
										e := at(cx+dx, cy+dy, cz+dz) - at(qx+dx, qy+dy, qz+dz)
										d += e * e
									}
								}
							}
							if d > nlmCutoff*h2*n {
								continue
							}
							w := math.Exp(-d / n / h2)
							best = max(best, w)
							cands = append(cands, cand{qx, qy, qz, w})
						}
					}
				}
				if best == 0 {
					best = 1
				}
				cands = append(cands, cand{cx, cy, cz, best})
				for dz := -patch[2]; dz <= patch[2]; dz++ {
					for dy := -patch[1]; dy <= patch[1]; dy++ {
						for dx := -patch[0]; dx <= patch[0]; dx++ {
							x, y, z := cx+dx, cy+dy, cz+dz
							if x < 0 || y < 0 || z < 0 || x >= dims[0] || y >= dims[1] || z >= dims[2] {
								continue
							}
							num, den := 0.0, 0.0
							for _, c := range cands {
								num += c.w * at(c.x+dx, c.y+dy, c.z+dz)
								den += c.w
							}
							i := src.Index(x, y, z)
							sum[i] += num / den
							count[i]++
						}
					}
				}
			}
		}
	}
	out := NewVolume(dims, src.SpacingMM)
	for i := range sum {
		out.Data[i] = float32(sum[i] / count[i])
	}
	return out
}

func TestNonLocalMeansMatchesTheDefinition(t *testing.T) {
	for _, c := range []struct {
		dims            [3]int
		patch, search   [3]int
		step            int
		sigma, beta     float64
		spacing         [3]float64
		patchMM, srchMM [3]float64
	}{
		{[3]int{17, 13, 1}, [3]int{1, 1, 0}, [3]int{3, 2, 0}, 2, 30, 1, [3]float64{1, 1, 0}, [3]float64{1, 1, 0}, [3]float64{3, 2, 0}},
		{[3]int{15, 12, 1}, [3]int{2, 1, 0}, [3]int{4, 4, 0}, 3, 25, 0.5, [3]float64{0.5, 1, 0}, [3]float64{1, 1, 0}, [3]float64{2, 4, 0}},
		{[3]int{11, 10, 9}, [3]int{1, 1, 1}, [3]int{2, 2, 1}, 2, 40, 1, [3]float64{1, 1, 2}, [3]float64{1, 1, 2}, [3]float64{2, 2, 2}},
		{[3]int{9, 8, 7}, [3]int{1, 1, 1}, [3]int{1, 2, 2}, 1, 20, 2, [3]float64{1, 1, 1}, [3]float64{1, 1, 1}, [3]float64{1, 2, 2}},
		{[3]int{10, 9, 6}, [3]int{1, 1, 0}, [3]int{2, 2, 1}, 2, 30, 1, [3]float64{1, 1, 1}, [3]float64{1, 1, 0}, [3]float64{2, 2, 1}},
	} {
		_, noisy := shapesPhantom(c.dims, c.spacing, c.sigma, 7)
		want := referenceNLM(noisy, c.patch, c.search, c.step, c.sigma, c.beta)
		opts := NonLocalMeansOptions{NoiseSigma: c.sigma, Beta: c.beta, PatchRadiusMM: c.patchMM, SearchRadiusMM: c.srchMM, BlockStep: c.step}
		var first *Volume
		for _, workers := range []int{1, 4} {
			opts.Workers = workers
			got, err := NonLocalMeans(context.Background(), noisy, opts)
			if err != nil {
				t.Fatal(err)
			}
			for i := range want.Data {
				if d := math.Abs(float64(got.Data[i] - want.Data[i])); d > 1e-3 {
					t.Fatalf("%v patch %v search %v step %d: sample %d is %v, want %v", c.dims, c.patch, c.search, c.step, i, got.Data[i], want.Data[i])
				}
			}
			if first == nil {
				first = got
			} else {
				equalVolumes(t, "workers", got, first)
			}
		}
	}
}

func TestNonLocalMeansStreamsBitIdentically(t *testing.T) {
	dims, spacing := [3]int{20, 18, 15}, [3]float64{0.8, 0.8, 1.2}
	_, noisy := shapesPhantom(dims, spacing, 30, 11)
	opts := NonLocalMeansOptions{NoiseSigma: 30, SearchRadiusMM: [3]float64{2.4, 2.4, 1.2}, Workers: 3}
	want, err := NonLocalMeans(context.Background(), noisy, opts)
	if err != nil {
		t.Fatal(err)
	}
	for _, budget := range []int64{1, 200 << 10, 0} {
		got := NewVolume(dims, spacing)
		reader := newCountingReader(noisy)
		res, err := FilterSlabs(context.Background(), opts, reader, dims, spacing, got, SlabOptions{MaxWorkingBytes: budget})
		if err != nil {
			t.Fatal(err)
		}
		equalVolumes(t, fmt.Sprintf("bound %d (%+v)", budget, res), got, want)
		if res.HaloSlices != 3 {
			t.Fatalf("halo %d slices, want 2·1 + 1", res.HaloSlices)
		}
	}
}

func TestNonLocalMeansConstantCancellationAndOptions(t *testing.T) {
	flat := NewVolume([3]int{24, 20, 5}, [3]float64{1, 1, 1})
	for i := range flat.Data {
		flat.Data[i] = -437
	}
	out, err := NonLocalMeans(context.Background(), flat, NonLocalMeansOptions{NoiseSigma: 5})
	if err != nil {
		t.Fatal(err)
	}
	for i, v := range out.Data {
		if math.Abs(float64(v)+437) > 1e-3 {
			t.Fatalf("a constant volume changed at %d: %v", i, v)
		}
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := NonLocalMeans(ctx, flat, NonLocalMeansOptions{NoiseSigma: 5}); !errors.Is(err, context.Canceled) {
		t.Fatalf("cancelled: %v", err)
	}
	for _, opts := range []NonLocalMeansOptions{
		{},
		{NoiseSigma: math.Inf(1)},
		{NoiseSigma: 5, Beta: -1},
		{NoiseSigma: 5, BlockStep: -2},
		{NoiseSigma: 5, PatchRadiusMM: [3]float64{-1, 1, 1}},
		{NoiseSigma: 5, SearchRadiusMM: [3]float64{9, 9, 9}, MaxSearchSamples: 100},
	} {
		if _, err := NonLocalMeans(context.Background(), flat, opts); !errors.Is(err, ErrInvalidVolume) {
			t.Errorf("%+v: %v", opts, err)
		}
	}
}

func TestBlockCoverCoversEverySample(t *testing.T) {
	for n := 1; n < 20; n++ {
		for radius := 0; radius < 3; radius++ {
			for step := 1; step <= 2*radius+1; step++ {
				centres, lo, hi := blockCover(n, radius, step)
				for p := 0; p < n; p++ {
					if lo[p] > hi[p] || centres[lo[p]] < p-radius || centres[hi[p]] > p+radius ||
						(lo[p] > 0 && centres[lo[p]-1] >= p-radius) || (hi[p]+1 < len(centres) && centres[hi[p]+1] <= p+radius) {
						t.Fatalf("n %d radius %d step %d: sample %d covered by %d..%d of %v", n, radius, step, p, lo[p], hi[p], centres)
					}
				}
			}
		}
	}
}

func BenchmarkNonLocalMeans2D512(b *testing.B) {
	_, noisy := shapesPhantom([3]int{512, 512, 1}, [3]float64{0.7, 0.7, 0}, 20, 1)
	b.SetBytes(int64(len(noisy.Data) * 4))
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if _, err := NonLocalMeans(context.Background(), noisy, NonLocalMeansOptions{NoiseSigma: 20}); err != nil {
			b.Fatal(err)
		}
	}
}

func BenchmarkNonLocalMeans3D128x128x32(b *testing.B) {
	_, noisy := shapesPhantom([3]int{128, 128, 32}, [3]float64{1, 1, 1}, 20, 1)
	b.SetBytes(int64(len(noisy.Data) * 4))
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if _, err := NonLocalMeans(context.Background(), noisy, NonLocalMeansOptions{NoiseSigma: 20, SearchRadiusMM: [3]float64{3, 3, 1}}); err != nil {
			b.Fatal(err)
		}
	}
}
