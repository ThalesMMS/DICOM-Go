package imagebuild

import (
	"context"
	"fmt"
	"image"
	"image/color"
	"io"
	"math"
	"strings"

	"github.com/ThalesMMS/dicom-go/core"
	"github.com/ThalesMMS/dicom-go/internal/derivedio"
	"github.com/ThalesMMS/dicom-go/object"
)

// MultiFrameTrueColorSecondaryCaptureStorage is the SOP Class UID of Multi-frame True Color SC Image Storage.
const MultiFrameTrueColorSecondaryCaptureStorage = "1.2.840.10008.5.1.4.1.1.7.4"

const maxRGBCineFrames = 10000

// SecondaryCaptureRGBCineOptions describes a native RGB8 cine instance at a
// fixed frame interval. The RGB options' MaxPixels applies to each frame.
type SecondaryCaptureRGBCineOptions struct {
	SecondaryCaptureRGBOptions
	Width, Height, FrameCount int
	FrameTimeMS               float64
	// ImageTypeDetail is an optional application-specific third Image Type
	// value after DERIVED and SECONDARY.
	ImageTypeDetail string
}

// BuildSecondaryCaptureRGBCineContext constructs metadata and a frame source
// for object.WriteFileFrames. The returned file uses Explicit VR Little Endian,
// and the frame source encodes interleaved RGB8 without per-frame padding.
// Only the final Pixel Data value receives even-length padding.
//
// frameAt is called synchronously with zero-based indexes at write time. It
// must return same-sized RGBA or NRGBA images and keep each image stable until
// the next call or until writing finishes. Alpha is composited as described by SecondaryCaptureRGBOptions.
// The frame source uses one reusable RGB frame buffer and is for serial use.
// The caller controls staging and publication if a producer or writer fails.
func BuildSecondaryCaptureRGBCineContext(ctx context.Context, opts SecondaryCaptureRGBCineOptions, frameAt func(context.Context, int) (image.Image, error)) (*object.File, object.FrameSource, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	if err := ctx.Err(); err != nil {
		return nil, nil, err
	}
	if opts.FrameCount <= 0 || opts.FrameCount > maxRGBCineFrames || frameAt == nil {
		return nil, nil, fmt.Errorf("%w: RGB cine frame count or producer", ErrInvalid)
	}
	length, err := rgbCaptureLength(opts.Width, opts.Height, opts.MaxPixels)
	if err != nil {
		return nil, nil, err
	}
	if length > (math.MaxUint32-1)/int64(opts.FrameCount) {
		return nil, nil, fmt.Errorf("%w: RGB cine Pixel Data exceeds 32-bit length", ErrInvalid)
	}
	if !(opts.FrameTimeMS > 0) || math.IsInf(opts.FrameTimeMS, 0) {
		return nil, nil, fmt.Errorf("%w: RGB cine frame interval must be finite and positive", ErrInvalid)
	}
	detail := opts.ImageTypeDetail
	if len(detail) > 16 || strings.IndexFunc(detail, func(r rune) bool {
		return !(r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' || r == '_' || r == ' ')
	}) >= 0 {
		return nil, nil, fmt.Errorf("%w: RGB cine Image Type detail", ErrInvalid)
	}
	file, err := buildRGBCaptureFile(ctx, opts.Width, opts.Height, MultiFrameTrueColorSecondaryCaptureStorage, opts.SecondaryCaptureRGBOptions)
	if err != nil {
		return nil, nil, err
	}
	if err := file.Dataset.SetIS(derivedio.TagNumberOfFrames, int64(opts.FrameCount)); err != nil {
		return nil, nil, err
	}
	frameTime := core.NewTag(0x0018, 0x1063)
	if err := file.Dataset.SetDS(frameTime, opts.FrameTimeMS); err != nil {
		return nil, nil, err
	}
	file.Dataset.SetAT(core.NewTag(0x0028, 0x0009), frameTime)
	if detail != "" {
		file.Dataset.Put(derivedio.Strings(tagImageType, core.VRCS, []string{"DERIVED", "SECONDARY", detail}))
	}

	return file, &rgbCineSource{
		ctx: ctx, frameAt: frameAt, width: opts.Width, height: opts.Height, count: opts.FrameCount,
		length: length, background: *opts.Background,
	}, nil
}

type rgbCineSource struct {
	ctx                  context.Context
	frameAt              func(context.Context, int) (image.Image, error)
	width, height, count int
	length               int64
	background           color.NRGBA
	pixels               []byte
}

func (s *rgbCineSource) Frames() int { return s.count }

func (s *rgbCineSource) FrameLength(i int) (int64, error) {
	if i < 0 || i >= s.count {
		return 0, fmt.Errorf("%w: RGB cine frame index %d", object.ErrFrameSource, i)
	}
	return s.length, s.ctx.Err()
}

func (s *rgbCineSource) WriteFrameTo(i int, w io.Writer) (int64, error) {
	if _, err := s.FrameLength(i); err != nil {
		return 0, err
	}
	frame, err := s.frameAt(s.ctx, i)
	if err != nil {
		return 0, err
	}
	if err := s.ctx.Err(); err != nil {
		return 0, err
	}
	layout, err := captureRGBLayout(frame, s.length/3)
	if err != nil {
		return 0, fmt.Errorf("RGB cine frame %d: %w", i+1, err)
	}
	if layout.width != s.width || layout.height != s.height {
		return 0, fmt.Errorf("%w: RGB cine frame %d dimensions do not match", object.ErrFrameSource, i+1)
	}
	if s.pixels == nil {
		s.pixels = make([]byte, int(s.length))
	}
	if err := layout.encode(s.ctx, s.background, s.pixels); err != nil {
		return 0, err
	}
	n, err := w.Write(s.pixels)
	if err == nil && n != len(s.pixels) {
		err = io.ErrShortWrite
	}
	return int64(n), err
}
