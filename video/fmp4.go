package video

import (
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"math"
	"sort"
	"strconv"
	"strings"
)

// Fragmented MP4 (ISO/IEC 14496-12 movie fragments with ISO/IEC 14496-15
// AVC/HEVC sample entries) muxing of Annex B elementary streams.

// ErrFrameRateRequired reports a mux request without a positive frame rate.
var ErrFrameRateRequired = errors.New("dicom: video frame rate is required for MP4 muxing")

// DefaultMaxSamplesPerFragment bounds a movie fragment when key frames are
// sparse.
const DefaultMaxSamplesPerFragment = 300

// MuxOptions configures MuxFragmentedMP4.
type MuxOptions struct {
	// Codec is CodecH264 or CodecHEVC.
	Codec Codec
	// FrameRate in frames per second; required. Samples get a constant
	// duration of 1/FrameRate.
	FrameRate float64
	// MaxSamplesPerFragment caps a fragment; zero selects
	// DefaultMaxSamplesPerFragment. A new fragment also starts at every key
	// frame.
	MaxSamplesPerFragment int
	// Limits bounds the input stream (MaxBytes) and its NAL unit count
	// (MaxFragments); zero fields select the package defaults.
	Limits Limits
}

// MuxInfo describes a muxed stream.
type MuxInfo struct {
	Codec Codec
	// CodecString is the RFC 6381 codecs parameter, for example
	// "avc1.64001F" or "hvc1.1.6.L93.B0", for a MIME type such as
	// `video/mp4; codecs="avc1.64001F"`.
	CodecString string
	// SampleEntry is avc1/hvc1 (parameter sets in the sample entry) or
	// avc3/hev1 when the stream changes parameter sets and keeps them in band.
	SampleEntry   string
	Width, Height int
	Samples       int
	KeyFrames     int
	Fragments     int
	// DroppedLeading counts pictures that cannot be decoded and were left
	// out: those before the first key frame, and RASL pictures of a CRA or
	// BLA picture that starts a coded video sequence.
	DroppedLeading int
	// Timescale and SampleDuration give the media timing: each sample lasts
	// SampleDuration/Timescale seconds.
	Timescale      uint32
	SampleDuration uint32
	// Reordered reports that presentation order differs from decoding order
	// (B-frames); composition offsets carry the difference.
	Reordered bool
	Size      int64
}

type muxSample struct {
	nals []([]byte)
	sync bool
	poc  int64
	cvs  int // coded video sequence: POC restarts at each
	pres int // presentation index
}

type paramSet struct {
	id  uint32
	raw []byte
}

type muxStream struct {
	codec    Codec
	samples  []muxSample
	inBand   bool
	sps, pps []paramSet // first set of each id, in order of appearance
	vps      []paramSet
	width    int
	height   int
	h264     *h264SPS
	hevc     *hevcSPS
	dropped  int
}

// MuxFragmentedMP4 writes an H.264 or HEVC Annex B elementary stream as a
// fragmented MP4 file (ftyp, moov with mvex, then one moof+mdat per
// fragment) to dst. Access units are delimited from the NAL units, the
// presentation order is derived from each picture's order count, so B-frame
// streams play in the right order (version 1 track runs with signed
// composition offsets), and every fragment begins at a key frame unless it
// would exceed MaxSamplesPerFragment. Interlaced field pictures and H.264
// memory management operation 5 are not supported.
func MuxFragmentedMP4(ctx context.Context, dst io.Writer, stream []byte, opts MuxOptions) (MuxInfo, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	if dst == nil {
		return MuxInfo{}, errors.New("dicom: video destination is nil")
	}
	if opts.Codec != CodecH264 && opts.Codec != CodecHEVC {
		return MuxInfo{}, fmt.Errorf("%w: MP4 muxing supports H.264 and HEVC, not %q", ErrUnsupportedStream, opts.Codec)
	}
	if !(opts.FrameRate > 0) || math.IsInf(opts.FrameRate, 0) || opts.FrameRate > 1000 {
		return MuxInfo{}, ErrFrameRateRequired
	}
	limits := opts.Limits.normalized()
	if int64(len(stream)) > limits.MaxBytes {
		return MuxInfo{}, fmt.Errorf("%w: limit %d bytes", ErrMediaTooLarge, limits.MaxBytes)
	}
	perFragment := opts.MaxSamplesPerFragment
	if perFragment <= 0 {
		perFragment = DefaultMaxSamplesPerFragment
	}
	nals, err := splitAnnexB(stream, limits.MaxFragments)
	if err != nil {
		return MuxInfo{}, err
	}
	var s *muxStream
	if opts.Codec == CodecH264 {
		s, err = analyzeH264(ctx, nals)
	} else {
		s, err = analyzeHEVC(ctx, nals)
	}
	if err != nil {
		return MuxInfo{}, err
	}
	if len(s.samples) == 0 {
		return MuxInfo{}, malformed("no decodable pictures (no key frame)")
	}
	if err := s.checkConfig(); err != nil {
		return MuxInfo{}, err
	}
	reordered := s.orderPresentation()
	timescale, duration := mediaTiming(opts.FrameRate)
	info := MuxInfo{
		Codec: opts.Codec, Width: s.width, Height: s.height, Samples: len(s.samples),
		DroppedLeading: s.dropped, Timescale: timescale, SampleDuration: duration, Reordered: reordered,
	}
	for _, sm := range s.samples {
		if sm.sync {
			info.KeyFrames++
		}
	}
	info.SampleEntry, info.CodecString = s.sampleEntry()
	w := &countingWriter{dst: dst}
	if _, err := w.Write(s.initSegment(info)); err != nil {
		return info, err
	}
	start := 0
	for start < len(s.samples) {
		if err := ctx.Err(); err != nil {
			return info, err
		}
		end := start + 1
		for end < len(s.samples) && end-start < perFragment && !s.samples[end].sync {
			end++
		}
		info.Fragments++
		if err := s.writeFragment(w, uint32(info.Fragments), start, end, duration); err != nil {
			return info, err
		}
		start = end
	}
	info.Size = w.n
	return info, nil
}

