package roi

import (
	"context"
	"math"
	"testing"
)

// Benchmarks are explicit cost profiles; they do not install a consumer default
// or claim equivalence to the existing integer contour simplifier.
func BenchmarkPhysicalPolyline(b *testing.B) {
	for _, kind := range []string{"Line", "Sine", "Helix", "Circle", "Protected"} {
		p := make([]Point3D, 257)
		closed := kind == "Circle"
		for i := range p {
			x := float64(i) / 16
			p[i] = Point3D{X: x}
			switch kind {
			case "Sine", "Protected":
				p[i].Y = math.Sin(x)
			case "Helix":
				p[i] = Point3D{X: math.Cos(x), Y: math.Sin(x), Z: x / 10}
			case "Circle":
				angle := 2 * math.Pi * float64(i) / float64(len(p))
				p[i] = Point3D{X: 10 * math.Cos(angle), Y: 10 * math.Sin(angle)}
			}
		}
		for _, profile := range []PolylineTopology{PolylineRequireSimple, PolylineIgnoreIntersections} {
			name := "Simple"
			if profile == PolylineIgnoreIntersections {
				name = "Unchecked"
			}
			b.Run(kind+"/"+name, func(b *testing.B) {
				o := PolylineSimplifyOptions{Closed: closed, ToleranceMM: .05, Topology: profile}
				if kind == "Protected" {
					for i := 0; i < len(p); i += 16 {
						o.ProtectedIndices = append(o.ProtectedIndices, i)
					}
				}
				b.ReportAllocs()
				b.ResetTimer()
				for i := 0; i < b.N; i++ {
					result, err := SimplifyPhysicalPolyline(context.Background(), p, o)
					if err != nil {
						b.Fatal(err)
					}
					if len(result.Points) == 0 {
						b.Fatal("empty result")
					}
				}
			})
		}
	}
}
