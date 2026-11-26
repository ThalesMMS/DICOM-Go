package render

import (
	"context"
	"image"
	"image/color"
	"sync"

	"github.com/ThalesMMS/dicom-go/internal/parallel"
)

// HalveBox reduces img by 2 in each dimension, averaging each 2×2 block (a
// box filter, so detail finer than the output grid is averaged instead of
// aliased). Like ITK's BinShrinkImageFilter the output size is floor(size/2):
// output pixel (x, y) covers source pixels [2x, 2x+2) × [2y, 2y+2), and an
// odd last row or column is dropped. Averaging is done on premultiplied
// RGBA, rounding to nearest. It returns nil when either dimension of img is
// below 2.
func HalveBox(img image.Image) *image.RGBA {
	if img == nil {
		return nil
	}
	b := img.Bounds()
	w, h := b.Dx()/2, b.Dy()/2
	if w < 1 || h < 1 {
		return nil
	}
	out := image.NewRGBA(image.Rect(0, 0, w, h))
	rows := func(y0, y1 int) {
		switch src := img.(type) {
		case *image.RGBA:
			halveRGBA(out, src.Pix, src.Stride, src.Rect.Min, b, y0, y1)
		case *image.NRGBA:
			halveNRGBA(out, src, b, y0, y1)
		case *image.Gray:
			halveGray(out, src, b, y0, y1)
		default:
			halveGeneric(out, img, b, y0, y1)
		}
	}
	// Rows are independent: split large images across the shared
	// concurrency budget.
	workers := min(parallel.DefaultWorkers(), max(1, w*h/(1<<16)), h)
	if workers <= 1 {
		rows(0, h)
		return out
	}
	_ = parallel.Do(context.Background(), workers, workers, func(i int) {
		rows(i*h/workers, (i+1)*h/workers)
	})
	return out
}

func halveRGBA(out *image.RGBA, pix []byte, stride int, min image.Point, b image.Rectangle, y0, y1 int) {
	w := out.Rect.Dx()
	for y := y0; y < y1; y++ {
		row0 := (b.Min.Y-min.Y+2*y)*stride + (b.Min.X-min.X)*4
		row1 := row0 + stride
		o := y * out.Stride
		for x := 0; x < w; x++ {
			i0, i1 := row0+8*x, row1+8*x
			for c := 0; c < 4; c++ {
				sum := uint32(pix[i0+c]) + uint32(pix[i0+4+c]) + uint32(pix[i1+c]) + uint32(pix[i1+4+c])
				out.Pix[o+4*x+c] = uint8((sum + 2) / 4)
			}
		}
	}
}

func halveNRGBA(out *image.RGBA, src *image.NRGBA, b image.Rectangle, y0, y1 int) {
	w := out.Rect.Dx()
	for y := y0; y < y1; y++ {
		row0 := src.PixOffset(b.Min.X, b.Min.Y+2*y)
		row1 := row0 + src.Stride
		o := y * out.Stride
		for x := 0; x < w; x++ {
			var sum [4]uint32
			for _, i := range [4]int{row0 + 8*x, row0 + 8*x + 4, row1 + 8*x, row1 + 8*x + 4} {
				a := uint32(src.Pix[i+3])
				if a == 255 {
					sum[0] += uint32(src.Pix[i])
					sum[1] += uint32(src.Pix[i+1])
					sum[2] += uint32(src.Pix[i+2])
				} else {
					// Premultiply so transparent pixels do not darken the block.
					sum[0] += (uint32(src.Pix[i])*a + 127) / 255
					sum[1] += (uint32(src.Pix[i+1])*a + 127) / 255
					sum[2] += (uint32(src.Pix[i+2])*a + 127) / 255
				}
				sum[3] += a
			}
			for c := 0; c < 4; c++ {
				out.Pix[o+4*x+c] = uint8((sum[c] + 2) / 4)
			}
		}
	}
}

func halveGray(out *image.RGBA, src *image.Gray, b image.Rectangle, y0, y1 int) {
	w := out.Rect.Dx()
	for y := y0; y < y1; y++ {
		row0 := src.PixOffset(b.Min.X, b.Min.Y+2*y)
		row1 := row0 + src.Stride
		o := y * out.Stride
		for x := 0; x < w; x++ {
			sum := uint32(src.Pix[row0+2*x]) + uint32(src.Pix[row0+2*x+1]) + uint32(src.Pix[row1+2*x]) + uint32(src.Pix[row1+2*x+1])
			v := uint8((sum + 2) / 4)
			out.Pix[o+4*x], out.Pix[o+4*x+1], out.Pix[o+4*x+2], out.Pix[o+4*x+3] = v, v, v, 255
		}
	}
}

func halveGeneric(out *image.RGBA, img image.Image, b image.Rectangle, y0, y1 int) {
	w := out.Rect.Dx()
	for y := y0; y < y1; y++ {
		for x := 0; x < w; x++ {
			var sum [4]uint32
			for _, p := range [4]image.Point{{0, 0}, {1, 0}, {0, 1}, {1, 1}} {
				r, g, bl, a := img.At(b.Min.X+2*x+p.X, b.Min.Y+2*y+p.Y).RGBA()
				sum[0] += r
				sum[1] += g
				sum[2] += bl
				sum[3] += a
			}
			out.SetRGBA(x, y, color.RGBA{
				R: uint8(((sum[0]+2)/4 + 128) / 257),
				G: uint8(((sum[1]+2)/4 + 128) / 257),
				B: uint8(((sum[2]+2)/4 + 128) / 257),
				A: uint8(((sum[3]+2)/4 + 128) / 257),
			})
		}
	}
}

// ShrunkPixelCenter is the source-pixel coordinate of the centre of output
// pixel index after shrinking by factor: factor·index + (factor−1)/2. In
// physical terms the shrunk image's origin moves by (factor−1)/2 source
// spacings and its spacing becomes factor times larger, as ITK's
// BinShrinkImageFilter defines the output geometry.
func ShrunkPixelCenter(index, factor int) float64 {
	return float64(factor*index) + float64(factor-1)/2
}

// ImagePyramid holds an image and its successive HalveBox levels, built on
// demand. Level k is shrunk by 2^k. It is safe for concurrent use.
type ImagePyramid struct {
	mu     sync.Mutex
	levels []image.Image
}

// NewImagePyramid returns the pyramid of img (level 0 is img itself).
func NewImagePyramid(img image.Image) *ImagePyramid {
	return &ImagePyramid{levels: []image.Image{img}}
}

// Source is level 0, the image the pyramid was built from.
func (p *ImagePyramid) Source() image.Image {
	if p == nil {
		return nil
	}
	return p.levels[0]
}

// ForDisplay returns the smallest level still at least width × height
// pixels (the level just above the size it will be drawn at, so the final
// scaling is a magnification or a reduction by less than 2), with its
// shrink factor. A non-positive size returns the source.
func (p *ImagePyramid) ForDisplay(width, height int) (image.Image, int) {
	if p == nil {
		return nil, 1
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	src := p.levels[0]
	if src == nil || width <= 0 || height <= 0 {
		return src, 1
	}
	level, factor := 0, 1
	for {
		b := p.levels[level].Bounds()
		if b.Dx()/2 < width || b.Dy()/2 < height {
			return p.levels[level], factor
		}
		if level+1 == len(p.levels) {
			next := HalveBox(p.levels[level])
			if next == nil {
				return p.levels[level], factor
			}
			p.levels = append(p.levels, next)
		}
		level++
		factor *= 2
	}
}
