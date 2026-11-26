package render

import (
	"errors"
	"fmt"
	"image"
	"math"

	"github.com/ThalesMMS/dicom-go/pixeldata/display"
	dicomframe "github.com/ThalesMMS/dicom-go/pixeldata/frame"
)

// ErrModalityPrecisionRequired rejects a float32-only display filter for a
// frame whose 2D VOI requires binary64 modality precision.
var ErrModalityPrecisionRequired = errors.New("render: float32 modality filter cannot preserve this frame's precision")

// ModalityRemap maps a modality value (after the Modality LUT or rescale) to
// the modality value that is windowed instead, for example
// imagefilter.HistogramMatch.Map. It must be safe for concurrent use.
type ModalityRemap func(float64) float64

// FrameModalityValues decodes a grayscale frame to its modality values, row
// by row, without caching them. The returned slice belongs to the caller.
// Color frames return ErrNotGrayscaleFrame.
func FrameModalityValues(slice *Frame) ([]float32, error) {
	if slice == nil {
		return nil, fmt.Errorf("render: nil slice")
	}
	if slice.DecodeErr != nil {
		return nil, slice.DecodeErr
	}
	if slice.PreserveModalityPrecision {
		return nil, ErrModalityPrecisionRequired
	}
	decoded, err := decodeSliceFrame(slice)
	if err != nil {
		return nil, err
	}
	if decoded.rgba != nil {
		return nil, fmt.Errorf("%w: %s", ErrNotGrayscaleFrame, decoded.photometric)
	}
	if decoded.grayscale == nil && decoded.grayscale64 == nil {
		return nil, fmt.Errorf("%w: missing grayscale pixels", dicomframe.ErrInvalidFrameMetadata)
	}
	return decoded.grayscale, nil
}

// RenderFrameRemapped renders a grayscale frame like RenderFrame, except that
// every modality value v is replaced by remap(v) before the VOI window is
// applied. It is the hook for display-only intensity transforms such as
// histogram matching of a follow-up examination: the stored pixel data and
// the cache's decoded values are never modified, and the 8-bit result is not
// cached (the caller owns the remap and caches its own results). A nil remap
// renders the plain frame. Color frames return ErrNotGrayscaleFrame; a nil
// cache decodes the frame on every call.
func (c *RenderCache) RenderFrameRemapped(slice *Frame, window WindowLevel, remap ModalityRemap) (*image.Gray, error) {
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
	if decoded.rgba != nil {
		return nil, fmt.Errorf("%w: %s", ErrNotGrayscaleFrame, decoded.photometric)
	}
	if !decoded.hasGrayscale() {
		return nil, fmt.Errorf("%w: missing grayscale pixels", dicomframe.ErrInvalidFrameMetadata)
	}
	invert := decoded.photometric == "MONOCHROME1"
	mapper := display.NewVOIByteMapper(display.VOILUT{Center: window.Center, Width: window.Width, Function: window.Function, LUT: window.LUT})
	if decoded.grayscale64 != nil {
		img := decoded.mapGray64(func(value float64) uint16 {
			if remap != nil {
				value = remap(value)
			}
			var gray uint8
			if !math.IsNaN(value) {
				gray = mapper.Byte(value)
			}
			if invert {
				gray = 255 - gray
			}
			return uint16(gray)
		}, false)
		return img.(*image.Gray), nil
	}

	out := decoded.mapGray(func(modality float32) uint8 {
		value := float64(modality)
		if remap != nil {
			value = remap(value)
		}
		var gray uint8
		if !math.IsNaN(value) {
			gray = mapper.Byte(value)
		}
		if invert {
			gray = 255 - gray
		}
		return gray
	})
	return out, nil
}
