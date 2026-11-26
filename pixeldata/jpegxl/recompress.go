package jpegxl

import (
	"bytes"

	"github.com/ThalesMMS/dicom-go/internal/brotli"
)

// Lossless JPEG recompression: a sequential Huffman-coded 8-bit JPEG is
// stored as a VarDCT frame holding its quantized DCT coefficients plus a
// jbrd box, so ReconstructJPEG (or libjxl) restores the identical file and
// any JPEG XL decoder renders the same image.

// RecompressJPEG converts a JPEG file into a JPEG XL file from which the
// original bytes can be reconstructed. The result is verified by
// reconstruction before it is returned; JPEG files that cannot be restored
// exactly (progressive or arithmetic coding, 12-bit precision, unusual
// sampling, non-canonical Huffman streams) fail with ErrNotReversible or
// ErrUnsupported.
func RecompressJPEG(jpeg []byte) (_ []byte, err error) {
	defer recoverCorrupt(&err)
	p, err := parseJPEG(jpeg)
	if err != nil {
		return nil, err
	}
	j := p.data
	if p.sofMarker == 0xC2 {
		return nil, notReversible("progressive JPEG")
	}
	out, err := encodeRecompressed(j, true)
	if err != nil {
		return nil, err
	}
	back, err := ReconstructJPEG(out)
	if err != nil {
		return nil, notReversible("reconstruction failed: %v", err)
	}
	if !bytes.Equal(back, jpeg) {
		return nil, notReversible("reconstruction differs from the source JPEG")
	}
	return out, nil
}

func jpegIsRGB(j *jpegData) bool {
	if len(j.components) != 3 {
		return false
	}
	if containsMarker(j.markerOrder, 0xE0) {
		return false
	}
	app := 0
	for _, m := range j.markerOrder {
		if m&0xF0 != 0xE0 {
			continue
		}
		if m == 0xEE {
			d := j.appData[app]
			if len(d) == 15 && string(d[3:8]) == "Adobe" {
				return d[14] == 0
			}
		}
		app++
	}
	c := j.components
	return c[0].id == 'R' && c[1].id == 'G' && c[2].id == 'B'
}

func samplingMode(h, v int) (uint32, bool) {
	switch {
	case h == 1 && v == 1:
		return 0, true
	case h == 2 && v == 2:
		return 1, true
	case h == 2 && v == 1:
		return 2, true
	case h == 1 && v == 2:
		return 3, true
	}
	return 0, false
}

