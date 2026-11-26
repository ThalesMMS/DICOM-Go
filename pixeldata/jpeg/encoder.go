package jpeg

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"image"
	stdjpeg "image/jpeg"
	"strings"

	"github.com/ThalesMMS/dicom-go/pixeldata"
)

const (
	// DefaultQuality is the explicit default used by applications that opt in
	// to lossy JPEG Baseline or Extended encoding.
	DefaultQuality = 90
	MinQuality     = 1
	MaxQuality     = 100

	jpegLossyMethod = "ISO_10918_1"
)

var ErrInvalidQuality = errors.New("dicom: JPEG quality must be between 1 and 100")

// Encoder encodes canonical native 8-bit frames as JPEG Baseline Process 1.
// Encoder is immutable after construction and safe for concurrent use.
type Encoder struct {
	quality         int
	subsampling     Subsampling
	optimizeHuffman bool
}

// Subsampling selects the chroma sampling factors of a color JPEG frame.
// Monochrome always uses one component sampled at 1x1.
type Subsampling uint8

const (
	// Subsampling420 preserves the previous encoder's color sampling.
	Subsampling420 Subsampling = iota
	Subsampling422
	Subsampling444
)

// EncoderOptions configures Baseline Process 1. Quality is explicit (1..100).
// The zero Subsampling is 4:2:0. OptimizeHuffman opts into the local encoder
// with frame-specific Huffman tables; 4:2:2 and 4:4:4 always use that encoder
// regardless of the flag. The legacy 4:2:0/monochrome encoder remains the
// default, with standard Huffman tables and no coefficient-frame scratch.
// All color modes return YBR_FULL_422: PS3.5
// Table 8.2.1-1 permits that value for lossy YCbCr JPEG, not YBR_FULL. Actual
// sampling is conveyed by SOF0; see PS3.3 C.7.6.3.1.2, YBR_FULL_422 note 3.
// 4:2:2 and 4:4:4 retain more chroma samples and may produce larger frames
// than 4:2:0 at the same quality. Optimizing Huffman tables adds CPU work and
// coefficient scratch (128 bytes per padded 8x8 block); smaller output is not
// guaranteed for every input. The quality scale matches image/jpeg but FDCT
// rounding may differ, so optimized frames need not decode identically.
type EncoderOptions struct {
	Quality         int
	Subsampling     Subsampling
	OptimizeHuffman bool
}

var ErrInvalidSubsampling = errors.New("dicom: JPEG subsampling must be 4:2:0, 4:2:2 or 4:4:4")

var _ pixeldata.FrameEncoder = (*Encoder)(nil)

// NewEncoder returns a JPEG Baseline encoder with an explicit quality.
func NewEncoder(quality int) (*Encoder, error) {
	return NewEncoderWithOptions(EncoderOptions{Quality: quality})
}

// NewEncoderWithOptions returns an immutable encoder with explicit sampling.
func NewEncoderWithOptions(options EncoderOptions) (*Encoder, error) {
	if options.Quality < MinQuality || options.Quality > MaxQuality {
		return nil, ErrInvalidQuality
	}
	if options.Subsampling > Subsampling444 {
		return nil, ErrInvalidSubsampling
	}
	return &Encoder{quality: options.Quality, subsampling: options.Subsampling, optimizeHuffman: options.OptimizeHuffman || options.Subsampling != Subsampling420}, nil
}

// RegisterEncoder registers a JPEG Baseline encoder with explicit quality.
func RegisterEncoder(registry pixeldata.EncoderRegistry, quality int) error {
	return RegisterEncoderWithOptions(registry, EncoderOptions{Quality: quality})
}

// RegisterEncoderWithOptions registers explicit quality and sampling.
func RegisterEncoderWithOptions(registry pixeldata.EncoderRegistry, options EncoderOptions) error {
	if registry == nil {
		return pixeldata.ErrEncoderRegistryNil
	}
	encoder, err := NewEncoderWithOptions(options)
	if err != nil {
		return err
	}
	return registry.RegisterEncoder(UID, encoder)
}

// Quality reports the configured JPEG quality.
func (e *Encoder) Quality() int {
	if e == nil {
		return 0
	}
	return e.quality
}

// Subsampling reports the configured color sampling (monochrome ignores it).
func (e *Encoder) Subsampling() Subsampling {
	if e == nil {
		return Subsampling420
	}
	return e.subsampling
}

// OptimizesHuffman reports whether the local optimized encoder is selected.
// Selecting 4:2:2 or 4:4:4 always enables it, also for monochrome frames.
func (e *Encoder) OptimizesHuffman() bool {
	return e != nil && e.optimizeHuffman
}

// Capabilities describes the canonical native frames accepted by Encoder.
func (e *Encoder) Capabilities() pixeldata.EncoderCapabilities {
	backend := "go-image-jpeg"
	if e.OptimizesHuffman() {
		backend = "pure-go"
	}
	return pixeldata.EncoderCapabilities{
		TransferSyntaxUID:                UID,
		BitsAllocated:                    []uint16{8},
		PixelRepresentations:             []uint16{0},
		SamplesPerPixel:                  []uint16{1, 3},
		PhotometricInterpretations:       []string{"MONOCHROME1", "MONOCHROME2", "RGB"},
		OutputPhotometricInterpretations: []string{"YBR_FULL_422"},
		OutputPlanarConfigurations:       []uint16{0},
		Lossless:                         false,
		LossyMethod:                      jpegLossyMethod,
		SupportsMultiFrame:               true,
		Backend:                          backend,
	}
}

