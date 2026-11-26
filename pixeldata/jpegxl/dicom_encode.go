package jpegxl

import (
	"context"
	"encoding/binary"
	"fmt"

	"github.com/ThalesMMS/dicom-go/object"
	"github.com/ThalesMMS/dicom-go/pixeldata"
	"github.com/ThalesMMS/dicom-go/transfer"
)

// LosslessEncoder encodes native frames as JPEG XL Lossless (.110): unsigned
// or signed monochrome and RGB, 1-16 stored bits in 8 or 16 allocated bits.
// It is immutable and safe for concurrent use.
type LosslessEncoder struct{}

var _ pixeldata.FrameEncoder = LosslessEncoder{}

// RegisterLosslessEncoder registers LosslessEncoder for .110.
func RegisterLosslessEncoder(registry pixeldata.EncoderRegistry) error {
	if registry == nil {
		return pixeldata.ErrEncoderRegistryNil
	}
	return registry.RegisterEncoder(transfer.JPEGXLLossless.UID, LosslessEncoder{})
}

// Capabilities describes the frames LosslessEncoder accepts.
func (LosslessEncoder) Capabilities() pixeldata.EncoderCapabilities {
	return pixeldata.EncoderCapabilities{
		TransferSyntaxUID:          transfer.JPEGXLLossless.UID,
		BitsAllocated:              []uint16{8, 16},
		PixelRepresentations:       []uint16{0, 1},
		SamplesPerPixel:            []uint16{1, 3},
		PhotometricInterpretations: []string{"MONOCHROME1", "MONOCHROME2", "RGB"},
		Lossless:                   true,
		SupportsMultiFrame:         true,
		Backend:                    Backend,
	}
}

// EncodeFrame encodes one little-endian, sample-interleaved native frame.
func (LosslessEncoder) EncodeFrame(ctx context.Context, frame []byte, m pixeldata.Metadata) (pixeldata.EncodedFrame, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	if err := ctx.Err(); err != nil {
		return pixeldata.EncodedFrame{}, err
	}
	img, err := frameImage(frame, m, false)
	if err != nil {
		return pixeldata.EncodedFrame{}, err
	}
	data, err := EncodeLossless(img, LosslessOptions{BitsPerSample: int(m.BitsStored)})
	if err != nil {
		return pixeldata.EncodedFrame{}, err
	}
	if err := ctx.Err(); err != nil {
		return pixeldata.EncodedFrame{}, err
	}
	return pixeldata.EncodedFrame{Data: data}, nil
}

// LossyEncoder encodes 8-bit unsigned monochrome and RGB native frames as
// JPEG XL (.112) with EncodeLossy at an explicit quality. Using it is an
// explicit lossy choice: transcoding requires TranscodeOptions.AllowLossy.
type LossyEncoder struct {
	quality int
}

var _ pixeldata.FrameEncoder = (*LossyEncoder)(nil)

// NewLossyEncoder returns a .112 encoder with quality 1-100.
func NewLossyEncoder(quality int) (*LossyEncoder, error) {
	if quality < MinLossyQuality || quality > MaxLossyQuality {
		return nil, fmt.Errorf("%w: quality %d outside 1..100", ErrUnsupported, quality)
	}
	return &LossyEncoder{quality: quality}, nil
}

// RegisterLossyEncoder registers a LossyEncoder for .112.
func RegisterLossyEncoder(registry pixeldata.EncoderRegistry, quality int) error {
	if registry == nil {
		return pixeldata.ErrEncoderRegistryNil
	}
	encoder, err := NewLossyEncoder(quality)
	if err != nil {
		return err
	}
	return registry.RegisterEncoder(transfer.JPEGXL.UID, encoder)
}

// Quality reports the configured quality.
func (e *LossyEncoder) Quality() int {
	if e == nil {
		return 0
	}
	return e.quality
}

