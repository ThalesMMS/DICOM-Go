package render

import (
	"context"
	"encoding/binary"
	"fmt"
	"math"
)

// ScalarValiditySamples owns decoded GPU readback samples. Decoding converts
// their representation only; it never samples a volume or composes an image.
type ScalarValiditySamples struct {
	Values []float64
	Valid  []bool
}

func DecodeScalarValidityFrame(frame FrameOutput) (ScalarValiditySamples, error) {
	return DecodeScalarValidityFrameContext(context.Background(), frame)
}

func DecodeScalarValidityFrameContext(ctx context.Context, frame FrameOutput) (ScalarValiditySamples, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	if err := ctx.Err(); err != nil {
		return ScalarValiditySamples{}, err
	}
	if err := ValidateFrameOutput(frame, ViewState{}); err != nil {
		return ScalarValiditySamples{}, err
	}
	if frame.Format != FrameFormatScalarValidityF32TopLeft {
		return ScalarValiditySamples{}, fmt.Errorf("%w: scalar validity format required", ErrInvalidFrameOutput)
	}
	count := int(frame.Width) * int(frame.Height)
	result := ScalarValiditySamples{Values: make([]float64, count), Valid: make([]bool, count)}
	for y := 0; y < int(frame.Height); y++ {
		if err := ctx.Err(); err != nil {
			return ScalarValiditySamples{}, err
		}
		for x := 0; x < int(frame.Width); x++ {
			at := uint64(y)*frame.StrideBytes + uint64(x)*8
			value := float64(math.Float32frombits(binary.LittleEndian.Uint32(frame.Pixels[at:])))
			flag := binary.LittleEndian.Uint32(frame.Pixels[at+4:])
			if math.IsNaN(value) || math.IsInf(value, 0) || (flag != 0 && flag != math.Float32bits(1)) {
				return ScalarValiditySamples{}, fmt.Errorf("%w: invalid quantitative sample at %d,%d", ErrInvalidFrameOutput, x, y)
			}
			index := y*int(frame.Width) + x
			result.Values[index], result.Valid[index] = value, flag == math.Float32bits(1)
		}
	}
	return result, nil
}
