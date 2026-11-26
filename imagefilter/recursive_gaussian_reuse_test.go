package imagefilter

// This test-only scheduler and recurrence preserve the implementation at
// d5b3e3796a390fc4b6f02afac6d77e28d94ac679. The comparison checks operation
// order and float32 rounding; independent convolution and existing analytic
// and ITK fixtures remain the numerical authorities.

import (
	"context"
	"errors"
	"fmt"
	"math"
	"math/cmplx"
	"slices"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/ThalesMMS/dicom-go/internal/parallel"
)

func reuseFixture(dims [3]int) *Volume {
	v := NewVolume(dims, [3]float64{0.7, 1.3, 2.1})
	for z := 0; z < dims[2]; z++ {
		for y := 0; y < dims[1]; y++ {
			for x := 0; x < dims[0]; x++ {
				i := v.Index(x, y, z)
				v.Data[i] = float32(13*math.Sin(float64(i)*0.173) + float64(x*x-2*y+3*z)/17)
			}
		}
	}
	return v
}

func reuseEqual(t *testing.T, got, want []float32) {
	t.Helper()
	if len(got) != len(want) {
		t.Fatalf("length %d, want %d", len(got), len(want))
	}
	for i, v := range got {
		if math.Float32bits(v) != math.Float32bits(want[i]) {
			t.Fatalf("sample %d: %g (%08x), want %g (%08x)", i, v, math.Float32bits(v), want[i], math.Float32bits(want[i]))
		}
	}
}

// legacyReuseAPI uses the old scheduler for the public API reductions too:
// squaring, addition and rounding occur in the original emission order.
func legacyReuseAPI(ctx context.Context, src *Volume, opts RecursiveGaussianOptions, api string, mixed [3]int) ([]*Volume, error) {
	var specs [][3]int
	switch api {
	case "smooth":
		specs = [][3]int{{}}
	case "mixed":
		specs = [][3]int{mixed}
	case "gradient", "magnitude":
		specs = [][3]int{{1, 0, 0}, {0, 1, 0}, {0, 0, 1}}
	case "hessian":
		specs = [][3]int{{2, 0, 0}, {1, 1, 0}, {1, 0, 1}, {0, 2, 0}, {0, 1, 1}, {0, 0, 2}}
	case "laplacian":
		specs = [][3]int{{2, 0, 0}, {0, 2, 0}, {0, 0, 2}}
	default:
		panic(api)
	}
	out := make([]*Volume, len(specs))
	var acc []float32
	err := legacyRunRecursiveGaussian(ctx, src, specs, opts, func(p *gaussianPlan, i int, data []float32) error {
		if api != "magnitude" && api != "laplacian" {
			out[i] = p.volume(data)
			return nil
		}
		if acc == nil {
			if api == "laplacian" {
				acc = data
				return nil
			}
			return p.parallelSpan(len(data), func(lo, hi int) {
				for j, v := range data[lo:hi] {
					data[lo+j] = v * v
				}
			}, func() { acc = data })
		}
		defer p.pool.put(data)
		return p.parallelSpan(len(data), func(lo, hi int) {
			for j, v := range data[lo:hi] {
				if api == "magnitude" {
					acc[lo+j] += v * v
				} else {
					acc[lo+j] += v
				}
			}
		}, nil)
	}, func(p *gaussianPlan) error {
		if api != "magnitude" {
			return nil
		}
		return p.parallelSpan(len(acc), func(lo, hi int) {
			for j, v := range acc[lo:hi] {
				acc[lo+j] = float32(math.Sqrt(float64(v)))
			}
		}, nil)
	})
	if err != nil {
		return nil, err
	}
	if acc != nil {
		return []*Volume{{Dims: src.Dims, SpacingMM: src.SpacingMM, Data: acc}}, nil
	}
	return out, nil
}