// mediaTiming picks an integer timescale and sample duration for a frame
// rate: NTSC rates (N*1000/1001) use 1001-tick samples, rates dividing 90 kHz
// use the 90 kHz clock, and others a millisecond-precision rate.
func mediaTiming(rate float64) (uint32, uint32) {
	if n := math.Round(rate * 1.001); n >= 1 && math.Abs(rate*1.001-n) < 1e-3 && math.Abs(rate-n) > 1e-3 {
		return uint32(n) * 1000, 1001
	}
	if d := 90000 / rate; math.Abs(d-math.Round(d)) < 1e-6 {
		return 90000, uint32(math.Round(d))
	}
	return uint32(math.Round(rate * 1000)), 1000
}

// orderPresentation assigns presentation indexes: within each coded video
// sequence pictures are output in increasing order count.
func (s *muxStream) orderPresentation() bool {
	reordered := false
	for i := 0; i < len(s.samples); {
		j := i + 1
		for j < len(s.samples) && s.samples[j].cvs == s.samples[i].cvs {
			j++
		}
		idx := make([]int, j-i)
		for k := range idx {
			idx[k] = i + k
		}
		sort.SliceStable(idx, func(a, b int) bool { return s.samples[idx[a]].poc < s.samples[idx[b]].poc })
		for rank, k := range idx {
			s.samples[k].pres = i + rank
			if k != i+rank {
				reordered = true
			}
		}
		i = j
	}
	return reordered
}

func addParamSet(list []paramSet, id uint32, raw []byte) ([]paramSet, bool) {
	for _, p := range list {
		if p.id == id {
			return list, bytes.Equal(p.raw, raw)
		}
	}
	return append(list, paramSet{id: id, raw: raw}), true
}

