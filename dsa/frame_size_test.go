package dsa

import (
	"errors"
	"math"
	"testing"
)

func TestMotionRejectsOverflowingFrameSize(t *testing.T) {
	// Multiplying these dimensions as int wraps to zero on 32- and 64-bit
	// hosts, so empty buffers used to pass the shape check.
	const columns, rows = math.MaxInt/16 + 1, 32
	if _, err := PhaseCorrelate(nil, nil, columns, rows, PhaseCorrelationOptions{}); !errors.Is(err, ErrFrameSizeMismatch) {
		t.Errorf("PhaseCorrelate error = %v, want %v", err, ErrFrameSizeMismatch)
	}
	if _, err := RegisterRigid(nil, nil, columns, rows, RigidOptions{}); !errors.Is(err, ErrMotion) {
		t.Errorf("RegisterRigid error = %v, want %v", err, ErrMotion)
	}
	if _, err := ApplyRigid(nil, columns, rows, Rigid{}); !errors.Is(err, ErrMotion) {
		t.Errorf("ApplyRigid error = %v, want %v", err, ErrMotion)
	}
	if _, err := GradientDifference(nil, nil, columns, rows); !errors.Is(err, ErrMotion) {
		t.Errorf("GradientDifference error = %v, want %v", err, ErrMotion)
	}
	if _, err := MatchBlocks(nil, nil, columns, rows, BlockOptions{}); !errors.Is(err, ErrMotion) {
		t.Errorf("MatchBlocks error = %v, want %v", err, ErrMotion)
	}
	if _, err := ApplyField(nil, columns, rows, DisplacementField{Columns: columns, Rows: rows}); !errors.Is(err, ErrMotion) {
		t.Errorf("ApplyField error = %v, want %v", err, ErrMotion)
	}
}

func TestMatchBlocksRejectsOverflowingSearchArea(t *testing.T) {
	const columns, rows = 32, 32
	frame := make([]float64, columns*rows)
	for _, options := range []BlockOptions{
		{BlockSize: math.MaxInt, SearchRadius: 1},
		{BlockSize: 4, SearchRadius: math.MaxInt},
	} {
		if _, err := MatchBlocks(frame, frame, columns, rows, options); !errors.Is(err, ErrMotion) {
			t.Errorf("MatchBlocks(%+v) error = %v, want %v", options, err, ErrMotion)
		}
	}
}

func TestPhaseCorrelationClampsSearchBoundBeforeIntegerConversion(t *testing.T) {
	if got := searchLimit(math.MaxFloat64, 64, 64); got != 16 {
		t.Errorf("search limit = %d, want 16", got)
	}
}
