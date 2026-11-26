package registration

import (
	"errors"
	"fmt"
	"math"
	"sync"

	"github.com/ThalesMMS/dicom-go/internal/parallel"
	"github.com/ThalesMMS/dicom-go/render"
)

// ErrTooFewSamples reports a metric evaluated where too few fixed points map
// inside the moving image (and its mask) to give a value.
var ErrTooFewSamples = errors.New("registration: too few samples overlap")

// MetricDomain chooses the fixed points a metric compares, as ITK's
// ImageToImageMetricv4 does with the fixed image as virtual domain.
type MetricDomain struct {
	// FixedMask and MovingMask, when set, restrict the comparison to fixed
	// points whose nearest mask sample is non-zero, and whose mapped point's
	// nearest moving-mask sample is non-zero. A mask has its own geometry.
	FixedMask, MovingMask *Image
	// Points are sampled fixed points in physical coordinates; nil
	// compares every fixed sample.
	Points []render.Vec3
	// Workers bounds the goroutines; zero uses the module's shared
	// concurrency budget (GOMAXPROCS by default). Results do not
	// depend on it: the points are split in fixed chunks reduced in order.
	Workers int
}

// sample is one fixed point that maps inside the moving image.
type sample struct {
	fixedPoint render.Vec3
	fixed      float64
	moving     float64
	// gradient is ∂moving/∂(mapped point), set when derivatives are needed.
	gradient render.Vec3
}

// metricChunk is the number of fixed points of one unit of work; chunks are
// reduced in index order, so a result does not depend on the workers.
const metricChunk = 4096

// evaluator walks the domain for one transform.
type evaluator struct {
	fixed, moving *Image
	domain        MetricDomain
}

func newEvaluator(fixed, moving *Image, domain MetricDomain) (*evaluator, error) {
	if fixed == nil || moving == nil {
		return nil, fmt.Errorf("%w: nil fixed or moving image", ErrImage)
	}
	return &evaluator{fixed: fixed, moving: moving, domain: domain}, nil
}

func (e *evaluator) pointCount() int {
	if e.domain.Points != nil {
		return len(e.domain.Points)
	}
	return len(e.fixed.data)
}

// fixedSample returns fixed point i and its value; false when outside the
// fixed image or its mask.
func (e *evaluator) fixedSample(i int) (render.Vec3, float64, bool) {
	var point render.Vec3
	var value float64
	if e.domain.Points != nil {
		point = e.domain.Points[i]
		v, ok := e.fixed.Interpolate(point)
		if !ok {
			return point, 0, false
		}
		value = v
	} else {
		size := e.fixed.size
		x, y, z := i%size[0], i/size[0]%size[1], i/(size[0]*size[1])
		point = e.fixed.Point(render.Vec3{X: float64(x), Y: float64(y), Z: float64(z)})
		value = float64(e.fixed.data[i])
	}
	if e.domain.FixedMask != nil && !e.domain.FixedMask.insideMask(point) {
		return point, 0, false
	}
	return point, value, true
}

// reduceChunks evaluates the samples of every chunk with visit, in
// parallel, and passes the results to merge in chunk order, as they become
// available: the reduction is the same for any number of workers, and at
// most a few chunk results per worker (maxInFlight in all, when positive)
// are held at once.
func reduceChunks[R any](e *evaluator, transform Transform, gradients bool, maxInFlight int, visit func(chunk []sample) R, merge func(R)) {
	reduceMetricChunks(e.pointCount(), e.domain.Workers, maxInFlight, func() func(int, int) R {
		buffer := make([]sample, 0, metricChunk)
		return func(start, end int) R {
			buffer = buffer[:0]
			for i := start; i < end; i++ {
				if s, ok := e.sampleAt(i, transform, gradients); ok {
					buffer = append(buffer, s)
				}
			}
			return visit(buffer)
		}
	}, merge)
}

