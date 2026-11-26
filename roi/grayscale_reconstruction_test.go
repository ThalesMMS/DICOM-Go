package roi

import (
	"context"
	"errors"
	"fmt"
	"math/rand"
	"testing"

	"github.com/ThalesMMS/dicom-go/imagefilter"
)

var testConnectivities = []Connectivity3D{Connectivity6, Connectivity18, Connectivity26}

// testVolume returns a volume of dims with unit spacing and the given
// samples (or zeros).
func testVolume(dims [3]int, data ...float32) *imagefilter.Volume {
	v := imagefilter.NewVolume(dims, [3]float64{1, 1, 1})
	copy(v.Data, data)
	return v
}

// randomLevels returns a volume of integer levels in [0, levels), which
// makes plateaus and ties common.
func randomLevels(rng *rand.Rand, dims [3]int, levels int) *imagefilter.Volume {
	v := testVolume(dims)
	for i := range v.Data {
		v.Data[i] = float32(rng.Intn(levels))
	}
	return v
}

// bruteNeighbours lists the in-bounds neighbours of voxel i by definition.
func bruteNeighbours(dims [3]int, c Connectivity3D, i int) []int {
	x, y, z := i%dims[0], i/dims[0]%dims[1], i/(dims[0]*dims[1])
	var out []int
	for dz := -1; dz <= 1; dz++ {
		for dy := -1; dy <= 1; dy++ {
			for dx := -1; dx <= 1; dx++ {
				n := absInt(dx) + absInt(dy) + absInt(dz)
				if n == 0 || c == Connectivity6 && n > 1 || c == Connectivity18 && n > 2 {
					continue
				}
				nx, ny, nz := x+dx, y+dy, z+dz
				if nx < 0 || ny < 0 || nz < 0 || nx >= dims[0] || ny >= dims[1] || nz >= dims[2] {
					continue
				}
				out = append(out, (nz*dims[1]+ny)*dims[0]+nx)
			}
		}
	}
	return out
}

// iteratedGeodesicDilation is the definition of reconstruction by dilation
// (ITK's GrayscaleGeodesicDilateImageFilter run to convergence): repeat an
// elementary dilation clamped by the mask until nothing changes.
func iteratedGeodesicDilation(marker, mask []float32, dims [3]int, c Connectivity3D) []float32 {
	cur := append([]float32(nil), marker...)
	for changed := true; changed; {
		changed = false
		next := append([]float32(nil), cur...)
		for i := range cur {
			v := cur[i]
			for _, q := range bruteNeighbours(dims, c, i) {
				v = max(v, cur[q])
			}
			v = min(v, mask[i])
			if v != next[i] {
				next[i], changed = v, true
			}
		}
		cur = next
	}
	return cur
}

func TestReconstructionMatchesIteratedGeodesicDilation(t *testing.T) {
	rng := rand.New(rand.NewSource(1105))
	for _, dims := range [][3]int{{9, 7, 1}, {6, 5, 4}, {1, 11, 3}} {
		for _, c := range testConnectivities {
			for trial := 0; trial < 20; trial++ {
				mask := randomLevels(rng, dims, 8)
				marker := testVolume(dims)
				for i, v := range mask.Data {
					if rng.Intn(6) == 0 {
						marker.Data[i] = v
					} else {
						marker.Data[i] = v - float32(1+rng.Intn(8))
					}
				}
				got, err := ReconstructionByDilation(context.Background(), marker, mask, GrayscaleOptions{Connectivity: c})
				if err != nil {
					t.Fatal(err)
				}
				want := iteratedGeodesicDilation(marker.Data, mask.Data, dims, c)
				requireSamples(t, fmt.Sprintf("dilation %v c%d #%d", dims, c, trial), got.Data, want)

				// Erosion is the dual: negate, dilate, negate.
				negMarker, negMask := testVolume(dims), testVolume(dims)
				for i := range mask.Data {
					negMarker.Data[i], negMask.Data[i] = -marker.Data[i], -mask.Data[i]
				}
				eroded, err := ReconstructionByErosion(context.Background(), negMarker, negMask, GrayscaleOptions{Connectivity: c})
				if err != nil {
					t.Fatal(err)
				}
				for i := range want {
					want[i] = -want[i]
				}
				requireSamples(t, fmt.Sprintf("erosion %v c%d #%d", dims, c, trial), eroded.Data, want)
			}
		}
	}
}

func requireSamples(t *testing.T, label string, got, want []float32) {
	t.Helper()
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("%s: sample %d = %v, want %v\n got %v\nwant %v", label, i, got[i], want[i], got, want)
		}
	}
}

