package roi

import (
	"context"
	"errors"
	"fmt"
	"math"
	"math/rand"
	"testing"

	"github.com/ThalesMMS/dicom-go/imagefilter"
)

// bruteBall is the flat erosion or dilation by the ball of radius r (mm)
// by definition, ignoring neighbours outside the image.
func bruteBall(v *imagefilter.Volume, r float64, dilate bool) []float32 {
	d, s := v.Dims, v.SpacingMM
	out := make([]float32, len(v.Data))
	for z := 0; z < d[2]; z++ {
		for y := 0; y < d[1]; y++ {
			for x := 0; x < d[0]; x++ {
				best := float32(math.Inf(1))
				if dilate {
					best = float32(math.Inf(-1))
				}
				for qz := 0; qz < d[2]; qz++ {
					for qy := 0; qy < d[1]; qy++ {
						for qx := 0; qx < d[0]; qx++ {
							dx, dy, dz := float64(qx-x)*s[0], float64(qy-y)*s[1], float64(qz-z)*s[2]
							if dx*dx+dy*dy+dz*dz > r*r {
								continue
							}
							value := v.Data[(qz*d[1]+qy)*d[0]+qx]
							if dilate {
								best = max(best, value)
							} else {
								best = min(best, value)
							}
						}
					}
				}
				out[(z*d[1]+y)*d[0]+x] = best
			}
		}
	}
	return out
}

func TestBallErosionAndDilationMatchDefinition(t *testing.T) {
	rng := rand.New(rand.NewSource(1105))
	cases := []struct {
		dims    [3]int
		spacing [3]float64
		radius  float64
	}{
		{[3]int{9, 8, 7}, [3]float64{1, 1.5, 2.5}, 3.2},
		{[3]int{12, 10, 1}, [3]float64{0.7, 0.9, 0}, 2.3},
		{[3]int{6, 6, 6}, [3]float64{1, 1, 1}, 0.4}, // the ball is one voxel
		{[3]int{5, 7, 6}, [3]float64{2, 1, 1}, 30},  // larger than the image
	}
	for _, tc := range cases {
		v := imagefilter.NewVolume(tc.dims, tc.spacing)
		for i := range v.Data {
			v.Data[i] = float32(rng.NormFloat64())
		}
		for _, dilate := range []bool{false, true} {
			var got *imagefilter.Volume
			var err error
			if dilate {
				got, err = GrayscaleDilate(context.Background(), v, tc.radius, GrayscaleOptions{})
			} else {
				got, err = GrayscaleErode(context.Background(), v, tc.radius, GrayscaleOptions{})
			}
			if err != nil {
				t.Fatal(err)
			}
			requireSamples(t, fmt.Sprintf("%v r=%v dilate=%v", tc.dims, tc.radius, dilate), got.Data, bruteBall(v, tc.radius, dilate))
		}
	}
}

func TestTopHatsExtractDetailsNarrowerThanTheBall(t *testing.T) {
	dims := [3]int{48, 48, 20}
	v := imagefilter.NewVolume(dims, [3]float64{1, 1, 2})
	spot := func(x, y, z int) bool { // a small bright lesion, radius ~2 mm
		dx, dy, dz := float64(x-10), float64(y-10), float64(z-10)*2
		return dx*dx+dy*dy+dz*dz <= 4
	}
	blob := func(x, y, z int) bool { // a large bright organ, radius 9 mm
		dx, dy, dz := float64(x-27), float64(y-27), float64(z-10)*2
		return dx*dx+dy*dy+dz*dz <= 81
	}
	pit := func(x, y, z int) bool { // a small dark cavity
		dx, dy, dz := float64(x-10), float64(y-28), float64(z-10)*2
		return dx*dx+dy*dy+dz*dz <= 4
	}
	for z := 0; z < dims[2]; z++ {
		for y := 0; y < dims[1]; y++ {
			for x := 0; x < dims[0]; x++ {
				value := float32(100)
				switch {
				case spot(x, y, z):
					value = 180
				case blob(x, y, z):
					value = 150
				case pit(x, y, z):
					value = 20
				}
				v.Data[v.Index(x, y, z)] = value
			}
		}
	}
	white, err := WhiteTopHat(context.Background(), v, 4, GrayscaleOptions{})
	if err != nil {
		t.Fatal(err)
	}
	black, err := BlackTopHat(context.Background(), v, 4, GrayscaleOptions{})
	if err != nil {
		t.Fatal(err)
	}
	for z := 0; z < dims[2]; z++ {
		for y := 0; y < dims[1]; y++ {
			for x := 0; x < dims[0]; x++ {
				i := v.Index(x, y, z)
				if white.Data[i] < 0 || black.Data[i] < 0 {
					t.Fatalf("(%d,%d,%d): negative top-hat", x, y, z)
				}
				// The digitized surface of the large blob has notches the
				// ball cannot follow; only check away from it.
				dx, dy, dz := float64(x-27), float64(y-27), float64(z-10)*2
				if r := math.Sqrt(dx*dx + dy*dy + dz*dz); r > 7 && r < 11 {
					continue
				}
				wantWhite, wantBlack := float32(0), float32(0)
				if spot(x, y, z) {
					wantWhite = 80
				}
				if pit(x, y, z) {
					wantBlack = 80
				}
				if white.Data[i] != wantWhite || black.Data[i] != wantBlack {
					t.Fatalf("(%d,%d,%d): white %v (want %v), black %v (want %v)", x, y, z, white.Data[i], wantWhite, black.Data[i], wantBlack)
				}
			}
		}
	}
}

func TestBallFilterRejectsInvalidRequests(t *testing.T) {
	v := imagefilter.NewVolume([3]int{4, 4, 4}, [3]float64{1, 1, 1})
	ctx := context.Background()
	for _, r := range []float64{-1, math.NaN(), math.Inf(1)} {
		if _, err := GrayscaleErode(ctx, v, r, GrayscaleOptions{}); !errors.Is(err, ErrInvalidMorphology) {
			t.Errorf("radius %v: %v", r, err)
		}
	}
	tiny := imagefilter.NewVolume([3]int{300, 300, 300}, [3]float64{0.01, 0.01, 0.01})
	if _, err := GrayscaleOpen(ctx, tiny, 1, GrayscaleOptions{MaxVoxels: 1 << 30}); !errors.Is(err, ErrInvalidMorphology) {
		t.Errorf("oversized ball: %v", err)
	}
	if _, err := GrayscaleClose(ctx, v, 1, GrayscaleOptions{MaxVoxels: 8}); !errors.Is(err, ErrMorphologyTooLarge) {
		t.Errorf("budget: %v", err)
	}
	cancelled, cancel := context.WithCancel(ctx)
	cancel()
	if _, err := WhiteTopHat(cancelled, v, 1, GrayscaleOptions{}); !errors.Is(err, context.Canceled) {
		t.Errorf("cancelled: %v", err)
	}
}
