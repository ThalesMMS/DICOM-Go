package video

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"

	"github.com/ThalesMMS/dicom-go/object"
)

// ExportFragmentedMP4 writes the H.264 or HEVC stream of a DICOM video object
// as fragmented MP4, the form Media Source Extensions and streaming players
// accept. The encapsulated stream must be an MPEG-2 Transport Stream (its
// first program's video elementary stream is demultiplexed) or a raw Annex B
// elementary stream; an object that already holds MP4 is refused with
// ErrUnsupportedContainer, since Copy extracts it unchanged. A zero
// opts.FrameRate takes the object's Frame Time, Recommended Display Frame
// Rate, Cine Rate, Frame Time Vector or Actual Frame Duration; opts.Codec is
// taken from the transfer syntax. The source object is not modified.
func ExportFragmentedMP4(ctx context.Context, file *object.File, dst io.Writer, opts MuxOptions) (MuxInfo, Info, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	if dst == nil {
		return MuxInfo{}, Info{}, errors.New("dicom: video destination is nil")
	}
	var media bytes.Buffer
	info, err := Copy(ctx, file, &media, opts.Limits)
	if err != nil && !errors.Is(err, ErrUnsupportedContainer) {
		return MuxInfo{}, info, err
	}
	if info.Codec != CodecH264 && info.Codec != CodecHEVC {
		return MuxInfo{}, info, fmt.Errorf("%w: MP4 export supports H.264 and HEVC, not %s", ErrUnsupportedContainer, info.Codec)
	}
	stream := media.Bytes()
	switch {
	case err == nil && info.Container == ContainerMPEGTransport:
		if stream, err = demuxTransportStream(ctx, stream, info.Codec); err != nil {
			return MuxInfo{}, info, err
		}
	case err == nil:
		return MuxInfo{}, info, fmt.Errorf("%w: the object already holds %s; use Copy", ErrUnsupportedContainer, info.Container)
	case !isAnnexB(stream):
		return MuxInfo{}, info, err
	default:
		info.Container, info.Extension, info.MIMEType = ContainerAnnexB, ".h264", "video/h264"
		if info.Codec == CodecHEVC {
			info.Extension, info.MIMEType = ".h265", "video/h265"
		}
	}
	opts.Codec = info.Codec
	if opts.FrameRate <= 0 {
		opts.FrameRate = info.FrameRate
	}
	mux, err := MuxFragmentedMP4(ctx, dst, stream, opts)
	return mux, info, err
}

// ContainerAnnexB is a raw H.264/HEVC Annex B elementary stream, which
// ExportFragmentedMP4 accepts although PS3.5 expects MP4 or MPEG-TS.
const ContainerAnnexB Container = "Annex B"

func isAnnexB(data []byte) bool {
	return bytes.HasPrefix(data, []byte{0, 0, 1}) || bytes.HasPrefix(data, []byte{0, 0, 0, 1})
}
