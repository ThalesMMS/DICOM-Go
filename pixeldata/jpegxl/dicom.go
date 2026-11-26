package jpegxl

import (
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"strings"

	"github.com/ThalesMMS/dicom-go/core"
	"github.com/ThalesMMS/dicom-go/object"
	"github.com/ThalesMMS/dicom-go/pixeldata"
	"github.com/ThalesMMS/dicom-go/pixeldata/jpeg"
	"github.com/ThalesMMS/dicom-go/transfer"
)

// DICOM integration of the pure-Go JPEG XL profile (PS3.5 A.4.12):
//
//   - Codec decodes JPEG XL Lossless (.110), JPEG XL JPEG Recompression
//     (.111) and JPEG XL (.112) frames within this package's profile.
//   - LosslessEncoder and LossyEncoder encode native frames as .110 and .112.
//   - Recompressor converts JPEG Baseline/Extended frames to .111 and back
//     without decoding pixels (pixeldata.CodestreamTranscoder).
//
// Nothing here registers itself; callers opt in explicitly.
//
// Signed samples (Pixel Representation 1, monochrome) are stored in the
// codestream with an offset-binary level shift: stored = value +
// 2^(BitsStored-1), so the codestream holds unsigned integers of BitsStored
// bits. Decoding subtracts the same offset.

// LossyMethod is the Lossy Image Compression Method (0028,2114) defined term
// for JPEG XL (ISO/IEC 18181-1).
const LossyMethod = "ISO_18181_1"

// Backend names this package in encoder capabilities.
const Backend = "dicom-go-jpegxl"

// UIDs lists the transfer syntaxes Codec decodes.
func UIDs() []string {
	return []string{transfer.JPEGXLLossless.UID, transfer.JPEGXLJPEGRecompression.UID, transfer.JPEGXL.UID}
}

// ErrFragmentLayout reports encapsulated fragments that cannot be grouped
// into the declared number of frames.
var ErrFragmentLayout = errors.New("jpegxl: unsupported fragment layout")

// Codec decodes DICOM JPEG XL pixel data. Frames with a JPEG bitstream
// reconstruction box are restored to their original JPEG and decoded by the
// JPEG codec, so a .111 frame yields the same pixels as its source .50/.51
// frame. Other frames are decoded with Decode: modular frames exactly and
// JPEG-compatible VarDCT frames as a JPEG decoder renders them. The .112
// registration additionally decodes the bounded sRGB XYB DCT8 profile.
type Codec struct {
	syntaxUID string
}

var _ pixeldata.ContextCodec = (*Codec)(nil)

// New returns a JPEG XL decoder.
func New() *Codec { return &Codec{} }

// Register registers Codec for the three JPEG XL transfer syntaxes.
func Register(registry pixeldata.Registry) error {
	if registry == nil {
		return pixeldata.ErrCodecRegistryNil
	}
	for _, uid := range UIDs() {
		if err := registry.RegisterCodec(uid, &Codec{syntaxUID: uid}); err != nil {
			return err
		}
	}
	return nil
}

// Decode decodes every frame (see DecodeContext).
func (c *Codec) Decode(pixel pixeldata.PixelData, obj *object.Object) (pixeldata.Frames, error) {
	return c.DecodeContext(context.Background(), pixel, obj)
}