func analyzeH264(ctx context.Context, nals [][]byte) (*muxStream, error) {
	s := &muxStream{codec: CodecH264}
	spss := map[uint32]*h264SPS{}
	ppss := map[uint32]*h264PPS{}
	var poc h264POC
	var cur *muxSample
	var pending [][]byte // non-VCL units before the next picture
	started := false
	cvs := -1
	flush := func() {
		if cur != nil {
			s.samples = append(s.samples, *cur)
			cur = nil
		}
	}
	for i, nal := range nals {
		if i%4096 == 0 {
			if err := ctx.Err(); err != nil {
				return nil, err
			}
		}
		if nal[0]&0x80 != 0 {
			return nil, malformed("forbidden_zero_bit set")
		}
		t := int(nal[0] & 0x1F)
		switch t {
		case h264NALSPS:
			sps, err := parseH264SPS(nal)
			if err != nil {
				return nil, err
			}
			spss[sps.id] = sps
			var same bool
			if s.sps, same = addParamSet(s.sps, sps.id, nal); !same {
				s.inBand = true
			}
			if s.h264 == nil {
				s.h264, s.width, s.height = sps, sps.width, sps.height
			}
		case h264NALPPS:
			pps, err := parseH264PPS(nal)
			if err != nil {
				return nil, err
			}
			ppss[pps.id] = pps
			var same bool
			if s.pps, same = addParamSet(s.pps, pps.id, nal); !same {
				s.inBand = true
			}
		}
		if t == h264NALSlice || t == h264NALIDR {
			sl, err := parseH264Slice(nal, spss, ppss)
			if err != nil {
				return nil, err
			}
			if sl.firstMb == 0 || cur == nil {
				flush()
				if sl.idr {
					cvs++
					started = true
				}
				if !started {
					s.dropped++
					pending = nil
					cur = nil
					continue
				}
				cur = &muxSample{sync: sl.idr, poc: poc.next(sl), cvs: cvs}
				cur.nals = append(cur.nals, pending...)
				pending = nil
			} else if !started {
				continue
			}
			cur.nals = append(cur.nals, nal)
			continue
		}
		// A non-VCL unit after a picture's slices starts the next access unit.
		switch t {
		case h264NALAUD, h264NALSPS, h264NALPPS, h264NALSEI, h264NALPrefix, h264NALSubsetSP, 16, 17, 18:
			flush()
		}
		switch t {
		case h264NALAUD, h264NALEndSeq, h264NALEndStrm, h264NALFiller:
			// not carried in samples
		case h264NALSPS, h264NALPPS:
			pending = append(pending, nal) // kept only with in-band parameter sets
		default:
			if cur != nil {
				cur.nals = append(cur.nals, nal)
			} else {
				pending = append(pending, nal)
			}
		}
	}
	flush()
	if len(s.sps) == 0 || len(s.pps) == 0 {
		return nil, malformed("H.264 stream has no SPS or PPS")
	}
	if !s.inBand {
		s.stripParamSets(func(nal []byte) bool { t := nal[0] & 0x1F; return t == h264NALSPS || t == h264NALPPS })
	}
	return s, nil
}

func analyzeHEVC(ctx context.Context, nals [][]byte) (*muxStream, error) {
	s := &muxStream{codec: CodecHEVC}
	spss := map[uint32]*hevcSPS{}
	ppss := map[uint32]*hevcPPS{}
	var poc hevcPOC
	var cur *muxSample
	var pending [][]byte
	cvs := -1
	started, skipRASL := false, false
	flush := func() {
		if cur != nil {
			s.samples = append(s.samples, *cur)
			cur = nil
		}
	}
	dropping := false // the current picture is being dropped
	for i, nal := range nals {
		if i%4096 == 0 {
			if err := ctx.Err(); err != nil {
				return nil, err
			}
		}
		if len(nal) < 3 || nal[0]&0x80 != 0 {
			return nil, malformed("HEVC NAL unit header")
		}
		t := hevcType(nal)
		switch t {
		case hevcNALVPS:
			if len(nal) < 3 {
				return nil, malformed("HEVC VPS too short")
			}
			id := uint32(nal[2] >> 4)
			var same bool
			if s.vps, same = addParamSet(s.vps, id, nal); !same {
				s.inBand = true
			}
		case hevcNALSPS:
			sps, err := parseHEVCSPS(nal)
			if err != nil {
				return nil, err
			}
			spss[sps.id] = sps
			var same bool
			if s.sps, same = addParamSet(s.sps, sps.id, nal); !same {
				s.inBand = true
			}
			if s.hevc == nil {
				s.hevc, s.width, s.height = sps, sps.width, sps.height
			}
		case hevcNALPPS:
			pps, err := parseHEVCPPS(nal)
			if err != nil {
				return nil, err
			}
			ppss[pps.id] = pps
			var same bool
			if s.pps, same = addParamSet(s.pps, pps.id, nal); !same {
				s.inBand = true
			}
		case hevcNALEOS:
			poc.afterEOS = true
		}
		if t < 32 { // VCL
			if len(nal) < 3 {
				return nil, malformed("HEVC slice too short")
			}
			first := nal[2]&0x80 != 0
			if first || (cur == nil && !dropping) {
				flush()
				dropping = false
				if !first {
					return nil, malformed("HEVC picture without a first slice segment")
				}
				sl, err := parseHEVCSlice(nal, spss, ppss)
				if err != nil {
					return nil, err
				}
				if hevcIRAP(t) {
					started = true
				}
				if !started || (skipRASL && hevcRASL(t)) {
					s.dropped++
					dropping = true
					pending = nil
					continue
				}
				p, newCVS := poc.next(sl)
				if hevcIRAP(t) {
					skipRASL = newCVS && !hevcIDR(t)
				}
				if newCVS {
					cvs++
				}
				cur = &muxSample{sync: hevcIRAP(t), poc: p, cvs: cvs}
				cur.nals = append(cur.nals, pending...)
				pending = nil
			}
			if dropping {
				continue
			}
			cur.nals = append(cur.nals, nal)
			continue
		}
		switch {
		case t >= hevcNALVPS && t <= hevcNALAUD, t == hevcNALSEIPre, t >= 41 && t <= 44, t >= 48 && t <= 55:
			flush()
			dropping = false
		}
		switch t {
		case hevcNALAUD, hevcNALEOS, hevcNALEOB, hevcNALFD:
		case hevcNALVPS, hevcNALSPS, hevcNALPPS:
			pending = append(pending, nal)
		default:
			if cur != nil {
				cur.nals = append(cur.nals, nal)
			} else if !dropping {
				pending = append(pending, nal)
			}
		}
	}
	flush()
	if len(s.vps) == 0 || len(s.sps) == 0 || len(s.pps) == 0 {
		return nil, malformed("HEVC stream has no VPS, SPS or PPS")
	}
	if !s.inBand {
		s.stripParamSets(func(nal []byte) bool { t := hevcType(nal); return t >= hevcNALVPS && t <= hevcNALPPS })
	}
	return s, nil
}

