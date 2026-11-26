package render

import (
	"errors"

	"github.com/ThalesMMS/dicom-go/pixeldata/display"
)

// MPR presentation on the backend (#1233), view state V12. An MPR request
// normally returns modality values (FrameFormatGrayF32TopLeft) that the caller
// windows. With MPRViewState.Presentation the backend presents the frame
// itself and returns FrameFormatRGBA8NRGBATopLeft:
//
//   - MPRPresentationGray windows the plane to display gray and maps that
//     gray through the base layer's opacity ramp to alpha, a slab opacity
//     table: pixel (g, g, g, round(255·ramp(g/255))). Pixels without a sample
//     present MPRViewState.OutsideValue, as the GRAY_F32 frame holds it.
//   - MPRPresentationFusion also samples a second, overlay volume at the same
//     patient points and slab, mapped through Fusion.TargetToOverlayIndex, as
//     ResliceFusedMPRScalarContext does, and composites the base and overlay
//     layers as FusedScalarLayers.Composite does with the layers'
//     FusionLayerPresentation and Fusion's composite mode.
//
// A layer is FusionLayerPresentation as request values
// (NewMPRLayerPresentation): its window already normalized, its palette as
// 256 packed RGB entries. VOI LUT windows are not presentable.

// ViewStateContractVersionV12 adds MPRViewState.Presentation (#1233).
const ViewStateContractVersionV12 uint32 = 12

// ViewStateHeaderSizeV12 adds the presentation mode and padding (8); the
// layers, ramps and palettes travel in the request's Presentation field.
const ViewStateHeaderSizeV12 uint32 = 304

// MPRPresentationMode selects the presented output of an MPR request.
type MPRPresentationMode uint32

const (
	// MPRPresentationNone returns modality values (GRAY_F32).
	MPRPresentationNone MPRPresentationMode = iota
	// MPRPresentationGray returns the windowed gray with an opacity ramp.
	MPRPresentationGray
	// MPRPresentationFusion returns the fused composite of base and overlay.
	MPRPresentationFusion
)

const (
	// MaxMPRPresentationRampPoints bounds a layer's opacity ramp.
	MaxMPRPresentationRampPoints = 64
	// MPRPresentationPaletteEntries is the length of a presentation palette.
	MPRPresentationPaletteEntries = 256
)

// ErrMPRPresentationUnsupported reports a presentation that a request cannot
// carry, such as a VOI LUT window; callers present on the CPU instead.
var ErrMPRPresentationUnsupported = errors.New("render: MPR presentation unsupported")

// MPRLayerPresentation is one presented layer, FusionLayerPresentation as
// request values.
type MPRLayerPresentation struct {
	WindowCenter float64
	WindowWidth  float64
	// WindowFunction is the display.VOIFunction of the window.
	WindowFunction uint32
	// Invert presents gray g as 255-g (MONOCHROME1).
	Invert           uint32
	Alpha            float64
	TransparentBelow uint32
	Bands            uint32
	OpacityRamp      []FusionOpacityPoint `json:",omitempty"`
	// Palette holds MPRPresentationPaletteEntries colours packed
	// R | G<<8 | B<<16, or nothing for gray.
	Palette []uint32 `json:",omitempty"`
}

// MPRFusionState is the overlay volume and composite of a fused request.
type MPRFusionState struct {
	// OverlayGeneration is the VolumeGeneration of the overlay volume, which
	// the backend holds beside the base volume.
	OverlayGeneration uint64
	OverlayDimensions [3]uint32
	// TargetToOverlayIndex maps a base patient LPS point to the overlay's
	// continuous voxel index: the overlay's PatientLPSToIndex composed with
	// the base-to-overlay registration (nothing more in one frame of
	// reference). The last row is (0, 0, 0, 1).
	TargetToOverlayIndex GeometryAffine
	// Composite and its parameters are FusionCompositeMode's with the
	// defaults resolved (NewMPRFusionComposite).
	Composite         FusionCompositeKind
	CheckerSizePx     uint32
	SwipePosition     float64
	DifferenceRange   float64
	DifferencePalette []uint32 `json:",omitempty"`
}

// MPRPresentationState is the presentation of an MPR request (V12).
type MPRPresentationState struct {
	Mode MPRPresentationMode
	// Base presents the plane of the request's volume.
	Base MPRLayerPresentation
	// Overlay and Fusion describe the overlay of MPRPresentationFusion.
	Overlay MPRLayerPresentation `json:",omitzero"`
	Fusion  MPRFusionState       `json:",omitzero"`
}

