package registration

import (
	"context"
	"errors"
	"math"
	"reflect"
	"testing"

	"github.com/ThalesMMS/dicom-go/render"
)

// The six axial points determine a full-rank 3D fit. Moving only the central
// point preserves the cross-covariance: all three LS families fit I and 4/7 X.
// No KD-tree or landmark residuals are used to compute the metric oracle.
func icpAnalyticClouds() ([]render.Vec3, []render.Vec3) {
	fixed := []render.Vec3{{}, {X: 100}, {X: -100}, {Y: 100}, {Y: -100}, {Z: 100}, {Z: -100}}
	moving := append([]render.Vec3(nil), fixed...)
	moving[0].X = 4
	return fixed, moving
}

func icpBruteMetrics(fixed, moving []render.Vec3, transform Transform) (float64, float64) {
	sum, sum2 := 0.0, 0.0
	for _, p := range fixed {
		q := transform.TransformPoint(p)
		d2 := math.Inf(1)
		for _, target := range moving {
			d := q.Sub(target)
			d2 = math.Min(d2, d.Dot(d))
		}
		sum += math.Sqrt(d2)
		sum2 += d2
	}
	return sum / float64(len(fixed)), math.Sqrt(sum2 / float64(len(fixed)))
}

func requireICPMetrics(t *testing.T, result ICPResult, fixed, moving []render.Vec3) {
	t.Helper()
	mean, rms := icpBruteMetrics(fixed, moving, result.Transform)
	if math.Abs(mean-result.MeanDistance) > 1e-12 || math.Abs(rms-result.RMS) > 1e-12 {
		t.Fatalf("returned transform metrics mean/RMS %g/%g, report %g/%g", mean, rms, result.MeanDistance, result.RMS)
	}
	if result.Converged != (result.StopReason == ICPStopTolerance || result.StopReason == ICPStopInitiallyAligned) {
		t.Fatalf("inconsistent stopping status: %+v", result)
	}
}

func TestICPMeanWorseningIsNotConvergence(t *testing.T) {
	fixed, moving := icpAnalyticClouds()
	initial := NewAffine()
	beforeMean, beforeRMS := icpBruteMetrics(fixed, moving, initial)
	if math.Abs(beforeMean-4.0/7) > 1e-12 || math.Abs(beforeRMS-4/math.Sqrt(7)) > 1e-12 {
		t.Fatal("analytic initial metrics differ")
	}
	for _, mode := range []LandmarkMode{LandmarkRigid, LandmarkSimilarity, LandmarkAffine} {
		t.Run(mode.String(), func(t *testing.T) {
			result, err := IterativeClosestPoint(context.Background(), fixed, moving, ICPOptions{Mode: mode, Initial: initial})
			if err != nil {
				t.Fatal(err)
			}
			requireICPMetrics(t, result, fixed, moving)
			if result.Converged || result.StopReason != ICPStopWorsened || result.Iterations != 1 {
				t.Fatalf("mean worsening accepted: %+v", result)
			}
			requireMatrixNear(t, result.Transform.Matrix(), Identity3(), 1e-12)
			if result.Transform.Offset().Sub(render.Vec3{X: 4.0 / 7}).Length() > 1e-12 {
				t.Fatal("unexpected analytical fit")
			}
			if math.Abs(result.MeanDistance-48.0/49) > 1e-12 || math.Abs(result.RMS-4*math.Sqrt(6)/7) > 1e-12 {
				t.Fatal("analytic final metrics differ")
			}
			if !(result.MeanDistance > beforeMean && result.RMS < beforeRMS) {
				t.Fatal("fixture does not separate mean from least-squares RMS")
			}
			// This is also the displacement RMS: every point moves by 4/7 mm.
			movement := 0.0
			for _, p := range fixed {
				d := result.Transform.TransformPoint(p).Sub(p)
				movement += d.Dot(d)
			}
			if math.Abs(math.Sqrt(movement/7)-4.0/7) > 1e-12 {
				t.Fatal("unexpected fit displacement")
			}
		})
	}
}

