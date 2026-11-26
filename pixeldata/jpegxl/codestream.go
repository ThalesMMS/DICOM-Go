package jpegxl

// Codestream entry: signature, headers, ICC stream and the first frame.

const maxImagePixels = 1 << 28

type codestreamHeader struct {
	meta imageMetadata
	icc  []byte // decoded ICC profile when colour encoding wants one
}

func iccContext(i int, b1, b2 byte) int {
	if i <= 128 {
		return 0
	}
	kind1 := func(b byte) int {
		switch {
		case b >= 'a' && b <= 'z', b >= 'A' && b <= 'Z':
			return 0
		case b >= '0' && b <= '9', b == '.', b == ',':
			return 1
		case b == 0:
			return 2
		case b == 1:
			return 3
		case b < 16:
			return 4
		case b == 255:
			return 6
		case b > 240:
			return 5
		}
		return 7
	}
	kind2 := func(b byte) int {
		switch {
		case b >= 'a' && b <= 'z', b >= 'A' && b <= 'Z':
			return 0
		case b >= '0' && b <= '9', b == '.', b == ',':
			return 1
		case b < 16:
			return 2
		case b > 240:
			return 3
		}
		return 4
	}
	return 1 + kind1(b1) + kind2(b2)*8
}

func readEncodedICC(br *bitReader) ([]byte, error) {
	size := br.u64()
	if size > 1<<28 {
		return nil, ErrLimit
	}
	code, err := readEntropyCode(br, 41)
	if err != nil {
		return nil, err
	}
	sr := newSymbolReader(code, br, 0)
	out := make([]byte, size)
	for i := range out {
		var b1, b2 byte
		if i > 0 {
			b1 = out[i-1]
		}
		if i > 1 {
			b2 = out[i-2]
		}
		v := sr.readUint(br, iccContext(i, b1, b2))
		if v > 255 {
			return nil, corrupt("ICC byte out of range")
		}
		out[i] = byte(v)
		if br.err != nil {
			return nil, br.err
		}
	}
	if !sr.finalStateOK() {
		return nil, corrupt("ICC ANS final state")
	}
	return out, nil
}

func readCodestreamHeader(br *bitReader) (*codestreamHeader, error) {
	if br.u(16) != 0x0AFF {
		return nil, corrupt("missing codestream signature")
	}
	w, h := readSizeHeader(br)
	m := readImageMetadata(br)
	if br.err != nil {
		return nil, br.err
	}
	if w <= 0 || h <= 0 || uint64(w)*uint64(h) > maxImagePixels {
		return nil, ErrLimit
	}
	m.width, m.height = w, h
	hdr := &codestreamHeader{meta: m}
	if m.colour.wantICC {
		if m.xybEncoded {
			return nil, unsupported("ICC output profile in XYB codestream")
		}
		enc, err := readEncodedICC(br)
		if err != nil {
			return nil, err
		}
		if hdr.icc, err = unpredictICC(enc); err != nil {
			return nil, err
		}
	}
	if m.havePreview {
		return nil, unsupported("preview frame")
	}
	br.zeroPadToByte()
	return hdr, br.err
}

// readFirstFrame reads the frame header and TOC of the single image frame
// this profile supports.
func readFirstFrame(br *bitReader, hdr *codestreamHeader) (*frameHeader, []int, []int, error) {
	f, err := readFrameHeader(br, &hdr.meta)
	if err != nil {
		return nil, nil, nil, err
	}
	if f.frameType != frameRegular || !f.isLast {
		return nil, nil, nil, unsupported("multi-frame or non-regular frame")
	}
	if f.haveCrop && (f.x0 != 0 || f.y0 != 0 || f.width != hdr.meta.width || f.height != hdr.meta.height) {
		return nil, nil, nil, unsupported("cropped frame")
	}
	offsets, sizes, err := readTOC(br, f)
	if err != nil {
		return nil, nil, nil, err
	}
	return f, offsets, sizes, nil
}

// sectionReaders returns a bit reader per TOC entry. A single-entry TOC
// shares one reader for every section.
func sectionReaders(br *bitReader, data []byte, offsets, sizes []int) ([]*bitReader, error) {
	base := br.bytePos()
	if base > len(data) {
		return nil, errTruncated
	}
	readers := make([]*bitReader, len(offsets))
	for i := range offsets {
		offset, size := offsets[i], sizes[i]
		if offset < 0 || offset > len(data)-base || size < 0 || size > len(data)-base-offset {
			return nil, errTruncated
		}
		start := base + offset
		readers[i] = &bitReader{data: data[start : start+size], ctx: br.ctx}
	}
	return readers, nil
}
