package roi

import (
	"context"
	"errors"
	"fmt"
	"math"
	"math/rand"
	"sort"
	"testing"

	"github.com/ThalesMMS/dicom-go/imagefilter"
	"github.com/ThalesMMS/dicom-go/render"
)

func unitGrid(dims [3]int) Grid {
	return axisGrid(dims[0], dims[1], dims[2], [3]float64{1, 1, 1}, render.Vec3{})
}

func markerVolume(dims [3]int, labels map[int]uint16) *LabelVolume {
	v := &LabelVolume{Grid: unitGrid(dims), Labels: make([]uint16, dims[0]*dims[1]*dims[2])}
	for i, l := range labels {
		v.Labels[i] = l
	}
	return v
}

// hierarchicalQueueWatershed re-expresses the flooding of ITK's
// MorphologicalWatershedFromMarkersImageFilter with its own data structure:
// an ordered map from level to FIFO queue, where the queue of the current
// level is taken out of the map and a voxel not above that level is
// appended to it. It checks that the heap ordering of WatershedFromMarkers
// visits voxels in the same order.
func hierarchicalQueueWatershed(relief []float32, markers []uint16, dims [3]int, c Connectivity3D, lines bool) []uint16 {
	fah := map[float32][]int{}
	pushLevel := func(level float32, i int) { fah[level] = append(fah[level], i) }
	popLevel := func() (float32, []int) {
		keys := make([]float64, 0, len(fah))
		for k := range fah {
			keys = append(keys, float64(k))
		}
		sort.Float64s(keys)
		level := float32(keys[0])
		queue := fah[level]
		delete(fah, level)
		return level, queue
	}
	out := append([]uint16(nil), markers...)
	if lines {
		status := make([]bool, len(out))
		for i, l := range markers {
			if l == 0 {
				continue
			}
			status[i] = true
			for _, q := range bruteNeighbours(dims, c, i) {
				if !status[q] && markers[q] == 0 {
					pushLevel(relief[q], q)
					status[q] = true
				}
			}
		}
		for len(fah) > 0 {
			current, queue := popLevel()
			for len(queue) > 0 {
				p := queue[0]
				queue = queue[1:]
				var label uint16
				collision := false
				for _, q := range bruteNeighbours(dims, c, p) {
					if o := out[q]; o != 0 {
						if label != 0 && o != label {
							collision = true
							break
						}
						label = o
					}
				}
				if collision {
					continue
				}
				out[p] = label
				for _, q := range bruteNeighbours(dims, c, p) {
					if !status[q] {
						if relief[q] <= current {
							queue = append(queue, q)
						} else {
							pushLevel(relief[q], q)
						}
						status[q] = true
					}
				}
			}
		}
		return out
	}
	for i, l := range markers {
		if l == 0 {
			continue
		}
		for _, q := range bruteNeighbours(dims, c, i) {
			if markers[q] == 0 {
				pushLevel(relief[i], i)
				break
			}
		}
	}
	for len(fah) > 0 {
		current, queue := popLevel()
		for len(queue) > 0 {
			p := queue[0]
			queue = queue[1:]
			for _, q := range bruteNeighbours(dims, c, p) {
				if out[q] == 0 {
					out[q] = out[p]
					if relief[q] <= current {
						queue = append(queue, q)
					} else {
						pushLevel(relief[q], q)
					}
				}
			}
		}
	}
	return out
}

func TestWatershedMatchesHierarchicalQueueFlooding(t *testing.T) {
	rng := rand.New(rand.NewSource(1105))
	for _, dims := range [][3]int{{12, 9, 1}, {7, 6, 5}} {
		for _, c := range testConnectivities {
			for _, lines := range []bool{false, true} {
				for trial := 0; trial < 15; trial++ {
					relief := randomLevels(rng, dims, 5)
					labels := map[int]uint16{}
					for k := 0; k < 2+rng.Intn(4); k++ {
						labels[rng.Intn(len(relief.Data))] = uint16(1 + rng.Intn(3))
					}
					markers := markerVolume(dims, labels)
					got, err := WatershedFromMarkers(context.Background(), relief, markers, WatershedOptions{Connectivity: c, MarkWatershedLine: lines})
					if err != nil {
						t.Fatal(err)
					}
					want := hierarchicalQueueWatershed(relief.Data, markers.Labels, dims, c, lines)
					for i := range want {
						if got.Labels.Labels[i] != want[i] {
							t.Fatalf("%v c%d lines=%v #%d: voxel %d = %d, want %d", dims, c, lines, trial, i, got.Labels.Labels[i], want[i])
						}
					}
					if lines {
						checkWatershedLines(t, got, markers.Labels, dims, c)
					} else if got.Unreached != 0 || got.LineVoxels != 0 {
						t.Fatalf("Beucher flood left %d unreached and %d line voxels", got.Unreached, got.LineVoxels)
					}
				}
			}
		}
	}
}

