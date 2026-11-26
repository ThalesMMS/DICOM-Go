package roi

import (
	"context"
	"fmt"
	"math"

	"github.com/ThalesMMS/dicom-go/imagefilter"
)

// Flat grayscale erosion, dilation, opening, closing and top-hats with a
// ball structuring element in millimetres (#1105). The ball of radius r mm
// holds the offsets (dx, dy, dz) with (dx*sx)²+(dy*sy)²+(dz*sz)² <= r² for
// the voxel spacing (sx, sy, sz), so anisotropic voxels get an ellipsoid of
// voxels that is round in patient space. The ball is decomposed into one
// x-run per (dy, dz) row, and each output line is the minimum (maximum) of
// the running minima (maxima) of the source lines under those rows,
// computed with a monotonic deque: the cost per voxel is proportional to
// the number of rows of the ball, not to its volume. Neighbours outside the
// image are ignored (ITK's "safe border" behaviour), so an opening never
// darkens the border. Written from the definitions; the top-hats follow
// the definitions of ITK's WhiteTopHatImageFilter and BlackTopHatImageFilter
// (input minus opening, closing minus input); no ITK code is copied.

// maxBallRows bounds the (dy, dz) rows of one structuring element.
const maxBallRows = 1 << 14

// ballRow is one x-run of the structuring element: the row (dy, dz) and the
// half-width of the run.
type ballRow struct{ dy, dz, half int }

// ballRows decomposes the ball of radius r (mm) on spacing into rows. Axes
// of one sample take only the offset 0.
func ballRows(dims [3]int, spacing [3]float64, r float64) ([]ballRow, error) {
	if !(r >= 0) || math.IsInf(r, 0) {
		return nil, fmt.Errorf("%w: radius %v mm", ErrInvalidMorphology, r)
	}
	extent := func(axis int) int {
		if dims[axis] == 1 {
			return 0
		}
		return min(int(math.Floor(r/spacing[axis]+1e-9)), dims[axis]-1)
	}
	ry, rz := extent(1), extent(2)
	if (2*ry+1)*(2*rz+1) > maxBallRows {
		return nil, fmt.Errorf("%w: a %v mm ball spans more than %d rows at this spacing", ErrInvalidMorphology, r, maxBallRows)
	}
	var rows []ballRow
	for dz := -rz; dz <= rz; dz++ {
		for dy := -ry; dy <= ry; dy++ {
			rest := r * r
			if dims[1] > 1 {
				rest -= math.Pow(float64(dy)*spacing[1], 2)
			}
			if dims[2] > 1 {
				rest -= math.Pow(float64(dz)*spacing[2], 2)
			}
			if rest < -1e-9*r*r {
				continue
			}
			half := 0
			if dims[0] > 1 {
				half = min(int(math.Floor(math.Sqrt(max(rest, 0))/spacing[0]+1e-9)), dims[0]-1)
			}
			rows = append(rows, ballRow{dy: dy, dz: dz, half: half})
		}
	}
	return rows, nil
}

// GrayscaleErode returns the flat erosion of src by a ball of radiusMM:
// each voxel becomes the minimum of src over the ball centred on it.
func GrayscaleErode(ctx context.Context, src *imagefilter.Volume, radiusMM float64, opts GrayscaleOptions) (*imagefilter.Volume, error) {
	return ballFilter(ctx, src, radiusMM, opts, false)
}

// GrayscaleDilate returns the flat dilation of src by a ball of radiusMM:
// each voxel becomes the maximum of src over the ball.
func GrayscaleDilate(ctx context.Context, src *imagefilter.Volume, radiusMM float64, opts GrayscaleOptions) (*imagefilter.Volume, error) {
	return ballFilter(ctx, src, radiusMM, opts, true)
}

// GrayscaleOpen returns the dilation of the erosion of src by a ball of
// radiusMM: bright structures narrower than the ball are removed.
func GrayscaleOpen(ctx context.Context, src *imagefilter.Volume, radiusMM float64, opts GrayscaleOptions) (*imagefilter.Volume, error) {
	eroded, err := GrayscaleErode(ctx, src, radiusMM, opts)
	if err != nil {
		return nil, err
	}
	return GrayscaleDilate(ctx, eroded, radiusMM, opts)
}

