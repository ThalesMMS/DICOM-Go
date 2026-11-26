// Package gridindex holds the canonical continuous→discrete index rules of
// ITK (#1115) for packages below render; render.NearestIndex and friends
// re-export them.
package gridindex

import "math"

// Nearest is the discrete index of continuous index x, floor(x+0.5)
// (itk::Math::RoundHalfIntegerUp).
func Nearest(x float64) int { return int(math.Floor(x + 0.5)) }

// Inside reports whether continuous index x lies on an axis of n voxels,
// -0.5 <= x < n-0.5.
func Inside(x float64, n int) bool {
	return n > 0 && x >= -0.5 && x < float64(n)-0.5
}

// Clamp clamps x to the voxel centres [0,n-1] when it lies inside the axis
// widened by tolerance voxels on both sides.
func Clamp(x float64, n int, tolerance float64) (float64, bool) {
	if n <= 0 || !(x >= -0.5-tolerance && x < float64(n)-0.5+tolerance) {
		return 0, false
	}
	return math.Max(0, math.Min(float64(n-1), x)), true
}
