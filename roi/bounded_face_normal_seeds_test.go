package roi

import (
	"context"
	"errors"
	"math"
	"testing"
)

func boundedSeedState(spacing [3]float64, dims int, values []float32) *levelSetState {
	size := [3]int{9, 9, 1}
	if dims == 3 {
		size[2] = 9
	}
	g := &levelSetGrid{ctx: context.Background(), nx: size[0], ny: size[1], nz: size[2], n: size[0] * size[1] * size[2], size: size, stride: [3]int{1, size[0], size[0] * size[1]}, dims: dims, h: spacing, hmin: math.Min(spacing[0], spacing[1])}
	if dims == 3 {
		g.hmin = math.Min(g.hmin, spacing[2])
	}
	return &levelSetState{levelSetGrid: g, phi: values, width: float32(float64(dims+1) * g.hmin), band: make([]int32, 0, g.n)}
}

func TestShapeBoundedFaceNormalIndependentPlanes(t *testing.T) {
	// Distance to the plane through axial half-face intersections:
	// isotropic2D 1/sqrt(8), isotropic3D1/sqrt(12), anisotropic
	// .35/.5 intersections give1/sqrt(1/.35²+1/.5²).
	cases := []struct {
		name       string
		spacing    [3]float64
		dims, axes int
		want       float64
	}{
		{"iso2", [3]float64{1, 1, 1}, 2, 2, 1 / math.Sqrt(8)},
		{"iso3", [3]float64{1, 1, 1}, 3, 3, 1 / math.Sqrt(12)},
		{"anis2", [3]float64{.7, 1, 1}, 2, 2, 1 / math.Sqrt(1/(.35*.35)+4)},
		{"axisSwap", [3]float64{1, .7, 1}, 2, 2, 1 / math.Sqrt(1/(.35*.35)+4)},
		{"anis3", [3]float64{.5, 1, 1.5}, 3, 3, 1 / math.Sqrt(16+4+16.0/9)},
		{"singleX", [3]float64{.5, 1, 1.5}, 3, 1, .25},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			for reflection := 0; reflection < 1<<c.axes; reflection++ {
				for _, phase := range []float32{-1, 1} {
					n := 81
					if c.dims == 3 {
						n = 729
					}
					phi := make([]float32, n)
					s := boundedSeedState(c.spacing, c.dims, phi)
					for i := range phi {
						pos := s.coordsArray(i)
						v := float32(.5)
						for a := 0; a < c.axes; a++ {
							direction := 1
							if reflection&(1<<a) != 0 {
								direction = -1
							}
							v += float32(direction * (pos[a] - 4))
						}
						phi[i] = phase * v
					}
					if err := s.initializeInterfaceAnchorsWithProfile(BoundedFaceNormalSeeds); err != nil {
						t.Fatal(err)
					}
					z := 0
					if c.dims == 3 {
						z = 4
					}
					got := phi[s.index(4, 4, z)]
					want := float32(c.want) * phase
					if got != want {
						t.Fatalf("reflection%d phase%g seed%.9g want%.9g", reflection, phase, got, want)
					}
				}
			}
		})
	}
}

func TestShapeBoundedFaceNormalUnequalFractionsAndZero(t *testing.T) {
	for _, phase := range []float32{-1, 1} {
		phi := make([]float32, 81)
		for i := range phi {
			phi[i] = phase * .25
		}
		s := boundedSeedState([3]float64{.7, 1, 1}, 2, phi)
		center := s.index(4, 4, 0)
		phi[center-1] = -phase * .75
		phi[center-9] = -phase * .25
		// Face intersections .175mm and .5mm, independent plane normal.
		want := float32(1/math.Sqrt(1/(.175*.175)+4)) * phase
		if err := s.initializeInterfaceAnchorsWithProfile(BoundedFaceNormalSeeds); err != nil {
			t.Fatal(err)
		}
		if phi[center] != want {
			t.Fatalf("unequal fractions: %g want%g", phi[center], want)
		}
	}
	phi := make([]float32, 81)
	for i := range phi {
		phi[i] = 1
	}
	s := boundedSeedState([3]float64{.7, 1, 1}, 2, phi)
	center := s.index(4, 4, 0)
	phi[center] = 0
	phi[center-1] = -1
	if err := s.initializeInterfaceAnchorsWithProfile(BoundedFaceNormalSeeds); err != nil {
		t.Fatal(err)
	}
	if phi[center] != 0 {
		t.Fatal("zero crossing changed")
	}
}

func TestShapeBoundedFaceNormalSingleAxisGauge(t *testing.T) {
	for axis := 0; axis < 3; axis++ {
		phi := make([]float32, 729)
		s := boundedSeedState([3]float64{.5, 1, 1.5}, 3, phi)
		for i := range phi {
			p := s.coordsArray(i)
			phi[i] = float32(p[axis]-4) + .25
		}
		if err := s.initializeInterfaceAnchorsWithProfile(BoundedFaceNormalSeeds); err != nil {
			t.Fatal(err)
		}
		if phi[s.index(4, 4, 4)] != .125 {
			t.Fatal("single-axis city-block gauge changed")
		}
	}
}

