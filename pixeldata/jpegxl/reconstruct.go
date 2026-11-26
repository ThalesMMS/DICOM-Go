package jpegxl

// JPEG bitstream reconstruction: JPEG XL files made by lossless JPEG
// recompression (DICOM JPEG XL JPEG Recompression, 1.2.840.10008.1.2.4.111)
// carry a jbrd box; together with the quantized DCT coefficients of the
// VarDCT frame it restores the original JPEG file byte for byte.

const cflFixedPointPrecision = 11
const defaultColourFactor = 84

// ReconstructJPEG returns the original JPEG bytes of a JPEG XL file produced
// by lossless JPEG recompression. It decodes only quantized coefficients; no
// inverse DCT or colour conversion is performed.
func ReconstructJPEG(data []byte) (_ []byte, err error) {
	defer recoverCorrupt(&err)
	c, err := parseContainer(data)
	if err != nil {
		return nil, err
	}
	if c.jbrd == nil {
		return nil, ErrNoReconstructionData
	}
	j, err := readJPEGData(c.jbrd)
	if err != nil {
		return nil, err
	}
	hdr, s, err := readJPEGCompatibleFrame(c.codestream)
	if err != nil {
		return nil, err
	}
	isGray := len(j.components) == 1
	isYCbCr := len(j.components) == 3 && s.f.doYCbCr
	if err := s.fillJPEG(j, isGray, isYCbCr); err != nil {
		return nil, err
	}
	if err := fillAppMarkers(j, hdr.icc, c.exif, c.xmp); err != nil {
		return nil, err
	}
	return writeJPEG(j)
}

// readJPEGCompatibleFrame decodes the quantized coefficients of a codestream
// whose single frame is JPEG-compatible VarDCT: no XYB, DCT8 blocks only and
// a RAW quantization table with the JPEG denominator.
func readJPEGCompatibleFrame(codestream []byte) (*codestreamHeader, *vardctState, error) {
	br := newBitReader(codestream)
	hdr, err := readCodestreamHeader(br)
	if err != nil {
		return nil, nil, err
	}
	if hdr.meta.xybEncoded {
		return nil, nil, unsupported("JPEG reconstruction requires a JPEG-compatible frame")
	}
	f, offsets, sizes, err := readFirstFrame(br, hdr)
	if err != nil {
		return nil, nil, err
	}
	if f.modular {
		return nil, nil, corrupt("JPEG-compatible decoding requires a VarDCT frame")
	}
	if len(hdr.meta.extraChannels) != 0 {
		return nil, nil, unsupported("extra channels in a JPEG-compatible frame")
	}
	s, err := readVarDCTSections(br, codestream, f, offsets, sizes, false)
	if err != nil {
		return nil, nil, err
	}
	if !s.rawQ || s.rawQTableDen < 1.0/(8*255)*0.999 || s.rawQTableDen > 1.0/(8*255)*1.001 {
		return nil, nil, unsupported("quantization table is not a JPEG table")
	}
	return hdr, s, nil
}

func readVarDCTSections(br *bitReader, codestream []byte, f *frameHeader, offsets, sizes []int, xyb bool) (*vardctState, error) {
	readers, err := sectionReaders(br, codestream, offsets, sizes)
	if err != nil {
		return nil, err
	}
	if xyb {
		end := br.bytePos()
		for i, off := range offsets {
			end = max(end, br.bytePos()+off+sizes[i])
		}
		if end != len(codestream) && !(end+1 == len(codestream) && end&1 == 1 && codestream[end] == 0) {
			return nil, corrupt("unexpected bytes after the XYB frame")
		}
	}
	section := func(i int) *bitReader {
		if len(readers) == 1 {
			return readers[0]
		}
		return readers[i]
	}
	s := &vardctState{f: f}
	if xyb {
		s.lfPrecision = make([]uint8, f.numLFGroups)
	}
	s.init()
	if err := s.readLFGlobal(section(0)); err != nil {
		return nil, err
	}
	for g := 0; g < f.numLFGroups; g++ {
		if err := s.readLFGroup(section(1+g), g); err != nil {
			return nil, err
		}
	}
	if err := s.readHFGlobal(section(1 + f.numLFGroups)); err != nil {
		return nil, err
	}
	for p := 0; p < int(f.numPasses); p++ {
		for g := 0; g < f.numGroups; g++ {
			if err := s.readPassGroup(section(2+f.numLFGroups+p*f.numGroups+g), p, g); err != nil {
				return nil, err
			}
		}
	}
	if xyb {
		for _, reader := range readers {
			reader.zeroPadToByte()
			if err := reader.checkpoint(); err != nil {
				return nil, err
			}
			if reader.bytePos() != len(reader.data) {
				return nil, corrupt("unexpected bytes in XYB frame section")
			}
		}
	}
	return s, nil
}

// jpegOrder maps JPEG XL channels (X, Y, B) to JPEG component indices.
func jpegOrder(isGray, isYCbCr bool) [3]int {
	switch {
	case isGray:
		return [3]int{0, 0, 0}
	case isYCbCr:
		return [3]int{1, 0, 2}
	}
	return [3]int{0, 1, 2}
}

