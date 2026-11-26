package registration

import (
	"context"
	"errors"
	"math"
	"reflect"
	"testing"

	"github.com/ThalesMMS/dicom-go/optimize"
	"github.com/ThalesMMS/dicom-go/render"
)

type coarseSearchQuadratic struct{}

func (coarseSearchQuadratic) Value(x []float64) (float64, error) { return (x[0] - 4) * (x[0] - 4), nil }
func (coarseSearchQuadratic) ValueAndGradient(x []float64) (float64, []float64, error) {
	v, _ := (coarseSearchQuadratic{}).Value(x)
	return v, []float64{2 * (x[0] - 4)}, nil
}

func TestCoarseSearchOnlyFirstLevelAndSnapshot(t *testing.T) {
	starts := [][]float64{{1}, {8}}
	grid := &optimize.Exhaustive{Steps: []int{1}, StepSizes: []float64{1}}
	levels := []int{}
	refine := func(level int, f optimize.GradientFunction, x, scales []float64) (optimize.Result, error) {
		levels = append(levels, level)
		return optimize.Result{X: []float64{4}, Value: -100}, nil
	}
	hook, err := CoarseSearchMinimizer(nil, CoarseSearchOptions{Starts: starts, Grid: grid}, refine)
	if err != nil {
		t.Fatal(err)
	}
	starts[0][0] = 1000
	grid.StepSizes[0] = 1000
	run, err := hook(0, coarseSearchQuadratic{}, []float64{0}, []float64{1})
	if err != nil || run.X[0] != 4 || run.Value != 0 || !reflect.DeepEqual(levels, []int{0, 0}) {
		t.Fatalf("coarse=%+v/%v levels%v", run, err, levels)
	}
	if _, err = hook(1, coarseSearchQuadratic{}, run.X, []float64{1}); err != nil || !reflect.DeepEqual(levels, []int{0, 0, 1}) {
		t.Fatal("later level repeated global search")
	}
	if _, err = CoarseSearchMinimizer(nil, CoarseSearchOptions{Starts: starts, Limits: optimize.SearchLimits{MaxWorkspaceBytes: 1}}, nil); !errors.Is(err, optimize.ErrSearchBudget) {
		t.Fatal("hook snapshot admitted over budget")
	}
}

// Exact voxel-permutation rotations supply an independent image oracle. The
// asymmetric compact object makes the four second-moment signs identifiable by
// a metric; zero padding stays zero under these right-angle grid rotations.
func globalRotationPair(t *testing.T, angle float64) (*Image, *Image, *Euler3D) {
	t.Helper()
	const n = 24
	fixedData, movingData := make([]float32, n*n*n), make([]float32, n*n*n)
	for z := 0; z < n; z++ {
		for y := 0; y < n; y++ {
			for x := 0; x < n; x++ {
				v := 0.0
				for _, b := range [][5]float64{{7, 11, 10, 3, 80}, {15, 9, 14, 2, 140}, {12, 17, 8, 1.5, 230}} {
					d := (math.Pow(float64(x)-b[0], 2) + math.Pow(float64(y)-b[1], 2) + math.Pow(float64(z)-b[2], 2)) / (b[3] * b[3])
					if d < 9 {
						v += b[4] * math.Exp(-d)
					}
				}
				fixedData[(z*n+y)*n+x] = float32(v)
			}
		}
	}
	quarter := int(math.Round(angle/(math.Pi/2))) % 4
	for z := 0; z < n; z++ {
		for y := 0; y < n; y++ {
			for x := 0; x < n; x++ {
				var mx, my int
				switch quarter {
				case 1:
					mx, my = n-1-y, x
				case 2:
					mx, my = n-1-x, n-1-y
				default:
					t.Fatal("fixture rotation")
				}
				movingData[(z*n+my)*n+mx] = fixedData[(z*n+y)*n+x]
			}
		}
	}
	spacing := render.Vec3{X: 1, Y: 1, Z: 1}
	fixed, err := NewImage([3]int{n, n, n}, fixedData, render.Vec3{}, spacing, Identity3())
	if err != nil {
		t.Fatal(err)
	}
	moving, err := NewImage([3]int{n, n, n}, movingData, render.Vec3{}, spacing, Identity3())
	if err != nil {
		t.Fatal(err)
	}
	truth := NewEuler3D()
	_ = truth.SetCenter(render.Vec3{X: 11.5, Y: 11.5, Z: 11.5})
	_ = truth.SetRotation(0, 0, angle)
	return fixed, moving, truth
}

