package jpip

import "math/bits"

// This file measures JPEG 2000 Part 1 packets (ISO/IEC 15444-1 B.9-B.10)
// without decoding code-block data: it reads each packet header to learn the
// header length and the body length, so the packet's byte range is known.

// headerBits reads packet-header bits with the bit-stuffing rule of B.10.1:
// after a 0xFF byte only seven bits of the next byte are used.
type headerBits struct {
	data   []byte
	pos    int
	buf    byte
	avail  uint
	lastFF bool
}

func (r *headerBits) bit() (uint32, error) {
	if r.avail == 0 {
		if r.pos >= len(r.data) {
			return 0, errTruncatedPacket
		}
		r.buf = r.data[r.pos]
		r.pos++
		r.avail = 8
		if r.lastFF {
			r.avail = 7
		}
		r.lastFF = r.buf == 0xff
	}
	r.avail--
	return uint32(r.buf>>r.avail) & 1, nil
}

func (r *headerBits) bits(n int) (uint32, error) {
	var v uint32
	for ; n > 0; n-- {
		b, err := r.bit()
		if err != nil {
			return 0, err
		}
		v = v<<1 | b
	}
	return v, nil
}

// end returns the offset after the header: the partial last byte is
// discarded, and a header whose last byte is 0xFF is followed by one stuffed
// byte.
func (r *headerBits) end() (int, error) {
	if r.lastFF {
		if r.pos >= len(r.data) {
			return 0, errTruncatedPacket
		}
		r.pos++
	}
	return r.pos, nil
}

type packetError string

func (e packetError) Error() string { return string(e) }

const (
	errTruncatedPacket packetError = "truncated JPEG 2000 packet"
	errInvalidPacket   packetError = "invalid JPEG 2000 packet header"
)

// tagTree decodes the tag trees of B.10.2.
type tagTree struct {
	widths []int
	value  [][]int32
	low    [][]int32
}

const tagUnknown = int32(1 << 30)

func newTagTree(w, h int) *tagTree {
	t := &tagTree{}
	for {
		n := w * h
		values := make([]int32, n)
		for i := range values {
			values[i] = tagUnknown
		}
		t.widths = append(t.widths, w)
		t.value = append(t.value, values)
		t.low = append(t.low, make([]int32, n))
		if w <= 1 && h <= 1 {
			return t
		}
		w, h = (w+1)/2, (h+1)/2
	}
}

// decode reports whether the leaf (x, y) has a value below threshold, reading
// only the bits needed to decide.
func (t *tagTree) decode(r *headerBits, x, y int, threshold int32) (bool, error) {
	var low int32
	for level := len(t.widths) - 1; level >= 0; level-- {
		i := (y>>uint(level))*t.widths[level] + x>>uint(level)
		if low > t.low[level][i] {
			t.low[level][i] = low
		} else {
			low = t.low[level][i]
		}
		for low < threshold && low < t.value[level][i] {
			b, err := r.bit()
			if err != nil {
				return false, err
			}
			if b == 1 {
				t.value[level][i] = low
			} else {
				low++
			}
		}
		t.low[level][i] = low
	}
	return t.value[0][y*t.widths[0]+x] < threshold, nil
}

type codeBlockState struct {
	included bool
	passes   int
	lblock   int
}

type bandState struct {
	grid          codeBlockGrid
	inclusion     *tagTree
	zeroBitPlanes *tagTree
	blocks        []codeBlockState
}

// precinctState carries the packet-header state of one precinct across its
// layers.
type precinctState struct {
	bands []bandState
}

func newPrecinctState(grids []codeBlockGrid) *precinctState {
	p := &precinctState{bands: make([]bandState, len(grids))}
	for i, g := range grids {
		if g.cw == 0 || g.ch == 0 {
			continue
		}
		p.bands[i] = bandState{
			grid:          g,
			inclusion:     newTagTree(g.cw, g.ch),
			zeroBitPlanes: newTagTree(g.cw, g.ch),
			blocks:        make([]codeBlockState, g.cw*g.ch),
		}
		for b := range p.bands[i].blocks {
			p.bands[i].blocks[b].lblock = 3
		}
	}
	return p
}

