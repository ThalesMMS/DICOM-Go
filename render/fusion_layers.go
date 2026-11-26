package render

import (
	"context"
	"errors"
	"image"
	"math"

	"github.com/ThalesMMS/dicom-go/pixeldata/display"
)

// Clinical fusion semantics (#1069), after VTK's vtkLookupTable (below-range
// colour), vtkDiscretizableColorTransferFunction (discrete bands),
// vtkImageStack (layers composited in order) and vtkImageCheckerboard
// (written from their behaviour, no VTK code is copied): each layer's
// opacity follows a ramp over its window, values under the window floor can
// be transparent so low PET uptake no longer tints the CT, palettes can be
// banded, any number of layers composite in order, and two layers can be
// compared by checkerboard, swipe or difference.

// FusionOpacityPoint is one node of a layer's opacity ramp: Opacity at the
// window coordinate Position (0 at the window floor, 1 at its ceiling).
type FusionOpacityPoint struct {
	Position float64
	Opacity  float64
}

// FusionCompositeKind selects how fused layers combine.
type FusionCompositeKind int

const (
	// FusionBlend draws every layer source-over, in order.
	FusionBlend FusionCompositeKind = iota
	// FusionCheckerboard alternates the first two layers in square tiles.
	FusionCheckerboard
	// FusionSwipe shows the first layer left of a vertical line and the
	// second right of it.
	FusionSwipe
	// FusionDifference shows the second layer minus the first through a
	// diverging palette.
	FusionDifference
)

// FusionCompositeMode configures a fused composite.
type FusionCompositeMode struct {
	Kind FusionCompositeKind
	// CheckerSizePx is the checkerboard tile side (default 32).
	CheckerSizePx int
	// SwipePosition is the swipe line as a fraction of the width (default
	// 0.5).
	SwipePosition float64
	// DifferenceRange is the modality difference mapped to either end of the
	// diverging palette; zero uses the second layer's window width.
	DifferenceRange float64
	// DifferencePalette overrides display.CoolWarmLUT.
	DifferencePalette *display.PaletteColorLUT
}

// DefaultFusionCheckerSizePx is the default checkerboard tile side.
const DefaultFusionCheckerSizePx = 32

// fusionLayerRenderer turns a layer's modality values into colour and
// opacity.
type fusionLayerRenderer struct {
	mapper       preparedVOI
	photometric  string
	presentation FusionLayerPresentation
	lower        float64
	window       WindowLevel
}

func newFusionLayerRenderer(plane *ScalarPlane, presentation FusionLayerPresentation) fusionLayerRenderer {
	presentation.Alpha = clampUnit(presentation.Alpha)
	window := normalizeWindow(presentation.Window, plane.defaultVOI)
	return fusionLayerRenderer{
		mapper:       prepareWindow(window),
		photometric:  plane.photometric,
		presentation: presentation,
		lower:        window.Center - window.Width/2,
		window:       window,
	}
}

// sample returns the layer's colour and opacity for value.
func (r *fusionLayerRenderer) sample(value float64) (uint8, uint8, uint8, float64) {
	p := &r.presentation
	if p.TransparentBelow && value < r.lower {
		return 0, 0, 0, 0
	}
	gray := displayGrayMapped(value, r.mapper, r.photometric)
	coordinate := float64(gray) / 255
	if p.Bands > 1 {
		band := min(p.Bands-1, int(coordinate*float64(p.Bands)))
		gray = uint8(math.Round(float64(band) * 255 / float64(p.Bands-1)))
	}
	alpha := p.Alpha * fusionRampOpacity(p.OpacityRamp, coordinate)
	if p.Palette == nil {
		return gray, gray, gray, alpha
	}
	red, green, blue := p.Palette.At(int(gray))
	return red, green, blue, alpha
}

