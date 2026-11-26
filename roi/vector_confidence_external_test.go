package roi_test

import (
	"context"
	"errors"
	"fmt"
	"math"
	"reflect"
	"sort"
	"sync"
	"testing"
	"time"

	"github.com/ThalesMMS/dicom-go/render"
	"github.com/ThalesMMS/dicom-go/roi"
)

func vectorConfidenceGrid(cols, rows, slices int) roi.VoxelGrid {
	return roi.VoxelGrid{Columns: cols, Rows: rows, Slices: slices, Origin: render.Vec3{X: -11, Y: 7, Z: 3},
		ColumnStep: render.Vec3{X: 0.6}, RowStep: render.Vec3{Y: 1.2}, SliceStep: render.Vec3{Z: 2.5}}
}

func vectorConfidenceVolume(grid roi.VoxelGrid, values ...func(int, int, int) (float64, bool)) roi.VectorConfidenceVolume {
	v := roi.VectorConfidenceVolume{Grid: grid, FrameOfReferenceID: "synthetic-frame"}
	for i, f := range values {
		v.Channels = append(v.Channels, roi.VectorConfidenceChannel{ID: fmt.Sprintf("channel-%d", i), Unit: "synthetic-unit", FrameOfReferenceID: v.FrameOfReferenceID, Grid: grid, Scale: 1, ValueAt: f})
	}
	return v
}

func vectorConfidencePhantom(cols, rows, slices, split int) roi.VectorConfidenceVolume {
	g := vectorConfidenceGrid(cols, rows, slices)
	return vectorConfidenceVolume(g, func(x, y, z int) (float64, bool) { return float64((x+y)%3 - 1), true },
		func(x, y, z int) (float64, bool) {
			v := float64((x+2*y+z)%3 - 1)
			if x > split {
				v += 8
			}
			return v, true
		})
}

func vectorConfidenceIndices(r *roi.VectorConfidenceResult) []int {
	out := []int{}
	for z := 0; z < r.Grid.Slices; z++ {
		mask := r.Masks[z]
		for y := 0; y < r.Grid.Rows; y++ {
			for x := 0; x < r.Grid.Columns; x++ {
				if mask != nil && mask.Get(x, y) {
					out = append(out, (z*r.Grid.Rows+y)*r.Grid.Columns+x)
				}
			}
		}
	}
	return out
}

func TestVectorConfidencePhantomsJointDiscrimination(t *testing.T) {
	for _, size := range [][4]int{{9, 7, 1, 4}, {7, 7, 5, 3}} {
		for _, iterations := range []int{0, 2} {
			v := vectorConfidencePhantom(size[0], size[1], size[2], size[3])
			seed := roi.RegionGrowSeed{X: 2, Y: 3, Slice: size[2] / 2}
			params := roi.VectorConfidenceParams{Multiplier: 2.5, InitialRadius: 1, Iterations: iterations}
			result, err := roi.GrowVectorConfidenceConnected(nil, v, []roi.RegionGrowSeed{seed}, params, roi.VectorConfidenceOptions{})
			if err != nil {
				t.Fatal(size, iterations, err)
			}
			expected := (size[3] + 1) * size[1] * size[2]
			if result.VoxelCount != expected || result.Fills != iterations+1 || result.Iterations != iterations {
				t.Fatal("wrong joint region", size, result.VoxelCount, expected)
			}
			for z := 0; z < size[2]; z++ {
				for y := 0; y < size[1]; y++ {
					for x := 0; x < size[0]; x++ {
						if result.Masks[z].Get(x, y) != (x <= size[3]) {
							t.Fatal("wrong class", x, y, z)
						}
					}
				}
			}
			if iterations == 0 {
				variance := 0.75
				samples := 9
				if size[2] > 1 {
					variance, samples = 18.0/26, 27
				}
				if result.InitialSamples != samples || math.Abs(result.Mean[0])+math.Abs(result.Mean[1]) > 1e-14 || math.Abs(result.Covariance[0]-variance) > 1e-14 || math.Abs(result.Covariance[3]-variance) > 1e-14 || math.Abs(result.Covariance[1]) > 1e-14 {
					t.Fatal("analytic initial statistics", result.Mean, result.Covariance)
				}
			}
			v.Channels = v.Channels[:1]
			single, err := roi.GrowVectorConfidenceConnected(nil, v, []roi.RegionGrowSeed{seed}, params, roi.VectorConfidenceOptions{})
			if err != nil || single.VoxelCount != size[0]*size[1]*size[2] {
				t.Fatal("one channel should not distinguish classes", err)
			}
		}
	}
}

