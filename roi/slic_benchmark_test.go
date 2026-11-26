package roi

import (
	"context"
	"testing"
)

var slic1500BenchmarkResult *SLICResult

// Input construction is outside operation timing; SLIC's own allocations and
// connectivity pass are inside. Whole-process peak RSS also includes input,
// preflight, compiler/runtime/test harness and any other requested cases.
func BenchmarkSLICPartition(b *testing.B) {
	cases := []struct {
		name    string
		size    [3]int
		spacing [3]float64
		texture bool
	}{
		{"constant2D32", [3]int{32, 32, 1}, [3]float64{1, 1, 1}, false},
		{"texture2D64", [3]int{64, 64, 1}, [3]float64{1, 1, 1}, true},
		{"anisotropic3D", [3]int{24, 24, 8}, [3]float64{.8, 1.2, 2.5}, true},
	}
	for _, tc := range cases {
		b.Run(tc.name, func(b *testing.B) {
			g := slic1500Grid(tc.size, tc.spacing)
			if tc.texture {
				for i := range g.Values {
					g.Values[i] = float64((i%tc.size[0] + i/tc.size[0]) % 7)
				}
			}
			o := slic1500Options()
			o.TargetStepMM = 6
			o.Compactness = 4
			o.Iterations = 10
			pre, e := SLIC(context.Background(), g, o)
			if e != nil || pre == nil || len(pre.Labels) != len(g.Values) {
				b.Fatalf("preflight %v", e)
			}
			for _, id := range pre.Labels {
				if id == 0 {
					b.Fatal("unassigned preflight")
				}
			}
			b.ReportAllocs()
			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				r, e := SLIC(context.Background(), g, o)
				if e != nil {
					b.Fatal(e)
				}
				slic1500BenchmarkResult = r
			}
			b.StopTimer()
			r := slic1500BenchmarkResult
			b.ReportMetric(float64(len(g.Values)*8), "caller_bytes")
			b.ReportMetric(float64(len(r.Labels)*4), "label_bytes")
			b.ReportMetric(float64(r.ReservedBytes), "logical_reserved_bytes")
			b.ReportMetric(float64(r.Clusters), "clusters/op")
			b.ReportMetric(float64(r.Work), "work/op")
		})
	}
}
