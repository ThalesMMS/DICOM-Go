package jpegxl

// Frame header, table of contents and frame dimensions (Annex C/F).

const (
	frameRegular         = 0
	frameLF              = 1
	frameReferenceOnly   = 2
	frameSkipProgressive = 3

	flagNoise    = 1
	flagPatches  = 2
	flagSplines  = 16
	flagUseLF    = 32
	flagSkipLFSm = 128
)

// chroma modes of jpeg_upsampling: 4:4:4, 4:2:0, 4:2:2, 4:4:0.
var chromaHShift = [4]int{0, 1, 1, 0}
var chromaVShift = [4]int{0, 1, 0, 1}

type frameHeader struct {
	frameType      uint32
	modular        bool
	flags          uint64
	doYCbCr        bool
	jpegUpsampling [3]uint32
	upsampling     uint32
	xybQuantScale  [2]uint32 // X and B channels
	ecUpsampling   []uint32
	groupSizeShift uint32
	numPasses      uint32
	passShift      []uint32
	lfLevel        uint32
	haveCrop       bool
	x0, y0         int32
	width, height  int
	isLast         bool
	blendMode      uint32
	gab            bool
	epfIters       uint32

	// derived
	groupDim                 int
	xsize, ysize             int
	xsizeBlocks, ysizeBlocks int
	xGroups, yGroups         int
	xLFGroups, yLFGroups     int
	numGroups, numLFGroups   int
	maxHShift, maxVShift     int
}

func (f *frameHeader) hshift(c int) int { return f.maxHShift - chromaHShift[f.jpegUpsampling[c]] }
func (f *frameHeader) vshift(c int) int { return f.maxVShift - chromaVShift[f.jpegUpsampling[c]] }

func readBlendingInfo(br *bitReader, numExtra int, fullFrame bool) uint32 {
	mode := br.u32(val(0), val(1), val(2), bitsOff(2, 3))
	if numExtra > 0 && (mode == 2 || mode == 3) {
		br.u32(val(0), val(1), val(2), bitsOff(3, 3))
	}
	if numExtra > 0 && (mode == 2 || mode == 3 || mode == 4) {
		br.bool()
	}
	if mode != 0 || !fullFrame {
		br.u32(val(0), val(1), val(2), val(3))
	}
	return mode
}

func readFrameHeader(br *bitReader, m *imageMetadata) (*frameHeader, error) {
	f := &frameHeader{upsampling: 1, xybQuantScale: [2]uint32{3, 2}, groupSizeShift: 1, numPasses: 1, isLast: true, gab: true, epfIters: 1}
	numExtra := len(m.extraChannels)
	f.ecUpsampling = make([]uint32, numExtra)
	for i := range f.ecUpsampling {
		f.ecUpsampling[i] = 1
	}
	f.width, f.height = m.width, m.height
	allDefault := br.bool()
	if !allDefault {
		f.frameType = br.u(2)
		f.modular = br.u(1) == 1
		f.flags = br.u64()
		if !m.xybEncoded {
			f.doYCbCr = br.bool()
		}
		if f.flags&flagUseLF == 0 {
			if f.doYCbCr {
				for i := range f.jpegUpsampling {
					f.jpegUpsampling[i] = br.u(2)
				}
			}
			f.upsampling = br.u32(val(1), val(2), val(4), val(8))
			for i := range f.ecUpsampling {
				f.ecUpsampling[i] = br.u32(val(1), val(2), val(4), val(8))
			}
		}
		if f.modular {
			f.groupSizeShift = br.u(2)
		}
		if m.xybEncoded && !f.modular {
			f.xybQuantScale[0] = br.u(3)
			f.xybQuantScale[1] = br.u(3)
		}
		if f.frameType != frameReferenceOnly {
			f.numPasses = br.u32(val(1), val(2), val(3), bitsOff(3, 4))
			if f.numPasses != 1 {
				numDS := br.u32(val(0), val(1), val(2), bitsOff(1, 3))
				if numDS > f.numPasses-1 {
					return nil, corrupt("too many downsampling passes")
				}
				f.passShift = make([]uint32, f.numPasses)
				for i := uint32(0); i+1 < f.numPasses; i++ {
					f.passShift[i] = br.u(2)
				}
				for i := uint32(0); i < numDS; i++ {
					br.u32(val(1), val(2), val(4), val(8))
				}
				for i := uint32(0); i < numDS; i++ {
					br.u32(val(0), val(1), val(2), bitsN(3))
				}
			}
		}
		if f.frameType == frameLF {
			f.lfLevel = br.u(2) + 1
		} else {
			f.haveCrop = br.bool()
		}
		if f.haveCrop {
			if f.frameType != frameReferenceOnly {
				f.x0 = unpackSigned(br.u32(bitsN(8), bitsOff(11, 256), bitsOff(14, 2304), bitsOff(30, 18688)))
				f.y0 = unpackSigned(br.u32(bitsN(8), bitsOff(11, 256), bitsOff(14, 2304), bitsOff(30, 18688)))
			}
			f.width = int(br.u32(bitsN(8), bitsOff(11, 256), bitsOff(14, 2304), bitsOff(30, 18688)))
			f.height = int(br.u32(bitsN(8), bitsOff(11, 256), bitsOff(14, 2304), bitsOff(30, 18688)))
		}
		fullFrame := !f.haveCrop || (f.x0 <= 0 && f.y0 <= 0 &&
			int(f.x0)+f.width >= m.width && int(f.y0)+f.height >= m.height)
		normal := f.frameType == frameRegular || f.frameType == frameSkipProgressive
		if normal {
			f.blendMode = readBlendingInfo(br, numExtra, fullFrame)
			for i := 0; i < numExtra; i++ {
				readBlendingInfo(br, numExtra, fullFrame)
			}
			if m.haveAnimation {
				br.u32(val(0), val(1), bitsN(8), bitsN(32))
			}
			if m.haveTimecodes {
				br.u(32)
			}
			f.isLast = br.bool()
		} else {
			f.isLast = false
		}
		if f.frameType != frameLF && !f.isLast {
			br.u(2) // save_as_reference
		}
		// save_before_color_transform; for the frames this decoder accepts
		// (single regular last frame) it is absent unless the frame is
		// reference-only.
		if f.frameType == frameReferenceOnly {
			br.bool()
		} else if !f.isLast && fullFrame && f.frameType != frameLF {
			// resets_canvas && can_reference: present for full replace
			// frames that may be referenced; multi-frame files are rejected
			// below, so the exact rule does not matter for accepted input.
			br.bool()
		}
		nameLen := br.u32(val(0), bitsN(4), bitsOff(5, 16), bitsOff(10, 48))
		br.skipLarge(8 * uint64(nameLen))
		// Restoration filter.
		if !br.bool() {
			f.gab = br.bool()
			if f.gab && br.bool() {
				for i := 0; i < 6; i++ {
					br.f16()
				}
			}
			f.epfIters = br.u(2)
			if f.epfIters > 0 {
				if !f.modular && br.bool() {
					for i := 0; i < 8; i++ {
						br.f16()
					}
				}
				if br.bool() {
					for i := 0; i < 5; i++ {
						br.f16()
					}
				}
				if br.bool() {
					if !f.modular {
						br.f16()
					}
					br.f16()
					br.f16()
					br.f16()
				}
				if f.modular {
					br.f16()
				}
			}
			br.extensions()
		}
		br.extensions()
	}
	if br.err != nil {
		return nil, br.err
	}
	if f.upsampling != 1 {
		return nil, unsupported("frame upsampling %d", f.upsampling)
	}
	f.computeDims()
	return f, nil
}