// checkConfig verifies that the parameter sets and picture size fit the
// decoder configuration record and visual sample entry fields.
func (s *muxStream) checkConfig() error {
	if s.width > math.MaxUint16 || s.height > math.MaxUint16 {
		return unsupportedStream("picture size %dx%d", s.width, s.height)
	}
	if s.codec == CodecH264 && (len(s.sps) > 31 || len(s.pps) > 255) {
		return unsupportedStream("%d SPS and %d PPS exceed the avcC record", len(s.sps), len(s.pps))
	}
	for _, list := range [][]paramSet{s.vps, s.sps, s.pps} {
		for _, p := range list {
			if len(p.raw) > math.MaxUint16 {
				return unsupportedStream("parameter set of %d bytes", len(p.raw))
			}
		}
	}
	return nil
}

func (s *muxStream) stripParamSets(isParamSet func([]byte) bool) {
	for i := range s.samples {
		kept := s.samples[i].nals[:0:0]
		for _, nal := range s.samples[i].nals {
			if !isParamSet(nal) {
				kept = append(kept, nal)
			}
		}
		s.samples[i].nals = kept
	}
}

func (s *muxStream) sampleEntry() (entry, codec string) {
	if s.codec == CodecH264 {
		entry = "avc1"
		if s.inBand {
			entry = "avc3"
		}
		sps := s.h264
		return entry, fmt.Sprintf("%s.%02X%02X%02X", entry, sps.profile, sps.compat, sps.level)
	}
	entry = "hvc1"
	if s.inBand {
		entry = "hev1"
	}
	return entry, hevcCodecString(entry, s.hevc.ptl)
}

func itoa(v int) string { return strconv.Itoa(v) }

func hexUpper(v uint64) string { return strings.ToUpper(strconv.FormatUint(v, 16)) }

// --- box writing ---

type boxWriter struct{ b []byte }

func (w *boxWriter) u8(v uint8)   { w.b = append(w.b, v) }
func (w *boxWriter) u16(v uint16) { w.b = binary.BigEndian.AppendUint16(w.b, v) }
func (w *boxWriter) u32(v uint32) { w.b = binary.BigEndian.AppendUint32(w.b, v) }
func (w *boxWriter) u64(v uint64) { w.b = binary.BigEndian.AppendUint64(w.b, v) }
func (w *boxWriter) raw(p []byte) { w.b = append(w.b, p...) }
func (w *boxWriter) zeros(n int)  { w.b = append(w.b, make([]byte, n)...) }

// box appends a box whose payload is written by fill.
func (w *boxWriter) box(typ string, fill func()) {
	start := len(w.b)
	w.u32(0)
	w.raw([]byte(typ))
	fill()
	binary.BigEndian.PutUint32(w.b[start:], uint32(len(w.b)-start))
}

// full appends a full box (version and flags).
func (w *boxWriter) full(typ string, version uint8, flags uint32, fill func()) {
	w.box(typ, func() {
		w.u32(uint32(version)<<24 | flags&0xFFFFFF)
		fill()
	})
}