// NewMPRLayerPresentation converts a fusion layer presentation of a volume
// with the given photometric interpretation into request values, with the
// window normalized as FusedScalarLayers.Composite normalizes it. A VOI LUT
// window returns ErrMPRPresentationUnsupported.
func NewMPRLayerPresentation(layer FusionLayerPresentation, photometric string) (MPRLayerPresentation, error) {
	if layer.Window.LUT != nil {
		return MPRLayerPresentation{}, ErrMPRPresentationUnsupported
	}
	if err := validateFusionPresentation(layer); err != nil {
		return MPRLayerPresentation{}, err
	}
	window := normalizeWindow(layer.Window, defaultScalarVOI())
	result := MPRLayerPresentation{
		WindowCenter:   window.Center,
		WindowWidth:    window.Width,
		WindowFunction: uint32(window.Function),
		Alpha:          clampUnit(layer.Alpha),
		Bands:          uint32(layer.Bands),
		OpacityRamp:    append([]FusionOpacityPoint(nil), layer.OpacityRamp...),
		Palette:        PackMPRPresentationPalette(layer.Palette),
	}
	if normalizedPhotometric(photometric) == "MONOCHROME1" {
		result.Invert = 1
	}
	if layer.TransparentBelow {
		result.TransparentBelow = 1
	}
	if len(result.OpacityRamp) > MaxMPRPresentationRampPoints {
		return MPRLayerPresentation{}, ErrMPRPresentationUnsupported
	}
	return result, nil
}

// PackMPRPresentationPalette packs the 256 colours of palette, or returns
// nil for a nil palette (gray).
func PackMPRPresentationPalette(palette *display.PaletteColorLUT) []uint32 {
	if palette == nil {
		return nil
	}
	packed := make([]uint32, MPRPresentationPaletteEntries)
	for i := range packed {
		r, g, b := palette.At(i)
		packed[i] = uint32(r) | uint32(g)<<8 | uint32(b)<<16
	}
	return packed
}

// NewMPRFusionComposite returns the composite part of a fused request with
// FusedScalarLayers.Composite's defaults resolved: the checkerboard tile, the
// centred swipe line, the overlay window width as the difference range and
// display.CoolWarmLUT as its palette.
func NewMPRFusionComposite(mode FusionCompositeMode, overlay MPRLayerPresentation) MPRFusionState {
	state := MPRFusionState{Composite: mode.Kind}
	checker := mode.CheckerSizePx
	if checker <= 0 {
		checker = DefaultFusionCheckerSizePx
	}
	state.CheckerSizePx = uint32(checker)
	state.SwipePosition = mode.SwipePosition
	if !finite(state.SwipePosition) || state.SwipePosition <= 0 || state.SwipePosition >= 1 {
		state.SwipePosition = 0.5
	}
	state.DifferenceRange = mode.DifferenceRange
	if !finite(state.DifferenceRange) || state.DifferenceRange <= 0 {
		state.DifferenceRange = overlay.WindowWidth
	}
	palette := mode.DifferencePalette
	if palette == nil {
		palette = display.CoolWarmLUT()
	}
	state.DifferencePalette = PackMPRPresentationPalette(palette)
	return state
}

