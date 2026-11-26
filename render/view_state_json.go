package render

import (
	"encoding/json"
	"reflect"
)

// MarshalJSON preserves the zero-value omission of the presentation block on
// Go 1.22 as well as toolchains that understand the omitzero field tag.
// Validation remains separate from serialization.
func (state MPRViewState) MarshalJSON() ([]byte, error) {
	type fields MPRViewState
	return json.Marshal(struct {
		fields
		Presentation *MPRPresentationState `json:"Presentation,omitempty"`
	}{fields: fields(state), Presentation: viewStateJSONOptional(state.Presentation)})
}

// MarshalJSON preserves optional blocks' Go zero-value semantics, including
// nil versus non-nil empty slices and non-zero sidecars excluded from JSON.
func (state VRViewState) MarshalJSON() ([]byte, error) {
	type fields VRViewState
	return json.Marshal(struct {
		fields
		ProjectionRange *VRValueRange            `json:"ProjectionRange,omitempty"`
		Stereo          *StereoPresentationState `json:"Stereo,omitempty"`
		Labels          *VRLabelState            `json:"Labels,omitempty"`
		Scattering      *VRScatteringState       `json:"Scattering,omitempty"`
		Cut             *VRCutPlaneState         `json:"Cut,omitempty"`
		Geometry        *VRGeometryState         `json:"Geometry,omitempty"`
		GradientOpacity *VRGradientOpacity       `json:"GradientOpacity,omitempty"`
		IsoSurfaces     *[]VRIsoSurfaceState     `json:"IsoSurfaces,omitempty"`
		Illumination    *VRIlluminationState     `json:"Illumination,omitempty"`
	}{
		fields: fields(state), ProjectionRange: viewStateJSONOptional(state.ProjectionRange), Stereo: viewStateJSONOptional(state.Stereo),
		Labels: viewStateJSONOptional(state.Labels), Scattering: viewStateJSONOptional(state.Scattering),
		Cut: viewStateJSONOptional(state.Cut), Geometry: viewStateJSONOptional(state.Geometry),
		GradientOpacity: viewStateJSONOptional(state.GradientOpacity), IsoSurfaces: viewStateJSONOptional(state.IsoSurfaces),
		Illumination: viewStateJSONOptional(state.Illumination),
	})
}

// MarshalJSON omits zero overlay and fusion blocks on all supported toolchains.
func (state MPRPresentationState) MarshalJSON() ([]byte, error) {
	type fields MPRPresentationState
	return json.Marshal(struct {
		fields
		Overlay *MPRLayerPresentation `json:"Overlay,omitempty"`
		Fusion  *MPRFusionState       `json:"Fusion,omitempty"`
	}{fields: fields(state), Overlay: viewStateJSONOptional(state.Overlay), Fusion: viewStateJSONOptional(state.Fusion)})
}

// The optional types do not define IsZero. Test the complete Go value, rather
// than enabled flags or serialized fields: ignored sidecars can make a block
// non-zero. This copies only the value, without modifying caller-owned slices.
func viewStateJSONOptional[T any](value T) *T {
	if reflect.ValueOf(value).IsZero() {
		return nil
	}
	return &value
}

// MarshalJSON keeps old zero-filter request serialization unchanged on Go 1.22.
func (state ViewState) MarshalJSON() ([]byte, error) {
	type fields ViewState
	return json.Marshal(struct {
		fields
		VolumeFilter *VolumeFilterState `json:"VolumeFilter,omitempty"`
	}{fields: fields(state), VolumeFilter: viewStateJSONOptional(state.VolumeFilter)})
}
