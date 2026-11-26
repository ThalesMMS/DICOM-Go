// Package brotli is a pure-Go decoder for the Brotli compressed data format
// (RFC 7932) and a minimal writer that stores data in uncompressed
// meta-blocks. It exists for JPEG XL container boxes (jbrd, brob), which use
// Brotli; it is not a general-purpose compressor.
package brotli

import (
	_ "embed"
	"errors"
	"fmt"
)

// ErrInvalid reports a malformed, truncated or unsupported Brotli stream.
var ErrInvalid = errors.New("brotli: invalid stream")

// ErrTooLarge reports a stream whose decoded size exceeds the caller limit.
var ErrTooLarge = errors.New("brotli: decoded size exceeds limit")

// dictionary is the RFC 7932 Appendix A static dictionary (122784 bytes,
// SHA-256 20e42eb1b511c21806d4d227d07e5dd06877d8ce7b3a817f378f313653f35c70).
//
//go:embed dictionary.bin
var dictionary []byte

type transform struct {
	prefix string
	kind   uint8
	suffix string
}

// ndBits is NDBITS from RFC 7932 Section 8, indexed by word length.
var ndBits = [25]uint{0, 0, 0, 0, 10, 10, 11, 11, 10, 10, 10, 10, 10, 9, 9, 8, 7, 7, 8, 7, 7, 6, 6, 5, 5}

var dOffset = func() (o [25]int) {
	for i := 4; i < 24; i++ {
		o[i+1] = o[i] + i<<ndBits[i]
	}
	return o
}()

func invalid(format string, args ...any) error {
	return fmt.Errorf("%w: %s", ErrInvalid, fmt.Sprintf(format, args...))
}

type bitReader struct {
	data []byte
	pos  int // byte position of the next unread byte
	bits uint64
	n    uint // valid bits in bits
	err  error
}

func (br *bitReader) fill(n uint) {
	for br.n < n {
		if br.pos >= len(br.data) {
			if br.err == nil {
				br.err = invalid("truncated")
			}
			// Feed zeros so callers can finish; err is sticky.
			br.n += 8
			continue
		}
		br.bits |= uint64(br.data[br.pos]) << br.n
		br.pos++
		br.n += 8
	}
}

func (br *bitReader) read(n uint) uint32 {
	if n == 0 {
		return 0
	}
	br.fill(n)
	v := uint32(br.bits & (1<<n - 1))
	br.bits >>= n
	br.n -= n
	return v
}

// alignZero skips to the next byte boundary; the skipped bits must be zero.
func (br *bitReader) alignZero() error {
	k := br.n % 8
	if br.read(k) != 0 {
		return invalid("non-zero padding")
	}
	return nil
}

// prefixCode is a canonical prefix code decoded one bit at a time.
type prefixCode struct {
	count   [16]uint16 // codes per length
	symbols []uint16   // symbols ordered by (length, value)
	single  int        // >=0: the only symbol, consuming no bits
}

func newPrefixCode(lengths []uint8) (prefixCode, error) {
	var pc prefixCode
	pc.single = -1
	used := 0
	last := 0
	for s, l := range lengths {
		if l > 15 {
			return pc, invalid("code length %d", l)
		}
		if l != 0 {
			pc.count[l]++
			used++
			last = s
		}
	}
	if used == 0 {
		return pc, invalid("empty prefix code")
	}
	if used == 1 {
		pc.single = last
		return pc, nil
	}
	left := 1
	for l := 1; l < 16; l++ {
		left <<= 1
		left -= int(pc.count[l])
		if left < 0 {
			return pc, invalid("over-subscribed prefix code")
		}
	}
	if left != 0 {
		return pc, invalid("incomplete prefix code")
	}
	var offs [16]int
	for l := 1; l < 15; l++ {
		offs[l+1] = offs[l] + int(pc.count[l])
	}
	pc.symbols = make([]uint16, used)
	for s, l := range lengths {
		if l != 0 {
			pc.symbols[offs[l]] = uint16(s)
			offs[l]++
		}
	}
	return pc, nil
}

func (pc *prefixCode) decode(br *bitReader) int {
	if pc.single >= 0 {
		return pc.single
	}
	code, first, index := 0, 0, 0
	for l := 1; l < 16; l++ {
		code |= int(br.read(1))
		count := int(pc.count[l])
		if code-count < first {
			return int(pc.symbols[index+(code-first)])
		}
		index += count
		first += count
		first <<= 1
		code <<= 1
	}
	if br.err == nil {
		br.err = invalid("bad prefix code")
	}
	return 0
}

