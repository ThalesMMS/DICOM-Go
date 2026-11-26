package display

import (
	"encoding/binary"
	"errors"
	"fmt"
	"image"
	"math"
	"strings"

	"github.com/ThalesMMS/dicom-go/transfer"
)

var (
	// ErrUnsupportedColorPhotometric reports a color Photometric Interpretation
	// the pipeline does not convert (e.g. YBR_ICT, YBR_RCT).
	ErrUnsupportedColorPhotometric = errors.New("dicom/display: unsupported color photometric interpretation")
	// ErrUnsupportedColorLayout reports a color pixel layout the pipeline cannot
	// interpret (e.g. signed or 32-bit RGB/YBR samples, odd-width 4:2:2, wrong samples
	// per pixel) so callers avoid producing misleading images.
	ErrUnsupportedColorLayout = errors.New("dicom/display: unsupported color pixel layout")
	// ErrMissingPalette reports a PALETTE COLOR frame without red/green/blue
	// palette LUTs.
	ErrMissingPalette = errors.New("dicom/display: PALETTE COLOR frame is missing palette LUTs")
	// ErrUnsupportedSegmentedPalette reports a segmented palette color LUT
	// construct outside the supported discrete/linear/indirect segment set.
	ErrUnsupportedSegmentedPalette = errors.New("dicom/display: unsupported segmented palette color LUT")
)

// PaletteColorLUT holds the red, green, and blue palette color lookup tables of
// a PALETTE COLOR image (PS3.3 C.7.6.3.1.5).
type PaletteColorLUT struct {
	Red, Green, Blue *LUT
}

// ColorFrame is the input to the color display pipeline: one frame's stored
// pixel bytes plus the attributes needed to convert them to RGB.
type ColorFrame struct {
	Rows, Columns       int
	Pixels              []byte
	Photometric         string
	SamplesPerPixel     int
	PlanarConfiguration int // 0 interleaved, 1 planar (color-by-plane)
	Format              PixelFormat
	// Palette is required for PALETTE COLOR and for positive retired ARGB
	// alpha indices; ARGB alpha is a palette override, not opacity.
	Palette *PaletteColorLUT
}

// RenderColor converts a color frame to an RGBA image. Supported photometric
// interpretations are RGB and YBR_FULL (interleaved or planar), YBR_FULL_422
// and YBR_PARTIAL_422 (4:2:2 subsampled, or full resolution once decoded),
// YBR_PARTIAL_420 (decoded full resolution), PALETTE COLOR, and retired
// ARGB with palette overrides (not transparency). RGB and YBR
// samples may be 8- or 16-bit containers with any Bits Stored up to 16; they
// are scaled to 8 bits (RenderColor16 keeps 16). Other layouts return a
// typed error rather than a misleading image.
func RenderColor(frame ColorFrame) (*image.RGBA, error) {
	if err := validateFrameDimensions(frame.Rows, frame.Columns); err != nil {
		return nil, err
	}
	if normalizePhotometric(frame.Photometric) == "ARGB" {
		return renderARGB(frame)
	}
	if normalizePhotometric(frame.Photometric) == "PALETTE COLOR" {
		return renderPalette(frame, frame.Rows, frame.Columns)
	}
	if out := copyRGB8(frame); out != nil {
		return out, nil
	}
	var out *image.RGBA
	var scale float64
	err := colorSamples(frame, func(maxValue float64) {
		out = image.NewRGBA(image.Rect(0, 0, frame.Columns, frame.Rows))
		scale = 255 / maxValue
	}, func(i int, r, g, b float64) {
		setRGBA(out, i, clampByte(r*scale), clampByte(g*scale), clampByte(b*scale))
	})
	if err != nil {
		return nil, err
	}
	return out, nil
}

