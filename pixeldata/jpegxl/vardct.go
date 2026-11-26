package jpegxl

// VarDCT coefficient decoding for DCT8 frames (Annex G/I). JPEG reconstruction
// uses the quantized coefficients directly. The XYB profile dequantizes them
// before applying the inverse transform.

const (
	numOrders          = 13
	nonZeroBuckets     = 37
	zeroDensityContext = 458
	numQuantTables     = 17
	colourTileBlocks   = 8
	acStrategyDCT8     = 0
)

var defaultBlockCtxMap = []uint8{
	0, 1, 2, 2, 3, 3, 4, 5, 6, 6, 6, 6, 6,
	7, 8, 9, 9, 10, 11, 12, 13, 14, 14, 14, 14, 14,
	7, 8, 9, 9, 10, 11, 12, 13, 14, 14, 14, 14, 14,
}

// Coefficient-order index of every AC strategy and its size in 8x8 blocks.
var strategyOrder = [27]int{0, 1, 1, 1, 2, 3, 4, 4, 5, 5, 6, 6, 1, 1, 1, 1, 1, 1, 7, 8, 8, 9, 10, 10, 11, 12, 12}
var orderLLF = [numOrders]int{1, 1, 4, 16, 2, 4, 8, 64, 32, 256, 128, 1024, 512}

var coeffFreqContext = [64]int{
	0, 0, 1, 2, 3, 4, 5, 6, 7, 8, 9, 10, 11, 12, 13, 14,
	15, 15, 16, 16, 17, 17, 18, 18, 19, 19, 20, 20, 21, 21, 22, 22,
	23, 23, 23, 23, 24, 24, 24, 24, 25, 25, 25, 25, 26, 26, 26, 26,
	27, 27, 27, 27, 28, 28, 28, 28, 29, 29, 29, 29, 30, 30, 30, 30,
}

var coeffNumNonzeroContext = [64]int{
	0, 0, 31, 62, 62, 93, 93, 93, 93, 123, 123, 123, 123,
	152, 152, 152, 152, 152, 152, 152, 152, 180, 180, 180, 180, 180,
	180, 180, 180, 180, 180, 180, 180, 206, 206, 206, 206, 206, 206,
	206, 206, 206, 206, 206, 206, 206, 206, 206, 206, 206, 206, 206,
	206, 206, 206, 206, 206, 206, 206, 206, 206, 206, 206, 206,
}

// Quant table sizes in 8x8 blocks, per quant table index.
var quantTableSizeX = [numQuantTables]int{1, 1, 1, 1, 2, 4, 1, 1, 2, 1, 1, 8, 4, 16, 8, 32, 16}
var quantTableSizeY = [numQuantTables]int{1, 1, 1, 1, 2, 4, 2, 4, 4, 1, 1, 8, 8, 16, 16, 32, 32}

type blockCtxMap struct {
	dcThresholds [3][]int32
	qfThresholds []uint32
	ctxMap       []uint8
	numDCCtxs    int
	numCtxs      int
}

func (b *blockCtxMap) context(dcIdx int, qf int32, ord, c int) int {
	qfIdx := 0
	for _, t := range b.qfThresholds {
		if uint32(qf) > t {
			qfIdx++
		}
	}
	idx := 2
	if c < 2 {
		idx = c ^ 1
	}
	idx = idx*numOrders + ord
	idx = idx*(len(b.qfThresholds)+1) + qfIdx
	idx = idx*b.numDCCtxs + dcIdx
	return int(b.ctxMap[idx])
}

func (b *blockCtxMap) nonZeroContext(nonZeros, blockCtx int) int {
	var ctx int
	switch {
	case nonZeros >= 64:
		ctx = 36
	case nonZeros >= 8:
		ctx = 4 + nonZeros/2
	default:
		ctx = nonZeros
	}
	return ctx*b.numCtxs + blockCtx
}

func (b *blockCtxMap) zeroDensityOffset(blockCtx int) int {
	return b.numCtxs*nonZeroBuckets + zeroDensityContext*blockCtx
}

func (b *blockCtxMap) numACContexts() int {
	return b.numCtxs * (nonZeroBuckets + zeroDensityContext)
}

