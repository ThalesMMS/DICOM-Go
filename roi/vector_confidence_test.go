package roi

import (
	"context"
	"errors"
	"math"
	"reflect"
	"sort"
	"testing"

	"github.com/ThalesMMS/dicom-go/render"
)

func vectorConfidenceTestModel(t *testing.T, covariance []float64, params VectorConfidenceParams) (*vectorConfidenceGrower, *vectorConfidenceModel) {
	t.Helper()
	c := int(math.Sqrt(float64(len(covariance))))
	l := DefaultVectorConfidenceLimits()
	if params.MinPivotRatio == 0 {
		params.MinPivotRatio = 1e-12
	}
	g := &vectorConfidenceGrower{ctx: context.Background(), params: params, limits: l}
	m := newVectorConfidenceModel(c)
	copy(m.cov, covariance)
	return g, &m
}

func TestVectorConfidenceMatrixAnalytic(t *testing.T) {
	g, m := vectorConfidenceTestModel(t, []float64{4, 2, 2, 3}, VectorConfidenceParams{})
	if err := m.factor(g); err != nil {
		t.Fatal(err)
	}
	for _, row := range []struct {
		v []float64
		q float64
	}{{[]float64{0, 0}, 0}, {[]float64{2, 1}, 1}, {[]float64{0, 2}, 2}, {[]float64{2, 3}, 3}} {
		d, err := m.distance(g, row.v)
		if err != nil || math.Abs(d*d-row.q) > 2e-14 {
			t.Fatalf("value %v distance %g error %v expected squared %g", row.v, d, err, row.q)
		}
	}
}

func TestVectorConfidencePivotAdmission(t *testing.T) {
	for _, scale := range []float64{1, 1e-100, 1e-300, 1e100} {
		for _, ratio := range []float64{0.5e-12, 2e-12} {
			g, m := vectorConfidenceTestModel(t, []float64{scale, 0, 0, scale * ratio}, VectorConfidenceParams{})
			err := m.factor(g)
			if ratio < 1e-12 && !errors.Is(err, ErrVectorConfidenceCovariance) || ratio > 1e-12 && err != nil {
				t.Fatalf("scale=%g ratio=%g error=%v", scale, ratio, err)
			}
		}
	}
	// Representable subnormal diagonal ratios must not silently pass a pivot
	// threshold rounded to zero by multiplication.
	g, m := vectorConfidenceTestModel(t, []float64{1e-310, 0, 0, math.SmallestNonzeroFloat64}, VectorConfidenceParams{MinPivotRatio: 1e-10})
	if !errors.Is(m.factor(g), ErrVectorConfidenceCovariance) {
		t.Fatal("subnormal small pivot admitted")
	}
	for _, cov := range [][]float64{{0}, {1, 1, 1, 1}, {1, 2, 2, 1}, {math.Inf(1)}} {
		g, m := vectorConfidenceTestModel(t, cov, VectorConfidenceParams{})
		if m.factor(g) == nil {
			t.Fatal("bad covariance admitted", cov)
		}
	}
}

func TestVectorConfidenceRidgeAndDistanceExtremes(t *testing.T) {
	g, m := vectorConfidenceTestModel(t, []float64{0, 0, 0, 0}, VectorConfidenceParams{CovariancePolicy: VectorCovarianceRidge, Ridge: 1e-200})
	if err := m.factor(g); err != nil {
		t.Fatal(err)
	}
	d, err := m.distance(g, []float64{1e100, -1e100})
	if err != nil || !vectorConfidenceFinite(d) || math.Abs(d/1e200-math.Sqrt2) > 1e-14 {
		t.Fatal("hypot must avoid squared-norm overflow", d, err)
	}
	if m.applied[0] != 1e-200 || m.applied[3] != 1e-200 || m.cov[0] != 0 {
		t.Fatal("ridge must leave sample covariance unchanged")
	}
	g, m = vectorConfidenceTestModel(t, []float64{math.SmallestNonzeroFloat64}, VectorConfidenceParams{})
	if err := m.factor(g); err != nil {
		t.Fatal(err)
	}
	if _, err := m.distance(g, []float64{math.SmallestNonzeroFloat64}); err != nil {
		t.Fatal("finite subnormal input rejected", err)
	}
}

