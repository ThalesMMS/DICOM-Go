package imagefilter

// Median filtering of modality values (#1185).
//
// Median replaces each sample by the median of a box neighbourhood of
// (2rx+1)·(2ry+1)·(2rz+1) samples, the radius given in millimetres per axis
// and rounded to whole samples. The behaviour follows ITK's
// MedianImageFilter (rectangular neighbourhood, an odd sample count so the
// median is the element of rank ⌊n/2⌋, borders extended by replicating the
// edge sample as ITK's zero-flux Neumann boundary condition does); the code
// is written from the published algorithms and copies none of ITK's.
//
// Integer-valued data spanning at most 65536 values — stored CT, CR, XA and
// US values, and their modality values whenever the rescale slope is 1 —
// use Huang's sliding histogram (T. S. Huang, G. J. Yang and G. Y. Tang, "A
// fast two-dimensional median filtering algorithm", IEEE Trans. ASSP 27(1),
// 1979): along each x line the histogram of the box is updated by removing
// the column that leaves it and adding the one that enters, so a step costs
// 2·(2ry+1)·(2rz+1) updates instead of a sort of the whole box, and the
// median is tracked from the previous one with the count of samples below
// it. The histogram has two levels (256 coarse bins of 256 fine bins each,
// as in S. Perreault and P. Hébert, "Median Filtering in Constant Time",
// IEEE TIP 16(9), 2007), so a median that jumps across an edge skips empty
// coarse bins. Other data (fractional rescale slopes, PET, MR with a scale)
// select the median of each box with a quickselect. Both paths return one of
// the input samples, the exact median, so the result does not depend on the
// path, the worker count or the slab split.

import (
	"context"
	"fmt"
	"math"
)

// MedianOptions configures Median.
type MedianOptions struct {
	// RadiusMM is the half-width of the box neighbourhood in millimetres
	// along x, y and z. Each is converted to whole samples by rounding
	// RadiusMM/spacing to the nearest integer (halves up), so 0.5 mm on a
	// 1 mm grid is one sample (a 3×3 box in a slice). A zero radius, or an
	// axis of a single sample (the z axis of a 2-D image), is not filtered
	// across, so {r, r, 0} filters each slice of a volume on its own.
	RadiusMM [3]float64
	// MaxKernelSamples bounds the box; 1<<16 when zero.
	MaxKernelSamples int
	// Workers bounds the goroutines (0 selects the shared concurrency budget). The result does
	// not depend on the worker count.
	Workers int
}

// MedianRadiusSamples returns the box radius in samples along x, y and z
// that Median uses for opts on a grid of the given dimensions and spacing.
func MedianRadiusSamples(opts MedianOptions, dims [3]int, spacingMM [3]float64) ([3]int, error) {
	return radiusSamples("median", opts.RadiusMM, dims, spacingMM)
}

// Median returns src with every sample replaced by the median of its box
// neighbourhood, borders replicating the edge samples. It removes impulsive
// noise (salt and pepper, dead pixels, speckle spikes in XA, US and CR)
// that diffusion and the bilateral filter keep as edges. ctx is checked
// between lines; on cancellation ctx.Err() is returned and no result.
func Median(ctx context.Context, src *Volume, opts MedianOptions) (*Volume, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	if err := src.Validate(); err != nil {
		return nil, err
	}
	k, err := newMedianKernel(src.Dims, src.SpacingMM, opts)
	if err != nil {
		return nil, err
	}
	out := &Volume{Dims: src.Dims, SpacingMM: src.SpacingMM, Data: make([]float32, len(src.Data))}
	if err := k.run(ctx, wholeView(src), 0, src.Dims[2], out.Data); err != nil {
		return nil, err
	}
	return out, nil
}

// medianKernel is a validated median request for one grid.
type medianKernel struct {
	dims    [3]int
	radius  [3]int
	workers int
	scratch scratchPool[medianScratch]
}

