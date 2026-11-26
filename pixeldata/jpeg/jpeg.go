package jpeg

import (
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"image"
	"image/color"
	stdjpeg "image/jpeg"
	"math"
	"strings"

	"github.com/ThalesMMS/dicom-go/internal/decoderecovery"
	"github.com/ThalesMMS/dicom-go/object"
	"github.com/ThalesMMS/dicom-go/pixeldata"
	"github.com/ThalesMMS/dicom-go/pixeldata/encapsulated"
)

// DICOM JPEG transfer syntax UIDs supported by this adapter.
const (
	// UID is the DICOM JPEG Baseline (Process 1) transfer syntax UID.
	UID = "1.2.840.10008.1.2.4.50"
	// UIDExtended is the DICOM JPEG Extended (Process 2 & 4) transfer syntax
	// UID. The codec supports 8-bit Process 2 through image/jpeg and the DICOM
	// 12-bit unsigned monochrome Process 4 subset through its pure-Go decoder.
	UIDExtended = "1.2.840.10008.1.2.4.51"
	// UIDSpectralSelection is the retired DICOM JPEG Spectral Selection,
	// Non-Hierarchical (Process 6 & 8) transfer syntax UID. The codec decodes
	// its 8-bit SOF2 frames through image/jpeg and qualified 12-bit frames
	// through the in-tree decoder.
	UIDSpectralSelection = "1.2.840.10008.1.2.4.53"
	// UIDFullProgression is the retired DICOM JPEG Full Progression,
	// Non-Hierarchical (Process 10 & 12) transfer syntax UID. The codec
	// decodes 8-bit SOF2 frames through image/jpeg and qualified 12-bit frames
	// through the in-tree decoder.
	UIDFullProgression = "1.2.840.10008.1.2.4.55"
)

var (
	ErrInvalidFragment            = errors.New("dicom: invalid JPEG still-image fragment")
	ErrUnsupportedBitsAllocated   = errors.New("dicom: unsupported JPEG still-image BitsAllocated")
	ErrUnsupportedBitsStored      = errors.New("dicom: unsupported JPEG still-image BitsStored")
	ErrUnsupportedHighBit         = errors.New("dicom: unsupported JPEG still-image HighBit")
	ErrUnsupportedSamplesPerPixel = errors.New("dicom: unsupported JPEG still-image SamplesPerPixel")
	ErrImageSizeMismatch          = errors.New("dicom: JPEG still-image size does not match metadata")
)

type codecMode uint8

const (
	codecModeAuto codecMode = iota
	codecModeBaseline
	codecModeExtended
	// codecModeProgressive decodes the retired progressive syntaxes (.53 and
	// .55): the documented 8-bit and strict 12-bit SOF2 subsets.
	codecModeProgressive
)

// Codec decodes DICOM JPEG Baseline, supported JPEG Extended and the retired
// progressive (Spectral Selection, Full Progression) encapsulated pixel
// data. Registered instances are transfer-syntax-specific; New retains
// the historical dual-syntax behavior for direct callers.
type Codec struct {
	mode         codecMode
	spectralOnly bool
}

// New returns a DICOM JPEG Baseline/Extended pixel data codec.
func New() *Codec {
	return &Codec{mode: codecModeAuto}
}

func newBaselineCodec() *Codec { return &Codec{mode: codecModeBaseline} }

func newExtendedCodec() *Codec { return &Codec{mode: codecModeExtended} }

func newProgressiveCodec() *Codec { return &Codec{mode: codecModeProgressive} }

// TransferSyntaxUIDs returns the transfer syntaxes Register adds, in
// registration order.
func TransferSyntaxUIDs() []string {
	return []string{UID, UIDExtended, UIDSpectralSelection, UIDFullProgression}
}

func codecFor(uid string) *Codec {
	switch uid {
	case UID:
		return newBaselineCodec()
	case UIDExtended:
		return newExtendedCodec()
	default:
		c := newProgressiveCodec()
		c.spectralOnly = uid == UIDSpectralSelection
		return c
	}
}