// Capabilities describes the frames LossyEncoder accepts.
func (*LossyEncoder) Capabilities() pixeldata.EncoderCapabilities {
	return pixeldata.EncoderCapabilities{
		TransferSyntaxUID:          transfer.JPEGXL.UID,
		BitsAllocated:              []uint16{8},
		PixelRepresentations:       []uint16{0},
		SamplesPerPixel:            []uint16{1, 3},
		PhotometricInterpretations: []string{"MONOCHROME1", "MONOCHROME2", "RGB"},
		LossyMethod:                LossyMethod,
		SupportsMultiFrame:         true,
		Backend:                    Backend,
	}
}

// EncodeFrame encodes one native frame.
func (e *LossyEncoder) EncodeFrame(ctx context.Context, frame []byte, m pixeldata.Metadata) (pixeldata.EncodedFrame, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	if err := ctx.Err(); err != nil {
		return pixeldata.EncodedFrame{}, err
	}
	if e == nil || e.quality < MinLossyQuality || e.quality > MaxLossyQuality {
		return pixeldata.EncodedFrame{}, fmt.Errorf("%w: invalid quality", ErrUnsupported)
	}
	if m.BitsAllocated != 8 || m.BitsStored != 8 || m.PixelRepresentation != 0 {
		return pixeldata.EncodedFrame{}, &pixeldata.UnsupportedEncoderMetadataError{Field: "BitsStored"}
	}
	img, err := frameImage(frame, m, true)
	if err != nil {
		return pixeldata.EncodedFrame{}, err
	}
	data, err := EncodeLossy(img, LossyOptions{Quality: e.quality})
	if err != nil {
		return pixeldata.EncodedFrame{}, err
	}
	if err := ctx.Err(); err != nil {
		return pixeldata.EncodedFrame{}, err
	}
	return pixeldata.EncodedFrame{Data: data}, nil
}

// frameImage converts a native frame to unsigned codestream samples, masking
// bits above Bits Stored and level-shifting signed samples.
func frameImage(frame []byte, m pixeldata.Metadata, lossy bool) (*Image, error) {
	field := ""
	switch {
	case m.Rows == 0:
		field = "Rows"
	case m.Columns == 0:
		field = "Columns"
	case m.BitsAllocated != 8 && m.BitsAllocated != 16:
		field = "BitsAllocated"
	case m.BitsStored == 0 || m.BitsStored > m.BitsAllocated:
		field = "BitsStored"
	case m.HighBit != m.BitsStored-1:
		field = "HighBit"
	case m.PixelRepresentation > 1 || m.PixelRepresentation == 1 && m.SamplesPerPixel != 1:
		field = "PixelRepresentation"
	case m.SamplesPerPixel != 1 && m.SamplesPerPixel != 3:
		field = "SamplesPerPixel"
	case m.SamplesPerPixel == 3 && (!m.PlanarConfigurationPresent || m.PlanarConfiguration != 0):
		field = "PlanarConfiguration"
	}
	photometric := normalizedPhotometric(m.PhotometricInterpretation)
	if field == "" {
		switch {
		case m.SamplesPerPixel == 1 && photometric != "MONOCHROME1" && photometric != "MONOCHROME2",
			m.SamplesPerPixel == 3 && photometric != "RGB":
			field = "PhotometricInterpretation"
		}
	}
	if field != "" {
		return nil, &pixeldata.UnsupportedEncoderMetadataError{Field: field}
	}
	if int64(len(frame)) != m.FrameSize() {
		return nil, fmt.Errorf("%w: native frame length", pixeldata.ErrPixelDataSizeMismatch)
	}
	n := int(m.Rows) * int(m.Columns) * int(m.SamplesPerPixel)
	img := &Image{Width: int(m.Columns), Height: int(m.Rows), Channels: int(m.SamplesPerPixel), BitsPerSample: int(m.BitsStored), Samples: make([]int32, n)}
	mask := uint32(1)<<m.BitsStored - 1
	signBit := uint32(1) << (m.BitsStored - 1)
	for i := range img.Samples {
		var raw uint32
		if m.BitsAllocated == 8 {
			raw = uint32(frame[i])
		} else {
			raw = uint32(binary.LittleEndian.Uint16(frame[2*i:]))
		}
		raw &= mask
		if m.PixelRepresentation == 1 {
			raw ^= signBit // two's complement to offset binary
		}
		img.Samples[i] = int32(raw)
	}
	return img, nil
}

