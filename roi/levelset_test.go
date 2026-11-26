package roi

import (
	"context"
	"errors"
	"math/rand"
	"testing"
)

// levelSetPhantom is a synthetic volume: a bright object (100) on a dark
// background (0) with Gaussian noise, and its exact voxel mask.
type levelSetPhantom struct {
	volume LevelSetVolume
	truth  []bool
	seed   RegionGrowSeed
	values []float64
}

func newLevelSetPhantom(nx, ny, nz int, spacing [3]float64, noise float64, inside func(x, y, z float64) bool, seed RegionGrowSeed) *levelSetPhantom {
	rng := rand.New(rand.NewSource(1178))
	p := &levelSetPhantom{truth: make([]bool, nx*ny*nz), values: make([]float64, nx*ny*nz), seed: seed}
	for z := 0; z < nz; z++ {
		for y := 0; y < ny; y++ {
			for x := 0; x < nx; x++ {
				i := (z*ny+y)*nx + x
				v := 0.0
				if inside(float64(x)*spacing[0], float64(y)*spacing[1], float64(z)*spacing[2]) {
					v = 100
					p.truth[i] = true
				}
				p.values[i] = v + noise*rng.NormFloat64()
			}
		}
	}
	p.volume = LevelSetVolume{Columns: nx, Rows: ny, Slices: nz, SpacingMM: spacing, ValueAt: func(x, y, z int) (float64, bool) {
		return p.values[(z*ny+y)*nx+x], true
	}}
	return p
}

// sphere centred at the middle of a 48×48×32 grid of 1×1×1.5 mm voxels,
// radius 10 mm.
func levelSetSpherePhantom(noise float64) *levelSetPhantom {
	c := [3]float64{24, 24, 24}
	return newLevelSetPhantom(48, 48, 32, [3]float64{1, 1, 1.5}, noise, func(x, y, z float64) bool {
		return sq(x-c[0])+sq(y-c[1])+sq(z-c[2]) <= 100
	}, RegionGrowSeed{X: 24, Y: 24, Slice: 16})
}

// cylinder along z of radius 7 mm and length 24 mm in a 40×40×24 grid of
// 0.8×0.8×2 mm voxels.
func levelSetCylinderPhantom(noise float64) *levelSetPhantom {
	return newLevelSetPhantom(40, 40, 24, [3]float64{0.8, 0.8, 2}, noise, func(x, y, z float64) bool {
		return sq(x-16)+sq(y-16) <= 49 && z >= 12 && z <= 36
	}, RegionGrowSeed{X: 20, Y: 20, Slice: 12})
}

// levelSetDiscPhantom is a single image with a disc of radius 12 mm.
func levelSetDiscPhantom(noise float64) *levelSetPhantom {
	return newLevelSetPhantom(64, 64, 1, [3]float64{0.7, 0.7, 0}, noise, func(x, y, _ float64) bool {
		return sq(x-22)+sq(y-22) <= 144
	}, RegionGrowSeed{X: 31, Y: 31})
}

func (p *levelSetPhantom) dice(t *testing.T, result *LevelSetResult) float64 {
	t.Helper()
	v := p.volume
	both, a, b := 0, 0, 0
	for z := 0; z < v.Slices; z++ {
		mask := result.Masks[z]
		for y := 0; y < v.Rows; y++ {
			for x := 0; x < v.Columns; x++ {
				got := mask != nil && mask.Get(x, y)
				want := p.truth[(z*v.Rows+y)*v.Columns+x]
				if got {
					a++
				}
				if want {
					b++
				}
				if got && want {
					both++
				}
			}
		}
	}
	if a != result.VoxelCount {
		t.Fatalf("masks hold %d voxels, VoxelCount %d", a, result.VoxelCount)
	}
	return 2 * float64(both) / float64(a+b)
}

func (p *levelSetPhantom) truthMasks(erode float64) map[int]*RasterMask {
	v := p.volume
	out := map[int]*RasterMask{}
	for z := 0; z < v.Slices; z++ {
		for y := 0; y < v.Rows; y++ {
			for x := 0; x < v.Columns; x++ {
				if !p.truth[(z*v.Rows+y)*v.Columns+x] {
					continue
				}
				if out[z] == nil {
					out[z] = NewRasterMask(v.Columns, v.Rows)
				}
				out[z].Set(x, y, true)
			}
		}
	}
	return out
}

