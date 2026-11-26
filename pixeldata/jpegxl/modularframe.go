package jpegxl

// Modular frames (lossless JPEG XL): the global modular image, LF group and
// pass group sub-bitstreams, and the final inverse transforms.

type modularFrame struct {
	f      *frameHeader
	img    *modularImage
	header groupHeader
	global modularContext
	nbChan int
}

func decodeModularFrame(hdr *codestreamHeader, f *frameHeader, readers []*bitReader) (*modularImage, error) {
	m := &hdr.meta
	if f.flags&(flagPatches|flagSplines|flagNoise) != 0 {
		return nil, unsupported("patches, splines or noise")
	}
	if m.xybEncoded {
		return nil, unsupported("XYB-encoded modular frame")
	}
	if f.doYCbCr {
		return nil, unsupported("YCbCr modular frame")
	}
	if m.depth.floatSample || m.depth.bitsPerSample > 31 {
		return nil, unsupported("%d-bit or floating-point samples", m.depth.bitsPerSample)
	}
	section := func(i int) *bitReader {
		if len(readers) == 1 {
			return readers[0]
		}
		return readers[i]
	}
	br := section(0)
	if !br.bool() { // LF channel dequantization
		for i := 0; i < 3; i++ {
			br.f16()
		}
	}
	mf := &modularFrame{f: f}
	nb := 3
	if m.colour.colourSpace == colourSpaceGrey {
		nb = 1
	}
	mf.nbChan = nb
	if br.bool() {
		limit := min(1<<22, 1024+f.xsize*f.ysize*(nb+len(m.extraChannels))/16)
		tree, err := readTree(br, limit)
		if err != nil {
			return nil, err
		}
		code, err := readEntropyCode(br, tree.numLeaves())
		if err != nil {
			return nil, err
		}
		mf.global = modularContext{tree: tree, code: code}
	}
	img := &modularImage{bitDepth: int(m.depth.bitsPerSample)}
	for c := 0; c < nb; c++ {
		img.channels = append(img.channels, newChannel(f.xsize, f.ysize))
	}
	for i, ec := range m.extraChannels {
		up := int(f.ecUpsampling[i])
		ch := newChannel(divCeil(f.xsize, up), divCeil(f.ysize, up))
		shift := int(ceilLog2(uint64(up)))
		ch.hshift, ch.vshift = shift, shift
		_ = ec
		img.channels = append(img.channels, ch)
	}
	hdrG, err := decodeModular(br, img, 0, &mf.global, f.groupDim, false)
	if err != nil {
		return nil, err
	}
	mf.img, mf.header = img, hdrG
	for g := 0; g < f.numLFGroups; g++ {
		gx, gy := g%f.xLFGroups, g/f.xLFGroups
		d := f.groupDim * 8
		if err := mf.decodeGroup(section(1+g), gx*d, gy*d, d, 3, 1000, 1+f.numLFGroups+g); err != nil {
			return nil, err
		}
	}
	for p := 0; p < int(f.numPasses); p++ {
		minShift, maxShift := f.downsamplingBracket(p)
		for g := 0; g < f.numGroups; g++ {
			gx, gy := g%f.xGroups, g/f.xGroups
			stream := 1 + 3*f.numLFGroups + numQuantTables + f.numGroups*p + g
			if err := mf.decodeGroup(section(2+f.numLFGroups+p*f.numGroups+g), gx*f.groupDim, gy*f.groupDim, f.groupDim, minShift, maxShift, stream); err != nil {
				return nil, err
			}
		}
	}
	if err := undoTransforms(img, hdrG.transforms, hdrG.wp); err != nil {
		return nil, err
	}
	if len(img.channels) != nb+len(m.extraChannels) {
		return nil, corrupt("channel count after transforms")
	}
	return img, nil
}

// downsamplingBracket returns the channel shift range decoded in a pass.
func (f *frameHeader) downsamplingBracket(pass int) (int, int) {
	if f.numPasses == 1 {
		return 0, 2
	}
	// Multi-pass modular frames are rejected before reaching here.
	return 0, 2
}

func (mf *modularFrame) decodeGroup(br *bitReader, x0, y0, size, minShift, maxShift, stream int) error {
	full := mf.img
	gi := &modularImage{bitDepth: full.bitDepth}
	type place struct{ c, x, y int }
	var places []place
	c := full.nbMeta
	for ; c < len(full.channels); c++ {
		fc := full.channels[c]
		if fc.w > mf.f.groupDim || fc.h > mf.f.groupDim {
			break
		}
	}
	for ; c < len(full.channels); c++ {
		fc := full.channels[c]
		shift := min(fc.hshift, fc.vshift)
		if shift > maxShift || shift < minShift {
			continue
		}
		rx, ry := x0>>fc.hshift, y0>>fc.vshift
		rw := min(size>>fc.hshift, fc.w-rx)
		rh := min(size>>fc.vshift, fc.h-ry)
		if rw <= 0 || rh <= 0 {
			continue
		}
		ch := newChannel(rw, rh)
		ch.hshift, ch.vshift = fc.hshift, fc.vshift
		gi.channels = append(gi.channels, ch)
		places = append(places, place{c, rx, ry})
	}
	if len(gi.channels) == 0 {
		return nil
	}
	if _, err := decodeModular(br, gi, stream, &mf.global, 1<<30, true); err != nil {
		return err
	}
	for i, p := range places {
		src := gi.channels[i]
		dst := &full.channels[p.c]
		for y := 0; y < src.h; y++ {
			copy(dst.data[(p.y+y)*dst.w+p.x:], src.data[y*src.w:(y+1)*src.w])
		}
	}
	return nil
}