func newMedianKernel(dims [3]int, spacingMM [3]float64, opts MedianOptions) (*medianKernel, error) {
	radius, err := MedianRadiusSamples(opts, dims, spacingMM)
	if err != nil {
		return nil, err
	}
	limit := opts.MaxKernelSamples
	if limit == 0 {
		limit = 1 << 16
	}
	if limit < 0 {
		return nil, fmt.Errorf("%w: median kernel bound %d", ErrInvalidVolume, limit)
	}
	samples := 1
	for _, r := range radius {
		samples *= 2*r + 1
		if samples > limit {
			return nil, fmt.Errorf("%w: the median box exceeds %d samples", ErrInvalidVolume, limit)
		}
	}
	return &medianKernel{dims: dims, radius: radius, workers: opts.Workers}, nil
}

func (opts MedianOptions) slabKernel(dims [3]int, spacingMM [3]float64) (slabKernel, error) {
	return newMedianKernel(dims, spacingMM, opts)
}

// halo is the number of slices read beyond each side of the output slices.
func (k *medianKernel) halo() int { return k.radius[2] }

// scratchBytes bounds the per-worker row offsets and histogram (or box).
func (k *medianKernel) scratchBytes(slices int) int64 {
	columnSamples := (2*k.radius[1] + 1) * (2*k.radius[2] + 1)
	box := int64(2*k.radius[0]+1) * int64(columnSamples) * 4
	perWorker := int64(columnSamples)*8 + max(box, (medianHistogramBins+medianHistogramBins>>8)*4)
	return int64(min(workerBound(k.workers), max(1, slices*k.dims[1]))) * perWorker
}

// medianHistogramBins is the widest value span of the histogram path.
const medianHistogramBins = 1 << 16

// run writes the median of the output slices [z0, z1) into dst, which holds
// those slices. The view must hold every slice within the halo of them that
// lies in the volume.
func (k *medianKernel) run(ctx context.Context, in slabView, z0, z1 int, dst []float32) error {
	nx, ny := k.dims[0], k.dims[1]
	lines := (z1 - z0) * ny
	lo, span, integral := integerSpan(in.data)
	histogram := integral && span <= medianHistogramBins
	rx, ry, rz := k.radius[0], k.radius[1], k.radius[2]
	columnSamples := (2*ry + 1) * (2*rz + 1)
	boxSamples := (2*rx + 1) * columnSamples
	rank := boxSamples / 2
	return runWithScratch(ctx, lines, workerCount(k.workers, lines), &k.scratch, func(s *medianScratch, line int) {
		if len(s.rows) != columnSamples {
			s.rows = make([]int, columnSamples)
		}
		y, z := line%ny, z0+line/ny
		// The row offsets of the column of samples at one x, clamped.
		i := 0
		for dz := -rz; dz <= rz; dz++ {
			cz := clampAxis(z+dz, k.dims[2])
			for dy := -ry; dy <= ry; dy++ {
				s.rows[i] = in.row(clampAxis(y+dy, ny), cz)
				i++
			}
		}
		out := dst[line*nx : (line+1)*nx]
		if histogram {
			if s.histogram == nil || len(s.histogram.fine) < span {
				s.histogram = newMedianHistogram(span)
			}
			s.histogram.line(in.data, s.rows, out, rx, lo, rank)
			return
		}
		if len(s.box) != boxSamples {
			s.box = make([]float32, boxSamples)
		}
		for x := 0; x < nx; x++ {
			n := 0
			for dx := -rx; dx <= rx; dx++ {
				cx := clampAxis(x+dx, nx)
				for _, r := range s.rows {
					s.box[n] = in.data[r+cx]
					n++
				}
			}
			out[x] = selectRank(s.box, rank)
		}
	})
}

// medianScratch is one worker's row offsets and histogram or box.
type medianScratch struct {
	rows      []int
	histogram *medianHistogram
	box       []float32
}

// integerSpan returns the smallest value and the number of distinct integer
// values from it to the largest one, and whether every value is an integer
// exactly representable in float32 (|v| < 2²⁴), so v − lo indexes a
// histogram without rounding.
func integerSpan(data []float32) (lo float32, span int, integral bool) {
	if len(data) == 0 {
		return 0, 0, false
	}
	lo, hi := data[0], data[0]
	for _, v := range data {
		if v != float32(math.Trunc(float64(v))) || v >= 1<<24 || v <= -(1<<24) {
			return 0, 0, false
		}
		lo, hi = min(lo, v), max(hi, v)
	}
	return lo, int(hi-lo) + 1, true
}

