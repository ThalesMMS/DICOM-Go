package render

import (
	"context"
	"errors"
	"math"
	"reflect"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// Global set union of closed cubical cells, independent of the local admission
// code. Components use full-neighbor BFS; cavities use padded face-neighbor BFS.
type topoOracleCell struct {
	P    [3]int
	Axes int
}

func topology1501Oracle(g FastMarchingGrid, mask []bool) (components, handles, cavities, chi int) {
	dim := 3
	if g.Nz == 1 {
		dim = 2
	}
	cells := make(map[topoOracleCell]bool)
	for i, v := range mask {
		if !v {
			continue
		}
		xyz := [3]int{i % g.Nx, i / g.Nx % g.Ny, i / (g.Nx * g.Ny)}
		for axes := 0; axes < (1 << dim); axes++ {
			for signs := 0; signs < (1 << dim); signs++ {
				key := topoOracleCell{Axes: axes}
				for a := 0; a < dim; a++ {
					key.P[a] = 2 * xyz[a]
					if axes&(1<<a) == 0 {
						if signs&(1<<a) == 0 {
							key.P[a]--
						} else {
							key.P[a]++
						}
					}
				}
				cells[key] = true
			}
		}
	}
	for key := range cells {
		degree := 0
		for a := 0; a < dim; a++ {
			if key.Axes&(1<<a) != 0 {
				degree++
			}
		}
		if degree%2 == 0 {
			chi++
		} else {
			chi--
		}
	}
	sizes := [3]int{g.Nx + 2, g.Ny + 2, g.Nz + 2}
	if dim == 2 {
		sizes[2] = 1
	}
	n := sizes[0] * sizes[1] * sizes[2]
	foreground := make([]bool, n)
	for i, v := range mask {
		if v {
			x, y, z := i%g.Nx, i/g.Nx%g.Ny, i/(g.Nx*g.Ny)
			if dim == 3 {
				z++
			}
			foreground[(z*sizes[1]+y+1)*sizes[0]+x+1] = true
		}
	}
	count := func(want, full bool) int {
		seen := make([]bool, n)
		c := 0
		queue := make([]int, 0, n)
		for start, v := range foreground {
			if v != want || seen[start] {
				continue
			}
			c++
			seen[start] = true
			queue = append(queue[:0], start)
			for head := 0; head < len(queue); head++ {
				i := queue[head]
				x, y, z := i%sizes[0], i/sizes[0]%sizes[1], i/(sizes[0]*sizes[1])
				zlo, zhi := -1, 1
				if dim == 2 {
					zlo, zhi = 0, 0
				}
				for dz := zlo; dz <= zhi; dz++ {
					for dy := -1; dy <= 1; dy++ {
						for dx := -1; dx <= 1; dx++ {
							distance := absFMOracle1501(dx) + absFMOracle1501(dy) + absFMOracle1501(dz)
							if distance == 0 || (!full && distance != 1) {
								continue
							}
							xx, yy, zz := x+dx, y+dy, z+dz
							if xx < 0 || xx >= sizes[0] || yy < 0 || yy >= sizes[1] || zz < 0 || zz >= sizes[2] {
								continue
							}
							j := (zz*sizes[1]+yy)*sizes[0] + xx
							if !seen[j] && foreground[j] == want {
								seen[j] = true
								queue = append(queue, j)
							}
						}
					}
				}
			}
		}
		return c
	}
	components = count(true, true)
	if dim == 3 {
		cavities = count(false, false) - 1
		handles = components + cavities - chi
	} else {
		handles = components - chi
	}
	return
}
func absFMOracle1501(x int) int {
	if x < 0 {
		return -x
	}
	return x
}
func topology1501Mask(r *FastMarchingResult) []bool {
	v := make([]bool, len(r.Times))
	for i, x := range r.Times {
		v[i] = !math.IsInf(x, 1)
	}
	return v
}
func topology1501Seeds(g FastMarchingGrid, s []FastMarchingSeed) []bool {
	v := make([]bool, len(g.Speed))
	for _, x := range s {
		v[(x.Index[2]*g.Ny+x.Index[1])*g.Nx+x.Index[0]] = true
	}
	return v
}
func TestFastMarchingTopologyOracleAnalytical(t *testing.T) {
	for _, nz := range []int{1, 7} {
		g := uniformFastMarchingGrid(7, 7, nz, Vec3{1, 1, 1}, 1)
		mask := make([]bool, len(g.Speed))
		for i := range mask {
			x, y, z := i%7, i/7%7, i/49
			if nz == 1 {
				mask[i] = x >= 1 && x <= 5 && y >= 1 && y <= 5 && (x == 1 || x == 5 || y == 1 || y == 5)
			} else {
				mask[i] = x >= 1 && x <= 5 && y >= 1 && y <= 5 && z >= 1 && z <= 5 && (x == 1 || x == 5 || y == 1 || y == 5 || z == 1 || z == 5)
			}
		}
		c, h, v, _ := topology1501Oracle(g, mask)
		if c != 1 || (nz == 1 && (h != 1 || v != 0)) || (nz > 1 && (h != 0 || v != 1)) {
			t.Fatalf("oracle nz%d: %d %d %d", nz, c, h, v)
		}
	}
}
func TestFastMarchingTopologyDisabledExact(t *testing.T) {
	for _, o := range []FastMarchingOptions{{}, {StoppingValue: 2}, {Targets: [][3]int{{4, 4, 0}}}, {MaxFrozen: 1}, {MaxTrial: 1}} {
		g := uniformFastMarchingGrid(7, 7, 1, Vec3{.5, 1, 2}, 1)
		s := []FastMarchingSeed{{Index: [3]int{1, 1, 0}}}
		want, e1 := FastMarching(context.Background(), g, s, o)
		got, e2 := FastMarchingConstrained(context.Background(), g, s, o, FastMarchingTopologyOptions{})
		if (e1 == nil) != (e2 == nil) {
			t.Fatal(e1, e2)
		}
		if e1 == nil && !reflect.DeepEqual(want, got.FastMarchingResult) {
			t.Fatal("disabled changed existing field/stop")
		}
		if e1 != nil && e1.Error() != e2.Error() {
			t.Fatal(e1, e2)
		}
	}
}
func TestFastMarchingTopologyBridgeModes(t *testing.T) {
	for _, nz := range []int{1, 3} {
		g := uniformFastMarchingGrid(7, 3, nz, Vec3{.5, 1, 2}, 0)
		z := 0
		if nz > 1 {
			z = 1
		}
		for x := 1; x <= 5; x++ {
			g.Speed[(z*3+1)*7+x] = 1
		}
		s := []FastMarchingSeed{{Index: [3]int{1, 1, z}}, {Index: [3]int{5, 1, z}}}
		snapshot := append([]float64(nil), g.Speed...)
		for _, mode := range []FastMarchingTopologyMode{FastMarchingTopologyStrict, FastMarchingTopologyNoHandles} {
			r, e := FastMarchingConstrained(context.Background(), g, s, FastMarchingOptions{}, FastMarchingTopologyOptions{Mode: mode})
			if e != nil {
				t.Fatal(e)
			}
			c, h, v, _ := topology1501Oracle(g, topology1501Mask(r.FastMarchingResult))
			want := 1
			if mode == FastMarchingTopologyStrict {
				want = 2
				if r.Report.RejectedMerge == 0 || r.Report.Complete || r.Report.AvailableUnreached == 0 {
					t.Fatal(r.Report)
				}
			} else if !r.Report.Complete {
				t.Fatal(r.Report)
			}
			if c != want || h != 0 || v != 0 || r.Report.FinalComponents != c {
				t.Fatalf("mode%d: c/h/v=%d/%d/%d %+v", mode, c, h, v, r.Report)
			}
			if !reflect.DeepEqual(snapshot, g.Speed) {
				t.Fatal("mutated source")
			}
		}
	}
}
func TestFastMarchingTopologyRingTunnelCavity(t *testing.T) {
	for _, kind := range []string{"ring", "tunnel", "cavity"} {
		nz := 1
		if kind != "ring" {
			nz = 7
		}
		g := uniformFastMarchingGrid(7, 7, nz, Vec3{.7, 1.1, 1.8}, 0)
		var seeds []FastMarchingSeed
		for i := range g.Speed {
			x, y, z := i%7, i/7%7, i/49
			if kind == "cavity" {
				if x >= 1 && x <= 5 && y >= 1 && y <= 5 && z >= 1 && z <= 5 {
					g.Speed[i] = 1
					if x == 1 || x == 5 || y == 1 || y == 5 || z == 1 || z == 5 {
						seeds = append(seeds, FastMarchingSeed{Index: [3]int{x, y, z}})
					}
				}
			} else if x >= 1 && x <= 5 && y >= 1 && y <= 5 && (x == 1 || x == 5 || y == 1 || y == 5) && (kind == "ring" || z >= 2 && z <= 4) {
				g.Speed[i] = 1
			}
		}
		if kind != "cavity" {
			z := 0
			if nz > 1 {
				z = 3
			}
			seeds = []FastMarchingSeed{{Index: [3]int{1, 1, z}}}
		}
		beforeC, beforeH, beforeV, _ := topology1501Oracle(g, topology1501Seeds(g, seeds))
		for _, mode := range []FastMarchingTopologyMode{FastMarchingTopologyStrict, FastMarchingTopologyNoHandles} {
			r, e := FastMarchingConstrained(context.Background(), g, seeds, FastMarchingOptions{}, FastMarchingTopologyOptions{Mode: mode})
			if e != nil {
				t.Fatal(e)
			}
			c, h, v, _ := topology1501Oracle(g, topology1501Mask(r.FastMarchingResult))
			if c != beforeC || h != beforeH || v != beforeV || r.Report.RejectedCandidates == 0 || r.Report.Complete {
				t.Fatalf("%s mode%d initial %d/%d/%d final%d/%d/%d %+v", kind, mode, beforeC, beforeH, beforeV, c, h, v, r.Report)
			}
			t.Logf("%s mode%d components/handles/cavities%d/%d/%d rejected%d unreached%d", kind, mode, c, h, v, r.Report.RejectedCandidates, r.Report.AvailableUnreached)
		}
		if kind != "cavity" {
			plain, e := FastMarching(context.Background(), g, seeds, FastMarchingOptions{})
			if e != nil {
				t.Fatal(e)
			}
			_, h, _, _ := topology1501Oracle(g, topology1501Mask(plain))
			if h < 1 {
				t.Fatal("unrestricted fixture did not form handle")
			}
		}
	}
}
func TestFastMarchingTopologyEulerIndependent(t *testing.T) {
	// Exhaust all 256 punctured XY neighborhoods; local delta must match
	// a global independent closed-cell set, including diagonal contacts.
	g := uniformFastMarchingGrid(5, 5, 1, Vec3{1, 1, 1}, 1)
	for bits := 0; bits < 256; bits++ {
		c := &fmTopology{nx: 5, ny: 5, nz: 1, dim: 2, parent: make([]int32, 25)}
		for i := range c.parent {
			c.parent[i] = -1
		}
		mask := make([]bool, 25)
		k := 0
		for y := 1; y <= 3; y++ {
			for x := 1; x <= 3; x++ {
				if x == 2 && y == 2 {
					continue
				}
				if bits&(1<<k) != 0 {
					mask[y*5+x] = true
					c.parent[y*5+x] = int32(y*5 + x)
				}
				k++
			}
		}
		_, _, _, before := topology1501Oracle(g, mask)
		mask[12] = true
		_, _, _, after := topology1501Oracle(g, mask)
		if got := c.eulerDelta(2, 2, 0); got != after-before {
			t.Fatalf("bits%d delta%d want%d", bits, got, after-before)
		}
	}
}
func TestFastMarchingTopologyLimitsSeedsAndCancel(t *testing.T) {
	g := uniformFastMarchingGrid(9, 9, 3, Vec3{.6, 1, 2}, 1)
	s := []FastMarchingSeed{{Index: [3]int{4, 4, 1}}}
	for _, o := range []FastMarchingTopologyOptions{{Mode: FastMarchingTopologyMode(99)}, {Mode: FastMarchingTopologyStrict, MaxBytes: 1}, {Mode: FastMarchingTopologyStrict, MaxWork: 1}, {Mode: FastMarchingTopologyStrict, MaxWork: 3000}} {
		r, e := FastMarchingConstrained(context.Background(), g, s, FastMarchingOptions{}, o)
		if e == nil || r != nil {
			t.Fatal("limit/mode accepted", o)
		}
	}
	for _, o := range []FastMarchingOptions{{MaxFrozen: 2}, {MaxTrial: 1}} {
		r, e := FastMarchingConstrained(context.Background(), g, s, o, FastMarchingTopologyOptions{Mode: FastMarchingTopologyStrict})
		var diagnostic *FastMarchingTopologyFailure
		var old *FastMarchingLimitError
		if r != nil || !errors.As(e, &diagnostic) || !errors.As(e, &old) || diagnostic.Report.StopReason != "budget" {
			t.Fatal(r, e)
		}
	}
	for _, bad := range [][]FastMarchingSeed{{{Index: [3]int{-1, 0, 0}}}, {{Index: [3]int{4, 4, 1}}, {Index: [3]int{5, 4, 1}, Value: 1}}} {
		r, e := FastMarchingConstrained(context.Background(), g, bad, FastMarchingOptions{}, FastMarchingTopologyOptions{Mode: FastMarchingTopologyStrict})
		if r != nil || !errors.Is(e, ErrFastMarchingInput) {
			t.Fatal(r, e)
		}
	}
	g.Speed[(1*9+4)*9+4] = 0
	if r, e := FastMarchingConstrained(context.Background(), g, s, FastMarchingOptions{}, FastMarchingTopologyOptions{Mode: FastMarchingTopologyStrict}); r != nil || !errors.Is(e, ErrFastMarchingInput) {
		t.Fatal(r, e)
	}
	g.Speed[(1*9+4)*9+4] = 1
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if r, e := FastMarchingConstrained(ctx, g, s, FastMarchingOptions{}, FastMarchingTopologyOptions{Mode: FastMarchingTopologyStrict}); r != nil || !errors.Is(e, context.Canceled) {
		t.Fatal(r, e)
	}
}
func TestFastMarchingTopologyOwnershipDeterminism(t *testing.T) {
	g := uniformFastMarchingGrid(11, 9, 3, Vec3{.6, 1, 2}, 1)
	s := []FastMarchingSeed{{Index: [3]int{5, 4, 1}}}
	opts := FastMarchingTopologyOptions{Mode: FastMarchingTopologyNoHandles}
	base, e := FastMarchingConstrained(context.Background(), g, s, FastMarchingOptions{StoppingValue: 2}, opts)
	if e != nil {
		t.Fatal(e)
	}
	if base.Report.Complete || base.Report.StopReason != "stopping-value" {
		t.Fatal(base.Report)
	}
	var wg sync.WaitGroup
	for i := 0; i < 3; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			r, e := FastMarchingConstrained(context.Background(), g, s, FastMarchingOptions{StoppingValue: 2}, opts)
			if e != nil || !reflect.DeepEqual(r, base) {
				t.Error("independent calls differ", e)
			}
		}()
	}
	wg.Wait()
	base.Times[0] = 123
	if g.Speed[0] != 1 {
		t.Fatal("owned field aliases speed")
	}
}

