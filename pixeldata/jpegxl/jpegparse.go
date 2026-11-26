package jpegxl

import (
	"encoding/binary"
	"fmt"
)

// JPEG parsing into reconstruction data and quantized coefficients, the
// first half of lossless JPEG recompression. Only Huffman-coded 8-bit
// sequential (SOF0/SOF1) and progressive (SOF2) JPEGs are accepted; any
// stream feature that cannot be restored byte for byte is rejected with
// ErrNotReversible.

func notReversible(format string, args ...any) error {
	return fmt.Errorf("%w: %s", ErrNotReversible, fmt.Sprintf(format, args...))
}

type huffDecoder struct {
	maxCode [18]int32
	valPtr  [17]int32
	minCode [17]int32
	values  []uint8
	ok      bool
}

func newHuffDecoder(h *jpegHuffmanCode) huffDecoder {
	var d huffDecoder
	code := int32(0)
	k := int32(0)
	for l := 1; l <= 16; l++ {
		n := int32(h.counts[l])
		d.valPtr[l] = k
		d.minCode[l] = code
		code += n
		k += n
		if n > 0 {
			d.maxCode[l] = code - 1
		} else {
			d.maxCode[l] = -1
		}
		code <<= 1
	}
	d.maxCode[17] = 0x7FFFFFFF
	for _, v := range h.values {
		if v < 256 {
			d.values = append(d.values, uint8(v))
		}
	}
	d.ok = true
	return d
}

type scanReader struct {
	data    []byte
	pos     int
	acc     uint32
	nbits   uint
	marker  bool // hit a marker; further bits read as zero
	fake    uint // zero bits appended after the marker, at the end of acc
	fakeAll uint // total zero bits appended
	padBits []uint8
}

func (r *scanReader) fill() {
	for r.nbits <= 24 {
		if r.marker || r.pos >= len(r.data) {
			r.marker = true
			r.nbits += 8
			r.fake += 8
			r.fakeAll += 8
			continue
		}
		b := r.data[r.pos]
		if b == 0xFF {
			if r.pos+1 < len(r.data) && r.data[r.pos+1] == 0 {
				r.pos += 2
			} else {
				r.marker = true
				r.nbits += 8
				r.fake += 8
				r.fakeAll += 8
				continue
			}
		} else {
			r.pos++
		}
		r.acc |= uint32(b) << (24 - r.nbits)
		r.nbits += 8
	}
}

func (r *scanReader) bits(n uint) uint32 {
	if n == 0 {
		return 0
	}
	r.fill()
	v := r.acc >> (32 - n)
	r.acc <<= n
	r.nbits -= n
	if r.fake > r.nbits {
		r.fake = r.nbits
	}
	return v
}

// overrun reports whether bits past the end of the entropy data were used.
func (r *scanReader) overrun() bool { return r.fake < r.fakeAll }

func (r *scanReader) decode(d *huffDecoder) (int, bool) {
	code := int32(0)
	for l := 1; l <= 16; l++ {
		code = code<<1 | int32(r.bits(1))
		if code <= d.maxCode[l] {
			idx := d.valPtr[l] + code - d.minCode[l]
			if int(idx) >= len(d.values) {
				return 0, false
			}
			return int(d.values[idx]), true
		}
	}
	return 0, false
}

// alignToByte drops the remaining bits of the current byte, recording them
// as padding, and returns the byte position after the entropy data.
func (r *scanReader) alignToByte() {
	extra := r.nbits % 8
	for i := uint(0); i < extra; i++ {
		r.padBits = append(r.padBits, uint8(r.bits(1)))
	}
}

// bytePos is the stream position of the next unread whole byte.
func (r *scanReader) bytePos() int {
	// Buffered whole bytes have not been consumed yet: walk back.
	pos := r.pos
	n := int((r.nbits - r.fake) / 8)
	for i := 0; i < n; i++ {
		pos--
		if pos > 0 && r.data[pos] == 0 && r.data[pos-1] == 0xFF {
			pos--
		}
	}
	return pos
}

func extendSign(v uint32, n uint) int32 {
	if n == 0 {
		return 0
	}
	if v < 1<<(n-1) {
		return int32(v) - int32(1<<n) + 1
	}
	return int32(v)
}

type parsedJPEG struct {
	data      *jpegData
	sofMarker uint8
	precision int
}