// GrayscaleClose returns the erosion of the dilation of src by a ball of
// radiusMM: dark structures narrower than the ball are filled.
func GrayscaleClose(ctx context.Context, src *imagefilter.Volume, radiusMM float64, opts GrayscaleOptions) (*imagefilter.Volume, error) {
	dilated, err := GrayscaleDilate(ctx, src, radiusMM, opts)
	if err != nil {
		return nil, err
	}
	return GrayscaleErode(ctx, dilated, radiusMM, opts)
}

// WhiteTopHat returns src minus its opening by a ball of radiusMM: the
// bright details narrower than the ball (small hot lesions on a varying
// background), never negative.
func WhiteTopHat(ctx context.Context, src *imagefilter.Volume, radiusMM float64, opts GrayscaleOptions) (*imagefilter.Volume, error) {
	opened, err := GrayscaleOpen(ctx, src, radiusMM, opts)
	if err != nil {
		return nil, err
	}
	for i, v := range src.Data {
		opened.Data[i] = v - opened.Data[i]
	}
	return opened, nil
}

// BlackTopHat returns the closing of src by a ball of radiusMM minus src:
// the dark details narrower than the ball, never negative.
func BlackTopHat(ctx context.Context, src *imagefilter.Volume, radiusMM float64, opts GrayscaleOptions) (*imagefilter.Volume, error) {
	closed, err := GrayscaleClose(ctx, src, radiusMM, opts)
	if err != nil {
		return nil, err
	}
	for i, v := range src.Data {
		closed.Data[i] -= v
	}
	return closed, nil
}

func ballFilter(ctx context.Context, src *imagefilter.Volume, radiusMM float64, opts GrayscaleOptions, dilate bool) (*imagefilter.Volume, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	opts, err := opts.normalize()
	if err != nil {
		return nil, err
	}
	if err := checkMorphologyVolume(src, opts.MaxVoxels, "image"); err != nil {
		return nil, err
	}
	rows, err := ballRows(src.Dims, src.SpacingMM, radiusMM)
	if err != nil {
		return nil, err
	}
	nx, ny, nz := src.Dims[0], src.Dims[1], src.Dims[2]
	out := &imagefilter.Volume{Dims: src.Dims, SpacingMM: src.SpacingMM, Data: make([]float32, len(src.Data))}
	fill := float32(math.Inf(1))
	if dilate {
		fill = float32(math.Inf(-1))
	}
	err = parallelLines(ctx, ny*nz, func() func(int) {
		run := make([]float32, nx)
		deque := make([]int, nx)
		return func(line int) {
			y, z := line%ny, line/ny
			acc := out.Data[line*nx : (line+1)*nx]
			for x := range acc {
				acc[x] = fill
			}
			for _, row := range rows {
				sy, sz := y+row.dy, z+row.dz
				if sy < 0 || sy >= ny || sz < 0 || sz >= nz {
					continue
				}
				source := src.Data[(sz*ny+sy)*nx : (sz*ny+sy+1)*nx]
				runningExtremum(run, source, row.half, deque, dilate)
				if dilate {
					for x, v := range run {
						acc[x] = max(acc[x], v)
					}
				} else {
					for x, v := range run {
						acc[x] = min(acc[x], v)
					}
				}
			}
		}
	})
	if err != nil {
		return nil, err
	}
	return out, nil
}

// runningExtremum sets dst[x] to the minimum (maximum when high) of
// src[x-half..x+half], clipped to the line, with a monotonic deque of
// indices.
func runningExtremum(dst, src []float32, half int, deque []int, high bool) {
	n := len(src)
	head, tail, next := 0, 0, 0
	for x := 0; x < n; x++ {
		for ; next < n && next <= x+half; next++ {
			v := src[next]
			for tail > head && (!high && src[deque[tail-1]] >= v || high && src[deque[tail-1]] <= v) {
				tail--
			}
			deque[tail] = next
			tail++
		}
		for deque[head] < x-half {
			head++
		}
		dst[x] = src[deque[head]]
	}
}
