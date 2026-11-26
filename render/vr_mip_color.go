package render

// VRMIPColor maps a maximum modality value through a baked, linear-light RGB
// table. The projection is opaque regardless of transfer opacity. Inversion
// complements each display-space channel before returning to linear light.
func VRMIPColor(lut VRLUT, modality float64, inverse bool) RGBA {
	low, high := lut.Domain()
	t := 0.0
	if high > low {
		t = (modality - low) / (high - low)
	}
	mapped := lut.Lookup(t)
	if inverse {
		mapped.R = srgbToLinear(1 - linearToSRGB(mapped.R))
		mapped.G = srgbToLinear(1 - linearToSRGB(mapped.G))
		mapped.B = srgbToLinear(1 - linearToSRGB(mapped.B))
	}
	mapped.A = 1
	return mapped
}