// Recompressor is a pixeldata.CodestreamTranscoder for reversible JPEG
// recompression: JPEG Baseline (.50) and Extended (.51) frames become JPEG
// XL JPEG Recompression (.111) frames with RecompressJPEG, and .111 frames
// become the original JPEG with ReconstructJPEG (SOF0 frames as .50, SOF1
// frames as .51). Pixels are never decoded, so the conversion is exact in
// both directions; a JPEG that cannot be restored byte for byte (progressive,
// 12-bit, unusual sampling) fails with ErrNotReversible or ErrUnsupported.
type Recompressor struct{}

var _ pixeldata.CodestreamTranscoder = Recompressor{}

// CanTranscodeCodestream reports the supported directions.
func (Recompressor) CanTranscodeCodestream(sourceUID, targetUID string) bool {
	jpegUID := sourceUID
	if sourceUID == transfer.JPEGXLJPEGRecompression.UID {
		jpegUID = targetUID
	} else if targetUID != transfer.JPEGXLJPEGRecompression.UID {
		return false
	}
	return jpegUID == transfer.JPEGBaseline.UID || jpegUID == transfer.JPEGExtended.UID
}

// TranscodeCodestreams converts every frame of pixel.
func (r Recompressor) TranscodeCodestreams(ctx context.Context, sourceUID, targetUID string, pixel pixeldata.PixelData, obj *object.Object) ([][]byte, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	if !r.CanTranscodeCodestream(sourceUID, targetUID) {
		return nil, fmt.Errorf("%w: %s to %s", ErrUnsupported, sourceUID, targetUID)
	}
	if !pixel.Encapsulated {
		return nil, fmt.Errorf("%w: encapsulated pixel data required", pixeldata.ErrIncompatiblePixelData)
	}
	m, err := pixeldata.ExtractMetadata(obj)
	if err != nil {
		return nil, err
	}
	if m.NumberOfFrames <= 0 {
		return nil, fmt.Errorf("%w: NumberOfFrames=%d", pixeldata.ErrInvalidMetadata, m.NumberOfFrames)
	}
	payloads, err := framePayloads(pixel.Sequence, m.NumberOfFrames)
	if err != nil {
		return nil, err
	}
	out := make([][]byte, len(payloads))
	for i, payload := range payloads {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		if targetUID == transfer.JPEGXLJPEGRecompression.UID {
			out[i], err = RecompressJPEG(trimJPEGPad(payload))
		} else {
			out[i], err = reconstructFor(payload, targetUID)
		}
		if err != nil {
			return nil, fmt.Errorf("frame %d: %w", i, err)
		}
	}
	return out, nil
}

// reconstructFor restores a JPEG and checks its process against the target.
func reconstructFor(payload []byte, targetUID string) ([]byte, error) {
	jpg, err := ReconstructJPEG(payload)
	if err != nil {
		return nil, err
	}
	p, err := parseJPEG(jpg)
	if err != nil {
		return nil, err
	}
	want := byte(0xC0)
	if targetUID == transfer.JPEGExtended.UID {
		want = 0xC1
	}
	if p.sofMarker != want {
		return nil, fmt.Errorf("%w: restored JPEG uses SOF%X, not the target's SOF%X", ErrUnsupported, p.sofMarker&0xF, want&0xF)
	}
	return jpg, nil
}

// trimJPEGPad drops one Item pad byte after the EOI marker.
func trimJPEGPad(payload []byte) []byte {
	n := len(payload)
	if n >= 3 && payload[n-1] == 0 && payload[n-3] == 0xFF && payload[n-2] == 0xD9 {
		return payload[:n-1]
	}
	return payload
}