// Register registers the JPEG Baseline, JPEG Extended and retired
// progressive (Spectral Selection and Full Progression) codecs in the
// provided pixel data registry.
func Register(registry pixeldata.Registry) error {
	if registry == nil {
		return pixeldata.ErrCodecRegistryNil
	}
	for _, uid := range TransferSyntaxUIDs() {
		if err := registry.RegisterCodec(uid, codecFor(uid)); err != nil {
			return err
		}
	}
	return nil
}

// RegisterDefault registers the codecs Register adds in
// pixeldata.DefaultRegistry.
func RegisterDefault() error {
	for _, uid := range TransferSyntaxUIDs() {
		if err := pixeldata.RegisterCodec(uid, codecFor(uid)); err != nil {
			return err
		}
	}
	return nil
}

func (c *Codec) Decode(pixel pixeldata.PixelData, obj *object.Object) (pixeldata.Frames, error) {
	return c.DecodeContext(context.Background(), pixel, obj)
}

// DecodeContext decodes with cancellation and the encapsulated recoveries
// carried by ctx (pixeldata.DecodeContextWithRecovery).
func (c *Codec) DecodeContext(ctx context.Context, pixel pixeldata.PixelData, obj *object.Object) (pixeldata.Frames, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	if err := ctx.Err(); err != nil {
		return pixeldata.Frames{}, err
	}
	if !pixel.Encapsulated {
		return pixeldata.Frames{}, fmt.Errorf("%w: JPEG still-image codec requires encapsulated pixel data", pixeldata.ErrIncompatiblePixelData)
	}

	metadata, err := pixeldata.ExtractMetadata(obj)
	if err != nil {
		return pixeldata.Frames{}, err
	}
	progressive12 := c.mode == codecModeProgressive && metadata.BitsAllocated == 16
	highPrecision := metadata.BitsAllocated == 16 && (c.mode == codecModeAuto || c.mode == codecModeExtended || progressive12)
	if c.mode == codecModeBaseline && metadata.BitsAllocated != 8 {
		return pixeldata.Frames{}, fmt.Errorf("%w: BitsAllocated=%d", ErrUnsupportedBitsAllocated, metadata.BitsAllocated)
	}
	if c.mode == codecModeProgressive && metadata.BitsAllocated != 8 && !progressive12 {
		return pixeldata.Frames{}, fmt.Errorf("%w: BitsAllocated=%d", ErrUnsupportedBitsAllocated, metadata.BitsAllocated)
	}
	if progressive12 && metadata.BitsStored != 12 {
		return pixeldata.Frames{}, fmt.Errorf("%w: progressive12 requires BitsStored=12", ErrUnsupportedBitsStored)
	}
	if metadata.BitsAllocated != 8 && !highPrecision {
		return pixeldata.Frames{}, fmt.Errorf("%w: BitsAllocated=%d", ErrUnsupportedBitsAllocated, metadata.BitsAllocated)
	}
	// Bits Stored may be below the codestream precision (8, or 12 for
	// Process 4); decoded samples are then limited to Bits Stored.
	if !highPrecision && (metadata.BitsStored == 0 || metadata.BitsStored > 8) {
		return pixeldata.Frames{}, fmt.Errorf("%w: BitsStored=%d", ErrUnsupportedBitsStored, metadata.BitsStored)
	}
	if highPrecision && (metadata.BitsStored <= 8 || metadata.BitsStored > extendedProcess4Precision) {
		// Preserve the historical classification for a 16-bit native layout
		// that is not a Process 4 representation.
		return pixeldata.Frames{}, fmt.Errorf("%w: BitsStored=%d", ErrUnsupportedBitsAllocated, metadata.BitsStored)
	}
	if metadata.HighBit != metadata.BitsStored-1 {
		return pixeldata.Frames{}, fmt.Errorf("%w: HighBit=%d", ErrUnsupportedHighBit, metadata.HighBit)
	}
	if metadata.SamplesPerPixel != 1 && metadata.SamplesPerPixel != 3 {
		return pixeldata.Frames{}, fmt.Errorf("%w: SamplesPerPixel=%d", ErrUnsupportedSamplesPerPixel, metadata.SamplesPerPixel)
	}
	// Signed samples pass through: the codestream holds their two's
	// complement bits (as DCMTK decodes them); color must be unsigned.
	if metadata.PixelRepresentation > 1 || (metadata.PixelRepresentation == 1 && metadata.SamplesPerPixel != 1) {
		return pixeldata.Frames{}, fmt.Errorf("%w: PixelRepresentation=%d", pixeldata.ErrUnsupportedPixelRepresentation, metadata.PixelRepresentation)
	}
	if metadata.PlanarConfigurationPresent && metadata.PlanarConfiguration != 0 {
		return pixeldata.Frames{}, fmt.Errorf("%w: PlanarConfiguration=%d", pixeldata.ErrUnsupportedPlanarConfiguration, metadata.PlanarConfiguration)
	}
	if !supportedPhotometricInterpretation(metadata) {
		return pixeldata.Frames{}, fmt.Errorf(
			"%w: PhotometricInterpretation=%s SamplesPerPixel=%d",
			pixeldata.ErrUnsupportedPhotometricInterpretation,
			strings.TrimSpace(metadata.PhotometricInterpretation),
			metadata.SamplesPerPixel,
		)
	}

	if progressive12 {
		if metadata.PixelRepresentation != 0 {
			return pixeldata.Frames{}, pixeldata.ErrUnsupportedPixelRepresentation
		}
		if metadata.SamplesPerPixel == 1 && metadata.PlanarConfigurationPresent || metadata.SamplesPerPixel == 3 && !metadata.PlanarConfigurationPresent {
			return pixeldata.Frames{}, pixeldata.ErrUnsupportedPlanarConfiguration
		}
		if metadata.SamplesPerPixel == 3 && (strings.ToUpper(strings.TrimSpace(metadata.PhotometricInterpretation)) != "RGB" || pixeldata.JPEGColorConversionFrom(ctx) == pixeldata.JPEGColorAlways) {
			return pixeldata.Frames{}, pixeldata.ErrUnsupportedPhotometricInterpretation
		}
	}

	codestreams, err := encapsulated.FromFragments(ctx, pixel.Sequence, obj, metadata.NumberOfFrames, encapsulated.JPEG, encapsulated.Limits{})
	if err != nil {
		if errors.Is(err, encapsulated.ErrFrameCount) {
			err = errors.Join(pixeldata.ErrPixelDataSizeMismatch, err)
		}
		return pixeldata.Frames{}, fmt.Errorf("%w: %w", ErrInvalidFragment, err)
	}
	if highPrecision && !progressive12 {
		if err := validateExtendedProcess4Request(metadata, pixel.Sequence.Fragments); err != nil {
			return pixeldata.Frames{}, err
		}
	}
	if progressive12 {
		if _, err := progressive12RequestBytes(metadata, codestreams.InputBytes(), codestreams.MaxFrameBytes()); err != nil {
			return pixeldata.Frames{}, fmt.Errorf("%w: %w", ErrInvalidFragment, err)
		}
	}

	// Retained native output, source payload, the largest joined compressed
	// frame and one decoder image coexist. Check them before any image decode.
	// JPEG color output has three samples at each pixel even when the
	// DICOM photometric interpretation denotes subsampled native storage.
	frameBytes := int64(metadata.Rows) * int64(metadata.Columns) * int64(metadata.SamplesPerPixel) * int64(metadata.BytesPerSample())
	const maxRequestBytes = uint64(512 << 20)
	if frameBytes <= 0 || uint64(frameBytes) > maxRequestBytes/uint64(metadata.NumberOfFrames) {
		return pixeldata.Frames{}, fmt.Errorf("%w: decoded request exceeds resource limit", ErrInvalidFragment)
	}
	requestBytes := uint64(frameBytes)*uint64(metadata.NumberOfFrames) + codestreams.InputBytes() + codestreams.MaxFrameBytes()
	if requestBytes+uint64(metadata.Rows)*uint64(metadata.Columns)*4 > maxRequestBytes {
		return pixeldata.Frames{}, fmt.Errorf("%w: request working set exceeds resource limit", ErrInvalidFragment)
	}

	frames := make([][]byte, codestreams.Len())
	var result pixeldata.Frames
	policy := pixeldata.JPEGColorConversionFrom(ctx)
	if codestreams.Recovery().BasicOffsetTableIgnored {
		result.AddDiagnostic(pixeldata.DiagnosticBasicOffsetTableIgnored)
	}
	progressiveWork := &progressive12WorkBudget{remaining: progressive12MaxBlockVisits}
	for i := range frames {
		if err := ctx.Err(); err != nil {
			return pixeldata.Frames{}, err
		}
		view, err := codestreams.Frame(ctx, i)
		if err != nil {
			return pixeldata.Frames{}, fmt.Errorf("%w: %w", ErrInvalidFragment, err)
		}
		fragment := view.Data
		if view.Padded && view.PadByte != 0 {
			result.AddDiagnostic(pixeldata.DiagnosticNonZeroFramePad)
		}
		baselineCandidate := !highPrecision && (c.mode == codecModeBaseline || c.mode == codecModeAuto)
		var header baselineHeader
		var sof byte
		if progressive12 {
			// The bounded contextual parser below validates SOF2 and every marker.
			sof = 0xc2
		} else if baselineCandidate {
			header, err = probeBaselineHeader(ctx, fragment)
			sof = header.sof
		} else {
			sof, err = jpegFrameSOFMarker(ctx, fragment)
		}
		if err != nil {
			if ctx.Err() != nil {
				return pixeldata.Frames{}, ctx.Err()
			}
			return pixeldata.Frames{}, fmt.Errorf("%w: frame %d: %w", ErrInvalidFragment, i, err)
		}
		if c.mode == codecModeBaseline && sof != 0xc0 {
			return pixeldata.Frames{}, fmt.Errorf("%w: frame %d: JPEG Baseline requires SOF0", ErrInvalidFragment, i)
		}
		if c.mode == codecModeExtended && sof != 0xc1 {
			// Baseline is a subset of Process 2 & 4: an 8-bit SOF0 frame is
			// decodable under .51. Other processes remain refused.
			if sof != 0xc0 || highPrecision {
				return pixeldata.Frames{}, fmt.Errorf("%w: frame %d: JPEG Extended requires SOF1 (or Baseline SOF0 at 8 bits)", ErrInvalidFragment, i)
			}
			result.AddDiagnostic(pixeldata.DiagnosticJPEGBaselineUnderExtended)
		}
		if c.mode == codecModeProgressive && sof != 0xc2 {
			// Processes 6 & 8 and 10 & 12 are Huffman progressive DCT (SOF2);
			// arithmetic (SOF10) and sequential frames are refused.
			return pixeldata.Frames{}, fmt.Errorf("%w: frame %d: retired JPEG progressive syntaxes require SOF2", ErrInvalidFragment, i)
		}
		if progressive12 {
			frame, err := decodeProgressive12WithBudget(ctx, fragment, metadata, c.spectralOnly, progressiveWork)
			if err != nil {
				return pixeldata.Frames{}, fmt.Errorf("%w: frame %d: %w", ErrInvalidFragment, i, err)
			}
			frames[i] = frame
			if metadata.SamplesPerPixel == 3 {
				result.PhotometricInterpretation = "RGB"
			}
			continue
		}
		if highPrecision {
			if sof != 0xc1 {
				return pixeldata.Frames{}, fmt.Errorf("%w: BitsAllocated=%d requires JPEG Extended SOF1", ErrUnsupportedBitsAllocated, metadata.BitsAllocated)
			}
			// The SOF1 decoder admits its own rasters against the other
			// frames and the compressed bytes already reserved here.
			reservedBytes := requestBytes - uint64(frameBytes)
			frame, ycbcr, ignored, err := decodeExtendedProcess4Options(ctx, fragment, metadata, decoderecovery.From(ctx).JPEGSequentialScanQuirks, reservedBytes)
			if err != nil {
				if ctxErr := ctx.Err(); ctxErr != nil {
					return pixeldata.Frames{}, ctxErr
				}
				return pixeldata.Frames{}, fmt.Errorf("%w: frame %d: %w", ErrInvalidFragment, i, err)
			}
			if ignored {
				result.AddDiagnostic(pixeldata.DiagnosticJPEGScanParametersIgnored)
			}
			if metadata.SamplesPerPixel == 3 {
				convert := convertsYCbCr(policy, metadata, ycbcr)
				if convert {
					ycbcrToRGB16(frame, extendedProcess4Precision)
				}
				result.PhotometricInterpretation = outputPhotometric(policy, metadata, convert)
			}
			limitStoredBits16(frame, metadata)
			if err := ctx.Err(); err != nil {
				return pixeldata.Frames{}, err
			}
			frames[i] = frame
			continue
		}
		if err := encapsulated.ValidateFrame(ctx, fragment, encapsulated.JPEG); err != nil {
			return pixeldata.Frames{}, fmt.Errorf("%w: %w", ErrInvalidFragment, err)
		}
		baseline := baselineCandidate && sof == 0xc0
		var img image.Image
		if baseline {
			img, err = decodeBaselineImage(ctx, fragment, metadata, header, i)
			if err != nil {
				return pixeldata.Frames{}, err
			}
		} else {
			config, err := stdjpeg.DecodeConfig(bytes.NewReader(fragment))
			if err != nil {
				return pixeldata.Frames{}, fmt.Errorf("%w: %w", ErrInvalidFragment, err)
			}
			if config.Width != int(metadata.Columns) || config.Height != int(metadata.Rows) {
				return pixeldata.Frames{}, ErrImageSizeMismatch
			}
			img, err = stdjpeg.Decode(bytes.NewReader(fragment))
			if err != nil {
				return pixeldata.Frames{}, fmt.Errorf("%w: frame %d: %w", ErrInvalidFragment, i, err)
			}
		}
		var frame []byte
		if baseline {
			frame, err = baselineImageToFrameBytes(ctx, img, metadata, policy)
		} else {
			frame, err = imageToFrameBytesPolicy(img, metadata, policy)
		}
		if err != nil {
			if ctx.Err() != nil {
				return pixeldata.Frames{}, ctx.Err()
			}
			return pixeldata.Frames{}, fmt.Errorf("decode JPEG still-image frame %d: %w", i, err)
		}
		if metadata.SamplesPerPixel == 3 {
			_, stdYCbCr := img.(*image.YCbCr)
			result.PhotometricInterpretation = outputPhotometric(policy, metadata, convertsYCbCr(policy, metadata, stdYCbCr))
		}
		limitStoredBits8(frame, metadata)
		if baseline && ctx.Err() != nil {
			return pixeldata.Frames{}, ctx.Err()
		}
		frames[i] = frame
	}

	result.Rows, result.Columns, result.Data = int(metadata.Rows), int(metadata.Columns), frames
	return result, nil
}