func candidateReuseAPI(ctx context.Context, src *Volume, opts RecursiveGaussianOptions, api string, mixed [3]int) ([]*Volume, error) {
	if api == "mixed" {
		v, err := RecursiveGaussian(ctx, src, mixed, opts)
		if err != nil {
			return nil, err
		}
		return []*Volume{v}, nil
	}
	return gaussianBenchmarkAPI(ctx, src, opts, api)
}

func TestRecursiveGaussianReuseBitExact(t *testing.T) {
	for _, dims := range [][3]int{{33, 7, 5}, {31, 3, 2}, {32, 5, 3}, {1, 33, 5}, {33, 1, 3}, {33, 5, 1}, {1, 1, 1}} {
		for _, sigma := range [][3]float64{{0.12, 0.35, 0.8}, {1.5, 3, 7}, {32, 64, 96}} {
			for _, normalized := range []bool{false, true} {
				t.Run(fmt.Sprintf("%v/%v/normalized=%v", dims, sigma, normalized), func(t *testing.T) {
					src := reuseFixture(dims)
					original := slices.Clone(src.Data)
					for _, api := range []string{"smooth", "mixed", "gradient", "magnitude", "hessian", "laplacian"} {
						for _, workers := range []int{1, 3} {
							opts := RecursiveGaussianOptions{SigmaMM: sigma, NormalizeAcrossScale: normalized, Workers: workers}
							got, err := candidateReuseAPI(context.Background(), src, opts, api, [3]int{1, 2, 1})
							if err != nil {
								t.Fatal(err)
							}
							want, err := legacyReuseAPI(context.Background(), src, opts, api, [3]int{1, 2, 1})
							if err != nil {
								t.Fatal(err)
							}
							for i, v := range got {
								if v.Dims != src.Dims || v.SpacingMM != src.SpacingMM {
									t.Fatal("output grid changed")
								}
								reuseEqual(t, v.Data, want[i].Data)
							}
							reuseEqual(t, src.Data, original)
							// Every emitted component owns a distinct backing, even
							// when some passes are identities or derivatives are zero.
							for i, v := range got {
								if &v.Data[0] == &src.Data[0] {
									t.Fatalf("%s output aliases input", api)
								}
								for j := 0; j < i; j++ {
									if &v.Data[0] == &got[j].Data[0] {
										t.Fatalf("%s outputs %d/%d alias", api, i, j)
									}
								}
							}
							got[0].Data[0] = 1234567
							reuseEqual(t, src.Data, original)
							for i := 1; i < len(got); i++ {
								reuseEqual(t, got[i].Data, want[i].Data)
							}
						}
					}
				})
			}
		}
	}
	// Selectively absent kernels exercise private identity ownership on both
	// y and x; an all-zero sigma still returns an independent source copy.
	for _, sigma := range [][3]float64{{0, 0, 0}, {0, 0, 2}, {0, 2, 0}, {2, 0, 0}, {0, 2, 3}, {2, 0, 3}} {
		src := reuseFixture([3]int{33, 5, 3})
		original := slices.Clone(src.Data)
		opts := RecursiveGaussianOptions{SigmaMM: sigma, Workers: 2}
		got, err := SmoothRecursiveGaussian(context.Background(), src, opts)
		if err != nil {
			t.Fatal(err)
		}
		want, err := legacyReuseAPI(context.Background(), src, opts, "smooth", [3]int{})
		if err != nil {
			t.Fatal(err)
		}
		reuseEqual(t, got.Data, want[0].Data)
		reuseEqual(t, src.Data, original)
		if &got.Data[0] == &src.Data[0] {
			t.Fatalf("sigma %v aliases source", sigma)
		}
	}
}

