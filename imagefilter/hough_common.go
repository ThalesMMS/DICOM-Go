package imagefilter

import (
	"context"
	"errors"
	"fmt"
	"image"
	"math"
)

var ErrInvalidHough2D = errors.New("imagefilter: invalid Hough 2D request")
var ErrHoughResourceLimit = errors.New("imagefilter: Hough 2D resource limit")

// HoughLimits bounds logical owned memory and work, excluding caller input and
// Go runtime overhead. Zero uses finite ceilings; overrides may only lower them.
// A work unit is one inspected sample, voting pair or accumulator-cell visit.
type HoughLimits struct {
	MaxPixels     int
	MaxBytes      int64
	MaxOperations uint64
}

// Hough2DOptions selects an explicit half-open source-index region. The input
// is a binary 0/1 edge Volume with one slice (for example CannyResult.Edges).
// Pixels are at (x*SpacingMM[0],y*SpacingMM[1]) in the caller's physical plane;
// patient origin/directions remain caller-owned. MinVotes is support, never a
// probability. Candidates are proposals only, not persisted ROIs.
type Hough2DOptions struct {
	Region        image.Rectangle
	Limits        HoughLimits
	MinVotes      uint32
	MaxCandidates int
}

// HoughReport records finite logical admission, not measured RSS or latency.
type HoughReport struct {
	WorkingBytes                   int64
	ReservedOperations, Operations uint64
	EdgePixels                     int
}

type houghWork struct {
	ctx    context.Context
	report HoughReport
	limit  uint64
}

func (w *houghWork) tick() error {
	w.report.Operations++
	if w.report.Operations > w.limit {
		return ErrHoughResourceLimit
	}
	if w.report.Operations&1023 == 0 {
		return w.ctx.Err()
	}
	return nil
}
func houghFinite(v float64) bool     { return !math.IsNaN(v) && !math.IsInf(v, 0) }
func houghInvalid(what string) error { return fmt.Errorf("%w: %s", ErrInvalidHough2D, what) }
func houghGrid(lo, hi, step float64, exclusive bool) (int, error) {
	if !houghFinite(lo) || !houghFinite(hi) || !(step > 0) || !houghFinite(step) || hi < lo || (exclusive && hi == lo) {
		return 0, houghInvalid("grid interval/step")
	}
	q := (hi - lo) / step
	if !houghFinite(q) || q > float64(1<<24) {
		return 0, ErrHoughResourceLimit
	}
	rounded := math.Round(q)
	if math.Abs(q-rounded) > 1e-8*math.Max(1, math.Min(10, math.Abs(q))) {
		return 0, houghInvalid("grid endpoints must align with step")
	}
	n := int(rounded) + 1
	if exclusive {
		n--
	}
	if n < 1 {
		return 0, houghInvalid("empty grid")
	}
	return n, nil
}
func houghMul(a, b uint64) (uint64, error) {
	if b != 0 && a > math.MaxUint64/b {
		return 0, ErrHoughResourceLimit
	}
	return a * b, nil
}
func houghSum(v ...uint64) (uint64, error) {
	var sum uint64
	for _, x := range v {
		if sum > math.MaxUint64-x {
			return 0, ErrHoughResourceLimit
		}
		sum += x
	}
	return sum, nil
}
func houghSetup(ctx context.Context, src *Volume, o Hough2DOptions, cells, scratch, angles int, ops uint64) (*houghWork, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if src == nil || src.Dims[2] != 1 {
		return nil, houghInvalid("requires one 2D slice")
	}
	n, ok := voxelCount(src.Dims)
	if !ok || len(src.Data) != n {
		return nil, houghInvalid("source dimensions/data")
	}
	l := o.Limits
	if l.MaxPixels < 0 || l.MaxPixels > 1<<20 || l.MaxBytes < 0 || l.MaxBytes > 64<<20 || l.MaxOperations > 100000000 {
		return nil, houghInvalid("limits may only lower finite ceilings")
	}
	if l.MaxPixels == 0 {
		l.MaxPixels = 1 << 20
	}
	if l.MaxBytes == 0 {
		l.MaxBytes = 64 << 20
	}
	if l.MaxOperations == 0 {
		l.MaxOperations = 100000000
	}
	if n > l.MaxPixels {
		return nil, ErrHoughResourceLimit
	}
	for a := 0; a < 2; a++ {
		s := src.SpacingMM[a]
		if !(s > 0) || !houghFinite(s) || !houghFinite(float64(src.Dims[a]-1)*s) {
			return nil, houghInvalid("physical spacing/extent")
		}
	}
	bounds := image.Rect(0, 0, src.Dims[0], src.Dims[1])
	if o.Region.Empty() || !o.Region.In(bounds) {
		return nil, houghInvalid("nonempty contained region required")
	}
	if o.MaxCandidates < 1 || o.MaxCandidates > 64 || o.MinVotes == 0 {
		return nil, houghInvalid("candidate cap/minimum votes")
	}
	if cells < 1 || scratch < 0 || angles < 1 || angles > 4096 {
		return nil, ErrHoughResourceLimit
	}
	bytes, err := houghSum(uint64(cells)*4, uint64(scratch)*4, uint64(angles)*16, uint64(o.MaxCandidates)*64, 4096)
	if err != nil || bytes > uint64(l.MaxBytes) || ops > l.MaxOperations {
		return nil, ErrHoughResourceLimit
	}
	w := &houghWork{ctx: ctx, limit: ops, report: HoughReport{WorkingBytes: int64(bytes), ReservedOperations: ops}}
	for _, v := range src.Data {
		if err := w.tick(); err != nil {
			return nil, err
		}
		if v != 0 && v != 1 {
			return nil, houghInvalid("binary edge mask required")
		}
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	return w, nil
}
func houghDimensions(src *Volume, o Hough2DOptions) (uint64, uint64, error) {
	if src == nil || src.Dims[2] != 1 {
		return 0, 0, houghInvalid("requires one 2D slice")
	}
	n, ok := voxelCount(src.Dims)
	if !ok || len(src.Data) != n {
		return 0, 0, houghInvalid("dimensions/data")
	}
	if n > 1<<20 {
		return 0, 0, ErrHoughResourceLimit
	}
	if o.MaxCandidates < 1 || o.MaxCandidates > 64 || o.Region.Empty() || !o.Region.In(image.Rect(0, 0, src.Dims[0], src.Dims[1])) {
		return 0, 0, houghInvalid("region/candidate cap")
	}
	return uint64(n), uint64(o.Region.Dx()) * uint64(o.Region.Dy()), nil
}
func houghPeak(w *houghWork, acc []uint32, min uint32) (int, error) {
	best := -1
	votes := uint32(0)
	for i, v := range acc {
		if err := w.tick(); err != nil {
			return -1, err
		}
		if v >= min && v > votes {
			best = i
			votes = v
		}
	}
	return best, nil
}