func supportedPhotometricInterpretation(metadata pixeldata.Metadata) bool {
	photometric := strings.ToUpper(strings.TrimSpace(metadata.PhotometricInterpretation))
	switch metadata.SamplesPerPixel {
	case 1:
		return photometric == "MONOCHROME1" || photometric == "MONOCHROME2"
	case 3:
		return photometric == "RGB" || photometric == "YBR_FULL" || photometric == "YBR_FULL_422"
	default:
		return false
	}
}

func imageToFrameBytes(img image.Image, metadata pixeldata.Metadata) ([]byte, error) {
	if img == nil {
		return nil, fmt.Errorf("%w: decoded image is nil", ErrInvalidFragment)
	}
	bounds := img.Bounds()
	rows := int(metadata.Rows)
	columns := int(metadata.Columns)
	if bounds.Dx() != columns || bounds.Dy() != rows {
		return nil, fmt.Errorf("%w: got %dx%d want %dx%d", ErrImageSizeMismatch, bounds.Dx(), bounds.Dy(), columns, rows)
	}

	switch metadata.SamplesPerPixel {
	case 1:
		out := make([]byte, rows*columns)
		for y := 0; y < rows; y++ {
			for x := 0; x < columns; x++ {
				gray := color.GrayModel.Convert(img.At(bounds.Min.X+x, bounds.Min.Y+y)).(color.Gray)
				out[y*columns+x] = gray.Y
			}
		}
		return out, nil
	case 3:
		if ycc, ok := img.(*image.YCbCr); ok && bounds.Min == (image.Point{}) {
			if out, ok := ycbcrToRGBFrame(ycc, rows, columns); ok {
				return out, nil
			}
		}
		out := make([]byte, rows*columns*3)
		for y := 0; y < rows; y++ {
			for x := 0; x < columns; x++ {
				rgba := color.RGBAModel.Convert(img.At(bounds.Min.X+x, bounds.Min.Y+y)).(color.RGBA)
				offset := (y*columns + x) * 3
				out[offset] = rgba.R
				out[offset+1] = rgba.G
				out[offset+2] = rgba.B
			}
		}
		return out, nil
	default:
		return nil, fmt.Errorf("%w: SamplesPerPixel=%d", ErrUnsupportedSamplesPerPixel, metadata.SamplesPerPixel)
	}
}