// medianHistogram is the two-level sliding histogram of one worker: fine
// bins of single values and coarse bins of 256 values, the current median
// bin and the number of samples below it. Between lines it is empty.
type medianHistogram struct {
	fine   []int32
	coarse []int32
	med    int
	below  int
}

func newMedianHistogram(span int) *medianHistogram {
	coarse := (span + 255) >> 8
	return &medianHistogram{fine: make([]int32, coarse<<8), coarse: make([]int32, coarse)}
}

// column adds (delta = 1) or removes (delta = -1) the samples of one column.
func (h *medianHistogram) column(data []float32, rows []int, cx int, lo float32, delta int32) {
	for _, r := range rows {
		b := int(data[r+cx] - lo)
		h.fine[b] += delta
		h.coarse[b>>8] += delta
		if b < h.med {
			h.below += int(delta)
		}
	}
}

// median moves the median bin until rank lies within it: below ≤ rank <
// below + fine[med].
//
// At the start of a coarse bin the walk moves by whole coarse bins while
// the median lies beyond them, so it crosses at most 255 fine bins in each
// of the coarse bins it starts and ends in.
func (h *medianHistogram) median(rank int) int {
	for h.below > rank {
		if h.med&255 == 0 {
			// Every sample of the coarse bin below lies below med.
			if b := h.med>>8 - 1; h.below-int(h.coarse[b]) > rank {
				h.below -= int(h.coarse[b])
				h.med -= 256
				continue
			}
		}
		h.med--
		h.below -= int(h.fine[h.med])
	}
	for {
		if h.med&255 == 0 {
			if c := int(h.coarse[h.med>>8]); h.below+c <= rank {
				h.below += c
				h.med += 256
				continue
			}
		}
		f := int(h.fine[h.med])
		if h.below+f > rank {
			return h.med
		}
		h.below += f
		h.med++
	}
}

// line filters one x line: the box of x is the columns x−rx … x+rx (clamped)
// of the rows; the histogram slides one column per sample and is emptied
// again at the end.
func (h *medianHistogram) line(data []float32, rows []int, out []float32, rx int, lo float32, rank int) {
	nx := len(out)
	for dx := -rx; dx <= rx; dx++ {
		h.column(data, rows, clampAxis(dx, nx), lo, 1)
	}
	for x := 0; x < nx; x++ {
		out[x] = lo + float32(h.median(rank))
		if x+1 < nx {
			h.column(data, rows, clampAxis(x-rx, nx), lo, -1)
			h.column(data, rows, clampAxis(x+rx+1, nx), lo, 1)
		}
	}
	for dx := -rx; dx <= rx; dx++ {
		h.column(data, rows, clampAxis(nx-1+dx, nx), lo, -1)
	}
	h.med, h.below = 0, 0
}

// selectRank returns the element of the given rank (0 = smallest) of v,
// reordering v (Hoare's quickselect with a median-of-three pivot).
func selectRank(v []float32, rank int) float32 {
	lo, hi := 0, len(v)-1
	for lo < hi {
		mid := lo + (hi-lo)/2
		if v[mid] < v[lo] {
			v[mid], v[lo] = v[lo], v[mid]
		}
		if v[hi] < v[lo] {
			v[hi], v[lo] = v[lo], v[hi]
		}
		if v[hi] < v[mid] {
			v[hi], v[mid] = v[mid], v[hi]
		}
		pivot := v[mid]
		i, j := lo, hi
		for i <= j {
			for v[i] < pivot {
				i++
			}
			for pivot < v[j] {
				j--
			}
			if i <= j {
				v[i], v[j] = v[j], v[i]
				i++
				j--
			}
		}
		switch {
		case rank <= j:
			hi = j
		case rank >= i:
			lo = i
		default:
			return v[rank]
		}
	}
	return v[rank]
}
