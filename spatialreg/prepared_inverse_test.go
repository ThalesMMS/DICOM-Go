package spatialreg

import (
	"context"
	"errors"
	"math"
	"reflect"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/ThalesMMS/dicom-go/render"
)

func inverter1531Fixture(kind string) *Registration {
	r := &Registration{Pre: identityMatrix(), Post: identityMatrix()}
	if kind == "identity" {
		return r
	}
	r.Pre.Values = render.GeometryAffine{1.1, .05, 0, 2, 0, .9, .03, -1, 0, 0, 1.05, 3, 0, 0, 0, 1}
	r.Post.Values = render.GeometryAffine{.95, 0, .02, -3, 0, 1.1, 0, 4, 0, 0, 1, 2, 0, 0, 0, 1}
	if kind == "affine" {
		return r
	}
	dims := [3]uint32{21, 21, 21}
	spacing := 5.0
	if kind == "large" {
		dims = [3]uint32{41, 41, 41}
		spacing = 2.5
	}
	r.Grid = fieldGrid(render.Vec3{X: -50, Y: -50, Z: -50}, spacing, dims, func(_, _, _ int, p render.Vec3) render.Vec3 {
		return render.Vec3{X: .6 * math.Sin(p.Y/15), Y: .4 * math.Cos(p.Z/20), Z: .3 * math.Sin((p.X+p.Y)/25)}
	})
	return r
}

func inverter1531Sources(t testing.TB, r *Registration, n int) []render.Vec3 {
	t.Helper()
	out := make([]render.Vec3, n)
	for i := range out {
		want := render.Vec3{X: float64((i*3)%11-5) * 3, Y: float64((i*7)%11-5) * 3, Z: float64((i*5)%11-5) * 3}
		var err error
		out[i], err = r.MapRegisteredToSource(want)
		if err != nil {
			t.Fatal(err)
		}
	}
	return out
}

func TestPreparedInverseExactOriginalAndOrders(t *testing.T) {
	for _, kind := range []string{"identity", "affine", "smooth", "large"} {
		t.Run(kind, func(t *testing.T) {
			r := inverter1531Fixture(kind)
			opts := InverseOptions{Tolerance: 1e-9}
			points := inverter1531Sources(t, r, 64)
			p, err := r.PrepareInverse(opts)
			if err != nil {
				t.Fatal(err)
			}
			defer p.Close()
			for _, reverse := range []bool{false, true} {
				ordered := append([]render.Vec3(nil), points...)
				if reverse {
					for i, j := 0, len(ordered)-1; i < j; i, j = i+1, j-1 {
						ordered[i], ordered[j] = ordered[j], ordered[i]
					}
				}
				batch, err := p.InvertPoints(context.Background(), ordered)
				if err != nil {
					t.Fatal(err)
				}
				for i, source := range ordered {
					want, e := r.invert1531Original(source, opts)
					current, ce := r.InvertPoint(source, opts)
					got, ge := p.InvertPoint(context.Background(), source)
					if e != nil || ce != nil || ge != nil || got != want || current != want || batch[i] != want {
						t.Fatalf("point%d exact original=%+v current=%+v prepared=%+v batch=%+v errors=%v/%v/%v", i, want, current, got, batch[i], e, ce, ge)
					}
				}
			}
		})
	}
}

func TestPreparedInverseIndependentKnownFields(t *testing.T) {
	for _, scale := range []float64{0, .125} {
		r := &Registration{Pre: identityMatrix(), Post: identityMatrix()}
		r.Pre.Values[3] = 2
		r.Post.Values[0] = 1.2
		r.Post.Values[7] = -4
		r.Grid = fieldGrid(render.Vec3{X: -20, Y: -20, Z: -20}, 2, [3]uint32{21, 21, 21}, func(_, _, _ int, p render.Vec3) render.Vec3 { return render.Vec3{X: scale*p.X + .5, Y: .25, Z: -.125} })
		p, e := r.PrepareInverse(InverseOptions{Tolerance: 1e-9})
		if e != nil {
			t.Fatal(e)
		}
		for _, want := range []render.Vec3{{X: 3.25, Y: -2, Z: 1}, {X: -4, Y: 5, Z: -6}} {
			// Independent published composition: Post(Pre(x)+Kx+d).
			source := render.Vec3{X: 1.2 * ((1+scale)*want.X + 2.5), Y: want.Y + .25 - 4, Z: want.Z - .125}
			got, e := p.InvertPoint(context.Background(), source)
			if e != nil || got.Registered.Sub(want).Length() > 1e-6 || got.Residual > 1e-9 || math.Abs(got.JacobianDeterminant-(1+scale)) > 1e-12 {
				t.Fatalf("knownfield=%+v error%v want%+v", got, e, want)
			}
		}
		p.Close()
	}
}

