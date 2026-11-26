package mesh

import (
	"math"
	"testing"
)

// The same asymmetric closed/open surfaces are used in before/after runs.
// Input construction and validation are outside the timed region.
func benchmarkDecimationMesh(resolution int, open bool) *Mesh {
	m := uvSphere(20, resolution, resolution)
	for i := range m.Vertices {
		m.Vertices[i][0] *= float32(1 + 0.02*math.Sin(float64(i)*1.618))
		m.Vertices[i][1] *= float32(1 + 0.025*math.Cos(float64(i)*2.718))
		m.Vertices[i][2] *= float32(1 + 0.015*math.Sin(float64(i)*math.Sqrt2))
	}
	if open {
		triangles := m.Triangles[:0]
		for _, triangle := range m.Triangles {
			if m.Vertices[triangle[0]][2] >= 0 && m.Vertices[triangle[1]][2] >= 0 && m.Vertices[triangle[2]][2] >= 0 {
				triangles = append(triangles, triangle)
			}
		}
		m.Triangles = triangles
		cleaned, _ := Clean(m, 0)
		m = cleaned
	}
	return m
}

func BenchmarkDecimateQuadric(b *testing.B) {
	for _, test := range []struct {
		name       string
		resolution int
		open       bool
	}{
		{"Closed32", 32, false},
		{"Closed70", 70, false},
		{"Boundary70", 70, true},
	} {
		b.Run(test.name, func(b *testing.B) {
			input := benchmarkDecimationMesh(test.resolution, test.open)
			if err := input.Validate(); err != nil {
				b.Fatal(err)
			}
			opts := DecimateOptions{TargetReduction: 0.7, PreserveBoundary: test.open}
			b.ReportAllocs()
			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				out, report, err := DecimateQuadric(input, opts)
				if err != nil || out == nil || report.TrianglesAfter >= report.TrianglesBefore {
					b.Fatalf("decimation: %+v, %v", report, err)
				}
			}
			b.ReportMetric(float64(len(input.Triangles))*float64(b.N)/b.Elapsed().Seconds(), "triangles/s")
		})
	}
}
