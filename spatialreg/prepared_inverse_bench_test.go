package spatialreg

import (
	"context"
	"fmt"
	"testing"
	"unsafe"
)

var inverter1531Results []InverseResult
var inverter1531Constants inverseConstants

// Every benchmark prepares input points outside the timer. Evaluation excludes
// the one prepared snapshot; total includes snapshot+batch+Close. Original
// retains no field copy; prepared logical field payload is12N, reported without
// pretending it is peak RSS. These compare to the namespaced frozen algorithm.
func BenchmarkPreparedInverse1531(b *testing.B) {
	for _, kind := range []string{"identity", "affine", "smooth", "large"} {
		for _, count := range []int{1, 16, 4096} {
			for _, strict := range []bool{false, true} {
				name := fmt.Sprintf("%s/n%d/strict%t", kind, count, strict)
				b.Run(name, func(b *testing.B) {
					r := inverter1531Fixture(kind)
					points := inverter1531Sources(b, r, count)
					opts := InverseOptions{}
					if strict {
						opts.Tolerance = 1e-9
					}
					var e error
					output := uint64(count) * uint64(unsafe.Sizeof(InverseResult{}))
					modes := []string{"original-constants", "prepared-construction", "original-total", "current-total", "prepared-evaluation", "prepared-total", "prepared-point-evaluation", "prepared-point-total"}
					for _, mode := range modes {
						b.Run(mode, func(b *testing.B) {
							// Original modes never prepare/copy a candidate grid, including setup.
							var prepared *PreparedInverse
							var retained uint64
							if mode == "prepared-evaluation" || mode == "prepared-point-evaluation" {
								prepared, e = r.PrepareInverse(opts)
								if e != nil {
									b.Fatal(e)
								}
								defer prepared.Close()
							}
							if mode != "original-constants" && mode != "original-total" && mode != "current-total" && r.Grid != nil {
								retained = uint64(len(r.Grid.vectors)) * 12
							}
							b.ReportAllocs()
							b.ResetTimer()
							for i := 0; i < b.N; i++ {
								switch mode {
								case "original-constants":
									postInv, _, err := inverseAffine(r.Post.Values)
									if err != nil {
										b.Fatal(err)
									}
									preInv, _, err := inverseAffine(r.Pre.Values)
									if err != nil {
										b.Fatal(err)
									}
									inverter1531Constants = inverseConstants{preInverse: preInv, postInverse: postInv}
								case "prepared-construction":
									p, e := r.PrepareInverse(opts)
									if e != nil {
										b.Fatal(e)
									}
									if e := p.Close(); e != nil {
										b.Fatal(e)
									}
								case "original-total", "current-total":
									out := make([]InverseResult, len(points))
									for j, source := range points {
										if mode == "original-total" {
											out[j], e = r.invert1531Original(source, opts)
										} else {
											out[j], e = r.InvertPoint(source, opts)
										}
										if e != nil {
											b.Fatal(e)
										}
									}
									inverter1531Results = out
								case "prepared-evaluation":
									inverter1531Results, e = prepared.InvertPoints(context.Background(), points)
									if e != nil {
										b.Fatal(e)
									}
								case "prepared-total":
									p, e := r.PrepareInverse(opts)
									if e != nil {
										b.Fatal(e)
									}
									inverter1531Results, e = p.InvertPoints(context.Background(), points)
									closeErr := p.Close()
									if e != nil || closeErr != nil {
										b.Fatalf("batch%v close%v", e, closeErr)
									}
								case "prepared-point-evaluation", "prepared-point-total":
									query := prepared
									if mode == "prepared-point-total" {
										query, e = r.PrepareInverse(opts)
										if e != nil {
											b.Fatal(e)
										}
									}
									out := make([]InverseResult, len(points))
									for j, source := range points {
										out[j], e = query.InvertPoint(context.Background(), source)
										if e != nil {
											b.Fatal(e)
										}
									}
									if mode == "prepared-point-total" {
										if e := query.Close(); e != nil {
											b.Fatal(e)
										}
									}
									inverter1531Results = out

								}
							}
							b.StopTimer()
							resultPayload := uint64(0)
							if mode != "original-constants" && mode != "prepared-construction" {
								resultPayload = output
								if len(inverter1531Results) != len(points) {
									b.Fatal("output count")
								}
								for j, source := range points {
									want, err := r.invert1531Original(source, opts)
									if err != nil || inverter1531Results[j] != want {
										b.Fatalf("cost oracle point%d err%v", j, err)
									}
								}
							}
							b.ReportMetric(float64(retained), "snapshot-payload-B")
							b.ReportMetric(float64(resultPayload), "result-payload-B")
						})
					}
				})
			}
		}
	}
}
