package jpegxl

import (
	"bytes"
	"fmt"
	"image"
	stdjpeg "image/jpeg"
)

// Lossy encoding: the image is DCT-coded and quantized with JPEG quality
// tables (ITU-T T.81 Annex K scaled by quality) and the quantized
// coefficients are stored in a JPEG-compatible VarDCT frame, entropy coded
// with JPEG XL's context modelling. Every JPEG XL decoder renders the result;
// there is no jbrd box, so no JPEG file is implied.

const (
	// DefaultLossyQuality is the quality used when LossyOptions.Quality is 0.
	DefaultLossyQuality = 90
	MinLossyQuality     = 1
	MaxLossyQuality     = 100
)

// LossyOptions configures EncodeLossy.
type LossyOptions struct {
	// Quality is 1 (smallest) to 100 (best); 0 selects DefaultLossyQuality.
	Quality int
}

// EncodeLossy encodes an 8-bit grey or RGB image as a bare JPEG XL
// codestream with a JPEG-compatible VarDCT frame. RGB is coded as YCbCr with
// 4:2:0 chroma subsampling.
func EncodeLossy(img *Image, opts LossyOptions) ([]byte, error) {
	quality := opts.Quality
	if quality == 0 {
		quality = DefaultLossyQuality
	}
	if quality < MinLossyQuality || quality > MaxLossyQuality {
		return nil, fmt.Errorf("%w: quality %d outside 1..100", ErrUnsupported, quality)
	}
	if err := validateImage(img); err != nil {
		return nil, err
	}
	if img.BitsPerSample != 8 {
		return nil, fmt.Errorf("%w: lossy encoding requires 8-bit samples", ErrUnsupported)
	}
	rect := image.Rect(0, 0, img.Width, img.Height)
	var src image.Image
	if img.Channels == 1 {
		g := image.NewGray(rect)
		for i, v := range img.Samples {
			g.Pix[i] = uint8(v)
		}
		src = g
	} else {
		rgba := image.NewRGBA(rect)
		for i := 0; i < img.Width*img.Height; i++ {
			copy(rgba.Pix[4*i:], []uint8{uint8(img.Samples[3*i]), uint8(img.Samples[3*i+1]), uint8(img.Samples[3*i+2]), 0xFF})
		}
		src = rgba
	}
	var jpg bytes.Buffer
	if err := stdjpeg.Encode(&jpg, src, &stdjpeg.Options{Quality: quality}); err != nil {
		return nil, err
	}
	p, err := parseJPEG(jpg.Bytes())
	if err != nil {
		return nil, err
	}
	return encodeRecompressed(p.data, false)
}

// validateImage checks dimensions, channel count and sample range.
func validateImage(img *Image) error {
	if img == nil || img.Width <= 0 || img.Height <= 0 {
		return fmt.Errorf("%w: empty image", ErrUnsupported)
	}
	if img.Width > 1<<30 || img.Height > 1<<30 || int64(img.Width)*int64(img.Height) > maxImagePixels {
		return fmt.Errorf("%w: image too large", ErrLimit)
	}
	if img.Channels != 1 && img.Channels != 3 {
		return fmt.Errorf("%w: %d channels", ErrUnsupported, img.Channels)
	}
	if img.BitsPerSample < 1 || img.BitsPerSample > 16 {
		return fmt.Errorf("%w: %d bits per sample", ErrUnsupported, img.BitsPerSample)
	}
	if len(img.Samples) != img.Width*img.Height*img.Channels {
		return fmt.Errorf("%w: %d samples for %dx%dx%d", ErrUnsupported, len(img.Samples), img.Width, img.Height, img.Channels)
	}
	maxVal := int32(1)<<img.BitsPerSample - 1
	for _, v := range img.Samples {
		if v < 0 || v > maxVal {
			return fmt.Errorf("%w: sample %d outside 0..%d", ErrUnsupported, v, maxVal)
		}
	}
	return nil
}
