package roi

import (
	"image"
	"math"
	"sort"
)

// Stats describes the rescaled pixel-value distribution inside a 2D ROI.
// Histogram is optional and is populated by Stats2DWithHistogram when bins is
// greater than zero. Variance and StdDev use the population definition, which
// matches the values shown by medical-image ROI inspectors.
type Stats struct {
	Count  int
	Sum    float64
	Min    float64
	Max    float64
	Mean   float64
	Median float64

	Variance float64
	StdDev   float64
	Skewness float64
	Kurtosis float64

	Histogram         []int
	HistogramMin      float64
	HistogramBinWidth float64
}

// SummaryStats describes the finite rescaled-value distribution inside a 2D
// ROI without quantiles, higher moments, histogram bins, or retained samples.
// Variance and StdDev use the population definition, matching Stats.
type SummaryStats struct {
	Count int
	Sum   float64
	Min   float64
	Max   float64
	Mean  float64

	Variance float64
	StdDev   float64
}

func LengthMM(a, b image.Point, spacing MeasureSpacing) (float64, bool) {
	return LengthMMWith(a, b, spacing)
}

// AngleDeg returns the angle at vertex in pixel units (no spacing); see
// SubpixelAngleDeg to apply the pixel spacing.
func AngleDeg(a, vertex, b image.Point) float64 {
	return SubpixelAngleDeg(point2D(a), point2D(vertex), point2D(b), MeasureSpacing{})
}

func RectangleAreaMM2(a, b image.Point, spacing MeasureSpacing) (float64, bool) {
	return RectangleAreaMM2With(a, b, spacing)
}

func EllipseAreaMM2(bbox image.Rectangle, spacing MeasureSpacing) (float64, bool) {
	return EllipseAreaMM2With(bbox, spacing)
}

func PolygonLengthMM(pts []image.Point, spacing MeasureSpacing) (float64, bool) {
	return PolygonLengthMMWith(pts, spacing)
}

func PolygonAreaMM2(pts []image.Point, spacing MeasureSpacing) (float64, bool) {
	return PolygonAreaMM2With(pts, spacing)
}

// CobbAngleDeg returns the acute angle between two lines in pixel units.
// The result is NaN if either segment has zero length.
func CobbAngleDeg(l1a, l1b, l2a, l2b image.Point) float64 {
	return SubpixelCobbAngleDeg(point2D(l1a), point2D(l1b), point2D(l2a), point2D(l2b), MeasureSpacing{})
}

// DeviationMM returns the distance in mm from point to the line through
// lineA and lineB.
func DeviationMM(lineA, lineB, point image.Point, spacing MeasureSpacing) (float64, bool) {
	return SubpixelDeviationMM(point2D(lineA), point2D(lineB), point2D(point), spacing)
}

func Stats2D(mask *RasterMask, valueAt func(x, y int) (float64, bool)) Stats {
	return Stats2DWithHistogram(mask, valueAt, 0)
}

// Stats2DSummary computes count, sum, range, mean, population variance, and
// standard deviation in one pass and O(1) auxiliary memory. It intentionally
// does not retain or sort individual samples; use Stats2D when an exact median
// or higher distribution moments are required.
func Stats2DSummary(mask *RasterMask, valueAt func(x, y int) (float64, bool)) SummaryStats {
	var summary SummaryStats
	if mask == nil || valueAt == nil {
		return summary
	}
	min, max := math.Inf(1), math.Inf(-1)
	var m2 float64
	mask.ForEachPixel(func(x, y int) {
		value, ok := valueAt(x, y)
		if !ok || math.IsNaN(value) || math.IsInf(value, 0) {
			return
		}
		previousCount := float64(summary.Count)
		summary.Count++
		count := float64(summary.Count)
		delta := value - summary.Mean
		deltaOverCount := delta / count
		m2 += delta * deltaOverCount * previousCount
		summary.Mean += deltaOverCount
		summary.Sum += value
		if value < min {
			min = value
		}
		if value > max {
			max = value
		}
	})
	if summary.Count == 0 {
		return SummaryStats{}
	}
	summary.Min = min
	summary.Max = max
	summary.Variance = m2 / float64(summary.Count)
	if summary.Variance < 0 {
		summary.Variance = 0
	}
	summary.StdDev = math.Sqrt(summary.Variance)
	return summary
}

