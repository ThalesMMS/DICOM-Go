//go:build !amd64 || purego || raster2dscalar

package render

const rasterGraySIMD = false

func rasterGraySIMDRow(dst *byte, src *int64, pairs *rasterGrayColumnPair, count int) bool {
	panic("gray SIMD unavailable")
}
