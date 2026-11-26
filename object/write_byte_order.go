package object

import (
	"encoding/binary"
	"fmt"
	"io"

	"github.com/ThalesMMS/dicom-go/core"
	"github.com/ThalesMMS/dicom-go/internal/valueencode"
)

// The writers encode raw values in the byte order of the destination
// transfer syntax. An Object keeps its raw binary values in ValueByteOrder
// (the order of the syntax it was read from), so writing it in a syntax of
// the other byte order reverses every fixed-width value first, as PS3.5 7.3
// requires:
//
//   - AT, OW, SS and US by 2 bytes; FL, OF, OL, SL and UL by 4; FD, OD, OV,
//     SV and UV by 8. OB, UN, text and encapsulated fragments are byte
//     streams and keep their bytes.
//   - Native OW Pixel Data with Bits Allocated 32 or 64 reverses each sample,
//     as the pixeldata transcoder and decoders read it; Bits Allocated comes
//     from the same data set or item.
//   - Sequence items are converted at any depth, and values deferred on the
//     source are reversed while they stream.
//   - A value whose length is not a multiple of its unit is refused rather
//     than written half converted, unless the writer keeps partial units
//     (WriteFileOptions.KeepPartialByteOrderUnits): then its whole units are
//     reversed and the trailing partial unit is written unchanged.
//   - core.BulkDataValue sources are written as the resolver returns them.
//   - Pixel Data that WriteFileFrames pulls from a FrameSource is already in
//     the destination order (the FrameSource contract) and keeps its bytes.

var tagWriteBitsAllocated = core.NewTag(0x0028, 0x0100)

// destinationOrderValueProvider is a value provider some of whose deferred
// values are produced in the byte order of the destination transfer syntax
// rather than in the object's ValueByteOrder.
type destinationOrderValueProvider interface {
	valueInDestinationOrder(tag core.Tag) bool
}

// deferredValueNeedsReversal reports whether the deferred value of tag that
// provider streams must be reversed when the object's order differs from
// the destination order.
func deferredValueNeedsReversal(provider valueProvider, tag core.Tag) bool {
	destination, ok := provider.(destinationOrderValueProvider)
	return !ok || !destination.valueInDestinationOrder(tag)
}

// isBigEndianOrder reports whether order writes the most significant byte
// first. A nil order is the Little Endian default of constructed objects.
func isBigEndianOrder(order binary.ByteOrder) bool {
	if order == nil {
		return false
	}
	var probe [2]byte
	order.PutUint16(probe[:], 1)
	return probe[0] == 0
}

// valueOrderDiffers reports whether raw values held in order must be
// reversed to be written in target.
func valueOrderDiffers(order, target binary.ByteOrder) bool {
	return isBigEndianOrder(order) != isBigEndianOrder(target)
}

// pixelDataSampleWidth is the byte-order unit of native OW Pixel Data in a
// data set level whose raw values are in order: the sample size for Bits
// Allocated 32 or 64, otherwise 0 (the VR rule applies).
func pixelDataSampleWidth(elements []core.Element, order binary.ByteOrder) int {
	for _, element := range elements {
		if element.Tag() != tagWriteBitsAllocated {
			continue
		}
		var bits uint16
		switch value := element.Value.(type) {
		case core.Uint16Value:
			if len(value) != 1 {
				return 0
			}
			bits = value[0]
		case core.RawValue:
			if len(value) != 2 {
				return 0
			}
			if order == nil {
				order = binary.LittleEndian
			}
			bits = order.Uint16(value)
		default:
			return 0
		}
		switch bits {
		case 32:
			return 4
		case 64:
			return 8
		}
		return 0
	}
	return 0
}

// byteOrderUnit is the width whose bytes a change of byte order reverses in
// element, given the Pixel Data sample width of its data set level.
func byteOrderUnit(element core.Element, sampleWidth int) int {
	width := valueencode.ByteOrderWidth(element.VR())
	if element.Tag() == core.TagPixelData && element.VR() == core.VROW && sampleWidth > width {
		width = sampleWidth
	}
	return width
}

// reverseElementsByteOrder returns a copy of elements whose raw values are in
// order with every fixed-width value reversed, through sequence items. Item
// offsets are kept so deferred nested values still bind to their source.
// A value that is not a whole number of units is refused unless keepPartial
// is set, which writes its trailing partial unit unchanged. elements is not
// modified.
func reverseElementsByteOrder(elements []core.Element, order binary.ByteOrder, keepPartial bool) ([]core.Element, error) {
	sampleWidth := pixelDataSampleWidth(elements, order)
	out := make([]core.Element, len(elements))
	for i, element := range elements {
		switch value := element.Value.(type) {
		case core.RawValue:
			if width := byteOrderUnit(element, sampleWidth); width > 1 {
				if len(value)%width != 0 && !keepPartial {
					return nil, byteOrderExtentError(element, width)
				}
				element.Value = core.RawValue(valueencode.ReverseFixedWidth(value, width))
			}
		case core.SequenceValue:
			items := make([]core.DataSet, len(value.Items))
			for j, item := range value.Items {
				converted, err := reverseElementsByteOrder(item.Elements, order, keepPartial)
				if err != nil {
					return nil, err
				}
				item.Elements = converted
				items[j] = item
			}
			element.Value = core.SequenceValue{Items: items}
		}
		out[i] = element
	}
	return out, nil
}

func byteOrderExtentError(element core.Element, width int) error {
	return fmt.Errorf("dicom: cannot change the byte order of %s with VR %s: value length is not a multiple of %d", element.Tag(), element.VR(), width)
}

// reversingCopy wraps copyValue, which streams a deferred value of element, so
// the value is reversed by unit while it is written. A value of undefined
// length (encapsulated Pixel Data) or of a byte-stream VR is copied as is. A
// value that is not a whole number of units is refused unless keepPartial is
// set, which writes its trailing partial unit unchanged.
func reversingCopy(element core.Element, sampleWidth int, keepPartial bool, copyValue func(io.Writer) (int64, error)) func(io.Writer) (int64, error) {
	width := byteOrderUnit(element, sampleWidth)
	if width <= 1 || copyValue == nil || !element.Header.HasLength() || element.Header.Length.IsUndefined() {
		return copyValue
	}
	if int64(element.Header.Length)%int64(width) != 0 && !keepPartial {
		return func(io.Writer) (int64, error) { return 0, byteOrderExtentError(element, width) }
	}
	return func(w io.Writer) (int64, error) {
		reversing := &reversingWriter{w: w, width: width}
		n, err := copyValue(reversing)
		if flushErr := reversing.flush(); err == nil {
			err = flushErr
		}
		return n, err
	}
}

// reversingDeferredValue wraps a parser.WriterOptions.DeferredValue so the
// nested deferred values it streams are reversed. The item it receives has
// already been converted, so Bits Allocated is read in target order.
func reversingDeferredValue(deferred func(core.DataSet, core.Element) (func(io.Writer) (int64, error), bool), target binary.ByteOrder, keepPartial bool) func(core.DataSet, core.Element) (func(io.Writer) (int64, error), bool) {
	if deferred == nil {
		return nil
	}
	return func(item core.DataSet, element core.Element) (func(io.Writer) (int64, error), bool) {
		copyValue, ok := deferred(item, element)
		if !ok {
			return copyValue, ok
		}
		return reversingCopy(element, pixelDataSampleWidth(item.Elements, target), keepPartial, copyValue), true
	}
}
