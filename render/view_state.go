package render

import (
	"errors"
	"fmt"
	"math"
)

const (
	ViewStateContractVersionV1 uint32 = 1
	ViewStateContractVersionV2 uint32 = 2
	ViewStateContractVersionV3 uint32 = 3
	ViewStateContractVersionV4 uint32 = 4
	ViewStateContractVersionV5 uint32 = 5
	ViewStateContractVersionV6 uint32 = 6
	ViewStateContractVersionV7 uint32 = 7
	ViewStateContractVersionV8 uint32 = 8
	ViewStateContractVersionV9 uint32 = 9
	// V10 carries the gradient opacity function (#1010).
	ViewStateContractVersionV10 uint32 = 10
	// V11 carries the iso-surfaces of VRModeIsoSurface (#1228).
	ViewStateContractVersionV11 uint32 = 11
	// V13 selects mapped-opacity normals for DVR illumination (#1368).
	ViewStateContractVersionV13 uint32 = 13
	// V14 carries bounded directional lights, tone mapping and specular lookup.
	ViewStateContractVersionV14 uint32 = 14
	// V15 requests the bounded 2x averaged MPR level.
	ViewStateContractVersionV15 uint32 = 15
	// V16 carries one-surface binocular presentation.
	ViewStateContractVersionV16 uint32 = 16
	ViewStateContractVersionV17 uint32 = 17
	// V18 supplies request-owned patient-space CPR column bases.
	ViewStateContractVersionV18 uint32 = 18
	// V19 requests quantitative modality scalars with explicit sample validity.
	ViewStateContractVersionV19 uint32 = 19
	// V20 admits the clipped-neighborhood median volume filter.
	ViewStateContractVersionV20 uint32 = 20
	// V21 carries a projection value range and Ray Sum reference distance.
	ViewStateContractVersionV21 uint32 = 21
	ViewStateContractVersionV22 uint32 = 22
	ViewStateContractVersionV23 uint32 = 23
	// V24 admits the physical multiscale vessel display filter.
	ViewStateContractVersionV24 uint32 = 24
	// V25 carries the normalized crop's 27-region selection mask.
	ViewStateContractVersionV25 uint32 = 25
	// V26 carries direct RGB or independent component presentation.
	ViewStateContractVersionV26 uint32 = 26
	// V27 binds a geometry material to a separately resident scalar source.
	ViewStateContractVersionV27 uint32 = 27
	// V28 admits CPR columns with a volume filter or one affine overlay.
	ViewStateContractVersionV28 uint32 = 28
	// V29 admits explicit modality-preserving filters with scalar validity.
	ViewStateContractVersionV29 uint32 = 29
	// ViewStateContractVersion remains the V1 default for source compatibility.
	// Callers using newer fields opt in explicitly with the matching version and
	// header-size constants.
	ViewStateContractVersion uint32 = ViewStateContractVersionV1
	ViewStateHeaderSizeV1    uint32 = 48
	ViewStateHeaderSizeV2    uint32 = 96
	ViewStateHeaderSizeV3    uint32 = 112
	ViewStateHeaderSizeV4    uint32 = 144
	ViewStateHeaderSizeV5    uint32 = 152
	// V6 adds the VR label header: generation (8), dimensions (12), bits
	// (4), only-inside flag (4) and padding (4).
	ViewStateHeaderSizeV6 uint32 = 184
	// V7 adds volumetric scattering: blend, reach and anisotropy (24) and the
	// light direction (24).
	ViewStateHeaderSizeV7 uint32 = 232
	// V8 adds cut planes: the plane mask and padding (8), the clipped voxel
	// intensity and cap opacity (16) and the cap window (16).
	ViewStateHeaderSizeV8 uint32 = 272
	// V9 adds the geometry flag and padding (8); the depth and colour
	// images travel beside the request.
	ViewStateHeaderSizeV9 uint32 = 280
	// V10 adds the gradient opacity point count and padding (8); the points
	// travel in the request's GradientOpacity field.
	ViewStateHeaderSizeV10 uint32 = 288
	// V11 adds the iso-surface count and padding (8); the surfaces travel in
	// the request's IsoSurfaces field.
	ViewStateHeaderSizeV11 uint32 = 296
	// V13 adds the opacity-normal flag and padding (8) to the V12 header.
	ViewStateHeaderSizeV13 uint32 = 312
	// V14 adds count/tone/lookup/padding (16) and three fixed lights (3*32).
	ViewStateHeaderSizeV14 uint32 = 424
	ViewStateHeaderSizeV15 uint32 = 432
	// Mode/padding plus two physical distances adds 24 bytes.
	ViewStateHeaderSizeV16 uint32 = 456
	ViewStateHeaderSizeV17 uint32 = 472
	ViewStateHeaderSizeV18 uint32 = 480
	ViewStateHeaderSizeV19 uint32 = 488
	// V20 admits the GPU median filter; no additional header fields.
	ViewStateHeaderSizeV20 uint32 = ViewStateHeaderSizeV19
	ViewStateHeaderSizeV21 uint32 = ViewStateHeaderSizeV20 + 32
	// V22 adds table dimensions, three scalar domains and the entry count.
	ViewStateHeaderSizeV22 uint32 = ViewStateHeaderSizeV21 + 40
	// V23 adds the secondary source, registered affine and transfer metadata.
	ViewStateHeaderSizeV23 uint32 = ViewStateHeaderSizeV22 + 192
	// V24 adds a filter mode without adding header fields.
	ViewStateHeaderSizeV24 uint32 = ViewStateHeaderSizeV23
	ViewStateHeaderSizeV25 uint32 = ViewStateHeaderSizeV24 + 8
	ViewStateHeaderSizeV26 uint32 = ViewStateHeaderSizeV25 + 160
	ViewStateHeaderSizeV27 uint32 = ViewStateHeaderSizeV26 + 512
	ViewStateHeaderSizeV28 uint32 = ViewStateHeaderSizeV27
	ViewStateHeaderSizeV29 uint32 = ViewStateHeaderSizeV28
	// MaxVRIsoSurfaces bounds the iso-surfaces of one request.
	MaxVRIsoSurfaces = 8
	// MaxVRLabelStyles bounds the per-label style table of a view request.
	MaxVRLabelStyles = 4096
	// MaxViewLabelBytes bounds the label volume uploaded to a GPU backend.
	MaxViewLabelBytes      uint64 = 512 << 20
	MaxViewOutputDimension uint32 = 8192
	MaxViewOutputBytes     uint64 = 256 << 20
	MaxViewCutMaskBytes    uint64 = 64 << 20
	MaxTransferLUTSamples         = 4096
	MaxClippingPlanes             = 8
)

