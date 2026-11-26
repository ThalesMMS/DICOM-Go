package roi

import (
	"context"
	"math"
	"testing"

	"github.com/ThalesMMS/dicom-go/render"
)

// A target shifted by whole voxels in the source basis is a pure index
// translation, even when a source row or slice crosses the target boundary.
func TestTranslateMaskWithinPatientGrid(t *testing.T) {
	const columns, rows, depth = 7, 5, 4
	const label uint16 = 13
	var masks [depth]*RasterMask
	masks[0] = NewRasterMask(columns, rows)
	masks[0].SetRun(0, 0, 4)
	masks[0].SetRun(3, 5, 7)
	masks[2] = NewRasterMask(columns, rows)
	masks[2].SetRun(4, 0, 2)
	masks[3] = NewRasterMask(columns, rows)
	masks[3].SetRun(2, 2, 6)
	fixtureSet := func(x, y, z int) bool {
		switch z {
		case 0:
			return (y == 0 && x >= 0 && x < 4) || (y == 3 && x >= 5 && x < 7)
		case 2:
			return y == 4 && x >= 0 && x < 2
		case 3:
			return y == 2 && x >= 2 && x < 6
		}
		return false
	}
	spacing := [3]float64{0.75, 2, 3.5}
	landmark := render.Vec3{X: 79.25, Y: -16.5, Z: 147.75}
	for _, basis := range []struct {
		name string
		grid Grid
	}{
		{"axial", axisGrid(columns, rows, depth, spacing, landmark)},
		{"oblique", rotatedGrid(columns, rows, depth, spacing, landmark, math.Pi/6)},
	} {
		t.Run(basis.name, func(t *testing.T) {
			source, err := NewMaskStack(basis.grid, masks[:], label)
			if err != nil {
				t.Fatal(err)
			}
			for _, shift := range []struct {
				name    string
				x, y, z int
			}{
				{"left-bottom-before", -2, 1, -1},
				{"right-top-after", 2, -2, 1},
				{"slice-only", 0, 0, 2},
				{"no-overlap", -7, 0, 0},
			} {
				t.Run(shift.name, func(t *testing.T) {
					target := basis.grid
					target.Origin = target.Origin.
						Add(target.RowDir.Scale(float64(shift.x) * spacing[0])).
						Add(target.ColDir.Scale(float64(shift.y) * spacing[1])).
						Add(target.SliceDir.Scale(float64(shift.z) * spacing[2]))
					got, err := ResampleLabelMasks(context.Background(), source, target, NearestLabels())
					if err != nil {
						t.Fatal(err)
					}
					var totalCovered, totalDrawn int
					for z := range depth {
						var layer *RasterMask
						if layers := got.Masks[label]; layers != nil {
							layer = layers[z]
						}
						var sliceCovered int
						for y := range rows {
							for x := range columns {
								sx, sy, sz := x+shift.x, y+shift.y, z+shift.z
								covered := sx >= 0 && sx < columns && sy >= 0 && sy < rows && sz >= 0 && sz < depth
								drawn := covered && fixtureSet(sx, sy, sz)
								if got.Coverage.Covered(x, y, z) != covered || layer.Get(x, y) != drawn {
									t.Fatalf("target (%d,%d,%d) from (%d,%d,%d): covered=%v drawn=%v, want %v/%v", x, y, z, sx, sy, sz, got.Coverage.Covered(x, y, z), layer.Get(x, y), covered, drawn)
								}
								if masks[z].Get(x, y) != fixtureSet(x, y, z) {
									t.Fatalf("source mask changed at (%d,%d,%d)", x, y, z)
								}
								if covered {
									sliceCovered++
								}
								if drawn {
									totalDrawn++
								}
							}
						}
						if n := got.Coverage.SlicePixels[z]; n != sliceCovered {
							t.Fatalf("slice %d reports %d covered pixels, want %d", z, n, sliceCovered)
						}
						totalCovered += sliceCovered
					}
					if got.Coverage.CoveredVoxels != int64(totalCovered) || got.Counts[label] != totalDrawn || len(got.Vanished) != 0 {
						t.Fatalf("covered=%d counts=%v vanished=%v, want %d covered and %d drawn", got.Coverage.CoveredVoxels, got.Counts, got.Vanished, totalCovered, totalDrawn)
					}
				})
			}
		})
	}
}
