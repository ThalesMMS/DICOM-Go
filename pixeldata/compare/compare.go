// Package compare measures how far the pixel values of a test image lie from
// those of a reference image, to judge lossy exports and transcodes by
// metrics rather than by hash: maximum and mean absolute error, root mean
// square error, peak signal-to-noise ratio and signal-to-noise ratio.
//
// The definitions are those of R. C. Gonzalez and R. E. Woods, Digital Image
// Processing, as DCMTK dcmicmp reports them, so results can be checked
// against that tool:
//
//	MAE  = Σ|r - t| / n
//	RMSE = sqrt(Σ(r - t)² / n)
//	PSNR = 10 log10(max r² / (Σ(r - t)² / n))   (dB; +Inf for identical images)
//	SNR  = 10 log10(Σ r² / Σ(r - t)²)           (dB; +Inf for identical images)
//
// where r and t run over every sample of every frame and the peak is the
// largest squared reference value. Files compares grayscale images on their
// modality values (after the Modality LUT, as dcmicmp does without VOI
// windowing) or on their stored values, and color images on 8-bit RGB (as
// dcmicmp does) or on their stored samples.
package compare

import (
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"math"
	"strings"

	"github.com/ThalesMMS/dicom-go/object"
	"github.com/ThalesMMS/dicom-go/pixeldata"
	"github.com/ThalesMMS/dicom-go/pixeldata/builtin"
	"github.com/ThalesMMS/dicom-go/pixeldata/display"
)

// ErrMismatch reports images that cannot be compared sample for sample:
// different dimensions, frame counts, samples per pixel or color model.
var ErrMismatch = errors.New("dicom/compare: images differ in geometry or color model")

// Metrics are the comparison results. Errors are in the units of the
// compared values (modality units, stored values or 8-bit RGB levels).
type Metrics struct {
	Frames  int   `json:"frames"`
	Samples int64 `json:"samples"`
	// MaxAbsoluteError is the largest |r - t|.
	MaxAbsoluteError float64 `json:"max_absolute_error"`
	// MeanAbsoluteError (MAE) is the mean of |r - t|.
	MeanAbsoluteError float64 `json:"mean_absolute_error"`
	// RootMeanSquareError (RMSE) is the square root of the mean of (r - t)².
	RootMeanSquareError float64 `json:"root_mean_square_error"`
	// PeakSignalToNoiseRatio (PSNR, dB) relates the peak squared reference
	// value to the mean squared error; +Inf when the images are identical.
	PeakSignalToNoiseRatio float64 `json:"psnr_db"`
	// SignalToNoiseRatio (SNR, dB) relates the reference energy to the error
	// energy; +Inf when the images are identical.
	SignalToNoiseRatio float64 `json:"snr_db"`
}

// Identical reports that every sample matched.
func (m Metrics) Identical() bool { return m.MaxAbsoluteError == 0 }

// Accumulator gathers reference and test values one sample at a time. Its
// zero value is ready to use.
type Accumulator struct {
	samples                         int64
	sumAbs, sumSquare, sumReference float64
	maxAbs, peak                    float64
}

// Add accounts for one sample.
func (a *Accumulator) Add(reference, test float64) {
	diff := math.Abs(reference - test)
	a.samples++
	a.sumAbs += diff
	a.sumSquare += diff * diff
	a.sumReference += reference * reference
	a.maxAbs = math.Max(a.maxAbs, diff)
	a.peak = math.Max(a.peak, reference*reference)
}

// Metrics returns the metrics of the samples added so far (Frames is left
// to the caller).
func (a *Accumulator) Metrics() Metrics {
	m := Metrics{Samples: a.samples, MaxAbsoluteError: a.maxAbs}
	if a.samples == 0 {
		return m
	}
	n := float64(a.samples)
	mse := a.sumSquare / n
	m.MeanAbsoluteError = a.sumAbs / n
	m.RootMeanSquareError = math.Sqrt(mse)
	if a.sumSquare == 0 {
		m.PeakSignalToNoiseRatio, m.SignalToNoiseRatio = math.Inf(1), math.Inf(1)
		return m
	}
	m.PeakSignalToNoiseRatio = 10 * math.Log10(a.peak/mse)
	m.SignalToNoiseRatio = 10 * math.Log10(a.sumReference/a.sumSquare)
	return m
}

// Domain selects the values compared.
type Domain int

const (
	// DomainModality compares grayscale images after the Modality LUT or
	// rescale of each frame, and color images as 8-bit RGB (dcmicmp's
	// default, without VOI windowing).
	DomainModality Domain = iota
	// DomainStored compares the stored samples as decoded, without the
	// Modality LUT or color conversion (dcmicmp -M for grayscale).
	DomainStored
)

// Options configures Files.
type Options struct {
	Domain Domain
	// Registry decodes compressed Pixel Data; nil uses the built-in codecs.
	Registry pixeldata.Registry
}

