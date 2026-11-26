package dsa

import (
	"errors"
	"math"
	"slices"
	"testing"
)

func TestApplyRigidRejectsNonFiniteGeometry(t *testing.T) {
	for _, rigid := range []Rigid{
		{Angle: math.NaN()},
		{TX: math.Inf(1)},
		{TY: math.Inf(-1)},
		{Center: [2]float64{0, math.NaN()}},
	} {
		got, err := ApplyRigid([]float64{10}, 1, 1, rigid)
		if got != nil || !errors.Is(err, ErrMotion) {
			t.Errorf("ApplyRigid(%+v) = %v, %v, want nil and %v", rigid, got, err, ErrMotion)
		}
	}
}

func TestApplyFieldRejectsNonFiniteDisplacement(t *testing.T) {
	for _, field := range []DisplacementField{
		{Columns: 2, Rows: 1, DX: []float64{0, math.NaN()}, DY: []float64{0, 0}},
		{Columns: 2, Rows: 1, DX: []float64{0, 0}, DY: []float64{0, math.Inf(-1)}},
	} {
		got, err := ApplyField([]float64{10, 20}, 2, 1, field)
		if got != nil || !errors.Is(err, ErrMotion) {
			t.Errorf("ApplyField(%+v) = %v, %v, want nil and %v", field, got, err, ErrMotion)
		}
	}
}

func TestApplyFieldKeepsFiniteDisplacementSampling(t *testing.T) {
	mask := []float64{10, 20, 30}
	field := DisplacementField{
		Columns: 3, Rows: 1,
		DX: []float64{math.MaxFloat64, 0.5, -math.MaxFloat64},
		DY: []float64{0, 0, 0},
	}
	got, err := ApplyField(mask, 3, 1, field)
	if err != nil || !slices.Equal(got, []float64{30, 25, 10}) {
		t.Fatalf("ApplyField = %v, %v, want [30 25 10]", got, err)
	}
}

func TestRegisterRigidRejectsNonFiniteInitialShift(t *testing.T) {
	const cols, rows = 16, 16
	frame := make([]float64, cols*rows)
	options := RigidOptions{Initial: Shift{Row: math.NaN()}}
	if _, err := RegisterRigid(frame, frame, cols, rows, options); !errors.Is(err, ErrMotion) || !errors.Is(err, ErrInvalidPixelShift) {
		t.Errorf("RegisterRigid error = %v, want %v and %v", err, ErrMotion, ErrInvalidPixelShift)
	}
}