func TestReconstructionRejectsInvalidInputs(t *testing.T) {
	ctx := context.Background()
	mask := testVolume([3]int{3, 1, 1}, 1, 2, 3)
	cases := []struct {
		name   string
		run    func() error
		target error
	}{
		{"marker above mask", func() error {
			_, err := ReconstructionByDilation(ctx, testVolume([3]int{3, 1, 1}, 0, 5, 0), mask, GrayscaleOptions{})
			return err
		}, ErrInvalidMorphology},
		{"marker below mask", func() error {
			_, err := ReconstructionByErosion(ctx, testVolume([3]int{3, 1, 1}, 9, 1, 9), mask, GrayscaleOptions{})
			return err
		}, ErrInvalidMorphology},
		{"dimensions differ", func() error {
			_, err := ReconstructionByDilation(ctx, testVolume([3]int{1, 3, 1}), mask, GrayscaleOptions{})
			return err
		}, ErrInvalidMorphology},
		{"voxel budget", func() error {
			_, err := ReconstructionByDilation(ctx, mask, mask, GrayscaleOptions{MaxVoxels: 2})
			return err
		}, ErrMorphologyTooLarge},
		{"negative budget", func() error {
			_, err := RegionalMaxima(ctx, mask, GrayscaleOptions{MaxVoxels: -1})
			return err
		}, ErrInvalidMorphology},
		{"connectivity", func() error {
			_, err := RegionalMaxima(ctx, mask, GrayscaleOptions{Connectivity: 8})
			return err
		}, ErrInvalidConnectivity},
		{"negative height", func() error {
			_, err := HMaxima(ctx, mask, -1, GrayscaleOptions{})
			return err
		}, ErrInvalidMorphology},
		{"nil image", func() error {
			_, err := HMinima(ctx, nil, 1, GrayscaleOptions{})
			return err
		}, ErrInvalidMorphology},
		{"non-finite sample", func() error {
			_, err := GrayscaleFillHoles(ctx, testVolume([3]int{2, 1, 1}, 1, float32(inf())), GrayscaleOptions{})
			return err
		}, ErrInvalidMorphology},
	}
	for _, tc := range cases {
		if err := tc.run(); !errors.Is(err, tc.target) {
			t.Errorf("%s: error %v, want %v", tc.name, err, tc.target)
		}
	}
}

func inf() float64 { var zero float64; return 1 / zero }

func TestReconstructionHonoursCancellation(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	v := randomLevels(rand.New(rand.NewSource(1)), [3]int{8, 8, 8}, 5)
	if _, err := HMaxima(ctx, v, 1, GrayscaleOptions{}); !errors.Is(err, context.Canceled) {
		t.Fatalf("HMaxima: %v", err)
	}
	if _, err := RegionalMinima(ctx, v, GrayscaleOptions{}); !errors.Is(err, context.Canceled) {
		t.Fatalf("RegionalMinima: %v", err)
	}
}

// bruteRegionalExtrema labels flat zones by definition and keeps those with
// no strictly more extreme neighbour.
func bruteRegionalExtrema(v *imagefilter.Volume, c Connectivity3D, minima bool) []bool {
	n := len(v.Data)
	zone := make([]int, n)
	for i := range zone {
		zone[i] = -1
	}
	var zones [][]int
	for i := range v.Data {
		if zone[i] >= 0 {
			continue
		}
		id := len(zones)
		members := []int{i}
		zone[i] = id
		for k := 0; k < len(members); k++ {
			for _, q := range bruteNeighbours(v.Dims, c, members[k]) {
				if zone[q] < 0 && v.Data[q] == v.Data[i] {
					zone[q] = id
					members = append(members, q)
				}
			}
		}
		zones = append(zones, members)
	}
	out := make([]bool, n)
	for _, members := range zones {
		extreme := true
		for _, p := range members {
			for _, q := range bruteNeighbours(v.Dims, c, p) {
				if !minima && v.Data[q] > v.Data[p] || minima && v.Data[q] < v.Data[p] {
					extreme = false
				}
			}
		}
		for _, p := range members {
			out[p] = extreme
		}
	}
	return out
}

func TestRegionalExtremaMatchDefinition(t *testing.T) {
	rng := rand.New(rand.NewSource(7))
	for _, dims := range [][3]int{{10, 8, 1}, {6, 5, 4}} {
		for _, c := range testConnectivities {
			for trial := 0; trial < 20; trial++ {
				v := randomLevels(rng, dims, 4)
				for _, minima := range []bool{false, true} {
					var got *VolumeMask
					var err error
					if minima {
						got, err = RegionalMinima(context.Background(), v, GrayscaleOptions{Connectivity: c})
					} else {
						got, err = RegionalMaxima(context.Background(), v, GrayscaleOptions{Connectivity: c})
					}
					if err != nil {
						t.Fatal(err)
					}
					want := bruteRegionalExtrema(v, c, minima)
					for i := range want {
						if got.Voxels[i] != want[i] {
							t.Fatalf("%v c%d minima=%v #%d: voxel %d = %v, want %v", dims, c, minima, trial, i, got.Voxels[i], want[i])
						}
					}
				}
			}
		}
	}
}