type topology1501CancelContext struct {
	calls atomic.Int64
	limit int64
}

func (c *topology1501CancelContext) Deadline() (time.Time, bool) { return time.Time{}, false }
func (c *topology1501CancelContext) Done() <-chan struct{}       { return nil }
func (c *topology1501CancelContext) Value(any) any               { return nil }
func (c *topology1501CancelContext) Err() error {
	if c.calls.Add(1) >= c.limit {
		return context.Canceled
	}
	return nil
}
func TestFastMarchingTopologyKernelCancellation(t *testing.T) {
	g := uniformFastMarchingGrid(9, 9, 3, Vec3{1, 1, 1}, 1)
	seeds := []FastMarchingSeed{{Index: [3]int{4, 4, 1}}}
	for _, limit := range []int64{10, 260, 400} {
		ctx := &topology1501CancelContext{limit: limit}
		r, e := FastMarchingConstrained(ctx, g, seeds, FastMarchingOptions{}, FastMarchingTopologyOptions{Mode: FastMarchingTopologyNoHandles})
		if r != nil || !errors.Is(e, context.Canceled) {
			t.Fatalf("cancel%d %v %v", limit, r, e)
		}
	}
}

func TestFastMarchingTopologyEuler3DIndependent(t *testing.T) {
	g := uniformFastMarchingGrid(5, 5, 5, Vec3{1, 1, 1}, 1)
	state := uint64(1501)
	for trial := 0; trial < 64; trial++ {
		c := &fmTopology{nx: 5, ny: 5, nz: 5, dim: 3, parent: make([]int32, 125)}
		for i := range c.parent {
			c.parent[i] = -1
		}
		mask := make([]bool, 125)
		for z := 1; z <= 3; z++ {
			for y := 1; y <= 3; y++ {
				for x := 1; x <= 3; x++ {
					if x == 2 && y == 2 && z == 2 {
						continue
					}
					state = state*6364136223846793005 + 1
					if state>>63 != 0 {
						i := (z*5+y)*5 + x
						mask[i] = true
						c.parent[i] = int32(i)
					}
				}
			}
		}
		_, _, _, before := topology1501Oracle(g, mask)
		mask[62] = true
		_, _, _, after := topology1501Oracle(g, mask)
		if got := c.eulerDelta(2, 2, 2); got != after-before {
			t.Fatalf("3Dcase%d delta%d want%d", trial, got, after-before)
		}
	}
}

