// Package storedsample is the one stored-sample interpretation shared by
// pixeldata, display and render: container width and byte order, the
// BitsStored/HighBit bit field and sign extension from BitsStored. It applies
// no modality, VOI or presentation transform.
package storedsample

import (
	"encoding/binary"
	"errors"
)

var (
	// ErrBitsAllocated reports a container other than 8, 16 or 32 bits.
	ErrBitsAllocated = errors.New("BitsAllocated must be 8, 16 or 32")
	// ErrBitsStored reports BitsStored outside 1..BitsAllocated.
	ErrBitsStored = errors.New("BitsStored must be between 1 and BitsAllocated")
	// ErrHighBit reports a HighBit that does not place BitsStored inside the
	// container.
	ErrHighBit = errors.New("HighBit must be between BitsStored-1 and BitsAllocated-1")
)

// Reader interprets one stored sample. The zero Reader reads nothing and
// returns 0.
type Reader struct {
	width   int
	shift   uint
	mask    uint64
	signBit uint64
	order   binary.ByteOrder
}

// New returns a Reader for a valid format. A nil order is little-endian.
func New(bitsAllocated, bitsStored, highBit int, signed bool, order binary.ByteOrder) (Reader, error) {
	if bitsAllocated != 8 && bitsAllocated != 16 && bitsAllocated != 32 {
		return Reader{}, ErrBitsAllocated
	}
	if bitsStored < 1 || bitsStored > bitsAllocated {
		return Reader{}, ErrBitsStored
	}
	if highBit < bitsStored-1 || highBit >= bitsAllocated {
		return Reader{}, ErrHighBit
	}
	return build(bitsAllocated, bitsStored, highBit, signed, order), nil
}

// Legacy keeps the repair that display and render applied before the shared
// reader existed: BitsStored 0 or above the container means the whole
// container, and an impossible HighBit means BitsStored-1. An unsupported
// container yields the zero Reader. New code uses New and reports the error.
func Legacy(bitsAllocated, bitsStored, highBit int, signed bool, order binary.ByteOrder) Reader {
	if bitsAllocated != 8 && bitsAllocated != 16 && bitsAllocated != 32 {
		return Reader{}
	}
	if bitsStored <= 0 || bitsStored > bitsAllocated {
		bitsStored = bitsAllocated
	}
	if highBit+1 < bitsStored || highBit >= bitsAllocated {
		highBit = bitsStored - 1
	}
	return build(bitsAllocated, bitsStored, highBit, signed, order)
}

func build(bitsAllocated, bitsStored, highBit int, signed bool, order binary.ByteOrder) Reader {
	if order == nil {
		order = binary.LittleEndian
	}
	r := Reader{
		width: bitsAllocated / 8,
		shift: uint(highBit + 1 - bitsStored),
		mask:  uint64(1)<<uint(bitsStored) - 1,
		order: order,
	}
	if signed {
		r.signBit = uint64(1) << uint(bitsStored-1)
	}
	return r
}

// Width is the container size in bytes; 0 for the zero Reader.
func (r *Reader) Width() int { return r.width }

// Signed reports whether values are sign-extended from BitsStored.
func (r *Reader) Signed() bool { return r.signBit != 0 }

// Value interprets the sample at the start of data, which must hold Width
// bytes.
func (r *Reader) Value(data []byte) int64 {
	var raw uint64
	switch r.width {
	case 1:
		raw = uint64(data[0])
	case 2:
		raw = uint64(r.order.Uint16(data))
	case 4:
		raw = uint64(r.order.Uint32(data))
	default:
		return 0
	}
	raw = raw >> r.shift & r.mask
	if raw&r.signBit != 0 {
		return int64(raw | ^r.mask)
	}
	return int64(raw)
}

// Put replaces the sample field at the start of data. The caller must ensure
// that data holds Width bytes and value fits in Range.
func (r *Reader) Put(data []byte, value int64) {
	fieldMask := r.mask << r.shift
	field := (uint64(value) & r.mask) << r.shift
	switch r.width {
	case 1:
		data[0] = data[0]&^byte(fieldMask) | byte(field)
	case 2:
		word := r.order.Uint16(data)&^uint16(fieldMask) | uint16(field)
		r.order.PutUint16(data, word)
	case 4:
		word := r.order.Uint32(data)&^uint32(fieldMask) | uint32(field)
		r.order.PutUint32(data, word)
	}
}

// Range returns the smallest and largest value the format can hold.
func (r *Reader) Range() (int64, int64) {
	if r.width == 0 {
		return 0, 0
	}
	if r.signBit != 0 {
		return -int64(r.signBit), int64(r.signBit) - 1
	}
	return 0, int64(r.mask)
}
