package seriesderive

import (
	"context"
	"fmt"
	"math"

	"github.com/ThalesMMS/dicom-go/object"
	"github.com/ThalesMMS/dicom-go/pixeldata"
)

type geometricImageGrid struct {
	crop                                     CropRegion
	rows, columns, resizeRows, resizeColumns int
	sourceColumns                            int
	inverse                                  [2][3]float64
	geometry                                 GeometricGeometry
	flipHorizontal, flipVertical             bool
	rotateClockwise                          int
}

func geometricGrid(source geometricImageProfile, opts GeometricOptions, limits GeometricLimits) (geometricImageGrid, error) {
	g := geometricImageGrid{crop: CropRegion{Width: source.columns, Height: source.rows}, sourceColumns: source.columns}
	if opts.Crop != nil {
		g.crop = *opts.Crop
	}
	if g.crop.Column < 0 || g.crop.Row < 0 || g.crop.Width <= 0 || g.crop.Height <= 0 ||
		g.crop.Column > source.columns || g.crop.Width > source.columns-g.crop.Column ||
		g.crop.Row > source.rows || g.crop.Height > source.rows-g.crop.Row {
		return g, fmt.Errorf("%w: crop must lie inside the source", ErrGeometricOptions)
	}
	rows, cols := opts.Rows, opts.Columns
	if rows > 65535 || cols > 65535 {
		return g, ErrGeometricLimit
	}
	physicalRows := float64(g.crop.Height) * source.geometry.Spacing[0]
	physicalCols := float64(g.crop.Width) * source.geometry.Spacing[1]
	if !geometricFinite(physicalRows) || !geometricFinite(physicalCols) || physicalRows <= 0 || physicalCols <= 0 {
		return g, ErrGeometricProfile
	}
	switch {
	case rows == 0 && cols == 0:
		rows, cols = g.crop.Height, g.crop.Width
	case rows == 0:
		n := math.Round(float64(cols) * physicalRows / physicalCols)
		if !geometricFinite(n) || n > 65535 {
			return g, ErrGeometricLimit
		}
		rows = int(math.Max(1, n))
	case cols == 0:
		n := math.Round(float64(rows) * physicalCols / physicalRows)
		if !geometricFinite(n) || n > 65535 {
			return g, ErrGeometricLimit
		}
		cols = int(math.Max(1, n))
	}
	count := int64(rows) * int64(cols)
	if rows <= 0 || cols <= 0 || rows > 65535 || cols > 65535 || count > limits.MaxOutputPixels ||
		count > int64(int(^uint(0)>>1))/2 || count*2 > limits.Clone.MaxPixelDataBytes || count*2 > limits.Clone.MaxTotalBytes {
		return g, ErrGeometricLimit
	}
	g.resizeRows, g.resizeColumns = rows, cols
	sx, sy := float64(g.crop.Width)/float64(cols), float64(g.crop.Height)/float64(rows)
	g.inverse = [2][3]float64{{sx, 0, float64(g.crop.Column) + sx*.5 - .5}, {0, sy, float64(g.crop.Row) + sy*.5 - .5}}
	compose := func(a [2][3]float64) {
		before := g.inverse
		for i := 0; i < 2; i++ {
			g.inverse[i][0] = before[i][0]*a[0][0] + before[i][1]*a[1][0]
			g.inverse[i][1] = before[i][0]*a[0][1] + before[i][1]*a[1][1]
			g.inverse[i][2] = before[i][0]*a[0][2] + before[i][1]*a[1][2] + before[i][2]
		}
	}
	if opts.FlipHorizontal {
		compose([2][3]float64{{-1, 0, float64(cols - 1)}, {0, 1, 0}})
	}
	if opts.FlipVertical {
		compose([2][3]float64{{1, 0, 0}, {0, -1, float64(rows - 1)}})
	}
	switch opts.RotateClockwise {
	case 90:
		compose([2][3]float64{{0, 1, 0}, {-1, 0, float64(rows - 1)}})
		rows, cols = cols, rows
	case 180:
		compose([2][3]float64{{-1, 0, float64(cols - 1)}, {0, -1, float64(rows - 1)}})
	case 270:
		compose([2][3]float64{{0, -1, float64(cols - 1)}, {1, 0, 0}})
		rows, cols = cols, rows
	}
	g.rows, g.columns = rows, cols
	g.flipHorizontal, g.flipVertical, g.rotateClockwise = opts.FlipHorizontal, opts.FlipVertical, opts.RotateClockwise
	var colStep, rowStep [3]float64
	for axis := 0; axis < 3; axis++ {
		x := source.geometry.Orientation[axis] * source.geometry.Spacing[1]
		y := source.geometry.Orientation[axis+3] * source.geometry.Spacing[0]
		g.geometry.Position[axis] = source.geometry.Position[axis] + x*g.inverse[0][2] + y*g.inverse[1][2]
		colStep[axis] = x*g.inverse[0][0] + y*g.inverse[1][0]
		rowStep[axis] = x*g.inverse[0][1] + y*g.inverse[1][1]
	}
	g.geometry.Spacing = [2]float64{math.Hypot(math.Hypot(rowStep[0], rowStep[1]), rowStep[2]), math.Hypot(math.Hypot(colStep[0], colStep[1]), colStep[2])}
	if g.geometry.Spacing[0] <= 0 || g.geometry.Spacing[1] <= 0 {
		return g, ErrGeometricProfile
	}
	for axis := 0; axis < 3; axis++ {
		g.geometry.Orientation[axis] = colStep[axis] / g.geometry.Spacing[1]
		g.geometry.Orientation[axis+3] = rowStep[axis] / g.geometry.Spacing[0]
	}
	if deviation, ok := object.ImageOrientationDeviation(g.geometry.Orientation[:]); !ok || deviation > geometricOrientationTolerance {
		return g, ErrGeometricProfile
	}
	// DS conversion has a finite, 16-byte value representation. Validate it
	// before any output pixel allocation or UID callback.
	for _, values := range [][]float64{g.geometry.Position[:], g.geometry.Orientation[:], g.geometry.Spacing[:]} {
		for _, value := range values {
			if !geometricFinite(value) {
				return g, ErrGeometricProfile
			}
			if _, err := object.FormatDS(value); err != nil {
				return g, ErrGeometricProfile
			}
		}
	}
	return g, nil
}

