package imagefilter

import (
	"context"
	"encoding/json"
	"errors"
	"math"
	"os"
	"reflect"
	"sync"
	"testing"
)

func TestCannyIndependentSuppressionAndHysteresis(t *testing.T) {
	dims := [3]int{5, 3, 1}
	m := []float64{0, 1, 4, 4, 1, 0, 1, 4, 4, 1, 0, 1, 4, 4, 1}
	var g [3]*Volume
	for a := range g {
		g[a] = &Volume{Data: make([]float32, 15)}
	}
	for i := range m {
		g[0].Data[i] = 1
	}
	p, err := cannySuppress(context.Background(), dims, [3]float64{1, 1, 0}, g, m)
	if err != nil {
		t.Fatal(err)
	}
	// >= at the negative probe, > at the positive probe chooses x=3 on
	// this exact plateau; calculation is independent of Gaussian output.
	for i, v := range p {
		want := float32(0)
		if i%5 == 3 {
			want = 4
		}
		if v != want {
			t.Fatalf("peak %d=%g want %g", i, v, want)
		}
	}
	d := [3]int{5, 5, 1}
	peaks := make([]float32, 25)
	peaks[0] = 10
	peaks[6] = 3
	peaks[12] = 3
	peaks[23] = 3
	e, n, err := cannyLink(context.Background(), d, peaks, 3, 10)
	if err != nil {
		t.Fatal(err)
	}
	if n != 3 || e[12] != 1 || e[23] != 0 {
		t.Fatalf("weak diagonal chain vs isolated: %v (%d)", e, n)
	}
	d3 := [3]int{3, 3, 3}
	q := make([]float32, 27)
	q[0] = 8
	q[13] = 4
	q[26] = 4
	e, n, err = cannyLink(context.Background(), d3, q, 4, 8)
	if err != nil || n != 3 || e[26] != 1 {
		t.Fatalf("26 connectivity: %v %d %v", e, n, err)
	}
}

func TestCannyErrorsLimitsOwnership(t *testing.T) {
	src := &Volume{Dims: [3]int{9, 7, 1}, SpacingMM: [3]float64{.7, 1.4, 0}, Data: make([]float32, 63)}
	for i := range src.Data {
		if i%9 >= 4 {
			src.Data[i] = 100
		}
	}
	before := append([]float32(nil), src.Data...)
	result, err := Canny(nil, src, CannyOptions{})
	if err != nil || result.EdgeCount == 0 {
		t.Fatalf("%v %+v", err, result)
	}
	result.Edges.Data[0] = 99
	result.Strength.Data[1] = 99
	if !reflect.DeepEqual(src.Data, before) {
		t.Fatal("source aliased")
	}
	if result.Strength.Data[0] == 99 || result.Edges.Data[1] == 99 {
		t.Fatal("output arrays aliased")
	}
	bad := []CannyOptions{{Workers: -1}, {CannyParameters: CannyParameters{SigmaMM: [3]float64{1, 0, 0}}}, {CannyParameters: CannyParameters{LowerThreshold: 2, UpperThreshold: 1}}, {CannyParameters: CannyParameters{UpperThreshold: math.NaN()}}, {Limits: CannyLimits{MaxVoxels: -1}}, {Limits: CannyLimits{MaxBytes: 513 << 20}}, {Limits: CannyLimits{MaxVoxels: 1}}, {Limits: CannyLimits{MaxBytes: 1}}}
	for _, o := range bad {
		r, e := Canny(nil, src, o)
		if r != nil || e == nil {
			t.Fatalf("accepted invalid %+v", o)
		}
	}
	for _, change := range []func(*Volume){func(v *Volume) { v.Data = v.Data[:1] }, func(v *Volume) { v.Data[0] = float32(math.Inf(1)) }, func(v *Volume) { v.SpacingMM[0] = 0 }, func(v *Volume) { v.SpacingMM[1] = math.Inf(1) }, func(v *Volume) { v.Dims = [3]int{math.MaxInt, 2, 2} }} {
		v := src.Clone()
		change(v)
		r, e := Canny(nil, v, CannyOptions{})
		if e == nil || r != nil {
			t.Fatal("invalid input accepted")
		}
	}
	if r, e := Canny(nil, nil, CannyOptions{}); r != nil || e == nil {
		t.Fatal("nil accepted")
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if r, e := Canny(ctx, src, CannyOptions{}); r != nil || !errors.Is(e, context.Canceled) {
		t.Fatalf("cancel: %v", e)
	}
	minimal := &Volume{Dims: [3]int{2, 2, 1}, SpacingMM: [3]float64{1, 1, 0}, Data: []float32{0, 1, 0, 1}}
	if _, e := Canny(nil, minimal, CannyOptions{}); e != nil {
		t.Fatal(e)
	}
}

func TestCannyElongatedScratchAndConcurrent(t *testing.T) {
	// One long active worker and several short ones; do not charge a long
	// causal array to every worker as the pre-#1305 allocation did.
	b, e := CannyGaussianScratchBytes([3]int{65537, 3, 3}, 2)
	if e != nil {
		t.Fatal(e)
	}
	want := int64(65537*9*8 + 96*8 + 2*(4*32*8+128))
	if b != want {
		t.Fatalf("retained scratch %d want %d", b, want)
	}
	src := &Volume{Dims: [3]int{23, 19, 1}, SpacingMM: [3]float64{1, 1, 0}, Data: make([]float32, 23*19)}
	for i := range src.Data {
		src.Data[i] = float32(i % 23)
	}
	baseline, e := Canny(nil, src, CannyOptions{Workers: 1})
	if e != nil {
		t.Fatal(e)
	}
	var wg sync.WaitGroup
	for j := 0; j < 4; j++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			r, e := Canny(nil, src, CannyOptions{Workers: 2})
			if e != nil {
				t.Error(e)
				return
			}
			if !reflect.DeepEqual(r.Edges.Data, baseline.Edges.Data) {
				t.Error("worker dependence")
			}
		}()
	}
	wg.Wait()
}

