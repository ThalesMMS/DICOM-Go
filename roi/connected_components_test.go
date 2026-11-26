package roi

import (
	"errors"
	"fmt"
	"math"
	"math/rand"
	"reflect"
	"testing"

	"github.com/ThalesMMS/dicom-go/render"
)

var (
	benchmarkComponents3D   []*Segmentation3D
	benchmarkComponent3D    *Segmentation3D
	benchmarkComponents2D   []*RasterMask
	benchmarkFill2D         *RasterMask
	benchmarkFill3D         map[int]*RasterMask
	benchmarkFillReference  map[int]*RasterMask
	benchmarkReferenceVoxel int
)

// The reference* functions are the pre-#1094 implementations, kept verbatim
// so the run-based labelling and span fill can be checked against them.

func referenceFloodFill(columns, rows, seedX, seedY int, inside func(x, y int) bool) *RasterMask {
	mask := NewRasterMask(columns, rows)
	if inside == nil || seedX < 0 || seedY < 0 || seedX >= columns || seedY >= rows || !inside(seedX, seedY) {
		return mask
	}
	visited := make([]bool, columns*rows)
	idx := func(x, y int) int { return y*columns + x }
	queue := [][2]int{{seedX, seedY}}
	visited[idx(seedX, seedY)] = true
	for len(queue) > 0 {
		p := queue[len(queue)-1]
		queue = queue[:len(queue)-1]
		mask.Set(p[0], p[1], true)
		for _, nb := range [][2]int{{p[0] - 1, p[1]}, {p[0] + 1, p[1]}, {p[0], p[1] - 1}, {p[0], p[1] + 1}} {
			nx, ny := nb[0], nb[1]
			if nx < 0 || ny < 0 || nx >= columns || ny >= rows || visited[idx(nx, ny)] {
				continue
			}
			if inside(nx, ny) {
				visited[idx(nx, ny)] = true
				queue = append(queue, [2]int{nx, ny})
			}
		}
	}
	return mask
}

func referenceConnectedComponents(mask *RasterMask, eightConnected bool) []*RasterMask {
	if mask == nil {
		return nil
	}
	visited := make([]bool, mask.Columns*mask.Rows)
	idx := func(x, y int) int { return y*mask.Columns + x }
	var comps []*RasterMask
	mask.ForEachPixel(func(sx, sy int) {
		if visited[idx(sx, sy)] {
			return
		}
		comp := NewRasterMask(mask.Columns, mask.Rows)
		queue := [][2]int{{sx, sy}}
		visited[idx(sx, sy)] = true
		for len(queue) > 0 {
			p := queue[len(queue)-1]
			queue = queue[:len(queue)-1]
			comp.Set(p[0], p[1], true)
			for _, nb := range referenceNeighbors2D(p[0], p[1], eightConnected) {
				nx, ny := nb[0], nb[1]
				if nx < 0 || ny < 0 || nx >= mask.Columns || ny >= mask.Rows || visited[idx(nx, ny)] {
					continue
				}
				if mask.Get(nx, ny) {
					visited[idx(nx, ny)] = true
					queue = append(queue, [2]int{nx, ny})
				}
			}
		}
		comps = append(comps, comp)
	})
	return comps
}

func referenceNeighbors2D(x, y int, eight bool) [][2]int {
	out := [][2]int{{x - 1, y}, {x + 1, y}, {x, y - 1}, {x, y + 1}}
	if eight {
		out = append(out, [2]int{x - 1, y - 1}, [2]int{x + 1, y - 1}, [2]int{x - 1, y + 1}, [2]int{x + 1, y + 1})
	}
	return out
}