func (s *vardctState) fillJPEG(j *jpegData, isGray, isYCbCr bool) error {
	f := s.f
	j.width, j.height = f.xsize, f.ysize
	cmap := jpegOrder(isGray, isYCbCr)
	qtSet := 0
	var qt [3][64]int32 // JPEG natural order per JPEG XL channel
	for c := 0; c < 3; c++ {
		quantC := c
		if isGray {
			quantC = 1
		}
		for x := 0; x < 8; x++ {
			for y := 0; y < 8; y++ {
				qt[c][x*8+y] = s.rawQTable[quantC*64+y*8+x]
			}
		}
		comp := &j.components[cmap[c]]
		q := int(comp.quantIdx)
		qtSet |= 1 << q
		j.quant[q].values = qt[c]
	}
	for i := range j.quant {
		if qtSet&(1<<i) != 0 {
			continue
		}
		if i == 0 {
			return corrupt("first JPEG quant table unused")
		}
		j.quant[i].values = j.quant[i-1].values
	}
	channels := []int{0, 1, 2}
	if isGray {
		channels = []int{1}
	}
	for _, c := range channels {
		comp := &j.components[cmap[c]]
		comp.hSamp = 1 << (f.maxHShift - f.hshift(c))
		comp.vSamp = 1 << (f.maxVShift - f.vshift(c))
		comp.widthBlocks = s.dcW[c]
		comp.heightBlocks = s.dcH[c]
		comp.coeffs = make([]int16, 64*s.dcW[c]*s.dcH[c])
	}
	is444 := f.maxHShift == 0 && f.maxVShift == 0
	var dcOff [3]int32
	if !f.doYCbCr {
		for c := 0; c < 3; c++ {
			quantC := c
			if isGray {
				quantC = 1
			}
			dcOff[c] = 1024 / s.rawQTable[quantC*64]
		}
	}
	// Scaled quantization ratios for chroma-from-luma, JPEG natural order.
	var scaledQ [3][64]int32
	for c := 0; c < 3; c++ {
		for i := 0; i < 64; i++ {
			scaledQ[c][i] = (1 << cflFixedPointPrecision) * qt[1][i] / qt[c][i]
		}
	}
	for by := 0; by < f.ysizeBlocks; by++ {
		for bx := 0; bx < f.xsizeBlocks; bx++ {
			tile := (by/colourTileBlocks)*s.tilesX + bx/colourTileBlocks
			cfl := [3]int32{s.ytoxMap[tile], 0, s.ytobMap[tile]}
			var yBlock [64]int32
			for _, c := range []int{1, 0, 2} {
				if isGray && c != 1 {
					continue
				}
				hs, vs := f.hshift(c), f.vshift(c)
				sbx, sby := bx>>hs, by>>vs
				if sbx<<hs != bx || sby<<vs != by {
					continue
				}
				src := s.coeffs[c][64*(sby*s.dcW[c]+sbx):]
				comp := &j.components[cmap[c]]
				dst := comp.coeffs[64*(sby*comp.widthBlocks+sbx):]
				var block [64]int32
				for k := 0; k < 64; k++ {
					block[k] = src[(k%8)*8+k/8] // transpose to JPEG layout
				}
				useCfL := is444 && !isGray && (cfl[0] != 0 || cfl[2] != 0)
				switch {
				case useCfL && c == 1:
					yBlock = block
				case useCfL:
					scale := cfl[c] * (1 << cflFixedPointPrecision) / defaultColourFactor
					round := int32(1 << (cflFixedPointPrecision - 1))
					for k := 0; k < 64; k++ {
						coeffScale := (scaledQ[c][k]*scale + round) >> cflFixedPointPrecision
						block[k] += (yBlock[k]*coeffScale + round) >> cflFixedPointPrecision
					}
				}
				for k := 1; k < 64; k++ {
					dst[k] = clampInt16(block[k])
				}
				dc := s.dc[c][sby*s.dcW[c]+sbx] - dcOff[c]
				dst[0] = int16(max(-2047, min(2047, dc)))
			}
		}
	}
	return nil
}

func clampInt16(v int32) int16 { return int16(max(-32768, min(32767, v))) }

func fillAppMarkers(j *jpegData, icc, exif, xmp []byte) error {
	pos := 0
	for i, m := range j.appData {
		if j.appMarkerType[i] != appICC {
			continue
		}
		n := len(m) - 17
		if pos+n > len(icc) {
			return corrupt("ICC profile shorter than its APP2 markers")
		}
		copy(m[17:], icc[pos:pos+n])
		pos += n
	}
	if pos != len(icc) && pos != 0 {
		return corrupt("ICC profile longer than its APP2 markers")
	}
	for i, m := range j.appData {
		switch j.appMarkerType[i] {
		case appExif:
			if len(exif) < 4 {
				return corrupt("Exif box missing for an Exif marker")
			}
			payload := exif[4:]
			if len(m) != len(payload)+3+len(exifTag) {
				return corrupt("Exif size mismatch")
			}
			copy(m[3+len(exifTag):], payload)
		case appXMP:
			if len(m) != len(xmp)+3+len(xmpTag) {
				return corrupt("XMP size mismatch")
			}
			copy(m[3+len(xmpTag):], xmp)
		}
	}
	return nil
}
