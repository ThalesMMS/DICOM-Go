package pixeldata

import (
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"image"
	"math/bits"
)

// MaxReductionLevel is the greatest PartialDecodeRequest.ReductionLevel: at
// 1/2^16 every frame Rows and Columns can describe is one pixel.
const MaxReductionLevel = 16

var (
	// ErrPartialDecodeUnsupported is returned by a PartialFrameDecoder that
	// cannot serve this request itself; FrameReader.DecodePartial then
	// decodes more and crops or reduces it, unless the request sets
	// NoFallback, in which case it returns this error.
	ErrPartialDecodeUnsupported = errors.New("dicom: codec cannot serve this partial decode")
	// ErrInvalidReduction reports a negative ReductionLevel, one above
	// MaxReductionLevel, or one beyond the decomposition levels of a
	// codestream whose codec reduces it natively.
	ErrInvalidReduction = errors.New("dicom: invalid resolution reduction")
	// ErrInvalidQualityLayers reports a negative QualityLayers or one beyond
	// the layers of a codestream whose codec truncates layers natively.
	ErrInvalidQualityLayers = errors.New("dicom: invalid quality layer count")
)

// PartialDecodeRequest asks for part of one frame: a region, at a reduced
// resolution, from the first quality layers. The zero value beyond Frame is
// the whole frame at full resolution and quality.
type PartialDecodeRequest struct {
	// Frame is the frame index, from 0.
	Frame int
	// Region is a rectangle of the frame in full-resolution pixels, within
	// its Rows x Columns; the zero rectangle is the whole frame.
	Region image.Rectangle
	// ReductionLevel n asks for 1/2^n of the resolution across and down:
	// the output holds the pixels of the reduced grid whose 2^n x 2^n cells
	// overlap Region (ReducedRegion). 0 is full resolution.
	ReductionLevel int
	// QualityLayers n asks for the first n quality layers of a layered
	// codestream (JPEG 2000, HTJ2K); 0 asks for all of them.
	QualityLayers int
	// NoFallback returns ErrPartialDecodeUnsupported instead of decoding the
	// whole frame, reducing by averaging, or ignoring QualityLayers.
	NoFallback bool
}

// PartialDecodeCapabilities is what a codec does itself for one frame.
type PartialDecodeCapabilities struct {
	// Region means the decoder can produce a region without materializing
	// a full-frame output. Predictive codecs can still read and reconstruct
	// samples outside the region as part of their decoding state.
	Region bool
	// ReductionLevels is the greatest ReductionLevel synthesized from the
	// codestream's own lower resolutions (a JPEG 2000 frame's decomposition
	// levels); 0 when only averaging reduces.
	ReductionLevels int
	// QualityLayers is how many quality layers the frame has when the codec
	// can stop after any of them; 0 when every decode has full quality.
	QualityLayers int
}

// PartialFrameDecoder is implemented by codecs that decode part of one
// frame (a region, a reduced resolution and the first quality layers) without
// materializing a whole frame to serve smaller requests. codestream is the
// frame's borrowed, read-only encoded bytes and metadata describes that
// frame. request is validated. Region is nonempty and within the frame,
// and the levels are in range.
// The result is one frame of ReducedRegion(request.Region,
// request.ReductionLevel) in the layout the codec's full decode produces.
// A codec returns ErrPartialDecodeUnsupported for a request it cannot serve
// whole, ErrInvalidReduction for a reduction beyond the codestream's levels,
// and ErrInvalidQualityLayers for layers beyond those it has.
type PartialFrameDecoder interface {
	PartialDecodeCapabilities(codestream []byte, metadata Metadata) (PartialDecodeCapabilities, error)
	DecodeFramePartial(ctx context.Context, codestream []byte, metadata Metadata, request PartialDecodeRequest) (Frames, error)
}

// PartialDecodePath says how FrameReader.DecodePartial produced a result.
type PartialDecodePath uint8