// TestRegionalMaximaEqualHConvexOfOne ports the cross-check of ITK's
// itkRegionalMaximaImageFilterTest and itkValuedRegionalMaximaImageFilterTest
// (and their minima twins): on an integer image, the regional maxima are
// exactly the voxels where HConvex with height 1 is positive, and the
// regional minima those where HConcave with height 1 is positive.
func TestRegionalMaximaEqualHConvexOfOne(t *testing.T) {
	rng := rand.New(rand.NewSource(35))
	for _, c := range []Connectivity3D{Connectivity6, Connectivity26} {
		for trial := 0; trial < 30; trial++ {
			v := randomLevels(rng, [3]int{9, 8, 3}, 6)
			ctx := context.Background()
			opts := GrayscaleOptions{Connectivity: c}
			maxima, err := RegionalMaxima(ctx, v, opts)
			if err != nil {
				t.Fatal(err)
			}
			convex, err := HConvex(ctx, v, 1, opts)
			if err != nil {
				t.Fatal(err)
			}
			minima, err := RegionalMinima(ctx, v, opts)
			if err != nil {
				t.Fatal(err)
			}
			concave, err := HConcave(ctx, v, 1, opts)
			if err != nil {
				t.Fatal(err)
			}
			for i := range v.Data {
				if maxima.Voxels[i] != (convex.Data[i] > 0) {
					t.Fatalf("c%d #%d: voxel %d maximum=%v, h-convex %v", c, trial, i, maxima.Voxels[i], convex.Data[i])
				}
				if minima.Voxels[i] != (concave.Data[i] > 0) {
					t.Fatalf("c%d #%d: voxel %d minimum=%v, h-concave %v", c, trial, i, minima.Voxels[i], concave.Data[i])
				}
			}
		}
	}
}

func TestFlatImageIsOneRegionalExtremum(t *testing.T) {
	v := testVolume([3]int{4, 3, 2})
	for i := range v.Data {
		v.Data[i] = 7
	}
	for _, minima := range []bool{false, true} {
		m, err := regionalExtrema(context.Background(), v, GrayscaleOptions{}, minima)
		if err != nil {
			t.Fatal(err)
		}
		if m.Count() != len(v.Data) {
			t.Fatalf("minima=%v: %d of %d voxels", minima, m.Count(), len(v.Data))
		}
	}
}