// fusionRampOpacity interpolates the ramp at x; an empty ramp is opaque.
func fusionRampOpacity(ramp []FusionOpacityPoint, x float64) float64 {
	if len(ramp) == 0 {
		return 1
	}
	if x <= ramp[0].Position {
		return clampUnit(ramp[0].Opacity)
	}
	for i := 1; i < len(ramp); i++ {
		a, b := ramp[i-1], ramp[i]
		if x <= b.Position {
			if b.Position <= a.Position {
				return clampUnit(b.Opacity)
			}
			t := (x - a.Position) / (b.Position - a.Position)
			return clampUnit(a.Opacity + (b.Opacity-a.Opacity)*t)
		}
	}
	return clampUnit(ramp[len(ramp)-1].Opacity)
}

func validateFusionPresentation(p FusionLayerPresentation) error {
	if !finite(p.Alpha) {
		return invalidMPRField("LayerAlpha")
	}
	if p.Bands < 0 || p.Bands > 256 {
		return invalidMPRField("LayerBands")
	}
	for i, point := range p.OpacityRamp {
		if !finite(point.Position) || !finite(point.Opacity) || (i > 0 && point.Position < p.OpacityRamp[i-1].Position) {
			return invalidMPRField("LayerOpacityRamp")
		}
	}
	return nil
}

// FusedMPROverlay is one secondary layer of an N-layer fusion.
type FusedMPROverlay struct {
	Volume         *Volume
	TargetToSource PatientPointMapper
}

// FusedMPRLayersRequest samples a base volume and any number of overlays on
// one target plane and slab.
type FusedMPRLayersRequest struct {
	Base        *Volume
	Overlays    []FusedMPROverlay
	TargetPlane Plane
	Width       int
	Height      int
	Slab        FusedMPRSlab
	Limits      MPRLimits
}

// FusedScalarLayers holds the modality values of every layer on the same
// target grid: Planes[0] is the base, then the overlays in order.
type FusedScalarLayers struct {
	Planes  []*ScalarPlane
	Reports []FusedMPRSamplingReport
}

// Bytes reports the combined retained scalar payload.
func (l *FusedScalarLayers) Bytes() int64 {
	if l == nil {
		return 0
	}
	total := int64(0)
	for _, plane := range l.Planes {
		total += plane.Bytes()
	}
	return total
}

// ResliceFusedMPRLayersContext samples the base and every overlay as
// ResliceFusedMPRScalarContext does, sharing the base plane. Overlays that
// miss the target are kept (all samples invalid) and reported with
// ErrFusionNoOverlap once every overlay misses; other errors are atomic.
func ResliceFusedMPRLayersContext(ctx context.Context, request FusedMPRLayersRequest) (*FusedScalarLayers, error) {
	if len(request.Overlays) == 0 {
		return nil, invalidMPRField("OverlayVolume")
	}
	result := &FusedScalarLayers{}
	overlap := false
	for _, overlay := range request.Overlays {
		planes, err := ResliceFusedMPRScalarContext(ctx, FusedMPRRequest{
			Base: request.Base, Overlay: overlay.Volume, TargetPlane: request.TargetPlane,
			Width: request.Width, Height: request.Height, TargetToSource: overlay.TargetToSource,
			Slab: request.Slab, Limits: request.Limits,
		})
		if err != nil && !errors.Is(err, ErrFusionNoOverlap) {
			return nil, err
		}
		if len(result.Planes) == 0 {
			result.Planes = append(result.Planes, planes.Base)
		}
		result.Planes = append(result.Planes, planes.Overlay)
		result.Reports = append(result.Reports, planes.Report)
		overlap = overlap || err == nil
	}
	if !overlap {
		return result, ErrFusionNoOverlap
	}
	return result, nil
}