// checkWatershedLines verifies that no two different basins touch (except
// where both voxels are markers) and that the line count matches the zero
// labels.
func checkWatershedLines(t *testing.T, r *WatershedResult, markers []uint16, dims [3]int, c Connectivity3D) {
	t.Helper()
	zeros := 0
	for i, l := range r.Labels.Labels {
		if l == 0 {
			zeros++
			continue
		}
		for _, q := range bruteNeighbours(dims, c, i) {
			if o := r.Labels.Labels[q]; o != 0 && o != l && (markers[i] == 0 || markers[q] == 0) {
				t.Fatalf("basins %d and %d touch at voxels %d and %d", l, o, i, q)
			}
		}
	}
	if zeros != r.LineVoxels+r.Unreached {
		t.Fatalf("%d zero labels, %d line and %d unreached voxels", zeros, r.LineVoxels, r.Unreached)
	}
}

func TestWatershedSplitsAtTheRidge(t *testing.T) {
	relief := testVolume([3]int{7, 1, 1}, 0, 1, 2, 3, 2, 1, 0)
	markers := markerVolume(relief.Dims, map[int]uint16{0: 1, 6: 2})
	beucher, err := WatershedFromMarkers(context.Background(), relief, markers, WatershedOptions{})
	if err != nil {
		t.Fatal(err)
	}
	// The ridge voxel goes to the basin queued first at its level.
	if got := fmt.Sprint(beucher.Labels.Labels); got != "[1 1 1 1 2 2 2]" {
		t.Fatalf("Beucher labels %s", got)
	}
	meyer, err := WatershedFromMarkers(context.Background(), relief, markers, WatershedOptions{MarkWatershedLine: true})
	if err != nil {
		t.Fatal(err)
	}
	if got := fmt.Sprint(meyer.Labels.Labels); got != "[1 1 1 0 2 2 2]" || meyer.LineVoxels != 1 {
		t.Fatalf("Meyer labels %s, %d line voxels", got, meyer.LineVoxels)
	}
}

func TestWatershedMaskConfinesTheFlood(t *testing.T) {
	dims := [3]int{8, 1, 1}
	relief := testVolume(dims)
	mask := NewVolumeMask(8, 1, 1, [3]float64{1, 1, 1})
	for _, x := range []int{0, 1, 2, 5, 6} {
		mask.Voxels[x] = true
	}
	markers := markerVolume(dims, map[int]uint16{1: 4})
	for _, lines := range []bool{false, true} {
		got, err := WatershedFromMarkers(context.Background(), relief, markers, WatershedOptions{Mask: mask, MarkWatershedLine: lines})
		if err != nil {
			t.Fatal(err)
		}
		if s := fmt.Sprint(got.Labels.Labels); s != "[4 4 4 0 0 0 0 0]" || got.Unreached != 2 {
			t.Fatalf("lines=%v: labels %s, %d unreached", lines, s, got.Unreached)
		}
	}
	markers.Labels[4] = 1
	if _, err := WatershedFromMarkers(context.Background(), relief, markers, WatershedOptions{Mask: mask}); !errors.Is(err, ErrInvalidMorphology) {
		t.Fatalf("marker outside the mask: %v", err)
	}
}