var unityMatrix = []uint32{0x00010000, 0, 0, 0, 0x00010000, 0, 0, 0, 0x40000000}

func (s *muxStream) initSegment(info MuxInfo) []byte {
	total := uint64(len(s.samples)) * uint64(info.SampleDuration)
	movieDuration := total * 1000 / uint64(info.Timescale)
	w := &boxWriter{}
	w.box("ftyp", func() {
		w.raw([]byte("iso6"))
		w.u32(0)
		w.raw([]byte("iso6iso5mp41"))
		w.raw([]byte(info.SampleEntry))
	})
	w.box("moov", func() {
		w.full("mvhd", 1, 0, func() {
			w.u64(0)
			w.u64(0)
			w.u32(1000)
			w.u64(movieDuration)
			w.u32(0x00010000) // rate
			w.u16(0x0100)     // volume
			w.zeros(10)
			for _, m := range unityMatrix {
				w.u32(m)
			}
			w.zeros(24)
			w.u32(2) // next_track_ID
		})
		w.box("trak", func() {
			w.full("tkhd", 1, 3, func() { // enabled, in movie
				w.u64(0)
				w.u64(0)
				w.u32(1) // track_ID
				w.u32(0)
				w.u64(movieDuration)
				w.zeros(8)
				w.u16(0) // layer
				w.u16(0) // alternate_group
				w.u16(0) // volume
				w.u16(0)
				for _, m := range unityMatrix {
					w.u32(m)
				}
				w.u32(uint32(s.width) << 16)
				w.u32(uint32(s.height) << 16)
			})
			w.box("mdia", func() {
				w.full("mdhd", 1, 0, func() {
					w.u64(0)
					w.u64(0)
					w.u32(info.Timescale)
					w.u64(total)
					w.u16(0x55C4) // "und"
					w.u16(0)
				})
				w.full("hdlr", 0, 0, func() {
					w.u32(0)
					w.raw([]byte("vide"))
					w.zeros(12)
					w.raw([]byte("VideoHandler\x00"))
				})
				w.box("minf", func() {
					w.full("vmhd", 0, 1, func() { w.zeros(8) })
					w.box("dinf", func() {
						w.full("dref", 0, 0, func() {
							w.u32(1)
							w.full("url ", 0, 1, func() {})
						})
					})
					w.box("stbl", func() {
						w.full("stsd", 0, 0, func() {
							w.u32(1)
							s.writeSampleEntry(w, info.SampleEntry)
						})
						w.full("stts", 0, 0, func() { w.u32(0) })
						w.full("stsc", 0, 0, func() { w.u32(0) })
						w.full("stsz", 0, 0, func() { w.u32(0); w.u32(0) })
						w.full("stco", 0, 0, func() { w.u32(0) })
					})
				})
			})
		})
		w.box("mvex", func() {
			w.full("mehd", 1, 0, func() { w.u64(movieDuration) })
			w.full("trex", 0, 0, func() {
				w.u32(1) // track_ID
				w.u32(1) // default_sample_description_index
				w.u32(0)
				w.u32(0)
				w.u32(0)
			})
		})
	})
	return w.b
}

func (s *muxStream) writeSampleEntry(w *boxWriter, entry string) {
	w.box(entry, func() {
		w.zeros(6)
		w.u16(1) // data_reference_index
		w.zeros(16)
		w.u16(uint16(s.width))
		w.u16(uint16(s.height))
		w.u32(0x00480000)
		w.u32(0x00480000)
		w.u32(0)
		w.u16(1) // frame_count
		w.zeros(32)
		w.u16(0x0018)
		w.u16(0xFFFF)
		if s.codec == CodecH264 {
			w.box("avcC", func() { s.writeAVCC(w) })
		} else {
			w.box("hvcC", func() { s.writeHVCC(w) })
		}
	})
}

// writeAVCC writes an AVCDecoderConfigurationRecord (ISO/IEC 14496-15 5.3.3).
func (s *muxStream) writeAVCC(w *boxWriter) {
	sps := s.h264
	w.u8(1)
	w.u8(sps.profile)
	w.u8(sps.compat)
	w.u8(sps.level)
	w.u8(0xFC | 3) // lengthSizeMinusOne
	w.u8(0xE0 | uint8(len(s.sps)))
	for _, p := range s.sps {
		w.u16(uint16(len(p.raw)))
		w.raw(p.raw)
	}
	w.u8(uint8(len(s.pps)))
	for _, p := range s.pps {
		w.u16(uint16(len(p.raw)))
		w.raw(p.raw)
	}
	if h264HighProfile(sps.profile) && sps.profile != 66 && sps.profile != 77 && sps.profile != 88 {
		w.u8(0xFC | uint8(sps.chromaFormat))
		w.u8(0xF8 | uint8(sps.bitDepthLuma-8))
		w.u8(0xF8 | uint8(sps.bitDepthChroma-8))
		w.u8(0)
	}
}

