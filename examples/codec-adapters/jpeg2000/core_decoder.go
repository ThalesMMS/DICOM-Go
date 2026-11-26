package jpeg2000

import (
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"math"
	"strings"

	"github.com/ThalesMMS/dicom-go/examples/codec-adapters/jpeg2000/internal/j2kcore"
	"github.com/ThalesMMS/dicom-go/object"
	"github.com/ThalesMMS/dicom-go/pixeldata"
	"github.com/ThalesMMS/dicom-go/pixeldata/encapsulated"
	"github.com/ThalesMMS/dicom-go/transfer"
)

var (
	// ErrSignednessMismatch reports a codestream whose component signedness
	// contradicts Pixel Representation under the selected policy.
	ErrSignednessMismatch = errors.New("jpeg2000adapter: codestream signedness contradicts PixelRepresentation")
	// ErrUnsupportedCodestreamFeature names a JPEG 2000 feature the core
	// backend refuses rather than guesses (region of interest, HT refinement
	// passes, mixed HT and Part 1 code-blocks, components above 16 bits).
	ErrUnsupportedCodestreamFeature = errors.New("jpeg2000adapter: unsupported JPEG 2000 codestream feature")
	// ErrJP2Wrapped reports a JP2 file where DICOM requires a bare codestream
	// (PS3.5 A.4.4). Unwrapping is an explicit recovery option.
	ErrJP2Wrapped = errors.New("jpeg2000adapter: JP2 container instead of a JPEG 2000 codestream")
	// ErrSampleOutOfRange reports a decoded sample that BitsStored and
	// PixelRepresentation cannot represent; it is never truncated.
	ErrSampleOutOfRange = errors.New("jpeg2000adapter: decoded sample outside the BitsStored range")
	// ErrInvalidReduction reports a reduced-resolution decode asking to drop
	// more resolution levels than a tile-component has.
	ErrInvalidReduction = errors.New("jpeg2000adapter: reduction beyond the decomposition levels")
	// ErrResourceLimit reports a codestream beyond the caller's budget.
	ErrResourceLimit = errors.New("jpeg2000adapter: JPEG 2000 resource limit exceeded")
)

// SignednessPolicy decides a stream/dataset disagreement on sample sign.
// PS3.5 8.2.4: the codestream controls decompression; the native
// representation written for the dataset is a separate step.
type SignednessPolicy int

const (
	// RejectSignednessMismatch refuses a mismatch in either direction.
	RejectSignednessMismatch SignednessPolicy = iota
	// ReinterpretUnsignedAsSigned accepts an unsigned codestream in a dataset
	// with PixelRepresentation 1 (a known legacy encoding) and writes each
	// decoded value as the two's complement of BitsStored. A signed
	// codestream with PixelRepresentation 0 is still refused.
	ReinterpretUnsignedAsSigned
)

// CoreLimits bounds one frame decode. Zero fields select finite defaults.
type CoreLimits struct {
	MaxInputBytes        int64
	MaxComponents        int
	MaxTiles             int
	MaxResolutions       int
	MaxSamples           int64
	MaxTileWorkBytes     int64
	MaxCodeBlocksPerTile int
}

// CoreOptions configures the pure-Go JPEG 2000 Part 1 core backend adapted
// from amrshadid/go-dicom v1.6.0.
type CoreOptions struct {
	Limits             CoreLimits
	SignednessMismatch SignednessPolicy
	// AllowJP2Wrapper unwraps a JP2 container's codestream box instead of
	// refusing it. It is a recovery policy, not DICOM conformance.
	AllowJP2Wrapper bool
}

type coreDecoder struct {
	options CoreOptions
}

// NewCoreDecoder returns the core backend as a Decoder/ContextDecoder.
func NewCoreDecoder(options CoreOptions) Decoder {
	return coreDecoder{options: options}
}

// NewCore returns a codec backed by the core decoder. Frame boundaries follow
// the Basic/Extended Offset Tables or the codestream structure, never one Item
// per frame by assumption.
func NewCore(options CoreOptions) *Codec {
	return &Codec{decoder: coreDecoder{options: options}, format: encapsulated.JPEG2000, allowJP2: options.AllowJP2Wrapper}
}

// CoreTransferSyntaxUIDs are the syntaxes the core backend implements: JPEG
// 2000 Part 1 lossless-only and lossy. Part 2 and HTJ2K stay with the other
// backends.
func CoreTransferSyntaxUIDs() []string {
	return []string{transfer.JPEG2000LosslessOnly.UID, transfer.JPEG2000.UID}
}

