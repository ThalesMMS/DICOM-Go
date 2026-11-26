package roi

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

func TestPhysicalPolylineLineAndOwnership(t *testing.T) {
	p := []Point3D{{}, {X: .125, Y: .25, Z: .5}, {X: .25, Y: .5, Z: 1}, {X: 1, Y: 2, Z: 4}}
	before := append([]Point3D(nil), p...)
	got, err := SimplifyPhysicalPolyline(context.Background(), p, PolylineSimplifyOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(got.OriginalIndices, []int{0, 3}) || got.RemovedPoints != 2 || got.Reduction != .5 || got.MaxDeviationMM != 0 {
		t.Fatalf("unexpected report %+v", got)
	}
	if got.InputLengthMM != got.OutputLengthMM || math.Abs(got.InputLengthMM-math.Sqrt(21)) > 1e-14 {
		t.Fatalf("length %+v", got)
	}
	got.Points[0].X = 99
	got.OriginalIndices[0] = 99
	if !reflect.DeepEqual(p, before) {
		t.Fatal("input was mutated or output aliases it")
	}
}

func TestPhysicalPolylineProtectedDuplicatesAndClosure(t *testing.T) {
	tests := []struct {
		name    string
		p       []Point3D
		o       PolylineSimplifyOptions
		indices []int
	}{
		{"protected duplicate", []Point3D{{}, {X: 1}, {X: 1}, {X: 2}, {X: 3}}, PolylineSimplifyOptions{ProtectedIndices: []int{1, 2, 2}}, []int{0, 1, 2, 4}},
		{"open duplicate endpoints", []Point3D{{}, {}, {}, {X: 1}, {X: 1}}, PolylineSimplifyOptions{}, []int{0, 4}},
		{"identical open", []Point3D{{X: 1}, {X: 1}, {X: 1}}, PolylineSimplifyOptions{}, []int{0, 2}},
		{"implicit triangle", []Point3D{{}, {X: 1}, {Y: 1}}, PolylineSimplifyOptions{Closed: true, ToleranceMM: 100}, []int{0, 1, 2}},
		{"explicit triangle", []Point3D{{}, {X: 1}, {Y: 1}, {}}, PolylineSimplifyOptions{Closed: true, ToleranceMM: 100, ProtectedIndices: []int{3}}, []int{0, 1, 2, 3}},
		{"closed protected duplicates", []Point3D{{}, {X: 1}, {X: 1}, {X: 1, Y: 1}, {Y: 1}, {}}, PolylineSimplifyOptions{Closed: true, ToleranceMM: .1, ProtectedIndices: []int{1, 2, 5}}, []int{0, 1, 2, 3, 4, 5}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			got, err := SimplifyPhysicalPolyline(context.Background(), test.p, test.o)
			if err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(got.OriginalIndices, test.indices) {
				t.Fatalf("got %v want %v", got.OriginalIndices, test.indices)
			}
			if got.Closed != test.o.Closed {
				t.Fatal("closure lost")
			}
			for i, j := range got.OriginalIndices {
				if got.Points[i] != test.p[j] {
					t.Fatal("original point lost")
				}
			}
		})
	}
}

func TestPhysicalPolylineTopologyRefusalsAndExplicitOptOut(t *testing.T) {
	tests := []struct {
		name string
		p    []Point3D
		o    PolylineSimplifyOptions
	}{
		{"introduced crossing", []Point3D{{}, {Y: 4}, {X: 4, Y: 4}, {X: 4}, {X: 2, Y: 3}, {X: 2, Y: -1}}, PolylineSimplifyOptions{ToleranceMM: 5, ProtectedIndices: []int{3, 4}}},
		{"original bow tie", []Point3D{{}, {X: 2, Y: 2}, {Y: 2}, {X: 2}}, PolylineSimplifyOptions{}},
		{"nonadjacent touch", []Point3D{{}, {X: 2}, {X: 2, Y: 1}, {X: 1}, {X: 1, Y: -1}}, PolylineSimplifyOptions{ProtectedIndices: []int{0, 1, 2, 3, 4}}},
		{"backtracking", []Point3D{{}, {X: 2}, {X: 1}}, PolylineSimplifyOptions{}},
		{"closed collinear", []Point3D{{}, {X: 1}, {X: 2}}, PolylineSimplifyOptions{Closed: true}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			got, err := SimplifyPhysicalPolyline(context.Background(), test.p, test.o)
			if !errors.Is(err, ErrPolylineTopology) || !reflect.DeepEqual(got, PolylineSimplifyResult{}) {
				t.Fatalf("got %+v, %v", got, err)
			}
			test.o.Topology = PolylineIgnoreIntersections
			if _, err := SimplifyPhysicalPolyline(context.Background(), test.p, test.o); err != nil {
				t.Fatalf("explicit unchecked profile: %v", err)
			}
		})
	}
}