// directGaussianLine is an independent, nonrecursive convolution with the
// fitted kernel. It sums each finite input sample explicitly and evaluates
// the two replicated infinite tails as geometric series.
func directGaussianLine(k *recursiveKernel, src []float32, j int) float64 {
	var sum complex128
	for pole, z := range k.z {
		for i, v := range src {
			if i <= j {
				sum += complex(float64(v), 0) * k.c[pole] * cmplx.Pow(z, complex(float64(j-i), 0))
			} else {
				sum += complex(float64(v), 0) * k.d[pole] * cmplx.Pow(z, complex(float64(i-j), 0))
			}
		}
		sum += complex(float64(src[0]), 0) * k.c[pole] * cmplx.Pow(z, complex(float64(j+1), 0)) / (1 - z)
		sum += complex(float64(src[len(src)-1]), 0) * k.d[pole] * cmplx.Pow(z, complex(float64(len(src)-j), 0)) / (1 - z)
	}
	return real(sum)
}

func TestRecursiveGaussianReuseLanesAndIndependentOracle(t *testing.T) {
	for _, count := range []int{1, 31, 32} {
		for _, interleaved := range []bool{false, true} {
			const length = 65
			step, stride := 1, length
			if interleaved {
				step, stride = count, 1
			}
			for _, sigma := range []float64{0.15, 1, 8, 64} {
				for order := 0; order <= 2; order++ {
					for _, normalized := range []bool{false, true} {
						scale := math.Pow(1.7, -float64(order))
						if normalized {
							scale *= math.Pow(sigma*1.7, float64(order))
						}
						k, err := newRecursiveKernel(sigma, order, scale)
						if err != nil {
							t.Fatal(err)
						}
						src := make([]float32, length*count)
						for l := 0; l < count; l++ {
							for j := 0; j < length; j++ {
								var v float64
								switch l % 4 {
								case 0:
									v = 3.25 // constant, including both faces
								case 1:
									v = float64(j-length/2) * 0.125 // ramp
								case 2:
									if j == length/2 {
										v = 17 // impulse
									}
								default:
									v = math.Sin(float64(j)*0.83) + float64(j%7)/11
								}
								src[l*stride+j*step] = float32(v)
							}
						}
						out, legacy, aliased := make([]float32, len(src)), make([]float32, len(src)), slices.Clone(src)
						tmp, state := make([]float64, length*count), make([]float64, 4*count)
						k.filterLanes(out, src, 0, step, stride, length, count, tmp, state)
						k.legacyFilterLanes(legacy, src, 0, step, stride, length, count, tmp, state)
						k.filterLanes(aliased, aliased, 0, step, stride, length, count, tmp, state)
						reuseEqual(t, out, legacy)
						reuseEqual(t, aliased, legacy)
						for l := 0; l < count; l++ {
							line := make([]float32, length)
							for j := range line {
								line[j] = src[l*stride+j*step]
							}
							for j := range line {
								want := directGaussianLine(k, line, j)
								got := float64(out[l*stride+j*step])
								if math.Abs(got-want) > 3e-6*max(1, math.Abs(want)) {
									t.Fatalf("count=%d interleaved=%v sigma=%g order=%d normalized=%v lane=%d sample=%d: %g want %g", count, interleaved, sigma, order, normalized, l, j, got, want)
								}
							}
						}
						// Compact windows keep their non-aliasing contract used by
						// vesselness. A partial causal buffer must not change faces.
						for _, limits := range [][2]int{{0, 1}, {0, length}, {7, 31}, {length - 1, length}} {
							lo, hi := limits[0], limits[1]
							window := make([]float32, (hi-lo)*count)
							w := laneWindow{step: count, laneStride: 1, lo: lo, hi: hi}
							k.filterLanesWindow(window, w, src, 0, step, stride, length, count, tmp, state)
							for j := lo; j < hi; j++ {
								for l := 0; l < count; l++ {
									if math.Float32bits(window[(j-lo)*count+l]) != math.Float32bits(legacy[l*stride+j*step]) {
										t.Fatal("compact window changed output")
									}
								}
							}
						}
					}
				}
			}
		}
	}
}