// Contract aliases keep the frozen MIN/MAX vocabulary explicit while reusing
// the established dicom-go slab enum and its persisted numeric values.
const (
	SlabMax = SlabMIP
	SlabMin = SlabMinIP
)

var ErrInvalidViewState = errors.New("render: invalid view state")

type ViewKind uint32

const (
	ViewKindUnknown ViewKind = iota
	ViewKindMPR
	ViewKindVR
)

type Interpolation uint32

const (
	InterpolationUnknown Interpolation = iota
	InterpolationNearest
	InterpolationLinear
	InterpolationCubic
)

type Projection uint32

const (
	ProjectionUnknown Projection = iota
	ProjectionPerspective
	ProjectionParallel
	ProjectionEndoscopy
)

type MPRViewState struct {
	// ScalarValidity requests modality float32 values paired with explicit
	// GPU sample-validity words. V19 only; no display presentation or filtering.
	ScalarValidity bool `json:",omitempty"`
	// CPRColumns replaces the planar basis with one basis per output column.
	// V18 only. Empty retains the established planar MPR contract.
	CPRColumns []CPRColumn `json:",omitempty"`
	// Antialias2x selects a cached averaged level only at coarse display density.
	// Quantitative source samples are unchanged. V15 only, single-plane grayscale.
	Antialias2x         bool `json:",omitempty"`
	PixelOriginLPS      [3]float64
	PixelStepULPS       [3]float64
	PixelStepVLPS       [3]float64
	Interpolation       Interpolation
	SlabMode            SlabMode
	SlabAxisLPS         [3]float64
	SlabThicknessMM     float64
	SlabSampleSpacingMM float64
	// OutsideValue is the output of a pixel with no sample within half a
	// voxel of the volume; samples inside that border repeat the edge voxels
	// and never blend with it (#1005).
	OutsideValue float32
	// Presentation (V12) presents the frame on the backend, alone or fused
	// with an overlay volume (#1233); omitted from JSON when zero.
	Presentation MPRPresentationState `json:",omitzero"`
}

type ClippingPlaneLPS [4]float64

// VRModeIsoSurface (V11) renders the first crossings of IsoSurfaces along
// each ray, render.RenderVRSurface's iso-surface preview (#1228). It is a
// view-state mode only: CPU presets render surfaces through RenderVRSurface.
const VRModeIsoSurface VRMode = 4

// VRIsoSurfaceState is one iso-surface of a VRModeIsoSurface request (V11).
// A ray hits it where a kept sample first reaches Value (value >= Value after
// a sample below it, refined by bisection) and shades it as
// render.RenderVRSurface does: Color is display RGB in 0..1 (the surface
// colour, not linear light), multiplied by the headlight's lit factor plus
// its specular term. Opacity in (0, 1] composites nearer surfaces over
// farther ones; a pixel without a hit is transparent.
type VRIsoSurfaceState struct {
	Value   float64
	Color   [3]float64
	Opacity float64
}

type TransferSample struct {
	// RGB is linear-light, matching VRTransferFunction.BakeLUT. A backend that
	// returns display NRGBA performs the final linear-to-sRGB conversion.
	R float32
	G float32
	B float32
	A float32
}

type TransferLUT struct {
	DomainMin float64
	DomainMax float64
	Samples   []TransferSample
}

// NormalizedCropBox is expressed in texture coordinates [0,1]. Enabled is an
// integer flag so the value-only contract has stable cross-language semantics.
type NormalizedCropBox struct {
	Enabled     uint32
	RegionFlags uint32 `json:",omitempty"`
	Min         [3]float64
	Max         [3]float64
}

// VoxelMask stores one cut bit per X-major voxel. Words are packed in little-
// endian bit order and travel as a binary worker sidecar, never as JSON.
type VoxelMask struct {
	Dimensions [3]uint32
	Words      []uint32 `json:"-"`
}

type VRViewState struct {
	Fusion             VRFusionState           `json:",omitzero"`
	Transfer2D         VRTransfer2DState       `json:",omitzero"`
	Stereo             StereoPresentationState `json:",omitzero"`
	PositionLPS        [3]float64
	FocalPointLPS      [3]float64
	ViewUpLPS          [3]float64
	Projection         Projection
	VerticalFOVRadians float64
	ParallelScaleMM    float64
	NearMM             float64
	FarMM              float64
	Mode               VRMode
	// ProjectionRange (V21) restricts Average and RaySum inclusively in modality
	// units. A ranged Average averages the mapped sample colors, matching the
	// clinical headless projection instead of mapping the mean modality value.
	ProjectionRange VRValueRange `json:",omitzero"`
	// RaySumReferenceMM (V21) divides the physical integral before presentation.
	// It is the captured volume's physical diagonal, never a sample count.
	RaySumReferenceMM float64 `json:",omitempty"`
	SampleDistanceMM  float64
	// OpacityUnitDistanceMM is the physical distance at which transfer alpha is
	// authored. V1/V2 zero values use the explicit 1 mm compatibility fallback;
	// V3 requests carry a finite positive value.
	OpacityUnitDistanceMM float64
	ImageSampleDistance   float64
	ShadingEnabled        uint32
	// OpacityNormals (V13) shades DVR with the gradient of transfer alpha.
	// Zero preserves the scalar-normal contract and old JSON requests.
	OpacityNormals bool                `json:",omitempty"`
	Illumination   VRIlluminationState `json:",omitzero"`
	Ambient        float64
	Diffuse        float64
	Specular       float64
	SpecularPower  float64
	// GradientOpacityScale modulates DVR sample opacity by the central-
	// difference HU gradient magnitude. Zero disables the modulation.
	GradientOpacityScale float64
	// BackgroundRGBA is linear and non-premultiplied. V4 viewport requests
	// require an opaque value; V1-V3 leave it zero and use black compatibility
	// semantics in the backend.
	BackgroundRGBA [4]float64
	// JitterEnabled (V5) offsets each output pixel's first sample by
	// VRRayJitter(x, y) of the sample distance, as the CPU renderer does for
	// VRQuality.Jitter; sampling rings become fine noise.
	JitterEnabled     uint32
	ClippingPlanesLPS []ClippingPlaneLPS
	Crop              NormalizedCropBox
	CutMask           VoxelMask
	TransferLUT       TransferLUT
	// Labels (V6) colours segments inside the volume (#1062). A zero value
	// is omitted from JSON, so label-free requests serialize as in V5.
	Labels VRLabelState `json:",omitzero"`
	// Scattering (V7) adds volumetric shadows (#1064); omitted from JSON when
	// zero.
	Scattering VRScatteringState `json:",omitzero"`
	// Cut (V8) marks clipping planes that cut the volume open (#1067);
	// omitted from JSON when zero.
	Cut VRCutPlaneState `json:",omitzero"`
	// Geometry (V9) is rasterized opaque geometry (#1068); omitted from
	// JSON when disabled.
	Geometry VRGeometryState `json:",omitzero"`
	// GradientOpacity (V10) is the piecewise-linear gradient opacity
	// function of the covariant gradient magnitude in modality units per mm
	// (#1010). It replaces GradientOpacityScale, which must then be zero;
	// omitted from JSON when empty.
	GradientOpacity VRGradientOpacity `json:",omitzero"`
	// IsoSurfaces (V11) are the surfaces of VRModeIsoSurface, at most
	// MaxVRIsoSurfaces; omitted from JSON when empty.
	IsoSurfaces  []VRIsoSurfaceState `json:",omitzero"`
	SurfaceProbe SurfaceProbeState   `json:",omitzero"`
}

