package render

// SourceRGBVRPreset carries neutral DVR camera/lighting defaults without a
// clinical HU gradient, scattering or directional-light classification.
// ComponentPresentation supplies the actual source color and opacity.
func SourceRGBVRPreset() VRPreset {
	p := DefaultVRPreset()
	p.Name = "Source RGB"
	p.Mode = VRModeDVR
	p.TF = NewNormalizedVRTransferFunction([]TFColorStop{{HU: 0}, {HU: 1, R: 1, G: 1, B: 1}}, []TFAlphaStop{{HU: 0}, {HU: 1, A: 1}}, 1)
	p.GradientOpacityScale = 0
	p.GradientOpacity = nil
	p.Scattering = VRScattering{}
	p.OpacityNormals = true
	return p
}
