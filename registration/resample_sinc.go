package registration

import (
	"context"
	"fmt"
	"math"

	"github.com/ThalesMMS/dicom-go/render"
)

const windowedSincRadius = 5
const windowedSincTaps = 2 * windowedSincRadius
const defaultWindowedSincOutputBytes int64 = 512 << 20

func isWindowedSinc(interpolator Interpolator) bool {
	return interpolator == InterpolateLanczosWindowedSinc || interpolator == InterpolateWelchWindowedSinc
}

// windowedSinc evaluates the documented separable windowed-sinc formula.
// Its scratch is fixed-size. The image is borrowed and must not be mutated.
type windowedSinc struct {
	image  *Image
	window Interpolator
}

func (s *windowedSinc) weight(distance float64) float64 {
	if distance == 0 {
		return 1
	}
	if math.Abs(distance) >= windowedSincRadius || distance == math.Trunc(distance) {
		return 0
	}
	t := math.Pi * distance
	kernel := math.Sin(t) / t
	if s.window == InterpolateLanczosWindowedSinc {
		window := t / windowedSincRadius
		return kernel * (math.Sin(window) / window)
	}
	return kernel * (1 - distance*distance/(windowedSincRadius*windowedSincRadius))
}

func (s *windowedSinc) at(ctx context.Context, index render.Vec3) (float64, int, int, error) {
	var at [3][windowedSincTaps]int
	var weights [3][windowedSincTaps]float64
	for axis, q := range [3]float64{index.X, index.Y, index.Z} {
		start := int(math.Floor(q)) + 1 - windowedSincRadius
		for tap := 0; tap < windowedSincTaps; tap++ {
			pixel := start + tap
			weights[axis][tap] = s.weight(q - float64(pixel))
			at[axis][tap] = min(max(pixel, 0), s.image.size[axis]-1)
		}
	}
	first, last := math.MaxInt, math.MinInt
	var value float64
	for k := 0; k < windowedSincTaps; k++ {
		if math.Abs(weights[2][k]) > 1e-12 {
			first, last = min(first, at[2][k]), max(last, at[2][k])
		}
		for j := 0; j < windowedSincTaps; j++ {
			if err := ctx.Err(); err != nil {
				return 0, 0, 0, err
			}
			for i := 0; i < windowedSincTaps; i++ {
				x, y, z := at[0][i], at[1][j], at[2][k]
				pixel := float64(s.image.At(x, y, z))
				// Validate even zero-weight taps: a stencil with invalid values
				// cannot silently become a finite reconstructed export.
				if !finite(pixel) {
					return 0, 0, 0, fmt.Errorf("%w: non-finite moving sample at (%d,%d,%d)", ErrResample, x, y, z)
				}
				value += pixel * weights[0][i] * weights[1][j] * weights[2][k]
			}
		}
	}
	if !finite(value) || !finite(float64(float32(value))) {
		return 0, 0, 0, fmt.Errorf("%w: windowed-sinc result is not finite float32", ErrResample)
	}
	return value, first, last, nil
}
