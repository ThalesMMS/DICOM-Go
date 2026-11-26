package render

import (
	"context"
	"errors"
	"fmt"
	"image"
	"math"

	"github.com/ThalesMMS/dicom-go/internal/parallel"
	"github.com/ThalesMMS/dicom-go/pixeldata"
)

var (
	ErrSourceConvolutionKernel = errors.New("render: invalid source convolution kernel")
	ErrSourceConvolutionFrame  = errors.New("render: unsupported source convolution frame")
	ErrSourceConvolutionLimit  = errors.New("render: source convolution sample limit exceeded")
)

// MaxSourceConvolutionSamples bounds the input components admitted per frame.
// The preparation retains at most eight bytes per pixel for grayscale or
// four for RGB. It does not retain the input bytes.
const MaxSourceConvolutionSamples = 16 << 20

// SourceKernel3x3 describes a row-major correlation: weights apply in the
// given order, with no flip. Sum the weighted samples, divide by Divisor
// and add Bias, in stored sample units. Divisor must be nonzero. MidrangeBias
// also adds 2^(BitsStored-1) for unsigned frames, useful for zero-sum kernels
// whose negative responses would otherwise clip.
type SourceKernel3x3 struct {
	Weights       [9]float64
	Divisor, Bias float64
	MidrangeBias  bool
}

// ConvolvedFrame is an immutable, detached display result. Grayscale retains
// fractional modality values and maps a new VOI without another convolution.
// RGB has no grayscale VOI and retains the output of color presentation.
type ConvolvedFrame struct {
	size          image.Point
	values        []float64
	rgb           []byte
	invert        bool
	defaultWindow WindowLevel
}

// MemoryBytes reports the cacheable payload, including owned default VOI
// entries. It excludes the Go struct, the source and each new output raster.
func (f *ConvolvedFrame) MemoryBytes() int64 {
	if f == nil {
		return 0
	}
	bytes := int64(len(f.values))*8 + int64(len(f.rgb))
	if f.defaultWindow.LUT != nil {
		bytes += int64(len(f.defaultWindow.LUT.Entries)) * 2
	}
	return bytes
}