// parseJPEG reads a complete JPEG file.
func parseJPEG(b []byte) (*parsedJPEG, error) {
	if len(b) < 4 || b[0] != 0xFF || b[1] != 0xD8 {
		return nil, notReversible("missing SOI marker")
	}
	j := &jpegData{}
	p := &parsedJPEG{data: j}
	pos := 2
	slotTable := map[uint32]int{} // DQT slot -> index in j.quant
	var dcDec, acDec [4]huffDecoder
	sawSOF := false
	progressive := false
	var padBits []uint8
	for {
		if pos >= len(b) {
			return nil, notReversible("missing EOI marker")
		}
		if b[pos] != 0xFF {
			start := pos
			for pos < len(b) && b[pos] != 0xFF {
				pos++
			}
			if len(j.markerOrder) == 0 || pos-start > 0xFFFF {
				return nil, notReversible("data between markers")
			}
			j.markerOrder = append(j.markerOrder, 0xFF)
			j.interMarker = append(j.interMarker, append([]byte{}, b[start:pos]...))
			continue
		}
		if pos+1 >= len(b) {
			return nil, notReversible("truncated marker")
		}
		m := b[pos+1]
		pos += 2
		if m == 0xD9 {
			j.markerOrder = append(j.markerOrder, m)
			j.tailData = append([]byte{}, b[pos:]...)
			break
		}
		if m < 0xC0 || m == 0xFF || (m >= 0xD0 && m <= 0xD8) {
			return nil, notReversible("unexpected marker %#x", m)
		}
		if pos+2 > len(b) {
			return nil, notReversible("truncated segment")
		}
		length := int(binary.BigEndian.Uint16(b[pos:]))
		if length < 2 || pos+length > len(b) {
			return nil, notReversible("segment length")
		}
		seg := b[pos+2 : pos+length]
		segStart := pos - 1 // the marker byte
		pos += length
		switch {
		case m >= 0xE0 && m <= 0xEF:
			j.markerOrder = append(j.markerOrder, m)
			j.appData = append(j.appData, append([]byte{}, b[segStart:pos]...))
			j.appMarkerType = append(j.appMarkerType, appUnknown)
		case m == 0xFE:
			j.markerOrder = append(j.markerOrder, m)
			j.comData = append(j.comData, append([]byte{}, b[segStart:pos]...))
		case m == 0xDB:
			j.markerOrder = append(j.markerOrder, m)
			first := len(j.quant)
			for len(seg) > 0 {
				pq, tq := uint32(seg[0]>>4), uint32(seg[0]&15)
				if pq > 1 || tq > 3 {
					return nil, notReversible("DQT table")
				}
				n := 1 + 64*(1+int(pq))
				if len(seg) < n {
					return nil, notReversible("truncated DQT")
				}
				var q jpegQuantTable
				q.precision, q.index = pq, tq
				for i := 0; i < 64; i++ {
					var v int32
					if pq == 1 {
						v = int32(binary.BigEndian.Uint16(seg[1+2*i:]))
					} else {
						v = int32(seg[1+i])
					}
					if v == 0 {
						return nil, notReversible("zero quantizer")
					}
					q.values[jpegNaturalOrder[i]] = v
				}
				if _, dup := slotTable[tq]; dup {
					return nil, notReversible("redefined quantization table")
				}
				slotTable[tq] = len(j.quant)
				j.quant = append(j.quant, q)
				seg = seg[n:]
			}
			if len(j.quant) == first {
				return nil, notReversible("empty DQT")
			}
			j.quant[len(j.quant)-1].isLast = true
		case m == 0xC4:
			j.markerOrder = append(j.markerOrder, m)
			first := len(j.huffman)
			for len(seg) > 0 {
				if len(seg) < 17 {
					return nil, notReversible("truncated DHT")
				}
				tc, th := seg[0]>>4, seg[0]&15
				if tc > 1 || th > 3 {
					return nil, notReversible("DHT table")
				}
				var h jpegHuffmanCode
				h.slotID = uint32(tc)<<4 | uint32(th)
				total := 0
				maxLen := 0
				space := 1 << 16
				for l := 1; l <= 16; l++ {
					h.counts[l] = uint32(seg[l])
					total += int(seg[l])
					if seg[l] != 0 {
						maxLen = l
					}
					space -= int(seg[l]) << (16 - l)
				}
				if total == 0 || len(seg) < 17+total || space <= 0 || total > 256 {
					return nil, notReversible("DHT code")
				}
				for i := 0; i < total; i++ {
					v := uint32(seg[17+i])
					if tc == 0 && v > 11 {
						return nil, notReversible("DC Huffman symbol")
					}
					h.values = append(h.values, v)
				}
				h.counts[maxLen]++
				h.values = append(h.values, 256)
				d := newHuffDecoder(&h)
				if tc == 0 {
					dcDec[th] = d
				} else {
					acDec[th] = d
				}
				j.huffman = append(j.huffman, h)
				seg = seg[17+total:]
			}
			if len(j.huffman) == first {
				return nil, notReversible("empty DHT")
			}
			j.huffman[len(j.huffman)-1].isLast = true
		case m == 0xDD:
			if len(seg) != 2 {
				return nil, notReversible("DRI length")
			}
			if containsMarker(j.markerOrder, 0xDD) && j.restartInterval != uint32(binary.BigEndian.Uint16(seg)) {
				return nil, notReversible("changing restart interval")
			}
			j.markerOrder = append(j.markerOrder, m)
			j.restartInterval = uint32(binary.BigEndian.Uint16(seg))
		case m == 0xC0 || m == 0xC1 || m == 0xC2:
			if sawSOF {
				return nil, notReversible("multiple frames")
			}
			sawSOF = true
			progressive = m == 0xC2
			j.markerOrder = append(j.markerOrder, m)
			p.sofMarker = m
			if len(seg) < 6 {
				return nil, notReversible("SOF length")
			}
			p.precision = int(seg[0])
			if p.precision != 8 {
				return nil, notReversible("%d-bit JPEG", p.precision)
			}
			j.height = int(binary.BigEndian.Uint16(seg[1:]))
			j.width = int(binary.BigEndian.Uint16(seg[3:]))
			nc := int(seg[5])
			if j.width == 0 || j.height == 0 || (nc != 1 && nc != 3) || len(seg) != 6+3*nc {
				return nil, notReversible("SOF parameters")
			}
			maxH, maxV := 1, 1
			for i := 0; i < nc; i++ {
				c := seg[6+3*i:]
				comp := jpegComponent{id: uint32(c[0]), hSamp: int(c[1] >> 4), vSamp: int(c[1] & 15)}
				if comp.hSamp < 1 || comp.hSamp > 2 || comp.vSamp < 1 || comp.vSamp > 2 {
					return nil, notReversible("sampling factors")
				}
				qi, ok := slotTable[uint32(c[2])]
				if !ok {
					return nil, notReversible("component quantization table undefined")
				}
				comp.quantIdx = uint32(qi)
				maxH, maxV = max(maxH, comp.hSamp), max(maxV, comp.vSamp)
				j.components = append(j.components, comp)
			}
			for i := range j.components {
				for k := i + 1; k < nc; k++ {
					if j.components[i].id == j.components[k].id {
						return nil, notReversible("duplicate component id")
					}
				}
			}
			mcuX, mcuY := divCeil(j.width, 8*maxH), divCeil(j.height, 8*maxV)
			for i := range j.components {
				c := &j.components[i]
				c.widthBlocks = mcuX * c.hSamp
				c.heightBlocks = mcuY * c.vSamp
				c.coeffs = make([]int16, 64*c.widthBlocks*c.heightBlocks)
			}
		case m == 0xDA:
			if !sawSOF {
				return nil, notReversible("scan before frame")
			}
			j.markerOrder = append(j.markerOrder, m)
			if len(seg) < 1 {
				return nil, notReversible("SOS length")
			}
			ns := int(seg[0])
			if ns < 1 || ns > 3 || len(seg) != 4+2*ns {
				return nil, notReversible("SOS parameters")
			}
			var s jpegScanInfo
			s.numComponents = uint32(ns)
			for k := 0; k < ns; k++ {
				id := uint32(seg[1+2*k])
				idx := -1
				for i, c := range j.components {
					if c.id == id {
						idx = i
					}
				}
				if idx < 0 {
					return nil, notReversible("scan component")
				}
				s.components[k] = jpegScanComponent{compIdx: uint32(idx), dcTbl: uint32(seg[2+2*k] >> 4), acTbl: uint32(seg[2+2*k] & 15)}
				if s.components[k].dcTbl > 3 || s.components[k].acTbl > 3 {
					return nil, notReversible("scan table index")
				}
			}
			t := seg[1+2*ns:]
			s.ss, s.se, s.ah, s.al = uint32(t[0]), uint32(t[1]), uint32(t[2]>>4), uint32(t[2]&15)
			if !progressive && (s.ss != 0 || s.se != 63 || s.ah != 0 || s.al != 0) {
				return nil, notReversible("sequential scan parameters")
			}
			if s.se > 63 || s.ss > s.se || s.al > 13 || s.ah > 13 {
				return nil, notReversible("scan parameters")
			}
			end, err := decodeScan(b, pos, j, &s, progressive, &dcDec, &acDec, containsMarker(j.markerOrder, 0xDD), &padBits)
			if err != nil {
				return nil, err
			}
			j.scans = append(j.scans, s)
			pos = end
		default:
			return nil, notReversible("unsupported marker %#x", m)
		}
	}
	if !sawSOF || len(j.scans) == 0 {
		return nil, notReversible("no image data")
	}
	if len(j.quant) > 3 {
		return nil, notReversible("more than three quantization tables")
	}
	for _, v := range padBits {
		if v == 0 {
			j.hasZeroPadding = true
			j.paddingBits = padBits
			break
		}
	}
	return p, nil
}