func TestRecursiveGaussianReusePrivateBackings(t *testing.T) {
	for _, tc := range []struct {
		name           string
		specs          [][3]int
		reduce         bool
		old, candidate int
	}{
		{"smooth", [][3]int{{}}, false, 2, 1},
		{"gradient", [][3]int{{1, 0, 0}, {0, 1, 0}, {0, 0, 1}}, false, 4, 3},
		{"hessian", [][3]int{{2, 0, 0}, {1, 1, 0}, {1, 0, 1}, {0, 2, 0}, {0, 1, 1}, {0, 0, 2}}, false, 7, 6},
		{"magnitude", [][3]int{{1, 0, 0}, {0, 1, 0}, {0, 0, 1}}, true, 3, 2},
		{"laplacian", [][3]int{{2, 0, 0}, {0, 2, 0}, {0, 0, 2}}, true, 3, 2},
	} {
		t.Run(tc.name, func(t *testing.T) {
			for _, legacy := range []bool{true, false} {
				src := reuseFixture([3]int{33, 7, 5})
				seen := map[*float32]bool{}
				var plan *gaussianPlan
				first := true
				run := runRecursiveGaussian
				if legacy {
					run = legacyRunRecursiveGaussian
				}
				err := run(context.Background(), src, tc.specs, RecursiveGaussianOptions{SigmaMM: IsotropicSigma(2), Workers: 1}, func(p *gaussianPlan, _ int, data []float32) error {
					plan = p
					seen[&data[0]] = true
					if tc.reduce && !first {
						p.pool.put(data)
					}
					first = false
					return nil
				})
				if err != nil {
					t.Fatal(err)
				}
				for _, data := range plan.pool.free {
					seen[&data[0]] = true
				}
				want := tc.candidate
				if legacy {
					want = tc.old
				}
				if len(seen) != want || seen[&src.Data[0]] {
					t.Fatalf("legacy=%v private backings=%d want=%d; source in pool=%v", legacy, len(seen), want, seen[&src.Data[0]])
				}
			}
		})
	}
}

func TestRecursiveGaussianReuseScratchAndOwnership(t *testing.T) {
	previous := parallel.SetBudget(2)
	defer parallel.SetBudget(previous)
	src := reuseFixture([3]int{33, 7, 5})
	opts := RecursiveGaussianOptions{SigmaMM: [3]float64{0, 2, 3}, Workers: 1 << 20}
	p, err := newGaussianPlan(src, [][3]int{{}}, opts)
	if err != nil {
		t.Fatal(err)
	}
	parallel.ResetPeak()
	z, err := p.pass(context.Background(), node{data: src.Data}, 2, 0, true)
	if err != nil || !z.owned || &z.data[0] == &src.Data[0] {
		t.Fatalf("borrowed source was consumed: %v", err)
	}
	addresses := make(map[*gaussianLaneScratch]*float64)
	for _, s := range p.scratch.items {
		if s.tmp != nil {
			addresses[s] = &s.state[0]
		}
	}
	y, err := p.pass(context.Background(), z, 1, 0, true)
	if err != nil || &y.data[0] != &z.data[0] || !y.owned {
		t.Fatalf("private pass failed ownership transfer: %v", err)
	}
	x, err := p.pass(context.Background(), y, 0, 0, true)
	if err != nil || &x.data[0] != &y.data[0] || !x.owned {
		t.Fatalf("identity failed ownership transfer: %v", err)
	}
	for _, s := range p.scratch.items {
		if old := addresses[s]; old != nil && old != &s.state[0] {
			t.Fatal("worker state scratch replaced between axes")
		}
		if len(s.tmp) > 7*32 {
			t.Fatalf("scratch samples=%d want maximum=%d", len(s.tmp), 7*32)
		}
	}
	causal := make(map[*gaussianLaneScratch]*float64)
	for _, s := range p.scratch.items {
		if s.tmp != nil {
			causal[s] = &s.tmp[0]
		}
	}
	if _, err := p.pass(context.Background(), x, 2, 0, true); err != nil {
		t.Fatal(err)
	}
	for _, s := range p.scratch.items {
		if old := causal[s]; old != nil && old != &s.tmp[0] {
			t.Fatal("causal scratch replaced by a smaller-axis pass")
		}
	}
	if len(p.scratch.items) > 3 || parallel.Peak() > 2 || parallel.InUse() != 0 {
		t.Fatalf("unbounded helpers/scratch: items=%d peak=%d inUse=%d", len(p.scratch.items), parallel.Peak(), parallel.InUse())
	}
	borrowed, err := p.pass(context.Background(), x, 0, 0, false)
	if err != nil || borrowed.owned || &borrowed.data[0] != &x.data[0] {
		t.Fatal("non-consuming identity claimed parent ownership")
	}
	q, err := newGaussianPlan(src, [][3]int{{}}, opts)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := q.pass(context.Background(), node{data: src.Data}, 2, 0, false); err != nil {
		t.Fatal(err)
	}
	for _, s := range q.scratch.items {
		if _, shared := addresses[s]; shared {
			t.Fatal("requests shared worker scratch")
		}
	}
}

