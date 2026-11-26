package registration

import (
	"bytes"
	"context"
	"errors"
	"math"
	"reflect"
	"testing"

	"github.com/ThalesMMS/dicom-go/object"
	"github.com/ThalesMMS/dicom-go/render"
	"github.com/ThalesMMS/dicom-go/spatialreg"
)

// Original analytic diffeomorphism: a smooth x displacement vanishes on every
// grid face, and 1+du/dx is strictly positive for amplitude < (n-1)/pi.
func synKnownDisplacement(x, y, z float64, n int, amplitude float64) float64 {
	return amplitude * math.Sin(math.Pi*x/float64(n-1)) * math.Pow(math.Sin(math.Pi*y/float64(n-1)), 2) * math.Pow(math.Sin(math.Pi*z/float64(n-1)), 2)
}

func synPhantomValue(x, y, z float64) float64 {
	return math.Sin(.65*x) + .6*math.Cos(.57*y) + .5*math.Sin(.49*z) + .2*math.Cos(.3*x+.37*y+.41*z)
}

func synPhantomPair(t testing.TB, n int, amplitude float64) (*Image, *Image) {
	t.Helper()
	spacing := render.Vec3{X: 1, Y: 1, Z: 1}
	f := neighborhoodTestImage(t, [3]int{n, n, n}, spacing, func(x, y, z int) float32 { return float32(synPhantomValue(float64(x), float64(y), float64(z))) })
	m := neighborhoodTestImage(t, f.size, spacing, func(x, y, z int) float32 {
		// Invert the known monotone x map independently by bisection.
		lo, hi := 0.0, float64(n-1)
		for k := 0; k < 60; k++ {
			mid := (lo + hi) / 2
			if mid+synKnownDisplacement(mid, float64(y), float64(z), n, amplitude) < float64(x) {
				lo = mid
			} else {
				hi = mid
			}
		}
		return float32(2.3*synPhantomValue((lo+hi)/2, float64(y), float64(z)) + 3)
	})
	return f, m
}

func TestSyNKnownDiffeomorphismAndInverse(t *testing.T) {
	const n = 16
	const amplitude = .8
	f, m := synPhantomPair(t, n, amplitude)
	o := SyNOptions{Levels: []SyNLevel{{Shrink: 1, Iterations: 60}}, Correlation: NeighborhoodCorrelationOptions{Radius: [3]int{2, 2, 2}}, LearningRateMM: .3, UpdateSigmaMM: .8, Regularization: .1, MinimumImprovement: 1e-9}
	r, err := SyN(context.Background(), f, m, o)
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("last=%+v payload=%d inverse residual=%g", r.Levels, r.EstimatedWorkingBytes, r.MaximumInverseResidualMM)
	grid, err := r.FixedToMoving.Grid()
	if err != nil {
		t.Fatal(err)
	}
	inverse, err := r.MovingToFixed.Grid()
	if err != nil {
		t.Fatal(err)
	}
	var worst, roundTrip float64
	for _, z := range []int{5, 8, 10} {
		for _, y := range []int{5, 8, 10} {
			for _, x := range []int{5, 8, 10} {
				p := render.Vec3{X: float64(x), Y: float64(y), Z: float64(z)}
				u, err := grid.Sample(p)
				if err != nil {
					t.Fatal(err)
				}
				want := render.Vec3{X: synKnownDisplacement(p.X, p.Y, p.Z, n, amplitude)}
				worst = math.Max(worst, u.Sub(want).Length())
				back, err := inverse.Sample(p.Add(u))
				if err != nil {
					t.Fatal(err)
				}
				roundTrip = math.Max(roundTrip, u.Add(back).Length())
			}
		}
	}
	t.Logf("landmark maximum error=%g mm, forward/inverse=%g mm, final gradient bounds=%g/%g", worst, roundTrip, synGradientBound(r.FixedToMoving), synGradientBound(r.MovingToFixed))
	if worst > .3 || roundTrip > .06 || synGradientBound(r.FixedToMoving) >= 1 || synGradientBound(r.MovingToFixed) >= 1 {
		t.Fatal("geometric qualification failed")
	}
	for i := range r.ForwardValid {
		if !r.ForwardValid[i] || !r.InverseValid[i] {
			t.Fatal("unexpected undefined matched-grid output", i)
		}
	}
	reverse, err := SyN(context.Background(), m, f, o)
	if err != nil {
		t.Fatal(err)
	}
	for i, v := range r.MovingToFixed.Vectors {
		if v.Sub(reverse.FixedToMoving.Vectors[i]).Length() > 1e-5 {
			t.Fatalf("swapping images changed the declared inverse at %d", i)
		}
	}
	identity := spatialreg.Matrix{Values: render.GeometryAffine{1, 0, 0, 0, 0, 1, 0, 0, 0, 0, 1, 0, 0, 0, 0, 1}}
	for name, g := range map[string]*spatialreg.Grid{"forward": grid, "inverse": inverse} {
		change, err := (&spatialreg.Registration{Pre: identity, Post: identity, Grid: g}).VolumeChange()
		if err != nil || change.Min <= 0 || change.Folded != 0 {
			t.Fatalf("%s node Jacobians=%+v error=%v", name, change, err)
		}
		t.Logf("%s node Jacobian min=%g max=%g folded=%d", name, change.Min, change.Max, change.Folded)
	}
}

