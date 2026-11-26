package jpegxl

import "math/bits"

// JPEG serialization from reconstruction data and quantized coefficients
// (ISO/IEC 18181-2 Annex, JPEG bitstream reconstruction).

// jpegNaturalOrder maps zig-zag index to natural (row-major) index.
var jpegNaturalOrder = [64]int{
	0, 1, 8, 16, 9, 2, 3, 10, 17, 24, 32, 25, 18, 11, 4, 5,
	12, 19, 26, 33, 40, 48, 41, 34, 27, 20, 13, 6, 7, 14, 21, 28,
	35, 42, 49, 56, 57, 50, 43, 36, 29, 22, 15, 23, 30, 37, 44, 51,
	58, 59, 52, 45, 38, 31, 39, 46, 53, 60, 61, 54, 47, 55, 62, 63,
}

type huffTable struct {
	depth [256]uint8
	code  [256]uint16
	ok    bool
}

func buildHuffTable(h *jpegHuffmanCode) (huffTable, bool) {
	var t huffTable
	var sizes []uint8
	for l := 1; l <= 16; l++ {
		for i := uint32(0); i < h.counts[l]; i++ {
			sizes = append(sizes, uint8(l))
		}
	}
	if h.counts[0] != 0 || len(sizes) != len(h.values) || len(sizes) == 0 {
		return t, false
	}
	code := 0
	si := sizes[0]
	p := 0
	codes := make([]int, len(sizes))
	for p < len(sizes) {
		for p < len(sizes) && sizes[p] == si {
			codes[p] = code
			code++
			p++
		}
		code <<= 1
		si++
	}
	for p := 0; p < len(sizes)-1; p++ {
		v := h.values[p]
		if v > 255 {
			return t, false
		}
		t.depth[v] = sizes[p]
		t.code[v] = uint16(codes[p])
	}
	t.ok = true
	return t, true
}

type jpegBitWriter struct {
	out   []byte
	acc   uint64
	nbits uint
	ok    bool
}

func (w *jpegBitWriter) emit(b byte) {
	w.out = append(w.out, b)
	if b == 0xFF {
		w.out = append(w.out, 0)
	}
}

func (w *jpegBitWriter) writeBits(n uint, v uint64) {
	if n == 0 {
		return
	}
	w.acc = w.acc<<n | v&(1<<n-1)
	w.nbits += n
	for w.nbits >= 8 {
		w.nbits -= 8
		w.emit(byte(w.acc >> w.nbits))
	}
	w.acc &= 1<<w.nbits - 1
}

func (w *jpegBitWriter) writeSymbol(sym int, t *huffTable) {
	if sym > 255 || t.depth[sym] == 0 {
		w.ok = false
		return
	}
	w.writeBits(uint(t.depth[sym]), uint64(t.code[sym]))
}

// padToByte completes the current byte with the padding pattern.
func (w *jpegBitWriter) padToByte(st *serializer) bool {
	n := (8 - w.nbits%8) % 8
	if n == 0 {
		return true
	}
	var pattern uint64
	if st.j.hasZeroPadding {
		for i := uint(0); i < n; i++ {
			if st.padPos >= len(st.j.paddingBits) {
				return false
			}
			pattern = pattern<<1 | uint64(st.j.paddingBits[st.padPos]&1)
			st.padPos++
		}
	} else {
		pattern = 1<<n - 1
	}
	w.writeBits(n, pattern)
	return true
}

type codingState struct {
	eobRun  int
	curAC   *huffTable
	refBits []uint8
}

const maxCorrectionBits = 1 << 16

func (s *codingState) flush(w *jpegBitWriter) {
	if s.eobRun > 0 {
		nb := uint(bits.Len32(uint32(s.eobRun))) - 1
		w.writeSymbol(int(nb<<4), s.curAC)
		w.writeBits(nb, uint64(s.eobRun))
		s.eobRun = 0
	}
	for _, b := range s.refBits {
		w.writeBits(1, uint64(b))
	}
	s.refBits = s.refBits[:0]
}

