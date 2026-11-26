package registration

import (
	"context"
	"testing"

	"github.com/ThalesMMS/dicom-go/render"
)

func BenchmarkSyNKnownField(b *testing.B) {
	f, m := synPhantomPair(b, 16, .8)
	o := SyNOptions{Levels: []SyNLevel{{Shrink: 1, Iterations: 60}}, Correlation: NeighborhoodCorrelationOptions{Radius: [3]int{2, 2, 2}}, LearningRateMM: .3, UpdateSigmaMM: .8, Regularization: .1, MinimumImprovement: 1e-9}
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		r, err := SyN(context.Background(), f, m, o)
		if err != nil || r.FixedToMoving == nil {
			b.Fatal(err)
		}
	}
}

func BenchmarkNeighborhoodCorrelation(b *testing.B) {
	f, m := synPhantomPair(b, 16, .8)
	c, err := NewNeighborhoodCorrelation(f, m, MetricDomain{}, NeighborhoodCorrelationOptions{Radius: [3]int{2, 2, 2}})
	if err != nil {
		b.Fatal(err)
	}
	tr := NewTranslation3D()
	_ = tr.SetTranslation(render.Vec3{X: .17, Y: .23, Z: .31})
	for _, derivative := range []bool{false, true} {
		name := "value"
		if derivative {
			name = "full-derivative"
		}
		b.Run(name, func(b *testing.B) {
			b.ReportAllocs()
			for i := 0; i < b.N; i++ {
				if derivative {
					_, _, err = c.ValueAndDerivative(tr)
				} else {
					_, err = c.Value(tr)
				}
				if err != nil {
					b.Fatal(err)
				}
			}
		})
	}
}
