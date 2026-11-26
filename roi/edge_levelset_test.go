package roi

import (
	"context"
	"encoding/json"
	"errors"
	"math"
	"os"
	"reflect"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/ThalesMMS/dicom-go/imagefilter"
)

func edgeTestVolume(n int, feature bool) LevelSetVolume {
	return LevelSetVolume{Columns: n, Rows: n, Slices: 1, SpacingMM: [3]float64{1, 1, 0}, ValueAt: func(x, y, z int) (float64, bool) {
		r := math.Hypot(float64(x-n/2), float64(y-n/2))
		if feature {
			return 1 / (1 + 100*math.Exp(-math.Pow((r-9)/.8, 2))), true
		}
		if r < 9 {
			return 100, true
		}
		return 0, true
	}}
}
func edgeTestInit(n int) LevelSetInitialization {
	return LevelSetInitialization{Seeds: []RegionGrowSeed{{X: n / 2, Y: n / 2}}, InitialRadiusMM: 5}
}

func TestEdgeLevelSetTermsAndDistance(t *testing.T) {
	g := &levelSetGrid{ctx: context.Background(), nx: 5, ny: 3, nz: 1, n: 15, size: [3]int{5, 3, 1}, stride: [3]int{1, 5, 15}, h: [3]float64{2, 3, 1}, hmin: 2, hmax: 3, dims: 2}
	edge := make([]float32, 15)
	edge[7] = 1
	d, a, e := g.cannyDistanceFields(edge)
	if e != nil {
		t.Fatal(e)
	}
	for i, v := range d {
		dx, dy := float64(i%5-2)*2, float64(i/5-1)*3
		want := math.Hypot(dx, dy)
		if math.Abs(float64(v)-want) > 1e-6 || math.Abs(float64(a[i])-want*want/2) > 1e-5 {
			t.Fatalf("D/a index%d got%g/%g want%g/%g", i, v, a[i], want, want*want/2)
		}
	}
	// Plane phi=x (mm), |grad|=1 and kappa=0. At x=1, a=(x-4)^2/2
	// has grad(a)=-2mm, so A=1 gives dphi/dt=-2 and attracts outward.
	phi := make([]float32, 15)
	for i := range phi {
		phi[i] = float32(i%5) * 2
	}
	s := &levelSetState{levelSetGrid: g, phi: phi}
	r, _, _ := s.rate(6, levelSetTerms{advection: 1, advectionField: a})
	if r != -2 {
		t.Fatalf("D² advection sign/units %g want -2", r)
	}
	speed := make([]float32, 15)
	for i := range speed {
		speed[i] = .4
	}
	r, _, _ = s.rate(6, levelSetTerms{propagation: 2, curvature: 3, speed: speed, curvatureWeight: speed})
	if math.Abs(float64(r)+.8) > 1e-6 {
		t.Fatalf("shape term %g", r)
	}
}

