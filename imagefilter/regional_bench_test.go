package imagefilter

import (
	"context"
	"fmt"
	"math"
	"testing"
)

type regionalBenchIO struct {
	volume    *Volume
	requested Region
	output    []float32
	bytes     int64
}

func (s *regionalBenchIO) ReadRegion(c context.Context, r Region, d []float32) error {
	s.bytes += int64(len(d)) * 4
	return s.volume.ReadRegion(c, r, d)
}
func (s *regionalBenchIO) ReadSlices(c context.Context, z0, z1 int, d []float32) error {
	s.bytes += int64(len(d)) * 4
	return s.volume.ReadSlices(c, z0, z1, d)
}
func (s *regionalBenchIO) WriteRegion(c context.Context, r Region, d []float32) error {
	for z := 0; z < r.Size[2]; z++ {
		for y := 0; y < r.Size[1]; y++ {
			for x := 0; x < r.Size[0]; x++ {
				gx, gy, gz := x+r.Origin[0], y+r.Origin[1], z+r.Origin[2]
				if regionalContains(s.requested, gx, gy, gz) {
					i := ((gz-s.requested.Origin[2])*s.requested.Size[1]+gy-s.requested.Origin[1])*s.requested.Size[0] + gx - s.requested.Origin[0]
					s.output[i] = d[(z*r.Size[1]+y)*r.Size[0]+x]
				}
			}
		}
	}
	return c.Err()
}
func (s *regionalBenchIO) WriteSlices(c context.Context, z0 int, d []float32) error {
	r := Region{Origin: [3]int{0, 0, z0}, Size: [3]int{s.volume.Dims[0], s.volume.Dims[1], len(d) / (s.volume.Dims[0] * s.volume.Dims[1])}}
	return s.WriteRegion(c, r, d)
}
func BenchmarkRegionalSmallROI(b *testing.B) {
	dims := [3]int{1024, 64, 2}
	spacing := [3]float64{.7, 1.3, 2.1}
	roi := Region{Origin: [3]int{500, 25, 0}, Size: [3]int{24, 12, 2}}
	n, _ := voxelCount(roi.Size)
	v := NewVolume(dims, spacing)
	for i := range v.Data {
		v.Data[i] = float32((i*17+i/1024*11)%127) - 63
	}
	v.Data[v.Index(501, 26, 0)] = 200
	filters := []struct {
		name   string
		filter SlabFilter
	}{{"median", MedianOptions{RadiusMM: [3]float64{.7, 1.3, 0}, Workers: 2}}, {"bilateral", BilateralOptions{SigmaMM: [3]float64{.3, .6, 0}, RangeSigma: 10, Workers: 2}}, {"nlm", NonLocalMeansOptions{NoiseSigma: 5, PatchRadiusMM: [3]float64{.7, 0, 0}, SearchRadiusMM: [3]float64{.7, 0, 0}, BlockStep: 2, Workers: 2}}}
	for _, f := range filters {
		reference := &regionalBenchIO{volume: v, requested: roi, output: make([]float32, n)}
		if _, e := FilterSlabs(context.Background(), f.filter, reference, dims, spacing, reference, SlabOptions{MaxWorkingBytes: 4 << 20}); e != nil {
			b.Fatal(e)
		}
		for _, mode := range []string{"slabs", "regions"} {
			b.Run(fmt.Sprintf("%s/%s", f.name, mode), func(b *testing.B) {
				io := &regionalBenchIO{volume: v, requested: roi, output: make([]float32, n)}
				run := func() error {
					io.bytes = 0
					if mode == "slabs" {
						_, e := FilterSlabs(context.Background(), f.filter, io, dims, spacing, io, SlabOptions{MaxWorkingBytes: 4 << 20})
						return e
					}
					_, e := FilterRegions(context.Background(), f.filter, io, dims, spacing, roi, io, RegionalOptions{MaxWorkingBytes: 1 << 20, TileSize: [3]int{12, 6, 1}})
					return e
				}
				if e := run(); e != nil {
					b.Fatal(e)
				}
				for i, x := range io.output {
					if math.Float32bits(x) != math.Float32bits(reference.output[i]) {
						b.Fatalf("whole-output preflight mismatch %d", i)
					}
				}
				b.Logf("matched ROI %v values%d; external source/output arrays excluded from working bound", roi, n)
				b.ReportAllocs()
				b.ResetTimer()
				bytes := int64(0)
				for i := 0; i < b.N; i++ {
					if e := run(); e != nil {
						b.Fatal(e)
					}
					bytes += io.bytes
				}
				b.StopTimer()
				b.ReportMetric(float64(bytes)/float64(b.N), "read-B/op")
			})
		}
	}
}