// TestLevelSetSegmentsSyntheticPhantoms segments a sphere, a capped
// cylinder on anisotropic voxels and a disc with noise (σ = 15 for a
// contrast of 100) from one click. The fast marching threshold stops where
// the edge speed falls, about a voxel inside the edge crest, which costs a
// voxel layer of these 7-12 mm objects; the level sets are drawn onto the
// edge (or the interval bound) and are held to a higher Dice.
func TestLevelSetSegmentsSyntheticPhantoms(t *testing.T) {
	phantoms := []struct {
		name string
		make func(noise float64) *levelSetPhantom
	}{
		{"sphere", levelSetSpherePhantom},
		{"cylinder", levelSetCylinderPhantom},
		{"disc 2D", levelSetDiscPhantom},
	}
	methods := []struct {
		name    string
		minDice float64
		run     func(p *levelSetPhantom) (*LevelSetResult, error)
	}{
		{"fast marching", 0.8, func(p *levelSetPhantom) (*LevelSetResult, error) {
			return FastMarchingSegment(context.Background(), p.volume, []RegionGrowSeed{p.seed}, FastMarchingSegmentParams{StoppingTime: 20}, LevelSetOptions{})
		}},
		{"geodesic active contour", 0.95, func(p *levelSetPhantom) (*LevelSetResult, error) {
			return GeodesicActiveContour(context.Background(), p.volume, LevelSetInitialization{Seeds: []RegionGrowSeed{p.seed}}, DefaultGeodesicActiveContourParams(), LevelSetOptions{MarginMM: 20})
		}},
		{"threshold level set", 0.95, func(p *levelSetPhantom) (*LevelSetResult, error) {
			return ThresholdLevelSet(context.Background(), p.volume, LevelSetInitialization{Seeds: []RegionGrowSeed{p.seed}}, DefaultThresholdLevelSetParams(50, 150), LevelSetOptions{MarginMM: 20})
		}},
		// A range of the object's mean ± 2.5 SD leaves a noisy speed.
		{"threshold level set ±2.5 SD", 0.95, func(p *levelSetPhantom) (*LevelSetResult, error) {
			return ThresholdLevelSet(context.Background(), p.volume, LevelSetInitialization{Seeds: []RegionGrowSeed{p.seed}}, DefaultThresholdLevelSetParams(62.5, 137.5), LevelSetOptions{MarginMM: 20})
		}},
	}
	for _, phantom := range phantoms {
		p := phantom.make(15)
		for _, method := range methods {
			t.Run(phantom.name+"/"+method.name, func(t *testing.T) {
				result, err := method.run(p)
				if err != nil {
					t.Fatal(err)
				}
				dice := p.dice(t, result)
				t.Logf("Dice %.4f, %d voxels, %d iterations (converged %v, RMS %.4g), alpha %.3g beta %.3g, region %v+%v", dice, result.VoxelCount, result.Iterations, result.Converged, result.RMSChange, result.Alpha, result.Beta, result.RegionOrigin, result.RegionSize)
				if dice < method.minDice {
					t.Fatalf("Dice %.4f below %.2f", dice, method.minDice)
				}
				if method.minDice > 0.9 && !result.Converged {
					t.Fatalf("not converged after %d iterations", result.Iterations)
				}
				if result.TouchesRegionBoundary {
					t.Fatalf("region reaches the sub-volume boundary")
				}
			})
		}
	}
}