// PrepareFrameConvolution3x3Context convolves decoded MONOCHROME1/2 or RGB
// frames with 8- or 16-bit containers. RGB must be unsigned. Packed and planar
// RGB are both supported. A deferred Frame uses its context-aware loader.
// Source bytes and metadata are borrowed during preparation and never modified.
//
// At each edge, repeat the nearest sample. Padding samples participate in the
// convolution, and a grayscale center marked as padding by the original data
// remains black. Grayscale saturates once to the signed or unsigned 16-bit
// intermediate (independent of Bits Stored), then applies linear rescale.
// RGB saturates to the unsigned container range before its normal displayed
// range mapping. Fractional values survive division, even for signed scalars.
// No partial result is returned on cancellation or error.
func PrepareFrameConvolution3x3Context(ctx context.Context, frame *Frame, kernel SourceKernel3x3) (*ConvolvedFrame, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if err := kernel.check(); err != nil {
		return nil, err
	}
	if frame == nil {
		return nil, ErrSourceConvolutionFrame
	}
	if err := checkSourceConvolutionSize(frame.Metadata); err != nil {
		return nil, err
	}
	frame, err := loadFrameContext(ctx, frame)
	if err != nil {
		return nil, err
	}
	meta := effectiveFrameMetadata(frame)
	photo := normalizedPhotometric(meta.PhotometricInterpretation)
	rgb := meta.SamplesPerPixel == 3
	if (meta.BitsAllocated != 8 && meta.BitsAllocated != 16) || meta.PixelRepresentation > 1 {
		return nil, ErrSourceConvolutionFrame
	}
	if rgb {
		if photo != "RGB" || meta.PlanarConfiguration > 1 || meta.PixelRepresentation != 0 {
			return nil, ErrSourceConvolutionFrame
		}
	} else if photo != "MONOCHROME1" && photo != "MONOCHROME2" {
		return nil, ErrSourceConvolutionFrame
	}
	format := meta.SampleFormat(frame.ByteOrder)
	reader, err := format.Reader()
	if err != nil {
		return nil, fmt.Errorf("%w: %w", ErrSourceConvolutionFrame, err)
	}
	storedMin, storedMax, _ := format.Range()
	if kernel.MidrangeBias && !format.Signed {
		kernel.Bias += float64((storedMax + 1) / 2)
	}
	if !kernel.finiteRange(storedMin, storedMax) {
		return nil, ErrSourceConvolutionKernel
	}

	columns, rows, channels := int(meta.Columns), int(meta.Rows), int(meta.SamplesPerPixel)
	inputBytes := columns * rows * channels * reader.Width()
	if len(frame.PixelBytes) != inputBytes && (inputBytes%2 == 0 || len(frame.PixelBytes) != inputBytes+1) {
		return nil, fmt.Errorf("%w: expected %d source bytes, got %d", ErrSourceConvolutionFrame, inputBytes, len(frame.PixelBytes))
	}
	result := &ConvolvedFrame{size: image.Pt(columns, rows)}
	if rgb {
		result.rgb = make([]byte, columns*rows*4)
	} else {
		lo, hi := convolutionGrayDomain(format.Signed)
		slope, intercept := sanitizedRescale(frame)
		if !finite(lo*slope+intercept) || !finite(hi*slope+intercept) {
			return nil, ErrSourceConvolutionFrame
		}
		window, err := ownConvolutionWindow(frame.DefaultWindow)
		if err != nil {
			return nil, err
		}
		result.defaultWindow = window
		result.invert = photo == "MONOCHROME1"
		result.values = make([]float64, columns*rows)
	}
	finish := result.convolutionOutput(frame, format, kernel.Divisor, kernel.Bias, float64(storedMax))
	source := frame.PixelBytes[:inputBytes]
	planar := rgb && meta.PlanarConfiguration == 1
	// Bound rolling scratch to three rows per worker, with at most eight
	// workers sharing the same process helper budget as other renderers.
	workers := min(8, parallel.DefaultWorkers(), rows)
	if rows < parallelRowsMinRows {
		workers = 1
	}
	err = parallel.Run(ctx, rows, workers, parallelRowChunk(rows, workers), func() func(int) {
		lineLength := columns * channels
		buffer := make([]int32, 3*lineLength)
		rowIndices := [3]int{-1, -1, -1}
		row := func(y int) []int32 {
			slot := y % 3
			dest := buffer[slot*lineLength : (slot+1)*lineLength]
			if rowIndices[slot] != y {
				readConvolutionRow(dest, source, &reader, y, columns, rows, planar)
				rowIndices[slot] = y
			}
			return dest
		}
		return func(y int) {
			top, center, bottom := row(max(0, y-1)), row(y), row(min(rows-1, y+1))
			for x := 0; x < columns; x++ {
				if x%4096 == 0 && ctx.Err() != nil {
					return
				}
				left, middle, right := max(0, x-1)*channels, x*channels, min(columns-1, x+1)*channels
				pixel := y*columns + x
				if !rgb && frame.PixelPadding != nil && frame.PixelPadding.Contains(int64(center[middle])) {
					result.values[pixel] = math.NaN()
					continue
				}
				var values [3]float64
				for c := 0; c < channels; c++ {
					values[c] = correlateSourceSamples(kernel.Weights, top, center, bottom, left+c, middle+c, right+c)
				}
				finish(pixel, values)
			}
		}
	})
	if err != nil {
		return nil, err
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	return result, nil
}

func checkSourceConvolutionSize(meta pixeldata.Metadata) error {
	columns, rows, channels := int(meta.Columns), int(meta.Rows), int(meta.SamplesPerPixel)
	if columns <= 0 || rows <= 0 || (channels != 1 && channels != 3) {
		return ErrSourceConvolutionFrame
	}
	if rows > MaxSourceConvolutionSamples/(columns*channels) {
		return ErrSourceConvolutionLimit
	}
	return nil
}

func (k SourceKernel3x3) check() error {
	if !finite(k.Divisor) || k.Divisor == 0 || !finite(k.Bias) {
		return ErrSourceConvolutionKernel
	}
	for _, weight := range k.Weights {
		if !finite(weight) {
			return ErrSourceConvolutionKernel
		}
	}
	return nil
}

func (k SourceKernel3x3) finiteRange(lo, hi int64) bool {
	absWeight := 0.0
	for _, weight := range k.Weights {
		absWeight += math.Abs(weight)
	}
	absSum := absWeight * max(math.Abs(float64(lo)), math.Abs(float64(hi)))
	return finite(absSum) && finite(absSum/math.Abs(k.Divisor)+math.Abs(k.Bias))
}

func ownConvolutionWindow(window WindowLevel) (WindowLevel, error) {
	if window.LUT != nil {
		const maxVOIEntries = 65536
		if len(window.LUT.Entries) > maxVOIEntries {
			return WindowLevel{}, ErrSourceConvolutionLimit
		}
		lut := *window.LUT
		lut.Entries = append([]uint16(nil), lut.Entries...)
		lut.Adjustments = nil
		window.LUT = &lut
	}
	return window, nil
}

func readConvolutionRow(dest []int32, source []byte, reader *pixeldata.StoredSampleReader, y, columns, rows int, planar bool) {
	width := reader.Width()
	if planar {
		for c := 0; c < 3; c++ {
			start := (c*rows + y) * columns * width
			values := source[start : start+columns*width]
			for x := 0; x < columns; x++ {
				dest[3*x+c] = int32(reader.Value(values[x*width:]))
			}
		}
		return
	}
	offset := y * len(dest) * width
	values := source[offset : offset+len(dest)*width]
	for i := range dest {
		dest[i] = int32(reader.Value(values[i*width:]))
	}
}

func correlateSourceSamples(w [9]float64, top, center, bottom []int32, left, middle, right int) float64 {
	return float64(top[left])*w[0] + float64(top[middle])*w[1] + float64(top[right])*w[2] +
		float64(center[left])*w[3] + float64(center[middle])*w[4] + float64(center[right])*w[5] +
		float64(bottom[left])*w[6] + float64(bottom[middle])*w[7] + float64(bottom[right])*w[8]
}

func (f *ConvolvedFrame) convolutionOutput(frame *Frame, format pixeldata.SampleFormat, divisor, bias, rgbMax float64) func(int, [3]float64) {
	if f.rgb != nil {
		containerMax := float64((uint32(1) << uint(format.BitsAllocated)) - 1)
		convert := frame.ColorTransform
		return func(pixel int, sums [3]float64) {
			var normalized [3]float64
			for c, sum := range sums {
				sample := clampFloat(sum/divisor+bias, 0, containerMax)
				normalized[c] = min(1, sample/rgbMax)
			}
			if convert != nil && !convert.Identity() {
				normalized[0], normalized[1], normalized[2] = convert.Convert(normalized[0], normalized[1], normalized[2])
			}
			p := f.rgb[pixel*4 : pixel*4+4]
			for c, value := range normalized {
				p[c] = uint8(math.Round(255 * clampFloat(value, 0, 1)))
			}
			p[3] = 255
		}
	}
	slope, intercept := sanitizedRescale(frame)
	lo, hi := convolutionGrayDomain(format.Signed)
	return func(pixel int, sums [3]float64) {
		stored := clampFloat(sums[0]/divisor+bias, lo, hi)
		f.values[pixel] = stored*slope + intercept
	}
}

func convolutionGrayDomain(signed bool) (float64, float64) {
	if signed {
		return -32768, 32767
	}
	return 0, 65535
}

// RenderContext maps convolved grayscale modality values through the DICOM
// VOI window or LUT followed by MONOCHROME1 inversion. The returned image
// is independent of the cache entry. RGB renders independently of window.
func (f *ConvolvedFrame) RenderContext(ctx context.Context, window WindowLevel) (image.Image, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if f == nil || f.size.X <= 0 || f.size.Y <= 0 {
		return nil, ErrSourceConvolutionFrame
	}
	bounds := image.Rectangle{Max: f.size}
	if f.rgb != nil {
		output := image.NewRGBA(bounds)
		for y := 0; y < bounds.Dy(); y++ {
			if err := ctx.Err(); err != nil {
				return nil, err
			}
			start := y * output.Stride
			end := start + output.Stride
			copy(output.Pix[start:end], f.rgb[start:end])
		}
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		return output, nil
	}
	if window.LUT == nil && !validWindowWidth(window.Width) {
		window = f.defaultWindow
	}
	window = normalizeWindow(window, f.defaultWindow)
	mapper := prepareWindow(window)
	output := image.NewGray(bounds)
	err := parallelRowsContext(ctx, bounds.Dy(), func(y int) {
		offset := y * output.Stride
		line := output.Pix[offset : offset+output.Stride]
		for x, value := range f.values[offset : offset+output.Stride] {
			if math.IsNaN(value) {
				continue
			}
			g := windowedGrayMapped(value, mapper)
			if f.invert {
				g = 255 - g
			}
			line[x] = g
		}
	})
	if err != nil {
		return nil, err
	}
	return output, nil
}