func TestRecursiveGaussianReuseElongatedScratch(t *testing.T) {
	previous := parallel.SetBudget(2)
	defer parallel.SetBudget(previous)
	src := reuseFixture([3]int{65537, 3, 3})
	p, err := newGaussianPlan(src, [][3]int{{}}, RecursiveGaussianOptions{SigmaMM: IsotropicSigma(2), Workers: 3})
	if err != nil {
		t.Fatal(err)
	}
	z, err := p.pass(context.Background(), node{data: src.Data}, 2, 0, false)
	if err != nil {
		t.Fatal(err)
	}
	for _, s := range p.scratch.items {
		if len(s.tmp) > 3*32 {
			t.Fatalf("short-axis worker eagerly allocated long-axis scratch: %d", len(s.tmp))
		}
	}
	y, err := p.pass(context.Background(), z, 1, 0, true)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := p.pass(context.Background(), y, 0, 0, true); err != nil {
		t.Fatal(err)
	}
	large := 0
	for _, s := range p.scratch.items {
		if len(s.tmp) > 3*32 {
			large++
			if len(s.tmp) != 65537*9 {
				t.Fatalf("one long group requires %d samples, got %d", 65537*9, len(s.tmp))
			}
		}
	}
	if large != 1 {
		t.Fatalf("long-axis scratch reached %d workers, want the sole active group", large)
	}
}

// checkedCancelContext cancels during line processing, after many successful
// claims, without a timer or a product hook. Err is safe for concurrent helpers.
type checkedCancelContext struct {
	context.Context
	cancel context.CancelFunc
	checks atomic.Int32
	limit  int32
}

func (c *checkedCancelContext) Err() error {
	if c.checks.Add(1) >= c.limit {
		c.cancel()
	}
	return c.Context.Err()
}

