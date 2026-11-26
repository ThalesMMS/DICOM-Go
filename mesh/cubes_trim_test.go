package mesh

import (
	"context"
	"errors"
	"fmt"
	"math"
	"reflect"
	"sync/atomic"
	"testing"
	"time"

	"github.com/ThalesMMS/dicom-go/render"
)

func TestCubesTrimAllCases(t *testing.T) {
	for c := 0; c < 256; c++ {
		f := &DenseField{Size: [3]int{2, 2, 2}, Affine: identityAffine, Data: make([]float32, 8)}
		for i := range f.Data {
			f.Data[i] = -1
			if c>>i&1 != 0 {
				f.Data[i] = 1
			}
		}
		frame, _ := newAffineFrame(f.Affine)
		e := newCubesExtractor(f, frame, Options{OpenBoundary: true})
		w := trimNewWorker(e, true)
		_, in0, _ := w.planeAt(context.Background(), 0)
		_, in1, _ := w.planeAt(context.Background(), 1)
		w.classify(in0, in1)
		if int(w.cases[0]) != c {
			t.Fatalf("case %d classified as %d", c, w.cases[0])
		}
		if (w.lo[0] < w.hi[0]) != (c != 0 && c != 255) {
			t.Fatalf("case %d interval [%d,%d)", c, w.lo[0], w.hi[0])
		}
		for _, open := range []bool{false, true} {
			trimRequireSame(t, f, Options{OpenBoundary: open, Workers: 2})
		}
	}
}

func trimRequireSame(t *testing.T, f Field, opts Options) *Mesh {
	t.Helper()
	want, err := Extract(context.Background(), f, opts)
	if err != nil {
		t.Fatal(err)
	}
	got, err := trimPrototypeRun(context.Background(), f, opts)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("candidate differs from baseline: vertices %d/%d triangles %d/%d", len(got.Vertices), len(want.Vertices), len(got.Triangles), len(want.Triangles))
	}
	if err := got.Validate(); err != nil {
		t.Fatal(err)
	}
	return got
}

func TestCubesTrimGeometry(t *testing.T) {
	a := GridAffine(render.Vec3{X: -8, Y: -10.2, Z: -12}, IdentityAxes, [3]float64{0.5, 0.6, 1})
	angle := 0.37
	oblique := GridAffine(render.Vec3{X: -8, Y: -10.2, Z: -12}, [3]render.Vec3{{X: math.Cos(angle), Y: math.Sin(angle)}, {X: -math.Sin(angle), Y: math.Cos(angle)}, {Z: 1}}, [3]float64{0.5, 0.6, 1})
	mirrored := a
	mirrored[0] = -mirrored[0]
	fields := map[string]Field{
		"sphere-anisotropic": sphereField([3]int{33, 35, 25}, a, render.Vec3{}, 6),
		"sphere-oblique":     sphereField([3]int{33, 35, 25}, oblique, render.Vec3{}, 5),
		"noise-ambiguous":    randomField(1495, [3]int{19, 17, 15}, mirrored),
		"torus":              torusField([3]int{48, 48, 24}, GridAffine(render.Vec3{X: -9.4, Y: -9.4, Z: -4.6}, IdentityAxes, [3]float64{0.4, 0.4, 0.4}), 6, 2),
	}
	for _, value := range []float64{-1, 0, 1} {
		fields[fmt.Sprintf("constant-%g", value)] = &FuncField{Size: [3]int{9, 7, 6}, Affine: a, Value: func(x, y, z int) float64 { return value }}
	}
	for axis := 0; axis < 3; axis++ {
		fields[fmt.Sprintf("plane-axis-%d", axis)] = &FuncField{Size: [3]int{9, 7, 6}, Affine: oblique, Value: func(x, y, z int) float64 { return 3.3 - float64([3]int{x, y, z}[axis]) }}
		thin := [3]int{9, 7, 6}
		thin[axis] = 1
		fields[fmt.Sprintf("thin-axis-%d", axis)] = &FuncField{Size: thin, Affine: mirrored, Value: func(x, y, z int) float64 { return 1 }}
	}
	fields["plane-oblique-clipped"] = &FuncField{Size: [3]int{9, 7, 6}, Affine: oblique, Value: func(x, y, z int) float64 { return 5.25 - float64(x) - 0.4*float64(y) - 0.3*float64(z) }}
	invalid := randomField(93, [3]int{9, 7, 6}, a)
	invalid.Data[0], invalid.Data[51], invalid.Data[130] = float32(math.NaN()), float32(math.Inf(1)), float32(math.Inf(-1))
	fields["nonfinite-boundary"] = invalid
	for name, field := range fields {
		t.Run(name, func(t *testing.T) {
			for _, open := range []bool{false, true} {
				for _, workers := range []int{1, 3} {
					for _, skip := range []bool{false, true} {
						m := trimRequireSame(t, field, Options{Workers: workers, OpenBoundary: open, SkipNormals: skip})
						if !open && len(m.Triangles) > 0 {
							requireClosedManifold(t, m)
						}
					}
				}
			}
		})
	}
	// Equality means zero surface distance, identical orientation, degeneracy
	// and topology. Record independent analytic limits on these two solids.
	for _, p := range []struct {
		name         string
		area, volume float64
		euler        int
	}{
		{"sphere-anisotropic", 4 * math.Pi * 36, 4 * math.Pi * 216 / 3, 2},
		{"torus", 4 * math.Pi * math.Pi * 6 * 2, 2 * math.Pi * math.Pi * 6 * 4, 0},
	} {
		m := trimRequireSame(t, fields[p.name], Options{Workers: 2})
		area, volume := massProperties(m)
		if !relClose(area, p.area, 0.01) || !relClose(volume, p.volume, 0.01) || requireClosedManifold(t, m).euler() != p.euler {
			t.Fatalf("%s area=%g volume=%g", p.name, area, volume)
		}
		t.Logf("%s area=%g volume=%g Euler=%d", p.name, area, volume, p.euler)
	}
}