// reduceMetricChunks visits fixed point-index ranges with private worker state.
// Chunk boundaries and merge order stay independent of the worker budget.
// At most maxInFlight chunk results (2 per worker when maxInFlight <= 0) are
// started and not yet merged at once, and no more workers than that run, so
// a caller whose results are large bounds their total memory with it.
func reduceMetricChunks[R any](count, requestedWorkers, maxInFlight int, newVisitor func() func(int, int) R, merge func(R)) {
	if count == 0 {
		return
	}
	chunks := (count-1)/metricChunk + 1
	want := min(parallel.Workers(requestedWorkers), chunks)
	if maxInFlight > 0 {
		want = min(want, maxInFlight)
	}
	workers, release := parallel.Reserve(want)
	defer release()
	inFlightLimit := 2 * workers
	if maxInFlight > 0 {
		inFlightLimit = min(inFlightLimit, maxInFlight)
	}
	rangeEnd := func(chunk int) int {
		start := chunk * metricChunk
		return start + min(metricChunk, count-start)
	}
	if workers == 1 {
		visit := newVisitor()
		for chunk := 0; chunk < chunks; chunk++ {
			merge(visit(chunk*metricChunk, rangeEnd(chunk)))
		}
		return
	}
	// The calling goroutine only merges, so its slot of the module's shared
	// concurrency budget goes to one of the evaluating goroutines.
	type result struct {
		chunk int
		value R
	}
	next := make(chan int)
	done := make(chan result, workers)
	// inFlight bounds the chunks started but not yet merged.
	inFlight := make(chan struct{}, inFlightLimit)
	var wg sync.WaitGroup
	for w := 0; w < workers; w++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			visit := newVisitor()
			for chunk := range next {
				done <- result{chunk: chunk, value: visit(chunk*metricChunk, rangeEnd(chunk))}
			}
		}()
	}
	go func() {
		for chunk := 0; chunk < chunks; chunk++ {
			inFlight <- struct{}{}
			next <- chunk
		}
		close(next)
		wg.Wait()
		close(done)
	}()
	pending := map[int]R{}
	merged := 0
	for r := range done {
		pending[r.chunk] = r.value
		for {
			value, ok := pending[merged]
			if !ok {
				break
			}
			delete(pending, merged)
			merge(value)
			merged++
			<-inFlight
		}
	}
}

// sampleAt maps fixed point i; false when it falls outside the moving image
// or a mask.
func (e *evaluator) sampleAt(i int, transform Transform, gradients bool) (sample, bool) {
	point, fixed, ok := e.fixedSample(i)
	if !ok {
		return sample{}, false
	}
	mapped := transform.TransformPoint(point)
	index := e.moving.ContinuousIndex(mapped)
	if !e.moving.inside(index) {
		return sample{}, false
	}
	if e.domain.MovingMask != nil && !e.domain.MovingMask.insideMask(mapped) {
		return sample{}, false
	}
	s := sample{fixedPoint: point, fixed: fixed}
	if gradients {
		s.moving, s.gradient = e.moving.linearWithGradientAt(index)
	} else {
		s.moving = e.moving.linearAt(index)
	}
	return s, true
}

// insideMask reports whether the nearest sample to a physical point is
// non-zero.
func (im *Image) insideMask(point render.Vec3) bool {
	index := im.ContinuousIndex(point)
	var at [3]int
	for axis, value := range [3]float64{index.X, index.Y, index.Z} {
		shifted := value + 0.5
		if !(shifted >= 0 && shifted < float64(im.size[axis])) {
			return false
		}
		at[axis] = int(shifted)
	}
	return im.At(at[0], at[1], at[2]) != 0
}

