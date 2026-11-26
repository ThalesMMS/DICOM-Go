package imagefilter

import "testing"

// Explicit benchmark campaign only: these do not run in ordinary tests.
// Forced selection is the candidate's exact scalar baseline; direct sort is
// independently authored. ReportAllocs is allocation traffic, not process RSS.
func BenchmarkMaskedRank(b *testing.B) {
	for _, dims := range [][3]int{{128, 128, 1}, {32, 32, 12}} {
		for _, fraction := range []bool{false, true} {
			for _, shape := range []RankNeighborhood{PhysicalBox, PhysicalEllipsoid} {
				for _, border := range []RankBoundary{ClipBoundary, ReplicateBoundary} {
					src := rankFixture(dims, fraction)
					mask := make([]bool, len(src.Data))
					for i := range mask {
						mask[i] = i%7 != 0
					}
					o := MaskedRankOptions{Rank: .5, RadiusMM: [3]float64{1.5, 2, 1.5}, Neighborhood: shape, Boundary: border, Workers: 1}
					name := benchmarkRankName(dims, fraction, shape, border)
					for _, p := range []struct {
						name string
						path rankPath
					}{{"candidate", rankPathAuto}, {"selection", rankPathSelect}} {
						b.Run(name+"/"+p.name, func(b *testing.B) {
							b.ReportAllocs()
							b.SetBytes(int64(len(src.Data)) * 4)
							for i := 0; i < b.N; i++ {
								if _, err := maskedRank(nil, src, mask, RankRegion{Size: dims}, o, p.path); err != nil {
									b.Fatal(err)
								}
							}
						})
					}
					b.Run(name+"/direct-sort", func(b *testing.B) {
						b.ReportAllocs()
						b.SetBytes(int64(len(src.Data)) * 4)
						for i := 0; i < b.N; i++ {
							rankDirect(src, mask, RankRegion{Size: dims}, o)
						}
					})
				}
			}
		}
	}
}
func benchmarkRankName(d [3]int, f bool, s RankNeighborhood, b RankBoundary) string {
	dimension := "2D"
	if d[2] > 1 {
		dimension = "3D"
	}
	family := "integer"
	if f {
		family = "fractional"
	}
	shape := "box"
	if s == PhysicalEllipsoid {
		shape = "ellipsoid"
	}
	boundary := "clip"
	if b == ReplicateBoundary {
		boundary = "replicate"
	}
	return dimension + "/" + family + "/" + shape + "/" + boundary
}