// EncodeFrame encodes one little-endian, sample-interleaved native frame.
func (e *Encoder) EncodeFrame(ctx context.Context, frame []byte, metadata pixeldata.Metadata) (pixeldata.EncodedFrame, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	if err := ctx.Err(); err != nil {
		return pixeldata.EncodedFrame{}, err
	}
	if e == nil || e.quality < MinQuality || e.quality > MaxQuality {
		return pixeldata.EncodedFrame{}, ErrInvalidQuality
	}
	if e.subsampling > Subsampling444 {
		return pixeldata.EncodedFrame{}, ErrInvalidSubsampling
	}
	frameLength, err := validateJPEGEncoderInput(metadata)
	if err != nil {
		return pixeldata.EncodedFrame{}, err
	}
	if uint64(len(frame)) != frameLength {
		return pixeldata.EncodedFrame{}, fmt.Errorf("%w: native frame length", pixeldata.ErrPixelDataSizeMismatch)
	}

	if e.optimizeHuffman {
		return e.encodeBaseline(ctx, frame, metadata)
	}
	img, output, err := jpegImage(frame, metadata)
	if err != nil {
		return pixeldata.EncodedFrame{}, err
	}
	var encoded bytes.Buffer
	if err := stdjpeg.Encode(&encoded, img, &stdjpeg.Options{Quality: e.quality}); err != nil {
		return pixeldata.EncodedFrame{}, err
	}
	if err := ctx.Err(); err != nil {
		return pixeldata.EncodedFrame{}, err
	}
	output.Data = encoded.Bytes()
	return output, nil
}

func validateJPEGEncoderInput(metadata pixeldata.Metadata) (uint64, error) {
	if metadata.Rows == 0 {
		return 0, unsupportedJPEGEncoderMetadata("Rows")
	}
	if metadata.Columns == 0 {
		return 0, unsupportedJPEGEncoderMetadata("Columns")
	}
	if metadata.BitsAllocated != 8 {
		return 0, unsupportedJPEGEncoderMetadata("BitsAllocated")
	}
	if metadata.BitsStored != 8 {
		return 0, unsupportedJPEGEncoderMetadata("BitsStored")
	}
	if metadata.HighBit != 7 {
		return 0, unsupportedJPEGEncoderMetadata("HighBit")
	}
	if metadata.PixelRepresentation != 0 {
		return 0, unsupportedJPEGEncoderMetadata("PixelRepresentation")
	}
	if metadata.NumberOfFrames < 1 {
		return 0, unsupportedJPEGEncoderMetadata("NumberOfFrames")
	}
	photometric := strings.ToUpper(strings.TrimSpace(metadata.PhotometricInterpretation))
	switch metadata.SamplesPerPixel {
	case 1:
		if photometric != "MONOCHROME1" && photometric != "MONOCHROME2" {
			return 0, unsupportedJPEGEncoderMetadata("PhotometricInterpretation")
		}
		if metadata.PlanarConfigurationPresent {
			return 0, unsupportedJPEGEncoderMetadata("PlanarConfiguration")
		}
	case 3:
		if photometric != "RGB" {
			return 0, unsupportedJPEGEncoderMetadata("PhotometricInterpretation")
		}
		if !metadata.PlanarConfigurationPresent || metadata.PlanarConfiguration != 0 {
			return 0, unsupportedJPEGEncoderMetadata("PlanarConfiguration")
		}
	default:
		return 0, unsupportedJPEGEncoderMetadata("SamplesPerPixel")
	}
	pixels := uint64(metadata.Rows) * uint64(metadata.Columns)
	if metadata.Rows != 0 && pixels/uint64(metadata.Rows) != uint64(metadata.Columns) {
		return 0, fmt.Errorf("%w: native frame length", pixeldata.ErrEncoderOutputInvalid)
	}
	frameLength := pixels * uint64(metadata.SamplesPerPixel)
	if pixels != 0 && frameLength/pixels != uint64(metadata.SamplesPerPixel) {
		return 0, fmt.Errorf("%w: native frame length", pixeldata.ErrEncoderOutputInvalid)
	}
	if frameLength > uint64(int(^uint(0)>>1)) {
		return 0, fmt.Errorf("%w: native frame length", pixeldata.ErrEncoderOutputInvalid)
	}
	return frameLength, nil
}

func jpegImage(frame []byte, metadata pixeldata.Metadata) (image.Image, pixeldata.EncodedFrame, error) {
	rows := int(metadata.Rows)
	columns := int(metadata.Columns)
	rect := image.Rect(0, 0, columns, rows)
	if metadata.SamplesPerPixel == 1 {
		img := image.NewGray(rect)
		copy(img.Pix, frame)
		return img, pixeldata.EncodedFrame{}, nil
	}
	img := image.NewRGBA(rect)
	for src, dst := 0, 0; src < len(frame); src, dst = src+3, dst+4 {
		img.Pix[dst] = frame[src]
		img.Pix[dst+1] = frame[src+1]
		img.Pix[dst+2] = frame[src+2]
		img.Pix[dst+3] = 0xff
	}
	planar := uint16(0)
	return img, pixeldata.EncodedFrame{
		PhotometricInterpretation: "YBR_FULL_422",
		PlanarConfiguration:       &planar,
	}, nil
}

func unsupportedJPEGEncoderMetadata(field string) error {
	return &pixeldata.UnsupportedEncoderMetadataError{Field: field}
}