// DecodeContext decodes every frame into native little-endian,
// sample-interleaved bytes. Colour frames decode to RGB.
func (c *Codec) DecodeContext(ctx context.Context, pixel pixeldata.PixelData, obj *object.Object) (pixeldata.Frames, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	if err := ctx.Err(); err != nil {
		return pixeldata.Frames{}, err
	}
	if !pixel.Encapsulated {
		return pixeldata.Frames{}, fmt.Errorf("%w: JPEG XL requires encapsulated pixel data", pixeldata.ErrIncompatiblePixelData)
	}
	metadata, err := pixeldata.ExtractMetadata(obj)
	if err != nil {
		return pixeldata.Frames{}, err
	}
	if err := validateDecodeMetadata(metadata); err != nil {
		return pixeldata.Frames{}, err
	}
	const maxRequestBytes = int64(512 << 20)
	if frameSize := metadata.FrameSize(); frameSize <= 0 || frameSize > maxRequestBytes/int64(metadata.NumberOfFrames) {
		return pixeldata.Frames{}, fmt.Errorf("%w: decoded request exceeds resource limit", ErrLimit)
	}
	payloads, err := framePayloads(pixel.Sequence, metadata.NumberOfFrames)
	if err != nil {
		return pixeldata.Frames{}, err
	}
	result := pixeldata.Frames{Rows: int(metadata.Rows), Columns: int(metadata.Columns), Data: make([][]byte, len(payloads))}
	allowXYB := c.syntaxUID == "" || c.syntaxUID == transfer.JPEGXL.UID
	var jpegFrames [][]byte
	for i, payload := range payloads {
		if err := ctx.Err(); err != nil {
			return pixeldata.Frames{}, err
		}
		if hasReconstructionData(payload) {
			jpg, err := ReconstructJPEG(payload)
			if err != nil {
				return pixeldata.Frames{}, fmt.Errorf("frame %d: %w", i, err)
			}
			jpegFrames = append(jpegFrames, jpg)
			continue
		}
		if jpegFrames != nil {
			return pixeldata.Frames{}, fmt.Errorf("%w: frames mix JPEG reconstruction and pixel codestreams", ErrUnsupported)
		}
		frame, photometric, err := decodePixelFrame(ctx, payload, metadata, allowXYB)
		if err != nil {
			return pixeldata.Frames{}, fmt.Errorf("frame %d: %w", i, err)
		}
		result.Data[i] = frame
		result.PhotometricInterpretation = photometric
	}
	if jpegFrames == nil {
		return result, nil
	}
	if len(jpegFrames) != len(payloads) {
		return pixeldata.Frames{}, fmt.Errorf("%w: frames mix JPEG reconstruction and pixel codestreams", ErrUnsupported)
	}
	return decodeReconstructedJPEG(ctx, jpegFrames, obj)
}

func validateDecodeMetadata(m pixeldata.Metadata) error {
	if m.Rows == 0 || m.Columns == 0 || m.NumberOfFrames <= 0 {
		return fmt.Errorf("%w: Rows=%d Columns=%d NumberOfFrames=%d", pixeldata.ErrInvalidMetadata, m.Rows, m.Columns, m.NumberOfFrames)
	}
	if m.BitsAllocated != 8 && m.BitsAllocated != 16 {
		return fmt.Errorf("%w: BitsAllocated=%d", ErrUnsupported, m.BitsAllocated)
	}
	if m.BitsStored == 0 || m.BitsStored > m.BitsAllocated || m.HighBit != m.BitsStored-1 {
		return fmt.Errorf("%w: BitsStored=%d HighBit=%d", pixeldata.ErrInvalidMetadata, m.BitsStored, m.HighBit)
	}
	if m.SamplesPerPixel != 1 && m.SamplesPerPixel != 3 {
		return fmt.Errorf("%w: SamplesPerPixel=%d", ErrUnsupported, m.SamplesPerPixel)
	}
	if m.PixelRepresentation > 1 || m.PixelRepresentation == 1 && m.SamplesPerPixel != 1 {
		return fmt.Errorf("%w: PixelRepresentation=%d", pixeldata.ErrUnsupportedPixelRepresentation, m.PixelRepresentation)
	}
	if m.PlanarConfigurationPresent && m.PlanarConfiguration != 0 {
		return fmt.Errorf("%w: PlanarConfiguration=%d", pixeldata.ErrUnsupportedPlanarConfiguration, m.PlanarConfiguration)
	}
	return nil
}

// decodePixelFrame decodes one frame with Decode and lays it out natively.
func decodePixelFrame(ctx context.Context, payload []byte, m pixeldata.Metadata, allowXYB bool) ([]byte, string, error) {
	if err := checkFrameInfo(payload, m); err != nil {
		return nil, "", err
	}
	img, err := decode(ctx, payload, allowXYB)
	if err != nil {
		return nil, "", err
	}
	if img.Width != int(m.Columns) || img.Height != int(m.Rows) || img.Channels != int(m.SamplesPerPixel) {
		return nil, "", fmt.Errorf("%w: codestream %dx%dx%d, metadata %dx%dx%d", pixeldata.ErrPixelDataSizeMismatch,
			img.Width, img.Height, img.Channels, m.Columns, m.Rows, m.SamplesPerPixel)
	}
	if img.BitsPerSample > int(m.BitsStored) {
		return nil, "", fmt.Errorf("%w: codestream has %d bits per sample, BitsStored=%d", ErrUnsupported, img.BitsPerSample, m.BitsStored)
	}
	bytesPerSample := int(m.BitsAllocated / 8)
	out := make([]byte, len(img.Samples)*bytesPerSample)
	offset := int32(0)
	if m.PixelRepresentation == 1 {
		offset = 1 << (m.BitsStored - 1)
	}
	for i, v := range img.Samples {
		if i&4095 == 0 {
			if err := ctx.Err(); err != nil {
				return nil, "", err
			}
		}
		v -= offset
		if bytesPerSample == 1 {
			out[i] = byte(v)
		} else {
			binary.LittleEndian.PutUint16(out[2*i:], uint16(v))
		}
	}
	photometric := ""
	if m.SamplesPerPixel == 3 && normalizedPhotometric(m.PhotometricInterpretation) != "RGB" {
		photometric = "RGB"
	}
	return out, photometric, ctx.Err()
}