func referenceConnectedComponents3D(seg *Segmentation3D) []*Segmentation3D {
	if seg == nil {
		return nil
	}
	type voxel struct{ x, y, z int }
	visited := map[voxel]bool{}
	var comps []*Segmentation3D
	for _, z := range seg.Slices() {
		mask := seg.masks[z]
		zCopy := z
		mask.ForEachPixel(func(sx, sy int) {
			seed := voxel{sx, sy, zCopy}
			if visited[seed] {
				return
			}
			comp := NewSegmentation3D(seg.Geometry, seg.Columns, seg.Rows)
			queue := []voxel{seed}
			visited[seed] = true
			for len(queue) > 0 {
				p := queue[len(queue)-1]
				queue = queue[:len(queue)-1]
				comp.SetVoxel(p.x, p.y, p.z, true)
				for _, nb := range []voxel{
					{p.x - 1, p.y, p.z}, {p.x + 1, p.y, p.z},
					{p.x, p.y - 1, p.z}, {p.x, p.y + 1, p.z},
					{p.x, p.y, p.z - 1}, {p.x, p.y, p.z + 1},
				} {
					if visited[nb] {
						continue
					}
					if seg.Voxel(nb.x, nb.y, nb.z) {
						visited[nb] = true
						queue = append(queue, nb)
					}
				}
			}
			comps = append(comps, comp)
		})
	}
	return comps
}

// bruteForceComponents3D labels voxels by BFS over the full 3×3×3
// neighbourhood restricted to the connectivity (|dx|+|dy|+|dz| <= 1, 2 or 3),
// in (slice, row, column) discovery order.
func bruteForceComponents3D(seg *Segmentation3D, connectivity Connectivity3D) []*Segmentation3D {
	limit := map[Connectivity3D]int{Connectivity6: 1, Connectivity18: 2, Connectivity26: 3}[connectivity]
	type voxel struct{ x, y, z int }
	visited := map[voxel]bool{}
	var comps []*Segmentation3D
	for _, z := range seg.Slices() {
		seg.masks[z].ForEachPixel(func(sx, sy int) {
			seed := voxel{sx, sy, z}
			if visited[seed] {
				return
			}
			comp := NewSegmentation3D(seg.Geometry, seg.Columns, seg.Rows)
			queue := []voxel{seed}
			visited[seed] = true
			for len(queue) > 0 {
				p := queue[len(queue)-1]
				queue = queue[:len(queue)-1]
				comp.SetVoxel(p.x, p.y, p.z, true)
				for dz := -1; dz <= 1; dz++ {
					for dy := -1; dy <= 1; dy++ {
						for dx := -1; dx <= 1; dx++ {
							d := abs(dx) + abs(dy) + abs(dz)
							nb := voxel{p.x + dx, p.y + dy, p.z + dz}
							if d == 0 || d > limit || visited[nb] || !seg.Voxel(nb.x, nb.y, nb.z) {
								continue
							}
							visited[nb] = true
							queue = append(queue, nb)
						}
					}
				}
			}
			comps = append(comps, comp)
		})
	}
	return comps
}

func abs(v int) int {
	if v < 0 {
		return -v
	}
	return v
}

func randomMask(rng *rand.Rand, columns, rows int, density float64) *RasterMask {
	mask := NewRasterMask(columns, rows)
	for y := 0; y < rows; y++ {
		for x := 0; x < columns; x++ {
			if rng.Float64() < density {
				mask.Set(x, y, true)
			}
		}
	}
	return mask
}

// randomSegmentation fills random slice indices (with gaps and negative
// indices) and, when oversized is set, some masks larger than the
// segmentation so clipping is exercised.
func randomSegmentation(rng *rand.Rand, columns, rows int, density float64, oversized bool) *Segmentation3D {
	seg := NewSegmentation3D(render.VolumeGeometry{MeanSpacing: 2}, columns, rows)
	for z := -2; z < 7; z++ {
		if rng.Intn(5) == 0 {
			continue
		}
		c, r := columns, rows
		if oversized && rng.Intn(3) == 0 {
			c, r = columns+rng.Intn(3), rows+rng.Intn(3)
		}
		seg.SetMask(z, randomMask(rng, c, r, density))
	}
	if rng.Intn(4) == 0 {
		seg.Mask(20) // tracked but empty slice
	}
	return seg
}

