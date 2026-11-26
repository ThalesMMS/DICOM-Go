package mesh

import (
	"math"
	"testing"
)

// The merge grid's cell index converted floats beyond the int64 range,
// which Go leaves to the architecture (math.MinInt64 on amd64, saturated on
// arm64). floorToInt64 fixes them to math.MinInt64, as the Rust port's
// fmath::f64_to_i64 does.
func TestFloorToInt64IsTheSameOnEveryArchitecture(t *testing.T) {
	for _, tc := range []struct {
		x    float64
		want int64
	}{
		{2.5, 2},
		{-2.5, -3},
		{-0.0, 0},
		{0x1p63 - 1024, math.MaxInt64 - 1023}, // the largest float below 2^63
		{-0x1p63, math.MinInt64},
		{0x1p63, math.MinInt64},
		{1e300, math.MinInt64},
		{-1e300, math.MinInt64},
		{math.MaxFloat64, math.MinInt64},
		{-math.MaxFloat64, math.MinInt64},
		{math.Inf(1), math.MinInt64},
		{math.Inf(-1), math.MinInt64},
		{math.NaN(), math.MinInt64},
	} {
		if got := floorToInt64(tc.x); got != tc.want {
			t.Errorf("floorToInt64(%v) = %d, want %d", tc.x, got, tc.want)
		}
	}
}

// Vertices whose cells fall outside the int64 range still merge only with
// vertices within the tolerance.
func TestCleanMergesVerticesBeyondTheCellRange(t *testing.T) {
	inf := float32(math.Inf(1))
	nan := float32(math.NaN())
	m := &Mesh{
		Vertices: [][3]float32{
			{3e38, 0, 0}, {-3e38, 0, 0}, {3e38, 0, 0}, {-3e38, 0, 0},
			{0, 0, 0}, {inf, 0, 0}, {nan, 0, 0}, {0, 1, 0},
		},
		Triangles: [][3]uint32{{0, 1, 4}, {2, 3, 7}, {5, 6, 4}},
	}
	representative := mergeVertices(m.Vertices, 1e-30)
	want := []uint32{0, 1, 0, 1, 4, 5, 6, 7}
	for i := range want {
		if representative[i] != want[i] {
			t.Fatalf("representatives %v, want %v", representative, want)
		}
	}
	_, report := Clean(m, 1e-30)
	if report.MergedVertices != 2 || report.DuplicateTriangles != 0 {
		t.Fatalf("report %+v", report)
	}
}
