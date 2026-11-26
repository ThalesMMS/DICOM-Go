package render

import (
	"context"
	"fmt"
	"image"
	"math"
)

// ScalarPlane is an immutable MPR geometry result in modality-value space.
// It retains only source fallback metadata needed by the standalone
// presentation API; explicit Window/Level and VOI LUT state remain external so
// callers may reuse the sampled values across presentation changes.
type ScalarPlane struct {
	width       int
	height      int
	values      []float64
	valid       []byte
	photometric string
	defaultVOI  WindowLevel
	antialiased bool
}

// Antialiased reports whether this plane sampled the averaged 2x volume level.
// These values are filtered modality values, not unchanged source samples.
func (p *ScalarPlane) Antialiased() bool { return p != nil && p.antialiased }

// Dimensions returns the immutable output dimensions.
func (p *ScalarPlane) Dimensions() (int, int) {
	if p == nil {
		return 0, 0
	}
	return p.width, p.height
}

// Bytes reports the retained payload size used by bounded viewer caches.
func (p *ScalarPlane) Bytes() int64 {
	if p == nil {
		return 0
	}
	return int64(len(p.values))*8 + int64(len(p.valid))
}

// ValueAt returns one retained modality value. ok is false outside the plane
// or where patient-space sampling found no source voxel.
func (p *ScalarPlane) ValueAt(x, y int) (float64, bool) {
	if p == nil || x < 0 || y < 0 || x >= p.width || y >= p.height {
		return 0, false
	}
	index := y*p.width + x
	if index >= len(p.values) || index >= len(p.valid) || p.valid[index] == 0 {
		return 0, false
	}
	return p.values[index], true
}

// ApplyWindow applies VOI/LUT presentation without recomputing MPR geometry.
func (p *ScalarPlane) ApplyWindow(window WindowLevel) image.Image {
	img, _ := p.ApplyWindowContext(context.Background(), window)
	return img
}

// ApplyWindowContext is the cancellable presentation pass for a scalar plane.
func (p *ScalarPlane) ApplyWindowContext(ctx context.Context, window WindowLevel) (image.Image, error) {
	if p == nil || p.width <= 0 || p.height <= 0 ||
		len(p.values) < p.width*p.height || len(p.valid) < p.width*p.height {
		return blankImage(512, 512), nil
	}
	if ctx == nil {
		ctx = context.Background()
	}
	mapper := prepareWindow(normalizeWindow(window, p.defaultVOI))
	img := image.NewGray(image.Rect(0, 0, p.width, p.height))
	err := parallelRowsContext(ctx, p.height, func(y int) {
		offset := y * p.width
		for x := 0; x < p.width; x++ {
			if ctx.Err() != nil {
				return
			}
			index := offset + x
			if p.valid[index] == 0 {
				continue
			}
			img.Pix[index] = displayGrayMapped(float64(p.values[index]), mapper, p.photometric)
		}
	})
	if err != nil {
		return nil, err
	}
	return img, nil
}