var codeLengthOrder = [18]int{1, 2, 3, 4, 0, 5, 17, 6, 16, 7, 8, 9, 10, 11, 12, 13, 14, 15}

// Static code for code length code lengths, indexed by the next 4 bits.
var clclLength = [16]uint{2, 2, 2, 3, 2, 2, 2, 4, 2, 2, 2, 3, 2, 2, 2, 4}
var clclValue = [16]uint8{0, 4, 3, 2, 0, 4, 3, 1, 0, 4, 3, 2, 0, 4, 3, 5}

func (br *bitReader) peek4() uint32 {
	br.fill(4)
	return uint32(br.bits & 15)
}

func readPrefixCode(br *bitReader, alphabet int) (prefixCode, error) {
	lengths := make([]uint8, alphabet)
	hskip := br.read(2)
	if hskip == 1 {
		maxBits := uint(0)
		for c := alphabet - 1; c > 0; c >>= 1 {
			maxBits++
		}
		nsym := int(br.read(2)) + 1
		var syms [4]int
		for i := 0; i < nsym; i++ {
			syms[i] = int(br.read(maxBits))
			if syms[i] >= alphabet {
				return prefixCode{}, invalid("simple code symbol out of range")
			}
			for j := 0; j < i; j++ {
				if syms[j] == syms[i] {
					return prefixCode{}, invalid("duplicate simple code symbol")
				}
			}
		}
		switch nsym {
		case 1:
			lengths[syms[0]] = 1 // single symbol, no bits consumed
		case 2:
			lengths[syms[0]], lengths[syms[1]] = 1, 1
		case 3:
			lengths[syms[0]], lengths[syms[1]], lengths[syms[2]] = 1, 2, 2
		case 4:
			if br.read(1) == 0 {
				for i := 0; i < 4; i++ {
					lengths[syms[i]] = 2
				}
			} else {
				lengths[syms[0]], lengths[syms[1]], lengths[syms[2]], lengths[syms[3]] = 1, 2, 3, 3
			}
		}
		if br.err != nil {
			return prefixCode{}, br.err
		}
		return newPrefixCode(lengths)
	}
	var clLengths [18]uint8
	space := 32
	numCodes := 0
	for i := int(hskip); i < 18; i++ {
		p := br.peek4()
		br.read(clclLength[p])
		v := clclValue[p]
		clLengths[codeLengthOrder[i]] = v
		if v != 0 {
			space -= 32 >> v
			numCodes++
			if space <= 0 {
				break
			}
		}
	}
	if numCodes != 1 && space != 0 {
		return prefixCode{}, invalid("bad code length code")
	}
	clCode, err := newPrefixCode(clLengths[:])
	if err != nil {
		return prefixCode{}, err
	}
	symbol := 0
	prevLen := uint8(8)
	repeat := 0
	repeatLen := uint8(0)
	space = 32768
	for symbol < alphabet && space > 0 {
		if br.err != nil {
			return prefixCode{}, br.err
		}
		cl := clCode.decode(br)
		if cl < 16 {
			repeat = 0
			lengths[symbol] = uint8(cl)
			symbol++
			if cl != 0 {
				prevLen = uint8(cl)
				space -= 32768 >> cl
			}
			continue
		}
		extra := uint(2)
		newLen := prevLen
		if cl == 17 {
			extra = 3
			newLen = 0
		}
		if repeatLen != newLen {
			repeat = 0
			repeatLen = newLen
		}
		old := repeat
		if repeat > 0 {
			repeat -= 2
			repeat <<= extra
		}
		repeat += int(br.read(extra)) + 3
		delta := repeat - old
		if symbol+delta > alphabet {
			return prefixCode{}, invalid("code length repeat overflows alphabet")
		}
		for i := 0; i < delta; i++ {
			lengths[symbol] = repeatLen
			symbol++
		}
		if repeatLen != 0 {
			space -= delta << (15 - repeatLen)
		}
	}
	if space != 0 {
		return prefixCode{}, invalid("incomplete code lengths")
	}
	if br.err != nil {
		return prefixCode{}, br.err
	}
	return newPrefixCode(lengths)
}

var blockLengthBase = [26]uint32{1, 5, 9, 13, 17, 25, 33, 41, 49, 65, 81, 97, 113, 145, 177, 209, 241, 305, 369, 497, 753, 1265, 2289, 4337, 8433, 16625}
var blockLengthExtra = [26]uint{2, 2, 2, 2, 3, 3, 3, 3, 4, 4, 4, 4, 5, 5, 5, 5, 6, 6, 7, 8, 9, 10, 11, 12, 13, 24}