func (s *codingState) bufferEOB(ac *huffTable, newBits []uint8, w *jpegBitWriter) {
	if s.eobRun == 0 {
		s.curAC = ac
	}
	s.eobRun++
	s.refBits = append(s.refBits, newBits...)
	if s.eobRun == 0x7FFF || len(s.refBits) > maxCorrectionBits-64+1 {
		s.flush(w)
	}
}

type serializer struct {
	j           *jpegData
	out         []byte
	dc          [4]huffTable
	ac          [4]huffTable
	dhtIndex    int
	dqtIndex    int
	appIndex    int
	comIndex    int
	interIndex  int
	scanIndex   int
	padPos      int
	progressive bool
	seenDRI     bool
}

func writeJPEG(j *jpegData) ([]byte, error) {
	st := &serializer{j: j, out: []byte{0xFF, 0xD8}}
	for _, m := range j.markerOrder {
		var err error
		switch {
		case m == 0xC0 || m == 0xC1 || m == 0xC2:
			err = st.sof(m)
		case m == 0xC4:
			err = st.dht()
		case m >= 0xD0 && m <= 0xD7:
			st.out = append(st.out, 0xFF, m)
		case m == 0xD9:
			st.out = append(st.out, 0xFF, 0xD9)
			st.out = append(st.out, j.tailData...)
		case m == 0xDA:
			err = st.scan()
		case m == 0xDB:
			err = st.dqt()
		case m == 0xDD:
			st.seenDRI = true
			st.out = append(st.out, 0xFF, 0xDD, 0, 4, byte(j.restartInterval>>8), byte(j.restartInterval))
		case m >= 0xE0 && m <= 0xEF:
			if st.appIndex >= len(j.appData) {
				return nil, corrupt("APP marker count")
			}
			st.out = append(st.out, 0xFF)
			st.out = append(st.out, j.appData[st.appIndex]...)
			st.appIndex++
		case m == 0xFE:
			if st.comIndex >= len(j.comData) {
				return nil, corrupt("COM marker count")
			}
			st.out = append(st.out, 0xFF)
			st.out = append(st.out, j.comData[st.comIndex]...)
			st.comIndex++
		case m == 0xFF:
			if st.interIndex >= len(j.interMarker) {
				return nil, corrupt("inter-marker data count")
			}
			st.out = append(st.out, j.interMarker[st.interIndex]...)
			st.interIndex++
		default:
			return nil, unsupported("JPEG marker %#x", m)
		}
		if err != nil {
			return nil, err
		}
	}
	if j.hasZeroPadding && st.padPos != len(j.paddingBits) {
		return nil, corrupt("unused JPEG padding bits")
	}
	return st.out, nil
}

func (st *serializer) sof(marker uint8) error {
	j := st.j
	st.progressive = marker == 0xC2
	n := len(j.components)
	length := 8 + 3*n
	st.out = append(st.out, 0xFF, marker, byte(length>>8), byte(length), 8,
		byte(j.height>>8), byte(j.height), byte(j.width>>8), byte(j.width), byte(n))
	for _, c := range j.components {
		st.out = append(st.out, byte(c.id), byte(c.hSamp<<4|c.vSamp), byte(j.quant[c.quantIdx].index))
	}
	return nil
}

