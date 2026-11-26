package roi

import (
	"context"
	"errors"
	"fmt"
	"math"
	"reflect"
	"sync"
	"sync/atomic"
	"testing"
)

func pcaCircleGeometry() PCAShapeGeometry {
	return PCAShapeGeometry{Dimensions: [3]int{41, 41, 1}, SpacingMM: [3]float64{.5, .8, 1}, OriginMM: [3]float64{10, -20, 3}, Direction: [9]float64{1, 0, 0, 0, 1, 0, 0, 0, 1}}
}
func pcaCircleField(g PCAShapeGeometry, radius float64) []float64 {
	field := make([]float64, g.Dimensions[0]*g.Dimensions[1]*g.Dimensions[2])
	cx, cy := float64(g.Dimensions[0]-1)/2, float64(g.Dimensions[1]-1)/2
	for y := 0; y < g.Dimensions[1]; y++ {
		for x := 0; x < g.Dimensions[0]; x++ {
			field[y*g.Dimensions[0]+x] = math.Hypot((float64(x)-cx)*g.SpacingMM[0], (float64(y)-cy)*g.SpacingMM[1]) - radius
		}
	}
	return field
}
func pcaCircleModel(t testing.TB) *PCAShapeModel {
	t.Helper()
	g := pcaCircleGeometry()
	var samples []PCAShapeTrainingSample
	for i, r := range []float64{2, 3, 4, 5, 6} {
		samples = append(samples, PCAShapeTrainingSample{ID: fmt.Sprintf("train-radius-%d", i), Geometry: g, FieldMM: pcaCircleField(g, r)})
	}
	m, e := TrainPCAShapeModel(context.Background(), samples, PCAShapeTrainOptions{ID: "analytic-circles-v1", Provenance: "project-authored analytic negative-inside circle SDFs; radii 2,3,4,5,6 mm; held-out 4.75 mm", Components: 2})
	if e != nil {
		t.Fatal(e)
	}
	return m
}
func TestPCAShapeTrainAnalyticHeldOutAndOwnership(t *testing.T) {
	m := pcaCircleModel(t)
	g := pcaCircleGeometry()
	d, e := m.Data(nil, PCAShapeLimits{})
	if e != nil {
		t.Fatal(e)
	}
	if len(d.Components) != 1 {
		t.Fatalf("analytic radius family rank %d", len(d.Components))
	}
	n := len(d.MeanMM)
	sigma := math.Sqrt(float64(n) * 2.5)
	if math.Abs(d.StandardDeviationsMM[0]-sigma) > 1e-10 {
		t.Fatalf("sigma=%g want=%g", d.StandardDeviationsMM[0], sigma)
	}
	mean := pcaCircleField(g, 4)
	for i, v := range d.MeanMM {
		if math.Abs(v-mean[i]) > 1e-12 || math.Abs(d.Components[0][i]-1/math.Sqrt(float64(n))) > 1e-12 {
			t.Fatalf("mean/basis at %d", i)
		}
	}
	held := pcaCircleField(g, 4.75)
	fit, e := m.Fit(nil, g, held, 3, .1, PCAShapeLimits{})
	if e != nil {
		t.Fatal(e)
	}
	if fit.OutsideDistribution || fit.ResidualRMSMM > 1e-12 || math.Abs(fit.CoefficientsSD[0]+.75/math.Sqrt(2.5)) > 1e-12 {
		t.Fatal(fit)
	}
	points := make([][3]float64, n)
	for y := 0; y < g.Dimensions[1]; y++ {
		for x := 0; x < g.Dimensions[0]; x++ {
			points[y*g.Dimensions[0]+x] = g.point(x, y, 0)
		}
	}
	r, e := m.Evaluate(nil, fit.CoefficientsSD, IdentityPCAShapePose(), points, PCARejectOutside, PCAShapeLimits{})
	if e != nil {
		t.Fatal(e)
	}
	for i, v := range r.FieldMM {
		if math.Abs(v-held[i]) > 1e-12 {
			t.Fatalf("held-out at %d %g != %g", i, v, held[i])
		}
	}
	// Snapshot/evaluation outputs cannot mutate the immutable shared model.
	d.MeanMM[0] = 1e6
	d.Components[0][0] = 1e6
	r.FieldMM[0] = 1e6
	r, e = m.Evaluate(nil, fit.CoefficientsSD, IdentityPCAShapePose(), points[:1], PCARejectOutside, PCAShapeLimits{})
	if e != nil || math.Abs(r.FieldMM[0]-held[0]) > 1e-12 {
		t.Fatalf("ownership %v", e)
	}
	ood, e := m.Fit(nil, g, pcaCircleField(g, 9), 3, .1, PCAShapeLimits{})
	if e != nil || !ood.OutsideDistribution {
		t.Fatalf("coefficient OOD: %+v %v", ood, e)
	}
	oval := append([]float64(nil), held...)
	for y := 0; y < g.Dimensions[1]; y++ {
		for x := 0; x < g.Dimensions[0]; x++ {
			oval[y*g.Dimensions[0]+x] = math.Hypot((float64(x)-20)*.5/1.4, (float64(y)-20)*.8) - 4.75
		}
	}
	ood, e = m.Fit(nil, g, oval, 3, .1, PCAShapeLimits{})
	if e != nil || !ood.OutsideDistribution || ood.ResidualRMSMM <= .1 {
		t.Fatalf("residual OOD: %+v %v", ood, e)
	}
}

