package registration

import (
	"context"
	"errors"
	"math"
	"sync"
	"testing"

	"github.com/ThalesMMS/dicom-go/render"
)

func warpCube() []render.Vec3 {
	var p []render.Vec3
	for x := 0; x < 2; x++ {
		for y := 0; y < 2; y++ {
			for z := 0; z < 2; z++ {
				p = append(p, render.Vec3{X: float64(x), Y: float64(y), Z: float64(z)})
			}
		}
	}
	return p
}

// Independent exact-family oracle: cube alternating weights annihilate 1,x,y,z.
// Thus no fitted coefficients are used to define either knot or off-knot truth.
func warpAnalytic(p render.Vec3, amplitude float64) render.Vec3 {
	d := 0.0
	for _, c := range warpCube() {
		sgn := 1.0
		if int(c.X+c.Y+c.Z)%2 != 0 {
			sgn = -1
		}
		q := p.Sub(c)
		d -= sgn * math.Sqrt(q.X*q.X+q.Y*q.Y+q.Z*q.Z)
	}
	p.X += amplitude * d
	return p
}
func warpMoving(p []render.Vec3, amplitude float64) []render.Vec3 {
	out := make([]render.Vec3, len(p))
	for i, x := range p {
		out[i] = warpAnalytic(x, amplitude)
	}
	return out
}
func warpRequireNear(t *testing.T, a, b render.Vec3, tol float64) {
	t.Helper()
	if a.Sub(b).Length() > tol {
		t.Fatalf("got%v want%v tolerance%g", a, b, tol)
	}
}

func TestLandmarkWarpIdentityAffine(t *testing.T) {
	ctx := context.Background()
	fixed := warpCube()
	for _, lambda := range []float64{0, .1, 10} {
		for _, c := range []struct {
			matrix   Matrix3
			offset   render.Vec3
			identity bool
		}{
			{matrix: Identity3(), identity: true},
			{matrix: Identity3(), offset: render.Vec3{X: 7, Y: -3, Z: 2}},
			{matrix: Matrix3{{1.2, .3, 0}, {0, .9, .2}, {.1, 0, 1.1}}, offset: render.Vec3{X: 7, Y: -3, Z: 2}},
		} {
			matrix, off := c.matrix, c.offset
			moving := make([]render.Vec3, len(fixed))
			for i, p := range fixed {
				moving[i] = matrix.MulVec(p).Add(off)
			}
			fit, err := FitLandmarkWarp3D(ctx, fixed, moving, LandmarkWarpOptions{Regularization: lambda})
			if err != nil {
				t.Fatal(err)
			}
			if fit.Diagnostics.FRE > 1e-10 || len(fit.Diagnostics.Residuals) != 8 || fit.Diagnostics.Condition < 1 {
				t.Fatal(fit.Diagnostics)
			}
			queries := []render.Vec3{{X: .2, Y: .3, Z: .4}, {X: -2, Y: 3, Z: 5}}
			if c.identity {
				queries = append(queries, fixed...)
			}
			for _, p := range queries {
				got, e := fit.Warp.PointContext(ctx, p)
				if e != nil {
					t.Fatal(e)
				}
				warpRequireNear(t, got, matrix.MulVec(p).Add(off), 1e-10)
				j, e := fit.Warp.JacobianContext(ctx, p)
				if e != nil {
					t.Fatal(e)
				}
				for r := 0; r < 3; r++ {
					for c := 0; c < 3; c++ {
						if math.Abs(j[r][c]-matrix[r][c]) > 1e-10 {
							t.Fatal(j, matrix)
						}
					}
				}
			}
		}
	}
}

func TestLandmarkWarpNonlinearIndependentValues(t *testing.T) {
	ctx := context.Background()
	fixed := warpCube()
	moving := warpMoving(fixed, .4)
	fit, err := FitLandmarkWarp3D(ctx, fixed, moving, LandmarkWarpOptions{})
	if err != nil {
		t.Fatal(err)
	}
	for _, p := range append(append([]render.Vec3{}, fixed...), render.Vec3{X: .2, Y: .3, Z: .4}, render.Vec3{X: 1.3, Y: .7, Z: -.2}) {
		got, e := fit.Warp.PointContext(ctx, p)
		if e != nil {
			t.Fatal(e)
		}
		warpRequireNear(t, got, warpAnalytic(p, .4), 1e-10)
	}
	if fit.Diagnostics.FRE > 1e-10 {
		t.Fatal(fit.Diagnostics.FRE)
	}
}

