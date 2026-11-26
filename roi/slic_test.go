package roi

import (
	"context"
	"encoding/json"
	"errors"
	"math"
	"os"
	"reflect"
	"sync"
	"testing"
	"time"
)

func slic1500Grid(s [3]int, spacing [3]float64) SLICGrid {
	return SLICGrid{Size: s, Spacing: spacing, Values: make([]float64, s[0]*s[1]*s[2])}
}
func slic1500Options() SLICOptions {
	return SLICOptions{TargetStepMM: 2, IntensityScale: 1, Compactness: 1, Iterations: 5}
}

// This oracle uses a disjoint-set of the final label graph, independent of the
// production flood queue. It checks that each positive label is exactly one
// connected component and no unavailable pixel has been absorbed.
func slic1500CheckConnected(t *testing.T, g SLICGrid, r *SLICResult) {
	t.Helper()
	n := len(r.Labels)
	parents := make([]int, n)
	for i := range parents {
		parents[i] = i
	}
	var root func(int) int
	root = func(i int) int {
		for parents[i] != i {
			i = parents[i]
		}
		return i
	}
	for i, id := range r.Labels {
		c := [3]int{i % g.Size[0], i / g.Size[0] % g.Size[1], i / (g.Size[0] * g.Size[1])}
		valid := (g.Available == nil || g.Available[i]) && !math.IsNaN(g.Values[i]) && !math.IsInf(g.Values[i], 0)
		if (id != 0) != valid {
			t.Fatalf("availability[%d] label%d", i, id)
		}
		if id == 0 {
			continue
		}
		for dz := -1; dz <= 1; dz++ {
			for dy := -1; dy <= 1; dy++ {
				for dx := -1; dx <= 1; dx++ {
					a := dx
					if a < 0 {
						a = -a
					}
					b := dy
					if b < 0 {
						b = -b
					}
					d := dz
					if d < 0 {
						d = -d
					}
					if a+b+d == 0 || (r.Options.Connectivity == SLICFaceConnectivity && a+b+d != 1) {
						continue
					}
					x, y, z := c[0]+dx, c[1]+dy, c[2]+dz
					if x < 0 || y < 0 || z < 0 || x >= g.Size[0] || y >= g.Size[1] || z >= g.Size[2] {
						continue
					}
					j := (z*g.Size[1]+y)*g.Size[0] + x
					if r.Labels[j] == id {
						parents[root(j)] = root(i)
					}
				}
			}
		}
	}
	unique := map[uint32]int{}
	for i, id := range r.Labels {
		if id == 0 {
			continue
		}
		p := root(i)
		if old, ok := unique[id]; ok && old != p {
			t.Fatalf("label%d disconnected", id)
		}
		unique[id] = p
	}
	if len(unique) != r.Clusters {
		t.Fatalf("reported clusters %d vs %d", r.Clusters, len(unique))
	}
}
func TestSLICIndependentPhysicalDistance(t *testing.T) {
	// The first-iteration lattice is known analytically: centers at the lower
	// integer in each 2x2 cell. All centers are inside every 4x4 search window.
	g := slic1500Grid([3]int{4, 4, 1}, [3]float64{1, 1, 1})
	for i := range g.Values {
		g.Values[i] = float64((i%4)+(i/4)%2) * .25
	}
	o := slic1500Options()
	o.Iterations = 1
	o.IntensityScale = 2
	o.Compactness = 3
	r, e := SLIC(context.Background(), g, o)
	if e != nil {
		t.Fatal(e)
	}
	centers := [][2]int{{0, 0}, {2, 0}, {0, 2}, {2, 2}}
	want := make([]int, 16)
	for i, v := range g.Values {
		best := math.Inf(1)
		for k, c := range centers {
			dx, dy := float64(i%4-c[0]), float64(i/4-c[1])
			dv := (v - g.Values[c[1]*4+c[0]]) / 2
			d := dv*dv + 9*(dx*dx+dy*dy)/4
			if d < best {
				best = d
				want[i] = k
			}
		}
	}
	// Cluster IDs are internal and connectivity canonicalizes them. Compare
	// equivalence relations rather than assuming their arbitrary numbering.
	for i := range want {
		for j := range want {
			if (want[i] == want[j]) != (r.Labels[i] == r.Labels[j]) {
				t.Fatalf("partition mismatch %d/%d", i, j)
			}
		}
	}
	if r.Converged || !r.IterationLimitReached || r.Iterations != 1 {
		t.Fatal("iteration cap misreported")
	}
	slic1500CheckConnected(t, g, r)
}
func TestSLICPhantomsPhysicalCompactness(t *testing.T) {
	for _, size := range [][3]int{{8, 4, 1}, {8, 4, 3}} {
		g := slic1500Grid(size, [3]float64{1, 2, 3})
		o := slic1500Options()
		o.TargetStepMM = 4
		r, e := SLIC(context.Background(), g, o)
		if e != nil {
			t.Fatal(e)
		}
		slic1500CheckConnected(t, g, r)
		// 4mm seeds: 4 columns, 2 rows and 1 slice (3mm spacing rounds to 1).
		expected := 2 * 2 * size[2]
		if r.InitialClusters != expected || r.Clusters != expected {
			t.Fatalf("physical grid %+v: init%d/final%d want%d", size, r.InitialClusters, r.Clusters, expected)
		}
		if !r.Converged || r.Objective < 0 || math.IsNaN(r.Objective) {
			t.Fatal("constant convergence/energy")
		}
	}
	g := slic1500Grid([3]int{8, 4, 1}, [3]float64{1, 1, 1})
	for i := range g.Values {
		if i%8 >= 4 {
			g.Values[i] = 100
		}
	}
	low := slic1500Options()
	low.Compactness = 0
	high := low
	high.Compactness = 1000
	a, e := SLIC(context.Background(), g, low)
	if e != nil {
		t.Fatal(e)
	}
	b, e := SLIC(context.Background(), g, high)
	if e != nil {
		t.Fatal(e)
	}
	if a.Clusters != 2 || b.Clusters <= a.Clusters {
		t.Fatalf("compactness response %d/%d", a.Clusters, b.Clusters)
	}
	for i := range g.Values {
		for j := range g.Values {
			if (g.Values[i] == g.Values[j]) != (a.Labels[i] == a.Labels[j]) {
				t.Fatal("sharp edge mixed under intensity-only distance")
			}
		}
	}
	slic1500CheckConnected(t, g, a)
	slic1500CheckConnected(t, g, b)
}
func TestSLICTextureOwnershipAndConcurrentDeterminism(t *testing.T) {
	g := slic1500Grid([3]int{8, 6, 3}, [3]float64{.8, 1.2, 2})
	for i := range g.Values {
		g.Values[i] = float64((i%8 + i/8%6 + i/48) % 3)
	}
	original := append([]float64(nil), g.Values...)
	o := slic1500Options()
	o.Compactness = 4
	r, e := SLIC(context.Background(), g, o)
	if e != nil {
		t.Fatal(e)
	}
	slic1500CheckConnected(t, g, r)
	var wg sync.WaitGroup
	for k := 0; k < 4; k++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			q, err := SLIC(context.Background(), g, o)
			if err != nil {
				t.Error(err)
				return
			}
			if !reflect.DeepEqual(q, r) {
				t.Error("nondeterministic result")
			}
			q.Labels[0] = math.MaxUint32
		}()
	}
	wg.Wait()
	if !reflect.DeepEqual(original, g.Values) || r.Labels[0] == math.MaxUint32 {
		t.Fatal("input or result aliases")
	}
}
func TestSLICConnectivityAndUnavailablePolicy(t *testing.T) {
	g := slic1500Grid([3]int{2, 2, 1}, [3]float64{1, 1, 1})
	g.Available = []bool{true, false, false, true}
	o := slic1500Options()
	o.TargetStepMM = 10
	a, e := SLIC(context.Background(), g, o)
	if e != nil {
		t.Fatal(e)
	}
	o.Connectivity = SLICFullConnectivity
	b, e := SLIC(context.Background(), g, o)
	if e != nil {
		t.Fatal(e)
	}
	if a.Clusters != 2 || a.ConnectivitySplits != 1 || b.Clusters != 1 {
		t.Fatalf("face/full %+v/%+v", a, b)
	}
	slic1500CheckConnected(t, g, a)
	slic1500CheckConnected(t, g, b)
	g.Available = nil
	g.Values[0] = math.NaN()
	g.Values[1] = math.Inf(1)
	q, e := SLIC(context.Background(), g, o)
	if e != nil {
		t.Fatal(e)
	}
	if q.AvailableVoxels != 2 || q.UnavailableVoxels != 2 || q.Labels[0] != 0 || q.Labels[1] != 0 {
		t.Fatal("nonfinite policy")
	}
	o.RejectNonFinite = true
	if q, e = SLIC(context.Background(), g, o); q != nil || !errors.Is(e, ErrSLICInput) {
		t.Fatal("nonfinite strict")
	}
	g.Available = make([]bool, 4)
	q, e = SLIC(context.Background(), g, o)
	if e != nil || q.Clusters != 0 || !q.Converged || q.AvailableVoxels != 0 {
		t.Fatal("empty domain")
	}
}