func TestPhysicalPolylineExactSegmentPredicates(t *testing.T) {
	tests := []struct {
		name       string
		a, b, c, d Point3D
		intersect  bool
	}{
		{"planar cross", Point3D{}, Point3D{X: 2, Y: 2}, Point3D{Y: 2}, Point3D{X: 2}, true},
		{"skew", Point3D{}, Point3D{X: 2, Y: 2}, Point3D{Y: 2, Z: math.SmallestNonzeroFloat64}, Point3D{X: 2, Z: math.SmallestNonzeroFloat64}, false},
		{"3D cross", Point3D{}, Point3D{X: 2, Y: 2, Z: 2}, Point3D{Y: 2, Z: 1}, Point3D{X: 2, Z: 1}, true},
		{"parallel separation", Point3D{}, Point3D{X: 2}, Point3D{Y: 1}, Point3D{X: 2, Y: 1}, false},
		{"collinear overlap", Point3D{}, Point3D{X: 2}, Point3D{X: 1}, Point3D{X: 3}, true},
		{"collinear separation", Point3D{}, Point3D{X: 2}, Point3D{X: 3}, Point3D{X: 4}, false},
		{"endpoint", Point3D{}, Point3D{X: 2}, Point3D{X: 2}, Point3D{X: 3, Y: 1}, true},
		{"projected only", Point3D{}, Point3D{X: 2, Y: 2, Z: 1}, Point3D{Y: 2, Z: 2}, Point3D{X: 2, Z: 1}, false},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if got := polylineSegmentsIntersect(test.a, test.b, test.c, test.d); got != test.intersect {
				t.Fatalf("intersect %v want %v", got, test.intersect)
			}
			if got := polylineSegmentsIntersect(test.d, test.c, test.b, test.a); got != test.intersect {
				t.Fatal("direction/order asymmetry")
			}
		})
	}
}

func TestPhysicalPolylineExactToleranceAndRanges(t *testing.T) {
	for _, height := range []float64{1e-20, math.SmallestNonzeroFloat64, 1, 1e8} {
		p := Point3D{X: .5, Y: height}
		a, b := Point3D{}, Point3D{X: 1}
		if !polylineWithin(p, a, b, height) {
			t.Fatalf("exact boundary refused %g", height)
		}
		if polylineWithin(p, a, b, math.Nextafter(height, 0)) {
			t.Fatalf("outside accepted %g", height)
		}
		points := []Point3D{a, p, b}
		got, err := SimplifyPhysicalPolyline(context.Background(), points, PolylineSimplifyOptions{ToleranceMM: math.Nextafter(height, 0)})
		if err != nil {
			t.Fatal(err)
		}
		if len(got.Points) != 3 {
			t.Fatalf("rounded-away exact violation at %g: %+v", height, got)
		}
	}
	got, err := SimplifyPhysicalPolyline(context.Background(), []Point3D{{X: -1e9}, {}, {X: 1e9}}, PolylineSimplifyOptions{})
	if err != nil || len(got.Points) != 2 || got.OutputLengthMM != 2e9 {
		t.Fatalf("range endpoints %+v %v", got, err)
	}
}