func TestLandmarkWarpRegularizationPhysicalScale(t *testing.T) {
	ctx := context.Background()
	fixed := warpCube()
	moving := warpMoving(fixed, .4)
	zero, e := FitLandmarkWarp3D(ctx, fixed, moving, LandmarkWarpOptions{})
	if e != nil {
		t.Fatal(e)
	}
	smooth, e := FitLandmarkWarp3D(ctx, fixed, moving, LandmarkWarpOptions{Regularization: .5})
	if e != nil {
		t.Fatal(e)
	}
	if !(smooth.Diagnostics.FRE > zero.Diagnostics.FRE+1e-5) {
		t.Fatal("regularization must retain residual", smooth.Diagnostics)
	}
	// Cube weight vector is an eigenvector of K; exact smoothed amplitude scales
	// by k/(k+lambda), where k is its row0 signed-distance dot product.
	k := 0.0
	for _, c := range fixed {
		sign := 1.0
		if int(c.X+c.Y+c.Z)%2 != 0 {
			sign = -1
		}
		k -= sign * math.Sqrt(c.X*c.X+c.Y*c.Y+c.Z*c.Z)
	}
	p := render.Vec3{X: .2, Y: .3, Z: .4}
	v, e := smooth.Warp.PointContext(ctx, p)
	if e != nil {
		t.Fatal(e)
	}
	warpRequireNear(t, v, warpAnalytic(p, .4*k/(k+.5)), 1e-10)
	for _, factor := range []float64{.001, 1000} {
		f, m := make([]render.Vec3, 8), make([]render.Vec3, 8)
		for i := range f {
			f[i] = fixed[i].Scale(factor)
			m[i] = moving[i].Scale(factor)
		}
		scaled, e := FitLandmarkWarp3D(ctx, f, m, LandmarkWarpOptions{Regularization: .5 * factor})
		if e != nil {
			t.Fatal(e)
		}
		got, e := scaled.Warp.PointContext(ctx, p.Scale(factor))
		if e != nil {
			t.Fatal(e)
		}
		warpRequireNear(t, got.Scale(1/factor), v, 1e-10)
	}
}

func TestLandmarkWarpInvalidAndConditionedSystems(t *testing.T) {
	fixed := warpCube()
	moving := warpMoving(fixed, .4)
	cases := []struct {
		name string
		f, m []render.Vec3
		o    LandmarkWarpOptions
		want error
	}{
		{"few", fixed[:3], moving[:3], LandmarkWarpOptions{}, ErrLandmarkWarpInput},
		{"mismatch", fixed, moving[:7], LandmarkWarpOptions{}, ErrLandmarkWarpInput},
		{"condition", fixed, moving, LandmarkWarpOptions{MaxCondition: 1}, ErrLandmarkWarpSystem},
		{"lambda", fixed, moving, LandmarkWarpOptions{Regularization: -1}, ErrLandmarkWarpInput},
		{"nonfiniteoption", fixed, moving, LandmarkWarpOptions{Regularization: math.Inf(1)}, ErrLandmarkWarpInput},
	}
	for _, conflict := range []bool{false, true} {
		f := append([]render.Vec3{}, fixed...)
		m := append([]render.Vec3{}, moving...)
		f[7] = f[0]
		m[7] = m[0]
		if conflict {
			m[7].X++
		}
		cases = append(cases, struct {
			name string
			f, m []render.Vec3
			o    LandmarkWarpOptions
			want error
		}{"duplicate", f, m, LandmarkWarpOptions{Regularization: 1}, ErrLandmarkWarpInput})
	}
	f := []render.Vec3{{}, {X: 1}, {Y: 1}, {X: 1, Y: 1}}
	cases = append(cases, struct {
		name string
		f, m []render.Vec3
		o    LandmarkWarpOptions
		want error
	}{"coplanar", f, f, LandmarkWarpOptions{Regularization: 1}, ErrLandmarkWarpSystem})
	f = append([]render.Vec3{}, fixed...)
	f[1].Z = math.NaN()
	cases = append(cases, struct {
		name string
		f, m []render.Vec3
		o    LandmarkWarpOptions
		want error
	}{"nonfinite", f, moving, LandmarkWarpOptions{}, ErrLandmarkWarpInput})
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			r, e := FitLandmarkWarp3D(context.Background(), c.f, c.m, c.o)
			if !errors.Is(e, c.want) || r.Warp != nil {
				t.Fatal(r, e)
			}
		})
	}
}