func TestWatershedRejectsInvalidInputs(t *testing.T) {
	ctx := context.Background()
	relief := testVolume([3]int{4, 3, 2})
	good := markerVolume(relief.Dims, map[int]uint16{0: 1})
	cases := map[string]struct {
		markers *LabelVolume
		opts    WatershedOptions
		target  error
	}{
		"nil markers":        {nil, WatershedOptions{}, ErrInvalidMorphology},
		"marker dimensions":  {markerVolume([3]int{3, 4, 2}, nil), WatershedOptions{}, ErrInvalidMorphology},
		"mask dimensions":    {good, WatershedOptions{Mask: NewVolumeMask(4, 3, 1, [3]float64{1, 1, 1})}, ErrInvalidMorphology},
		"connectivity":       {good, WatershedOptions{Connectivity: 4}, ErrInvalidConnectivity},
		"voxel budget":       {good, WatershedOptions{MaxVoxels: 10}, ErrMorphologyTooLarge},
		"invalid mask":       {good, WatershedOptions{Mask: &VolumeMask{Columns: 4, Rows: 3, Slices: 2}}, ErrInvalidMorphology},
		"cancelled context":  {good, WatershedOptions{}, nil},
		"meyer on cancelled": {good, WatershedOptions{MarkWatershedLine: true}, nil},
	}
	for name, tc := range cases {
		c := ctx
		target := tc.target
		if target == nil {
			cancelled, cancel := context.WithCancel(ctx)
			cancel()
			c, target = cancelled, context.Canceled
		}
		if _, err := WatershedFromMarkers(c, relief, tc.markers, tc.opts); !errors.Is(err, target) {
			t.Errorf("%s: %v, want %v", name, err, target)
		}
	}
}

func TestLabelComponentsNumbersInScanOrder(t *testing.T) {
	rng := rand.New(rand.NewSource(3))
	for _, c := range testConnectivities {
		for trial := 0; trial < 20; trial++ {
			dims := [3]int{7, 6, 5}
			mask := NewVolumeMask(dims[0], dims[1], dims[2], [3]float64{1, 1, 1})
			for i := range mask.Voxels {
				mask.Voxels[i] = rng.Intn(3) == 0
			}
			labels, count, err := LabelComponents(context.Background(), unitGrid(dims), mask, c)
			if err != nil {
				t.Fatal(err)
			}
			// Reference: breadth-first labelling in scan order.
			want := make([]uint16, len(mask.Voxels))
			next := uint16(0)
			for i, in := range mask.Voxels {
				if !in || want[i] != 0 {
					continue
				}
				next++
				want[i] = next
				queue := []int{i}
				for len(queue) > 0 {
					p := queue[0]
					queue = queue[1:]
					for _, q := range bruteNeighbours(dims, c, p) {
						if mask.Voxels[q] && want[q] == 0 {
							want[q] = next
							queue = append(queue, q)
						}
					}
				}
			}
			if count != int(next) {
				t.Fatalf("c%d #%d: %d components, want %d", c, trial, count, next)
			}
			for i := range want {
				if labels.Labels[i] != want[i] {
					t.Fatalf("c%d #%d: voxel %d label %d, want %d", c, trial, i, labels.Labels[i], want[i])
				}
			}
		}
	}
	if _, _, err := LabelComponents(context.Background(), unitGrid([3]int{2, 2, 2}), NewVolumeMask(2, 2, 1, [3]float64{1, 1, 1}), Connectivity6); !errors.Is(err, ErrInvalidMorphology) {
		t.Fatalf("grid mismatch: %v", err)
	}
	// 65536 isolated voxels do not fit 16-bit labels.
	many := NewVolumeMask(512, 256, 1, [3]float64{1, 1, 1})
	for y := 0; y < 256; y++ {
		for x := y % 2; x < 512; x += 2 {
			many.Voxels[y*512+x] = true
		}
	}
	if _, _, err := LabelComponents(context.Background(), unitGrid([3]int{512, 256, 1}), many, Connectivity6); !errors.Is(err, ErrTooManyLabels) {
		t.Fatalf("too many labels: %v", err)
	}
}

// addBall sets the voxels within radius (mm) of centre (mm).
func addBall(m *VolumeMask, centre [3]float64, radius float64) {
	for z := 0; z < m.Slices; z++ {
		for y := 0; y < m.Rows; y++ {
			for x := 0; x < m.Columns; x++ {
				dx, dy, dz := m.X[x]-centre[0], m.Y[y]-centre[1], m.Z[z]-centre[2]
				if dx*dx+dy*dy+dz*dz <= radius*radius {
					m.Voxels[m.index(x, y, z)] = true
				}
			}
		}
	}
}

func maskGrid(m *VolumeMask) Grid {
	spacing := [3]float64{axisStep(m.X), axisStep(m.Y), axisStep(m.Z)}
	return axisGrid(m.Columns, m.Rows, m.Slices, spacing, render.Vec3{})
}

type splitBall struct {
	centre [3]float64
	radius float64
}

