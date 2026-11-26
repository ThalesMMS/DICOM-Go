package render

import (
	"context"
	"errors"
	"fmt"
	"image"
	"math"
	"sync"
	"sync/atomic"

	"github.com/ThalesMMS/dicom-go/pixeldata/display"
)

var (
	// ErrFusionMapping reports that a target-to-source mapper failed or
	// produced non-finite patient coordinates. No partial result is returned.
	ErrFusionMapping = errors.New("dicom/render: fused MPR mapping failed")
	// ErrFusionNoOverlap reports that every mapped overlay sample fell outside
	// the secondary volume. The returned scalar planes remain usable so callers
	// can display the base layer with an explicit diagnostic.
	ErrFusionNoOverlap = errors.New("dicom/render: fused MPR overlay has no sampled overlap")
	// ErrMapperOutsideDomain lets a PatientPointMapper report that one target
	// point lies outside the transform's qualified domain (for example beyond
	// a deformable registration grid). Mapper errors matching it through
	// errors.Is leave that overlay sample unset instead of failing the whole
	// reslice; every other mapper error remains atomic.
	ErrMapperOutsideDomain = errors.New("dicom/render: target point outside the mapper domain")
)

// PatientPointMapper maps a point on the target output geometry into the
// secondary source volume's patient coordinate system. A nil mapper is the
// identity mapping for volumes already expressed in the same Frame of
// Reference.
type PatientPointMapper func(Vec3) (Vec3, error)

// FusedMPRSlab defines one patient-space slab shared by both layers.
// ThicknessMM is the full extent centered on TargetPlane. SampleSpacingMM zero
// uses the smallest positive source spacing. Axis zero uses the plane normal.
type FusedMPRSlab struct {
	Mode            SlabMode
	Axis            Vec3
	ThicknessMM     float64
	SampleSpacingMM float64
}

// FusedMPRRequest describes scalar sampling before either layer's presentation
// is applied.
type FusedMPRRequest struct {
	Base           *Volume
	Overlay        *Volume
	TargetPlane    Plane
	Width          int
	Height         int
	TargetToSource PatientPointMapper
	Slab           FusedMPRSlab
	Limits         MPRLimits
}

// FusedMPRSamplingReport makes partial volume-domain coverage explicit. Counts
// are slab samples, not output pixels.
type FusedMPRSamplingReport struct {
	TargetSamples   int64
	BaseInBounds    int64
	OverlayInBounds int64
}

// FusedScalarPlanes retains independently sampled modality values for base and
// overlay on exactly the same target grid and slab extent.
type FusedScalarPlanes struct {
	Base    *ScalarPlane
	Overlay *ScalarPlane
	Report  FusedMPRSamplingReport
}

// Bytes reports the combined retained scalar payload.
func (p *FusedScalarPlanes) Bytes() int64 {
	if p == nil {
		return 0
	}
	return p.Base.Bytes() + p.Overlay.Bytes()
}

// FusionLayerPresentation controls one layer after scalar sampling. Alpha is
// clamped to [0,1]. A nil palette produces grayscale.
type FusionLayerPresentation struct {
	Window  WindowLevel
	Palette *display.PaletteColorLUT
	Alpha   float64
	// OpacityRamp scales Alpha over the window coordinate (#1069), sorted by
	// Position; empty keeps Alpha everywhere.
	OpacityRamp []FusionOpacityPoint
	// TransparentBelow draws nothing for values under the window floor
	// (#1069), so low uptake does not tint the layers below.
	TransparentBelow bool
	// Bands > 1 quantizes the palette into that many discrete colours.
	Bands int
}

// DefaultFusionPresentation returns an opaque grayscale base and a 50% gray
// overlay.
func DefaultFusionPresentation() (base, overlay FusionLayerPresentation) {
	return FusionLayerPresentation{Alpha: 1}, FusionLayerPresentation{Alpha: 0.5}
}

// ResliceFusedMPRScalarContext samples both volumes at the same patient-space
// locations and reduces the same physical slab extent before VOI or palette
// application. Mapper failure is atomic: no partially mapped planes escape.
func ResliceFusedMPRScalarContext(ctx context.Context, request FusedMPRRequest) (*FusedScalarPlanes, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if request.Base == nil {
		return nil, invalidMPRField("BaseVolume")
	}
	if request.Overlay == nil {
		return nil, invalidMPRField("OverlayVolume")
	}
	layers, err := resliceFusedLayersContext(ctx, request, 18)
	if err != nil {
		return nil, err
	}
	result := &FusedScalarPlanes{Base: layers.base, Overlay: layers.overlay, Report: layers.report}
	if result.Report.OverlayInBounds == 0 {
		return result, ErrFusionNoOverlap
	}
	return result, nil
}