// VRGeometryState is the depth-composited geometry of a VR request (V9),
// render.RasterizeVRGeometryContext's image at the output size: per pixel,
// row-major from the top left, the ray parameter of the nearest opaque
// surface in millimetres from the ray origin (+Inf where none) and its
// shaded colour as sRGB8 RGBA (A unused). Rays stop at the depth and the
// colour replaces the background. Depth and Color travel beside the
// request, not in JSON.
type VRGeometryState struct {
	Enabled uint32
	Depth   []float32 `json:"-"`
	Color   []uint32  `json:"-"`
}

// VRGeometryBytes is the size of the geometry sidecar of a w×h request.
func VRGeometryBytes(w, h uint32) uint64 { return uint64(w) * uint64(h) * 8 }

// VRGeometryStateFromDepth converts a depth image to a request's geometry.
func VRGeometryStateFromDepth(depth *VRDepthImage) VRGeometryState {
	if depth == nil {
		return VRGeometryState{}
	}
	state := VRGeometryState{Enabled: 1, Depth: append([]float32(nil), depth.T...), Color: make([]uint32, len(depth.T))}
	for i := range depth.T {
		r, g, b := to8(linearToSRGB(float64(depth.R[i]))), to8(linearToSRGB(float64(depth.G[i]))), to8(linearToSRGB(float64(depth.B[i])))
		state.Color[i] = uint32(r) | uint32(g)<<8 | uint32(b)<<16 | 0xff<<24
	}
	return state
}

// VRCutPlaneState is the cut-plane part of a VR request (V8), render.VRClip's
// AddCutPlane and SetCapOpacity: bit i of Planes marks ClippingPlanesLPS[i]
// as a cut plane, whose clipped side reads ClippedVoxelIntensity in shading
// gradients; a DVR ray entering through one composites a cap of CapOpacity,
// the modality value at its first kept sample mapped to gray between the
// CapWindow bounds (sRGB-encoded, as the MPR displays it).
type VRCutPlaneState struct {
	Planes                uint32
	ClippedVoxelIntensity float64
	CapOpacity            float64
	CapWindow             [2]float64
}

// VRScatteringState is the volumetric-scattering part of a VR request (V7):
// render.VRScattering with the reach in millimetres and the direction toward
// the key light in patient LPS. Blend zero disables it.
type VRScatteringState struct {
	Blend             float64
	ReachMM           float64
	Anisotropy        float64
	LightDirectionLPS [3]float64
}

// VRLabelState is the label-map part of a VR request (V6). The voxels travel
// separately from the request, once per Generation: backends upload Volume
// and render only when their resident generation matches.
type VRLabelState struct {
	// Generation identifies an immutable label volume; zero disables labels.
	Generation uint64
	Dimensions [3]uint32
	// Bits is 8 or 16, the storage of the label values.
	Bits       uint32
	OnlyInside uint32
	// Styles is indexed by label; labels without a style are hidden.
	Styles []VRLabelStyleState
	// Volume holds the voxels in process; it is not serialized.
	Volume *VRLabelMap `json:"-"`
}

// VRLabelStyleState is one label style: linear RGB, opacity and blend in
// 0..1 and the visibility flag.
type VRLabelStyleState struct {
	RGB     [3]float64
	Opacity float64
	Blend   float64
	Visible uint32
}

// VRLabelStateFromMap describes m under generation for a V6 request.
func VRLabelStateFromMap(m *VRLabelMap, generation uint64) VRLabelState {
	if m == nil || generation == 0 {
		return VRLabelState{}
	}
	cols, rows, depth := m.Dimensions()
	state := VRLabelState{
		Generation: generation,
		Dimensions: [3]uint32{uint32(cols), uint32(rows), uint32(depth)},
		Bits:       m.StorageBits(),
		Volume:     m,
	}
	if m.OnlyInside() {
		state.OnlyInside = 1
	}
	styles := m.Styles()
	if len(styles) > MaxVRLabelStyles {
		styles = styles[:MaxVRLabelStyles]
	}
	for _, style := range styles {
		visible := uint32(0)
		if style.Visible {
			visible = 1
		}
		state.Styles = append(state.Styles, VRLabelStyleState{
			RGB: [3]float64{style.Color.R, style.Color.G, style.Color.B}, Opacity: style.Opacity, Blend: style.Blend, Visible: visible,
		})
	}
	return state
}

func (state VRViewState) EffectiveOpacityUnitDistanceMM() float64 {
	if !finite(state.OpacityUnitDistanceMM) || state.OpacityUnitDistanceMM <= 0 {
		return DefaultVROpacityUnitDistanceMM
	}
	return state.OpacityUnitDistanceMM
}

// ViewState is the value-only, backend-neutral rendering request frozen for
// native, CPU and WebGPU engines. PresentationGeneration is carried through MPR
// even though MPR output remains modality-domain scalar data.
type ViewState struct {
	Components             ComponentPresentation `json:",omitzero"`
	VolumeFilter           VolumeFilterState     `json:",omitzero"`
	ContractVersion        uint32
	StructSize             uint32
	Kind                   ViewKind
	VolumeGeneration       uint64
	ViewGeneration         uint64
	PresentationGeneration uint64
	OutputWidth            uint32
	OutputHeight           uint32
	MPR                    MPRViewState
	VR                     VRViewState
}