func TestPCAShapePoseAndPhysicalOrientation(t *testing.T) {
	m := pcaCircleModel(t)
	g := pcaCircleGeometry()
	pose := IdentityPCAShapePose()
	pose.TranslationMM = [3]float64{g.SpacingMM[0], -g.SpacingMM[1], 0}
	p := g.point(12, 24, 0)
	r, e := m.Evaluate(nil, []float64{0}, pose, [][3]float64{p}, PCARejectOutside, PCAShapeLimits{})
	if e != nil {
		t.Fatal(e)
	}
	want := pcaCircleField(g, 4)[23*41+13]
	if math.Abs(r.FieldMM[0]-want) > 1e-12 {
		t.Fatalf("pose mm %g want %g", r.FieldMM[0], want)
	}
	if _, e = m.Evaluate(nil, []float64{0}, pose, [][3]float64{g.point(40, 0, 0)}, PCARejectOutside, PCAShapeLimits{}); !errors.Is(e, ErrPCAShapeOutside) {
		t.Fatal(e)
	}
	r, e = m.Evaluate(nil, []float64{0}, pose, [][3]float64{g.point(40, 0, 0)}, PCANearestBoundary, PCAShapeLimits{})
	if e != nil || r.OutsideSamples != 1 {
		t.Fatalf("explicit extrapolation %v %+v", e, r)
	}
	d, _ := m.Data(nil, PCAShapeLimits{})
	d.Geometry.Direction = [9]float64{0, -1, 0, 1, 0, 0, 0, 0, 1}
	rotated, e := NewPCAShapeModel(nil, d, PCAShapeLimits{})
	if e != nil {
		t.Fatal(e)
	}
	pt := d.Geometry.point(12, 24, 0)
	r, e = rotated.Evaluate(nil, []float64{0}, IdentityPCAShapePose(), [][3]float64{pt}, PCARejectOutside, PCAShapeLimits{})
	if e != nil || math.Abs(r.FieldMM[0]-d.MeanMM[24*41+12]) > 1e-12 {
		t.Fatalf("physical direction %v", e)
	}
}