func TestVectorConfidenceUnitsPermutationAndOwnedOutput(t *testing.T) {
	v := vectorConfidencePhantom(9, 7, 1, 4)
	p := roi.VectorConfidenceParams{Multiplier: 2.5, InitialRadius: 1, Iterations: 2}
	seeds := []roi.RegionGrowSeed{{X: 2, Y: 3}, {X: 3, Y: 2}}
	baseline, err := roi.GrowVectorConfidenceConnected(nil, v, seeds, p, roi.VectorConfidenceOptions{})
	if err != nil {
		t.Fatal(err)
	}
	original := v.Channels[1].ValueAt
	v.Channels[1].ValueAt = func(x, y, z int) (float64, bool) { f, ok := original(x, y, z); return 1000*f + 273, ok }
	v.Channels[1].Offset, v.Channels[1].Scale, v.Channels[1].Unit = 273, 1000, "scaled-unit"
	v.Channels[0], v.Channels[1] = v.Channels[1], v.Channels[0]
	scaled, err := roi.GrowVectorConfidenceConnected(nil, v, []roi.RegionGrowSeed{seeds[1], seeds[0], seeds[1]}, p, roi.VectorConfidenceOptions{})
	if err != nil || !reflect.DeepEqual(vectorConfidenceIndices(baseline), vectorConfidenceIndices(scaled)) || scaled.DuplicateSeeds != 1 || scaled.UniqueSeeds != 2 {
		t.Fatal("units/permutation/duplicate seeds", err)
	}
	if math.Abs(baseline.Mean[0]-scaled.Mean[1]) > 1e-14 || math.Abs(baseline.Covariance[0]-scaled.Covariance[3]) > 1e-14 {
		t.Fatal("permuted statistics")
	}
	scaled.Mean[0] = 999
	scaled.Covariance[0] = 999
	scaled.Masks[0].Set(0, 0, false)
	v.Channels[0].ID = "changed"
	if baseline.Mean[0] == 999 || !baseline.Masks[0].Get(0, 0) || scaled.Channels[0].ID == "changed" || scaled.AppliedCovariance[0] == 999 {
		t.Fatal("output ownership")
	}
}

func TestVectorConfidenceSingularPolicy(t *testing.T) {
	grid := vectorConfidenceGrid(5, 5, 1)
	for _, kind := range []string{"constant", "duplicate"} {
		f := func(x, y, z int) (float64, bool) { return 4, true }
		if kind == "duplicate" {
			f = func(x, y, z int) (float64, bool) { return float64((x+y)%3 - 1), true }
		}
		v := vectorConfidenceVolume(grid, f, f)
		p := roi.VectorConfidenceParams{Multiplier: 2.5, InitialRadius: 1, Iterations: 2}
		seeds := []roi.RegionGrowSeed{{X: 2, Y: 2}}
		r, err := roi.GrowVectorConfidenceConnected(nil, v, seeds, p, roi.VectorConfidenceOptions{})
		if r != nil || !errors.Is(err, roi.ErrVectorConfidenceCovariance) {
			t.Fatal("strict singular", kind, err)
		}
		p.CovariancePolicy, p.Ridge = roi.VectorCovarianceRidge, 0.1
		r, err = roi.GrowVectorConfidenceConnected(nil, v, seeds, p, roi.VectorConfidenceOptions{})
		if err != nil || r.VoxelCount != 25 || math.Abs(r.AppliedCovariance[0]-r.Covariance[0]-0.1) > 1e-14 || math.Abs(r.AppliedCovariance[3]-r.Covariance[3]-0.1) > 1e-14 {
			t.Fatal("ridge", kind, err)
		}
		second, err := roi.GrowVectorConfidenceConnected(nil, v, seeds, p, roi.VectorConfidenceOptions{})
		if err != nil || !reflect.DeepEqual(r, second) {
			t.Fatal("ridge nondeterministic", err)
		}
	}
}

