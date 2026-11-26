//go:build amd64 && !purego && !raster2dscalar

package render

const rasterGraySIMD = true

//go:noescape
func rasterGraySIMDRow(dst *byte, src *int64, pairs *rasterGrayColumnPair, count int) (hasTies bool)
