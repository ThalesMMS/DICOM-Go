package render

import (
	"context"
	"errors"
	"image"
	"image/color"
	"math"
)

// RenderVRSurface renders the first iso-value crossing along each camera ray.
// This CPU headless renderer samples the scalar field with the VR camera,
// crop, and clip state. Desktop GPU-only workflows must use their GPU renderer.
func RenderVRSurface(vol *Volume, cam VRCamera, iso float64, surfaceColor color.NRGBA, shading bool, clip *VRClip, quality VRQuality) image.Image {
	img, _ := RenderVRSurfaceContext(context.Background(), vol, cam, iso, surfaceColor, shading, clip, quality)
	return img
}

// RenderVRSurfaceContext is the cancellable form of RenderVRSurface.
func RenderVRSurfaceContext(ctx context.Context, vol *Volume, cam VRCamera, iso float64, surfaceColor color.NRGBA, shading bool, clip *VRClip, quality VRQuality) (image.Image, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	if vol == nil {
		return newVRSurfaceImage(quality), nil
	}
	if err := ctx.Err(); err != nil {
		return newVRSurfaceImage(quality), err
	}
	prepared, err := vol.PrepareVRVolumeContext(ctx, VRPrefilterNone)
	if err != nil {
		return newVRSurfaceImage(quality), err
	}
	return RenderVRSurfacePreparedContext(ctx, prepared, cam, iso, surfaceColor, shading, clip, quality)
}

// RenderVRSurfacePrepared renders the immutable generation selected by prepared.
// The prepared source and block grid can be reused across headless frames.
func RenderVRSurfacePrepared(prepared *VRPreparedVolume, cam VRCamera, iso float64, surfaceColor color.NRGBA, shading bool, clip *VRClip, quality VRQuality) image.Image {
	img, _ := RenderVRSurfacePreparedContext(context.Background(), prepared, cam, iso, surfaceColor, shading, clip, quality)
	return img
}

// RenderVRSurfacePreparedContext is the cancellable prepared surface renderer.
// A sampler lease is acquired and released for each frame; the cached source
// and min/max grid belong to prepared, rather than the returned image.
func RenderVRSurfacePreparedContext(ctx context.Context, prepared *VRPreparedVolume, cam VRCamera, iso float64, surfaceColor color.NRGBA, shading bool, clip *VRClip, quality VRQuality) (image.Image, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	img := newVRSurfaceImage(quality)
	w, h := img.Bounds().Dx(), img.Bounds().Dy()
	if prepared == nil || prepared.Volume == nil {
		return img, nil
	}
	if err := ctx.Err(); err != nil {
		return img, err
	}
	vol := prepared.Volume
	size := vol.PhysicalSize()
	if size.X <= 0 || size.Y <= 0 || size.Z <= 0 {
		return img, nil
	}
	samplingQuality := quality
	samplingQuality.AdaptiveStep = VRAdaptiveStep{}
	plan, err := volumeVRRaySampling(vol, samplingQuality)
	if err != nil {
		return nil, err
	}
	ctx, cancel := context.WithCancelCause(ctx)
	defer cancel(nil)
	lease, err := prepared.AcquireSnapshotContext(ctx)
	if err != nil {
		return img, err
	}
	sampler, err := newVolumeSamplerFromLease(vol, lease)
	if err != nil {
		return img, err
	}
	defer sampler.Close()
	var leap *vrSpaceLeap
	if clip == nil || !clip.fullRays {
		grid, err := surfaceVRBlockGrid(ctx, prepared, sampler)
		if err != nil {
			return img, err
		}
		leap = newVRSpaceLeap(grid, sampler)
	}
	if surfaceColor.A == 0 {
		surfaceColor.A = 255
	}
	boxMin := size.Scale(-0.5)
	boxMax := size.Scale(0.5)
	stepLen := plan.SampleDistanceMM
	rayGen := newVRRayGenerator(cam, w, h)
	// Cut planes (#1067) light their face as a surface: clipped neighbours
	// read below the iso-value.
	cutGradients := shading && clip.hasCutPlanes()
	clippedValue := iso - 1
	if huMin, _, ok := prepared.huRange(); ok {
		clippedValue = math.Min(clippedValue, huMin)
	}

	// Opaque geometry (#1068) hides the surface behind it; translucent
	// fragments (#1236) in front of the surface are composited over it.
	geometry, err := clip.geometryDepth(ctx, cam, w, h)
	if err != nil {
		return img, err
	}

	renderRow := func(py int) {
		rowBase := rayGen.rowBase(py)
		var fragmentBuffer [VRMaxTranslucentFragments]VRFragment
		for px := 0; px < w; px++ {
			if ctx.Err() != nil {
				return
			}
			o, d := rayGen.ray(px, rowBase)
			t0, t1, hit := intersectAABB(o, d, boxMin, boxMax)
			tGeometry, geometryColor, geometryHit := geometry.At(px, py)
			fragments := geometry.Fragments(px, py, fragmentBuffer[:0])
			// setComposite draws the fragments up to tEnd over a
			// linear-light colour (transparent where nothing is hit).
			setComposite := func(tEnd float64, base RGBA) {
				front := fragments
				for len(front) > 0 && front[len(front)-1].T >= tEnd {
					front = front[:len(front)-1]
				}
				c := compositeVRFragments(front, base)
				if c.A <= 0 {
					return
				}
				img.SetNRGBA(px, py, color.NRGBA{R: to8(linearToSRGB(c.R / c.A)), G: to8(linearToSRGB(c.G / c.A)), B: to8(linearToSRGB(c.B / c.A)), A: to8(c.A)})
			}
			drawGeometry := func() {
				setComposite(math.Inf(1), geometryColor)
			}
			if geometryHit {
				t1 = math.Min(t1, tGeometry)
			}
			if !hit || t0 > t1 {
				if geometryHit {
					drawGeometry()
				} else if len(fragments) > 0 {
					setComposite(math.Inf(1), RGBA{})
				}
				continue
			}
			guard := vrRayGuard{plan: plan}
			tex, ok := firstVRSurfaceLeapHitContext(ctx, sampler, leap, o, d, t0, t1, stepLen, boxMin, size, iso, clip, &guard)
			if guard.err != nil {
				cancel(guard.err)
				return
			}
			if !ok {
				if geometryHit {
					drawGeometry()
				} else if len(fragments) > 0 {
					setComposite(math.Inf(1), RGBA{})
				}
				continue
			}
			lit, specular := 1.0, 0.0
			if shading {
				gradient, cut := Vec3{}, false
				if cutGradients {
					gradient, cut = sampler.clippedGradientIndex(tex, clip, clippedValue)
				}
				if !cut {
					gradient = sampler.gradientAt(tex)
				}
				lit, specular = vol.shadeWithGradient(gradient, d)
			}
			surface := color.NRGBA{
				R: to8(float64(surfaceColor.R)/255*lit + specular),
				G: to8(float64(surfaceColor.G)/255*lit + specular),
				B: to8(float64(surfaceColor.B)/255*lit + specular),
				A: surfaceColor.A,
			}
			if len(fragments) > 0 {
				hitPoint := boxMin.Add(Vec3{X: tex.X * size.X, Y: tex.Y * size.Y, Z: tex.Z * size.Z})
				alpha := float64(surface.A) / 255
				setComposite(hitPoint.Sub(o).Dot(d), RGBA{
					R: srgbToLinear(float64(surface.R)/255) * alpha,
					G: srgbToLinear(float64(surface.G)/255) * alpha,
					B: srgbToLinear(float64(surface.B)/255) * alpha,
					A: alpha,
				})
				continue
			}
			img.SetNRGBA(px, py, surface)
		}
	}
	err = parallelRowsContext(ctx, h, renderRow)
	if cause := context.Cause(ctx); errors.Is(cause, ErrVRSampleBudgetExceeded) {
		return nil, cause
	}
	return img, err
}

