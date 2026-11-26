package dsa

import "fmt"

// SubtractModality returns contrast minus the shifted mask, before windowing.
// Both inputs are row-major modality values on the same columns by rows grid.
// The mask shift uses ApplyShift's bilinear interpolation and edge extension.
// The result owns its buffer and retains signed float64 differences without
// clipping, rescaling, or changing photometric polarity.
func SubtractModality(contrast, mask []float64, columns, rows int, shift Shift) ([]float64, error) {
	if len(contrast) != len(mask) {
		return nil, fmt.Errorf("%w: contrast has %d values, mask has %d", ErrFrameSizeMismatch, len(contrast), len(mask))
	}
	shifted, err := ApplyShift(mask, columns, rows, shift)
	if err != nil {
		return nil, err
	}
	difference := shifted
	if shift.IsZero() {
		// ApplyShift returns the input mask directly for a zero shift.
		difference = make([]float64, len(contrast))
	}
	for i, value := range contrast {
		difference[i] = value - shifted[i]
	}
	return difference, nil
}