func TestCannyFrozenITKGeometry(t *testing.T) {
	b, e := os.ReadFile("testdata/canny/itk-5.4.json")
	if e != nil {
		t.Fatal(e)
	}
	var doc struct {
		Cases []struct {
			Name           string
			Dims           [3]int
			Spacing, Sigma [3]float64
			Lower, Upper   float64
			Data           []float32
			Edges          []int
		}
	}
	if e = json.Unmarshal(b, &doc); e != nil {
		t.Fatal(e)
	}
	if len(doc.Cases) != 10 {
		t.Fatalf("corpus count %d", len(doc.Cases))
	}
	for _, c := range doc.Cases {
		t.Run(c.Name, func(t *testing.T) {
			src := &Volume{Dims: c.Dims, SpacingMM: c.Spacing, Data: c.Data}
			r, e := Canny(nil, src, CannyOptions{CannyParameters: CannyParameters{SigmaMM: c.Sigma, LowerThreshold: c.Lower, UpperThreshold: c.Upper}})
			if e != nil {
				t.Fatal(e)
			}
			want := make([]bool, len(c.Edges))
			got := make([]bool, len(c.Edges))
			for i, v := range c.Edges {
				want[i] = v != 0
				got[i] = r.Edges.Data[i] != 0
			}
			wc, gc := cannyComponents(want, c.Dims), cannyComponents(got, c.Dims)
			if wc != gc {
				t.Errorf("components Go=%d ITK=%d", gc, wc)
			}
			maxH := math.Max(c.Spacing[0], c.Spacing[1])
			if c.Dims[2] > 1 {
				maxH = math.Max(maxH, c.Spacing[2])
			}
			// Independent brute-force positions, no production EDT, preregistered
			// one-maximum-spacing tolerance in each direction.
			for direction, pair := range [][2][]bool{{got, want}, {want, got}} {
				for i, on := range pair[0] {
					if !on {
						continue
					}
					nearest := math.Inf(1)
					x, y, z := i%c.Dims[0], i/c.Dims[0]%c.Dims[1], i/(c.Dims[0]*c.Dims[1])
					for j, to := range pair[1] {
						if !to {
							continue
						}
						dx := float64(x-j%c.Dims[0]) * c.Spacing[0]
						dy := float64(y-j/c.Dims[0]%c.Dims[1]) * c.Spacing[1]
						dz := float64(z-j/(c.Dims[0]*c.Dims[1])) * c.Spacing[2]
						nearest = math.Min(nearest, math.Sqrt(dx*dx+dy*dy+dz*dz))
					}
					if nearest > maxH+1e-6 {
						t.Errorf("direction %d voxel %d distance %.5g > %.5g", direction, i, nearest, maxH)
						break
					}
				}
			}
			if c.Name == "constant" && r.EdgeCount != 0 {
				t.Fatal("constant not empty")
			}
		})
	}
}
func cannyComponents(on []bool, d [3]int) int {
	seen := make([]bool, len(on))
	count := 0
	for i, v := range on {
		if !v || seen[i] {
			continue
		}
		count++
		seen[i] = true
		q := []int{i}
		for head := 0; head < len(q); head++ {
			j := q[head]
			x, y, z := j%d[0], j/d[0]%d[1], j/(d[0]*d[1])
			for dz := -1; dz <= 1; dz++ {
				for dy := -1; dy <= 1; dy++ {
					for dx := -1; dx <= 1; dx++ {
						a, b, c := x+dx, y+dy, z+dz
						if a < 0 || b < 0 || c < 0 || a >= d[0] || b >= d[1] || c >= d[2] {
							continue
						}
						k := (c*d[1]+b)*d[0] + a
						if on[k] && !seen[k] {
							seen[k] = true
							q = append(q, k)
						}
					}
				}
			}
		}
	}
	return count
}