func TestGeodesicActiveContourRefinesGrownRegion(t *testing.T) {
	for _, tc := range []struct {
		name        string
		propagation float64
		masks       func(p *levelSetPhantom) map[int]*RasterMask
	}{
		// A region grown too small (radius 6 instead of 10 mm) expands to
		// the edge; one that leaked into a bar of background shrinks back
		// to it.
		{"undersegmented", 1, func(p *levelSetPhantom) map[int]*RasterMask {
			out := map[int]*RasterMask{}
			for z := 0; z < p.volume.Slices; z++ {
				for y := 0; y < p.volume.Rows; y++ {
					for x := 0; x < p.volume.Columns; x++ {
						if sq(float64(x)-24)+sq(float64(y)-24)+sq(float64(z)*1.5-24) <= 36 {
							if out[z] == nil {
								out[z] = NewRasterMask(p.volume.Columns, p.volume.Rows)
							}
							out[z].Set(x, y, true)
						}
					}
				}
			}
			return out
		}},
		{"leaked", -1, func(p *levelSetPhantom) map[int]*RasterMask {
			out := p.truthMasks(0)
			for z := 14; z <= 18; z++ {
				out[z].SetRun(23, 34, 44)
				out[z].SetRun(24, 34, 44)
				out[z].SetRun(25, 34, 44)
			}
			return out
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			p := levelSetSpherePhantom(15)
			params := DefaultGeodesicActiveContourParams()
			params.PropagationScaling = tc.propagation
			result, err := GeodesicActiveContour(context.Background(), p.volume, LevelSetInitialization{Masks: tc.masks(p)}, params, LevelSetOptions{MarginMM: 10})
			if err != nil {
				t.Fatal(err)
			}
			dice := p.dice(t, result)
			t.Logf("Dice %.4f after %d iterations", dice, result.Iterations)
			if dice < 0.95 {
				t.Fatalf("Dice %.4f below 0.95", dice)
			}
		})
	}
}

func TestLevelSetErrorsAndLimits(t *testing.T) {
	p := levelSetSpherePhantom(5)
	seeds := []RegionGrowSeed{p.seed}
	init := LevelSetInitialization{Seeds: seeds}
	ctx := context.Background()
	cancelled, cancel := context.WithCancel(ctx)
	cancel()
	for _, tc := range []struct {
		name string
		run  func() error
		want error
	}{
		{"no seeds", func() error {
			_, err := GeodesicActiveContour(ctx, p.volume, LevelSetInitialization{}, DefaultGeodesicActiveContourParams(), LevelSetOptions{})
			return err
		}, ErrLevelSetEmpty},
		{"seed outside", func() error {
			_, err := FastMarchingSegment(ctx, p.volume, []RegionGrowSeed{{X: -1}}, FastMarchingSegmentParams{StoppingTime: 5}, LevelSetOptions{})
			return err
		}, ErrLevelSetInvalidInput},
		{"missing stopping time", func() error {
			_, err := FastMarchingSegment(ctx, p.volume, seeds, FastMarchingSegmentParams{}, LevelSetOptions{})
			return err
		}, ErrLevelSetInvalidInput},
		{"inverted interval", func() error {
			_, err := ThresholdLevelSet(ctx, p.volume, init, DefaultThresholdLevelSetParams(10, 5), LevelSetOptions{})
			return err
		}, ErrLevelSetInvalidInput},
		{"negative curvature", func() error {
			params := DefaultGeodesicActiveContourParams()
			params.CurvatureScaling = -1
			_, err := GeodesicActiveContour(ctx, p.volume, init, params, LevelSetOptions{})
			return err
		}, ErrLevelSetInvalidInput},
		{"bad mask size", func() error {
			_, err := GeodesicActiveContour(ctx, p.volume, LevelSetInitialization{Masks: map[int]*RasterMask{0: NewRasterMask(3, 3)}}, DefaultGeodesicActiveContourParams(), LevelSetOptions{})
			return err
		}, ErrLevelSetInvalidInput},
		{"voxel budget", func() error {
			_, err := GeodesicActiveContour(ctx, p.volume, init, DefaultGeodesicActiveContourParams(), LevelSetOptions{Limits: LevelSetLimits{MaxVoxels: 1000}})
			return err
		}, ErrLevelSetResourceLimit},
		{"memory budget", func() error {
			_, err := ThresholdLevelSet(ctx, p.volume, init, DefaultThresholdLevelSetParams(50, 150), LevelSetOptions{Limits: LevelSetLimits{MaxBytes: 1 << 20}})
			return err
		}, ErrLevelSetResourceLimit},
		{"cancelled", func() error {
			_, err := GeodesicActiveContour(cancelled, p.volume, init, DefaultGeodesicActiveContourParams(), LevelSetOptions{})
			return err
		}, context.Canceled},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if err := tc.run(); !errors.Is(err, tc.want) {
				t.Fatalf("error = %v, want %v", err, tc.want)
			}
		})
	}
}