func requireSameSegmentations(t *testing.T, label string, got, want []*Segmentation3D) {
	t.Helper()
	if len(got) != len(want) {
		t.Fatalf("%s: %d components, want %d", label, len(got), len(want))
	}
	for i := range want {
		if !reflect.DeepEqual(got[i], want[i]) {
			t.Fatalf("%s: component %d differs:\n got %s\nwant %s", label, i, describeSegmentation(got[i]), describeSegmentation(want[i]))
		}
	}
}

func describeSegmentation(seg *Segmentation3D) string {
	if seg == nil {
		return "<nil>"
	}
	out := ""
	for z, mask := range seg.masks {
		out += fmt.Sprintf(" z%d:", z)
		mask.ForEachRun(func(y int, run MaskRun) {
			out += fmt.Sprintf(" (%d,%d-%d)", y, run.Start, run.End)
		})
	}
	return out
}

func TestConnectedComponentsMatchReferenceOnRandomMasks(t *testing.T) {
	rng := rand.New(rand.NewSource(1094))
	for trial := 0; trial < 300; trial++ {
		columns, rows := 1+rng.Intn(24), 1+rng.Intn(24)
		mask := randomMask(rng, columns, rows, rng.Float64())
		for _, eight := range []bool{false, true} {
			got, want := ConnectedComponents(mask, eight), referenceConnectedComponents(mask, eight)
			if !reflect.DeepEqual(got, want) {
				t.Fatalf("trial %d eight=%v: ConnectedComponents differs from reference (%d vs %d components)", trial, eight, len(got), len(want))
			}
		}
	}
	if ConnectedComponents(nil, true) != nil || ConnectedComponents(NewRasterMask(4, 4), false) != nil {
		t.Fatal("nil or empty mask should have no components")
	}
}

func TestConnectedComponents3DMatchesReferenceOnRandomSegmentations(t *testing.T) {
	rng := rand.New(rand.NewSource(949))
	for trial := 0; trial < 300; trial++ {
		columns, rows := 1+rng.Intn(12), 1+rng.Intn(12)
		seg := randomSegmentation(rng, columns, rows, rng.Float64()*0.8, trial%2 == 1)
		requireSameSegmentations(t, "6-connected", ConnectedComponents3D(seg), referenceConnectedComponents3D(seg))
	}
	if ConnectedComponents3D(nil) != nil {
		t.Fatal("nil segmentation should have no components")
	}
	if got := ConnectedComponents3D(NewSegmentation3D(render.VolumeGeometry{}, 3, 3)); got != nil {
		t.Fatalf("empty segmentation components = %v, want nil", got)
	}
}

func TestConnectedComponents3DConnectivityMatchesBruteForce(t *testing.T) {
	rng := rand.New(rand.NewSource(26))
	for trial := 0; trial < 200; trial++ {
		columns, rows := 1+rng.Intn(10), 1+rng.Intn(10)
		seg := randomSegmentation(rng, columns, rows, rng.Float64()*0.5, false)
		for _, connectivity := range []Connectivity3D{Connectivity6, Connectivity18, Connectivity26} {
			got, err := ConnectedComponents3DWithConnectivity(seg, connectivity)
			if err != nil {
				t.Fatal(err)
			}
			requireSameSegmentations(t, fmt.Sprintf("connectivity %d", connectivity), got, bruteForceComponents3D(seg, connectivity))
		}
	}
	if _, err := ConnectedComponents3DWithConnectivity(NewSegmentation3D(render.VolumeGeometry{}, 1, 1), 8); !errors.Is(err, ErrInvalidConnectivity) {
		t.Fatalf("connectivity 8 error = %v, want ErrInvalidConnectivity", err)
	}
}

