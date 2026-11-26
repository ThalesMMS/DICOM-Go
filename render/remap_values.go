package render

import (
	"fmt"
	"image"
	"math"

	"github.com/ThalesMMS/dicom-go/pixeldata/display"
	dicomframe "github.com/ThalesMMS/dicom-go/pixeldata/frame"
)

// RenderFrameValues renders a grayscale frame like RenderFrame, except that
// its modality values are replaced by values (one per pixel, row by row, as
// FrameModalityValues returns them) before the VOI window is applied. It is
// the hook for display-only filters of modality values, such as
// imagefilter.GradientAnisotropicDiffusion on a displayed slice: the stored
// pixel data and the cache's decoded values are never modified, and the
// 8-bit result is not cached. NaN values render black. Color frames return
// ErrNotGrayscaleFrame; a nil cache decodes the frame on every call.
func (c *RenderCache) RenderFrameValues(slice *Frame, window WindowLevel, values []float32) (*image.Gray, error) {
	if slice == nil {
		return nil, fmt.Errorf("render: nil slice")
	}
	if slice.DecodeErr != nil {
		return nil, slice.DecodeErr
	}
	if slice.PreserveModalityPrecision {
		return nil, ErrModalityPrecisionRequired
	}
	window = normalizedRenderWindow(slice, window)
	decoded, err := c.decoded(slice)
	if err != nil {
		return nil, err
	}
	if decoded.rgba != nil {
		return nil, fmt.Errorf("%w: %s", ErrNotGrayscaleFrame, decoded.photometric)
	}
	if decoded.grayscale == nil && !decoded.hasLevelIndex() {
		return nil, fmt.Errorf("%w: missing grayscale pixels", dicomframe.ErrInvalidFrameMetadata)
	}
	if len(values) != decoded.rows*decoded.cols {
		return nil, fmt.Errorf("render: %d values for a %d×%d frame", len(values), decoded.cols, decoded.rows)
	}
	invert := decoded.photometric == "MONOCHROME1"
	mapper := display.NewVOIByteMapper(display.VOILUT{Center: window.Center, Width: window.Width, Function: window.Function, LUT: window.LUT})
	out := image.NewGray(image.Rect(0, 0, decoded.cols, decoded.rows))
	parallelRows(decoded.rows, func(y int) {
		start := y * decoded.cols
		for i, value := range values[start : start+decoded.cols] {
			var gray uint8
			if !math.IsNaN(float64(value)) {
				gray = mapper.Byte(float64(value))
			}
			if invert {
				gray = 255 - gray
			}
			out.Pix[start+i] = gray
		}
	})
	return out, nil
}