func (st *serializer) dht() error {
	j := st.j
	length := 2
	for i := st.dhtIndex; i < len(j.huffman); i++ {
		length += 16
		for _, c := range j.huffman[i].counts {
			length += int(c)
		}
		if j.huffman[i].isLast {
			break
		}
	}
	st.out = append(st.out, 0xFF, 0xC4, byte(length>>8), byte(length))
	for {
		if st.dhtIndex >= len(j.huffman) {
			return corrupt("DHT marker count")
		}
		h := &j.huffman[st.dhtIndex]
		st.dhtIndex++
		t, ok := buildHuffTable(h)
		if !ok {
			return corrupt("Huffman table")
		}
		if h.slotID&0x10 != 0 {
			st.ac[h.slotID&3] = t
		} else {
			st.dc[h.slotID&3] = t
		}
		maxLen := 0
		total := 0
		for l, c := range h.counts {
			if c != 0 {
				maxLen = l
			}
			total += int(c)
		}
		st.out = append(st.out, byte(h.slotID))
		for l := 1; l <= 16; l++ {
			c := h.counts[l]
			if l == maxLen {
				c--
			}
			st.out = append(st.out, byte(c))
		}
		for _, v := range h.values[:total-1] {
			st.out = append(st.out, byte(v))
		}
		if h.isLast {
			return nil
		}
	}
}

func (st *serializer) dqt() error {
	j := st.j
	length := 2
	for i := st.dqtIndex; i < len(j.quant); i++ {
		length += 1 + 64*(1+int(j.quant[i].precision))
		if j.quant[i].isLast {
			break
		}
	}
	st.out = append(st.out, 0xFF, 0xDB, byte(length>>8), byte(length))
	for {
		if st.dqtIndex >= len(j.quant) {
			return corrupt("DQT marker count")
		}
		q := &j.quant[st.dqtIndex]
		st.dqtIndex++
		st.out = append(st.out, byte(q.precision<<4|q.index))
		for i := 0; i < 64; i++ {
			v := q.values[jpegNaturalOrder[i]]
			if q.precision != 0 {
				st.out = append(st.out, byte(v>>8))
			}
			st.out = append(st.out, byte(v))
		}
		if q.isLast {
			return nil
		}
	}
}

func (j *jpegData) mcuSize(s *jpegScanInfo) (perRow, rows int) {
	interleaved := s.numComponents > 1
	base := j.components[s.components[0].compIdx]
	hg, vg := 1, 1
	if !interleaved {
		hg, vg = base.hSamp, base.vSamp
	}
	maxH, maxV := 1, 1
	for _, c := range j.components {
		maxH, maxV = max(maxH, c.hSamp), max(maxV, c.vSamp)
	}
	return divCeil(j.width*hg, 8*maxH), divCeil(j.height*vg, 8*maxV)
}