// checkFrameInfo compares the codestream size header with the metadata
// before any frame data is decoded.
func checkFrameInfo(payload []byte, m pixeldata.Metadata) error {
	c, err := parseContainer(payload)
	if err != nil {
		return err
	}
	hdr, err := readCodestreamHeader(newBitReader(c.codestream))
	if err != nil {
		return err
	}
	if hdr.meta.width != int(m.Columns) || hdr.meta.height != int(m.Rows) {
		return fmt.Errorf("%w: codestream %dx%d, metadata %dx%d", pixeldata.ErrPixelDataSizeMismatch, hdr.meta.width, hdr.meta.height, m.Columns, m.Rows)
	}
	return nil
}

func hasReconstructionData(payload []byte) bool {
	c, err := parseContainer(payload)
	return err == nil && c.jbrd != nil
}

// decodeReconstructedJPEG decodes restored JPEG frames with the JPEG codec,
// using obj's Image Pixel metadata without its JPEG XL offset tables.
func decodeReconstructedJPEG(ctx context.Context, frames [][]byte, obj *object.Object) (pixeldata.Frames, error) {
	skip := map[core.Tag]bool{
		core.NewTag(0x7FE0, 0x0001): true, core.NewTag(0x7FE0, 0x0002): true,
		core.NewTag(0x7FE0, 0x0003): true, core.TagPixelData: true,
	}
	var elements []core.Element
	for _, e := range obj.Elements() {
		if !skip[e.Header.Tag] {
			elements = append(elements, e)
		}
	}
	view := object.FromElements(elements, nil)
	view.SetValueByteOrder(obj.ValueByteOrder())
	pixel := pixeldata.PixelData{Encapsulated: true, Sequence: core.FragmentSequence{Fragments: frames}}
	return jpeg.New().DecodeContext(ctx, pixel, view)
}

// framePayloads groups fragments into frames: one fragment per frame, all
// fragments of a single frame, or the Basic Offset Table's grouping.
func framePayloads(seq core.FragmentSequence, frames int) ([][]byte, error) {
	fragments := seq.Fragments
	switch {
	case len(fragments) == 0:
		return nil, fmt.Errorf("%w: no fragments", ErrFragmentLayout)
	case len(fragments) == frames:
		return fragments, nil
	case frames == 1:
		return [][]byte{bytes.Join(fragments, nil)}, nil
	case len(seq.OffsetTable) != 4*frames:
		return nil, fmt.Errorf("%w: %d fragments for %d frames without a Basic Offset Table", ErrFragmentLayout, len(fragments), frames)
	}
	starts := make(map[uint32]int, len(fragments))
	offset := uint64(0)
	for i, f := range fragments {
		if offset > uint64(^uint32(0)) {
			return nil, fmt.Errorf("%w: offsets exceed 32 bits", ErrFragmentLayout)
		}
		starts[uint32(offset)] = i
		offset += 8 + uint64(len(f)) + uint64(len(f)&1)
	}
	first := make([]int, frames+1)
	for i := 0; i < frames; i++ {
		idx, ok := starts[binary.LittleEndian.Uint32(seq.OffsetTable[4*i:])]
		if !ok || i > 0 && idx <= first[i-1] || i == 0 && idx != 0 {
			return nil, fmt.Errorf("%w: Basic Offset Table entry %d", ErrFragmentLayout, i)
		}
		first[i] = idx
	}
	first[frames] = len(fragments)
	out := make([][]byte, frames)
	for i := range out {
		out[i] = bytes.Join(fragments[first[i]:first[i+1]], nil)
	}
	return out, nil
}

func normalizedPhotometric(s string) string {
	return strings.ToUpper(strings.TrimSpace(s))
}