func TestFastMarchingTopologyBudgetDiagnostics(t *testing.T) {
	g := uniformFastMarchingGrid(9, 9, 3, Vec3{1, 1, 1}, 1)
	seeds := []FastMarchingSeed{{Index: [3]int{4, 4, 1}}}
	for _, quota := range []struct {
		bytes, work uint64
		progress    bool
	}{{1, 0, false}, {0, 1, false}, {0, uint64(len(g.Speed))*3 + 8192*4, true}} {
		r, err := FastMarchingConstrained(context.Background(), g, seeds, FastMarchingOptions{}, FastMarchingTopologyOptions{Mode: FastMarchingTopologyStrict, MaxBytes: quota.bytes, MaxWork: quota.work})
		var diagnostic *FastMarchingTopologyFailure
		if r != nil || !errors.Is(err, ErrFastMarchingTopologyLimit) || !errors.As(err, &diagnostic) || diagnostic.Report.StopReason != "budget" || diagnostic.Report.Complete || diagnostic.Report.Mode != FastMarchingTopologyStrict {
			t.Fatalf("quota%+v result%v error%v", quota, r, err)
		}
		if quota.progress && (diagnostic.Report.Work == 0 || diagnostic.Report.Frozen < 1) {
			t.Fatalf("candidate budget lacks progress %+v", diagnostic.Report)
		}
		if !quota.progress && diagnostic.Report.Frozen != 0 {
			t.Fatalf("admission reports frozen nodes %+v", diagnostic.Report)
		}
	}
}

