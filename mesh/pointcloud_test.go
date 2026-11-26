package mesh

import (
	"context"
	"errors"
	"math"
	"reflect"
	"runtime"
	"sort"
	"strings"
	"testing"
	"time"
)

func cloudProcess(t *testing.T, c *PointCloud, o PointCloudOptions) *PointCloudResult {
	t.Helper()
	r, err := ProcessPointCloud(context.Background(), c, o)
	if err != nil || !r.Report.Complete || r.Cloud == nil {
		t.Fatalf("process cloud: %+v, %v", r.Report, err)
	}
	return r
}

func TestPointCloudStatisticalBruteForceAndRareProtection(t *testing.T) {
	c := &PointCloud{Units: "mm"}
	// Unequal density, an isolated thin line, duplicate and remote outlier.
	for y := 0; y < 9; y++ {
		for x := 0; x < 9; x++ {
			c.Points = append(c.Points, [3]float32{float32(x) * 0.1, float32(y) * 0.1, 0})
		}
	}
	for x := 0; x < 8; x++ {
		c.Points = append(c.Points, [3]float32{3 + float32(x)*0.5, 0, 0})
	}
	for x := 0; x < 4; x++ {
		c.Points = append(c.Points, [3]float32{float32(x), 4, 0})
	}
	c.Points = append(c.Points, c.Points[0], [3]float32{100, 100, 100})
	c.Protected = make([]bool, len(c.Points))
	for i := 81; i < 93; i++ {
		c.Protected[i] = true
	}
	o := PointCloudOptions{Neighbors: 4, SigmaFactor: 0.25}
	r := cloudProcess(t, c, o)
	for i, p := range c.Points {
		var d []float64
		for j, q := range c.Points {
			if i != j {
				d = append(d, math.Sqrt(pointCloudDistance2(pointCloud64(p), q)))
			}
		}
		sort.Float64s(d)
		want := (d[0] + d[1] + d[2] + d[3]) / 4
		if math.Abs(r.LocalMeanMM[i]-want) > 1e-12 {
			t.Fatalf("point %d nearest mean %.16g, brute force %.16g", i, r.LocalMeanMM[i], want)
		}
	}
	if r.Report.OutputPoints != len(c.Points) || r.Report.Candidates == 0 {
		t.Fatal("analysis applied candidate removal automatically")
	}
	o.RemoveOutliers = true
	r = cloudProcess(t, c, o)
	if r.Removed[len(c.Points)-1] != PointStatisticalOutlier {
		t.Fatal("remote outlier survived explicit cleanup")
	}
	for i := range c.Points {
		if c.Protected[i] && r.Removed[i] != PointKept {
			t.Fatalf("rare protected structure lost at %d", i)
		}
	}
	t.Logf("unequal-density phantom: %d points, %d candidates, %d actual removals, %d protected retained", len(c.Points), r.Report.Candidates, r.Report.OutliersRemoved, r.Report.Protected)
}

func TestPointCloudDensityBiasIsAProposal(t *testing.T) {
	c := &PointCloud{Units: "mm"}
	for i := 0; i < 100; i++ {
		c.Points = append(c.Points, [3]float32{float32(i) * 0.01, 0, 0})
	}
	for i := 0; i < 5; i++ {
		c.Points = append(c.Points, [3]float32{10 + float32(i), 0, 0})
	}
	r := cloudProcess(t, c, PointCloudOptions{Neighbors: 4, SigmaFactor: 1})
	losses := 0
	for i := 100; i < 105; i++ {
		if r.OutlierCandidate[i] {
			losses++
		}
	}
	if losses != 5 || r.Report.OutputPoints != 105 {
		t.Fatalf("density limitation/proposal changed: %d candidates, %d retained", losses, r.Report.OutputPoints)
	}
	t.Logf("sparse but valid thin line: %d/5 false statistical candidates, 0 applied removals", losses)
	c.Protected = make([]bool, len(c.Points))
	for i := 100; i < 105; i++ {
		c.Protected[i] = true
	}
	protected := cloudProcess(t, c, PointCloudOptions{Neighbors: 4, SigmaFactor: 1, RemoveOutliers: true})
	if protected.Report.OutputPoints != 105 || protected.Report.Candidates != 5 || protected.Report.OutliersRemoved != 0 {
		t.Fatal("explicit statistical removal lost protected sparse structure")
	}
}