// copyRGB8 copies interleaved 8-bit RGB with all 8 bits stored straight
// into a new image, the common case the general conversion would only
// reproduce; it returns nil for every other layout and for frames the
// general path must reject.
func copyRGB8(frame ColorFrame) *image.RGBA {
	format := frame.Format
	if normalizePhotometric(frame.Photometric) != "RGB" || frame.SamplesPerPixel != 3 || frame.PlanarConfiguration != 0 ||
		format.BitsAllocated != 8 || format.Signed || (format.BitsStored != 0 && format.BitsStored != 8) {
		return nil
	}
	layout, err := checkedFrameLayout(frame.Rows, frame.Columns, 3, 1, 4)
	if err != nil || len(frame.Pixels) < layout.sourceBytes {
		return nil
	}
	out := image.NewRGBA(image.Rect(0, 0, frame.Columns, frame.Rows))
	src, dst := frame.Pixels[:layout.sourceBytes], out.Pix
	for i := 0; i < layout.pixels; i++ {
		dst[4*i], dst[4*i+1], dst[4*i+2], dst[4*i+3] = src[3*i], src[3*i+1], src[3*i+2], 255
	}
	return out
}

// RenderColor16 is RenderColor with 16 bits per channel: RGB and YBR samples
// wider than 8 bits keep their precision (scaled to 0..65535). PALETTE COLOR
// uses the palette entries at 16 bits.
func RenderColor16(frame ColorFrame) (*image.RGBA64, error) {
	if err := validateFrameDimensions(frame.Rows, frame.Columns); err != nil {
		return nil, err
	}
	if normalizePhotometric(frame.Photometric) == "ARGB" {
		return renderARGB16(frame)
	}
	if normalizePhotometric(frame.Photometric) == "PALETTE COLOR" {
		out := image.NewRGBA64(image.Rect(0, 0, frame.Columns, frame.Rows))
		if err := renderPalette16(frame, out); err != nil {
			return nil, err
		}
		return out, nil
	}
	var out *image.RGBA64
	var scale float64
	err := colorSamples(frame, func(maxValue float64) {
		out = image.NewRGBA64(image.Rect(0, 0, frame.Columns, frame.Rows))
		scale = 65535 / maxValue
	}, func(i int, r, g, b float64) {
		dst := out.Pix[8*i : 8*i+8]
		for channel, v := range [3]float64{r, g, b} {
			value := uint16(math.Round(clampFloat(v*scale, 0, 65535)))
			dst[2*channel], dst[2*channel+1] = uint8(value>>8), uint8(value)
		}
		dst[6], dst[7] = 0xFF, 0xFF
	})
	if err != nil {
		return nil, err
	}
	return out, nil
}