// RegisterCore registers the core backend for .90/.91 in registry only.
func RegisterCore(registry pixeldata.Registry, options CoreOptions) error {
	if registry == nil {
		return pixeldata.ErrCodecRegistryNil
	}
	for _, uid := range CoreTransferSyntaxUIDs() {
		codec := NewCore(options)
		codec.allowPalette = uid == transfer.JPEG2000LosslessOnly.UID
		if err := registry.RegisterCodec(uid, codec); err != nil {
			return err
		}
	}
	return nil
}

// CoreHTJ2KTransferSyntaxUIDs are the HTJ2K syntaxes RegisterCoreHTJ2K
// registers: codestreams whose code-blocks are each coded by one HT cleanup
// pass (ISO/IEC 15444-15), as the pure-Go dicom-go encoder and OpenJPH
// write them. HT refinement passes, placeholder passes and mixed HT/Part 1
// code-blocks are refused by name.
func CoreHTJ2KTransferSyntaxUIDs() []string {
	return []string{transfer.HTJ2KLossless.UID, transfer.HTJ2KLosslessRPCL.UID, transfer.HTJ2K.UID}
}

// RegisterCoreHTJ2K registers the core backend for .201/.202/.203 in
// registry only. It is separate from RegisterCore so that a registry keeping
// another HTJ2K backend is unchanged.
func RegisterCoreHTJ2K(registry pixeldata.Registry, options CoreOptions) error {
	if registry == nil {
		return pixeldata.ErrCodecRegistryNil
	}
	codec := NewCore(options)
	for _, uid := range CoreHTJ2KTransferSyntaxUIDs() {
		if err := registry.RegisterCodec(uid, codec); err != nil {
			return err
		}
	}
	return nil
}

func (d coreDecoder) DecodeFrame(payload []byte, metadata pixeldata.Metadata) ([]byte, error) {
	return d.DecodeFrameContext(context.Background(), payload, metadata)
}

func (d coreDecoder) DecodeFrameContext(ctx context.Context, payload []byte, metadata pixeldata.Metadata) ([]byte, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	limits := d.options.Limits
	maxInput := limits.MaxInputBytes
	if maxInput <= 0 {
		maxInput = 1 << 30
	}
	if int64(len(payload)) > maxInput {
		return nil, fmt.Errorf("%w: codestream of %d bytes, limit %d", ErrResourceLimit, len(payload), maxInput)
	}
	if isJP2(payload) && !d.options.AllowJP2Wrapper {
		return nil, ErrJP2Wrapped
	}
	stream, err := j2kcore.ParseCodestream(payload)
	if err != nil {
		return nil, classifyCoreError(err)
	}
	chroma422 := isYBRFull422(metadata)
	if chroma422 {
		if err := checkChroma422(stream, metadata); err != nil {
			return nil, err
		}
	}
	if err := checkCoreGeometry(stream, metadata); err != nil {
		return nil, err
	}
	reinterpret, err := d.signedness(stream, metadata)
	if err != nil {
		return nil, err
	}
	image, err := j2kcore.DecodeContext(ctx, stream, j2kcore.Limits{
		MaxComponents:        limits.MaxComponents,
		MaxTiles:             limits.MaxTiles,
		MaxResolutions:       limits.MaxResolutions,
		MaxSamples:           limits.MaxSamples,
		MaxTileWorkBytes:     limits.MaxTileWorkBytes,
		MaxCodeBlocksPerTile: limits.MaxCodeBlocksPerTile,
	})
	if err != nil {
		if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
			return nil, err
		}
		return nil, classifyCoreError(err)
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	frame, err := packCoreSamples(image, metadata, reinterpret)
	if err != nil || !chroma422 {
		return frame, err
	}
	// The decoded planes already repeat each chroma sample over the two
	// columns it covers on the reference grid.
	ybrFullToRGB8(frame)
	return frame, nil
}

// isYBRFull422 reports Photometric Interpretation YBR_FULL_422.
func isYBRFull422(metadata pixeldata.Metadata) bool {
	return strings.EqualFold(strings.Trim(metadata.PhotometricInterpretation, " \x00"), "YBR_FULL_422")
}