// maxPassesPerBlock bounds coding passes: 3 per bit-plane over at most 74
// magnitude bit-planes (ISO/IEC 15444-1 Annex E guard bits and exponents).
const maxPassesPerBlock = 3*74 - 2

// measurePacket reads the packet for the given layer that starts at
// data[pos:] and returns the offset just past it. Packets of one precinct
// must be measured in layer order with the same state.
func measurePacket(data []byte, pos int, state *precinctState, layer int, scod, cmodes byte) (int, error) {
	if scod&scodSOP != 0 && pos+6 <= len(data) && data[pos] == 0xff && data[pos+1] == 0x91 {
		if data[pos+2] != 0 || data[pos+3] != 4 {
			return 0, errInvalidPacket
		}
		pos += 6
	}
	r := &headerBits{data: data, pos: pos}
	present, err := r.bit()
	if err != nil {
		return 0, err
	}
	var body uint64
	if present == 1 {
		for b := range state.bands {
			band := &state.bands[b]
			for i := range band.blocks {
				block := &band.blocks[i]
				x, y := i%band.grid.cw, i/band.grid.cw
				var included bool
				if !block.included {
					included, err = band.inclusion.decode(r, x, y, int32(layer+1))
				} else {
					var bit uint32
					bit, err = r.bit()
					included = bit == 1
				}
				if err != nil {
					return 0, err
				}
				if !included {
					continue
				}
				if !block.included {
					for threshold := int32(1); ; threshold++ {
						if threshold > 80 {
							return 0, errInvalidPacket
						}
						known, err := band.zeroBitPlanes.decode(r, x, y, threshold)
						if err != nil {
							return 0, err
						}
						if known {
							break
						}
					}
					block.included = true
				}
				passes, err := readPassCount(r)
				if err != nil {
					return 0, err
				}
				for {
					bit, err := r.bit()
					if err != nil {
						return 0, err
					}
					if bit == 0 {
						break
					}
					block.lblock++
					if block.lblock > 32 {
						return 0, errInvalidPacket
					}
				}
				if block.passes+passes > maxPassesPerBlock {
					return 0, errInvalidPacket
				}
				for first := block.passes; first < block.passes+passes; {
					last := min(segmentEnd(first, cmodes), block.passes+passes)
					n := bits.Len(uint(last-first)) - 1 + block.lblock
					if n > 32 {
						return 0, errInvalidPacket
					}
					length, err := r.bits(n)
					if err != nil {
						return 0, err
					}
					body += uint64(length)
					first = last
				}
				block.passes += passes
			}
		}
	}
	end, err := r.end()
	if err != nil {
		return 0, err
	}
	if scod&scodEPH != 0 {
		if end+2 > len(data) || data[end] != 0xff || data[end+1] != 0x92 {
			return 0, errInvalidPacket
		}
		end += 2
	}
	if body > uint64(len(data)-end) {
		return 0, errTruncatedPacket
	}
	return end + int(body), nil
}

// readPassCount decodes the number of new coding passes (Table B.4).
func readPassCount(r *headerBits) (int, error) {
	if b, err := r.bit(); err != nil || b == 0 {
		return 1, err
	}
	if b, err := r.bit(); err != nil || b == 0 {
		return 2, err
	}
	v, err := r.bits(2)
	if err != nil || v < 3 {
		return 3 + int(v), err
	}
	v, err = r.bits(5)
	if err != nil || v < 31 {
		return 6 + int(v), err
	}
	v, err = r.bits(7)
	return 37 + int(v), err
}

// segmentEnd returns the pass that ends the codeword segment containing pass
// p (Annex D.4.1 and Table D.8): without termination modes one segment holds
// every pass; with TERMALL every pass is terminated; with the arithmetic
// coding bypass the first ten passes form one segment, then the raw
// significance and refinement passes of each bit-plane share one and its
// cleanup pass has its own.
func segmentEnd(p int, cmodes byte) int {
	switch {
	case cmodes&cmodeTermAll != 0:
		return p + 1
	case cmodes&cmodeBypass != 0:
		if p < 10 {
			return 10
		}
		if (p-10)%3 == 2 {
			return p + 1
		}
		return p - (p-10)%3 + 2
	default:
		return maxPassesPerBlock + 1
	}
}