// encodeRecompressed writes the VarDCT codestream of j's coefficients; with
// reconstruct it is wrapped in a container with the jbrd box.
func encodeRecompressed(j *jpegData, reconstruct bool) ([]byte, error) {
	isGray := len(j.components) == 1
	doYCbCr := isGray || !jpegIsRGB(j)
	cmap := jpegOrder(isGray, doYCbCr && !isGray)
	f := &frameHeader{width: j.width, height: j.height, groupSizeShift: 1, numPasses: 1, doYCbCr: doYCbCr}
	if doYCbCr {
		for c := 0; c < 3; c++ {
			if isGray && c != 1 {
				continue
			}
			comp := j.components[cmap[c]]
			m, ok := samplingMode(comp.hSamp, comp.vSamp)
			if !ok {
				return nil, notReversible("sampling factors %dx%d", comp.hSamp, comp.vSamp)
			}
			f.jpegUpsampling[c] = m
		}
	} else {
		for _, comp := range j.components {
			if comp.hSamp != 1 || comp.vSamp != 1 {
				return nil, notReversible("subsampled RGB JPEG")
			}
		}
	}
	f.computeDims()
	channels := []int{0, 1, 2}
	if isGray {
		channels = []int{1}
	}
	for _, c := range channels {
		comp := j.components[cmap[c]]
		if comp.widthBlocks != f.xsizeBlocks>>f.hshift(c) || comp.heightBlocks != f.ysizeBlocks>>f.vshift(c) {
			return nil, notReversible("component block grid does not match the frame")
		}
		if comp.hSamp != 1<<(f.maxHShift-f.hshift(c)) || comp.vSamp != 1<<(f.maxVShift-f.vshift(c)) {
			return nil, notReversible("sampling factors cannot be restored")
		}
	}
	// A JPEG XL frame carries only the tables its components use; an unused
	// table is reconstructed as a copy of the previous one.
	used := 0
	for _, comp := range j.components {
		used |= 1 << comp.quantIdx
	}
	for i := range j.quant {
		if reconstruct && used&(1<<i) == 0 && (i == 0 || j.quant[i].values != j.quant[i-1].values) {
			return nil, notReversible("unused quantization table %d", j.quant[i].index)
		}
	}
	// Quantization tables in JPEG XL layout per channel (X, Y, B).
	var qt [3][64]int32
	for c := 0; c < 3; c++ {
		src := j.components[cmap[c]]
		if isGray {
			src = j.components[0]
		}
		vals := j.quant[src.quantIdx].values
		for y := 0; y < 8; y++ {
			for x := 0; x < 8; x++ {
				qt[c][y*8+x] = vals[x*8+y]
			}
		}
	}
	var dcW, dcH [3]int
	for c := 0; c < 3; c++ {
		dcW[c] = f.xsizeBlocks >> f.hshift(c)
		dcH[c] = f.ysizeBlocks >> f.vshift(c)
	}
	var dcOff [3]int32
	if !doYCbCr {
		for c := 0; c < 3; c++ {
			dcOff[c] = 1024 / qt[c][0]
		}
	}
	// coefficient access in JPEG XL layout
	coeff := func(c, bx, by int) (dc int32, ac [64]int32) {
		if isGray && c != 1 {
			return 0, ac
		}
		comp := &j.components[cmap[c]]
		blk := comp.coeffs[64*(by*comp.widthBlocks+bx):]
		for k := 0; k < 64; k++ {
			ac[(k%8)*8+k/8] = int32(blk[k])
		}
		return int32(blk[0]) + dcOff[c], ac
	}

	// Modular tree shared by LF coefficients, AC metadata and the
	// quantization table: zero prediction for metadata streams, the
	// self-correcting predictor for LF coefficients.
	root := &tnode{prop: 1, split: int32(f.numLFGroups), left: leafNode(0), right: weightedTree(11)}
	tree, order := flattenTree(root)
	numCtx := tree.numLeaves()

	type lfGroupData struct {
		dc, meta *modularStream
		count    int
	}
	lfData := make([]lfGroupData, f.numLFGroups)
	var modularTokens []*tokenStream
	for g := 0; g < f.numLFGroups; g++ {
		gx, gy := g%f.xLFGroups, g/f.xLFGroups
		x0, y0 := gx*f.groupDim, gy*f.groupDim
		w := min(f.groupDim, f.xsizeBlocks-x0)
		h := min(f.groupDim, f.ysizeBlocks-y0)
		img := &modularImage{bitDepth: 8}
		for _, c := range [3]int{1, 0, 2} {
			cw, ch := w>>f.hshift(c), h>>f.vshift(c)
			cx0, cy0 := x0>>f.hshift(c), y0>>f.vshift(c)
			chn := newChannel(cw, ch)
			for y := 0; y < ch; y++ {
				for x := 0; x < cw; x++ {
					dc, _ := coeff(c, cx0+x, cy0+y)
					chn.data[y*cw+x] = dc
				}
			}
			img.channels = append(img.channels, chn)
		}
		dcStream, err := encodeModularStream(img, tree, defaultWP, 1+g, 1<<30, numCtx)
		if err != nil {
			return nil, err
		}
		tw, th := (w+7)>>3, (h+7)>>3
		count := w * h
		meta := &modularImage{bitDepth: 8, channels: []channel{
			newChannel(tw, th), newChannel(tw, th), newChannel(count, 2), newChannel(w, h),
		}}
		meta.channels[0].hshift, meta.channels[0].vshift = 3, 3
		meta.channels[1].hshift, meta.channels[1].vshift = 3, 3
		metaStream, err := encodeModularStream(meta, tree, defaultWP, 1+2*f.numLFGroups+g, 1<<30, numCtx)
		if err != nil {
			return nil, err
		}
		lfData[g] = lfGroupData{dc: dcStream, meta: metaStream, count: count}
		modularTokens = append(modularTokens, &dcStream.tokens, &metaStream.tokens)
	}
	qimg := &modularImage{bitDepth: 8}
	for c := 0; c < 3; c++ {
		ch := newChannel(8, 8)
		copy(ch.data, qt[c][:])
		qimg.channels = append(qimg.channels, ch)
	}
	qStream, err := encodeModularStream(qimg, tree, defaultWP, 1+3*f.numLFGroups, 1<<30, numCtx)
	if err != nil {
		return nil, err
	}
	modularTokens = append(modularTokens, &qStream.tokens)
	modEnc := buildEntropyEncoder(numCtx, modularTokens...)

	// HF coefficients with the default block context map.
	bctx := &blockCtxMap{ctxMap: defaultBlockCtxMap, numCtxs: 15, numDCCtxs: 1}
	numAC := bctx.numACContexts()
	hfStreams := make([]*tokenStream, f.numGroups)
	natural := naturalOrderDCT8()
	gb := f.groupDim / 8
	for g := 0; g < f.numGroups; g++ {
		ts := &tokenStream{numContexts: numAC}
		hfStreams[g] = ts
		gx, gy := g%f.xGroups, g/f.xGroups
		bx0, by0 := gx*gb, gy*gb
		bw := min(gb, f.xsizeBlocks-bx0)
		bh := min(gb, f.ysizeBlocks-by0)
		var nz [3][]int32
		for c := range nz {
			nz[c] = make([]int32, gb*gb)
		}
		for by := 0; by < bh; by++ {
			for bx := 0; bx < bw; bx++ {
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
					blockCtx := bctx.context(0, 1, 0, c)
					_, ac := coeff(c, (bx0>>hs)+sbx, (by0>>vs)+sby)
					nzeros := 0
					for k := 1; k < 64; k++ {
						if ac[natural[k]] != 0 {
							nzeros++
						}
					}
					ts.add(bctx.nonZeroContext(pred, blockCtx), uint32(nzeros))
					row[sbx] = int32(nzeros)
					histOff := bctx.zeroDensityOffset(blockCtx)
					prev := 1
					if nzeros > 4 {
						prev = 0
					}
					left := nzeros
					for k := 1; k < 64 && left != 0; k++ {
						v := ac[natural[k]]
						ts.add(histOff+zeroDensityCtx(left, k, prev), packSigned(v))
						if v != 0 {
							prev = 1
							left--
						} else {
							prev = 0
						}
					}
				}
			}
		}
	}
	hfEnc := buildEntropyEncoder(numAC, hfStreams...)

	// LF global section.
	lfg := &bitWriter{}
	lfg.bool(false) // LF dequantization
	for c := 0; c < 3; c++ {
		dq := float32(1) / (float32(255*8) / float32(qt[c][0]))
		lfg.f16(dq * 128)
	}
	lfg.u32(65536, bitsOff(11, 1), bitsOff(11, 2049), bitsOff(12, 4097), bitsOff(16, 8193))
	lfg.u32(1, val(16), bitsOff(5, 1), bitsOff(8, 1), bitsOff(16, 1))
	// Explicit block context map (the default map's values, written out).
	lfg.bool(false)
	for i := 0; i < 4; i++ {
		lfg.write(4, 0) // no LF or quant-field thresholds
	}
	writeContextMap(lfg, defaultBlockCtxMap, 15)
	lfg.bool(false)
	lfg.u32(defaultColourFactor, val(84), val(256), bitsOff(8, 2), bitsOff(16, 258))
	lfg.f16(0)
	lfg.f16(0)
	lfg.write(8, 128)
	lfg.write(8, 128)
	lfg.bool(true) // global tree
	writeTree(lfg, order)
	modEnc.writeHeader(lfg)

	lfSections := make([]*bitWriter, f.numLFGroups)
	for g, d := range lfData {
		w := &bitWriter{}
		w.write(2, 0) // LF extra precision
		writeGroupHeader(w, true, nil)
		modEnc.writeStream(w, d.dc.tokens.tokens)
		w.write(ceilLog2(uint64(d.count)), uint64(d.count-1))
		writeGroupHeader(w, true, nil)
		modEnc.writeStream(w, d.meta.tokens.tokens)
		lfSections[g] = w
	}

	hfg := &bitWriter{}
	hfg.bool(false) // custom dequantization matrices
	hfg.write(3, 7) // RAW table for DCT8
	hfg.f16(1.0 / (8 * 255))
	writeGroupHeader(hfg, true, nil)
	modEnc.writeStream(hfg, qStream.tokens.tokens)
	for i := 1; i < numQuantTables; i++ {
		hfg.write(3, 0) // library tables
	}
	hfg.write(ceilLog2(uint64(f.numGroups)), 0)         // one histogram set
	hfg.u32(0, val(0x5F), val(0x13), val(0), bitsN(13)) // natural orders
	hfEnc.writeHeader(hfg)

	passSections := make([]*bitWriter, f.numGroups)
	for g, ts := range hfStreams {
		w := &bitWriter{}
		hfEnc.writeStream(w, ts.tokens)
		passSections[g] = w
	}

	cs := &bitWriter{}
	cs.write(16, 0x0AFF)
	writeSizeHeader(cs, j.width, j.height)
	writeImageMetadata(cs, 8, isGray)
	cs.zeroPad()
	writeFrameHeader(cs, frameSpec{doYCbCr: doYCbCr, jpegUpsampling: f.jpegUpsampling, flags: flagSkipLFSm})
	var sections [][]byte
	if f.numGroups == 1 {
		all := &bitWriter{}
		all.appendWriter(lfg)
		for _, s := range lfSections {
			all.appendWriter(s)
		}
		all.appendWriter(hfg)
		for _, s := range passSections {
			all.appendWriter(s)
		}
		sections = [][]byte{all.bytes()}
	} else {
		sections = append(sections, lfg.bytes())
		for _, s := range lfSections {
			sections = append(sections, s.bytes())
		}
		sections = append(sections, hfg.bytes())
		for _, s := range passSections {
			sections = append(sections, s.bytes())
		}
	}
	writeFrame(cs, sections)
	if !reconstruct {
		return cs.bytes(), nil
	}
	jbrd, err := writeJPEGData(j)
	if err != nil {
		return nil, err
	}
	return wrapContainer(cs.bytes(), jbrd), nil
}