func TestConnectedComponents3DDistinguishesEdgeAndCornerContact(t *testing.T) {
	seg := NewSegmentation3D(render.VolumeGeometry{}, 4, 4)
	seg.SetVoxel(0, 0, 0, true)
	seg.SetVoxel(1, 1, 0, true) // in-plane edge contact with (0,0,0)
	seg.SetVoxel(2, 2, 1, true) // corner contact with (1,1,0)
	seg.SetVoxel(3, 3, 3, true) // slice 2 is empty: never adjacent
	for connectivity, want := range map[Connectivity3D]int{Connectivity6: 4, Connectivity18: 3, Connectivity26: 2} {
		comps, err := ConnectedComponents3DWithConnectivity(seg, connectivity)
		if err != nil || len(comps) != want {
			t.Fatalf("connectivity %d: %d components, %v; want %d", connectivity, len(comps), err, want)
		}
	}
}

func TestConnectedComponent3DAtLargestAndRemoveSmall(t *testing.T) {
	geometry := render.BuildVolumeGeometry([]render.SliceGeometry{
		axialSlice(0), axialSlice(1), axialSlice(3),
	}, render.DefaultGeometryTolerances())
	seg := NewSegmentation3D(geometry, 8, 8)
	// A: 2 voxels over slices 0-1; B: 3 voxels in slice 2; C: 1 voxel, D: 3 voxels in slice 0.
	seg.SetVoxel(0, 0, 0, true)
	seg.SetVoxel(0, 0, 1, true)
	seg.Mask(2).SetRun(5, 0, 3)
	seg.SetVoxel(7, 7, 1, true)
	seg.Mask(0).SetRun(4, 4, 7)
	all := ConnectedComponents3D(seg)
	if len(all) != 4 {
		t.Fatalf("components = %d, want 4", len(all))
	}

	at, err := ConnectedComponent3DAt(seg, 1, 5, 2, Connectivity6)
	if err != nil || !reflect.DeepEqual(at, pruned(all[3])) {
		t.Fatalf("ConnectedComponent3DAt = %s, %v; want %s", describeSegmentation(at), err, describeSegmentation(all[3]))
	}
	for _, seed := range [][3]int{{1, 1, 0}, {0, 0, 2}, {0, 0, 9}, {-1, 0, 0}, {0, 99, 0}} {
		if at, err := ConnectedComponent3DAt(seg, seed[0], seed[1], seed[2], Connectivity6); at != nil || err != nil {
			t.Fatalf("ConnectedComponent3DAt%v = %s, %v; want nil", seed, describeSegmentation(at), err)
		}
	}

	largest, err := LargestConnectedComponents3D(seg, 2, Connectivity6)
	if err != nil || largest.VoxelCount() != 6 || !largest.Voxel(4, 4, 0) || !largest.Voxel(2, 5, 2) || largest.Voxel(0, 0, 0) {
		t.Fatalf("LargestConnectedComponents3D(2) = %s, %v; want the two 3-voxel components", describeSegmentation(largest), err)
	}
	if none, _ := LargestConnectedComponents3D(seg, 0, Connectivity6); none.VoxelCount() != 0 || len(none.Slices()) != 0 {
		t.Fatalf("LargestConnectedComponents3D(0) = %s, want empty", describeSegmentation(none))
	}
	if tie, _ := LargestConnectedComponents3D(seg, 1, Connectivity6); !tie.Voxel(4, 4, 0) || tie.VoxelCount() != 3 {
		t.Fatalf("LargestConnectedComponents3D(1) = %s, want the earlier 3-voxel component", describeSegmentation(tie))
	}

	// Slice thicknesses are 1, 1.5 and 2 mm: A = 2.5, D = 3, C = 1.5, B = 6 mm³.
	kept, err := RemoveSmallConnectedComponents3D(seg, 3.5, Connectivity6)
	if err != nil || kept.VoxelCount() != 3 || !kept.Voxel(0, 5, 2) {
		t.Fatalf("RemoveSmallConnectedComponents3D(3.5) = %s, %v; want only the 6 mm³ component", describeSegmentation(kept), err)
	}
	kept, _ = RemoveSmallConnectedComponents3D(seg, 2, Connectivity6)
	if kept.VoxelCount() != 8 || kept.Voxel(7, 7, 1) {
		t.Fatalf("RemoveSmallConnectedComponents3D(2) = %s, want all but the 1-voxel island", describeSegmentation(kept))
	}
	if _, err := LargestConnectedComponents3D(seg, 1, 0); !errors.Is(err, ErrInvalidConnectivity) {
		t.Fatalf("invalid connectivity error = %v", err)
	}
}