func TestPointCloudReductionAttributeMapsAndReproducibility(t *testing.T) {
	c := &PointCloud{Units: "mm", Scalars: []VertexScalarAttribute{{Name: "Density", Units: "g/mm^3"}, {Name: "Label"}}}
	for i := 0; i < 200; i++ {
		c.Points = append(c.Points, [3]float32{float32(i%20) * 0.2, float32(i/20) * 0.2, 0})
		c.Normals = append(c.Normals, [3]float32{0, 0, 1})
		c.Colors = append(c.Colors, [4]uint8{uint8(i), uint8(i / 2), 9, 255})
		c.Scalars[0].Values = append(c.Scalars[0].Values, float64(i)+0.25)
		c.Scalars[1].Values = append(c.Scalars[1].Values, float64(i%3))
		c.Protected = append(c.Protected, i == 0)
	}
	before := cloudProcess(t, c, PointCloudOptions{}).Cloud
	o := PointCloudOptions{MinDistanceMM: 0.5, DistanceToleranceMM: 1e-9, Seed: 73}
	r := cloudProcess(t, c, o)
	r2 := cloudProcess(t, c, o)
	if !reflect.DeepEqual(r, r2) || !reflect.DeepEqual(c, before) || r.Report.OutputPoints >= 200 {
		t.Fatal("not repeatable/reduced or input mutated")
	}
	if r.InputToOutput[0] < 0 {
		t.Fatal("protected point suppressed")
	}
	for oi, ii := range r.OriginalIndices {
		if r.InputToOutput[ii] != int64(oi) || r.Removed[ii] != PointKept ||
			r.Cloud.Points[oi] != c.Points[ii] || r.Cloud.Normals[oi] != c.Normals[ii] ||
			r.Cloud.Colors[oi] != c.Colors[ii] || r.Cloud.Protected[oi] != c.Protected[ii] {
			t.Fatal("point/vector/color/mask mapping mismatch")
		}
		for j, a := range r.Cloud.Scalars {
			if a.Name != c.Scalars[j].Name || a.Units != c.Scalars[j].Units || a.Values[oi] != c.Scalars[j].Values[ii] {
				t.Fatal("scalar mapping mismatch")
			}
		}
		for _, q := range r.Cloud.Points[:oi] {
			if math.Sqrt(pointCloudDistance2(pointCloud64(q), r.Cloud.Points[oi])) < o.MinDistanceMM-o.DistanceToleranceMM {
				t.Fatal("minimum distance violated")
			}
		}
	}
	for i, oi := range r.InputToOutput {
		if (oi < 0) != (r.Removed[i] != PointKept) {
			t.Fatal("removed map mismatch")
		}
	}
	o.Seed++
	if reflect.DeepEqual(r.OriginalIndices, cloudProcess(t, c, o).OriginalIndices) {
		t.Fatal("seed failed to alter the ambiguous greedy subset")
	}
	r.Cloud.Points[0][0] = 99
	r.Cloud.Scalars[0].Values[0] = -99
	r.Cloud.Colors[0][0] = 99
	r.Cloud.Protected[0] = false
	if !reflect.DeepEqual(c, before) {
		t.Fatal("output aliases source")
	}
}

func TestPointCloudDuplicatesToleranceAndProtectedConflict(t *testing.T) {
	c := &PointCloud{Units: "mm", Points: [][3]float32{{0, 0, 0}, {0, 0, 0}, {1, 0, 0}, {2, 0, 0}}}
	for _, radius := range []float64{1, 1e-300, math.MaxFloat64} {
		r := cloudProcess(t, c, PointCloudOptions{MinDistanceMM: radius})
		if r.InputToOutput[0] >= 0 && r.InputToOutput[1] >= 0 {
			t.Fatal("duplicate survived positive distance, including underflow radius")
		}
	}
	c.Protected = []bool{true, false, true, true}
	r := cloudProcess(t, c, PointCloudOptions{MinDistanceMM: 1.01, DistanceToleranceMM: 0.02})
	if r.Report.OutputPoints != 3 {
		t.Fatal("declared tolerance not respected")
	}
	bad, err := ProcessPointCloud(context.Background(), c, PointCloudOptions{MinDistanceMM: 1.01})
	if !errors.Is(err, ErrInvalidField) || bad.Cloud != nil || bad.Report.Complete {
		t.Fatal("protected conflict returned a complete cloud")
	}
}