func TestSyNLocalAndMembraneDerivatives(t *testing.T) {
	size := [3]int{5, 4, 3}
	n := 60
	w := synWarped{fixed: make([]float64, n), moving: make([]float64, n), valid: make([]bool, n)}
	for i := range w.fixed {
		w.fixed[i] = math.Sin(float64(i)*.19) + .03*float64(i)
		w.moving[i] = math.Cos(float64(i)*.23) + .02*float64(i)
		w.valid[i] = i%7 != 0
	}
	_, df, dm, _, err := synCorrelation(context.Background(), w, size, [3]int{1, 1, 1}, 0, true)
	if err != nil {
		t.Fatal(err)
	}
	const h = 1e-6
	for _, at := range []int{13, 25, 40} {
		for channel := 0; channel < 2; channel++ {
			data, derivative := w.fixed, df
			if channel == 1 {
				data, derivative = w.moving, dm
			}
			old := data[at]
			data[at] = old + h
			plus, _, _, _, e1 := synCorrelation(context.Background(), w, size, [3]int{1, 1, 1}, 0, false)
			data[at] = old - h
			minus, _, _, _, e2 := synCorrelation(context.Background(), w, size, [3]int{1, 1, 1}, 0, false)
			data[at] = old
			if e1 != nil || e2 != nil || math.Abs((plus-minus)/(2*h)-derivative[at]) > 1e-8 {
				t.Fatal("local field derivative", at, channel, derivative[at], (plus-minus)/(2*h), e1, e2)
			}
		}
	}
	im := neighborhoodTestImage(t, size, render.Vec3{X: .7, Y: 1.1, Z: 1.6}, func(x, y, z int) float32 { return float32(x + y + z) })
	field := newField(im)
	for i := range field.Vectors {
		field.Vectors[i] = render.Vec3{X: .03 * float64(i%5), Y: .02 * float64(i%3)}
	}
	_, gradient, err := synMembrane(context.Background(), field, true)
	if err != nil {
		t.Fatal(err)
	}
	for _, at := range []int{13, 25, 40} {
		old := field.Vectors[at].X
		field.Vectors[at].X = old + h
		plus, _, _ := synMembrane(context.Background(), field, false)
		field.Vectors[at].X = old - h
		minus, _, _ := synMembrane(context.Background(), field, false)
		field.Vectors[at].X = old
		if math.Abs((plus-minus)/(2*h)-gradient[at].X) > 1e-9 {
			t.Fatal("physical membrane derivative", at)
		}
	}
}