// MappedMPRRequest samples one secondary volume on a target geometry that the
// volume does not own, such as the pixel grid of an already rendered 2D base
// slice. It uses the same patient-space mapping, trilinear modality-value
// sampling, and slab reduction as the overlay layer of
// ResliceFusedMPRScalarContext.
type MappedMPRRequest struct {
	Source         *Volume
	TargetPlane    Plane
	Width          int
	Height         int
	TargetToSource PatientPointMapper
	Slab           FusedMPRSlab
	Limits         MPRLimits
}

// MappedScalarPlane is the modality-value result of ResliceMappedMPRScalarContext.
// Report.BaseInBounds is always zero because no base volume is sampled.
type MappedScalarPlane struct {
	Plane  *ScalarPlane
	Report FusedMPRSamplingReport
}

// ResliceMappedMPRScalarContext samples request.Source at every target-plane
// pixel after mapping it through TargetToSource. Mapper failure is atomic, as
// in ResliceFusedMPRScalarContext; a mapper error matching
// ErrMapperOutsideDomain only leaves that sample unset. When no sample reaches
// the source volume the plane is returned together with ErrFusionNoOverlap.
func ResliceMappedMPRScalarContext(ctx context.Context, request MappedMPRRequest) (*MappedScalarPlane, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if request.Source == nil {
		return nil, invalidMPRField("SourceVolume")
	}
	layers, err := resliceFusedLayersContext(ctx, FusedMPRRequest{
		Overlay:        request.Source,
		TargetPlane:    request.TargetPlane,
		Width:          request.Width,
		Height:         request.Height,
		TargetToSource: request.TargetToSource,
		Slab:           request.Slab,
		Limits:         request.Limits,
	}, 9)
	if err != nil {
		return nil, err
	}
	result := &MappedScalarPlane{Plane: layers.overlay, Report: layers.report}
	if result.Report.OverlayInBounds == 0 {
		return result, ErrFusionNoOverlap
	}
	return result, nil
}

type fusedLayers struct {
	base    *ScalarPlane
	overlay *ScalarPlane
	report  FusedMPRSamplingReport
}

// resliceFusedLayersContext is the shared patient-space sampling kernel. A nil
// request.Base samples only the overlay layer.
func resliceFusedLayersContext(ctx context.Context, request FusedMPRRequest, retainedBytesPerPixel int64) (*fusedLayers, error) {
	axis, firstOffset, step, sampleCount, err := fusedSlabSampling(request)
	if err != nil {
		return nil, err
	}
	limits, err := validateMPRReslice(request.TargetPlane, request.Width, request.Height, sampleCount, retainedBytesPerPixel, request.Limits)
	if err != nil {
		return nil, err
	}
	var baseReader *VolumeReader
	if request.Base != nil {
		baseReader, err = request.Base.AcquireReader()
		if err != nil {
			return nil, err
		}
		defer baseReader.Close()
	}
	overlayReader, err := request.Overlay.AcquireReader()
	if err != nil {
		return nil, err
	}
	defer overlayReader.Close()

	var basePlane *ScalarPlane
	if request.Base != nil {
		basePlane, err = newScalarPlane(request.Width, request.Height, sampleCount, request.Base.Photometric(), defaultScalarVOI(), limits)
		if err != nil {
			return nil, err
		}
	}
	overlayPlane, err := newScalarPlane(request.Width, request.Height, sampleCount, request.Overlay.Photometric(), defaultScalarVOI(), limits)
	if err != nil {
		return nil, err
	}

	renderCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	var mappingOnce sync.Once
	var mappingErr error
	var baseInBounds atomic.Int64
	var overlayInBounds atomic.Int64
	denomW := float64(maxInt(request.Width-1, 1))
	denomH := float64(maxInt(request.Height-1, 1))
	err = parallelRowsContext(renderCtx, request.Height, func(y int) {
		t := float64(y) / denomH
		// Coverage is counted per row and published once, so workers do not
		// contend on the shared counters for every sample.
		var rowBaseInBounds, rowOverlayInBounds int64
		defer func() {
			baseInBounds.Add(rowBaseInBounds)
			overlayInBounds.Add(rowOverlayInBounds)
		}()
		for x := 0; x < request.Width; x++ {
			if renderCtx.Err() != nil {
				return
			}
			center := request.TargetPlane.At(float64(x)/denomW, t)
			var baseAcc, overlayAcc float64
			baseCount, overlayCount := 0, 0
			for sample := 0; sample < sampleCount; sample++ {
				if renderCtx.Err() != nil {
					return
				}
				target := center.Add(axis.Scale(firstOffset + float64(sample)*step))
				if baseReader != nil {
					if value, ok := baseReader.SamplePatient(target); ok {
						baseAcc, baseCount = accumulateSlabValue(request.Slab.Mode, baseAcc, baseCount, value)
						rowBaseInBounds++
					}
				}
				source := target
				if request.TargetToSource != nil {
					mapped, mapErr := request.TargetToSource(target)
					if mapErr != nil && errors.Is(mapErr, ErrMapperOutsideDomain) {
						continue
					}
					if mapErr != nil || !finiteMPRVec3(mapped) {
						mappingOnce.Do(func() {
							if mapErr == nil {
								mapErr = invalidMPRField("TargetToSource")
							}
							mappingErr = &FusionMappingError{Err: mapErr}
							cancel()
						})
						return
					}
					source = mapped
				}
				if value, ok := overlayReader.SamplePatient(source); ok {
					overlayAcc, overlayCount = accumulateSlabValue(request.Slab.Mode, overlayAcc, overlayCount, value)
					rowOverlayInBounds++
				}
			}
			if basePlane != nil {
				baseValue, baseOK := finishSlabValue(request.Slab.Mode, baseAcc, baseCount)
				basePlane.set(x, y, baseValue, baseOK)
			}
			overlayValue, overlayOK := finishSlabValue(request.Slab.Mode, overlayAcc, overlayCount)
			overlayPlane.set(x, y, overlayValue, overlayOK)
		}
	})
	if mappingErr != nil {
		return nil, mappingErr
	}
	if err != nil {
		return nil, err
	}
	return &fusedLayers{
		base:    basePlane,
		overlay: overlayPlane,
		report: FusedMPRSamplingReport{
			TargetSamples:   int64(request.Width) * int64(request.Height) * int64(sampleCount),
			BaseInBounds:    baseInBounds.Load(),
			OverlayInBounds: overlayInBounds.Load(),
		},
	}, nil
}