func TestRecursiveGaussianReuseCancellationJoins(t *testing.T) {
	previous := parallel.SetBudget(2)
	defer parallel.SetBudget(previous)
	for _, api := range []string{"smooth", "gradient", "magnitude", "hessian", "laplacian"} {
		t.Run(api, func(t *testing.T) {
			src := reuseFixture([3]int{128, 64, 16})
			original := slices.Clone(src.Data)
			base, cancel := context.WithCancel(context.Background())
			defer cancel()
			ctx := &checkedCancelContext{Context: base, cancel: cancel, limit: 64}
			out, err := candidateReuseAPI(ctx, src, RecursiveGaussianOptions{SigmaMM: IsotropicSigma(2), Workers: 3}, api, [3]int{})
			if !errors.Is(err, context.Canceled) || out != nil || ctx.checks.Load() < ctx.limit {
				t.Fatalf("out=%v err=%v checks=%d", out, err, ctx.checks.Load())
			}
			reuseEqual(t, src.Data, original)
			if parallel.InUse() != 0 {
				t.Fatalf("canceled request retained %d helpers", parallel.InUse())
			}
		})
	}
	// A consuming pass may have written a private prefix before cancellation;
	// no partial node escapes and the sole backing returns exactly once.
	src := reuseFixture([3]int{128, 64, 16})
	p, err := newGaussianPlan(src, [][3]int{{}}, RecursiveGaussianOptions{SigmaMM: IsotropicSigma(2), Workers: 1})
	if err != nil {
		t.Fatal(err)
	}
	private := p.pool.get()
	copy(private.data, src.Data)
	base, cancel := context.WithCancel(context.Background())
	defer cancel()
	ctx := &checkedCancelContext{Context: base, cancel: cancel, limit: 64}
	got, err := p.pass(ctx, private, 2, 0, true)
	if !errors.Is(err, context.Canceled) || got.data != nil || len(p.pool.free) != 1 || &p.pool.free[0][0] != &private.data[0] {
		t.Fatalf("partial ownership escaped: node=%v err=%v free=%d", got.owned, err, len(p.pool.free))
	}
	if slices.Equal(private.data, src.Data) {
		t.Fatal("cancellation fixture never entered filtering work")
	}
}

func TestRecursiveGaussianReuseConcurrentRequests(t *testing.T) {
	previous := parallel.SetBudget(2)
	defer parallel.SetBudget(previous)
	shared := reuseFixture([3]int{33, 17, 9})
	original := slices.Clone(shared.Data)
	opts := RecursiveGaussianOptions{SigmaMM: IsotropicSigma(3), Workers: 3}
	const calls = 8
	sources := make([]*Volume, calls)
	want := make([][]*Volume, calls)
	for i := range sources {
		sources[i] = shared
		if i%2 != 0 {
			sources[i] = shared.Clone()
			for j := range sources[i].Data {
				sources[i].Data[j] += float32(i)
			}
		}
		var err error
		want[i], err = legacyReuseAPI(context.Background(), sources[i], opts, "hessian", [3]int{})
		if err != nil {
			t.Fatal(err)
		}
	}
	var wg sync.WaitGroup
	got := make([][]*Volume, calls)
	errs := make([]error, calls)
	parallel.ResetPeak()
	for i := range sources {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			got[i], errs[i] = candidateReuseAPI(context.Background(), sources[i], opts, "hessian", [3]int{})
		}(i)
	}
	wg.Wait()
	for i := range got {
		if errs[i] != nil {
			t.Fatal(errs[i])
		}
		for j := range got[i] {
			reuseEqual(t, got[i][j].Data, want[i][j].Data)
		}
	}
	reuseEqual(t, shared.Data, original)
	if parallel.Peak() > 2 || parallel.InUse() != 0 {
		t.Fatalf("shared budget exceeded: peak=%d inUse=%d", parallel.Peak(), parallel.InUse())
	}
}

