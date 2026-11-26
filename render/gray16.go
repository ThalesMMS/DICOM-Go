package render

import (
	"errors"
	"fmt"
	"image"
	"math"

	"github.com/ThalesMMS/dicom-go/pixeldata/display"
	dicomframe "github.com/ThalesMMS/dicom-go/pixeldata/frame"
)

// ErrNotGrayscaleFrame reports a 16-bit presentation request for a color
// frame.
var ErrNotGrayscaleFrame = errors.New("render: frame is not grayscale")

// RenderFrameGray16 renders a grayscale frame through the same modality and
// VOI path as RenderFrame but keeps 16 bits of presentation value (P-values
// 0..65535, MONOCHROME1 already inverted) instead of quantizing to 8 bits.
// Display-domain filters such as imagefilter.CLAHE use it so enhancement
// works on the full post-VOI precision of 12-16 bit acquisitions. Color
// frames return ErrNotGrayscaleFrame.
func RenderFrameGray16(slice *Frame, window WindowLevel) (*image.Gray16, error) {
	var cache *RenderCache
	return cache.RenderFrameGray16(slice, window)
}

// RenderFrameGray16 is the package-level RenderFrameGray16 reusing the
// cache's decoded modality values. The 16-bit result is not cached; a nil
// cache decodes the frame on every call.
func (c *RenderCache) RenderFrameGray16(slice *Frame, window WindowLevel) (*image.Gray16, error) {
	if slice == nil {
		return nil, fmt.Errorf("render: nil slice")
	}
	if slice.DecodeErr != nil {
		return nil, slice.DecodeErr
	}
	window = normalizedRenderWindow(slice, window)
	decoded, err := c.decoded(slice)
	if err != nil {
		return nil, err
	}
	return renderDecodedFrameGray16(decoded, window)
}

func renderDecodedFrameGray16(decoded *decodedFrame, window WindowLevel) (*image.Gray16, error) {
	if decoded == nil {
		return nil, fmt.Errorf("render: nil decoded frame")
	}
	if decoded.rgba != nil {
		return nil, fmt.Errorf("%w: %s", ErrNotGrayscaleFrame, decoded.photometric)
	}
	if !decoded.hasGrayscale() {
		return nil, fmt.Errorf("%w: missing grayscale pixels", dicomframe.ErrInvalidFrameMetadata)
	}
	invert := decoded.photometric == "MONOCHROME1"
	invalid := math.IsNaN(window.Center) || math.IsNaN(window.Width)
	mapper := display.NewVOIByteMapper(display.VOILUT{Center: window.Center, Width: window.Width, Function: window.Function, LUT: window.LUT})
	if decoded.grayscale64 != nil {
		img := decoded.mapGray64(func(value float64) uint16 {
			var p uint16
			if !invalid && !math.IsNaN(value) {
				p = uint16(mapper.Unit(value)*65535 + 0.5)
			}
			if invert {
				p = 65535 - p
			}
			return p
		}, true)
		return img.(*image.Gray16), nil
	}

	// A 4k radiograph is 16M VOI evaluations: mapGray16 evaluates once per
	// stored level when the frame is compact and spreads rows over the
	// shared row workers.
	out := decoded.mapGray16(func(modality float32) uint16 {
		var p uint16
		if value := float64(modality); !invalid && !math.IsNaN(value) {
			p = uint16(mapper.Unit(value)*65535 + 0.5)
		}
		if invert {
			p = 65535 - p
		}
		return p
	})
	return out, nil
}
