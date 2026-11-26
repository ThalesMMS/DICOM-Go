package jpegxl

// Entropy decoding (ISO/IEC 18181-1 Annex C): clustered ANS or prefix-code
// histograms, hybrid integer configurations and optional LZ77.

const (
	ansLogTabSize = 12
	ansTabSize    = 1 << ansLogTabSize
	ansSignature  = 0x13 << 16
	maxClusters   = 256
	lz77Window    = 1 << 20
)

type hybridConfig struct {
	splitExp uint32
	msb      uint32
	lsb      uint32
}

func readHybridConfig(br *bitReader, logAlpha uint32) hybridConfig {
	var c hybridConfig
	c.splitExp = br.u(ceilLog2(uint64(logAlpha) + 1))
	if c.splitExp != logAlpha {
		c.msb = br.u(ceilLog2(uint64(c.splitExp) + 1))
		if c.msb > c.splitExp {
			br.fail(corrupt("hybrid config msb"))
			return c
		}
		c.lsb = br.u(ceilLog2(uint64(c.splitExp-c.msb) + 1))
	}
	if c.splitExp > 31 || c.msb+c.lsb > c.splitExp {
		br.fail(corrupt("hybrid config"))
	}
	return c
}

func (c hybridConfig) value(br *bitReader, token uint32) uint32 {
	split := uint32(1) << c.splitExp
	if token < split {
		return token
	}
	n := c.splitExp - (c.msb + c.lsb) + ((token - split) >> (c.msb + c.lsb))
	if n > 32 {
		br.fail(corrupt("hybrid integer too large"))
		return 0
	}
	low := token & (1<<c.lsb - 1)
	token >>= c.lsb
	high := (1<<c.msb | token&(1<<c.msb-1))
	return uint32(((uint64(high)<<n|br.readBits(uint(n)))<<c.lsb | uint64(low)) & 0xFFFFFFFF)
}

type aliasEntry struct {
	cutoff   uint32
	right    uint32
	offsets1 uint32
	freq0    uint32
	freq1    uint32
}

type prefixDecoder struct {
	single  int // >= 0 when the alphabet has one symbol
	fast    [256]uint16
	fastLen [256]uint8
	count   [16]uint16
	symbols []uint16
}

type entropyCode struct {
	lz77Enabled   bool
	lz77MinSymbol uint32
	lz77MinLength uint32
	lz77Config    hybridConfig
	contextMap    []uint8
	usePrefix     bool
	logAlphaSize  uint32
	configs       []hybridConfig
	prefix        []prefixDecoder
	alias         [][]aliasEntry
}

func readEntropyCode(br *bitReader, numContexts int) (*entropyCode, error) {
	return readEntropyCodeLZ(br, numContexts, false)
}

func readEntropyCodeLZ(br *bitReader, numContexts int, disallowLZ77 bool) (*entropyCode, error) {
	code := &entropyCode{}
	code.lz77Enabled = br.bool()
	if code.lz77Enabled {
		if disallowLZ77 {
			return nil, corrupt("LZ77 in a two-entry context map")
		}
		code.lz77MinSymbol = br.u32(val(224), val(512), val(4096), bitsOff(15, 8))
		code.lz77MinLength = br.u32(val(3), val(4), bitsOff(2, 5), bitsOff(8, 9))
		code.lz77Config = readHybridConfig(br, 8)
		numContexts++
	}
	numClusters := 1
	code.contextMap = make([]uint8, numContexts)
	if numContexts > 1 {
		var err error
		if numClusters, err = readContextMap(br, code.contextMap); err != nil {
			return nil, err
		}
	}
	code.usePrefix = br.bool()
	if code.usePrefix {
		code.logAlphaSize = 15
	} else {
		code.logAlphaSize = 5 + br.u(2)
	}
	code.configs = make([]hybridConfig, numClusters)
	for i := range code.configs {
		code.configs[i] = readHybridConfig(br, code.logAlphaSize)
	}
	if br.err != nil {
		return nil, br.err
	}
	if code.usePrefix {
		sizes := make([]int, numClusters)
		for i := range sizes {
			sizes[i] = 1
			if br.bool() {
				n := uint(br.u(4))
				if n > 14 {
					return nil, corrupt("prefix alphabet size")
				}
				sizes[i] = 1 + 1<<n + int(br.u(n))
				if sizes[i] > 1<<15 {
					return nil, corrupt("prefix alphabet size")
				}
			}
		}
		code.prefix = make([]prefixDecoder, numClusters)
		for i := range code.prefix {
			if sizes[i] == 1 {
				code.prefix[i].single = 0
				continue
			}
			if err := readPrefixCode(br, sizes[i], &code.prefix[i]); err != nil {
				return nil, err
			}
		}
	} else {
		code.alias = make([][]aliasEntry, numClusters)
		for i := range code.alias {
			dist, err := readANSDistribution(br, code.logAlphaSize)
			if err != nil {
				return nil, err
			}
			if code.alias[i], err = buildAliasTable(dist, code.logAlphaSize); err != nil {
				return nil, err
			}
		}
	}
	return code, br.err
}