func readBlockCtxMap(br *bitReader) (*blockCtxMap, error) {
	b := &blockCtxMap{numDCCtxs: 1}
	if br.bool() {
		b.ctxMap = defaultBlockCtxMap
		b.numCtxs = 15
		return b, nil
	}
	for j := 0; j < 3; j++ {
		n := int(br.u(4))
		b.numDCCtxs *= n + 1
		for i := 0; i < n; i++ {
			v := br.u32(bitsN(4), bitsOff(8, 16), bitsOff(16, 272), bitsOff(32, 65808))
			b.dcThresholds[j] = append(b.dcThresholds[j], unpackSigned(v))
		}
	}
	nq := int(br.u(4))
	for i := 0; i < nq; i++ {
		b.qfThresholds = append(b.qfThresholds, br.u32(bitsN(2), bitsOff(3, 4), bitsOff(5, 12), bitsOff(8, 44))+1)
	}
	if b.numDCCtxs*(nq+1) > 64 {
		return nil, corrupt("block context map too large")
	}
	b.ctxMap = make([]uint8, 3*numOrders*b.numDCCtxs*(nq+1))
	n, err := readContextMap(br, b.ctxMap)
	if err != nil {
		return nil, err
	}
	if n > 16 {
		return nil, corrupt("too many block contexts")
	}
	b.numCtxs = n
	return b, nil
}

// vardctState holds the frame-global VarDCT data needed for coefficients.
type vardctState struct {
	f            *frameHeader
	bctx         *blockCtxMap
	global       modularContext
	ytoxDC       int32
	ytobDC       int32
	colourFactor uint32
	baseCorrX    float32
	baseCorrB    float32
	// per-block data in the luma block grid
	acs     []uint8
	qf      []int32
	qdc     []uint8
	ytoxMap []int32 // per colour tile
	ytobMap []int32
	tilesX  int
	// quantized LF per channel (X, Y, B) in each channel's block grid
	dc       [3][]int32
	dcW, dcH [3]int
	// RAW quantization table (3 x 64, JXL layout) when present
	rawQTable    []int32
	rawQTableDen float32
	rawQ         bool
	// coefficient orders: per order per channel
	orders      [numOrders][3][]int
	numHisto    int
	hfCodes     []*entropyCode
	coeffs      [3][]int32 // 64 per block in each channel's block grid
	lfPrecision []uint8    // per LF group, nil for JPEG-compatible frames
	// Frame parameters kept for diagnostics and encoder tests.
	lfDequantDefault bool
	lfDequant        [3]float32
	globalScale      uint32
	quantLF          uint32
	epf              []int32
	quantModes       [numQuantTables]uint32
}

func (s *vardctState) readLFGlobal(br *bitReader) error {
	f := s.f
	if f.flags&(flagPatches|flagSplines|flagNoise) != 0 {
		return unsupported("patches, splines or noise in a VarDCT frame")
	}
	s.lfDequantDefault = br.bool()
	if !s.lfDequantDefault { // LF channel dequantization
		for i := 0; i < 3; i++ {
			s.lfDequant[i] = br.f16()
		}
	}
	s.globalScale = br.u32(bitsOff(11, 1), bitsOff(11, 2049), bitsOff(12, 4097), bitsOff(16, 8193))
	s.quantLF = br.u32(val(16), bitsOff(5, 1), bitsOff(8, 1), bitsOff(16, 1))
	var err error
	if s.bctx, err = readBlockCtxMap(br); err != nil {
		return err
	}
	s.colourFactor = 84
	if !br.bool() {
		s.colourFactor = br.u32(val(84), val(256), bitsOff(8, 2), bitsOff(16, 258))
		s.baseCorrX = br.f16()
		s.baseCorrB = br.f16()
		s.ytoxDC = int32(br.u(8)) - 128
		s.ytobDC = int32(br.u(8)) - 128
	} else {
		s.baseCorrB = 1
	}
	return s.readGlobalModular(br)
}

func (s *vardctState) readGlobalModular(br *bitReader) error {
	if br.bool() {
		limit := min(1<<22, 1024+s.f.xsize*s.f.ysize*3/16)
		tree, err := readTree(br, limit)
		if err != nil {
			return err
		}
		code, err := readEntropyCode(br, tree.numLeaves())
		if err != nil {
			return err
		}
		s.global = modularContext{tree: tree, code: code}
	}
	return br.err
}

func (s *vardctState) lfGroupRect(g int) (x0, y0, w, h int) {
	f := s.f
	gx, gy := g%f.xLFGroups, g/f.xLFGroups
	x0, y0 = gx*f.groupDim, gy*f.groupDim
	w = min(f.groupDim, f.xsizeBlocks-x0)
	h = min(f.groupDim, f.ysizeBlocks-y0)
	return
}