// writeJPEGData serializes the jbrd box (the inverse of readJPEGData).
func writeJPEGData(j *jpegData) ([]byte, error) {
	w := &bitWriter{}
	w.bool(len(j.components) == 1)
	for _, m := range j.markerOrder {
		w.write(6, uint64(m-0xC0))
	}
	for i, a := range j.appData {
		w.u32(j.appMarkerType[i], val(0), val(1), bitsOff(1, 2), bitsOff(2, 4))
		w.write(16, uint64(len(a)-1))
	}
	for _, c := range j.comData {
		w.write(16, uint64(len(c)-1))
	}
	if len(j.quant) < 1 || len(j.quant) > 3 {
		return nil, notReversible("%d quantization tables", len(j.quant))
	}
	w.u32(uint32(len(j.quant)), val(1), val(2), val(3), val(4))
	for _, q := range j.quant {
		w.write(1, uint64(q.precision))
		w.write(2, uint64(q.index))
		w.bool(q.isLast)
	}
	comps := j.components
	switch {
	case len(comps) == 1 && comps[0].id == 1:
		w.write(2, 0)
	case len(comps) == 3 && comps[0].id == 1 && comps[1].id == 2 && comps[2].id == 3:
		w.write(2, 1)
	case len(comps) == 3 && comps[0].id == 'R' && comps[1].id == 'G' && comps[2].id == 'B':
		w.write(2, 2)
	default:
		w.write(2, 3)
		w.u32(uint32(len(comps)), val(1), val(2), val(3), val(4))
		for _, c := range comps {
			w.write(8, uint64(c.id))
		}
	}
	for _, c := range comps {
		w.write(2, uint64(c.quantIdx))
	}
	w.u32(uint32(len(j.huffman)), val(4), bitsOff(3, 2), bitsOff(4, 10), bitsOff(6, 26))
	for _, h := range j.huffman {
		w.bool(h.slotID&0x10 != 0)
		w.write(2, uint64(h.slotID&3))
		w.bool(h.isLast)
		for _, c := range h.counts {
			w.u32(c, val(0), val(1), bitsOff(3, 2), bitsN(8))
		}
		for _, v := range h.values {
			w.u32(v, bitsN(2), bitsOff(2, 4), bitsOff(4, 8), bitsOff(8, 1))
		}
	}
	for _, s := range j.scans {
		w.u32(s.numComponents, val(1), val(2), val(3), val(4))
		w.write(6, uint64(s.ss))
		w.write(6, uint64(s.se))
		w.write(4, uint64(s.al))
		w.write(4, uint64(s.ah))
		for k := uint32(0); k < s.numComponents; k++ {
			c := s.components[k]
			w.write(2, uint64(c.compIdx))
			w.write(2, uint64(c.acTbl))
			w.write(2, uint64(c.dcTbl))
		}
		w.u32(0, val(0), val(1), val(2), bitsOff(3, 3))
	}
	if containsMarker(j.markerOrder, 0xDD) {
		w.write(16, uint64(j.restartInterval))
	}
	for _, s := range j.scans {
		w.u32(uint32(len(s.resetPoints)), val(0), bitsOff(2, 1), bitsOff(4, 4), bitsOff(16, 20))
		last := int64(-1)
		for _, r := range s.resetPoints {
			w.u32(uint32(int64(r)-last-1), val(0), bitsOff(3, 1), bitsOff(5, 9), bitsOff(28, 41))
			last = int64(r)
		}
		w.u32(uint32(len(s.extraZeroRuns)), val(0), bitsOff(2, 1), bitsOff(4, 4), bitsOff(16, 20))
		last = -1
		for _, z := range s.extraZeroRuns {
			w.u32(z.numRuns, val(1), bitsOff(2, 2), bitsOff(4, 5), bitsOff(8, 20))
			w.u32(uint32(int64(z.blockIdx)-last-1), val(0), bitsOff(3, 1), bitsOff(5, 9), bitsOff(28, 41))
			last = int64(z.blockIdx)
		}
	}
	for _, d := range j.interMarker {
		if len(d) > 0xFFFF {
			return nil, notReversible("inter-marker data too long")
		}
		w.write(16, uint64(len(d)))
	}
	if len(j.tailData) > 4260096 {
		return nil, notReversible("tail data too long")
	}
	w.u32(uint32(len(j.tailData)), val(0), bitsOff(8, 1), bitsOff(16, 257), bitsOff(22, 65793))
	w.bool(j.hasZeroPadding)
	if j.hasZeroPadding {
		w.write(24, uint64(len(j.paddingBits)))
		for _, b := range j.paddingBits {
			w.write(1, uint64(b))
		}
	}
	var plain []byte
	for i, a := range j.appData {
		if j.appMarkerType[i] == appUnknown {
			plain = append(plain, a...)
		}
	}
	for _, c := range j.comData {
		plain = append(plain, c...)
	}
	for _, d := range j.interMarker {
		plain = append(plain, d...)
	}
	plain = append(plain, j.tailData...)
	return append(w.bytes(), brotli.EncodeStored(plain)...), nil
}