func TestVectorConfidenceRejectedInputs(t *testing.T) {
	good := func() roi.VectorConfidenceVolume { return vectorConfidencePhantom(9, 7, 1, 4) }
	p := roi.VectorConfidenceParams{Multiplier: 2.5, InitialRadius: 1}
	for _, row := range []struct {
		name string
		edit func(*roi.VectorConfidenceVolume)
		want error
	}{
		{"missing", func(v *roi.VectorConfidenceVolume) { v.Channels[1].ValueAt = nil }, roi.ErrRegionGrowInvalidVolume},
		{"unavailable-neighborhood", func(v *roi.VectorConfidenceVolume) {
			v.Channels[1].ValueAt = func(x, y, z int) (float64, bool) { return 0, !(x == 1 && y == 2) }
		}, roi.ErrVectorConfidenceUnavailable},
		{"NaN", func(v *roi.VectorConfidenceVolume) {
			v.Channels[0].ValueAt = func(int, int, int) (float64, bool) { return math.NaN(), true }
		}, roi.ErrVectorConfidenceNumeric},
		{"positive-inf", func(v *roi.VectorConfidenceVolume) {
			v.Channels[0].ValueAt = func(int, int, int) (float64, bool) { return math.Inf(1), true }
		}, roi.ErrVectorConfidenceNumeric},
		{"negative-inf", func(v *roi.VectorConfidenceVolume) {
			v.Channels[0].ValueAt = func(int, int, int) (float64, bool) { return math.Inf(-1), true }
		}, roi.ErrVectorConfidenceNumeric},
		{"normalized-range", func(v *roi.VectorConfidenceVolume) { v.Channels[0].Scale = 1e-200 }, roi.ErrVectorConfidenceNumeric},
		{"subtraction-overflow", func(v *roi.VectorConfidenceVolume) {
			v.Channels[0].Offset = -math.MaxFloat64
			v.Channels[0].ValueAt = func(int, int, int) (float64, bool) { return math.MaxFloat64, true }
		}, roi.ErrVectorConfidenceNumeric},
		{"scale-zero", func(v *roi.VectorConfidenceVolume) { v.Channels[1].Scale = 0 }, roi.ErrRegionGrowInvalidParameters},
		{"unit-empty", func(v *roi.VectorConfidenceVolume) { v.Channels[1].Unit = " " }, roi.ErrRegionGrowInvalidVolume},
		{"duplicate-ID", func(v *roi.VectorConfidenceVolume) { v.Channels[1].ID = v.Channels[0].ID }, roi.ErrRegionGrowInvalidVolume},
		{"frame", func(v *roi.VectorConfidenceVolume) { v.Channels[1].FrameOfReferenceID = "different" }, roi.ErrVectorConfidenceGeometry},
		{"unaligned", func(v *roi.VectorConfidenceVolume) { v.Channels[1].Grid.Origin.X += 1e-12 }, roi.ErrVectorConfidenceGeometry},
		{"degenerate-grid", func(v *roi.VectorConfidenceVolume) { v.Grid.SliceStep = render.Vec3{} }, roi.ErrVectorConfidenceGeometry},
		{"grid-transform-overflow", func(v *roi.VectorConfidenceVolume) { v.Grid.ColumnStep.X = math.MaxFloat64 }, roi.ErrVectorConfidenceGeometry},
		{"grid-multiplication-overflow", func(v *roi.VectorConfidenceVolume) {
			v.Grid.Columns = int(^uint(0) >> 1)
			v.Grid.Rows = int(^uint(0) >> 1)
		}, roi.ErrRegionGrowResourceLimit},
	} {
		t.Run(row.name, func(t *testing.T) {
			v := good()
			row.edit(&v)
			r, err := roi.GrowVectorConfidenceConnected(nil, v, []roi.RegionGrowSeed{{X: 2, Y: 3}}, p, roi.VectorConfidenceOptions{})
			if r != nil || !errors.Is(err, row.want) {
				t.Fatal("expected nil and", row.want, "got", err)
			}
		})
	}
	for _, seeds := range [][]roi.RegionGrowSeed{nil, {{X: -1}}, {{X: 9}}, {{X: 2, Y: 3}}} {
		params := p
		params.InitialRadius = 0
		r, err := roi.GrowVectorConfidenceConnected(nil, good(), seeds, params, roi.VectorConfidenceOptions{})
		if r != nil || err == nil {
			t.Fatal("empty/invalid/insufficient seed neighborhood accepted")
		}
	}
}