// checkChroma422 admits the one subsampled layout decoded under
// YBR_FULL_422 (#2028), as GDCM does: three unsigned 8-bit components, the
// first at full resolution and the other two at XRsiz=2, YRsiz=1, without a
// multiple component transform. Each component is checked against its own
// sampling factors and the size they give it (ISO/IEC 15444-1 B.2), so an
// odd width has ceil(Columns/2) chroma columns. Any other layout keeps
// failing.
func checkChroma422(stream *j2kcore.Codestream, metadata pixeldata.Metadata) error {
	if len(stream.Components) != 3 {
		return fmt.Errorf("%w: YBR_FULL_422 with %d components", ErrUnsupportedCodestreamFeature, len(stream.Components))
	}
	sampling := [3][2]int{{1, 1}, {2, 1}, {2, 1}}
	x0, y0 := int64(stream.XOffset), int64(stream.YOffset)
	for i, component := range stream.Components {
		dx, dy := sampling[i][0], sampling[i][1]
		if component.DX != dx || component.DY != dy {
			return fmt.Errorf("%w: YBR_FULL_422 component %d sampled %dx%d, want %dx%d (4:2:2)", ErrUnsupportedCodestreamFeature, i, component.DX, component.DY, dx, dy)
		}
		if component.Depth != 8 || component.Signed {
			return fmt.Errorf("%w: YBR_FULL_422 component %d is a %d-bit signed=%t sample, want unsigned 8-bit", ErrUnsupportedCodestreamFeature, i, component.Depth, component.Signed)
		}
		width := sampledExtent(x0, int64(stream.Width), dx)
		height := sampledExtent(y0, int64(stream.Height), dy)
		wantWidth := sampledExtent(x0, x0+int64(metadata.Columns), dx)
		wantHeight := sampledExtent(y0, y0+int64(metadata.Rows), dy)
		if width != wantWidth || height != wantHeight {
			return fmt.Errorf("%w: YBR_FULL_422 component %d is %dx%d, want %dx%d for Columns=%d Rows=%d", ErrImageSizeMismatch, i, width, height, wantWidth, wantHeight, metadata.Columns, metadata.Rows)
		}
	}
	for tile := 0; tile < stream.NumTiles(); tile++ {
		if stream.CodingForTile(tile, 0).MCT {
			return fmt.Errorf("%w: YBR_FULL_422 with a multiple component transform", ErrUnsupportedCodestreamFeature)
		}
	}
	return nil
}

// sampledExtent is the number of samples a component sampled every step
// positions has between reference grid positions from and to: ceil(to/step)
// - ceil(from/step) (ISO/IEC 15444-1 B.2).
func sampledExtent(from, to int64, step int) int64 {
	s := int64(step)
	return (to+s-1)/s - (from+s-1)/s
}

// ybrFullToRGB8 converts interleaved 8-bit full-range YCbCr to RGB in place
// with the YBR_FULL equations of PS3.3 C.7.6.3.1.2, clamped to 0..255 and
// rounded half away from zero. The explicit conversions keep each product
// rounded on its own, so no platform fuses it into an addition.
func ybrFullToRGB8(frame []byte) {
	clamp := func(v float64) byte { return byte(math.Round(math.Max(0, math.Min(255, v)))) }
	for i := 0; i+2 < len(frame); i += 3 {
		y := float64(frame[i])
		cb, cr := float64(frame[i+1])-128, float64(frame[i+2])-128
		frame[i] = clamp(y + float64(1.402*cr))
		frame[i+1] = clamp(y - float64(0.344136*cb) - float64(0.714136*cr))
		frame[i+2] = clamp(y + float64(1.772*cb))
	}
}

// CoreImage is a codestream decoded by the core backend without DICOM
// metadata, such as a reduced-resolution preview.
type CoreImage struct {
	Columns, Rows int
	// Components holds one plane per component in raster order, level
	// shifted and clamped to the component's depth; a subsampled component
	// repeats its samples over the grid of the most finely sampled one.
	Components [][]int32
	Depths     []int
	Signed     []bool
}

