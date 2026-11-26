package roi

import (
	"context"
	"errors"
	"math"
	"testing"
	"time"
)

func diskImage(size int, cx, cy, r float64) func(x, y int) (float64, bool) {
	return func(x, y int) (float64, bool) {
		if math.Hypot(float64(x)-cx, float64(y)-cy) <= r {
			return 1000, true
		}
		return 0, true
	}
}

// TestLivewireFollowsSharpEdges: on a disk the path between two boundary
// anchors runs along the circle, every pixel within one pixel of it, on the
// short arc; the chord straight across is the length-only answer.
func TestLivewireFollowsSharpEdges(t *testing.T) {
	const size = 96
	cx, cy, r := 47.3, 48.1, 30.0
	costs, err := NewLivewireCostMap(size, size, diskImage(size, cx, cy, r), LivewireOptions{})
	if err != nil {
		t.Fatal(err)
	}
	onCircle := func(angle float64) [2]int {
		return [2]int{int(math.Round(cx + r*math.Cos(angle))), int(math.Round(cy + r*math.Sin(angle)))}
	}
	for _, curvature := range []float64{0, 0.5} {
		costs.opts.CurvatureWeight = curvature
		path, err := costs.LivewirePath(context.Background(), onCircle(0.2), onCircle(2.1))
		if err != nil {
			t.Fatal(err)
		}
		worst := 0.0
		for _, p := range path {
			worst = math.Max(worst, math.Abs(math.Hypot(p[0]-cx, p[1]-cy)-r))
		}
		if worst > 1 || len(path) < 40 {
			t.Fatalf("curvature %v: path of %d pixels strays %.2f px from the edge", curvature, len(path), worst)
		}
		for i := 1; i < len(path); i++ {
			if math.Abs(path[i][0]-path[i-1][0]) > 1 || math.Abs(path[i][1]-path[i-1][1]) > 1 {
				t.Fatalf("path jumps between %v and %v", path[i-1], path[i])
			}
		}
	}
	// A straight vertical edge between columns 30 and 31.
	step, err := NewLivewireCostMap(64, 64, func(x, y int) (float64, bool) {
		if x > 30 {
			return 500, true
		}
		return 0, true
	}, LivewireOptions{})
	if err != nil {
		t.Fatal(err)
	}
	path, err := step.LivewirePath(context.Background(), [2]int{30, 3}, [2]int{31, 60})
	if err != nil {
		t.Fatal(err)
	}
	for _, p := range path {
		if math.Abs(p[0]-30.5) > 1 {
			t.Fatalf("pixel %v strays from the edge at x 30.5", p)
		}
	}
}

func TestLivewireTreeAnchorsAndInvalidPixels(t *testing.T) {
	costs, err := NewLivewireCostMap(20, 10, func(x, y int) (float64, bool) {
		return float64(x), !(x == 10 && y < 9)
	}, LivewireOptions{ColSpacing: 0.5, RowSpacing: 2})
	if err != nil {
		t.Fatal(err)
	}
	tree, err := costs.Tree(context.Background(), [2]int{2, 2})
	if err != nil {
		t.Fatal(err)
	}
	path := tree.PathTo([2]int{17, 2})
	if path == nil || path[0] != [2]float64{2, 2} || path[len(path)-1] != [2]float64{17, 2} {
		t.Fatalf("path %v", path)
	}
	for _, p := range path {
		if p[0] == 10 && p[1] < 9 {
			t.Fatalf("path crosses the invalid wall at %v", p)
		}
	}
	if tree.Distance([2]int{2, 2}) != 0 || math.IsInf(tree.Distance([2]int{17, 2}), 1) || tree.PathTo([2]int{-1, 0}) != nil {
		t.Fatal("tree distances")
	}
	if _, err := costs.Tree(context.Background(), [2]int{10, 0}); !errors.Is(err, ErrLivewire) {
		t.Fatalf("invalid anchor error %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	big, _ := NewLivewireCostMap(128, 128, diskImage(128, 64, 64, 40), LivewireOptions{})
	if _, err := big.Tree(ctx, [2]int{1, 1}); !errors.Is(err, context.Canceled) {
		t.Fatalf("cancelled tree error %v", err)
	}
	if _, err := NewLivewireCostMap(0, 5, nil, LivewireOptions{}); !errors.Is(err, ErrLivewire) {
		t.Fatalf("empty slice error %v", err)
	}
}

// TestLivewireLatency measures the interactive cost on a 512 x 512 slice:
// the cost map once per slice, the tree once per anchor, and the path per
// pointer move.
func TestLivewireLatency(t *testing.T) {
	started := time.Now()
	costs, err := NewLivewireCostMap(512, 512, diskImage(512, 256, 256, 150), LivewireOptions{})
	if err != nil {
		t.Fatal(err)
	}
	costTime := time.Since(started)
	started = time.Now()
	tree, err := costs.Tree(context.Background(), [2]int{406, 256})
	if err != nil {
		t.Fatal(err)
	}
	treeTime := time.Since(started)
	started = time.Now()
	for i := 0; i < 100; i++ {
		tree.PathTo([2]int{256, 106 + i})
	}
	pathTime := time.Since(started) / 100
	t.Logf("512x512: cost map %v, tree %v, path per pointer move %v", costTime, treeTime, pathTime)
	if pathTime > 5*time.Millisecond {
		t.Fatalf("path lookup %v is not interactive", pathTime)
	}
}

func BenchmarkLivewireTree512(b *testing.B) {
	costs, err := NewLivewireCostMap(512, 512, diskImage(512, 256, 256, 150), LivewireOptions{})
	if err != nil {
		b.Fatal(err)
	}
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if _, err := costs.Tree(context.Background(), [2]int{406, 256}); err != nil {
			b.Fatal(err)
		}
	}
}