// Independent boundary examples: exact magnitudes, no Gaussian/oracle.
func TestCannySuppressionDistinctBoundaryProbes(t *testing.T) {
	for dimensions := 2; dimensions <= 3; dimensions++ {
		for _, h := range [][3]float64{{1, 1, 1}, {.7, 1.3, 2.1}} {
			for axis := 0; axis < dimensions; axis++ {
				for _, side := range []int{0, 1} {
					for _, sign := range []float32{-1, 1} {
						d := [3]int{3, 3, 3}
						if dimensions == 2 {
							d[2] = 1
						}
						n := d[0] * d[1] * d[2]
						m := make([]float64, n)
						var gradient [3]*Volume
						for a := range gradient {
							gradient[a] = &Volume{Data: make([]float32, n)}
						}
						for i := range m {
							p := [3]int{i % d[0], i / d[0] % d[1], i / (d[0] * d[1])}
							distance := p[axis]
							if side == 1 {
								distance = d[axis] - 1 - distance
							}
							m[i] = float64(3 - distance)
							gradient[axis].Data[i] = sign
						}
						peaks, e := cannySuppress(context.Background(), d, h, gradient, m)
						if e != nil {
							t.Fatal(e)
						}
						for i, v := range peaks {
							p := [3]int{i % d[0], i / d[0] % d[1], i / (d[0] * d[1])}
							boundary := 0
							if side == 1 {
								boundary = d[axis] - 1
							}
							want := float32(0)
							if p[axis] == boundary {
								want = 3
							}
							if v != want {
								t.Fatalf("dims%d axis%d side%d sign%g h%v voxel%d peak%g want%g", dimensions, axis, side, sign, h, i, v, want)
							}
						}
					}
				}
			}
		}
	}
	// Equal dominant components, all corners: isotropic normalized directions
	// are exact ties; tie selection remains deterministic for both signs.
	d := [3]int{3, 3, 1}
	for _, sx := range []int{-1, 1} {
		for _, sy := range []int{-1, 1} {
			for _, sign := range []float32{-1, 1} {
				m := make([]float64, 9)
				var g [3]*Volume
				for a := range g {
					g[a] = &Volume{Data: make([]float32, 9)}
				}
				corner := 0
				if sx < 0 {
					corner += 2
				}
				if sy < 0 {
					corner += 6
				}
				for i := range m {
					x, y := i%3, i/3
					cx, cy := corner%3, corner/3
					m[i] = float64(5 - absCanny(x-cx) - absCanny(y-cy))
					g[0].Data[i] = sign * float32(sx)
					g[1].Data[i] = sign * float32(sy)
				}
				p, e := cannySuppress(context.Background(), d, [3]float64{1, 1, 0}, g, m)
				if e != nil || p[corner] != 5 {
					t.Fatalf("corner%d signs%d/%d/%g: %v %v", corner, sx, sy, sign, p, e)
				}
			}
		}
	}
}
func absCanny(x int) int {
	if x < 0 {
		return -x
	}
	return x
}