// convertsYCbCr reports whether a three-component frame is converted from
// YCbCr to RGB under policy; codestreamYCbCr is the libjpeg rule's verdict.
func convertsYCbCr(policy pixeldata.JPEGColorConversion, metadata pixeldata.Metadata, codestreamYCbCr bool) bool {
	switch policy {
	case pixeldata.JPEGColorAlways:
		return true
	case pixeldata.JPEGColorNever:
		return false
	case pixeldata.JPEGColorPhotometric:
		return strings.HasPrefix(strings.ToUpper(strings.TrimSpace(metadata.PhotometricInterpretation)), "YBR")
	default:
		return codestreamYCbCr
	}
}

// outputPhotometric is the Photometric Interpretation of decoded color
// samples: RGB when converted or when the codestream said RGB (guess), else
// the object's, with 4:2:2 upsampled to YBR_FULL.
func outputPhotometric(policy pixeldata.JPEGColorConversion, metadata pixeldata.Metadata, converted bool) string {
	if converted || policy == pixeldata.JPEGColorGuess {
		return "RGB"
	}
	photometric := strings.ToUpper(strings.TrimSpace(metadata.PhotometricInterpretation))
	if strings.HasPrefix(photometric, "YBR") {
		return "YBR_FULL"
	}
	return photometric
}