func TestEdgeLevelSetInputLimitsOwnership(t *testing.T) {
	v := edgeTestVolume(25, false)
	init := edgeTestInit(25)
	sp := DefaultShapeDetectionParams()
	sp.MaxIterations = 10
	var reads int
	original := v.ValueAt
	v.ValueAt = func(x, y, z int) (float64, bool) { reads++; return original(x, y, z) }
	if r, e := ShapeDetectionLevelSet(nil, v, init, sp, LevelSetOptions{Limits: LevelSetLimits{MaxBytes: 1}}); r != nil || !errors.Is(e, ErrLevelSetResourceLimit) || reads != 0 {
		t.Fatalf("quota before callback: r%v e%v reads%d", r, e, reads)
	}
	for _, bad := range []ShapeDetectionParams{{Input: 99}, {Input: EdgePotentialImage, Edge: EdgePotentialParams{SigmaMM: 1}}, {PropagationScaling: math.Inf(1)}, {CurvatureScaling: -1}, {MaxIterations: 10001}} {
		if r, e := ShapeDetectionLevelSet(nil, v, init, bad, LevelSetOptions{}); r != nil || e == nil {
			t.Fatal("bad shape accepted")
		}
	}
	cp := DefaultCannyLevelSetParams()
	cp.MaxIterations = 10
	hole := v
	hole.ValueAt = func(x, y, z int) (float64, bool) {
		if x == 4 && y == 4 {
			return 0, false
		}
		return original(x, y, z)
	}
	if r, e := CannyLevelSet(nil, hole, init, cp, LevelSetOptions{}); r != nil || !errors.Is(e, ErrCannyDomain) {
		t.Fatalf("hole: %v", e)
	}
	constant := v
	constant.ValueAt = func(x, y, z int) (float64, bool) { return 0, true }
	if r, e := CannyLevelSet(nil, constant, init, cp, LevelSetOptions{}); r != nil || !errors.Is(e, ErrCannyNoEdges) {
		t.Fatalf("no edge: %v", e)
	}
	badFeature := v
	badFeature.ValueAt = func(x, y, z int) (float64, bool) { return 1.1, true }
	sp.Input = EdgePotentialImage
	sp.Edge = EdgePotentialParams{}
	if r, e := ShapeDetectionLevelSet(nil, badFeature, init, sp, LevelSetOptions{}); r != nil || e == nil {
		t.Fatal("feature inferred/clamped")
	}
	goodFeature := edgeTestVolume(25, true)
	sp.MaxIterations = 20
	baseline, e := ShapeDetectionLevelSet(nil, goodFeature, init, sp, LevelSetOptions{})
	if e != nil {
		t.Fatal(e)
	}
	var wg sync.WaitGroup
	for k := 0; k < 3; k++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			r, e := ShapeDetectionLevelSet(nil, goodFeature, init, sp, LevelSetOptions{})
			if e != nil || r.VoxelCount != baseline.VoxelCount {
				t.Errorf("concurrent %v", e)
			}
		}()
	}
	wg.Wait()
	for _, m := range baseline.Masks {
		m.Set(0, 0, true)
	}
	again, e := ShapeDetectionLevelSet(nil, goodFeature, init, sp, LevelSetOptions{})
	if e != nil || again.Masks[0].Get(0, 0) {
		t.Fatal("retained output")
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if r, e := CannyLevelSet(ctx, v, init, cp, LevelSetOptions{}); r != nil || !errors.Is(e, context.Canceled) {
		t.Fatalf("pre-cancel %v", e)
	}
	if r, e := ShapeDetectionLevelSet(nil, v, init, DefaultShapeDetectionParams(), LevelSetOptions{Limits: LevelSetLimits{MaxVoxels: 17 << 20}}); r != nil || e == nil {
		t.Fatal("ceiling accepted")
	}
	if r, e := CannyLevelSet(nil, LevelSetVolume{}, init, cp, LevelSetOptions{}); r != nil || e == nil {
		t.Fatal("nil callback accepted")
	}
}

// Deterministic context cancels after a specified number of internal polls;
// it exercises every private stage without timer/CPU scheduling assumptions.
type edgePollContext struct {
	context.Context
	polls, limit int64
}

