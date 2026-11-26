package render

import (
	"context"
	"encoding/binary"
	"image"
	"math"

	"github.com/ThalesMMS/dicom-go/internal/storedsample"
)

// AutoWindowStrategy selects how an automatic VOI window is derived from a
// frame's pixel values.
type AutoWindowStrategy int

const (
	// AutoWindowMinMax spans the smallest to the largest value (DCMTK +Wm).
	AutoWindowMinMax AutoWindowStrategy = iota
	// AutoWindowMinMaxIgnoreExtremes spans the values left after removing
	// every pixel equal to the minimum or the maximum, which are often
	// background or saturation (DCMTK +Wn).
	AutoWindowMinMaxIgnoreExtremes
	// AutoWindowHistogram drops HistogramIgnore of the pixels at each end of
	// the histogram (DCMTK +Wh).
	AutoWindowHistogram
)

// String names the strategy for menus and logs.
func (s AutoWindowStrategy) String() string {
	switch s {
	case AutoWindowMinMaxIgnoreExtremes:
		return "min-max-ignore-extremes"
	case AutoWindowHistogram:
		return "histogram"
	default:
		return "min-max"
	}
}

// AutoWindowOptions configures AutoWindowWith.
type AutoWindowOptions struct {
	Strategy AutoWindowStrategy
	// HistogramIgnore is the fraction of pixels (0..0.5) ignored at each end
	// by AutoWindowHistogram; 0.05 drops the darkest and brightest 5%.
	HistogramIgnore float64
	// Region, when not empty, restricts the pixels to a rectangle in image
	// coordinates (a region of interest, DCMTK +Wr with min-max).
	Region image.Rectangle
	// Include, when set, further restricts the pixels to those it accepts,
	// such as the inside of a drawn ROI.
	Include func(x, y int) bool
}

// AutoWindow computes a full-dynamic window from a frame's pixel values
// (AutoWindowMinMax): Pixel Padding values never count, and the window
// covers the minimum to the maximum modality value inclusive, so the LINEAR
// function maps them to black and white. It returns false when the frame has
// no readable single-sample pixels or a constant value.
func AutoWindow(slice *Frame) (WindowLevel, bool) {
	return AutoWindowWith(slice, AutoWindowOptions{})
}

// AutoWindowWith computes an automatic window with a strategy. Values are
// counted per stored value (one histogram bin per stored value) with Pixel
// Padding excluded, then converted through the rescale; for a slope of 1 the
// results equal DCMTK's dcm2img +Wm, +Wn, +Wh and +Wr on images without
// padding (DCMTK does not exclude padding).
func AutoWindowWith(slice *Frame, opts AutoWindowOptions) (WindowLevel, bool) {
	loaded, err := loadFrameContext(context.Background(), slice)
	if err != nil {
		return WindowLevel{}, false
	}
	slice = loaded
	histogram, ok := storedHistogram(slice, opts)
	if !ok {
		return WindowLevel{}, false
	}
	var lo, hi int64
	switch opts.Strategy {
	case AutoWindowMinMaxIgnoreExtremes:
		first, last := histogram.first()+1, histogram.last()-1
		for first <= last && histogram.count(first) == 0 {
			first++
		}
		for last >= first && histogram.count(last) == 0 {
			last--
		}
		if first > last {
			return WindowLevel{}, false
		}
		lo, hi = first, last
	case AutoWindowHistogram:
		ignore := opts.HistogramIgnore
		if math.IsNaN(ignore) || ignore < 0 {
			ignore = 0
		}
		if ignore > 0.5 {
			ignore = 0.5
		}
		threshold := int64(ignore * float64(histogram.total))
		// The low bound is the value after the ignored low tail, the high
		// bound the first value of the ignored high tail (the maximum + 1
		// when nothing is ignored), as DCMTK computes it.
		lo = histogram.first()
		if threshold > 0 {
			var sum int64
			for value := histogram.first(); value <= histogram.last(); value++ {
				sum += histogram.count(value)
				if sum >= threshold {
					lo = value + 1
					break
				}
			}
		}
		hi = histogram.last() + 1
		if threshold > 0 {
			var sum int64
			for value := histogram.last(); value >= histogram.first(); value-- {
				sum += histogram.count(value)
				if sum >= threshold {
					hi = value
					break
				}
			}
		}
	default:
		lo, hi = histogram.first(), histogram.last()
	}
	if lo >= hi {
		return WindowLevel{}, false
	}
	return rescaledWindow(lo, hi, slice.Rescale), true
}