const (
	// PartialDecodeCodec: the codec produced the requested region,
	// reduction and quality layers directly.
	PartialDecodeCodec PartialDecodePath = iota + 1
	// PartialDecodeNativeRows: native Pixel Data, of which only the rows
	// (and columns) the region needs were read.
	PartialDecodeNativeRows
	// PartialDecodeCodecRegion: a FrameRegionDecoder decoded the region,
	// and any reduction was then done by averaging.
	PartialDecodeCodecRegion
	// PartialDecodeFullFrame: the whole frame was decoded, then cropped and
	// reduced by averaging.
	PartialDecodeFullFrame
)

func (p PartialDecodePath) String() string {
	switch p {
	case PartialDecodeCodec:
		return "codec"
	case PartialDecodeNativeRows:
		return "native-rows"
	case PartialDecodeCodecRegion:
		return "codec-region"
	case PartialDecodeFullFrame:
		return "full-frame"
	}
	return fmt.Sprintf("PartialDecodePath(%d)", uint8(p))
}

// PartialDecodeResult is part of one decoded frame.
type PartialDecodeResult struct {
	// Data holds the samples, laid out as Frame lays out a whole frame:
	// interleaved, or plane after plane for planar native data.
	Data []byte
	// Metadata describes Data, one frame of Region's size.
	Metadata Metadata
	// Region is the output's rectangle on the reduced grid,
	// ReducedRegion(Request.Region, Request.ReductionLevel).
	Region image.Rectangle
	// Request is the request served, its Region resolved to the frame.
	Request PartialDecodeRequest
	Path    PartialDecodePath
	// Fallback: some part of the request was served by decoding more than
	// it needs (the whole frame for a region, averaging for the reduction,
	// or every quality layer when fewer were asked for).
	Fallback bool
	// AveragedReduction: the reduction is the rounded mean of each
	// 2^n x 2^n cell of the full-resolution decode (clipped at the frame's
	// edge), not the codestream's own lower resolution.
	AveragedReduction bool
	// QualityLayers is the number of layers decoded; 0 when every layer
	// was, including when the request asked for fewer but the path cannot
	// truncate them.
	QualityLayers int
}

// ReducedRegion is the rectangle of the grid reduced by 2^level whose
// cells, each 2^level x 2^level full-resolution pixels from the frame's
// origin, overlap region: Min rounded down and Max rounded up. This is the
// mapping of ISO/IEC 15444-1 B.5 for an image whose origin is a multiple of
// 2^level.
func ReducedRegion(region image.Rectangle, level int) image.Rectangle {
	if level <= 0 {
		return region
	}
	s := 1 << level
	return image.Rect(floorDiv(region.Min.X, s), floorDiv(region.Min.Y, s), ceilDiv(region.Max.X, s), ceilDiv(region.Max.Y, s))
}

func floorDiv(a, b int) int {
	if a >= 0 {
		return a / b
	}
	return -((-a + b - 1) / b)
}

func ceilDiv(a, b int) int { return -floorDiv(-a, b) }

// validatePartialRequest checks request against a frame of bounds and
// returns it with Region resolved.
func validatePartialRequest(request PartialDecodeRequest, bounds image.Rectangle) (PartialDecodeRequest, error) {
	if request.ReductionLevel < 0 || request.ReductionLevel > MaxReductionLevel {
		return request, fmt.Errorf("%w: level %d, want 0 to %d", ErrInvalidReduction, request.ReductionLevel, MaxReductionLevel)
	}
	if request.QualityLayers < 0 {
		return request, fmt.Errorf("%w: %d", ErrInvalidQualityLayers, request.QualityLayers)
	}
	if request.Region == (image.Rectangle{}) {
		request.Region = bounds
	}
	if request.Region.Empty() || !request.Region.In(bounds) {
		return request, fmt.Errorf("%w: %v in %v", ErrInvalidRegion, request.Region, bounds)
	}
	return request, nil
}

