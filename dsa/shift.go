package dsa

import (
	"errors"
	"fmt"
	"math"
)

// ErrNoFrames reports AverageFrames called without frames.
var ErrNoFrames = errors.New("dsa: no frames to average")

// Shift is a mask pixel shift in the convention of Mask Sub-pixel Shift
// (0028,6114), PS3.3 C.7.6.10.1.2: Row is the vertical and Column the
// horizontal shift, in pixels, applied to the mask before it is subtracted.
// A positive Row moves the mask toward the lower rows (down); a positive
// Column moves it toward the left-hand columns. Column therefore has the
// opposite sign of an image x displacement.
type Shift struct {
	Row, Column float64
}

// Displacement returns the shift as an image displacement with x to the
// right and y down: dx = -Column, dy = Row.
func (s Shift) Displacement() (dx, dy float64) {
	return -s.Column, s.Row
}

// ShiftFromDisplacement is the Shift that moves the mask by (dx, dy) pixels,
// x to the right and y down.
func ShiftFromDisplacement(dx, dy float64) Shift {
	return Shift{Row: dy, Column: -dx}
}

// Add returns the composition of two shifts.
func (s Shift) Add(other Shift) Shift {
	return Shift{Row: s.Row + other.Row, Column: s.Column + other.Column}
}

// IsZero reports whether the shift moves nothing.
func (s Shift) IsZero() bool {
	return s.Row == 0 && s.Column == 0
}

func (s Shift) isFinite() bool {
	return finite(s.Row) && finite(s.Column)
}

func finite(value float64) bool {
	return !math.IsNaN(value) && !math.IsInf(value, 0)
}

// AverageFrames returns the pixel-wise mean of frames, which must all have
// the same length. Averaging is meant for modality values (after the
// Modality LUT), as the Mask Module averages pixel data before subtraction.
// A single frame is returned as is; several frames produce a new slice.
func AverageFrames(frames ...[]float64) ([]float64, error) {
	if len(frames) == 0 {
		return nil, ErrNoFrames
	}
	if len(frames) == 1 {
		return frames[0], nil
	}
	size := len(frames[0])
	sum := make([]float64, size)
	for index, frame := range frames {
		if len(frame) != size {
			return nil, fmt.Errorf("%w: frame %d has %d values, want %d", ErrFrameSizeMismatch, index, len(frame), size)
		}
		for i, value := range frame {
			sum[i] += value
		}
	}
	scale := 1 / float64(len(frames))
	for i := range sum {
		sum[i] *= scale
	}
	return sum, nil
}

// ApplyShift returns the mask, a columns x rows row-major frame, moved by
// shift with bilinear interpolation: out(x, y) = mask(x - dx, y - dy) where
// (dx, dy) = shift.Displacement(). Samples outside the frame repeat the
// nearest edge pixel, so the shifted-in border subtracts to about zero
// instead of an artificial step. A zero shift returns mask itself.
func ApplyShift(mask []float64, columns, rows int, shift Shift) ([]float64, error) {
	if !frameSizeMatches(columns, rows, mask) {
		return nil, fmt.Errorf("%w: %d values for %dx%d", ErrFrameSizeMismatch, len(mask), columns, rows)
	}
	if !shift.isFinite() {
		return nil, fmt.Errorf("%w: %+v", ErrInvalidPixelShift, shift)
	}
	if shift.IsZero() {
		return mask, nil
	}
	dx, dy := shift.Displacement()
	// A displacement past the whole frame already samples only its edge.
	// Cap it before converting source coordinates to integers.
	dx = min(max(dx, -float64(columns)), float64(columns))
	dy = min(max(dy, -float64(rows)), float64(rows))
	out := make([]float64, len(mask))
	at := func(x, y int) float64 {
		x = min(max(x, 0), columns-1)
		y = min(max(y, 0), rows-1)
		return mask[y*columns+x]
	}
	for y := 0; y < rows; y++ {
		sy := float64(y) - dy
		y0 := int(math.Floor(sy))
		ty := sy - float64(y0)
		for x := 0; x < columns; x++ {
			sx := float64(x) - dx
			x0 := int(math.Floor(sx))
			tx := sx - float64(x0)
			top := at(x0, y0)*(1-tx) + at(x0+1, y0)*tx
			bottom := at(x0, y0+1)*(1-tx) + at(x0+1, y0+1)*tx
			out[y*columns+x] = top*(1-ty) + bottom*ty
		}
	}
	return out, nil
}