func containsMarker(order []uint8, m uint8) bool {
	for _, v := range order {
		if v == m {
			return true
		}
	}
	return false
}

// decodeScan decodes one scan starting at pos and returns the position of
// the marker that follows it.
func decodeScan(b []byte, pos int, j *jpegData, s *jpegScanInfo, progressive bool, dcDec, acDec *[4]huffDecoder, haveDRI bool, padBits *[]uint8) (int, error) {
	if progressive && !(s.ss == 0 && s.se == 63 && s.ah == 0 && s.al == 0) {
		return 0, notReversible("progressive JPEG recompression is not supported")
	}
	restart := 0
	if haveDRI {
		restart = int(j.restartInterval)
	}
	r := &scanReader{data: b, pos: pos}
	perRow, rows := j.mcuSize(s)
	n := int(s.numComponents)
	var lastDC [4]int32
	blockIdx := 0
	restartsToGo := restart
	nextRST := 0
	for mcuY := 0; mcuY < rows; mcuY++ {
		for mcuX := 0; mcuX < perRow; mcuX++ {
			if restart > 0 && restartsToGo == 0 {
				r.alignToByte()
				p := r.bytePos()
				if p+1 >= len(b) || b[p] != 0xFF || b[p+1] != byte(0xD0+nextRST) {
					return 0, notReversible("missing restart marker")
				}
				nextRST = (nextRST + 1) & 7
				r = &scanReader{data: b, pos: p + 2, padBits: r.padBits}
				restartsToGo = restart
				lastDC = [4]int32{}
			}
			for k := 0; k < n; k++ {
				sc := s.components[k]
				comp := &j.components[sc.compIdx]
				dcD, acD := &dcDec[sc.dcTbl], &acDec[sc.acTbl]
				if !dcD.ok || !acD.ok {
					return 0, notReversible("scan uses an undefined Huffman table")
				}
				nby, nbx := 1, 1
				if n > 1 {
					nby, nbx = comp.vSamp, comp.hSamp
				}
				for iy := 0; iy < nby; iy++ {
					for ix := 0; ix < nbx; ix++ {
						by, bx := mcuY*nby+iy, mcuX*nbx+ix
						blk := comp.coeffs[64*(by*comp.widthBlocks+bx):]
						runs, err := decodeBlockSequential(r, dcD, acD, blk, &lastDC[sc.compIdx])
						if err != nil {
							return 0, err
						}
						if runs > 0 {
							s.extraZeroRuns = append(s.extraZeroRuns, extraZeroRun{blockIdx: uint32(blockIdx), numRuns: uint32(runs)})
						}
						blockIdx++
					}
				}
			}
			restartsToGo--
			if r.overrun() {
				return 0, notReversible("scan data ends early")
			}
		}
	}
	r.alignToByte()
	*padBits = append(*padBits, r.padBits...)
	end := r.bytePos()
	if end+1 >= len(b) || b[end] != 0xFF {
		return 0, notReversible("scan not followed by a marker")
	}
	return end, nil
}