func TestPreparedInverseFailsClosedAndBatchAtomic(t *testing.T) {
	cases := []struct {
		name     string
		r        *Registration
		source   render.Vec3
		opts     InverseOptions
		sentinel error
	}{
		{"outside", inverter1531Fixture("smooth"), render.Vec3{X: 200}, InverseOptions{}, ErrOutsideDomain},
		{"nonfinite", inverter1531Fixture("identity"), render.Vec3{X: math.NaN()}, InverseOptions{}, ErrInvalidObject},
		{"nonconvergence", inverter1531Fixture("smooth"), render.Vec3{X: 10, Y: 10}, InverseOptions{MaximumIterations: 1, Tolerance: 1e-12, MaxResidual: 1e-12}, ErrInverseNotConverged},
	}
	folding := &Registration{Pre: identityMatrix(), Post: identityMatrix(), Grid: fieldGrid(render.Vec3{X: -20, Y: -20, Z: -20}, 2, [3]uint32{21, 21, 21}, func(_, _, _ int, p render.Vec3) render.Vec3 { return render.Vec3{X: -1.5 * p.X} })}
	cases = append(cases, struct {
		name     string
		r        *Registration
		source   render.Vec3
		opts     InverseOptions
		sentinel error
	}{"folding", folding, render.Vec3{X: 3, Y: 1, Z: 2}, InverseOptions{}, ErrFolding})
	undef := inverter1531Fixture("smooth")
	for i := range undef.Grid.vectors {
		undef.Grid.vectors[i] = vector3f{X: float32(math.NaN()), Y: float32(math.NaN()), Z: float32(math.NaN())}
	}
	cases = append(cases, struct {
		name     string
		r        *Registration
		source   render.Vec3
		opts     InverseOptions
		sentinel error
	}{"undefined", undef, render.Vec3{}, InverseOptions{}, ErrUndefinedDomain})
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			p, e := c.r.PrepareInverse(c.opts)
			if e != nil {
				t.Fatal(e)
			}
			defer p.Close()
			want, we := c.r.invert1531Original(c.source, c.opts)
			got, ge := p.InvertPoint(context.Background(), c.source)
			if !errors.Is(we, c.sentinel) || !errors.Is(ge, c.sentinel) || !reflect.DeepEqual(got, want) || ge.Error() != we.Error() {
				t.Fatalf("diagnostic original=%+v/%v prepared=%+v/%v", want, we, got, ge)
			}
			out, e := p.InvertPoints(context.Background(), []render.Vec3{c.source})
			var indexed *InverseBatchError
			if out != nil || !errors.As(e, &indexed) || indexed.Index != 0 || !errors.Is(e, c.sentinel) || !reflect.DeepEqual(indexed.Result, want) {
				t.Fatalf("partialbatch out%v err%v", out, e)
			}
		})
	}
	singular := inverter1531Fixture("identity")
	singular.Pre.Values[0] = 0
	if _, e := singular.PrepareInverse(InverseOptions{}); !errors.Is(e, ErrSingularMatrix) {
		t.Fatalf("singular preparation%v", e)
	}
	_, original := singular.invert1531Original(render.Vec3{}, InverseOptions{})
	if !errors.Is(original, ErrSingularMatrix) {
		t.Fatalf("singular original%v", original)
	}
	p, e := inverter1531Fixture("identity").PrepareInverse(InverseOptions{})
	if e != nil {
		t.Fatal(e)
	}
	defer p.Close()
	out, e := p.InvertPoints(context.Background(), []render.Vec3{{}, {X: math.Inf(1)}})
	var indexed *InverseBatchError
	if out != nil || !errors.As(e, &indexed) || indexed.Index != 1 || !errors.Is(e, ErrInvalidObject) {
		t.Fatalf("latefailurepartial out%v err%v", out, e)
	}
}