func TestLevelSetCancelsDuringEvolution(t *testing.T) {
	p := levelSetSpherePhantom(15)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	_, err := GeodesicActiveContour(ctx, p.volume, LevelSetInitialization{Seeds: []RegionGrowSeed{p.seed}}, DefaultGeodesicActiveContourParams(), LevelSetOptions{
		Progress: func(progress LevelSetProgress) {
			if progress.Iteration == 3 {
				cancel()
			}
		},
	})
	if !errors.Is(err, ErrLevelSetCancelled) || !errors.Is(err, context.Canceled) {
		t.Fatalf("error = %v", err)
	}
}

func TestLevelSetSubVolumeIsBoundedInMillimetres(t *testing.T) {
	p := levelSetSpherePhantom(15)
	// With a 4 mm margin the contour cannot reach the 10 mm sphere's edge
	// from a seed region of 3 mm: it is cut by the sub-volume and says so.
	result, err := GeodesicActiveContour(context.Background(), p.volume, LevelSetInitialization{Seeds: []RegionGrowSeed{p.seed}}, DefaultGeodesicActiveContourParams(), LevelSetOptions{MarginMM: 4})
	if err != nil {
		t.Fatal(err)
	}
	if want := [3]int{9, 9, 7}; result.RegionSize != want || result.RegionOrigin != [3]int{20, 20, 13} {
		t.Fatalf("sub-volume %v+%v, want [20 20 13]+%v", result.RegionOrigin, result.RegionSize, want)
	}
	if !result.TouchesRegionBoundary {
		t.Fatal("a region cut by the sub-volume must be reported")
	}
	for slice, mask := range result.Masks {
		mask.ForEachPixel(func(x, y int) {
			if x < 20 || x > 28 || y < 20 || y > 28 || slice < 13 || slice > 19 {
				t.Fatalf("voxel (%d, %d, %d) outside the sub-volume", x, y, slice)
			}
		})
	}
}

func TestLevelSetSkipsUnavailableVoxels(t *testing.T) {
	p := levelSetDiscPhantom(5)
	valueAt := p.volume.ValueAt
	p.volume.ValueAt = func(x, y, z int) (float64, bool) {
		if x < 31 {
			return 0, false
		}
		return valueAt(x, y, z)
	}
	result, err := ThresholdLevelSet(context.Background(), p.volume, LevelSetInitialization{Seeds: []RegionGrowSeed{p.seed}}, DefaultThresholdLevelSetParams(50, 150), LevelSetOptions{MarginMM: 15})
	if err != nil {
		t.Fatal(err)
	}
	if result.VoxelCount == 0 {
		t.Fatal("empty region")
	}
	result.Masks[0].ForEachPixel(func(x, _ int) {
		if x < 31 {
			t.Fatalf("unavailable column %d joined the region", x)
		}
	})
}

func BenchmarkGeodesicActiveContourSphere(b *testing.B) {
	p := levelSetSpherePhantom(15)
	init := LevelSetInitialization{Seeds: []RegionGrowSeed{p.seed}}
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if _, err := GeodesicActiveContour(context.Background(), p.volume, init, DefaultGeodesicActiveContourParams(), LevelSetOptions{MarginMM: 20}); err != nil {
			b.Fatal(err)
		}
	}
}

func BenchmarkThresholdLevelSetSphere(b *testing.B) {
	p := levelSetSpherePhantom(15)
	init := LevelSetInitialization{Seeds: []RegionGrowSeed{p.seed}}
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if _, err := ThresholdLevelSet(context.Background(), p.volume, init, DefaultThresholdLevelSetParams(50, 150), LevelSetOptions{MarginMM: 20}); err != nil {
			b.Fatal(err)
		}
	}
}