// Composite presents every layer and combines them by mode. Blend draws the
// layers source-over in order (layers[i] presents Planes[i]); checkerboard,
// swipe and difference compare the first two layers and draw them opaque,
// falling back to whichever layer is sampled.
func (l *FusedScalarLayers) Composite(ctx context.Context, layers []FusionLayerPresentation, mode FusionCompositeMode) (image.Image, error) {
	if l == nil || len(l.Planes) == 0 || len(layers) != len(l.Planes) {
		return nil, invalidMPRField("FusedScalarLayers")
	}
	width, height := l.Planes[0].Dimensions()
	for _, plane := range l.Planes {
		if plane == nil || len(plane.values) < width*height || len(plane.valid) < width*height {
			return nil, invalidMPRField("FusedScalarLayers")
		}
		if w, h := plane.Dimensions(); w != width || h != height {
			return nil, invalidMPRField("FusedScalarPlaneDimensions")
		}
	}
	for _, layer := range layers {
		if err := validateFusionPresentation(layer); err != nil {
			return nil, err
		}
	}
	if mode.Kind != FusionBlend && len(l.Planes) < 2 {
		return nil, invalidMPRField("FusionComparisonLayers")
	}
	if ctx == nil {
		ctx = context.Background()
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	renderers := make([]fusionLayerRenderer, len(layers))
	for i := range layers {
		renderers[i] = newFusionLayerRenderer(l.Planes[i], layers[i])
	}
	checker := mode.CheckerSizePx
	if checker <= 0 {
		checker = DefaultFusionCheckerSizePx
	}
	swipe := mode.SwipePosition
	if !finite(swipe) || swipe <= 0 || swipe >= 1 {
		swipe = 0.5
	}
	swipeX := int(math.Round(swipe * float64(width)))
	differenceRange := mode.DifferenceRange
	if !finite(differenceRange) || differenceRange <= 0 {
		differenceRange = renderers[1%len(renderers)].window.Width
	}
	differencePalette := mode.DifferencePalette
	if differencePalette == nil {
		differencePalette = display.CoolWarmLUT()
	}
	out := image.NewRGBA(image.Rect(0, 0, width, height))
	opaque := func(layer, sample int) (uint8, uint8, uint8, bool) {
		plane := l.Planes[layer]
		if plane.valid[sample] == 0 {
			return 0, 0, 0, false
		}
		r, g, b, _ := renderers[layer].sample(plane.values[sample])
		return r, g, b, true
	}
	err := parallelRowsContext(ctx, height, func(y int) {
		for x := 0; x < width; x++ {
			if ctx.Err() != nil {
				return
			}
			sample := y*width + x
			index := sample * 4
			var red, green, blue, alpha float64
			switch mode.Kind {
			case FusionBlend:
				for i, plane := range l.Planes {
					if plane.valid[sample] == 0 {
						continue
					}
					r, g, b, a := renderers[i].sample(plane.values[sample])
					if a > 0 {
						red, green, blue, alpha = compositeFusionLayer(red, green, blue, alpha, r, g, b, a)
					}
				}
			default:
				first, second := 0, 1
				switch mode.Kind {
				case FusionCheckerboard:
					if (x/checker+y/checker)%2 == 1 {
						first, second = 1, 0
					}
				case FusionSwipe:
					if x >= swipeX {
						first, second = 1, 0
					}
				case FusionDifference:
					if l.Planes[0].valid[sample] != 0 && l.Planes[1].valid[sample] != 0 {
						d := l.Planes[1].values[sample] - l.Planes[0].values[sample]
						index8 := int(math.Round(clampUnit(0.5+d/(2*differenceRange)) * 255))
						r, g, b := differencePalette.At(index8)
						red, green, blue, alpha = float64(r)/255, float64(g)/255, float64(b)/255, 1
					}
				}
				if alpha == 0 {
					r, g, b, ok := opaque(first, sample)
					if !ok {
						r, g, b, ok = opaque(second, sample)
					}
					if ok {
						red, green, blue, alpha = float64(r)/255, float64(g)/255, float64(b)/255, 1
					}
				}
			}
			if alpha > 0 {
				out.Pix[index+0] = byte(math.Round(clampUnit(red/alpha) * 255))
				out.Pix[index+1] = byte(math.Round(clampUnit(green/alpha) * 255))
				out.Pix[index+2] = byte(math.Round(clampUnit(blue/alpha) * 255))
				out.Pix[index+3] = byte(math.Round(clampUnit(alpha) * 255))
			}
		}
	})
	if err != nil {
		return nil, err
	}
	return out, nil
}
