// Package objectview provides read-only, module-internal views over object values.
package objectview

import (
	"encoding/binary"

	"github.com/ThalesMMS/dicom-go/core"
	"github.com/ThalesMMS/dicom-go/object"
)

// Raw is an immutable view over a raw element value. It intentionally does not
// expose the backing byte slice.
type Raw struct {
	data []byte
}

// Len returns the raw value length in bytes.
func (r Raw) Len() int {
	return len(r.data)
}

// Byte returns the byte at index. It panics when index is out of bounds, like a
// byte-slice index operation.
func (r Raw) Byte(index int) byte {
	return r.data[index]
}

// Uint16LE returns the little-endian uint16 at valueIndex. It panics when the
// two-byte value is out of bounds.
func (r Raw) Uint16LE(valueIndex int) uint16 {
	offset := valueIndex * 2
	return binary.LittleEndian.Uint16(r.data[offset : offset+2])
}

// VisitRaw calls visit with a read-only view of tag's raw value. The view is
// valid only for the duration of the callback and must not be retained.
func VisitRaw(obj *object.Object, tag core.Tag, visit func(Raw)) bool {
	if obj == nil || visit == nil {
		return false
	}
	elem, ok := obj.Get(tag)
	if !ok {
		return false
	}
	data, ok := elem.RawBytes()
	if !ok {
		return false
	}
	visit(Raw{data: data})
	return true
}

// VisitNativePixels is VisitRaw for native pixel samples: when obj holds Big
// Endian values (Explicit VR Big Endian) and the element is OW, the view is a
// copy with the bytes of every 16-bit word swapped (PS3.5 8.1.1 encodes OW
// Pixel Data as words, whatever Bits Allocated), so Byte and Uint16LE read
// the samples as in a Little Endian encoding. OB values are byte streams in
// both byte orders and are viewed as stored.
func VisitNativePixels(obj *object.Object, tag core.Tag, visit func(Raw)) bool {
	if obj == nil || visit == nil {
		return false
	}
	elem, ok := obj.Get(tag)
	if !ok {
		return false
	}
	data, ok := elem.RawBytes()
	if !ok {
		return false
	}
	if obj.ValueByteOrder() == binary.BigEndian && elem.VR() == core.VROW {
		swapped := make([]byte, len(data))
		for i := 0; i+1 < len(data); i += 2 {
			swapped[i], swapped[i+1] = data[i+1], data[i]
		}
		if len(data)%2 == 1 {
			swapped[len(data)-1] = data[len(data)-1]
		}
		data = swapped
	}
	visit(Raw{data: data})
	return true
}
