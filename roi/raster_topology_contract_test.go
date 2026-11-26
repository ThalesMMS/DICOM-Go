package roi

import (
	"context"
	"image"
	"slices"
	"testing"
)

func TestRasterContoursPreserveDiagonalComponentsAndHoleWinding(t *testing.T) {
	for _, tc := range []struct {
		name      string
		pattern   []string
		wantAreas []float64
	}{
		{"downward diagonal contact", []string{"##..", "##..", "..#.", "...."}, []float64{1, 4}},
		{"upward diagonal contact", []string{"..#.", ".#..", "#..."}, []float64{1, 1, 1}},
		{"hole with an island", []string{"#######", "#.....#", "#..#..#", "#.....#", "#######"}, []float64{-15, 1, 35}},
		{"one region with two holes at the canvas border", []string{"######", "#.##.#", "######"}, []float64{-1, -1, 18}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			mask := contractRasterPattern(tc.pattern)
			contours := mask.Contours()
			gotAreas := make([]float64, 0, len(contours))
			rebuilt := NewRasterMask(mask.Columns, mask.Rows)
			for _, contour := range contours {
				if len(contour) < 4 || contour[0] == contour[len(contour)-1] {
					t.Fatalf("contour is not an implicit closed pixel-edge loop: %v", contour)
				}
				points := Point2DsFromImage(contour)
				gotAreas = append(gotAreas, ClosedPolygonArea(points))
				rebuilt.XOR(RasterizePolygon(points, mask.Columns, mask.Rows))
			}
			slices.Sort(gotAreas)
			if !slices.Equal(gotAreas, tc.wantAreas) {
				t.Fatalf("oriented contour areas = %v, want %v", gotAreas, tc.wantAreas)
			}
			assertRasterMasksEqual(t, rebuilt, mask)
		})
	}
}

func TestRasterizePolygonVisitsClippedPixelsExactlyOnceInRowOrder(t *testing.T) {
	for _, tc := range []struct {
		name    string
		points  []Point2D
		pattern []string
	}{
		{
			name:    "full canvas with both edges outside",
			points:  []Point2D{{-1.5, -2}, {8, -2}, {8, 8}, {-1.5, 8}},
			pattern: []string{"#####", "#####", "#####"},
		},
		{
			name:    "concavity leaves two runs on a scanline",
			points:  []Point2D{{0, 0}, {5, 0}, {5, 3}, {3, 3}, {3, 1}, {1, 1}, {1, 3}, {0, 3}},
			pattern: []string{"#####", "#..##", "#..##"},
		},
		{
			name:    "outside concave notch with repeated closing vertex",
			points:  []Point2D{{-2, -2}, {6, -2}, {6, 3}, {3, 3}, {3, 1}, {1, 1}, {1, 3}, {-2, 3}, {-2, -2}},
			pattern: []string{"####", "#..#", "#..#", "...."},
		},
		{
			name:    "zero area polygon never samples pixels",
			points:  []Point2D{{0, 1}, {2, 1}, {3, 1}, {0, 1}},
			pattern: []string{"....", "....", "...."},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var want []image.Point
			for y, row := range tc.pattern {
				for x, pixel := range row {
					if pixel == '#' {
						want = append(want, image.Pt(x, y))
					}
				}
			}
			points := slices.Clone(tc.points)
			for winding := range 2 {
				var got []image.Point
				mask := RasterizePolygon(points, len(tc.pattern[0]), len(tc.pattern))
				mask.ForEachPixel(func(x, y int) { got = append(got, image.Pt(x, y)) })
				if !slices.Equal(got, want) {
					t.Fatalf("winding %d visited %v, want %v", winding, got, want)
				}
				slices.Reverse(points)
			}
		})
	}
}

