package hdf5

import (
	"encoding/binary"
	"math/bits"
)

// metadataChecksum computes the little-endian Jenkins lookup3 hash with
// seed zero, as used for HDF5 metadata checksums and group name indexes.
func metadataChecksum(data []byte) uint32 {
	a := uint32(0xdeadbeef) + uint32(len(data))
	b, c := a, a
	for len(data) > 12 {
		a += binary.LittleEndian.Uint32(data[0:4])
		b += binary.LittleEndian.Uint32(data[4:8])
		c += binary.LittleEndian.Uint32(data[8:12])
		a = (a - c) ^ bits.RotateLeft32(c, 4)
		c += b
		b = (b - a) ^ bits.RotateLeft32(a, 6)
		a += c
		c = (c - b) ^ bits.RotateLeft32(b, 8)
		b += a
		a = (a - c) ^ bits.RotateLeft32(c, 16)
		c += b
		b = (b - a) ^ bits.RotateLeft32(a, 19)
		a += c
		c = (c - b) ^ bits.RotateLeft32(b, 4)
		b += a
		data = data[12:]
	}
	if len(data) == 0 {
		return c
	}
	a += uint32(leUint(data[:min(4, len(data))]))
	if len(data) > 4 {
		b += uint32(leUint(data[4:min(8, len(data))]))
	}
	if len(data) > 8 {
		c += uint32(leUint(data[8:]))
	}
	c = (c ^ b) - bits.RotateLeft32(b, 14)
	a = (a ^ c) - bits.RotateLeft32(c, 11)
	b = (b ^ a) - bits.RotateLeft32(a, 25)
	c = (c ^ b) - bits.RotateLeft32(b, 16)
	a = (a ^ c) - bits.RotateLeft32(c, 4)
	b = (b ^ a) - bits.RotateLeft32(a, 14)
	return (c ^ b) - bits.RotateLeft32(b, 24)
}
