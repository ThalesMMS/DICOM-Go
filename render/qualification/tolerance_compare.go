package qualification

import (
	"bytes"
	"fmt"
	"image"
	"image/png"
	"math"
	"os"
	"path/filepath"
)

// MaxToleranceRadius bounds the neighbourhood searched per pixel.
const MaxToleranceRadius = 8

// RGBAToleranceV1 configures a neighbourhood-tolerant frame comparison,
// after ITK's Testing::ComparisonImageFilter: a test pixel matches when some
// baseline pixel within ToleranceRadius (a square neighbourhood) differs by
// at most DifferenceThreshold in every channel. A radius of 1 absorbs edges
// that moved by one pixel, as rasterization and interpolation differences
// between backends do. Pixels whose neighbourhood leaves the frame are
// skipped when IgnoreBoundaryPixels is set, and otherwise search the part of
// the neighbourhood inside the frame.
type RGBAToleranceV1 struct {
	ToleranceRadius      int      `json:"tolerance_radius"`
	DifferenceThreshold  [4]uint8 `json:"difference_threshold"`
	IgnoreBoundaryPixels bool     `json:"ignore_boundary_pixels"`
	// MaximumDifferentPixels is how many pixels may still differ.
	MaximumDifferentPixels uint64 `json:"maximum_different_pixels"`
}

// Validate checks the radius bounds.
func (tolerance RGBAToleranceV1) Validate() error {
	if tolerance.ToleranceRadius < 0 || tolerance.ToleranceRadius > MaxToleranceRadius {
		return fmt.Errorf("%w: tolerance radius %d outside [0, %d]", ErrInvalidFrameMetrics, tolerance.ToleranceRadius, MaxToleranceRadius)
	}
	return nil
}

// RGBAToleranceResultV1 is the outcome of a tolerant comparison. A pixel's
// difference is the smallest, over its neighbourhood, of the largest channel
// difference; Difference holds it as a grey image (black where the pixel
// matched or was not compared).
type RGBAToleranceResultV1 struct {
	Width, Height     uint32
	ComparedPixels    uint64
	DifferentPixels   uint64
	MaximumDifference uint8
	// MeanDifference averages the differences of the different pixels.
	MeanDifference float64
	Difference     *image.Gray
}

// Pass reports whether at most tolerance.MaximumDifferentPixels differ.
func (result RGBAToleranceResultV1) Pass(tolerance RGBAToleranceV1) bool {
	return result.DifferentPixels <= tolerance.MaximumDifferentPixels
}

// CompareRGBA8WithTolerance compares tightly packed RGBA8 frames of
// width × height pixels (test against baseline) with a neighbourhood
// tolerance.
func CompareRGBA8WithTolerance(test, baseline []byte, width, height uint32, tolerance RGBAToleranceV1) (RGBAToleranceResultV1, error) {
	if err := tolerance.Validate(); err != nil {
		return RGBAToleranceResultV1{}, err
	}
	if width == 0 || height == 0 {
		return RGBAToleranceResultV1{}, fmt.Errorf("%w: zero frame extent", ErrInvalidFrameMetrics)
	}
	pixelCount := uint64(width) * uint64(height)
	if pixelCount > uint64(math.MaxInt)/4 {
		return RGBAToleranceResultV1{}, fmt.Errorf("%w: frame exceeds Go slice capacity", ErrInvalidFrameMetrics)
	}
	if want := int(pixelCount * 4); len(test) != want || len(baseline) != want {
		return RGBAToleranceResultV1{}, fmt.Errorf("%w: frame lengths %d/%d, want %d", ErrInvalidFrameMetrics, len(test), len(baseline), want)
	}
	w, h, r := int(width), int(height), tolerance.ToleranceRadius
	result := RGBAToleranceResultV1{Width: width, Height: height, Difference: image.NewGray(image.Rect(0, 0, w, h))}
	var sum float64
	for y := 0; y < h; y++ {
		for x := 0; x < w; x++ {
			if tolerance.IgnoreBoundaryPixels && (x < r || y < r || x >= w-r || y >= h-r) {
				continue
			}
			result.ComparedPixels++
			t := test[4*(y*w+x):]
			best := 256
			for ny := max(0, y-r); ny <= min(h-1, y+r) && best > 0; ny++ {
				for nx := max(0, x-r); nx <= min(w-1, x+r); nx++ {
					b := baseline[4*(ny*w+nx):]
					largest, within := 0, true
					for c := 0; c < 4; c++ {
						d := int(absoluteByteDifference(t[c], b[c]))
						largest = max(largest, d)
						if d > int(tolerance.DifferenceThreshold[c]) {
							within = false
						}
					}
					if within {
						best = 0
						break
					}
					best = min(best, largest)
				}
			}
			if best == 0 {
				continue
			}
			result.DifferentPixels++
			result.Difference.Pix[y*result.Difference.Stride+x] = uint8(best)
			result.MaximumDifference = max(result.MaximumDifference, uint8(best))
			sum += float64(best)
		}
	}
	if result.DifferentPixels > 0 {
		result.MeanDifference = sum / float64(result.DifferentPixels)
	}
	return result, nil
}

