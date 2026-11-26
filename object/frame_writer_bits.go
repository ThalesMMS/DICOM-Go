package object

import (
	"fmt"
	"io"
	"math"

	"github.com/ThalesMMS/dicom-go/core"
)

func nativePackedFrameBits(dataset *Object) (int64, error) {
	allocated, err := dataset.GetInt(tagFrameBitsAllocated)
	if err != nil || allocated != 1 {
		return 0, fmt.Errorf("%w: packed bit source requires native Bits Allocated 1", ErrFrameSource)
	}
	bits := int64(1)
	for _, tag := range []core.Tag{core.NewTag(0x0028, 0x0010), core.NewTag(0x0028, 0x0011), core.NewTag(0x0028, 0x0002)} {
		n, err := dataset.GetInt(tag)
		if err != nil || n <= 0 || bits > (math.MaxInt64-7)/n {
			return 0, fmt.Errorf("%w: invalid packed frame dimensions", ErrFrameSource)
		}
		bits *= n
	}
	return bits, nil
}

// This buffer is independent of frame count and dimensions. The source still
// owns its payload; output errors propagate before another frame is requested.
type packedBitWriter struct {
	w         io.Writer
	buffer    [32 << 10]byte
	used      int
	written   int64
	carry     uint16
	carryBits uint
	remaining int64
	err       error
}

func (w *packedBitWriter) flush() error {
	if w.err != nil {
		return w.err
	}
	if w.used == 0 {
		return nil
	}
	n, err := w.w.Write(w.buffer[:w.used])
	w.written += int64(n)
	if err == nil && n != w.used {
		err = io.ErrShortWrite
	}
	w.used = 0
	w.err = err
	return err
}

func (w *packedBitWriter) put(b byte) error {
	w.buffer[w.used] = b
	w.used++
	if w.used == len(w.buffer) {
		return w.flush()
	}
	return nil
}

func (w *packedBitWriter) Write(data []byte) (int, error) {
	if w.err != nil {
		return 0, w.err
	}
	if int64(len(data)) > (w.remaining+7)/8 {
		return 0, fmt.Errorf("%w: packed frame exceeds declared length", ErrFrameSource)
	}
	for i, b := range data {
		n := uint(min(int64(8), w.remaining))
		if n < 8 && b>>n != 0 {
			return i, fmt.Errorf("%w: nonzero unused packed frame bits", ErrFrameSource)
		}
		w.carry |= uint16(b) << w.carryBits
		w.carryBits += n
		w.remaining -= int64(n)
		if w.carryBits >= 8 {
			if err := w.put(byte(w.carry)); err != nil {
				return i + 1, err
			}
			w.carry >>= 8
			w.carryBits -= 8
		}
	}
	return len(data), nil
}

func (p *frameValueProvider) copyPackedBits(destination io.Writer) (int64, error) {
	w := &packedBitWriter{w: destination}
	for i, length := range p.lengths {
		w.remaining = p.packedBits
		n, err := p.frames.WriteFrameTo(i, w)
		if w.err != nil {
			return w.written, w.err
		}
		if err != nil {
			return w.written, err
		}
		if n != length || w.remaining != 0 {
			return w.written, fmt.Errorf("%w: packed frame %d wrote %d bytes, declared %d", ErrFrameSource, i+1, n, length)
		}
	}
	if w.carryBits > 0 {
		if err := w.put(byte(w.carry)); err != nil {
			return w.written, err
		}
	}
	if p.pad {
		if err := w.put(0); err != nil {
			return w.written, err
		}
	}
	err := w.flush()
	return w.written, err
}
