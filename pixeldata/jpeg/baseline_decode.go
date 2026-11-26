package jpeg

import (
	"bytes"
	"context"
	"encoding/binary"
	"fmt"
	"image"
	stdjpeg "image/jpeg"
	"io"

	"github.com/ThalesMMS/dicom-go/pixeldata"
)

// baselineHeader augments the existing SOF admission walk with dimensions.
// It does not interpret quantization/Huffman tables, APP color semantics, or
// entropy data. The standard decoder remains the authority for those fields.
type baselineHeader struct {
	sof           byte
	width, height int
	admitted      bool
}

func probeBaselineHeader(ctx context.Context, stream []byte) (baselineHeader, error) {
	if len(stream) < 4 || stream[0] != 0xff || stream[1] != 0xd8 {
		return baselineHeader{}, fmt.Errorf("missing JPEG SOI")
	}
	for pos := 2; pos < len(stream); {
		if err := ctx.Err(); err != nil {
			return baselineHeader{}, err
		}
		if stream[pos] != 0xff {
			return baselineHeader{}, fmt.Errorf("expected JPEG marker at byte %d", pos)
		}
		for pos < len(stream) && stream[pos] == 0xff {
			if pos%4096 == 0 {
				if err := ctx.Err(); err != nil {
					return baselineHeader{}, err
				}
			}
			pos++
		}
		if pos >= len(stream) || stream[pos] == 0 {
			return baselineHeader{}, fmt.Errorf("truncated or stuffed JPEG marker")
		}
		marker := stream[pos]
		pos++
		switch marker {
		case 0xc0, 0xc1, 0xc2, 0xc3, 0xc5, 0xc6, 0xc7, 0xc9, 0xca, 0xcb, 0xcd, 0xce, 0xcf:
			header := baselineHeader{sof: marker}
			// Truncated/unusual SOF shapes go to DecodeConfig's historical
			// refusal path. Never use incomplete dimensions for admission.
			if marker != 0xc0 || pos+2 > len(stream) {
				return header, nil
			}
			length := int(binary.BigEndian.Uint16(stream[pos:]))
			if length < 8 || length > len(stream)-pos {
				return header, nil
			}
			sof := stream[pos+2 : pos+length]
			components := int(sof[5])
			if sof[0] != 8 || (components != 1 && components != 3) || len(sof) != 6+3*components {
				return header, nil
			}
			header.height = int(binary.BigEndian.Uint16(sof[1:3]))
			header.width = int(binary.BigEndian.Uint16(sof[3:5]))
			header.admitted = header.width > 0 && header.height > 0
			return header, nil
		case 0xd8, 0xd9, 0xda:
			return baselineHeader{}, fmt.Errorf("JPEG frame has no SOF marker")
		case 0xd0, 0xd1, 0xd2, 0xd3, 0xd4, 0xd5, 0xd6, 0xd7:
			return baselineHeader{}, fmt.Errorf("JPEG restart marker before SOF")
		}
		if pos+2 > len(stream) {
			return baselineHeader{}, fmt.Errorf("truncated JPEG segment length")
		}
		length := int(binary.BigEndian.Uint16(stream[pos:]))
		if length < 2 || length > len(stream)-pos {
			return baselineHeader{}, fmt.Errorf("invalid JPEG segment length")
		}
		pos += length
	}
	return baselineHeader{}, fmt.Errorf("JPEG frame has no SOF marker")
}

type baselineContextReader struct {
	ctx context.Context
	r   io.Reader
}

func (r baselineContextReader) Read(p []byte) (int, error) {
	if err := r.ctx.Err(); err != nil {
		return 0, err
	}
	return r.r.Read(p)
}

// decodeBaselineImage uses one standard header/decode pass on the matching
// common SOF0 path. Dimensions are admitted before standard image allocation.
// A mismatching/unusual header uses the old DecodeConfig gate. On a malformed
// decode, a config-only retry preserves historical error classification; it is
// never used to retry pixel decoding or to authorize a different image size.
func decodeBaselineImage(ctx context.Context, stream []byte, metadata pixeldata.Metadata, header baselineHeader, frameIndex int) (image.Image, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	checkedConfig := false
	if !header.admitted || header.width != int(metadata.Columns) || header.height != int(metadata.Rows) {
		config, err := stdjpeg.DecodeConfig(baselineContextReader{ctx, bytes.NewReader(stream)})
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		if err != nil {
			return nil, fmt.Errorf("%w: %w", ErrInvalidFragment, err)
		}
		if config.Width != int(metadata.Columns) || config.Height != int(metadata.Rows) {
			return nil, ErrImageSizeMismatch
		}
		checkedConfig = true
	}
	img, err := stdjpeg.Decode(baselineContextReader{ctx, bytes.NewReader(stream)})
	if ctx.Err() != nil {
		return nil, ctx.Err()
	}
	if err != nil {
		if !checkedConfig {
			_, configErr := stdjpeg.DecodeConfig(baselineContextReader{ctx, bytes.NewReader(stream)})
			if ctx.Err() != nil {
				return nil, ctx.Err()
			}
			if configErr != nil {
				return nil, fmt.Errorf("%w: %w", ErrInvalidFragment, configErr)
			}
		}
		return nil, fmt.Errorf("%w: frame %d: %w", ErrInvalidFragment, frameIndex, err)
	}
	return img, nil
}

func baselineImageToFrameBytes(ctx context.Context, img image.Image, metadata pixeldata.Metadata, policy pixeldata.JPEGColorConversion) ([]byte, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	gray, ok := img.(*image.Gray)
	if !ok || metadata.SamplesPerPixel != 1 {
		out, err := imageToFrameBytesPolicy(img, metadata, policy)
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		return out, err
	}
	if gray == nil {
		return nil, fmt.Errorf("%w: decoded image is nil", ErrInvalidFragment)
	}
	bounds := gray.Bounds()
	rows, columns := int(metadata.Rows), int(metadata.Columns)
	if bounds.Dx() != columns || bounds.Dy() != rows {
		return nil, fmt.Errorf("%w: got %dx%d want %dx%d", ErrImageSizeMismatch, bounds.Dx(), bounds.Dy(), columns, rows)
	}
	// Validate the complete visible extent before allocating/copying. Rows of
	// a subimage can include padding or a nonzero origin; those bytes are not
	// part of the output. The output is always owned, never Gray.Pix itself.
	if rows <= 0 || columns <= 0 || gray.Stride < columns || len(gray.Pix) < columns || (rows-1) > (len(gray.Pix)-columns)/gray.Stride {
		return nil, fmt.Errorf("%w: decoded grayscale layout", ErrInvalidFragment)
	}
	out := make([]byte, rows*columns)
	for y := 0; y < rows; y++ {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		source := gray.PixOffset(bounds.Min.X, bounds.Min.Y+y)
		copy(out[y*columns:(y+1)*columns], gray.Pix[source:source+columns])
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	return out, nil
}