func legacyRunRecursiveGaussian(ctx context.Context, src *Volume, specs [][3]int, opts RecursiveGaussianOptions,
	emit func(p *gaussianPlan, i int, data []float32) error, finish ...func(p *gaussianPlan) error,
) error {
	if ctx == nil {
		ctx = context.Background()
	}
	p, err := newGaussianPlan(src, specs, opts)
	if err != nil {
		return err
	}
	p.ctx = ctx
	if err := ctx.Err(); err != nil {
		return err
	}
	done := make([]bool, len(specs))
	for i, spec := range specs {
		if done[i] {
			continue
		}
		if p.isZero(spec) {
			done[i] = true
			if err := emit(p, i, make([]float32, p.n)); err != nil {
				return err
			}
			continue
		}
		z, err := p.legacyPass(ctx, node{data: src.Data}, 2, spec[2])
		if err != nil {
			return err
		}
		for j := i; j < len(specs); j++ {
			if done[j] || specs[j][2] != spec[2] || p.isZero(specs[j]) {
				continue
			}
			y, err := p.legacyPass(ctx, z, 1, specs[j][1])
			if err != nil {
				return err
			}
			if y.owned && !p.needsAgain(specs, done, j) {
				// The last y pass from z: its buffer can serve an x pass.
				p.release(z)
				z = node{}
			}
			for k := j; k < len(specs); k++ {
				if done[k] || specs[k][2] != spec[2] || specs[k][1] != specs[j][1] || p.isZero(specs[k]) {
					continue
				}
				x, err := p.legacyPass(ctx, y, 0, specs[k][0])
				if err != nil {
					return err
				}
				if !x.owned {
					buf := p.pool.get()
					copy(buf.data, x.data)
					x = buf
				}
				done[k] = true
				if err := emit(p, k, x.data); err != nil {
					return err
				}
			}
			p.release(y)
		}
		p.release(z)
	}
	for _, f := range finish {
		if err := f(p); err != nil {
			return err
		}
	}
	return ctx.Err()
}

func (p *gaussianPlan) legacyPass(ctx context.Context, in node, axis, order int) (node, error) {
	k := p.kernels[axis][order]
	if k == nil {
		return node{data: in.data}, nil
	}
	out := p.pool.get()
	dims := p.src.Dims
	nx, ny, nz := dims[0], dims[1], dims[2]
	var (
		length, step, laneStride, lanes int
		groups, groupStride             int
	)
	// Lines along the axis are filtered laneWidth at a time, so the
	// independent recursions interleave and the memory accesses of each step
	// stay within a few cache lines.
	switch axis {
	case 0: // lanes are consecutive rows
		length, step, laneStride, lanes = nx, 1, nx, ny*nz
		groups, groupStride = 1, 0
	case 1: // lanes are consecutive columns of one slice
		length, step, laneStride, lanes = ny, nx, 1, nx
		groups, groupStride = nz, nx*ny
	default: // lanes are consecutive voxels of a slice
		length, step, laneStride, lanes = nz, nx*ny, 1, nx*ny
		groups, groupStride = 1, 0
	}
	chunks := (lanes + laneWidth - 1) / laneWidth
	err := runParallel(ctx, groups*chunks, p.workers, func() func(int) {
		tmp := make([]float64, length*laneWidth)
		state := make([]float64, 4*laneWidth)
		return func(item int) {
			g, c := item/chunks, item%chunks
			first := c * laneWidth
			count := min(laneWidth, lanes-first)
			base := g*groupStride + first*laneStride
			k.legacyFilterLanes(out.data, in.data, base, step, laneStride, length, count, tmp, state)
		}
	})
	if err != nil {
		p.pool.put(out.data)
		return node{}, err
	}
	return out, nil
}

func (k *recursiveKernel) legacyFilterLanes(dst, src []float32, base, step, laneStride, length, count int, tmp, state []float64) {
	k.legacyFilterLanesWindow(dst, laneWindow{base: base, step: step, laneStride: laneStride, lo: 0, hi: length}, src, base, step, laneStride, length, count, tmp, state)
}