func TestVectorConfidenceResourceBoundaries(t *testing.T) {
	v := vectorConfidencePhantom(9, 7, 1, 4)
	p := roi.VectorConfidenceParams{Multiplier: 2.5, InitialRadius: 1, Iterations: 1}
	seeds := []roi.RegionGrowSeed{{X: 2, Y: 3}}
	baseline, err := roi.GrowVectorConfidenceConnected(nil, v, seeds, p, roi.VectorConfidenceOptions{})
	if err != nil {
		t.Fatal(err)
	}
	for _, row := range []struct {
		name string
		l    roi.VectorConfidenceLimits
	}{
		{"channels", roi.VectorConfidenceLimits{MaxChannels: 1}}, {"grid", roi.VectorConfidenceLimits{MaxGridVoxels: 62}},
		{"region", roi.VectorConfidenceLimits{MaxRegionVoxels: 34}}, {"neighborhood", roi.VectorConfidenceLimits{MaxNeighborhoodSamples: 8}},
		{"fills", roi.VectorConfidenceLimits{MaxFills: 1}}, {"work", roi.VectorConfidenceLimits{MaxWork: baseline.Work - 1}},
		{"bytes", roi.VectorConfidenceLimits{MaxEstimatedBytes: baseline.EstimatedBytes - 1}},
	} {
		t.Run(row.name, func(t *testing.T) {
			r, err := roi.GrowVectorConfidenceConnected(nil, v, seeds, p, roi.VectorConfidenceOptions{Limits: row.l})
			if r != nil || !errors.Is(err, roi.ErrRegionGrowResourceLimit) {
				t.Fatal("resource failure", err)
			}
		})
	}
	r, err := roi.GrowVectorConfidenceConnected(nil, v, seeds, p, roi.VectorConfidenceOptions{Limits: roi.VectorConfidenceLimits{MaxWork: baseline.Work, MaxEstimatedBytes: baseline.EstimatedBytes}})
	if err != nil || r.Work != baseline.Work || r.EstimatedBytes != baseline.EstimatedBytes {
		t.Fatal("exact limit should pass", err)
	}
	for _, l := range []roi.VectorConfidenceLimits{{MaxChannels: 33}, {MaxRegionVoxels: -1}, {MaxWork: 1<<34 + 1}, {MaxEstimatedBytes: 1<<30 + 1}} {
		r, err := roi.GrowVectorConfidenceConnected(nil, v, seeds, p, roi.VectorConfidenceOptions{Limits: l})
		if r != nil || !errors.Is(err, roi.ErrRegionGrowInvalidParameters) {
			t.Fatal("hard profile invalid", err)
		}
	}
	for _, l := range []roi.VectorConfidenceLimits{{MaxGridVoxels: 1}, {MaxEstimatedBytes: 1}, {MaxWork: 1}} {
		calls := 0
		copyVolume := vectorConfidencePhantom(9, 7, 1, 4)
		copyVolume.Channels[0].ValueAt = func(int, int, int) (float64, bool) { calls++; return 0, true }
		_, err := roi.GrowVectorConfidenceConnected(nil, copyVolume, seeds, p, roi.VectorConfidenceOptions{Limits: l})
		if err == nil || calls != 0 {
			t.Fatal("pre-admission must precede callbacks", err, calls)
		}
	}
}

