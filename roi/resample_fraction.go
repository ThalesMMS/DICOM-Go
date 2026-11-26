package roi

import (
	"context"
	"fmt"
	"math"
)

// Fractional (probability or occupancy) resampling (#1114): fractions are
// continuous, so they are interpolated linearly (trilinear in the source
// index space, piecewise linear between irregular slices) and a caller
// thresholds the result, as DICOM FRACTIONAL segmentations are displayed.

// FractionSource is a non-negative scalar image on a patient-space grid.
type FractionSource interface {
	FractionGrid() Grid
	FractionAt(x, y, z int) float64
}

// FractionStack stores fractions as one row-major Columns*Rows byte slice
// per grid slice (nil for an all-zero slice), such as the frames of a DICOM
// FRACTIONAL segmentation, in sample units (0 to its maximum). The slices are
// shared and must not change while the stack is in use.
type FractionStack struct {
	grid   Grid
	slices [][]uint8
	bounds IndexBox
	filled bool
}

// NewFractionStack wraps per-slice samples on grid.
func NewFractionStack(grid Grid, slices [][]uint8) (*FractionStack, error) {
	if err := grid.Validate(); err != nil {
		return nil, err
	}
	if len(slices) != grid.Slices {
		return nil, fmt.Errorf("%w: %d fraction slices for %d grid slices", ErrInvalidResample, len(slices), grid.Slices)
	}
	s := &FractionStack{grid: grid, slices: slices}
	b := newBoundsBuilder()
	for z, values := range slices {
		if values == nil {
			continue
		}
		if len(values) != grid.Columns*grid.Rows {
			return nil, fmt.Errorf("%w: fraction slice %d has %d values, want %d", ErrInvalidResample, z, len(values), grid.Columns*grid.Rows)
		}
		for i, value := range values {
			if value != 0 {
				b.add(i%grid.Columns, i/grid.Columns, z)
			}
		}
	}
	s.bounds, s.filled = b.box()
	return s, nil
}

func (s *FractionStack) FractionGrid() Grid { return s.grid }

func (s *FractionStack) FractionAt(x, y, z int) float64 {
	values := s.slices[z]
	if values == nil {
		return 0
	}
	return float64(values[y*s.grid.Columns+x])
}

// LabelBounds returns the index box of the non-zero samples.
func (s *FractionStack) LabelBounds() (IndexBox, bool) { return s.bounds, s.filled }

// FractionResampleOptions bound fraction resampling.
type FractionResampleOptions struct {
	// MaxVoxels bounds the target voxels; zero means DefaultMaxResampleVoxels.
	MaxVoxels int64
	// Mapper, when set, maps target points into the source's Frame of
	// Reference; see LabelResampleMode.Mapper.
	Mapper PointMapper
}

// FractionResampleResult holds linearly interpolated fractions on the target
// grid, x fastest; uncovered voxels are 0.
type FractionResampleResult struct {
	Grid     Grid
	Values   []float32
	Coverage ResampleCoverage
}

// FractionMaskResult holds the target voxels whose interpolated fraction
// reaches a threshold, per target slice (nil when none).
type FractionMaskResult struct {
	Grid     Grid
	Masks    []*RasterMask
	Count    int
	Coverage ResampleCoverage
}

// ResampleFractions interpolates src linearly at every target voxel centre.
func ResampleFractions(ctx context.Context, src FractionSource, target Grid, opts FractionResampleOptions) (*FractionResampleResult, error) {
	if err := checkTargetBudget(target, fractionBudget(opts)); err != nil {
		return nil, err
	}
	out := &FractionResampleResult{Grid: target, Values: make([]float32, target.VoxelCount())}
	coverage, err := resampleFractions(ctx, src, target, opts.Mapper, func(y, z, x0 int, values []float64) {
		row := out.Values[(z*target.Rows+y)*target.Columns+x0:]
		for i, value := range values {
			row[i] = float32(value)
		}
	})
	if err != nil {
		return nil, err
	}
	out.Coverage = coverage
	return out, nil
}

// ResampleFractionMask interpolates src linearly and keeps the target voxels
// whose value is at least threshold (in the source's units) and above zero.
func ResampleFractionMask(ctx context.Context, src FractionSource, target Grid, threshold float64, opts FractionResampleOptions) (*FractionMaskResult, error) {
	if math.IsNaN(threshold) || math.IsInf(threshold, 0) {
		return nil, fmt.Errorf("%w: threshold %v", ErrInvalidResample, threshold)
	}
	if err := checkTargetBudget(target, fractionBudget(opts)); err != nil {
		return nil, err
	}
	out := &FractionMaskResult{Grid: target, Masks: make([]*RasterMask, target.Slices)}
	rowCounts := make([]int32, target.Rows*target.Slices)
	// Slice masks are created up front so rows can be written concurrently.
	for z := range out.Masks {
		out.Masks[z] = NewRasterMask(target.Columns, target.Rows)
	}
	coverage, err := resampleFractions(ctx, src, target, opts.Mapper, func(y, z, x0 int, values []float64) {
		start, n := -1, 0
		for i := 0; i <= len(values); i++ {
			on := i < len(values) && values[i] > 0 && values[i] >= threshold-1e-9
			if on {
				n++
			}
			if on && start < 0 {
				start = i
			} else if !on && start >= 0 {
				out.Masks[z].SetRun(y, x0+start, x0+i)
				start = -1
			}
		}
		rowCounts[z*target.Rows+y] = int32(n)
	})
	if err != nil {
		return nil, err
	}
	for z := range out.Masks {
		n := 0
		for y := 0; y < target.Rows; y++ {
			n += int(rowCounts[z*target.Rows+y])
		}
		out.Count += n
		if n == 0 {
			out.Masks[z] = nil
		}
	}
	out.Coverage = coverage
	return out, nil
}