// TestSplitTouchingObjectsSeparatesObjectsInContact is the acceptance case
// of #1105: synthetic objects that touch are split into one region each,
// every voxel clearly nearer one object's centre goes to that object's
// region, and single objects stay whole.
func TestSplitTouchingObjectsSeparatesObjectsInContact(t *testing.T) {
	cases := []struct {
		name    string
		dims    [3]int
		spacing [3]float64
		balls   []splitBall
		height  float64
		conn    Connectivity3D
	}{
		{"two discs in contact", [3]int{60, 40, 1}, [3]float64{1, 1, 1},
			[]splitBall{{[3]float64{18, 20, 0}, 11}, {[3]float64{38, 20, 0}, 11}}, 1, Connectivity6},
		{"unequal discs, 8-connected", [3]int{60, 44, 1}, [3]float64{0.8, 0.8, 1},
			[]splitBall{{[3]float64{14, 17, 0}, 10}, {[3]float64{29, 17, 0}, 6.5}}, 1, Connectivity26},
		{"two spheres, anisotropic voxels", [3]int{40, 30, 16}, [3]float64{0.8, 0.8, 2},
			[]splitBall{{[3]float64{10, 12, 15}, 7.5}, {[3]float64{22, 12, 15}, 7.5}}, 1.5, Connectivity6},
		{"chain of three nodules", [3]int{56, 24, 22}, [3]float64{1, 1, 1},
			[]splitBall{{[3]float64{10, 12, 11}, 7}, {[3]float64{23, 12, 11}, 7}, {[3]float64{38, 12, 11}, 9}}, 1, Connectivity18},
		{"separate spheres", [3]int{40, 20, 20}, [3]float64{1, 1, 1},
			[]splitBall{{[3]float64{9, 10, 10}, 6}, {[3]float64{29, 10, 10}, 5}}, 1, Connectivity6},
		{"single sphere", [3]int{30, 30, 30}, [3]float64{1, 1, 1},
			[]splitBall{{[3]float64{15, 15, 15}, 10}}, 1, Connectivity6},
		{"sphere touching the border", [3]int{20, 20, 12}, [3]float64{1, 1, 1.5},
			[]splitBall{{[3]float64{0, 10, 8}, 8}}, 1, Connectivity26},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			mask := NewVolumeMask(tc.dims[0], tc.dims[1], tc.dims[2], tc.spacing)
			for _, b := range tc.balls {
				addBall(mask, b.centre, b.radius)
			}
			res, err := SplitTouchingObjects(context.Background(), maskGrid(mask), mask, SplitOptions{HeightMM: tc.height, Connectivity: tc.conn})
			if err != nil {
				t.Fatal(err)
			}
			if res.Regions() != len(tc.balls) {
				t.Fatalf("%d regions %v, want %d", res.Regions(), res.RegionVoxels, len(tc.balls))
			}
			total := 0
			for k, n := range res.RegionVoxels {
				total += n
				if k > 0 && n > res.RegionVoxels[k-1] {
					t.Fatalf("regions not ordered by size: %v", res.RegionVoxels)
				}
			}
			if total != mask.Count() {
				t.Fatalf("regions hold %d voxels, mask %d", total, mask.Count())
			}
			// Map each ball to the region at its centre, then check every
			// voxel clearly nearer (by surface distance) one ball.
			owner := make([]uint16, len(tc.balls))
			for b, ball := range tc.balls {
				x, y, z := nearestIndex(mask.X, ball.centre[0]), nearestIndex(mask.Y, ball.centre[1]), nearestIndex(mask.Z, ball.centre[2])
				owner[b] = res.Labels.LabelAt(x, y, z)
				for other := 0; other < b; other++ {
					if owner[other] == owner[b] {
						t.Fatalf("balls %d and %d share region %d", other, b, owner[b])
					}
				}
			}
			misplaced := 0
			for z := 0; z < mask.Slices; z++ {
				for y := 0; y < mask.Rows; y++ {
					for x := 0; x < mask.Columns; x++ {
						i := mask.index(x, y, z)
						label := res.Labels.Labels[i]
						if !mask.Voxels[i] {
							if label != 0 {
								t.Fatalf("background voxel (%d,%d,%d) labelled %d", x, y, z, label)
							}
							continue
						}
						if label == 0 {
							t.Fatalf("mask voxel (%d,%d,%d) unlabelled", x, y, z)
						}
						best, second := -1, math.Inf(1)
						bestD := math.Inf(1)
						for b, ball := range tc.balls {
							dx, dy, dz := mask.X[x]-ball.centre[0], mask.Y[y]-ball.centre[1], mask.Z[z]-ball.centre[2]
							d := math.Sqrt(dx*dx+dy*dy+dz*dz) - ball.radius
							if d < bestD {
								second, bestD, best = bestD, d, b
							} else if d < second {
								second = d
							}
						}
						if second-bestD > 2.5*tc.spacing[2] && label != owner[best] {
							misplaced++
						}
					}
				}
			}
			if misplaced != 0 {
				t.Fatalf("%d voxels clearly nearer another object", misplaced)
			}
		})
	}
}

