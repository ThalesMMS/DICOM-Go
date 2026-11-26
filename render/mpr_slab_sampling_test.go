package render

import (
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"math"
	"testing"
)

// The reference profile and its half-position values are independent of the
// planner and distinguish sample spacing from endpoint weighting.
var slabProfile = []float64{40, 0, 100, 0, 20}

func slabProfileStack(axis MPRPlane, geometry bool) *Stack {
	s := &Stack{PixelSpacing: []float64{2, 3}, SliceThickness: 4}
	for z := 0; z < 5; z++ {
		data := make([]byte, 25)
		for y := 0; y < 5; y++ {
			for x := 0; x < 5; x++ {
				k := z
				if axis == MPRPlaneCoronal {
					k = y
				}
				if axis == MPRPlaneSagittal {
					k = x
				}
				data[y*5+x] = byte(slabProfile[k])
			}
		}
		f := volumeTestFrame(5, 5, data, z)
		f.Rescale = Rescale{Slope: 2, Intercept: -100}
		f.PixelSpacing = []float64{2, 3}
		f.ImagePosition[2] = float64(z) * 4
		if !geometry {
			f.ImagePosition = nil
			f.ImageOrientation = nil
		}
		s.Frames = append(s.Frames, f)
	}
	return s
}

func referenceSlabReduction(values []float64, mode SlabMode, trapezoid bool) float64 {
	sum, weights := 0.0, 0.0
	result := 2*values[0] - 100
	for i, stored := range values {
		v := 2*stored - 100
		if mode == SlabMIP && v > result {
			result = v
		}
		if mode == SlabMinIP && v < result {
			result = v
		}
		weight := 1.0
		if trapezoid && len(values) > 1 && (i == 0 || i == len(values)-1) {
			weight = .5
		}
		sum += v * weight
		weights += weight
	}
	if mode == SlabAverage {
		return sum / weights
	}
	if mode == SlabSum {
		return sum
	}
	return result
}

func TestScalarSlabSamplingEveryOrthogonalPath(t *testing.T) {
	refined := map[Interpolation][]float64{
		InterpolationLinear:  {40, 20, 0, 50, 100, 50, 0, 10, 20},
		InterpolationNearest: {40, 0, 0, 100, 100, 0, 0, 20, 20},
		InterpolationCubic:   {40, 13.75, 0, 53.75, 100, 55, 0, 3.75, 20},
	}
	for _, geometry := range []bool{true, false} {
		for _, axis := range []MPRPlane{MPRPlaneAxial, MPRPlaneCoronal, MPRPlaneSagittal} {
			for interpolation, fine := range refined {
				for _, fraction := range []float64{0, 1, .5} {
					for _, trap := range []bool{false, true} {
						for _, mode := range []SlabMode{SlabMIP, SlabMinIP, SlabAverage, SlabSum} {
							name := fmt.Sprintf("geometry=%v/%s/interp=%d/fraction=%g/trap=%v/mode=%d", geometry, axis, interpolation, fraction, trap, mode)
							t.Run(name, func(t *testing.T) {
								s := slabProfileStack(axis, geometry)
								p, err := RenderScalarSlabWithOptionsContext(context.Background(), s, axis, 2, 5, mode, MPRRenderOptions{Interpolation: interpolation, Slab: SlabSampling{SpacingFraction: fraction, Trapezoid: trap}})
								if err != nil {
									t.Fatal(err)
								}
								values := slabProfile
								if fraction == .5 {
									values = fine
								}
								want := referenceSlabReduction(values, mode, trap)
								w, h := p.Dimensions()
								for y := 0; y < h; y++ {
									for x := 0; x < w; x++ {
										got, ok := p.ValueAt(x, y)
										if !ok || math.Abs(got-want) > 1e-8 {
											t.Fatalf("(%d,%d)=%g/%v want %g", x, y, got, ok, want)
										}
									}
								}
							})
						}
					}
				}
			}
		}
	}
}