func (s *vardctState) init() {
	f := s.f
	nb := f.xsizeBlocks * f.ysizeBlocks
	s.acs = make([]uint8, nb)
	for i := range s.acs {
		s.acs[i] = 0xFF
	}
	s.qf = make([]int32, nb)
	s.qdc = make([]uint8, nb)
	s.tilesX = divCeil(f.xsizeBlocks, colourTileBlocks)
	tiles := s.tilesX * divCeil(f.ysizeBlocks, colourTileBlocks)
	s.ytoxMap = make([]int32, tiles)
	s.ytobMap = make([]int32, tiles)
	for c := 0; c < 3; c++ {
		s.dcW[c] = f.xsizeBlocks >> f.hshift(c)
		s.dcH[c] = f.ysizeBlocks >> f.vshift(c)
		s.dc[c] = make([]int32, s.dcW[c]*s.dcH[c])
		s.coeffs[c] = make([]int32, 64*s.dcW[c]*s.dcH[c])
	}
}

func (s *vardctState) readLFGroup(br *bitReader, g int) error {
	f := s.f
	x0, y0, w, h := s.lfGroupRect(g)
	// VarDCT LF coefficients.
	extra := br.u(2)
	if s.lfPrecision != nil {
		s.lfPrecision[g] = uint8(extra)
	} else if extra != 0 {
		return unsupported("LF extra precision in a JPEG frame")
	}
	img := &modularImage{bitDepth: 8, channels: make([]channel, 3)}
	order := [3]int{1, 0, 2} // modular channel i holds X/Y/B channel order[i]
	for i := 0; i < 3; i++ {
		c := order[i]
		img.channels[i] = newChannel(w>>f.hshift(c), h>>f.vshift(c))
	}
	if _, err := decodeModular(br, img, 1+g, &s.global, 1<<30, true); err != nil {
		return err
	}
	for i := 0; i < 3; i++ {
		c := order[i]
		ch := img.channels[i]
		cx0, cy0 := x0>>f.hshift(c), y0>>f.vshift(c)
		for y := 0; y < ch.h; y++ {
			copy(s.dc[c][(cy0+y)*s.dcW[c]+cx0:], ch.data[y*ch.w:(y+1)*ch.w])
		}
	}
	// DC context buckets per luma block.
	for y := 0; y < h; y++ {
		for x := 0; x < w; x++ {
			bx, by := x0+x, y0+y
			var bucket [3]int
			for c := 0; c < 3; c++ {
				v := s.dc[c][(by>>f.vshift(c))*s.dcW[c]+bx>>f.hshift(c)]
				for _, t := range s.bctx.dcThresholds[c] {
					if v > t {
						bucket[c]++
					}
				}
			}
			b := bucket[0]
			b = b*(len(s.bctx.dcThresholds[2])+1) + bucket[2]
			b = b*(len(s.bctx.dcThresholds[1])+1) + bucket[1]
			s.qdc[by*f.xsizeBlocks+bx] = uint8(b)
		}
	}
	// Modular LF group: only extra channels, which JPEG frames do not have.
	// AC metadata.
	count := int(br.u(ceilLog2(uint64(w*h)))) + 1
	tw, th := (w+7)>>3, (h+7)>>3
	meta := &modularImage{bitDepth: 8, channels: []channel{
		newChannel(tw, th), newChannel(tw, th), newChannel(count, 2), newChannel(w, h),
	}}
	meta.channels[0].hshift, meta.channels[0].vshift = 3, 3
	meta.channels[1].hshift, meta.channels[1].vshift = 3, 3
	streamID := 1 + 2*f.numLFGroups + g
	if _, err := decodeModular(br, meta, streamID, &s.global, 1<<30, true); err != nil {
		return err
	}
	s.epf = append(s.epf, meta.channels[3].data...)
	tx0, ty0 := x0>>3, y0>>3
	for y := 0; y < th; y++ {
		for x := 0; x < tw; x++ {
			s.ytoxMap[(ty0+y)*s.tilesX+tx0+x] = clampInt8(meta.channels[0].data[y*tw+x])
			s.ytobMap[(ty0+y)*s.tilesX+tx0+x] = clampInt8(meta.channels[1].data[y*tw+x])
		}
	}
	num := 0
	acsRow, qfRow := meta.channels[2].data[:count], meta.channels[2].data[count:]
	for iy := 0; iy < h; iy++ {
		for ix := 0; ix < w; ix++ {
			bx, by := x0+ix, y0+iy
			if sh := meta.channels[3].data[iy*w+ix]; sh < 0 || sh >= 8 {
				return corrupt("EPF sharpness")
			}
			if s.acs[by*f.xsizeBlocks+bx] != 0xFF {
				continue
			}
			if num >= count {
				return corrupt("AC metadata count")
			}
			strat := acsRow[num]
			if strat < 0 || strat >= 27 {
				return corrupt("AC strategy")
			}
			if strat != acStrategyDCT8 {
				return unsupported("AC strategy %d (only DCT8 is supported)", strat)
			}
			s.acs[by*f.xsizeBlocks+bx] = uint8(strat)
			s.qf[by*f.xsizeBlocks+bx] = 1 + max(0, min(255, qfRow[num]))
			num++
		}
	}
	return br.err
}

