package video

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"github.com/ThalesMMS/dicom-go/transfer"
	"math"
)

var ErrImportMedia = errors.New("dicom: malformed or unsupported video import")

// ImportLimits limits metadata/container work and projected native spans. No
// video decoding occurs. Zero selects safe defaults; negative fields refuse.
type ImportLimits struct {
	MaxBytes, MaxPixels, MaxDecodedBytes int64
	MaxFrames                            int
}

// SourceInfo is derived from validated container tables and coding headers,
// without reconstructing or verifying coded pictures.
type SourceInfo struct {
	Rows, Columns, Frames int
	FrameRate             float64
	TransferSyntax        transfer.Syntax
	Container             Container
	CompressionMethod     string
}

func (l ImportLimits) normalize() (ImportLimits, error) {
	if l.MaxBytes < 0 || l.MaxBytes > 1<<30 || l.MaxPixels < 0 || l.MaxDecodedBytes < 0 || l.MaxFrames < 0 || l.MaxFrames > 100000 {
		return l, ErrImportMedia
	}
	if l.MaxBytes == 0 {
		l.MaxBytes = 256 << 20
	}
	if l.MaxPixels == 0 {
		l.MaxPixels = 16 << 20
	}
	if l.MaxDecodedBytes == 0 {
		l.MaxDecodedBytes = 512 << 20
	}
	if l.MaxFrames == 0 {
		l.MaxFrames = 1024
	}
	return l, nil
}

// InspectSource checks a single AVC MP4 or MPEG-2 Program Stream for import.
// Fragmented/edited/rotated/multiple-track MP4, changing SPS, field pictures and
// unsupported profiles refuse explicitly. Payloads remain untouched; successful
// metadata inspection is not video codec decode acceptance.
func InspectSource(ctx context.Context, data []byte, limits ImportLimits) (SourceInfo, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	if e := ctx.Err(); e != nil {
		return SourceInfo{}, e
	}
	limits, e := limits.normalize()
	if e != nil {
		return SourceInfo{}, e
	}
	if int64(len(data)) > limits.MaxBytes {
		return SourceInfo{}, ErrMediaTooLarge
	}
	var info SourceInfo
	switch {
	case len(data) >= 12 && string(data[4:8]) == "ftyp":
		info, e = inspectMP4(ctx, data, limits)
	case len(data) >= 4 && bytes.Equal(data[:4], []byte{0, 0, 1, 0xba}):
		info, e = inspectMPEG(ctx, data, limits)
	default:
		e = ErrUnsupportedContainer
	}
	if e != nil {
		return SourceInfo{}, e
	}
	if info.Rows < 1 || info.Columns < 1 || info.Rows > 65535 || info.Columns > 65535 || info.Frames < 1 || info.Frames > limits.MaxFrames || !(info.FrameRate > 0) || math.IsInf(info.FrameRate, 0) || info.FrameRate > 1000 {
		return SourceInfo{}, ErrImportMedia
	}
	pixels := int64(info.Rows) * int64(info.Columns)
	if pixels > limits.MaxPixels || pixels > limits.MaxDecodedBytes/3/int64(info.Frames) {
		return SourceInfo{}, ErrMediaTooLarge
	}
	if e = ctx.Err(); e != nil {
		return SourceInfo{}, e
	}
	return info, nil
}
func importError(what string) error { return fmt.Errorf("%w: %s", ErrImportMedia, what) }
