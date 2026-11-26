package render

import (
	"context"
	"errors"
	"image"
	"image/color"
	"math"
	"testing"
)

func TestResolveVRRaySamplingPhysicalSpacingAndBudget(t *testing.T) {
	q := DefaultVRQuality(8, 8)
	plan, err := ResolveVRRaySampling([3]float64{0.4, 0.8, 3}, 600, q)
	if err != nil || plan.SampleDistanceMM != 0.2 || plan.RequiredSteps != 3001 || plan.MaxSteps != 4096 {
		t.Fatalf("plan=%+v err=%v", plan, err)
	}
	for _, cap := range []int{4096, 8192} {
		q.MaxSteps = cap
		got, err := ResolveVRRaySampling([3]float64{0.4, 0.8, 3}, 600, q)
		if err != nil || got.SampleDistanceMM != plan.SampleDistanceMM {
			t.Fatalf("cap changed sampling: %+v %v", got, err)
		}
	}
	q.MaxSteps = 3000
	if _, err := ResolveVRRaySampling([3]float64{0.4, 0.8, 3}, 600, q); !errors.Is(err, ErrVRSampleBudgetExceeded) {
		t.Fatal(err)
	}
	for _, value := range []float64{-1, 0, math.NaN(), math.Inf(1)} {
		if _, err := ResolveVRRaySampling([3]float64{value, 1, 1}, 10, VRQuality{}); !errors.Is(err, ErrInvalidVRSampling) {
			t.Fatalf("spacing %g: %v", value, err)
		}
	}
	for _, value := range []float64{-1, math.NaN(), math.Inf(1)} {
		if _, err := ResolveVRRaySampling([3]float64{1, 1, 1}, 10, VRQuality{SpacingFraction: value}); !errors.Is(err, ErrInvalidVRSampling) {
			t.Fatalf("fraction %g: %v", value, err)
		}
	}
	q = DefaultVRQuality(8, 8)
	q.AdaptiveStep = VRAdaptiveStep{Max: 4, Min: 0.25}
	if _, err := ResolveVRRaySampling([3]float64{0.4, 0.8, 3}, 600, q); !errors.Is(err, ErrVRSampleBudgetExceeded) {
		t.Fatalf("adaptive minimum not bounded: %v", err)
	}
}

func thinPlaneVolume(t testing.TB, spacing, sliceSpacing float64, depth int) *Volume {
	t.Helper()
	grid := fusionGrid{cols: 7, rows: 7, depth: depth, pixelSpacing: spacing, spacing: sliceSpacing, axisX: Vec3{X: 1}, axisY: Vec3{Y: 1}, normal: Vec3{Z: 1}}
	center := float64(depth/2) * sliceSpacing
	if spacing == sliceSpacing {
		center -= sliceSpacing
	}
	stack := buildFusionStack(grid, func(p Vec3) byte {
		if math.Abs(p.Z-center) < sliceSpacing/2 {
			return 255
		}
		return 0
	})
	vol, err := BuildVolume(stack)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { vol.Close() })
	return vol
}

func TestVRPhysicalSamplingDetectsOneVoxelPlane(t *testing.T) {
	for _, tc := range []struct {
		name           string
		spacing, slice float64
		depth          int
	}{
		{"isotropic", 0.7, 0.7, 1025}, {"anisotropic", 0.4, 3, 257},
	} {
		t.Run(tc.name, func(t *testing.T) {
			vol := thinPlaneVolume(t, tc.spacing, tc.slice, tc.depth)
			cam := NewVRCamera(vol.BoundingRadiusMM())
			cam.FitVolume(vol)
			q := DefaultVRQuality(3, 3)
			q.Jitter = false
			q.Scattering = false
			window := WindowLevel{Center: 128, Width: 256}
			for _, mode := range []VRMode{VRModeDVR, VRModeMIP} {
				preset := opaqueAbovePreset()
				preset.Mode = mode
				frame, err := RenderVRContext(context.Background(), vol, cam, preset, window, false, nil, q)
				if err != nil || frame == nil {
					t.Fatalf("mode=%v: %v", mode, err)
				}
				if vrBrightness(frame.(*image.NRGBA), 1, 1) < 120 {
					t.Fatalf("mode %v missed one-voxel plane", mode)
				}
				q.MaxSteps = 8192
				higher, err := RenderVRContext(context.Background(), vol, cam, preset, window, false, nil, q)
				if err != nil || !sameImage(frame, higher) {
					t.Fatalf("cap changed pixels: %v", err)
				}
				q.MaxSteps = DefaultVRMaxSteps
			}
			frame, err := RenderVRSurfaceContext(context.Background(), vol, cam, 127.5, color.NRGBA{R: 255, G: 255, B: 255, A: 255}, false, nil, q)
			if err != nil || frame == nil || vrBrightness(frame.(*image.NRGBA), 1, 1) < 200 {
				t.Fatalf("surface missed plane: %v", err)
			}
			if tc.name == "isotropic" {
				// An independent, unaccelerated ray exposes the old phase:
				// diagonal/512 lands on either side of this one-voxel plane.
				sampler, ok := newVolumeSampler(vol)
				if !ok {
					t.Fatal("sampler")
				}
				size := vol.PhysicalSize()
				preset := opaqueAbovePreset()
				preset.Mode = VRModeMIP
				projection := newVRProjection(preset, window, size.Length())
				peak, _ := marchProjectionContext(context.Background(), sampler, Vec3{Z: size.Z/2 + 1}, Vec3{Z: -1}, 1, 1+size.Z, size.Length()/512, size.Scale(-0.5), size, projection, nil, nil)
				sampler.Close()
				if peak >= 0.1 {
					t.Fatalf("old unaccelerated ray still detected plane: %g", peak)
				}
			}
			q.MaxSteps = 2
			for _, mode := range []VRMode{VRModeDVR, VRModeMIP} {
				preset := opaqueAbovePreset()
				preset.Mode = mode
				frame, err := RenderVRContext(context.Background(), vol, cam, preset, window, false, nil, q)
				if frame != nil || !errors.Is(err, ErrVRSampleBudgetExceeded) {
					t.Fatalf("budget published a frame: %v %v", frame, err)
				}
			}
			frame, err = RenderVRSurfaceContext(context.Background(), vol, cam, 127.5, color.NRGBA{A: 255}, false, nil, q)
			if frame != nil || !errors.Is(err, ErrVRSampleBudgetExceeded) {
				t.Fatalf("surface budget published a frame: %v %v", frame, err)
			}
		})
	}
}

func TestVRRayGuardBoundsNonAdvancingRay(t *testing.T) {
	vol := thinPlaneVolume(t, 1, 1, 9)
	sampler, ok := newVolumeSampler(vol)
	if !ok {
		t.Fatal("sampler")
	}
	defer sampler.Close()
	guard := vrRayGuard{plan: VRRaySamplingPlan{SampleDistanceMM: 0.5, MaxSteps: 4}}
	_, found := firstVRSurfaceHitContext(context.Background(), sampler, Vec3{}, Vec3{Z: 1}, 1e20, 1e20, 0.5, Vec3{}, vol.PhysicalSize(), 1000, nil, &guard)
	if found || guard.iterations != 4 || !errors.Is(guard.err, ErrVRSampleBudgetExceeded) {
		t.Fatalf("guard=%+v found=%v", guard, found)
	}
}