// decodeBlockSequential decodes one block and returns the number of zero-run
// symbols written before the end of block without a following coefficient.
func decodeBlockSequential(r *scanReader, dcD, acD *huffDecoder, blk []int16, lastDC *int32) (int, error) {
	s, ok := r.decode(dcD)
	if !ok || s > 11 {
		return 0, notReversible("bad DC code")
	}
	diff := extendSign(r.bits(uint(s)), uint(s))
	dc := *lastDC + diff
	if dc < -32768 || dc > 32767 {
		return 0, notReversible("DC overflow")
	}
	*lastDC = dc
	blk[0] = int16(dc)
	runs := 0
	for k := 1; k < 64; {
		sym, ok := r.decode(acD)
		if !ok {
			return 0, notReversible("bad AC code")
		}
		run, size := sym>>4, sym&15
		if size == 0 {
			if run == 15 {
				runs++
				k += 16
				if k > 64 {
					return 0, notReversible("zero run past the block end")
				}
				continue
			}
			if run != 0 {
				return 0, notReversible("invalid AC symbol")
			}
			return runs, nil // EOB
		}
		runs = 0
		k += run
		if k > 63 {
			return 0, notReversible("AC index past the block end")
		}
		blk[jpegNaturalOrder[k]] = int16(extendSign(r.bits(uint(size)), uint(size)))
		k++
	}
	return runs, nil
}