func readContextMap(br *bitReader, m []uint8) (int, error) {
	if br.bool() { // simple
		bitsPer := uint(br.u(2))
		for i := range m {
			m[i] = uint8(br.u(bitsPer))
		}
	} else {
		useMTF := br.bool()
		inner, err := readEntropyCodeLZ(br, 1, len(m) <= 2)
		if err != nil {
			return 0, err
		}
		sr := newSymbolReader(inner, br, 0)
		for i := range m {
			v := sr.readUint(br, 0)
			if v >= maxClusters {
				return 0, corrupt("context map cluster out of range")
			}
			m[i] = uint8(v)
		}
		if !sr.finalStateOK() {
			return 0, corrupt("context map ANS final state")
		}
		if useMTF {
			var mtf [256]uint8
			for i := range mtf {
				mtf[i] = uint8(i)
			}
			for i, v := range m {
				val := mtf[v]
				m[i] = val
				copy(mtf[1:int(v)+1], mtf[:v])
				mtf[0] = val
			}
		}
	}
	if br.err != nil {
		return 0, br.err
	}
	max := 0
	for _, v := range m {
		max = maxInt(max, int(v))
	}
	seen := make([]bool, max+1)
	n := 0
	for _, v := range m {
		if !seen[v] {
			seen[v] = true
			n++
		}
	}
	if n != max+1 {
		return 0, corrupt("incomplete context map")
	}
	return max + 1, nil
}

func maxInt(a, b int) int {
	if a > b {
		return a
	}
	return b
}

// readVarLenUint8 is the 0..255 code used by ANS histograms.
func readVarLenUint8(br *bitReader) int {
	if !br.bool() {
		return 0
	}
	n := uint(br.u(3))
	return 1<<n + int(br.u(n))
}

// logCountCode decodes the static prefix code of ANS log-counts, indexed by
// the next 7 bits: low byte is the symbol, high byte the code length.
var logCountCode = func() (t [128][2]uint8) {
	codes := [14]struct{ bits, length uint8 }{
		{17, 5}, {11, 4}, {15, 4}, {3, 4}, {9, 4}, {7, 4}, {4, 3}, {2, 3},
		{5, 3}, {6, 3}, {0, 3}, {33, 6}, {1, 7}, {65, 7},
	}
	for idx := 0; idx < 128; idx++ {
		for sym, c := range codes {
			if uint8(idx)&(1<<c.length-1) == c.bits {
				t[idx] = [2]uint8{c.length, uint8(sym)}
			}
		}
	}
	return t
}()

func popcountPrecision(logcount, shift uint32) uint32 {
	r := int32(logcount)
	if alt := int32(shift) - int32((ansLogTabSize-logcount)>>1); alt < r {
		r = alt
	}
	if r < 0 {
		return 0
	}
	return uint32(r)
}