func TestCannyConstantAndThreeDimensionalCorners(t *testing.T) {
	for _, v := range []float32{0, 10, -100} {
		source := &Volume{Dims: [3]int{5, 7, 3}, SpacingMM: [3]float64{.7, 1.3, 2.1}, Data: make([]float32, 105)}
		for i := range source.Data {
			source.Data[i] = v
		}
		r, e := Canny(nil, source, CannyOptions{})
		if e != nil || r.EdgeCount != 0 {
			t.Fatalf("constant%g: %v %+v", v, e, r)
		}
	}
	d := [3]int{3, 3, 3}
	for _, h := range [][3]float64{{1, 1, 1}, {.7, 1.3, 2.1}} {
		for corner := 0; corner < 8; corner++ {
			for _, sign := range []float32{-1, 1} {
				var c [3]int
				for a := range c {
					if corner&(1<<a) != 0 {
						c[a] = 2
					}
				}
				magnitude := make([]float64, 27)
				var g [3]*Volume
				for a := range g {
					g[a] = &Volume{Data: make([]float32, 27)}
				}
				for i := range magnitude {
					pos := [3]int{i % 3, i / 3 % 3, i / 9}
					magnitude[i] = 7
					for a := range pos {
						magnitude[i] -= float64(absCanny(pos[a] - c[a]))
						direction := sign
						if c[a] == 2 {
							direction = -sign
						}
						g[a].Data[i] = direction * float32(h[a])
					}
				}
				p, e := cannySuppress(context.Background(), d, h, g, magnitude)
				i := (c[2]*3+c[1])*3 + c[0]
				if e != nil || p[i] != 7 {
					t.Fatalf("corner%v sign%g h%v: peak%g %v", c, sign, h, p[i], e)
				}
			}
		}
	}
}

func TestCannyInternalCancellation(t *testing.T) {
	const n = 16384
	dims := [3]int{128, 128, 1}
	m := make([]float64, n)
	peaks := make([]float32, n)
	var gradient [3]*Volume
	for a := range gradient {
		gradient[a] = &Volume{Data: make([]float32, n)}
	}
	for i := range m {
		m[i] = 1
		gradient[0].Data[i] = 1
		peaks[i] = 1
	}
	for _, stage := range []string{"suppression", "hysteresis", "source-validation", "causal-line", "anticausal-line"} {
		t.Run(stage, func(t *testing.T) {
			base, cancel := context.WithCancel(context.Background())
			defer cancel()
			ctx := &checkedCancelContext{Context: base, cancel: cancel, limit: 2}
			switch stage {
			case "suppression":
				if r, e := cannySuppress(ctx, dims, [3]float64{1, 1, 0}, gradient, m); r != nil || !errors.Is(e, context.Canceled) {
					t.Fatalf("partial %v", e)
				}
			case "hysteresis":
				if r, _, e := cannyLink(ctx, dims, peaks, 0, 0); r != nil || !errors.Is(e, context.Canceled) {
					t.Fatalf("partial %v", e)
				}
			case "source-validation":
				if e := validateGaussianSource(ctx, &Volume{Dims: dims, SpacingMM: [3]float64{1, 1, 0}, Data: make([]float32, n)}); !errors.Is(e, context.Canceled) {
					t.Fatalf("validation %v", e)
				}
			default:
				// One lane spans enough samples to cancel inside either recurrence, not
				// just between Gaussian work items. Private partial arrays are discarded.
				const length = 4097
				data := make([]float32, length)
				out := make([]float32, length)
				for i := range data {
					data[i] = float32(i % 17)
					out[i] = -999
				}
				k, e := newRecursiveKernel(1, 0, 1)
				if e != nil {
					t.Fatal(e)
				}
				if stage == "anticausal-line" {
					ctx.limit = 6
				}
				k.filterLanesWindowContext(ctx, out, laneWindow{lo: 0, hi: length, base: 0, step: 1, laneStride: length}, data, 0, 1, length, length, 1, make([]float64, length), make([]float64, 4*laneWidth))
				if !errors.Is(ctx.Err(), context.Canceled) || ctx.checks.Load() < ctx.limit {
					t.Fatal("no recurrence poll")
				}
				if stage == "causal-line" && out[length-1] != -999 {
					t.Fatal("causal cancellation continued to output")
				}
			}
		})
	}
}
