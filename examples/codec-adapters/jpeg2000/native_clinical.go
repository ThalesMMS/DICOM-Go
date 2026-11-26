package jpeg2000

import (
	"bytes"
	"context"
	"encoding/binary"

	"github.com/ThalesMMS/dicom-go/pixeldata"
)

type clinicalDecoder struct {
	lookup func(NativeBackend) *NativeSession
	engine NativeBackend
	cli    ContextDecoder
}

func (d clinicalDecoder) DecodeFrame(b []byte, metadata pixeldata.Metadata) ([]byte, error) {
	return d.DecodeFrameContext(context.Background(), b, metadata)
}

func (d clinicalDecoder) DecodeFrameContext(ctx context.Context, b []byte, metadata pixeldata.Metadata) ([]byte, error) {
	if session := d.lookup(d.engine); session != nil && session.cfg.Backend == d.engine && session.clinicalFrame(b, metadata) {
		return session.DecodeFrameContext(ctx, b, metadata)
	}
	return d.cli.DecodeFrameContext(ctx, b, metadata)
}

func (s *NativeSession) clinicalFrame(b []byte, m pixeldata.Metadata) bool {
	if m.PixelRepresentation != 0 || (m.BitsStored != 8 && m.BitsStored != 12 && m.BitsStored != 16) {
		return false
	}
	if uint64(m.FrameSize()) > uint64(s.cfg.MaxDecodedBytes) || uint64(len(b)) > uint64(s.cfg.MaxEncodedBytes) {
		return false
	}
	// Only raw codestreams with a complete SIZ marker and full-resolution
	// components belong to this profile. The native decoder still verifies
	// the metadata and codestream independently before decoding.
	const componentOffset = 42
	samples := int(m.SamplesPerPixel)
	end := componentOffset + 3*samples
	if (samples != 1 && samples != 3) || len(b) < end ||
		!bytes.Equal(b[:4], []byte{0xff, 0x4f, 0xff, 0x51}) ||
		int(binary.BigEndian.Uint16(b[4:6])) != end-4 ||
		int(binary.BigEndian.Uint16(b[40:42])) != samples {
		return false
	}
	for component := componentOffset; component < end; component += 3 {
		// Preserve the existing CLI handling of data such as the legacy
		// multiframe MR fixture with 16-bit coded and 12-bit declared samples.
		if b[component] != byte(m.BitsStored-1) || b[component+1] != 1 || b[component+2] != 1 {
			return false
		}
	}
	return !isPart2Codestream(b) && (s.cfg.Backend == NativeOpenJPH) == isHTJ2KCodestream(b)
}