var insertBase = [24]uint32{0, 1, 2, 3, 4, 5, 6, 8, 10, 14, 18, 26, 34, 50, 66, 98, 130, 194, 322, 578, 1090, 2114, 6210, 22594}
var insertExtra = [24]uint{0, 0, 0, 0, 0, 0, 1, 1, 2, 2, 3, 3, 4, 4, 5, 5, 6, 7, 8, 9, 10, 12, 14, 24}
var copyBase = [24]uint32{2, 3, 4, 5, 6, 7, 8, 9, 10, 12, 14, 18, 22, 30, 38, 54, 70, 102, 134, 198, 326, 582, 1094, 2118}
var copyExtra = [24]uint{0, 0, 0, 0, 0, 0, 0, 0, 1, 1, 2, 2, 3, 3, 4, 4, 5, 5, 6, 7, 8, 9, 10, 24}

// Insert-and-copy cells: base insert code and base copy code per 64 symbols.
var cellInsert = [11]int{0, 0, 0, 0, 8, 8, 0, 16, 8, 16, 16}
var cellCopy = [11]int{0, 8, 0, 8, 0, 8, 16, 0, 16, 8, 16}

func readVarLenUint8(br *bitReader) int {
	if br.read(1) == 0 {
		return 0
	}
	n := uint(br.read(3))
	if n == 0 {
		return 1
	}
	return 1<<n + int(br.read(n))
}

type blockCategory struct {
	numTypes   int
	typeCode   prefixCode
	lenCode    prefixCode
	current    int
	previous   int
	remaining  uint32
	hasSwitchs bool
}

func (c *blockCategory) init(br *bitReader) error {
	c.numTypes = readVarLenUint8(br) + 1
	c.current, c.previous = 0, 1
	c.remaining = 1 << 24
	c.hasSwitchs = c.numTypes >= 2
	if !c.hasSwitchs {
		return br.err
	}
	var err error
	if c.typeCode, err = readPrefixCode(br, c.numTypes+2); err != nil {
		return err
	}
	if c.lenCode, err = readPrefixCode(br, 26); err != nil {
		return err
	}
	c.remaining = readBlockLength(br, &c.lenCode)
	return br.err
}

func readBlockLength(br *bitReader, code *prefixCode) uint32 {
	s := code.decode(br)
	if s >= 26 {
		return 0
	}
	return blockLengthBase[s] + br.read(blockLengthExtra[s])
}

// next consumes one unit of the current block, switching type when exhausted.
func (c *blockCategory) next(br *bitReader) {
	if c.remaining == 0 && c.hasSwitchs {
		code := c.typeCode.decode(br)
		var t int
		switch code {
		case 0:
			t = c.previous
		case 1:
			t = c.current + 1
		default:
			t = code - 2
		}
		if t >= c.numTypes {
			t -= c.numTypes
		}
		c.previous, c.current = c.current, t
		c.remaining = readBlockLength(br, &c.lenCode)
	}
	c.remaining--
}

func readContextMap(br *bitReader, size, numTrees int) ([]uint8, error) {
	m := make([]uint8, size)
	if numTrees < 2 {
		return m, br.err
	}
	rleMax := 0
	if br.read(1) == 1 {
		rleMax = int(br.read(4)) + 1
	}
	code, err := readPrefixCode(br, numTrees+rleMax)
	if err != nil {
		return nil, err
	}
	for i := 0; i < size; {
		if br.err != nil {
			return nil, br.err
		}
		s := code.decode(br)
		switch {
		case s == 0:
			m[i] = 0
			i++
		case s <= rleMax:
			run := 1<<uint(s) + int(br.read(uint(s)))
			if i+run > size {
				return nil, invalid("context map run overflows")
			}
			i += run // zeros already present
		default:
			m[i] = uint8(s - rleMax)
			i++
		}
	}
	if br.read(1) == 1 {
		mtf := make([]uint8, 256)
		for i := range mtf {
			mtf[i] = uint8(i)
		}
		for i, v := range m {
			idx := int(v)
			val := mtf[idx]
			m[i] = val
			copy(mtf[1:idx+1], mtf[:idx])
			mtf[0] = val
		}
	}
	return m, br.err
}