func ValidateViewState(state ViewState) error {
	fail := func(format string, args ...any) error {
		return fmt.Errorf("%w: %s", ErrInvalidViewState, fmt.Sprintf(format, args...))
	}
	minimumHeader := uint32(0)
	switch state.ContractVersion {
	case ViewStateContractVersionV1:
		minimumHeader = ViewStateHeaderSizeV1
	case ViewStateContractVersionV2:
		minimumHeader = ViewStateHeaderSizeV2
	case ViewStateContractVersionV3:
		minimumHeader = ViewStateHeaderSizeV3
	case ViewStateContractVersionV4:
		minimumHeader = ViewStateHeaderSizeV4
	case ViewStateContractVersionV5:
		minimumHeader = ViewStateHeaderSizeV5
	case ViewStateContractVersionV6:
		minimumHeader = ViewStateHeaderSizeV6
	case ViewStateContractVersionV7:
		minimumHeader = ViewStateHeaderSizeV7
	case ViewStateContractVersionV8:
		minimumHeader = ViewStateHeaderSizeV8
	case ViewStateContractVersionV9:
		minimumHeader = ViewStateHeaderSizeV9
	case ViewStateContractVersionV10:
		minimumHeader = ViewStateHeaderSizeV10
	case ViewStateContractVersionV11:
		minimumHeader = ViewStateHeaderSizeV11
	case ViewStateContractVersionV12:
		minimumHeader = ViewStateHeaderSizeV12
	case ViewStateContractVersionV13:
		minimumHeader = ViewStateHeaderSizeV13
	case ViewStateContractVersionV14:
		minimumHeader = ViewStateHeaderSizeV14
	case ViewStateContractVersionV15:
		minimumHeader = ViewStateHeaderSizeV15
	case ViewStateContractVersionV16:
		minimumHeader = ViewStateHeaderSizeV16
	case ViewStateContractVersionV17:
		minimumHeader = ViewStateHeaderSizeV17
	case ViewStateContractVersionV18:
		minimumHeader = ViewStateHeaderSizeV18
	case ViewStateContractVersionV19:
		minimumHeader = ViewStateHeaderSizeV19
	case ViewStateContractVersionV20:
		minimumHeader = ViewStateHeaderSizeV20
	case ViewStateContractVersionV21:
		minimumHeader = ViewStateHeaderSizeV21
	case ViewStateContractVersionV22:
		minimumHeader = ViewStateHeaderSizeV22
	case ViewStateContractVersionV23:
		minimumHeader = ViewStateHeaderSizeV23
	case ViewStateContractVersionV24:
		minimumHeader = ViewStateHeaderSizeV24
	case ViewStateContractVersionV25:
		minimumHeader = ViewStateHeaderSizeV25
	case ViewStateContractVersionV26:
		minimumHeader = ViewStateHeaderSizeV26
	case ViewStateContractVersionV27:
		minimumHeader = ViewStateHeaderSizeV27
	case ViewStateContractVersionV28:
		minimumHeader = ViewStateHeaderSizeV28
	case ViewStateContractVersionV29:
		minimumHeader = ViewStateHeaderSizeV29
	default:
		return fail("contract version %d", state.ContractVersion)
	}
	if state.StructSize < minimumHeader {
		return fail("struct size %d below V%d minimum %d", state.StructSize, state.ContractVersion, minimumHeader)
	}
	if err := state.Components.Validate(); err != nil {
		return err
	}
	if err := state.VR.SurfaceProbe.Validate(); err != nil {
		return err
	}
	if state.VR.SurfaceProbe.Enabled() && (state.ContractVersion < ViewStateContractVersionV27 || state.Kind != ViewKindVR || state.VR.Fusion.Enabled() || state.Components.Mode != ComponentPresentationScalar) {
		return fail("surface probe requires V27 scalar VR and its own secondary source")
	}
	if state.Components.Mode != ComponentPresentationScalar {
		if state.ContractVersion < ViewStateContractVersionV26 || state.MPR.ScalarValidity || state.MPR.Antialias2x || len(state.MPR.CPRColumns) != 0 || state.MPR.Presentation.Mode != MPRPresentationNone || state.VolumeFilter != (VolumeFilterState{}) {
			return fail("component presentation requires V26 plain MPR/VR without scalar-only features")
		}
	}
	if state.VolumeGeneration == 0 || state.ViewGeneration == 0 {
		return fail("zero volume or view generation")
	}
	if state.OutputWidth == 0 || state.OutputHeight == 0 ||
		state.OutputWidth > MaxViewOutputDimension || state.OutputHeight > MaxViewOutputDimension {
		return fail("output dimensions %dx%d outside 1..%d", state.OutputWidth, state.OutputHeight, MaxViewOutputDimension)
	}
	if uint64(state.OutputWidth) > math.MaxUint64/uint64(state.OutputHeight) {
		return fail("output dimension overflow")
	}
	if pixels := uint64(state.OutputWidth) * uint64(state.OutputHeight); pixels > MaxViewOutputBytes/4 {
		return fail("output byte size exceeds %d", MaxViewOutputBytes)
	}
	if state.MPR.Antialias2x && (state.ContractVersion < ViewStateContractVersionV15 || state.Kind != ViewKindMPR || state.MPR.SlabMode != SlabNone || state.MPR.Interpolation == InterpolationNearest || state.MPR.Presentation.Mode == MPRPresentationFusion) {
		return fail("Antialias2x requires V15 single-plane non-label MPR")
	}
	if len(state.MPR.CPRColumns) != 0 {
		if state.ContractVersion < ViewStateContractVersionV18 || state.Kind != ViewKindMPR || state.MPR.Antialias2x {
			return fail("CPR columns require V18 scalar MPR without averaged levels")
		}
		if (state.MPR.Presentation.Mode == MPRPresentationFusion || state.VolumeFilter != (VolumeFilterState{})) &&
			state.ContractVersion < ViewStateContractVersionV28 {
			return fail("filtered or fused CPR requires V28")
		}
		if err := validateCPRColumns(state.MPR, state.OutputWidth, state.OutputHeight, fail); err != nil {
			return err
		}
	}
	if state.MPR.ScalarValidity {
		if state.ContractVersion < ViewStateContractVersionV19 || state.Kind != ViewKindMPR || state.MPR.Presentation.Mode != MPRPresentationNone || state.MPR.Antialias2x {
			return fail("scalar validity requires V19 scalar MPR without presentation or averaged levels")
		}
		if state.VolumeFilter != (VolumeFilterState{}) &&
			(state.ContractVersion < ViewStateContractVersionV29 || state.VolumeFilter.Mode == VolumeFilterVesselEnhance) {
			return fail("filtered scalar validity requires V29 and a modality-preserving filter")
		}
		if uint64(state.OutputWidth)*uint64(state.OutputHeight) > MaxViewOutputBytes/8 {
			return fail("scalar validity output exceeds byte budget")
		}
	}
	if state.VolumeFilter.Mode == VolumeFilterMedian3x3x3 && state.ContractVersion < ViewStateContractVersionV20 {
		return fmt.Errorf("%w: median filter requires V20", ErrInvalidViewState)
	}
	if state.VolumeFilter.Mode == VolumeFilterVesselEnhance && state.ContractVersion < ViewStateContractVersionV24 {
		return fail("vessel enhancement requires V24")
	}
	if table := state.VR.Transfer2D; state.Kind != ViewKindVR && (table.Enabled() || len(table.Samples) != 0 || table.IntensityMin != 0 || table.IntensityMax != 0 || table.GradientMax != 0) {
		return fail("2D transfer classification requires a VR view")
	}
	if err := state.VR.Fusion.Validate(); err != nil {
		return err
	}
	if state.VR.Fusion.Enabled() && (state.Kind != ViewKindVR || state.VR.Mode != VRModeDVR || state.ContractVersion < ViewStateContractVersionV23) {
		return fail("volume fusion requires DVR V23")
	}
	if state.VolumeFilter != (VolumeFilterState{}) {
		if state.ContractVersion < ViewStateContractVersionV17 {
			return fail("volume filter requires V17")
		}
		if err := state.VolumeFilter.Validate(); err != nil {
			return err
		}
		if state.MPR.Antialias2x || state.MPR.Presentation.Mode == MPRPresentationFusion || state.VR.Geometry.Enabled != 0 {
			return fail("volume filter does not combine with averaged MPR levels, fusion or preprojected geometry")
		}
	}
	if state.VR.Stereo != (StereoPresentationState{}) {
		if state.Kind != ViewKindVR || state.ContractVersion < ViewStateContractVersionV16 {
			return fail("stereo requires V16 VR")
		}
		if err := state.VR.Stereo.Validate(); err != nil {
			return err
		}
		if state.VR.Stereo.Mode != StereoMono && (state.VR.Geometry.Enabled != 0 || state.VR.ImageSampleDistance != 1) {
			return fail("stereo requires per-eye GPU rendering without preprojected geometry or legacy image LOD")
		}
		if state.VR.Stereo.Mode == StereoSideBySide && state.OutputWidth < 2 {
			return fail("side-by-side needs two output pixels")
		}
	}
	switch state.Kind {
	case ViewKindMPR:
		if err := validateMPRPresentation(state.MPR.Presentation, state.ContractVersion, fail); err != nil {
			return err
		}
		return validateMPRViewState(state.MPR, state.OutputWidth, state.OutputHeight, fail)
	case ViewKindVR:
		if err := validateMPRPresentation(state.MPR.Presentation, ViewStateContractVersionV1, fail); err != nil {
			return err
		}
		return validateVRViewState(state.VR, state.ContractVersion, state.OutputWidth, state.OutputHeight, fail)
	default:
		return fail("unsupported view kind %d", state.Kind)
	}
}