func (k *recursiveKernel) legacyFilterLanesWindow(dst []float32, w laneWindow, src []float32, base, step, laneStride, length, count int, tmp, state []float64) {
	z1r, z1i, z2r, z2i := real(k.z[0]), imag(k.z[0]), real(k.z[1]), imag(k.z[1])
	c1r, c1i, c2r, c2i := real(k.c[0]), imag(k.c[0]), real(k.c[1]), imag(k.c[1])
	st := state[:4*count]
	lo, hi := w.lo, w.hi
	tmp = tmp[:(hi-lo)*count]

	// Causal pass: y_i[j] = c_i x[j] + z_i y_i[j−1], from the steady state
	// of the first sample replicated before the line.
	for l := 0; l < count; l++ {
		x := complex(float64(src[base+l*laneStride]), 0)
		y1, y2 := k.cInit[0]*x, k.cInit[1]*x
		st[4*l], st[4*l+1], st[4*l+2], st[4*l+3] = real(y1), imag(y1), real(y2), imag(y2)
	}
	for j := 0; j < hi; j++ {
		row := base + j*step
		if j < lo {
			for l := 0; l < count; l++ {
				x := float64(src[row+l*laneStride])
				s := st[4*l : 4*l+4 : 4*l+4]
				r1 := c1r*x + z1r*s[0] - z1i*s[1]
				i1 := c1i*x + z1r*s[1] + z1i*s[0]
				r2 := c2r*x + z2r*s[2] - z2i*s[3]
				i2 := c2i*x + z2r*s[3] + z2i*s[2]
				s[0], s[1], s[2], s[3] = r1, i1, r2, i2
			}
			continue
		}
		out := tmp[(j-lo)*count : (j-lo+1)*count]
		for l := range out {
			x := float64(src[row+l*laneStride])
			s := st[4*l : 4*l+4 : 4*l+4]
			r1 := c1r*x + z1r*s[0] - z1i*s[1]
			i1 := c1i*x + z1r*s[1] + z1i*s[0]
			r2 := c2r*x + z2r*s[2] - z2i*s[3]
			i2 := c2i*x + z2r*s[3] + z2i*s[2]
			s[0], s[1], s[2], s[3] = r1, i1, r2, i2
			out[l] = r1 + r2
		}
	}

	// Anticausal pass: w_i[j] = z_i (d_i x[j+1] + w_i[j+1]), from the steady
	// state of the last sample replicated after the line.
	d1r, d1i, d2r, d2i := real(k.d[0]), imag(k.d[0]), real(k.d[1]), imag(k.d[1])
	last := base + (length-1)*step
	for l := 0; l < count; l++ {
		x := complex(float64(src[last+l*laneStride]), 0)
		w1, w2 := k.dInit[0]*x, k.dInit[1]*x
		st[4*l], st[4*l+1], st[4*l+2], st[4*l+3] = real(w1), imag(w1), real(w2), imag(w2)
		if length-1 < hi {
			dst[w.base+l*w.laneStride+(length-1-lo)*w.step] = float32(tmp[(length-1-lo)*count+l] + real(w1) + real(w2))
		}
	}
	for j := length - 2; j >= lo; j-- {
		next := base + (j+1)*step
		if j >= hi {
			for l := 0; l < count; l++ {
				x := float64(src[next+l*laneStride])
				s := st[4*l : 4*l+4 : 4*l+4]
				u1r, u1i := d1r*x+s[0], d1i*x+s[1]
				u2r, u2i := d2r*x+s[2], d2i*x+s[3]
				s[0], s[1] = z1r*u1r-z1i*u1i, z1r*u1i+z1i*u1r
				s[2], s[3] = z2r*u2r-z2i*u2i, z2r*u2i+z2i*u2r
			}
			continue
		}
		row := w.base + (j-lo)*w.step
		in := tmp[(j-lo)*count : (j-lo+1)*count]
		for l := range in {
			x := float64(src[next+l*laneStride])
			s := st[4*l : 4*l+4 : 4*l+4]
			u1r, u1i := d1r*x+s[0], d1i*x+s[1]
			u2r, u2i := d2r*x+s[2], d2i*x+s[3]
			r1, i1 := z1r*u1r-z1i*u1i, z1r*u1i+z1i*u1r
			r2, i2 := z2r*u2r-z2i*u2i, z2r*u2i+z2i*u2r
			s[0], s[1], s[2], s[3] = r1, i1, r2, i2
			dst[row+l*w.laneStride] = float32(in[l] + r1 + r2)
		}
	}
}