type inverter1531CancelChecks struct {
	context.Context
	cancel context.CancelFunc
	count  atomic.Int32
	at     int32
}

func (c *inverter1531CancelChecks) Err() error {
	if c.count.Add(1) == c.at {
		c.cancel()
	}
	return c.Context.Err()
}

func TestPreparedInverseSnapshotCancelCloseConcurrency(t *testing.T) {
	r := inverter1531Fixture("smooth")
	points := inverter1531Sources(t, r, 32)
	p, e := r.PrepareInverse(InverseOptions{})
	if e != nil {
		t.Fatal(e)
	}
	want, e := p.InvertPoints(context.Background(), points)
	if e != nil {
		t.Fatal(e)
	}
	payload := uint64(len(r.Grid.vectors)) * 12
	if p.RetainedGridBytes() != payload {
		t.Fatal("snapshot payload")
	}
	// Mutation AFTER construction: readers must never touch the old generation.
	var wg sync.WaitGroup
	errs := make(chan error, 8)
	for worker := 0; worker < 8; worker++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for k := 0; k < 8; k++ {
				got, e := p.InvertPoints(context.Background(), points)
				if e != nil {
					errs <- e
					return
				}
				if !reflect.DeepEqual(got, want) {
					errs <- errors.New("snapshot changed")
					return
				}
			}
		}()
	}
	r.Pre.Values[3] = 999
	r.Post.Values[0] = 0
	r.Grid.Origin.X = 999
	r.Grid.Spacing.X = 0
	r.Grid.Dimensions[0] = 0
	for i := range r.Grid.vectors {
		r.Grid.vectors[i] = vector3f{X: 999}
	}
	wg.Wait()
	close(errs)
	for e := range errs {
		t.Fatal(e)
	}
	for _, at := range []int32{1, 5, 50} {
		ctx, cancel := context.WithCancel(context.Background())
		hook := &inverter1531CancelChecks{Context: ctx, cancel: cancel, at: at}
		got, e := p.InvertPoints(hook, points)
		cancel()
		if got != nil || !errors.Is(e, context.Canceled) {
			t.Fatalf("cancelchecks%d got%v err%v", at, got, e)
		}
	}
	if e := p.Close(); e != nil {
		t.Fatal(e)
	}
	if e := p.Close(); e != nil {
		t.Fatal(e)
	}
	if p.RetainedGridBytes() != 0 {
		t.Fatal("retained closed")
	}
	if _, e := p.InvertPoint(context.Background(), points[0]); !errors.Is(e, ErrInverseClosed) {
		t.Fatal(e)
	}
	if _, e := p.InvertPoints(context.Background(), points); !errors.Is(e, ErrInverseClosed) {
		t.Fatal(e)
	}
}

type inverter1531GateContext struct {
	context.Context
	checks           atomic.Int32
	entered, release chan struct{}
}

func (c *inverter1531GateContext) Err() error {
	if c.checks.Add(1) == 3 {
		close(c.entered)
		<-c.release
	}
	return c.Context.Err()
}
func TestPreparedInverseCloseWaitsForBatch(t *testing.T) {
	p, e := inverter1531Fixture("identity").PrepareInverse(InverseOptions{})
	if e != nil {
		t.Fatal(e)
	}
	ctx := &inverter1531GateContext{Context: context.Background(), entered: make(chan struct{}), release: make(chan struct{})}
	var release sync.Once
	defer release.Do(func() { close(ctx.release) })
	queryDone := make(chan error, 1)
	go func() { _, e := p.InvertPoints(ctx, []render.Vec3{{}}); queryDone <- e }()
	select {
	case <-ctx.entered:
	case <-time.After(3 * time.Second):
		t.Fatal("batch never entered")
	}
	if p.mu.TryLock() {
		p.mu.Unlock()
		t.Fatal("active batch did not hold snapshot lease")
	}
	closeStarted := make(chan struct{})
	closeDone := make(chan error, 1)
	go func() { close(closeStarted); closeDone <- p.Close() }()
	<-closeStarted
	select {
	case e := <-closeDone:
		t.Fatalf("close before batchjoin%v", e)
	default:
	}
	release.Do(func() { close(ctx.release) })
	if e := <-queryDone; e != nil {
		t.Fatal(e)
	}
	if e := <-closeDone; e != nil {
		t.Fatal(e)
	}
	if p.RetainedGridBytes() != 0 {
		t.Fatal("closed retained")
	}
}