func (c *edgePollContext) Err() error {
	atomic.AddInt64(&c.polls, 1)
	if c.polls >= c.limit {
		return context.Canceled
	}
	return nil
}
func TestEdgeLevelSetInternalCancellation(t *testing.T) {
	for _, stage := range []string{"distance", "quantile", "band-sort", "potential", "initial", "rates", "result"} {
		t.Run(stage, func(t *testing.T) {
			ctx := &edgePollContext{Context: context.Background(), limit: 2}
			g := &levelSetGrid{ctx: ctx, nx: 128, ny: 128, nz: 1, n: 16384, size: [3]int{128, 128, 1}, stride: [3]int{1, 128, 16384}, h: [3]float64{1, 1, 1}, hmin: 1, hmax: 1, dims: 2, image: make([]float32, 16384), bounded: true}
			switch stage {
			case "distance":
				edge := make([]float32, g.n)
				edge[0] = 1
				_, _, e := g.cannyDistanceFields(edge)
				if !errors.Is(e, context.Canceled) {
					t.Fatalf("%v", e)
				}
			case "quantile":
				data := make([]float32, g.n)
				for i := range data {
					data[i] = float32(g.n - i)
				}
				_, e := quantile32Context(ctx, data, .5)
				if !errors.Is(e, context.Canceled) {
					t.Fatalf("%v", e)
				}
			case "band-sort":
				data := make([]int32, g.n)
				for i := range data {
					data[i] = int32(i)
				}
				if e := sortLevelSetBand(ctx, data); !errors.Is(e, context.Canceled) {
					t.Fatalf("%v", e)
				}
			case "potential":
				_, _, _, e := g.edgePotential(EdgePotentialParams{}, LevelSetInitialization{}, .5)
				if !errors.Is(e, context.Canceled) {
					t.Fatalf("%v", e)
				}
			case "initial":
				_, e := g.initialLevelSet(LevelSetInitialization{}, nil)
				if !errors.Is(e, context.Canceled) {
					t.Fatalf("%v", e)
				}
			case "rates":
				s := &levelSetState{levelSetGrid: g, phi: make([]float32, g.n), band: make([]int32, g.n)}
				for i := range s.band {
					s.band[i] = int32(i)
				}
				s.rates(levelSetTerms{}, make([]float32, g.n))
				if ctx.polls < 2 {
					t.Fatal("no poll")
				}
			case "result":
				g.volume = LevelSetVolume{Columns: g.nx, Rows: g.ny, Slices: 1}
				if r := g.result(func(int) bool { return true }); r != nil {
					t.Fatal("partial result on cancellation")
				}
			}
		})
	}
	// Cancel in callback after image fill begins; nil result and typed error.
	ctx, cancel := context.WithCancel(context.Background())
	v := edgeTestVolume(25, false)
	base := v.ValueAt
	calls := 0
	v.ValueAt = func(x, y, z int) (float64, bool) {
		calls++
		if calls == 1 {
			cancel()
		}
		return base(x, y, z)
	}
	if r, e := CannyLevelSet(ctx, v, edgeTestInit(25), DefaultCannyLevelSetParams(), LevelSetOptions{}); r != nil || !errors.Is(e, context.Canceled) {
		t.Fatalf("sampling cancellation %v", e)
	}
}

func TestEdgeLevelSetQuantileMatchesLegacy(t *testing.T) {
	for _, v := range [][]float32{{}, {1}, {2, 1, 2, 3, 0}, {1, 1, 1}, {9, 8, 7, 6, 5, 4, 3, 2, 1, 0}} {
		for _, q := range []float64{0, .25, .5, .75, 1} {
			before := append([]float32(nil), v...)
			got, e := quantile32Context(context.Background(), v, q)
			if e != nil || got != quantile32(v, q) || len(v) != len(before) || !reflect.DeepEqual(v, before) && len(v) > 0 {
				t.Fatalf("q%g v%v: %g %v", q, v, got, e)
			}
		}
	}
}

type frozenEdgeCase struct {
	Name, Kind, Profile                                 string
	Dims                                                [3]int
	Spacing                                             [3]float64
	Data, Feature                                       []float64
	Initial, Mask                                       []int
	MaxIterations                                       int     `json:"max_iterations"`
	MaximumRMS                                          float64 `json:"maximum_rms"`
	Propagation, Curvature, Advection, Sigma, Threshold float64
}

