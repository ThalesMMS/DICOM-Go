package jpeg2000

import "math"

// reconstructLossy is a test-only model of what a decoder should return for
// an irreversible codestream: the encoder's own quantization indices,
// dequantized at the middle of their interval, synthesized in float64 and
// clamped to the stored range.
func reconstructLossy(tc frameCase, c coding) [][]int32 {
	m := tc.metadata
	w, h := int(m.Columns), int(m.Rows)
	depth := int(m.BitsStored)
	signed := m.PixelRepresentation == 1
	comps := len(tc.samples)
	levels := autoLevels(w, h)
	if c.levels > 0 {
		levels = min(levels, c.levels)
	}
	shift := 0.0
	if !signed {
		shift = math.Ldexp(1, depth-1)
	}
	planes := make([][]float64, comps)
	for i, p := range tc.samples {
		planes[i] = make([]float64, len(p))
		for k, v := range p {
			planes[i][k] = float64(v) - shift
		}
	}
	mct := c.mct && comps == 3
	if mct {
		forwardICT(planes[0], planes[1], planes[2])
	}
	for i, plane := range planes {
		forwardDWT97(plane, w, h, levels)
		bands := quantizeBands(plane, w, h, levels, depth, c.step)
		for k := range plane {
			plane[k] = 0
		}
		for _, res := range bands {
			for _, band := range res {
				delta := math.Ldexp(1+float64(band.mantissa)/2048, depth-band.exponent)
				for y := 0; y < band.h; y++ {
					for x := 0; x < band.w; x++ {
						q := band.coeffs[y*band.w+x]
						v := 0.0
						switch {
						case q > 0:
							v = (float64(q) + 0.5) * delta
						case q < 0:
							v = (float64(q) - 0.5) * delta
						}
						plane[(band.y0+y)*w+band.x0+x] = v
					}
				}
			}
		}
		inverseDWT97(plane, w, h, levels)
		planes[i] = plane
	}
	if mct {
		for k := range planes[0] {
			y, cb, cr := planes[0][k], planes[1][k], planes[2][k]
			planes[0][k] = y + 1.402*cr
			planes[1][k] = y - 0.34413*cb - 0.71414*cr
			planes[2][k] = y + 1.772*cb
		}
	}
	low, high := 0.0, math.Ldexp(1, depth)-1
	if signed {
		low, high = -math.Ldexp(1, depth-1), math.Ldexp(1, depth-1)-1
	}
	out := make([][]int32, comps)
	for i, plane := range planes {
		out[i] = make([]int32, len(plane))
		for k, v := range plane {
			out[i][k] = int32(math.Min(math.Max(math.Round(v+shift), low), high))
		}
	}
	return out
}

// maxDifference is the largest absolute sample difference.
func maxDifference(a, b [][]int32) int32 {
	var worst int32
	for c := range a {
		for i := range a[c] {
			d := a[c][i] - b[c][i]
			if d < 0 {
				d = -d
			}
			worst = max(worst, d)
		}
	}
	return worst
}