func TestVectorConfidenceCancellationPhases(t *testing.T) {
	base := vectorConfidencePhantom(9, 7, 1, 4)
	p := roi.VectorConfidenceParams{Multiplier: 2.5, InitialRadius: 1, Iterations: 2}
	seed := []roi.RegionGrowSeed{{X: 2, Y: 3}}
	complete, err := roi.GrowVectorConfidenceConnected(nil, base, seed, p, roi.VectorConfidenceOptions{})
	if err != nil {
		t.Fatal(err)
	}
	for _, cancelAt := range []int{0, 1, 4, 20, 30, 120, int(complete.CallbackReads / 2)} {
		ctx, cancel := context.WithCancel(context.Background())
		v := vectorConfidencePhantom(9, 7, 1, 4)
		count := 0
		original := v.Channels[0].ValueAt
		v.Channels[0].ValueAt = func(x, y, z int) (float64, bool) {
			count++
			if count == cancelAt {
				cancel()
			}
			return original(x, y, z)
		}
		if cancelAt == 0 {
			cancel()
		}
		r, err := roi.GrowVectorConfidenceConnected(ctx, v, seed, p, roi.VectorConfidenceOptions{})
		cancel()
		if r != nil || !errors.Is(err, roi.ErrRegionGrowCancelled) || !errors.Is(err, context.Canceled) {
			t.Fatal("cancel did not refuse partial output", cancelAt, err)
		}
	}
	ctx, cancel := context.WithDeadline(context.Background(), time.Unix(0, 0))
	defer cancel()
	r, err := roi.GrowVectorConfidenceConnected(ctx, base, seed, p, roi.VectorConfidenceOptions{})
	if r != nil || !errors.Is(err, context.DeadlineExceeded) {
		t.Fatal("deadline identity", err)
	}
}

func TestVectorConfidenceSeedExpansionConnectivityAndSparseReads(t *testing.T) {
	g := vectorConfidenceGrid(64, 64, 1)
	v := vectorConfidenceVolume(g, func(x, y, z int) (float64, bool) {
		if x == y && x < 4 {
			return float64(x), true
		}
		return 100, true
	})
	p := roi.VectorConfidenceParams{Multiplier: 0.1, CovariancePolicy: roi.VectorCovarianceRidge, Ridge: 0.1}
	seeds := []roi.RegionGrowSeed{{X: 0, Y: 0}, {X: 1, Y: 1}}
	for _, conn := range []roi.Connectivity3D{roi.Connectivity6, roi.Connectivity18, roi.Connectivity26} {
		r, err := roi.GrowVectorConfidenceConnected(nil, v, seeds, p, roi.VectorConfidenceOptions{Connectivity: conn})
		if err != nil || r.EffectiveMultiplier <= p.Multiplier || r.VoxelCount != 2 || r.CallbackReads >= 64 {
			t.Fatal("seed expansion or sparse callbacks", conn, err)
		}
	}
	// Constant connected diagonal points: a box supplies at least two samples
	// and ridge makes the model usable while distant background is excluded.
	v = vectorConfidenceVolume(vectorConfidenceGrid(5, 5, 1), func(x, y, z int) (float64, bool) {
		if x == y {
			return 0, true
		}
		return 100, true
	})
	p = roi.VectorConfidenceParams{Multiplier: 1, CovariancePolicy: roi.VectorCovarianceRidge, Ridge: 1}
	seeds = []roi.RegionGrowSeed{{X: 0, Y: 0}, {X: 1, Y: 1}}
	for _, conn := range []roi.Connectivity3D{roi.Connectivity6, roi.Connectivity18, roi.Connectivity26} {
		r, err := roi.GrowVectorConfidenceConnected(nil, v, seeds, p, roi.VectorConfidenceOptions{Connectivity: conn})
		want := 5
		if conn == roi.Connectivity6 {
			want = 2
		}
		if err != nil || r.VoxelCount != want {
			t.Fatal("diagonal connectivity", conn, err)
		}
	}
}