func fractionBudget(opts FractionResampleOptions) int64 {
	if opts.MaxVoxels > 0 {
		return opts.MaxVoxels
	}
	return DefaultMaxResampleVoxels
}

// resampleFractions evaluates the covered target voxels near the non-zero
// samples and hands each row's values to emit (rows are distinct across
// concurrent calls).
func resampleFractions(ctx context.Context, src FractionSource, target Grid, mapper PointMapper, emit func(y, z, x0 int, values []float64)) (ResampleCoverage, error) {
	if src == nil {
		return ResampleCoverage{}, fmt.Errorf("%w: nil source", ErrInvalidResample)
	}
	sourceGrid := src.FractionGrid()
	sm, err := newGridMap(sourceGrid)
	if err != nil {
		return ResampleCoverage{}, err
	}
	box, filled := IndexBox{Max: [3]int{sourceGrid.Columns, sourceGrid.Rows, sourceGrid.Slices}}, true
	if bounded, ok := src.(labelBounded); ok {
		box, filled = bounded.LabelBounds()
	}
	var evalLo, evalHi [3]float64
	if filled {
		// Linear interpolation reaches one voxel beyond the non-zero box.
		grown := box
		limits := [3]int{sourceGrid.Columns, sourceGrid.Rows, sourceGrid.Slices}
		for i := range grown.Min {
			grown.Min[i], grown.Max[i] = max(0, box.Min[i]-1), min(limits[i], box.Max[i]+1)
		}
		evalLo, evalHi = sm.boxExtent(grown)
	}
	rows := newTargetRows(sm, target)
	coverage := newCoverage(target)
	err = parallelLines(ctx, target.Rows*target.Slices, func() func(int) {
		buffer := make([]float64, target.Columns)
		var coords [][3]float64
		var inside []bool
		if mapper != nil {
			coords, inside = make([][3]float64, target.Columns), make([]bool, target.Columns)
		}
		return func(line int) {
			y, z := line%target.Rows, line/target.Rows
			if mapper != nil {
				mapRow(sm, target, mapper, y, z, coords, inside)
				coverage.setMask(y, z, inside)
				if !filled {
					return
				}
				for x := range buffer {
					buffer[x] = 0
					u, v, s := coords[x][0], coords[x][1], coords[x][2]
					if inside[x] && u >= evalLo[0] && u < evalHi[0] && v >= evalLo[1] && v < evalHi[1] && s >= evalLo[2] && s < evalHi[2] {
						buffer[x] = trilinear(src, sm, box, u, v, s)
					}
				}
				emit(y, z, 0, buffer)
				return
			}
			l := rows.line(y, z)
			cx0, cx1 := coveredRun(sm, l, target.Columns)
			coverage.setRow(y, z, cx0, cx1)
			if !filled || cx1 <= cx0 {
				return
			}
			r0, r1 := l.columnRange(target.Columns, evalLo, evalHi)
			r0, r1 = max(r0, cx0), min(r1, cx1)
			if r1 <= r0 {
				return
			}
			values := buffer[:r1-r0]
			for x := r0; x < r1; x++ {
				u, v, s := l.at(x)
				value := 0.0
				if u >= evalLo[0] && u < evalHi[0] && v >= evalLo[1] && v < evalHi[1] && s >= evalLo[2] && s < evalHi[2] {
					value = trilinear(src, sm, box, u, v, s)
				}
				values[x-r0] = value
			}
			emit(y, z, r0, values)
		}
	})
	if err != nil {
		return ResampleCoverage{}, err
	}
	coverage.finish()
	return coverage.ResampleCoverage, nil
}

// trilinear interpolates between the eight voxel centres around (u, v, s),
// clamped to the grid; voxels outside box read 0.
func trilinear(src FractionSource, sm *gridMap, box IndexBox, u, v, s float64) float64 {
	var i0, i1 [3]int
	var t [3]float64
	for axis, c := range [3]float64{u, v, s} {
		a := sm.axes[axis]
		ci := math.Min(math.Max(a.continuous(c), 0), float64(a.n-1))
		i0[axis] = int(math.Floor(ci))
		i1[axis] = min(i0[axis]+1, a.n-1)
		t[axis] = ci - float64(i0[axis])
	}
	at := func(x, y, z int) float64 {
		if !box.contains(x, y, z) {
			return 0
		}
		return src.FractionAt(x, y, z)
	}
	sum := 0.0
	for corner := 0; corner < 8; corner++ {
		weight := 1.0
		var idx [3]int
		for axis := 0; axis < 3; axis++ {
			if corner&(1<<axis) != 0 {
				idx[axis], weight = i1[axis], weight*t[axis]
			} else {
				idx[axis], weight = i0[axis], weight*(1-t[axis])
			}
		}
		if weight != 0 {
			sum += weight * at(idx[0], idx[1], idx[2])
		}
	}
	return sum
}