// FusionMappingError preserves the mapper's cause while also matching
// ErrFusionMapping through errors.Is.
type FusionMappingError struct {
	Err error
}

func (e *FusionMappingError) Error() string {
	if e == nil || e.Err == nil {
		return ErrFusionMapping.Error()
	}
	return fmt.Sprintf("%v: %v", ErrFusionMapping, e.Err)
}

func (e *FusionMappingError) Unwrap() error {
	if e == nil {
		return nil
	}
	return e.Err
}

func (e *FusionMappingError) Is(target error) bool {
	return target == ErrFusionMapping || (e != nil && errors.Is(e.Err, target))
}

func fusedSlabSampling(request FusedMPRRequest) (axis Vec3, firstOffset, step float64, sampleCount int, err error) {
	mode := request.Slab.Mode
	if mode < SlabNone || mode > SlabSum {
		return Vec3{}, 0, 0, 0, invalidMPRField("SlabMode")
	}
	thickness := request.Slab.ThicknessMM
	spacing := request.Slab.SampleSpacingMM
	if math.IsNaN(thickness) || math.IsInf(thickness, 0) || thickness < 0 {
		return Vec3{}, 0, 0, 0, invalidMPRField("SlabThicknessMM")
	}
	if math.IsNaN(spacing) || math.IsInf(spacing, 0) || spacing < 0 {
		return Vec3{}, 0, 0, 0, invalidMPRField("SlabSampleSpacingMM")
	}
	axis = request.Slab.Axis
	if axis.Length() == 0 {
		axis = request.TargetPlane.U.Cross(request.TargetPlane.V)
	}
	if !finiteMPRVec3(axis) || axis.Length() == 0 {
		return Vec3{}, 0, 0, 0, invalidMPRField("SlabAxis")
	}
	axis = axis.Normalize()
	if mode == SlabNone || thickness == 0 {
		return axis, 0, 0, 1, nil
	}
	if spacing == 0 {
		spacing = smallestPositiveVolumeSpacing(request.Base, request.Overlay)
	}
	if spacing <= 0 {
		return Vec3{}, 0, 0, 0, invalidMPRField("SlabSampleSpacingMM")
	}
	intervalsFloat := math.Ceil(thickness / spacing)
	if math.IsInf(intervalsFloat, 0) || intervalsFloat >= float64(MaxObliqueSlabSamples) {
		return Vec3{}, 0, 0, 0, limitedMPRField("SlabSamples")
	}
	intervals := maxInt(1, int(intervalsFloat))
	sampleCount = intervals + 1
	return axis, -thickness / 2, thickness / float64(intervals), sampleCount, nil
}

func smallestPositiveVolumeSpacing(volumes ...*Volume) float64 {
	spacing := math.Inf(1)
	for _, volume := range volumes {
		if volume == nil {
			continue
		}
		for _, candidate := range []float64{volume.ColSpacing, volume.RowSpacing, volume.SliceSpacing} {
			if finitePositiveSpacing(candidate) && candidate < spacing {
				spacing = candidate
			}
		}
	}
	if math.IsInf(spacing, 1) {
		return 0
	}
	return spacing
}