// RenderScalarSlabWithOptionsContext renders an orthogonal MPR plane in
// modality-value space. Cancellation is checked between rows, pixels, and slab
// samples so interactive supersession does not wait for a whole projection.
func RenderScalarSlabWithOptionsContext(
	ctx context.Context,
	series *Stack,
	plane MPRPlane,
	center, thickness int,
	mode SlabMode,
	options MPRRenderOptions,
) (*ScalarPlane, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	if series == nil || len(series.Frames) == 0 {
		return nil, fmt.Errorf("render: no series selected")
	}
	first := firstRenderableSlice(series)
	if first == nil {
		return nil, fmt.Errorf("render: series has no displayable images")
	}
	rows := int(first.Metadata.Rows)
	cols := int(first.Metadata.Columns)
	if rows <= 0 || cols <= 0 {
		return nil, fmt.Errorf("render: invalid slice dimensions")
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if mode < SlabNone || mode > SlabSum {
		return nil, invalidMPRField("SlabMode")
	}
	limits, err := normalizeMPRLimits(options.Limits)
	if err != nil {
		return nil, err
	}

	axisCount := len(series.Frames)
	switch plane {
	case MPRPlaneAxial:
	case MPRPlaneCoronal:
		axisCount = rows
	case MPRPlaneSagittal:
		axisCount = cols
	default:
		return nil, invalidMPRField("Plane")
	}
	start, end := scalarProjectionRange(center, thickness, axisCount, mode)
	if _, err := options.Slab.SampleCount(end-start+1, limits); err != nil {
		return nil, err
	}

	correctTilt := options.GantryTiltMode != GantryTiltSourceGeometry
	defaultVOI := normalizeWindow(
		series.DefaultWindow,
		WindowLevel{Center: defaultWindowCenter, Width: defaultWindowWidth},
	)
	// The legacy CPU oracle renders a plain axial plane directly from its
	// source frame (float64 rescale), while projected slabs use the cached
	// float32 volume. Preserve that distinction exactly.
	if plane == MPRPlaneAxial && mode == SlabNone {
		return renderFallbackScalarSlabContext(
			ctx, series, plane, center, thickness, mode, rows, cols, defaultVOI, limits, options,
		)
	}
	if vol, sampler, err := stackVolumeSampler(series, correctTilt); err == nil {
		defer sampler.Close()
		if scalar, rendered, renderErr := renderCachedScalarSlabContext(
			ctx, vol, sampler, plane, center, thickness, mode, correctTilt, defaultVOI, limits, options,
		); rendered || renderErr != nil {
			return scalar, renderErr
		}
	} else if isNonCorrectableGeometry(err) {
		return nil, err
	}
	return renderFallbackScalarSlabContext(ctx, series, plane, center, thickness, mode, rows, cols, defaultVOI, limits, options)
}

func renderCachedScalarSlabContext(
	ctx context.Context,
	vol *Volume,
	sampler *volumeSampler,
	plane MPRPlane,
	center, thickness int,
	mode SlabMode,
	correctGantryTilt bool,
	defaultVOI WindowLevel,
	limits MPRLimits,
	options MPRRenderOptions,
) (*ScalarPlane, bool, error) {
	if vol == nil || sampler == nil || sampler.rows <= 0 || sampler.cols <= 0 || sampler.depth <= 0 {
		return nil, false, nil
	}
	if correctGantryTilt {
		if corrected, width, height, ok := vol.correctedOrthogonalPlane(plane, center); ok {
			if mode == SlabNone || thickness <= 1 {
				scalar, err := resliceScalarWithSamplerContext(ctx, sampler, corrected, width, height, defaultVOI, limits, options.Interpolation)
				return scalar, true, err
			}
			slabAxis, step, sampleCount := vol.correctedSlabAxis(plane, vol.Rows, vol.Cols)
			clampedCenter := clampIndex(center, sampleCount)
			start, end := mipProjectionRange(clampedCenter, thickness, sampleCount)
			samples, err := options.Slab.SampleCount(end-start+1, limits)
			if err != nil {
				return nil, true, err
			}
			firstOffset := float64(start - clampedCenter)
			if samples > 1 && samples != end-start+1 {
				refinedStep := step * float64(end-start) / float64(samples-1)
				firstOffset *= step / refinedStep
				step = refinedStep
			}
			scalar, err := resliceScalarSlabWithSamplerContext(
				ctx, sampler, corrected, slabAxis, width, height,
				firstOffset, samples, step, mode, defaultVOI, limits, options.Interpolation, options.Slab.Trapezoid,
			)
			return scalar, true, err
		}
	}

	rows, cols, depth := sampler.rows, sampler.cols, sampler.depth
	switch plane {
	case MPRPlaneAxial:
		start, end := scalarProjectionRange(center, thickness, depth, mode)
		samples, err := options.Slab.SampleCount(end-start+1, limits)
		if err != nil {
			return nil, true, err
		}
		scalar, allocationErr := newScalarPlane(cols, rows, samples, sampler.photometric, defaultVOI, limits)
		if allocationErr != nil {
			return nil, true, allocationErr
		}
		err = renderCachedScalarSlabRowsContext(ctx, scalar, sampler, plane, start, end, samples, mode, options.Interpolation, options.Slab.Trapezoid)
		if err != nil {
			return nil, true, err
		}
		return scalar, true, nil
	case MPRPlaneCoronal:
		start, end := scalarProjectionRange(center, thickness, rows, mode)
		samples, err := options.Slab.SampleCount(end-start+1, limits)
		if err != nil {
			return nil, true, err
		}
		height := orthogonalVolumeHeight(depth, vol.SliceSpacing, vol.ColSpacing)
		scalar, allocationErr := newScalarPlane(cols, height, samples, sampler.photometric, defaultVOI, limits)
		if allocationErr != nil {
			return nil, true, allocationErr
		}
		err = renderCachedScalarSlabRowsContext(ctx, scalar, sampler, plane, start, end, samples, mode, options.Interpolation, options.Slab.Trapezoid)
		if err != nil {
			return nil, true, err
		}
		return scalar, true, nil
	case MPRPlaneSagittal:
		start, end := scalarProjectionRange(center, thickness, cols, mode)
		samples, err := options.Slab.SampleCount(end-start+1, limits)
		if err != nil {
			return nil, true, err
		}
		height := orthogonalVolumeHeight(depth, vol.SliceSpacing, vol.RowSpacing)
		scalar, allocationErr := newScalarPlane(rows, height, samples, sampler.photometric, defaultVOI, limits)
		if allocationErr != nil {
			return nil, true, allocationErr
		}
		err = renderCachedScalarSlabRowsContext(ctx, scalar, sampler, plane, start, end, samples, mode, options.Interpolation, options.Slab.Trapezoid)
		if err != nil {
			return nil, true, err
		}
		return scalar, true, nil
	default:
		return nil, false, nil
	}
}

func renderFallbackScalarSlabContext(
	ctx context.Context,
	series *Stack,
	plane MPRPlane,
	center, thickness int,
	mode SlabMode,
	rows, cols int,
	defaultVOI WindowLevel,
	limits MPRLimits,
	options MPRRenderOptions,
) (*ScalarPlane, error) {
	photometric := firstRenderableSlice(series).Metadata.PhotometricInterpretation
	switch plane {
	case MPRPlaneAxial:
		start, end := scalarProjectionRange(center, thickness, len(series.Frames), mode)
		samples, err := options.Slab.SampleCount(end-start+1, limits)
		if err != nil {
			return nil, err
		}
		scalar, err := newScalarPlane(cols, rows, samples, photometric, defaultVOI, limits)
		if err != nil {
			return nil, err
		}
		err = parallelRowsContext(ctx, rows, func(y int) {
			for x := 0; x < cols; x++ {
				value, ok := reduceSampledScalarContext(ctx, mode, start, end, samples, options.Slab.Trapezoid, func(coordinate float64) (float64, bool) {
					return interpolateScalarAxis(coordinate, len(series.Frames), options.Interpolation, func(z int) (float64, bool) {
						value, _, ok := sampleVoxel(series.Frames[z], x, y)
						return value, ok
					})
				})
				scalar.set(x, y, value, ok)
			}
		})
		if err != nil {
			return nil, err
		}
		return scalar, nil
	case MPRPlaneCoronal:
		start, end := scalarProjectionRange(center, thickness, rows, mode)
		samples, err := options.Slab.SampleCount(end-start+1, limits)
		if err != nil {
			return nil, err
		}
		height := orthogonalMPRHeight(series, seriesColumnSpacing(series))
		scalar, err := newScalarPlane(cols, height, samples, photometric, defaultVOI, limits)
		if err != nil {
			return nil, err
		}
		err = parallelRowsContext(ctx, height, func(outZ int) {
			frame := series.Frames[orthogonalDisplaySliceIndex(outZ, height, len(series.Frames))]
			for x := 0; x < cols; x++ {
				value, ok := reduceSampledScalarContext(ctx, mode, start, end, samples, options.Slab.Trapezoid, func(coordinate float64) (float64, bool) {
					return interpolateScalarAxis(coordinate, rows, options.Interpolation, func(y int) (float64, bool) {
						value, _, ok := sampleVoxel(frame, x, y)
						return value, ok
					})
				})
				scalar.set(x, outZ, value, ok)
			}
		})
		if err != nil {
			return nil, err
		}
		return scalar, nil
	case MPRPlaneSagittal:
		start, end := scalarProjectionRange(center, thickness, cols, mode)
		samples, err := options.Slab.SampleCount(end-start+1, limits)
		if err != nil {
			return nil, err
		}
		height := orthogonalMPRHeight(series, seriesRowSpacing(series))
		scalar, err := newScalarPlane(rows, height, samples, photometric, defaultVOI, limits)
		if err != nil {
			return nil, err
		}
		err = parallelRowsContext(ctx, height, func(outZ int) {
			frame := series.Frames[orthogonalDisplaySliceIndex(outZ, height, len(series.Frames))]
			for y := 0; y < rows; y++ {
				value, ok := reduceSampledScalarContext(ctx, mode, start, end, samples, options.Slab.Trapezoid, func(coordinate float64) (float64, bool) {
					return interpolateScalarAxis(coordinate, cols, options.Interpolation, func(x int) (float64, bool) {
						value, _, ok := sampleVoxel(frame, x, y)
						return value, ok
					})
				})
				scalar.set(y, outZ, value, ok)
			}
		})
		if err != nil {
			return nil, err
		}
		return scalar, nil
	default:
		return nil, fmt.Errorf("render: unsupported slab plane %q", plane)
	}
}

// ResliceObliqueScalarContext renders one arbitrary plane without VOI.
func ResliceObliqueScalarContext(ctx context.Context, vol *Volume, plane Plane, outW, outH int) (*ScalarPlane, error) {
	return ResliceObliqueScalarWithLimitsContext(ctx, vol, plane, outW, outH, MPRLimits{})
}

// ResliceObliqueScalarWithLimitsContext validates resource ceilings before
// retaining float64 modality values and the validity mask.
func ResliceObliqueScalarWithLimitsContext(ctx context.Context, vol *Volume, plane Plane, outW, outH int, limits MPRLimits) (*ScalarPlane, error) {
	return ResliceObliqueScalarWithOptionsContext(ctx, vol, plane, outW, outH, MPRResliceOptions{Limits: limits})
}

// MPRResliceOptions tunes the oblique scalar reslices.
type MPRResliceOptions struct {
	Limits MPRLimits
	// Antialias2x opts single-plane scalar MPR into the cached 2x BinShrink
	// level when both output pixel spacings are at least twice the largest
	// source spacing. Native-density and unsupported grids retain the original
	// source. Nearest-neighbor label sampling cannot enable this option.
	// It does not apply to slab reductions or GPU application presentation.
	Antialias2x bool
	// Slab refines the thick-slab variant.
	Slab SlabSampling
	// Interpolation of the samples (zero value: trilinear): cubic for
	// settled frames, nearest for labelmaps.
	Interpolation Interpolation
}

// ResliceObliqueScalarWithOptionsContext renders one arbitrary plane without
// VOI with the given limits and interpolation.
func ResliceObliqueScalarWithOptionsContext(ctx context.Context, vol *Volume, plane Plane, outW, outH int, options MPRResliceOptions) (*ScalarPlane, error) {
	limits := options.Limits
	if ctx == nil {
		ctx = context.Background()
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	limits, err := validateMPRReslice(plane, outW, outH, 1, 9, limits)
	if err != nil {
		return nil, err
	}
	if options.Antialias2x && options.Interpolation == InterpolationNearest {
		return nil, invalidMPRField("Antialias2x")
	}
	if vol == nil {
		return newScalarPlane(outW, outH, 1, "", defaultScalarVOI(), limits)
	}
	antialiased := false
	if options.Antialias2x {
		vol, antialiased, err = mprAntialiasVolume(ctx, vol, plane, outW, outH, limits)
		if err != nil {
			return nil, err
		}
		sampler, err := newVolumeSamplerWithError(vol)
		if err != nil {
			return nil, err
		}
		defer sampler.Close()
		result, err := resliceScalarWithSamplerContext(ctx, sampler, plane, outW, outH, defaultScalarVOI(), limits, options.Interpolation)
		if result != nil {
			result.antialiased = antialiased
		}
		return result, err
	}
	sampler, ok := newVolumeSampler(vol)
	if !ok {
		return newScalarPlane(outW, outH, 1, vol.Photometric(), defaultScalarVOI(), limits)
	}
	defer sampler.Close()
	return resliceScalarWithSamplerContext(ctx, sampler, plane, outW, outH, defaultScalarVOI(), limits, options.Interpolation)
}

// ResliceObliqueSlabScalarContext renders an arbitrary thick slab without VOI.
func ResliceObliqueSlabScalarContext(
	ctx context.Context,
	vol *Volume,
	plane Plane,
	outW, outH, thickness int,
	mode SlabMode,
) (*ScalarPlane, error) {
	return ResliceObliqueSlabScalarWithLimitsContext(ctx, vol, plane, outW, outH, thickness, mode, MPRLimits{})
}

// ResliceObliqueSlabScalarWithLimitsContext is the bounded scalar thick-slab
// variant. Dimension, retained bytes, and sample work are checked up front.
func ResliceObliqueSlabScalarWithLimitsContext(
	ctx context.Context,
	vol *Volume,
	plane Plane,
	outW, outH, thickness int,
	mode SlabMode,
	limits MPRLimits,
) (*ScalarPlane, error) {
	return ResliceObliqueSlabScalarWithOptionsContext(ctx, vol, plane, outW, outH, thickness, mode, MPRResliceOptions{Limits: limits})
}

// ResliceObliqueSlabScalarWithOptionsContext is the thick-slab variant with
// the given limits and interpolation.
func ResliceObliqueSlabScalarWithOptionsContext(
	ctx context.Context,
	vol *Volume,
	plane Plane,
	outW, outH, thickness int,
	mode SlabMode,
	options MPRResliceOptions,
) (*ScalarPlane, error) {
	limits := options.Limits
	if ctx == nil {
		ctx = context.Background()
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if mode < SlabNone || mode > SlabSum {
		return nil, invalidMPRField("SlabMode")
	}
	coarseSamples := 1
	if thickness > 1 && mode != SlabNone {
		coarseSamples = thickness
	}
	slabSamples, err := options.Slab.SampleCount(coarseSamples, limits)
	if err != nil {
		return nil, err
	}
	limits, err = validateMPRReslice(plane, outW, outH, slabSamples, 9, limits)
	if err != nil {
		return nil, err
	}
	if thickness <= 1 || mode == SlabNone {
		options.Limits = limits
		return ResliceObliqueScalarWithOptionsContext(ctx, vol, plane, outW, outH, options)
	}
	if options.Antialias2x {
		return nil, invalidMPRField("Antialias2x")
	}
	if vol == nil {
		return newScalarPlane(outW, outH, slabSamples, "", defaultScalarVOI(), limits)
	}
	sampler, ok := newVolumeSampler(vol)
	if !ok {
		return newScalarPlane(outW, outH, slabSamples, vol.Photometric(), defaultScalarVOI(), limits)
	}
	defer sampler.Close()
	half := float64(slabSamples-1) / 2
	step := vol.SliceSpacing * float64(thickness-1) / float64(slabSamples-1)
	return resliceScalarSlabWithSamplerContext(
		ctx, sampler, plane, plane.U.Cross(plane.V), outW, outH,
		-half, slabSamples, step, mode, defaultScalarVOI(), limits, options.Interpolation, options.Slab.Trapezoid,
	)
}

// SlabSampling refines orthogonal, tilt-corrected and oblique scalar slabs.
// The zero value preserves the original sampling and reduction.
type SlabSampling struct {
	// Trapezoid weights the first and last samples by one half in Average
	// and Sum (the trapezoid rule over the slab; Average then divides by
	// the summed weights).
	Trapezoid bool
	// SpacingFraction in (0,1] requests that fraction of source-axis
	// spacing over the same extent: thickness 5 at 0.5 takes 9 samples.
	// The interval count is rounded to retain both endpoints; the effective
	// spacing can therefore differ slightly from the requested fraction.
	// Zero means 1.
	SpacingFraction float64
}

// SampleCount computes bounded work for a slab with coarseSamples positions.
// Refinement retains its endpoints, using round((coarseSamples-1)/fraction)
// intervals. Limits are checked before converting the float count to int.
func (s SlabSampling) SampleCount(coarseSamples int, limits MPRLimits) (int, error) {
	fraction := s.SpacingFraction
	if fraction == 0 {
		fraction = 1
	}
	if !(fraction > 0 && fraction <= 1) {
		return 0, invalidMPRField("Slab.SpacingFraction")
	}
	limits, err := normalizeMPRLimits(limits)
	if err != nil {
		return 0, err
	}
	if coarseSamples < 1 {
		return 0, invalidMPRField("SlabSamples")
	}
	intervals := math.Round(float64(coarseSamples-1) / fraction)
	if math.IsInf(intervals, 0) || intervals > float64(limits.MaxSlabSamples-1) {
		return 0, limitedMPRField("SlabSamples")
	}
	return int(intervals) + 1, nil
}

// interpolateScalarAxis reuses the source-grid interpolation kernels while
// keeping fallback rescale values in float64 and the default integer samples
// unchanged. All contributing taps must have a valid source value.
func interpolateScalarAxis(coordinate float64, count int, interpolation Interpolation, sample func(int) (float64, bool)) (float64, bool) {
	if coordinate == math.Trunc(coordinate) {
		return sample(int(coordinate))
	}
	if interpolation == InterpolationNearest {
		return sample(min(NearestIndex(coordinate), count-1))
	}
	if interpolation == InterpolationCubic {
		indices, weights, taps := cubicTaps(coordinate, count)
		value := 0.0
		for k := 0; k < taps; k++ {
			v, ok := sample(indices[k])
			if !ok {
				return 0, false
			}
			value += weights[k] * v
		}
		return value, true
	}
	lo := int(math.Floor(coordinate))
	a, aOK := sample(lo)
	b, bOK := sample(min(lo+1, count-1))
	if !aOK || !bOK {
		return 0, false
	}
	fraction := coordinate - float64(lo)
	return a*(1-fraction) + b*fraction, true
}

func reduceSampledScalarContext(ctx context.Context, mode SlabMode, start, end, count int, trapezoid bool, sample func(float64) (float64, bool)) (float64, bool) {
	at := func(k int) (float64, bool) {
		if ctx != nil && ctx.Err() != nil {
			return 0, false
		}
		coordinate := float64(start)
		if count > 1 {
			coordinate += float64(k) * float64(end-start) / float64(count-1)
			if k == count-1 {
				coordinate = float64(end)
			}
		}
		return sample(coordinate)
	}
	if trapezoid && (mode == SlabAverage || mode == SlabSum) {
		return reduceTrapezoid(mode, count, at)
	}
	return reduceCachedScalarContext(ctx, mode, 0, count-1, at)
}

func resliceScalarWithSamplerContext(
	ctx context.Context,
	sampler *volumeSampler,
	plane Plane,
	outW, outH int,
	defaultVOI WindowLevel,
	limits MPRLimits,
	interpolation Interpolation,
) (*ScalarPlane, error) {
	scalar, err := newScalarPlane(outW, outH, 1, sampler.photometric, defaultVOI, limits)
	if err != nil {
		return nil, err
	}
	denomW := float64(maxInt(outW-1, 1))
	denomH := float64(maxInt(outH-1, 1))
	column := func(x int) float64 { return float64(x) / denomW }
	err = parallelRowsWorkerContext(ctx, outH, func() func(int) {
		// Cubic rows along a voxel axis read precomputed taps (#1256).
		var plan *cubicLinePlan
		if interpolation == InterpolationCubic {
			plan = newCubicLinePlan(sampler, outW, column)
		}
		return func(y int) {
			t := float64(y) / denomH
			row := sampler.vol.voxelLine(plane.Origin.Add(plane.V.Scale(t)), plane.U)
			if plan != nil && plan.prepare(row) {
				for x := 0; x < outW; x++ {
					if ctx.Err() != nil {
						return
					}
					value, ok := plan.sample(x)
					scalar.set(x, y, value, ok)
				}
				return
			}
			for x := 0; x < outW; x++ {
				if ctx.Err() != nil {
					return
				}
				value, ok := sampler.sampleAt(row.at(column(x)), interpolation)
				scalar.set(x, y, value, ok)
			}
		}
	})
	if err != nil {
		return nil, err
	}
	return scalar, nil
}

func resliceScalarSlabWithSamplerContext(
	ctx context.Context,
	sampler *volumeSampler,
	plane Plane,
	slabAxis Vec3,
	outW, outH int,
	firstOffset float64,
	sampleCount int,
	step float64,
	mode SlabMode,
	defaultVOI WindowLevel,
	limits MPRLimits,
	interpolation Interpolation,
	trapezoid bool,
) (*ScalarPlane, error) {
	if step <= 0 {
		step = 1
	}
	normal := slabAxis.Normalize()
	if scalar, rendered, err := resliceScalarSlabRowsContext(ctx, sampler, plane, normal, outW, outH,
		firstOffset, sampleCount, step, mode, defaultVOI, limits, interpolation, trapezoid); rendered {
		return scalar, err
	}
	scalar, err := newScalarPlane(outW, outH, sampleCount, sampler.photometric, defaultVOI, limits)
	if err != nil {
		return nil, err
	}
	denomW := float64(maxInt(outW-1, 1))
	denomH := float64(maxInt(outH-1, 1))
	offset := func(sample int) float64 { return (firstOffset + float64(sample)) * step }
	err = parallelRowsWorkerContext(ctx, outH, func() func(int) {
		// Cubic rays along a voxel axis read precomputed taps (#1256).
		var plan *cubicLinePlan
		if interpolation == InterpolationCubic {
			plan = newCubicLinePlan(sampler, sampleCount, offset)
		}
		return func(y int) {
			t := float64(y) / denomH
			for x := 0; x < outW; x++ {
				if ctx.Err() != nil {
					return
				}
				ray := sampler.vol.voxelLine(plane.At(float64(x)/denomW, t), normal)
				sample := func(sample int) (float64, bool) {
					if ctx.Err() != nil {
						return 0, false
					}
					return sampler.sampleAt(ray.at(offset(sample)), interpolation)
				}
				if plan != nil && plan.prepare(ray) {
					sample = func(k int) (float64, bool) {
						if ctx.Err() != nil {
							return 0, false
						}
						return plan.sample(k)
					}
				}
				var value float64
				var ok bool
				if trapezoid && (mode == SlabAverage || mode == SlabSum) {
					value, ok = reduceTrapezoid(mode, sampleCount, sample)
				} else {
					value, ok = reduceCachedScalarContext(ctx, mode, 0, sampleCount-1, sample)
				}
				scalar.set(x, y, value, ok)
			}
		}
	})
	if err != nil {
		return nil, err
	}
	return scalar, nil
}

func newScalarPlane(width, height, slabSamples int, photometric string, defaultVOI WindowLevel, limits MPRLimits) (*ScalarPlane, error) {
	limits, err := validateMPROutput(width, height, slabSamples, 9, limits)
	if err != nil {
		return nil, err
	}
	count := int64(width) * int64(height)
	return &ScalarPlane{
		width:       width,
		height:      height,
		values:      make([]float64, int(count)),
		valid:       make([]byte, int(count)),
		photometric: photometric,
		defaultVOI:  normalizeWindow(defaultVOI, defaultScalarVOI()),
	}, nil
}

func defaultScalarVOI() WindowLevel {
	return WindowLevel{Center: defaultWindowCenter, Width: defaultWindowWidth}
}

func (p *ScalarPlane) set(x, y int, value float64, ok bool) {
	if p == nil || !ok || x < 0 || y < 0 || x >= p.width || y >= p.height {
		return
	}
	index := y*p.width + x
	p.values[index] = value
	p.valid[index] = 1
}

func scalarProjectionRange(center, thickness, count int, mode SlabMode) (int, int) {
	if mode == SlabNone {
		thickness = 1
	}
	return mipProjectionRange(center, thickness, count)
}

// reduceTrapezoid is the trapezoid-rule Sum or Average of count samples:
// the first and last weigh one half; Average divides by the weights of the
// samples inside the volume.
func reduceTrapezoid(mode SlabMode, count int, sample func(int) (float64, bool)) (float64, bool) {
	sum, weight := 0.0, 0.0
	for k := 0; k < count; k++ {
		value, ok := sample(k)
		if !ok {
			continue
		}
		w := 1.0
		if count > 1 && (k == 0 || k == count-1) {
			w = 0.5
		}
		sum += w * value
		weight += w
	}
	if weight == 0 {
		return 0, false
	}
	if mode == SlabAverage {
		return sum / weight, true
	}
	return sum, true
}

func reduceCachedScalarContext(
	ctx context.Context,
	mode SlabMode,
	start, end int,
	sample func(int) (float64, bool),
) (float64, bool) {
	var acc float64
	count := 0
	for index := start; index <= end; index++ {
		if ctx != nil && ctx.Err() != nil {
			return 0, false
		}
		value, ok := sample(index)
		if !ok {
			continue
		}
		acc, count = accumulateSlabValue(mode, acc, count, value)
	}
	return finishSlabValue(mode, acc, count)
}