func TestSyNRotatedAnisotropicDICOMRoundTrip(t *testing.T) {
	f, m := synPhantomPair(t, 12, .6)
	angle := math.Pi / 6
	direction := Matrix3{{math.Cos(angle), -math.Sin(angle), 0}, {math.Sin(angle), math.Cos(angle), 0}, {0, 0, 1}}
	origin, spacing := render.Vec3{X: 11, Y: -9, Z: 22}, render.Vec3{X: .7, Y: 1.1, Z: 1.6}
	var err error
	f, err = NewImage(f.size, f.data, origin, spacing, direction)
	if err != nil {
		t.Fatal(err)
	}
	m, err = NewImage(m.size, m.data, origin, spacing, direction)
	if err != nil {
		t.Fatal(err)
	}
	r, err := SyN(context.Background(), f, m, SyNOptions{Levels: []SyNLevel{{Shrink: 2, Iterations: 20}, {Shrink: 1, Iterations: 40}}, Correlation: NeighborhoodCorrelationOptions{RadiusMM: [3]float64{1.4, 2.2, 3.2}}, LearningRateMM: .2, UpdateSigmaMM: .7, Regularization: .1})
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		field              *DisplacementField
		registered, source string
	}{{r.FixedToMoving, "1.2.3.4", "1.2.3.5"}, {r.MovingToFixed, "1.2.3.5", "1.2.3.4"}} {
		grid, err := tc.field.Grid()
		if err != nil {
			t.Fatal(err)
		}
		file, err := spatialreg.WriteDeformable(spatialreg.DeformableWriteOptions{RegisteredFrameOfReferenceUID: tc.registered, Registrations: []spatialreg.DeformableRegistration{{SourceFrameOfReferenceUID: tc.source, Grid: grid, Comment: "Original synthetic SyN proposal"}}})
		if err != nil {
			t.Fatal(err)
		}
		var buffer bytes.Buffer
		if err := object.WriteFile(&buffer, file); err != nil {
			t.Fatal(err)
		}
		parsedFile, err := object.ReadFile(bytes.NewReader(buffer.Bytes()))
		if err != nil {
			t.Fatal(err)
		}
		defer parsedFile.Close()
		parsed, err := spatialreg.Read(parsedFile)
		if err != nil {
			t.Fatal(err)
		}
		if parsed.RegisteredFrameOfReferenceUID != tc.registered || len(parsed.Registrations) != 1 || parsed.Registrations[0].SourceFrameOfReferenceUID != tc.source {
			t.Fatal("mapping direction changed")
		}
		decoded := parsed.Registrations[0].Grid
		if decoded.Origin.Sub(origin).Length() > 1e-9 || decoded.Spacing.Sub(spacing).Length() > 1e-9 || decoded.RowDirection.Sub(render.Vec3{X: direction[0][0], Y: direction[1][0], Z: direction[2][0]}).Length() > 1e-9 {
			t.Fatal("physical DICOM geometry changed")
		}
		for _, index := range []render.Vec3{{X: 4, Y: 5, Z: 6}, {X: 6, Y: 4, Z: 5}} {
			p := f.Point(index)
			before, e1 := grid.Sample(p)
			after, e2 := decoded.Sample(p)
			if e1 != nil || e2 != nil || before.Sub(after).Length() > 1e-6 {
				t.Fatal("DICOM deformation changed", before, after, e1, e2)
			}
		}
	}
}