func TestFastMarchingTopologyByteAdmissionLedger(t *testing.T) {
	g := uniformFastMarchingGrid(5, 5, 1, Vec3{1, 1, 1}, 1)
	seeds := []FastMarchingSeed{{Index: [3]int{2, 2, 0}}}
	options := FastMarchingOptions{Targets: [][3]int{{2, 2, 0}}}
	want := uint64(30*25 + 64 + 32 + 8192)
	for _, max := range []uint64{want - 1, want} {
		r, e := FastMarchingConstrained(context.Background(), g, seeds, options, FastMarchingTopologyOptions{Mode: FastMarchingTopologyStrict, MaxBytes: max})
		if max < want {
			var f *FastMarchingTopologyFailure
			if r != nil || !errors.As(e, &f) || !errors.Is(e, ErrFastMarchingTopologyLimit) || f.Report.LogicalBytes != want || f.Report.StopReason != "budget" {
				t.Fatalf("admission %v %v", r, e)
			}
		} else if e != nil || r == nil || r.Report.LogicalBytes != want {
			t.Fatalf("exact quota %v %v", r, e)
		}
	}
}

// A punctured local 2D complement can have two components while the global
// complement remains connected around the ends of two disjoint foreground arms.
func TestFastMarchingTopologyTwoDimensionalBridgeAdmission(t *testing.T) {
	g := uniformFastMarchingGrid(7, 5, 1, Vec3{1, 1, 1}, 0)
	seeds := []FastMarchingSeed{{Index: [3]int{2, 2, 0}}, {Index: [3]int{4, 2, 0}}}
	for x := 2; x <= 4; x++ {
		g.Speed[2*7+x] = 1
	}
	before := topology1501Seeds(g, seeds)
	c0, h0, v0, chi0 := topology1501Oracle(g, before)
	after := append([]bool(nil), before...)
	after[2*7+3] = true
	c1, h1, v1, chi1 := topology1501Oracle(g, after)
	if c0 != 2 || c1 != 1 || h0 != 0 || h1 != 0 || v0 != 0 || v1 != 0 || chi1-chi0 != -1 {
		t.Fatalf("independent bridge invariants %d/%d/%d/%d -> %d/%d/%d/%d", c0, h0, v0, chi0, c1, h1, v1, chi1)
	}
	for _, mode := range []FastMarchingTopologyMode{FastMarchingTopologyStrict, FastMarchingTopologyNoHandles} {
		c, e := newFMTopology(context.Background(), g, seeds, FastMarchingOptions{}, FastMarchingTopologyOptions{Mode: mode})
		if e != nil {
			t.Fatal(e)
		}
		if c.backgroundComponents(3, 2, 0) != 2 || c.eulerDelta(3, 2, 0) != -1 {
			t.Fatal("fixture does not exhibit local/global distinction")
		}
		admitted, e := c.admit(2*7 + 3)
		if e != nil || admitted != (mode == FastMarchingTopologyNoHandles) {
			t.Fatalf("mode%d admitted%v error%v", mode, admitted, e)
		}
		if mode == FastMarchingTopologyStrict && c.report.RejectedMerge != 1 {
			t.Fatal(c.report)
		}
		if mode == FastMarchingTopologyNoHandles && (c.report.FinalComponents != 1 || c.report.RejectedCandidates != 0) {
			t.Fatal(c.report)
		}
	}
}

