package render

// VRRayJitter is the deterministic ray-start offset, in [0,1) of a sample
// step, of output pixel (x,y). It is an integer hash (wrapping uint32
// arithmetic, as WGSL u32) whose top 24 bits scale exactly to a fraction, so
// the WebGPU shader computes the same offset.
func VRRayJitter(x, y int) float64 {
	h := uint32(x)*73856093 ^ uint32(y)*19349663
	h = (h ^ h>>16) * 0x7feb352d
	h = (h ^ h>>15) * 0x846ca68b
	h ^= h >> 16
	return float64(h>>8) / (1 << 24)
}