func (f *frameHeader) computeDims() {
	for c := 0; c < 3; c++ {
		f.maxHShift = max(f.maxHShift, chromaHShift[f.jpegUpsampling[c]])
		f.maxVShift = max(f.maxVShift, chromaVShift[f.jpegUpsampling[c]])
	}
	f.groupDim = (256 >> 1) << f.groupSizeShift
	f.xsize, f.ysize = f.width, f.height
	f.xsizeBlocks = divCeil(f.xsize, 8<<f.maxHShift) << f.maxHShift
	f.ysizeBlocks = divCeil(f.ysize, 8<<f.maxVShift) << f.maxVShift
	f.xGroups = divCeil(f.xsize, f.groupDim)
	f.yGroups = divCeil(f.ysize, f.groupDim)
	f.xLFGroups = divCeil(f.xsizeBlocks, f.groupDim)
	f.yLFGroups = divCeil(f.ysizeBlocks, f.groupDim)
	f.numGroups = f.xGroups * f.yGroups
	f.numLFGroups = f.xLFGroups * f.yLFGroups
}

// readTOC returns the byte offset and size of every section, in logical
// order, relative to the first byte after the TOC.
func readTOC(br *bitReader, f *frameHeader) ([]int, []int, error) {
	n := 1
	if f.numGroups != 1 || f.numPasses != 1 {
		n = 1 + f.numLFGroups + 1 + f.numGroups*int(f.numPasses)
	}
	if n > 1<<22 {
		return nil, nil, ErrLimit
	}
	var perm []int
	if br.bool() {
		code, err := readEntropyCode(br, 8)
		if err != nil {
			return nil, nil, err
		}
		sr := newSymbolReader(code, br, 0)
		if perm, err = readPermutation(br, sr, n, 0); err != nil {
			return nil, nil, err
		}
		if !sr.finalStateOK() {
			return nil, nil, corrupt("TOC permutation final state")
		}
	}
	br.zeroPadToByte()
	sizes := make([]int, n)
	for i := range sizes {
		sizes[i] = int(br.u32(bitsN(10), bitsOff(14, 1024), bitsOff(22, 17408), bitsOff(30, 4211712)))
	}
	br.zeroPadToByte()
	if br.err != nil {
		return nil, nil, br.err
	}
	offsets := make([]int, n)
	total := 0
	for i, s := range sizes {
		if s > int(^uint(0)>>1)-total {
			return nil, nil, ErrLimit
		}
		offsets[i] = total
		total += s
	}
	if perm != nil {
		po, ps := make([]int, n), make([]int, n)
		for i := range perm {
			po[i], ps[i] = offsets[perm[i]], sizes[perm[i]]
		}
		offsets, sizes = po, ps
	}
	return offsets, sizes, nil
}
