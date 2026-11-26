package render

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"image"
	"image/color"
	"math"
	"sync"
	"testing"
	"time"
)

func TestSurfaceSpaceLeapLegacyCachePublished(t *testing.T) {
	volume, err := BuildVolume(sphereStack(24, 5))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = volume.Close() })
	_, err = RenderVRSurfaceContext(context.Background(), volume,
		NewVRCamera(volume.BoundingRadiusMM()), 128, color.NRGBA{A: 255},
		false, nil, DefaultVRQuality(16, 16))
	if err != nil {
		t.Fatal(err)
	}
	prepared, err := volume.PrepareVRVolume(VRPrefilterNone)
	if err != nil {
		t.Fatal(err)
	}
	prepared.blocksMu.Lock()
	grid := prepared.blocks
	prepared.blocksMu.Unlock()
	if grid == nil {
		t.Fatal("surface render did not publish the reusable prepared block grid")
	}
}

func TestSurfaceSpaceLeapImagesMatchFullMarch(t *testing.T) {
	stacks := []*Stack{
		sphereStack(29, 5),
		ctPhantomStack(37, 29, 21),
		stack16(29, true, func(int) Rescale { return Rescale{Slope: -0.5, Intercept: 75} },
			func(x, y, z int) int { return (x-15)*61 + (y-11)*7 + z*3 }),
	}
	for source, stack := range stacks {
		volume, err := BuildVolume(stack)
		if err != nil {
			t.Fatal(err)
		}
		prepared, err := volume.PrepareVRVolume(VRPrefilterNone)
		if err != nil {
			t.Fatal(err)
		}
		crop := NewVRClip()
		crop.SetCropBox(Vec3{X: .1, Y: .12, Z: .15}, Vec3{X: .85, Y: .9, Z: .8})
		cut := NewVRClip()
		cut.AddCutPlane(Vec3{X: 0.3, Y: 0.7, Z: 0.2}, .52)
		cut.SetGeometry(&VRGeometry{Meshes: []VRGeometryMesh{
			VRSphereMesh(Vec3{X: 1, Y: -7, Z: 0}, 3, RGBA{R: .3, G: .6, B: .1, A: .4}),
			VRSphereMesh(Vec3{X: -3, Y: 8, Z: 1}, 2, RGBA{B: 1, A: 1}),
		}})
		for _, projection := range []VRCameraProjection{VRProjectionPerspective, VRProjectionParallel} {
			camera := NewVRCamera(volume.BoundingRadiusMM())
			camera.FitVolume(volume)
			camera.Rotate(17, -9)
			camera.Projection = projection
			for _, shading := range []bool{false, true} {
				for clipIndex, clip := range []*VRClip{nil, crop, cut} {
					for _, iso := range []float64{-5000, 128, 10000} {
						name := fmt.Sprintf("source%d/projection%d/shading%v/clip%d/iso%g", source, projection, shading, clipIndex, iso)
						t.Run(name, func(t *testing.T) {
							quality := DefaultVRQuality(24, 20)
							quality.SpacingFraction = .37
							style := color.NRGBA{R: 219, G: 175, B: 107, A: 180}
							fast, err := RenderVRSurfacePreparedContext(context.Background(), prepared, camera, iso, style, shading, clip, quality)
							if err != nil {
								t.Fatal(err)
							}
							slow, err := RenderVRSurfacePreparedContext(context.Background(), prepared, camera, iso, style, shading, fullMarch(clip), quality)
							if err != nil {
								t.Fatal(err)
							}
							if !bytes.Equal(fast.(*image.NRGBA).Pix, slow.(*image.NRGBA).Pix) {
								t.Fatal("surface image differs from the complete march")
							}
						})
					}
				}
			}
		}
		_ = volume.Close()
	}
}

func TestSurfaceSpaceLeapSolidUnknownAndThresholdBounds(t *testing.T) {
	for _, fixture := range []struct {
		name   string
		lo, hi float32
		iso    float64
		want   bool
	}{
		{"empty", -4, 0, 1, true},
		{"all solid must hit first sample", 5, 8, 1, false},
		{"equality must interpolate", -1, 1, 1, false},
		{"rounding margin", -1, 1, 1 + 1e-10, false},
		{"NaN", float32(math.NaN()), 0, 1, false},
		{"infinite", -1, float32(math.Inf(1)), 1, false},
		{"NaN iso", -1, 0, math.NaN(), false},
	} {
		t.Run(fixture.name, func(t *testing.T) {
			grid := &vrBlockGrid{min: []float32{fixture.lo}, max: []float32{fixture.hi}}
			if got := skipsVRSurfaceBlock(grid, 0, fixture.iso); got != fixture.want {
				t.Fatalf("skip=%v want%v", got, fixture.want)
			}
		})
	}
}

