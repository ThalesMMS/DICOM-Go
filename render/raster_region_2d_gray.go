package render

import (
	"context"
	"encoding/binary"
	"image"
	"math"

	"github.com/ThalesMMS/dicom-go/pixeldata"
)

type rasterGrayAxis struct {
	lo, hi          int
	fraction        float64
	fractionFixed   int64
	inside, covered bool
}

// rasterGrayAxisRows specializes the separable, unmasked grayscale transform.
// All coordinates remain on the original source lattice, including reduced
// crops and images with nonzero bounds or a larger backing stride. The
// accompanying flag reports whether the completed raster is opaque.
func rasterGrayAxisRows(ctx context.Context, gray *image.Gray, source RasterRegion2D, request RasterViewport2DRequest, out *image.NRGBA, determinant, step float64) (func(int, int) error, bool, error) {
	lattice := pixeldata.ReducedRegion(image.Rectangle{Max: source.FullSize}, source.ReductionLevel)
	plan := func(count, full, limit, low, high int, numerator, translation float64) ([]rasterGrayAxis, error) {
		axis := make([]rasterGrayAxis, count)
		for i := range axis {
			// Keep multiply followed by divide, rather than a precomputed inverse:
			// reassociation can change a pixel at an interpolation rounding tie.
			coordinate := (numerator * (float64(i) + .5 - translation)) / determinant
			if math.IsNaN(coordinate) || math.IsInf(coordinate, 0) {
				return nil, ErrRasterRegion2D
			}
			if coordinate < 0 || coordinate >= float64(full) {
				continue
			}
			entry := rasterGrayAxis{inside: true}
			if request.Sampling == RasterNearest2D {
				entry.lo = min(limit-1, max(0, int(math.Floor(coordinate/step))))
				entry.hi = entry.lo
			} else {
				u := coordinate/step - .5
				base := int(math.Floor(u))
				entry.fraction = u - float64(base)
				entry.fractionFixed = int64(entry.fraction * (1 << 24))
				entry.lo = min(limit-1, max(0, base))
				entry.hi = min(limit-1, max(0, base+1))
			}
			entry.covered = entry.lo >= low && entry.lo < high && (entry.fraction == 0 || entry.hi >= low && entry.hi < high)
			entry.lo -= low
			entry.hi -= low
			axis[i] = entry
		}
		return axis, nil
	}
	columns, err := plan(request.Width, source.FullSize.X, lattice.Dx(), source.Grid.Min.X, source.Grid.Max.X, request.Affine[3], request.Affine[4])
	if err != nil {
		return nil, false, err
	}
	rows, err := plan(request.Height, source.FullSize.Y, lattice.Dy(), source.Grid.Min.Y, source.Grid.Max.Y, request.Affine[0], request.Affine[5])
	if err != nil {
		return nil, false, err
	}
	// A diagonal affine maps the source to one contiguous horizontal span,
	// including reflections. Background columns need no per-pixel sampling or
	// repeated coverage checks, and image pixels need no background prefill.
	xBegin, xEnd, columnsCovered := len(columns), 0, true
	for x, column := range columns {
		if column.inside {
			xBegin = min(xBegin, x)
			xEnd = x + 1
			columnsCovered = columnsCovered && column.covered
		}
	}
	// The vector kernel consumes the same vertical scratch as the scalar
	// enlargement. Only covered pairs enter the assembly loop.
	enlarged := request.Sampling == RasterBilinear2D && gray.Rect.Dx() < xEnd-xBegin
	var pairs []rasterGrayColumnPair
	if rasterGraySIMD && enlarged && columnsCovered {
		pairs = rasterGrayPairs(columns[xBegin:xEnd])
	}
	background := make([]byte, out.Stride)
	for i := 0; i < len(background); i += 4 {
		background[i], background[i+1], background[i+2], background[i+3] = request.Background.R, request.Background.G, request.Background.B, request.Background.A
	}
	// Diagonal coordinates are monotone, including reflected axes. When
	// both endpoint rows are inside, every intervening row is inside too.
	opaque := request.Background.A == 255 ||
		xBegin == 0 && xEnd == len(columns) && rows[0].inside && rows[len(rows)-1].inside
	return func(first, last int) error {
		// For horizontal enlargement, interpolate each decoded source column
		// vertically once per row, rather than all four taps per output pixel.
		// This small scratch row belongs exclusively to this worker.
		var vertical []int64
		if enlarged {
			vertical = make([]int64, gray.Rect.Dx())
		}
		for y := first; y < last; y++ {
			if err := ctx.Err(); err != nil {
				return err
			}
			dst := out.Pix[y*out.Stride : (y+1)*out.Stride]
			row := rows[y]
			if !row.inside || xBegin >= xEnd {
				copy(dst, background)
				continue
			}
			if !row.covered || !columnsCovered {
				return ErrRasterRegion2D
			}
			copy(dst[:xBegin*4], background[:xBegin*4])
			copy(dst[xEnd*4:], background[xEnd*4:])
			lo, hi := row.lo*gray.Stride, row.hi*gray.Stride
			fy := row.fraction
			for sx := range vertical {
				value := int64(gray.Pix[lo+sx])
				vertical[sx] = value << 24
				if fy != 0 {
					vertical[sx] += (int64(gray.Pix[hi+sx]) - value) * row.fractionFixed
				}
			}
			xStart := xBegin
			if rasterGraySIMD && len(pairs) != 0 {
				xStart += len(pairs) * 2
				if rasterGraySIMDRow(&dst[xBegin*4], &vertical[0], &pairs[0], len(pairs)) {
					for x := xBegin; x < xStart; x++ {
						if dst[x*4+3] == 0 {
							exact := rasterCanonicalGrayBilinear(gray, columns[x], row)
							binary.LittleEndian.PutUint32(dst[x*4:], uint32(exact)*0x010101|0xff000000)
						}
					}
				}
			}
			for x := xStart; x < xEnd; x++ {
				column := columns[x]
				value := gray.Pix[lo+column.lo]
				if request.Sampling == RasterBilinear2D {
					fx := column.fraction
					// Interior taps are all decoded. Separable interpolation avoids
					// alpha normalization for opaque Gray, except near a rounding
					// boundary where the canonical operation order decides the byte.
					var approximate int64
					if vertical != nil {
						left := vertical[column.lo]
						approximate = left << 24
						if fx != 0 {
							approximate += (vertical[column.hi] - left) * column.fractionFixed
						}
					} else if fx != 0 && fy != 0 {
						top := int64(value)<<24 + (int64(gray.Pix[lo+column.hi])-int64(value))*column.fractionFixed
						bottomLo := int64(gray.Pix[hi+column.lo])
						bottom := bottomLo<<24 + (int64(gray.Pix[hi+column.hi])-bottomLo)*column.fractionFixed
						approximate = top<<24 + (bottom-top)*row.fractionFixed
					}
					if vertical != nil || fx != 0 && fy != 0 {
						// Q24 coefficients differ by less than 1/(1<<24) each.
						// Bilinear byte interpolation changes by at most
						// 2*255/(1<<24) (<1/32768), plus floating-point roundoff.
						// A wider 1/16384 guard around half-integers therefore
						// preserves the canonical byte; ties use its exact order.
						fraction := uint64(approximate) & (1<<48 - 1)
						if fraction < 1<<47-1<<34 || fraction > 1<<47+1<<34 {
							value = uint8((approximate + 1<<47) >> 48)
							binary.LittleEndian.PutUint32(dst[x*4:], uint32(value)*0x010101|0xff000000)
							continue
						}
					}
					value = rasterCanonicalGrayBilinear(gray, column, row)
				}
				binary.LittleEndian.PutUint32(dst[x*4:], uint32(value)*0x010101|0xff000000)
			}
		}
		return nil
	}, opaque, nil
}

