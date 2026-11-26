package render

import (
	"context"
	"errors"
	"image"
	"image/color"
	"math"
)

// RenderVR renders a 3D volume to a 2D image using the specified camera,
// transfer function, rendering mode, and clipping parameters. Non-positive
// quality dimensions default to 256×256. Shading is optional. A nil or empty
// volume returns an empty image.
func RenderVR(vol *Volume, cam VRCamera, preset VRPreset, window WindowLevel, shading bool, clip *VRClip, quality VRQuality) image.Image {
	img, _ := RenderVRContext(context.Background(), vol, cam, preset, window, shading, clip, quality)
	return img
}

// RenderVRContext is the cancellable form of RenderVR. Cancellation is checked
// between rows, pixels, and ray samples; the returned image may be partial.
func RenderVRContext(ctx context.Context, vol *Volume, cam VRCamera, preset VRPreset, window WindowLevel, shading bool, clip *VRClip, quality VRQuality) (image.Image, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	if vol == nil {
		return newVRViewportImage(quality, preset.EffectiveBackground()), nil
	}
	prepared, err := vol.PrepareVRVolumeContext(ctx, preset.Prefilter)
	if err != nil {
		return newVRViewportImage(quality, preset.EffectiveBackground()), err
	}
	return RenderVRPreparedContext(ctx, prepared, cam, preset, window, shading, clip, quality)
}

// RenderVRPrepared renders an explicitly selected immutable VR generation.
// Headless callers and independent GPU oracles reuse the prepared source
// identity. This CPU renderer is not an application 3D fallback.
func RenderVRPrepared(prepared *VRPreparedVolume, cam VRCamera, preset VRPreset, window WindowLevel, shading bool, clip *VRClip, quality VRQuality) image.Image {
	img, _ := RenderVRPreparedContext(context.Background(), prepared, cam, preset, window, shading, clip, quality)
	return img
}

// RenderVRPreparedContext is the cancellable prepared-volume renderer.
func RenderVRPreparedContext(ctx context.Context, prepared *VRPreparedVolume, cam VRCamera, preset VRPreset, window WindowLevel, shading bool, clip *VRClip, quality VRQuality) (image.Image, error) {
	return renderVRPreparedContext(ctx, prepared, cam, preset, window, shading, clip, quality, nil)
}

