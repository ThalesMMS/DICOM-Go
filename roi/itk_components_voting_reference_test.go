package roi_test

import (
	"errors"
	"fmt"
	"math"
	"slices"
	"testing"

	"github.com/ThalesMMS/dicom-go/render"
	"github.com/ThalesMMS/dicom-go/roi"
)

// A 3D checkerboard has no face contacts. Edge hops preserve checkerboard
// parity, and connect every foreground site when all three axes have room.
// This exercises the case behind ITK's Checkerboard3D regression.
func TestITKReferenceComponentsOnCheckerboard(t *testing.T) {
	for _, grid := range []struct {
		size       [3]int
		foreground int
	}{
		{[3]int{3, 5, 7}, 53},
		{[3]int{8, 6, 8}, 192},
		{[3]int{1, 1, 7}, 4},
	} {
		for _, connectivity := range []roi.Connectivity3D{roi.Connectivity6, roi.Connectivity18, roi.Connectivity26} {
			t.Run(fmt.Sprintf("%dx%dx%d/%d", grid.size[0], grid.size[1], grid.size[2], connectivity), func(t *testing.T) {
				nx, ny, nz := grid.size[0], grid.size[1], grid.size[2]
				seg := roi.NewSegmentation3D(render.VolumeGeometry{}, nx, ny)
				// Insert slices, rows, and pixels in reverse raster order.
				for z := nz - 1; z >= 0; z-- {
					for y := ny - 1; y >= 0; y-- {
						for x := nx - 1; x >= 0; x-- {
							if (x+y+z)%2 == 0 {
								seg.SetVoxel(x, y, z, true)
							}
						}
					}
				}
				got, err := roi.ConnectedComponents3DWithConnectivity(seg, connectivity)
				if err != nil {
					t.Fatal(err)
				}
				separate := connectivity == roi.Connectivity6 || nx == 1
				wantCount := 1
				if separate {
					wantCount = grid.foreground
				}
				if len(got) != wantCount {
					t.Fatalf("component count %d, want %d", len(got), wantCount)
				}
				if !separate && got[0].VoxelCount() != grid.foreground {
					t.Fatalf("component size %d, want %d", got[0].VoxelCount(), grid.foreground)
				}
				index := 0
				for z := range nz {
					for y := range ny {
						for x := range nx {
							wantForeground := (x+y+z)%2 == 0
							if !separate && got[0].Voxel(x, y, z) != wantForeground {
								t.Errorf("wrong membership at (%d,%d,%d)", x, y, z)
							}
							if separate && wantForeground {
								if component := got[index]; component.VoxelCount() != 1 || !component.Voxel(x, y, z) {
									t.Errorf("component %d should contain only (%d,%d,%d)", index, x, y, z)
								}
								index++
							}
						}
					}
				}
				got[0].SetVoxel(0, 0, 0, false)
				if !seg.Voxel(0, 0, 0) || seg.VoxelCount() != grid.foreground {
					t.Fatal("editing one returned component changed the source")
				}
			})
		}
	}
}

// A near-threshold bridge joins two objects and a third object stays
// separate. ITK tests threshold-search invariance under a large intensity
// offset. The expected component sizes below provide a threshold sweep for
// the Go threshold and labeling primitives.
func TestITKReferenceThresholdComponentSweepPreservesIntensityOffsets(t *testing.T) {
	image := []string{
		"...........",
		".88....55..",
		".88222255..",
		"...........",
		"....999....",
	}
	for _, offset := range []float64{-3e9, 0, 3e9} {
		for _, tc := range []struct {
			name  string
			level float64
			above bool
			sizes []int
		}{
			{"at bridge", 2, false, []int{12, 3}},
			{"above bridge", 2, true, []int{4, 4, 3}},
			{"at smaller lobe", 5, false, []int{4, 4, 3}},
			{"above smaller lobe", 5, true, []int{4, 3}},
			{"at maximum", 9, false, []int{3}},
			{"above maximum", 9, true, nil},
		} {
			t.Run(fmt.Sprintf("offset_%g/%s", offset, tc.name), func(t *testing.T) {
				lower := offset + tc.level
				if tc.above {
					lower = math.Nextafter(lower, math.Inf(1))
				}
				mask := roi.ThresholdMask(len(image[0]), len(image), lower, math.Inf(1), func(x, y int) (float64, bool) {
					if image[y][x] == '.' {
						return offset - 10, true
					}
					return offset + float64(image[y][x]-'0'), true
				})
				var sizes []int
				for _, component := range roi.ConnectedComponents(mask, false) {
					sizes = append(sizes, component.Count())
				}
				if !slices.Equal(sizes, tc.sizes) {
					t.Fatalf("component sizes at threshold %.17g = %v, want %v", lower, sizes, tc.sizes)
				}
			})
		}
	}
}