// writeHVCC writes an HEVCDecoderConfigurationRecord (ISO/IEC 14496-15 8.3.3).
func (s *muxStream) writeHVCC(w *boxWriter) {
	sps := s.hevc
	w.u8(1)
	w.raw(sps.ptl[:])
	w.u16(0xF000) // min_spatial_segmentation_idc 0
	w.u8(0xFC)    // parallelismType 0
	w.u8(0xFC | uint8(sps.chromaFormat))
	w.u8(0xF8 | uint8(sps.bitDepthLuma-8))
	w.u8(0xF8 | uint8(sps.bitDepthChroma-8))
	w.u16(0) // avgFrameRate
	nesting := uint8(0)
	if sps.temporalIDNesting {
		nesting = 1
	}
	w.u8(uint8(sps.maxSubLayers)<<3 | nesting<<2 | 3)
	w.u8(3)
	for _, arr := range []struct {
		t    uint8
		sets []paramSet
	}{{hevcNALVPS, s.vps}, {hevcNALSPS, s.sps}, {hevcNALPPS, s.pps}} {
		complete := uint8(0x80)
		if s.inBand {
			complete = 0
		}
		w.u8(complete | arr.t)
		w.u16(uint16(len(arr.sets)))
		for _, p := range arr.sets {
			w.u16(uint16(len(p.raw)))
			w.raw(p.raw)
		}
	}
}

const (
	sampleFlagsSync    = 0x02000000 // sample_depends_on = 2 (independent)
	sampleFlagsNonSync = 0x01010000 // depends on others, non-sync
)

// writeFragment writes moof+mdat for samples [start, end).
func (s *muxStream) writeFragment(dst io.Writer, seq uint32, start, end int, duration uint32) error {
	samples := s.samples[start:end]
	sizes := make([]uint32, len(samples))
	var payload uint64
	for i, sm := range samples {
		var n uint64
		for _, nal := range sm.nals {
			n += 4 + uint64(len(nal))
		}
		if n == 0 || n > math.MaxUint32 {
			return malformed("access unit size %d", n)
		}
		sizes[i] = uint32(n)
		payload += n
	}
	if payload+8 > math.MaxUint32 {
		return fmt.Errorf("%w: fragment exceeds 4 GiB", ErrMediaTooLarge)
	}
	w := &boxWriter{}
	var dataOffsetPos int
	w.box("moof", func() {
		w.full("mfhd", 0, 0, func() { w.u32(seq) })
		w.box("traf", func() {
			w.full("tfhd", 0, 0x020000, func() { w.u32(1) }) // default-base-is-moof
			w.full("tfdt", 1, 0, func() { w.u64(uint64(start) * uint64(duration)) })
			w.full("trun", 1, 0x000F01, func() {
				w.u32(uint32(len(samples)))
				dataOffsetPos = len(w.b)
				w.u32(0)
				for i, sm := range samples {
					w.u32(duration)
					w.u32(sizes[i])
					if sm.sync {
						w.u32(sampleFlagsSync)
					} else {
						w.u32(sampleFlagsNonSync)
					}
					w.u32(uint32(int32(sm.pres-(start+i)) * int32(duration)))
				}
			})
		})
	})
	binary.BigEndian.PutUint32(w.b[dataOffsetPos:], uint32(len(w.b)+8))
	w.u32(uint32(payload + 8))
	w.raw([]byte("mdat"))
	if _, err := dst.Write(w.b); err != nil {
		return err
	}
	var length [4]byte
	for _, sm := range samples {
		for _, nal := range sm.nals {
			binary.BigEndian.PutUint32(length[:], uint32(len(nal)))
			if _, err := dst.Write(length[:]); err != nil {
				return err
			}
			if _, err := dst.Write(nal); err != nil {
				return err
			}
		}
	}
	return nil
}

type countingWriter struct {
	dst io.Writer
	n   int64
}

func (w *countingWriter) Write(p []byte) (int, error) {
	n, err := w.dst.Write(p)
	w.n += int64(n)
	if err == nil && n != len(p) {
		err = io.ErrShortWrite
	}
	return n, err
}