// linearWithGradientAt uses one cell for the value and the six interpolated
// face values required by the gradient. At sample planes and in the clamped
// margins, gradientAt handles the adjacent cells and flat extrapolation.
func (im *Image) linearWithGradientAt(index render.Vec3) (float64, render.Vec3) {
	x, y, z := int(index.X), int(index.Y), int(index.Z)
	tx, ty, tz := index.X-float64(x), index.Y-float64(y), index.Z-float64(z)
	// Extremely small fractions can underflow a corner weight to zero.
	// Leave those to the general sampler, which skips zero-weight corners.
	const minFraction = 0x1p-256
	if !(tx > minFraction && ty > minFraction && tz > minFraction) ||
		x >= im.size[0]-1 || y >= im.size[1]-1 || z >= im.size[2]-1 {
		return im.linearAt(index), im.gradientAt(index)
	}
	row := im.size[0]
	plane := row * im.size[1]
	base := x + y*row + z*plane
	// Corner order is x fastest, then y, then z, as in linearAt.
	a, b := float64(im.data[base]), float64(im.data[base+1])
	c, d := float64(im.data[base+row]), float64(im.data[base+row+1])
	e, f := float64(im.data[base+plane]), float64(im.data[base+plane+1])
	g, h := float64(im.data[base+plane+row]), float64(im.data[base+plane+row+1])
	sx, sy, sz := 1-tx, 1-ty, 1-tz
	xy00, xy10, xy01, xy11 := sx*sy, tx*sy, sx*ty, tx*ty
	value := xy00*sz*a + xy10*sz*b + xy01*sz*c + xy11*sz*d +
		xy00*tz*e + xy10*tz*f + xy01*tz*g + xy11*tz*h
	yz00, yz10, yz01, yz11 := sy*sz, ty*sz, sy*tz, ty*tz
	xz00, xz10, xz01, xz11 := sx*sz, tx*sz, sx*tz, tx*tz
	dx := (yz00*b + yz10*d + yz01*f + yz11*h) - (yz00*a + yz10*c + yz01*e + yz11*g)
	dy := (xz00*c + xz10*d + xz01*g + xz11*h) - (xz00*a + xz10*b + xz01*e + xz11*f)
	dz := (xy00*e + xy10*f + xy01*g + xy11*h) - (xy00*a + xy10*b + xy01*c + xy11*d)
	return value, im.physicalToIndex.Transpose().MulVec(render.Vec3{X: dx, Y: dy, Z: dz})
}

// gradientAt is the physical gradient at a continuous index inside the
// buffer: the derivative of the linear interpolation the metrics compare,
// so that their derivatives are exact. Along each index axis it is the
// slope of the cell the index falls in (zero beyond the outer samples,
// where the interpolation clamps, and along a single-sample axis); on a
// sample plane, where the slope changes, it is the mean of the slopes
// either side, the central difference over one sample.
func (im *Image) gradientAt(index render.Vec3) render.Vec3 {
	var perIndex [3]float64
	position := [3]float64{index.X, index.Y, index.Z}
	at := func(p [3]float64) float64 { return im.linearAt(render.Vec3{X: p[0], Y: p[1], Z: p[2]}) }
	for axis := 0; axis < 3; axis++ {
		last := float64(im.size[axis] - 1)
		value := position[axis]
		if im.size[axis] == 1 || value < 0 || value > last {
			continue
		}
		low, high := position, position
		if floor := math.Floor(value); floor == value {
			// On a sample plane: the mean of the neighbouring cell slopes
			// (one of them flat at the border).
			low[axis], high[axis] = value-1, value+1
			perIndex[axis] = (at(high) - at(low)) / 2
			continue
		} else {
			low[axis], high[axis] = floor, floor+1
		}
		perIndex[axis] = at(high) - at(low)
	}
	// ∂m/∂x = (∂i/∂x)ᵀ ∂m/∂i.
	return im.physicalToIndex.Transpose().MulVec(render.Vec3{X: perIndex[0], Y: perIndex[1], Z: perIndex[2]})
}

// accumulateDerivative adds weight·(∇m · ∂T/∂p) to derivative for a sample.
func accumulateDerivative(derivative []float64, jacobian *Jacobian, transform Transform, s sample, weight float64) {
	transform.JacobianWithRespectToParameters(s.fixedPoint, jacobian)
	for p := range derivative {
		derivative[p] += weight * (s.gradient.X*jacobian.At(0, p) + s.gradient.Y*jacobian.At(1, p) + s.gradient.Z*jacobian.At(2, p))
	}
}