func validateMPRViewState(
	state MPRViewState,
	outputWidth, outputHeight uint32,
	fail func(string, ...any) error,
) error {
	if !finite3(state.PixelOriginLPS) || !finite3(state.PixelStepULPS) || !finite3(state.PixelStepVLPS) {
		return fail("non-finite MPR basis")
	}
	u, uOK := normalize3(state.PixelStepULPS)
	v, vOK := normalize3(state.PixelStepVLPS)
	cross, crossOK := cross3Finite(u, v)
	if !uOK || !vOK || !crossOK || norm3(cross) <= 1e-12 {
		return fail("degenerate MPR basis")
	}
	switch state.Interpolation {
	case InterpolationNearest, InterpolationLinear, InterpolationCubic:
	default:
		return fail("unsupported interpolation %d", state.Interpolation)
	}
	if !finite(float64(state.OutsideValue)) || !finite(state.SlabThicknessMM) || state.SlabThicknessMM < 0 {
		return fail("invalid MPR outside value or slab thickness")
	}
	switch state.SlabMode {
	case SlabNone:
	case SlabMIP, SlabMinIP, SlabAverage:
		slabNorm := norm3(state.SlabAxisLPS)
		if !finite3(state.SlabAxisLPS) || !finite(slabNorm) || math.Abs(slabNorm-1) > 1e-6 {
			return fail("slab axis is not finite unit length")
		}
		if !finite(state.SlabSampleSpacingMM) || state.SlabSampleSpacingMM <= 0 {
			return fail("invalid slab sample spacing")
		}
	default:
		return fail("unsupported slab mode %d", state.SlabMode)
	}
	return validateMPRDerivedGeometry(state, outputWidth, outputHeight, fail)
}

func validateMPRDerivedGeometry(
	state MPRViewState,
	outputWidth, outputHeight uint32,
	fail func(string, ...any) error,
) error {
	lastX := float64(outputWidth - 1)
	lastY := float64(outputHeight - 1)
	xValues := []float64{0, math.Max(0, lastX-1), lastX}
	yValues := []float64{0, math.Max(0, lastY-1), lastY}
	offsets := []float64{0}
	if state.SlabMode != SlabNone {
		offsets = []float64{-state.SlabThicknessMM / 2, state.SlabThicknessMM / 2}
	}
	for _, x := range xValues {
		for _, y := range yValues {
			point, ok := scaledAdd3(state.PixelOriginLPS, state.PixelStepULPS, x)
			if !ok {
				return fail("MPR U extent is non-finite")
			}
			point, ok = scaledAdd3(point, state.PixelStepVLPS, y)
			if !ok {
				return fail("MPR V extent is non-finite")
			}
			for _, offset := range offsets {
				sample := point
				if state.SlabMode != SlabNone {
					var sampleOK bool
					sample, sampleOK = scaledAdd3(point, state.SlabAxisLPS, offset)
					if !sampleOK {
						return fail("MPR slab extent is non-finite")
					}
				}
				if x < lastX {
					next, nextOK := scaledAdd3(sample, state.PixelStepULPS, 1)
					if !nextOK || next == sample {
						return fail("MPR U step collapses within the derived extent")
					}
				}
				if y < lastY {
					next, nextOK := scaledAdd3(sample, state.PixelStepVLPS, 1)
					if !nextOK || next == sample {
						return fail("MPR V step collapses within the derived extent")
					}
				}
			}
		}
	}
	return nil
}