func TestICPStopUsesReturnedRefitNotPreviousCorrespondences(t *testing.T) {
	fixed, _ := icpAnalyticClouds()
	moving := applyLinear(Identity3(), render.Vec3{X: 4, Y: -3, Z: 2}, fixed)
	beforeMean, _ := icpBruteMetrics(fixed, moving, NewAffine())
	for _, tc := range []struct {
		tol       float64
		reason    ICPStopReason
		converged bool
	}{{100, ICPStopTolerance, true}, {1e-10, ICPStopMaximumIterations, false}, {beforeMean, ICPStopMaximumIterations, false}} {
		result, err := IterativeClosestPoint(nil, fixed, moving, ICPOptions{Initial: NewAffine(), MaxIterations: 1, Tolerance: tc.tol})
		if err != nil {
			t.Fatal(err)
		}
		requireICPMetrics(t, result, fixed, moving)
		if result.Iterations != 1 || result.StopReason != tc.reason || result.Converged != tc.converged || result.MeanDistance > 1e-12 {
			t.Fatalf("post-refit status: %+v", result)
		}
	}
}

func TestICPObjectivePlateauAndExactAlignment(t *testing.T) {
	fixed, _ := icpAnalyticClouds()
	for _, mode := range []LandmarkMode{LandmarkRigid, LandmarkSimilarity, LandmarkAffine} {
		result, err := IterativeClosestPoint(context.Background(), fixed, fixed, ICPOptions{Mode: mode, Initial: NewAffine()})
		if err != nil {
			t.Fatal(err)
		}
		requireICPMetrics(t, result, fixed, fixed)
		if !result.Converged || result.Iterations != 0 || result.StopReason != ICPStopInitiallyAligned || result.MeanDistance != 0 {
			t.Fatalf("exact alignment: %+v", result)
		}
	}
	// The analytical LS solution is an objective plateau with nonzero error.
	// StopTolerance denotes small improvement, not a residual-distance bound.
	fixed, moving := icpAnalyticClouds()
	initial := NewAffine()
	_ = initial.SetOffset(render.Vec3{X: 4.0 / 7})
	result, err := IterativeClosestPoint(nil, fixed, moving, ICPOptions{Initial: initial})
	if err != nil {
		t.Fatal(err)
	}
	requireICPMetrics(t, result, fixed, moving)
	if !result.Converged || result.StopReason != ICPStopTolerance || result.Iterations != 1 || result.MeanDistance < .9 {
		t.Fatalf("objective plateau: %+v", result)
	}
}

func TestICPInitiallyAlignedReturnsOwnedCloneWithoutRefitRoundoff(t *testing.T) {
	fixed, _ := icpAnalyticClouds()
	rotation, _ := VersorFromAxisAngle(render.Vec3{X: 1, Y: 2, Z: 3}, .123)
	for _, tc := range []struct {
		mode   LandmarkMode
		matrix Matrix3
	}{
		{LandmarkRigid, rotation.Matrix()},
		{LandmarkSimilarity, rotation.Matrix().Scale(1.25)},
		{LandmarkAffine, Matrix3{{1.1, .02, 0}, {.01, .95, .03}, {0, .02, 1.2}}},
	} {
		t.Run(tc.mode.String(), func(t *testing.T) {
			initial := NewAffine()
			_ = initial.SetMatrix(tc.matrix)
			_ = initial.SetOffset(render.Vec3{X: 4, Y: -3, Z: 2})
			moving := make([]render.Vec3, len(fixed))
			for i, p := range fixed {
				moving[i] = initial.TransformPoint(p)
			}
			result, err := IterativeClosestPoint(nil, fixed, moving, ICPOptions{Mode: tc.mode, Initial: initial})
			if err != nil {
				t.Fatal(err)
			}
			requireICPMetrics(t, result, fixed, moving)
			if result.StopReason != ICPStopInitiallyAligned || result.Iterations != 0 || !result.Converged || result.MeanDistance != 0 || result.RMS != 0 {
				t.Fatalf("initially aligned: %+v", result)
			}
			_ = initial.SetOffset(render.Vec3{X: 40})
			requireICPMetrics(t, result, fixed, moving)
			if result.MeanDistance != 0 {
				t.Fatal("result retained mutable caller initial")
			}
		})
	}
	// An exactly matching initial shear cannot bypass a requested rigid family.
	initial := NewAffine()
	_ = initial.SetMatrix(Matrix3{{1, .2, 0}, {0, 1, 0}, {0, 0, 1}})
	moving := make([]render.Vec3, len(fixed))
	for i, p := range fixed {
		moving[i] = initial.TransformPoint(p)
	}
	result, err := IterativeClosestPoint(nil, fixed, moving, ICPOptions{Initial: initial})
	if err != nil {
		t.Fatal(err)
	}
	requireICPMetrics(t, result, fixed, moving)
	if result.StopReason == ICPStopInitiallyAligned || result.Iterations != 1 || !result.Transform.Matrix().orthogonal(DefaultOrthogonalityTolerance) {
		t.Fatalf("incompatible family bypass: %+v", result)
	}
}

