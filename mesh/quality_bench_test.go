package mesh

import (
	"context"
	"errors"
	"testing"
)

func BenchmarkQualitySeparated(b *testing.B) {
	m := &Mesh{}
	for i := 0; i < 4096; i++ {
		x := float32(i * 3)
		base := uint32(len(m.Vertices))
		m.Vertices = append(m.Vertices, [3]float32{x, 0, 0}, [3]float32{x + 1, 0, 0}, [3]float32{x, 1, 0})
		m.Triangles = append(m.Triangles, [3]uint32{base, base + 1, base + 2})
	}
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		r, e := AnalyzeQuality(context.Background(), m, QualityOptions{MaxResults: 8})
		if e != nil || !r.Complete || r.Candidates != 0 {
			b.Fatal(r, e)
		}
	}
}
func BenchmarkQualityDenseLimited(b *testing.B) {
	m := &Mesh{}
	for i := 0; i < 512; i++ {
		base := uint32(len(m.Vertices))
		m.Vertices = append(m.Vertices, [3]float32{0, 0, 0}, [3]float32{1, 0, 0}, [3]float32{0, 1, 0})
		m.Triangles = append(m.Triangles, [3]uint32{base, base + 1, base + 2})
	}
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		r, e := AnalyzeQuality(context.Background(), m, QualityOptions{MaxCandidates: 64, MaxResults: 4})
		if !errors.Is(e, ErrBudgetExceeded) || r.Candidates != 64 || r.Complete {
			b.Fatal(r, e)
		}
	}
}
