package video

// HEVC (ITU-T H.265 | ISO/IEC 23008-2) parameter sets, slice segment headers
// and picture order counts (clause 8.3.1), as far as MP4 muxing needs them.

const (
	hevcNALRADLN    = 6
	hevcNALRASLR    = 9
	hevcNALRASLN    = 8
	hevcNALBLAWLP   = 16
	hevcNALIDRWRADL = 19
	hevcNALIDRNLP   = 20
	hevcNALCRA      = 21
	hevcNALIRAPMax  = 23
	hevcNALVPS      = 32
	hevcNALSPS      = 33
	hevcNALPPS      = 34
	hevcNALAUD      = 35
	hevcNALEOS      = 36
	hevcNALEOB      = 37
	hevcNALFD       = 38
	hevcNALSEIPre   = 39
	hevcNALSEISuf   = 40
)

func hevcType(nal []byte) int { return int(nal[0]>>1) & 0x3F }

func hevcTemporalID(nal []byte) int { return int(nal[1]&7) - 1 }

func hevcIRAP(t int) bool { return t >= hevcNALBLAWLP && t <= hevcNALIRAPMax }

func hevcIDR(t int) bool { return t == hevcNALIDRWRADL || t == hevcNALIDRNLP }

func hevcRASL(t int) bool { return t == hevcNALRASLN || t == hevcNALRASLR }

type hevcSPS struct {
	id                  uint32
	raw                 []byte
	ptl                 [12]byte // general profile_tier_level bytes
	maxSubLayers        int
	temporalIDNesting   bool
	chromaFormat        uint32
	separateColourPlane bool
	bitDepthLuma        uint32
	bitDepthChroma      uint32
	log2MaxPocLsb       uint32
	width, height       int
}

type hevcPPS struct {
	id, spsID               uint32
	raw                     []byte
	outputFlagPresent       bool
	numExtraSliceHeaderBits int
}

func parseHEVCSPS(nal []byte) (*hevcSPS, error) {
	if len(nal) < 16 {
		return nil, malformed("HEVC SPS too short")
	}
	r := newBitReader(rbsp(nal[2:]))
	s := &hevcSPS{raw: nal}
	r.skip(4) // sps_video_parameter_set_id
	maxSubLayersMinus1 := int(r.u(3))
	s.maxSubLayers = maxSubLayersMinus1 + 1
	s.temporalIDNesting = r.flag()
	for i := range s.ptl {
		s.ptl[i] = byte(r.u(8))
	}
	var profilePresent, levelPresent [8]bool
	for i := 0; i < maxSubLayersMinus1; i++ {
		profilePresent[i], levelPresent[i] = r.flag(), r.flag()
	}
	if maxSubLayersMinus1 > 0 {
		r.skip(2 * (8 - maxSubLayersMinus1))
	}
	for i := 0; i < maxSubLayersMinus1; i++ {
		if profilePresent[i] {
			r.skip(88)
		}
		if levelPresent[i] {
			r.skip(8)
		}
	}
	s.id = r.ueMax(15, "sps_seq_parameter_set_id")
	s.chromaFormat = r.ueMax(3, "chroma_format_idc")
	if s.chromaFormat == 3 {
		s.separateColourPlane = r.flag()
	}
	width := int(r.ueMax(1<<16, "pic_width_in_luma_samples"))
	height := int(r.ueMax(1<<16, "pic_height_in_luma_samples"))
	if r.flag() { // conformance_window_flag
		left, right, top, bottom := r.ue(), r.ue(), r.ue(), r.ue()
		subW, subH := 1, 1
		if !s.separateColourPlane {
			switch s.chromaFormat {
			case 1:
				subW, subH = 2, 2
			case 2:
				subW = 2
			}
		}
		width -= int(left+right) * subW
		height -= int(top+bottom) * subH
	}
	s.bitDepthLuma = r.ueMax(8, "bit_depth_luma_minus8") + 8
	s.bitDepthChroma = r.ueMax(8, "bit_depth_chroma_minus8") + 8
	s.log2MaxPocLsb = r.ueMax(12, "log2_max_pic_order_cnt_lsb_minus4") + 4
	if r.err != nil {
		return nil, r.err
	}
	if width <= 0 || height <= 0 {
		return nil, malformed("HEVC SPS picture size %dx%d", width, height)
	}
	s.width, s.height = width, height
	return s, nil
}

