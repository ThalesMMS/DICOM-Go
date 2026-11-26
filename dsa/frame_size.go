package dsa

import "math"

func frameSizeMatches(columns, rows int, frames ...[]float64) bool {
	if columns <= 0 || rows <= 0 || columns > math.MaxInt/rows {
		return false
	}
	size := columns * rows
	for _, frame := range frames {
		if len(frame) != size {
			return false
		}
	}
	return true
}
