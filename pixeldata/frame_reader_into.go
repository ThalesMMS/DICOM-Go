package pixeldata

import (
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"image"
	"io"

	"github.com/ThalesMMS/dicom-go/transfer"
)

// ErrBufferLayoutUnavailable means a legacy codec cannot predict its decoded
// layout without decoding. FrameInto/RegionInto still support explicit copying.
var ErrBufferLayoutUnavailable = errors.New("dicom: decoded buffer layout unavailable without decoding")
var ErrBufferTooSmall = errors.New("dicom: destination buffer too small")

// Retain at most one input per reader, bounded by a 256x256 16-bit frame.
const maxReusableRLEInput = 128 << 10

// BufferSizeError reports the exact required destination length. Capacity beyond
// len(dst) is not writable; the caller must explicitly reslice it.
type BufferSizeError struct{ Required, Provided int64 }

func (e *BufferSizeError) Error() string {
	return fmt.Sprintf("%v: need %d bytes, have %d", ErrBufferTooSmall, e.Required, e.Provided)
}
func (e *BufferSizeError) Unwrap() error { return ErrBufferTooSmall }

// BufferMode distinguishes direct destination writes from allocated output copies.
type BufferMode string

const (
	BufferNativeDirect    BufferMode = "native-direct"
	BufferCodecDirect     BufferMode = "codec-direct"
	BufferFrameCopy       BufferMode = "frame-copy"
	BufferRegionCopy      BufferMode = "region-copy"
	BufferFrameRegionCopy BufferMode = "frame-region-copy"
)

// BufferLayout describes one packed output frame/region. Bytes are little-endian;
// planar/interleaved organization follows Metadata.PlanarConfiguration. There is
// no caller-selected stride or color/layout conversion in this API.
type BufferLayout struct {
	Metadata Metadata
	Bytes    int64
	Mode     BufferMode
}

// BufferResult describes a completed or failed operation. Written is len(output)
// on success. On error it is the extent of the prefix that MAY have been touched,
// not a valid frame or the count of valid samples: codecs can write sparse bands.
// Bytes after Written are unchanged. Layout is available when negotiated.
type BufferResult struct {
	Layout      BufferLayout
	Written     int
	Diagnostics []string
}

// FrameBufferRequest distinguishes whole-frame admission from regional prefix
// decoding, even if Region covers all stored pixels. WholeFrame is true only
// for FrameInto; its Region must equal the stored frame bounds.
type FrameBufferRequest struct {
	Region     image.Rectangle
	WholeFrame bool
}

// FrameBufferDecoder is an optional codec capability. region is in stored-frame
// coordinates (the whole bounds for FrameInto). WholeFrame preserves full-frame
// admission rather than regional prefix clipping. BufferMetadata must predict the
// exact decoded metadata, including color conversion and packing, without writes.
// ErrRegionUnsupported opts this request into the legacy copy fallback.
// DecodeFrameInto receives exactly the required slice, must not retain or alias
// dst/codestream, and must honor ctx/recovery options. Its output is little-endian.
// On failure n is the highest potentially touched offset, not necessarily a dense
// valid prefix. On success n must equal len(dst). Scratch/encoded inputs may still
// allocate; this capability promises direct output, not zero total allocations.
type FrameBufferDecoder interface {
	BufferMetadata(ctx context.Context, metadata Metadata, request FrameBufferRequest) (Metadata, error)
	DecodeFrameInto(ctx context.Context, codestream []byte, metadata Metadata, request FrameBufferRequest, dst []byte) (n int, diagnostics []string, err error)
}

// FrameBufferLayout queries a direct path without reading the pixel payload.
// Legacy codecs return ErrBufferLayoutUnavailable. A concurrent registry change
// can change the subsequent operation's layout; always inspect its actual result.
func (r *FrameReader) FrameBufferLayout(ctx context.Context, i int) (BufferLayout, error) {
	codec, _ := r.codec()
	return r.bufferLayout(ctx, i, image.Rectangle{}, false, codec)
}

