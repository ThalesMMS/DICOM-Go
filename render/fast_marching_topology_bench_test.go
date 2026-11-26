package render

import (
	"context"
	"testing"
)

func BenchmarkFastMarchingTopologyCost(b *testing.B) {
	g := uniformFastMarchingGrid(24, 20, 5, Vec3{.7, 1.1, 1.8}, 1)
	seeds := []FastMarchingSeed{{Index: [3]int{4, 10, 2}}, {Index: [3]int{19, 10, 2}}}
	for _, profile := range []struct {
		name string
		mode FastMarchingTopologyMode
	}{{"None", FastMarchingTopologyNone}, {"Strict", FastMarchingTopologyStrict}, {"NoHandles", FastMarchingTopologyNoHandles}} {
		b.Run(profile.name, func(b *testing.B) {
			b.ReportAllocs()
			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				r, e := FastMarchingConstrained(context.Background(), g, seeds, FastMarchingOptions{}, FastMarchingTopologyOptions{Mode: profile.mode})
				if e != nil {
					b.Fatal(e)
				}
				if profile.mode != FastMarchingTopologyNone {
					b.ReportMetric(float64(r.Report.RejectedCandidates), "rejected/op")
					b.ReportMetric(float64(r.Report.Work), "work/op")
				}
			}
		})
	}
}