func readFrozenEdgeCases(t *testing.T) ([]frozenEdgeCase, []frozenEdgeCase) {
	t.Helper()
	b, e := os.ReadFile("testdata/edge_levelset/itk-5.4.json")
	if e != nil {
		t.Fatal(e)
	}
	var doc struct {
		Cases    []frozenEdgeCase
		Positive []frozenEdgeCase `json:"positive_cases"`
	}
	if e = json.Unmarshal(b, &doc); e != nil {
		t.Fatal(e)
	}
	if len(doc.Cases) != 4 || len(doc.Positive) != 4 {
		t.Fatalf("frozen original/positive cases %d/%d", len(doc.Cases), len(doc.Positive))
	}
	return doc.Cases, doc.Positive
}
func compareFrozenEdgeCase(t *testing.T, c frozenEdgeCase, positive bool) (*LevelSetResult, OverlapMetrics) {
	t.Helper()
	init := LevelSetInitialization{Masks: map[int]*RasterMask{}}
	for z := 0; z < c.Dims[2]; z++ {
		m := NewRasterMask(c.Dims[0], c.Dims[1])
		for y := 0; y < c.Dims[1]; y++ {
			start := -1
			for x := 0; x <= c.Dims[0]; x++ {
				on := x < c.Dims[0] && c.Initial[(z*c.Dims[1]+y)*c.Dims[0]+x] != 0
				if on && start < 0 {
					start = x
				} else if !on && start >= 0 {
					m.SetRun(y, start, x)
					start = -1
				}
			}
		}
		init.Masks[z] = m
	}
	values := c.Data
	if c.Kind == "shape" {
		values = c.Feature
	}
	v := LevelSetVolume{Columns: c.Dims[0], Rows: c.Dims[1], Slices: c.Dims[2], SpacingMM: c.Spacing, ValueAt: func(x, y, z int) (float64, bool) { return values[(z*c.Dims[1]+y)*c.Dims[0]+x], true }}
	var r *LevelSetResult
	var e error
	if c.Kind == "shape" {
		p := ShapeDetectionParams{Input: EdgePotentialImage, PropagationScaling: c.Propagation, CurvatureScaling: c.Curvature, MaxIterations: c.MaxIterations, MaximumRMSChange: c.MaximumRMS}
		if positive {
			p.CurvatureConvention = SumCurvature
			p.Stopping = RMSOnly
			if c.Profile == "curvature-horizon" {
				p.Stopping = FixedIterations
			}
		}
		r, e = ShapeDetectionLevelSet(nil, v, init, p, LevelSetOptions{})
	} else {
		r, e = CannyLevelSet(nil, v, init, CannyLevelSetParams{Canny: imagefilter.CannyParameters{SigmaMM: imagefilter.IsotropicSigma(c.Sigma), LowerThreshold: c.Threshold, UpperThreshold: c.Threshold}, PropagationScaling: c.Propagation, CurvatureScaling: c.Curvature, AdvectionScaling: c.Advection, MaxIterations: c.MaxIterations, MaximumRMSChange: c.MaximumRMS}, LevelSetOptions{})
	}
	if e != nil {
		t.Fatal(e)
	}
	got, want := NewVolumeMask(c.Dims[0], c.Dims[1], c.Dims[2], c.Spacing), NewVolumeMask(c.Dims[0], c.Dims[1], c.Dims[2], c.Spacing)
	for i := range c.Mask {
		x, y, z := i%c.Dims[0], i/c.Dims[0]%c.Dims[1], i/(c.Dims[0]*c.Dims[1])
		got.Voxels[i] = r.Masks[z] != nil && r.Masks[z].Get(x, y)
		want.Voxels[i] = c.Mask[i] != 0
	}
	metrics, e := CompareMasks(context.Background(), got, want)
	if e != nil {
		t.Fatal(e)
	}
	t.Logf("Go count%d ITK count%d Dice%.9f Hausdorff%.9f iterations%d RMS%.9f stop%d sourceBoundary%v", got.Count(), want.Count(), metrics.Dice, metrics.HausdorffMM, r.Iterations, r.RMSChange, r.StopReason, r.TouchesVolumeBoundary)
	if !finiteNonNegative(metrics.Dice) || !finiteNonNegative(metrics.HausdorffMM) {
		t.Fatalf("nonfinite comparison %+v", metrics)
	}
	return r, metrics
}
func TestEdgeLevelSetFrozenITK(t *testing.T) {
	original, positive := readFrozenEdgeCases(t)
	for _, c := range original {
		t.Run(c.Name, func(t *testing.T) {
			r, m := compareFrozenEdgeCase(t, c, false)
			h := math.Max(c.Spacing[0], c.Spacing[1])
			if c.Dims[2] > 1 {
				h = math.Max(h, c.Spacing[2])
			}
			if c.Kind == "shape" {
				// These two original oracle comparisons remain incompatible under the
				// original frozen criteria. Characterize the limitation rather than
				// relabel it as parity or replace its feature/mask with the new probes.
				if m.Dice >= .95 && m.HausdorffMM <= 2*h {
					t.Fatal("historical incompatibility changed; review the preserved RED evidence")
				}
				if c.Dims[2] == 1 {
					if r.VoxelCount != 421 || r.StopReason != LevelSetStopStableMask || m.Dice < .4335 || m.Dice > .4337 || m.HausdorffMM < 15.55 || m.HausdorffMM > 15.57 {
						t.Fatalf("historical 2D limit changed: %+v %+v", r, m)
					}
				} else {
					if r.VoxelCount != 24381 || r.StopReason != LevelSetStopIterationLimit || m.Dice < .8927 || m.Dice > .8930 || m.HausdorffMM != 6 {
						t.Fatalf("historical 3D limit changed: %+v %+v", r, m)
					}
				}
				t.Log("LIMIT: original Dice>=.95/Hausdorff<=2h criteria NOT met; original RED retained")
			} else if m.Dice < .95 || m.HausdorffMM > 2*h {
				t.Fatalf("fixed criteria Dice>=.95/Hausdorff<=%g: %+v", 2*h, m)
			}
		})
	}
	for _, c := range positive {
		t.Run(c.Name, func(t *testing.T) {
			r, m := compareFrozenEdgeCase(t, c, true)
			h := math.Max(c.Spacing[0], c.Spacing[1])
			if c.Dims[2] > 1 {
				h = math.Max(h, c.Spacing[2])
			}
			if m.Dice < .95 || m.HausdorffMM > 2*h {
				t.Fatalf("frozen positive criteria Dice>=.95/Hausdorff<=%g: %+v", 2*h, m)
			}
			if c.Profile == "curvature-horizon" && (r.Iterations != 3 || r.StopReason != LevelSetStopIterationLimit) {
				t.Fatalf("fixed horizon %+v", r)
			}
		})
	}
}