func TestSyNIdentityAndCancelNoProposal(t *testing.T) {
	f, _ := synPhantomPair(t, 8, .5)
	opts := SyNOptions{Levels: []SyNLevel{{Shrink: 2, Iterations: 2}, {Shrink: 1, Iterations: 2}}, Correlation: NeighborhoodCorrelationOptions{Radius: [3]int{1, 1, 1}}}
	r, err := SyN(context.Background(), f, f, opts)
	if err != nil {
		t.Fatal(err)
	}
	for _, field := range []*DisplacementField{r.MiddleToFixed, r.MiddleToMoving, r.FixedToMoving, r.MovingToFixed} {
		for _, v := range field.Vectors {
			if v.Length() > 1e-8 {
				t.Fatal("identity moved", v)
			}
		}
	}
	before := append([]float32(nil), f.data...)
	for level := range opts.Levels {
		ctx, cancel := context.WithCancel(context.Background())
		seen := false
		opts.Progress = func(p SyNProgress) {
			if p.Level == level {
				seen = true
				cancel()
			}
		}
		if r, err := SyN(ctx, f, f, opts); !seen || !errors.Is(err, context.Canceled) || !reflect.DeepEqual(r, SyNResult{}) {
			t.Fatal("partial proposal on level cancellation", level, r, err)
		}
		cancel()
	}
	if !reflect.DeepEqual(before, f.data) {
		t.Fatal("borrowed input changed")
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if r, err := SyN(ctx, f, f, opts); !errors.Is(err, context.Canceled) || r.FixedToMoving != nil {
		t.Fatal("pre-cancel", r, err)
	}
}

func TestSyNUndefinedDomainsAndPayloadAdmission(t *testing.T) {
	f := neighborhoodTestImage(t, [3]int{8, 8, 8}, render.Vec3{X: 1, Y: 1, Z: 1}, func(x, y, z int) float32 { return float32(x + 2*y + 3*z) })
	m, err := NewImage(f.size, append([]float32(nil), f.data...), render.Vec3{X: 2}, f.spacing, f.direction)
	if err != nil {
		t.Fatal(err)
	}
	for i := range m.data {
		m.data[i] += 2
	}
	o := SyNOptions{Levels: []SyNLevel{{Shrink: 1, Iterations: 1}}, Correlation: NeighborhoodCorrelationOptions{Radius: [3]int{1, 1, 1}}}
	_, estimated, err := prepareSyN(context.Background(), f, m, o)
	if err != nil || estimated != 640*512+80*27 {
		t.Fatal("payload estimate", estimated, err)
	}
	o.MaxWorkingBytes = estimated - 1
	if _, err := SyN(context.Background(), f, m, o); !errors.Is(err, ErrSyNResourceLimit) {
		t.Fatal("payload below admission", err)
	}
	o.MaxWorkingBytes = estimated
	r, err := SyN(context.Background(), f, m, o)
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		field *DisplacementField
		valid []bool
	}{{r.FixedToMoving, r.ForwardValid}, {r.MovingToFixed, r.InverseValid}} {
		undefined := 0
		for i, v := range tc.field.Vectors {
			if !tc.valid[i] {
				undefined++
				if !math.IsNaN(v.X) || !math.IsNaN(v.Y) || !math.IsNaN(v.Z) {
					t.Fatal("undefined domain concealed", i, v)
				}
			}
		}
		if undefined != 128 {
			t.Fatal("unexpected outside overlap", undefined)
		}
		grid, err := tc.field.Grid()
		if err != nil {
			t.Fatal(err)
		}
		file, err := spatialreg.WriteDeformable(spatialreg.DeformableWriteOptions{RegisteredFrameOfReferenceUID: "1.2.3.4", Registrations: []spatialreg.DeformableRegistration{{SourceFrameOfReferenceUID: "1.2.3.5", Grid: grid}}})
		if err != nil {
			t.Fatal(err)
		}
		var buffer bytes.Buffer
		if err := object.WriteFile(&buffer, file); err != nil {
			t.Fatal(err)
		}
		parsedFile, err := object.ReadFile(bytes.NewReader(buffer.Bytes()))
		if err != nil {
			t.Fatal(err)
		}
		defer parsedFile.Close()
		parsed, err := spatialreg.Read(parsedFile)
		if err != nil {
			t.Fatal(err)
		}
		decoded := parsed.Registrations[0].Grid
		for i, valid := range tc.valid {
			p := tc.field.Origin.Add(tc.field.Direction.MulVec(render.Vec3{X: float64(i % 8), Y: float64(i / 8 % 8), Z: float64(i / 64)}))
			v, err := decoded.Sample(p)
			if !valid && !errors.Is(err, spatialreg.ErrUndefinedDomain) || valid && (err != nil || v.Length() > 1e-6) {
				t.Fatal("DICOM domain validity changed", i, valid, v, err)
			}
		}
	}
}

func TestSyNAdmissionBeforeProgress(t *testing.T) {
	f, _ := synPhantomPair(t, 8, .5)
	base := SyNOptions{Correlation: NeighborhoodCorrelationOptions{Radius: [3]int{1, 1, 1}}}
	far, err := NewImage(f.size, f.data, render.Vec3{X: 100}, f.spacing, f.direction)
	if err != nil {
		t.Fatal(err)
	}
	if r, err := SyN(context.Background(), f, far, base); !errors.Is(err, ErrTooFewSamples) || r.FixedToMoving != nil {
		t.Fatal("no-overlap proposal", r, err)
	}
	for _, change := range []func(*SyNOptions){
		func(o *SyNOptions) { o.MaxVoxels = 10 }, func(o *SyNOptions) { o.MaxWorkingBytes = 100 }, func(o *SyNOptions) { o.MaxWork = 100 }, func(o *SyNOptions) { o.MaxBacktracks = 30 },
	} {
		o := base
		change(&o)
		o.Progress = func(SyNProgress) { t.Fatal("work before admission") }
		if _, err := SyN(context.Background(), f, f, o); !errors.Is(err, ErrSyNResourceLimit) {
			t.Fatal("resource admission", err)
		}
	}
	for _, change := range []func(*SyNOptions){
		func(o *SyNOptions) { o.LearningRateMM = math.NaN() }, func(o *SyNOptions) { o.MaximumHalfGradient = 1 }, func(o *SyNOptions) { o.UpdateSigmaMM = -1 }, func(o *SyNOptions) { o.Levels = []SyNLevel{{Shrink: 2, Iterations: 1}} },
	} {
		o := base
		change(&o)
		if _, err := SyN(context.Background(), f, f, o); !errors.Is(err, ErrSyN) {
			t.Fatal("invalid option", err)
		}
	}
}