// TestHMaximaProfile checks h-maxima, h-convex and extended maxima on a
// profile with a dominant peak (10) and a secondary one (6) whose dynamic
// above the saddle (3) is 3.
func TestHMaximaProfile(t *testing.T) {
	profile := testVolume([3]int{7, 1, 1}, 0, 2, 10, 3, 6, 3, 0)
	ctx := context.Background()
	cases := []struct {
		h        float64
		hmax     []float32
		convex   []float32
		extended []bool
	}{
		{2, []float32{0, 2, 8, 3, 4, 3, 0}, []float32{0, 0, 2, 0, 2, 0, 0}, []bool{false, false, true, false, true, false, false}},
		{3, []float32{0, 2, 7, 3, 3, 3, 0}, []float32{0, 0, 3, 0, 3, 0, 0}, []bool{false, false, true, false, false, false, false}},
		// Above the highest dynamic the image flattens at its maximum - h.
		{12, []float32{-2, -2, -2, -2, -2, -2, -2}, []float32{2, 4, 12, 5, 8, 5, 2}, []bool{true, true, true, true, true, true, true}},
	}
	for _, tc := range cases {
		hmax, err := HMaxima(ctx, profile, tc.h, GrayscaleOptions{})
		if err != nil {
			t.Fatal(err)
		}
		requireSamples(t, fmt.Sprintf("h-maxima h=%v", tc.h), hmax.Data, tc.hmax)
		convex, err := HConvex(ctx, profile, tc.h, GrayscaleOptions{})
		if err != nil {
			t.Fatal(err)
		}
		requireSamples(t, fmt.Sprintf("h-convex h=%v", tc.h), convex.Data, tc.convex)
		extended, err := ExtendedMaxima(ctx, profile, tc.h, GrayscaleOptions{})
		if err != nil {
			t.Fatal(err)
		}
		for i, want := range tc.extended {
			if extended.Voxels[i] != want {
				t.Fatalf("extended maxima h=%v: %v, want %v", tc.h, extended.Voxels, tc.extended)
			}
		}
	}
	// h-minima is the dual.
	negated := testVolume([3]int{7, 1, 1}, 0, -2, -10, -3, -6, -3, 0)
	hmin, err := HMinima(ctx, negated, 2, GrayscaleOptions{})
	if err != nil {
		t.Fatal(err)
	}
	requireSamples(t, "h-minima", hmin.Data, []float32{0, -2, -8, -3, -4, -3, 0})
	extended, err := ExtendedMinima(ctx, negated, 3, GrayscaleOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if extended.Count() != 1 || !extended.Voxels[2] {
		t.Fatalf("extended minima: %v", extended.Voxels)
	}
}

func TestGrayscaleFillHoles(t *testing.T) {
	// A bright ring (5) encloses a dark hole (1) with a darker centre (0)
	// on a background of 2; a basin open to the border (0) stays.
	img := testVolume([3]int{9, 7, 1},
		2, 2, 2, 2, 2, 2, 2, 0, 0,
		2, 5, 5, 5, 5, 5, 2, 2, 0,
		2, 5, 1, 1, 1, 5, 2, 2, 2,
		2, 5, 1, 0, 1, 5, 2, 2, 2,
		2, 5, 1, 1, 1, 5, 2, 2, 2,
		2, 5, 5, 5, 5, 5, 2, 2, 2,
		2, 2, 2, 2, 2, 2, 2, 2, 2,
	)
	want := append([]float32(nil), img.Data...)
	for y := 2; y <= 4; y++ {
		for x := 2; x <= 4; x++ {
			want[y*9+x] = 5
		}
	}
	got, err := GrayscaleFillHoles(context.Background(), img, GrayscaleOptions{})
	if err != nil {
		t.Fatal(err)
	}
	requireSamples(t, "fill holes", got.Data, want)

	// Opening the ring's corner leaks the hole to the border diagonally: the
	// hole stays sealed with 4-connectivity (filled to 5) but drains to the
	// leak's level (1) with 8-connectivity.
	img.Data[1*9+5] = 1 // corner of the ring
	img.Data[0*9+6] = 1 // outside the corner
	four, err := GrayscaleFillHoles(context.Background(), img, GrayscaleOptions{Connectivity: Connectivity6})
	if err != nil {
		t.Fatal(err)
	}
	eight, err := GrayscaleFillHoles(context.Background(), img, GrayscaleOptions{Connectivity: Connectivity26})
	if err != nil {
		t.Fatal(err)
	}
	if four.Data[3*9+3] != 5 || eight.Data[3*9+3] != 1 {
		t.Fatalf("leaking hole centre: 4-connected %v (want 5), 8-connected %v (want 1)", four.Data[3*9+3], eight.Data[3*9+3])
	}

	// On random images the result is above the input, idempotent, and has
	// no regional minimum away from the border.
	rng := rand.New(rand.NewSource(11))
	for trial := 0; trial < 20; trial++ {
		v := randomLevels(rng, [3]int{7, 6, 5}, 6)
		filled, err := GrayscaleFillHoles(context.Background(), v, GrayscaleOptions{})
		if err != nil {
			t.Fatal(err)
		}
		again, err := GrayscaleFillHoles(context.Background(), filled, GrayscaleOptions{})
		if err != nil {
			t.Fatal(err)
		}
		requireSamples(t, "idempotent", again.Data, filled.Data)
		minima := bruteRegionalExtrema(filled, Connectivity6, true)
		for i, value := range filled.Data {
			if value < v.Data[i] {
				t.Fatalf("voxel %d lowered from %v to %v", i, v.Data[i], value)
			}
		}
		// Every regional minimum of the result touches the border.
		touches := map[int]bool{}
		zoneOf := flatZones(filled, Connectivity6)
		for i := range filled.Data {
			x, y, z := i%7, i/7%6, i/42
			if x == 0 || y == 0 || z == 0 || x == 6 || y == 5 || z == 4 {
				touches[zoneOf[i]] = true
			}
		}
		for i, isMin := range minima {
			if isMin && !touches[zoneOf[i]] {
				t.Fatalf("trial %d: interior regional minimum left at voxel %d", trial, i)
			}
		}
	}
}

// flatZones numbers the flat zones of v.
func flatZones(v *imagefilter.Volume, c Connectivity3D) []int {
	zone := make([]int, len(v.Data))
	for i := range zone {
		zone[i] = -1
	}
	next := 0
	for i := range v.Data {
		if zone[i] >= 0 {
			continue
		}
		stack := []int{i}
		zone[i] = next
		for len(stack) > 0 {
			p := stack[len(stack)-1]
			stack = stack[:len(stack)-1]
			for _, q := range bruteNeighbours(v.Dims, c, p) {
				if zone[q] < 0 && v.Data[q] == v.Data[i] {
					zone[q] = next
					stack = append(stack, q)
				}
			}
		}
		next++
	}
	return zone
}
