package dicomxml

import (
	"encoding/binary"
	"io"

	"github.com/ThalesMMS/dicom-go/core"
	"github.com/ThalesMMS/dicom-go/internal/valueencode"
	"github.com/ThalesMMS/dicom-go/object"
)

// encodeLittleEndianWidth returns the unit at which the encoder reverses
// elem's value to write it in Little Endian, or 1 when it is written as is.
// Bits Allocated is read in the effective ByteOrder, which may differ from the
// object's own.
func (e *nativeEncoder) encodeLittleEndianWidth(obj *object.Object, elem core.Element) int {
	if e.opts.SourceByteOrderInlineBinary || e.opts.ByteOrder != binary.BigEndian {
		return 1
	}
	if _, fragments := elem.Value.(core.FragmentSequence); fragments {
		return 1
	}
	var bits int64
	if elem.Tag() == core.TagPixelData && elem.VR() == core.VROW {
		// An item never inherits its parent's Bits Allocated.
		if allocated, ok := obj.Get(valueencode.BitsAllocatedTag); ok {
			bits = valueencode.BitsAllocated(allocated, e.opts.ByteOrder)
		}
	}
	return valueencode.LittleEndianUnitWidth(elem.Tag(), elem.VR(), bits)
}

// elementsBitsAllocated returns the Bits Allocated among decoded elements of
// one data set or item, or 0.
func elementsBitsAllocated(elements []core.Element, order binary.ByteOrder) int64 {
	for _, elem := range elements {
		if elem.Tag() == valueencode.BitsAllocatedTag {
			return valueencode.BitsAllocated(elem, order)
		}
	}
	return 0
}

// toValueByteOrder converts a decoded Little Endian OW, OF, OD, OL or OV
// Value Field to opts.ByteOrder, unless SourceByteOrderInlineBinary takes it
// as already in that order. The decoder owns the bytes, which are reversed in
// place.
func toValueByteOrder(elem core.Element, bitsAllocated int64, opts UnmarshalOptions) core.Element {
	if opts.SourceByteOrderInlineBinary || opts.ByteOrder != binary.BigEndian {
		return elem
	}
	raw, ok := elem.Value.(core.RawValue)
	if !ok || len(raw) == 0 {
		return elem
	}
	if width := valueencode.LittleEndianUnitWidth(elem.Tag(), elem.VR(), bitsAllocated); width > 1 {
		valueencode.ReverseUnitsInPlace(raw, width)
	}
	return elem
}

// xmlReverseWriter reverses the bytes of every width-sized unit written
// through it. Close writes a trailing partial unit unchanged, as
// valueencode.ReverseFixedWidth does.
type xmlReverseWriter struct {
	writer  io.Writer
	width   int
	pending int
	unit    [8]byte
	buf     []byte
}

func (w *xmlReverseWriter) Write(data []byte) (int, error) {
	total := len(data)
	for len(data) > 0 {
		if w.pending > 0 || len(data) < w.width {
			n := copy(w.unit[w.pending:w.width], data)
			w.pending += n
			data = data[n:]
			if w.pending == w.width {
				w.pending = 0
				valueencode.ReverseUnitsInPlace(w.unit[:w.width], w.width)
				if _, err := w.writer.Write(w.unit[:w.width]); err != nil {
					return total - len(data), err
				}
			}
			continue
		}
		if w.buf == nil {
			w.buf = make([]byte, 32<<10)
		}
		whole := min(len(data)-len(data)%w.width, len(w.buf)-len(w.buf)%w.width)
		chunk := w.buf[:whole]
		copy(chunk, data)
		valueencode.ReverseUnitsInPlace(chunk, w.width)
		if _, err := w.writer.Write(chunk); err != nil {
			return total - len(data), err
		}
		data = data[whole:]
	}
	return total, nil
}

// Close writes a trailing partial unit.
func (w *xmlReverseWriter) Close() error {
	if w.pending == 0 {
		return nil
	}
	n := w.pending
	w.pending = 0
	_, err := w.writer.Write(w.unit[:n])
	return err
}
