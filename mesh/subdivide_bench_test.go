package mesh

import "testing"

func BenchmarkSubdivisionLocalExpansion(b *testing.B) {
	for _, profile := range []string{"plane", "closed", "attributes"} {
		b.Run(profile, func(b *testing.B) {
			m := subdivisionGrid(16)
			o := subdivisionTestOptions(.6)
			if profile == "closed" {
				m = subdivisionTetra()
				o.MaxEdgeLengthMM = .4
			}
			if profile == "attributes" {
				values := make([]float64, len(m.Vertices))
				values2 := make([]float64, len(m.Vertices))
				for i, v := range m.Vertices {
					values[i] = float64(v[0])
					values2[i] = float64(v[1])
				}
				o.VertexAttributes = []SubdivisionVertexAttribute{{Attribute: VertexScalarAttribute{Name: "coordinate_u", Values: values}}, {Attribute: VertexScalarAttribute{Name: "coordinate_v", Values: values2}}}
				labels := make([]float64, len(m.Triangles))
				for i := range labels {
					labels[i] = float64(i % 2)
				}
				o.FaceAttributes = []VertexScalarAttribute{{Name: "part", Values: labels}}
			}
			b.ReportAllocs()
			b.ResetTimer()
			var r *SubdivisionResult
			for i := 0; i < b.N; i++ {
				var err error
				r, err = SubdivideEdges(nil, m, o)
				if err != nil {
					b.Fatal(err)
				}
			}
			b.StopTimer()
			b.ReportMetric(float64(len(m.Vertices)), "input-V")
			b.ReportMetric(float64(len(m.Triangles)), "input-T")
			b.ReportMetric(float64(len(r.Mesh.Vertices)), "output-V")
			b.ReportMetric(float64(len(r.Mesh.Triangles)), "output-T")
			b.ReportMetric(float64(r.Report.AllocatedBytes), "charged-B")
			b.ReportMetric(float64(r.Report.Passes), "passes/op")
		})
	}
}