// PartialDecodeCapabilities reports what DecodePartial does without
// decoding more than a request needs, for frame i: native Pixel Data reads
// regions by rows. A PartialFrameDecoder reports its capabilities explicitly,
// including an empty result. Otherwise, a FrameRegionDecoder decodes regions.
func (r *FrameReader) PartialDecodeCapabilities(ctx context.Context, i int) (PartialDecodeCapabilities, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	if err := r.checkIndex(i); err != nil {
		return PartialDecodeCapabilities{}, err
	}
	if !r.syntax.Encapsulated {
		return PartialDecodeCapabilities{Region: !isSubsampled422(r.metadata.PhotometricInterpretation)}, nil
	}
	codec, ok := r.codec()
	if !ok {
		return PartialDecodeCapabilities{}, nil
	}
	if decoder, ok := codec.(PartialFrameDecoder); ok {
		codestream, err := r.readAdmittedFrame(ctx, i)
		if err != nil {
			return PartialDecodeCapabilities{}, err
		}
		return decoder.PartialDecodeCapabilities(codestream, r.frameMetadata())
	}
	_, region := codec.(FrameRegionDecoder)
	return PartialDecodeCapabilities{Region: region}, nil
}

// DecodePartial decodes the part of a frame request asks for. A codec that
// implements PartialFrameDecoder serves it whole when it can; otherwise
// native frames read only the rows the region needs, a FrameRegionDecoder
// decodes the region, or the frame is decoded whole and cropped, and a
// reduction is then the mean of each 2^n x 2^n cell. Each fallback is
// reported in the result (Fallback, Path, AveragedReduction,
// QualityLayers), and request.NoFallback refuses them with
// ErrPartialDecodeUnsupported. Invalid requests fail with ErrInvalidRegion,
// ErrInvalidReduction or ErrInvalidQualityLayers. Beyond what the codec
// needs to decode, memory is bounded by the region at full resolution,
// rounded out to whole reduction cells, and the output.
func (r *FrameReader) DecodePartial(ctx context.Context, request PartialDecodeRequest) (PartialDecodeResult, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	if err := r.checkIndex(request.Frame); err != nil {
		return PartialDecodeResult{}, err
	}
	bounds := image.Rect(0, 0, int(r.metadata.Columns), int(r.metadata.Rows))
	request, err := validatePartialRequest(request, bounds)
	if err != nil {
		return PartialDecodeResult{}, err
	}
	level := request.ReductionLevel
	out := ReducedRegion(request.Region, level)
	// The full-resolution pixels of the reduced cells, clipped to the frame:
	// the region itself without a reduction.
	source := image.Rect(out.Min.X<<level, out.Min.Y<<level, out.Max.X<<level, out.Max.Y<<level).Intersect(bounds)
	// A reduction or a layer count only a PartialFrameDecoder serves; and
	// whether the whole frame is all the request needs.
	beyondRegion := level > 0 || request.QualityLayers > 0
	whole := source == bounds && !beyondRegion
	refuse := func(why string) error {
		return fmt.Errorf("%w: frame %d, %v at level %d, %d layers: %s", ErrPartialDecodeUnsupported, request.Frame, request.Region, level, request.QualityLayers, why)
	}
	result := PartialDecodeResult{Request: request, Region: out, AveragedReduction: level > 0, Fallback: beyondRegion}

	var (
		data     []byte
		metadata Metadata
	)
	switch {
	case !r.syntax.Encapsulated:
		if beyondRegion && request.NoFallback {
			return PartialDecodeResult{}, refuse("native Pixel Data has no lower resolutions or layers")
		}
		if isSubsampled422(r.metadata.PhotometricInterpretation) {
			return PartialDecodeResult{}, fmt.Errorf("%w: regions of subsampled 4:2:2 Pixel Data are unsupported", ErrFrameLayout)
		}
		if data, metadata, err = r.nativeRegion(ctx, request.Frame, source); err != nil {
			return PartialDecodeResult{}, err
		}
		result.Path = PartialDecodeNativeRows
	default:
		codec, _ := r.codec()
		var codestream []byte
		if decoder, ok := codec.(PartialFrameDecoder); ok {
			codestream, err = r.readAdmittedFrame(ctx, request.Frame)
			if err != nil {
				return PartialDecodeResult{}, err
			}
			frames, err := decoder.DecodeFramePartial(ctx, codestream, r.frameMetadata(), request)
			switch {
			case err == nil:
				data, metadata, err := r.checkDecodedPart(frames, out)
				if err != nil {
					return PartialDecodeResult{}, err
				}
				result.Data, result.Metadata, result.Path = data, metadata, PartialDecodeCodec
				result.AveragedReduction, result.QualityLayers, result.Fallback = false, request.QualityLayers, false
				return result, nil
			case !errors.Is(err, ErrPartialDecodeUnsupported) && !errors.Is(err, ErrRegionUnsupported):
				return PartialDecodeResult{}, err
			}
		}
		if beyondRegion && request.NoFallback {
			return PartialDecodeResult{}, refuse("the codec cannot reduce or truncate layers itself")
		}
		if decoder, ok := codec.(FrameRegionDecoder); ok && !whole {
			if codestream == nil {
				codestream, err = r.readAdmittedFrame(ctx, request.Frame)
				if err != nil {
					return PartialDecodeResult{}, err
				}
			}
			frames, err := decoder.DecodeFrameRegion(ctx, codestream, r.frameMetadata(), source)
			switch {
			case err == nil:
				if data, metadata, err = r.checkDecodedPart(frames, source); err != nil {
					return PartialDecodeResult{}, err
				}
				result.Path = PartialDecodeCodecRegion
			case !errors.Is(err, ErrRegionUnsupported):
				return PartialDecodeResult{}, err
			}
		}
		if result.Path == 0 {
			if !whole && request.NoFallback {
				return PartialDecodeResult{}, refuse("the codec decodes whole frames only")
			}
			frame, frameMetadata, err := r.Frame(ctx, request.Frame)
			if err != nil {
				return PartialDecodeResult{}, err
			}
			if whole {
				data, metadata = frame, frameMetadata
			} else if data, metadata, err = cropFrame(frame, frameMetadata, source); err != nil {
				return PartialDecodeResult{}, err
			}
			result.Path, result.Fallback = PartialDecodeFullFrame, !whole || beyondRegion
		}
	}
	if level > 0 {
		if data, metadata, err = averageCells(ctx, data, metadata, level); err != nil {
			return PartialDecodeResult{}, err
		}
	}
	if int(metadata.Columns) != out.Dx() || int(metadata.Rows) != out.Dy() {
		return PartialDecodeResult{}, fmt.Errorf("%w: %dx%d output for %v", ErrPixelDataSizeMismatch, metadata.Columns, metadata.Rows, out)
	}
	result.Data, result.Metadata = data, metadata
	return result, nil
}

