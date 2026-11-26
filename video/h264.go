package video

// H.264 (ITU-T H.264 | ISO/IEC 14496-10) parameter sets, slice headers and
// picture order counts (clause 8.2.1), as far as MP4 muxing needs them.

const (
	h264NALSlice    = 1
	h264NALIDR      = 5
	h264NALSEI      = 6
	h264NALSPS      = 7
	h264NALPPS      = 8
	h264NALAUD      = 9
	h264NALEndSeq   = 10
	h264NALEndStrm  = 11
	h264NALFiller   = 12
	h264NALSPSExt   = 13
	h264NALPrefix   = 14
	h264NALSubsetSP = 15
)

type h264SPS struct {
	id                      uint32
	raw                     []byte
	profile, compat, level  byte
	chromaFormat            uint32
	separateColourPlane     bool
	bitDepthLuma            uint32
	bitDepthChroma          uint32
	log2MaxFrameNum         uint32
	pocType                 uint32
	log2MaxPocLsb           uint32
	deltaPicOrderAlwaysZero bool
	offsetForNonRefPic      int32
	offsetForTopToBottomFld int32
	offsetForRefFrame       []int32
	frameMbsOnly            bool
	width, height           int
	// vuiBitOffset records metadata position without consuming VUI or changing
	// the muxer's established SPS parsing/POC behavior.
	vuiBitOffset int
}

type h264PPS struct {
	id, spsID                  uint32
	raw                        []byte
	bottomFieldPicOrderPresent bool
}

func h264HighProfile(p byte) bool {
	switch p {
	case 100, 110, 122, 244, 44, 83, 86, 118, 128, 138, 139, 134, 135:
		return true
	}
	return false
}

func skipScalingList(r *bitReader, size int) {
	last, next := int32(8), int32(8)
	for j := 0; j < size; j++ {
		if next != 0 {
			next = (last + r.se() + 256) % 256
		}
		if next != 0 {
			last = next
		}
	}
}

func parseH264SPS(nal []byte) (*h264SPS, error) {
	if len(nal) < 4 {
		return nil, malformed("H.264 SPS too short")
	}
	r := newBitReader(rbsp(nal[1:]))
	s := &h264SPS{raw: nal, chromaFormat: 1, bitDepthLuma: 8, bitDepthChroma: 8}
	s.profile, s.compat, s.level = byte(r.u(8)), byte(r.u(8)), byte(r.u(8))
	s.id = r.ueMax(31, "seq_parameter_set_id")
	if h264HighProfile(s.profile) {
		s.chromaFormat = r.ueMax(3, "chroma_format_idc")
		if s.chromaFormat == 3 {
			s.separateColourPlane = r.flag()
		}
		s.bitDepthLuma = r.ueMax(6, "bit_depth_luma_minus8") + 8
		s.bitDepthChroma = r.ueMax(6, "bit_depth_chroma_minus8") + 8
		r.skip(1) // qpprime_y_zero_transform_bypass_flag
		if r.flag() {
			n := 8
			if s.chromaFormat == 3 {
				n = 12
			}
			for i := 0; i < n; i++ {
				if r.flag() {
					size := 16
					if i >= 6 {
						size = 64
					}
					skipScalingList(r, size)
				}
			}
		}
	}
	s.log2MaxFrameNum = r.ueMax(12, "log2_max_frame_num_minus4") + 4
	s.pocType = r.ueMax(2, "pic_order_cnt_type")
	switch s.pocType {
	case 0:
		s.log2MaxPocLsb = r.ueMax(12, "log2_max_pic_order_cnt_lsb_minus4") + 4
	case 1:
		s.deltaPicOrderAlwaysZero = r.flag()
		s.offsetForNonRefPic = r.se()
		s.offsetForTopToBottomFld = r.se()
		n := r.ueMax(255, "num_ref_frames_in_pic_order_cnt_cycle")
		for i := uint32(0); i < n && r.err == nil; i++ {
			s.offsetForRefFrame = append(s.offsetForRefFrame, r.se())
		}
	}
	r.ue() // max_num_ref_frames
	r.skip(1)
	widthMbs := r.ueMax(1<<16, "pic_width_in_mbs_minus1") + 1
	heightUnits := r.ueMax(1<<16, "pic_height_in_map_units_minus1") + 1
	s.frameMbsOnly = r.flag()
	if !s.frameMbsOnly {
		r.skip(1) // mb_adaptive_frame_field_flag
	}
	r.skip(1) // direct_8x8_inference_flag
	frameHeightMul := uint32(2)
	if s.frameMbsOnly {
		frameHeightMul = 1
	}
	width := int(widthMbs) * 16
	height := int(frameHeightMul*heightUnits) * 16
	if r.flag() { // frame_cropping_flag
		left, right, top, bottom := r.ue(), r.ue(), r.ue(), r.ue()
		cropX, cropY := 1, int(frameHeightMul)
		if s.chromaFormat != 0 && !s.separateColourPlane {
			subW, subH := 2, 2
			switch s.chromaFormat {
			case 2:
				subH = 1
			case 3:
				subW, subH = 1, 1
			}
			cropX, cropY = subW, subH*int(frameHeightMul)
		}
		width -= int(left+right) * cropX
		height -= int(top+bottom) * cropY
	}
	if r.err != nil {
		return nil, r.err
	}
	if width <= 0 || height <= 0 || width >= 1<<16 || height >= 1<<16 {
		return nil, malformed("H.264 SPS picture size %dx%d", width, height)
	}
	s.width, s.height = width, height
	s.vuiBitOffset = r.pos
	return s, nil
}