// ITK's LabelVoting tests mix majorities, ties, and default or explicit
// undecided labels. Go returns ints, so a uint16 maximum can also elect a
// default undecided value of 65536.
func TestITKReferenceLabelVoting(t *testing.T) {
	for _, tc := range []struct {
		name      string
		readers   [][]uint16
		undecided int
		want      []int
	}{
		{
			name:      "two readers tie only where they disagree",
			readers:   [][]uint16{{1, 7, 0, 3}, {1, 0, 0, 7}},
			undecided: -1,
			want:      []int{1, 8, 0, 8},
		},
		{
			name:      "three readers can resolve a tie or introduce one",
			readers:   [][]uint16{{1, 7, 0, 3}, {1, 0, 0, 7}, {0, 7, 8, 8}},
			undecided: -1,
			want:      []int{1, 7, 0, 9},
		},
		{
			name:      "explicit zero is also a background label",
			readers:   [][]uint16{{7, 8, 0, 8}, {7, 7, 0, 8}},
			undecided: 0,
			want:      []int{7, 0, 0, 8},
		},
		{
			name:      "explicit value outside input range",
			readers:   [][]uint16{{1, 5, 5}, {2, 5, 5}},
			undecided: 90000,
			want:      []int{90000, 5, 5},
		},
		{
			name:      "full uint16 input range widens auto sentinel",
			readers:   [][]uint16{{65535, 65535, 0}, {0, 65535, 0}},
			undecided: -1,
			want:      []int{65536, 65535, 0},
		},
		{
			name:      "four readers tie or elect a plurality above other ties",
			readers:   [][]uint16{{0, 8, 0}, {0, 8, 1}, {8, 1, 3}, {8, 3, 3}},
			undecided: -1,
			want:      []int{9, 8, 3},
		},
		{
			name:      "votes from preceding voxels cannot break a tie",
			readers:   [][]uint16{{1, 2, 3, 2}, {1, 2, 3, 1}, {1, 2, 1, 3}},
			undecided: 9,
			want:      []int{1, 2, 3, 9},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, err := roi.LabelVoting(tc.readers, tc.undecided)
			if err != nil || !slices.Equal(got, tc.want) {
				t.Fatalf("LabelVoting = %v, %v, want %v", got, err, tc.want)
			}
			got[0] = -1
			reversed := slices.Clone(tc.readers)
			slices.Reverse(reversed)
			got, err = roi.LabelVoting(reversed, tc.undecided)
			if err != nil || !slices.Equal(got, tc.want) {
				t.Fatalf("LabelVoting with reversed readers = %v, %v, want %v", got, err, tc.want)
			}
		})
	}
}

func TestITKReferenceLabelVotingRejectsUnpairedVoxels(t *testing.T) {
	for _, tc := range []struct {
		name    string
		readers [][]uint16
	}{
		{"no reader", nil},
		{"one reader", [][]uint16{{2, 3}}},
		{"empty readers", [][]uint16{{}, {}}},
		{"short second reader", [][]uint16{{2, 3}, {3}}},
		{"long second reader", [][]uint16{{2}, {3, 4}}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, err := roi.LabelVoting(tc.readers, -1)
			if got != nil || !errors.Is(err, roi.ErrConsensusInput) {
				t.Fatalf("LabelVoting = %v, %v, want nil and ErrConsensusInput", got, err)
			}
		})
	}
}
