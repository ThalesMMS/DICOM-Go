package imagefilter

import (
	"math"
	"testing"
)

func BenchmarkHough2DCirclePhantom(b *testing.B) {
	v, c, _ := houghCirclePhantom(128, [2]float64{.5, .5}, 10, 2*math.Pi)
	o := houghTestCircleOptions(v, c, 10)
	o.CenterMinMM = [2]float64{24, 24}
	o.CenterMaxMM = [2]float64{40, 40}
	o.RadiusMinMM = 8
	o.RadiusMaxMM = 12
	o.MinVotes = 36
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		r, e := HoughCircles2D(nil, v, o)
		if e != nil || len(r.Candidates) == 0 {
			b.Fatal("phantom", e)
		}
		b.ReportMetric(float64(r.Report.WorkingBytes), "reserved-B")
		b.ReportMetric(float64(r.Report.Operations), "work/op")
		b.ReportMetric(float64(r.Report.ReservedOperations), "reserved-work")
	}
}
func BenchmarkHough2DLinesPhantom(b *testing.B) {
	v := NewVolume([3]int{128, 128, 1}, [3]float64{.5, .5, 0})
	for y := 0; y < 128; y++ {
		v.Data[v.Index(64, y, 0)] = 1
	}
	o := houghTestLineOptions(v)
	o.RhoMinMM = -48
	o.RhoMaxMM = 48
	o.MinVotes = 64
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		r, e := HoughLines2D(nil, v, o)
		if e != nil || len(r.Candidates) == 0 {
			b.Fatal("phantom", e)
		}
		b.ReportMetric(float64(r.Report.WorkingBytes), "reserved-B")
		b.ReportMetric(float64(r.Report.Operations), "work/op")
		b.ReportMetric(float64(r.Report.ReservedOperations), "reserved-work")
	}
}