func validateVRViewState(state VRViewState, contractVersion, outputWidth, outputHeight uint32, fail func(string, ...any) error) error {
	if err := state.Transfer2D.Validate(); err != nil {
		return fail("invalid VR 2D transfer table: %v", err)
	}
	if state.Transfer2D.Enabled() && (contractVersion < ViewStateContractVersionV22 || state.Mode != VRModeDVR) {
		return fail("2D transfer table requires DVR V22")
	}
	if !finite3(state.PositionLPS) || !finite3(state.FocalPointLPS) || !finite3(state.ViewUpLPS) {
		return fail("non-finite VR camera")
	}
	direction := sub3(state.FocalPointLPS, state.PositionLPS)
	directionUnit, directionOK := normalize3(direction)
	upUnit, upOK := normalize3(state.ViewUpLPS)
	cameraCross, crossOK := cross3Finite(directionUnit, upUnit)
	if !finite3(direction) || !directionOK || !upOK || !crossOK ||
		norm3(cameraCross) <= 1e-12 {
		return fail("degenerate VR camera")
	}
	switch state.Projection {
	case ProjectionPerspective, ProjectionEndoscopy:
		if state.Projection == ProjectionEndoscopy && contractVersion < ViewStateContractVersionV2 {
			return fail("endoscopy projection requires V2")
		}
		if !finite(state.VerticalFOVRadians) || state.VerticalFOVRadians <= 0 || state.VerticalFOVRadians >= math.Pi {
			return fail("invalid perspective FOV")
		}
	case ProjectionParallel:
		if !finite(state.ParallelScaleMM) || state.ParallelScaleMM <= 0 {
			return fail("invalid parallel scale")
		}
	default:
		return fail("unsupported projection %d", state.Projection)
	}
	if !finite(state.NearMM) || !finite(state.FarMM) || state.NearMM <= 0 || state.NearMM >= state.FarMM {
		return fail("invalid near/far planes")
	}
	switch state.Mode {
	case VRModeRaySum:
		if contractVersion < ViewStateContractVersionV21 || !finite(state.RaySumReferenceMM) || state.RaySumReferenceMM <= 0 || len(state.IsoSurfaces) != 0 {
			return fail("Ray Sum requires V21 and a positive reference distance")
		}
	case VRModeDVR, VRModeMIP, VRModeMinIP, VRModeAverage:
		if len(state.IsoSurfaces) != 0 {
			return fail("VR iso-surfaces require the iso-surface mode")
		}
	case VRModeIsoSurface:
		if contractVersion < ViewStateContractVersionV11 {
			return fail("VR iso-surface mode requires V11")
		}
		if len(state.IsoSurfaces) == 0 || len(state.IsoSurfaces) > MaxVRIsoSurfaces {
			return fail("VR iso-surface count %d outside 1..%d", len(state.IsoSurfaces), MaxVRIsoSurfaces)
		}
		for _, surface := range state.IsoSurfaces {
			if !finite(surface.Value) || !finite(surface.Opacity) || surface.Opacity <= 0 || surface.Opacity > 1 {
				return fail("invalid VR iso-surface %+v", surface)
			}
			for _, channel := range surface.Color {
				if !finite(channel) || channel < 0 || channel > 1 {
					return fail("invalid VR iso-surface colour %+v", surface.Color)
				}
			}
		}
	default:
		return fail("unsupported VR mode %d", state.Mode)
	}
	if state.Mode != VRModeRaySum && state.RaySumReferenceMM != 0 {
		return fail("reference distance requires Ray Sum")
	}
	if state.ProjectionRange.Enabled && (contractVersion < ViewStateContractVersionV21 || (state.Mode != VRModeAverage && state.Mode != VRModeRaySum) || !state.ProjectionRange.Valid()) {
		return fail("invalid or unsupported projection range")
	}
	if !finite(state.SampleDistanceMM) || state.SampleDistanceMM <= 0 ||
		!finite(state.ImageSampleDistance) || state.ImageSampleDistance <= 0 {
		return fail("invalid VR sampling distance")
	}
	if contractVersion >= ViewStateContractVersionV3 {
		if !finite(state.OpacityUnitDistanceMM) || state.OpacityUnitDistanceMM <= 0 {
			return fail("invalid VR opacity unit distance")
		}
	} else if state.OpacityUnitDistanceMM != 0 &&
		(!finite(state.OpacityUnitDistanceMM) || state.OpacityUnitDistanceMM <= 0) {
		return fail("invalid VR opacity unit distance")
	}
	if contractVersion >= ViewStateContractVersionV4 {
		if !finite4(state.BackgroundRGBA) || state.BackgroundRGBA[3] != 1 {
			return fail("invalid opaque VR background")
		}
		for _, channel := range state.BackgroundRGBA[:3] {
			if channel < 0 || channel > 1 {
				return fail("VR background channel outside 0..1")
			}
		}
	} else if state.BackgroundRGBA != [4]float64{} {
		return fail("VR background requires V4")
	}
	if state.ShadingEnabled > 1 {
		return fail("invalid shading flag %d", state.ShadingEnabled)
	}
	if state.OpacityNormals && (contractVersion < ViewStateContractVersionV13 || state.Mode != VRModeDVR) {
		return fail("opacity normals require V13 DVR")
	}
	if state.Illumination != (VRIlluminationState{}) {
		if contractVersion < ViewStateContractVersionV14 || state.Mode != VRModeDVR {
			return fail("illumination requires V14 DVR")
		}
		if err := validateVRIllumination(state.Illumination, VRLightingMaterial{state.Ambient, state.Diffuse, state.Specular, state.SpecularPower}); err != nil {
			return fail("illumination: %v", err)
		}
	}
	if state.JitterEnabled > 1 {
		return fail("invalid jitter flag %d", state.JitterEnabled)
	}
	if state.JitterEnabled != 0 && contractVersion < ViewStateContractVersionV5 {
		return fail("VR jitter requires V5")
	}
	if scattering := state.Scattering; scattering != (VRScatteringState{}) {
		if contractVersion < ViewStateContractVersionV7 {
			return fail("VR scattering requires V7")
		}
		light := Vec3{X: scattering.LightDirectionLPS[0], Y: scattering.LightDirectionLPS[1], Z: scattering.LightDirectionLPS[2]}
		if !finite(scattering.Blend) || scattering.Blend <= 0 || scattering.Blend > 2 ||
			!finite(scattering.ReachMM) || scattering.ReachMM <= 0 ||
			!finite(scattering.Anisotropy) || math.Abs(scattering.Anisotropy) >= 1 ||
			!finite(light.X) || !finite(light.Y) || !finite(light.Z) || math.Abs(light.Length()-1) > 1e-6 {
			return fail("invalid VR scattering %+v", scattering)
		}
	}
	if len(state.GradientOpacity) != 0 {
		if contractVersion < ViewStateContractVersionV10 {
			return fail("VR gradient opacity function requires V10")
		}
		if err := state.GradientOpacity.Validate(); err != nil {
			return fail("%v", err)
		}
		if state.GradientOpacityScale != 0 {
			return fail("VR gradient opacity function and scale are exclusive")
		}
	}
	if geometry := state.Geometry; geometry.Enabled != 0 || len(geometry.Depth) != 0 || len(geometry.Color) != 0 {
		if contractVersion < ViewStateContractVersionV9 {
			return fail("VR geometry requires V9")
		}
		if geometry.Enabled != 1 {
			return fail("invalid VR geometry flag %d", geometry.Enabled)
		}
		if pixels := uint64(outputWidth) * uint64(outputHeight); uint64(len(geometry.Depth)) != pixels || uint64(len(geometry.Color)) != pixels {
			return fail("VR geometry of %d/%d pixels for a %dx%d output", len(geometry.Depth), len(geometry.Color), outputWidth, outputHeight)
		}
		for _, t := range geometry.Depth {
			if math.IsNaN(float64(t)) || t <= 0 {
				return fail("invalid VR geometry depth %v", t)
			}
		}
	}
	if cut := state.Cut; cut != (VRCutPlaneState{}) {
		if contractVersion < ViewStateContractVersionV8 {
			return fail("VR cut planes require V8")
		}
		if cut.Planes == 0 || cut.Planes>>uint(len(state.ClippingPlanesLPS)) != 0 ||
			!finite(cut.ClippedVoxelIntensity) ||
			!finite(cut.CapOpacity) || cut.CapOpacity < 0 || cut.CapOpacity > 1 ||
			(cut.CapOpacity > 0 && (!finite(cut.CapWindow[0]) || !finite(cut.CapWindow[1]) || cut.CapWindow[1] <= cut.CapWindow[0])) {
			return fail("invalid VR cut planes %+v", cut)
		}
	}
	for _, value := range []float64{
		state.Ambient,
		state.Diffuse,
		state.Specular,
		state.SpecularPower,
		state.GradientOpacityScale,
	} {
		if !finite(value) || value < 0 {
			return fail("invalid shading or gradient-opacity coefficient")
		}
	}
	if len(state.ClippingPlanesLPS) > MaxClippingPlanes {
		return fail("too many clipping planes: %d", len(state.ClippingPlanesLPS))
	}
	for _, plane := range state.ClippingPlanesLPS {
		normal := [3]float64{plane[0], plane[1], plane[2]}
		if !finite4(plane) || maxAbs3(normal) == 0 {
			return fail("invalid clipping plane")
		}
	}
	if contractVersion < ViewStateContractVersionV2 {
		if state.Crop.Enabled != 0 || state.Crop.RegionFlags != 0 || state.Crop.Min != [3]float64{} || state.Crop.Max != [3]float64{} ||
			state.CutMask.Dimensions != [3]uint32{} || len(state.CutMask.Words) != 0 {
			return fail("crop or cut mask requires V2")
		}
	} else {
		if err := validateNormalizedCrop(state.Crop, fail); err != nil {
			return err
		}
		if state.Crop.RegionFlags != 0 && contractVersion < ViewStateContractVersionV25 {
			return fail("crop regions require V25")
		}
		if err := validateVoxelMask(state.CutMask, fail); err != nil {
			return err
		}
	}
	if err := validateVRLabelState(state.Labels, contractVersion, fail); err != nil {
		return err
	}
	if !finite(state.TransferLUT.DomainMin) || !finite(state.TransferLUT.DomainMax) ||
		state.TransferLUT.DomainMin >= state.TransferLUT.DomainMax {
		return fail("invalid transfer domain")
	}
	if len(state.TransferLUT.Samples) < 2 || len(state.TransferLUT.Samples) > MaxTransferLUTSamples {
		return fail("transfer sample count %d outside 2..%d", len(state.TransferLUT.Samples), MaxTransferLUTSamples)
	}
	for _, sample := range state.TransferLUT.Samples {
		for _, value := range []float32{sample.R, sample.G, sample.B, sample.A} {
			if !finite(float64(value)) || value < 0 || value > 1 {
				return fail("invalid transfer sample")
			}
		}
	}
	return nil
}