// DecodeCoreReduced decodes a JPEG 2000 Part 1 codestream at 1/2^reduce of
// its size: the reduce finest resolution levels are dropped and the image is
// synthesized from the lower ones (ISO/IEC 15444-1 Annex F), which is what a
// thumbnail or preview needs and far cheaper than a full decode. reduce must
// not exceed the decomposition levels of any tile-component. The limits,
// cancellation and JP2 policy of options apply; the signedness policy does
// not, as there is no Pixel Representation to compare with.
func DecodeCoreReduced(ctx context.Context, codestream []byte, reduce int, options CoreOptions) (*CoreImage, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	maxInput := options.Limits.MaxInputBytes
	if maxInput <= 0 {
		maxInput = 1 << 30
	}
	if int64(len(codestream)) > maxInput {
		return nil, fmt.Errorf("%w: codestream of %d bytes, limit %d", ErrResourceLimit, len(codestream), maxInput)
	}
	if isJP2(codestream) && !options.AllowJP2Wrapper {
		return nil, ErrJP2Wrapped
	}
	stream, err := j2kcore.ParseCodestream(codestream)
	if err != nil {
		return nil, classifyCoreError(err)
	}
	if reduce < 0 {
		return nil, fmt.Errorf("%w: %d", ErrInvalidReduction, reduce)
	}
	for tile := 0; tile < stream.NumTiles(); tile++ {
		for comp := range stream.Components {
			if levels := stream.CodingForTile(tile, comp).Levels; reduce > levels {
				return nil, fmt.Errorf("%w: %d levels asked, tile %d component %d has %d", ErrInvalidReduction, reduce, tile, comp, levels)
			}
		}
	}
	limits := options.Limits
	image, err := j2kcore.DecodeContextOptions(ctx, stream, j2kcore.Limits{
		MaxComponents:        limits.MaxComponents,
		MaxTiles:             limits.MaxTiles,
		MaxResolutions:       limits.MaxResolutions,
		MaxSamples:           limits.MaxSamples,
		MaxTileWorkBytes:     limits.MaxTileWorkBytes,
		MaxCodeBlocksPerTile: limits.MaxCodeBlocksPerTile,
	}, j2kcore.DecodeOptions{Reduce: reduce})
	if err != nil {
		if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
			return nil, err
		}
		return nil, classifyCoreError(err)
	}
	return &CoreImage{Columns: image.Width, Rows: image.Height, Components: image.Components, Depths: image.Depths, Signed: image.Signed}, nil
}

func isJP2(payload []byte) bool {
	return len(payload) >= 12 && bytes.Equal(payload[4:8], []byte("jP  "))
}

func classifyCoreError(err error) error {
	var unsupported *j2kcore.ErrUnsupported
	if errors.As(err, &unsupported) {
		return fmt.Errorf("%w: %s", ErrUnsupportedCodestreamFeature, unsupported.Feature)
	}
	var limit *j2kcore.ErrLimit
	if errors.As(err, &limit) {
		return fmt.Errorf("%w: %v", ErrResourceLimit, limit)
	}
	return fmt.Errorf("%w: %v", ErrMalformedCodestream, err)
}

// checkCoreGeometry compares the SIZ parameters that control decompression
// with the DICOM description of the native result.
func checkCoreGeometry(stream *j2kcore.Codestream, metadata pixeldata.Metadata) error {
	// The decoded size: the image area, sampled as the most finely sampled
	// component is (a subsampled component repeats its samples).
	width, height := stream.OutputSize(0)
	if width != int(metadata.Columns) || height != int(metadata.Rows) {
		return fmt.Errorf("%w: JPEG 2000 size=%dx%d Columns=%d Rows=%d", ErrImageSizeMismatch, width, height, metadata.Columns, metadata.Rows)
	}
	if len(stream.Components) != int(metadata.SamplesPerPixel) {
		return fmt.Errorf("%w: JPEG 2000 components=%d SamplesPerPixel=%d", ErrImageSizeMismatch, len(stream.Components), metadata.SamplesPerPixel)
	}
	for i, component := range stream.Components {
		// The codestream precision controls decompression (PS3.5 8.2.4). A
		// precision other than BitsStored is reconciled only if it fits the
		// container; every decoded sample must then fit BitsStored.
		if component.Depth > int(metadata.BitsAllocated) {
			return fmt.Errorf("%w: JPEG 2000 precision=%d BitsAllocated=%d component=%d", ErrUnsupportedMetadata, component.Depth, metadata.BitsAllocated, i)
		}
		if component.Signed != stream.Components[0].Signed {
			return fmt.Errorf("%w: components differ in signedness", ErrUnsupportedMetadata)
		}
	}
	return nil
}