// renderVRPreparedContext is the shared ray caster; fusion, when not nil,
// adds the secondary volume to every DVR sample (#1180).
func renderVRPreparedContext(ctx context.Context, prepared *VRPreparedVolume, cam VRCamera, preset VRPreset, window WindowLevel, shading bool, clip *VRClip, quality VRQuality, fusion *vrFusionFrame) (image.Image, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	w, h := quality.Width, quality.Height
	if w <= 0 || h <= 0 {
		w, h = 256, 256
	}
	background := preset.EffectiveBackground()
	img := newVRViewportImage(VRQuality{Width: w, Height: h}, background)
	if prepared == nil || prepared.Volume == nil {
		return img, nil
	}
	if err := ctx.Err(); err != nil {
		return img, err
	}
	if !preset.ProjectionRange.Valid() {
		return img, ErrInvalidVRValueRange
	}
	vol := prepared.Volume
	size := vol.PhysicalSize()
	if size.X <= 0 || size.Y <= 0 || size.Z <= 0 {
		return img, nil
	}
	samplingQuality := quality
	if preset.Mode != VRModeDVR || fusion != nil || clip.activeLabels(vol) != nil {
		samplingQuality.AdaptiveStep = VRAdaptiveStep{}
	}
	plan, err := volumeVRRaySampling(vol, samplingQuality)
	if err != nil {
		return nil, err
	}
	ctx, cancel := context.WithCancelCause(ctx)
	defer cancel(nil)
	sampler, err := prepared.newSampler()
	if err != nil {
		return img, err
	}
	defer sampler.Close()
	boxMin := size.Scale(-0.5)
	boxMax := size.Scale(0.5)

	lut, window, dataMin, dataMax := vrTransferSetup(prepared, preset, window)

	diag := size.Length()
	stepLen := plan.SampleDistanceMM
	rayGen := newVRRayGenerator(cam, w, h)
	projection := newVRProjection(preset, window, diag)

	// Empty-space skipping (#1003) for DVR, MIP and MinIP, and for Average
	// and RaySum with a value range (#1181). Labels that add
	// opacity where the transfer function has none (#1062) use conservative
	// nearest-neighbour occupancy instead of disabling the grid.
	var leap *vrSpaceLeap
	labelsAddOpacity := preset.Mode == VRModeDVR && clip.activeLabels(vol).addsOpacity()
	// A fused secondary adds opacity where the primary transfer function has
	// none. Identity patient registration combines a conservative secondary grid;
	// arbitrary mapping callbacks retain the full march.
	if (clip == nil || !clip.fullRays) && (fusion == nil || fusion.mapper == nil) {
		if leap, err = newRenderLeap(ctx, prepared, sampler, preset, lut, window, dataMin, dataMax, clip); err != nil {
			return img, err
		}
		if leap != nil && fusion != nil {
			if err := leap.limitFusion(ctx, sampler, fusion); err != nil {
				return img, err
			}
		}
	}

	// Volumetric scattering (#1064): the settled frame precomputes the light
	// attenuation volume once.
	scatter, err := newVRScatterFrame(ctx, sampler, lut, preset, window, dataMin, dataMax, shading, clip, quality, cam, boxMin, size, stepLen)
	if err != nil {
		return img, err
	}

	// Opaque geometry (#1068) ends the rays at its depth and replaces the
	// background behind the volume; translucent fragments (#1236) are
	// composited where the rays pass them.
	geometry, err := clip.geometryDepth(ctx, cam, w, h)
	if err != nil {
		return img, err
	}
	// The adaptive step (#1231) classifies samples by the scalar transfer
	// function alone, so labelled and fused rays keep the fixed step.
	adaptive := quality.AdaptiveStep
	if labelsAddOpacity || clip.activeLabels(vol) != nil || fusion != nil {
		adaptive = VRAdaptiveStep{}
	}
	setLinear := func(px, py int, c RGBA) {
		img.SetNRGBA(px, py, color.NRGBA{R: to8(linearToSRGB(c.R)), G: to8(linearToSRGB(c.G)), B: to8(linearToSRGB(c.B)), A: 255})
	}

	// Render rows in parallel; each goroutine writes disjoint scanlines, so the
	// shared image needs no synchronization.
	renderRow := func(py int, gradientCell *volumeGradientCell, fusionWorker *vrFusionWorker, stepper *vrAdaptiveStepper) {
		rowBase := rayGen.rowBase(py)
		var fragmentBuffer [VRMaxTranslucentFragments]VRFragment
		for px := 0; px < w; px++ {
			if ctx.Err() != nil {
				return
			}
			o, d := rayGen.ray(px, rowBase)
			t0, t1, hit := intersectAABB(o, d, boxMin, boxMax)
			if quality.Jitter {
				t0 += VRRayJitter(px, py) * stepLen
			}
			fragments := geometry.Fragments(px, py, fragmentBuffer[:0])
			pixelBackground := background
			if tGeometry, geometryColor, geometryHit := geometry.At(px, py); geometryHit {
				pixelBackground = geometryColor
				t1 = math.Min(t1, tGeometry)
				if !hit || t0 > t1 {
					setLinear(px, py, compositeVRFragments(fragments, geometryColor))
					continue
				}
			}
			guard := vrRayGuard{plan: plan}
			if preset.Mode != VRModeDVR {
				v := 0.0
				sampled := false
				if hit {
					v, sampled = marchProjectionContext(ctx, sampler, o, d, t0, t1, stepLen, boxMin, size, projection, clip, leap, &guard)
				}
				if guard.err != nil {
					cancel(guard.err)
					return
				}
				if !sampled {
					if pixelBackground != background || len(fragments) > 0 {
						setLinear(px, py, compositeVRFragments(fragments, pixelBackground))
					}
					continue
				}
				if projection.coloredMIP {
					mapped := VRMIPColor(lut, v, preset.Inverse)
					setLinear(px, py, compositeVRFragments(fragments, mapped))
					continue
				}
				if preset.Inverse {
					v = 1 - v
				}
				if len(fragments) > 0 {
					// A projection has no depth: translucent geometry lies
					// over it.
					gray := srgbToLinear(clampUnit(v))
					setLinear(px, py, compositeVRFragments(fragments, RGBA{R: gray, G: gray, B: gray, A: 1}))
					continue
				}
				gy := to8(v)
				img.SetNRGBA(px, py, color.NRGBA{R: gy, G: gy, B: gy, A: 255})
				continue
			}
			if !hit {
				if len(fragments) > 0 {
					setLinear(px, py, compositeVRFragments(fragments, background))
				}
				continue
			}
			r, g, b, a := marchDVRContext(ctx, sampler, gradientCell, lut, o, d, t0, t1, stepLen, boxMin, size, window, dataMin, dataMax, preset, shading, clip, leap, scatter, fusionWorker, stepper, fragments, &guard)
			if guard.err != nil {
				cancel(guard.err)
				return
			}
			r += (1 - a) * pixelBackground.R
			g += (1 - a) * pixelBackground.G
			b += (1 - a) * pixelBackground.B
			img.SetNRGBA(px, py, color.NRGBA{R: to8(linearToSRGB(r)), G: to8(linearToSRGB(g)), B: to8(linearToSRGB(b)), A: 255})
		}
	}

	// Each worker owns one gradient cell for every row it claims (#1061).
	err = parallelRowsWorkerContext(ctx, h, func() func(int) {
		cell := new(volumeGradientCell)
		fusionWorker := fusion.newWorker(sampler.vol)
		var stepper *vrAdaptiveStepper
		if adaptive.Enabled() {
			stepper = newVRAdaptiveStepper(adaptive, stepLen)
		}
		return func(py int) { renderRow(py, cell, fusionWorker, stepper) }
	})
	if cause := context.Cause(ctx); errors.Is(cause, ErrVRSampleBudgetExceeded) {
		return nil, cause
	}
	return img, err
}