// CompareRGBA8WithBaselines compares test against each alternative baseline
// (a case may accept several correct renderings, like ITK's .1, .2 baseline
// files) and returns the result and index of the baseline with the fewest
// different pixels; the first passing baseline wins.
func CompareRGBA8WithBaselines(test []byte, baselines [][]byte, width, height uint32, tolerance RGBAToleranceV1) (RGBAToleranceResultV1, int, error) {
	if len(baselines) == 0 {
		return RGBAToleranceResultV1{}, -1, fmt.Errorf("%w: no baseline", ErrInvalidFrameMetrics)
	}
	var best RGBAToleranceResultV1
	bestIndex := -1
	for i, baseline := range baselines {
		result, err := CompareRGBA8WithTolerance(test, baseline, width, height, tolerance)
		if err != nil {
			return RGBAToleranceResultV1{}, -1, fmt.Errorf("baseline %d: %w", i, err)
		}
		if bestIndex < 0 || result.DifferentPixels < best.DifferentPixels {
			best, bestIndex = result, i
		}
		if result.Pass(tolerance) {
			break
		}
	}
	return best, bestIndex, nil
}

// WriteComparisonPNGs writes name.test.png, name.baseline.png and
// name.diff.png (the difference image, stretched so the largest difference
// is white) into directory, for inspecting a failed comparison, and returns
// their paths.
func WriteComparisonPNGs(directory, name string, test, baseline []byte, result RGBAToleranceResultV1) ([]string, error) {
	if err := os.MkdirAll(directory, 0o755); err != nil {
		return nil, err
	}
	w, h := int(result.Width), int(result.Height)
	frame := func(pix []byte) image.Image {
		return &image.NRGBA{Pix: pix, Stride: 4 * w, Rect: image.Rect(0, 0, w, h)}
	}
	diff := image.NewGray(image.Rect(0, 0, w, h))
	if result.Difference != nil && result.MaximumDifference > 0 {
		for i, v := range result.Difference.Pix {
			diff.Pix[i] = uint8((int(v)*255 + int(result.MaximumDifference)/2) / int(result.MaximumDifference))
		}
	}
	var paths []string
	for _, item := range []struct {
		suffix string
		img    image.Image
	}{{"test", frame(test)}, {"baseline", frame(baseline)}, {"diff", diff}} {
		var encoded bytes.Buffer
		if err := png.Encode(&encoded, item.img); err != nil {
			return paths, err
		}
		path := filepath.Join(directory, name+"."+item.suffix+".png")
		if err := os.WriteFile(path, encoded.Bytes(), 0o644); err != nil {
			return paths, err
		}
		paths = append(paths, path)
	}
	return paths, nil
}
