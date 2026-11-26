package jpegxl

import (
	"context"
	"math"
)

const (
	maxXYBDimension  = 2048
	maxXYBCodestream = 32 << 20
	xybScaleUnit     = 1 << 16
)

func decodeXYB(ctx context.Context, hdr *codestreamHeader, br *bitReader, codestream []byte, f *frameHeader, offsets, sizes []int) (*Image, error) {
	m := hdr.meta
	if m.width > maxXYBDimension || m.height > maxXYBDimension || len(codestream) > maxXYBCodestream {
		return nil, ErrLimit
	}
	if !m.colour.isSRGB || m.depth.floatSample || m.depth.bitsPerSample != 8 ||
		m.orientation != 1 || len(m.extraChannels) != 0 || m.customOpsin || m.customTone ||
		m.haveAnimation || m.havePreview {
		return nil, unsupported("XYB requires static 8-bit sRGB with the default colour transform")
	}
	if f.numPasses != 1 || f.flags & ^uint64(flagSkipLFSm) != 0 || f.blendMode != 0 || f.gab || f.epfIters != 0 {
		return nil, unsupported("XYB requires one replacement pass without restoration filters")
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	br.ctx = ctx
	s, err := readVarDCTSections(br, codestream, f, offsets, sizes, true)
	if err != nil {
		return nil, err
	}
	lf, err := s.xybDC(ctx)
	if err != nil {
		return nil, err
	}
	return s.renderXYB(ctx, lf)
}

// ISO/IEC 18181-1 Annex G default radial weight parameters for DCT8.
// The first term is an absolute weight. Later terms encode relative changes.
var xybDCT8Weights = func() (weights [3][64]float64) {
	bands := [3][6]float64{
		{3150, 0, -0.4, -0.4, -0.4, -2},
		{560, 0, -0.3, -0.3, -0.3, -0.3},
		{512, -2, -1, 0, -1, -2},
	}
	for c, ratios := range bands {
		var radial [6]float64
		radial[0] = ratios[0]
		for i, ratio := range ratios[1:] {
			factor := 1 + ratio
			if ratio < 0 {
				factor = 1 / (1 - ratio)
			}
			radial[i+1] = radial[i] * factor
		}
		for k := range weights[c] {
			distance := math.Hypot(float64(k%8), float64(k/8)) * 5 / (7 * (math.Sqrt2 + 1e-6))
			i := int(distance)
			weight := radial[i] * math.Pow(radial[i+1]/radial[i], distance-float64(i))
			weights[c][k] = 1 / weight
		}
	}
	return weights
}()

func (s *vardctState) xybDC(ctx context.Context) ([3][]float64, error) {
	var dc [3][]float64
	steps := [3]float64{1.0 / 4096, 1.0 / 512, 1.0 / 256}
	for c := range steps {
		if !s.lfDequantDefault {
			steps[c] = float64(s.lfDequant[c]) / 128
			if steps[c] < 1e-8 {
				return dc, corrupt("invalid XYB LF dequantization")
			}
		}
		steps[c] *= xybScaleUnit / (float64(s.globalScale) * float64(s.quantLF))
		dc[c] = make([]float64, len(s.dc[c]))
	}
	corrX := float64(s.baseCorrX) + float64(s.ytoxDC)/float64(s.colourFactor)
	corrB := float64(s.baseCorrB) + float64(s.ytobDC)/float64(s.colourFactor)
	for g, precision := range s.lfPrecision {
		x0, y0, w, h := s.lfGroupRect(g)
		precisionScale := math.Ldexp(1, -int(precision))
		for y := y0; y < y0+h; y++ {
			if err := ctx.Err(); err != nil {
				return dc, err
			}
			start := y*s.f.xsizeBlocks + x0
			for i := start; i < start+w; i++ {
				y := float64(s.dc[1][i]) * steps[1] * precisionScale
				dc[0][i] = float64(s.dc[0][i])*steps[0]*precisionScale + corrX*y
				dc[1][i] = y
				dc[2][i] = float64(s.dc[2][i])*steps[2]*precisionScale + corrB*y
			}
		}
	}
	if s.f.flags&flagSkipLFSm == 0 {
		return smoothXYBLF(ctx, dc, s.f.xsizeBlocks, s.f.ysizeBlocks, steps)
	}
	return dc, nil
}

// The adaptive LF filter shares a gate across all channels. The gate is
// calculated from unfiltered neighbors, and the outer block ring stays intact.
func smoothXYBLF(ctx context.Context, planes [3][]float64, width, height int, steps [3]float64) ([3][]float64, error) {
	if width <= 2 || height <= 2 {
		return planes, nil
	}
	const sideWeight = 0.20345139757231578
	const cornerWeight = 0.0334829185968739
	const centerWeight = 1 - 4*(sideWeight+cornerWeight)
	var result [3][]float64
	for c := range result {
		result[c] = append([]float64(nil), planes[c]...)
	}
	for y := 1; y < height-1; y++ {
		if err := ctx.Err(); err != nil {
			return result, err
		}
		for x := 1; x < width-1; x++ {
			i := y*width + x
			var delta [3]float64
			gap := 0.5
			for c, p := range planes {
				side := p[i-1] + p[i+1] + p[i-width] + p[i+width]
				corner := p[i-width-1] + p[i-width+1] + p[i+width-1] + p[i+width+1]
				delta[c] = centerWeight*p[i] + sideWeight*side + cornerWeight*corner - p[i]
				gap = max(gap, math.Abs(delta[c])/steps[c])
			}
			strength := max(0, 3-4*gap)
			for c := range result {
				result[c][i] += strength * delta[c]
			}
		}
	}
	return result, nil
}

func (s *vardctState) renderXYB(ctx context.Context, dc [3][]float64) (*Image, error) {
	f := s.f
	img := &Image{Width: f.xsize, Height: f.ysize, Channels: 3, BitsPerSample: 8}
	img.Samples = make([]int32, f.xsize*f.ysize*3)
	xScale := math.Pow(0.8, float64(f.xybQuantScale[0])-2)
	bScale := math.Pow(0.8, float64(f.xybQuantScale[1])-2)
	biasRoot := math.Cbrt(xybBias)
	for by := 0; by < f.ysizeBlocks; by++ {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		for bx := 0; bx < f.xsizeBlocks; bx++ {
			i := by*f.xsizeBlocks + bx
			tile := by/colourTileBlocks*s.tilesX + bx/colourTileBlocks
			corrX := float64(s.baseCorrX) + float64(s.ytoxMap[tile])/float64(s.colourFactor)
			corrB := float64(s.baseCorrB) + float64(s.ytobMap[tile])/float64(s.colourFactor)
			scale := xybScaleUnit / (float64(s.globalScale) * float64(s.qf[i]))
			var coeff, pixels [3][64]float64
			for c := range coeff {
				// JPEG XL's DCT stores the block mean in coefficient zero.
				// Convert its normalization to the orthonormal basis.
				coeff[c][0] = 8 * dc[c][i]
			}
			for k := 1; k < 64; k++ {
				x := xybAC(s.coeffs[0][64*i+k], 0) * xybDCT8Weights[0][k] * scale * xScale
				y := xybAC(s.coeffs[1][64*i+k], 1) * xybDCT8Weights[1][k] * scale
				b := xybAC(s.coeffs[2][64*i+k], 2) * xybDCT8Weights[2][k] * scale * bScale
				natural := (k%8)*8 + k/8
				coeff[0][natural] = 8 * (x + corrX*y)
				coeff[1][natural] = 8 * y
				coeff[2][natural] = 8 * (b + corrB*y)
			}
			for c := range pixels {
				idct8x8(&coeff[c], &pixels[c])
			}
			for dy := 0; dy < min(8, f.ysize-by*8); dy++ {
				for dx := 0; dx < min(8, f.xsize-bx*8); dx++ {
					j := dy*8 + dx
					part := (by*8+dy)*f.xsize + bx*8 + dx
					rgb := xybToSRGB(pixels[0][j], pixels[1][j], pixels[2][j], biasRoot)
					copy(img.Samples[part*3:], rgb[:])
				}
			}
		}
	}
	return img, ctx.Err()
}

var xybUnitQuant = [3]float64{0.945349926692846, 0.9299455010825141, 0.9500648966626563}

func xybAC(coefficient int32, channel int) float64 {
	value := float64(coefficient)
	switch coefficient {
	case 0:
		return 0
	case -1, 1:
		return value * xybUnitQuant[channel]
	default:
		return value - 0.145/value
	}
}

const xybBias = 0.0037930732552754493

// ISO/IEC 18181-1 Annex G defines the inverse opsin matrix for linear sRGB.
var defaultInverseOpsin = [3][3]float64{
	{11.031566901960783, -9.866943921568629, -0.16462299647058826},
	{-3.254147380392157, 4.418770392156863, -0.16462299647058826},
	{-3.6588512862745097, 2.7129230470588235, 1.9459282392156863},
}

func xybToSRGB(x, y, b, biasRoot float64) (out [3]int32) {
	gamma := [3]float64{y + x + biasRoot, y - x + biasRoot, b + biasRoot}
	var opsin [3]float64
	for i, g := range gamma {
		opsin[i] = g*g*g - xybBias
	}
	for i, row := range defaultInverseOpsin {
		v := min(1, max(0, row[0]*opsin[0]+row[1]*opsin[1]+row[2]*opsin[2]))
		if v <= 0.0031308 {
			v *= 12.92
		} else {
			v = 1.055*math.Pow(v, 1.0/2.4) - 0.055
		}
		out[i] = int32(math.Round(v * 255))
	}
	return out
}