func TestShapeBoundedFaceNormalDefaultAndInvalidBeforeRead(t *testing.T) {
	// Default retains exact L1 seeds for an obliqueplane; no oldtest rewritten.
	phi := make([]float32, 81)
	s := boundedSeedState([3]float64{.7, 1, 1}, 2, phi)
	for i := range phi {
		p := s.coordsArray(i)
		phi[i] = float32(p[0]+p[1]-8) + .25
	}
	if err := s.initializeInterfaceAnchors(); err != nil {
		t.Fatal(err)
	}
	if phi[s.index(4, 4, 0)] != float32(.7*.25) {
		t.Fatal("default city-block seed changed")
	}
	for _, p := range []ShapeDetectionParams{{Initialization: 255}, {Evolution: DenseBand, Initialization: BoundedFaceNormalSeeds}} {
		reads := 0
		volume := LevelSetVolume{Columns: 9, Rows: 9, Slices: 1, ValueAt: func(x, y, z int) (float64, bool) { reads++; return 1, true }}
		r, err := ShapeDetectionLevelSet(nil, volume, LevelSetInitialization{}, p, LevelSetOptions{})
		if r != nil || !errors.Is(err, ErrLevelSetInvalidInput) || reads != 0 {
			t.Fatalf("invalid option result%v err%v reads%d", r, err, reads)
		}
	}
}

func TestShapeBoundedFaceNormalCancelledBeforeWrites(t *testing.T) {
	phi := make([]float32, 81)
	for i := range phi {
		phi[i] = 1
	}
	s := boundedSeedState([3]float64{.7, 1, 1}, 2, phi)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	s.ctx = ctx
	if err := s.initializeInterfaceAnchorsWithProfile(BoundedFaceNormalSeeds); !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
	for _, v := range phi {
		if v != 1 {
			t.Fatal("cancel changed input")
		}
	}
}

// TestShapeBoundedFaceNormalOriginalFrozenITK qualifies the exact two original
// edge_levelset fixtures under the explicit profile. Dense historical controls
// remain in TestEdgeLevelSetFrozenITK, without changed masks or tolerances.
func TestShapeBoundedFaceNormalOriginalFrozenITK(t *testing.T) {
	original, _ := readFrozenEdgeCases(t)
	shapes := 0
	for _, c := range original {
		if c.Kind != "shape" {
			continue
		}
		shapes++
		t.Run(c.Name, func(t *testing.T) {
			if c.Propagation != 1 || c.Curvature != .2 || c.MaxIterations != 300 || c.MaximumRMS != .0005 {
				t.Fatal("original frozen settings changed")
			}
			init := LevelSetInitialization{Masks: map[int]*RasterMask{}}
			for z := 0; z < c.Dims[2]; z++ {
				m := NewRasterMask(c.Dims[0], c.Dims[1])
				for y := 0; y < c.Dims[1]; y++ {
					for x := 0; x < c.Dims[0]; x++ {
						if c.Initial[(z*c.Dims[1]+y)*c.Dims[0]+x] != 0 {
							m.Set(x, y, true)
						}
					}
				}
				init.Masks[z] = m
			}
			volume := LevelSetVolume{Columns: c.Dims[0], Rows: c.Dims[1], Slices: c.Dims[2], SpacingMM: c.Spacing, ValueAt: func(x, y, z int) (float64, bool) { return c.Feature[(z*c.Dims[1]+y)*c.Dims[0]+x], true }}
			p := ShapeDetectionParams{Evolution: InterfaceLocal, Initialization: BoundedFaceNormalSeeds, SurfaceSampling: GridCentres, Input: EdgePotentialImage, Stopping: RMSOnly, CurvatureConvention: SumCurvature, PropagationScaling: c.Propagation, CurvatureScaling: c.Curvature, MaxIterations: c.MaxIterations, MaximumRMSChange: c.MaximumRMS}
			r, err := ShapeDetectionLevelSet(nil, volume, init, p, LevelSetOptions{MarginMM: 1000})
			if err != nil || r == nil {
				t.Fatalf("original profile: %v", err)
			}
			got, want := NewVolumeMask(c.Dims[0], c.Dims[1], c.Dims[2], c.Spacing), NewVolumeMask(c.Dims[0], c.Dims[1], c.Dims[2], c.Spacing)
			for i := range c.Mask {
				x, y, z := i%c.Dims[0], i/c.Dims[0]%c.Dims[1], i/(c.Dims[0]*c.Dims[1])
				got.Voxels[i] = r.Masks[z] != nil && r.Masks[z].Get(x, y)
				want.Voxels[i] = c.Mask[i] != 0
			}
			metrics, err := CompareMasks(context.Background(), got, want)
			if err != nil {
				t.Fatal(err)
			}
			h := math.Max(c.Spacing[0], c.Spacing[1])
			if c.Dims[2] > 1 {
				h = math.Max(h, c.Spacing[2])
			}
			t.Logf("ORIGINAL_SHAPE profile=InterfaceLocal/BoundedFaceNormalSeeds id=%s Go=%d ITK=%d Dice=%.9f HD=%.9f iterations=%d RMS=%.9g stop=%d sourceBoundary=%t", c.Name, r.VoxelCount, want.Count(), metrics.Dice, metrics.HausdorffMM, r.Iterations, r.RMSChange, r.StopReason, r.TouchesVolumeBoundary)
			if !finiteNonNegative(metrics.Dice) || !finiteNonNegative(metrics.HausdorffMM) || metrics.Dice < .95 || metrics.HausdorffMM > 2*h {
				t.Fatalf("original Dice>=.95 and HD<=%g criteria: %+v", 2*h, metrics)
			}
		})
	}
	if shapes != 2 {
		t.Fatalf("original shape cardinality %d want2", shapes)
	}
}