// Decode decompresses a complete Brotli stream. The stream must end exactly
// at the end of src (only zero padding bits may follow the last meta-block
// within the final byte) and must not decode to more than limit bytes.
func Decode(src []byte, limit int) ([]byte, error) {
	br := &bitReader{data: src}
	var wbits uint
	if br.read(1) == 0 {
		wbits = 16
	} else if n := br.read(3); n != 0 {
		wbits = 17 + uint(n)
	} else if n := br.read(3); n == 1 {
		return nil, invalid("large-window streams are unsupported")
	} else if n != 0 {
		wbits = 8 + uint(n)
	} else {
		wbits = 17
	}
	maxBackward := 1<<wbits - 16
	out := make([]byte, 0, min(limit, 1<<16))
	dist := [4]int{16, 15, 11, 4} // dist[3] is the last distance
	for {
		if br.err != nil {
			return nil, br.err
		}
		isLast := br.read(1) == 1
		if isLast && br.read(1) == 1 {
			break
		}
		nibbles := br.read(2)
		if nibbles == 3 {
			if br.read(1) != 0 {
				return nil, invalid("reserved bit set")
			}
			skipBytes := uint(br.read(2))
			skip := 0
			for i := uint(0); i < skipBytes; i++ {
				b := int(br.read(8))
				if i+1 == skipBytes && skipBytes > 1 && b == 0 {
					return nil, invalid("non-minimal metadata length")
				}
				skip |= b << (8 * i)
			}
			if skipBytes > 0 {
				skip++
			}
			if err := br.alignZero(); err != nil {
				return nil, err
			}
			if br.n != 0 || br.pos+skip > len(src) {
				return nil, invalid("truncated metadata block")
			}
			br.pos += skip
			if isLast {
				break
			}
			continue
		}
		nn := uint(nibbles) + 4
		mlen := 0
		for i := uint(0); i < nn; i++ {
			v := int(br.read(4))
			if i+1 == nn && nn > 4 && v == 0 {
				return nil, invalid("non-minimal meta-block length")
			}
			mlen |= v << (4 * i)
		}
		mlen++
		if len(out)+mlen > limit {
			return nil, ErrTooLarge
		}
		if !isLast && br.read(1) == 1 {
			if err := br.alignZero(); err != nil {
				return nil, err
			}
			if br.n != 0 || br.pos+mlen > len(src) {
				return nil, invalid("truncated uncompressed meta-block")
			}
			out = append(out, src[br.pos:br.pos+mlen]...)
			br.pos += mlen
			continue
		}
		var err error
		out, err = decodeCompressed(br, out, mlen, maxBackward, &dist)
		if err != nil {
			return nil, err
		}
		if isLast {
			break
		}
	}
	if err := br.alignZero(); err != nil {
		return nil, err
	}
	if br.err != nil {
		return nil, br.err
	}
	if br.pos != len(src) {
		return nil, invalid("trailing data after stream")
	}
	return out, nil
}