// vrTransferSetup returns the baked LUT, the effective window and the data
// range of HU-domain DVR for one render.
func vrTransferSetup(prepared *VRPreparedVolume, preset VRPreset, window WindowLevel) (lut VRLUT, effective WindowLevel, dataMin, dataMax float64) {
	dataMin, dataMax = 0.0, 1.0
	if (preset.Mode == VRModeDVR || preset.Mode == VRModeMIP && preset.ColoredMIP) && preset.TF.Domain() == VRTransferDomainHU {
		if minimum, maximum, ok := prepared.huRange(); ok {
			dataMin, dataMax = minimum, maximum
		}
	}
	effective = normalizeWindow(window, prepared.Volume.RecommendedWindow())
	if preset.TF.Domain() == VRTransferDomainHU {
		lut = preset.TF.BakeLUT(dataMin, dataMax, preset.TF.PreferredLUTSize())
	} else {
		lut = preset.TF.BakeWindowLUT(effective, preset.TF.PreferredLUTSize())
	}
	return lut, effective, dataMin, dataMax
}

// newRenderLeap returns the empty-space skipping state of one render, or nil
// for the modes it does not accelerate (Average and RaySum without a value
// range, where every sample counts).
func newRenderLeap(ctx context.Context, prepared *VRPreparedVolume, sampler *volumeSampler, preset VRPreset, lut VRLUT, window WindowLevel, dataMin, dataMax float64, clips ...*VRClip) (*vrSpaceLeap, error) {
	// Windowed extrema cannot bound a raw maximum for a non-monotonic CLUT.
	if preset.Mode == VRModeMIP && preset.ColoredMIP {
		return nil, nil
	}
	if preset.Mode != VRModeDVR && preset.Mode != VRModeMIP && preset.Mode != VRModeMinIP && !preset.UsesValueRange() {
		return nil, nil
	}
	grid, err := prepared.blockGrid(ctx, sampler)
	if err != nil {
		return nil, err
	}
	leap := newVRSpaceLeap(grid, sampler)
	// Cut-plane replacement intensities can introduce gradients absent in the
	// source grid; keep the scalar-only bound in that profile.
	cutGradients := len(clips) > 0 && clips[0].hasCutPlanes()
	var gradients []float64
	if preset.Mode == VRModeDVR && !cutGradients && (preset.Transfer2D != nil || preset.usesGradientOpacity()) {
		gradients, err = grid.physicalGradientBounds(ctx, sampler)
		if err != nil {
			return nil, err
		}
	}
	switch {
	case preset.Mode == VRModeDVR && preset.Transfer2D != nil:
		leap.forDVR2D(preset.Transfer2D, gradients)
	case preset.Mode == VRModeDVR:
		leap.forDVR(preset.TF, lut, window, dataMin, dataMax)
		leap.limitGradientOpacity(preset, gradients)
	case preset.Mode == VRModeMIP || preset.Mode == VRModeMinIP:
		leap.forProjection(window)
	default:
		leap.forValueRange(preset.ProjectionRange)
	}
	if preset.Mode == VRModeDVR && len(clips) > 0 {
		leap.limitLabels(clips[0].activeLabels(sampler.vol))
	}
	return leap, nil
}

