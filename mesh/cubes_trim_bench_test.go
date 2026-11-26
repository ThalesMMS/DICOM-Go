package mesh

import (
	"context"
	"fmt"
	"math"
	"sync/atomic"
	"testing"
	"time"
)

// The corpus and promotion gates are declared before the experiment in
// docs/MESH_TRIMMING_EXPERIMENT.md. Input construction is outside timed work.
func trimPerfField(name string) *DenseField {
	dims := [3]int{96, 96, 96}
	if name == "dense" {
		return randomField(1495, dims, identityAffine)
	}
	f := &DenseField{Size: dims, Affine: identityAffine, Data: make([]float32, 96*96*96)}
	for z := 0; z < 96; z++ {
		for y := 0; y < 96; y++ {
			for x := 0; x < 96; x++ {
				dx, dy, dz := float64(x)-47.5, float64(y)-47.5, float64(z)-47.5
				f.Data[(z*96+y)*96+x] = float32(10 - math.Sqrt(dx*dx+dy*dy+dz*dz))
			}
		}
	}
	return f
}

type trimReadMeter struct {
	Field
	calls, samples, nanos atomic.Int64
}

func (f *trimReadMeter) ReadSlice(ctx context.Context, z int, dst []float32) error {
	start := time.Now()
	err := f.Field.ReadSlice(ctx, z, dst)
	f.nanos.Add(time.Since(start).Nanoseconds())
	f.calls.Add(1)
	f.samples.Add(int64(len(dst)))
	return err
}

func TestCubesTrimBaselineInstrumentation(t *testing.T) {
	for _, corpus := range []string{"sparse", "dense"} {
		for _, normals := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/normals=%t", corpus, normals), func(t *testing.T) {
				f := &trimReadMeter{Field: trimPerfField(corpus)}
				start := time.Now()
				m, err := Extract(context.Background(), f, Options{Workers: 2, SkipNormals: !normals})
				if err != nil {
					t.Fatal(err)
				}
				t.Logf("instrumented diagnostic only: elapsed=%s field_read=%s calls=%d samples=%d vertices=%d triangles=%d output_bytes=%d", time.Since(start), time.Duration(f.nanos.Load()), f.calls.Load(), f.samples.Load(), len(m.Vertices), len(m.Triangles), m.Bytes())
			})
		}
	}
}

var trimBenchmarkSink int64

func BenchmarkCubesTrimBaseline(b *testing.B) {
	trimBenchmark(b, false)
}

func BenchmarkCubesTrimCandidate(b *testing.B) {
	trimBenchmark(b, true)
}

func trimBenchmark(b *testing.B, candidate bool) {
	for _, name := range []string{"sparse", "dense"} {
		f := trimPerfField(name)
		b.Run(name, func(b *testing.B) {
			b.Run("FieldRead", func(b *testing.B) {
				dst := make([]float32, 96*96)
				b.ReportAllocs()
				b.SetBytes(int64(len(f.Data) * 4))
				b.ResetTimer()
				for n := 0; n < b.N; n++ {
					for z := 0; z < 96; z++ {
						if err := f.ReadSlice(context.Background(), z, dst); err != nil {
							b.Fatal(err)
						}
					}
				}
			})
			b.Run("Classification", func(b *testing.B) {
				flags := make([]uint8, len(f.Data))
				for i, v := range f.Data {
					if v >= 0 {
						flags[i] = 1
					}
				}
				b.ReportAllocs()
				b.ResetTimer()
				var total int64
				for n := 0; n < b.N; n++ {
					for z := 0; z < 95; z++ {
						in0, in1 := flags[z*96*96:(z+1)*96*96], flags[(z+1)*96*96:(z+2)*96*96]
						for y := 0; y < 95; y++ {
							if candidate {
								i := y * 96
								left := trimColumnCase(in0, in1, i, 96)
								for x := 0; x < 95; x, i = x+1, i+1 {
									right := trimColumnCase(in0, in1, i+1, 96)
									total += int64(mcTriangleCount[left|right<<1])
									left = right
								}
							} else {
								for x, i := 0, y*96; x < 95; x, i = x+1, i+1 {
									total += int64(mcTriangleCount[cellCase(in0, in1, i, 96)])
								}
							}
						}
					}
				}
				trimBenchmarkSink = total
			})
			b.Run("Count", func(b *testing.B) {
				frame, err := newAffineFrame(f.Affine)
				if err != nil {
					b.Fatal(err)
				}
				b.ReportAllocs()
				b.ResetTimer()
				for n := 0; n < b.N; n++ {
					e := newCubesExtractor(f, frame, Options{Workers: 2})
					e.xyCount, e.zCount, e.triCount = make([]int64, e.nZ), make([]int64, e.nZ-1), make([]int64, e.nZ-1)
					if err := forEachSlab(context.Background(), e.nZ-1, 2, func() slabFunc {
						if candidate {
							return trimNewWorker(e, false).count
						}
						return e.newWorker(false).count
					}); err != nil {
						b.Fatal(err)
					}
				}
			})
			for _, normals := range []bool{false, true} {
				label := "ExtractNoNormals"
				if normals {
					label = "ExtractNormals"
				}
				b.Run(label, func(b *testing.B) {
					b.ReportAllocs()
					b.ResetTimer()
					for n := 0; n < b.N; n++ {
						var m *Mesh
						var err error
						if candidate {
							m, err = trimPrototypeRun(context.Background(), f, Options{Workers: 2, SkipNormals: !normals})
						} else {
							m, err = Extract(context.Background(), f, Options{Workers: 2, SkipNormals: !normals})
						}
						if err != nil {
							b.Fatal(err)
						}
						trimBenchmarkSink = int64(len(m.Triangles))
					}
					// Slice payload only: all allocations are also measured in B/op.
					plane := 98 * 98
					scratch := 40*plane + 4*96*96
					if candidate {
						scratch += plane + 8*98
					}
					b.ReportMetric(float64(scratch), "scratch-B/worker")
				})
			}
		})
	}
}