func TestFastMarchingTopologyAdmissionGlobalInvariants(t *testing.T) {
	// The complete foreground/complement and cubical-cell oracle must agree
	// with every admitted addition, including corner contacts and boundaries.
	for _, dim := range []int{2, 3} {
		nz, cases := 1, 256
		if dim == 3 {
			nz, cases = 5, 384
		}
		g := uniformFastMarchingGrid(5, 5, nz, Vec3{.3, 1.2, 2.7}, 1)
		state := uint64(1501)
		for trial := 0; trial < cases; trial++ {
			candidate := [3]int{2, 2, 0}
			if dim == 3 {
				candidate[2] = 2
				if trial%3 == 0 {
					candidate = [3]int{0, 0, 0}
				} else if trial%3 == 1 {
					candidate = [3]int{4, 2, 4}
				}
			}
			var seeds []FastMarchingSeed
			if dim == 2 {
				bit := 0
				for y := 1; y <= 3; y++ {
					for x := 1; x <= 3; x++ {
						if x == 2 && y == 2 {
							continue
						}
						if trial&(1<<bit) != 0 {
							seeds = append(seeds, FastMarchingSeed{Index: [3]int{x, y, 0}})
						}
						bit++
					}
				}
			} else {
				for i := range g.Speed {
					xyz := [3]int{i % 5, i / 5 % 5, i / 25}
					state = state*6364136223846793005 + 1
					if xyz != candidate && state>>63 != 0 {
						seeds = append(seeds, FastMarchingSeed{Index: xyz})
					}
				}
			}
			if len(seeds) == 0 {
				continue
			}
			before := topology1501Seeds(g, seeds)
			bc, bh, bv, _ := topology1501Oracle(g, before)
			for _, mode := range []FastMarchingTopologyMode{FastMarchingTopologyStrict, FastMarchingTopologyNoHandles} {
				control, err := newFMTopology(context.Background(), g, seeds, FastMarchingOptions{}, FastMarchingTopologyOptions{Mode: mode})
				if err != nil {
					t.Fatal(err)
				}
				i := (candidate[2]*g.Ny+candidate[1])*g.Nx + candidate[0]
				admitted, err := control.admit(i)
				if err != nil {
					t.Fatal(err)
				}
				if !admitted {
					continue
				}
				after := append([]bool(nil), before...)
				after[i] = true
				ac, ah, av, _ := topology1501Oracle(g, after)
				if ah != bh || av != bv || ac > bc || (mode == FastMarchingTopologyStrict && ac != bc) || control.report.FinalComponents != ac {
					t.Fatalf("dim%d case%d candidate%v mode%d: %d/%d/%d -> %d/%d/%d report%+v", dim, trial, candidate, mode, bc, bh, bv, ac, ah, av, control.report)
				}
			}
		}
	}
}