func newVRViewportImage(quality VRQuality, background RGBA) *image.NRGBA {
	w, h := quality.Width, quality.Height
	if w <= 0 || h <= 0 {
		w, h = 256, 256
	}
	pixel := color.NRGBA{
		R: to8(linearToSRGB(background.R)),
		G: to8(linearToSRGB(background.G)),
		B: to8(linearToSRGB(background.B)),
		A: 255,
	}
	img := image.NewNRGBA(image.Rect(0, 0, w, h))
	for offset := 0; offset < len(img.Pix); offset += 4 {
		img.Pix[offset], img.Pix[offset+1], img.Pix[offset+2], img.Pix[offset+3] = pixel.R, pixel.G, pixel.B, pixel.A
	}
	return img
}

// RenderVRPreview renders a cheaper interaction frame. It preserves projection
// mode, transfer function, window and clipping, but disables shading and gradient
// opacity so drag/orbit does not pay the gradient path.
func RenderVRPreview(vol *Volume, cam VRCamera, preset VRPreset, window WindowLevel, clip *VRClip, quality VRQuality) image.Image {
	img, _ := RenderVRPreviewContext(context.Background(), vol, cam, preset, window, clip, quality)
	return img
}

// RenderVRPreviewContext is the cancellable form of RenderVRPreview.
func RenderVRPreviewContext(ctx context.Context, vol *Volume, cam VRCamera, preset VRPreset, window WindowLevel, clip *VRClip, quality VRQuality) (image.Image, error) {
	preset.GradientOpacityScale, preset.GradientOpacity = 0, nil
	return RenderVRContext(ctx, vol, cam, preset, window, false, clip, quality)
}

// RenderVRPreparedPreviewContext preserves the selected prepared intensity
// source and LUT while temporarily disabling the gradient path.
func RenderVRPreparedPreviewContext(ctx context.Context, prepared *VRPreparedVolume, cam VRCamera, preset VRPreset, window WindowLevel, clip *VRClip, quality VRQuality) (image.Image, error) {
	preset.GradientOpacityScale, preset.GradientOpacity = 0, nil
	return RenderVRPreparedContext(ctx, prepared, cam, preset, window, false, clip, quality)
}

type vrRayGenerator struct {
	origin     Vec3
	right      Vec3
	up         Vec3
	forward    Vec3
	projection VRCameraProjection
	distance   float64
	aspectTan  float64
	tanHalfFov float64
	invWidth   float64
	invHeight  float64
}

type vrRayRow struct {
	originBase Vec3
	dirBase    Vec3
}

func newVRRayGenerator(cam VRCamera, w, h int) vrRayGenerator {
	right, up, forward := cam.basis()
	aspect := 1.0
	if h > 0 {
		aspect = float64(w) / float64(h)
	}
	invW, invH := 1.0, 1.0
	if w > 0 {
		invW = 1 / float64(w)
	}
	if h > 0 {
		invH = 1 / float64(h)
	}
	tanHalfFov := math.Tan(cam.FovY / 2)
	projection := cam.Projection.normalized()
	origin := cam.Position()
	if projection == VRProjectionEndoscopy {
		origin = cam.Target
	}
	return vrRayGenerator{
		origin:     origin,
		right:      right,
		up:         up,
		forward:    forward,
		projection: projection,
		distance:   cam.Distance,
		aspectTan:  aspect * tanHalfFov,
		tanHalfFov: tanHalfFov,
		invWidth:   invW,
		invHeight:  invH,
	}
}

func (g vrRayGenerator) rowBase(py int) vrRayRow {
	ndcy := (1 - 2*(float64(py)+0.5)*g.invHeight) * g.tanHalfFov
	if g.projection == VRProjectionParallel {
		return vrRayRow{
			originBase: g.origin.Add(g.up.Scale(ndcy * g.distance)),
			dirBase:    g.forward,
		}
	}
	return vrRayRow{
		originBase: g.origin,
		dirBase:    g.forward.Add(g.up.Scale(ndcy)),
	}
}

func (g vrRayGenerator) ray(px int, rowBase vrRayRow) (origin, dir Vec3) {
	ndcx := (2*(float64(px)+0.5)*g.invWidth - 1) * g.aspectTan
	if g.projection == VRProjectionParallel {
		return rowBase.originBase.Add(g.right.Scale(ndcx * g.distance)), g.forward
	}
	return rowBase.originBase, rowBase.dirBase.Add(g.right.Scale(ndcx)).Normalize()
}

