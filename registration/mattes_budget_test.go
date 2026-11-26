package registration

import (
	"fmt"
	"math"
	"sync"
	"sync/atomic"
	"testing"
)

func TestMattesChunksInFlight(t *testing.T) {
	const mib = 1 << 20
	for _, tc := range []struct {
		budget                     int64
		totalFloats, partialFloats int
		want                       int
	}{
		// One 32 MiB array beside a 32 MiB total: 15 more fit in 512 MiB.
		{512 * mib, 4 * mib, 4 * mib, 15},
		// The total alone exhausts the budget: one partial still runs.
		{512 * mib, 64 * mib, 32 * mib, 1},
		{0, 1, 1, 1},
		// Small arrays leave the in-flight count to the workers.
		{512 * mib, 2550, 2550, (512*mib - 2550*8) / (2550 * 8)},
		{512 * mib, 0, 0, 512 * mib / 8},
	} {
		if got := mattesChunksInFlight(tc.budget, tc.totalFloats, tc.partialFloats); got != tc.want {
			t.Errorf("mattesChunksInFlight(%d, %d, %d) = %d, want %d", tc.budget, tc.totalFloats, tc.partialFloats, got, tc.want)
		}
	}
}

// TestMetricChunksInFlightRespectBudget runs the ordered reduction with the
// in-flight bound a Mattes budget gives: chunk results started and not yet
// merged never exceed it, whatever the workers, and chunks merge in order.
func TestMetricChunksInFlightRespectBudget(t *testing.T) {
	const partialFloats = 1 << 17 // a 1 MiB derivative partial
	for _, tc := range []struct {
		budget  int64
		workers int
	}{
		{4 << 20, 16}, {8 << 20, 16}, {1 << 20, 8}, {512 << 20, 4},
	} {
		t.Run(fmt.Sprintf("budget_%d_workers_%d", tc.budget, tc.workers), func(t *testing.T) {
			inFlight := mattesChunksInFlight(tc.budget, partialFloats, partialFloats)
			if want := max(1, int(tc.budget/(8*partialFloats))-1); inFlight != want {
				t.Fatalf("in flight %d, want %d", inFlight, want)
			}
			var live, peak atomic.Int64
			var mu sync.Mutex
			allocated := 0
			merged := 0
			count := 40*metricChunk + 17
			reduceMetricChunks(count, tc.workers, inFlight, func() func(int, int) int {
				return func(start, end int) int {
					mu.Lock()
					allocated++
					mu.Unlock()
					now := live.Add(1)
					for {
						p := peak.Load()
						if now <= p || peak.CompareAndSwap(p, now) {
							break
						}
					}
					return start
				}
			}, func(start int) {
				if start != merged*metricChunk {
					t.Errorf("merged chunk at %d, want %d", start, merged*metricChunk)
				}
				merged++
				live.Add(-1)
			})
			if merged != 41 || allocated != 41 {
				t.Fatalf("merged %d, visited %d chunks, want 41", merged, allocated)
			}
			if p := int(peak.Load()); p > inFlight {
				t.Fatalf("%d chunk results in flight, budget allows %d", p, inFlight)
			}
			if bytes := int64(peak.Load()+1) * 8 * partialFloats; bytes > max(tc.budget, 2*8*partialFloats) {
				t.Fatalf("%d bytes held, budget %d", bytes, tc.budget)
			}
		})
	}
}

// TestMattesWorkspaceBudgetDoesNotChangeResult evaluates the dense and the
// local-support derivatives under a budget that allows one chunk in flight
// and under the default: the values and derivatives are identical.
func TestMattesWorkspaceBudgetDoesNotChangeResult(t *testing.T) {
	fixed, moving := ffdTestImages(t, [3]int{32, 32, 16}, 2, 2)
	bspline, err := NewBSplineForImage(fixed, [3]int{4, 4, 2})
	if err != nil {
		t.Fatal(err)
	}
	parameters := make([]float64, bspline.NumberOfParameters())
	for i := range parameters {
		parameters[i] = .1 * math.Sin(float64(i))
	}
	if err := bspline.SetParameters(parameters); err != nil {
		t.Fatal(err)
	}
	for _, c := range []struct {
		name      string
		transform Transform
	}{{"local", bspline}, {"dense", denseOnly{bspline}}} {
		t.Run(c.name, func(t *testing.T) {
			var want []float64
			var wantValue float64
			for _, budget := range []int64{0, 1} {
				metric, err := NewMattesMutualInformation(fixed, moving, MetricDomain{Workers: 8}, 32)
				if err != nil {
					t.Fatal(err)
				}
				metric.workspaceBytes = budget
				value, derivative, err := metric.ValueAndDerivative(c.transform)
				if err != nil {
					t.Fatal(err)
				}
				if want == nil {
					want, wantValue = derivative, value
					continue
				}
				if value != wantValue {
					t.Fatalf("value %g under budget %d, want %g", value, budget, wantValue)
				}
				for i := range want {
					if derivative[i] != want[i] {
						t.Fatalf("derivative[%d] = %g under budget %d, want %g", i, derivative[i], budget, want[i])
					}
				}
			}
		})
	}
}