func TestPhysicalPolylineSmallInvalidAndLimits(t *testing.T) {
	for _, p := range [][]Point3D{nil, {{X: 1}}} {
		got, err := SimplifyPhysicalPolyline(context.Background(), p, PolylineSimplifyOptions{})
		if err != nil || len(got.Points) != len(p) {
			t.Fatalf("small input %+v %v", got, err)
		}
	}
	bad := []PolylineSimplifyOptions{{ToleranceMM: -1}, {ToleranceMM: math.NaN()}, {ToleranceMM: math.Inf(1)}, {ToleranceMM: 1e9 + 1}, {Topology: 2}, {ProtectedIndices: []int{-1}}, {ProtectedIndices: []int{2}}, {Limits: PolylineLimits{MaxPoints: -1}}, {Limits: PolylineLimits{MaxPoints: 100001}}, {Limits: PolylineLimits{MaxWork: -1}}, {Limits: PolylineLimits{MaxWork: 1_000_000_001}}, {Limits: PolylineLimits{MaxBytes: -1}}, {Limits: PolylineLimits{MaxBytes: 256*1024*1024 + 1}}}
	for i, o := range bad {
		got, err := SimplifyPhysicalPolyline(context.Background(), []Point3D{{}, {X: 1}}, o)
		if !errors.Is(err, ErrPolylineInvalid) || !reflect.DeepEqual(got, PolylineSimplifyResult{}) {
			t.Fatalf("invalid %d %+v %v", i, got, err)
		}
	}
	for _, p := range []Point3D{{X: math.NaN()}, {Y: math.Inf(-1)}, {Z: 1e9 + 1}} {
		if _, err := SimplifyPhysicalPolyline(context.Background(), []Point3D{p}, PolylineSimplifyOptions{}); !errors.Is(err, ErrPolylineInvalid) {
			t.Fatalf("point %+v %v", p, err)
		}
	}
	for _, p := range [][]Point3D{nil, {{}}, {{}, {X: 1}}, {{}, {X: 1}, {}, {X: 1}}} {
		if _, err := SimplifyPhysicalPolyline(context.Background(), p, PolylineSimplifyOptions{Closed: true}); !errors.Is(err, ErrPolylineInvalid) {
			t.Fatalf("closed small %v: %v", p, err)
		}
	}
	for _, l := range []PolylineLimits{{MaxPoints: 1}, {MaxBytes: polylineFixedBytes + 2*polylineBytesPerPoint - 1}, {MaxWork: 1}} {
		if got, err := SimplifyPhysicalPolyline(context.Background(), []Point3D{{}, {X: 1}}, PolylineSimplifyOptions{Limits: l}); !errors.Is(err, ErrPolylineLimit) || !reflect.DeepEqual(got, PolylineSimplifyResult{}) {
			t.Fatalf("limit %+v %v", got, err)
		}
	}
	if _, err := SimplifyPhysicalPolyline(nil, nil, PolylineSimplifyOptions{}); !errors.Is(err, ErrPolylineInvalid) {
		t.Fatal("nil context accepted")
	}
}

type polylineCancelContext struct {
	calls atomic.Int64
	after int64
}

func (*polylineCancelContext) Deadline() (time.Time, bool) { return time.Time{}, false }
func (*polylineCancelContext) Done() <-chan struct{}       { return nil }
func (c *polylineCancelContext) Err() error {
	if c.calls.Add(1) >= c.after {
		return context.Canceled
	}
	return nil
}
func (*polylineCancelContext) Value(any) any { return nil }

func TestPhysicalPolylineCancellationAndConcurrentOwnedResults(t *testing.T) {
	p := make([]Point3D, 300)
	for i := range p {
		p[i] = Point3D{X: float64(i), Y: math.Sin(float64(i) / 20)}
	}
	// These controls deterministically stop in input, topology, RDP and reporting
	// phases, without a timing-dependent cancellation race.
	for _, at := range []int64{1, 80, 700, 40_000, 46_000} {
		got, err := SimplifyPhysicalPolyline(&polylineCancelContext{after: at}, p, PolylineSimplifyOptions{ToleranceMM: .1})
		if !errors.Is(err, context.Canceled) || !reflect.DeepEqual(got, PolylineSimplifyResult{}) {
			t.Fatalf("cancel at %d %+v %v", at, got, err)
		}
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := SimplifyPhysicalPolyline(ctx, p, PolylineSimplifyOptions{}); !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
	ctx, stop := context.WithDeadline(context.Background(), time.Now().Add(-time.Second))
	defer stop()
	if _, err := SimplifyPhysicalPolyline(ctx, p, PolylineSimplifyOptions{}); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatal(err)
	}

	reference, err := SimplifyPhysicalPolyline(context.Background(), p, PolylineSimplifyOptions{ToleranceMM: .1})
	if err != nil {
		t.Fatal(err)
	}
	if got, err := SimplifyPhysicalPolyline(&polylineCancelContext{after: reference.Work}, p, PolylineSimplifyOptions{ToleranceMM: .1}); !errors.Is(err, context.Canceled) || !reflect.DeepEqual(got, PolylineSimplifyResult{}) {
		t.Fatalf("late report cancellation %+v %v", got, err)
	}
	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			got, err := SimplifyPhysicalPolyline(context.Background(), p, PolylineSimplifyOptions{ToleranceMM: .1})
			if err != nil {
				t.Error(err)
				return
			}
			got.Points[0].X = -999
			got.OriginalIndices[0] = -999
		}()
	}
	wg.Wait()
	if p[0].X != 0 {
		t.Fatal("concurrent results alias input")
	}
}

