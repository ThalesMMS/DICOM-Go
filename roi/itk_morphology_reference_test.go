package roi_test

import (
	"context"
	"math"
	"slices"
	"testing"

	"github.com/ThalesMMS/dicom-go/roi"
)

// Opening and closing are exercised independently in ITK's binary
// morphology suite. With irregular physical coordinates, index radius and
// millimetre radius can select different neighbours.
func TestITKReferenceBinaryOpeningAndClosingOnIrregularAxis(t *testing.T) {
	type morphology func(*roi.VolumeMask, context.Context, float64) (*roi.VolumeMask, error)
	for _, tc := range []struct {
		name     string
		apply    morphology
		in, want string
		radius   float64
	}{
		{"open-zero-radius", (*roi.VolumeMask).Open, ".#.###.", ".#.###.", 0},
		{"close-zero-radius", (*roi.VolumeMask).Close, ".#.###.", ".#.###.", 0},
		{"open-one-mm-removes-near-background", (*roi.VolumeMask).Open, ".#.###.", "...##..", 1},
		{"open-two-mm-removes-entire-foreground", (*roi.VolumeMask).Open, ".#.###.", ".......", 2},
		{"close-below-first-neighbour-distance", (*roi.VolumeMask).Close, ".#.###.", ".#.###.", math.Nextafter(1, 0)},
		{"close-at-first-neighbour-distance", (*roi.VolumeMask).Close, ".#.###.", "#######", 1},
		{"close-preserves-wider-physical-gap", (*roi.VolumeMask).Close, "##...##", "##...##", 2},
		{"open-full-grid", (*roi.VolumeMask).Open, "#######", "#######", 20},
		{"close-empty-grid", (*roi.VolumeMask).Close, ".......", ".......", 20},
	} {
		t.Run(tc.name, func(t *testing.T) {
			for _, axis := range []string{"column", "slice"} {
				t.Run(axis, func(t *testing.T) {
					coords := []float64{70, 71, 74, 75, 76, 80, 81}
					spacing := [3]float64{1, 1, 1}
					var mask *roi.VolumeMask
					if axis == "column" {
						mask = roi.NewVolumeMask(len(coords), 1, 1, spacing)
						mask.X = coords
					} else {
						mask = roi.NewVolumeMask(1, 1, len(coords), spacing)
						mask.Z = coords
					}
					for i := range tc.in {
						mask.Voxels[i] = tc.in[i] == '#'
					}
					result, err := tc.apply(mask, context.Background(), tc.radius)
					if err != nil {
						t.Fatal(err)
					}
					if result.Columns != mask.Columns || result.Rows != mask.Rows || result.Slices != mask.Slices {
						t.Fatalf("morphology changed the grid to %dx%dx%d", result.Columns, result.Rows, result.Slices)
					}
					if !slices.Equal(result.X, mask.X) || !slices.Equal(result.Y, mask.Y) || !slices.Equal(result.Z, mask.Z) {
						t.Fatalf("morphology changed grid coordinates to %v %v %v", result.X, result.Y, result.Z)
					}
					for i := range tc.want {
						if result.Voxels[i] != (tc.want[i] == '#') {
							t.Errorf("output at %g mm = %t, want %t", coords[i], result.Voxels[i], tc.want[i] == '#')
						}
					}
					result.Voxels[0] = !result.Voxels[0]
					coordinate := mask.X[0]
					result.X[0]--
					if mask.X[0] != coordinate {
						t.Errorf("editing output coordinates changed the source to %g", mask.X[0])
					}
					for i := range tc.in {
						if mask.Voxels[i] != (tc.in[i] == '#') {
							t.Errorf("source changed at %g mm", coords[i])
						}
					}
				})
			}
		})
	}
}