// marchProjectionContext accumulates the grayscale projection of one ray
// segment: the MIP or MinIP extreme or the mean of the windowed samples, or
// the ray-sum of the modality values times the step (#1181). Average and
// RaySum take only the samples inside the projection's value range. The
// boolean reports whether any sample contributed; the value is the gray in
// [0,1] before inversion. leap, when not nil, skips the samples of blocks
// that cannot change the projection.
func marchProjectionContext(ctx context.Context, sampler *volumeSampler, o, d Vec3, t0, t1, stepLen float64, boxMin, size Vec3, projection vrProjection, clip *VRClip, leap *vrSpaceLeap, guards ...*vrRayGuard) (float64, bool) {
	acc, count := accumulateProjection(ctx, sampler, o, d, t0, t1, stepLen, boxMin, size, projection, clip, leap, guards...)
	if count == 0 {
		return 0, false
	}
	return projection.display(acc, count), true
}

// accumulateProjection returns the projection's accumulator and the number
// of samples that contributed: the windowed extreme or sum for MIP, MinIP and
// Average, and the integral of the modality value in units × mm for RaySum.
// A cancelled march contributes nothing.
func accumulateProjection(ctx context.Context, sampler *volumeSampler, o, d Vec3, t0, t1, stepLen float64, boxMin, size Vec3, projection vrProjection, clip *VRClip, leap *vrSpaceLeap, guards ...*vrRayGuard) (float64, int) {
	var acc float64
	count := 0
	guard := firstVRRayGuard(guards)
	sampleIndex := 0
	mode := projection.mode
	switch mode {
	case VRModeMinIP:
		acc = 1
	}
	t0, t1, visible := clip.clipRay(o, d, boxMin, size, t0, t1, stepLen)
	if !visible {
		return 0, 0
	}
	var leapA, leapB Vec3
	if leap != nil {
		leapA, leapB = leap.rayVoxel(o, d, boxMin, size)
	}
	labels := clip.activeLabels(sampler.vol)
	for t := t0; t <= t1; t += stepLen {
		if !guard.take() {
			return 0, 0
		}
		if sampleIndex%vrCancellationCheckStride == 0 && ctx != nil && ctx.Err() != nil {
			return 0, 0
		}
		sampleIndex++
		pw := o.Add(d.Scale(t))
		tex := Vec3{
			X: (pw.X - boxMin.X) / size.X,
			Y: (pw.Y - boxMin.Y) / size.Y,
			Z: (pw.Z - boxMin.Z) / size.Z,
		}
		if leap != nil {
			if block := leap.block(tex); block >= 0 && leap.skipsProjectionSample(block, mode, acc, count) {
				t = leap.leap(block, leapA, leapB, t, t1, stepLen)
				continue
			}
		}
		if !clip.keep(tex) || !labels.keeps(tex) {
			continue
		}
		hu, ok := sampler.textureAt(tex)
		if !ok || !projection.valueRange.contains(hu) {
			continue
		}
		switch mode {
		case VRModeMinIP:
			if dv := windowedUnit(hu, projection.window); count == 0 || dv < acc {
				acc = dv
			}
		case VRModeAverage:
			acc += windowedUnit(hu, projection.window)
		case VRModeRaySum:
			acc += hu * stepLen
		default: // VRModeMIP
			if projection.coloredMIP {
				if count == 0 || hu > acc {
					acc = hu
				}
				break
			}
			if dv := windowedUnit(hu, projection.window); count == 0 || dv > acc {
				acc = dv
			}
		}
		count++
	}
	return acc, count
}

// marchDVR composites samples front-to-back along [t0,t1].
func marchDVR(sampler *volumeSampler, gradientCell *volumeGradientCell, lut VRLUT, o, d Vec3, t0, t1, stepLen float64, boxMin, size Vec3, window WindowLevel, dataMin, dataMax float64, preset VRPreset, shading bool, clip *VRClip) (accR, accG, accB, accA float64) {
	return marchDVRContext(context.Background(), sampler, gradientCell, lut, o, d, t0, t1, stepLen, boxMin, size, window, dataMin, dataMax, preset, shading, clip, nil, nil, nil, nil, nil)
}