func TestPixelPolylineExplicitTransform(t *testing.T) {
	p := []Point2D{{}, {X: .5, Y: .2}, {X: 1}, {X: 2}}
	plane := PolylinePixelPlane{OriginMM: Point3D{X: 10, Y: -5, Z: 3}, ColumnStepMM: Point3D{X: 2, Z: 2}, RowStepMM: Point3D{Y: 3}}
	before := append([]Point2D(nil), p...)
	got, err := SimplifyPixelPolyline(context.Background(), p, plane, PolylineSimplifyOptions{ToleranceMM: .7})
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(got.Physical.OriginalIndices, []int{0, 3}) || got.Physical.Points[1] != (Point3D{X: 14, Y: -5, Z: 7}) || math.Abs(got.Physical.MaxDeviationMM-.6) > 1e-14 {
		t.Fatalf("transform %+v", got)
	}
	got.Points[0].X = 99
	got.Physical.Points[0].X = 99
	if !reflect.DeepEqual(p, before) {
		t.Fatal("pixel input aliases result")
	}
	got, err = SimplifyPixelPolyline(context.Background(), p, plane, PolylineSimplifyOptions{ToleranceMM: .5})
	if err != nil {
		t.Fatal(err)
	}
	if len(got.Points) <= 2 {
		t.Fatal("anisotropy ignored")
	}
	for _, bad := range []PolylinePixelPlane{{}, {ColumnStepMM: Point3D{X: 1}, RowStepMM: Point3D{X: 2}}, {ColumnStepMM: Point3D{X: 1}, RowStepMM: Point3D{Y: math.Inf(1)}}, {OriginMM: Point3D{X: 1e9}, ColumnStepMM: Point3D{X: 1}, RowStepMM: Point3D{Y: 1}}} {
		if _, err := SimplifyPixelPolyline(context.Background(), p, bad, PolylineSimplifyOptions{}); !errors.Is(err, ErrPolylineInvalid) {
			t.Fatalf("invalid plane %+v %v", bad, err)
		}
	}
	if _, err := SimplifyPixelPolyline(context.Background(), []Point2D{{X: math.NaN()}}, plane, PolylineSimplifyOptions{}); !errors.Is(err, ErrPolylineInvalid) {
		t.Fatal(err)
	}
}

func TestPhysicalPolylineFloat32AndPublicSubnormalSkew(t *testing.T) {
	original := [][3]float32{{0, 0, 0}, {.125, .25, .5}, {.5, 1, 2}, {1, 2, 4}}
	p := make([]Point3D, len(original))
	for i, v := range original {
		p[i] = Point3D{X: float64(v[0]), Y: float64(v[1]), Z: float64(v[2])}
	}
	got, err := SimplifyPhysicalPolyline(context.Background(), p, PolylineSimplifyOptions{ProtectedIndices: []int{1}})
	if err != nil || !reflect.DeepEqual(got.OriginalIndices, []int{0, 1, 3}) {
		t.Fatalf("float32 %+v %v", got, err)
	}
	s := math.SmallestNonzeroFloat64
	p = []Point3D{{}, {X: 2, Y: 2}, {Y: 2, Z: s}, {X: 2, Z: s}}
	got, err = SimplifyPhysicalPolyline(context.Background(), p, PolylineSimplifyOptions{ProtectedIndices: []int{0, 1, 2, 3}})
	if err != nil || len(got.Points) != 4 {
		t.Fatalf("subnormal skew incorrectly intersects %+v %v", got, err)
	}
	p[2].Z = 0
	p[3].Z = 0
	if _, err = SimplifyPhysicalPolyline(context.Background(), p, PolylineSimplifyOptions{ProtectedIndices: []int{0, 1, 2, 3}}); !errors.Is(err, ErrPolylineTopology) {
		t.Fatalf("coplanar crossing accepted: %v", err)
	}
}