func TestPreparedInverseCancellationDuringPoint(t *testing.T) {
	t.Run("iteration", func(t *testing.T) {
		r := inverter1531Fixture("smooth")
		points := inverter1531Sources(t, r, 1)
		p, e := r.PrepareInverse(InverseOptions{Tolerance: 1e-12})
		if e != nil {
			t.Fatal(e)
		}
		defer p.Close()
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		hook := &inverter1531CancelChecks{Context: ctx, cancel: cancel, at: 3}
		result, e := p.InvertPoint(hook, points[0])
		// Two prechecks, cancel at first iteration, then terminal publication check.
		if result != (InverseResult{}) || !errors.Is(e, context.Canceled) || hook.count.Load() != 4 {
			t.Fatalf("iteration result%+v err%v checks%d", result, e, hook.count.Load())
		}
	})
	cases := []struct {
		name     string
		r        *Registration
		source   render.Vec3
		sentinel error
	}{
		{"identity-terminal", inverter1531Fixture("identity"), render.Vec3{X: 3, Y: 4, Z: 5}, nil},
		{"affine-terminal", inverter1531Fixture("affine"), render.Vec3{X: 3, Y: 4, Z: 5}, nil},
	}
	folding := &Registration{Pre: identityMatrix(), Post: identityMatrix(), Grid: fieldGrid(render.Vec3{X: -20, Y: -20, Z: -20}, 2, [3]uint32{21, 21, 21}, func(_, _, _ int, p render.Vec3) render.Vec3 { return render.Vec3{X: -1.5 * p.X} })}
	cases = append(cases, struct {
		name     string
		r        *Registration
		source   render.Vec3
		sentinel error
	}{"folding-diagnostic-terminal", folding, render.Vec3{}, ErrFolding})
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			original, e := c.r.invert1531Original(c.source, InverseOptions{})
			if original.Iterations != 0 || !errors.Is(e, c.sentinel) {
				t.Fatalf("fixture does not reach no-iteration terminal: %+v err%v", original, e)
			}
			p, e := c.r.PrepareInverse(InverseOptions{})
			if e != nil {
				t.Fatal(e)
			}
			defer p.Close()
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			hook := &inverter1531CancelChecks{Context: ctx, cancel: cancel, at: 3}
			result, e := p.InvertPoint(hook, c.source)
			if result != (InverseResult{}) || !errors.Is(e, context.Canceled) || hook.count.Load() != 3 {
				t.Fatalf("terminal published %+v err%v checks%d", result, e, hook.count.Load())
			}
			// Batch adds one loop precheck, then the same terminal cancellation.
			batchCtx, batchCancel := context.WithCancel(context.Background())
			defer batchCancel()
			batchHook := &inverter1531CancelChecks{Context: batchCtx, cancel: batchCancel, at: 4}
			out, e := p.InvertPoints(batchHook, []render.Vec3{c.source})
			var indexed *InverseBatchError
			if out != nil || !errors.Is(e, context.Canceled) || !errors.As(e, &indexed) || indexed.Index != 0 || indexed.Result != (InverseResult{}) || batchHook.count.Load() != 4 {
				t.Fatalf("terminal batch published out%v err%v checks%d", out, e, batchHook.count.Load())
			}
		})
	}
}

func TestGridVectorBytesActualStorage(t *testing.T) {
	var absent *Grid
	if absent.VectorBytes() != 0 {
		t.Fatal("nil payload")
	}
	r := inverter1531Fixture("smooth")
	want := uint64(21 * 21 * 21 * 12)
	if got := r.Grid.VectorBytes(); got != want {
		t.Fatalf("actual bytes=%d want=%d", got, want)
	}
	r.Grid.Dimensions = [3]uint32{1, 1, 1}
	if got := r.Grid.VectorBytes(); got != want {
		t.Fatalf("metadata changed payload=%d want=%d", got, want)
	}
}
