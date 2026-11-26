package registration

import (
	"context"
	"fmt"
	"github.com/ThalesMMS/dicom-go/render"
	"testing"
)

func maskedFFTCostFixture(t testing.TB, size [3]int) (*Image, *Image, *Image, *Image) {
	t.Helper()
	n := size[0] * size[1] * size[2]
	f, m, fm, mm := make([]float32, n), make([]float32, n), make([]float32, n), make([]float32, n)
	for z := 0; z < size[2]; z++ {
		for y := 0; y < size[1]; y++ {
			for x := 0; x < size[0]; x++ {
				i := (z*size[1]+y)*size[0] + x
				f[i] = float32((i*37+i*i*13)%101 - 50)
				if (x+2*y+z)%5 != 0 {
					fm[i] = 1
				}
				if x+1 < size[0] && y+1 < size[1] {
					j := (z*size[1]+y+1)*size[0] + x + 1
					m[j] = 3*f[i] + 17
					if (2*x+y+z)%7 != 0 {
						mm[j] = 1
					}
				}
			}
		}
	}
	spacing := render.Vec3{X: 1, Y: 1, Z: 2}
	return maskedFFTImage(t, size, f, render.Vec3{}, spacing, Identity3()), maskedFFTImage(t, size, m, render.Vec3{}, spacing, Identity3()), maskedFFTImage(t, size, fm, render.Vec3{}, spacing, Identity3()), maskedFFTImage(t, size, mm, render.Vec3{}, spacing, Identity3())
}
func BenchmarkMaskedFFTEquivalentSearch(b *testing.B) {
	for _, size := range [][3]int{{32, 24, 1}, {12, 10, 6}} {
		b.Run(fmt.Sprintf("%dx%dx%d", size[0], size[1], size[2]), func(b *testing.B) {
			f, m, fm, mm := maskedFFTCostFixture(b, size)
			o := MaskedFFTOptions{MinOverlap: 12, MinOverlapFraction: .2, Search: &MaskedFFTSearch{Min: render.Vec3{X: -3, Y: -3, Z: -4}, Max: render.Vec3{X: 3, Y: 3, Z: 4}}}
			s, score, n := maskedFFTDirectSearch(f, m, fm, mm, o)
			r, e := EstimateMaskedFFTTranslation(nil, f, m, fm, mm, o)
			if e != nil || r.IndexShift != s || r.Overlap != n || r.Score-score > 1e-8 || score-r.Score > 1e-8 {
				b.Fatal(r, s, score, n, e)
			}
			for _, mode := range []string{"FFT", "Direct"} {
				b.Run(mode, func(b *testing.B) {
					b.ReportAllocs()
					b.ResetTimer()
					for i := 0; i < b.N; i++ {
						if mode == "FFT" {
							r, e := EstimateMaskedFFTTranslation(context.Background(), f, m, fm, mm, o)
							if e != nil || r.IndexShift != s || r.Overlap != n {
								b.Fatal(r, e)
							}
						} else {
							ss, v, nn := maskedFFTDirectSearch(f, m, fm, mm, o)
							if ss != s || nn != n || v != score {
								b.Fatal(ss, v, nn)
							}
						}
					}
				})
			}
		})
	}
}