func TestVectorConfidenceBoundaryRepeatsAndConcurrentCalls(t *testing.T) {
	g := vectorConfidenceGrid(3, 3, 1)
	v := vectorConfidenceVolume(g, func(x, y, z int) (float64, bool) { return float64(x + 2*y), true })
	p := roi.VectorConfidenceParams{Multiplier: 2.5, InitialRadius: 1}
	seeds := []roi.RegionGrowSeed{{}}
	r, err := roi.GrowVectorConfidenceConnected(nil, v, seeds, p, roi.VectorConfidenceOptions{})
	if err != nil || r.InitialSamples != 9 || math.Abs(r.Mean[0]-1) > 1e-14 || math.Abs(r.Covariance[0]-1.25) > 1e-14 {
		t.Fatal("Neumann border repetitions", err)
	}
	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			next, err := roi.GrowVectorConfidenceConnected(nil, v, seeds, p, roi.VectorConfidenceOptions{})
			if err != nil || !reflect.DeepEqual(r, next) {
				t.Error("concurrent immutable calls", err)
			}
		}()
	}
	wg.Wait()
}

// Independent dense fixed-point connectivity and direct 1D/2x2 inverse. This
// deliberately does not use production Welford, Cholesky or FIFO traversal.
func vectorConfidenceDenseReference(v roi.VectorConfidenceVolume, seeds []roi.RegionGrowSeed, p roi.VectorConfidenceParams, conn roi.Connectivity3D) []int {
	g, c := v.Grid, len(v.Channels)
	n := g.Columns * g.Rows * g.Slices
	values := make([][]float64, n)
	coords := func(i int) (int, int, int) { return i % g.Columns, (i / g.Columns) % g.Rows, i / (g.Columns * g.Rows) }
	for i := range values {
		x, y, z := coords(i)
		values[i] = make([]float64, c)
		for j, ch := range v.Channels {
			f, _ := ch.ValueAt(x, y, z)
			values[i][j] = (f - ch.Offset) / ch.Scale
		}
	}
	ids := []int{}
	for _, s := range seeds {
		id := (s.Slice*g.Rows+s.Y)*g.Columns + s.X
		found := false
		for _, old := range ids {
			if old == id {
				found = true
			}
		}
		if !found {
			ids = append(ids, id)
		}
	}
	sort.Ints(ids)
	samples := [][]float64{}
	r, rz := p.InitialRadius, p.InitialRadius
	if g.Slices == 1 {
		rz = 0
	}
	for _, id := range ids {
		x, y, z := coords(id)
		for zz := z - rz; zz <= z+rz; zz++ {
			for yy := y - r; yy <= y+r; yy++ {
				for xx := x - r; xx <= x+r; xx++ {
					a, b, d := min(max(xx, 0), g.Columns-1), min(max(yy, 0), g.Rows-1), min(max(zz, 0), g.Slices-1)
					samples = append(samples, values[(d*g.Rows+b)*g.Columns+a])
				}
			}
		}
	}
	var selected []bool
	for fill := 0; fill <= p.Iterations; fill++ {
		if fill > 0 {
			samples = nil
			for i, on := range selected {
				if on {
					samples = append(samples, values[i])
				}
			}
		}
		mean := make([]float64, c)
		for _, sample := range samples {
			for j := range mean {
				mean[j] += sample[j]
			}
		}
		for j := range mean {
			mean[j] /= float64(len(samples))
		}
		cov := make([]float64, c*c)
		for _, sample := range samples {
			for i := 0; i < c; i++ {
				for j := 0; j < c; j++ {
					cov[i*c+j] += (sample[i] - mean[i]) * (sample[j] - mean[j])
				}
			}
		}
		denominator := len(samples) - 1
		if p.CovarianceNormalization == roi.VectorCovariancePopulation {
			denominator = len(samples)
		}
		for j := range cov {
			cov[j] /= float64(denominator)
		}
		q := func(value []float64) float64 {
			a := value[0] - mean[0]
			if c == 1 {
				return a * a / cov[0]
			}
			b := value[1] - mean[1]
			return (cov[3]*a*a - 2*cov[1]*a*b + cov[0]*b*b) / (cov[0]*cov[3] - cov[1]*cov[2])
		}
		limit := p.Multiplier * p.Multiplier
		for _, id := range ids {
			limit = math.Max(limit, q(values[id]))
		}
		selected = make([]bool, n)
		for _, id := range ids {
			selected[id] = true
		}
		for changed := true; changed; {
			changed = false
			for i := 0; i < n; i++ {
				if selected[i] || q(values[i]) > limit {
					continue
				}
				x, y, z := coords(i)
				connected := false
				for j, on := range selected {
					if !on {
						continue
					}
					xx, yy, zz := coords(j)
					a, b, d := int(math.Abs(float64(xx-x))), int(math.Abs(float64(yy-y))), int(math.Abs(float64(zz-z)))
					sum := a + b + d
					if a <= 1 && b <= 1 && d <= 1 && sum > 0 && (conn == roi.Connectivity26 || conn == roi.Connectivity18 && sum <= 2 || conn == roi.Connectivity6 && sum == 1) {
						connected = true
						break
					}
				}
				if connected {
					selected[i] = true
					changed = true
				}
			}
		}
	}
	out := []int{}
	for i, on := range selected {
		if on {
			out = append(out, i)
		}
	}
	return out
}

