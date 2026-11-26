package mrdiffusion

import (
	"fmt"
	"testing"
)

func BenchmarkTensorReconstruction(b *testing.B) {
	for _, size := range [][3]int{{8, 8, 4}, {32, 24, 8}} {
		b.Run(fmt.Sprintf("%dx%dx%d", size[0], size[1], size[2]), func(b *testing.B) {
			g := tensorTestGrid()
			g.Size = size
			v := tensorTestFixture(g, Tensor3{.0018, .0007, .0004}, TensorLPS)
			b.ReportAllocs()
			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				r, e := FitTensor(nil, v, TensorOptions{Workers: 1})
				if e != nil || r.Counts[TensorValid] != len(v[0].Signals) {
					b.Fatal(r.Counts, e)
				}
				tensorNear(b, r.Voxels[0].MD, (.0018+.0007+.0004)/3, 3e-10)
			}
		})
	}
}
func BenchmarkTensorDerivedMap(b *testing.B) {
	g := tensorTestGrid()
	v := tensorTestFixture(g, Tensor3{.0018, .0007, .0004}, TensorLPS)
	r, e := FitTensor(nil, v, TensorOptions{})
	if e != nil {
		b.Fatal(e)
	}
	o := tensorTestMapOptions()
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		f, e := WriteTensorMap(nil, r, TensorMapFA, o)
		if e != nil || f == nil {
			b.Fatal(e)
		}
	}
}
