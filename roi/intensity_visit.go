package roi

import (
	"context"
	"fmt"
	"math"
)

// IntensityVisit emits borrowed scalar samples synchronously. Emit errors must
// terminate the traversal. The visitor and its inputs remain caller-owned.
type IntensityVisit func(emit func(float64) error) error

// IntensityMomentsVisit computes the existing finite-value moments without a
// sample buffer. NaN and infinities contribute only to NonFinite.
func IntensityMomentsVisit(ctx context.Context, visit IntensityVisit) (IntensityMoments, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	var a IntensityAccumulator
	if err := ctx.Err(); err != nil {
		return a.Moments(), err
	}
	if visit == nil {
		return a.Moments(), fmt.Errorf("roi: nil intensity visitor")
	}
	err := visit(func(v float64) error {
		if err := ctx.Err(); err != nil {
			return err
		}
		a.Add(v)
		return nil
	})
	if err == nil {
		err = ctx.Err()
	}
	return a.Moments(), err
}

// IntensityStatisticsVisit owns a single bounded finite-value buffer. The
// declared maximum is checked against maxBytes BEFORE allocating or visiting.
// Exact type-7 percentiles use the same interpolation as NewIntensityStatistics;
// that constructor's copying contract remains unchanged. Sorting is cancellable.
func IntensityStatisticsVisit(ctx context.Context, maximum int, maxBytes int64, visit IntensityVisit) (IntensityStatistics, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	if maximum < 0 || maxBytes < 0 || int64(maximum) > maxBytes/8 || visit == nil {
		return IntensityStatistics{}, fmt.Errorf("roi: intensity sample limit exceeded or invalid visitor")
	}
	if err := ctx.Err(); err != nil {
		return IntensityStatistics{}, err
	}
	values := make([]float64, 0, maximum)
	var a IntensityAccumulator
	var refusal error
	err := visit(func(v float64) error {
		if refusal != nil {
			return refusal
		}
		if e := ctx.Err(); e != nil {
			refusal = e
			return e
		}
		if !math.IsNaN(v) && !math.IsInf(v, 0) {
			if len(values) == maximum {
				refusal = fmt.Errorf("roi: intensity sample limit exceeded")
				return refusal
			}
			values = append(values, v)
		}
		a.Add(v)
		return nil
	})
	if err == nil {
		err = refusal
	}
	if err == nil {
		err = ctx.Err()
	}
	if err != nil {
		return IntensityStatistics{}, err
	}
	if err = cancellableIntensitySort(ctx, values); err != nil {
		return IntensityStatistics{}, err
	}
	m := a.Moments()
	order := &intensityOrder{values: values}
	order.once.Do(func() {})
	s := IntensityStatistics{Count: m.Count, NonFinite: m.NonFinite, Min: m.Min, Max: m.Max, Sum: m.Sum, Mean: m.Mean, Variance: m.Variance, StandardDeviation: m.StandardDeviation, Median: math.NaN(), order: order}
	if median, ok := s.Percentile(50); ok {
		s.Median = median
	}
	return s, nil
}

// In-place heapsort requires no second sample buffer and checks cancellation
// within bounded work, including heap construction. It sorts only finite values.
func cancellableIntensitySort(ctx context.Context, v []float64) error {
	steps := 0
	sift := func(root, end int) error {
		for root*2+1 < end {
			steps++
			if steps%1024 == 0 {
				if err := ctx.Err(); err != nil {
					return err
				}
			}
			child := root*2 + 1
			if child+1 < end && v[child] < v[child+1] {
				child++
			}
			if v[root] >= v[child] {
				break
			}
			v[root], v[child] = v[child], v[root]
			root = child
		}
		return nil
	}
	for start := len(v)/2 - 1; start >= 0; start-- {
		if err := sift(start, len(v)); err != nil {
			return err
		}
	}
	for end := len(v) - 1; end > 0; end-- {
		v[0], v[end] = v[end], v[0]
		if err := sift(0, end); err != nil {
			return err
		}
	}
	return ctx.Err()
}

