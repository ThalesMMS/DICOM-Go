package dsa

import (
	"errors"
	"math"
	"slices"
	"testing"
)

func TestSubtractModalityRetainsSignedValues(t *testing.T) {
	contrast := []float64{300, 100, -150, 2, 8, 700}
	mask := []float64{0, 4, 8, 20, 24, 28}
	for _, tc := range []struct {
		name  string
		shift Shift
		want  []float64
	}{
		{"unshifted", Shift{}, []float64{300, 96, -158, -18, -16, 672}},
		{"bilinear", Shift{Row: 0.5, Column: 0.5}, []float64{298, 94, -158, -10, -8, 682}},
		{"outside both axes", Shift{Row: math.MaxFloat64, Column: math.MaxFloat64}, []float64{292, 92, -158, -6, 0, 692}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, err := SubtractModality(contrast, mask, 3, 2, tc.shift)
			if err != nil || !slices.Equal(got, tc.want) {
				t.Fatalf("difference = %v, %v, want %v", got, err, tc.want)
			}
			got[0] = 123
			if contrast[0] != 300 || mask[0] != 0 {
				t.Fatal("difference aliases an input frame")
			}
		})
	}
}

func TestSubtractModalityRejectsInvalidInputs(t *testing.T) {
	frame := make([]float64, 4)
	for _, tc := range []struct {
		name          string
		contrast      []float64
		columns, rows int
		shift         Shift
		wantErr       error
	}{
		{"mismatched input lengths", frame[:3], 2, 2, Shift{}, ErrFrameSizeMismatch},
		{"mismatched dimensions", frame, 1, 2, Shift{}, ErrFrameSizeMismatch},
		{"empty grid", frame, 0, 2, Shift{}, ErrFrameSizeMismatch},
		{"overflowing grid", frame, math.MaxInt/2 + 2, 4, Shift{}, ErrFrameSizeMismatch},
		{"invalid shift", frame, 2, 2, Shift{Column: math.NaN()}, ErrInvalidPixelShift},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, err := SubtractModality(tc.contrast, frame, tc.columns, tc.rows, tc.shift)
			if got != nil || !errors.Is(err, tc.wantErr) {
				t.Fatalf("difference = %v, %v, want nil and %v", got, err, tc.wantErr)
			}
		})
	}
}