func TestShapePublicCharacterization(t *testing.T) {
	b, e := os.ReadFile("testdata/edge_levelset/itk-5.4.json")
	if e != nil {
		t.Fatal(e)
	}
	var doc struct {
		Cases []struct {
			Dims    [3]int
			Spacing [3]float64
			Feature []float64
			Initial []int
		}
	}
	if e = json.Unmarshal(b, &doc); e != nil {
		t.Fatal(e)
	}
	c := doc.Cases[0]
	initial := NewRasterMask(c.Dims[0], c.Dims[1])
	for i, v := range c.Initial {
		if v != 0 {
			initial.Set(i%c.Dims[0], i/c.Dims[0], true)
		}
	}
	volume := LevelSetVolume{Columns: c.Dims[0], Rows: c.Dims[1], Slices: 1, SpacingMM: c.Spacing, ValueAt: func(x, y, z int) (float64, bool) { return c.Feature[y*c.Dims[0]+x], true }}
	init := LevelSetInitialization{Masks: map[int]*RasterMask{0: initial}}
	for _, iterations := range []int{20, 50, 70, 100, 300} {
		r, e := ShapeDetectionLevelSet(nil, volume, init, ShapeDetectionParams{Input: EdgePotentialImage, PropagationScaling: 1, CurvatureScaling: .2, MaxIterations: iterations, MaximumRMSChange: .0005}, LevelSetOptions{})
		if e != nil {
			t.Fatal(e)
		}
		t.Logf("frozen-shape2d max%d iterations%d count%d rms%.9f converged%v", iterations, r.Iterations, r.VoxelCount, r.RMSChange, r.Converged)
	}
	// Independent plane formula and curvature scaling, independent of output
	// masks/timestep internals from either implementation.
	for _, h := range []float64{1, 2} {
		for _, potential := range []float32{1, .01} {
			g := &levelSetGrid{ctx: context.Background(), nx: 33, ny: 33, nz: 1, n: 1089, size: [3]int{33, 33, 1}, stride: [3]int{1, 33, 1089}, h: [3]float64{h, h, 1}, hmin: h, hmax: h, dims: 2}
			phi := make([]float32, g.n)
			speed := make([]float32, g.n)
			for i := range phi {
				phi[i] = float32(float64(i%33-16) * h)
				speed[i] = potential
			}
			s := &levelSetState{levelSetGrid: g, phi: phi}
			rate, first, curve := s.rate(16*33+16, levelSetTerms{propagation: 1, curvature: .2, speed: speed, curvatureWeight: speed})
			if math.Abs(float64(rate)+float64(potential)) > 1e-7 {
				t.Fatalf("plane formula h%g g%g rate%g", h, potential, rate)
			}
			dt := levelSetTimeStep(first, curve, h, 2)
			t.Logf("plane h%g g%g rate%g dt%g displacement%g", h, potential, rate, dt, -float64(rate)*dt)
		}
	}
}