func (st *serializer) scan() error {
	j := st.j
	if st.scanIndex >= len(j.scans) {
		return corrupt("scan count")
	}
	s := &j.scans[st.scanIndex]
	st.scanIndex++
	n := int(s.numComponents)
	length := 6 + 2*n
	st.out = append(st.out, 0xFF, 0xDA, byte(length>>8), byte(length), byte(n))
	for k := 0; k < n; k++ {
		c := s.components[k]
		st.out = append(st.out, byte(j.components[c.compIdx].id), byte(c.dcTbl<<4|c.acTbl))
	}
	st.out = append(st.out, byte(s.ss), byte(s.se), byte(s.ah<<4|s.al))

	al, ah, ss, se := 0, 0, 0, 63
	if st.progressive {
		al, ah, ss, se = int(s.al), int(s.ah), int(s.ss), int(s.se)
	}
	mode := 0
	if st.progressive && !(ah == 0 && al == 0 && ss == 0 && se == 63) {
		mode = 1
		if ah != 0 {
			mode = 2
		}
	}
	restart := 0
	if st.seenDRI {
		restart = int(j.restartInterval)
	}
	w := &jpegBitWriter{out: st.out, ok: true}
	var cs codingState
	var lastDC [4]int32
	restartsToGo := restart
	nextRST := 0
	blockScanIndex := 0
	resetPos, zeroPos := 0, 0
	nextReset := -1
	if len(s.resetPoints) > 0 {
		nextReset = int(s.resetPoints[0])
		resetPos = 1
	}
	nextZero := -1
	if len(s.extraZeroRuns) > 0 {
		nextZero = int(s.extraZeroRuns[0].blockIdx)
	}
	perRow, rows := j.mcuSize(s)
	interleaved := n > 1
	for mcuY := 0; mcuY < rows; mcuY++ {
		for mcuX := 0; mcuX < perRow; mcuX++ {
			if restart > 0 && restartsToGo == 0 {
				cs.flush(w)
				if !w.padToByte(st) {
					return corrupt("JPEG padding bits exhausted")
				}
				w.out = append(w.out, 0xFF, byte(0xD0+nextRST))
				nextRST = (nextRST + 1) & 7
				restartsToGo = restart
				lastDC = [4]int32{}
			}
			for k := 0; k < n; k++ {
				sc := s.components[k]
				comp := &j.components[sc.compIdx]
				dcT, acT := &st.dc[sc.dcTbl], &st.ac[sc.acTbl]
				wantDC := ss == 0
				wantAC := ss != 0 || se != 0
				if (wantDC && mode != 2 && !dcT.ok) || (wantAC && !acT.ok) {
					return corrupt("Huffman table missing for scan")
				}
				nby, nbx := 1, 1
				if interleaved {
					nby, nbx = comp.vSamp, comp.hSamp
				}
				for iy := 0; iy < nby; iy++ {
					for ix := 0; ix < nbx; ix++ {
						blockY := mcuY*nby + iy
						blockX := mcuX*nbx + ix
						if blockX >= comp.widthBlocks || blockY >= comp.heightBlocks {
							return corrupt("JPEG block outside the component")
						}
						blockIdx := blockY*comp.widthBlocks + blockX
						if blockScanIndex == nextReset {
							cs.flush(w)
							nextReset = -1
							if resetPos < len(s.resetPoints) {
								nextReset = int(s.resetPoints[resetPos])
								resetPos++
							}
						}
						zeroRuns := 0
						if blockScanIndex == nextZero {
							zeroRuns = int(s.extraZeroRuns[zeroPos].numRuns)
							zeroPos++
							nextZero = -1
							if zeroPos < len(s.extraZeroRuns) {
								nextZero = int(s.extraZeroRuns[zeroPos].blockIdx)
							}
						}
						coeffs := comp.coeffs[64*blockIdx : 64*blockIdx+64]
						var ok bool
						switch mode {
						case 0:
							ok = encodeSequential(coeffs, dcT, acT, zeroRuns, &lastDC[sc.compIdx], w)
						case 1:
							ok = encodeProgressive(coeffs, dcT, acT, ss, se, al, zeroRuns, &cs, &lastDC[sc.compIdx], w)
						default:
							ok = encodeRefinement(coeffs, acT, ss, se, al, &cs, w)
						}
						if !ok || !w.ok {
							return corrupt("JPEG coefficient cannot be encoded")
						}
						blockScanIndex++
					}
				}
			}
			restartsToGo--
		}
	}
	cs.flush(w)
	if !w.padToByte(st) {
		return corrupt("JPEG padding bits exhausted")
	}
	if !w.ok {
		return corrupt("JPEG symbol missing from Huffman table")
	}
	st.out = w.out
	return nil
}

func encodeSequential(coeffs []int16, dcT, acT *huffTable, zeroRuns int, lastDC *int32, w *jpegBitWriter) bool {
	dc := int32(coeffs[0])
	diff := dc - *lastDC
	*lastDC = dc
	mag, val := magnitude(diff)
	nb := uint(bits.Len32(mag))
	w.writeSymbol(int(nb), dcT)
	if nb >= 12 {
		return false
	}
	w.writeBits(nb, uint64(val))
	r := 0
	for i := 1; i < 64; i++ {
		c := int32(coeffs[jpegNaturalOrder[i]])
		if c == 0 {
			r++
			continue
		}
		for r > 15 {
			w.writeSymbol(0xF0, acT)
			r -= 16
		}
		mag, val := magnitude(c)
		nb := uint(bits.Len32(mag))
		if nb > 15 {
			return false
		}
		w.writeSymbol(r<<4|int(nb), acT)
		w.writeBits(nb, uint64(val))
		r = 0
	}
	for i := 0; i < zeroRuns; i++ {
		w.writeSymbol(0xF0, acT)
		r -= 16
	}
	if r > 0 {
		w.writeSymbol(0, acT)
	}
	return true
}