// imageToFrameBytesPolicy is imageToFrameBytes under a color policy: the
// standard decoder converts YCbCr (by the libjpeg rule), so JPEGColorNever
// reads the YCbCr planes, and JPEGColorAlways converts frames the decoder
// took for RGB.
func imageToFrameBytesPolicy(img image.Image, metadata pixeldata.Metadata, policy pixeldata.JPEGColorConversion) ([]byte, error) {
	if metadata.SamplesPerPixel != 3 || policy == pixeldata.JPEGColorGuess {
		return imageToFrameBytes(img, metadata)
	}
	ycc, stdYCbCr := img.(*image.YCbCr)
	convert := convertsYCbCr(policy, metadata, stdYCbCr)
	switch {
	case convert == stdYCbCr:
		// The decoder already did what the policy asks.
		return imageToFrameBytes(img, metadata)
	case stdYCbCr:
		// Keep YCbCr: replicate chroma to full resolution.
		bounds := ycc.Bounds()
		rows, columns := int(metadata.Rows), int(metadata.Columns)
		if bounds.Dx() != columns || bounds.Dy() != rows {
			return nil, fmt.Errorf("%w: got %dx%d want %dx%d", ErrImageSizeMismatch, bounds.Dx(), bounds.Dy(), columns, rows)
		}
		out := make([]byte, rows*columns*3)
		for y := 0; y < rows; y++ {
			for x := 0; x < columns; x++ {
				yi := ycc.YOffset(bounds.Min.X+x, bounds.Min.Y+y)
				ci := ycc.COffset(bounds.Min.X+x, bounds.Min.Y+y)
				o := (y*columns + x) * 3
				out[o], out[o+1], out[o+2] = ycc.Y[yi], ycc.Cb[ci], ycc.Cr[ci]
			}
		}
		return out, nil
	default:
		// The decoder kept the components; convert them from YCbCr.
		out, err := imageToFrameBytes(img, metadata)
		if err != nil {
			return nil, err
		}
		for i := 0; i+2 < len(out); i += 3 {
			r, g, b := color.YCbCrToRGB(out[i], out[i+1], out[i+2])
			out[i], out[i+1], out[i+2] = r, g, b
		}
		return out, nil
	}
}

