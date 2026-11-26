package compare

import (
	"context"
	"fmt"
	stdimage "image"
	"image/color"
	"math"

	"github.com/ThalesMMS/dicom-go/render/qualification"
)

// PresentedImagePolicy declares how already-rendered image pixels are compared.
// RGB encoding is srgb-code or linear-rgb (IEC sRGB decoding before comparison).
// Gray is gray-pvalue, with no gamma conversion. Alpha is opaque (refuse any
// transparency), ignore (unassociated channels), or composite. Background is
// an explicitly declared unassociated sRGB code triplet, normalized to [0,1];
// compositing occurs in the chosen comparison encoding, not implicitly in linear.
// For grayscale, all three background values must be equal.
type PresentedImagePolicy struct {
	Depth      int        `json:"depth"`
	Encoding   string     `json:"encoding"`
	Alpha      string     `json:"alpha"`
	Background [3]float64 `json:"background"`
}

func (p PresentedImagePolicy) validate() error {
	if (p.Depth != 8 && p.Depth != 16) || (p.Encoding != "gray-pvalue" && p.Encoding != "srgb-code" && p.Encoding != "linear-rgb") || (p.Alpha != "opaque" && p.Alpha != "ignore" && p.Alpha != "composite") {
		return ErrPresentedConfiguration
	}
	for _, v := range p.Background {
		if !finite(v) || v < 0 || v > 1 {
			return ErrPresentedConfiguration
		}
	}
	if p.Encoding == "gray-pvalue" && (p.Background[0] != p.Background[1] || p.Background[0] != p.Background[2]) {
		return ErrPresentedConfiguration
	}
	return nil
}
func srgbToLinear(v float64) float64 {
	if v <= .04045 {
		return v / 12.92
	}
	return math.Pow((v+.055)/1.055, 2.4)
}

func supportedPresentedImage(im stdimage.Image, depth int) bool {
	switch v := im.(type) {
	case *stdimage.Gray:
		return v != nil && depth == 8
	case *stdimage.RGBA:
		return v != nil && depth == 8
	case *stdimage.NRGBA:
		return v != nil && depth == 8
	case *stdimage.Gray16:
		return v != nil && depth == 16
	case *stdimage.RGBA64:
		return v != nil && depth == 16
	case *stdimage.NRGBA64:
		return v != nil && depth == 16
	}
	return false
}
func imagePlane(ctx context.Context, im stdimage.Image, p PresentedImagePolicy, o PresentedOptions) (PresentedFrame, error) {
	if im == nil {
		return PresentedFrame{}, ErrMismatch
	}
	if err := p.validate(); err != nil {
		return PresentedFrame{}, err
	}
	bounds := im.Bounds()
	c := 3
	if p.Encoding == "gray-pvalue" {
		c = 1
		switch im.(type) {
		case *stdimage.Gray, *stdimage.Gray16:
		default:
			return PresentedFrame{}, fmt.Errorf("%w: grayscale requires a Gray/Gray16 image", ErrMismatch)
		}
	}
	f := PresentedFrame{Width: bounds.Dx(), Height: bounds.Dy(), Components: c, Depth: p.Depth, Encoding: p.Encoding}
	bytes, err := presentedBudget(f.Width, f.Height, c, o, int64(c)*16)
	if err != nil {
		return f, err
	}
	_ = bytes
	n := int64(f.Width) * int64(f.Height)
	f.Values = make([]float64, int(n)*c)
	background := p.Background
	if p.Encoding == "linear-rgb" {
		for i := range background {
			background[i] = srgbToLinear(background[i])
		}
	}
	for y := 0; y < f.Height; y++ {
		if err := ctx.Err(); err != nil {
			return PresentedFrame{}, err
		}
		for x := 0; x < f.Width; x++ {
			if x%1024 == 0 {
				if err := ctx.Err(); err != nil {
					return PresentedFrame{}, err
				}
			}
			v := color.NRGBA64Model.Convert(im.At(bounds.Min.X+x, bounds.Min.Y+y)).(color.NRGBA64)
			values := [3]float64{float64(v.R) / 65535, float64(v.G) / 65535, float64(v.B) / 65535}
			alpha := float64(v.A) / 65535
			if p.Alpha == "opaque" && v.A != 65535 {
				return PresentedFrame{}, fmt.Errorf("%w: nonopaque pixel", ErrMismatch)
			}
			for ch := 0; ch < c; ch++ {
				if p.Encoding == "linear-rgb" {
					values[ch] = srgbToLinear(values[ch])
				}
				if p.Alpha == "composite" {
					values[ch] = alpha*values[ch] + (1-alpha)*background[ch]
				}
				f.Values[(y*f.Width+x)*c+ch] = values[ch]
			}
		}
	}
	return f, nil
}

// ComparePresentedImages accepts already-produced images; it never generates a
// volume/3D frame. Bounds may have different origins but equal extents. The
// declared depth must match Gray/NRGBA/RGBA8 or their 16-bit counterparts;
// unsupported custom color models are refused instead of guessing color space.
func ComparePresentedImages(ctx context.Context, reference, candidate stdimage.Image, p PresentedImagePolicy, o PresentedOptions) (*PresentedResult, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if !supportedPresentedImage(reference, p.Depth) || !supportedPresentedImage(candidate, p.Depth) {
		return nil, ErrMismatch
	}
	if reference.Bounds().Size() != candidate.Bounds().Size() {
		return nil, ErrMismatch
	}
	a, err := imagePlane(ctx, reference, p, o)
	if err != nil {
		return nil, err
	}
	b, err := imagePlane(ctx, candidate, p, o)
	if err != nil {
		return nil, err
	}
	r, err := ComparePresented(ctx, a, b, o)
	if err == nil {
		r.ImagePolicy = &p
		r.LogicalWorkingBytes += int64(len(a.Values)+len(b.Values)) * 8
	}
	return r, err
}

// ComparePresentedRGBA8 reuses the existing pre-background RGBA component and
// silhouette metrics before applying the explicitly selected presentation policy.
// RawRGBA is distinct from post-composite numerical error and local similarity.
func ComparePresentedRGBA8(ctx context.Context, reference, candidate []byte, width, height int, p PresentedImagePolicy, o PresentedOptions, t qualification.RGBAFrameThresholdsV1) (*PresentedResult, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if width <= 0 || height <= 0 || uint64(width) > math.MaxUint32 || uint64(height) > math.MaxUint32 || int64(width) > math.MaxInt64/int64(height) || int64(width)*int64(height) > o.MaxPixels {
		return nil, ErrPresentedLimit
	}
	if int64(width)*int64(height) > int64(math.MaxInt)/4 {
		return nil, ErrPresentedLimit
	}
	want := width * height * 4
	if len(reference) != want || len(candidate) != want || p.Depth != 8 || p.Encoding == "gray-pvalue" {
		return nil, ErrMismatch
	}
	raw, err := qualification.CompareRGBA8Frames(reference, candidate, uint32(width), uint32(height), t)
	if err != nil {
		return nil, err
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	rect := stdimage.Rect(0, 0, width, height)
	a := &stdimage.NRGBA{Pix: reference, Stride: width * 4, Rect: rect}
	b := &stdimage.NRGBA{Pix: candidate, Stride: width * 4, Rect: rect}
	r, err := ComparePresentedImages(ctx, a, b, p, o)
	if err == nil {
		r.RawRGBA = &raw
	}
	return r, err
}