// checkDecodedPart checks a codec's one-frame result of rect's size and
// returns it with its metadata.
func (r *FrameReader) checkDecodedPart(frames Frames, rect image.Rectangle) ([]byte, Metadata, error) {
	if len(frames.Data) != 1 || frames.Rows != rect.Dy() || frames.Columns != rect.Dx() {
		return nil, Metadata{}, fmt.Errorf("%w: partial decoder returned %d frames of %dx%d for %dx%d", ErrPixelDataSizeMismatch, len(frames.Data), frames.Columns, frames.Rows, rect.Dx(), rect.Dy())
	}
	metadata := decompressedOutputMetadata(r.frameMetadata(), r.syntax, frames)
	metadata.Rows, metadata.Columns = uint16(rect.Dy()), uint16(rect.Dx())
	bytes := metadata.FrameSize()
	if int64(len(frames.Data[0])) != bytes {
		return nil, Metadata{}, fmt.Errorf("%w: part of %d bytes, want %d", ErrPixelDataSizeMismatch, len(frames.Data[0]), bytes)
	}
	if bytes > r.decodeLimits.MaxNativeBytes {
		return nil, Metadata{}, decompressLimitError("MaxNativeBytes", bytes, r.decodeLimits.MaxNativeBytes)
	}
	return frames.Data[0], metadata, nil
}

// codec is the registered codec of the reader's syntax.
func (r *FrameReader) codec() (Codec, bool) {
	registry := r.options.Registry
	if registry == nil {
		registry = DefaultRegistry
	}
	if registry == nil {
		return nil, false
	}
	return registry.GetCodec(r.syntax.UID)
}

