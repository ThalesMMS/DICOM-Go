package jpeg2000

import (
	"context"
	"fmt"
	"math"
	"strconv"
	"strings"

	"github.com/ThalesMMS/dicom-go/pixeldata"
	"github.com/ThalesMMS/dicom-go/transfer"
)

// jpeg2000LossyMethod is the Lossy Image Compression Method defined term for
// JPEG 2000 irreversible compression (PS3.3 C.7.6.1.1.5.1).
const jpeg2000LossyMethod = "ISO_15444_1"

// OpenJPEGLossyOptions selects how an OpenJPEG .91 encoder compresses: the
// quality layers by compression ratio or by PSNR, the tiles, the resolution
// levels, the wavelet and the progression. It mirrors the controls of
// opj_compress (and gdcmconv's -r, -q, -t, -n and --irreversible).
type OpenJPEGLossyOptions struct {
	// Rates are compression ratios, one per quality layer and decreasing
	// (for example 40, 20, 10): layer i ends at 1/Rates[i] of the native
	// size. A last rate of 1 with Reversible makes the last layer lossless.
	Rates []float64
	// Quality are target PSNRs in dB, one per layer and increasing. Exactly
	// one of Rates and Quality is set.
	Quality []float64
	// TileWidth and TileHeight tile the image; zero codes a single tile.
	TileWidth, TileHeight int
	// Resolutions is the number of resolution levels (decompositions plus
	// one); zero picks as many as the image allows, up to six. Every tile
	// side must hold 2^(Resolutions-1) samples.
	Resolutions int
	// Reversible codes with the 5/3 wavelet (and RCT for colour) instead of
	// the irreversible 9/7 (and ICT); the rates then truncate a reversible
	// codestream.
	Reversible bool
	// Progression is LRCP (the default), RLCP, RPCL, PCRL or CPRL.
	Progression string
	// KeepRGB codes RGB without the component transform, so the output stays
	// RGB rather than YBR_ICT or YBR_RCT; it compresses less.
	KeepRGB bool
}

const maxLossyLayers = 16

func (o OpenJPEGLossyOptions) validate() error {
	switch {
	case (len(o.Rates) == 0) == (len(o.Quality) == 0):
		return fmt.Errorf("%w: set exactly one of Rates and Quality", ErrOpenJPEGEncoderOptions)
	case len(o.Rates) > maxLossyLayers || len(o.Quality) > maxLossyLayers:
		return fmt.Errorf("%w: more than %d quality layers", ErrOpenJPEGEncoderOptions, maxLossyLayers)
	case o.TileWidth < 0 || o.TileHeight < 0 || (o.TileWidth == 0) != (o.TileHeight == 0):
		return fmt.Errorf("%w: tile size %dx%d", ErrOpenJPEGEncoderOptions, o.TileWidth, o.TileHeight)
	case o.Resolutions < 0 || o.Resolutions > 33:
		return fmt.Errorf("%w: %d resolutions", ErrOpenJPEGEncoderOptions, o.Resolutions)
	}
	for i, rate := range o.Rates {
		if math.IsNaN(rate) || rate < 1 || rate > 10000 || i > 0 && rate >= o.Rates[i-1] {
			return fmt.Errorf("%w: rates must be 1 to 10000 and decreasing", ErrOpenJPEGEncoderOptions)
		}
	}
	for i, q := range o.Quality {
		if math.IsNaN(q) || q <= 0 || q > 200 || i > 0 && q <= o.Quality[i-1] {
			return fmt.Errorf("%w: PSNR targets must be above 0 and increasing", ErrOpenJPEGEncoderOptions)
		}
	}
	switch strings.ToUpper(o.Progression) {
	case "", "LRCP", "RLCP", "RPCL", "PCRL", "CPRL":
	default:
		return fmt.Errorf("%w: progression %q", ErrOpenJPEGEncoderOptions, o.Progression)
	}
	return nil
}

// arguments builds the opj_compress coding arguments for a frame and names
// the Photometric Interpretation of the result ("" when unchanged).
func (o OpenJPEGLossyOptions) arguments(m pixeldata.Metadata) ([]string, string, error) {
	if err := o.validate(); err != nil {
		return nil, "", err
	}
	format := func(values []float64) string {
		parts := make([]string, len(values))
		for i, v := range values {
			parts[i] = strconv.FormatFloat(v, 'g', -1, 64)
		}
		return strings.Join(parts, ",")
	}
	var args []string
	if len(o.Rates) > 0 {
		args = append(args, "-r", format(o.Rates))
	} else {
		args = append(args, "-q", format(o.Quality))
	}
	if !o.Reversible {
		args = append(args, "-I")
	}
	width, height := int(m.Columns), int(m.Rows)
	if o.TileWidth > 0 {
		width, height = min(width, o.TileWidth), min(height, o.TileHeight)
		args = append(args, "-t", fmt.Sprintf("%d,%d", o.TileWidth, o.TileHeight))
	}
	resolutions := o.Resolutions
	if resolutions == 0 {
		resolutions = 1
		for side := min(width, height); side >= 2 && resolutions < 6; side /= 2 {
			resolutions++
		}
	}
	// OpenJPEG needs every tile side to hold one sample of the lowest
	// resolution (2^(n-1) on the grid).
	if min(width, height) < 1<<(resolutions-1) {
		return nil, "", fmt.Errorf("%w: %d resolutions for a %dx%d tile", ErrOpenJPEGEncoderOptions, resolutions, width, height)
	}
	args = append(args, "-n", strconv.Itoa(resolutions))
	if o.Progression != "" {
		args = append(args, "-p", strings.ToUpper(o.Progression))
	}
	photometric := ""
	if m.SamplesPerPixel == 3 && !o.KeepRGB {
		args = append(args, "-mct", "1")
		photometric = "YBR_ICT"
		if o.Reversible {
			photometric = "YBR_RCT"
		}
	} else {
		args = append(args, "-mct", "0")
	}
	return append(args, "-threads", "1"), photometric, nil
}