func parseHEVCPPS(nal []byte) (*hevcPPS, error) {
	r := newBitReader(rbsp(nal[2:]))
	p := &hevcPPS{raw: nal}
	p.id = r.ueMax(63, "pps_pic_parameter_set_id")
	p.spsID = r.ueMax(15, "pps_seq_parameter_set_id")
	r.skip(1) // dependent_slice_segments_enabled_flag
	p.outputFlagPresent = r.flag()
	p.numExtraSliceHeaderBits = int(r.u(3))
	if r.err != nil {
		return nil, r.err
	}
	return p, nil
}

// hevcSlice is the part of a first slice segment header that orders
// pictures.
type hevcSlice struct {
	nalType    int
	temporalID int
	pocLsb     uint32
	sps        *hevcSPS
}

func parseHEVCSlice(nal []byte, spss map[uint32]*hevcSPS, ppss map[uint32]*hevcPPS) (*hevcSlice, error) {
	t := hevcType(nal)
	r := newBitReader(rbsp(nal[2:min(len(nal), 64)]))
	sl := &hevcSlice{nalType: t, temporalID: hevcTemporalID(nal)}
	if sl.temporalID < 0 {
		return nil, malformed("HEVC nuh_temporal_id_plus1 is 0")
	}
	r.skip(1) // first_slice_segment_in_pic_flag (set: checked by the caller)
	if hevcIRAP(t) {
		r.skip(1) // no_output_of_prior_pics_flag
	}
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
	r.skip(pps.numExtraSliceHeaderBits)
	r.ue() // slice_type
	if pps.outputFlagPresent {
		r.skip(1)
	}
	if sps.separateColourPlane {
		r.skip(2)
	}
	if !hevcIDR(t) {
		sl.pocLsb = r.u(int(sps.log2MaxPocLsb))
	}
	if r.err != nil {
		return nil, r.err
	}
	return sl, nil
}

// hevcPOC derives picture order counts in decoding order (clause 8.3.1).
type hevcPOC struct {
	prevMsb, prevLsb int64
	afterEOS         bool
	started          bool
}

// next returns the picture's order count and whether it starts a new coded
// video sequence (an IRAP picture with NoRaslOutputFlag, whose RASL
// pictures are not output).
func (p *hevcPOC) next(sl *hevcSlice) (int64, bool) {
	maxLsb := int64(1) << sl.sps.log2MaxPocLsb
	lsb := int64(sl.pocLsb)
	t := sl.nalType
	noRaslOutput := hevcIRAP(t) && (hevcIDR(t) || t < hevcNALIDRWRADL || !p.started || p.afterEOS)
	p.started, p.afterEOS = true, false
	var msb int64
	if !noRaslOutput {
		msb = p.prevMsb
		switch {
		case lsb < p.prevLsb && p.prevLsb-lsb >= maxLsb/2:
			msb += maxLsb
		case lsb > p.prevLsb && lsb-p.prevLsb > maxLsb/2:
			msb -= maxLsb
		}
	}
	subLayerNonRef := t <= 14 && t%2 == 0
	if sl.temporalID == 0 && !hevcRASL(t) && t != hevcNALRADLN && t != hevcNALRADLN+1 && !subLayerNonRef {
		p.prevMsb, p.prevLsb = msb, lsb
	}
	return msb + lsb, noRaslOutput
}

// hevcCodecString is the RFC 6381 codecs parameter of ISO/IEC 14496-15
// Annex E, for example "hvc1.1.6.L93.B0".
func hevcCodecString(entry string, ptl [12]byte) string {
	space := ptl[0] >> 6
	tier := ptl[0] >> 5 & 1
	profile := ptl[0] & 0x1F
	compat := uint32(ptl[1])<<24 | uint32(ptl[2])<<16 | uint32(ptl[3])<<8 | uint32(ptl[4])
	var reversed uint32
	for i := 0; i < 32; i++ {
		reversed = reversed<<1 | compat>>i&1
	}
	s := entry + "."
	if space > 0 {
		s += string(rune('A' + space - 1))
	}
	s += itoa(int(profile)) + "." + hexUpper(uint64(reversed))
	if tier == 1 {
		s += ".H"
	} else {
		s += ".L"
	}
	s += itoa(int(ptl[11]))
	constraints := ptl[5:11]
	last := len(constraints)
	for last > 0 && constraints[last-1] == 0 {
		last--
	}
	for _, b := range constraints[:last] {
		s += "." + hexUpper(uint64(b))
	}
	return s
}