func TestVectorConfidenceWelfordIndependentCenteredSums(t *testing.T) {
	g, m := vectorConfidenceTestModel(t, make([]float64, 9), VectorConfidenceParams{})
	values := [][]float64{{1, 3, 7}, {2, 0, 5}, {-1, 2, 9}, {4, 1, -1}, {0, 7, 2}}
	mean := make([]float64, 3)
	for _, v := range values {
		if err := m.add(g, v); err != nil {
			t.Fatal(err)
		}
		for i := range mean {
			mean[i] += v[i] / float64(len(values))
		}
	}
	if err := m.covariance(g); err != nil {
		t.Fatal(err)
	}
	for i := range mean {
		if math.Abs(mean[i]-m.mean[i]) > 1e-14 {
			t.Fatal("mean", mean, m.mean)
		}
		for j := range mean {
			sum := 0.0
			for _, v := range values {
				sum += (v[i] - mean[i]) * (v[j] - mean[j])
			}
			expected := sum / float64(len(values)-1)
			if math.Abs(expected-m.cov[i*3+j]) > 1e-14 {
				t.Fatal("covariance", m.cov, i, j, expected)
			}
		}
	}
	g.params.CovarianceNormalization = VectorCovariancePopulation
	if err := m.covariance(g); err != nil {
		t.Fatal(err)
	}
	for i := range mean {
		for j := range mean {
			sum := 0.0
			for _, v := range values {
				sum += (v[i] - mean[i]) * (v[j] - mean[j])
			}
			if math.Abs(sum/float64(len(values))-m.cov[i*3+j]) > 1e-14 {
				t.Fatal("independent population covariance", i, j)
			}
		}
	}
	if err := m.reset(g); err != nil || m.samples != 0 || m.mean[0] != 0 || m.m2[0] != 0 {
		t.Fatal("reset", err)
	}
	if !errors.Is(m.covariance(g), ErrVectorConfidenceCovariance) {
		t.Fatal("empty statistics admitted")
	}
}

func TestVectorConfidenceCounterOverflowRefused(t *testing.T) {
	if _, ok := vectorConfidenceProduct(math.MaxUint64, 2); ok {
		t.Fatal("overflow")
	}
	g := vectorConfidenceGrower{ctx: context.Background(), limits: DefaultVectorConfidenceLimits()}
	g.work = g.limits.MaxWork - 1
	if !errors.Is(g.charge(math.MaxUint64), ErrRegionGrowResourceLimit) || g.work != g.limits.MaxWork-1 {
		t.Fatal("charge overflow")
	}
}

type vectorConfidenceCountingContext struct {
	context.Context
	checks, trigger int
	cancel          context.CancelFunc
	onCheck         func()
}

func (c *vectorConfidenceCountingContext) Err() error {
	c.checks++
	if c.onCheck != nil {
		c.onCheck()
	}
	if c.trigger > 0 && c.checks == c.trigger {
		c.cancel()
	}
	return c.Context.Err()
}

func vectorConfidenceControlledContext(trigger int) *vectorConfidenceCountingContext {
	ctx, cancel := context.WithCancel(context.Background())
	return &vectorConfidenceCountingContext{Context: ctx, trigger: trigger, cancel: cancel}
}

func TestVectorConfidenceSeedSortBoundedAndCancelable(t *testing.T) {
	for _, source := range [][]uint32{nil, {1}, {0, 1, 2, 3, 4}, {4, 3, 2, 1, 0}, {3, 1, 3, 2, 2, 0}, {7, 7, 7, 7}} {
		got := append([]uint32(nil), source...)
		want := append([]uint32(nil), source...)
		sort.Slice(want, func(i, j int) bool { return want[i] < want[j] })
		if err := vectorConfidenceSortSeeds(context.Background(), got); err != nil || !reflect.DeepEqual(got, want) {
			t.Fatal("heap ordering/multiset", source, got, err)
		}
	}
	large := make([]uint32, 65536)
	for i := range large {
		large[i] = uint32((i * 257) % 65536)
	}
	want := append([]uint32(nil), large...)
	sort.Slice(want, func(i, j int) bool { return want[i] < want[j] })
	complete := append([]uint32(nil), large...)
	if err := vectorConfidenceSortSeeds(context.Background(), complete); err != nil || !reflect.DeepEqual(complete, want) {
		t.Fatal("large heap multiset", err)
	}
	ctx := vectorConfidenceControlledContext(3)
	defer ctx.cancel()
	if err := vectorConfidenceSortSeeds(ctx, large); !errors.Is(err, ErrRegionGrowCancelled) || !errors.Is(err, context.Canceled) || ctx.checks != 3 {
		t.Fatal("sort must stop at a bulk poll", err, ctx.checks)
	}
}