func TestEdgeLevelSetPoliciesAndLegacyFields(t *testing.T) {
	v := edgeTestVolume(25, true)
	init := edgeTestInit(25)
	reads := 0
	orig := v.ValueAt
	v.ValueAt = func(x, y, z int) (float64, bool) { reads++; return orig(x, y, z) }
	for _, p := range []ShapeDetectionParams{{Stopping: 99}, {CurvatureConvention: 99}, {Boundary: 99}, {CurvatureConvention: SumCurvature, CurvatureScaling: math.MaxFloat32}} {
		bad := v
		if p.CurvatureScaling == math.MaxFloat32 {
			bad.Slices = 3
		}
		if r, e := ShapeDetectionLevelSet(nil, bad, init, p, LevelSetOptions{}); r != nil || !errors.Is(e, ErrLevelSetInvalidInput) || reads != 0 {
			t.Fatalf("policy before reads %+v %v reads%d", r, e, reads)
		}
	}
	for _, p := range []CannyLevelSetParams{{Stopping: 99}, {CurvatureConvention: 99}, {Boundary: 99}} {
		if r, e := CannyLevelSet(nil, v, init, p, LevelSetOptions{}); r != nil || !errors.Is(e, ErrLevelSetInvalidInput) || reads != 0 {
			t.Fatalf("Canny policy before reads %+v %v reads%d", r, e, reads)
		}
	}
	p := ShapeDetectionParams{Input: EdgePotentialImage, PropagationScaling: 1, CurvatureScaling: .2, MaxIterations: 100, MaximumRMSChange: .0005}
	// A large numerical tolerance stops RMS-only, but is ignored by the mask
	// and fixed-horizon policies; no mask stopping may claim RMS convergence.
	p.MaximumRMSChange = 100
	p.Stopping = RMSOnly
	r, e := ShapeDetectionLevelSet(nil, v, init, p, LevelSetOptions{})
	if e != nil || r.StopReason != LevelSetStopRMS || r.Iterations != 1 || !r.Converged {
		t.Fatalf("RMS policy %+v %v", r, e)
	}
	p.Stopping = FixedIterations
	p.MaxIterations = 5
	r, e = ShapeDetectionLevelSet(nil, v, init, p, LevelSetOptions{})
	if e != nil || r.StopReason != LevelSetStopIterationLimit || r.Iterations != 5 || r.Converged {
		t.Fatalf("fixed policy %+v %v", r, e)
	}
	p.Stopping = MaskStabilityOnly
	p.MaxIterations = 100
	r, e = ShapeDetectionLevelSet(nil, v, init, p, LevelSetOptions{})
	if e != nil || r.StopReason == LevelSetStopRMS || r.Iterations <= 1 {
		t.Fatalf("mask policy %+v %v", r, e)
	}
	p.Stopping = RMSOnly
	p.MaxIterations = 3
	p.MaximumRMSChange = .00000001
	r, e = ShapeDetectionLevelSet(nil, v, init, p, LevelSetOptions{})
	if e != nil || r.StopReason != LevelSetStopIterationLimit || r.Converged {
		t.Fatalf("RMS iteration cap %+v %v", r, e)
	}
	p.PropagationScaling = 0
	p.CurvatureScaling = 0
	r, e = ShapeDetectionLevelSet(nil, v, init, p, LevelSetOptions{})
	if e != nil || r.StopReason != LevelSetStopZeroTimeStep || r.Iterations != 0 {
		t.Fatalf("zero force %+v %v", r, e)
	}
	// Existing methods leave additive diagnostics unspecified, including source
	// boundary contact, so old result fields retain their observable contract.
	old := DefaultGeodesicActiveContourParams()
	old.MaxIterations = 1
	r, e = GeodesicActiveContour(nil, edgeTestVolume(25, false), init, old, LevelSetOptions{})
	if e != nil || r.StopReason != LevelSetStopUnspecified || r.TouchesVolumeBoundary {
		t.Fatalf("legacy fields %+v %v", r, e)
	}
	oldThreshold := DefaultThresholdLevelSetParams(0, 100)
	oldThreshold.MaxIterations = 1
	r, e = ThresholdLevelSet(nil, edgeTestVolume(25, false), init, oldThreshold, LevelSetOptions{})
	if e != nil || r.StopReason != LevelSetStopUnspecified || r.TouchesVolumeBoundary {
		t.Fatalf("threshold fields %+v %v", r, e)
	}
}