func pruned(seg *Segmentation3D) *Segmentation3D {
	seg.Prune()
	return seg
}

func TestFloodFillMatchesReferenceOnRandomMasks(t *testing.T) {
	rng := rand.New(rand.NewSource(4))
	for trial := 0; trial < 300; trial++ {
		columns, rows := 1+rng.Intn(24), 1+rng.Intn(24)
		mask := randomMask(rng, columns, rows, rng.Float64())
		inside := func(x, y int) bool { return mask.Get(x, y) }
		for seed := 0; seed < 5; seed++ {
			x, y := rng.Intn(columns+2)-1, rng.Intn(rows+2)-1
			got, want := FloodFill(columns, rows, x, y, inside), referenceFloodFill(columns, rows, x, y, inside)
			if !reflect.DeepEqual(got.runs, want.runs) || got.Columns != want.Columns || got.Rows != want.Rows {
				t.Fatalf("trial %d seed (%d,%d): FloodFill differs from reference", trial, x, y)
			}
		}
	}
}

// TestFloodFill3DMatchesThresholdedComponent checks the seeded fill against
// the former region-growing path: threshold every slice, label all
// components and keep the one holding the seed.
func TestFloodFill3DMatchesThresholdedComponent(t *testing.T) {
	rng := rand.New(rand.NewSource(3))
	for trial := 0; trial < 200; trial++ {
		columns, rows, slices := 1+rng.Intn(10), 1+rng.Intn(10), 1+rng.Intn(6)
		density := rng.Float64() * 0.8
		voxels := make([]bool, columns*rows*slices)
		for i := range voxels {
			voxels[i] = rng.Float64() < density
		}
		inside := func(x, y, z int) bool { return voxels[(z*rows+y)*columns+x] }
		seg := NewSegmentation3D(render.VolumeGeometry{}, columns, rows)
		for z := 0; z < slices; z++ {
			seg.SetMask(z, ThresholdMask(columns, rows, 1, 1, func(x, y int) (float64, bool) {
				if inside(x, y, z) {
					return 1, true
				}
				return 0, true
			}))
		}
		for seed := 0; seed < 4; seed++ {
			x, y, z := rng.Intn(columns), rng.Intn(rows), rng.Intn(slices)
			for _, connectivity := range []Connectivity3D{Connectivity6, Connectivity18, Connectivity26} {
				got, err := FloodFill3D(columns, rows, slices, x, y, z, connectivity, inside)
				if err != nil {
					t.Fatal(err)
				}
				var components []*Segmentation3D
				if connectivity == Connectivity6 {
					components = referenceConnectedComponents3D(seg)
				} else {
					components = bruteForceComponents3D(seg, connectivity)
				}
				want := map[int]*RasterMask{}
				for _, component := range components {
					if component.Voxel(x, y, z) {
						for _, index := range component.Slices() {
							want[index], _ = component.MaskAt(index)
						}
						break
					}
				}
				if len(got) != len(want) {
					t.Fatalf("trial %d connectivity %d: fill touches %d slices, want %d", trial, connectivity, len(got), len(want))
				}
				for index, mask := range want {
					if !reflect.DeepEqual(got[index].runs, mask.runs) {
						t.Fatalf("trial %d connectivity %d slice %d: fill %v, want %v", trial, connectivity, index, got[index].runs, mask.runs)
					}
				}
			}
		}
	}
}

