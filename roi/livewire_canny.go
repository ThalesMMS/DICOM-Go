package roi

import (
	"context"
	"fmt"
	"math"

	"github.com/ThalesMMS/dicom-go/imagefilter"
	"github.com/ThalesMMS/dicom-go/internal/parallel"
)

// CannyLivewireOptions selects a bounded Canny edge cost and existing path
// weights. Spacing must be explicitly positive finite; values are rescaled
// intensity and Canny thresholds are rescaled intensity/mm. Zero limits use
// Canny defaults. No unavailable/padding samples or fallback are supported.
type CannyLivewireOptions struct {
	Canny   imagefilter.CannyParameters
	Path    LivewireOptions
	Limits  imagefilter.CannyLimits
	Workers int
}

// NewCannyLivewireCostMap detects thin linked edges and assigns normalized
// retained-strength cost 1-strength/maxStrength. Non-edge pixels cost 1.
// Value is sampled only after source/scratch admission; caller data is never
// retained. No edges, unavailable data and cancellation return nil/error.
func NewCannyLivewireCostMap(ctx context.Context, columns, rows int, value func(int, int) (float64, bool), o CannyLivewireOptions) (*LivewireCostMap, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	if e := ctx.Err(); e != nil {
		return nil, e
	}
	if columns < 2 || rows < 2 || value == nil || o.Workers < 0 {
		return nil, fmt.Errorf("%w: Canny source/workers", ErrLivewire)
	}
	l, e := o.Limits.Normalize()
	if e != nil {
		return nil, e
	}
	if columns > l.MaxVoxels/rows {
		return nil, fmt.Errorf("%w: livewire source", imagefilter.ErrCannyResourceLimit)
	}
	p, e := o.Canny.Normalize(2)
	if e != nil {
		return nil, e
	}
	path := o.Path
	if !finiteNonNegative(path.ImageWeight) || !finiteNonNegative(path.CurvatureWeight) || math.IsNaN(path.LengthWeight) || math.IsInf(path.LengthWeight, 0) || !(path.ColSpacing > 0 && path.ColSpacing <= math.MaxFloat64) || !(path.RowSpacing > 0 && path.RowSpacing <= math.MaxFloat64) {
		return nil, fmt.Errorf("%w: finite Canny weights/spacing required", ErrLivewire)
	}
	if path.ImageWeight == 0 {
		path.ImageWeight = 1
	}
	if path.LengthWeight == 0 {
		path.LengthWeight = .1
	}
	n := columns * rows
	workers := min(parallel.Workers(o.Workers), parallel.DefaultWorkers())
	bytes, e := imagefilter.CannyWorkingBytes([3]int{columns, rows, 1}, workers)
	if e != nil {
		return nil, e
	}
	// Include callback materialization and subsequent cost storage in the same
	// operation's conservative reservation before either allocation.
	if bytes+int64(n)*12 > l.MaxBytes {
		return nil, fmt.Errorf("%w: livewire source/filter/cost", imagefilter.ErrCannyResourceLimit)
	}
	src := &imagefilter.Volume{Dims: [3]int{columns, rows, 1}, SpacingMM: [3]float64{path.ColSpacing, path.RowSpacing, 0}, Data: make([]float32, n)}
	for i := range src.Data {
		if i%4096 == 0 {
			if e := ctx.Err(); e != nil {
				return nil, e
			}
		}
		v, ok := value(i%columns, i/columns)
		if !ok {
			if e := ctx.Err(); e != nil {
				return nil, e
			}
			return nil, ErrCannyDomain
		}
		if math.IsNaN(v) || math.IsInf(v, 0) || math.Abs(v) > math.MaxFloat32 {
			return nil, fmt.Errorf("%w: non-finite Canny value", ErrLivewire)
		}
		src.Data[i] = float32(v)
	}
	r, e := imagefilter.Canny(ctx, src, imagefilter.CannyOptions{CannyParameters: p, Workers: workers, Limits: l})
	if e != nil {
		return nil, e
	}
	if r.EdgeCount == 0 {
		return nil, ErrCannyNoEdges
	}
	maximum := float32(0)
	for i, v := range r.Strength.Data {
		if i%4096 == 0 {
			if e := ctx.Err(); e != nil {
				return nil, e
			}
		}
		maximum = max(maximum, v)
	}
	cost := make([]float64, n)
	for i, v := range r.Strength.Data {
		if i%4096 == 0 {
			if e := ctx.Err(); e != nil {
				return nil, e
			}
		}
		cost[i] = 1 - float64(v)/float64(maximum)
	}
	if e := ctx.Err(); e != nil {
		return nil, e
	}
	return &LivewireCostMap{Columns: columns, Rows: rows, cost: cost, opts: path, maxBytes: l.MaxBytes}, nil
}

// cannyTreeAdmission covers owned costs, parent/distance/done arrays, the
// maximum lazy-queue capacity (eight relaxations per settled state plus
// initial directions), two trace-back arrays and transient queue growth.
func (c *LivewireCostMap) cannyTreeAdmission(states, directions int) (int, error) {
	if states > 16<<20 || states > math.MaxInt32 || states < 1 {
		return 0, fmt.Errorf("%w: livewire states", imagefilter.ErrCannyResourceLimit)
	}
	pushes := int64(states)*8 + int64(directions)
	// A growth holds its old and new arrays until copy completes: <1.5
	// times the finite queue capacity. 24 bytes per possible lazy item.
	bytes := int64(len(c.cost))*8 + int64(states)*(13+48) + pushes*24 + 4096
	if pushes > math.MaxInt || bytes > c.maxBytes {
		return 0, fmt.Errorf("%w: livewire tree reserves %d bytes", imagefilter.ErrCannyResourceLimit, bytes)
	}
	return int(pushes), nil
}