func readANSDistribution(br *bitReader, logAlpha uint32) ([]int32, error) {
	if br.bool() { // simple
		n := int(br.u(1)) + 1
		var syms [2]int
		maxSym := 0
		for i := 0; i < n; i++ {
			syms[i] = readVarLenUint8(br)
			maxSym = maxInt(maxSym, syms[i])
		}
		counts := make([]int32, maxSym+1)
		if n == 1 {
			counts[syms[0]] = ansTabSize
		} else {
			if syms[0] == syms[1] {
				return nil, corrupt("duplicate ANS symbol")
			}
			counts[syms[0]] = int32(br.u(ansLogTabSize))
			counts[syms[1]] = ansTabSize - counts[syms[0]]
		}
		return counts, checkAlphabet(counts, logAlpha)
	}
	if br.bool() { // flat
		size := readVarLenUint8(br) + 1
		counts := make([]int32, size)
		for i := range counts {
			counts[i] = int32(ansTabSize / size)
			if i < ansTabSize%size {
				counts[i]++
			}
		}
		return counts, checkAlphabet(counts, logAlpha)
	}
	log := uint(0)
	for ; log < 3; log++ {
		if br.u(1) == 0 {
			break
		}
	}
	shift := (br.u(log) | 1<<log) - 1
	if shift > ansLogTabSize+1 {
		return nil, corrupt("ANS histogram shift")
	}
	length := readVarLenUint8(br) + 3
	counts := make([]int32, length)
	logcounts := make([]uint32, length)
	same := make([]int, length)
	omitLog, omitPos := -1, -1
	for i := 0; i < length; i++ {
		idx := br.peekBits(7)
		e := logCountCode[idx]
		br.skipBits(uint(e[0]))
		logcounts[i] = uint32(e[1])
		if logcounts[i] == ansLogTabSize+1 {
			rle := readVarLenUint8(br)
			same[i] = rle + 5
			i += rle + 3
			continue
		}
		if int(logcounts[i]) > omitLog {
			omitLog = int(logcounts[i])
			omitPos = i
		}
	}
	if br.err != nil {
		return nil, br.err
	}
	if omitPos < 0 || (omitPos+1 < length && logcounts[omitPos+1] == ansLogTabSize+1) {
		return nil, corrupt("ANS histogram")
	}
	total := int32(0)
	numSame := 0
	prev := int32(0)
	for i := 0; i < length; i++ {
		if same[i] != 0 {
			numSame = same[i] - 1
			if i > 0 {
				prev = counts[i-1]
			} else {
				prev = 0
			}
		}
		if numSame > 0 {
			counts[i] = prev
			numSame--
		} else {
			code := logcounts[i]
			switch {
			case i == omitPos || code == 0:
				continue
			case code == 1:
				counts[i] = 1
			default:
				bc := popcountPrecision(code-1, shift)
				counts[i] = 1<<(code-1) + int32(br.u(uint(bc)))<<(code-1-bc)
			}
		}
		total += counts[i]
	}
	counts[omitPos] = ansTabSize - total
	if counts[omitPos] <= 0 {
		return nil, corrupt("ANS histogram total")
	}
	return counts, checkAlphabet(counts, logAlpha)
}

func checkAlphabet(counts []int32, logAlpha uint32) error {
	n := len(counts)
	for n > 0 && counts[n-1] == 0 {
		n--
	}
	if n > 1<<logAlpha {
		return corrupt("ANS alphabet larger than the table")
	}
	return nil
}