type slic1500Cancel struct {
	context.Context
	calls int
	after int
}

func (c *slic1500Cancel) Err() error {
	c.calls++
	if c.calls >= c.after {
		return context.Canceled
	}
	return nil
}
func TestSLICAdmissionAndCancellation(t *testing.T) {
	g := slic1500Grid([3]int{16, 16, 2}, [3]float64{1, 1, 1})
	base := slic1500Options()
	for name, change := range map[string]func(*SLICOptions){"voxels": func(o *SLICOptions) { o.MaxVoxels = 1 }, "clusters": func(o *SLICOptions) { o.MaxClusters = 1 }, "bytes": func(o *SLICOptions) { o.MaxBytes = 1 }, "work": func(o *SLICOptions) { o.MaxWork = 1 }, "iterations": func(o *SLICOptions) { o.MaxIterations = 1 }} {
		t.Run(name, func(t *testing.T) {
			o := base
			change(&o)
			if q, e := SLIC(context.Background(), g, o); q != nil || !errors.Is(e, ErrSLICLimit) {
				t.Fatalf("%+v %v", q, e)
			}
		})
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if q, e := SLIC(ctx, g, base); q != nil || !errors.Is(e, context.Canceled) {
		t.Fatal("initial cancellation")
	}
	for _, after := range []int{3, 5, 9} {
		if q, e := SLIC(&slic1500Cancel{Context: context.Background(), after: after}, g, base); q != nil || !errors.Is(e, context.Canceled) {
			t.Fatalf("during cancellation %d: %+v %v", after, q, e)
		}
	}
	small := slic1500Grid([3]int{2, 2, 1}, [3]float64{1, 1, 1})
	small.Available = []bool{true, false, false, true}
	o := base
	o.TargetStepMM = 10
	o.MaxClusters = 1
	if q, e := SLIC(context.Background(), small, o); q != nil || !errors.Is(e, ErrSLICLimit) {
		t.Fatal("connected output cluster quota")
	}
	invalid := []SLICOptions{base, base, base, base}
	invalid[0].IntensityScale = 0
	invalid[1].Compactness = math.NaN()
	invalid[2].Connectivity = 255
	invalid[3].Iterations = 0
	for _, o := range invalid {
		if q, e := SLIC(context.Background(), g, o); q != nil || !errors.Is(e, ErrSLICInput) {
			t.Fatal("invalid metric accepted")
		}
	}
	bad := g
	bad.Available = []bool{true}
	if q, e := SLIC(context.Background(), bad, base); q != nil || !errors.Is(e, ErrSLICInput) {
		t.Fatal("incompatible availability")
	}
	bad = g
	bad.Size[0] = 0
	if q, e := SLIC(context.Background(), bad, base); q != nil || !errors.Is(e, ErrSLICInput) {
		t.Fatal("empty dimensions")
	}
	bad = slic1500Grid([3]int{1, 1, 1}, [3]float64{1, 1, 1})
	bad.Values[0] = math.MaxFloat64
	if q, e := SLIC(context.Background(), bad, base); q != nil || !errors.Is(e, ErrSLICInput) {
		t.Fatal("metric overflow")
	}
}

// This is an optional independent comparison, not a built-in golden generated
// by this implementation. Partition differences are reported, not hidden by
// equating arbitrary IDs or assuming both connectivity policies are identical.
func TestSLICITKOracleOptIn(t *testing.T) {
	path := os.Getenv("SLIC_ITK_FIXTURE")
	if path == "" {
		t.Skip("SLIC_ITK_FIXTURE absent: independent ITK comparison not executed")
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if info.Size() > 1<<20 {
		t.Fatal("independent fixture exceeds 1MiB")
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var f struct {
		Schema  string `json:"schema"`
		Version string `json:"itk_version"`
		Cases   []struct {
			Name        string     `json:"name"`
			Size        [3]int     `json:"size"`
			Spacing     [3]float64 `json:"spacing"`
			Target      float64    `json:"target_step_mm"`
			Scale       float64    `json:"intensity_scale"`
			Compactness float64    `json:"compactness"`
			Iterations  int        `json:"iterations"`
			Perturb     bool       `json:"initialization_perturbation"`
			Connect     bool       `json:"enforce_connectivity"`
			Elapsed     int64      `json:"elapsed_ns"`
			Values      []float64  `json:"values"`
			Labels      []uint32   `json:"labels"`
		} `json:"cases"`
	}
	if err = json.Unmarshal(raw, &f); err != nil {
		t.Fatal(err)
	}
	if f.Schema != "slic-itk-reference-v1" || f.Version == "" || len(f.Cases) != 6 {
		t.Fatal("invalid independent fixture schema")
	}
	names := map[string]bool{}
	for _, c := range f.Cases {
		t.Run(c.Name, func(t *testing.T) {
			if names[c.Name] || c.Name == "" || c.Perturb || !c.Connect || c.Elapsed <= 0 {
				t.Fatal("invalid provenance/settings")
			}
			names[c.Name] = true
			if c.Size[0] != 9 || c.Size[1] != 9 || (c.Size[2] != 1 && c.Size[2] != 3) || c.Spacing != [3]float64{1, 1, 1} || c.Target != 3 || c.Scale != 1 || c.Iterations != 5 || (c.Compactness != 4 && c.Compactness != 16) || len(c.Values) > 243 {
				t.Fatal("fixture does not match finite comparison recipe")
			}
			g := SLICGrid{Size: c.Size, Spacing: c.Spacing, Values: c.Values}
			o := SLICOptions{TargetStepMM: c.Target, IntensityScale: c.Scale, Compactness: c.Compactness, Iterations: c.Iterations}
			before := time.Now()
			r, e := SLIC(context.Background(), g, o)
			nativeElapsed := time.Since(before).Nanoseconds()
			if e != nil {
				t.Fatal(e)
			}
			if len(c.Labels) != len(r.Labels) {
				t.Fatal("reference geometry mismatch")
			}
			slic1500CheckConnected(t, g, r)
			var differences, pairs int64
			for i := range r.Labels {
				for j := i + 1; j < len(r.Labels); j++ {
					pairs++
					if (r.Labels[i] == r.Labels[j]) != (c.Labels[i] == c.Labels[j]) {
						differences++
					}
				}
			}
			t.Logf("ITK%s %s partition_pair_disagreements=%d/%d itk_elapsed_ns=%d native_elapsed_ns=%d native_work=%d native_clusters=%d", f.Version, c.Name, differences, pairs, c.Elapsed, nativeElapsed, r.Work, r.Clusters)
		})
	}
}
