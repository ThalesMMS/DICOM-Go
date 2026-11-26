package render

import (
	"context"
	"errors"
	"math"
	"reflect"
	"testing"
)

func uniformFastMarchingGrid(nx, ny, nz int, spacing Vec3, speed float64) FastMarchingGrid {
	speeds := make([]float64, nx*ny*nz)
	for i := range speeds {
		speeds[i] = speed
	}
	return FastMarchingGrid{Nx: nx, Ny: ny, Nz: nz, Spacing: spacing, Speed: speeds}
}

// itkFastMarchingSeeds is the seed layout of itkFastMarchingTest.cxx and
// itkFastMarchingUpwindGradientTest.cxx (ITK 35b196e13c): an alive point at
// (28, 35) and its four neighbours as trial points of value 1.
func itkFastMarchingSeeds() []FastMarchingSeed {
	return []FastMarchingSeed{
		{Index: [3]int{28, 35, 0}, Value: 0},
		{Index: [3]int{29, 35, 0}, Value: 1},
		{Index: [3]int{28, 36, 0}, Value: 1},
		{Index: [3]int{27, 35, 0}, Value: 1},
		{Index: [3]int{28, 34, 0}, Value: 1},
	}
}

func TestFastMarchingUnitSpeedApproximatesEuclideanDistanceITK(t *testing.T) {
	result, err := FastMarching(context.Background(), uniformFastMarchingGrid(64, 64, 1, Vec3{X: 1, Y: 1, Z: 1}, 1), itkFastMarchingSeeds(), FastMarchingOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if result.Frozen != 64*64 {
		t.Fatalf("froze %d nodes", result.Frozen)
	}
	worst := 0.0
	for y := 0; y < 64; y++ {
		for x := 0; x < 64; x++ {
			distance := math.Hypot(float64(x-28), float64(y-35))
			if distance == 0 {
				continue
			}
			value := result.Times[y*64+x]
			// itkFastMarchingTest.cxx accepts |T|/distance up to 1.42.
			if ratio := math.Abs(value) / distance; ratio > 1.42 {
				t.Fatalf("(%d,%d): T=%g distance=%g", x, y, value, distance)
			}
			if distance >= 10 {
				worst = math.Max(worst, (value-distance)/distance)
			}
			if (x == 28 || y == 35) && math.Abs(value-distance) > 1e-12 {
				t.Fatalf("axis node (%d,%d): T=%g, want %g", x, y, value, distance)
			}
		}
	}
	// Away from the seed, first-order fast marching overestimates diagonals
	// by a few percent.
	if worst > 0.1 {
		t.Fatalf("worst relative overestimate %.3f", worst)
	}
	if testing.Verbose() {
		t.Logf("worst relative overestimate beyond 10 px: %.4f", worst)
	}
}

func TestFastMarchingUpwindGradientIsRadialWithEikonalNormITK(t *testing.T) {
	result, err := FastMarching(context.Background(), uniformFastMarchingGrid(64, 64, 1, Vec3{X: 1, Y: 1, Z: 1}, 1), itkFastMarchingSeeds(), FastMarchingOptions{})
	if err != nil {
		t.Fatal(err)
	}
	// The norm check of itkFastMarchingUpwindGradientTest.cxx holds exactly:
	// the upwind differences are those the Eikonal update solved.
	minDot := 1.0
	for y := 0; y < 64; y++ {
		for x := 0; x < 64; x++ {
			dx, dy := float64(x-28), float64(y-35)
			distance := math.Hypot(dx, dy)
			if distance == 0 {
				continue
			}
			g, ok := result.Gradient([3]int{x, y, 0})
			if !ok {
				t.Fatalf("no gradient at (%d,%d)", x, y)
			}
			if norm := g.Length(); math.Abs(norm-1) > 1e-4 {
				t.Fatalf("(%d,%d): |∇T| = %g", x, y, norm)
			}
			minDot = math.Min(minDot, (dx*g.X+dy*g.Y)/distance)
		}
	}
	// ITK's test also asks for a radial direction within 0.99; the
	// first-order stencil meets it except next to the seed diagonals.
	if minDot < 0.9 {
		t.Fatalf("gradient deviates from radial: min dot %.3f", minDot)
	}
}

func TestFastMarchingAnisotropicSpacingAndSpeed(t *testing.T) {
	spacing := Vec3{X: 0.5, Y: 1, Z: 2}
	result, err := FastMarching(context.Background(), uniformFastMarchingGrid(21, 11, 6, spacing, 2),
		[]FastMarchingSeed{{Index: [3]int{0, 0, 0}}}, FastMarchingOptions{})
	if err != nil {
		t.Fatal(err)
	}
	for _, c := range []struct {
		index [3]int
		mm    float64
	}{{[3]int{20, 0, 0}, 10}, {[3]int{0, 10, 0}, 10}, {[3]int{0, 0, 5}, 10}} {
		got := result.Times[(c.index[2]*11+c.index[1])*21+c.index[0]]
		if math.Abs(got-c.mm/2) > 1e-12 {
			t.Fatalf("T%v = %g, want %g", c.index, got, c.mm/2)
		}
	}
	// A diagonal is close to the Euclidean distance over the speed.
	diagonal := result.Times[(5*11+10)*21+20]
	if want := math.Sqrt(3*100) / 2; diagonal < want || diagonal > want*1.1 {
		t.Fatalf("diagonal T = %g, want about %g", diagonal, want)
	}
}

func TestFastMarchingBlockedNodesAndStopping(t *testing.T) {
	grid := uniformFastMarchingGrid(20, 20, 1, Vec3{X: 1, Y: 1, Z: 1}, 1)
	// A wall at x = 10 with a gap at y = 19 forces a detour.
	for y := 0; y < 19; y++ {
		grid.Speed[y*20+10] = 0
	}
	result, err := FastMarching(context.Background(), grid, []FastMarchingSeed{{Index: [3]int{5, 0, 0}}}, FastMarchingOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if !math.IsInf(result.Times[5*20+10], 1) {
		t.Fatal("a blocked node was reached")
	}
	if got := result.Times[0*20+15]; got < 19+19 {
		t.Fatalf("T behind the wall = %g, want a detour through the gap", got)
	}
	stopped, err := FastMarching(context.Background(), grid, []FastMarchingSeed{{Index: [3]int{5, 0, 0}}}, FastMarchingOptions{StoppingValue: 3})
	if err != nil {
		t.Fatal(err)
	}
	for i, value := range stopped.Times {
		if !math.IsInf(value, 1) && value > 3 {
			t.Fatalf("node %d frozen at %g beyond the stopping value", i, value)
		}
	}
	targeted, err := FastMarching(context.Background(), uniformFastMarchingGrid(50, 50, 1, Vec3{X: 1, Y: 1, Z: 1}, 1),
		[]FastMarchingSeed{{Index: [3]int{0, 0, 0}}}, FastMarchingOptions{Targets: [][3]int{{5, 5, 0}}})
	if err != nil || !targeted.TargetsReached || targeted.Frozen >= 2500 {
		t.Fatalf("targeted march: reached %v, frozen %d, %v", targeted.TargetsReached, targeted.Frozen, err)
	}
	if _, err := FastMarching(context.Background(), uniformFastMarchingGrid(50, 50, 1, Vec3{X: 1, Y: 1, Z: 1}, 1),
		[]FastMarchingSeed{{Index: [3]int{0, 0, 0}}}, FastMarchingOptions{MaxFrozen: 10}); !errors.As(err, new(*FastMarchingLimitError)) {
		t.Fatalf("MaxFrozen: %v", err)
	}
}

func TestFastMarchingMinimalPathIsStraightInUniformSpeed(t *testing.T) {
	spacing := Vec3{X: 0.7, Y: 1.3, Z: 1}
	result, err := FastMarching(context.Background(), uniformFastMarchingGrid(60, 40, 1, spacing, 1),
		[]FastMarchingSeed{{Index: [3]int{5, 5, 0}}}, FastMarchingOptions{})
	if err != nil {
		t.Fatal(err)
	}
	from := Vec3{X: 52.4, Y: 33.6}
	path, err := result.MinimalPath(context.Background(), from, 0.25)
	if err != nil {
		t.Fatal(err)
	}
	if path[0] != from || path[len(path)-1] != (Vec3{X: 5, Y: 5}) {
		t.Fatalf("path ends %+v .. %+v", path[0], path[len(path)-1])
	}
	a := Vec3{X: from.X * spacing.X, Y: from.Y * spacing.Y}
	b := Vec3{X: 5 * spacing.X, Y: 5 * spacing.Y}
	worst := 0.0
	for _, p := range path {
		worst = math.Max(worst, distanceToSegment(Vec3{X: p.X * spacing.X, Y: p.Y * spacing.Y}, a, b))
	}
	if worst > 0.5 {
		t.Fatalf("path leaves the straight line by %.3f mm", worst)
	}
}

func TestFastMarchingInputErrorsAndCancellation(t *testing.T) {
	good := uniformFastMarchingGrid(4, 4, 1, Vec3{X: 1, Y: 1, Z: 1}, 1)
	seed := []FastMarchingSeed{{Index: [3]int{1, 1, 0}}}
	for name, run := range map[string]func() error{
		"size": func() error {
			g := good
			g.Nx = 0
			_, err := FastMarching(context.Background(), g, seed, FastMarchingOptions{})
			return err
		},
		"speeds": func() error {
			g := good
			g.Speed = g.Speed[:3]
			_, err := FastMarching(context.Background(), g, seed, FastMarchingOptions{})
			return err
		},
		"spacing": func() error {
			g := good
			g.Spacing.Y = 0
			_, err := FastMarching(context.Background(), g, seed, FastMarchingOptions{})
			return err
		},
		"no seed": func() error {
			_, err := FastMarching(context.Background(), good, nil, FastMarchingOptions{})
			return err
		},
		"outside seed": func() error {
			_, err := FastMarching(context.Background(), good, []FastMarchingSeed{{Index: [3]int{4, 0, 0}}}, FastMarchingOptions{})
			return err
		},
		"target": func() error {
			_, err := FastMarching(context.Background(), good, seed, FastMarchingOptions{Targets: [][3]int{{0, 9, 0}}})
			return err
		},
	} {
		if err := run(); !errors.Is(err, ErrFastMarchingInput) {
			t.Errorf("%s: %v", name, err)
		}
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := FastMarching(ctx, good, seed, FastMarchingOptions{}); !errors.Is(err, context.Canceled) {
		t.Fatalf("canceled: %v", err)
	}
	result, err := FastMarching(context.Background(), good, seed, FastMarchingOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := result.MinimalPath(context.Background(), Vec3{X: 9}, 0.5); !errors.Is(err, ErrFastMarchingInput) {
		t.Fatalf("outside path start: %v", err)
	}
}

// Err is polled at deterministic boundaries; no timers or subprocesses.
type fmInitializationContext struct {
	context.Context
	polls, limit int
}

func (c *fmInitializationContext) Err() error {
	c.polls++
	if c.polls >= c.limit {
		return context.Canceled
	}
	return nil
}
func TestFastMarchingCancelsInitializationAndFinalCleanup(t *testing.T) {
	grid := uniformFastMarchingGrid(128, 128, 1, Vec3{X: 1, Y: 1, Z: 1}, 1)
	seed := []FastMarchingSeed{{Index: [3]int{64, 64, 0}}}
	for _, limit := range []int{1, 5, 10} {
		ctx := &fmInitializationContext{Context: context.Background(), limit: limit}
		result, e := FastMarching(ctx, grid, seed, FastMarchingOptions{StoppingValue: .1})
		if result != nil || !errors.Is(e, context.Canceled) {
			t.Fatalf("poll limit%d result%v err%v polls%d", limit, result, e, ctx.polls)
		}
	}
	// With a tiny stopping time, the four initial pos polls, four time polls,
	// first heap poll and cleanup polls identify the final stage exactly.
}

func TestFastMarchingFixedBuffersMatchesLegacy(t *testing.T) {
	spacing2D := Vec3{X: .7, Y: 1.4, Z: 2.3}
	spacing3D := Vec3{X: .5, Y: 1.25, Z: 2}
	grid2D := uniformFastMarchingGrid(9, 7, 1, spacing2D, 1)
	grid3D := uniformFastMarchingGrid(7, 5, 4, spacing3D, 1)
	for _, grid := range []FastMarchingGrid{grid2D, grid3D} {
		for i := range grid.Speed {
			grid.Speed[i] = .5 + float64(i%7)/4
		}
	}
	for y := 0; y < 6; y++ {
		grid2D.Speed[y*grid2D.Nx+4] = 0
	}
	grid2D.Speed[2*grid2D.Nx+4] = math.NaN()
	grid2D.Speed[3*grid2D.Nx+4] = math.Inf(1)
	for z := 0; z < 3; z++ {
		for y := 0; y < 4; y++ {
			grid3D.Speed[(z*grid3D.Ny+y)*grid3D.Nx+3] = 0
		}
	}
	seeds2D := []FastMarchingSeed{
		{Index: [3]int{1, 1, 0}, Value: 3},
		{Index: [3]int{7, 5, 0}, Value: .5},
		{Index: [3]int{1, 1, 0}, Value: 0},
		{Index: [3]int{1, 1, 0}, Value: 2},
	}
	seeds3D := []FastMarchingSeed{
		{Index: [3]int{1, 1, 1}, Value: 3},
		{Index: [3]int{5, 3, 2}, Value: .5},
		{Index: [3]int{1, 1, 1}, Value: 0},
		{Index: [3]int{1, 1, 1}, Value: 2},
	}
	isolated := uniformFastMarchingGrid(7, 5, 1, spacing2D, 1)
	for y := 0; y < isolated.Ny; y++ {
		isolated.Speed[y*isolated.Nx+3] = 0
	}
	for _, tc := range []struct {
		name    string
		grid    FastMarchingGrid
		seeds   []FastMarchingSeed
		options FastMarchingOptions
	}{
		{"2D anisotropic full", grid2D, seeds2D, FastMarchingOptions{}},
		{"2D anisotropic stop", grid2D, seeds2D, FastMarchingOptions{StoppingValue: 4}},
		{"2D anisotropic targets", grid2D, seeds2D, FastMarchingOptions{Targets: [][3]int{{2, 2, 0}, {6, 5, 0}, {2, 2, 0}}}},
		{"3D anisotropic full", grid3D, seeds3D, FastMarchingOptions{}},
		{"3D anisotropic stop", grid3D, seeds3D, FastMarchingOptions{StoppingValue: 4}},
		{"3D anisotropic targets", grid3D, seeds3D, FastMarchingOptions{Targets: [][3]int{{2, 2, 1}, {5, 3, 3}, {2, 2, 1}}}},
		{"2D tied fronts", uniformFastMarchingGrid(9, 9, 1, Vec3{X: 1, Y: 1, Z: 1}, 1),
			[]FastMarchingSeed{{Index: [3]int{2, 4, 0}}, {Index: [3]int{6, 4, 0}}, {Index: [3]int{2, 4, 0}}}, FastMarchingOptions{}},
		{"3D tied fronts", uniformFastMarchingGrid(7, 7, 5, Vec3{X: 1, Y: 1, Z: 1}, 1),
			[]FastMarchingSeed{{Index: [3]int{1, 3, 2}}, {Index: [3]int{5, 3, 2}}, {Index: [3]int{1, 3, 2}}}, FastMarchingOptions{}},
		{"unreachable target", isolated, []FastMarchingSeed{{Index: [3]int{1, 2, 0}}}, FastMarchingOptions{Targets: [][3]int{{5, 2, 0}}}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			legacy, err := FastMarching(context.Background(), tc.grid, tc.seeds, tc.options)
			if err != nil {
				t.Fatal(err)
			}
			fixed, err := FastMarchingFixedBuffers(context.Background(), tc.grid, tc.seeds, tc.options)
			if err != nil {
				t.Fatal(err)
			}
			assertFastMarchingResultsEqual(t, legacy, fixed)
			if cap(fixed.Times) != len(tc.grid.Speed) || cap(fixed.Seeds) != len(tc.seeds) {
				t.Fatalf("capacities times=%d seeds=%d, want %d and %d", cap(fixed.Times), cap(fixed.Seeds), len(tc.grid.Speed), len(tc.seeds))
			}
		})
	}
}

func assertFastMarchingResultsEqual(t *testing.T, legacy, fixed *FastMarchingResult) {
	t.Helper()
	if !reflect.DeepEqual(legacy, fixed) {
		t.Fatal("fixed-buffer result differs from the complete legacy result")
	}
	// Compare bits as well as numeric values: even signed zero must retain
	// the legacy arithmetic, and +Inf marks the same unreached nodes.
	for i, value := range legacy.Times {
		if math.Float64bits(value) != math.Float64bits(fixed.Times[i]) {
			t.Fatalf("node %d: legacy bits=%x fixed bits=%x", i, math.Float64bits(value), math.Float64bits(fixed.Times[i]))
		}
	}
}

func TestFastMarchingFixedBuffersDuplicateSeedsAndOwnedResults(t *testing.T) {
	grid := uniformFastMarchingGrid(3, 1, 1, Vec3{X: 1, Y: 1, Z: 1}, 1)
	seeds := []FastMarchingSeed{
		{Index: [3]int{1, 0, 0}, Value: 5},
		{Index: [3]int{1, 0, 0}},
		{Index: [3]int{1, 0, 0}, Value: 2},
		{Index: [3]int{2, 0, 0}},
		{Index: [3]int{2, 0, 0}, Value: 3},
	}
	options := FastMarchingOptions{Targets: [][3]int{{0, 0, 0}, {0, 0, 0}}}
	speedBefore := append([]float64(nil), grid.Speed...)
	seedsBefore := append([]FastMarchingSeed(nil), seeds...)
	targetsBefore := append([][3]int(nil), options.Targets...)
	legacy, err := FastMarching(nil, grid, seeds, options)
	if err != nil {
		t.Fatal(err)
	}
	fixed, err := FastMarchingFixedBuffers(nil, grid, seeds, options)
	if err != nil {
		t.Fatal(err)
	}
	assertFastMarchingResultsEqual(t, legacy, fixed)
	wantSeeds := [][3]int{{1, 0, 0}, {1, 0, 0}, {1, 0, 0}, {2, 0, 0}, {2, 0, 0}}
	if !reflect.DeepEqual(fixed.Times, []float64{1, 0, 0}) || fixed.Frozen != 3 || !fixed.TargetsReached || !reflect.DeepEqual(fixed.Seeds, wantSeeds) {
		t.Fatalf("duplicate seed semantics: %+v", fixed)
	}
	if cap(fixed.Times) != 3 || cap(fixed.Seeds) != len(seeds) {
		t.Fatalf("result capacities: times=%d seeds=%d", cap(fixed.Times), cap(fixed.Seeds))
	}
	fixed.Times[0] = 123
	fixed.Seeds[0] = [3]int{9, 9, 9}
	if !reflect.DeepEqual(grid.Speed, speedBefore) || !reflect.DeepEqual(seeds, seedsBefore) || !reflect.DeepEqual(options.Targets, targetsBefore) {
		t.Fatal("marching or result mutation changed input storage")
	}
	if !reflect.DeepEqual(legacy.Times, []float64{1, 0, 0}) || !reflect.DeepEqual(legacy.Seeds, wantSeeds) {
		t.Fatal("returned results share storage")
	}
	again, err := FastMarchingFixedBuffers(nil, grid, seeds, options)
	if err != nil {
		t.Fatal(err)
	}
	assertFastMarchingResultsEqual(t, legacy, again)
}

func TestFastMarchingFixedBuffersHeapCapacityAndPositions(t *testing.T) {
	const count = 33
	h := fmHeap{nodes: make([]int32, 0, count), pos: make([]int32, count), times: make([]float64, count)}
	for i := range h.pos {
		h.pos[i] = -1
		h.times[i] = float64(i % 5) // Ties exercise the unchanged heap ordering.
	}
	backing := &h.nodes[:cap(h.nodes)][0]
	assertHeap := func() {
		t.Helper()
		if len(h.nodes) > count || cap(h.nodes) != count || &h.nodes[:cap(h.nodes)][0] != backing {
			t.Fatalf("queue backing changed: len=%d cap=%d", len(h.nodes), cap(h.nodes))
		}
		var seen [count]bool
		for i, node := range h.nodes {
			if node < 0 || int(node) >= count || seen[node] || h.pos[node] != int32(i) {
				t.Fatalf("invalid membership at %d: node=%d positions=%v", i, node, h.pos)
			}
			seen[node] = true
			if i > 0 && h.less(i, (i-1)/2) {
				t.Fatalf("heap order violated at %d", i)
			}
		}
		for node, present := range seen {
			if !present && h.pos[node] != -1 {
				t.Fatalf("absent node %d has position %d", node, h.pos[node])
			}
		}
	}
	for i := 0; i < count; i++ {
		h.update((i * 13) % count)
		assertHeap()
	}
	for i := 0; i < count; i++ {
		node := (i * 13) % count
		h.times[node] -= float64(i + 10)
		h.update(node)
		if len(h.nodes) != count {
			t.Fatalf("decrease inserted a duplicate: %d entries", len(h.nodes))
		}
		assertHeap()
	}
	previous := math.Inf(-1)
	for len(h.nodes) > 0 {
		node := h.pop()
		if h.times[node] < previous {
			t.Fatalf("pop time=%g after %g", h.times[node], previous)
		}
		previous = h.times[node]
		assertHeap()
	}
}

func TestFastMarchingFixedBuffersInputErrorsAndLimits(t *testing.T) {
	good := uniformFastMarchingGrid(4, 4, 1, Vec3{X: 1, Y: 1, Z: 1}, 1)
	seed := []FastMarchingSeed{{Index: [3]int{1, 1, 0}}}
	for _, tc := range []struct {
		name    string
		grid    FastMarchingGrid
		seeds   []FastMarchingSeed
		options FastMarchingOptions
	}{
		{"dimensions", FastMarchingGrid{Nx: 0, Ny: 4, Nz: 1}, seed, FastMarchingOptions{}},
		{"overflow", FastMarchingGrid{Nx: int(^uint(0) >> 1), Ny: 2, Nz: 2}, seed, FastMarchingOptions{}},
		{"node limit", FastMarchingGrid{Nx: MaxFastMarchingNodes + 1, Ny: 1, Nz: 1}, seed, FastMarchingOptions{}},
		{"speed count", FastMarchingGrid{Nx: 4, Ny: 4, Nz: 1, Spacing: good.Spacing, Speed: good.Speed[:3]}, seed, FastMarchingOptions{}},
		{"spacing", FastMarchingGrid{Nx: 4, Ny: 4, Nz: 1, Spacing: Vec3{X: 1, Y: 0, Z: 1}, Speed: good.Speed}, seed, FastMarchingOptions{}},
		{"no seeds", good, nil, FastMarchingOptions{}},
		{"seed index", good, []FastMarchingSeed{{Index: [3]int{4, 0, 0}}}, FastMarchingOptions{}},
		{"seed NaN", good, []FastMarchingSeed{{Index: [3]int{1, 1, 0}, Value: math.NaN()}}, FastMarchingOptions{}},
		{"seed infinity", good, []FastMarchingSeed{{Index: [3]int{1, 1, 0}, Value: math.Inf(1)}}, FastMarchingOptions{}},
		{"target", good, seed, FastMarchingOptions{Targets: [][3]int{{0, 4, 0}}}},
		{"negative stopping", good, seed, FastMarchingOptions{StoppingValue: -1}},
		{"NaN stopping", good, seed, FastMarchingOptions{StoppingValue: math.NaN()}},
		{"negative frozen", good, seed, FastMarchingOptions{MaxFrozen: -1}},
		{"negative trial", good, seed, FastMarchingOptions{MaxTrial: -1}},
		{"frozen limit", good, seed, FastMarchingOptions{MaxFrozen: 1}},
		{"trial limit", good, seed, FastMarchingOptions{MaxTrial: 1}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			legacy, legacyErr := FastMarching(context.Background(), tc.grid, tc.seeds, tc.options)
			fixed, fixedErr := FastMarchingFixedBuffers(context.Background(), tc.grid, tc.seeds, tc.options)
			if legacy != nil || fixed != nil || legacyErr == nil || fixedErr == nil || legacyErr.Error() != fixedErr.Error() {
				t.Fatalf("legacy=(%v,%v), fixed=(%v,%v)", legacy, legacyErr, fixed, fixedErr)
			}
			var legacyLimit, fixedLimit *FastMarchingLimitError
			if errors.As(legacyErr, &legacyLimit) {
				if !errors.As(fixedErr, &fixedLimit) || *legacyLimit != *fixedLimit {
					t.Fatalf("limit differs: legacy=%v fixed=%v", legacyErr, fixedErr)
				}
			} else if !errors.Is(fixedErr, ErrFastMarchingInput) {
				t.Fatalf("input error identity lost: %v", fixedErr)
			}
		})
	}
}

func TestFastMarchingFixedBuffersCancelsBeforeAllocationAndDuringMarch(t *testing.T) {
	grid := uniformFastMarchingGrid(128, 128, 1, Vec3{X: 1, Y: 1, Z: 1}, 1)
	seed := []FastMarchingSeed{{Index: [3]int{64, 64, 0}}}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	var result *FastMarchingResult
	var err error
	allocs := testing.AllocsPerRun(1, func() {
		result, err = FastMarchingFixedBuffers(ctx, grid, seed, FastMarchingOptions{})
	})
	if result != nil || !errors.Is(err, context.Canceled) || allocs != 0 {
		t.Fatalf("pre-canceled result=%v err=%v allocations=%g", result, err, allocs)
	}
	for _, limit := range []int{2, 6, 10, 11} {
		ctx := &fmInitializationContext{Context: context.Background(), limit: limit}
		result, err := FastMarchingFixedBuffers(ctx, grid, seed, FastMarchingOptions{StoppingValue: .1})
		if result != nil || !errors.Is(err, context.Canceled) {
			t.Fatalf("poll limit=%d result=%v err=%v polls=%d", limit, result, err, ctx.polls)
		}
	}
	// In addition to the new preallocation poll, limits cover positions,
	// times, the first heap pop and final cleanup with the tiny stop value.
}