func clampInt8(v int32) int32 { return max(-128, min(127, v)) }

func (s *vardctState) readHFGlobal(br *bitReader) error {
	f := s.f
	if !br.bool() { // dequant matrices not all default
		if s.lfPrecision != nil {
			return unsupported("custom XYB AC quantization")
		}
		for idx := 0; idx < numQuantTables; idx++ {
			if err := s.readQuantEncoding(br, idx); err != nil {
				return err
			}
		}
	}
	s.numHisto = int(br.u(ceilLog2(uint64(f.numGroups)))) + 1
	s.hfCodes = make([]*entropyCode, f.numPasses)
	for p := 0; p < int(f.numPasses); p++ {
		used := br.u32(val(0x5F), val(0x13), val(0), bitsN(numOrders))
		if err := s.readCoeffOrders(br, used); err != nil {
			return err
		}
		code, err := readEntropyCode(br, s.numHisto*s.bctx.numACContexts())
		if err != nil {
			return err
		}
		s.hfCodes[p] = code
	}
	return br.err
}

func readDCTParams(br *bitReader) {
	n := int(br.u(4)) + 1
	for c := 0; c < 3; c++ {
		for i := 0; i < n; i++ {
			br.f16()
		}
	}
}

func (s *vardctState) readQuantEncoding(br *bitReader, idx int) error {
	mode := br.u(3)
	s.quantModes[idx] = mode
	single := quantTableSizeX[idx]*quantTableSizeY[idx] == 1
	switch mode {
	case 0: // library
	case 1: // identity
		if !single {
			return corrupt("quant mode")
		}
		for i := 0; i < 9; i++ {
			br.f16()
		}
	case 2: // DCT2
		if !single {
			return corrupt("quant mode")
		}
		for i := 0; i < 18; i++ {
			br.f16()
		}
	case 3: // DCT4
		if !single {
			return corrupt("quant mode")
		}
		for i := 0; i < 6; i++ {
			br.f16()
		}
		readDCTParams(br)
	case 4: // DCT4x8
		if !single {
			return corrupt("quant mode")
		}
		for i := 0; i < 3; i++ {
			br.f16()
		}
		readDCTParams(br)
	case 5: // AFV
		if !single {
			return corrupt("quant mode")
		}
		for i := 0; i < 27; i++ {
			br.f16()
		}
		readDCTParams(br)
		readDCTParams(br)
	case 6: // DCT
		readDCTParams(br)
	case 7: // RAW
		den := br.f16()
		w, h := quantTableSizeX[idx]*8, quantTableSizeY[idx]*8
		img := &modularImage{bitDepth: 8, channels: []channel{newChannel(w, h), newChannel(w, h), newChannel(w, h)}}
		streamID := 1 + 3*s.f.numLFGroups + idx
		if _, err := decodeModular(br, img, streamID, &s.global, 1<<30, true); err != nil {
			return err
		}
		if idx == 0 {
			s.rawQ = true
			s.rawQTableDen = den
			s.rawQTable = make([]int32, 0, 3*w*h)
			for c := 0; c < 3; c++ {
				for _, v := range img.channels[c].data {
					if v <= 0 {
						return corrupt("raw quantization table")
					}
					s.rawQTable = append(s.rawQTable, v)
				}
			}
		}
	default:
		return corrupt("quant encoding mode")
	}
	return br.err
}

func (s *vardctState) readCoeffOrders(br *bitReader, used uint32) error {
	var sr *symbolReader
	if used != 0 {
		code, err := readEntropyCode(br, 8)
		if err != nil {
			return err
		}
		sr = newSymbolReader(code, br, 0)
	}
	for ord := 0; ord < numOrders; ord++ {
		llf := orderLLF[ord]
		size := 64 * llf
		for c := 0; c < 3; c++ {
			if used&(1<<ord) == 0 {
				if ord == 0 {
					s.orders[0][c] = naturalOrderDCT8()
				}
				continue
			}
			perm, err := readPermutation(br, sr, size, llf)
			if err != nil {
				return err
			}
			if ord == 0 {
				nat := naturalOrderDCT8()
				o := make([]int, 64)
				for k := range o {
					o[k] = nat[perm[k]]
				}
				s.orders[0][c] = o
			}
		}
	}
	if sr != nil && !sr.finalStateOK() {
		return corrupt("coefficient order final state")
	}
	return br.err
}

