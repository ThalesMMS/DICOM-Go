package jpeg2000

// Tier-2 of ISO/IEC 15444-1 (Annex B.9 and B.10): packet headers. Each
// resolution of each component forms one precinct (the maximal precinct
// size) and, with one quality layer, one packet.

// headerWriter writes packet header bits MSB first, stuffing a zero bit at
// the top of the byte after an 0xFF (B.10.1).
type headerWriter struct {
	buf  []byte
	cur  byte
	free uint // free bits in cur
}

func newHeaderWriter() headerWriter { return headerWriter{free: 8} }

func (w *headerWriter) bit(b int) {
	w.cur = w.cur<<1 | byte(b&1)
	w.free--
	if w.free == 0 {
		w.buf = append(w.buf, w.cur)
		w.free = 8
		if w.cur == 0xFF {
			w.free = 7
		}
		w.cur = 0
	}
}

func (w *headerWriter) bits(v uint32, n int) {
	for i := n - 1; i >= 0; i-- {
		w.bit(int(v >> uint(i) & 1))
	}
}

// finish pads the header to a byte boundary; a header ending in 0xFF gets a
// stuffed zero byte so that the packet body cannot form a marker code.
func (w *headerWriter) finish() []byte {
	full := uint(8)
	if len(w.buf) > 0 && w.buf[len(w.buf)-1] == 0xFF {
		full = 7
	}
	if w.free != full {
		w.buf = append(w.buf, w.cur<<w.free)
	} else if len(w.buf) > 0 && w.buf[len(w.buf)-1] == 0xFF {
		w.buf = append(w.buf, 0)
	}
	return w.buf
}

// tagTree is the tag tree of B.10.2 over a w x h grid of leaves.
type tagTree struct {
	levels []tagLevel
}

type tagLevel struct {
	w, h  int
	value []int
	low   []int
	known []bool
}

func newTagTree(w, h int, values []int) *tagTree {
	t := &tagTree{}
	for {
		level := tagLevel{w: w, h: h, value: make([]int, w*h), low: make([]int, w*h), known: make([]bool, w*h)}
		t.levels = append(t.levels, level)
		if w == 1 && h == 1 {
			break
		}
		w, h = (w+1)/2, (h+1)/2
	}
	copy(t.levels[0].value, values)
	for l := 1; l < len(t.levels); l++ {
		parent, child := t.levels[l], t.levels[l-1]
		for i := range parent.value {
			parent.value[i] = int(^uint(0) >> 1)
		}
		for y := 0; y < child.h; y++ {
			for x := 0; x < child.w; x++ {
				p := (y/2)*parent.w + x/2
				if v := child.value[y*child.w+x]; v < parent.value[p] {
					parent.value[p] = v
				}
			}
		}
	}
	return t
}

// encode writes what a decoder needs to learn whether the leaf's value is
// below threshold, and the value itself if it is.
func (t *tagTree) encode(w *headerWriter, x, y, threshold int) {
	low := 0
	for l := len(t.levels) - 1; l >= 0; l-- {
		level := &t.levels[l]
		i := (y>>uint(l))*level.w + x>>uint(l)
		if low > level.low[i] {
			level.low[i] = low
		} else {
			low = level.low[i]
		}
		for low < threshold {
			if low >= level.value[i] {
				if !level.known[i] {
					w.bit(1)
					level.known[i] = true
				}
				break
			}
			w.bit(0)
			low++
		}
		level.low[i] = low
	}
}

// passCount writes the number of coding passes (Table B.4).
func passCount(w *headerWriter, n int) {
	switch {
	case n == 1:
		w.bit(0)
	case n == 2:
		w.bits(2, 2)
	case n <= 5:
		w.bits(3, 2)
		w.bits(uint32(n-3), 2)
	case n <= 36:
		w.bits(0xF, 4)
		w.bits(uint32(n-6), 5)
	default:
		w.bits(0x1FF, 9)
		w.bits(uint32(n-37), 7)
	}
}

func floorLog2(n int) int {
	r := 0
	for n > 1 {
		n >>= 1
		r++
	}
	return r
}

func bitLength(n int) int {
	r := 0
	for n > 0 {
		n >>= 1
		r++
	}
	return r
}

// codedBlock is one code-block's contribution to its packet.
type codedBlock struct {
	data   []byte
	passes int
	planes int // magnitude bit-planes K (Part 1) or 0 for HT
}

// packetBand is one subband of a packet: its code-block grid and blocks in
// raster order.
type packetBand struct {
	cols, rows int
	blocks     []codedBlock
	mb         int // magnitude bit-planes Mb of the subband
}

// writePacket emits the header and body of one single-layer packet. ht
// selects the HT reading of the zero bit-plane count (Mb-1-p, p = 0) for
// coded blocks.
func writePacket(out []byte, bands []packetBand, ht bool) []byte {
	w := newHeaderWriter()
	empty := true
	for _, band := range bands {
		for _, block := range band.blocks {
			if block.passes > 0 {
				empty = false
			}
		}
	}
	if empty {
		w.bit(0)
		return append(out, w.finish()...)
	}
	w.bit(1)
	for _, band := range bands {
		if len(band.blocks) == 0 {
			continue
		}
		inclusion := make([]int, len(band.blocks))
		zero := make([]int, len(band.blocks))
		for i, block := range band.blocks {
			if block.passes == 0 {
				// Never coded; a large value keeps the tree's minima those
				// of the included blocks.
				inclusion[i] = 1
				zero[i] = band.mb
				continue
			}
			if ht {
				zero[i] = band.mb - 1
			} else {
				zero[i] = band.mb - block.planes
			}
		}
		inclusionTree := newTagTree(band.cols, band.rows, inclusion)
		zeroTree := newTagTree(band.cols, band.rows, zero)
		for i, block := range band.blocks {
			x, y := i%band.cols, i/band.cols
			inclusionTree.encode(&w, x, y, 1)
			if block.passes == 0 {
				continue
			}
			zeroTree.encode(&w, x, y, 1<<30)
			passCount(&w, block.passes)
			lblock := 3
			lengthBits := bitLength(len(block.data))
			extra := lengthBits - floorLog2(block.passes) - lblock
			for ; extra > 0; extra-- {
				w.bit(1)
				lblock++
			}
			w.bit(0)
			w.bits(uint32(len(block.data)), lblock+floorLog2(block.passes))
		}
	}
	out = append(out, w.finish()...)
	for _, band := range bands {
		for _, block := range band.blocks {
			out = append(out, block.data...)
		}
	}
	return out
}