// ycbcrToRGB16 converts interleaved little-endian YCbCr samples of the given
// precision to RGB in place (T.871 full range, chroma centered on half).
func ycbcrToRGB16(frame []byte, precision int) {
	maximum := float64(int(1)<<precision - 1)
	half := float64(int(1) << (precision - 1))
	clamp := func(v float64) uint16 {
		return uint16(math.Round(math.Max(0, math.Min(maximum, v))))
	}
	for i := 0; i+5 < len(frame); i += 6 {
		y := float64(binary.LittleEndian.Uint16(frame[i:]))
		cb := float64(binary.LittleEndian.Uint16(frame[i+2:])) - half
		cr := float64(binary.LittleEndian.Uint16(frame[i+4:])) - half
		binary.LittleEndian.PutUint16(frame[i:], clamp(y+1.402*cr))
		binary.LittleEndian.PutUint16(frame[i+2:], clamp(y-0.344136*cb-0.714136*cr))
		binary.LittleEndian.PutUint16(frame[i+4:], clamp(y+1.772*cb))
	}
}

// limitStoredBits8 and limitStoredBits16 keep decoded samples within Bits
// Stored when it is below the codestream precision: unsigned samples are
// clamped (lossy overshoot), signed ones keep their low Bits Stored bits.
func limitStoredBits8(frame []byte, metadata pixeldata.Metadata) {
	bits := int(metadata.BitsStored)
	if bits <= 0 || bits >= 8 {
		return
	}
	mask := byte(1<<bits - 1)
	for i, v := range frame {
		if metadata.PixelRepresentation == 1 {
			frame[i] = v & mask
		} else if v > mask {
			frame[i] = mask
		}
	}
}

func limitStoredBits16(frame []byte, metadata pixeldata.Metadata) {
	bits := int(metadata.BitsStored)
	if bits <= 0 || bits >= extendedProcess4Precision {
		return
	}
	mask := uint16(1<<bits - 1)
	for i := 0; i+1 < len(frame); i += 2 {
		v := binary.LittleEndian.Uint16(frame[i:])
		if metadata.PixelRepresentation == 1 {
			v &= mask
		} else if v > mask {
			v = mask
		}
		binary.LittleEndian.PutUint16(frame[i:], v)
	}
}
