package roi

import (
	"context"
	"errors"
	"testing"

	"github.com/ThalesMMS/dicom-go/render"
)

// mappedGrid is an axis-aligned grid of 1 mm voxels at origin.
func mappedGrid(columns, rows, slices int, origin render.Vec3) Grid {
	return Grid{
		Origin: origin, RowDir: render.Vec3{X: 1}, ColDir: render.Vec3{Y: 1}, SliceDir: render.Vec3{Z: 1},
		Columns: columns, Rows: rows, Slices: slices, ColSpacing: 1, RowSpacing: 1, SliceSpacing: 1,
	}
}

// quarterTurn maps a target point into the source frame by a 90° turn
// about z followed by a shift: (x, y, z) → (y + 2, −x + 19, z − 1).
func quarterTurn(p render.Vec3) (render.Vec3, error) {
	return render.Vec3{X: p.Y + 2, Y: -p.X + 19, Z: p.Z - 1}, nil
}

// TestMappedLabelResamplingFollowsASyntheticTransform: nearest labels read
// through a rotation and shift land exactly where the analytic transform
// puts them, voxels the mapper refuses or maps outside stay uncovered, and
// the Gaussian mode keeps the interior of a large label.
func TestMappedLabelResamplingFollowsASyntheticTransform(t *testing.T) {
	source, err := NewLabelVolume(mappedGrid(20, 20, 8, render.Vec3{}))
	if err != nil {
		t.Fatal(err)
	}
	label := func(x, y, z int) uint16 {
		switch {
		case x >= 3 && x < 9 && y >= 4 && y < 12 && z >= 2 && z < 6:
			return 1
		case x >= 12 && x < 17 && y >= 10 && y < 15:
			return 2
		}
		return 0
	}
	for z := 0; z < 8; z++ {
		for y := 0; y < 20; y++ {
			for x := 0; x < 20; x++ {
				source.Set(x, y, z, label(x, y, z))
			}
		}
	}
	target := mappedGrid(22, 22, 8, render.Vec3{})
	result, err := ResampleLabels(context.Background(), source, target, LabelResampleMode{Mapper: quarterTurn})
	if err != nil {
		t.Fatal(err)
	}
	for z := 0; z < 8; z++ {
		for y := 0; y < 22; y++ {
			for x := 0; x < 22; x++ {
				sx, sy, sz := y+2, -x+19, z-1
				inside := sx >= 0 && sx < 20 && sy >= 0 && sy < 20 && sz >= 0 && sz < 8
				if result.Coverage.Covered(x, y, z) != inside {
					t.Fatalf("coverage at (%d, %d, %d) = %v", x, y, z, !inside)
				}
				want := uint16(0)
				if inside {
					want = label(sx, sy, sz)
				}
				if got := result.Volume.LabelAt(x, y, z); got != want {
					t.Fatalf("label at (%d, %d, %d) = %d, want %d", x, y, z, got, want)
				}
			}
		}
	}
	if result.SourceLabels != nil || result.Vanished != nil {
		t.Fatalf("mapped report lists source labels %v, vanished %v", result.SourceLabels, result.Vanished)
	}
	// A mapper that refuses half the plane leaves it uncovered.
	refusing := func(p render.Vec3) (render.Vec3, error) {
		if p.X < 11 {
			return render.Vec3{}, errors.New("outside the registration domain")
		}
		return quarterTurn(p)
	}
	masks, err := ResampleLabelMasks(context.Background(), source, target, LabelResampleMode{Mapper: refusing})
	if err != nil {
		t.Fatal(err)
	}
	for y := 0; y < 22; y++ {
		for x := 0; x < 11; x++ {
			if masks.Coverage.Covered(x, y, 3) {
				t.Fatalf("refused voxel (%d, %d) covered", x, y)
			}
		}
	}
	// Label Gaussian through the mapper keeps the interior of label 1.
	gaussian := LabelGaussian(0.8)
	gaussian.Mapper = quarterTurn
	smooth, err := ResampleLabels(context.Background(), source, target, gaussian)
	if err != nil {
		t.Fatal(err)
	}
	// Source (5, 8, 3) is the interior of label 1: target (11, 3, 4).
	if got := smooth.Volume.LabelAt(11, 3, 4); got != 1 {
		t.Fatalf("Gaussian label at the interior = %d", got)
	}
	if gaussian.String() != "label Gaussian σ=0.8 mm through a registration" {
		t.Fatalf("mode %q", gaussian.String())
	}
}

// TestMappedFractionResamplingFollowsASyntheticTransform: a linear ramp read
// through the transform matches its analytic value.
func TestMappedFractionResamplingFollowsASyntheticTransform(t *testing.T) {
	slices := make([][]uint8, 8)
	for z := range slices {
		slices[z] = make([]uint8, 20*20)
		for i := range slices[z] {
			slices[z][i] = uint8(10 * (i % 20)) // 10·x
		}
	}
	source, err := NewFractionStack(mappedGrid(20, 20, 8, render.Vec3{}), slices)
	if err != nil {
		t.Fatal(err)
	}
	target := mappedGrid(22, 22, 8, render.Vec3{})
	result, err := ResampleFractions(context.Background(), source, target, FractionResampleOptions{Mapper: quarterTurn})
	if err != nil {
		t.Fatal(err)
	}
	// Target (x, y, z) reads source x = y + 2.
	for _, p := range [][3]int{{5, 3, 4}, {10, 12, 2}, {18, 0, 6}} {
		want := float32(10 * (p[1] + 2))
		if got := result.Values[(p[2]*22+p[1])*22+p[0]]; got != want {
			t.Fatalf("fraction at %v = %g, want %g", p, got, want)
		}
	}
	mask, err := ResampleFractionMask(context.Background(), source, target, 100, FractionResampleOptions{Mapper: quarterTurn})
	if err != nil {
		t.Fatal(err)
	}
	// Row 8 reads source x = 10 (100), row 7 reads x = 9 (90).
	if !mask.Masks[4].Get(5, 8) || mask.Masks[4].Get(5, 7) {
		t.Fatal("thresholded mapped fractions")
	}
}
