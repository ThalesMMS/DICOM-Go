package htj2k

import "fmt"

// magSgnReader reads the forward MagSgn stream, LSB first. Past the end of
// MagSgn it reads 0xFF, as ISO/IEC 15444-15 specifies.
type magSgnReader struct {
	data    []byte
	pos     int
	acc     uint64
	bits    uint
	unstuff bool
}

func (r *magSgnReader) get(n uint) uint32 {
	for r.bits < n {
		b := byte(0xFF)
		if r.pos < len(r.data) {
			b = r.data[r.pos]
			r.pos++
		}
		width := uint(8)
		if r.unstuff {
			width = 7
		}
		r.acc |= uint64(b&byte(1<<width-1)) << r.bits
		r.bits += width
		r.unstuff = b == 0xFF
	}
	v := uint32(r.acc & (1<<n - 1))
	r.acc >>= n
	r.bits -= n
	return v
}

// melReader decodes MEL symbols from the forward, MSB-first MEL stream.
type melReader struct {
	data     []byte
	pos      int
	cur      byte
	left     uint
	lastFF   bool
	state    int
	run      int  // zero symbols still owed
	pending1 bool // a one follows the owed zeros
}

func (r *melReader) bit() uint8 {
	if r.left == 0 {
		b := byte(0xFF)
		if r.pos < len(r.data) {
			b = r.data[r.pos]
			r.pos++
		}
		r.left = 8
		if r.lastFF {
			r.left = 7
		}
		r.cur = b
		r.lastFF = b == 0xFF
	}
	r.left--
	return r.cur >> r.left & 1
}

func (r *melReader) event() bool {
	for {
		if r.run > 0 {
			r.run--
			return false
		}
		if r.pending1 {
			r.pending1 = false
			return true
		}
		e := int(melExponent[r.state])
		if r.bit() == 1 {
			r.run = 1 << e
			if r.state < 12 {
				r.state++
			}
			continue
		}
		n := 0
		for i := 0; i < e; i++ {
			n = n<<1 | int(r.bit())
		}
		if r.state > 0 {
			r.state--
		}
		r.run, r.pending1 = n, true
	}
}

// vlcReader reads the backward VLC stream, LSB first, from the byte before
// the last of the segment.
type vlcReader struct {
	data    []byte
	pos     int
	acc     uint64
	bits    uint
	unstuff bool
}

func newVLCReader(segment []byte) vlcReader {
	n := len(segment)
	b := segment[n-2]
	r := vlcReader{data: segment, pos: n - 3, acc: uint64(b >> 4), bits: 4}
	if r.acc&7 == 7 {
		r.acc &= 7
		r.bits = 3
	}
	r.unstuff = b|0x0F > 0x8F
	return r
}

func (r *vlcReader) fill(n uint) {
	for r.bits < n {
		b := byte(0)
		if r.pos >= 0 {
			b = r.data[r.pos]
			r.pos--
		}
		width := uint(8)
		if r.unstuff && b&0x7F == 0x7F {
			width = 7
		}
		r.acc |= uint64(b&byte(1<<width-1)) << r.bits
		r.bits += width
		r.unstuff = b > 0x8F
	}
}

func (r *vlcReader) peek7() uint8 {
	r.fill(7)
	return uint8(r.acc & 0x7F)
}

func (r *vlcReader) get(n uint) uint32 {
	r.fill(n)
	v := uint32(r.acc & (1<<n - 1))
	r.acc >>= n
	r.bits -= n
	return v
}

func (r *vlcReader) uPrefix() uint32 {
	switch {
	case r.get(1) == 1:
		return 1
	case r.get(1) == 1:
		return 2
	case r.get(1) == 1:
		return 3
	default:
		return 5
	}
}

func (r *vlcReader) uSuffix(prefix uint32) uint32 {
	switch prefix {
	case 1, 2:
		return prefix
	case 3:
		return 3 + r.get(1)
	}
	s := r.get(5)
	if s < 28 {
		return 5 + s
	}
	return 33 + (s - 28) + 4*r.get(4)
}