func TestCubesTrimVTKFixtures(t *testing.T) {
	compared := 0
	for _, p := range loadVTKMeshFixture(t).Phantoms {
		if p.FlyingEdges == nil {
			continue
		}
		field := &FuncField{Size: p.Dims, Affine: identityAffine, Value: func(x, y, z int) float64 {
			dx, dy, dz := doubled(x, p.Dims[0]), doubled(y, p.Dims[1]), doubled(z, p.Dims[2])
			if p.Name == "sphere" {
				r := float64(p.Radius)
				return r*r - (dx*dx+dy*dy+dz*dz)/4
			}
			a, b, c := float64(p.SemiAxes[0]), float64(p.SemiAxes[1]), float64(p.SemiAxes[2])
			return 1 - (dx*dx/(4*a*a) + dy*dy/(4*b*b) + dz*dz/(4*c*c))
		}}
		for _, open := range []bool{false, true} {
			m := trimRequireSame(t, field, Options{Workers: 2, OpenBoundary: open, Iso: p.FlyingEdges.Iso})
			area, volume := massProperties(m)
			if len(m.Triangles) != p.FlyingEdges.Triangles || !relClose(area, p.FlyingEdges.Area, 1e-3) || !relClose(volume, p.FlyingEdges.Volume, 1e-3) {
				t.Fatalf("%s VTK mismatch", p.Name)
			}
			requireClosedManifold(t, m)
		}
		compared++
	}
	if compared != 2 {
		t.Fatalf("VTK cases %d", compared)
	}
}

func TestCubesTrimScratchAndBudget(t *testing.T) {
	for _, open := range []bool{false, true} {
		for x := 1; x <= 5; x++ {
			for y := 1; y <= 5; y++ {
				f := &FuncField{Size: [3]int{x, y, 3}, Affine: identityAffine, Value: func(x, y, z int) float64 { return 1 }}
				if open && (x == 1 || y == 1) {
					// run returns before constructing workers for an empty cell grid.
					if m := trimRequireSame(t, f, Options{OpenBoundary: true}); len(m.Vertices) != 0 {
						t.Fatal("thin open grid has vertices")
					}
					continue
				}
				frame, _ := newAffineFrame(f.Affine)
				e := newCubesExtractor(f, frame, Options{OpenBoundary: open})
				w := trimNewWorker(e, true)
				payload := len(w.cases) + 4*(len(w.lo)+len(w.hi)+len(w.scratch)+len(w.zID))
				for i := range w.vals {
					payload += 4*len(w.vals[i]) + len(w.in[i])
				}
				for i := range w.xID {
					payload += 4 * (len(w.xID[i]) + len(w.yID[i]))
				}
				if payload > 48*e.plane {
					t.Fatalf("open=%t %dx%d scratch=%d bound=%d", open, x, y, payload, 48*e.plane)
				}
			}
		}
	}
	f := randomField(1495, [3]int{13, 11, 9}, identityAffine)
	for _, opts := range []Options{{Workers: 2, MaxBytes: 1}, {Workers: 2, MaxTriangles: 1}, {Workers: 2, MaxBytes: 1, SkipNormals: true}} {
		want, err0 := Extract(context.Background(), f, opts)
		got, err1 := trimPrototypeRun(context.Background(), f, opts)
		var b0, b1 *BudgetError
		if want != nil || got != nil || !errors.As(err0, &b0) || !errors.As(err1, &b1) || *b0 != *b1 {
			t.Fatalf("budget outputs=%v/%v errors=%v/%v", want, got, err0, err1)
		}
	}
}

type trimCancelField struct {
	Field
	entered chan struct{}
	active  atomic.Int64
}

func (f *trimCancelField) ReadSlice(ctx context.Context, z int, dst []float32) error {
	f.active.Add(1)
	defer f.active.Add(-1)
	select {
	case f.entered <- struct{}{}:
	default:
	}
	<-ctx.Done()
	return ctx.Err()
}