// averageCells reduces a decoded little-endian frame part, whose origin is
// on the reduced grid, by 2^level: each output sample is the mean, rounded
// half up, of the stored values (BitsStored at HighBit, sign extended) of
// one cell of 2^level x 2^level samples, clipped to the part's last row and
// column. The layout, interleaved or planar, is kept.
func averageCells(ctx context.Context, data []byte, metadata Metadata, level int) ([]byte, Metadata, error) {
	if isSubsampled422(metadata.PhotometricInterpretation) {
		return nil, Metadata{}, fmt.Errorf("%w: reducing subsampled 4:2:2 Pixel Data is unsupported", ErrFrameLayout)
	}
	format := metadata.SampleFormat(binary.LittleEndian)
	minimum, _, err := format.Range()
	if err != nil {
		return nil, Metadata{}, err
	}
	if int64(len(data)) < metadata.FrameSize() {
		return nil, Metadata{}, fmt.Errorf("%w: part of %d bytes, want %d", ErrPixelDataSizeMismatch, len(data), metadata.FrameSize())
	}
	width := metadata.BytesPerSample()
	columns, rows := int(metadata.Columns), int(metadata.Rows)
	samples := int(metadata.SamplesPerPixel)
	planes, perPixel := 1, samples
	if samples > 1 && metadata.PlanarConfiguration == 1 {
		planes, perPixel = samples, 1
	}
	cell := 1 << level
	outColumns, outRows := ceilDiv(columns, cell), ceilDiv(rows, cell)
	out := make([]byte, outColumns*outRows*samples*width)

	shift := uint(int(metadata.HighBit) + 1 - int(metadata.BitsStored))
	mask := uint64(1)<<uint(metadata.BitsStored) - 1
	signBit := uint64(0)
	if format.Signed {
		signBit = uint64(1) << (uint(metadata.BitsStored) - 1)
	}
	// offset is value - minimum, which is never negative: the stored value
	// with its sign bit flipped for signed samples.
	offset := func(at int) uint64 {
		var raw uint64
		switch width {
		case 1:
			raw = uint64(data[at])
		case 2:
			raw = uint64(binary.LittleEndian.Uint16(data[at:]))
		default:
			raw = uint64(binary.LittleEndian.Uint32(data[at:]))
		}
		return (raw>>shift)&mask ^ signBit
	}
	// One output row's sums of value-minimum, in 128 bits: a cell holds up
	// to 2^32 samples below 2^32.
	lineSamples := outColumns * perPixel
	hi, lo := make([]uint64, lineSamples), make([]uint64, lineSamples)
	at := 0
	for plane := 0; plane < planes; plane++ {
		for oy := 0; oy < outRows; oy++ {
			if err := ctx.Err(); err != nil {
				return nil, Metadata{}, err
			}
			clear(hi)
			clear(lo)
			y0, y1 := oy*cell, min((oy+1)*cell, rows)
			for y := y0; y < y1; y++ {
				row := (plane*rows + y) * columns * perPixel * width
				for x := 0; x < columns; x++ {
					base := (x >> level) * perPixel
					for s := 0; s < perPixel; s++ {
						var carry uint64
						lo[base+s], carry = bits.Add64(lo[base+s], offset(row), 0)
						hi[base+s] += carry
						row += width
					}
				}
			}
			for ox := 0; ox < outColumns; ox++ {
				count := uint64((y1 - y0) * (min((ox+1)*cell, columns) - ox*cell))
				for s := 0; s < perPixel; s++ {
					i := ox*perPixel + s
					sum, carry := bits.Add64(lo[i], count/2, 0)
					mean, _ := bits.Div64(hi[i]+carry, sum, count)
					raw := ((uint64(int64(mean)+minimum) & mask) << shift)
					switch width {
					case 1:
						out[at] = byte(raw)
					case 2:
						binary.LittleEndian.PutUint16(out[at:], uint16(raw))
					default:
						binary.LittleEndian.PutUint32(out[at:], uint32(raw))
					}
					at += width
				}
			}
		}
	}
	metadata.Rows, metadata.Columns = uint16(outRows), uint16(outColumns)
	return out, metadata, nil
}