func TestLandmarkWarpJacobianFoldAndExtrapolation(t *testing.T) {
	ctx := context.Background()
	fixed := warpCube()
	fit, e := FitLandmarkWarp3D(ctx, fixed, warpMoving(fixed, .4), LandmarkWarpOptions{})
	if e != nil {
		t.Fatal(e)
	}
	p := render.Vec3{X: .2, Y: .3, Z: .4}
	j, e := fit.Warp.JacobianContext(ctx, p)
	if e != nil {
		t.Fatal(e)
	}
	h := 1e-6
	for c := 0; c < 3; c++ {
		d := render.Vec3{}
		switch c {
		case 0:
			d.X = h
		case 1:
			d.Y = h
		case 2:
			d.Z = h
		}
		plus, e := fit.Warp.PointContext(ctx, p.Add(d))
		if e != nil {
			t.Fatal(e)
		}
		minus, e := fit.Warp.PointContext(ctx, p.Sub(d))
		if e != nil {
			t.Fatal(e)
		}
		v := plus.Sub(minus).Scale(.5 / h)
		for r, want := range []float64{v.X, v.Y, v.Z} {
			if math.Abs(j[r][c]-want) > 1e-8 {
				t.Fatal(j, v)
			}
		}
	}
	if _, e = fit.Warp.JacobianContext(ctx, fixed[0]); !errors.Is(e, ErrLandmarkWarpDerivative) {
		t.Fatal("cusp must be explicit", e)
	}
	samples, e := fit.Warp.InspectContext(ctx, []render.Vec3{fixed[0], p, {X: 2, Y: .2, Z: .3}})
	if e != nil || samples[0].JacobianDefined || !samples[1].JacobianDefined || samples[1].OutsideLandmarkBox || !samples[2].OutsideLandmarkBox {
		t.Fatal(samples, e)
	}
	moving := append([]render.Vec3{}, fixed...)
	for i := range moving {
		moving[i].X = -moving[i].X
	}
	ref, e := FitLandmarkWarp3D(ctx, fixed, moving, LandmarkWarpOptions{})
	if e != nil {
		t.Fatal(e)
	}
	samples, e = ref.Warp.InspectContext(ctx, []render.Vec3{p})
	if e != nil || !samples[0].NonPositiveJacobian || math.Abs(samples[0].Determinant+1) > 1e-10 {
		t.Fatal(samples, e)
	}
	// A non-linear fold oracle: det=1+alpha*sum(-sign*(px-cx)/r).
	// This is independent of fitted coefficients and the Jacobian method.
	gradientX := 0.0
	for _, c := range fixed {
		sign := 1.0
		if int(c.X+c.Y+c.Z)%2 != 0 {
			sign = -1
		}
		d := p.Sub(c)
		gradientX -= sign * d.X / math.Sqrt(d.X*d.X+d.Y*d.Y+d.Z*d.Z)
	}
	nonlinear, e := FitLandmarkWarp3D(ctx, fixed, warpMoving(fixed, 20), LandmarkWarpOptions{})
	if e != nil {
		t.Fatal(e)
	}
	samples, e = nonlinear.Warp.InspectContext(ctx, []render.Vec3{p})
	wantDet := 1 + 20*gradientX
	if wantDet >= 0 || e != nil || !samples[0].NonPositiveJacobian || math.Abs(samples[0].Determinant-wantDet) > 1e-9 {
		t.Fatal(samples, e, wantDet)
	}
}