func buildAliasTable(dist []int32, logAlpha uint32) ([]aliasEntry, error) {
	for len(dist) > 0 && dist[len(dist)-1] == 0 {
		dist = dist[:len(dist)-1]
	}
	if len(dist) == 0 {
		dist = []int32{ansTabSize}
	}
	tableSize := 1 << logAlpha
	entrySize := uint32(ansTabSize >> logAlpha)
	table := make([]aliasEntry, tableSize)
	sum := int32(0)
	single := -1
	for s, v := range dist {
		sum += v
		if v == ansTabSize {
			single = s
		}
		if v < 0 {
			return nil, corrupt("negative ANS count")
		}
	}
	if sum != ansTabSize {
		return nil, corrupt("ANS histogram does not sum to 4096")
	}
	if single >= 0 {
		for i := range table {
			table[i] = aliasEntry{cutoff: 0, right: uint32(single), offsets1: entrySize * uint32(i), freq0: 0, freq1: ansTabSize}
		}
		return table, nil
	}
	cutoffs := make([]uint32, tableSize)
	var under, over []int
	for i := range dist {
		cutoffs[i] = uint32(dist[i])
		if cutoffs[i] > entrySize {
			over = append(over, i)
		} else if cutoffs[i] < entrySize {
			under = append(under, i)
		}
	}
	for i := len(dist); i < tableSize; i++ {
		under = append(under, i)
	}
	for len(over) > 0 {
		o := over[len(over)-1]
		over = over[:len(over)-1]
		if len(under) == 0 {
			return nil, corrupt("ANS alias construction")
		}
		u := under[len(under)-1]
		under = under[:len(under)-1]
		by := entrySize - cutoffs[u]
		cutoffs[o] -= by
		table[u].right = uint32(o)
		table[u].offsets1 = cutoffs[o]
		if cutoffs[o] < entrySize {
			under = append(under, o)
		} else if cutoffs[o] > entrySize {
			over = append(over, o)
		}
	}
	for i := range table {
		if cutoffs[i] == entrySize {
			table[i].right = uint32(i)
			table[i].offsets1 = 0
			table[i].cutoff = 0
		} else {
			table[i].offsets1 -= cutoffs[i]
			table[i].cutoff = cutoffs[i]
		}
		if i < len(dist) {
			table[i].freq0 = uint32(dist[i])
		}
		if r := int(table[i].right); r < len(dist) {
			table[i].freq1 = uint32(dist[r])
		}
	}
	return table, nil
}

var codeLengthOrder = [18]int{1, 2, 3, 4, 0, 5, 17, 6, 16, 7, 8, 9, 10, 11, 12, 13, 14, 15}
var clclLength = [16]uint8{2, 2, 2, 3, 2, 2, 2, 4, 2, 2, 2, 3, 2, 2, 2, 4}
var clclValue = [16]uint8{0, 4, 3, 2, 0, 4, 3, 1, 0, 4, 3, 2, 0, 4, 3, 5}