func geometricFinite(v float64) bool { return !math.IsNaN(v) && !math.IsInf(v, 0) }

func resampleGeometric(ctx context.Context, src []byte, format pixeldata.SampleFormat, grid geometricImageGrid, method Interpolation) ([]byte, error) {
	reader, err := format.Reader()
	if err != nil {
		return nil, err
	}
	lo, hi, err := format.Range()
	if err != nil {
		return nil, err
	}
	// Source dimensions are recoverable from the native input length only with
	// its full row stride. The crop does not change that stride.
	stride := grid.sourceColumns
	out := make([]byte, grid.rows*grid.columns*2)
	sample := func(x, y int) int64 { return reader.Value(src[(y*stride+x)*2:]) }
	dx, dy := int64(2*grid.resizeColumns), int64(2*grid.resizeRows)
	denominator := dx * dy
	for row := 0; row < grid.rows; row++ {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		for column := 0; column < grid.columns; column++ {
			// Invert flip/rotate on integer grid indices before resampling. This
			// makes ties independent of floating-point affine composition order.
			x, y := column, row
			switch grid.rotateClockwise {
			case 90:
				x, y = row, grid.resizeRows-1-column
			case 180:
				x, y = grid.resizeColumns-1-column, grid.resizeRows-1-row
			case 270:
				x, y = grid.resizeColumns-1-row, column
			}
			if grid.flipVertical {
				y = grid.resizeRows - 1 - y
			}
			if grid.flipHorizontal {
				x = grid.resizeColumns - 1 - x
			}
			var stored int64
			if method == Nearest {
				// floor(((index+.5)*cropSize/outputSize-.5)+.5)
				// selects the larger source index at an exact half-pixel tie.
				sx := min(int((int64(2*x+1)*int64(grid.crop.Width))/dx), grid.crop.Width-1) + grid.crop.Column
				sy := min(int((int64(2*y+1)*int64(grid.crop.Height))/dy), grid.crop.Height-1) + grid.crop.Row
				stored = sample(sx, sy)
			} else {
				// Exact rational coordinates/weights. For 16-bit samples and US
				// dimensions the weighted sum is <=65535*(2*65535)^2 <2^51,
				// safely within int64. Clamp at crop centres, not source borders.
				nx := max(int64(0), min(int64(2*x+1)*int64(grid.crop.Width)-int64(grid.resizeColumns), int64(grid.crop.Width-1)*dx))
				ny := max(int64(0), min(int64(2*y+1)*int64(grid.crop.Height)-int64(grid.resizeRows), int64(grid.crop.Height-1)*dy))
				x0, y0 := int(nx/dx)+grid.crop.Column, int(ny/dy)+grid.crop.Row
				x1, y1 := min(x0+1, grid.crop.Column+grid.crop.Width-1), min(y0+1, grid.crop.Row+grid.crop.Height-1)
				fx, fy := nx%dx, ny%dy
				sum := (dx-fx)*(dy-fy)*sample(x0, y0) + fx*(dy-fy)*sample(x1, y0) + (dx-fx)*fy*sample(x0, y1) + fx*fy*sample(x1, y1)
				stored = sum / denominator
				if remainder := sum % denominator; remainder*2 >= denominator {
					stored++
				} else if -remainder*2 >= denominator {
					stored--
				}
			}
			stored = max(lo, min(hi, stored))
			// Two's complement in BitsStored; unused high bits are cleared. Their
			// original contents carry no stored sample information (PS3.5 8.1).
			mask := uint16((uint32(1) << uint(format.BitsStored)) - 1)
			format.ByteOrder.PutUint16(out[(row*grid.columns+column)*2:], uint16(stored)&mask)
		}
	}
	return out, nil
}
