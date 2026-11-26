package jpeg

import "image"

// Chroma reconstruction for subsampled YCbCr JPEG frames.
//
// image/jpeg returns 4:2:2 and 4:2:0 chroma at its stored resolution and
// image.YCbCr.At replicates each chroma sample over its block (nearest
// neighbour). That produces blocky colour edges and differs from the
// reference decoders DICOM viewers are compared with, which reconstruct
// chroma with a triangular (linear, 3:1 weighted) filter centred between the
// co-sited samples of JFIF (ISO/IEC 10918-5 places subsampled chroma midway
// between luma samples). This file implements that filter and the BT.601
// full-range conversion in 16.16 fixed point, independently of any upstream
// source. Rounding biases alternate between neighbouring outputs so the
// filter is unbiased over a row.

// ycbcrToRGBFrame converts a decoded YCbCr image into interleaved 8-bit RGB.
// It returns false for subsampling ratios it does not reconstruct itself.
func ycbcrToRGBFrame(img *image.YCbCr, rows, columns int) ([]byte, bool) {
	b := img.Bounds()
	if b.Dx() != columns || b.Dy() != rows {
		return nil, false
	}
	var hFactor, vFactor int
	switch img.SubsampleRatio {
	case image.YCbCrSubsampleRatio444:
		hFactor, vFactor = 1, 1
	case image.YCbCrSubsampleRatio422:
		hFactor, vFactor = 2, 1
	case image.YCbCrSubsampleRatio420:
		hFactor, vFactor = 2, 2
	default:
		return nil, false
	}
	out := make([]byte, rows*columns*3)
	cbRow := make([]byte, columns)
	crRow := make([]byte, columns)
	chromaWidth := (columns + hFactor - 1) / hFactor
	chromaHeight := (rows + vFactor - 1) / vFactor
	for y := 0; y < rows; y++ {
		switch vFactor {
		case 1:
			cy := y
			upsampleRow(cbRow, chromaRow(img.Cb, img.CStride, cy, chromaWidth), hFactor)
			upsampleRow(crRow, chromaRow(img.Cr, img.CStride, cy, chromaWidth), hFactor)
		default:
			// Each output row lies a quarter of a chroma row from its own
			// chroma row and three quarters from the neighbour on its side.
			cy := y / 2
			near := cy - 1
			if y%2 == 1 {
				near = cy + 1
			}
			if near < 0 {
				near = 0
			}
			if near >= chromaHeight {
				near = chromaHeight - 1
			}
			upsample2D(cbRow, chromaRow(img.Cb, img.CStride, cy, chromaWidth), chromaRow(img.Cb, img.CStride, near, chromaWidth))
			upsample2D(crRow, chromaRow(img.Cr, img.CStride, cy, chromaWidth), chromaRow(img.Cr, img.CStride, near, chromaWidth))
		}
		luma := img.Y[y*img.YStride : y*img.YStride+columns]
		o := out[y*columns*3 : (y+1)*columns*3]
		for x := 0; x < columns; x++ {
			r, g, bl := ycbcrToRGB(luma[x], cbRow[x], crRow[x])
			o[3*x], o[3*x+1], o[3*x+2] = r, g, bl
		}
	}
	return out, true
}

func chromaRow(plane []byte, stride, row, width int) []byte {
	return plane[row*stride : row*stride+width]
}

// upsampleRow widens one chroma row by factor 1 or 2 with the triangular
// filter: each output is 3/4 of its own sample and 1/4 of the nearer
// neighbour; the outermost outputs copy the edge sample.
func upsampleRow(dst, src []byte, factor int) {
	if factor == 1 {
		copy(dst, src)
		return
	}
	n := len(src)
	for i := 0; i < n; i++ {
		c := int(src[i]) * 3
		left, right := int(src[i]), int(src[i])
		if i > 0 {
			left = int(src[i-1])
		}
		if i+1 < n {
			right = int(src[i+1])
		}
		if 2*i < len(dst) {
			if i == 0 {
				dst[0] = src[0]
			} else {
				dst[2*i] = byte((c + left + 1) >> 2)
			}
		}
		if 2*i+1 < len(dst) {
			if i == n-1 {
				dst[2*i+1] = src[i]
			} else {
				dst[2*i+1] = byte((c + right + 2) >> 2)
			}
		}
	}
}

// upsample2D applies the triangular filter vertically (3/4 own row, 1/4 the
// nearer neighbour row) and then horizontally, keeping the combined sums in
// sixteenths until one final rounding.
func upsample2D(dst, cur, near []byte) {
	n := len(cur)
	col := func(i int) int { return int(cur[i])*3 + int(near[i]) }
	for i := 0; i < n; i++ {
		this := col(i)
		last, next := this, this
		if i > 0 {
			last = col(i - 1)
		}
		if i+1 < n {
			next = col(i + 1)
		}
		if 2*i < len(dst) {
			if i == 0 {
				dst[0] = byte((this*4 + 8) >> 4)
			} else {
				dst[2*i] = byte((this*3 + last + 8) >> 4)
			}
		}
		if 2*i+1 < len(dst) {
			if i == n-1 {
				dst[2*i+1] = byte((this*4 + 7) >> 4)
			} else {
				dst[2*i+1] = byte((this*3 + next + 7) >> 4)
			}
		}
	}
}

const (
	fixShift = 16
	fixHalf  = 1 << (fixShift - 1)
	crToR    = 91881  // round(1.40200 * 2^16)
	cbToG    = -22554 // -round(0.34414 * 2^16)
	crToG    = -46802 // -round(0.71414 * 2^16)
	cbToB    = 116130 // round(1.77200 * 2^16)
)

// ycbcrToRGB is the JFIF full-range BT.601 inverse transform in 16.16 fixed
// point with round-half-up and clamping.
func ycbcrToRGB(y, cb, cr byte) (byte, byte, byte) {
	yy := int(y)
	cbv := int(cb) - 128
	crv := int(cr) - 128
	r := yy + ((crToR*crv + fixHalf) >> fixShift)
	g := yy + ((cbToG*cbv + crToG*crv + fixHalf) >> fixShift)
	b := yy + ((cbToB*cbv + fixHalf) >> fixShift)
	return clamp8(r), clamp8(g), clamp8(b)
}

func clamp8(v int) byte {
	if v < 0 {
		return 0
	}
	if v > 255 {
		return 255
	}
	return byte(v)
}