// colorSamples converts an RGB or YBR frame to RGB triplets in 0..maxValue,
// the largest stored sample value (2^BitsStored - 1): it validates the
// frame, reports maxValue to setMax, then passes each pixel's triplet to
// emit, without materializing the converted image.
func colorSamples(frame ColorFrame, setMax func(maxValue float64), emit func(pixel int, r, g, b float64)) error {
	rows, cols := frame.Rows, frame.Columns
	var convert func(a, b, c, half float64) (float64, float64, float64)
	subsampled := false
	switch normalizePhotometric(frame.Photometric) {
	case "RGB":
		convert = rgbSamples
	case "YBR_FULL":
		convert = ybrFullSamples
	case "YBR_FULL_422":
		convert, subsampled = ybrFullSamples, true
	case "YBR_PARTIAL_422":
		convert, subsampled = ybrPartialSamples, true
	case "YBR_PARTIAL_420":
		// Only compressed (MPEG, HEVC) pixel data may be 4:2:0; decoders
		// return full-resolution samples.
		convert = ybrPartialSamples
	default:
		return frameValidationError("PhotometricInterpretation", ErrUnsupportedColorPhotometric)
	}
	if frame.SamplesPerPixel != 3 {
		return frameValidationError("SamplesPerPixel", ErrUnsupportedColorLayout)
	}
	bits := frame.Format.BitsAllocated
	if bits != 8 && bits != 16 {
		return frameValidationError("BitsAllocated", ErrUnsupportedColorLayout)
	}
	if frame.Format.Signed {
		return frameValidationError("PixelRepresentation", ErrUnsupportedColorLayout)
	}
	if frame.PlanarConfiguration != 0 && frame.PlanarConfiguration != 1 {
		return frameValidationError("PlanarConfiguration", ErrUnsupportedColorLayout)
	}
	bytesPerSample := bits / 8
	full, err := checkedFrameLayout(rows, cols, 3, bytesPerSample, 4)
	if err != nil {
		return err
	}
	// A 4:2:2 photometric with full-resolution data was upsampled by a
	// decoder; native 4:2:2 stores two samples per pixel.
	if subsampled && len(frame.Pixels) >= full.sourceBytes {
		subsampled = false
	}
	if subsampled && cols%2 != 0 {
		return frameValidationError("Columns", ErrUnsupportedColorLayout)
	}
	if subsampled && frame.PlanarConfiguration != 0 {
		return frameValidationError("PlanarConfiguration", ErrUnsupportedColorLayout)
	}
	sourceBytes := full.sourceBytes
	if subsampled {
		sourceBytes = full.pixels * 2 * bytesPerSample
	}
	if len(frame.Pixels) < sourceBytes {
		return frameValidationError("Pixels", ErrPixelDataTooShort)
	}
	order := frame.Format.ByteOrder
	if order == nil {
		order = binary.LittleEndian
	}
	format := frame.Format
	if format.BitsStored <= 0 || format.BitsStored > bits {
		format.BitsStored, format.HighBit = bits, bits-1
	}
	reader := newStoredPixelReader(format, order)
	sample := func(index int) float64 {
		return float64(reader.Value(frame.Pixels[index*bytesPerSample:]))
	}
	maxValue := math.Pow(2, float64(format.BitsStored)) - 1
	half := (maxValue + 1) / 2
	setMax(maxValue)
	if subsampled {
		for pair := 0; pair < full.pixels/2; pair++ {
			y0, y1, cb, cr := sample(4*pair), sample(4*pair+1), sample(4*pair+2), sample(4*pair+3)
			r, g, b := convert(y0, cb, cr, half)
			emit(2*pair, r, g, b)
			r, g, b = convert(y1, cb, cr, half)
			emit(2*pair+1, r, g, b)
		}
		return nil
	}
	planar := frame.PlanarConfiguration == 1
	for i := 0; i < full.pixels; i++ {
		var a, b, c float64
		if planar {
			a, b, c = sample(i), sample(full.pixels+i), sample(2*full.pixels+i)
		} else {
			a, b, c = sample(3*i), sample(3*i+1), sample(3*i+2)
		}
		r, g, bl := convert(a, b, c, half)
		emit(i, r, g, bl)
	}
	return nil
}

func rgbSamples(r, g, b, _ float64) (float64, float64, float64) { return r, g, b }

// ybrFullSamples converts full-range YCbCr (PS3.3 C.7.6.3.1.2) whose chroma
// is centered on half, the midpoint of the sample range.
func ybrFullSamples(y, cb, cr, half float64) (float64, float64, float64) {
	cb, cr = cb-half, cr-half
	return y + 1.402*cr, y - 0.344136*cb - 0.714136*cr, y + 1.772*cb
}

// ybrPartialSamples converts partial-range YCbCr (ITU-R BT.601: Y 16..235,
// chroma 16..240 at 8 bits, scaled to the sample depth) to full-range RGB.
func ybrPartialSamples(y, cb, cr, half float64) (float64, float64, float64) {
	scale := half / 128
	y = 1.164383 * (y - 16*scale)
	cb, cr = cb-half, cr-half
	return y + 1.596027*cr, y - 0.391762*cb - 0.812968*cr, y + 2.017232*cb
}

// ValidateColorPhotometric reports a color Photometric Interpretation that
// PS3.3 C.7.6.3.1.2 does not allow with the transfer syntax: YBR_PARTIAL_420
// is defined only for MPEG and HEVC pixel data, YBR_ICT and YBR_RCT only for
// JPEG 2000. YBR_PARTIAL_422 is retired but still rendered.
func ValidateColorPhotometric(photometric, transferSyntaxUID string) error {
	name := normalizePhotometric(photometric)
	switch name {
	case "YBR_PARTIAL_420":
		if !transfer.IsVideoTransferSyntax(transferSyntaxUID) {
			return fmt.Errorf("%w: YBR_PARTIAL_420 is valid only for MPEG or HEVC pixel data, not %s", ErrUnsupportedColorPhotometric, transferSyntaxUID)
		}
	case "YBR_ICT", "YBR_RCT":
		if !transfer.IsJPEG2000TransferSyntax(transferSyntaxUID) {
			return fmt.Errorf("%w: %s is valid only for JPEG 2000 pixel data, not %s", ErrUnsupportedColorPhotometric, name, transferSyntaxUID)
		}
	}
	return nil
}