func decodeCompressed(br *bitReader, out []byte, mlen, maxBackward int, dist *[4]int) ([]byte, error) {
	var lit, cmd, dst blockCategory
	for _, c := range []*blockCategory{&lit, &cmd, &dst} {
		if err := c.init(br); err != nil {
			return nil, err
		}
	}
	npostfix := uint(br.read(2))
	ndirect := int(br.read(4)) << npostfix
	modes := make([]uint8, lit.numTypes)
	for i := range modes {
		modes[i] = uint8(br.read(2))
	}
	nTreesL := readVarLenUint8(br) + 1
	cmapL, err := readContextMap(br, 64*lit.numTypes, nTreesL)
	if err != nil {
		return nil, err
	}
	nTreesD := readVarLenUint8(br) + 1
	cmapD, err := readContextMap(br, 4*dst.numTypes, nTreesD)
	if err != nil {
		return nil, err
	}
	litCodes := make([]prefixCode, nTreesL)
	for i := range litCodes {
		if litCodes[i], err = readPrefixCode(br, 256); err != nil {
			return nil, err
		}
	}
	cmdCodes := make([]prefixCode, cmd.numTypes)
	for i := range cmdCodes {
		if cmdCodes[i], err = readPrefixCode(br, 704); err != nil {
			return nil, err
		}
	}
	distAlphabet := 16 + ndirect + 48<<npostfix
	distCodes := make([]prefixCode, nTreesD)
	for i := range distCodes {
		if distCodes[i], err = readPrefixCode(br, distAlphabet); err != nil {
			return nil, err
		}
	}
	end := len(out) + mlen
	postfixMask := 1<<npostfix - 1
	for len(out) < end {
		if br.err != nil {
			return nil, br.err
		}
		cmd.next(br)
		sym := cmdCodes[cmd.current].decode(br)
		cell := sym >> 6
		insCode := cellInsert[cell] + (sym>>3)&7
		cpyCode := cellCopy[cell] + sym&7
		insLen := int(insertBase[insCode] + br.read(insertExtra[insCode]))
		cpyLen := int(copyBase[cpyCode] + br.read(copyExtra[cpyCode]))
		for i := 0; i < insLen; i++ {
			if len(out) >= end {
				return nil, invalid("insert overflows meta-block")
			}
			lit.next(br)
			var p1, p2 byte
			if n := len(out); n > 0 {
				p1 = out[n-1]
				if n > 1 {
					p2 = out[n-2]
				}
			}
			mode := int(modes[lit.current])
			ctx := contextLookup[mode*512+int(p1)] | contextLookup[mode*512+256+int(p2)]
			tree := cmapL[64*lit.current+int(ctx)]
			out = append(out, byte(litCodes[tree].decode(br)))
		}
		if len(out) >= end {
			break
		}
		var distance int
		implicit := sym < 128
		push := !implicit
		if implicit {
			distance = dist[3]
		} else {
			dst.next(br)
			dctx := 3
			if cpyLen <= 4 {
				dctx = cpyLen - 2
			}
			dcode := distCodes[cmapD[4*dst.current+dctx]].decode(br)
			switch {
			case dcode < 16:
				push = dcode != 0
				distance = specialDistance(dcode, dist)
				if distance <= 0 {
					return nil, invalid("invalid distance")
				}
			case dcode < 16+ndirect:
				distance = dcode - 15
			default:
				d := dcode - ndirect - 16
				nbits := uint(1 + d>>(npostfix+1))
				hcode := d >> npostfix
				lcode := d & postfixMask
				offset := (2+hcode&1)<<nbits - 4
				distance = (offset+int(br.read(nbits)))<<npostfix + lcode + ndirect + 1
			}
		}
		maxDist := min(maxBackward, len(out))
		if distance > maxDist {
			// Static dictionary reference.
			if cpyLen < 4 || cpyLen > 24 {
				return nil, invalid("dictionary word length")
			}
			wordID := distance - maxDist - 1
			nb := ndBits[cpyLen]
			idx := wordID & (1<<nb - 1)
			tid := wordID >> nb
			if tid >= len(transforms) {
				return nil, invalid("dictionary transform")
			}
			word := dictionary[dOffset[cpyLen]+idx*cpyLen : dOffset[cpyLen]+(idx+1)*cpyLen]
			out = applyTransform(out, word, transforms[tid])
			if len(out) > end {
				return nil, invalid("dictionary word overflows meta-block")
			}
			continue
		}
		if push {
			dist[0], dist[1], dist[2], dist[3] = dist[1], dist[2], dist[3], distance
		}
		if len(out)+cpyLen > end {
			return nil, invalid("copy overflows meta-block")
		}
		start := len(out) - distance
		for i := 0; i < cpyLen; i++ {
			out = append(out, out[start+i])
		}
	}
	return out, br.err
}

func specialDistance(code int, dist *[4]int) int {
	last, second := dist[3], dist[2]
	switch code {
	case 0:
		return last
	case 1:
		return second
	case 2:
		return dist[1]
	case 3:
		return dist[0]
	}
	deltas := [...]int{-1, 1, -2, 2, -3, 3}
	if code < 10 {
		return last + deltas[code-4]
	}
	return second + deltas[code-10]
}

func applyTransform(out, word []byte, t transform) []byte {
	out = append(out, t.prefix...)
	w := word
	kind := int(t.kind)
	switch {
	case kind >= 1 && kind <= 9:
		if kind >= len(w) {
			w = nil
		} else {
			w = w[:len(w)-kind]
		}
	case kind >= 12 && kind <= 20:
		skip := kind - 11
		if skip >= len(w) {
			w = nil
		} else {
			w = w[skip:]
		}
	}
	start := len(out)
	out = append(out, w...)
	switch kind {
	case 10:
		uppercase(out[start:], true)
	case 11:
		uppercase(out[start:], false)
	}
	return append(out, t.suffix...)
}

// uppercase applies the RFC 7932 ferment step to the first character or to
// every character of b.
func uppercase(b []byte, firstOnly bool) {
	for i := 0; i < len(b); {
		step := 1
		switch {
		case b[i] < 0xC0:
			if b[i] >= 'a' && b[i] <= 'z' {
				b[i] ^= 32
			}
		case b[i] < 0xE0:
			if i+1 < len(b) {
				b[i+1] ^= 32
			}
			step = 2
		default:
			if i+2 < len(b) {
				b[i+2] ^= 5
			}
			step = 3
		}
		if firstOnly {
			return
		}
		i += step
	}
}