func validateMPRPresentation(state MPRPresentationState, contractVersion uint32, fail func(string, ...any) error) error {
	if state.Mode == MPRPresentationNone {
		if !mprPresentationLayerZero(state.Base) || !mprPresentationLayerZero(state.Overlay) || !mprFusionZero(state.Fusion) {
			return fail("MPR presentation without a mode")
		}
		return nil
	}
	if contractVersion < ViewStateContractVersionV12 {
		return fail("MPR presentation requires V12")
	}
	if err := validateMPRPresentationLayer(state.Base, fail); err != nil {
		return err
	}
	switch state.Mode {
	case MPRPresentationGray:
		if !mprPresentationLayerZero(state.Overlay) || !mprFusionZero(state.Fusion) {
			return fail("gray MPR presentation carries an overlay")
		}
		return nil
	case MPRPresentationFusion:
	default:
		return fail("unsupported MPR presentation mode %d", state.Mode)
	}
	if err := validateMPRPresentationLayer(state.Overlay, fail); err != nil {
		return err
	}
	fusion := state.Fusion
	if fusion.OverlayGeneration == 0 ||
		fusion.OverlayDimensions[0] == 0 || fusion.OverlayDimensions[1] == 0 || fusion.OverlayDimensions[2] == 0 {
		return fail("fused MPR presentation without an overlay volume")
	}
	affine := fusion.TargetToOverlayIndex
	for _, value := range affine {
		if !finite(value) {
			return fail("non-finite fused MPR overlay mapping")
		}
	}
	if affine[12] != 0 || affine[13] != 0 || affine[14] != 0 || affine[15] != 1 {
		return fail("fused MPR overlay mapping is not affine")
	}
	switch fusion.Composite {
	case FusionBlend, FusionCheckerboard, FusionSwipe, FusionDifference:
	default:
		return fail("unsupported fused MPR composite %d", fusion.Composite)
	}
	if fusion.CheckerSizePx == 0 || !finite(fusion.SwipePosition) || fusion.SwipePosition <= 0 || fusion.SwipePosition >= 1 ||
		!finite(fusion.DifferenceRange) || fusion.DifferenceRange <= 0 {
		return fail("invalid fused MPR composite parameters")
	}
	if fusion.Composite == FusionDifference && len(fusion.DifferencePalette) != MPRPresentationPaletteEntries {
		return fail("fused MPR difference needs a %d-entry palette", MPRPresentationPaletteEntries)
	}
	if len(fusion.DifferencePalette) != 0 && len(fusion.DifferencePalette) != MPRPresentationPaletteEntries {
		return fail("fused MPR difference palette of %d entries", len(fusion.DifferencePalette))
	}
	return nil
}

func validateMPRPresentationLayer(layer MPRLayerPresentation, fail func(string, ...any) error) error {
	if !finite(layer.WindowCenter) || !finite(layer.WindowWidth) || layer.WindowWidth <= 0 {
		return fail("invalid MPR presentation window")
	}
	if layer.WindowFunction > uint32(display.VOISigmoid) || layer.Invert > 1 || layer.TransparentBelow > 1 {
		return fail("invalid MPR presentation flags")
	}
	if !finite(layer.Alpha) || layer.Alpha < 0 || layer.Alpha > 1 || layer.Bands > 256 {
		return fail("invalid MPR presentation alpha or bands")
	}
	if len(layer.OpacityRamp) > MaxMPRPresentationRampPoints {
		return fail("%d MPR presentation ramp points over %d", len(layer.OpacityRamp), MaxMPRPresentationRampPoints)
	}
	for i, point := range layer.OpacityRamp {
		if !finite(point.Position) || !finite(point.Opacity) || (i > 0 && point.Position < layer.OpacityRamp[i-1].Position) {
			return fail("invalid MPR presentation opacity ramp")
		}
	}
	if len(layer.Palette) != 0 && len(layer.Palette) != MPRPresentationPaletteEntries {
		return fail("MPR presentation palette of %d entries", len(layer.Palette))
	}
	for _, packed := range layer.Palette {
		if packed>>24 != 0 {
			return fail("invalid MPR presentation palette entry")
		}
	}
	return nil
}

func mprPresentationLayerZero(layer MPRLayerPresentation) bool {
	return layer.WindowCenter == 0 && layer.WindowWidth == 0 && layer.WindowFunction == 0 && layer.Invert == 0 &&
		layer.Alpha == 0 && layer.TransparentBelow == 0 && layer.Bands == 0 &&
		len(layer.OpacityRamp) == 0 && len(layer.Palette) == 0
}

func mprFusionZero(fusion MPRFusionState) bool {
	return fusion.OverlayGeneration == 0 && fusion.OverlayDimensions == [3]uint32{} &&
		fusion.TargetToOverlayIndex == GeometryAffine{} && fusion.Composite == FusionBlend &&
		fusion.CheckerSizePx == 0 && fusion.SwipePosition == 0 && fusion.DifferenceRange == 0 &&
		len(fusion.DifferencePalette) == 0
}

// freezeMPRPresentation copies the presentation's slices.
func freezeMPRPresentation(state MPRPresentationState) MPRPresentationState {
	copyLayer := func(layer MPRLayerPresentation) MPRLayerPresentation {
		layer.OpacityRamp = append([]FusionOpacityPoint(nil), layer.OpacityRamp...)
		layer.Palette = append([]uint32(nil), layer.Palette...)
		return layer
	}
	state.Base = copyLayer(state.Base)
	state.Overlay = copyLayer(state.Overlay)
	state.Fusion.DifferencePalette = append([]uint32(nil), state.Fusion.DifferencePalette...)
	return state
}

// mprPresentationRGBA reports whether an MPR request returns RGBA8.
func mprPresentationRGBA(state MPRViewState) bool {
	return state.Presentation.Mode != MPRPresentationNone
}