// RegionBufferLayout is FrameBufferLayout for a nonempty in-bounds rectangle.
func (r *FrameReader) RegionBufferLayout(ctx context.Context, i int, region image.Rectangle) (BufferLayout, error) {
	codec, _ := r.codec()
	return r.bufferLayout(ctx, i, region, true, codec)
}
func (r *FrameReader) bufferLayout(ctx context.Context, i int, region image.Rectangle, regional bool, codec Codec) (BufferLayout, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	if err := ctx.Err(); err != nil {
		return BufferLayout{}, err
	}
	if err := r.checkIndex(i); err != nil {
		return BufferLayout{}, err
	}
	if r.collection != nil {
		return BufferLayout{}, ErrBufferLayoutUnavailable
	}
	bounds := image.Rect(0, 0, int(r.metadata.Columns), int(r.metadata.Rows))
	if !regional {
		region = bounds
	} else if region.Empty() || !region.In(bounds) {
		return BufferLayout{}, ErrInvalidRegion
	}
	m := r.frameMetadata()
	mode := BufferNativeDirect
	if r.native != nil {
		if regional && isSubsampled422(m.PhotometricInterpretation) {
			return BufferLayout{}, ErrFrameLayout
		}
		m.Rows, m.Columns = uint16(region.Dy()), uint16(region.Dx())
	} else {
		if codec == nil {
			return BufferLayout{}, ErrBufferLayoutUnavailable
		}
		into, ok := codec.(FrameBufferDecoder)
		if !ok {
			return BufferLayout{}, ErrBufferLayoutUnavailable
		}
		var err error
		m, err = into.BufferMetadata(ctx, m, FrameBufferRequest{Region: region, WholeFrame: !regional})
		if errors.Is(err, ErrRegionUnsupported) {
			return BufferLayout{}, ErrBufferLayoutUnavailable
		}
		if err != nil {
			return BufferLayout{}, err
		}
		mode = BufferCodecDirect
	}
	layout, err := checkedBufferLayout(m, region, mode)
	if err != nil {
		return BufferLayout{}, err
	}
	if r.native != nil {
		if err := r.checkNativeRequestLimits(i, region, regional, layout); err != nil {
			return layout, err
		}
	}
	return layout, nil
}
func checkedBufferLayout(m Metadata, region image.Rectangle, mode BufferMode) (BufferLayout, error) {
	if int(m.Rows) != region.Dy() || int(m.Columns) != region.Dx() || m.NumberOfFrames != 1 || m.SamplesPerPixel == 0 ||
		(m.BitsAllocated < 8 && (mode != BufferNativeDirect || m.BitsAllocated != 1)) || m.FrameSize() <= 0 {
		return BufferLayout{}, ErrFrameLayout
	}
	return BufferLayout{Metadata: m, Bytes: m.FrameSize(), Mode: mode}, nil
}

// FrameInto reads one decoded frame into dst. See BufferResult for partial errors.
// Preflight failures (invalid request/layout, short buffer, prior cancellation)
// leave dst unchanged. Copy fallback decodes before checking the actual size.
// The reader never retains dst. Concurrent reads require distinct destinations
// and a concurrently safe source/codec; Close must not race with a read.
func (r *FrameReader) FrameInto(ctx context.Context, i int, dst []byte) (BufferResult, error) {
	return r.into(ctx, i, image.Rectangle{}, false, dst)
}

