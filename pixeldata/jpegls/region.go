package jpegls

import (
	"context"
	"errors"
	"fmt"
	"image"
	"strings"

	"github.com/ThalesMMS/dicom-go/internal/decoderecovery"
	"github.com/ThalesMMS/dicom-go/pixeldata"
	"github.com/ThalesMMS/dicom-go/pixeldata/encapsulated"
)

var (
	_ pixeldata.PartialFrameDecoder = (*Codec)(nil)
	_ pixeldata.FrameRegionDecoder  = (*Codec)(nil)
)

// PartialDecodeCapabilities reports region-sized output for qualified
// monochrome frames. Entropy decoding still walks all predicted samples to
// validate the complete codestream. JPEG-LS has no reduced resolutions or layers.
func (c *Codec) PartialDecodeCapabilities(codestream []byte, metadata pixeldata.Metadata) (pixeldata.PartialDecodeCapabilities, error) {
	_, err := c.prepareRegion(context.Background(), codestream, metadata)
	if errors.Is(err, pixeldata.ErrRegionUnsupported) {
		return pixeldata.PartialDecodeCapabilities{}, nil
	}
	return pixeldata.PartialDecodeCapabilities{Region: err == nil}, err
}

// DecodeFramePartial decodes a full-resolution monochrome region, with the
// same stored samples and little-endian byte order as complete frame decode.
func (c *Codec) DecodeFramePartial(ctx context.Context, codestream []byte, metadata pixeldata.Metadata, request pixeldata.PartialDecodeRequest) (pixeldata.Frames, error) {
	if request.ReductionLevel != 0 || request.QualityLayers != 0 {
		return pixeldata.Frames{}, pixeldata.ErrPartialDecodeUnsupported
	}
	frames, err := c.DecodeFrameRegion(ctx, codestream, metadata, request.Region)
	if errors.Is(err, pixeldata.ErrRegionUnsupported) {
		return pixeldata.Frames{}, pixeldata.ErrPartialDecodeUnsupported
	}
	return frames, err
}

// DecodeFrameRegion reconstructs a monochrome region using three predictor
// rows and one packed region-sized output. A failure publishes no pixels.
func (c *Codec) DecodeFrameRegion(ctx context.Context, codestream []byte, metadata pixeldata.Metadata, region image.Rectangle) (pixeldata.Frames, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	if err := ctx.Err(); err != nil {
		return pixeldata.Frames{}, err
	}
	if !validRegion(metadata, region) {
		return pixeldata.Frames{}, pixeldata.ErrInvalidRegion
	}
	scan, err := c.prepareRegion(ctx, codestream, metadata)
	if err != nil {
		return pixeldata.Frames{}, err
	}
	bytesPerSample := int(metadata.BitsAllocated / 8)
	output := make([]byte, region.Dx()*region.Dy()*bytesPerSample)
	if err := scan.decode(ctx, region, bytesPerSample, output); err != nil {
		return pixeldata.Frames{}, err
	}
	return pixeldata.Frames{
		Rows: region.Dy(), Columns: region.Dx(), Data: [][]byte{output}, Diagnostics: scan.diagnostics,
	}, nil
}

func validRegion(metadata pixeldata.Metadata, region image.Rectangle) bool {
	return !region.Empty() && region.In(image.Rect(0, 0, int(metadata.Columns), int(metadata.Rows)))
}

func (c *Codec) validateRegionalMetadata(metadata pixeldata.Metadata) error {
	photo := strings.ToUpper(strings.TrimSpace(metadata.PhotometricInterpretation))
	if metadata.SamplesPerPixel != 1 || photo != "MONOCHROME1" && photo != "MONOCHROME2" {
		return pixeldata.ErrRegionUnsupported
	}
	metadata.NumberOfFrames = 1
	return c.validateDecodeMetadata(metadata)
}

type regionalScan struct {
	header      frameHeader
	entropy     []byte
	diagnostics []string
}

func (c *Codec) prepareRegion(ctx context.Context, codestream []byte, metadata pixeldata.Metadata) (regionalScan, error) {
	if err := c.validateRegionalMetadata(metadata); err != nil {
		return regionalScan{}, err
	}
	if uint64(len(codestream)) > encapsulated.Defaults().MaxFrameBytes {
		return regionalScan{}, encapsulated.ErrResourceLimit
	}
	pad, padded := encapsulated.TrailingPad(codestream)
	if padded {
		if pad != 0 && len(codestream)%2 != 0 {
			return regionalScan{}, fmt.Errorf("%w: trailing data after EOI", ErrInvalidCodestream)
		}
		codestream = codestream[:len(codestream)-1]
	}
	header, scans, err := splitScansWithHPContext(ctx, codestream, false)
	if err != nil {
		return regionalScan{}, err
	}
	if err := validateSplitFrame(header, metadata, c.nearLossless); err != nil {
		return regionalScan{}, err
	}
	var diagnostics pixeldata.Frames
	if padded && pad != 0 {
		diagnostics.AddDiagnostic(pixeldata.DiagnosticNonZeroFramePad)
	}
	if decoderecovery.From(ctx).JPEGLSJAIThresholds && header.precision > 12 && !header.explicitThresholds {
		header.preset, err = withJAIThresholds(header.preset, header.precision)
		if err != nil {
			return regionalScan{}, err
		}
		diagnostics.AddDiagnostic(pixeldata.DiagnosticJPEGLSJAIThresholds)
	}
	return regionalScan{header: header, entropy: scans[0].entropy, diagnostics: diagnostics.Diagnostics}, ctx.Err()
}

func (s regionalScan) decode(ctx context.Context, region image.Rectangle, bytesPerSample int, dst []byte) error {
	if err := decodeNativeScan(ctx, s.entropy, s.header, s.header.preset, dst, region, 0, bytesPerSample); err != nil {
		return err
	}
	return ctx.Err()
}