// Files compares every frame of test with the same frame of reference.
// Compressed Pixel Data is decoded first; neither file is modified.
func Files(ctx context.Context, reference, test *object.File, opts Options) (Metrics, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	if reference == nil || test == nil {
		return Metrics{}, fmt.Errorf("%w: nil file", ErrMismatch)
	}
	registry := opts.Registry
	if registry == nil {
		var err error
		if registry, err = builtin.NewRegistry(); err != nil {
			return Metrics{}, err
		}
	}
	ref, err := decode(ctx, reference, registry)
	if err != nil {
		return Metrics{}, fmt.Errorf("reference: %w", err)
	}
	tst, err := decode(ctx, test, registry)
	if err != nil {
		return Metrics{}, fmt.Errorf("test: %w", err)
	}
	a, b := ref.frames.Metadata, tst.frames.Metadata
	if a.Rows != b.Rows || a.Columns != b.Columns || a.SamplesPerPixel != b.SamplesPerPixel ||
		len(ref.frames.Data) != len(tst.frames.Data) || ref.monochrome() != tst.monochrome() {
		return Metrics{}, fmt.Errorf("%w: %dx%dx%d with %d sample(s) %s against %dx%dx%d with %d sample(s) %s", ErrMismatch,
			a.Columns, a.Rows, len(ref.frames.Data), a.SamplesPerPixel, a.PhotometricInterpretation,
			b.Columns, b.Rows, len(tst.frames.Data), b.SamplesPerPixel, b.PhotometricInterpretation)
	}
	var acc Accumulator
	for frame := range ref.frames.Data {
		if err := ctx.Err(); err != nil {
			return Metrics{}, err
		}
		r, err := ref.values(ctx, frame, opts.Domain)
		if err != nil {
			return Metrics{}, fmt.Errorf("reference frame %d: %w", frame+1, err)
		}
		t, err := tst.values(ctx, frame, opts.Domain)
		if err != nil {
			return Metrics{}, fmt.Errorf("test frame %d: %w", frame+1, err)
		}
		if len(r) != len(t) {
			return Metrics{}, fmt.Errorf("%w: frame %d has %d and %d samples", ErrMismatch, frame+1, len(r), len(t))
		}
		for i := range r {
			acc.Add(r[i], t[i])
		}
	}
	m := acc.Metrics()
	m.Frames = len(ref.frames.Data)
	return m, nil
}

// image is a decoded file.
type image struct {
	dataset *object.Object
	frames  *pixeldata.NativeFrames
	order   binary.ByteOrder
}

func decode(ctx context.Context, file *object.File, registry pixeldata.Registry) (image, error) {
	native := file
	if file.TransferSyntax.Encapsulated || file.TransferSyntax.RequiresCodec() {
		decoded, err := pixeldata.DecompressFileContext(ctx, file, pixeldata.DecompressOptions{Registry: registry})
		if err != nil {
			return image{}, err
		}
		native = decoded
	}
	frames, err := pixeldata.ExtractNativeFramesView(native.Dataset)
	if err != nil {
		return image{}, err
	}
	order := native.TransferSyntax.ByteOrder
	if order == nil {
		order = binary.LittleEndian
	}
	return image{dataset: native.Dataset, frames: frames, order: order}, nil
}

func (im image) monochrome() bool {
	return strings.HasPrefix(strings.ToUpper(strings.TrimSpace(im.frames.Metadata.PhotometricInterpretation)), "MONOCHROME")
}

// values returns the compared values of one frame, sample by sample.
func (im image) values(ctx context.Context, frame int, domain Domain) ([]float64, error) {
	m := im.frames.Metadata
	data := im.frames.Data[frame]
	if domain == DomainStored {
		samples, err := pixeldata.StoredSamples[int64](ctx, data, m.SampleFormat(im.order), int(m.Rows)*int(m.Columns)*int(m.SamplesPerPixel))
		if err != nil {
			return nil, err
		}
		out := make([]float64, len(samples))
		for i, v := range samples {
			out[i] = float64(v)
		}
		return out, nil
	}
	format := display.PixelFormat{BitsAllocated: int(m.BitsAllocated), BitsStored: int(m.BitsStored), HighBit: int(m.HighBit),
		Signed: m.PixelRepresentation == 1, ByteOrder: im.order}
	if im.monochrome() {
		modality, err := display.ModalityLUTAt(im.dataset, frame)
		if err != nil {
			return nil, err
		}
		return display.DecodeModality(display.Frame{Rows: int(m.Rows), Columns: int(m.Columns), Pixels: data, Format: format, Modality: modality})
	}
	color := display.ColorFrame{Rows: int(m.Rows), Columns: int(m.Columns), Pixels: data, Photometric: strings.ToUpper(strings.TrimSpace(m.PhotometricInterpretation)),
		SamplesPerPixel: int(m.SamplesPerPixel), PlanarConfiguration: int(m.PlanarConfiguration), Format: format}
	if color.Photometric == "PALETTE COLOR" {
		palette, err := display.PaletteFromObject(im.dataset)
		if err != nil {
			return nil, err
		}
		color.Palette = palette
	}
	rgba, err := display.RenderColor(color)
	if err != nil {
		return nil, err
	}
	out := make([]float64, 0, 3*len(rgba.Pix)/4)
	for i := 0; i+3 < len(rgba.Pix); i += 4 {
		out = append(out, float64(rgba.Pix[i]), float64(rgba.Pix[i+1]), float64(rgba.Pix[i+2]))
	}
	return out, nil
}
