package pixeldata

import (
	"context"
	"encoding/binary"
	"image"
	"io"
	"math"

	"github.com/ThalesMMS/dicom-go/core"
)

// PS3.5 8.2 packs smaller samples into the least-significant lanes of
// successive OW words. Big Endian stores each such word high byte first.
func nativeOWByteLanes(order binary.ByteOrder, vr core.VR, m Metadata) bool {
	return order == binary.BigEndian && vr == core.VROW && m.BitsAllocated <= 8
}

func (r *FrameReader) nativeOWByteLanes() bool {
	return nativeOWByteLanes(r.nativeOrder, r.pixelHeader.VR, r.metadata)
}

func (r *FrameReader) nativeSharedUnits() bool {
	return r.metadata.BitsAllocated == 1 || r.nativeOWByteLanes()
}

// nativeRuns describes logical spans in native Pixel Data, in bits for
// Bits Allocated 1 and bytes otherwise. Each span resumes immediately after
// the last selected destination sample, regardless of its source offset.
func (r *FrameReader) nativeRuns(i int, region image.Rectangle, regional bool, visit func(start, length int64) error) error {
	m := r.metadata
	frameUnits, sampleUnits := m.FrameSize(), int64(m.BytesPerSample())
	if m.BitsAllocated == 1 {
		frameUnits, sampleUnits = packedFrameBits(m), 1
	}
	start := int64(i) * frameUnits
	if !regional {
		return visit(start, frameUnits)
	}
	planes, perPixel := int64(1), int64(m.SamplesPerPixel)
	if perPixel > 1 && m.PlanarConfiguration == 1 {
		planes, perPixel = perPixel, 1
	}
	columns, rows := int64(m.Columns), int64(m.Rows)
	rowUnits := int64(region.Dx()) * perPixel * sampleUnits
	for p := int64(0); p < planes; p++ {
		for y := int64(region.Min.Y); y < int64(region.Max.Y); y++ {
			offset := ((p*rows+y)*columns + int64(region.Min.X)) * perPixel * sampleUnits
			if err := visit(start+offset, rowUnits); err != nil {
				return err
			}
		}
	}
	return nil
}

func (r *FrameReader) nativeWireRange(start, length int64) (first, end int64) {
	first, end = start, start+length
	if r.metadata.BitsAllocated == 1 {
		first, end = first/8, packedBytes(end)
	}
	if r.nativeOWByteLanes() {
		first &^= 1
		end += end % 2
	}
	return first, end
}

func (r *FrameReader) nativeRequestInputBytes(i int, region image.Rectangle, regional bool) (int64, error) {
	if !r.nativeSharedUnits() {
		m := r.metadata
		if regional {
			m.Rows, m.Columns = uint16(region.Dy()), uint16(region.Dx())
		}
		return m.FrameSize(), nil
	}
	var total int64
	err := r.nativeRuns(i, region, regional, func(start, length int64) error {
		first, end := r.nativeWireRange(start, length)
		if first < 0 || end < first || end > r.valueLength || end-first > math.MaxInt64-total {
			return ErrFrameLayout
		}
		total += end - first
		return nil
	})
	return total, err
}

func (r *FrameReader) readNative(ctx context.Context, i int, region image.Rectangle, regional bool) ([]byte, Metadata, error) {
	layout, err := r.bufferLayout(ctx, i, region, regional, nil)
	if err != nil {
		return nil, Metadata{}, err
	}
	if layout.Bytes > int64(math.MaxInt) {
		return nil, Metadata{}, ErrFrameLayout
	}
	data := make([]byte, int(layout.Bytes))
	if _, err := r.nativeInto(ctx, i, region, regional, data); err != nil {
		return nil, Metadata{}, err
	}
	return data, layout.Metadata, nil
}

func (r *FrameReader) rawNativeFrame(ctx context.Context, i int) ([]byte, error) {
	size, err := r.RawFrameLength(i)
	if err != nil {
		return nil, err
	}
	if size > int64(math.MaxInt) {
		return nil, ErrFrameLayout
	}
	frame := make([]byte, int(size))
	if r.nativeSharedUnits() {
		if _, err := r.nativeInto(ctx, i, image.Rectangle{}, false, frame[:r.metadata.FrameSize()]); err != nil {
			return nil, err
		}
		if r.nativeOWByteLanes() {
			swapNativeWords(frame)
		}
		return frame, nil
	}
	n, err := r.native.ReadAt(frame, r.nativeStart+int64(i)*size)
	if err != nil {
		return nil, err
	}
	if n != len(frame) {
		return nil, io.ErrUnexpectedEOF
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	return frame, nil
}

func (r *FrameReader) nativeInto(ctx context.Context, i int, region image.Rectangle, regional bool, dst []byte) (int, error) {
	if !r.nativeSharedUnits() {
		touched := 0
		err := r.nativeRuns(i, region, regional, func(start, length int64) error {
			n, err := r.readNativeInto(ctx, dst[touched:touched+int(length)], r.nativeStart+start)
			touched += n
			return err
		})
		return touched, err
	}
	return r.sharedUnitsInto(ctx, i, region, regional, dst)
}

func (r *FrameReader) sharedUnitsInto(ctx context.Context, i int, region image.Rectangle, regional bool, dst []byte) (int, error) {
	bitPacked := r.metadata.BitsAllocated == 1
	unitScale := int64(1)
	if bitPacked {
		unitScale = 8
	}
	words := r.nativeOWByteLanes()
	var next int64
	var touched int
	var scratch []byte
	err := r.nativeRuns(i, region, regional, func(start, length int64) error {
		first, end := r.nativeWireRange(start, length)
		if first < 0 || end < first || end > r.valueLength {
			return ErrFrameLayout
		}
		width := int(min(64<<10, end-first))
		if len(scratch) < width {
			scratch = make([]byte, width)
		}
		for at := first; at < end; {
			if err := ctx.Err(); err != nil {
				return err
			}
			chunk := scratch[:min(int64(width), end-at)]
			n, err := r.native.ReadAt(chunk, r.nativeStart+at)
			if err != nil {
				return err
			}
			if n != len(chunk) {
				return io.ErrUnexpectedEOF
			}
			if words {
				swapNativeWords(chunk)
			}
			lo := max(start, at*unitScale)
			hi := min(start+length, (at+int64(n))*unitScale)
			target := next + lo - start
			if bitPacked {
				copyPackedBits(dst, target, chunk, lo-at*unitScale, hi-lo)
				touched = int(packedBytes(target + hi - lo))
			} else {
				copy(dst[target:], chunk[lo-at:hi-at])
				touched = int(target + hi - lo)
			}
			at += int64(n)
		}
		next += length
		return nil
	})
	if err != nil {
		return touched, err
	}
	if bitPacked && next%8 != 0 {
		dst[touched-1] &= byte(1<<uint(next%8) - 1)
	}
	return touched, ctx.Err()
}

func swapNativeWords(data []byte) {
	for i := 0; i < len(data); i += 2 {
		data[i], data[i+1] = data[i+1], data[i]
	}
}