func TestLandmarkWarpBoundsAndCancellation(t *testing.T) {
	fixed := warpCube()
	moving := warpMoving(fixed, .4)
	ctx := context.Background()
	for _, o := range []LandmarkWarpOptions{{MaxLandmarks: 7}, {MaxMatrixBytes: 1}, {MaxScratchBytes: 1}, {MaxSolveWork: 1}, {MaxLandmarks: 1025}} {
		r, e := FitLandmarkWarp3D(ctx, fixed, moving, o)
		if !errors.Is(e, ErrLandmarkWarpLimit) || r.Warp != nil {
			t.Fatal(r, e)
		}
	}
	canceled, cancel := context.WithCancel(ctx)
	cancel()
	r, e := FitLandmarkWarp3D(canceled, fixed, moving, LandmarkWarpOptions{})
	if !errors.Is(e, context.Canceled) || r.Warp != nil {
		t.Fatal(r, e)
	}
	if _, e = FitLandmarkWarp3D(nil, fixed, moving, LandmarkWarpOptions{}); !errors.Is(e, ErrLandmarkWarpInput) {
		t.Fatal(e)
	}
	for _, o := range []LandmarkWarpOptions{{MaxQueries: 1}, {MaxQueryInteractions: 8}, {MaxQueryBytes: 112}} {
		r, e = FitLandmarkWarp3D(ctx, fixed, moving, o)
		if e != nil {
			t.Fatal(e)
		}
		s, e := r.Warp.InspectContext(ctx, []render.Vec3{{X: .2}, {X: .3}})
		if !errors.Is(e, ErrLandmarkWarpLimit) || s != nil {
			t.Fatal(s, e)
		}
	}
	fit, e := FitLandmarkWarp3D(ctx, fixed, moving, LandmarkWarpOptions{})
	if e != nil {
		t.Fatal(e)
	}
	if _, e = fit.Warp.PointContext(canceled, fixed[0]); !errors.Is(e, context.Canceled) {
		t.Fatal(e)
	}
	if _, e = fit.Warp.PointContext(ctx, render.Vec3{X: math.Inf(1)}); !errors.Is(e, ErrLandmarkWarpInput) {
		t.Fatal(e)
	}
	var empty LandmarkWarp3D
	if _, e = empty.InspectContext(ctx, nil); !errors.Is(e, ErrLandmarkWarpInput) {
		t.Fatal(e)
	}
}

func TestLandmarkWarpImmutableConcurrent(t *testing.T) {
	ctx := context.Background()
	fixed := warpCube()
	moving := warpMoving(fixed, .4)
	fit, e := FitLandmarkWarp3D(ctx, fixed, moving, LandmarkWarpOptions{})
	if e != nil {
		t.Fatal(e)
	}
	p := render.Vec3{X: .2, Y: .3, Z: .4}
	want := warpAnalytic(p, .4)
	fixed[0].X = 99
	moving[0].X = 99
	fit.Diagnostics.Residuals[0] = 99
	var wg sync.WaitGroup
	for k := 0; k < 8; k++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := 0; i < 64; i++ {
				got, e := fit.Warp.PointContext(ctx, p)
				if e != nil || got.Sub(want).Length() > 1e-10 {
					t.Errorf("immutable query %v %v", got, e)
				}
				if _, e = fit.Warp.JacobianContext(ctx, p); e != nil {
					t.Error(e)
				}
			}
		}()
	}
	wg.Wait()
}

// Drive real context cancellation at a known admission/solver/query checkpoint.
// This avoids timing-dependent races or pretending a pre-canceled call covers
// cancellation after numeric allocation and during elimination.
type warpCancelCheckpoint struct {
	context.Context
	cancel    context.CancelFunc
	remaining int
}

func (c *warpCancelCheckpoint) Err() error {
	c.remaining--
	if c.remaining == 0 {
		c.cancel()
	}
	return c.Context.Err()
}
func TestLandmarkWarpCancellationDuringWork(t *testing.T) {
	base, cancel := context.WithCancel(context.Background())
	defer cancel()
	ctx := &warpCancelCheckpoint{Context: base, cancel: cancel, remaining: 100}
	fixed := warpCube()
	moving := warpMoving(fixed, .4)
	fit, e := FitLandmarkWarp3D(ctx, fixed, moving, LandmarkWarpOptions{})
	if !errors.Is(e, context.Canceled) || fit.Warp != nil || fit.Diagnostics.MatrixBytes == 0 {
		t.Fatal("solver cancellation", fit, e)
	}
	fit, e = FitLandmarkWarp3D(context.Background(), fixed, moving, LandmarkWarpOptions{})
	if e != nil {
		t.Fatal(e)
	}
	base, cancel = context.WithCancel(context.Background())
	defer cancel()
	ctx = &warpCancelCheckpoint{Context: base, cancel: cancel, remaining: 7}
	samples, e := fit.Warp.InspectContext(ctx, []render.Vec3{{X: .2, Y: .3, Z: .4}, {X: .3, Y: .2, Z: .1}})
	if !errors.Is(e, context.Canceled) || samples != nil {
		t.Fatal("batch must not publish partial samples", samples, e)
	}
}