// Keep the original operation order at quantization boundaries. Separating this
// uncommon calculation keeps its weights and taps out of the hot loop's state.
func rasterCanonicalGrayBilinear(gray *image.Gray, column, row rasterGrayAxis) uint8 {
	lo, hi := row.lo*gray.Stride, row.hi*gray.Stride
	fx, fy := column.fraction, row.fraction
	w0, w1, w2, w3 := (1-fx)*(1-fy)*255, fx*(1-fy)*255, (1-fx)*fy*255, fx*fy*255
	// Unused taps may lie outside the decoded crop.
	a, v := w0, w0*float64(gray.Pix[lo+column.lo])
	if w1 != 0 {
		a += w1
		v += w1 * float64(gray.Pix[lo+column.hi])
	}
	if w2 != 0 {
		a += w2
		v += w2 * float64(gray.Pix[hi+column.lo])
	}
	if w3 != 0 {
		a += w3
		v += w3 * float64(gray.Pix[hi+column.hi])
	}
	// Every contributing Gray tap is opaque; accumulated alpha rounds to 255.
	return rasterRoundOpaqueGray(v / a)
}

// rasterRoundOpaqueGray matches uint8(math.Round(value)) for the finite,
// nonnegative 8-bit interpolated values above. Below 0.5, adding 0.5 can round
// the immediate predecessor up to 1, so handle that threshold separately.
// At the remaining half-integers the predecessor's spacing is preserved by
// the addition, and truncation implements the canonical ties-away rounding.
func rasterRoundOpaqueGray(value float64) uint8 {
	if value < 0.5 {
		return 0
	}
	return uint8(value + 0.5)
}
