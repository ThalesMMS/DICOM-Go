package jpegxl

import "math"

// Rendering of JPEG-compatible VarDCT frames: frames whose blocks are all
// DCT8 with a RAW JPEG quantization table and no XYB, which is what JPEG
// recompression and EncodeLossy produce. Their coefficients are exactly JPEG
// coefficients, so the image is rendered as a JPEG decoder would render it:
// dequantization, an 8x8 inverse DCT, a +128 level shift, triangle-filter
// chroma upsampling and the JFIF YCbCr to RGB conversion. Restoration
// filters and LF smoothing signalled by the frame are not applied; the result
// can differ from libjxl by a few levels, as two JPEG decoders do.

func decodeJPEGCompatible(codestream []byte) (*Image, error) {
	hdr, s, err := readJPEGCompatibleFrame(codestream)
	if err != nil {
		return nil, err
	}
	f := s.f
	if f.upsampling != 1 {
		return nil, unsupported("upsampled VarDCT frame")
	}
	grey := hdr.meta.colour.colourSpace == colourSpaceGrey
	n := 3
	if grey {
		n = 1
	}
	j := &jpegData{components: make([]jpegComponent, n), quant: make([]jpegQuantTable, n)}
	for i := range j.components {
		j.components[i].id = uint32(i + 1)
		j.components[i].quantIdx = uint32(i)
	}
	isYCbCr := !grey && f.doYCbCr
	if err := s.fillJPEG(j, grey, isYCbCr); err != nil {
		return nil, err
	}
	return renderJPEG(j, isYCbCr, int(hdr.meta.depth.bitsPerSample)), nil
}

var idctBasis = func() (t [8][8]float64) {
	for u := 0; u < 8; u++ {
		cu := 0.5
		if u == 0 {
			cu = 0.5 / math.Sqrt2
		}
		for x := 0; x < 8; x++ {
			t[u][x] = cu * math.Cos(float64(2*x+1)*float64(u)*math.Pi/16)
		}
	}
	return t
}()

// idct8x8 applies the orthonormal inverse DCT to one block in row-major order.
func idct8x8(coef *[64]float64, out *[64]float64) {
	var tmp [64]float64
	for v := 0; v < 8; v++ { // rows of coefficients -> rows of x
		for x := 0; x < 8; x++ {
			sum := 0.0
			for u := 0; u < 8; u++ {
				sum += idctBasis[u][x] * coef[v*8+u]
			}
			tmp[v*8+x] = sum
		}
	}
	for x := 0; x < 8; x++ {
		for y := 0; y < 8; y++ {
			sum := 0.0
			for v := 0; v < 8; v++ {
				sum += idctBasis[v][y] * tmp[v*8+x]
			}
			out[y*8+x] = sum
		}
	}
}

type plane struct {
	w, h int
	v    []float64
}

func (p *plane) at(x, y int) float64 {
	x = max(0, min(p.w-1, x))
	y = max(0, min(p.h-1, y))
	return p.v[y*p.w+x]
}

// sample returns the value of a plane subsampled by (hs, vs) at full
// resolution position (x, y), interpolating between sample centres.
func (p *plane) sample(x, y, hs, vs int) float64 {
	if hs == 0 && vs == 0 {
		return p.at(x, y)
	}
	fx := (float64(x)+0.5)/float64(int(1)<<hs) - 0.5
	fy := (float64(y)+0.5)/float64(int(1)<<vs) - 0.5
	x0, y0 := int(math.Floor(fx)), int(math.Floor(fy))
	ax, ay := fx-float64(x0), fy-float64(y0)
	top := p.at(x0, y0)*(1-ax) + p.at(x0+1, y0)*ax
	bottom := p.at(x0, y0+1)*(1-ax) + p.at(x0+1, y0+1)*ax
	return top*(1-ay) + bottom*ay
}

func renderJPEG(j *jpegData, isYCbCr bool, bits int) *Image {
	maxH, maxV := 1, 1
	for _, c := range j.components {
		maxH, maxV = max(maxH, c.hSamp), max(maxV, c.vSamp)
	}
	planes := make([]plane, len(j.components))
	shifts := make([][2]int, len(j.components))
	for ci := range j.components {
		c := &j.components[ci]
		q := &j.quant[c.quantIdx].values
		p := plane{w: c.widthBlocks * 8, h: c.heightBlocks * 8}
		p.v = make([]float64, p.w*p.h)
		var coef, out [64]float64
		for by := 0; by < c.heightBlocks; by++ {
			for bx := 0; bx < c.widthBlocks; bx++ {
				blk := c.coeffs[64*(by*c.widthBlocks+bx):]
				for k := 0; k < 64; k++ {
					coef[k] = float64(int32(blk[k]) * q[k])
				}
				idct8x8(&coef, &out)
				for y := 0; y < 8; y++ {
					dst := p.v[(by*8+y)*p.w+bx*8:]
					for x := 0; x < 8; x++ {
						dst[x] = out[y*8+x] + 128
					}
				}
			}
		}
		planes[ci] = p
		shifts[ci] = [2]int{log2Ratio(maxH / c.hSamp), log2Ratio(maxV / c.vSamp)}
	}
	n := len(j.components)
	img := &Image{Width: j.width, Height: j.height, Channels: n, BitsPerSample: bits}
	img.Samples = make([]int32, j.width*j.height*n)
	maxVal := float64(int(1)<<bits - 1)
	scale := maxVal / 255
	put := func(i int, v float64) {
		img.Samples[i] = int32(math.Round(max(0, min(maxVal, v*scale))))
	}
	for y := 0; y < j.height; y++ {
		for x := 0; x < j.width; x++ {
			i := (y*j.width + x) * n
			if n == 1 {
				put(i, planes[0].sample(x, y, shifts[0][0], shifts[0][1]))
				continue
			}
			a := planes[0].sample(x, y, shifts[0][0], shifts[0][1])
			b := planes[1].sample(x, y, shifts[1][0], shifts[1][1])
			c := planes[2].sample(x, y, shifts[2][0], shifts[2][1])
			if isYCbCr {
				cb, cr := b-128, c-128
				a, b, c = a+1.402*cr, a-0.344136*cb-0.714136*cr, a+1.772*cb
			}
			put(i, a)
			put(i+1, b)
			put(i+2, c)
		}
	}
	return img
}

func log2Ratio(r int) int {
	if r >= 2 {
		return 1
	}
	return 0
}
