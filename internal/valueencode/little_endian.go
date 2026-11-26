package valueencode

import (
	"encoding/binary"

	"github.com/ThalesMMS/dicom-go/core"
)

// BitsAllocatedTag is Bits Allocated (0028,0100), which sets the sample width
// of OW Pixel Data in the same data set or item.
var BitsAllocatedTag = core.NewTag(0x0028, 0x0100)

// LittleEndianUnitWidth returns the unit whose bytes are reversed between the
// Big Endian Value Field of tag and the Little Endian form DICOM JSON and the
// Native DICOM Model give it: the sample width for OW Pixel Data with
// bitsAllocated 32 or 64, by the rules of pixeldata.TranscodeDataSet, the VR
// width for OW, OF, OD, OL and OV, and 1 for any other VR.
func LittleEndianUnitWidth(tag core.Tag, vr core.VR, bitsAllocated int64) int {
	switch vr {
	case core.VROW:
		if tag == core.TagPixelData && (bitsAllocated == 32 || bitsAllocated == 64) {
			return int(bitsAllocated / 8)
		}
		return ByteOrderWidth(vr)
	case core.VROF, core.VROD, core.VROL, core.VROV:
		return ByteOrderWidth(vr)
	default:
		return 1
	}
}

// ReverseUnitsInPlace reverses the bytes of every whole width-sized unit of
// data, which the caller owns; a trailing partial unit is kept, as
// ReverseFixedWidth keeps it.
func ReverseUnitsInPlace(data []byte, width int) {
	for start := 0; start+width <= len(data); start += width {
		for a, b := start, start+width-1; a < b; a, b = a+1, b-1 {
			data[a], data[b] = data[b], data[a]
		}
	}
}

// BitsAllocated returns the value of a Bits Allocated element: its raw US
// Value Field read in order, the byte order the value is written in, or its
// first typed value; 0 when it holds neither.
func BitsAllocated(elem core.Element, order binary.ByteOrder) int64 {
	if raw, ok := elem.RawBytes(); ok {
		if len(raw) < 2 || order == nil {
			return 0
		}
		return int64(order.Uint16(raw))
	}
	switch values := elem.Value.(type) {
	case core.Uint16Value:
		if len(values) > 0 {
			return int64(values[0])
		}
	case core.Int16Value:
		if len(values) > 0 {
			return int64(values[0])
		}
	case core.Uint32Value:
		if len(values) > 0 {
			return int64(values[0])
		}
	case core.Int32Value:
		if len(values) > 0 {
			return int64(values[0])
		}
	case core.Int64Value:
		if len(values) > 0 {
			return values[0]
		}
	}
	return 0
}