func nearestIndex(axis []float64, v float64) int {
	best := 0
	for i, c := range axis {
		if math.Abs(c-v) < math.Abs(axis[best]-v) {
			best = i
		}
	}
	return best
}

func TestSplitTouchingObjectsHeightMergesShallowNecks(t *testing.T) {
	// Two discs whose neck is only slightly narrower than the smaller disc:
	// a low height splits them, a height above the dynamic keeps them whole.
	mask := NewVolumeMask(50, 30, 1, [3]float64{1, 1, 1})
	addBall(mask, [3]float64{16, 15, 0}, 9)
	addBall(mask, [3]float64{31, 15, 0}, 8)
	grid := maskGrid(mask)
	low, err := SplitTouchingObjects(context.Background(), grid, mask, SplitOptions{HeightMM: 0.5})
	if err != nil {
		t.Fatal(err)
	}
	high, err := SplitTouchingObjects(context.Background(), grid, mask, SplitOptions{HeightMM: 6})
	if err != nil {
		t.Fatal(err)
	}
	if low.Regions() != 2 || high.Regions() != 1 {
		t.Fatalf("height 0.5 mm: %d regions, height 6 mm: %d regions", low.Regions(), high.Regions())
	}
}

func TestSplitTouchingObjectsEdgeCases(t *testing.T) {
	ctx := context.Background()
	empty := NewVolumeMask(5, 5, 5, [3]float64{1, 1, 1})
	res, err := SplitTouchingObjects(ctx, maskGrid(empty), empty, SplitOptions{HeightMM: 1})
	if err != nil || res.Regions() != 0 {
		t.Fatalf("empty mask: %v, %v", res, err)
	}
	full := NewVolumeMask(4, 3, 2, [3]float64{1, 1, 1})
	for i := range full.Voxels {
		full.Voxels[i] = true
	}
	res, err = SplitTouchingObjects(ctx, maskGrid(full), full, SplitOptions{HeightMM: 1})
	if err != nil || res.Regions() != 1 || res.RegionVoxels[0] != 24 || res.Labels.Count(1) != 24 {
		t.Fatalf("full mask: %+v, %v", res, err)
	}
	ball := NewVolumeMask(20, 20, 20, [3]float64{1, 1, 1})
	addBall(ball, [3]float64{10, 10, 10}, 6)
	for name, tc := range map[string]struct {
		grid   Grid
		opts   SplitOptions
		target error
	}{
		"zero height":  {maskGrid(ball), SplitOptions{}, ErrInvalidMorphology},
		"grid":         {unitGrid([3]int{20, 20, 19}), SplitOptions{HeightMM: 1}, ErrInvalidMorphology},
		"budget":       {maskGrid(ball), SplitOptions{HeightMM: 1, MaxVoxels: 1000}, ErrMorphologyTooLarge},
		"connectivity": {maskGrid(ball), SplitOptions{HeightMM: 1, Connectivity: 7}, ErrInvalidConnectivity},
	} {
		if _, err := SplitTouchingObjects(ctx, tc.grid, ball, tc.opts); !errors.Is(err, tc.target) {
			t.Errorf("%s: %v, want %v", name, err, tc.target)
		}
	}
	cancelled, cancel := context.WithCancel(ctx)
	cancel()
	if _, err := SplitTouchingObjects(cancelled, maskGrid(ball), ball, SplitOptions{HeightMM: 1}); !errors.Is(err, context.Canceled) {
		t.Errorf("cancelled: %v", err)
	}
}