// leap, when not nil, skips the samples of transparent blocks. fusion, when
// not nil, composites the registered secondary volume into every sample: the
// opacities add and the premultiplied colours mix (#1180). stepper, when not
// nil, classifies the samples for the adaptive step (#1231): an enabled
// stepper varies the step around stepLen.
//
// Translucent fragments (#1236, nearest first, all in front of the ray's
// end) are composited before the first composited sample beyond their
// depth, once the adaptive stepper has accepted that sample (a rejected
// long step resumes behind it); those beyond the last sample are composited
// last.
func marchDVRContext(ctx context.Context, sampler *volumeSampler, gradientCell *volumeGradientCell, lut VRLUT, o, d Vec3, t0, t1, stepLen float64, boxMin, size Vec3, window WindowLevel, dataMin, dataMax float64, preset VRPreset, shading bool, clip *VRClip, leap *vrSpaceLeap, scatter *vrScatterFrame, fusion *vrFusionWorker, stepper *vrAdaptiveStepper, fragments []VRFragment, guards ...*vrRayGuard) (accR, accG, accB, accA float64) {
	// A 2D transfer function (#1232) classifies by value and gradient
	// magnitude and replaces the gradient opacity.
	transfer2D := preset.Transfer2D
	gradientOpacity := transfer2D == nil && preset.usesGradientOpacity()
	needGradient := shading || gradientOpacity || transfer2D != nil
	// The cached, interpolated stencil is a measured win for gradient-opacity
	// presets and 2D transfer functions. Shading-only presets retain the
	// exact central-difference path, which is cheaper for their sparse opaque
	// samples.
	useGradientCell := gradientCell != nil && (gradientOpacity || transfer2D != nil)
	t0, t1, visible := clip.clipRay(o, d, boxMin, size, t0, t1, stepLen)
	if !visible {
		_, accR, accG, accB, accA = compositeFragmentsBefore(fragments, math.Inf(1), accR, accG, accB, accA)
		return
	}
	if leap != nil && leap.empty == nil {
		leap = nil
	}
	var leapA, leapB Vec3
	if leap != nil {
		leapA, leapB = leap.rayVoxel(o, d, boxMin, size)
	}
	labels := clip.activeLabels(sampler.vol)
	cutGradients := needGradient && clip.hasCutPlanes()
	// Cut gradients read the clipped side as the data minimum: the 2D
	// table's lowest value when it classifies.
	clippedValue := dataMin
	if transfer2D != nil {
		clippedValue, _ = transfer2D.IntensityRange()
	}
	// The cap (#1067) is decided at the ray's first kept sample.
	capPending := clip.CapOpacity() > 0
	sampleIndex := 0
	stepper.beginRay()
	// step is the distance to the next sample: stepLen, unless the adaptive
	// stepper varies it.
	step := stepLen
	guard := firstVRRayGuard(guards)
	for t := t0; t <= t1; t += step {
		if !guard.take() {
			return 0, 0, 0, 0
		}
		if sampleIndex%vrCancellationCheckStride == 0 && ctx != nil && ctx.Err() != nil {
			return
		}
		sampleIndex++
		pw := o.Add(d.Scale(t))
		tex := Vec3{
			X: (pw.X - boxMin.X) / size.X,
			Y: (pw.Y - boxMin.Y) / size.Y,
			Z: (pw.Z - boxMin.Z) / size.Z,
		}
		if capPending && clip.keep(tex) {
			capPending = false
			previous := o.Add(d.Scale(t - stepLen))
			if clip.capsAt(Vec3{
				X: (previous.X - boxMin.X) / size.X,
				Y: (previous.Y - boxMin.Y) / size.Y,
				Z: (previous.Z - boxMin.Z) / size.Z,
			}) {
				if value, ok := sampler.textureAt(tex); ok {
					gray := vrCapGray(value, clip.capWindowOr(window))
					alpha := clip.CapOpacity()
					if len(fragments) > 0 {
						fragments, accR, accG, accB, accA = compositeFragmentsBefore(fragments, t, accR, accG, accB, accA)
					}
					inv := 1 - accA
					accR += inv * alpha * gray
					accG += inv * alpha * gray
					accB += inv * alpha * gray
					accA += inv * alpha
					if accA > vrOpacityThreshold {
						break
					}
				}
			}
		}
		// A pending cut cap must observe the first retained sample.
		if leap != nil && !capPending {
			if block := leap.block(tex); block >= 0 && leap.empty[block] {
				// Preserve the no-overlap result with a real bounded probe, even
				// when every secondary contribution along a ray is transparent.
				last := leap.leap(block, leapA, leapB, t, t1, step)
				if fusion != nil && fusion.inBounds == 0 {
					fusion.probeSkipped(ctx, o, d, boxMin, size, t, last, step, clip, labels)
				}
				t = last
				stepper.reset()
				step = stepLen
				continue
			}
		}
		if !clip.keep(tex) {
			stepper.reset()
			step = stepLen
			continue
		}
		var style VRLabelStyle
		labelled := false
		if labels != nil {
			if style, labelled = labels.visibleStyle(labels.labelAt(tex)); !labelled && labels.onlyInside {
				continue
			}
		}
		labelOpacity := labelled && style.Blend > 0 && style.Opacity > 0
		var hu float64
		var textureSample volumeTextureSample
		var ok bool
		if useGradientCell {
			textureSample, ok = sampler.textureSample(tex)
			hu = textureSample.value()
		} else {
			hu, ok = sampler.textureAt(tex)
		}
		if !ok {
			stepper.reset()
			step = stepLen
			continue
		}
		var rgba RGBA
		var indexGradient Vec3
		haveGradient := false
		if transfer2D != nil {
			// Values transparent at every gradient skip the gradient.
			if transfer2D.maxAlphaAt(hu) > 0 {
				indexGradient = dvrIndexGradient(sampler, gradientCell, textureSample, tex, clip, cutGradients, useGradientCell, clippedValue)
				haveGradient = true
				rgba = transfer2D.Lookup(hu, sampler.vol.physicalGradientMagnitude(indexGradient))
			}
		} else {
			rgba = dvrTransferSample(preset.TF, lut, hu, window, dataMin, dataMax)
		}
		// sampleLen is the ray length this sample stands for.
		sampleLen := stepLen
		if stepper != nil {
			accept, length, next, resume := stepper.observe(t, rgba)
			step = next
			if !accept {
				t = resume
				continue
			}
			sampleLen = length
		}
		alpha := rgba.A
		var secondary RGBA
		if fusion != nil {
			secondary = fusion.sample(tex, stepLen)
		}
		if alpha <= 0 && !labelOpacity {
			if secondary.A <= 0 {
				continue
			}
			// Only the secondary volume is visible at this sample.
			if len(fragments) > 0 {
				fragments, accR, accG, accB, accA = compositeFragmentsBefore(fragments, t, accR, accG, accB, accA)
			}
			inv := 1 - accA
			accR += inv * secondary.A * secondary.R
			accG += inv * secondary.A * secondary.G
			accB += inv * secondary.A * secondary.B
			accA += inv * secondary.A
			if accA > vrOpacityThreshold {
				break
			}
			continue
		}
		cr, cg, cb := rgba.R, rgba.G, rgba.B
		// Scalar gradients drive opacity modulation and scattering. Scalar
		// illumination reuses them; opacity illumination samples its own field.
		if needGradient {
			if !haveGradient && (!preset.OpacityNormals || gradientOpacity || transfer2D != nil || scatter != nil) {
				indexGradient = dvrIndexGradient(sampler, gradientCell, textureSample, tex, clip, cutGradients, useGradientCell, clippedValue)
			}
			if gradientOpacity {
				alpha *= preset.gradientOpacityFactor(sampler, indexGradient)
			}
			if labelled {
				// The label mixes with the gradient-modulated transfer function;
				// its own opacity is not modulated.
				cr, cg, cb, alpha = mixVRLabel(style, cr, cg, cb, alpha)
				labelled = false
			}
			if shading {
				material := preset.EffectiveLightingMaterial()
				shadingGradient := indexGradient
				if preset.OpacityNormals {
					shadingGradient = dvrOpacityGradient(sampler, lut, tex, preset, window, dataMin, dataMax, clip, clippedValue)
				}
				lit, specular := sampler.vol.shadeVRWithGradient(shadingGradient, d, material)
				sr, sg, sb := cr*lit+specular, cg*lit+specular, cb*lit+specular
				if scatter != nil {
					sr, sg, sb = scatter.shade(sr, sg, sb, cr, cg, cb, alpha, tex, d, indexGradient, material.Diffuse)
				}
				cr, cg, cb = sr, sg, sb
			}
		}
		if labelled {
			cr, cg, cb, alpha = mixVRLabel(style, cr, cg, cb, alpha)
		}
		alpha = CorrectVRAlpha(alpha, 1, sampleLen, preset.TF.OpacityUnitDistanceMM())
		if secondary.A > 0 {
			cr, cg, cb, alpha = mixVRFusionSample(cr, cg, cb, alpha, secondary)
		}
		if alpha <= 0 {
			continue
		}
		if len(fragments) > 0 {
			fragments, accR, accG, accB, accA = compositeFragmentsBefore(fragments, t, accR, accG, accB, accA)
		}
		inv := 1 - accA
		accR += inv * alpha * cr
		accG += inv * alpha * cg
		accB += inv * alpha * cb
		accA += inv * alpha
		if accA > vrOpacityThreshold {
			break
		}
	}
	_, accR, accG, accB, accA = compositeFragmentsBefore(fragments, math.Inf(1), accR, accG, accB, accA)
	return accR, accG, accB, accA
}

