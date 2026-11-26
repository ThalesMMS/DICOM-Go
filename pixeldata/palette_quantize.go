package pixeldata

import (
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"math"

	"github.com/ThalesMMS/dicom-go/core"
	"github.com/ThalesMMS/dicom-go/internal/derivedio"
	"github.com/ThalesMMS/dicom-go/object"
	"github.com/ThalesMMS/dicom-go/pixeldata/display"
)

// ErrPaletteQuantize reports an unsupported input or invalid quantizer option.
var ErrPaletteQuantize = errors.New("dicom: palette quantization")

// ErrPaletteQuantizeQuality reports that the measured RGB error exceeds MaxRMSE.
var ErrPaletteQuantizeQuality = errors.New("dicom: palette quantization error exceeds limit")

// PaletteQuantizeOptions controls RGB8 quantization. Zero values select 256
// colors, no dithering, a limit of 16 million pixels, and no quality rejection.
type PaletteQuantizeOptions struct {
	// MaxColors is in [1,256]. All frames share one palette.
	MaxColors int
	// Dither enables serpentine Floyd–Steinberg error diffusion. Error does
	// not cross frame boundaries. Dithering can increase per-pixel RGB error.
	Dither bool
	// MaxPixels bounds the total number of pixels in all input frames.
	MaxPixels int64
	// MaxRMSE, if positive, rejects a result whose RGB8 channel RMSE is
	// larger. The metric is in stored sample units [0,255], not perceptual
	// or color-managed units. NaN, infinity and negative values are invalid.
	MaxRMSE float64
}

// PaletteQuantization owns a fresh Image Pixel Module and its Pixel Data.
// It contains no SOP identity, patient/study metadata, ICC profile or derivation
// attributes from the source. The caller must select a compatible IOD and
// record the new instance's identity and derivation when building a file.
type PaletteQuantization struct {
	ImagePixel       *object.Object
	Colors           int
	Frames           int
	RMSE             float64
	MaxAbsoluteError uint8
}

// QuantizePaletteColor quantizes native, unsigned RGB8 (8/8/7, SamplesPerPixel
// 3, PlanarConfiguration 0 or 1) to unsigned 8-bit PALETTE COLOR indices. It
// emits direct 16-bit RGB LUT entries (RGB8 * 257) through the LUT encoder.
// The palette is global across frames; only the complete Pixel Data value is
// padded to even length. The source is read-only and must not be mutated while
// this call is running. The returned object shares no source value buffers.
//
// This builds pixel attributes, not a conversion of an arbitrary SOP Instance:
// for example, Multi-frame True Color Secondary Capture does not admit PALETTE
// COLOR. Encapsulated, YBR, signed and higher-depth input must be converted to
// native RGB8 before calling. Big-endian OW Pixel Data must first be normalized
// to OB or little-endian OW; byte-oriented OB is independent of byte order.
// A nil context means context.Background().
func QuantizePaletteColor(ctx context.Context, source *object.Object, opts PaletteQuantizeOptions) (*PaletteQuantization, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if source == nil {
		return nil, fmt.Errorf("%w: source is nil", ErrPaletteQuantize)
	}
	if opts.MaxColors == 0 {
		opts.MaxColors = 256
	}
	if opts.MaxPixels == 0 {
		opts.MaxPixels = 16_000_000
	}
	if opts.MaxColors < 1 || opts.MaxColors > 256 || opts.MaxPixels < 1 || opts.MaxRMSE < 0 || math.IsNaN(opts.MaxRMSE) || math.IsInf(opts.MaxRMSE, 0) {
		return nil, fmt.Errorf("%w: invalid options", ErrPaletteQuantize)
	}
	m, err := ExtractMetadata(source)
	if err != nil {
		return nil, err
	}
	if m.PhotometricInterpretation != "RGB" || m.SamplesPerPixel != 3 || m.BitsAllocated != 8 || m.BitsStored != 8 || m.HighBit != 7 || m.PixelRepresentation != 0 || !m.PlanarConfigurationPresent || m.PlanarConfiguration > 1 || m.Rows == 0 || m.Columns == 0 || m.NumberOfFrames < 1 {
		return nil, fmt.Errorf("%w: expected native unsigned RGB8 with planar configuration 0 or 1", ErrPaletteQuantize)
	}
	pixelsPerFrame := int64(m.Rows) * int64(m.Columns)
	if int64(m.NumberOfFrames) > opts.MaxPixels/pixelsPerFrame || pixelsPerFrame > int64(int(^uint(0)>>1))/3 || int64(m.NumberOfFrames) > (int64(int(^uint(0)>>1))-1)/pixelsPerFrame {
		return nil, fmt.Errorf("%w: pixel count exceeds limit", ErrPaletteQuantize)
	}
	if pixel, ok := source.Get(core.TagPixelData); ok && pixel.VR() == core.VROW && source.ValueByteOrder() == binary.BigEndian {
		return nil, fmt.Errorf("%w: big-endian OW RGB8 requires byte-order normalization", ErrPaletteQuantize)
	}
	native, err := ExtractNativeFramesView(source)
	if err != nil {
		return nil, err
	}
	palette, err := paletteQuantizeBuild(ctx, native, opts.MaxColors)
	if err != nil {
		return nil, err
	}
	indices, squaredError, maxError, err := paletteQuantizeMap(ctx, native, palette, opts.Dither)
	if err != nil {
		return nil, err
	}
	rmse := math.Sqrt(squaredError / (float64(pixelsPerFrame) * float64(m.NumberOfFrames) * 3))
	if opts.MaxRMSE > 0 && rmse > opts.MaxRMSE {
		return nil, fmt.Errorf("%w: RMSE %.6f > %.6f", ErrPaletteQuantizeQuality, rmse, opts.MaxRMSE)
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if len(indices)%2 != 0 {
		indices = append(indices, 0)
	}
	out := derivedio.Object(
		derivedio.US(tagRows, m.Rows), derivedio.US(tagColumns, m.Columns),
		derivedio.US(tagSamplesPerPixel, 1), derivedio.CS(tagPhotometricInterpretation, "PALETTE COLOR"),
		derivedio.US(tagBitsAllocated, 8), derivedio.US(tagBitsStored, 8),
		derivedio.US(tagHighBit, 7), derivedio.US(tagPixelRepresentation, 0),
		core.NewRawElement(core.TagPixelData, core.VROB, indices),
	)
	if m.NumberOfFrames > 1 {
		out.Put(derivedio.IS(tagNumberOfFrames, m.NumberOfFrames))
	}
	for channel := 0; channel < 3; channel++ {
		entries := make([]uint16, len(palette))
		for i, rgb := range palette {
			entries[i] = uint16(rgb[channel]) * 257
		}
		out.Put(derivedio.US(paletteDescriptorTags[channel], uint16(len(palette)), 0, 16))
		out.Put(core.NewRawElement(paletteDataTags[channel], core.VROW, display.EncodeLUTData(entries, 16)))
	}
	return &PaletteQuantization{ImagePixel: out, Colors: len(palette), Frames: m.NumberOfFrames, RMSE: rmse, MaxAbsoluteError: maxError}, nil
}
