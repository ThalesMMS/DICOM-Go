package render

import (
	"errors"
	"math"
	"slices"
	"testing"
)

func TestVolumeBrickSampleStencil(t *testing.T) {
	grid := VolumeBrickGrid{Dimensions: [3]uint32{8, 8, 8}, BrickSize: [3]uint32{4, 4, 4}}
	for _, tc := range []struct {
		name     string
		index    [3]float32
		gradient bool
		want     []int
	}{
		{"interior", [3]float32{1, 1, 1}, false, []int{0}},
		{"corner", [3]float32{3.5, 3.5, 3.5}, false, []int{0, 1, 2, 3, 4, 5, 6, 7}},
		{"gradient crosses boundary", [3]float32{2.5, 2.5, 2.5}, true, []int{0, 1, 2, 3, 4, 5, 6, 7}},
		{"lower border", [3]float32{-100, -100, -100}, true, []int{0}},
		{"upper border", [3]float32{100, 100, 100}, true, []int{7}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, err := VolumeBrickSampleStencil(grid, tc.index, tc.gradient)
			if err != nil || !slices.Equal(got, tc.want) {
				t.Fatalf("stencil=%v err=%v want=%v", got, err, tc.want)
			}
		})
	}
	grid.BrickSize = [3]uint32{1, 1, 1}
	got, err := VolumeBrickSampleStencil(grid, [3]float32{3.5, 3.5, 3.5}, true)
	if err != nil || len(got) != 64 {
		t.Fatalf("maximal stencil=%d err=%v", len(got), err)
	}
	if _, err := VolumeBrickSampleStencil(grid, [3]float32{float32(math.NaN()), 0, 0}, true); !errors.Is(err, ErrInvalidVolumeBricks) {
		t.Fatal(err)
	}
}

func TestVolumeBrickBatchBudget(t *testing.T) {
	budget := VolumeBrickBatchBudget{MaxPasses: 2, MaxUploadBytes: 10}
	var usage VolumeBrickBatchUsage
	if err := usage.Charge(budget, 6); err != nil {
		t.Fatal(err)
	}
	before := usage
	if err := usage.Charge(budget, 5); !errors.Is(err, ErrVolumeBrickBudget) || usage != before {
		t.Fatalf("upload rejection: %+v %v", usage, err)
	}
	if err := usage.Charge(budget, 4); err != nil {
		t.Fatal(err)
	}
	before = usage
	if err := usage.Charge(budget, 0); !errors.Is(err, ErrVolumeBrickBudget) || usage != before {
		t.Fatalf("pass rejection: %+v %v", usage, err)
	}
	usage = VolumeBrickBatchUsage{UploadBytes: math.MaxUint64}
	if err := usage.Charge(VolumeBrickBatchBudget{MaxPasses: 1, MaxUploadBytes: math.MaxUint64}, 1); !errors.Is(err, ErrVolumeBrickBudget) {
		t.Fatal(err)
	}
}
