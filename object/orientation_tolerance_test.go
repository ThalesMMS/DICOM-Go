package object

import (
	"math"
	"testing"
)

func TestImageOrientationDeviation(t *testing.T) {
	for _, tc := range []struct {
		name string
		iop  []float64
		want float64
		ok   bool
	}{
		{"axial", []float64{1, 0, 0, 0, 1, 0}, 0, true},
		{"stretched row", []float64{1.002, 0, 0, 0, 1, 0}, 0.002, true},
		{"short column", []float64{1, 0, 0, 0, 0.9995, 0}, 0.0005, true},
		{"skewed", []float64{1, 0, 0, 0.003, 1, 0}, 0.003, true},
		{"too few values", []float64{1, 0, 0, 0, 1}, 0, false},
		{"NaN", []float64{1, 0, 0, 0, math.NaN(), 0}, 0, false},
	} {
		got, ok := ImageOrientationDeviation(tc.iop)
		if ok != tc.ok || math.Abs(got-tc.want) > 1e-6 {
			t.Errorf("%s: %v %v, want about %v %v", tc.name, got, ok, tc.want, tc.ok)
		}
	}
}