func TestEdgeLevelSetCurvatureAndBoundaryPolicies(t *testing.T) {
	for _, d := range []int{1, 3} {
		got, e := edgeCurvature(.2, SumCurvature, d)
		want := .2
		if d > 1 {
			want = .4
		}
		if e != nil || got != want {
			t.Fatalf("sum curvature %g %v", got, e)
		}
	}
	// A caller-authored full mask has no active front and touches the source
	// boundary. Rejection returns no masks; reporting distinguishes this from RMS.
	v := LevelSetVolume{Columns: 7, Rows: 7, Slices: 1, SpacingMM: [3]float64{2, 3, 0}, ValueAt: func(int, int, int) (float64, bool) { return 1, true }}
	m := NewRasterMask(7, 7)
	for y := 0; y < 7; y++ {
		m.SetRun(y, 0, 7)
	}
	init := LevelSetInitialization{Masks: map[int]*RasterMask{0: m}}
	p := ShapeDetectionParams{Input: EdgePotentialImage, PropagationScaling: 1, MaxIterations: 5}
	r, e := ShapeDetectionLevelSet(nil, v, init, p, LevelSetOptions{})
	if e != nil || !r.TouchesVolumeBoundary || r.StopReason != LevelSetStopEmptyBand {
		t.Fatalf("full region %+v %v", r, e)
	}
	p.Boundary = RejectTouches
	r, e = ShapeDetectionLevelSet(nil, v, init, p, LevelSetOptions{})
	if r != nil || !errors.Is(e, ErrEdgeLevelSetBoundary) || m.Count() != 49 {
		t.Fatalf("reject owns no result %+v %v sourcecount%d", r, e, m.Count())
	}
	// Source z=0 in a 2D crop is inactive; interior x/y points do not touch it.
	g := &levelSetGrid{ctx: context.Background(), volume: v, nx: 3, ny: 3, nz: 1, n: 9, origin: [3]int{2, 2, 0}, size: [3]int{3, 3, 1}, h: [3]float64{2, 3, 1}, image: make([]float32, 9), bounded: true, dims: 2}
	r = g.result(func(i int) bool { return i == 4 })
	if r == nil || r.TouchesVolumeBoundary || r.TouchesRegionBoundary {
		t.Fatalf("2D inactive z %+v", r)
	}
}