// Stats2DWithHistogram computes distribution statistics over mask. bins > 0
// also creates an exact-range histogram. A 2D ROI is bounded to one image, so
// retaining its values for an exact median is preferable to approximating the
// median from histogram bins.
func Stats2DWithHistogram(mask *RasterMask, valueAt func(x, y int) (float64, bool), bins int) Stats {
	return stats2D(mask, valueAt, bins, 0, 0, false)
}

// Stats2DWithHistogramBounds computes the same distribution statistics as
// Stats2DWithHistogram while placing histogram samples in the explicit
// [histogramMin,histogramMax] display range. Values outside that range are
// clamped into the first/last bin. This is useful for medical-image histogram
// windows whose horizontal range follows the full modality range of the image
// rather than the smaller range found inside the ROI.
func Stats2DWithHistogramBounds(mask *RasterMask, valueAt func(x, y int) (float64, bool), bins int, histogramMin, histogramMax float64) Stats {
	if bins <= 0 || math.IsNaN(histogramMin) || math.IsNaN(histogramMax) || math.IsInf(histogramMin, 0) || math.IsInf(histogramMax, 0) || histogramMax <= histogramMin {
		return Stats2DWithHistogram(mask, valueAt, bins)
	}
	return stats2D(mask, valueAt, bins, histogramMin, histogramMax, true)
}

func stats2D(mask *RasterMask, valueAt func(x, y int) (float64, bool), bins int, histogramMin, histogramMax float64, useHistogramBounds bool) Stats {
	var stats Stats
	if mask == nil || valueAt == nil {
		return stats
	}
	min, max := math.Inf(1), math.Inf(-1)
	var moments centralMoments
	values := make([]float64, 0)
	mask.ForEachPixel(func(x, y int) {
		v, ok := valueAt(x, y)
		if !ok || math.IsNaN(v) || math.IsInf(v, 0) {
			return
		}
		moments.Add(v)
		stats.Sum += v
		values = append(values, v)
		if v < min {
			min = v
		}
		if v > max {
			max = v
		}
	})
	if moments.Count == 0 {
		return Stats{}
	}
	stats.Count = moments.Count
	stats.Min = min
	stats.Max = max
	stats.Mean = moments.Mean
	n := float64(stats.Count)
	stats.Variance = moments.M2 / n
	if stats.Variance < 0 {
		stats.Variance = 0
	}
	stats.StdDev = math.Sqrt(stats.Variance)
	if stats.StdDev > 0 {
		stats.Skewness = (moments.M3 / n) / math.Pow(stats.StdDev, 3)
		stats.Kurtosis = (moments.M4/n)/(stats.Variance*stats.Variance) - 3
	}

	sort.Float64s(values)
	middle := len(values) / 2
	if len(values)%2 == 0 {
		stats.Median = (values[middle-1] + values[middle]) / 2
	} else {
		stats.Median = values[middle]
	}

	if bins > 0 {
		stats.Histogram = make([]int, bins)
		stats.HistogramMin = min
		histogramRangeMax := max
		if useHistogramBounds {
			stats.HistogramMin = histogramMin
			histogramRangeMax = histogramMax
		}
		if histogramRangeMax == stats.HistogramMin {
			stats.HistogramBinWidth = 1
			stats.Histogram[0] = stats.Count
			return stats
		}
		stats.HistogramBinWidth = (histogramRangeMax - stats.HistogramMin) / float64(bins)
		for _, value := range values {
			bin := int((value - stats.HistogramMin) / stats.HistogramBinWidth)
			if bin >= bins {
				bin = bins - 1
			}
			if bin < 0 {
				bin = 0
			}
			stats.Histogram[bin]++
		}
	}
	return stats
}

func Stats3D(seg *Segmentation3D, valueAt func(x, y, slice int) (float64, bool), bins int) VolumeStats {
	return seg.Statistics(valueAt, bins)
}