// signedness applies the policy and reports whether unsigned stream values
// must be reinterpreted as two's complement of BitsStored.
func (d coreDecoder) signedness(stream *j2kcore.Codestream, metadata pixeldata.Metadata) (bool, error) {
	signed := stream.Components[0].Signed
	switch {
	case signed && metadata.PixelRepresentation == 0:
		return false, fmt.Errorf("%w: signed codestream, PixelRepresentation=0", ErrSignednessMismatch)
	case !signed && metadata.PixelRepresentation == 1:
		if d.options.SignednessMismatch == ReinterpretUnsignedAsSigned {
			return true, nil
		}
		return false, fmt.Errorf("%w: unsigned codestream, PixelRepresentation=1", ErrSignednessMismatch)
	default:
		return false, nil
	}
}

// packCoreSamples writes interleaved little-endian native samples in the
// BitsAllocated container without any intermediate image model, scaling or
// windowing. Signed samples are stored as two's complement.
func packCoreSamples(image *j2kcore.Image, metadata pixeldata.Metadata, reinterpret bool) ([]byte, error) {
	rows, columns := int64(metadata.Rows), int64(metadata.Columns)
	components := int64(metadata.SamplesPerPixel)
	bytesPerSample := int64(metadata.BitsAllocated / 8)
	if image.Width != int(columns) || image.Height != int(rows) || int64(len(image.Components)) != components {
		return nil, fmt.Errorf("%w: decoded %dx%dx%d", ErrImageSizeMismatch, image.Width, image.Height, len(image.Components))
	}
	pixels := rows * columns
	if pixels <= 0 || components <= 0 || pixels > math.MaxInt64/components/bytesPerSample {
		return nil, fmt.Errorf("%w: output size overflows", ErrImageSizeMismatch)
	}
	size := pixels * components * bytesPerSample
	if size > int64(^uint(0)>>1) {
		return nil, fmt.Errorf("%w: output size exceeds this platform", ErrResourceLimit)
	}
	out := make([]byte, int(size))
	bitsStored := uint(metadata.BitsStored)
	low, high := int32(0), int32(1)<<bitsStored-1
	if metadata.PixelRepresentation == 1 && !reinterpret {
		low, high = -(int32(1) << (bitsStored - 1)), int32(1)<<(bitsStored-1)-1
	}
	for c := int64(0); c < components; c++ {
		plane := image.Components[c]
		if int64(len(plane)) != pixels {
			return nil, fmt.Errorf("%w: component %d has %d samples", ErrImageSizeMismatch, c, len(plane))
		}
		for i, v := range plane {
			if v < low || v > high {
				return nil, fmt.Errorf("%w: component %d", ErrSampleOutOfRange, c)
			}
			if reinterpret && v >= 1<<(bitsStored-1) {
				v -= 1 << bitsStored
			}
			offset := (int64(i)*components + c) * bytesPerSample
			switch bytesPerSample {
			case 1:
				out[offset] = byte(v)
			case 2:
				binary.LittleEndian.PutUint16(out[offset:], uint16(v))
			}
		}
	}
	return out, nil
}

// planJPEG2000Frames plans frames from the offset tables or the codestream
// structure and validates each frame's framing before it is decoded. A JP2
// frame is only passed through when the recovery option allows it.
func planJPEG2000Frames(ctx context.Context, pixel pixeldata.PixelData, obj *object.Object, frames int, allowJP2 bool) ([][]byte, error) {
	if len(pixel.Sequence.Fragments) > 0 && isJP2(pixel.Sequence.Fragments[0]) {
		if !allowJP2 {
			return nil, ErrJP2Wrapped
		}
		if frames == 1 {
			return [][]byte{bytes.Join(pixel.Sequence.Fragments, nil)}, nil
		}
	}
	plan, err := encapsulated.FromFragments(ctx, pixel.Sequence, obj, frames, encapsulated.JPEG2000, encapsulated.Limits{})
	if err != nil {
		return nil, fmt.Errorf("%w: %w", ErrUnsupportedFragmentLayout, err)
	}
	out := make([][]byte, plan.Len())
	for i := range out {
		view, err := plan.Frame(ctx, i)
		if err != nil {
			return nil, fmt.Errorf("%w: %w", ErrUnsupportedFragmentLayout, err)
		}
		if err := encapsulated.ValidateFrame(ctx, view.Data, encapsulated.JPEG2000); err != nil {
			if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
				return nil, err
			}
			return nil, fmt.Errorf("%w: frame %d: %w", ErrMalformedCodestream, i, err)
		}
		out[i] = view.Data
	}
	return out, nil
}
