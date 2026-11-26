package roi

import (
	"context"
	"errors"
	"math"
	"reflect"
	"sync"
	"testing"
)

type cvFixture struct {
	v      LevelSetVolume
	values []float64
	truth  []bool
	init   map[int]*RasterMask
}

func cvFixture1436(nx, ny, nz int, h [3]float64, large, noise, face bool) cvFixture {
	f := cvFixture{values: make([]float64, nx*ny*nz), truth: make([]bool, nx*ny*nz), init: make(map[int]*RasterMask)}
	cx, cy, cz := float64(nx-1)*h[0]/2, float64(ny-1)*h[1]/2, float64(nz-1)*h[2]/2
	if face {
		cx = 0
	}
	radius := math.Min(float64(nx)*h[0], float64(ny)*h[1]) * .23
	ir := radius * .6
	if large {
		ir = radius * 1.4
	}
	for z := 0; z < nz; z++ {
		m := NewRasterMask(nx, ny)
		for y := 0; y < ny; y++ {
			for x := 0; x < nx; x++ {
				i := (z*ny+y)*nx + x
				d := (float64(x)*h[0]-cx)*(float64(x)*h[0]-cx) + (float64(y)*h[1]-cy)*(float64(y)*h[1]-cy)
				if nz > 1 {
					d += (float64(z)*h[2] - cz) * (float64(z)*h[2] - cz)
				}
				f.truth[i] = d <= radius*radius
				f.values[i] = 10
				if f.truth[i] {
					f.values[i] = 11
				}
				if noise {
					f.values[i] += .06 * math.Sin(float64(i)*2.17)
				}
				if d <= ir*ir {
					m.Set(x, y, true)
				}
			}
		}
		f.init[z] = m
	}
	f.v = LevelSetVolume{Columns: nx, Rows: ny, Slices: nz, SpacingMM: h, ValueAt: func(x, y, z int) (float64, bool) { return f.values[(z*ny+y)*nx+x], true }}
	return f
}
func cvDice1436(f cvFixture, r *ChanVeseResult) float64 {
	a, b, c := 0, 0, 0
	for z := 0; z < f.v.Slices; z++ {
		for y := 0; y < f.v.Rows; y++ {
			for x := 0; x < f.v.Columns; x++ {
				i := (z*f.v.Rows+y)*f.v.Columns + x
				got := r.Masks[z] != nil && r.Masks[z].Get(x, y)
				if got {
					a++
				}
				if f.truth[i] {
					b++
				}
				if got && f.truth[i] {
					c++
				}
			}
		}
	}
	if a+b == 0 {
		return 1
	}
	return 2 * float64(c) / float64(a+b)
}
func TestChanVeseRegionalPhantoms(t *testing.T) {
	for _, p := range []struct {
		name        string
		nz          int
		h           [3]float64
		noise, face bool
	}{{"uniform", 1, [3]float64{1, 1, 0}, false, false}, {"noise", 1, [3]float64{1, 1, 0}, true, false}, {"anisotropic", 7, [3]float64{.8, 1.2, 2}, false, false}, {"face", 1, [3]float64{1, 1, 0}, false, true}} {
		for _, large := range []bool{false, true} {
			t.Run(p.name+map[bool]string{false: "-internal", true: "-external"}[large], func(t *testing.T) {
				f := cvFixture1436(20, 20, p.nz, p.h, large, p.noise, p.face)
				before := append([]float64(nil), f.values...)
				masks := make(map[int]*RasterMask)
				for z, m := range f.init {
					masks[z] = m.Clone()
				}
				o := DefaultChanVeseOptions()
				o.Mu = .005
				o.MaxIterations = 600
				o.MaximumRMSChangeMM = 1e-6
				r, e := ChanVese(nil, f.v, f.init, o)
				if e != nil {
					t.Fatal(e)
				}
				if d := cvDice1436(f, r); d < .95 {
					t.Fatalf("Dice %g < preset .95, stop=%s iterations=%d", d, r.StopReason, r.Iterations)
				}
				if !reflect.DeepEqual(before, f.values) || !reflect.DeepEqual(masks, f.init) {
					t.Fatal("source mutation")
				}
				if p.face && !r.TouchesVolumeBoundary {
					t.Fatal("face contact absent")
				}
			})
		}
	}
}
func TestChanVeseAdmissionAndCrop(t *testing.T) {
	for _, kind := range []string{"inside", "outside", "nan", "inf", "grid", "voxel", "bytes", "work", "parameter", "crop"} {
		t.Run(kind, func(t *testing.T) {
			f := cvFixture1436(12, 12, 1, [3]float64{1, 1, 0}, false, false, false)
			o := DefaultChanVeseOptions()
			switch kind {
			case "inside":
				f.init = map[int]*RasterMask{}
			case "outside":
				m := NewRasterMask(12, 12)
				for y := 0; y < 12; y++ {
					m.SetRun(y, 0, 12)
				}
				f.init = map[int]*RasterMask{0: m}
			case "nan":
				f.values[0] = math.NaN()
			case "inf":
				f.values[0] = math.Inf(1)
			case "grid":
				f.init[0] = NewRasterMask(2, 2)
			case "voxel":
				o.Limits.MaxVoxels = 1
			case "bytes":
				o.Limits.MaxBytes = 1
			case "work":
				o.MaxWork = 1
			case "parameter":
				o.Mu = -1
			case "crop":
				o.RegionOrigin = [3]int{11, 0, 0}
				o.RegionSize = [3]int{2, 12, 1}
			}
			r, e := ChanVese(nil, f.v, f.init, o)
			if e == nil || r != nil {
				t.Fatal(kind, r, e)
			}
		})
	}
	f := cvFixture1436(12, 12, 1, [3]float64{1, 1, 0}, false, false, false)
	reads := 0
	old := f.v.ValueAt
	f.v.ValueAt = func(x, y, z int) (float64, bool) {
		reads++
		if x < 2 || x >= 10 || y < 2 || y >= 10 {
			t.Fatal("outside crop read")
		}
		return old(x, y, z)
	}
	o := DefaultChanVeseOptions()
	o.RegionOrigin = [3]int{2, 2, 0}
	o.RegionSize = [3]int{8, 8, 1}
	o.MaxIterations = 1
	r, e := ChanVese(nil, f.v, f.init, o)
	if e != nil || reads != 64 || r.RegionOrigin != o.RegionOrigin {
		t.Fatal(r, e, reads)
	}
}
func TestChanVeseUnavailableAndStops(t *testing.T) {
	f := cvFixture1436(12, 12, 1, [3]float64{1, 1, 0}, false, false, false)
	old := f.v.ValueAt
	f.v.ValueAt = func(x, y, z int) (float64, bool) {
		if x == 0 {
			return math.NaN(), false
		}
		return old(x, y, z)
	}
	o := DefaultChanVeseOptions()
	o.MaxIterations = 2
	r, e := ChanVese(nil, f.v, f.init, o)
	if e != nil || r.Iterations != 2 || r.StopReason != "iterations" || len(r.History) != 3 {
		t.Fatal(r, e)
	}
	for y := 0; y < 12; y++ {
		if r.Masks[0].Get(0, y) {
			t.Fatal("unavailable inside")
		}
	}
	o.Mu = 0
	o.Nu = 0
	o.MaxIterations = 20
	for _, constant := range []float64{5, -5, math.MaxFloat32, -math.MaxFloat32, math.SmallestNonzeroFloat32} {
		f.v.ValueAt = func(x, y, z int) (float64, bool) { return constant, true }
		r, e = ChanVese(nil, f.v, f.init, o)
		if e != nil || !r.Converged || r.StopReason != "rms" || r.Iterations != 1 {
			t.Fatal(r, e)
		}
		// Independent exact invariant: identical samples have each regional
		// mean equal to that literal value, zero L2 energy and zero force.
		for _, h := range r.History {
			if h.Energy != 0 || h.InsideMean != constant || h.OutsideMean != constant || h.RMSChangeMM != 0 {
				t.Fatal("constant region energy/mean", constant, h)
			}
		}
	}

}
func TestChanVeseCancellationAndConcurrentOwnership(t *testing.T) {
	for _, phase := range []string{"pre", "prepare", "evolve"} {
		t.Run(phase, func(t *testing.T) {
			f := cvFixture1436(12, 12, 1, [3]float64{1, 1, 0}, false, false, false)
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			o := DefaultChanVeseOptions()
			switch phase {
			case "pre":
				cancel()
			case "prepare":
				old := f.v.ValueAt
				f.v.ValueAt = func(x, y, z int) (float64, bool) { cancel(); return old(x, y, z) }
			case "evolve":
				o.Progress = func(ChanVeseIteration) { cancel() }
			}
			r, e := ChanVese(ctx, f.v, f.init, o)
			if r != nil || !errors.Is(e, context.Canceled) || !errors.Is(e, ErrLevelSetCancelled) {
				t.Fatal(r, e)
			}
		})
	}
	f := cvFixture1436(12, 12, 1, [3]float64{1, 1, 0}, false, false, false)
	o := DefaultChanVeseOptions()
	o.MaxIterations = 3
	want, e := ChanVese(nil, f.v, f.init, o)
	if e != nil {
		t.Fatal(e)
	}
	var wg sync.WaitGroup
	for k := 0; k < 4; k++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			got, e := ChanVese(nil, f.v, f.init, o)
			if e != nil || !reflect.DeepEqual(got, want) {
				t.Error("concurrent/determinism", e)
			}
		}()
	}
	wg.Wait()
	f.init[0].Set(0, 0, true)
	f.values[0] = 50
	if want.Masks[0].Get(0, 0) {
		t.Fatal("result aliases initial")
	}
}