func TestFastMarchingTopologyCancellationPublicationReport(t *testing.T) {
	g := uniformFastMarchingGrid(5, 5, 1, Vec3{1, 1, 1}, 1)
	s := []FastMarchingSeed{{Index: [3]int{2, 2, 0}}}
	o := FastMarchingTopologyOptions{Mode: FastMarchingTopologyStrict}
	probe := &topology1501CancelContext{limit: math.MaxInt64}
	r, err := FastMarchingConstrained(probe, g, s, FastMarchingOptions{}, o)
	if err != nil || !r.Report.Complete {
		t.Fatal(r, err)
	}
	// Cancel exactly at the final publication poll, after the report scan.
	ctx := &topology1501CancelContext{limit: probe.calls.Load()}
	r, err = FastMarchingConstrained(ctx, g, s, FastMarchingOptions{}, o)
	var failure *FastMarchingTopologyFailure
	if r != nil || !errors.Is(err, context.Canceled) || !errors.As(err, &failure) || failure.Report.Complete || failure.Report.StopReason != "canceled" || failure.Report.Frozen != len(g.Speed) {
		t.Fatalf("publication result%v error%v diagnostic%+v", r, err, failure)
	}
	deadline, cancel := context.WithDeadline(context.Background(), time.Now().Add(-time.Second))
	defer cancel()
	r, err = FastMarchingConstrained(deadline, g, s, FastMarchingOptions{}, o)
	if r != nil || !errors.Is(err, context.DeadlineExceeded) || !errors.As(err, &failure) || failure.Report.Complete || failure.Report.StopReason != "deadline" {
		t.Fatalf("deadline result%v error%v", r, err)
	}
}