func TestFloodFill3DOnlyVisitsTheRegionAndItsBorder(t *testing.T) {
	const columns, rows, slices = 200, 200, 50
	calls := 0
	inside := func(x, y, z int) bool {
		calls++
		return x >= 10 && x < 14 && y >= 20 && y < 23 && z >= 5 && z < 7
	}
	got, err := FloodFill3D(columns, rows, slices, 11, 21, 6, Connectivity6, inside)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 || got[5].Count() != 12 || got[6].Count() != 12 {
		t.Fatalf("fill = %d slices, want two 4×3 slabs", len(got))
	}
	if calls > 200 {
		t.Fatalf("inside called %d times, want only the 24-voxel region and its border", calls)
	}
	for _, bad := range []struct{ columns, rows, slices int }{{0, 1, 1}, {1, -1, 1}, {1, 1, 0}} {
		if _, err := FloodFill3D(bad.columns, bad.rows, bad.slices, 0, 0, 0, Connectivity6, inside); !errors.Is(err, ErrInvalidFillVolume) {
			t.Fatalf("FloodFill3D%+v error = %v, want ErrInvalidFillVolume", bad, err)
		}
	}
	if _, err := FloodFill3D(1, 1, 1, 0, 0, 0, 4, inside); !errors.Is(err, ErrInvalidConnectivity) {
		t.Fatalf("connectivity 4 error = %v", err)
	}
	if got, err := FloodFill3D(4, 4, 4, 9, 0, 0, Connectivity6, inside); err != nil || len(got) != 0 {
		t.Fatalf("out-of-volume seed = %v, %v; want empty", got, err)
	}
}

// syntheticSegmentationValue is a deterministic smooth field plus hashed noise,
// thresholded into CT-like blobs with small islands.
func syntheticSegmentationValue(x, y, z int) float64 {
	h := uint32(x)*73856093 ^ uint32(y)*19349663 ^ uint32(z)*83492791
	h ^= h >> 13
	h *= 0x5bd1e995
	h ^= h >> 15
	noise := float64(h&0xffff)/32767.5 - 1
	return math.Sin(float64(x)*0.11)*math.Cos(float64(y)*0.07) +
		math.Sin(float64(z)*0.13+float64(y)*0.05) + 0.3*noise
}

// syntheticCTVolume thresholds the synthetic field into a segmentation and a
// dense copy (for predicate-based filling).
func syntheticCTVolume(columns, rows, slices int, threshold float64) (*Segmentation3D, []bool) {
	seg := NewSegmentation3D(render.VolumeGeometry{}, columns, rows)
	dense := make([]bool, columns*rows*slices)
	for z := 0; z < slices; z++ {
		for y := 0; y < rows; y++ {
			for x := 0; x < columns; x++ {
				dense[(z*rows+y)*columns+x] = syntheticSegmentationValue(x, y, z) >= threshold
			}
		}
		seg.SetMask(z, ThresholdMask(columns, rows, 1, 1, func(x, y int) (float64, bool) {
			if dense[(z*rows+y)*columns+x] {
				return 1, true
			}
			return 0, true
		}))
	}
	return seg, dense
}