// readPrefixCode reads a Brotli-style prefix code (RFC 7932 Section 3.4/3.5).
func readPrefixCode(br *bitReader, alphabet int, pd *prefixDecoder) error {
	lengths := make([]uint8, alphabet)
	hskip := br.u(2)
	if hskip == 1 {
		maxBits := uint(0)
		for c := alphabet - 1; c > 0; c >>= 1 {
			maxBits++
		}
		nsym := int(br.u(2)) + 1
		var syms [4]int
		for i := 0; i < nsym; i++ {
			syms[i] = int(br.u(maxBits)) % alphabet
			for j := 0; j < i; j++ {
				if syms[j] == syms[i] {
					return corrupt("duplicate simple prefix symbol")
				}
			}
		}
		switch nsym {
		case 1:
			*pd = prefixDecoder{single: syms[0]}
			return br.err
		case 2:
			lengths[syms[0]], lengths[syms[1]] = 1, 1
		case 3:
			lengths[syms[0]], lengths[syms[1]], lengths[syms[2]] = 1, 2, 2
		case 4:
			if br.u(1) == 0 {
				for i := 0; i < 4; i++ {
					lengths[syms[i]] = 2
				}
			} else {
				lengths[syms[0]], lengths[syms[1]], lengths[syms[2]], lengths[syms[3]] = 1, 2, 3, 3
			}
		}
		return pd.build(lengths)
	}
	var cl [18]uint8
	space := 32
	num := 0
	for i := int(hskip); i < 18; i++ {
		p := br.peekBits(4)
		br.skipBits(uint(clclLength[p]))
		v := clclValue[p]
		cl[codeLengthOrder[i]] = v
		if v != 0 {
			space -= 32 >> v
			num++
			if space <= 0 {
				break
			}
		}
	}
	if num != 1 && space != 0 {
		return corrupt("prefix code length code")
	}
	var clCode prefixDecoder
	if err := clCode.build(cl[:]); err != nil {
		return err
	}
	symbol := 0
	prevLen := uint8(8)
	repeat := 0
	repeatLen := uint8(0)
	space = 32768
	for symbol < alphabet && space > 0 {
		if br.err != nil {
			return br.err
		}
		c := clCode.decode(br)
		if c < 16 {
			repeat = 0
			lengths[symbol] = uint8(c)
			symbol++
			if c != 0 {
				prevLen = uint8(c)
				space -= 32768 >> c
			}
			continue
		}
		extra := uint(2)
		newLen := prevLen
		if c == 17 {
			extra = 3
			newLen = 0
		}
		if repeatLen != newLen {
			repeat = 0
			repeatLen = newLen
		}
		old := repeat
		if repeat > 0 {
			repeat = (repeat - 2) << extra
		}
		repeat += int(br.u(extra)) + 3
		delta := repeat - old
		if symbol+delta > alphabet {
			return corrupt("prefix code repeat overflow")
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
		return corrupt("incomplete prefix code")
	}
	return pd.build(lengths)
}

func (pd *prefixDecoder) build(lengths []uint8) error {
	*pd = prefixDecoder{single: -1}
	used, last := 0, 0
	for s, l := range lengths {
		if l > 15 {
			return corrupt("prefix code length")
		}
		if l != 0 {
			pd.count[l]++
			used++
			last = s
		}
	}
	if used == 0 {
		return corrupt("empty prefix code")
	}
	if used == 1 {
		pd.single = last
		return nil
	}
	left := 1
	for l := 1; l < 16; l++ {
		left = left<<1 - int(pd.count[l])
		if left < 0 {
			return corrupt("over-subscribed prefix code")
		}
	}
	if left != 0 {
		return corrupt("incomplete prefix code")
	}
	var offs [17]int
	for l := 1; l < 16; l++ {
		offs[l+1] = offs[l] + int(pd.count[l])
	}
	pd.symbols = make([]uint16, used)
	next := offs
	for s, l := range lengths {
		if l != 0 {
			pd.symbols[next[l]] = uint16(s)
			next[l]++
		}
	}
	// Fast table for codes of at most 8 bits, indexed by the next 8 stream
	// bits (the code's first bit is the lowest stream bit).
	code := 0
	idx := 0
	for l := 1; l <= 8; l++ {
		for k := 0; k < int(pd.count[l]); k++ {
			rev := 0
			for b := 0; b < l; b++ {
				if code>>(l-1-b)&1 == 1 {
					rev |= 1 << b
				}
			}
			for fill := rev; fill < 256; fill += 1 << l {
				pd.fast[fill] = pd.symbols[idx]
				pd.fastLen[fill] = uint8(l)
			}
			code++
			idx++
		}
		code <<= 1
	}
	return nil
}

func (pd *prefixDecoder) decode(br *bitReader) int {
	if pd.single >= 0 {
		return pd.single
	}
	p := br.peekBits(8)
	if l := pd.fastLen[p]; l != 0 {
		br.skipBits(uint(l))
		return int(pd.fast[p])
	}
	code, first, index := 0, 0, 0
	for l := 1; l < 16; l++ {
		code |= int(br.readBits(1))
		count := int(pd.count[l])
		if code-count < first {
			return int(pd.symbols[index+(code-first)])
		}
		index += count
		first = (first + count) << 1
		code <<= 1
	}
	br.fail(corrupt("invalid prefix code"))
	return 0
}

// specialDistances are the LZ77 (dx, dy) distance pairs of Annex C.
var specialDistances = [120][2]int8{
	{0, 1}, {1, 0}, {1, 1}, {-1, 1}, {0, 2}, {2, 0}, {1, 2}, {-1, 2},
	{2, 1}, {-2, 1}, {2, 2}, {-2, 2}, {0, 3}, {3, 0}, {1, 3}, {-1, 3},
	{3, 1}, {-3, 1}, {2, 3}, {-2, 3}, {3, 2}, {-3, 2}, {0, 4}, {4, 0},
	{1, 4}, {-1, 4}, {4, 1}, {-4, 1}, {3, 3}, {-3, 3}, {2, 4}, {-2, 4},
	{4, 2}, {-4, 2}, {0, 5}, {3, 4}, {-3, 4}, {4, 3}, {-4, 3}, {5, 0},
	{1, 5}, {-1, 5}, {5, 1}, {-5, 1}, {2, 5}, {-2, 5}, {5, 2}, {-5, 2},
	{4, 4}, {-4, 4}, {3, 5}, {-3, 5}, {5, 3}, {-5, 3}, {0, 6}, {6, 0},
	{1, 6}, {-1, 6}, {6, 1}, {-6, 1}, {2, 6}, {-2, 6}, {6, 2}, {-6, 2},
	{4, 5}, {-4, 5}, {5, 4}, {-5, 4}, {3, 6}, {-3, 6}, {6, 3}, {-6, 3},
	{0, 7}, {7, 0}, {1, 7}, {-1, 7}, {5, 5}, {-5, 5}, {7, 1}, {-7, 1},
	{4, 6}, {-4, 6}, {6, 4}, {-6, 4}, {2, 7}, {-2, 7}, {7, 2}, {-7, 2},
	{3, 7}, {-3, 7}, {7, 3}, {-7, 3}, {5, 6}, {-5, 6}, {6, 5}, {-6, 5},
	{8, 0}, {4, 7}, {-4, 7}, {7, 4}, {-7, 4}, {8, 1}, {8, 2}, {6, 6},
	{-6, 6}, {8, 3}, {5, 7}, {-5, 7}, {7, 5}, {-7, 5}, {8, 4}, {6, 7},
	{-6, 7}, {7, 6}, {-7, 6}, {8, 5}, {7, 7}, {-7, 7}, {8, 6}, {8, 7},
}

// symbolReader decodes hybrid integers from one entropy-coded stream.
type symbolReader struct {
	code       *entropyCode
	state      uint32
	window     []uint32
	numToCopy  uint32
	copyPos    uint32
	numDecoded uint32
	special    []uint32
	lzCluster  int
}

func newSymbolReader(code *entropyCode, br *bitReader, distMultiplier int) *symbolReader {
	sr := &symbolReader{code: code}
	if !code.usePrefix {
		sr.state = uint32(br.readBits(32))
	}
	if code.lz77Enabled {
		sr.window = make([]uint32, lz77Window)
		sr.lzCluster = int(code.contextMap[len(code.contextMap)-1])
		if distMultiplier != 0 {
			sr.special = make([]uint32, len(specialDistances))
			for i, d := range specialDistances {
				v := int(d[0]) + distMultiplier*int(d[1])
				if v < 1 {
					v = 1
				}
				sr.special[i] = uint32(v)
			}
		}
	}
	return sr
}

func (sr *symbolReader) readSymbol(br *bitReader, cluster int) uint32 {
	if sr.code.usePrefix {
		return uint32(sr.code.prefix[cluster].decode(br))
	}
	table := sr.code.alias[cluster]
	res := sr.state & (ansTabSize - 1)
	logEntry := ansLogTabSize - sr.code.logAlphaSize
	i := res >> logEntry
	pos := res & (1<<logEntry - 1)
	e := &table[i]
	var sym, offset, freq uint32
	if pos >= e.cutoff {
		sym, offset, freq = e.right, e.offsets1+pos, e.freq1
	} else {
		sym, offset, freq = i, pos, e.freq0
	}
	sr.state = freq*(sr.state>>ansLogTabSize) + offset
	if sr.state < 1<<16 {
		sr.state = sr.state<<16 | uint32(br.readBits(16))
	}
	return sym
}

// readUint decodes one value in context ctx.
func (sr *symbolReader) readUint(br *bitReader, ctx int) uint32 {
	if sr.numToCopy > 0 {
		v := sr.window[sr.copyPos&(lz77Window-1)]
		sr.copyPos++
		sr.numToCopy--
		sr.window[sr.numDecoded&(lz77Window-1)] = v
		sr.numDecoded++
		return v
	}
	if ctx < 0 || ctx >= len(sr.code.contextMap) {
		if br.err == nil {
			br.err = corrupt("entropy context %d out of range", ctx)
		}
		return 0
	}
	cluster := int(sr.code.contextMap[ctx])
	token := sr.readSymbol(br, cluster)
	if sr.code.lz77Enabled && token >= sr.code.lz77MinSymbol {
		sr.numToCopy = sr.code.lz77Config.value(br, token-sr.code.lz77MinSymbol) + sr.code.lz77MinLength
		dtoken := sr.readSymbol(br, sr.lzCluster)
		distance := sr.code.configs[sr.lzCluster].value(br, dtoken)
		if int(distance) < len(sr.special) {
			distance = sr.special[distance]
		} else {
			distance = distance + 1 - uint32(len(sr.special))
		}
		if distance > sr.numDecoded {
			distance = sr.numDecoded
		}
		if distance > lz77Window {
			distance = lz77Window
		}
		sr.copyPos = sr.numDecoded - distance
		if distance == 0 {
			n := min(sr.numToCopy, lz77Window)
			clear(sr.window[:n])
		}
		if sr.numToCopy == 0 {
			return 0
		}
		v := sr.window[sr.copyPos&(lz77Window-1)]
		sr.copyPos++
		sr.numToCopy--
		sr.window[sr.numDecoded&(lz77Window-1)] = v
		sr.numDecoded++
		return v
	}
	v := sr.code.configs[cluster].value(br, token)
	if sr.window != nil {
		sr.window[sr.numDecoded&(lz77Window-1)] = v
		sr.numDecoded++
	}
	return v
}

func (sr *symbolReader) finalStateOK() bool {
	return sr.code.usePrefix || sr.state == ansSignature
}

// readPermutation decodes a Lehmer-coded permutation of size elements whose
// first skip entries are fixed (Annex C, coefficient orders and TOC).
func readPermutation(br *bitReader, sr *symbolReader, size, skip int) ([]int, error) {
	end := int(sr.readUint(br, coeffOrderContext(uint32(size)))) + skip
	if end > size {
		return nil, corrupt("permutation end")
	}
	lehmer := make([]int, size)
	last := uint32(0)
	for i := skip; i < end; i++ {
		v := sr.readUint(br, coeffOrderContext(last))
		last = v
		if int(v) >= size-i {
			return nil, corrupt("lehmer code")
		}
		lehmer[i] = int(v)
	}
	if br.err != nil {
		return nil, br.err
	}
	return decodeLehmer(lehmer), nil
}

// decodeLehmer maps a Lehmer code to its permutation in O(n log n) with a
// Fenwick tree over the remaining elements.
func decodeLehmer(lehmer []int) []int {
	n := len(lehmer)
	tree := make([]int, n+1)
	for i := 1; i <= n; i++ {
		tree[i]++
		if j := i + i&-i; j <= n {
			tree[j] += tree[i]
		}
	}
	top := 1
	for top*2 <= n {
		top *= 2
	}
	perm := make([]int, n)
	for i, k := range lehmer {
		// Find the (k+1)-th remaining element.
		pos, rem := 0, k+1
		for step := top; step > 0; step >>= 1 {
			if next := pos + step; next <= n && tree[next] < rem {
				pos = next
				rem -= tree[next]
			}
		}
		perm[i] = pos
		for j := pos + 1; j <= n; j += j & -j {
			tree[j]--
		}
	}
	return perm
}

func coeffOrderContext(x uint32) int {
	return int(min(ceilLog2(uint64(x)+1), 7))
}