func TestFastMarchingTopologyDiagonalSeedsAndDisconnectedDomain(t *testing.T) {
	g := uniformFastMarchingGrid(5, 5, 3, Vec3{.4, 1.3, 2.2}, 0)
	s := []FastMarchingSeed{{Index: [3]int{1, 1, 0}}, {Index: [3]int{2, 2, 1}}, {Index: [3]int{1, 1, 0}}}
	for _, seed := range s {
		g.Speed[(seed.Index[2]*5+seed.Index[1])*5+seed.Index[0]] = 1
	}
	g.Speed[(2*5+4)*5+4] = 1 // available, but disconnected from the seeds
	g.Speed[0] = math.NaN()
	g.Speed[1] = math.Inf(1)
	g.Speed[2] = -1
	r, err := FastMarchingConstrained(context.Background(), g, s, FastMarchingOptions{}, FastMarchingTopologyOptions{Mode: FastMarchingTopologyStrict})
	if err != nil {
		t.Fatal(err)
	}
	if r.Report.InitialComponents != 1 || r.Report.FinalComponents != 1 || r.Report.Frozen != 2 || r.Report.AvailableUnreached != 1 || r.Report.Unavailable != len(g.Speed)-3 || r.Report.Complete || r.Report.StopReason != "front-exhausted" || len(r.Seeds) != len(s) {
		t.Fatal(r.Report)
	}
}

