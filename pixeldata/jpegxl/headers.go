package jpegxl

// Codestream headers (ISO/IEC 18181-1 Annex A).

const (
	colourSpaceRGB  = 0
	colourSpaceGrey = 1
	colourSpaceXYB  = 2

	extraChannelAlpha = 0
)

type bitDepth struct {
	floatSample   bool
	bitsPerSample uint32
	expBits       uint32
}

func readBitDepth(br *bitReader) bitDepth {
	var d bitDepth
	d.floatSample = br.bool()
	if !d.floatSample {
		d.bitsPerSample = br.u32(val(8), val(10), val(12), bitsOff(6, 1))
	} else {
		d.bitsPerSample = br.u32(val(32), val(16), val(24), bitsOff(6, 1))
		d.expBits = br.u(4) + 1
	}
	return d
}

type extraChannelInfo struct {
	kind     uint32
	depth    bitDepth
	dimShift uint32
}

func readExtraChannelInfo(br *bitReader) extraChannelInfo {
	e := extraChannelInfo{kind: extraChannelAlpha, depth: bitDepth{bitsPerSample: 8}}
	if br.bool() { // all default
		return e
	}
	e.kind = br.enum()
	e.depth = readBitDepth(br)
	e.dimShift = br.u32(val(0), val(3), val(4), bitsOff(3, 1))
	nameLen := br.u32(val(0), bitsN(4), bitsOff(5, 16), bitsOff(10, 48))
	br.skipLarge(8 * uint64(nameLen))
	switch e.kind {
	case 0: // alpha
		br.bool()
	case 2: // spot colour
		for i := 0; i < 4; i++ {
			br.f16()
		}
	case 5: // CFA
		br.u32(val(1), bitsN(2), bitsOff(4, 3), bitsOff(8, 19))
	}
	return e
}

type colourEncoding struct {
	wantICC     bool
	colourSpace uint32
	isSRGB      bool
}

func readCustomXY(br *bitReader) {
	for i := 0; i < 2; i++ {
		br.u32(bitsN(19), bitsOff(19, 524288), bitsOff(20, 1048576), bitsOff(21, 2097152))
	}
}

func readColourEncoding(br *bitReader) colourEncoding {
	c := colourEncoding{colourSpace: colourSpaceRGB, isSRGB: true}
	if br.bool() {
		return c
	}
	c.wantICC = br.bool()
	c.colourSpace = br.enum()
	c.isSRGB = false
	if c.wantICC {
		return c
	}
	var whitePoint, primaries, transfer uint32
	if c.colourSpace != colourSpaceXYB {
		whitePoint = br.enum()
		if whitePoint == 2 { // custom white point
			readCustomXY(br)
		}
	}
	if c.colourSpace != colourSpaceXYB && c.colourSpace != colourSpaceGrey {
		primaries = br.enum()
		if primaries == 2 { // custom primaries
			readCustomXY(br)
			readCustomXY(br)
			readCustomXY(br)
		}
	}
	if c.colourSpace != colourSpaceXYB {
		if br.bool() { // have gamma
			br.u(24)
		} else {
			transfer = br.enum()
		}
	}
	c.isSRGB = c.colourSpace == colourSpaceRGB && whitePoint == 1 && primaries == 1 && transfer == 13
	br.enum() // rendering intent
	return c
}

type imageMetadata struct {
	width, height  int
	orientation    uint32
	haveAnimation  bool
	haveTimecodes  bool
	havePreview    bool
	depth          bitDepth
	modular16Bit   bool
	extraChannels  []extraChannelInfo
	xybEncoded     bool
	customOpsin    bool
	customTone     bool
	colour         colourEncoding
	upsamplingMask uint32
}

func readSizeHeader(br *bitReader) (int, int) {
	var h, w uint64
	small := br.bool()
	if small {
		h = (uint64(br.u(5)) + 1) * 8
	} else {
		h = uint64(br.u32(bitsOff(9, 1), bitsOff(13, 1), bitsOff(18, 1), bitsOff(30, 1)))
	}
	ratio := br.u(3)
	if ratio == 0 {
		if small {
			w = (uint64(br.u(5)) + 1) * 8
		} else {
			w = uint64(br.u32(bitsOff(9, 1), bitsOff(13, 1), bitsOff(18, 1), bitsOff(30, 1)))
		}
	} else {
		w = fixedRatioWidth(h, ratio)
	}
	return int(w), int(h)
}

func fixedRatioWidth(h uint64, ratio uint32) uint64 {
	num := [8]uint64{0, 1, 12, 4, 3, 16, 5, 2}
	den := [8]uint64{1, 1, 10, 3, 2, 9, 4, 1}
	return h * num[ratio] / den[ratio]
}

func readPreviewHeader(br *bitReader) {
	div8 := br.bool()
	if div8 {
		br.u32(val(16), val(32), bitsOff(5, 1), bitsOff(9, 33))
	} else {
		br.u32(bitsOff(6, 1), bitsOff(8, 65), bitsOff(10, 321), bitsOff(12, 1345))
	}
	if br.u(3) == 0 {
		if div8 {
			br.u32(val(16), val(32), bitsOff(5, 1), bitsOff(9, 33))
		} else {
			br.u32(bitsOff(6, 1), bitsOff(8, 65), bitsOff(10, 321), bitsOff(12, 1345))
		}
	}
}

func readImageMetadata(br *bitReader) imageMetadata {
	m := imageMetadata{
		orientation:  1,
		depth:        bitDepth{bitsPerSample: 8},
		modular16Bit: true,
		xybEncoded:   true,
		colour:       colourEncoding{colourSpace: colourSpaceRGB, isSRGB: true},
	}
	if !br.bool() { // !all_default
		extraFields := br.bool()
		if extraFields {
			m.orientation = 1 + br.u(3)
			if br.bool() { // intrinsic size
				readSizeHeader(br)
			}
			if m.havePreview = br.bool(); m.havePreview {
				readPreviewHeader(br)
			}
			if m.haveAnimation = br.bool(); m.haveAnimation {
				br.u32(val(100), val(1000), bitsOff(10, 1), bitsOff(30, 1))
				br.u32(val(1), val(1001), bitsOff(8, 1), bitsOff(10, 1))
				br.u32(val(0), bitsN(3), bitsN(16), bitsN(32))
				m.haveTimecodes = br.bool()
			}
		}
		m.depth = readBitDepth(br)
		m.modular16Bit = br.bool()
		n := br.u32(val(0), val(1), bitsOff(4, 2), bitsOff(12, 1))
		if n > 256 {
			br.fail(unsupported("%d extra channels", n))
			return m
		}
		m.extraChannels = make([]extraChannelInfo, n)
		for i := range m.extraChannels {
			m.extraChannels[i] = readExtraChannelInfo(br)
		}
		m.xybEncoded = br.bool()
		m.colour = readColourEncoding(br)
		if extraFields {
			m.customTone = !br.bool()
			if m.customTone {
				br.f16()
				br.f16()
				br.bool()
				br.f16()
			}
		}
		br.extensions()
	}
	// CustomTransformData.
	if !br.bool() {
		if m.xybEncoded {
			m.customOpsin = !br.bool()
			if m.customOpsin {
				for i := 0; i < 16; i++ {
					br.f16()
				}
			}
		}
		m.upsamplingMask = br.u(3)
		counts := [3]int{15, 55, 210}
		for i, c := range counts {
			if m.upsamplingMask&(1<<i) != 0 {
				for k := 0; k < c; k++ {
					br.f16()
				}
			}
		}
	}
	return m
}