func TestICPKnownFamiliesAndDeterminism(t *testing.T) {
	fixed, _ := icpAnalyticClouds()
	rotation, _ := VersorFromAxisAngle(render.Vec3{X: 1, Y: 2, Z: 3}, 0.03)
	offset := render.Vec3{X: 4, Y: -3, Z: 2}
	for _, tc := range []struct {
		mode   LandmarkMode
		matrix Matrix3
	}{{LandmarkRigid, rotation.Matrix()}, {LandmarkSimilarity, rotation.Matrix().Scale(1.03)}, {LandmarkAffine, Matrix3{{1.04, .01, -.02}, {.03, .98, .01}, {-.01, .02, 1.01}}}} {
		t.Run(tc.mode.String(), func(t *testing.T) {
			moving := applyLinear(tc.matrix, offset, fixed)
			beforeFixed, beforeMoving := append([]render.Vec3(nil), fixed...), append([]render.Vec3(nil), moving...)
			first, err := IterativeClosestPoint(nil, fixed, moving, ICPOptions{Mode: tc.mode})
			if err != nil {
				t.Fatal(err)
			}
			requireICPMetrics(t, first, fixed, moving)
			requireMatrixNear(t, first.Transform.Matrix(), tc.matrix, 1e-11)
			if first.Transform.Offset().Sub(offset).Length() > 1e-10 || first.MeanDistance > 1e-10 {
				t.Fatalf("known transform not recovered: %+v", first)
			}
			for i := 0; i < 4; i++ {
				got, err := IterativeClosestPoint(nil, fixed, moving, ICPOptions{Mode: tc.mode})
				if err != nil {
					t.Fatal(err)
				}
				if !reflect.DeepEqual(got, first) {
					t.Fatal("same input/options changed output")
				}
			}
			if !reflect.DeepEqual(fixed, beforeFixed) || !reflect.DeepEqual(moving, beforeMoving) {
				t.Fatal("input clouds mutated")
			}
		})
	}
}

func TestICPPartialObservationOutliersAndSampling(t *testing.T) {
	var fixed []render.Vec3
	for z := 0; z < 2; z++ {
		for y := 0; y < 3; y++ {
			for x := 0; x < 4; x++ {
				fixed = append(fixed, render.Vec3{X: float64(x * 30), Y: float64(y * 35), Z: float64(z * 40)})
			}
		}
	}
	initial := NewAffine()
	_ = initial.SetOffset(render.Vec3{X: 4, Y: -3, Z: 2})
	var moving []render.Vec3
	for i, p := range fixed {
		if i != 3 && i != 17 {
			moving = append(moving, initial.TransformPoint(p))
		}
	}
	moving = append(moving, render.Vec3{X: 1000, Y: 1000, Z: 1000}, render.Vec3{X: -1000, Y: -1000, Z: -1000})
	for _, mode := range []LandmarkMode{LandmarkRigid, LandmarkSimilarity, LandmarkAffine} {
		result, err := IterativeClosestPoint(nil, fixed, moving, ICPOptions{Mode: mode, Initial: initial, MaxIterations: 8})
		if err != nil {
			t.Fatal(err)
		}
		requireICPMetrics(t, result, fixed, moving)
		if math.IsNaN(result.MeanDistance) || math.IsInf(result.RMS, 0) || result.Iterations > 8 {
			t.Fatal("invalid partial/outlier outcome")
		}
	}
	fixed, moving = icpAnalyticClouds()
	result, err := IterativeClosestPoint(nil, fixed, moving, ICPOptions{Initial: NewAffine(), MaxFixedPoints: 4, MaxIterations: 1})
	if err != nil {
		t.Fatal(err)
	}
	// Existing sampling is fixed[i*len(fixed)/MaxFixedPoints], not the full cloud.
	sample := []render.Vec3{fixed[0], fixed[1], fixed[3], fixed[5]}
	requireICPMetrics(t, result, sample, moving)
}

type icpOpaqueInitial struct{ Transform }