func TestPCAShapeInvalidModelsBudgetsAndCancellation(t *testing.T) {
	m := pcaCircleModel(t)
	base, _ := m.Data(nil, PCAShapeLimits{})
	for name, mutate := range map[string]func(*PCAShapeModelData){
		"sign-convention": func(d *PCAShapeModelData) { d.Convention = "positive-inside" },
		"nonfinite":       func(d *PCAShapeModelData) { d.MeanMM[0] = math.NaN() },
		"basis":           func(d *PCAShapeModelData) { d.Components[0][0] += 1 },
		"sigma":           func(d *PCAShapeModelData) { d.StandardDeviationsMM[0] = 0 },
		"length":          func(d *PCAShapeModelData) { d.Components[0] = d.Components[0][:2] },
		"orientation":     func(d *PCAShapeModelData) { d.Geometry.Direction[0] = 2 },
		"spacing":         func(d *PCAShapeModelData) { d.Geometry.SpacingMM[0] = math.Inf(1) },
	} {
		t.Run(name, func(t *testing.T) {
			d, _ := m.Data(nil, PCAShapeLimits{})
			mutate(&d)
			if out, e := NewPCAShapeModel(nil, d, PCAShapeLimits{}); out != nil || !errors.Is(e, ErrPCAShapeInvalid) {
				t.Fatalf("%+v %v", out, e)
			}
		})
	}
	if out, e := NewPCAShapeModel(nil, base, PCAShapeLimits{MaxBytes: 100}); out != nil || !errors.Is(e, ErrPCAShapeLimit) {
		t.Fatal(e)
	}
	if out, e := NewPCAShapeModel(nil, base, PCAShapeLimits{MaxWork: 10}); out != nil || !errors.Is(e, ErrPCAShapeLimit) {
		t.Fatal(e)
	}
	samples := []PCAShapeTrainingSample{{ID: "a", Geometry: base.Geometry, FieldMM: base.MeanMM}, {ID: "b", Geometry: base.Geometry, FieldMM: base.MeanMM}}
	samples[1].Geometry.SpacingMM[1] *= 2
	if out, e := TrainPCAShapeModel(nil, samples, PCAShapeTrainOptions{ID: "id", Provenance: "synthetic"}); out != nil || !errors.Is(e, ErrPCAShapeInvalid) {
		t.Fatal(e)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if out, e := TrainPCAShapeModel(ctx, samples, PCAShapeTrainOptions{}); out != nil || !errors.Is(e, context.Canceled) {
		t.Fatal(e)
	}
	if out, e := m.Evaluate(ctx, nil, PCAShapePose{}, nil, 0, PCAShapeLimits{}); out != nil || !errors.Is(e, context.Canceled) {
		t.Fatal(e)
	}
	if out, e := m.Fit(ctx, PCAShapeGeometry{}, nil, 1, 1, PCAShapeLimits{}); out != nil || !errors.Is(e, context.Canceled) {
		t.Fatal(e)
	}
}

func TestPCAShapeConcurrentImmutableEvaluation(t *testing.T) {
	m := pcaCircleModel(t)
	g := pcaCircleGeometry()
	var wg sync.WaitGroup
	for i := 0; i < 16; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			r, e := m.Evaluate(nil, []float64{0}, IdentityPCAShapePose(), [][3]float64{g.point(20, 20, 0)}, PCARejectOutside, PCAShapeLimits{})
			if e != nil || math.Abs(r.FieldMM[0]+4) > 1e-12 {
				t.Errorf("concurrent %v %+v", e, r)
			} else {
				r.FieldMM[0] = 99
			}
		}()
	}
	wg.Wait()
}

func pcaPriorInputs(t testing.TB) (*PCAShapeModel, LevelSetVolume, LevelSetInitialization, ShapeDetectionParams, PCAShapePriorOptions) {
	t.Helper()
	m := pcaCircleModel(t)
	g := pcaCircleGeometry()
	mask := NewRasterMask(41, 41)
	field := pcaCircleField(g, 2)
	for y := 0; y < 41; y++ {
		for x := 0; x < 41; x++ {
			if field[y*41+x] < 0 {
				mask.SetRun(y, x, x+1)
			}
		}
	}
	v := LevelSetVolume{Columns: 41, Rows: 41, Slices: 1, SpacingMM: g.SpacingMM, ValueAt: func(x, y, z int) (float64, bool) { return 0, true }}
	p := ShapeDetectionParams{Input: EdgePotentialImage, Stopping: FixedIterations, MaxIterations: 150, MaximumRMSChange: 1e-8}
	fit, e := m.Fit(nil, g, pcaCircleField(g, 4.75), 3, .1, PCAShapeLimits{})
	if e != nil {
		t.Fatal(e)
	}
	prior := PCAShapePriorOptions{Model: m, Geometry: g, Pose: IdentityPCAShapePose(), CoefficientsSD: fit.CoefficientsSD, Weight: 1, MaxResidualMM: 10}
	return m, v, LevelSetInitialization{Masks: map[int]*RasterMask{0: mask}}, p, prior
}

