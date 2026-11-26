package roi

import (
	"context"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"errors"
	"math"
	"os"
	"reflect"
	"slices"
	"testing"
)

func TestShapeInterfaceOptionsBeforeRead(t *testing.T) {
	reads := 0
	v := LevelSetVolume{Columns: 9, Rows: 9, Slices: 1, SpacingMM: [3]float64{1, 1, 1}, ValueAt: func(x, y, z int) (float64, bool) { reads++; return 1, true }}
	init := LevelSetInitialization{Seeds: []RegionGrowSeed{{X: 4, Y: 4}}}
	for _, p := range []ShapeDetectionParams{{Evolution: 99}, {SurfaceSampling: 99}, {SurfaceSampling: InterpolatedInterface}, {Evolution: InterfaceLocal}, {Evolution: InterfaceLocal, Input: EdgePotentialImage, MaxIterations: 10001}} {
		if r, e := ShapeDetectionLevelSet(nil, v, init, p, LevelSetOptions{}); r != nil || e == nil || reads != 0 {
			t.Fatalf("pre-read refusal: result=%v error=%v reads=%d", r, e, reads)
		}
	}
	p := ShapeDetectionParams{Evolution: InterfaceLocal, Input: EdgePotentialImage}
	if r, e := ShapeDetectionLevelSet(nil, v, init, p, LevelSetOptions{Limits: LevelSetLimits{MaxBytes: 1}}); r != nil || !errors.Is(e, ErrLevelSetResourceLimit) || reads != 0 {
		t.Fatalf("budget before read: %v %v %d", r, e, reads)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if r, e := ShapeDetectionLevelSet(ctx, v, init, p, LevelSetOptions{}); r != nil || !errors.Is(e, context.Canceled) || reads != 0 {
		t.Fatalf("cancel before read: %v %v %d", r, e, reads)
	}
}

func TestShapeInterfaceDomainCancelOwnership(t *testing.T) {
	n := 17
	init := edgeTestInit(n)
	p := ShapeDetectionParams{Evolution: InterfaceLocal, Input: EdgePotentialImage, PropagationScaling: 1, MaxIterations: 4, Stopping: FixedIterations}
	v := LevelSetVolume{Columns: n, Rows: n, Slices: 1, SpacingMM: [3]float64{.5, 1, 1}, ValueAt: func(x, y, z int) (float64, bool) { return 1, x != 0 || y != 0 }}
	if r, e := ShapeDetectionLevelSet(nil, v, init, p, LevelSetOptions{}); r != nil || !errors.Is(e, ErrShapeInterfaceDomain) {
		t.Fatalf("hole: %v %v", r, e)
	}
	v.ValueAt = func(x, y, z int) (float64, bool) { return math.NaN(), true }
	if r, e := ShapeDetectionLevelSet(nil, v, init, p, LevelSetOptions{}); r != nil || !errors.Is(e, ErrShapeInterfaceDomain) {
		t.Fatalf("nonfinite: %v %v", r, e)
	}
	v.ValueAt = func(x, y, z int) (float64, bool) { return 1, true }
	ctx, cancel := context.WithCancel(context.Background())
	if r, e := ShapeDetectionLevelSet(ctx, v, init, p, LevelSetOptions{Progress: func(LevelSetProgress) { cancel() }}); r != nil || !errors.Is(e, context.Canceled) {
		t.Fatalf("progress cancellation: %v %v", r, e)
	}
	a, e := ShapeDetectionLevelSet(nil, v, init, p, LevelSetOptions{})
	if e != nil {
		t.Fatal(e)
	}
	b, e := ShapeDetectionLevelSet(nil, v, init, p, LevelSetOptions{})
	if e != nil {
		t.Fatal(e)
	}
	if !reflect.DeepEqual(a, b) {
		t.Fatal("nondeterministic owned result")
	}
	for _, m := range a.Masks {
		for _, row := range m.runs {
			if len(row) > 0 {
				row[0].Start++
				break
			}
		}
		break
	}
	c, e := ShapeDetectionLevelSet(nil, v, init, p, LevelSetOptions{})
	if e != nil || !reflect.DeepEqual(b, c) {
		t.Fatalf("result mutation leaked: %v", e)
	}
}

func TestShapeInterfaceStopAndBoundary(t *testing.T) {
	const n = 9
	v := LevelSetVolume{Columns: n, Rows: n, Slices: 1, SpacingMM: [3]float64{1, 1, 1}, ValueAt: func(x, y, z int) (float64, bool) { return 0, true }}
	m := NewRasterMask(n, n)
	m.SetRun(4, 3, 6)
	init := LevelSetInitialization{Masks: map[int]*RasterMask{0: m}}
	p := ShapeDetectionParams{Evolution: InterfaceLocal, Input: EdgePotentialImage, PropagationScaling: 1, MaxIterations: 3}
	r, e := ShapeDetectionLevelSet(nil, v, init, p, LevelSetOptions{})
	if e != nil || r == nil || r.Converged || r.StopReason != LevelSetStopZeroTimeStep || r.Iterations != 0 {
		t.Fatalf("zero step must not claim RMS: %+v %v", r, e)
	}
	for y := 0; y < n; y++ {
		m.SetRun(y, 0, n)
	}
	r, e = ShapeDetectionLevelSet(nil, v, init, p, LevelSetOptions{})
	if e != nil || r == nil || r.Converged || r.StopReason != LevelSetStopEmptyBand || r.VoxelCount != n*n || !r.TouchesVolumeBoundary {
		t.Fatalf("empty active: %+v %v", r, e)
	}
	p.Boundary = RejectTouches
	if r, e = ShapeDetectionLevelSet(nil, v, init, p, LevelSetOptions{}); r != nil || !errors.Is(e, ErrEdgeLevelSetBoundary) {
		t.Fatalf("atomic contact refusal: %v %v", r, e)
	}
	reads := 0
	ctx, cancel := context.WithCancel(context.Background())
	v.ValueAt = func(x, y, z int) (float64, bool) { reads++; cancel(); return 0, true }
	if r, e = ShapeDetectionLevelSet(ctx, v, init, p, LevelSetOptions{}); r != nil || !errors.Is(e, context.Canceled) || reads != 1 {
		t.Fatalf("callback cancellation: %v %v %d", r, e, reads)
	}
}

func TestShapeInterfaceAnalyticRateAndActive(t *testing.T) {
	for _, dims := range [][3]int{{25, 23, 1}, {25, 23, 21}} {
		h := [3]float64{.5, 1, 1.5}
		d := 3
		if dims[2] == 1 {
			d = 2
			h[2] = 1
		}
		g := &levelSetGrid{ctx: context.Background(), nx: dims[0], ny: dims[1], nz: dims[2], size: dims, stride: [3]int{1, dims[0], dims[0] * dims[1]}, dims: d, n: dims[0] * dims[1] * dims[2], h: h, hmin: .5, hmax: h[d-1], bounded: true}
		phi, field := make([]float32, g.n), make([]float32, g.n)
		for i := range phi {
			x, _, _ := g.coords(i)
			phi[i] = float32(float64(x)*h[0] - 6.125)
			field[i] = .25
		}
		s := &levelSetState{levelSetGrid: g, phi: phi, band: make([]int32, g.n)}
		for i := range s.band {
			s.band[i] = int32(i)
		}
		active, e := s.interfaceActive(make([]int32, 0, g.n))
		if e != nil {
			t.Fatal(e)
		}
		for k, i := range active {
			if k > 0 && i <= active[k-1] {
				t.Fatal("active duplicate/order")
			}
			x, _, _ := g.coords(int(i))
			if x != 12 {
				t.Fatalf("updated non-interface voxel %d", x)
			}
		}
		expected := dims[1] - 2
		if d == 3 {
			expected *= dims[2] - 2
		}
		if len(active) != expected {
			t.Fatal("missing nearest crossing endpoints")
		}
		for _, p := range []float32{-1, 1} {
			rate, first, c := s.rate(g.index(12, 11, dims[2]/2), levelSetTerms{propagation: p, speed: field})
			if math.Abs(float64(rate)+float64(p)*.25) > 1e-6 || first != .25 || c != 0 {
				t.Fatalf("physical plane rate %g %g %g", rate, first, c)
			}
		}
		if sample := s.interfacePotential(g.index(12, 11, dims[2]/2), field); sample != .25 {
			t.Fatal("constant potential interpolation")
		}
		terms := levelSetTerms{propagation: 1, speed: field, curvature: 1, curvatureWeight: make([]float32, g.n)}
		for i := range field {
			field[i] = 0
		}
		rate, first, c := s.rate(g.index(12, 11, dims[2]/2), terms)
		if rate != 0 || first != 0 || c != 0 {
			t.Fatal("g=0 barrier")
		}
	}
}

func TestShapeInterfaceThinCrossingCoverage(t *testing.T) {
	for _, dims := range [][3]int{{11, 9, 1}, {11, 9, 7}} {
		d := 3
		if dims[2] == 1 {
			d = 2
		}
		g := &levelSetGrid{ctx: context.Background(), nx: dims[0], ny: dims[1], nz: dims[2], size: dims, stride: [3]int{1, dims[0], dims[0] * dims[1]}, dims: d, n: dims[0] * dims[1] * dims[2], h: [3]float64{.5, 1, 1.5}}
		phi := make([]float32, g.n)
		band := make([]int32, g.n)
		for i := range phi {
			x, y, z := g.coords(i)
			phi[i] = float32((x-5)+(y-4)*2+(z-dims[2]/2)*3) + .25
			band[i] = int32(i)
		}
		s := &levelSetState{levelSetGrid: g, phi: phi, band: band}
		for _, tied := range []bool{false, true} {
			if tied {
				for i, p := range phi {
					phi[i] = .5
					if p < 0 {
						phi[i] = -.5
					}
				}
			}
			active, err := s.interfaceActive(make([]int32, 0, g.n))
			if err != nil {
				t.Fatal(err)
			}
			selected := make([]bool, g.n)
			supported := func(i int) bool {
				pos := s.coordsArray(i)
				for a := 0; a < d; a++ {
					if pos[a] == 0 || pos[a] == g.size[a]-1 {
						return false
					}
				}
				return true
			}
			for k, i := range active {
				if !supported(int(i)) || selected[i] || k > 0 && active[k-1] >= i {
					t.Fatal("noncanonical thin set")
				}
				selected[i] = true
			}
			for i, p := range phi {
				pos := s.coordsArray(i)
				for a := 0; a < d; a++ {
					if pos[a]+1 >= g.size[a] {
						continue
					}
					j := i + g.stride[a]
					if (p < 0) == (phi[j] < 0) {
						continue
					}
					wantI := supported(i) && abs32(p) <= abs32(phi[j])
					wantJ := supported(j) && abs32(phi[j]) <= abs32(p)
					if wantI && !selected[i] || wantJ && !selected[j] {
						t.Fatal("unrepresented supported crossing/tie")
					}
				}
			}
		}
	}
}

func TestShapeInterfaceAnchoredPlaneTransfer(t *testing.T) {
	for _, dims := range [][3]int{{25, 15, 1}, {25, 15, 9}} {
		d := 3
		if dims[2] == 1 {
			d = 2
		}
		g := &levelSetGrid{ctx: context.Background(), nx: dims[0], ny: dims[1], nz: dims[2], size: dims, stride: [3]int{1, dims[0], dims[0] * dims[1]}, dims: d, n: dims[0] * dims[1] * dims[2], h: [3]float64{.5, 1, 1.5}, hmin: .5, hmax: 1.5, bounded: true}
		for _, change := range []float32{-.2, .2, 0} {
			phi, old := make([]float32, g.n), make([]float32, g.n)
			band := make([]int32, g.n)
			for i := range phi {
				x, _, _ := g.coords(i)
				phi[i] = float32(float64(x)*g.h[0] - 6.125)
				band[i] = int32(i)
			}
			s := &levelSetState{levelSetGrid: g, phi: phi, band: band, status: make([]byte, g.n), touched: make([]int32, 0, g.n), width: 4}
			s.heap.maxItems = 7 * g.n
			active, err := s.interfaceActive(make([]int32, 0, g.n))
			if err != nil {
				t.Fatal(err)
			}
			copy(old, phi)
			for _, i := range active {
				old[i] = phi[i]
				phi[i] += change
			}
			member, staged := make([]byte, g.n), make([]float32, g.n)
			next, err := s.interfacePromote(active, make([]int32, 0, g.n), old, staged, member)
			if err != nil {
				t.Fatal(err)
			}
			if err := s.refreshInterfaceLayers(next, old, member); err != nil {
				t.Fatal(err)
			}
			for z := 0; z < dims[2]; z++ {
				for y := 0; y < dims[1]; y++ {
					for x := 10; x <= 14; x++ {
						want := float32(float64(x)*g.h[0]-6.125) + change
						if got := phi[g.index(x, y, z)]; math.Abs(float64(got-want)) > 1e-6 {
							t.Fatalf("d=%d change=%g x=%d: anchor/normal shift %g want %g", d, change, x, got, want)
						}
					}
				}
			}
		}
	}
}

func TestShapeInterfaceTransferTieAndCancel(t *testing.T) {
	dims := [3]int{9, 5, 1}
	g := &levelSetGrid{ctx: context.Background(), nx: dims[0], ny: dims[1], nz: dims[2], size: dims, stride: [3]int{1, dims[0], dims[0] * dims[1]}, dims: 2, n: dims[0] * dims[1], h: [3]float64{1, 2, 1}, hmin: 1, hmax: 2, bounded: true}
	phi, old := make([]float32, g.n), make([]float32, g.n)
	band := make([]int32, g.n)
	for i := range phi {
		phi[i], band[i] = .4, int32(i)
	}
	lo, hi, middle := g.index(3, 2, 0), g.index(5, 2, 0), g.index(4, 2, 0)
	old[lo], old[hi] = -.2, -.2
	phi[lo], phi[hi] = -.1, -.3
	s := &levelSetState{levelSetGrid: g, phi: phi, band: band, status: make([]byte, g.n), touched: make([]int32, 0, g.n), width: 5}
	s.heap.maxItems = 7 * g.n
	if err := s.refreshInterfaceLayers([]int32{int32(lo), int32(hi)}, old, make([]byte, g.n)); err != nil {
		t.Fatal(err)
	}
	if phi[lo] != -.1 || phi[hi] != -.3 || math.Abs(float64(phi[middle]-.7)) > 1e-6 {
		t.Fatalf("anchors/tie staging overwritten: %g %g %g", phi[lo], phi[hi], phi[middle])
	}
	before := append([]float32(nil), phi...)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	g.ctx = ctx
	if err := s.refreshInterfaceLayers(nil, old, make([]byte, g.n)); !errors.Is(err, context.Canceled) || !reflect.DeepEqual(phi, before) {
		t.Fatal("cancellation before transfer mutated phi")
	}
}

func TestShapeInterfaceFinalStepCompletesLayers(t *testing.T) {
	const n = 17
	g := &levelSetGrid{ctx: context.Background(), volume: LevelSetVolume{Columns: n, Rows: n, Slices: 1}, nx: n, ny: n, nz: 1, n: n * n, size: [3]int{n, n, 1}, stride: [3]int{1, n, n * n}, dims: 2, h: [3]float64{1, 1, 1}, hmin: 1, hmax: 1, bounded: true}
	phi, speed := make([]float32, g.n), make([]float32, g.n)
	for i := range phi {
		x, _, _ := g.coords(i)
		phi[i] = float32(x) - 7.5
		speed[i] = 1
	}
	stoppedPhi, fixedPhi := append([]float32(nil), phi...), append([]float32(nil), phi...)
	terms := levelSetTerms{propagation: 1, speed: speed, stopping: RMSOnly}
	rms, err := g.evolveInterface(stoppedPhi, terms, GridCentres, 1, 1, nil)
	if err != nil {
		t.Fatal(err)
	}
	terms.stopping = FixedIterations
	fixed, err := g.evolveInterface(fixedPhi, terms, GridCentres, 1, 1, nil)
	if err != nil {
		t.Fatal(err)
	}
	if rms.StopReason != LevelSetStopRMS || !rms.Converged || fixed.StopReason != LevelSetStopIterationLimit || !reflect.DeepEqual(stoppedPhi, fixedPhi) || !reflect.DeepEqual(rms.Masks, fixed.Masks) || rms.RMSChange != fixed.RMSChange {
		t.Fatalf("last-step field differs by stopping: rms=%+v fixed=%+v", rms, fixed)
	}
}

func TestShapeInterfaceBoundaryEmbeddingMoves(t *testing.T) {
	const n = 9
	mask := NewRasterMask(n, n)
	for y := 0; y < n; y++ {
		mask.SetRun(y, 0, 4)
	}
	v := LevelSetVolume{Columns: n, Rows: n, Slices: 1, SpacingMM: [3]float64{1, 1, 1}, ValueAt: func(x, y, z int) (float64, bool) { return 1, true }}
	p := ShapeDetectionParams{Evolution: InterfaceLocal, Input: EdgePotentialImage, PropagationScaling: -1, MaxIterations: 8, Stopping: FixedIterations}
	r, err := ShapeDetectionLevelSet(nil, v, LevelSetInitialization{Masks: map[int]*RasterMask{0: mask}}, p, LevelSetOptions{MarginMM: 1000})
	if err != nil {
		t.Fatal(err)
	}
	if m := r.Masks[0]; m == nil || !m.Get(0, 0) || m.Get(3, 0) || m.Get(3, n-1) {
		t.Fatal("boundary embedding must move with adjacent interior interface; it is not frozen")
	}
	// An outer closest endpoint has no supported PDE anchor. Return an
	// explicit domain stop, preserving its sign instead of calling RMS.
	g := &levelSetGrid{ctx: context.Background(), volume: v, nx: n, ny: n, nz: 1, n: n * n, size: [3]int{n, n, 1}, stride: [3]int{1, n, n * n}, dims: 2, h: [3]float64{1, 1, 1}, hmin: 1, hmax: 1, bounded: true}
	phi, speed := make([]float32, g.n), make([]float32, g.n)
	for i := range phi {
		x, _, _ := g.coords(i)
		phi[i], speed[i] = -.75, 1
		if x == n-1 {
			phi[i] = .25
		}
	}
	out, err := g.evolveInterface(phi, levelSetTerms{propagation: 1, speed: speed}, GridCentres, 8, .0005, nil)
	if err != nil || out == nil || out.Converged || out.StopReason != LevelSetStopBoundaryActiveExhausted || out.Iterations != 0 {
		t.Fatalf("unsupported boundary must be explicit: %+v %v", out, err)
	}
	if r, err := edgeBoundaryResult(&LevelSetResult{StopReason: LevelSetStopBoundaryActiveExhausted}, RejectTouches); r != nil || !errors.Is(err, ErrEdgeLevelSetBoundary) {
		t.Fatal("boundary exhaustion must respect complete-result refusal")
	}
}

func TestShapeInterfaceCityBlockLayersAndPhysicalMotion(t *testing.T) {
	for _, dims := range [][3]int{{25, 25, 1}, {25, 25, 25}} {
		d := 3
		if dims[2] == 1 {
			d = 2
		}
		g := &levelSetGrid{ctx: context.Background(), volume: LevelSetVolume{Columns: dims[0], Rows: dims[1], Slices: dims[2]}, nx: dims[0], ny: dims[1], nz: dims[2], n: dims[0] * dims[1] * dims[2], size: dims, stride: [3]int{1, dims[0], dims[0] * dims[1]}, dims: d, h: [3]float64{.5, 1, 1.5}, hmin: .5, hmax: 1.5, bounded: true}
		for axis := 0; axis < d; axis++ {
			phi, speed := make([]float32, g.n), make([]float32, g.n)
			centre := [3]int{dims[0] / 2, dims[1] / 2, dims[2] / 2}
			for i := range phi {
				x, y, z := g.coords(i)
				pos := [3]int{x, y, z}
				phi[i] = float32((float64(pos[axis]-centre[axis]) - .25) * g.hmin)
				speed[i] = 1
			}
			s := &levelSetState{levelSetGrid: g, phi: phi, width: float32(float64(d+1) * g.hmin), status: make([]byte, g.n), band: make([]int32, 0, g.n), touched: make([]int32, 0, g.n)}
			s.heap.maxItems = 7 * g.n
			if err := s.initializeInterfaceAnchors(); err != nil {
				t.Fatal(err)
			}
			active, e := s.initialInterfaceActive(make([]int32, 0, g.n))
			if e != nil {
				t.Fatal(e)
			}
			if e := s.refreshInterfaceLayers(active, make([]float32, g.n), make([]byte, g.n)); e != nil {
				t.Fatal(e)
			}
			for step := -d; step <= d; step++ {
				pos := centre
				pos[axis] += step
				want := float32((float64(step) - .25) * g.hmin)
				if got := phi[g.index(pos[0], pos[1], pos[2])]; got != want {
					t.Fatalf("d=%d axis=%d layer=%d phi=%g want=%g", d, axis, step, got, want)
				}
			}
			i := g.index(centre[0], centre[1], centre[2])
			rate, first, curvature := s.rate(i, levelSetTerms{propagation: 1, speed: speed})
			wantRate := -g.hmin / g.h[axis]
			if math.Abs(float64(rate)-wantRate) > 1e-6 || first != 1 || curvature != 0 {
				t.Fatal("city-block embedding must retain physical PDE derivatives")
			}
			dt := interfaceTimeStep(first, curvature, g.hmin, d)
			physicalMotion := -dt * float64(rate) * g.h[axis] / g.hmin
			if math.Abs(physicalMotion-dt) > 1e-7 {
				t.Fatal("embedding must not compensate the physical speed twice")
			}
		}
	}
}

func TestShapeInterfaceHalfLayerPromotion(t *testing.T) {
	for _, dims := range [][3]int{{17, 17, 1}, {17, 17, 17}} {
		d := 3
		if dims[2] == 1 {
			d = 2
		}
		g := &levelSetGrid{ctx: context.Background(), nx: dims[0], ny: dims[1], nz: dims[2], n: dims[0] * dims[1] * dims[2], size: dims, stride: [3]int{1, dims[0], dims[0] * dims[1]}, dims: d, h: [3]float64{.5, 1, 1.5}, hmin: .5, hmax: 1.5, bounded: true}
		for axis := 0; axis < d; axis++ {
			for _, direction := range []int{-1, 1} {
				phi, old := make([]float32, g.n), make([]float32, g.n)
				for i := range phi {
					x, y, z := g.coords(i)
					pos := [3]int{x, y, z}
					phi[i] = float32((float64(pos[axis]-8) - float64(direction)*.48) * g.hmin)
				}
				s := &levelSetState{levelSetGrid: g, phi: phi, width: float32(float64(d+1) * g.hmin), status: make([]byte, g.n), band: make([]int32, 0, g.n), touched: make([]int32, 0, g.n)}
				s.heap.maxItems = 7 * g.n
				if e := s.initializeInterfaceAnchors(); e != nil {
					t.Fatal(e)
				}
				active, e := s.interfaceActive(make([]int32, 0, g.n))
				if e != nil {
					t.Fatal(e)
				}
				copy(old, phi)
				delta := float32(float64(-direction) * interfaceTimeStep(1, 0, g.hmin, d) * g.hmin / g.h[axis])
				for _, index := range active {
					pos := s.coordsArray(int(index))
					if pos[axis] != 8 {
						t.Fatal("wrong old anchor")
					}
					phi[index] += delta
				}
				member, staged := make([]byte, g.n), make([]float32, g.n)
				promoted, e := s.interfacePromote(active, make([]int32, 0, g.n), old, staged, member)
				if e != nil || len(promoted) != len(active) {
					t.Fatalf("promotion count/error %d/%d %v", len(promoted), len(active), e)
				}
				if e := s.refreshInterfaceLayers(promoted, old, member); e != nil {
					t.Fatal(e)
				}
				for k, index := range promoted {
					pos := s.coordsArray(int(index))
					if pos[axis] != 8+direction || s.phi[index] <= float32(-g.hmin/2) || s.phi[index] > float32(g.hmin/2) || k > 0 && index <= promoted[k-1] {
						t.Fatal("promotion phase/order")
					}
				}
			}
		}
	}
}

func TestShapeInterfaceCompletePhysicalPlaneMotion(t *testing.T) {
	for _, dims := range [][3]int{{25, 25, 1}, {25, 25, 25}} {
		d := 3
		if dims[2] == 1 {
			d = 2
		}
		g := &levelSetGrid{ctx: context.Background(), volume: LevelSetVolume{Columns: dims[0], Rows: dims[1], Slices: dims[2]}, nx: dims[0], ny: dims[1], nz: dims[2], n: dims[0] * dims[1] * dims[2], size: dims, stride: [3]int{1, dims[0], dims[0] * dims[1]}, dims: d, h: [3]float64{.5, 1, 1.5}, hmin: .5, hmax: 1.5, bounded: true}
		for normal := 0; normal <= d; normal++ {
			for _, propagation := range []float32{-1, 1} {
				phi, speed := make([]float32, g.n), make([]float32, g.n)
				gradient := 0.0
				for a := 0; a < d; a++ {
					if normal == d || normal == a {
						gradient += g.hmin * g.hmin / (g.h[a] * g.h[a])
					}
				}
				gradient = math.Sqrt(gradient)
				for i := range phi {
					x, y, z := g.coords(i)
					pos := [3]int{x, y, z}
					value := .5
					for a := 0; a < d; a++ {
						if normal == d || normal == a {
							value += float64(pos[a] - 12)
						}
					}
					phi[i] = float32(value * g.hmin)
					speed[i] = 1
				}
				centre := g.index(12, 12, dims[2]/2)
				before := phi[centre]
				r, e := g.evolveInterface(phi, levelSetTerms{propagation: propagation, speed: speed, curvature: float32(d - 1), curvatureWeight: speed, stopping: FixedIterations}, GridCentres, 1, .0005, nil)
				if e != nil {
					t.Fatal(e)
				}
				dt := interfaceTimeStep(1, 1, g.hmin, d)
				physical := -float64(phi[centre]-before) / gradient
				if r.Iterations != 1 || math.Abs(physical-float64(propagation)*dt) > 1e-6 {
					t.Fatalf("d=%d normal=%d P=%g complete physical move %g want %g", d, normal, propagation, physical, float64(propagation)*dt)
				}
			}
		}
	}
}

func TestShapeInterfaceSignedLayerComplement(t *testing.T) {
	for _, dims := range [][3]int{{17, 17, 1}, {17, 17, 17}} {
		d := 3
		if dims[2] == 1 {
			d = 2
		}
		g := &levelSetGrid{ctx: context.Background(), nx: dims[0], ny: dims[1], nz: dims[2], n: dims[0] * dims[1] * dims[2], size: dims, stride: [3]int{1, dims[0], dims[0] * dims[1]}, dims: d, h: [3]float64{.5, 1, 1.5}, hmin: .5, hmax: 1.5, bounded: true}
		phi := make([]float32, g.n)
		for i := range phi {
			x, _, _ := g.coords(i)
			phi[i] = 1
			if x < 8 {
				phi[i] = -1
			}
		}
		centre := [3]int{8, 8, dims[2] / 2}
		anchor := g.index(centre[0], centre[1], centre[2])
		phi[anchor] = .2
		s := &levelSetState{levelSetGrid: g, phi: phi, width: float32(float64(d+1) * g.hmin), status: make([]byte, g.n), band: make([]int32, 0, g.n)}
		s.heap.maxItems = 7 * g.n
		if e := s.refreshInterfaceLayers([]int32{int32(anchor)}, make([]float32, g.n), make([]byte, g.n)); e != nil {
			t.Fatal(e)
		}
		if phi[anchor] != .2 {
			t.Fatal("signed anchor overwritten")
		}
		for _, direction := range []int{-1, 1} {
			for steps := 1; steps <= d; steps++ {
				pos := centre
				pos[0] += direction
				if steps > 1 {
					pos[1]++
				}
				if steps > 2 {
					pos[2]++
				}
				want := .2 + float64(direction*steps)*g.hmin
				if got := phi[g.index(pos[0], pos[1], pos[2])]; math.Abs(float64(got)-want) > 1e-6 {
					t.Fatalf("signed graph distance %g want %g", got, want)
				}
			}
		}
		inside := phi[g.index(7, 8, centre[2])]
		if math.Abs(float64(abs32(inside)+abs32(phi[anchor]))-g.hmin) > 1e-6 {
			t.Fatal("signed crossing endpoints do not complement one layer")
		}
	}
}

func TestShapeInterfaceBoundaryProjectionSnapshot(t *testing.T) {
	// A size-three axis has only one interior sample. Its next inward
	// neighbour is a boundary sample, so compatibility must use one
	// immutable snapshot rather than earlier projection writes.
	for _, dims := range [][3]int{{3, 3, 1}, {3, 3, 3}} {
		d := 3
		if dims[2] == 1 {
			d = 2
		}
		g := &levelSetGrid{ctx: context.Background(), nx: dims[0], ny: dims[1], nz: dims[2], n: dims[0] * dims[1] * dims[2], dims: d, size: dims, stride: [3]int{1, dims[0], dims[0] * dims[1]}}
		phi := make([]float32, g.n)
		for i := range phi {
			phi[i] = .4
		}
		centre := [3]int{1, 1, dims[2] / 2}
		phi[g.index(centre[0], centre[1], centre[2])] = .2
		for a := 0; a < d; a++ {
			p := centre
			p[a]++
			phi[g.index(p[0], p[1], p[2])] = .2
		}
		before, want := append([]float32(nil), phi...), append([]float32(nil), phi...)
		for i := range want {
			x, y, z := g.coords(i)
			p := [3]int{x, y, z}
			compatible := true
			for a := 0; a < d; a++ {
				if p[a] != 1 {
					next := centre
					if p[a] == 0 {
						next[a]++
					} else {
						next[a]--
					}
					if before[g.index(next[0], next[1], next[2])] != before[g.index(centre[0], centre[1], centre[2])] {
						compatible = false
					}
				}
			}
			if compatible {
				want[i] = before[g.index(centre[0], centre[1], centre[2])]
			}
		}
		s := &levelSetState{levelSetGrid: g, phi: phi, width: 3}
		saved := make([]float32, g.n)
		if e := s.projectInterfaceBoundary(saved); e != nil {
			t.Fatal(e)
		}
		if !reflect.DeepEqual(phi, want) {
			t.Fatalf("d=%d projection depends on earlier boundary writes: got %v want %v", d, phi, want)
		}
		if !reflect.DeepEqual(saved, before) {
			t.Fatal("boundary compatibility snapshot changed")
		}
	}
}

func TestShapeInterfaceInitialCityBlockDistance(t *testing.T) {
	// In the c-scaled L1 embedding, shortest distance to an affine
	// plane is c*min(alpha), including oblique planes. Euclidean normal
	// distance is a different metric and cannot be mixed with c layers.
	for _, dims := range [][3]int{{9, 9, 1}, {9, 9, 9}} {
		d := 3
		if dims[2] == 1 {
			d = 2
		}
		for k := 1; k <= d; k++ {
			for _, sign := range []float64{-1, 1} {
				g := &levelSetGrid{ctx: context.Background(), nx: dims[0], ny: dims[1], nz: dims[2], n: dims[0] * dims[1] * dims[2], size: dims, stride: [3]int{1, dims[0], dims[0] * dims[1]}, dims: d, h: [3]float64{.5, 1, 1.5}, hmin: .5}
				phi := make([]float32, g.n)
				for i := range phi {
					x, y, z := g.coords(i)
					pos := [3]int{x, y, z}
					value := sign * .25
					for a := 0; a < k; a++ {
						value += float64(pos[a] - 4)
					}
					phi[i] = float32(g.hmin * value)
				}
				s := &levelSetState{levelSetGrid: g, phi: phi, width: float32(float64(d+1) * g.hmin), band: make([]int32, 0, g.n)}
				if e := s.initializeInterfaceAnchors(); e != nil {
					t.Fatal(e)
				}
				want := sign * g.hmin * .25
				point := g.index(4, 4, dims[2]/2)
				if math.Abs(float64(phi[point])-want) > 1e-7 {
					t.Fatalf("dimension%d normalAxes%d sign%g initial distance%g want%g", d, k, sign, phi[point], want)
				}
			}
		}
	}
}

func TestShapeInterfaceInitialCrossingAndPhase(t *testing.T) {
	for _, dims := range [][3]int{{17, 17, 1}, {17, 17, 17}} {
		d := 3
		if dims[2] == 1 {
			d = 2
		}
		for axes := 1; axes <= d; axes++ {
			for _, sign := range []int{-1, 1} {
				g := &levelSetGrid{ctx: context.Background(), nx: dims[0], ny: dims[1], nz: dims[2], n: dims[0] * dims[1] * dims[2], size: dims, stride: [3]int{1, dims[0], dims[0] * dims[1]}, dims: d, h: [3]float64{.5, 1, 1.5}, hmin: .5}
				phi := make([]float32, g.n)
				for i := range phi {
					x, y, z := g.coords(i)
					pos := [3]int{x, y, z}
					sum := 0
					for a := 0; a < axes; a++ {
						sum += pos[a] - 8
					}
					phi[i] = float32(sign) * .25
					if sum < 0 {
						phi[i] = -float32(sign) * .25
					}
				}
				s := &levelSetState{levelSetGrid: g, phi: phi, width: float32(float64(d+1) * g.hmin), status: make([]byte, g.n), band: make([]int32, 0, g.n)}
				s.heap.maxItems = 7 * g.n
				if e := s.initializeInterfaceAnchors(); e != nil {
					t.Fatal(e)
				}
				active, e := s.initialInterfaceActive(make([]int32, 0, g.n))
				if e != nil {
					t.Fatal(e)
				}
				for k, index := range active {
					if abs32(phi[index]) > .25 || k > 0 && index <= active[k-1] {
						t.Fatal("initial phase/order")
					}
					x, y, z := g.coords(int(index))
					pos := [3]int{x, y, z}
					sum := 0
					for a := 0; a < axes; a++ {
						sum += pos[a] - 8
					}
					if sum != -1 && sum != 0 {
						t.Fatal("initial exact tie includes a non-crossing endpoint")
					}
				}
				if e = s.refreshInterfaceLayers(active, make([]float32, g.n), make([]byte, g.n)); e != nil {
					t.Fatal(e)
				}
				point := g.index(8, 8, dims[2]/2)
				want := float32(sign) * .25
				if phi[point] != want || phi[point-1] != -want {
					t.Fatal("initial affine crossing changed")
				}
				crossing := -float64(phi[point]) / (float64(phi[point]) - float64(phi[point-1]))
				if crossing != -.5 {
					t.Fatal("initial continuous crossing changed")
				}
				// A zero PDE update transfers the initial closed endpoint into
				// the persistent half-open phase without moving its crossing.
				old := append([]float32(nil), phi...)
				member := make([]byte, g.n)
				next, e := s.interfacePromote(active, make([]int32, 0, g.n), old, make([]float32, g.n), member)
				if e != nil {
					t.Fatal(e)
				}
				if e := s.refreshInterfaceLayers(next, old, member); e != nil {
					t.Fatal(e)
				}
				if phi[point] != want || phi[point-1] != -want {
					t.Fatal("zero-term phase transfer changed crossing")
				}
			}
		}
	}
}

// A storage-order tie can choose different physical sides of a curved
// interface after reflecting an otherwise identical input. This test checks
// all reflection combinations without using a public expected segmentation.
func TestShapeInterfaceInitialReflectionEquivariance(t *testing.T) {
	for _, dimensions := range []int{2, 3} {
		for _, spacing := range [][3]float64{{1, 1, 1}, {.5, 1, 2}} {
			for _, sign := range []float32{-1, 1} {
				size := [3]int{7, 7, 7}
				if dimensions == 2 {
					size[2] = 1
				}
				g := &levelSetGrid{ctx: context.Background(), nx: size[0], ny: size[1], nz: size[2], size: size, stride: [3]int{1, size[0], size[0] * size[1]}, n: size[0] * size[1] * size[2], dims: dimensions, h: spacing, hmin: spacing[0], hmax: spacing[dimensions-1]}
				original := make([]float32, g.n)
				for i := range original {
					position := [3]int{i % g.nx, (i / g.nx) % g.ny, i / (g.nx * g.ny)}
					inside := true
					for axis := 0; axis < dimensions; axis++ {
						inside = inside && position[axis] >= 2 && position[axis] <= 3
					}
					original[i] = sign * float32(g.hmin/2)
					if inside {
						original[i] = -original[i]
					}
				}
				selectActive := func(input []float32) []bool {
					s := &levelSetState{levelSetGrid: g, phi: append([]float32(nil), input...), width: float32(float64(dimensions+1) * g.hmin), band: make([]int32, 0, g.n)}
					if e := s.initializeInterfaceAnchors(); e != nil {
						t.Fatal(e)
					}
					active, e := s.initialInterfaceActive(make([]int32, 0, g.n))
					if e != nil {
						t.Fatal(e)
					}
					selected := make([]bool, g.n)
					for _, i := range active {
						selected[i] = true
					}
					return selected
				}
				base := selectActive(original)
				for reflection := 1; reflection < 1<<dimensions; reflection++ {
					mirror := func(i int) int {
						position := [3]int{i % g.nx, (i / g.nx) % g.ny, i / (g.nx * g.ny)}
						for axis := 0; axis < dimensions; axis++ {
							if reflection&(1<<axis) != 0 {
								position[axis] = size[axis] - 1 - position[axis]
							}
						}
						return g.index(position[0], position[1], position[2])
					}
					input := make([]float32, g.n)
					for i := range input {
						input[i] = original[mirror(i)]
					}
					actual := selectActive(input)
					mismatch := 0
					for i := range actual {
						if actual[i] != base[mirror(i)] {
							mismatch++
						}
					}
					if mismatch != 0 {
						t.Errorf("D=%d h=%v sign=%g reflection=%d: %d active entries depend on storage order", dimensions, spacing, sign, reflection, mismatch)
					}
				}
			}
		}
	}
}

func TestShapeInterfaceInitialPhaseComplement(t *testing.T) {
	for _, dimensions := range []int{2, 3} {
		size := [3]int{9, 9, 9}
		if dimensions == 2 {
			size[2] = 1
		}
		g := &levelSetGrid{ctx: context.Background(), nx: size[0], ny: size[1], nz: size[2], size: size, stride: [3]int{1, size[0], size[0] * size[1]}, n: size[0] * size[1] * size[2], dims: dimensions, h: [3]float64{.5, 1, 2}, hmin: .5, hmax: 2}
		input := make([]float32, g.n)
		for i := range input {
			position := [3]int{i % g.nx, (i / g.nx) % g.ny, i / (g.nx * g.ny)}
			inside := true
			for axis := 0; axis < dimensions; axis++ {
				inside = inside && position[axis] >= 3 && position[axis] <= 4
			}
			input[i] = .25
			if inside {
				input[i] = -.25
			}
		}
		var selected [2][]bool
		for phase := range selected {
			phi := append([]float32(nil), input...)
			if phase == 1 {
				for i := range phi {
					phi[i] = -phi[i]
				}
			}
			s := &levelSetState{levelSetGrid: g, phi: phi, width: float32(float64(dimensions+1) * g.hmin), band: make([]int32, 0, g.n)}
			if e := s.initializeInterfaceAnchors(); e != nil {
				t.Fatal(e)
			}
			active, e := s.initialInterfaceActive(make([]int32, 0, g.n))
			if e != nil || len(active) == 0 {
				t.Fatalf("missing initial crossing: %v", e)
			}
			selected[phase] = make([]bool, g.n)
			for _, i := range active {
				selected[phase][i] = true
			}
		}
		mismatch := 0
		for i := range selected[0] {
			if selected[0][i] != selected[1][i] {
				mismatch++
			}
		}
		if mismatch != 0 {
			t.Errorf("D=%d: %d geometric nearest ties change with sign complement", dimensions, mismatch)
		}
	}
}

func TestShapeInterfaceReflectedCompleteStep(t *testing.T) {
	for _, dimensions := range []int{2, 3} {
		for _, spacing := range [][3]float64{{1, 1, 1}, {.5, 1, 2}} {
			size := [3]int{9, 9, 9}
			if dimensions == 2 {
				size[2] = 1
			}
			g := &levelSetGrid{ctx: context.Background(), volume: LevelSetVolume{Columns: size[0], Rows: size[1], Slices: size[2]}, nx: size[0], ny: size[1], nz: size[2], size: size, stride: [3]int{1, size[0], size[0] * size[1]}, n: size[0] * size[1] * size[2], dims: dimensions, h: spacing, hmin: spacing[0], hmax: spacing[dimensions-1], bounded: true}
			original, speed := make([]float32, g.n), make([]float32, g.n)
			for i := range original {
				position := [3]int{i % g.nx, (i / g.nx) % g.ny, i / (g.nx * g.ny)}
				inside := true
				for axis := 0; axis < dimensions; axis++ {
					inside = inside && position[axis] >= 3 && position[axis] <= 4
				}
				original[i], speed[i] = float32(g.hmin/2), 1
				if inside {
					original[i] = -original[i]
				}
			}
			for _, propagation := range []float32{-1, 1} {
				for _, curvature := range []float32{0, .25} {
					terms := levelSetTerms{propagation: propagation, speed: speed, curvature: curvature, curvatureWeight: speed, stopping: FixedIterations}
					base := append([]float32(nil), original...)
					if _, e := g.evolveInterface(base, terms, GridCentres, 1, .0005, nil); e != nil {
						t.Fatal(e)
					}
					for reflection := 1; reflection < 1<<dimensions; reflection++ {
						mirror := func(i int) int {
							position := [3]int{i % g.nx, (i / g.nx) % g.ny, i / (g.nx * g.ny)}
							for axis := 0; axis < dimensions; axis++ {
								if reflection&(1<<axis) != 0 {
									position[axis] = size[axis] - 1 - position[axis]
								}
							}
							return g.index(position[0], position[1], position[2])
						}
						actual := make([]float32, g.n)
						for i := range actual {
							actual[i] = original[mirror(i)]
						}
						if _, e := g.evolveInterface(actual, terms, GridCentres, 1, .0005, nil); e != nil {
							t.Fatal(e)
						}
						for i := range actual {
							want := base[mirror(i)]
							if (actual[i] < 0) != (want < 0) || math.Abs(float64(actual[i]-want)) > 1e-6*g.hmin {
								t.Fatalf("D=%d h=%v P=%g C=%g reflection=%d i=%d phi=%g want=%g", dimensions, spacing, propagation, curvature, reflection, i, actual[i], want)
							}
						}
					}
				}
			}
		}
	}
}

func TestShapeInterfacePromotionSignedEnvelope(t *testing.T) {
	for _, dimensions := range []int{2, 3} {
		size := [3]int{7, 7, 7}
		if dimensions == 2 {
			size[2] = 1
		}
		g := &levelSetGrid{ctx: context.Background(), nx: size[0], ny: size[1], nz: size[2], size: size, stride: [3]int{1, size[0], size[0] * size[1]}, n: size[0] * size[1] * size[2], dims: dimensions, h: [3]float64{1, 2, 3}, hmin: 1, hmax: 3}
		centre := [3]int{3, 3, size[2] / 2}
		target := g.index(centre[0], centre[1], centre[2])
		for axis := 0; axis < dimensions; axis++ {
			for secondAxis := axis + 1; secondAxis < dimensions; secondAxis++ {
				for _, direction := range []int{-1, 1} {
					for _, secondDirection := range []int{-1, 1} {
						for _, phase := range []float32{-1, 1} {
							for _, swap := range []bool{false, true} {
								old, phi := make([]float32, g.n), make([]float32, g.n)
								for i := range old {
									old[i], phi[i] = -3*phase, -3*phase
								}
								old[target], phi[target] = -.6*phase, -.6*phase
								a, b := target+direction*g.stride[axis], target+secondDirection*g.stride[secondAxis]
								old[a], old[b] = .4*phase, .4*phase
								phi[a], phi[b] = .9*phase, 1.4*phase
								if swap {
									phi[a], phi[b] = phi[b], phi[a]
								}
								first, second := phi[a]-phase, phi[b]-phase
								want := min(first, second)
								if old[target] < 0 {
									want = max(first, second)
								}
								active := []int32{int32(a), int32(b)}
								if active[0] > active[1] {
									active[0], active[1] = active[1], active[0]
								}
								s := &levelSetState{levelSetGrid: g, phi: phi}
								_, e := s.interfacePromote(active, make([]int32, 0, g.n), old, make([]float32, g.n), make([]byte, g.n))
								if e != nil {
									t.Fatal(e)
								}
								if phi[target] != want || (phi[target] < 0) != (want < 0) {
									t.Errorf("D=%d axes=%d,%d dirs=%d,%d phase=%g swap=%t: candidate=%g want signed envelope=%g", dimensions, axis, secondAxis, direction, secondDirection, phase, swap, phi[target], want)
								}
							}
						}
					}
				}
			}
		}
	}
}

func TestShapeInterfaceZeroTermsPreserveBoundaryPhase(t *testing.T) {
	for _, dimensions := range []int{2, 3} {
		for _, spacing := range [][3]float64{{1, 1, 1}, {.5, 1, 2}} {
			size := [3]int{9, 9, 9}
			if dimensions == 2 {
				size[2] = 1
			}
			g := &levelSetGrid{ctx: context.Background(), volume: LevelSetVolume{Columns: size[0], Rows: size[1], Slices: size[2]}, nx: size[0], ny: size[1], nz: size[2], size: size, stride: [3]int{1, size[0], size[0] * size[1]}, n: size[0] * size[1] * size[2], dims: dimensions, h: spacing, hmin: spacing[0], hmax: spacing[dimensions-1], bounded: true}
			for reflection := 0; reflection < 1<<dimensions; reflection++ {
				for _, phase := range []float32{-1, 1} {
					phi := make([]float32, g.n)
					before := make([]bool, g.n)
					for i := range phi {
						position := [3]int{i % g.nx, (i / g.nx) % g.ny, i / (g.nx * g.ny)}
						for axis := 0; axis < dimensions; axis++ {
							if reflection&(1<<axis) != 0 {
								position[axis] = size[axis] - 1 - position[axis]
							}
						}
						inside := position[0] <= 3 || position[0] == 4 && position[1] == 0
						phi[i] = phase * float32(g.hmin/2)
						if inside {
							phi[i] = -phi[i]
						}
						before[i] = phi[i] < 0
					}
					r, e := g.evolveInterface(phi, levelSetTerms{}, GridCentres, 1, .0005, nil)
					if e != nil || r == nil || r.Iterations != 0 || r.StopReason != LevelSetStopZeroTimeStep {
						t.Fatalf("zero terms D=%d h=%v reflection=%d phase=%g: %+v %v", dimensions, spacing, reflection, phase, r, e)
					}
					changed := 0
					for i := range phi {
						if (phi[i] < 0) != before[i] {
							changed++
						}
					}
					if changed != 0 {
						t.Errorf("D=%d h=%v reflection=%d phase=%g: initial refresh changed %d source signs without a PDE update", dimensions, spacing, reflection, phase, changed)
					}
				}
			}
		}
	}
}

func TestShapeInterfaceStopsWhenSurfaceDisappears(t *testing.T) {
	for _, dimensions := range []int{2, 3} {
		size := [3]int{9, 9, 9}
		if dimensions == 2 {
			size[2] = 1
		}
		g := &levelSetGrid{ctx: context.Background(), volume: LevelSetVolume{Columns: size[0], Rows: size[1], Slices: size[2]}, nx: size[0], ny: size[1], nz: size[2], size: size, stride: [3]int{1, size[0], size[0] * size[1]}, n: size[0] * size[1] * size[2], dims: dimensions, h: [3]float64{1, 1, 1}, hmin: 1, hmax: 1, bounded: true}
		phi, speed := make([]float32, g.n), make([]float32, g.n)
		for i := range phi {
			phi[i], speed[i] = .5, 1
		}
		phi[g.index(4, 4, size[2]/2)] = -.5
		vanished := 0
		r, e := g.evolveInterface(phi, levelSetTerms{propagation: -1, speed: speed, stopping: FixedIterations}, GridCentres, 64, .0005, func(progress LevelSetProgress) {
			inside := false
			for _, p := range phi {
				inside = inside || p < 0
			}
			if !inside && vanished == 0 {
				vanished = progress.Iteration
			}
		})
		if e != nil || r == nil || vanished == 0 || r.VoxelCount != 0 || r.Converged || r.StopReason != LevelSetStopEmptyBand {
			t.Fatalf("D=%d disappearance=%d: %+v %v", dimensions, vanished, r, e)
		}
		if r.Iterations != vanished {
			t.Errorf("D=%d took %d PDE steps after the first complete empty foreground step%d", dimensions, r.Iterations-vanished, vanished)
		}
	}
}

func TestShapeInterfaceCrossingPartnerAtBoundary(t *testing.T) {
	for _, dimensions := range []int{2, 3} {
		for _, spacing := range [][3]float64{{1, 1, 1}, {.5, 1, 2}} {
			size := [3]int{9, 9, 9}
			if dimensions == 2 {
				size[2] = 1
			}
			g := &levelSetGrid{ctx: context.Background(), volume: LevelSetVolume{Columns: size[0], Rows: size[1], Slices: size[2]}, nx: size[0], ny: size[1], nz: size[2], size: size, stride: [3]int{1, size[0], size[0] * size[1]}, n: size[0] * size[1] * size[2], dims: dimensions, h: spacing, hmin: spacing[0], hmax: spacing[dimensions-1], bounded: true}
			for _, propagation := range []float32{-1, 1} {
				phi, speed := make([]float32, g.n), make([]float32, g.n)
				for i := range phi {
					x := i % g.nx
					phi[i], speed[i] = float32(g.hmin*(float64(x-(g.nx-2))-.1)), 1
				}
				index := g.index(g.nx-2, 4, size[2]/2)
				initial := float64(phi[index])
				r, e := g.evolveInterface(phi, levelSetTerms{propagation: propagation, speed: speed, stopping: FixedIterations}, GridCentres, 1, .0005, nil)
				if e != nil || r == nil || r.Iterations != 1 || r.StopReason != LevelSetStopIterationLimit || r.Converged {
					t.Fatalf("D=%d h=%v P=%g: supported near-wall anchor did not take one step: %+v %v", dimensions, spacing, propagation, r, e)
				}
				dt := interfaceTimeStep(1, 0, g.hmin, dimensions)
				want := initial - float64(propagation)*dt
				if math.Abs(float64(phi[index])-want) > 1e-6*g.hmin {
					t.Errorf("D=%d h=%v P=%g: near-wall plane=%g want=%g", dimensions, spacing, propagation, phi[index], want)
				}
			}
		}
	}
}

func TestShapeInterfaceBoundaryRateContinuity(t *testing.T) {
	for _, dimensions := range []int{2, 3} {
		for _, spacing := range [][3]float64{{1, 1, 1}, {.5, 1, 2}} {
			size := [3]int{9, 9, 9}
			if dimensions == 2 {
				size[2] = 1
			}
			g := &levelSetGrid{ctx: context.Background(), nx: size[0], ny: size[1], nz: size[2], size: size, stride: [3]int{1, size[0], size[0] * size[1]}, n: size[0] * size[1] * size[2], dims: dimensions, h: spacing, hmin: spacing[0], hmax: spacing[dimensions-1]}
			for reflection := 0; reflection < 1<<(dimensions-1); reflection++ {
				for _, phase := range []float32{-1, 1} {
					for _, propagation := range []float32{-1, 0, 1} {
						for _, curvature := range []float32{0, .25} {
							var plane float32
							for _, kappa := range []float64{0, 1.0 / (1 << 18)} {
								phi, speed, allowed := make([]float32, g.n), make([]float32, g.n), make([]byte, g.n)
								point := [3]int{4, 1, 1}
								if dimensions == 2 {
									point[2] = 0
								}
								for axis := 1; axis < dimensions; axis++ {
									if reflection&(1<<(axis-1)) != 0 {
										point[axis] = size[axis] - 2
									}
								}
								for i := range phi {
									pos := [3]int{i % g.nx, (i / g.nx) % g.ny, i / (g.nx * g.ny)}
									value := float64(pos[0]-4) - .25
									boundary := false
									for axis := 1; axis < dimensions; axis++ {
										u := pos[axis]
										if reflection&(1<<(axis-1)) != 0 {
											u = size[axis] - 1 - u
										}
										value += kappa * float64(u*u) / 2
										boundary = boundary || u == 0
									}
									// The stored graph's two projection branches differ
									// by c for any positive kappa. Neither is changed here.
									if boundary {
										if kappa == 0 {
											allowed[i] = 4
										} else {
											value += math.Copysign(1, value)
											allowed[i] = 4
										}
									}
									phi[i], speed[i] = phase*float32(g.hmin*value), 1
								}
								s := &levelSetState{levelSetGrid: g, phi: phi}
								terms := levelSetTerms{propagation: propagation, speed: speed, curvature: curvature, curvatureWeight: speed}
								index := g.index(point[0], point[1], point[2])
								actual, first, diffusion := s.ratePotentialBoundary(index, terms, false, 0, 1e-6, allowed)
								gx := float64(phase) * g.hmin / spacing[0]
								grad2, norm2, numerator := gx*gx, gx*gx, 0.0
								var gradient, second [3]float64
								for axis := 1; axis < dimensions; axis++ {
									gradient[axis] = float64(phase) * .75 * g.hmin * kappa / spacing[axis]
									second[axis] = float64(phase) * 1.5 * g.hmin * kappa / (spacing[axis] * spacing[axis])
									norm2 += gradient[axis] * gradient[axis]
									if float64(propagation)*float64(phase) < 0 {
										d := 1.5 * g.hmin * kappa / spacing[axis]
										grad2 += d * d
									}
								}
								for axis := 1; axis < dimensions; axis++ {
									numerator += second[axis] * (norm2 - gradient[axis]*gradient[axis])
								}
								weight := float64(curvature) / float64(dimensions-1)
								want := -float64(propagation)*math.Sqrt(grad2) + weight*numerator/(norm2+1e-6)
								if math.IsNaN(float64(actual)) || math.IsInf(float64(actual), 0) || math.Abs(float64(actual)-want) > 1e-6 || first != math.Abs(float64(propagation)) || diffusion != weight {
									t.Fatalf("D=%d h=%v reflection=%d phase=%g P=%g C=%g kappa=%g rate=%g want=%g first=%g diffusion=%g", dimensions, spacing, reflection, phase, propagation, curvature, kappa, actual, want, first, diffusion)
								}
								if kappa == 0 {
									plane = actual
								} else if math.Abs(float64(actual-plane)) > 10*kappa {
									t.Fatalf("nonvanishing boundary rate jump: D=%d phase=%g P=%g C=%g delta=%g kappa=%g", dimensions, phase, propagation, curvature, actual-plane, kappa)
								}
							}
						}
					}
				}
			}
		}
	}
}

func TestShapeInterfaceBoundaryStencilPhaseAndCorners(t *testing.T) {
	for _, dimensions := range []int{2, 3} {
		for _, side := range []int{3, 9} {
			size := [3]int{side, side, side}
			if dimensions == 2 {
				size[2] = 1
			}
			g := &levelSetGrid{ctx: context.Background(), nx: size[0], ny: size[1], nz: size[2], size: size, stride: [3]int{1, size[0], size[0] * size[1]}, n: size[0] * size[1] * size[2], dims: dimensions, h: [3]float64{.5, 1, 2}, hmin: .5, hmax: 2}
			phi, allowed := make([]float32, g.n), make([]byte, g.n)
			for i := range phi {
				phi[i] = .25 + float32(i)*.001
			}
			s := &levelSetState{levelSetGrid: g, phi: phi}
			for corner := 0; corner < 1<<dimensions; corner++ {
				pos, projected := [3]int{}, [3]int{}
				for axis := 0; axis < dimensions; axis++ {
					projected[axis] = 1
					if corner&(1<<axis) != 0 {
						pos[axis], projected[axis] = side-1, side-2
					}
				}
				i, j := g.index(pos[0], pos[1], pos[2]), g.index(projected[0], projected[1], projected[2])
				original := phi[i]
				if s.interfaceStencilValue(i, allowed) != original {
					t.Fatal("ineligible actual boundary sample changed")
				}
				allowed[i] = 4
				if s.interfaceStencilValue(i, allowed) != phi[j] || phi[i] != original {
					t.Fatalf("D=%d size%d corner%d: virtual simultaneous projection or stored source changed", dimensions, side, corner)
				}
				phi[i] = -original
				if s.interfaceStencilValue(i, allowed) != phi[i] {
					t.Fatal("crossing born in current field was hidden by previous readiness")
				}
				phi[i], allowed[i] = original, 0
			}
		}
	}
}

func TestShapeInterfacePruningRebuildsFinalAnchorField(t *testing.T) {
	for _, dimensions := range []int{2, 3} {
		for _, spacing := range [][3]float64{{1, 1, 1}, {.5, 1, 2}} {
			size := [3]int{7, 7, 7}
			if dimensions == 2 {
				size[2] = 1
			}
			g := &levelSetGrid{ctx: context.Background(), nx: size[0], ny: size[1], nz: size[2], size: size, stride: [3]int{1, size[0], size[0] * size[1]}, n: size[0] * size[1] * size[2], dims: dimensions, h: spacing, hmin: spacing[0], hmax: spacing[dimensions-1]}
			for _, reflected := range []bool{false, true} {
				for _, phase := range []float32{-1, 1} {
					for _, ghostValue := range []float32{0, .1} {
						// Negative zero is outside; use a nonzero inside orphan
						// for the complementary phase rather than inventing -0.
						if phase < 0 && ghostValue == 0 {
							continue
						}
						phi, saved, member := make([]float32, g.n), make([]float32, g.n), make([]byte, g.n)
						s := &levelSetState{levelSetGrid: g, phi: phi, width: float32(float64(dimensions+1) * g.hmin), status: make([]byte, g.n), band: make([]int32, 0, g.n)}
						s.heap.maxItems = 7 * g.n
						active := make([]int32, 0, g.n)
						for i := range phi {
							pos := s.coordsArray(i)
							x := pos[0]
							if reflected {
								x = g.nx - 1 - x
							}
							phi[i] = phase * s.width
							if x >= 6 {
								phi[i] = -phi[i]
							}
							supported := pos[1] > 0 && pos[1] < g.ny-1 && (dimensions == 2 || pos[2] > 0 && pos[2] < g.nz-1)
							if x == 5 && supported {
								phi[i] = phase * float32(.4*g.hmin)
								active = append(active, int32(i))
							}
						}
						ghost := g.index(3, 3, size[2]/2)
						phi[ghost] = phase * ghostValue * float32(g.hmin)
						active = append(active, int32(ghost))
						slices.Sort(active)
						if e := s.refreshInterfaceLayers(active, saved, member); e != nil {
							t.Fatal(e)
						}
						planeX, sampleX := 5, 4
						if reflected {
							planeX, sampleX = 1, 2
						}
						anchor := g.index(planeX, 3, size[2]/2)
						neighbour := g.index(sampleX, 3, size[2]/2)
						ghostOffset := phase * float32((1+float64(ghostValue))*g.hmin)
						if math.Abs(float64(phi[neighbour]-ghostOffset)) > 1e-6*g.hmin {
							t.Fatalf("counterproof lacks ghost influence: %g want%g", phi[neighbour], ghostOffset)
						}
						// A normal crossing from the previous step can leave a
						// raw boundary value blocked despite current agreement.
						// Pruning a distant orphan must not re-enable that copy.
						remote := g.index(planeX, 0, size[2]/2)
						phi[remote] = phase * float32(1.4*g.hmin)
						remoteValue := phi[remote]
						phases, readiness := make([]bool, g.n), make([]byte, g.n)
						for i := range member {
							// Include both readiness states for each phase, even
							// when the current boundary/interior phases agree.
							pos, boundary := s.coordsArray(i), false
							for axis := 0; axis < dimensions; axis++ {
								boundary = boundary || pos[axis] == 0 || pos[axis] == size[axis]-1
							}
							if boundary && i%2 == 0 {
								member[i] |= 4
							}
							phases[i], readiness[i] = phi[i] < 0, member[i]&4
						}
						member[remote] &^= 4
						readiness[remote] = 0
						speed := make([]float32, g.n)
						for i := range speed {
							speed[i] = 1
						}
						terms := levelSetTerms{propagation: -phase, speed: speed}
						remoteAnchor := remote + g.stride[1]
						remoteRate, _, _ := s.ratePotentialBoundary(remoteAnchor, terms, false, 0, 1e-6, member)
						count := len(active)
						filtered, e := s.pruneInterfaceAnchors(active, saved, member)
						if e != nil || len(filtered) != count-1 || slices.Contains(filtered, int32(ghost)) || !slices.IsSorted(filtered) {
							t.Fatalf("D=%d phase%g ghost%g filtered=%v error=%v", dimensions, phase, ghostValue, filtered, e)
						}
						for i := range phi {
							if (phi[i] < 0) != phases[i] || member[i]&4 != readiness[i] || member[i]&8 != 0 {
								t.Fatalf("rebuild changed phase or temporal readiness: D=%d i%d phi%g member%d oldphase%t oldready%d", dimensions, i, phi[i], member[i], phases[i], readiness[i])
							}
						}
						newRemoteRate, _, _ := s.ratePotentialBoundary(remoteAnchor, terms, false, 0, 1e-6, member)
						if phi[remote] != remoteValue || newRemoteRate != remoteRate {
							t.Fatalf("distant pruning reset blocked projection: D=%d raw=%g want=%g rate=%g want=%g", dimensions, phi[remote], remoteValue, newRemoteRate, remoteRate)
						}
						want := phase * float32(1.4*g.hmin)
						if math.Abs(float64(phi[neighbour]-want)) > 1e-6*g.hmin {
							t.Fatalf("final graph retained removed ghost: phi=%g want=%g", phi[neighbour], want)
						}
						rate, _, _ := s.ratePotentialBoundary(anchor, terms, false, 0, 1e-6, member)
						if math.Abs(float64(rate-phase)) > 1e-6 {
							t.Fatalf("D=%d h=%v phase%g reflected%t surviving physical plane rate=%g want=%g", dimensions, spacing, phase, reflected, rate, phase)
						}
					}
				}
			}
		}
	}
}

func TestShapeInterfacePruningRetainsZeroTouchingInside(t *testing.T) {
	g := &levelSetGrid{ctx: context.Background(), nx: 5, ny: 5, nz: 1, size: [3]int{5, 5, 1}, stride: [3]int{1, 5, 25}, n: 25, dims: 2, h: [3]float64{1, 1, 1}, hmin: 1, hmax: 1}
	phi := make([]float32, g.n)
	for i := range phi {
		phi[i] = 1
	}
	anchor := g.index(2, 2, 0)
	phi[anchor], phi[anchor-1] = 0, -1
	s := &levelSetState{levelSetGrid: g, phi: phi}
	active, e := s.pruneInterfaceAnchors([]int32{int32(anchor)}, nil, nil)
	if e != nil || len(active) != 1 || int(active[0]) != anchor || phi[anchor] != 0 {
		t.Fatalf("zero touching negative lost: %v %v", active, e)
	}
	phi[anchor-1] = 1
	active, e = s.pruneInterfaceAnchors(active, nil, nil)
	if e != nil || len(active) != 0 {
		t.Fatalf("isolated outside zero survived: %v %v", active, e)
	}
}

func TestShapeInterfaceExactRuns(t *testing.T) {
	for _, dims := range [][3]int{{1, 7, 3}, {13, 9, 3}, {7, 1, 1}} {
		g := &levelSetGrid{ctx: context.Background(), volume: LevelSetVolume{Columns: dims[0] + 4, Rows: 100, Slices: dims[2] + 2}, nx: dims[0], ny: dims[1], nz: dims[2], size: dims, stride: [3]int{1, dims[0], dims[0] * dims[1]}, origin: [3]int{2, 41, 1}, n: dims[0] * dims[1] * dims[2], dims: 3, h: [3]float64{1, 1, 1}}
		phi := make([]float32, g.n)
		for i := range phi {
			phi[i] = 1
			if i%2 == 0 {
				phi[i] = -1
			}
		}
		r, e := g.interfaceResult(phi)
		if e != nil {
			t.Fatal(e)
		}
		for z, m := range r.Masks {
			if len(m.runs) != 100 || cap(m.runs) != 100 {
				t.Fatal("source-height header")
			}
			for y, row := range m.runs {
				if cap(row) != len(row) {
					t.Fatal("uncapped arena view")
				}
				for _, run := range row {
					for x := run.Start; x < run.End; x++ {
						i := g.index(x-g.origin[0], y-g.origin[1], z-g.origin[2])
						if phi[i] >= 0 {
							t.Fatal("wrong run")
						}
					}
				}
			}
		}
		for i := range phi {
			phi[i] = 1
		}
		empty, e := g.interfaceResult(phi)
		if e != nil || empty.VoxelCount != 0 || len(empty.Masks) != 0 {
			t.Fatal("empty exact output")
		}
	}
}

func TestShapeInterfaceAnalyticCurvatureAndCFL(t *testing.T) {
	for _, dims := range [][3]int{{97, 65, 1}, {97, 65, 49}} {
		h, d := [3]float64{.5, 1, 1.5}, 3
		if dims[2] == 1 {
			d = 2
			h[2] = 1
		}
		g := &levelSetGrid{ctx: context.Background(), nx: dims[0], ny: dims[1], nz: dims[2], size: dims, stride: [3]int{1, dims[0], dims[0] * dims[1]}, n: dims[0] * dims[1] * dims[2], dims: d, h: h, hmin: .5, hmax: h[d-1]}
		phi, potential := make([]float32, g.n), make([]float32, g.n)
		for i := range phi {
			x, y, z := g.coords(i)
			dx, dy, dz := float64(x-dims[0]/2)*h[0], float64(y-dims[1]/2)*h[1], float64(z-dims[2]/2)*h[2]
			phi[i] = float32(math.Sqrt(dx*dx+dy*dy+dz*dz) - 16)
			potential[i] = 1
		}
		s := &levelSetState{levelSetGrid: g, phi: phi}
		i := g.index(dims[0]/2+32, dims[1]/2, dims[2]/2)
		for _, scale := range []float32{1, float32(d - 1)} {
			rate, first, c := s.rate(i, levelSetTerms{curvature: scale, curvatureWeight: potential})
			expected := float64(scale) / 16
			if first != 0 || rate <= 0 || math.Abs(float64(rate)-expected) > expected*.10 {
				t.Fatalf("physical curvature d=%d: rate=%g expected=%g", d, rate, expected)
			}
			dt := interfaceTimeStep(first, c, g.hmin, g.dims)
			if dt*c*2*float64(d)/(g.hmin*g.hmin) > 1+1e-12 {
				t.Fatal("diffusion bound")
			}
		}
		dt := interfaceTimeStep(1, .2, g.hmin, d)
		if dt > .5*g.hmin || dt*.2*2*float64(d)/(g.hmin*g.hmin) > 1 {
			t.Fatal("combined physical CFL bound")
		}
	}
}

// The polynomial and power-of-two spacings make central and mixed derivatives
// exact in float32. This probes the old cutoff from both sides and the new
// denominator independently of the public corpus and layer maintenance.
func TestShapeInterfaceRegularizedCurvature(t *testing.T) {
	const length = 8.0 // mm; epsilon and gradient-square regularization are dimensionless.
	for _, dimensions := range []int{2, 3} {
		for _, spacing := range [][3]float64{{1, 1, 1}, {.5, 1, 2}} {
			for quadratic := 1; quadratic < dimensions; quadratic++ {
				for _, epsilon := range []float64{0, math.Ldexp(1, -20), math.Ldexp(1, -19), .5} {
					size := [3]int{5, 5, 5}
					if dimensions == 2 {
						size[2] = 1
					}
					g := &levelSetGrid{ctx: context.Background(), nx: size[0], ny: size[1], nz: size[2], size: size, stride: [3]int{1, size[0], size[0] * size[1]}, n: size[0] * size[1] * size[2], dims: dimensions, h: spacing, hmin: spacing[0], hmax: spacing[dimensions-1]}
					phi, potential := make([]float32, g.n), make([]float32, g.n)
					for i := range phi {
						x, y, z := g.coords(i)
						dx, dy, dz := float64(x-2)*spacing[0], float64(y-2)*spacing[1], float64(z-size[2]/2)*spacing[2]
						value := epsilon*dx + dy*dy/(2*length)
						if quadratic == 2 {
							value += dz * dz / (2 * length)
						}
						phi[i], potential[i] = float32(value), 1
					}
					s := &levelSetState{levelSetGrid: g, phi: phi}
					index := g.index(2, 2, size[2]/2)
					for _, propagation := range []float32{-1, 0, 1} {
						for _, curvature := range []float32{0, .25, .25 * float32(dimensions-1)} {
							for _, sampled := range []bool{false, true} {
								factor := 1.0
								if sampled {
									factor = .5
								}
								terms := levelSetTerms{propagation: propagation, speed: potential, curvature: curvature, curvatureWeight: potential}
								q := epsilon * epsilon
								gradientSquare := q
								if propagation < 0 {
									for axis := 1; axis <= quadratic; axis++ {
										slope := spacing[axis] / (2 * length)
										gradientSquare += 2 * slope * slope
									}
								}
								propagationRate := 0.0
								if propagation != 0 {
									propagationRate -= float64(propagation) * factor * math.Sqrt(gradientSquare)
								}
								weight := float64(curvature) * factor / float64(dimensions-1)
								numerator := float64(quadratic) * q / length
								for _, regularization := range []float64{0, 1e-6} {
									want := propagationRate
									if regularization > 0 {
										want += weight * numerator / (q + regularization)
									} else if q > 1e-12 {
										want += weight * numerator / q
									}
									rate, first, c := s.ratePotentialRegularized(index, terms, sampled, .5, regularization)
									if math.Float32bits(rate) != math.Float32bits(float32(want)) || first != math.Abs(float64(propagation))*factor || c != math.Abs(weight) {
										t.Fatalf("D=%d h=%v k=%d epsilon=%g P=%g C=%g sample=%t r=%g: rate=%g want=%g first=%g c=%g", dimensions, spacing, quadratic, epsilon, propagation, curvature, sampled, regularization, rate, float32(want), first, c)
									}
									if regularization == 0 {
										oldRate, oldFirst, oldCurvature := s.ratePotential(index, terms, sampled, .5)
										if math.Float32bits(oldRate) != math.Float32bits(rate) || oldFirst != first || oldCurvature != c {
											t.Fatal("historical facade differs from zero-regularization branch")
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
}

type shapeOracleCase struct {
	ID       string     `json:"id"`
	Family   string     `json:"family"`
	Dims     [3]int     `json:"dims"`
	Spacing  [3]float64 `json:"spacing_mm"`
	Feature  string     `json:"feature_blob"`
	Initial  string     `json:"initial_phi_blob"`
	Settings struct {
		Propagation float64 `json:"propagation"`
		Curvature   float64 `json:"curvature"`
		Iterations  int     `json:"maximum_iterations"`
		RMS         float64 `json:"maximum_rms"`
	} `json:"settings"`
	Comparison struct {
		Role     string  `json:"role"`
		Dice     float64 `json:"minimum_dice"`
		Distance float64 `json:"max_symmetric_boundary_distance_in_max_spacing"`
	} `json:"comparison"`
}

// shapeInterfaceDistanceWitness records the actual surface-distance witness
// for a failed frozen criterion. It uses the same six-neighbour surfaces as
// CompareMasks, and does not change either input or the acceptance threshold.
func shapeInterfaceDistanceWitness(t *testing.T, got, want *VolumeMask) [3]int {
	t.Helper()
	maximum, furthest := -1.0, [3]int{}
	for direction, pair := range [][2]*VolumeMask{{got, want}, {want, got}} {
		ctx := context.Background()
		from, e := pair[0].surface(ctx)
		if e != nil {
			t.Fatal(e)
		}
		to, e := pair[1].surface(ctx)
		if e != nil {
			t.Fatal(e)
		}
		distance, e := to.DistanceTransform(ctx, true)
		if e != nil {
			t.Fatal(e)
		}
		witness, far := -1, -1.0
		for i, inside := range from.Voxels {
			if inside && distance[i] > far {
				witness, far = i, distance[i]
			}
		}
		coordinates := func(i int) [3]int {
			return [3]int{i % got.Columns, i / got.Columns % got.Rows, i / (got.Columns * got.Rows)}
		}
		p := coordinates(witness)
		nearest, best := -1, math.Inf(1)
		for i, inside := range to.Voxels {
			if !inside {
				continue
			}
			q := coordinates(i)
			d2 := math.Pow(got.X[p[0]]-got.X[q[0]], 2) + math.Pow(got.Y[p[1]]-got.Y[q[1]], 2) + math.Pow(got.Z[p[2]]-got.Z[q[2]], 2)
			if d2 < best {
				nearest, best = i, d2
			}
		}
		if far > maximum {
			maximum, furthest = far, p
		}
		t.Logf("SHAPE_DISTANCE_WITNESS direction=%d from=%v nearest=%v distance=%.9g", direction, p, coordinates(nearest), far)
	}
	packed := make([]byte, (len(got.Voxels)+7)/8)
	for i, inside := range got.Voxels {
		if inside {
			packed[i/8] |= 1 << uint(i%8)
		}
	}
	t.Logf("SHAPE_FAILED_MASK hex=%s", hex.EncodeToString(packed))
	return furthest
}

// Diagnostic replay observes the same frozen failed input, without running
// the public oracle, changing parameters, or replacing the original result.
func shapeInterfaceFailedField(t *testing.T, volume LevelSetVolume, init LevelSetInitialization, p ShapeDetectionParams, point [3]int) {
	t.Helper()
	g, e := newEdgeLevelSetGridProfile(nil, volume, init, LevelSetOptions{MarginMM: 1000}, true, true)
	if e != nil {
		t.Fatal(e)
	}
	edge := append([]float32(nil), g.image...)
	phi, e := g.initialLevelSet(init, edge)
	if e != nil {
		t.Fatal(e)
	}
	c, e := edgeCurvature(p.CurvatureScaling, p.CurvatureConvention, volume.Slices)
	if e != nil {
		t.Fatal(e)
	}
	terms := levelSetTerms{stopping: p.Stopping, propagation: float32(p.PropagationScaling), curvature: float32(c), speed: edge, curvatureWeight: edge}
	_, e = g.evolveInterface(phi, terms, p.SurfaceSampling, p.MaxIterations, p.MaximumRMSChange, func(progress LevelSetProgress) {
		if progress.Iteration != 1 && progress.Iteration != 30 && progress.Iteration != 60 && progress.Iteration != 90 && progress.Iteration != p.MaxIterations {
			return
		}
		for y := max(0, point[1]-2); y <= min(g.ny-1, point[1]+2); y++ {
			values := []float32{}
			for x := max(0, point[0]-4); x <= min(g.nx-1, point[0]+2); x++ {
				values = append(values, phi[g.index(x, y, point[2])])
			}
			t.Logf("SHAPE_FIELD iteration=%d row=%d x0=%d phi=%v", progress.Iteration, y, max(0, point[0]-4), values)
		}
	})
	if e != nil {
		t.Fatal(e)
	}
}

func TestShapeInterfacePublicCorpus(t *testing.T) {
	raw, e := os.ReadFile("testdata/shape_weak_edge/itk-5.4.json")
	if e != nil {
		t.Fatal(e)
	}
	var doc struct {
		Inputs struct {
			Cases []shapeOracleCase `json:"cases"`
			Blobs map[string]struct {
				Bytes    []byte `json:"base64"`
				Count    int    `json:"count"`
				Encoding string `json:"encoding"`
			} `json:"blobs"`
		} `json:"inputs"`
		Public struct {
			Cases []struct {
				ID   string `json:"id"`
				Mask []byte `json:"mask_base64"`
			} `json:"cases"`
		} `json:"public_outputs"`
	}
	if e = json.Unmarshal(raw, &doc); e != nil {
		t.Fatal(e)
	}
	if len(doc.Inputs.Cases) != 504 || len(doc.Public.Cases) != 504 || len(doc.Inputs.Blobs) != 77 {
		t.Fatal("sealed cardinality")
	}
	for digest, b := range doc.Inputs.Blobs {
		sum := sha256.Sum256(b.Bytes)
		if hex.EncodeToString(sum[:]) != digest {
			t.Fatal("input blob changed")
		}
	}
	decode := func(key string) []float32 {
		b := doc.Inputs.Blobs[key]
		if len(b.Bytes) != 4*b.Count {
			t.Fatal("float payload")
		}
		out := make([]float32, b.Count)
		for i := range out {
			out[i] = math.Float32frombits(binary.LittleEndian.Uint32(b.Bytes[i*4:]))
			if math.IsNaN(float64(out[i])) || math.IsInf(float64(out[i]), 0) {
				t.Fatal("nonfinite authored input")
			}
		}
		return out
	}
	for ordinal, c := range doc.Inputs.Cases {
		expected := doc.Public.Cases[ordinal]
		if expected.ID != c.ID {
			t.Fatal("sealed order")
		}
		t.Run(c.ID, func(t *testing.T) {
			feature, initial := decode(c.Feature), decode(c.Initial)
			nx, ny, nz := c.Dims[0], c.Dims[1], c.Dims[2]
			if len(feature) != nx*ny*nz || len(initial) != len(feature) || len(expected.Mask) != (len(feature)+7)/8 {
				t.Fatal("sealed dimensions")
			}
			masks := make(map[int]*RasterMask, nz)
			for z := 0; z < nz; z++ {
				m := NewRasterMask(nx, ny)
				nonempty := false
				for y := 0; y < ny; y++ {
					start := -1
					for x := 0; x <= nx; x++ {
						in := x < nx && initial[x+nx*(y+ny*z)] < 0
						if in {
							nonempty = true
							if start < 0 {
								start = x
							}
						} else if start >= 0 {
							m.SetRun(y, start, x)
							start = -1
						}
					}
				}
				if nonempty {
					masks[z] = m
				}
			}
			volume := LevelSetVolume{Columns: nx, Rows: ny, Slices: nz, SpacingMM: c.Spacing, ValueAt: func(x, y, z int) (float64, bool) { return float64(feature[x+nx*(y+ny*z)]), true }}
			p := ShapeDetectionParams{Evolution: InterfaceLocal, Initialization: BoundedFaceNormalSeeds, SurfaceSampling: GridCentres, Input: EdgePotentialImage, Stopping: RMSOnly, CurvatureConvention: SumCurvature, PropagationScaling: c.Settings.Propagation, CurvatureScaling: c.Settings.Curvature, MaxIterations: c.Settings.Iterations, MaximumRMSChange: c.Settings.RMS}
			r, e := ShapeDetectionLevelSet(nil, volume, LevelSetInitialization{Masks: masks}, p, LevelSetOptions{MarginMM: 1000})
			if e != nil {
				t.Fatal(e)
			}
			got, want := NewVolumeMask(nx, ny, nz, c.Spacing), NewVolumeMask(nx, ny, nz, c.Spacing)
			for i := range got.Voxels {
				x, y, z := i%nx, i/nx%ny, i/(nx*ny)
				if m := r.Masks[z]; m != nil {
					got.Voxels[i] = m.Get(x, y)
				}
				want.Voxels[i] = expected.Mask[i/8]&(1<<uint(i%8)) != 0
			}
			// Surface distance is undefined for one empty foreground. Record
			// an explicit infinity sentinel; two empty sets match exactly.
			metrics := OverlapMetrics{}
			gotCount, wantCount := got.Count(), want.Count()
			if gotCount == 0 && wantCount == 0 {
				metrics.Dice = 1
			} else if gotCount == 0 || wantCount == 0 {
				metrics.HausdorffMM = math.Inf(1)
			} else {
				metrics, e = CompareMasks(context.Background(), got, want)
				if e != nil {
					t.Fatal(e)
				}
			}
			if math.IsNaN(metrics.Dice) || math.IsInf(metrics.Dice, 0) || math.IsNaN(metrics.HausdorffMM) || math.IsNaN(r.RMSChange) || math.IsInf(r.RMSChange, 0) {
				t.Fatal("nonfinite overlap/update observation")
			}
			pass := metrics.Dice >= c.Comparison.Dice && metrics.HausdorffMM <= c.Comparison.Distance*max(c.Spacing[0], c.Spacing[1], c.Spacing[2])
			reason := map[LevelSetStopReason]string{LevelSetStopIterationLimit: "iteration_limit", LevelSetStopRMS: "rms", LevelSetStopStableMask: "stable_mask", LevelSetStopEmptyBand: "empty_active", LevelSetStopZeroTimeStep: "zero_dt", LevelSetStopBoundaryActiveExhausted: "boundary_active_exhausted"}[r.StopReason]
			t.Logf("SHAPE_METRIC id=%s family=%s criteria_met=%t accepted_for_qualification=%t dice=%.9f hd=%.9f go_count=%d itk_count=%d iterations=%d rms=%.9g reason=%s converged=%t crop_contact=%t source_contact=%t", c.ID, c.Family, pass, pass && c.Comparison.Role == "geometric qualification", metrics.Dice, metrics.HausdorffMM, r.VoxelCount, wantCount, r.Iterations, r.RMSChange, reason, r.Converged, r.TouchesRegionBoundary, r.TouchesVolumeBoundary)
			if c.Comparison.Role == "geometric qualification" && !pass {
				if gotCount > 0 && wantCount > 0 {
					point := shapeInterfaceDistanceWitness(t, got, want)
					shapeInterfaceFailedField(t, volume, LevelSetInitialization{Masks: masks}, p, point)
				}
				t.Errorf("frozen geometric criteria RED: Dice %.9f HD %.9f", metrics.Dice, metrics.HausdorffMM)
			}
		})
	}
}

func TestShapeInterfaceGlobalStepBounds(t *testing.T) {
	for _, d := range []int{2, 3} {
		for _, h := range []float64{.5, 1, 1.5} {
			for _, p := range []float64{0, .01, 1, 4} {
				for _, c := range []float64{0, .2, 1, 4} {
					dt := interfaceTimeStep(p, c, h, d)
					if p == 0 && c == 0 {
						if dt != 0 {
							t.Fatal("zero terms")
						}
						continue
					}
					if !(dt > 0) || math.IsInf(dt, 0) || math.IsNaN(dt) {
						t.Fatal("finite positive step")
					}
					if p > 0 && dt*p/h > 1/(2*float64(d))+1e-12 {
						t.Fatal("global propagation constraint")
					}
					if c > 0 && dt*c/(h*h) > 1/(2*float64(d))+1e-12 {
						t.Fatal("global curvature constraint")
					}
					if dt*(math.Sqrt(2*float64(d))*p/h+2*float64(d)*c/(h*h)) > 1+1e-12 {
						t.Fatal("combined principal bound at corner/extremum")
					}
				}
			}
		}
	}
}