func TestSurfaceSpaceLeapChargesLogicalSamplesAndPreservesAddition(t *testing.T) {
	leap := &vrSpaceLeap{grid: &vrBlockGrid{nx: 1, ny: 1, nz: 1}}
	a, b := Vec3{X: 1, Y: 1, Z: 1}, Vec3{Z: 1}
	const start, end, step = .2, 10.0, .17
	want := leap.leap(0, a, b, start, end, step)
	guard := &vrRayGuard{plan: VRRaySamplingPlan{MaxSteps: 1000, SampleDistanceMM: step}}
	guard.take() // The first sample was charged by the outer march.
	got, samples, ok := advanceVRSurfaceLeap(context.Background(), leap, 0, a, b, start, end, step, guard, 1)
	if !ok || got != want || guard.iterations != samples || samples <= 2 {
		t.Fatalf("end%v want%v samples%d guard%d ok%v", got, want, samples, guard.iterations, ok)
	}
	limited := &vrRayGuard{plan: VRRaySamplingPlan{MaxSteps: 3, SampleDistanceMM: step}}
	limited.take()
	_, _, ok = advanceVRSurfaceLeap(context.Background(), leap, 0, a, b, start, end, step, limited, 1)
	if ok || !errors.Is(limited.err, ErrVRSampleBudgetExceeded) || limited.iterations != 3 {
		t.Fatalf("skipped samples bypassed budget: ok%v guard%+v", ok, limited)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, _, ok = advanceVRSurfaceLeap(ctx, leap, 0, a, b, start, end, .001, nil, vrCancellationCheckStride)
	if ok {
		t.Fatal("skipped samples bypassed cancellation")
	}
}

func TestSurfaceSpaceLeapCacheWaitCanCancel(t *testing.T) {
	prepared := &VRPreparedVolume{}
	prepared.blocksMu.Lock()
	defer prepared.blocksMu.Unlock()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	started := make(chan struct{})
	result := make(chan error, 1)
	go func() {
		close(started)
		_, err := surfaceVRBlockGrid(ctx, prepared, nil)
		result <- err
	}()
	<-started
	cancel()
	select {
	case err := <-result:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("cache wait error=%v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("cancellation waited for the cache mutex to unlock")
	}
}

func TestSurfaceSpaceLeapPreparedReuseFilteredAndLifetime(t *testing.T) {
	volume, err := BuildVolume(sphereStack(29, 5))
	if err != nil {
		t.Fatal(err)
	}
	defer volume.Close()
	prepared, err := volume.PrepareVRVolume(VRPrefilterBasicSmooth5x5)
	if err != nil {
		t.Fatal(err)
	}
	var wg sync.WaitGroup
	for i := 0; i < 4; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, err := RenderVRSurfacePreparedContext(context.Background(), prepared,
				NewVRCamera(volume.BoundingRadiusMM()), 128, color.NRGBA{A: 255},
				true, nil, DefaultVRQuality(24, 20))
			if err != nil {
				t.Error(err)
			}
		}()
	}
	wg.Wait()
	firstGrid := prepared.blocks
	if firstGrid == nil {
		t.Fatal("prepared render did not cache its grid")
	}
	style, quality := color.NRGBA{R: 217, A: 255}, DefaultVRQuality(24, 20)
	camera := NewVRCamera(volume.BoundingRadiusMM())
	fast, err := RenderVRSurfacePreparedContext(context.Background(), prepared, camera, 128, style, true, nil, quality)
	if err != nil {
		t.Fatal(err)
	}
	slow, err := RenderVRSurfacePreparedContext(context.Background(), prepared, camera, 128, style, true, fullMarch(nil), quality)
	if err != nil || !bytes.Equal(fast.(*image.NRGBA).Pix, slow.(*image.NRGBA).Pix) || prepared.blocks != firstGrid {
		t.Fatalf("filtered generation or grid changed: %v", err)
	}
	if err := volume.Close(); err != nil {
		t.Fatal(err)
	}
	_, err = RenderVRSurfacePreparedContext(context.Background(), prepared, camera, 128, style, true, nil, quality)
	if !errors.Is(err, ErrVolumeStoreClosed) {
		t.Fatalf("closed prepared source rendered: %v", err)
	}
}