// naturalOrderDCT8 is the zig-zag order of an 8x8 block in JPEG XL's
// coefficient layout (row-major, index y*8+x).
func naturalOrderDCT8() []int {
	order := make([]int, 0, 64)
	order = append(order, 0)
	for i := 0; i < 8; i++ {
		for j := 0; j <= i; j++ {
			x, y := j, i-j
			if i%2 == 1 {
				x, y = y, x
			}
			if x == 0 && y == 0 {
				continue
			}
			order = append(order, y*8+x)
		}
	}
	for ip := 7; ip > 0; ip-- {
		i := ip - 1
		for j := 0; j <= i; j++ {
			x, y := 7-(i-j), 7-j
			if i%2 == 1 {
				x, y = y, x
			}
			order = append(order, y*8+x)
		}
	}
	return order
}

func predictNonZeros(top, cur []int32, x int, hasTop bool) int {
	switch {
	case x == 0 && !hasTop:
		return 32
	case x == 0:
		return int(top[x])
	case !hasTop:
		return int(cur[x-1])
	}
	return int(top[x]+cur[x-1]+1) / 2
}

func zeroDensityCtx(nonZerosLeft, k, prev int) int {
	return (coeffNumNonzeroContext[nonZerosLeft]+coeffFreqContext[k])*2 + prev
}

// readPassGroup decodes the HF coefficients of one group for one pass.
func (s *vardctState) readPassGroup(br *bitReader, pass, g int) error {
	f := s.f
	histo := int(br.u(ceilLog2(uint64(s.numHisto))))
	if histo >= s.numHisto {
		return corrupt("histogram selector")
	}
	ctxOffset := histo * s.bctx.numACContexts()
	sr := newSymbolReader(s.hfCodes[pass], br, 0)
	gx, gy := g%f.xGroups, g/f.xGroups
	gb := f.groupDim / 8
	bx0, by0 := gx*gb, gy*gb
	bw := min(gb, f.xsizeBlocks-bx0)
	bh := min(gb, f.ysizeBlocks-by0)
	shift := uint(0)
	if len(f.passShift) > pass {
		shift = uint(f.passShift[pass])
	}
	var nz [3][]int32
	for c := 0; c < 3; c++ {
		nz[c] = make([]int32, gb*gb)
	}
	for by := 0; by < bh; by++ {
		if err := br.checkpoint(); err != nil {
			return err
		}
		for bx := 0; bx < bw; bx++ {
			abx, aby := bx0+bx, by0+by
			if s.acs[aby*f.xsizeBlocks+abx] != acStrategyDCT8 {
				return unsupported("AC strategy")
			}
			qfRowBase := aby * f.xsizeBlocks
			for _, c := range [3]int{1, 0, 2} {
				hs, vs := f.hshift(c), f.vshift(c)
				sbx, sby := bx>>hs, by>>vs
				if sbx<<hs != bx || sby<<vs != by {
					continue
				}
				row := nz[c][sby*gb : (sby+1)*gb]
				var top []int32
				if sby > 0 {
					top = nz[c][(sby-1)*gb : sby*gb]
				}
				pred := predictNonZeros(top, row, sbx, sby > 0)
				qf := s.qf[qfRowBase+bx0+sbx]
				blockCtx := s.bctx.context(int(s.qdc[aby*f.xsizeBlocks+abx]), qf, 0, c)
				nzCtx := s.bctx.nonZeroContext(pred, blockCtx) + ctxOffset
				nzeros := int(sr.readUint(br, nzCtx))
				if nzeros > 63 {
					return corrupt("too many non-zero coefficients")
				}
				row[sbx] = int32(nzeros)
				histOff := ctxOffset + s.bctx.zeroDensityOffset(blockCtx)
				cbx, cby := (bx0>>hs)+sbx, (by0>>vs)+sby
				block := s.coeffs[c][64*(cby*s.dcW[c]+cbx):]
				order := s.orders[0][c]
				prev := 1
				if nzeros > 4 {
					prev = 0
				}
				for k := 1; k < 64 && nzeros != 0; k++ {
					u := sr.readUint(br, histOff+zeroDensityCtx(nzeros, k, prev))
					coeff := int32(unpackSigned(u)) << shift
					block[order[k]] += coeff
					if u != 0 {
						prev = 1
						nzeros--
					} else {
						prev = 0
					}
				}
				if nzeros != 0 {
					return corrupt("non-zero count mismatch")
				}
			}
		}
		if br.err != nil {
			return br.err
		}
	}
	if !sr.finalStateOK() {
		return corrupt("HF ANS final state")
	}
	return br.err
}