func TestEdgeMaskAndDistanceLineCancellation(t *testing.T) {
	m := NewRasterMask(1, 16384)
	ctx := &edgePollContext{Context: context.Background(), limit: 2}
	if e := levelSetMaskRuns(ctx, m, func(int, MaskRun) error { return nil }); !errors.Is(e, context.Canceled) {
		t.Fatalf("empty row cancel %v", e)
	}
	const n = 16384
	f, c, out := make([]float64, n), make([]float64, n), make([]float64, n)
	for i := range f {
		f[i] = math.Inf(1)
		c[i] = float64(i)
	}
	f[n/2] = 0
	ctx = &edgePollContext{Context: context.Background(), limit: 2}
	if e := squaredDistance1DContext(ctx, out, f, c, make([]int, n), make([]float64, n+1)); !errors.Is(e, context.Canceled) {
		t.Fatalf("EDT line cancel %v", e)
	}
}

func TestEdgeLevelSetPhysicalRangeBeforeReads(t *testing.T) {
	for _, h := range []float64{math.MaxFloat64, math.SmallestNonzeroFloat64} {
		v := edgeTestVolume(25, true)
		reads := 0
		v.SpacingMM[0] = h
		v.ValueAt = func(int, int, int) (float64, bool) { reads++; return 1, true }
		if r, e := ShapeDetectionLevelSet(nil, v, edgeTestInit(25), ShapeDetectionParams{Input: EdgePotentialImage}, LevelSetOptions{}); r != nil || !errors.Is(e, ErrLevelSetInvalidInput) || reads != 0 {
			t.Fatalf("range before reads %v reads%d", e, reads)
		}
	}
	v := edgeTestVolume(25, true)
	reads := 0
	v.ValueAt = func(int, int, int) (float64, bool) { reads++; return 1, true }
	init := edgeTestInit(25)
	init.InitialRadiusMM = math.MaxFloat64
	if r, e := CannyLevelSet(nil, v, init, DefaultCannyLevelSetParams(), LevelSetOptions{}); r != nil || !errors.Is(e, ErrLevelSetInvalidInput) || reads != 0 {
		t.Fatalf("radius before reads %v reads%d", e, reads)
	}
}

func TestEdgeResultRunOwnershipAndLegacyEquivalence(t *testing.T) {
	g := &levelSetGrid{ctx: context.Background(), volume: LevelSetVolume{Columns: 5000, Rows: 13, Slices: 1}, nx: 4097, ny: 3, nz: 1, n: 4097 * 3, origin: [3]int{9, 4, 0}, h: [3]float64{1, 1, 1}, image: make([]float32, 4097*3), bounded: true, dims: 2}
	inside := func(i int) bool { return i%2 == 0 }
	r := g.result(inside)
	g.bounded = false
	old := g.result(inside)
	if r == nil || old == nil || !reflect.DeepEqual(r.Masks, old.Masks) || r.VoxelCount != old.VoxelCount || r.VolumeMM3 != old.VolumeMM3 || r.TouchesRegionBoundary != old.TouchesRegionBoundary {
		t.Fatal("row construction changed masks")
	}
	r.Masks[0].runs[4][0].Start++
	if reflect.DeepEqual(r.Masks, old.Masks) {
		t.Fatal("output masks alias")
	}
	m := NewVolumeMask(16384, 1, 1, [3]float64{1, 1, 1})
	ctx := &edgePollContext{Context: context.Background(), limit: 2}
	if r, e := m.DistanceTransform(ctx, true); r != nil || !errors.Is(e, context.Canceled) {
		t.Fatalf("coordinate validation cancel %v", e)
	}
}
