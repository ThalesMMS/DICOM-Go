package imagefilter

import (
	"math"
	"testing"
)

func TestVesselnessSlabPlanStableDomainsAndInvalidMetadata(t *testing.T) {
	for _, tc := range []struct {
		dims   [3]int
		budget int64
		want   int
	}{{[3]int{6, 5, 2052}, 384 << 20, 2052}, {[3]int{512, 512, 200}, 384 << 20, 54}, {[3]int{512, 512, 200}, 1, 1}, {[3]int{2, 3, 17}, 0, 17}} {
		got, err := PlanVesselnessSlabSlices(tc.dims, tc.budget)
		if err != nil || got != tc.want {
			t.Fatal(tc, got, err)
		}
	}
	for _, tc := range []struct {
		dims   [3]int
		budget int64
	}{{[3]int{0, 3, 17}, 0}, {[3]int{2, -1, 17}, 0}, {[3]int{2, 3, 0}, 0}, {[3]int{2, 3, 17}, -1}, {[3]int{math.MaxInt, math.MaxInt, 2}, 0}} {
		if _, err := PlanVesselnessSlabSlices(tc.dims, tc.budget); err == nil {
			t.Fatal("invalid metadata accepted", tc)
		}
	}
}