func TestICPNonMatrixInitialAndNearestTieRemainAcceptedDeterministic(t *testing.T) {
	fixed, _ := icpAnalyticClouds()
	result, err := IterativeClosestPoint(nil, fixed, fixed, ICPOptions{Initial: icpOpaqueInitial{NewAffine()}})
	if err != nil {
		t.Fatal(err)
	}
	requireICPMetrics(t, result, fixed, fixed)
	if result.Iterations != 1 || result.StopReason != ICPStopTolerance {
		t.Fatalf("non-matrix initial changed path: %+v", result)
	}
	// The center has two equally close targets. Repeat the same input/order;
	// no guarantee of invariance to arbitrary permutation of tie candidates.
	moving := append([]render.Vec3(nil), fixed...)
	moving[0] = render.Vec3{X: 4}
	moving = append(moving, render.Vec3{X: -4})
	first, err := IterativeClosestPoint(nil, fixed, moving, ICPOptions{Initial: NewAffine()})
	if err != nil {
		t.Fatal(err)
	}
	requireICPMetrics(t, first, fixed, moving)
	for i := 0; i < 12; i++ {
		result, err := IterativeClosestPoint(nil, fixed, moving, ICPOptions{Initial: NewAffine()})
		if err != nil {
			t.Fatal(err)
		}
		if !reflect.DeepEqual(result, first) {
			t.Fatal("nearest tie changed output for identical input")
		}
	}
}

type icpCancelTransform struct {
	Transform
	calls  int
	cancel context.CancelFunc
}

func (s *icpCancelTransform) TransformPoint(p render.Vec3) render.Vec3 {
	s.calls++
	if s.calls == 17 {
		s.cancel()
	}
	return s.Transform.TransformPoint(p)
}

func TestICPCancelIsBoundedAndReturnsNoPartialResult(t *testing.T) {
	fixed := make([]render.Vec3, 512)
	for i := range fixed {
		fixed[i] = render.Vec3{X: float64(i % 8), Y: float64((i / 8) % 8), Z: float64(i / 64)}
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	initial := &icpCancelTransform{Transform: NewAffine(), cancel: cancel}
	result, err := IterativeClosestPoint(ctx, fixed, fixed, ICPOptions{Initial: initial})
	if !errors.Is(err, context.Canceled) || result.Transform != nil || result.StopReason != "" {
		t.Fatalf("partial result on cancel: %+v %v", result, err)
	}
	if initial.calls > 256 || initial.calls < 17 {
		t.Fatalf("cancel checked after %d points", initial.calls)
	}
	ctx, cancel = context.WithCancel(context.Background())
	cancel()
	if _, err = IterativeClosestPoint(ctx, fixed, fixed, ICPOptions{}); !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
}

func TestICPRejectsInvalidOptionsDegeneracyAndUnrepresentableDistances(t *testing.T) {
	fixed, moving := icpAnalyticClouds()
	for _, opts := range []ICPOptions{{Tolerance: -1}, {Tolerance: math.NaN()}, {Tolerance: math.Inf(1)}, {MaxIterations: -1}, {MaxFixedPoints: 3}, {Mode: LandmarkMode(9)}} {
		if _, err := IterativeClosestPoint(nil, fixed, moving, opts); !errors.Is(err, ErrLandmarks) {
			t.Fatalf("options %+v: %v", opts, err)
		}
	}
	line := []render.Vec3{{}, {X: 1}, {X: 2}, {X: 3}}
	plane := []render.Vec3{{}, {X: 1}, {Y: 1}, {X: 1, Y: 1}}
	for _, tc := range []struct {
		cloud []render.Vec3
		mode  LandmarkMode
	}{{line, LandmarkRigid}, {plane, LandmarkAffine}} {
		if _, err := IterativeClosestPoint(nil, tc.cloud, tc.cloud, ICPOptions{Mode: tc.mode}); !errors.Is(err, ErrLandmarks) {
			t.Fatalf("degenerate input: %v", err)
		}
	}
	hugeFixed, hugeMoving := icpAnalyticClouds()
	for i := range hugeFixed {
		hugeFixed[i] = hugeFixed[i].Scale(1e155)
		hugeMoving[i] = hugeMoving[i].Scale(1e155)
	}
	if _, err := IterativeClosestPoint(nil, hugeFixed, hugeMoving, ICPOptions{Initial: NewAffine()}); !errors.Is(err, ErrLandmarks) {
		t.Fatalf("unrepresentable distance: %v", err)
	}
}