// DecodeCleanup decodes the cleanup segment of a width x height code-block
// coded by one HT cleanup pass. It writes each sample's signed magnitude at
// the cleanup bit-plane into out, in raster order; the caller scales it by
// the bit-plane and dequantizes. maxBound limits the exponent bound U of
// any quad (the magnitude bit-planes the code-block may hold plus one) and
// must be at most 31.
func DecodeCleanup(segment []byte, width, height int, out []int32, maxBound int) error {
	if width <= 0 || height <= 0 || width > 1024 || height > 1024 || width*height > 4096 || len(out) != width*height {
		return fmt.Errorf("%w: %dx%d code-block", ErrUnsupported, width, height)
	}
	if maxBound <= 0 || maxBound > 31 {
		maxBound = 31
	}
	lcup := len(segment)
	if lcup < 2 {
		return fmt.Errorf("%w: %d-byte segment", ErrMalformed, lcup)
	}
	scup := int(segment[lcup-1])<<4 | int(segment[lcup-2]&0x0F)
	if scup < 2 || scup > lcup || scup > MaxScup {
		return fmt.Errorf("%w: Scup %d in a %d-byte segment", ErrMalformed, scup, lcup)
	}
	for i := range out {
		out[i] = 0
	}
	pcup := lcup - scup
	ms := magSgnReader{data: segment[:pcup]}
	mel := melReader{data: segment[pcup:]}
	vlc := newVLCReader(segment)

	quadCols := (width + 1) / 2
	quadRows := (height + 1) / 2
	above := make([]uint8, 2*quadCols+3)
	quads := make([]quad, quadCols)
	uOff := make([]bool, quadCols)

	for qy := 0; qy < quadRows; qy++ {
		initial := qy == 0
		row := 0
		if !initial {
			row = 1
		}
		for qx := 0; qx < quadCols; qx += 2 {
			pair := 1
			if qx+1 < quadCols {
				pair = 2
			}
			for k := 0; k < pair; k++ {
				c := qx + k
				q := &quads[c]
				*q = quad{}
				uOff[c] = false
				context := quadContext(quads, above, c, initial)
				if context == 0 && !mel.event() {
					continue
				}
				symbol := decodeTables[row][context][vlc.peek7()]
				if symbol.length == 0 {
					return fmt.Errorf("%w: invalid codeword", ErrMalformed)
				}
				vlc.get(uint(symbol.length))
				q.rho = symbol.rho
				q.code = vlcCode{ek: symbol.ek, e1: symbol.e1}
				uOff[c] = symbol.uOff != 0 && symbol.rho != 0
			}

			ua, ub := readUValues(&vlc, &mel, uOff[qx], pair == 2 && uOff[qx+1], initial)
			for k := 0; k < pair; k++ {
				c := qx + k
				q := &quads[c]
				if q.rho == 0 {
					continue
				}
				kappa := uint32(1)
				if !initial && q.rho&(q.rho-1) != 0 {
					m := max(above[2*c], above[2*c+1], above[2*c+2], above[2*c+3])
					if m > 2 {
						kappa = uint32(m) - 1
					}
				}
				u := ua
				if k == 1 {
					u = ub
				}
				bound := kappa + u
				if bound > uint32(maxBound) {
					return fmt.Errorf("%w: exponent bound %d above %d", ErrMalformed, bound, maxBound)
				}
				for i := 0; i < 4; i++ {
					if q.rho>>i&1 == 0 {
						continue
					}
					x, y := 2*c+i>>1, 2*qy+i&1
					if x >= width || y >= height {
						return fmt.Errorf("%w: significant sample outside the code-block", ErrMalformed)
					}
					m := uint(bound) - uint(q.code.ek>>i&1)
					v := ms.get(m) | uint32(q.code.e1>>i&1)<<m
					if v>>1 >= MaxMagnitude {
						return fmt.Errorf("%w: magnitude above 2^30", ErrMalformed)
					}
					mu := int32(v>>1) + 1
					if v&1 != 0 {
						mu = -mu
					}
					out[y*width+x] = mu
				}
			}
		}
		for x := -1; x <= 2*quadCols+1; x++ {
			e := uint8(0)
			if y := 2*qy + 1; x >= 0 && x < width && y < height {
				e = exponent(magnitude(out[y*width+x]))
			}
			above[x+1] = e
		}
	}
	return nil
}

func readUValues(vlc *vlcReader, mel *melReader, a, b, initial bool) (uint32, uint32) {
	if initial && a && b {
		if mel.event() {
			pa, pb := vlc.uPrefix(), vlc.uPrefix()
			return vlc.uSuffix(pa) + 2, vlc.uSuffix(pb) + 2
		}
		pa := vlc.uPrefix()
		if pa > 2 {
			ub := vlc.get(1) + 1
			return vlc.uSuffix(pa), ub
		}
		pb := vlc.uPrefix()
		ua := vlc.uSuffix(pa)
		return ua, vlc.uSuffix(pb)
	}
	var pa, pb, ua, ub uint32
	if a {
		pa = vlc.uPrefix()
	}
	if b {
		pb = vlc.uPrefix()
	}
	if a {
		ua = vlc.uSuffix(pa)
	}
	if b {
		ub = vlc.uSuffix(pb)
	}
	return ua, ub
}