func TestScalarSlabSamplingTiltGeometry(t *testing.T) {
	for _, axis := range []MPRPlane{MPRPlaneCoronal, MPRPlaneSagittal} {
		s := slabProfileStack(axis, true)
		for z, f := range s.Frames {
			f.ImagePosition[1] = float64(z) * .5
		}
		vol, err := s.Volume()
		if err != nil {
			t.Fatal(err)
		}
		if !vol.Geometry().GantryTilted {
			t.Fatal("fixture must use the corrected tilt path")
		}
		// The first output row lies at z=16. The canonical regular grid
		// shifts acquired y by one row and fills its outside position with
		// modality zero. Refinement interpolates that already regularized grid.
		coarse := []float64{0, -20, -100, 100, -100}
		fine := []float64{0, -10, -20, -60, -100, 0, 100, 0, -100}
		if axis == MPRPlaneSagittal {
			coarse = []float64{-20, -100, 100, -100, -60}
			fine = []float64{-20, -60, -100, 0, 100, 0, -100, -80, -60}
		}
		for _, fraction := range []float64{1, .5} {
			for _, trap := range []bool{false, true} {
				p, err := RenderScalarSlabWithOptionsContext(context.Background(), s, axis, 2, 5, SlabAverage, MPRRenderOptions{Slab: SlabSampling{SpacingFraction: fraction, Trapezoid: trap}})
				if err != nil {
					t.Fatal(err)
				}
				w, h := p.Dimensions()
				wantH := 6
				if axis == MPRPlaneSagittal {
					wantH = 9
				}
				if w != 5 || h != wantH {
					t.Fatalf("corrected dimensions=%dx%d want5x%d", w, h, wantH)
				}
				got, ok := p.ValueAt(2, 0)
				if !ok {
					t.Fatal("missing corrected sample")
				}
				values := coarse
				if fraction == .5 {
					values = fine
				}
				sum, weights := 0.0, 0.0
				for i, v := range values {
					weight := 1.0
					if trap && (i == 0 || i == len(values)-1) {
						weight = .5
					}
					sum += weight * v
					weights += weight
				}
				if math.Abs(got-sum/weights) > 1e-6 {
					t.Fatalf("%s fraction%g trap%v got%g want%g", axis, fraction, trap, got, sum/weights)
				}
			}
		}
	}
}

func TestScalarSlabSamplingAdmissionAndSingleton(t *testing.T) {
	for _, fraction := range []float64{-1, 1.1, math.NaN(), math.Inf(1), math.SmallestNonzeroFloat64} {
		for _, axis := range []MPRPlane{MPRPlaneAxial, MPRPlaneCoronal, MPRPlaneSagittal} {
			p, err := RenderScalarSlabWithOptionsContext(context.Background(), slabProfileStack(axis, true), axis, 2, 5, SlabSum, MPRRenderOptions{Slab: SlabSampling{SpacingFraction: fraction}})
			if err == nil || p != nil {
				t.Fatalf("fraction%g admission=%v/%v", fraction, p, err)
			}
		}
	}
	for _, axis := range []MPRPlane{MPRPlaneAxial, MPRPlaneCoronal, MPRPlaneSagittal} {
		s := slabProfileStack(axis, true)
		p, err := RenderScalarSlabWithOptionsContext(context.Background(), s, axis, 2, 5, SlabSum, MPRRenderOptions{Slab: SlabSampling{SpacingFraction: .5}, Limits: MPRLimits{MaxSlabSamples: 8}})
		if p != nil || !errors.Is(err, ErrMPRResourceLimit) {
			t.Fatalf("quota=%v/%v", p, err)
		}
		for _, mode := range []SlabMode{SlabNone, SlabSum, SlabAverage} {
			p, err = RenderScalarSlabWithOptionsContext(context.Background(), s, axis, 2, 1, mode, MPRRenderOptions{Slab: SlabSampling{SpacingFraction: .5, Trapezoid: true}})
			if err != nil {
				t.Fatal(err)
			}
			v, ok := p.ValueAt(0, 0)
			if !ok || v != 100 {
				t.Fatalf("singleton=%g/%v", v, ok)
			}
		}
		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		p, err = RenderScalarSlabWithOptionsContext(ctx, s, axis, 2, 5, SlabSum, MPRRenderOptions{Slab: SlabSampling{SpacingFraction: .5, Trapezoid: true}})
		if p != nil || !errors.Is(err, context.Canceled) {
			t.Fatalf("cancel=%v/%v", p, err)
		}
	}
	if _, err := (SlabSampling{SpacingFraction: .5}).SampleCount(math.MaxInt, MPRLimits{}); !errors.Is(err, ErrMPRResourceLimit) {
		t.Fatal(err)
	}
	if _, err := (SlabSampling{}).SampleCount(0, MPRLimits{}); !errors.Is(err, ErrMPRInvalidInput) {
		t.Fatal(err)
	}
}