// compositeFragmentsBefore composites, front to back behind the
// accumulated colour, the fragments in front of t and returns the rest.
func compositeFragmentsBefore(fragments []VRFragment, t, accR, accG, accB, accA float64) ([]VRFragment, float64, float64, float64, float64) {
	for len(fragments) > 0 && fragments[0].T < t {
		f := fragments[0]
		fragments = fragments[1:]
		inv := 1 - accA
		accR += inv * f.Color.R
		accG += inv * f.Color.G
		accB += inv * f.Color.B
		accA += inv * f.Color.A
	}
	return fragments, accR, accG, accB, accA
}

// dvrIndexGradient is a DVR sample's central-difference index gradient: the
// cut-plane gradient that reads clipped neighbours as clippedValue when the
// sample is next to a cut, else the cached cell's interpolated stencil or the
// exact central difference.
func dvrIndexGradient(sampler *volumeSampler, cell *volumeGradientCell, sample volumeTextureSample, tex Vec3, clip *VRClip, cutGradients, useCell bool, clippedValue float64) Vec3 {
	if cutGradients {
		if gradient, ok := sampler.clippedGradientIndex(tex, clip, clippedValue); ok {
			return gradient
		}
	}
	if useCell {
		return cell.gradientIndex(sampler, sample)
	}
	return sampler.gradientIndexAt(tex)
}