// FreezeViewState validates and deep-copies the variable-size members before a
// request crosses a scheduler or process boundary. Caller mutations after this
// function returns cannot alter the admitted request.
func FreezeViewState(state ViewState) (ViewState, error) {
	if err := ValidateViewState(state); err != nil {
		return ViewState{}, err
	}
	state.Components = state.Components.Clone()
	state.VR.SurfaceProbe = state.VR.SurfaceProbe.Clone()
	state.VR.ClippingPlanesLPS = append([]ClippingPlaneLPS(nil), state.VR.ClippingPlanesLPS...)
	state.VR.TransferLUT.Samples = append([]TransferSample(nil), state.VR.TransferLUT.Samples...)
	state.VR.Transfer2D.Samples = append([]TransferSample(nil), state.VR.Transfer2D.Samples...)
	state.VR.Fusion.Samples = append([]TransferSample(nil), state.VR.Fusion.Samples...)
	state.VR.CutMask.Words = append([]uint32(nil), state.VR.CutMask.Words...)
	state.VR.Labels.Styles = append([]VRLabelStyleState(nil), state.VR.Labels.Styles...)
	state.VR.Geometry.Depth = append([]float32(nil), state.VR.Geometry.Depth...)
	state.VR.Geometry.Color = append([]uint32(nil), state.VR.Geometry.Color...)
	state.VR.IsoSurfaces = append([]VRIsoSurfaceState(nil), state.VR.IsoSurfaces...)
	state.MPR.Presentation = freezeMPRPresentation(state.MPR.Presentation)
	return state, nil
}