// TestHotspotMarkersSplitTouchingLesions exercises the PET use of #1105:
// the extended maxima of an uptake image mark each hotspot, and the
// watershed of the negated image, confined to a threshold mask, separates
// two lesions whose uptake regions merge.
func TestHotspotMarkersSplitTouchingLesions(t *testing.T) {
	dims := [3]int{40, 30, 20}
	pet := imagefilter.NewVolume(dims, [3]float64{2, 2, 2})
	lesions := [][4]float64{{14, 15, 10, 8}, {25, 15, 10, 5}} // x, y, z, peak SUV
	for z := 0; z < dims[2]; z++ {
		for y := 0; y < dims[1]; y++ {
			for x := 0; x < dims[0]; x++ {
				value := 1.0 // background uptake
				for _, l := range lesions {
					dx, dy, dz := float64(x)-l[0], float64(y)-l[1], float64(z)-l[2]
					value += l[3] * math.Exp(-(dx*dx+dy*dy+dz*dz)/(2*3*3))
				}
				pet.Data[pet.Index(x, y, z)] = float32(value)
			}
		}
	}
	ctx := context.Background()
	opts := GrayscaleOptions{Connectivity: Connectivity26}
	hot, err := ExtendedMaxima(ctx, pet, 0.5, opts)
	if err != nil {
		t.Fatal(err)
	}
	grid := axisGrid(dims[0], dims[1], dims[2], pet.SpacingMM, render.Vec3{})
	markers, count, err := LabelComponents(ctx, grid, hot, Connectivity26)
	if err != nil {
		t.Fatal(err)
	}
	if count != 2 {
		t.Fatalf("%d hotspot markers, want 2", count)
	}
	threshold := NewVolumeMask(dims[0], dims[1], dims[2], pet.SpacingMM)
	for i, v := range pet.Data {
		threshold.Voxels[i] = v >= 2.5
	}
	if _, n, _ := LabelComponents(ctx, grid, threshold, Connectivity26); n != 1 {
		t.Fatalf("the threshold mask has %d components, want the lesions merged into 1", n)
	}
	negated := pet.Clone()
	for i := range negated.Data {
		negated.Data[i] = -negated.Data[i]
	}
	res, err := WatershedFromMarkers(ctx, negated, markers, WatershedOptions{Connectivity: Connectivity26, Mask: threshold})
	if err != nil {
		t.Fatal(err)
	}
	if res.Unreached != 0 {
		t.Fatalf("%d unreached voxels", res.Unreached)
	}
	a := res.Labels.LabelAt(14, 15, 10)
	b := res.Labels.LabelAt(25, 15, 10)
	if a == 0 || b == 0 || a == b {
		t.Fatalf("lesion labels %d and %d", a, b)
	}
	// The boundary lies at the uptake saddle between the two peaks.
	for x := 0; x < dims[0]; x++ {
		l := res.Labels.LabelAt(x, 15, 10)
		if l == 0 {
			continue
		}
		if x <= 19 && l != a || x >= 22 && l != b {
			t.Fatalf("x=%d labelled %d (lesions %d, %d)", x, l, a, b)
		}
	}
}

func BenchmarkSplitTouchingObjects(b *testing.B) {
	mask := NewVolumeMask(160, 120, 100, [3]float64{0.8, 0.8, 1.25})
	addBall(mask, [3]float64{45, 48, 62}, 30)
	addBall(mask, [3]float64{92, 48, 62}, 22)
	grid := maskGrid(mask)
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		res, err := SplitTouchingObjects(context.Background(), grid, mask, SplitOptions{HeightMM: 1.5})
		if err != nil || res.Regions() != 2 {
			b.Fatalf("%v regions, %v", res, err)
		}
	}
}

func BenchmarkHMaxima(b *testing.B) {
	rng := rand.New(rand.NewSource(1))
	v := imagefilter.NewVolume([3]int{256, 256, 64}, [3]float64{1, 1, 1})
	for i := range v.Data {
		x, y, z := i%256, i/256%256, i/65536
		v.Data[i] = float32(100*math.Sin(float64(x)/9)*math.Cos(float64(y)/7)+20*math.Sin(float64(z)/5)) + float32(rng.NormFloat64())
	}
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if _, err := HMaxima(context.Background(), v, 10, GrayscaleOptions{Connectivity: Connectivity26}); err != nil {
			b.Fatal(err)
		}
	}
}

func BenchmarkWhiteTopHat(b *testing.B) {
	rng := rand.New(rand.NewSource(1))
	v := imagefilter.NewVolume([3]int{256, 256, 64}, [3]float64{0.8, 0.8, 2})
	for i := range v.Data {
		v.Data[i] = float32(rng.NormFloat64())
	}
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if _, err := WhiteTopHat(context.Background(), v, 5, GrayscaleOptions{}); err != nil {
			b.Fatal(err)
		}
	}
}