func TestVectorConfidenceIndependentDenseReference(t *testing.T) {
	for _, depth := range []int{1, 3} {
		for _, conn := range []roi.Connectivity3D{roi.Connectivity6, roi.Connectivity18, roi.Connectivity26} {
			for _, iterations := range []int{0, 1, 2} {
				grid := vectorConfidenceGrid(8, 6, depth)
				v := vectorConfidenceVolume(grid, func(x, y, z int) (float64, bool) { return float64((x+2*y+z)%5-2) * 0.7, true }, func(x, y, z int) (float64, bool) {
					a := float64((2*x+y+3*z)%7-3) * 0.4
					if x >= 5 {
						a += 7
					}
					return a, true
				})
				seeds := []roi.RegionGrowSeed{{X: 2, Y: 2, Slice: depth / 2}, {X: 3, Y: 3, Slice: depth / 2}}
				p := roi.VectorConfidenceParams{Multiplier: 2.35, InitialRadius: 1, Iterations: iterations}
				r, err := roi.GrowVectorConfidenceConnected(nil, v, seeds, p, roi.VectorConfidenceOptions{Connectivity: conn})
				if err != nil {
					t.Fatal(depth, conn, iterations, err)
				}
				want := vectorConfidenceDenseReference(v, seeds, p, conn)
				if !reflect.DeepEqual(vectorConfidenceIndices(r), want) {
					t.Fatal("independent dense mask mismatch", depth, conn, iterations)
				}
			}
		}
	}
}