// Summary returns only mask count and physical volume, without shape run lists,
// principal axes, surfaces or radiomics. Coordinates use ShapeStatistics' voxel
// boundary convention, including irregular slice spacing.
func (m *VolumeMask) Summary(ctx context.Context) (count int, volume float64, err error) {
	if ctx == nil {
		ctx = context.Background()
	}
	if err = m.validateContext(ctx); err != nil {
		return
	}
	for i, on := range m.Voxels {
		if i%4096 == 0 {
			if err = ctx.Err(); err != nil {
				return
			}
		}
		if on {
			count++
			volume += m.voxelVolume(i)
		}
	}
	return
}

// TextureFeaturesBounded admits a conservative workspace upper bound before
// calling the existing texture kernel. maxVoxels bounds traversal/collection;
// maxBytes bounds collection growth, dense grid, all directional matrices and
// feature scratch. This is a logical allocation bound, not measured process RSS.
func (m *VolumeMask) TextureFeaturesBounded(ctx context.Context, valueAt func(int, int, int) (float64, bool), opts TextureOptions, maxVoxels int, maxBytes int64) (TextureFeatures, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	if err := m.validateContext(ctx); err != nil {
		return TextureFeatures{}, err
	}
	opts, err := opts.normalized()
	if err != nil {
		return TextureFeatures{}, err
	}
	n := m.Count()
	run := max(m.Columns, m.Rows, m.Slices)
	// Arithmetic in floats is admission-only; reject equality near overflow rather
	// than permitting an integer product to wrap. Actual kernels stay unchanged.
	bytes := float64(n)*128 + float64(len(m.Voxels))*4 + 14*8*(float64(opts.MaxGreyLevels)*float64(opts.MaxGreyLevels)+float64(opts.MaxGreyLevels)*float64(run)) + 4096*float64(opts.MaxGreyLevels+run+1)
	if maxVoxels <= 0 || n > maxVoxels || maxBytes <= 0 || bytes > float64(maxBytes) {
		return TextureFeatures{}, fmt.Errorf("roi: texture workspace limit exceeded")
	}
	if err := ctx.Err(); err != nil {
		return TextureFeatures{}, err
	}
	return m.TextureFeatures(ctx, valueAt, opts)
}

// SampleStorageBytes reports the logical capacity of the private float64
// collection retained by this result, including capacity beyond finite count.
// It excludes object/runtime overhead and is not resident-set memory.
func (s IntensityStatistics) SampleStorageBytes() int64 {
	if s.order == nil {
		return 0
	}
	return int64(cap(s.order.values)) * 8
}

// ShapeWorkspaceBytes bounds the existing shape kernel's logical scratch using
// full mask dimensions (a conservative bound on any sparse bounding box), not
// just foreground count. It includes dense Feret y/z extrema planes, Crofton
// row indices, growing run/point/tree storage and coordinate/feature scratch.
// Borrowed input mask storage and process/runtime overhead are separate.
func (m *VolumeMask) ShapeWorkspaceBytes(ctx context.Context) (int64, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	if err := m.validateContext(ctx); err != nil {
		return 0, err
	}
	runs := uint64(0)
	previous := false
	for i, on := range m.Voxels {
		if i%4096 == 0 {
			if err := ctx.Err(); err != nil {
				return 0, err
			}
		}
		if on && (!previous || i%m.Columns == 0) {
			runs++
		}
		previous = on
	}
	total := uint64(16384)
	add := func(value uint64) bool {
		if value > uint64(math.MaxInt64)-total {
			return false
		}
		total += value
		return true
	}
	mul := func(a, b, c uint64) bool {
		if a != 0 && b > uint64(math.MaxInt64)/a {
			return false
		}
		v := a * b
		if c != 0 && v > uint64(math.MaxInt64)/c {
			return false
		}
		return add(v * c)
	}
	nx, ny, nz := uint64(m.Columns), uint64(m.Rows), uint64(m.Slices)
	// 8 bytes conservatively accounts for int on supported native targets.
	if !mul(nx, nz, 8) || !mul(nx, ny, 8) || !mul(ny, nz, 8) || !mul(runs, 1, 1024) || !mul(nx+ny+nz+3, 1, 64) {
		return 0, fmt.Errorf("roi: shape workspace size overflow")
	}
	return int64(total), nil
}