// Counts checks in a complete deterministic call, then cancels at its last
// publication check. No worker scheduler or implementation staging is replaced.
type cvCountingContext1436 struct {
	context.Context
	calls, limit int
}

func (c *cvCountingContext1436) Err() error {
	c.calls++
	if c.limit > 0 && c.calls >= c.limit {
		return context.Canceled
	}
	return nil
}
func TestChanVeseFinalPublicationCancellation(t *testing.T) {
	f := cvFixture1436(12, 12, 1, [3]float64{1, 1, 0}, false, false, false)
	o := DefaultChanVeseOptions()
	o.MaxIterations = 1
	c := &cvCountingContext1436{Context: context.Background()}
	r, e := ChanVese(c, f.v, f.init, o)
	if e != nil || r == nil {
		t.Fatal(e)
	}
	final := &cvCountingContext1436{Context: context.Background(), limit: c.calls}
	r, e = ChanVese(final, f.v, f.init, o)
	if r != nil || !errors.Is(e, context.Canceled) {
		t.Fatal("late canceled publication", r, e)
	}
}

func TestChanVeseFragmentedMaskAdmissionAndOutput(t *testing.T) {
	const n = 4096
	mask := NewRasterMask(n, 1)
	for x := 0; x < n; x += 2 {
		mask.runs[0] = append(mask.runs[0], MaskRun{Start: x, End: x + 1})
	}
	before := append([]MaskRun(nil), mask.runs[0]...)
	reads := 0
	v := LevelSetVolume{Columns: n, Rows: 1, Slices: 1, SpacingMM: [3]float64{1, 1, 0}, ValueAt: func(x, y, z int) (float64, bool) { reads++; return float64(1 - x%2), true }}
	o := DefaultChanVeseOptions()
	o.Mu = 0
	o.MaxIterations = 1
	got, err := ChanVese(nil, v, map[int]*RasterMask{0: mask}, o)
	if err != nil || got == nil || !reflect.DeepEqual(got.Masks[0].runs[0], before) || !reflect.DeepEqual(mask.runs[0], before) {
		t.Fatal("linear fragmented ownership", err)
	}
	reads = 0
	o.MaxWork = uint64(8 * n * 2)
	got, err = ChanVese(nil, v, map[int]*RasterMask{0: mask}, o)
	if got != nil || !errors.Is(err, ErrLevelSetResourceLimit) || reads != 0 {
		t.Fatal("run work before sampling", err, reads)
	}
	o.MaxWork = 0
	for _, bad := range []*RasterMask{{Columns: n, Rows: 1}, {Columns: n, Rows: 1, runs: [][]MaskRun{{{Start: -1, End: 1}}}}, {Columns: n, Rows: 1, runs: [][]MaskRun{{{Start: 2, End: n + 1}}}}} {
		got, err = ChanVese(nil, v, map[int]*RasterMask{0: bad}, o)
		if got != nil || !errors.Is(err, ErrLevelSetInvalidInput) {
			t.Fatal("malformed storage must fail", err)
		}
	}
	counter := &cvCountingContext1436{Context: context.Background()}
	got, err = ChanVese(counter, v, map[int]*RasterMask{0: mask}, o)
	if err != nil || got == nil || counter.calls < 20 {
		t.Fatal(err, counter.calls)
	}
	// Last eight checks lie inside publication of this long single row;
	// cancellation is observed before exposing a partially appended result.
	cancel := &cvCountingContext1436{Context: context.Background(), limit: counter.calls - 8}
	got, err = ChanVese(cancel, v, map[int]*RasterMask{0: mask}, o)
	if got != nil || !errors.Is(err, context.Canceled) {
		t.Fatal("mid-output cancellation", err)
	}
}