// renderPalette converts a PALETTE COLOR frame to RGB by indexing the stored
// pixel value into the red/green/blue palette LUTs.
func renderPalette(frame ColorFrame, rows, cols int) (*image.RGBA, error) {
	if frame.Palette == nil || frame.Palette.Red == nil || frame.Palette.Green == nil || frame.Palette.Blue == nil {
		return nil, ErrMissingPalette
	}
	if frame.SamplesPerPixel != 1 {
		return nil, frameValidationError("SamplesPerPixel", ErrUnsupportedColorLayout)
	}
	bits := frame.Format.BitsAllocated
	if bits != 8 && bits != 16 {
		return nil, frameValidationError("BitsAllocated", ErrUnsupportedBitsAllocated)
	}
	bytesPerSample := bits / 8
	layout, err := checkedFrameLayout(rows, cols, 1, bytesPerSample, 4)
	if err != nil {
		return nil, err
	}
	if len(frame.Pixels) < layout.sourceBytes {
		return nil, frameValidationError("Pixels", ErrPixelDataTooShort)
	}
	order := frame.Format.ByteOrder
	if order == nil {
		order = binary.LittleEndian
	}

	out := image.NewRGBA(image.Rect(0, 0, cols, rows))
	reader := newStoredPixelReader(frame.Format, order)
	for i := 0; i < layout.pixels; i++ {
		stored := int(reader.Value(frame.Pixels[i*bytesPerSample:]))
		r := paletteChannel(frame.Palette.Red, stored)
		g := paletteChannel(frame.Palette.Green, stored)
		b := paletteChannel(frame.Palette.Blue, stored)
		setRGBA(out, i, r, g, b)
	}
	return out, nil
}

// renderPalette16 is renderPalette with 16-bit channels: 8-bit palette
// entries are replicated (e*257).
func renderPalette16(frame ColorFrame, out *image.RGBA64) error {
	if frame.Palette == nil || frame.Palette.Red == nil || frame.Palette.Green == nil || frame.Palette.Blue == nil {
		return ErrMissingPalette
	}
	if frame.SamplesPerPixel != 1 {
		return frameValidationError("SamplesPerPixel", ErrUnsupportedColorLayout)
	}
	bits := frame.Format.BitsAllocated
	if bits != 8 && bits != 16 {
		return frameValidationError("BitsAllocated", ErrUnsupportedBitsAllocated)
	}
	bytesPerSample := bits / 8
	layout, err := checkedFrameLayout(frame.Rows, frame.Columns, 1, bytesPerSample, 8)
	if err != nil {
		return err
	}
	if len(frame.Pixels) < layout.sourceBytes {
		return frameValidationError("Pixels", ErrPixelDataTooShort)
	}
	order := frame.Format.ByteOrder
	if order == nil {
		order = binary.LittleEndian
	}
	reader := newStoredPixelReader(frame.Format, order)
	luts := []*LUT{frame.Palette.Red, frame.Palette.Green, frame.Palette.Blue}
	for i := 0; i < layout.pixels; i++ {
		stored := int(reader.Value(frame.Pixels[i*bytesPerSample:]))
		dst := out.Pix[8*i : 8*i+8]
		for c, lut := range luts {
			value := lut.entry16(lut.Lookup(stored))
			dst[2*c], dst[2*c+1] = uint8(value>>8), uint8(value)
		}
		dst[6], dst[7] = 0xFF, 0xFF
	}
	return nil
}

// paletteChannel looks up a palette entry and scales it to 8 bits: entries of
// more than 8 bits keep their top eight (PS3.3 C.7.6.3.1.5).
func paletteChannel(lut *LUT, value int) uint8 {
	return lut.entryByte(lut.Lookup(value))
}

func setRGBA(img *image.RGBA, pixelIndex int, r, g, b uint8) {
	dst := pixelIndex * 4
	img.Pix[dst] = r
	img.Pix[dst+1] = g
	img.Pix[dst+2] = b
	img.Pix[dst+3] = 255
}

func clampByte(v float64) uint8 {
	return uint8(math.Round(clampFloat(v, 0, 255)))
}

func normalizePhotometric(value string) string {
	return strings.Trim(strings.ToUpper(value), " \x00")
}