// OpenJPEGLossyEncoder encodes .91 frames with OpenJPEG, like
// OpenJPEGLosslessEncoder but with the coding its OpenJPEGLossyOptions
// select. The transcoder records the lossy compression (Lossy Image
// Compression, ratio and method ISO_15444_1) and, for colour coded with a
// component transform, the YBR_ICT or YBR_RCT photometry.
type OpenJPEGLossyEncoder struct {
	lossless *OpenJPEGLosslessEncoder
	coding   OpenJPEGLossyOptions
}

// NewOpenJPEGLossyEncoder validates coding and probes OpenJPEG 2.5.4 as the
// lossless encoder does.
func NewOpenJPEGLossyEncoder(ctx context.Context, opts OpenJPEGEncoderOptions, coding OpenJPEGLossyOptions) (*OpenJPEGLossyEncoder, error) {
	if opts.TileWidth != 0 || opts.TileHeight != 0 || opts.Resolutions != 0 {
		return nil, fmt.Errorf("%w: .91 coding belongs in OpenJPEGLossyOptions", ErrOpenJPEGEncoderOptions)
	}
	if err := coding.validate(); err != nil {
		return nil, err
	}
	coding.Rates = append([]float64(nil), coding.Rates...)
	coding.Quality = append([]float64(nil), coding.Quality...)
	base, err := NewOpenJPEGLosslessEncoder(ctx, opts)
	if err != nil {
		return nil, err
	}
	return &OpenJPEGLossyEncoder{lossless: base, coding: coding}, nil
}

// RegisterOpenJPEGLossyEncoder adds only .91 after the runtime preflight.
func RegisterOpenJPEGLossyEncoder(ctx context.Context, r pixeldata.EncoderRegistry, opts OpenJPEGEncoderOptions, coding OpenJPEGLossyOptions) error {
	if r == nil {
		return pixeldata.ErrEncoderRegistryNil
	}
	e, err := NewOpenJPEGLossyEncoder(ctx, opts, coding)
	if err != nil {
		return err
	}
	return r.RegisterEncoder(transfer.JPEG2000.UID, e)
}

func (e *OpenJPEGLossyEncoder) Capabilities() pixeldata.EncoderCapabilities {
	outputs := []string{"YBR_ICT"}
	if e != nil && e.coding.Reversible {
		outputs = []string{"YBR_RCT"}
	}
	return pixeldata.EncoderCapabilities{
		TransferSyntaxUID: transfer.JPEG2000.UID, BitsAllocated: []uint16{8, 16}, PixelRepresentations: []uint16{0},
		SamplesPerPixel: []uint16{1, 3}, PhotometricInterpretations: []string{"MONOCHROME1", "MONOCHROME2", "RGB"},
		OutputPhotometricInterpretations: outputs, OutputPlanarConfigurations: []uint16{0},
		Lossless: false, LossyMethod: jpeg2000LossyMethod, SupportsMultiFrame: true,
		Backend: "OpenJPEG 2.5.4 opj_compress (optional subprocess)",
	}
}

func (e *OpenJPEGLossyEncoder) EncodeFrame(ctx context.Context, frame []byte, m pixeldata.Metadata) (pixeldata.EncodedFrame, error) {
	if e == nil || e.lossless == nil || e.lossless.gate == nil {
		return pixeldata.EncodedFrame{}, ErrOpenJPEGEncoderUnavailable
	}
	if ctx == nil {
		ctx = context.Background()
	}
	ctx, cancel := context.WithTimeout(ctx, e.lossless.opts.Timeout)
	defer cancel()
	if err := ctx.Err(); err != nil {
		return pixeldata.EncodedFrame{}, err
	}
	if err := validateOpenJPEGEncodeFrame(frame, m, e.lossless.opts.MaxFrameBytes); err != nil {
		return pixeldata.EncodedFrame{}, err
	}
	args, photometric, err := e.coding.arguments(m)
	if err != nil {
		return pixeldata.EncodedFrame{}, err
	}
	select {
	case e.lossless.gate <- struct{}{}:
		defer func() { <-e.lossless.gate }()
	case <-ctx.Done():
		return pixeldata.EncodedFrame{}, ctx.Err()
	}
	encoded, err := e.lossless.encodeWith(ctx, frame, m, args)
	if err != nil {
		return pixeldata.EncodedFrame{}, err
	}
	if photometric != "" {
		encoded.PhotometricInterpretation = photometric
		planar := uint16(0)
		encoded.PlanarConfiguration = &planar
	}
	return encoded, nil
}