type pcaCancelAfterContext struct {
	context.Context
	calls  atomic.Int32
	at     int32
	cancel context.CancelFunc
}

func (c *pcaCancelAfterContext) Err() error {
	if c.calls.Add(1) >= c.at {
		c.cancel()
	}
	return c.Context.Err()
}
func TestPCAShapeInFlightTrainFitAndEvaluationCancel(t *testing.T) {
	f := pcaNumpyFixture(t)
	samples := make([]PCAShapeTrainingSample, len(f.Training))
	for i, s := range f.Training {
		samples[i] = PCAShapeTrainingSample{ID: s.ID, Geometry: f.Geometry, FieldMM: append([]float64(nil), s.FieldMM...)}
	}
	makeContext := func(at int32) *pcaCancelAfterContext {
		ctx, cancel := context.WithCancel(context.Background())
		t.Cleanup(cancel)
		return &pcaCancelAfterContext{Context: ctx, at: at, cancel: cancel}
	}
	// Cancellation checks 1..9 validate the input fields; the tenth happens
	// while training computes its mean. Input storage must remain untouched.
	ctx := makeContext(10)
	if model, e := TrainPCAShapeModel(ctx, samples, PCAShapeTrainOptions{ID: "cancel", Provenance: "synthetic", Components: 6}); model != nil || !errors.Is(e, context.Canceled) || ctx.calls.Load() < 10 {
		t.Fatalf("train %v", e)
	}
	for i, s := range samples {
		if !reflect.DeepEqual(s.FieldMM, f.Training[i].FieldMM) {
			t.Fatal("training input modified")
		}
	}
	m := pcaCircleModel(t)
	g := pcaCircleGeometry()
	ctx = makeContext(3)
	if fit, e := m.Fit(ctx, g, pcaCircleField(g, 4.75), 3, .1, PCAShapeLimits{}); fit != nil || !errors.Is(e, context.Canceled) {
		t.Fatalf("fit %v", e)
	}
	points := make([][3]float64, 2048)
	for i := range points {
		points[i] = g.point(20, 20, 0)
	}
	ctx = makeContext(3)
	if result, e := m.Evaluate(ctx, []float64{0}, IdentityPCAShapePose(), points, PCARejectOutside, PCAShapeLimits{}); result != nil || !errors.Is(e, context.Canceled) {
		t.Fatalf("evaluate %v", e)
	}
}
func TestPCAShapePriorZeroPreservesBaseAndInfluence(t *testing.T) {
	_, v, init, p, prior := pcaPriorInputs(t)
	o := LevelSetOptions{MarginMM: 100}
	base, e := ShapeDetectionLevelSet(nil, v, init, p, o)
	if e != nil {
		t.Fatal(e)
	}
	prior.Weight = 0
	off, e := ShapePriorDetectionLevelSet(nil, v, init, p, o, prior)
	if e != nil || !reflect.DeepEqual(base, off.Segmentation) {
		t.Fatalf("weight zero %v", e)
	}
	prior.Weight = 1
	on, e := ShapePriorDetectionLevelSet(nil, v, init, p, o, prior)
	if e != nil {
		t.Fatal(e)
	}
	if on.Segmentation.VoxelCount <= base.VoxelCount || on.Prior.FinalFieldRMSMM >= on.Prior.InitialFieldRMSMM || on.Prior.FinalMeanPotential >= on.Prior.InitialMeanPotential || on.Prior.ModelID != "analytic-circles-v1" || on.Prior.Weight != 1 {
		t.Fatalf("influence before=%d after=%d %+v", base.VoxelCount, on.Segmentation.VoxelCount, on.Prior)
	}
	want := pcaCircleField(prior.Geometry, 4.75)
	intersection, total := 0, 0
	mask := on.Segmentation.Masks[0]
	for y := 0; y < 41; y++ {
		for x := 0; x < 41; x++ {
			a, b := mask.Get(x, y), want[y*41+x] < 0
			if a && b {
				intersection++
			}
			if a {
				total++
			}
			if b {
				total++
			}
		}
	}
	dice := 2 * float64(intersection) / float64(total)
	t.Logf("held-out radius Dice=%g, field RMS %g -> %g mm, iterations=%d", dice, on.Prior.InitialFieldRMSMM, on.Prior.FinalFieldRMSMM, on.Segmentation.Iterations)
	if dice < .95 {
		t.Fatalf("held-out mismatch Dice %g", dice)
	}
}
func TestPCAShapePriorRefusalAndInFlightCancellation(t *testing.T) {
	_, v, init, p, prior := pcaPriorInputs(t)
	reads := 0
	original := v.ValueAt
	v.ValueAt = func(x, y, z int) (float64, bool) { reads++; return original(x, y, z) }
	bad := prior
	bad.Geometry.SpacingMM[0] *= 2
	if r, e := ShapePriorDetectionLevelSet(nil, v, init, p, LevelSetOptions{}, bad); r != nil || !errors.Is(e, ErrPCAShapeInvalid) || reads != 0 {
		t.Fatalf("geometry before source: %v %d", e, reads)
	}
	if r, e := ShapePriorDetectionLevelSet(nil, v, init, p, LevelSetOptions{}, PCAShapePriorOptions{Weight: math.NaN()}); r != nil || !errors.Is(e, ErrPCAShapeInvalid) || reads != 0 {
		t.Fatal(e)
	}
	bad = prior
	bad.Limits.MaxBytes = 100
	if r, e := ShapePriorDetectionLevelSet(nil, v, init, p, LevelSetOptions{}, bad); r != nil || !errors.Is(e, ErrPCAShapeLimit) || reads != 0 {
		t.Fatalf("budget before source: %v %d", e, reads)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	o := LevelSetOptions{MarginMM: 100, Progress: func(progress LevelSetProgress) {
		if progress.Iteration == 2 {
			cancel()
		}
	}}
	if r, e := ShapePriorDetectionLevelSet(ctx, v, init, p, o, prior); r != nil || !errors.Is(e, context.Canceled) || !errors.Is(e, ErrLevelSetCancelled) {
		t.Fatalf("inflight result %v err %v", r, e)
	}
	v.ValueAt = func(x, y, z int) (float64, bool) { return 0, x != 20 || y != 20 }
	if r, e := ShapePriorDetectionLevelSet(nil, v, init, p, LevelSetOptions{MarginMM: 100}, prior); r != nil || !errors.Is(e, ErrPCAShapeInvalid) {
		t.Fatalf("dense field %v", e)
	}
}

func TestPCAShapePriorReactionMatchesPotentialGradient(t *testing.T) {
	g := &levelSetGrid{nx: 3, ny: 3, nz: 1, n: 9, size: [3]int{3, 3, 1}, stride: [3]int{1, 3, 9}, h: [3]float64{1, 1, 1}, hmin: 1, hmax: 1, dims: 2}
	phi, target, edge := make([]float32, 9), make([]float32, 9), make([]float32, 9)
	for y := 0; y < 3; y++ {
		for x := 0; x < 3; x++ {
			i := y*3 + x
			phi[i] = float32(x - 1)
			target[i] = phi[i] - .25
			edge[i] = 1
		}
	}
	s := levelSetState{levelSetGrid: g, phi: phi}
	baseTerms := levelSetTerms{propagation: .2, curvature: .1, speed: edge, curvatureWeight: edge}
	base, _, _ := s.rate(4, baseTerms)
	weighted := baseTerms
	weighted.shapePrior = &pcaShapeEvolution{options: PCAShapePriorOptions{Weight: 2}, target: target}
	got, first, _ := s.rate(4, weighted)
	epsilon := 1e-6
	potential := func(v float64) float64 { delta := v - float64(target[4]); return delta * delta } // weight/2 = 1
	gradient := (potential(float64(phi[4])+epsilon) - potential(float64(phi[4])-epsilon)) / (2 * epsilon)
	if math.Abs(float64(got-base)+gradient) > 1e-7 || math.Abs(first-(.2+.5)) > 1e-7 {
		t.Fatalf("reaction vs energy gradient: base=%g got=%g gradient=%g first=%g", base, got, gradient, first)
	}
}