func TestScalarSlabThinVesselObliquePeak(t *testing.T) {
	// One-voxel-wide vessel parallel to Y. On a 45-degree X/Z ray its
	// trilinear field is 100*(1-|dx|)*(1-|dz|). Its peak is half a coarse
	// step from the slab center, so a half-step sample reaches it exactly.
	s := singleBrightVoxelStack(7, 3, 3, 3)
	for _, f := range s.Frames {
		for i := range f.PixelBytes {
			f.PixelBytes[i] = 0
		}
	}
	for y := 0; y < 7; y++ {
		s.Frames[3].PixelBytes[y*7+3] = 100
	}
	vol, err := s.Volume()
	if err != nil {
		t.Fatal(err)
	}
	root2 := math.Sqrt(.5)
	normal := Vec3{X: root2, Z: root2}
	center := Vec3{X: 3, Y: 3, Z: 3}.Sub(normal.Scale(.5))
	plane := Plane{Origin: center, U: Vec3{X: root2, Z: -root2}, V: Vec3{Y: 1}}
	results := map[float64]float64{}
	for _, fraction := range []float64{1, .5, .25} {
		p, err := ResliceObliqueSlabScalarWithOptionsContext(context.Background(), vol, plane, 1, 1, 3, SlabMIP, MPRResliceOptions{Slab: SlabSampling{SpacingFraction: fraction}})
		if err != nil {
			t.Fatal(err)
		}
		v, ok := p.ValueAt(0, 0)
		if !ok {
			t.Fatal("vessel invalid")
		}
		results[fraction] = v
	}
	wantCoarse := 100 * math.Pow(1-.5*root2, 2)
	if math.Abs(results[1]-wantCoarse) > 1e-9 || math.Abs(results[.5]-100) > 1e-9 || math.Abs(results[.25]-100) > 1e-9 {
		t.Fatalf("peak=%v coarse oracle=%g", results, wantCoarse)
	}
	t.Logf("synthetic trilinear peak: standard %.9f, half-step %.9f, quarter-step %.9f", results[1], results[.5], results[.25])
}

func TestScalarSlabSamplingObliqueWeightsAndMissingEndpoint(t *testing.T) {
	s := slabProfileStack(MPRPlaneAxial, true)
	vol, err := s.Volume()
	if err != nil {
		t.Fatal(err)
	}
	plane := Plane{Origin: Vec3{Z: 8}, U: Vec3{X: 12}, V: Vec3{Y: 8}}
	for _, fraction := range []float64{1, .5} {
		for _, trap := range []bool{false, true} {
			for _, mode := range []SlabMode{SlabMIP, SlabMinIP, SlabAverage, SlabSum} {
				p, e := ResliceObliqueSlabScalarWithOptionsContext(context.Background(), vol, plane, 5, 5, 5, mode, MPRResliceOptions{Slab: SlabSampling{SpacingFraction: fraction, Trapezoid: trap}})
				if e != nil {
					t.Fatal(e)
				}
				values := slabProfile
				if fraction == .5 {
					values = []float64{40, 20, 0, 50, 100, 50, 0, 10, 20}
				}
				got, ok := p.ValueAt(2, 2)
				want := referenceSlabReduction(values, mode, trap)
				if !ok || math.Abs(got-want) > 1e-9 {
					t.Fatalf("oblique fraction%g trap%v mode%d=%g/%v want%g", fraction, trap, mode, got, ok, want)
				}
			}
		}
	}
	// A failed source endpoint must not be replaced by a weighted valid zero in
	// the geometry-free fallback. Endpoint weights belong to slab positions.
	s = slabProfileStack(MPRPlaneAxial, false)
	s.Frames[0].DecodeErr = errors.New("synthetic missing endpoint")
	s.Frames[0].PixelBytes = nil
	for _, fraction := range []float64{1, .5} {
		p, e := RenderScalarSlabWithOptionsContext(context.Background(), s, MPRPlaneAxial, 2, 5, SlabAverage, MPRRenderOptions{Slab: SlabSampling{SpacingFraction: fraction, Trapezoid: true}})
		if e != nil {
			t.Fatal(e)
		}
		got, ok := p.ValueAt(2, 2)
		want := -130.0 / 3.5
		if fraction == .5 {
			want = -210.0 / 6.5
		}
		if !ok || math.Abs(got-want) > 1e-9 {
			t.Fatalf("missing endpoint fraction%g=%g/%v want%g", fraction, got, ok, want)
		}
	}
}