func TestGlobalRegistrationConvergesFrom90And180DegreeStarts(t *testing.T) {
	for _, angle := range []float64{math.Pi / 2, math.Pi} {
		t.Run(fmtAngle(angle), func(t *testing.T) {
			fixed, moving, truth := globalRotationPair(t, angle)
			beforeFixed, beforeMoving := append([]float32(nil), fixed.data...), append([]float32(nil), moving.data...)
			candidates, err := PrincipalAxisCandidates(context.Background(), fixed, moving, PrincipalAxisOptions{})
			if err != nil {
				t.Fatal(err)
			}
			starts := make([][]float64, 4)
			for i, c := range candidates {
				starts[i] = c.Parameters()
			}
			initial := candidates[0].Clone()
			// A coarse rotation grid checks the public hook as well as principal
			// candidates. Local gradient descent refines the selected neighborhood.
			grid := &optimize.Exhaustive{Steps: []int{0, 0, 1, 0, 0, 0}, StepSizes: []float64{1, 1, math.Pi / 12, 1, 1, 1}}
			refine := func(level int, f optimize.GradientFunction, x, scales []float64) (optimize.Result, error) {
				o := optimize.RegularStepGradientDescent{Scales: scales, MaximumIterations: 60, MinimumStepLength: 1e-7, GradientMagnitudeTolerance: 1e-12}
				return o.Minimize(f, x)
			}
			hook, err := CoarseSearchMinimizer(context.Background(), CoarseSearchOptions{Starts: starts, Grid: grid, Limits: optimize.SearchLimits{MaxEvaluations: 2000}}, refine)
			if err != nil {
				t.Fatal(err)
			}
			method := Method{Fixed: fixed, Moving: moving, Transform: initial, NewMetric: func(f, m *Image, d MetricDomain) (Metric, error) { return NewMeanSquares(f, m, d) }, Levels: []Level{{Shrink: 2}, {Shrink: 1}}, Minimize: hook}
			run, err := method.Run(context.Background())
			if err != nil {
				t.Fatal(err)
			}
			estimate := run.Transform.(MatrixTransform)
			product := estimate.Matrix().Mul(truth.Matrix().Transpose())
			cosine := (product[0][0] + product[1][1] + product[2][2] - 1) / 2
			degrees := math.Acos(math.Max(-1, math.Min(1, cosine))) * 180 / math.Pi
			maximum := 0.0
			for _, p := range CornerPoints(fixed) {
				maximum = math.Max(maximum, estimate.TransformPoint(p).Sub(truth.TransformPoint(p)).Length())
			}
			t.Logf("rotation%g degrees: residual %.9g degrees / %.9g mm / value%.9g / coarse%d evaluations", angle*180/math.Pi, degrees, maximum, run.Value, run.Levels[0].Evaluations)
			if degrees > .5 || maximum > .5 {
				t.Fatalf("failed predefined0.5degree/0.5mm acceptance: %g/%g", degrees, maximum)
			}
			if !reflect.DeepEqual(fixed.data, beforeFixed) || !reflect.DeepEqual(moving.data, beforeMoving) {
				t.Fatal("source samples changed")
			}
			if !reflect.DeepEqual(initial.Parameters(), starts[0]) {
				t.Fatal("initial transform changed")
			}
		})
	}
}

func fmtAngle(angle float64) string {
	if angle < 2 {
		return "90"
	}
	return "180"
}

func TestCoarseSearchConcurrentWorkspaceAdmission(t *testing.T) {
	// Independent reservation: hook 8192+8+24+16, and two simultaneous
	// searches each 8192+96+(8+24). One byte less must reject before Value.
	const total int64 = 24880
	for _, capBytes := range []int64{total - 1, total} {
		calls := 0
		f := coarseCountingQuadratic{calls: &calls}
		hook, err := CoarseSearchMinimizer(nil, CoarseSearchOptions{Starts: [][]float64{{1}}, Grid: &optimize.Exhaustive{Steps: []int{1}, StepSizes: []float64{1}}, Limits: optimize.SearchLimits{MaxWorkspaceBytes: capBytes}}, nil)
		if err != nil {
			t.Fatal(err)
		}
		run, err := hook(0, f, []float64{1}, []float64{1})
		if capBytes < total {
			if !errors.Is(err, optimize.ErrSearchBudget) || calls != 0 || len(run.X) != 0 {
				t.Fatalf("below coexisting budget: %+v/%v calls%d", run, err, calls)
			}
		} else if err != nil || run.X[0] != 2 || run.Value != 4 || calls != 5 {
			t.Fatalf("admitted exact budget: %+v/%v calls%d", run, err, calls)
		}
	}
	if _, err := CoarseSearchMinimizer(nil, CoarseSearchOptions{Limits: optimize.SearchLimits{MaxWorkspaceBytes: 1}}, nil); !errors.Is(err, optimize.ErrSearchBudget) {
		t.Fatal("empty options bypassed finite hook reservation")
	}
}

type coarseCountingQuadratic struct{ calls *int }

func (f coarseCountingQuadratic) Value(x []float64) (float64, error) {
	*f.calls++
	return (coarseSearchQuadratic{}).Value(x)
}
func (f coarseCountingQuadratic) ValueAndGradient(x []float64) (float64, []float64, error) {
	*f.calls++
	return (coarseSearchQuadratic{}).ValueAndGradient(x)
}

func TestCoarseSearchLaterRefinementCancellation(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	hook, err := CoarseSearchMinimizer(ctx, CoarseSearchOptions{}, func(level int, f optimize.GradientFunction, x, scales []float64) (optimize.Result, error) {
		cancel()
		return optimize.Result{X: []float64{4}, Value: 0}, nil
	})
	if err != nil {
		t.Fatal(err)
	}
	run, err := hook(1, coarseSearchQuadratic{}, []float64{1}, []float64{1})
	if !errors.Is(err, context.Canceled) || len(run.X) != 0 {
		t.Fatalf("later refinement returned partial success: %+v/%v", run, err)
	}
}