func TestPointCloudAdmissionAndBudgets(t *testing.T) {
	c := &PointCloud{Units: "mm", Points: [][3]float32{{0, 0, 0}, {1, 0, 0}, {2, 0, 0}}}
	for _, o := range []PointCloudOptions{{MaxPoints: 2}, {MaxBytes: 1}, {MaxWork: 1}, {Neighbors: 2, MaxNeighbors: 1}} {
		r, err := ProcessPointCloud(context.Background(), c, o)
		var limit *PointCloudLimitError
		if !errors.Is(err, ErrBudgetExceeded) || !errors.As(err, &limit) || r.Report.Complete || r.Cloud != nil || r.InputToOutput != nil {
			t.Fatalf("budget returned output: %+v, %v", r, err)
		}
	}
	for _, o := range []PointCloudOptions{{Neighbors: 3}, {Neighbors: -1}, {SigmaFactor: math.NaN()}, {MinDistanceMM: -1},
		{DistanceToleranceMM: 1}, {MinDistanceMM: 1, DistanceToleranceMM: 1}, {MaxBytes: -1}, {RemoveOutliers: true}} {
		if _, err := ProcessPointCloud(context.Background(), c, o); !errors.Is(err, ErrInvalidField) {
			t.Fatalf("invalid options accepted %+v: %v", o, err)
		}
	}
	for _, mutate := range []func(*PointCloud){
		func(v *PointCloud) { v.Units = "m" },
		func(v *PointCloud) { v.Points = [][3]float32{{float32(math.Inf(1)), 0, 0}} },
		func(v *PointCloud) { v.Normals = [][3]float32{{0, 0, 1}} },
		func(v *PointCloud) { v.Colors = [][4]uint8{{0, 0, 0, 0}} },
		func(v *PointCloud) { v.Protected = []bool{true} },
		func(v *PointCloud) { v.Scalars = []VertexScalarAttribute{{Name: "x", Values: []float64{1, 2, 3}}} },
		func(v *PointCloud) {
			v.Scalars = []VertexScalarAttribute{{Name: "A", Values: []float64{1, 2, math.NaN()}}}
		},
	} {
		bad := *c
		mutate(&bad)
		if _, err := ProcessPointCloud(context.Background(), &bad, PointCloudOptions{}); !errors.Is(err, ErrInvalidField) {
			t.Fatalf("invalid input accepted: %v", err)
		}
	}
	if _, err := ProcessPointCloud(nil, c, PointCloudOptions{}); !errors.Is(err, ErrInvalidField) {
		t.Fatal("nil context accepted")
	}
	empty := cloudProcess(t, &PointCloud{Units: "mm"}, PointCloudOptions{})
	if empty.Report.OutputPoints != 0 {
		t.Fatal("empty cloud mishandled")
	}
	// Exact payload and work admission boundary; one unit less fails.
	r := cloudProcess(t, c, PointCloudOptions{Neighbors: 1, MinDistanceMM: 0.5})
	for _, o := range []PointCloudOptions{
		{Neighbors: 1, MinDistanceMM: 0.5, MaxBytes: r.Report.ReservedBytes - 1},
		{Neighbors: 1, MinDistanceMM: 0.5, MaxWork: r.Report.Work - 1},
	} {
		if _, err := ProcessPointCloud(context.Background(), c, o); !errors.Is(err, ErrBudgetExceeded) {
			t.Fatal("exact budget boundary not enforced")
		}
	}
	cloudProcess(t, c, PointCloudOptions{Neighbors: 1, MinDistanceMM: 0.5, MaxBytes: r.Report.ReservedBytes, MaxWork: r.Report.Work})
}

