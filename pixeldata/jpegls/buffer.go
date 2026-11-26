package jpegls

import (
	"context"
	"image"

	"github.com/ThalesMMS/dicom-go/internal/decoderecovery"
	"github.com/ThalesMMS/dicom-go/pixeldata"
	"github.com/ThalesMMS/dicom-go/pixeldata/encapsulated"
)

var _ pixeldata.FrameBufferDecoder = (*Codec)(nil)

// BufferMetadata describes a complete packed little-endian frame or a
// supported monochrome region.
func (c *Codec) BufferMetadata(ctx context.Context, metadata pixeldata.Metadata, request pixeldata.FrameBufferRequest) (pixeldata.Metadata, error) {
	if ctx != nil {
		if err := ctx.Err(); err != nil {
			return pixeldata.Metadata{}, err
		}
	}
	if !request.WholeFrame {
		if !validRegion(metadata, request.Region) {
			return pixeldata.Metadata{}, pixeldata.ErrInvalidRegion
		}
		if err := c.validateRegionalMetadata(metadata); err != nil {
			return pixeldata.Metadata{}, err
		}
		metadata.Rows = uint16(request.Region.Dy())
		metadata.Columns = uint16(request.Region.Dx())
		metadata.NumberOfFrames = 1
		return metadata, nil
	}
	bounds := image.Rect(0, 0, int(metadata.Columns), int(metadata.Rows))
	if request.Region != bounds {
		return pixeldata.Metadata{}, pixeldata.ErrInvalidRegion
	}
	metadata.NumberOfFrames = 1
	if err := c.validateDecodeMetadata(metadata); err != nil {
		return pixeldata.Metadata{}, err
	}
	return metadata, nil
}

// DecodeFrameInto decodes a complete JPEG-LS frame or supported monochrome
// region into caller storage.
// Once scan decoding begins, a failed decode may have written some pixels.
// The reported extent then conservatively covers the requested output.
func (c *Codec) DecodeFrameInto(ctx context.Context, codestream []byte, metadata pixeldata.Metadata, request pixeldata.FrameBufferRequest, dst []byte) (int, []string, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	layout, err := c.BufferMetadata(ctx, metadata, request)
	if err != nil {
		return 0, nil, err
	}
	required := layout.FrameSize()
	if int64(len(dst)) < required {
		return 0, nil, &pixeldata.BufferSizeError{Required: required, Provided: int64(len(dst))}
	}
	if uint64(len(codestream)) > encapsulated.Defaults().MaxFrameBytes {
		return 0, nil, encapsulated.ErrResourceLimit
	}
	dst = dst[:int(required):int(required)]
	if !request.WholeFrame {
		scan, err := c.prepareRegion(ctx, codestream, metadata)
		if err != nil {
			return 0, nil, err
		}
		err = scan.decode(ctx, request.Region, int(metadata.BitsAllocated/8), dst)
		if err != nil {
			return len(dst), nil, err
		}
		return len(dst), scan.diagnostics, nil
	}
	var result pixeldata.Frames
	decoded, err := c.decodeNativeFrame(ctx, codestream, layout, dst, decoderecovery.From(ctx), &result)
	return len(decoded), result.Diagnostics, err
}
