package roi

import (
	"context"
	"errors"
	"github.com/ThalesMMS/dicom-go/imagefilter"
	"math"
	"testing"
)

func TestCannyLivewireEdgePathAndBudgets(t *testing.T) {
	const n = 41
	reads := 0
	value := func(x, y int) (float64, bool) {
		reads++
		if math.Hypot(float64(x-20), float64(y-20)) < 12 {
			return 100, true
		}
		return 0, true
	}
	o := CannyLivewireOptions{Path: LivewireOptions{ColSpacing: 1, RowSpacing: 1}}
	limited := o
	limited.Limits.MaxBytes = 1
	if r, e := NewCannyLivewireCostMap(nil, n, n, value, limited); r != nil || !errors.Is(e, imagefilter.ErrCannyResourceLimit) || reads != 0 {
		t.Fatalf("quota before sample %v reads%d", e, reads)
	}
	costs, e := NewCannyLivewireCostMap(nil, n, n, value, o)
	if e != nil {
		t.Fatal(e)
	}
	if reads != n*n {
		t.Fatalf("samples%d", reads)
	}
	path, e := costs.LivewirePath(nil, [2]int{31, 20}, [2]int{20, 31})
	if e != nil {
		t.Fatal(e)
	}
	if len(path) < 12 {
		t.Fatal("path cut across disk")
	}
	for _, p := range path {
		if d := math.Abs(math.Hypot(p[0]-20, p[1]-20) - 12); d > 1.6 {
			t.Fatalf("path off edge %.5g at%v", d, p)
		}
	}
	// Curvature expands states by eight. Filtering fits this lower quota, but
	// Tree's finite lazy-queue/trace-back reservation must fail before make.
	o.Path.CurvatureWeight = 1
	o.Limits.MaxBytes = 1 << 20
	costs, e = NewCannyLivewireCostMap(nil, 64, 64, func(x, y int) (float64, bool) {
		if x < 32 {
			return 100, true
		}
		return 0, true
	}, o)
	if e != nil {
		t.Fatal(e)
	}
	if r, e := costs.Tree(nil, [2]int{32, 32}); r != nil || !errors.Is(e, imagefilter.ErrCannyResourceLimit) {
		t.Fatalf("tree admission %v", e)
	}
}
func TestCannyLivewireRejectsDomainAndOptions(t *testing.T) {
	value := func(x, y int) (float64, bool) { return float64(x), true }
	o := CannyLivewireOptions{Path: LivewireOptions{ColSpacing: .7, RowSpacing: 1.3}}
	for _, change := range []func(*CannyLivewireOptions){func(o *CannyLivewireOptions) { o.Path.ColSpacing = 0 }, func(o *CannyLivewireOptions) { o.Path.RowSpacing = math.Inf(1) }, func(o *CannyLivewireOptions) { o.Path.ImageWeight = -1 }, func(o *CannyLivewireOptions) { o.Path.CurvatureWeight = math.NaN() }, func(o *CannyLivewireOptions) { o.Workers = -1 }} {
		bad := o
		change(&bad)
		if r, e := NewCannyLivewireCostMap(nil, 9, 7, value, bad); r != nil || e == nil {
			t.Fatal("invalid accepted")
		}
	}
	if r, e := NewCannyLivewireCostMap(nil, 9, 7, func(int, int) (float64, bool) { return 0, false }, o); r != nil || !errors.Is(e, ErrCannyDomain) {
		t.Fatalf("domain %v", e)
	}
	if r, e := NewCannyLivewireCostMap(nil, 9, 7, func(int, int) (float64, bool) { return 10, true }, o); r != nil || !errors.Is(e, ErrCannyNoEdges) {
		t.Fatalf("flat %v", e)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if r, e := NewCannyLivewireCostMap(ctx, 9, 7, value, o); r != nil || !errors.Is(e, context.Canceled) {
		t.Fatalf("cancel %v", e)
	}
	costs, e := NewCannyLivewireCostMap(nil, 9, 7, value, o)
	if e != nil {
		t.Fatal(e)
	}
	if tree, e := costs.Tree(ctx, [2]int{0, 0}); tree != nil || !errors.Is(e, context.Canceled) {
		t.Fatalf("tree cancel %v", e)
	}
}