func TestVectorConfidencePrepareCancellationBeforeWorkspace(t *testing.T) {
	grid := VoxelGrid{Columns: 100, Rows: 100, Slices: 1, ColumnStep: render.Vec3{X: 1}, RowStep: render.Vec3{Y: 1}, SliceStep: render.Vec3{Z: 1}}
	calls := 0
	v := VectorConfidenceVolume{Grid: grid, FrameOfReferenceID: "frame", Channels: []VectorConfidenceChannel{{ID: "a", Unit: "unit", FrameOfReferenceID: "frame", Grid: grid, Scale: 1, ValueAt: func(int, int, int) (float64, bool) { calls++; return 0, true }}}}
	seeds := make([]RegionGrowSeed, 10000)
	for i := range seeds {
		seeds[i] = RegionGrowSeed{X: i % 100, Y: i / 100}
	}
	before := append([]RegionGrowSeed(nil), seeds...)
	for _, trigger := range []int{3, 14, 24} {
		ctx := vectorConfidenceControlledContext(trigger)
		g := vectorConfidenceGrower{ctx: ctx, volume: v, params: VectorConfidenceParams{Multiplier: 2.5}}
		err := g.prepare(seeds, VectorConfidenceLimits{MaxSeeds: 16384})
		ctx.cancel()
		if !errors.Is(err, ErrRegionGrowCancelled) || g.state != nil || g.queue != nil || g.seedValues != nil || calls != 0 || !reflect.DeepEqual(seeds, before) {
			t.Fatal("prepare cancel before remaining allocations", trigger, err)
		}
	}
}

func TestVectorConfidenceNarrowMaskBulkPolling(t *testing.T) {
	for _, columns := range []int{1, 2} {
		const n = 4096
		ctx := vectorConfidenceControlledContext(0)
		g := vectorConfidenceGrower{ctx: ctx, volume: VectorConfidenceVolume{Grid: VoxelGrid{Columns: columns, Rows: n / columns, Slices: 1}}, limits: DefaultVectorConfidenceLimits(), n: n, state: make([]byte, n)}
		for i := range g.state {
			if i%2 == 0 {
				g.state[i] = 2
				g.count++
			}
		}
		masks, err := g.masks()
		minimum := n/1024 + (n+n/columns+1023)/1024 + 4
		if err != nil || masks[0] == nil || ctx.checks < minimum {
			t.Fatal("sentinel visits must participate in polls", columns, ctx.checks, minimum, err)
		}
		ctx.cancel()
		cancelCtx := vectorConfidenceControlledContext(10)
		g.ctx = cancelCtx
		partial, err := g.masks()
		cancelCtx.cancel()
		if partial != nil || !errors.Is(err, ErrRegionGrowCancelled) || !errors.Is(err, context.Canceled) {
			t.Fatal("mask bulk cancellation publishes no partial output", columns, err)
		}
	}
}

// Adverse schedule controls qualify poll intervals, not merely the total
// number of checks. Production masks uses the visit ordinal below; the previous
// linear-index rule is retained only as a negative control, including duplicate
// checks at a row sentinel and the next row's start. No product hook is added.
func TestVectorConfidencePollingCadenceAdverseControls(t *testing.T) {
	for _, columns := range []int{1, 2} {
		for _, legacy := range []bool{false, true} {
			visit := 0
			positions := []int{}
			ctx := vectorConfidenceControlledContext(0)
			ctx.onCheck = func() { positions = append(positions, visit) }
			for y := 0; y < 4096; y++ {
				for x := 0; x <= columns; x++ {
					ordinal := visit
					if legacy {
						ordinal = y*columns + x
					}
					if err := vectorConfidenceBulkPoll(ctx, ordinal); err != nil {
						t.Fatal(err)
					}
					visit++
				}
			}
			ctx.cancel()
			positions = append(positions, visit)
			gap := 0
			for i := 1; i < len(positions); i++ {
				gap = max(gap, positions[i]-positions[i-1])
			}
			if legacy && gap <= 1024 || !legacy && gap > 1024 {
				t.Fatal("adverse cadence control", columns, legacy, gap)
			}
			t.Logf("columns=%d legacy=%v maximum visit gap=%d", columns, legacy, gap)
		}
	}
}
