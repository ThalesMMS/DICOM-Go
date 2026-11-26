package display

import (
	"errors"
	"fmt"
	"image"
	"math"
)

// MaxExportPixels bounds the output allocations of TransformImage and EncodeTIFF.
// It is a pixel count, not a process memory or input-file quota.
const MaxExportPixels = 1 << 26

var ErrInvalidImageTransform = errors.New("dicom/display: invalid image transform")

// ImageTransform applies a clockwise right-angle rotation, then flips in the
// rotated coordinate system, then nearest-neighbour scaling. Width and Height
// specify both output dimensions; otherwise Scale (zero means one) is used.
// Gray and Gray16 preserve their sample precision; other inputs become RGBA.
type ImageTransform struct {
	RotationDegrees              int
	FlipHorizontal, FlipVertical bool
	Scale                        float64
	Width, Height                int
}

func checkedExportSize(width, height int) bool {
	return width > 0 && height > 0 && width <= MaxExportPixels && height <= MaxExportPixels && width <= MaxExportPixels/height
}

// TransformImage returns a transformed image without modifying the input. An
// identity transform returns the input itself. Scaling selects source pixel
// floor(outputCoordinate * sourceDimension / outputDimension).
func TransformImage(src image.Image, opts ImageTransform) (image.Image, error) {
	if src == nil {
		return nil, fmt.Errorf("%w: nil image", ErrInvalidImageTransform)
	}
	b := src.Bounds()
	w, h := b.Dx(), b.Dy()
	if !checkedExportSize(w, h) {
		return nil, fmt.Errorf("%w: input size exceeds %d pixels", ErrInvalidImageTransform, MaxExportPixels)
	}
	rw, rh := w, h
	switch opts.RotationDegrees {
	case 0, 180:
	case 90, 270:
		rw, rh = h, w
	default:
		return nil, fmt.Errorf("%w: rotation must be 0, 90, 180 or 270", ErrInvalidImageTransform)
	}
	scale := opts.Scale
	if scale == 0 {
		scale = 1
	}
	if math.IsNaN(scale) || math.IsInf(scale, 0) || scale < 1.0/64 || scale > 64 {
		return nil, fmt.Errorf("%w: scale must be finite and in 1/64..64", ErrInvalidImageTransform)
	}
	ow, oh := opts.Width, opts.Height
	if ow == 0 && oh == 0 {
		ow, oh = int(math.Round(float64(rw)*scale)), int(math.Round(float64(rh)*scale))
	} else if ow <= 0 || oh <= 0 || opts.Scale != 0 {
		return nil, fmt.Errorf("%w: size needs two positive dimensions and cannot accompany scale", ErrInvalidImageTransform)
	}
	if !checkedExportSize(ow, oh) {
		return nil, fmt.Errorf("%w: output size exceeds %d pixels or rounds to zero", ErrInvalidImageTransform, MaxExportPixels)
	}
	if opts.RotationDegrees == 0 && !opts.FlipHorizontal && !opts.FlipVertical && ow == w && oh == h {
		return src, nil
	}
	bounds := image.Rect(0, 0, ow, oh)
	var dst image.Image
	switch src.(type) {
	case *image.Gray:
		dst = image.NewGray(bounds)
	case *image.Gray16:
		dst = image.NewGray16(bounds)
	default:
		dst = image.NewRGBA(bounds)
	}
	for y := 0; y < oh; y++ {
		for x := 0; x < ow; x++ {
			u, v := int(int64(x)*int64(rw)/int64(ow)), int(int64(y)*int64(rh)/int64(oh))
			if opts.FlipHorizontal {
				u = rw - 1 - u
			}
			if opts.FlipVertical {
				v = rh - 1 - v
			}
			sx, sy := u, v
			switch opts.RotationDegrees {
			case 90:
				sx, sy = v, h-1-u
			case 180:
				sx, sy = w-1-u, h-1-v
			case 270:
				sx, sy = w-1-v, u
			}
			value := src.At(b.Min.X+sx, b.Min.Y+sy)
			switch d := dst.(type) {
			case *image.Gray:
				d.Set(x, y, value)
			case *image.Gray16:
				d.Set(x, y, value)
			case *image.RGBA:
				d.Set(x, y, value)
			}
		}
	}
	return dst, nil
}