func parseH264PPS(nal []byte) (*h264PPS, error) {
	r := newBitReader(rbsp(nal[1:]))
	p := &h264PPS{raw: nal}
	p.id = r.ueMax(255, "pic_parameter_set_id")
	p.spsID = r.ueMax(31, "seq_parameter_set_id")
	r.skip(1) // entropy_coding_mode_flag
	p.bottomFieldPicOrderPresent = r.flag()
	if r.err != nil {
		return nil, r.err
	}
	return p, nil
}

// h264Slice is the part of a slice header that orders pictures.
type h264Slice struct {
	firstMb       uint32
	frameNum      uint32
	idr           bool
	refIdc        uint32
	pocLsb        uint32
	deltaBottom   int32
	deltaPicOrder [2]int32
	sps           *h264SPS
}

func parseH264Slice(nal []byte, spss map[uint32]*h264SPS, ppss map[uint32]*h264PPS) (*h264Slice, error) {
	r := newBitReader(rbsp(nal[1:min(len(nal), 64)]))
	sl := &h264Slice{idr: nal[0]&0x1F == h264NALIDR, refIdc: uint32(nal[0]>>5) & 3}
	sl.firstMb = r.ue()
	r.ue() // slice_type
	ppsID := r.ue()
	if r.err != nil {
		return nil, r.err
	}
	pps, ok := ppss[ppsID]
	if !ok {
		return nil, malformed("slice refers to missing PPS %d", ppsID)
	}
	sps, ok := spss[pps.spsID]
	if !ok {
		return nil, malformed("PPS %d refers to missing SPS %d", ppsID, pps.spsID)
	}
	sl.sps = sps
	if sps.separateColourPlane {
		r.skip(2)
	}
	sl.frameNum = r.u(int(sps.log2MaxFrameNum))
	if !sps.frameMbsOnly && r.flag() {
		return nil, unsupportedStream("H.264 field pictures (interlaced coding)")
	}
	if sl.idr {
		r.ue() // idr_pic_id
	}
	switch sps.pocType {
	case 0:
		sl.pocLsb = r.u(int(sps.log2MaxPocLsb))
		if pps.bottomFieldPicOrderPresent {
			sl.deltaBottom = r.se()
		}
	case 1:
		if !sps.deltaPicOrderAlwaysZero {
			sl.deltaPicOrder[0] = r.se()
			if pps.bottomFieldPicOrderPresent {
				sl.deltaPicOrder[1] = r.se()
			}
		}
	}
	if r.err != nil {
		return nil, r.err
	}
	return sl, nil
}

// h264POC derives picture order counts in decoding order (clause 8.2.1).
// Memory management operation 5 is not tracked.
type h264POC struct {
	prevMsb, prevLsb   int64
	prevFrameNum       uint32
	prevFrameNumOffset int64
}

func (p *h264POC) next(sl *h264Slice) int64 {
	sps := sl.sps
	maxFrameNum := int64(1) << sps.log2MaxFrameNum
	frameNumOffset := func() int64 {
		switch {
		case sl.idr:
			return 0
		case p.prevFrameNum > sl.frameNum:
			return p.prevFrameNumOffset + maxFrameNum
		}
		return p.prevFrameNumOffset
	}
	var poc int64
	switch sps.pocType {
	case 0:
		if sl.idr {
			p.prevMsb, p.prevLsb = 0, 0
		}
		maxLsb := int64(1) << sps.log2MaxPocLsb
		lsb := int64(sl.pocLsb)
		msb := p.prevMsb
		switch {
		case lsb < p.prevLsb && p.prevLsb-lsb >= maxLsb/2:
			msb += maxLsb
		case lsb > p.prevLsb && lsb-p.prevLsb > maxLsb/2:
			msb -= maxLsb
		}
		top := msb + lsb
		poc = min(top, top+int64(sl.deltaBottom))
		if sl.refIdc != 0 {
			p.prevMsb, p.prevLsb = msb, lsb
		}
	case 1:
		offset := frameNumOffset()
		n := int64(len(sps.offsetForRefFrame))
		abs := int64(0)
		if n != 0 {
			abs = offset + int64(sl.frameNum)
		}
		if sl.refIdc == 0 && abs > 0 {
			abs--
		}
		var expected int64
		if abs > 0 {
			var perCycle int64
			for _, d := range sps.offsetForRefFrame {
				perCycle += int64(d)
			}
			cycle, inCycle := (abs-1)/n, (abs-1)%n
			expected = cycle * perCycle
			for i := int64(0); i <= inCycle; i++ {
				expected += int64(sps.offsetForRefFrame[i])
			}
		}
		if sl.refIdc == 0 {
			expected += int64(sps.offsetForNonRefPic)
		}
		top := expected + int64(sl.deltaPicOrder[0])
		bottom := top + int64(sps.offsetForTopToBottomFld) + int64(sl.deltaPicOrder[1])
		poc = min(top, bottom)
		p.prevFrameNumOffset = offset
	default:
		offset := frameNumOffset()
		switch {
		case sl.idr:
			poc = 0
		case sl.refIdc == 0:
			poc = 2*(offset+int64(sl.frameNum)) - 1
		default:
			poc = 2 * (offset + int64(sl.frameNum))
		}
		p.prevFrameNumOffset = offset
	}
	p.prevFrameNum = sl.frameNum
	return poc
}