func TestCubesTrimCancellationJoin(t *testing.T) {
	for _, candidate := range []bool{false, true} {
		f := &trimCancelField{Field: randomField(1, [3]int{19, 17, 15}, identityAffine), entered: make(chan struct{}, 2)}
		ctx, cancel := context.WithCancel(context.Background())
		done := make(chan error, 1)
		go func() {
			var m *Mesh
			var err error
			if candidate {
				m, err = trimPrototypeRun(ctx, f, Options{Workers: 2})
			} else {
				m, err = Extract(ctx, f, Options{Workers: 2})
			}
			if m != nil {
				done <- fmt.Errorf("published partial output")
				return
			}
			done <- err
		}()
		select {
		case <-f.entered:
		case <-time.After(3 * time.Second):
			cancel()
			t.Fatal("read did not start")
		}
		start := time.Now()
		cancel()
		select {
		case err := <-done:
			if !errors.Is(err, context.Canceled) || f.active.Load() != 0 {
				t.Fatalf("candidate=%t err=%v active=%d", candidate, err, f.active.Load())
			}
			t.Logf("candidate=%t cancel join=%s", candidate, time.Since(start))
		case <-time.After(3 * time.Second):
			t.Fatal("workers did not join")
		}
	}
}

type trimLateFaultField struct {
	Field
	active, calls atomic.Int64
	hit           atomic.Bool
	fault         error
	cancel        context.CancelFunc
}

func (f *trimLateFaultField) ReadSlice(ctx context.Context, z int, dst []float32) error {
	f.active.Add(1)
	defer f.active.Add(-1)
	// The counting pass reads at most nz+(workers*4-1) actual slices.
	// With nz=15/workers=2, read 30 necessarily belongs to generation.
	if f.calls.Add(1) >= 30 {
		f.hit.Store(true)
		if f.cancel != nil {
			f.cancel()
			return ctx.Err()
		}
		return f.fault
	}
	return f.Field.ReadSlice(ctx, z, dst)
}

func TestCubesTrimGenerationFailureAtomic(t *testing.T) {
	fault := errors.New("test field generation failure")
	for _, candidate := range []bool{false, true} {
		for _, cancelled := range []bool{false, true} {
			ctx, cancel := context.WithCancel(context.Background())
			f := &trimLateFaultField{Field: randomField(1, [3]int{19, 17, 15}, identityAffine), fault: fault}
			want := fault
			if cancelled {
				f.cancel = cancel
				want = context.Canceled
			}
			var m *Mesh
			var err error
			if candidate {
				m, err = trimPrototypeRun(ctx, f, Options{Workers: 2})
			} else {
				m, err = Extract(ctx, f, Options{Workers: 2})
			}
			cancel()
			if !f.hit.Load() || m != nil || !errors.Is(err, want) || f.active.Load() != 0 {
				t.Fatalf("candidate=%t cancelled=%t hit=%t output=%v err=%v active=%d", candidate, cancelled, f.hit.Load(), m, err, f.active.Load())
			}
		}
	}
}

type trimLatencyField struct {
	Field
	calls, started, active atomic.Int64
	trigger                int64
	cancel                 context.CancelFunc
	epoch                  time.Time
}

func (f *trimLatencyField) ReadSlice(ctx context.Context, z int, dst []float32) error {
	f.active.Add(1)
	defer f.active.Add(-1)
	err := f.Field.ReadSlice(ctx, z, dst)
	if f.calls.Add(1) == f.trigger {
		f.started.Store(time.Since(f.epoch).Nanoseconds() + 1)
		f.cancel()
	}
	// Return the real read result, so cancellation must join the current
	// plane's CPU work instead of just returning an injected read error.
	return err
}

func TestCubesTrimCancellationLatency(t *testing.T) {
	for _, corpus := range []string{"sparse", "dense"} {
		input := trimPerfField(corpus)
		for _, phase := range []string{"count", "generate"} {
			trigger := int64(8)
			if phase == "generate" {
				trigger = 192
			} // Count reads at most96+7 actual slices.
			for _, candidate := range []bool{false, true} {
				ctx, cancel := context.WithCancel(context.Background())
				f := &trimLatencyField{Field: input, trigger: trigger, cancel: cancel, epoch: time.Now()}
				var m *Mesh
				var err error
				if candidate {
					m, err = trimPrototypeRun(ctx, f, Options{Workers: 2})
				} else {
					m, err = Extract(ctx, f, Options{Workers: 2})
				}
				end := time.Since(f.epoch).Nanoseconds()
				cancel()
				if f.started.Load() == 0 || m != nil || !errors.Is(err, context.Canceled) || f.active.Load() != 0 {
					t.Fatalf("%s/%s candidate=%t output=%v err=%v active=%d trigger=%d reads=%d", corpus, phase, candidate, m, err, f.active.Load(), trigger, f.calls.Load())
				}
				t.Logf("corpus=%s phase=%s candidate=%t reads=%d cancel_tail_ns=%d", corpus, phase, candidate, f.calls.Load(), end-(f.started.Load()-1))
			}
		}
	}
}