func newVRSurfaceImage(quality VRQuality) *image.NRGBA {
	w, h := quality.Width, quality.Height
	if w <= 0 || h <= 0 {
		w, h = 256, 256
	}
	return image.NewNRGBA(image.Rect(0, 0, w, h))
}

func firstVRSurfaceHitContext(ctx context.Context, sampler *volumeSampler, origin, direction Vec3, t0, t1, stepLen float64, boxMin, size Vec3, iso float64, clip *VRClip, guards ...*vrRayGuard) (Vec3, bool) {
	return firstVRSurfaceLeapHitContext(ctx, sampler, nil, origin, direction, t0, t1, stepLen, boxMin, size, iso, clip, firstVRRayGuard(guards))
}

func firstVRSurfaceLeapHitContext(ctx context.Context, sampler *volumeSampler, leap *vrSpaceLeap, origin, direction Vec3, t0, t1, stepLen float64, boxMin, size Vec3, iso float64, clip *VRClip, guard *vrRayGuard) (Vec3, bool) {
	if sampler == nil || stepLen <= 0 {
		return Vec3{}, false
	}
	texAt := func(t float64) Vec3 {
		point := origin.Add(direction.Scale(t))
		return Vec3{
			X: (point.X - boxMin.X) / size.X,
			Y: (point.Y - boxMin.Y) / size.Y,
			Z: (point.Z - boxMin.Z) / size.Z,
		}
	}
	solid := func(t float64) bool {
		tex := texAt(t)
		if clip != nil && !clip.keep(tex) {
			return false
		}
		value, ok := sampler.textureAt(tex)
		return ok && value >= iso
	}
	sampleIndex := 0
	var leapA, leapB Vec3
	if leap != nil {
		leapA, leapB = leap.rayVoxel(origin, direction, boxMin, size)
	}
	for t := t0; t <= t1; t += stepLen {
		if !guard.take() {
			return Vec3{}, false
		}
		if sampleIndex%vrCancellationCheckStride == 0 && ctx.Err() != nil {
			return Vec3{}, false
		}
		sampleIndex++
		if leap != nil {
			if block := leap.block(texAt(t)); block >= 0 && skipsVRSurfaceBlock(leap.grid, block, iso) {
				var ok bool
				t, sampleIndex, ok = advanceVRSurfaceLeap(ctx, leap, block, leapA, leapB, t, t1, stepLen, guard, sampleIndex)
				if !ok {
					return Vec3{}, false
				}
				continue
			}
		}
		if solid(t) {
			if sampleIndex > 1 {
				// The crossing lies after the previous sample.
				t = bisectVRRay(t-stepLen, t, solid)
			}
			return texAt(t), true
		}
	}
	return Vec3{}, false
}

// vrBisectionSteps refine a ray hit to 1/16 of the march step (#1004).
const vrBisectionSteps = 4

// bisectVRRay narrows [before, hit], where inside(hit) holds and the march
// found it false at before, to the first parameter inside holds for.
func bisectVRRay(before, hit float64, inside func(float64) bool) float64 {
	for i := 0; i < vrBisectionSteps; i++ {
		mid := before + (hit-before)/2
		if inside(mid) {
			hit = mid
		} else {
			before = mid
		}
	}
	return hit
}
