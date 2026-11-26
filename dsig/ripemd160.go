package dsig

import (
	"encoding/binary"
	"hash"
	"math/bits"
)

// RIPEMD-160 (Dobbertin, Bosselaers and Preneel, 1996; ISO/IEC 10118-3),
// written from the published algorithm description. DICOM Digital Signature
// profiles list it among the MAC algorithms and DCMTK dcmsign uses it by
// default; the Go standard library does not provide it.

const ripemd160Size = 20
const ripemd160BlockSize = 64

var (
	ripemdLeftWord = [80]uint8{
		0, 1, 2, 3, 4, 5, 6, 7, 8, 9, 10, 11, 12, 13, 14, 15,
		7, 4, 13, 1, 10, 6, 15, 3, 12, 0, 9, 5, 2, 14, 11, 8,
		3, 10, 14, 4, 9, 15, 8, 1, 2, 7, 0, 6, 13, 11, 5, 12,
		1, 9, 11, 10, 0, 8, 12, 4, 13, 3, 7, 15, 14, 5, 6, 2,
		4, 0, 5, 9, 7, 12, 2, 10, 14, 1, 3, 8, 11, 6, 15, 13,
	}
	ripemdRightWord = [80]uint8{
		5, 14, 7, 0, 9, 2, 11, 4, 13, 6, 15, 8, 1, 10, 3, 12,
		6, 11, 3, 7, 0, 13, 5, 10, 14, 15, 8, 12, 4, 9, 1, 2,
		15, 5, 1, 3, 7, 14, 6, 9, 11, 8, 12, 2, 10, 0, 4, 13,
		8, 6, 4, 1, 3, 11, 15, 0, 5, 12, 2, 13, 9, 7, 10, 14,
		12, 15, 10, 4, 1, 5, 8, 7, 6, 2, 13, 14, 0, 3, 9, 11,
	}
	ripemdLeftShift = [80]uint8{
		11, 14, 15, 12, 5, 8, 7, 9, 11, 13, 14, 15, 6, 7, 9, 8,
		7, 6, 8, 13, 11, 9, 7, 15, 7, 12, 15, 9, 11, 7, 13, 12,
		11, 13, 6, 7, 14, 9, 13, 15, 14, 8, 13, 6, 5, 12, 7, 5,
		11, 12, 14, 15, 14, 15, 9, 8, 9, 14, 5, 6, 8, 6, 5, 12,
		9, 15, 5, 11, 6, 8, 13, 12, 5, 12, 13, 14, 11, 8, 5, 6,
	}
	ripemdRightShift = [80]uint8{
		8, 9, 9, 11, 13, 15, 15, 5, 7, 7, 8, 11, 14, 14, 12, 6,
		9, 13, 15, 7, 12, 8, 9, 11, 7, 7, 12, 7, 6, 15, 13, 11,
		9, 7, 15, 11, 8, 6, 6, 14, 12, 13, 5, 14, 13, 13, 7, 5,
		15, 5, 8, 11, 14, 14, 6, 14, 6, 9, 12, 9, 12, 5, 15, 8,
		8, 5, 12, 9, 12, 5, 14, 6, 8, 13, 6, 5, 15, 13, 11, 11,
	}
	ripemdLeftK  = [5]uint32{0x00000000, 0x5A827999, 0x6ED9EBA1, 0x8F1BBCDC, 0xA953FD4E}
	ripemdRightK = [5]uint32{0x50A28BE6, 0x5C4DD124, 0x6D703EF3, 0x7A6D76E9, 0x00000000}
)

type ripemd160Digest struct {
	h   [5]uint32
	buf [ripemd160BlockSize]byte
	n   int
	len uint64
}

// newRIPEMD160 returns a RIPEMD-160 hash.Hash.
func newRIPEMD160() hash.Hash {
	d := &ripemd160Digest{}
	d.Reset()
	return d
}

func (d *ripemd160Digest) Reset() {
	d.h = [5]uint32{0x67452301, 0xEFCDAB89, 0x98BADCFE, 0x10325476, 0xC3D2E1F0}
	d.n = 0
	d.len = 0
}

func (d *ripemd160Digest) Size() int      { return ripemd160Size }
func (d *ripemd160Digest) BlockSize() int { return ripemd160BlockSize }

func (d *ripemd160Digest) Write(p []byte) (int, error) {
	written := len(p)
	d.len += uint64(written)
	if d.n > 0 {
		copied := copy(d.buf[d.n:], p)
		d.n += copied
		p = p[copied:]
		if d.n == ripemd160BlockSize {
			d.block(d.buf[:])
			d.n = 0
		}
	}
	for len(p) >= ripemd160BlockSize {
		d.block(p[:ripemd160BlockSize])
		p = p[ripemd160BlockSize:]
	}
	d.n += copy(d.buf[:], p)
	return written, nil
}

func (d *ripemd160Digest) Sum(in []byte) []byte {
	clone := *d
	length := clone.len << 3
	var pad [ripemd160BlockSize + 8]byte
	pad[0] = 0x80
	padLength := 56 - int(clone.len%64)
	if padLength <= 0 {
		padLength += 64
	}
	binary.LittleEndian.PutUint64(pad[padLength:], length)
	_, _ = clone.Write(pad[:padLength+8])
	var out [ripemd160Size]byte
	for i, word := range clone.h {
		binary.LittleEndian.PutUint32(out[i*4:], word)
	}
	return append(in, out[:]...)
}

func ripemdF(step int, x, y, z uint32) uint32 {
	switch step / 16 {
	case 0:
		return x ^ y ^ z
	case 1:
		return (x & y) | (^x & z)
	case 2:
		return (x | ^y) ^ z
	case 3:
		return (x & z) | (y & ^z)
	default:
		return x ^ (y | ^z)
	}
}

func (d *ripemd160Digest) block(p []byte) {
	var x [16]uint32
	for i := range x {
		x[i] = binary.LittleEndian.Uint32(p[i*4:])
	}
	al, bl, cl, dl, el := d.h[0], d.h[1], d.h[2], d.h[3], d.h[4]
	ar, br, cr, dr, er := al, bl, cl, dl, el
	for j := 0; j < 80; j++ {
		t := bits.RotateLeft32(al+ripemdF(j, bl, cl, dl)+x[ripemdLeftWord[j]]+ripemdLeftK[j/16], int(ripemdLeftShift[j])) + el
		al, el, dl, cl, bl = el, dl, bits.RotateLeft32(cl, 10), bl, t
		t = bits.RotateLeft32(ar+ripemdF(79-j, br, cr, dr)+x[ripemdRightWord[j]]+ripemdRightK[j/16], int(ripemdRightShift[j])) + er
		ar, er, dr, cr, br = er, dr, bits.RotateLeft32(cr, 10), br, t
	}
	t := d.h[1] + cl + dr
	d.h[1] = d.h[2] + dl + er
	d.h[2] = d.h[3] + el + ar
	d.h[3] = d.h[4] + al + br
	d.h[4] = d.h[0] + bl + cr
	d.h[0] = t
}