// vrCapGray is the linear-light gray of a cut-face cap (#1067): the modality
// value through the window as the MPR displays it, decoded from sRGB.
func vrCapGray(value float64, window WindowLevel) float64 {
	lower, upper := VRWindowBounds(window)
	if !(upper > lower) {
		return 0
	}
	return srgbToLinear(clampUnit((value - lower) / (upper - lower)))
}

func dvrTransferSample(tf VRTransferFunction, lut VRLUT, hu float64, window WindowLevel, dataMin, dataMax float64) RGBA {
	if tf.Domain() == VRTransferDomainNormalized {
		return lut.Lookup(tf.windowCoordinate(hu, window))
	}
	// Compatibility path for pre-clinical HU-authored functions. New clinical
	// presets never multiply their LUT alpha by a second window gain.
	densityData := normalizedVRDataCoordinate(hu, dataMin, dataMax)
	rgba := lut.Lookup(densityData)
	rgba.A *= windowedUnit(hu, window)
	return rgba
}

func normalizedVRDataCoordinate(value, minimum, maximum float64) float64 {
	if !finite(value) || !finite(minimum) || !finite(maximum) || maximum <= minimum {
		return 0
	}
	return clampUnit((value - minimum) / (maximum - minimum))
}

const vrCancellationCheckStride = 32

// intersectAABB returns the [t0,t1] entry/exit parameters of the ray with the
// axis-aligned box (slab method), or hit=false when it misses.
func intersectAABB(o, d, bmin, bmax Vec3) (t0, t1 float64, hit bool) {
	t0, t1 = math.Inf(-1), math.Inf(1)
	axis := func(oa, da, mn, mx float64) bool {
		if math.Abs(da) < 1e-12 {
			return oa >= mn && oa <= mx
		}
		inv := 1 / da
		ta := (mn - oa) * inv
		tb := (mx - oa) * inv
		if ta > tb {
			ta, tb = tb, ta
		}
		if ta > t0 {
			t0 = ta
		}
		if tb < t1 {
			t1 = tb
		}
		return t0 <= t1
	}
	if !axis(o.X, d.X, bmin.X, bmax.X) || !axis(o.Y, d.Y, bmin.Y, bmax.Y) || !axis(o.Z, d.Z, bmin.Z, bmax.Z) {
		return 0, 0, false
	}
	if t1 < 0 {
		return 0, 0, false
	}
	if t0 < 0 {
		t0 = 0
	}
	return t0, t1, true
}

// AdjustVRWindow applies a smooth windowing drag (3d-toolbar-plan.md §5.8): dx
// widens the window and dy raises the level, by width/100 per pixel and at
// least 1 (#1223), so narrow and wide windows are equally controllable; the
// width stays at least 1. The resulting window directly addresses both RGB
// and alpha in the normalized clinical LUT.
func AdjustVRWindow(wl WindowLevel, dx, dy float64) WindowLevel {
	step := math.Max(math.Abs(wl.Width)/100, 1)
	wl.Center -= dy * step
	wl.Width += dx * step
	if wl.Width < 1 {
		wl.Width = 1
	}
	return wl
}

func to8(v float64) uint8 {
	if v <= 0 {
		return 0
	}
	if v >= 1 {
		return 255
	}
	return uint8(v*255 + 0.5)
}