// rescaledWindow is the LINEAR window covering stored values lo..hi
// inclusive: center (lo+hi+1)/2 and width hi-lo+1 in stored units, scaled to
// modality values.
func rescaledWindow(lo, hi int64, rescale Rescale) WindowLevel {
	slope, intercept := rescale.Slope, rescale.Intercept
	if slope == 0 || math.IsNaN(slope) || math.IsInf(slope, 0) {
		slope = 1
	}
	if math.IsNaN(intercept) || math.IsInf(intercept, 0) {
		intercept = 0
	}
	low, high := float64(lo)*slope+intercept, float64(hi)*slope+intercept
	if low > high {
		low, high = high, low
	}
	unit := math.Abs(slope)
	return WindowLevel{Center: (low + high + unit) / 2, Width: high - low + unit}
}

// storedValueHistogram counts stored values offset from base.
type storedValueHistogram struct {
	base   int64
	counts []int64
	lo, hi int64
	total  int64
}

func (h storedValueHistogram) first() int64 { return h.lo }
func (h storedValueHistogram) last() int64  { return h.hi }
func (h storedValueHistogram) count(value int64) int64 {
	index := value - h.base
	if index < 0 || index >= int64(len(h.counts)) {
		return 0
	}
	return h.counts[index]
}

// storedHistogram counts the frame's usable stored values: single-sample 8-
// or 16-bit pixels inside the region and include filter, without padding.
func storedHistogram(slice *Frame, opts AutoWindowOptions) (storedValueHistogram, bool) {
	var out storedValueHistogram
	if slice == nil || slice.Metadata.SamplesPerPixel != 1 {
		return out, false
	}
	columns, rows := int(slice.Metadata.Columns), int(slice.Metadata.Rows)
	bytesPerSample := int(slice.Metadata.BitsAllocated / 8)
	if columns <= 0 || rows <= 0 || (bytesPerSample != 1 && bytesPerSample != 2) {
		return out, false
	}
	order := slice.ByteOrder
	if order == nil {
		order = binary.LittleEndian
	}
	signed := slice.Metadata.PixelRepresentation != 0
	reader := storedsample.Legacy(int(slice.Metadata.BitsAllocated), int(slice.Metadata.BitsStored), int(slice.Metadata.HighBit), signed, order)
	out.base = 0
	if signed {
		out.base = -1 << (8*bytesPerSample - 1)
	}
	out.counts = make([]int64, 1<<(8*bytesPerSample))
	area := image.Rect(0, 0, columns, rows)
	if !opts.Region.Empty() {
		area = area.Intersect(opts.Region)
	}
	out.lo, out.hi = math.MaxInt64, math.MinInt64
	for y := area.Min.Y; y < area.Max.Y; y++ {
		for x := area.Min.X; x < area.Max.X; x++ {
			offset := (y*columns + x) * bytesPerSample
			if offset+bytesPerSample > len(slice.PixelBytes) {
				continue
			}
			if opts.Include != nil && !opts.Include(x, y) {
				continue
			}
			stored := reader.Value(slice.PixelBytes[offset:])
			if slice.PixelPadding != nil && slice.PixelPadding.Contains(stored) {
				continue
			}
			index := stored - out.base
			if index < 0 || index >= int64(len(out.counts)) {
				continue
			}
			out.counts[index]++
			out.total++
			out.lo, out.hi = min(out.lo, stored), max(out.hi, stored)
		}
	}
	return out, out.total > 0
}

// PixelValueRange returns the smallest and largest modality values of a
// frame's single-sample pixels, Pixel Padding excluded, for histogram axes
// and fill values. ok is false when no pixel is usable.
func PixelValueRange(slice *Frame) (lo, hi float64, ok bool) {
	loaded, err := loadFrameContext(context.Background(), slice)
	if err != nil {
		return 0, 0, false
	}
	slice = loaded
	histogram, ok := storedHistogram(slice, AutoWindowOptions{})
	if !ok {
		return 0, 0, false
	}
	slope, intercept := slice.Rescale.Slope, slice.Rescale.Intercept
	if slope == 0 || math.IsNaN(slope) || math.IsInf(slope, 0) {
		slope = 1
	}
	if math.IsNaN(intercept) || math.IsInf(intercept, 0) {
		intercept = 0
	}
	lo = float64(histogram.first())*slope + intercept
	hi = float64(histogram.last())*slope + intercept
	if lo > hi {
		lo, hi = hi, lo
	}
	return lo, hi, true
}