// RegionInto writes the packed rectangle with the same layout as Region.
func (r *FrameReader) RegionInto(ctx context.Context, i int, region image.Rectangle, dst []byte) (BufferResult, error) {
	return r.into(ctx, i, region, true, dst)
}
func (r *FrameReader) into(ctx context.Context, i int, region image.Rectangle, regional bool, dst []byte) (BufferResult, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	if err := ctx.Err(); err != nil {
		return BufferResult{}, err
	}
	codec, _ := r.codec()
	layout, err := r.bufferLayout(ctx, i, region, regional, codec)
	if err != nil && !errors.Is(err, ErrBufferLayoutUnavailable) {
		return BufferResult{Layout: layout}, err
	}
	if errors.Is(err, ErrBufferLayoutUnavailable) {
		return r.copyInto(ctx, i, region, regional, dst)
	}
	result := BufferResult{Layout: layout}
	if int64(len(dst)) < layout.Bytes {
		return result, &BufferSizeError{Required: layout.Bytes, Provided: int64(len(dst))}
	}
	dst = dst[:int(layout.Bytes):int(layout.Bytes)]
	if r.native != nil {
		result.Written, err = r.nativeInto(ctx, i, region, regional, dst)
		return result, err
	}
	into := codec.(FrameBufferDecoder)
	if !regional {
		region = image.Rect(0, 0, int(r.metadata.Columns), int(r.metadata.Rows))
	}
	inputBytes, err := r.admittedFrameInputBytes(ctx, i)
	if err != nil {
		return result, err
	}
	if layout.Bytes > r.decodeLimits.MaxNativeBytes {
		return result, decompressLimitError("MaxNativeBytes", layout.Bytes, r.decodeLimits.MaxNativeBytes)
	}
	if ratio := decompressionExpansionRatio(layout.Bytes, inputBytes); ratio > r.decodeLimits.MaxExpansionRatio {
		return result, decompressLimitError("MaxExpansionRatio", ratio, r.decodeLimits.MaxExpansionRatio)
	}
	var encoded []byte
	if !regional && r.syntax.UID == transfer.RLELossless.UID && r.plan == nil &&
		r.metadata.NumberOfFrames > 1 && inputBytes > 0 && inputBytes <= min(layout.Bytes, maxReusableRLEInput) &&
		r.rleInputMu.TryLock() {
		defer r.rleInputMu.Unlock()
		if err := ctx.Err(); err != nil {
			return result, err
		}
		size := int(inputBytes)
		if cap(r.rleInput) < size {
			r.rleInput = make([]byte, size)
		}
		encoded = r.rleInput[:size:size]
		err = r.readFrameFragment(ctx, i, encoded)
	} else {
		encoded, err = r.RawFrame(ctx, i)
	}
	if err != nil {
		return result, err
	}
	if err = ctx.Err(); err != nil {
		return result, err
	}
	result.Written, result.Diagnostics, err = into.DecodeFrameInto(ctx, encoded, r.frameMetadata(), FrameBufferRequest{Region: region, WholeFrame: !regional}, dst[:int(layout.Bytes):int(layout.Bytes)])
	// A codec reporting an invalid extent cannot substantiate an unchanged tail.
	if result.Written < 0 || result.Written > int(layout.Bytes) {
		result.Written = int(layout.Bytes)
		return result, codecDecodeError(r.syntax.UID, ErrPixelDataSizeMismatch)
	}
	if err != nil {
		if isContextTermination(err) {
			return result, err
		}
		return result, codecDecodeError(r.syntax.UID, err)
	}
	if result.Written != int(layout.Bytes) {
		return result, codecDecodeError(r.syntax.UID, ErrPixelDataSizeMismatch)
	}
	if err = ctx.Err(); err != nil {
		return result, err
	}
	return result, nil
}
func (r *FrameReader) copyInto(ctx context.Context, i int, region image.Rectangle, regional bool, dst []byte) (BufferResult, error) {
	mode := BufferFrameCopy
	var data []byte
	var m Metadata
	var err error
	if regional {
		mode = BufferFrameRegionCopy
		data, m, mode, err = r.regionCopySource(ctx, i, region)
	} else {
		data, m, err = r.Frame(ctx, i)
	}
	if err != nil {
		return BufferResult{Layout: BufferLayout{Mode: mode}}, err
	}
	result := BufferResult{Layout: BufferLayout{Metadata: m, Bytes: int64(len(data)), Mode: mode}}
	if int64(len(data)) != m.FrameSize() {
		return result, ErrPixelDataSizeMismatch
	}
	if len(dst) < len(data) {
		return result, &BufferSizeError{Required: int64(len(data)), Provided: int64(len(dst))}
	}
	if err := ctx.Err(); err != nil {
		return result, err
	}
	result.Written = copy(dst, data)
	return result, nil
}

// regionCopySource preserves the existing regional decoder before a full-frame
// fallback, while reporting which allocation path actually completed.
func (r *FrameReader) regionCopySource(ctx context.Context, i int, region image.Rectangle) ([]byte, Metadata, BufferMode, error) {
	if decoder, ok := r.regionDecoder(); ok {
		encoded, err := r.readAdmittedFrame(ctx, i)
		if err != nil {
			return nil, Metadata{}, BufferRegionCopy, err
		}
		frames, err := decoder.DecodeFrameRegion(ctx, encoded, r.frameMetadata(), region)
		if err == nil {
			data, metadata, err := r.checkDecodedPart(frames, region)
			return data, metadata, BufferRegionCopy, err
		}
		if !errors.Is(err, ErrRegionUnsupported) {
			return nil, Metadata{}, BufferRegionCopy, err
		}
	}
	frame, m, err := r.Frame(ctx, i)
	if err != nil {
		return nil, Metadata{}, BufferFrameRegionCopy, err
	}
	data, m, err := cropFrame(frame, m, region)
	return data, m, BufferFrameRegionCopy, err
}
func (r *FrameReader) readNativeInto(ctx context.Context, dst []byte, offset int64) (int, error) {
	width := r.metadata.BytesPerSample()
	chunk := (64 << 10) / width * width
	touched := 0
	for touched < len(dst) {
		if err := ctx.Err(); err != nil {
			return touched, err
		}
		end := min(len(dst), touched+chunk)
		n, err := r.native.ReadAt(dst[touched:end], offset+int64(touched))
		if n < 0 || n > end-touched {
			return end, io.ErrUnexpectedEOF
		}
		if r.nativeOrder == binary.BigEndian && width > 1 {
			part := dst[touched : touched+n]
			for at := 0; at+width <= len(part); at += width {
				for lo, hi := at, at+width-1; lo < hi; lo, hi = lo+1, hi-1 {
					part[lo], part[hi] = part[hi], part[lo]
				}
			}
		}
		touched += n
		if err != nil {
			return touched, err
		}
		if touched != end {
			return touched, io.ErrUnexpectedEOF
		}
	}
	return touched, ctx.Err()
}