// Region-growing benchmarks on a 256×256×128 synthetic segmentation with
// about 3.4 M set voxels (~40%) in ~40 k components; the reference
// sub-benchmarks run the pre-#1094 algorithms.
func BenchmarkConnectedComponents3D(b *testing.B) {
	const columns, rows, slices = 256, 256, 128
	seg, dense := syntheticCTVolume(columns, rows, slices, 0.25)
	seedX, seedY, seedZ := 14, 0, 0 // inside the largest blob
	if !dense[(seedZ*rows+seedY)*columns+seedX] {
		b.Fatal("benchmark seed is not set")
	}
	inside := func(x, y, z int) bool { return dense[(z*rows+y)*columns+x] }

	b.Run("all/runs", func(b *testing.B) {
		b.ReportAllocs()
		for i := 0; i < b.N; i++ {
			benchmarkComponents3D = ConnectedComponents3D(seg)
		}
		b.ReportMetric(float64(len(benchmarkComponents3D)), "components")
	})
	b.Run("all/reference", func(b *testing.B) {
		b.ReportAllocs()
		for i := 0; i < b.N; i++ {
			benchmarkComponents3D = referenceConnectedComponents3D(seg)
		}
		b.ReportMetric(float64(len(benchmarkComponents3D)), "components")
	})
	b.Run("seed/component-at", func(b *testing.B) {
		b.ReportAllocs()
		for i := 0; i < b.N; i++ {
			benchmarkComponent3D, _ = ConnectedComponent3DAt(seg, seedX, seedY, seedZ, Connectivity6)
		}
		b.ReportMetric(float64(benchmarkComponent3D.VoxelCount()), "voxels")
	})
	b.Run("seed/flood-fill", func(b *testing.B) {
		b.ReportAllocs()
		for i := 0; i < b.N; i++ {
			benchmarkFill3D, _ = FloodFill3D(columns, rows, slices, seedX, seedY, seedZ, Connectivity6, inside)
		}
	})
	b.Run("seed/reference", func(b *testing.B) {
		// The former Twin-Viewer path: label everything, keep the seed's component.
		b.ReportAllocs()
		for i := 0; i < b.N; i++ {
			for _, component := range referenceConnectedComponents3D(seg) {
				if component.Voxel(seedX, seedY, seedZ) {
					benchmarkReferenceVoxel = component.VoxelCount()
					break
				}
			}
		}
	})
	b.Run("largest/runs", func(b *testing.B) {
		b.ReportAllocs()
		for i := 0; i < b.N; i++ {
			benchmarkComponent3D, _ = LargestConnectedComponents3D(seg, 1, Connectivity6)
		}
	})
	b.Run("all26/runs", func(b *testing.B) {
		b.ReportAllocs()
		for i := 0; i < b.N; i++ {
			benchmarkComponents3D, _ = ConnectedComponents3DWithConnectivity(seg, Connectivity26)
		}
		b.ReportMetric(float64(len(benchmarkComponents3D)), "components")
	})
}

// BenchmarkConnectedComponents3DCT runs the run-based paths on a full CT-sized
// 512×512×400 volume at ~30% (the reference would need tens of GB).
func BenchmarkConnectedComponents3DCT(b *testing.B) {
	const columns, rows, slices = 512, 512, 400
	seg, dense := syntheticCTVolume(columns, rows, slices, 0.45)
	inside := func(x, y, z int) bool { return dense[(z*rows+y)*columns+x] }
	seedX, seedY, seedZ := 14, 0, 0
	if !inside(seedX, seedY, seedZ) {
		b.Fatal("benchmark seed is not set")
	}
	b.Run("largest", func(b *testing.B) {
		b.ReportAllocs()
		for i := 0; i < b.N; i++ {
			benchmarkComponent3D, _ = LargestConnectedComponents3D(seg, 1, Connectivity6)
		}
		b.ReportMetric(float64(seg.VoxelCount()), "voxels")
	})
	b.Run("flood-fill", func(b *testing.B) {
		b.ReportAllocs()
		for i := 0; i < b.N; i++ {
			benchmarkFill3D, _ = FloodFill3D(columns, rows, slices, seedX, seedY, seedZ, Connectivity6, inside)
		}
	})
}

func BenchmarkConnectedComponents2D(b *testing.B) {
	seg, _ := syntheticCTVolume(512, 512, 1, 0.25)
	mask, _ := seg.MaskAt(0)
	inside := func(x, y int) bool { return mask.Get(x, y) }
	b.Run("label/runs", func(b *testing.B) {
		b.ReportAllocs()
		for i := 0; i < b.N; i++ {
			benchmarkComponents2D = ConnectedComponents(mask, true)
		}
	})
	b.Run("label/reference", func(b *testing.B) {
		b.ReportAllocs()
		for i := 0; i < b.N; i++ {
			benchmarkComponents2D = referenceConnectedComponents(mask, true)
		}
	})
	b.Run("fill/span", func(b *testing.B) {
		b.ReportAllocs()
		for i := 0; i < b.N; i++ {
			benchmarkFill2D = FloodFill(512, 512, 14, 0, inside)
		}
	})
	b.Run("fill/reference", func(b *testing.B) {
		b.ReportAllocs()
		for i := 0; i < b.N; i++ {
			benchmarkFill2D = referenceFloodFill(512, 512, 14, 0, inside)
		}
	})
}