func TestPointCloudMetadataDiagnosticIsBounded(t *testing.T) {
	large := strings.Repeat("A", 10_000_000)
	for _, attribute := range []VertexScalarAttribute{{Name: large}, {Name: "A", Units: large}} {
		c := &PointCloud{Units: "mm", Scalars: []VertexScalarAttribute{attribute}}
		// Caller input is outside the quota. Rejection may allocate a small
		// fixed report/error but must not copy or quote the 10 MB metadata.
		runtime.GC()
		var before, after runtime.MemStats
		runtime.ReadMemStats(&before)
		r, err := ProcessPointCloud(context.Background(), c, PointCloudOptions{MaxBytes: 64, MaxWork: 1})
		runtime.ReadMemStats(&after)
		if !errors.Is(err, ErrInvalidField) || r.Report.Complete || r.Cloud != nil || len(err.Error()) > 256 {
			t.Fatalf("unbounded/incorrect metadata diagnostic: %v", err)
		}
		if used := after.TotalAlloc - before.TotalAlloc; used > 16<<10 {
			t.Fatalf("metadata rejection allocated %d bytes", used)
		}
	}
}

type cloudCancelContext struct{ calls, limit int }

func (c *cloudCancelContext) Deadline() (time.Time, bool) { return time.Time{}, false }
func (c *cloudCancelContext) Done() <-chan struct{}       { return nil }
func (c *cloudCancelContext) Value(any) any               { return nil }
func (c *cloudCancelContext) Err() error {
	c.calls++
	if c.calls >= c.limit {
		return context.Canceled
	}
	return nil
}

func TestPointCloudCancellationAllStages(t *testing.T) {
	c := &PointCloud{Units: "mm"}
	for i := 0; i < 128; i++ {
		c.Points = append(c.Points, [3]float32{float32(i % 13), float32(i % 7), float32(i % 11)})
	}
	o := PointCloudOptions{Neighbors: 8, MinDistanceMM: 0.5, RemoveOutliers: true, SigmaFactor: 2}
	complete := cloudProcess(t, c, o)
	for _, limit := range []int{1, 130, 900, 2000, int(complete.Report.Work), int(complete.Report.Work) + 2} {
		r, err := ProcessPointCloud(&cloudCancelContext{limit: limit}, c, o)
		if !errors.Is(err, context.Canceled) || r.Cloud != nil || r.Report.Complete || r.LocalMeanMM != nil || r.Report.State != "canceled" {
			t.Fatalf("cancellation %d escaped output: %+v, %v", limit, r.Report, err)
		}
	}
}

func TestPointCloudIndexAgainstBruteForce(t *testing.T) {
	seed := uint64(42)
	for _, n := range []int{1, 2, 7, 31, 128} {
		c := &PointCloud{Units: "mm"}
		for i := 0; i < n; i++ {
			c.Points = append(c.Points, [3]float32{float32(pointCloudRandom(&seed) % 5), float32(pointCloudRandom(&seed) % 11), float32(pointCloudRandom(&seed) % 3)})
		}
		o, _ := pointCloudOptions(PointCloudOptions{})
		run := pointCloudRun{context.Background(), &PointCloudReport{Options: o}}
		tree, err := buildPointCloudTree(c.Points, &run)
		if err != nil {
			t.Fatal(err)
		}
		for qi := 0; qi < 12; qi++ {
			q := [3]float64{float64(qi) * 0.7, 4.2, -0.2}
			want := make([]pointCloudNeighbor, n)
			for i, p := range c.Points {
				want[i] = pointCloudNeighbor{int32(i), pointCloudDistance2(q, p)}
			}
			sort.Slice(want, func(i, j int) bool { return cloudNeighborWorse(want[j], want[i]) })
			for _, k := range []int{1, n} {
				got, err := tree.nearest(q, -1, k, make([]pointCloudNeighbor, 0, k), &run)
				if err != nil {
					t.Fatal(err)
				}
				sort.Slice(got, func(i, j int) bool { return cloudNeighborWorse(got[j], got[i]) })
				if !reflect.DeepEqual(got, want[:k]) {
					t.Fatalf("nearest mismatch n=%d k=%d", n, k)
				}
			}
		}
	}
}