func TestVolumeMaskMarginsUseExactAnisotropicBoundaries(t *testing.T) {
	ctx := context.Background()
	t.Run("expansion admits physical face and slice neighbors at equality", func(t *testing.T) {
		seed := NewVolumeMask(5, 5, 5, [3]float64{1, 2, 4})
		seed.Voxels[seed.index(2, 2, 2)] = true
		for _, tc := range []struct {
			name  string
			mm    float64
			count int
		}{
			{"subvoxel radius", 0.75, 1},
			{"column neighbor exactly on limit", 1, 3},
			{"row neighbor exactly on limit", 2, 7},
			{"slice neighbor exactly on limit", 4, 19},
		} {
			t.Run(tc.name, func(t *testing.T) {
				got, err := seed.Expand(ctx, tc.mm)
				if err != nil {
					t.Fatal(err)
				}
				if got.Count() != tc.count || seed.Count() != 1 {
					t.Fatalf("expanded count %d, seed count %d, want %d and 1", got.Count(), seed.Count(), tc.count)
				}
				for z := range seed.Slices {
					for y := range seed.Rows {
						for x := range seed.Columns {
							dx, dy, dz := x-2, 2*(y-2), 4*(z-2)
							want := float64(dx*dx+dy*dy+dz*dz) <= tc.mm*tc.mm
							if got.Voxels[got.index(x, y, z)] != want {
								t.Errorf("voxel (%d, %d, %d): want %t", x, y, z, want)
							}
						}
					}
				}
			})
		}
	})
	t.Run("erosion removes voxels at exactly the margin", func(t *testing.T) {
		block := NewVolumeMask(5, 5, 5, [3]float64{0.5, 2, 3})
		for z := 1; z <= 3; z++ {
			for y := 1; y <= 3; y++ {
				for x := 1; x <= 3; x++ {
					block.Voxels[block.index(x, y, z)] = true
				}
			}
		}
		got, err := block.Shrink(ctx, 0.5)
		if err != nil {
			t.Fatal(err)
		}
		if got.Count() != 9 {
			t.Fatalf("erosion retained %d voxels, want 9", got.Count())
		}
		for z := 1; z <= 3; z++ {
			for y := 1; y <= 3; y++ {
				if !got.Voxels[got.index(2, y, z)] {
					t.Errorf("central column voxel (2, %d, %d) is missing", y, z)
				}
			}
		}
		gone, err := block.Shrink(ctx, 1)
		if err != nil || gone.Count() != 0 || block.Count() != 27 {
			t.Fatalf("erosion by 1 mm returned %+v, %v; source count %d", gone, err, block.Count())
		}
	})
}

func TestVolumeMaskFillHolesUsesFaceConnectedExterior(t *testing.T) {
	for _, tc := range []struct {
		name       string
		background [][3]int
		outside    [][3]int
	}{
		{"diagonal contact to a border leaves the interior enclosed", [][3]int{{1, 1, 1}, {0, 0, 0}}, [][3]int{{0, 0, 0}}},
		{"face contact to a border keeps the channel open", [][3]int{{1, 1, 1}, {0, 1, 1}}, [][3]int{{1, 1, 1}, {0, 1, 1}}},
		{"axial channel is open across otherwise enclosed slices", [][3]int{{2, 2, 0}, {2, 2, 1}, {2, 2, 2}, {2, 2, 3}, {2, 2, 4}}, [][3]int{{2, 2, 0}, {2, 2, 1}, {2, 2, 2}, {2, 2, 3}, {2, 2, 4}}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			m := NewVolumeMask(5, 5, 5, [3]float64{1, 1, 3})
			for i := range m.Voxels {
				m.Voxels[i] = true
			}
			for _, p := range tc.background {
				m.Voxels[m.index(p[0], p[1], p[2])] = false
			}
			got, err := m.FillHoles(context.Background())
			if err != nil {
				t.Fatal(err)
			}
			if got.Count() != 125-len(tc.outside) || m.Count() != 125-len(tc.background) {
				t.Fatalf("filled count = %d, source count = %d", got.Count(), m.Count())
			}
			for _, p := range tc.outside {
				if got.Voxels[got.index(p[0], p[1], p[2])] {
					t.Errorf("exterior voxel %v was filled", p)
				}
			}
		})
	}
}

func contractRasterPattern(pattern []string) *RasterMask {
	mask := NewRasterMask(len(pattern[0]), len(pattern))
	for y, row := range pattern {
		for x, pixel := range row {
			if pixel == '#' {
				mask.Set(x, y, true)
			}
		}
	}
	return mask
}