// magnitude returns |v| and the JPEG value bits (one's complement for
// negative values).
func magnitude(v int32) (uint32, uint32) {
	if v < 0 {
		return uint32(-v), uint32(v - 1)
	}
	return uint32(v), uint32(v)
}

func encodeProgressive(coeffs []int16, dcT, acT *huffTable, ss, se, al, zeroRuns int, cs *codingState, lastDC *int32, w *jpegBitWriter) bool {
	eobAllowed := ss > 0
	if ss == 0 {
		dc := int32(coeffs[0]) >> uint(al)
		diff := dc - *lastDC
		*lastDC = dc
		mag, val := magnitude(diff)
		nb := uint(bits.Len32(mag))
		w.writeSymbol(int(nb), dcT)
		w.writeBits(nb, uint64(val))
		ss++
	}
	if ss > se {
		return true
	}
	r := 0
	for k := ss; k <= se; k++ {
		c := int32(coeffs[jpegNaturalOrder[k]])
		if c == 0 {
			r++
			continue
		}
		var t, t2 int32
		if c < 0 {
			t = (-c) >> uint(al)
			t2 = ^t
		} else {
			t = c >> uint(al)
			t2 = t
		}
		if t == 0 {
			r++
			continue
		}
		cs.flush(w)
		for r > 15 {
			w.writeSymbol(0xF0, acT)
			r -= 16
		}
		nb := uint(bits.Len32(uint32(t)))
		w.writeSymbol(r<<4|int(nb), acT)
		w.writeBits(nb, uint64(uint32(t2)))
		r = 0
	}
	if zeroRuns > 0 {
		cs.flush(w)
		for i := 0; i < zeroRuns; i++ {
			w.writeSymbol(0xF0, acT)
			r -= 16
		}
	}
	if r > 0 {
		cs.bufferEOB(acT, nil, w)
		if !eobAllowed {
			cs.flush(w)
		}
	}
	return true
}

func encodeRefinement(coeffs []int16, acT *huffTable, ss, se, al int, cs *codingState, w *jpegBitWriter) bool {
	eobAllowed := ss > 0
	if ss == 0 {
		w.writeBits(1, uint64((int32(coeffs[0])>>uint(al))&1))
		ss++
	}
	if ss > se {
		return true
	}
	var absVals [64]int32
	eob := 0
	for k := ss; k <= se; k++ {
		v := int32(coeffs[jpegNaturalOrder[k]])
		if v < 0 {
			v = -v
		}
		absVals[k] = v >> uint(al)
		if absVals[k] == 1 {
			eob = k
		}
	}
	r := 0
	var refBits []uint8
	for k := ss; k <= se; k++ {
		if absVals[k] == 0 {
			r++
			continue
		}
		for r > 15 && k <= eob {
			cs.flush(w)
			w.writeSymbol(0xF0, acT)
			r -= 16
			for _, b := range refBits {
				w.writeBits(1, uint64(b))
			}
			refBits = refBits[:0]
		}
		if absVals[k] > 1 {
			refBits = append(refBits, uint8(absVals[k]&1))
			continue
		}
		cs.flush(w)
		bit := uint64(1)
		if coeffs[jpegNaturalOrder[k]] < 0 {
			bit = 0
		}
		w.writeSymbol(r<<4|1, acT)
		w.writeBits(1, bit)
		for _, b := range refBits {
			w.writeBits(1, uint64(b))
		}
		refBits = refBits[:0]
		r = 0
	}
	if r > 0 || len(refBits) > 0 {
		cs.bufferEOB(acT, refBits, w)
		if !eobAllowed {
			cs.flush(w)
		}
	}
	return true
}