func validateVRLabelState(labels VRLabelState, contractVersion uint32, fail func(string, ...any) error) error {
	if labels.Generation == 0 {
		if labels.Dimensions != [3]uint32{} || labels.Bits != 0 || labels.OnlyInside != 0 || len(labels.Styles) != 0 || labels.Volume != nil {
			return fail("VR labels without a generation")
		}
		return nil
	}
	if contractVersion < ViewStateContractVersionV6 {
		return fail("VR labels require V6")
	}
	if labels.Dimensions[0] == 0 || labels.Dimensions[1] == 0 || labels.Dimensions[2] == 0 {
		return fail("empty VR label dimensions")
	}
	if labels.Bits != 8 && labels.Bits != 16 {
		return fail("VR label bits %d", labels.Bits)
	}
	voxels := uint64(labels.Dimensions[0])
	for _, dimension := range labels.Dimensions[1:] {
		if voxels > math.MaxUint64/uint64(dimension) {
			return fail("VR label dimensions overflow")
		}
		voxels *= uint64(dimension)
	}
	if voxels > MaxViewLabelBytes/uint64(labels.Bits/8) {
		return fail("VR label volume over %d bytes", MaxViewLabelBytes)
	}
	if labels.OnlyInside > 1 {
		return fail("invalid VR label only-inside flag %d", labels.OnlyInside)
	}
	if len(labels.Styles) > MaxVRLabelStyles {
		return fail("%d VR label styles over %d", len(labels.Styles), MaxVRLabelStyles)
	}
	for _, style := range labels.Styles {
		for _, value := range []float64{style.RGB[0], style.RGB[1], style.RGB[2], style.Opacity, style.Blend} {
			if !finite(value) || value < 0 || value > 1 {
				return fail("invalid VR label style")
			}
		}
		if style.Visible > 1 {
			return fail("invalid VR label visibility %d", style.Visible)
		}
	}
	if m := labels.Volume; m != nil {
		cols, rows, depth := m.Dimensions()
		if [3]uint32{uint32(cols), uint32(rows), uint32(depth)} != labels.Dimensions || m.StorageBits() > labels.Bits {
			return fail("VR label volume does not match its header")
		}
	}
	return nil
}

func validateNormalizedCrop(crop NormalizedCropBox, fail func(string, ...any) error) error {
	if crop.RegionFlags & ^VRCropAllRegions != 0 {
		return fail("invalid crop region mask")
	}
	if crop.Enabled > 1 {
		return fail("invalid crop flag %d", crop.Enabled)
	}
	if crop.Enabled == 0 {
		if crop.RegionFlags != 0 || crop.Min != [3]float64{} || crop.Max != [3]float64{} {
			return fail("disabled crop carries bounds")
		}
		return nil
	}
	if !finite3(crop.Min) || !finite3(crop.Max) {
		return fail("non-finite crop bounds")
	}
	for axis := range crop.Min {
		if crop.Min[axis] < 0 || crop.Max[axis] > 1 || crop.Min[axis] > crop.Max[axis] {
			return fail("invalid normalized crop bounds")
		}
	}
	return nil
}

func validateVoxelMask(mask VoxelMask, fail func(string, ...any) error) error {
	if mask.Dimensions == [3]uint32{} && len(mask.Words) == 0 {
		return nil
	}
	if mask.Dimensions[0] == 0 || mask.Dimensions[1] == 0 || mask.Dimensions[2] == 0 {
		return fail("cut mask has zero dimension")
	}
	voxels := uint64(mask.Dimensions[0])
	for _, dimension := range mask.Dimensions[1:] {
		if voxels > math.MaxUint64/uint64(dimension) {
			return fail("cut mask dimensions overflow")
		}
		voxels *= uint64(dimension)
	}
	// Derive the ceiling without adding to voxels, so the size check remains
	// correct even at the uint64 boundary.
	words := (voxels-1)/32 + 1
	if words > MaxViewCutMaskBytes/4 || uint64(len(mask.Words)) != words {
		return fail("cut mask word count %d does not match %d voxels", len(mask.Words), voxels)
	}
	return nil
}

func finite(value float64) bool { return !math.IsNaN(value) && !math.IsInf(value, 0) }
func finite3(value [3]float64) bool {
	return finite(value[0]) && finite(value[1]) && finite(value[2])
}
func finite4(value [4]float64) bool {
	return finite(value[0]) && finite(value[1]) && finite(value[2]) && finite(value[3])
}
func norm3(value [3]float64) float64 {
	return math.Hypot(math.Hypot(value[0], value[1]), value[2])
}

func normalize3(value [3]float64) ([3]float64, bool) {
	scale := maxAbs3(value)
	if scale == 0 || !finite(scale) {
		return [3]float64{}, false
	}
	scaled := [3]float64{
		value[0] / scale,
		value[1] / scale,
		value[2] / scale,
	}
	length := norm3(scaled)
	result := [3]float64{
		scaled[0] / length,
		scaled[1] / length,
		scaled[2] / length,
	}
	return result, finite3(result)
}

func maxAbs3(value [3]float64) float64 {
	return math.Max(math.Abs(value[0]), math.Max(math.Abs(value[1]), math.Abs(value[2])))
}

func cross3Finite(a, b [3]float64) ([3]float64, bool) {
	result := [3]float64{
		a[1]*b[2] - a[2]*b[1],
		a[2]*b[0] - a[0]*b[2],
		a[0]*b[1] - a[1]*b[0],
	}
	return result, finite3(result)
}

func scaledAdd3(base, direction [3]float64, scale float64) ([3]float64, bool) {
	var result [3]float64
	for component := 0; component < 3; component++ {
		product := direction[component] * scale
		result[component] = base[component] + product
		if !finite(product) || !finite(result[component]) {
			return [3]float64{}, false
		}
	}
	return result, true
}

func sub3(a, b [3]float64) [3]float64 {
	return [3]float64{a[0] - b[0], a[1] - b[1], a[2] - b[2]}
}