func TestVectorConfidenceInvalidParameterProfiles(t *testing.T) {
	v := vectorConfidencePhantom(9, 7, 1, 4)
	base := roi.VectorConfidenceParams{Multiplier: 2.5, InitialRadius: 1}
	for _, edit := range []func(*roi.VectorConfidenceParams){func(p *roi.VectorConfidenceParams) { p.Multiplier = 0 }, func(p *roi.VectorConfidenceParams) { p.Multiplier = math.NaN() }, func(p *roi.VectorConfidenceParams) { p.Multiplier = 1e6 + 1 }, func(p *roi.VectorConfidenceParams) { p.Iterations = 65 }, func(p *roi.VectorConfidenceParams) { p.InitialRadius = 17 }, func(p *roi.VectorConfidenceParams) { p.Ridge = 1 }, func(p *roi.VectorConfidenceParams) { p.CovariancePolicy = roi.VectorCovarianceRidge }, func(p *roi.VectorConfidenceParams) { p.CovariancePolicy = 9 }, func(p *roi.VectorConfidenceParams) { p.MinPivotRatio = 1e-20 }} {
		p := base
		edit(&p)
		r, err := roi.GrowVectorConfidenceConnected(nil, v, []roi.RegionGrowSeed{{X: 2, Y: 3}}, p, roi.VectorConfidenceOptions{})
		if r != nil || !errors.Is(err, roi.ErrRegionGrowInvalidParameters) {
			t.Fatal("invalid parameter accepted", err)
		}
	}
	if d := roi.DefaultVectorConfidenceParams(); d.Multiplier != 2.5 || d.InitialRadius != 1 || d.Iterations != 4 {
		t.Fatal("defaults")
	}
}

func TestVectorConfidenceExplicitNormalization(t *testing.T) {
	for _, depth := range []int{1, 5} {
		v := vectorConfidencePhantom(9, 7, depth, 4)
		seeds := []roi.RegionGrowSeed{{X: 2, Y: 3, Slice: depth / 2}}
		for _, iterations := range []int{0, 2} {
			p := roi.VectorConfidenceParams{Multiplier: 2.5, InitialRadius: 1, Iterations: iterations}
			sample, err := roi.GrowVectorConfidenceConnected(nil, v, seeds, p, roi.VectorConfidenceOptions{})
			if err != nil {
				t.Fatal(err)
			}
			p.CovarianceNormalization = roi.VectorCovariancePopulation
			population, err := roi.GrowVectorConfidenceConnected(nil, v, seeds, p, roi.VectorConfidenceOptions{})
			if err != nil || sample.CovarianceNormalization != roi.VectorCovarianceSample || population.CovarianceNormalization != roi.VectorCovariancePopulation {
				t.Fatal("normalization metadata", err)
			}
			if !reflect.DeepEqual(vectorConfidenceIndices(population), vectorConfidenceDenseReference(v, seeds, p, roi.Connectivity6)) {
				t.Fatal("independent population segmentation")
			}
			for i := range sample.Covariance {
				expected := sample.Covariance[i] * float64(sample.ModelSamples-1) / float64(sample.ModelSamples)
				if math.Abs(population.Covariance[i]-expected) > 1e-14 {
					t.Fatal("population denominator", population.Covariance, expected)
				}
			}
			if iterations == 0 && math.Abs(population.Covariance[0]-2.0/3) > 1e-14 {
				t.Fatal("analytic population covariance")
			}
		}
	}
	calls := 0
	v := vectorConfidencePhantom(9, 7, 1, 4)
	v.Channels[0].ValueAt = func(int, int, int) (float64, bool) { calls++; return 0, true }
	p := roi.DefaultVectorConfidenceParams()
	p.CovarianceNormalization = 9
	r, err := roi.GrowVectorConfidenceConnected(nil, v, []roi.RegionGrowSeed{{X: 2, Y: 3}}, p, roi.VectorConfidenceOptions{})
	if r != nil || !errors.Is(err, roi.ErrRegionGrowInvalidParameters) || calls != 0 {
		t.Fatal("unknown normalization must refuse before callbacks", err)
	}
}