func TestScalarSlabSamplingSignedBigEndianAndWorkLimit(t *testing.T) {
	levels := []int16{-30, 12, -5, 60, 8}
	s := slabProfileStack(MPRPlaneAxial, true)
	for z, f := range s.Frames {
		f.Metadata.BitsAllocated = 16
		f.Metadata.BitsStored = 16
		f.Metadata.HighBit = 15
		f.Metadata.PixelRepresentation = 1
		f.ByteOrder = binary.BigEndian
		f.PixelBytes = make([]byte, 50)
		for i := 0; i < 25; i++ {
			binary.BigEndian.PutUint16(f.PixelBytes[2*i:], uint16(levels[z]))
		}
		f.Rescale = Rescale{Slope: 1.25, Intercept: -13.5}
	}
	fine := []float64{-30, -9, 12, 3.5, -5, 27.5, 60, 34, 8}
	sum := 0.0
	for k, v := range fine {
		weight := 1.0
		if k == 0 || k == 8 {
			weight = .5
		}
		sum += weight * (1.25*v - 13.5)
	}
	p, e := RenderScalarSlabWithOptionsContext(context.Background(), s, MPRPlaneAxial, 2, 5, SlabSum, MPRRenderOptions{Slab: SlabSampling{SpacingFraction: .5, Trapezoid: true}})
	if e != nil {
		t.Fatal(e)
	}
	v, ok := p.ValueAt(2, 2)
	if !ok || v != sum {
		t.Fatalf("signed BE=%g/%v want%g", v, ok, sum)
	}
	// Same retained pixels; the finer projection needs more admitted work.
	opts := MPRRenderOptions{Limits: MPRLimits{MaxWorkingBytes: 25 * (9 + 8*4)}}
	if p, e = RenderScalarSlabWithOptionsContext(context.Background(), s, MPRPlaneAxial, 2, 5, SlabSum, opts); e != nil || p == nil {
		t.Fatalf("coarse work=%v/%v", p, e)
	}
	opts.Slab.SpacingFraction = .5
	if p, e = RenderScalarSlabWithOptionsContext(context.Background(), s, MPRPlaneAxial, 2, 5, SlabSum, opts); p != nil || !errors.Is(e, ErrMPRResourceLimit) {
		t.Fatalf("fine work=%v/%v", p, e)
	}
}

func TestScalarSlabSamplingTiltSourceGeometry(t *testing.T) {
	for _, axis := range []MPRPlane{MPRPlaneCoronal, MPRPlaneSagittal} {
		s := slabProfileStack(axis, true)
		for z, f := range s.Frames {
			f.ImagePosition[1] = float64(z) * .5
		}
		for _, fraction := range []float64{1, .5} {
			for _, trap := range []bool{false, true} {
				for _, mode := range []SlabMode{SlabMIP, SlabMinIP, SlabAverage, SlabSum} {
					opts := MPRRenderOptions{GantryTiltMode: GantryTiltSourceGeometry, Slab: SlabSampling{SpacingFraction: fraction, Trapezoid: trap}}
					p, e := RenderScalarSlabWithOptionsContext(context.Background(), s, axis, 2, 5, mode, opts)
					if e != nil {
						t.Fatal(e)
					}
					values := slabProfile
					if fraction == .5 {
						values = []float64{40, 20, 0, 50, 100, 50, 0, 10, 20}
					}
					want := referenceSlabReduction(values, mode, trap)
					got, ok := p.ValueAt(2, 0)
					if !ok || math.Abs(got-want) > 1e-8 {
						t.Fatalf("source grid %s fraction%g trap%v mode%d=%g/%v want%g", axis, fraction, trap, mode, got, ok, want)
					}
					w, h := p.Dimensions()
					wantH := 7
					if axis == MPRPlaneSagittal {
						wantH = 10
					}
					if w != 5 || h != wantH {
						t.Fatalf("source spacing dimensions=%dx%d want5x%d", w, h, wantH)
					}
				}
			}
		}
	}
	if got, e := (SlabSampling{SpacingFraction: .3}).SampleCount(5, MPRLimits{}); e != nil || got != 14 {
		t.Fatalf("rounded count=%d/%v want14", got, e)
	}
}