// Composite applies VOI, optional palettes, and source-over alpha independently
// to the retained base and overlay planes (FusedScalarLayers.Composite with
// FusionBlend).
func (p *FusedScalarPlanes) Composite(ctx context.Context, base, overlay FusionLayerPresentation) (image.Image, error) {
	if p == nil || p.Base == nil || p.Overlay == nil {
		return nil, invalidMPRField("FusedScalarPlanes")
	}
	layers := &FusedScalarLayers{Planes: []*ScalarPlane{p.Base, p.Overlay}}
	return layers.Composite(ctx, []FusionLayerPresentation{base, overlay}, FusionCompositeMode{})
}

// CompositeOver applies layer's VOI, optional palette, and source-over alpha to
// the retained modality values and draws them over base, an already presented
// image with the plane's dimensions (for example a 2D slice rendered with its
// own presentation state). Pixels without a sampled value keep base exactly.
// The result is a new premultiplied RGBA image; base is never modified.
func (p *ScalarPlane) CompositeOver(ctx context.Context, base image.Image, layer FusionLayerPresentation) (*image.RGBA, error) {
	if p == nil || base == nil {
		return nil, invalidMPRField("ScalarPlane")
	}
	if len(p.values) < p.width*p.height || len(p.valid) < p.width*p.height {
		return nil, invalidMPRField("ScalarPlane")
	}
	bounds := base.Bounds()
	if bounds.Dx() != p.width || bounds.Dy() != p.height {
		return nil, invalidMPRField("BaseImageDimensions")
	}
	if err := validateFusionPresentation(layer); err != nil {
		return nil, err
	}
	if ctx == nil {
		ctx = context.Background()
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	renderer := newFusionLayerRenderer(p, layer)
	out := image.NewRGBA(image.Rect(0, 0, p.width, p.height))
	gray, _ := base.(*image.Gray)
	rgba, _ := base.(*image.RGBA)
	err := parallelRowsContext(ctx, p.height, func(y int) {
		for x := 0; x < p.width; x++ {
			if ctx.Err() != nil {
				return
			}
			var baseR, baseG, baseB, baseA uint8
			switch {
			case gray != nil:
				value := gray.Pix[gray.PixOffset(bounds.Min.X+x, bounds.Min.Y+y)]
				baseR, baseG, baseB, baseA = value, value, value, 255
			case rgba != nil:
				offset := rgba.PixOffset(bounds.Min.X+x, bounds.Min.Y+y)
				baseR, baseG, baseB, baseA = rgba.Pix[offset], rgba.Pix[offset+1], rgba.Pix[offset+2], rgba.Pix[offset+3]
			default:
				r, g, b, a := base.At(bounds.Min.X+x, bounds.Min.Y+y).RGBA()
				baseR, baseG, baseB, baseA = uint8(r>>8), uint8(g>>8), uint8(b>>8), uint8(a>>8)
			}
			index := (y*p.width + x) * 4
			sample := y*p.width + x
			var r, g, b uint8
			var layerAlpha float64
			if p.valid[sample] != 0 {
				r, g, b, layerAlpha = renderer.sample(p.values[sample])
			}
			if layerAlpha <= 0 {
				out.Pix[index+0], out.Pix[index+1], out.Pix[index+2], out.Pix[index+3] = baseR, baseG, baseB, baseA
				continue
			}
			red, green, blue, alpha := compositeFusionLayer(
				float64(baseR)/255, float64(baseG)/255, float64(baseB)/255, float64(baseA)/255,
				r, g, b, layerAlpha,
			)
			out.Pix[index+0] = byte(math.Round(clampUnit(red) * 255))
			out.Pix[index+1] = byte(math.Round(clampUnit(green) * 255))
			out.Pix[index+2] = byte(math.Round(clampUnit(blue) * 255))
			out.Pix[index+3] = byte(math.Round(clampUnit(alpha) * 255))
		}
	})
	if err != nil {
		return nil, err
	}
	return out, nil
}

func compositeFusionLayer(dstR, dstG, dstB, dstA float64, sourceR, sourceG, sourceB uint8, sourceA float64) (float64, float64, float64, float64) {
	sourceA = clampUnit(sourceA)
	oneMinusSource := 1 - sourceA
	return float64(sourceR)/255*sourceA + dstR*oneMinusSource,
		float64(sourceG)/255*sourceA + dstG*oneMinusSource,
		float64(sourceB)/255*sourceA + dstB*oneMinusSource,
		sourceA + dstA*oneMinusSource
}
