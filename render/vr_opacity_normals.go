package render

// dvrOpacityGradient samples classified alpha at six clamped one-voxel
// neighbours. Mapping a scalar difference afterwards is incorrect for a
// non-monotonic transfer function. The index-space covector uses the same
// patient-space conversion as scalar normals. Cuts map their clipped scalar.
func dvrOpacityGradient(sampler *volumeSampler, lut VRLUT, tex Vec3, preset VRPreset, window WindowLevel, dataMin, dataMax float64, clip *VRClip, clippedValue float64) Vec3 {
	index := Vec3{X: tex.X * float64(sampler.cols-1), Y: tex.Y * float64(sampler.rows-1), Z: tex.Z * float64(sampler.depth-1)}
	alphaAt := func(offset Vec3) float64 {
		q := sampler.clampIndex(index.Add(offset))
		axis := func(value float64, dimension int) float64 {
			if dimension <= 1 {
				return 0
			}
			return value / float64(dimension-1)
		}
		neighbour := Vec3{X: axis(q.X, sampler.cols), Y: axis(q.Y, sampler.rows), Z: axis(q.Z, sampler.depth)}
		value, _ := sampler.trilinearAt(q)
		if clip.cutAway(neighbour) {
			value = clippedValue
		}
		if preset.Transfer2D != nil {
			gradient := dvrIndexGradient(sampler, nil, volumeTextureSample{}, neighbour, clip, clip.hasCutPlanes(), false, clippedValue)
			return preset.Transfer2D.Lookup(value, sampler.vol.physicalGradientMagnitude(gradient)).A
		}
		return dvrTransferSample(preset.TF, lut, value, window, dataMin, dataMax).A
	}
	return Vec3{
		X: alphaAt(Vec3{X: 1}) - alphaAt(Vec3{X: -1}),
		Y: alphaAt(Vec3{Y: 1}) - alphaAt(Vec3{Y: -1}),
		Z: alphaAt(Vec3{Z: 1}) - alphaAt(Vec3{Z: -1}),
	}
}