func TestFastMarchingTopologyTargetsRetainInitialSeedsUnderRoundoff(t *testing.T) {
	g := uniformFastMarchingGrid(5, 3, 1, Vec3{1.7, 1, 1}, 1e20)
	s := []FastMarchingSeed{{Index: [3]int{0, 1, 0}, Value: 1e12}, {Index: [3]int{4, 1, 0}, Value: 1e12}}
	o := FastMarchingOptions{Targets: [][3]int{{1, 1, 0}}}
	for _, mode := range []FastMarchingTopologyMode{FastMarchingTopologyStrict, FastMarchingTopologyNoHandles} {
		r, err := FastMarchingConstrained(context.Background(), g, s, o, FastMarchingTopologyOptions{Mode: mode})
		if err != nil {
			t.Fatal(err)
		}
		for _, seed := range s {
			i := (seed.Index[2]*g.Ny+seed.Index[1])*g.Nx + seed.Index[0]
			if r.Times[i] != seed.Value {
				t.Fatalf("mode%d lost initial seed %v: time%g", mode, seed.Index, r.Times[i])
			}
		}
		components, handles, cavities, _ := topology1501Oracle(g, topology1501Mask(r.FastMarchingResult))
		if components != r.Report.FinalComponents || handles != 0 || cavities != 0 || (mode == FastMarchingTopologyStrict && components != 2) {
			t.Fatalf("mode%d mask%d/%d/%d report%+v", mode, components, handles, cavities, r.Report)
		}
	}
}

func TestFastMarchingTopologyFinalScanWorkBudget(t *testing.T) {
	g := uniformFastMarchingGrid(5, 5, 1, Vec3{1, 1, 1}, 1)
	s := []FastMarchingSeed{{Index: [3]int{2, 2, 0}}}
	o := FastMarchingOptions{Targets: [][3]int{{2, 2, 0}}}
	r, err := FastMarchingConstrained(context.Background(), g, s, o, FastMarchingTopologyOptions{Mode: FastMarchingTopologyStrict})
	if err != nil {
		t.Fatal(err)
	}
	// Exhaust quota on the final report scan. No partial successful field is
	// published, and the node cleanup/target checks are in the same ledger.
	quota := r.Report.Work - 1
	r, err = FastMarchingConstrained(context.Background(), g, s, o, FastMarchingTopologyOptions{Mode: FastMarchingTopologyStrict, MaxWork: quota})
	var failure *FastMarchingTopologyFailure
	if r != nil || !errors.Is(err, ErrFastMarchingTopologyLimit) || !errors.As(err, &failure) || failure.Report.Complete || failure.Report.StopReason != "budget" || failure.Report.Work != quota {
		t.Fatalf("final scan result%v error%v", r, err)
	}
}
