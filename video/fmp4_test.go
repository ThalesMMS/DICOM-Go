package video

import (
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"os"
	"path/filepath"
	"sort"
	"testing"

	"github.com/ThalesMMS/dicom-go/transfer"
)

type mp4Box struct {
	typ     string
	payload []byte
	start   int // offset of the box header in the file
}

func parseBoxes(t *testing.T, data []byte, base int) []mp4Box {
	t.Helper()
	var boxes []mp4Box
	for pos := 0; pos < len(data); {
		if len(data)-pos < 8 {
			t.Fatalf("truncated box header at %d", base+pos)
		}
		size := int(binary.BigEndian.Uint32(data[pos:]))
		if size < 8 || pos+size > len(data) {
			t.Fatalf("box %q at %d has size %d", data[pos+4:pos+8], base+pos, size)
		}
		boxes = append(boxes, mp4Box{typ: string(data[pos+4 : pos+8]), payload: data[pos+8 : pos+size], start: base + pos})
		pos += size
	}
	return boxes
}

func child(t *testing.T, boxes []mp4Box, path ...string) mp4Box {
	t.Helper()
	for _, b := range boxes {
		if b.typ != path[0] {
			continue
		}
		if len(path) == 1 {
			return b
		}
		inner := b.payload
		base := b.start + 8
		switch b.typ {
		case "stsd": // full box + entry count
			inner, base = inner[8:], base+8
		case "avc1", "avc3", "hvc1", "hev1": // visual sample entry fields
			inner, base = inner[78:], base+78
		}
		return child(t, parseBoxes(t, inner, base), path[1:]...)
	}
	t.Fatalf("box %v not found", path)
	return mp4Box{}
}

// muxedSample is one sample as read back from the moof/mdat boxes.
type muxedSample struct {
	dts, pts int64
	sync     bool
	nals     [][]byte
}

type muxedFile struct {
	sampleEntry string
	config      []byte // avcC or hvcC payload
	timescale   uint32
	samples     []muxedSample
	fragments   int
}

// readMuxed checks the structure of a fragmented MP4 and returns its samples.
func readMuxed(t *testing.T, data []byte) muxedFile {
	t.Helper()
	top := parseBoxes(t, data, 0)
	if len(top) < 4 || top[0].typ != "ftyp" || top[1].typ != "moov" {
		t.Fatalf("top-level boxes %v", boxTypes(top))
	}
	if string(top[0].payload[:4]) != "iso6" {
		t.Fatalf("major brand %q", top[0].payload[:4])
	}
	moov := parseBoxes(t, top[1].payload, top[1].start+8)
	var f muxedFile
	mdhd := child(t, moov, "trak", "mdia", "mdhd").payload
	f.timescale = binary.BigEndian.Uint32(mdhd[20:])
	if hdlr := child(t, moov, "trak", "mdia", "hdlr").payload; string(hdlr[8:12]) != "vide" {
		t.Fatalf("handler %q", hdlr[8:12])
	}
	stsd := child(t, moov, "trak", "mdia", "minf", "stbl", "stsd")
	entries := parseBoxes(t, stsd.payload[8:], stsd.start+16)
	if len(entries) != 1 {
		t.Fatalf("%d sample entries", len(entries))
	}
	f.sampleEntry = entries[0].typ
	configType := "avcC"
	if f.sampleEntry == "hvc1" || f.sampleEntry == "hev1" {
		configType = "hvcC"
	}
	f.config = child(t, moov, "trak", "mdia", "minf", "stbl", "stsd", f.sampleEntry, configType).payload
	trex := child(t, moov, "mvex", "trex").payload
	if binary.BigEndian.Uint32(trex[4:]) != 1 {
		t.Fatal("trex track_ID")
	}
	var dts int64
	for i := 2; i < len(top); i += 2 {
		moof := top[i]
		if moof.typ != "moof" || i+1 >= len(top) || top[i+1].typ != "mdat" {
			t.Fatalf("expected moof+mdat at %d, got %v", i, boxTypes(top[i:]))
		}
		f.fragments++
		mdat := top[i+1]
		boxes := parseBoxes(t, moof.payload, moof.start+8)
		if seq := binary.BigEndian.Uint32(child(t, boxes, "mfhd").payload[4:]); seq != uint32(f.fragments) {
			t.Fatalf("mfhd sequence %d, want %d", seq, f.fragments)
		}
		tfhd := child(t, boxes, "traf", "tfhd").payload
		if binary.BigEndian.Uint32(tfhd)&0xFFFFFF != 0x020000 || binary.BigEndian.Uint32(tfhd[4:]) != 1 {
			t.Fatalf("tfhd % x", tfhd)
		}
		tfdt := child(t, boxes, "traf", "tfdt").payload
		if tfdt[0] != 1 || int64(binary.BigEndian.Uint64(tfdt[4:])) != dts {
			t.Fatalf("tfdt %d, want %d", binary.BigEndian.Uint64(tfdt[4:]), dts)
		}
		trun := child(t, boxes, "traf", "trun").payload
		if trun[0] != 1 || binary.BigEndian.Uint32(trun)&0xFFFFFF != 0x000F01 {
			t.Fatalf("trun version/flags % x", trun[:4])
		}
		count := int(binary.BigEndian.Uint32(trun[4:]))
		offset := int(int32(binary.BigEndian.Uint32(trun[8:])))
		if offset != mdat.start+8-moof.start {
			t.Fatalf("trun data_offset %d, want %d", offset, mdat.start+8-moof.start)
		}
		pos := 0
		for k := 0; k < count; k++ {
			e := trun[12+16*k:]
			dur := int64(binary.BigEndian.Uint32(e))
			size := int(binary.BigEndian.Uint32(e[4:]))
			flags := binary.BigEndian.Uint32(e[8:])
			cto := int64(int32(binary.BigEndian.Uint32(e[12:])))
			if pos+size > len(mdat.payload) {
				t.Fatal("sample beyond mdat")
			}
			s := muxedSample{dts: dts, pts: dts + cto, sync: flags&0x00010000 == 0}
			au := mdat.payload[pos : pos+size]
			for len(au) > 0 {
				n := int(binary.BigEndian.Uint32(au))
				if 4+n > len(au) || n == 0 {
					t.Fatal("bad NAL length prefix")
				}
				s.nals = append(s.nals, au[4:4+n])
				au = au[4+n:]
			}
			f.samples = append(f.samples, s)
			pos += size
			dts += dur
		}
		if pos != len(mdat.payload) {
			t.Fatalf("mdat holds %d bytes, samples %d", len(mdat.payload), pos)
		}
	}
	return f
}

func boxTypes(boxes []mp4Box) []string {
	var out []string
	for _, b := range boxes {
		out = append(out, b.typ)
	}
	return out
}

func fixture(t *testing.T, name string) []byte {
	t.Helper()
	data, err := os.ReadFile(filepath.Join("testdata", name))
	if err != nil {
		t.Fatal(err)
	}
	return data
}

// checkPresentation requires PTS to be a permutation of 0, d, 2d, ... with
// key frames presented first in their coded video sequences.
func checkPresentation(t *testing.T, f muxedFile, dur int64) []int64 {
	t.Helper()
	pts := make([]int64, len(f.samples))
	for i, s := range f.samples {
		pts[i] = s.pts
	}
	sorted := append([]int64(nil), pts...)
	sort.Slice(sorted, func(a, b int) bool { return sorted[a] < sorted[b] })
	for i, p := range sorted {
		if p != int64(i)*dur {
			t.Fatalf("presentation times %v are not 0, %d, %d, ...", sorted, dur, 2*dur)
		}
	}
	if !f.samples[0].sync || pts[0] != 0 {
		t.Fatal("first sample is not a key frame presented first")
	}
	return pts
}

func TestMuxH264FixtureStructure(t *testing.T) {
	stream := fixture(t, "vt-h264.h264")
	var out bytes.Buffer
	info, err := MuxFragmentedMP4(context.Background(), &out, stream, MuxOptions{Codec: CodecH264, FrameRate: 25, MaxSamplesPerFragment: 5})
	if err != nil {
		t.Fatal(err)
	}
	if info.Width != 96 || info.Height != 64 || info.Samples != 24 || info.KeyFrames < 2 || !info.Reordered ||
		info.SampleEntry != "avc1" || info.Timescale != 90000 || info.SampleDuration != 3600 || info.Size != int64(out.Len()) {
		t.Fatalf("info %+v", info)
	}
	f := readMuxed(t, out.Bytes())
	if f.sampleEntry != "avc1" || len(f.samples) != 24 || f.fragments != info.Fragments || f.timescale != 90000 {
		t.Fatalf("read back %s %d samples %d fragments", f.sampleEntry, len(f.samples), f.fragments)
	}
	nals, err := splitAnnexB(stream, 1<<20)
	if err != nil {
		t.Fatal(err)
	}
	var sps, pps []byte
	for _, nal := range nals {
		switch nal[0] & 0x1F {
		case h264NALSPS:
			sps = nal
		case h264NALPPS:
			pps = nal
		}
	}
	cfg := f.config
	if cfg[0] != 1 || cfg[1] != sps[1] || cfg[3] != sps[3] || cfg[4] != 0xFF || cfg[5] != 0xE1 ||
		!bytes.Equal(cfg[8:8+len(sps)], sps) || !bytes.Equal(cfg[11+len(sps):11+len(sps)+len(pps)], pps) {
		t.Fatalf("avcC % x", cfg)
	}
	if info.CodecString != "avc1.640016" && info.CodecString[:7] != "avc1.64" {
		t.Fatalf("codec string %s", info.CodecString)
	}
	checkPresentation(t, f, 3600)
	slices := 0
	for _, s := range f.samples {
		for _, nal := range s.nals {
			switch nal[0] & 0x1F {
			case h264NALSPS, h264NALPPS, h264NALAUD:
				t.Fatal("parameter sets or delimiters left in samples")
			case h264NALSlice, h264NALIDR:
				slices++
				if (nal[0]&0x1F == h264NALIDR) != s.sync {
					t.Fatal("sync flag does not match IDR slices")
				}
			}
		}
	}
	if slices < 24 {
		t.Fatalf("%d slices in 24 samples", slices)
	}
}

func TestMuxHEVCFixtureStructure(t *testing.T) {
	var out bytes.Buffer
	info, err := MuxFragmentedMP4(context.Background(), &out, fixture(t, "vt-hevc.h265"), MuxOptions{Codec: CodecHEVC, FrameRate: 29.97})
	if err != nil {
		t.Fatal(err)
	}
	if info.Width != 96 || info.Height != 64 || info.Samples != 24 || info.SampleEntry != "hvc1" || info.Timescale != 30000 || info.SampleDuration != 1001 {
		t.Fatalf("info %+v", info)
	}
	f := readMuxed(t, out.Bytes())
	cfg := f.config
	if cfg[0] != 1 || cfg[22] != 3 {
		t.Fatalf("hvcC header % x", cfg[:23])
	}
	pos, types := 23, []byte{}
	for i := 0; i < 3; i++ {
		types = append(types, cfg[pos]&0x3F)
		if cfg[pos]&0x80 == 0 {
			t.Fatal("hvcC array not complete")
		}
		n := int(binary.BigEndian.Uint16(cfg[pos+1:]))
		pos += 3
		for k := 0; k < n; k++ {
			pos += 2 + int(binary.BigEndian.Uint16(cfg[pos:]))
		}
	}
	if !bytes.Equal(types, []byte{32, 33, 34}) || pos != len(cfg) {
		t.Fatalf("hvcC arrays %v, %d of %d bytes", types, pos, len(cfg))
	}
	checkPresentation(t, f, 1001)
	for _, s := range f.samples {
		for _, nal := range s.nals {
			if typ := hevcType(nal); typ >= 32 && typ <= 35 {
				t.Fatal("parameter sets or delimiters left in samples")
			}
		}
	}
}

// pesTimestamps returns the PTS of each PES packet of the first video PID.
func pesTimestamps(t *testing.T, ts []byte) []int64 {
	t.Helper()
	var out []int64
	for off := 0; off+188 <= len(ts); off += 188 {
		p := ts[off : off+188]
		if p[1]&0x40 == 0 || (int(p[1]&0x1F)<<8|int(p[2])) != 0x100 {
			continue
		}
		pos := 4
		if p[3]&0x20 != 0 {
			pos += 1 + int(p[4])
		}
		pes := p[pos:]
		if pes[7]&0x80 == 0 {
			t.Fatal("PES without PTS")
		}
		b := pes[9:14]
		pts := int64(b[0]>>1&7)<<30 | int64(b[1])<<22 | int64(b[2]>>1)<<15 | int64(b[3])<<7 | int64(b[4]>>1)
		out = append(out, pts)
	}
	return out
}

// The presentation order derived from picture order counts matches the
// encoder's own PES timestamps in an FFmpeg transport stream.
func TestMuxPresentationOrderMatchesTransportStreamTimestamps(t *testing.T) {
	ts := fixture(t, "vt-h264.ts")
	stream, err := demuxTransportStream(context.Background(), ts, CodecH264)
	if err != nil {
		t.Fatal(err)
	}
	var out bytes.Buffer
	if _, err := MuxFragmentedMP4(context.Background(), &out, stream, MuxOptions{Codec: CodecH264, FrameRate: 25}); err != nil {
		t.Fatal(err)
	}
	got := checkPresentation(t, readMuxed(t, out.Bytes()), 3600)
	want := pesTimestamps(t, ts)
	if len(want) != len(got) {
		t.Fatalf("%d PES packets, %d samples", len(want), len(got))
	}
	// Same presentation ranks (FFmpeg's PES clock has 1-tick rounding).
	if wr, gr := ranks(want), ranks(got); !equalInts(wr, gr) {
		t.Fatalf("presentation ranks: PES %v, muxed %v", wr, gr)
	}
}

func TestExportFragmentedMP4FromDICOM(t *testing.T) {
	ts := fixture(t, "vt-h264.ts")
	file := videoFile(t, transfer.MPEG4HP41, ts)
	var out bytes.Buffer
	mux, info, err := ExportFragmentedMP4(context.Background(), file, &out, MuxOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if info.Container != ContainerMPEGTransport || mux.Samples != 24 || mux.SampleDuration != 3600 || mux.Timescale != 90000 {
		t.Fatalf("info %+v mux %+v", info, mux)
	}
	readMuxed(t, out.Bytes())

	annexB := videoFile(t, transfer.HEVCMP51, fixture(t, "vt-hevc.h265"))
	if mux, info, err = ExportFragmentedMP4(context.Background(), annexB, &bytes.Buffer{}, MuxOptions{FrameRate: 50}); err != nil || info.Container != ContainerAnnexB || mux.Codec != CodecHEVC || mux.SampleDuration != 1800 {
		t.Fatalf("Annex B export: %v %+v %+v", err, info, mux)
	}
	mp4 := videoFile(t, transfer.MPEG4HP41, mp4TestPayload())
	if _, _, err := ExportFragmentedMP4(context.Background(), mp4, &bytes.Buffer{}, MuxOptions{}); !errors.Is(err, ErrUnsupportedContainer) {
		t.Fatalf("MP4 source: %v", err)
	}
	mpeg2 := videoFile(t, transfer.MPEG2MPML, []byte{0, 0, 1, 0xB3, 1, 2, 3, 4})
	if _, _, err := ExportFragmentedMP4(context.Background(), mpeg2, &bytes.Buffer{}, MuxOptions{}); !errors.Is(err, ErrUnsupportedContainer) {
		t.Fatalf("MPEG-2 source: %v", err)
	}
}

func TestMuxDropsPicturesBeforeTheFirstKeyFrame(t *testing.T) {
	stream := fixture(t, "vt-h264.h264")
	nals, err := splitAnnexB(stream, 1<<20)
	if err != nil {
		t.Fatal(err)
	}
	// Keep the parameter sets, drop the IDR picture and start at the next
	// slices: those pictures reference the missing IDR.
	var cut []byte
	idrSeen := false
	for _, nal := range nals {
		typ := nal[0] & 0x1F
		if typ == h264NALIDR && !idrSeen {
			idrSeen = true
			continue
		}
		if !idrSeen && typ != h264NALSPS && typ != h264NALPPS {
			continue
		}
		cut = append(append(cut, 0, 0, 0, 1), nal...)
	}
	var out bytes.Buffer
	info, err := MuxFragmentedMP4(context.Background(), &out, cut, MuxOptions{Codec: CodecH264, FrameRate: 25})
	if err != nil {
		t.Fatal(err)
	}
	if info.DroppedLeading == 0 || info.Samples+info.DroppedLeading != 23 {
		t.Fatalf("info %+v", info)
	}
	checkPresentation(t, readMuxed(t, out.Bytes()), 3600)
}

// A parameter set that changes mid-stream selects an in-band sample entry
// (avc3) whose samples keep their parameter sets.
func TestMuxKeepsChangingParameterSetsInBand(t *testing.T) {
	nals, err := splitAnnexB(fixture(t, "vt-h264.h264"), 1<<20)
	if err != nil {
		t.Fatal(err)
	}
	var stream []byte
	var pps []byte
	idrs := 0
	for _, nal := range nals {
		switch nal[0] & 0x1F {
		case h264NALPPS:
			pps = nal
		case h264NALIDR:
			idrs++
			if idrs == 2 {
				changed := append(append([]byte(nil), pps...), 0x80) // extra RBSP byte
				stream = append(append(stream, 0, 0, 1), changed...)
			}
		}
		stream = append(append(stream, 0, 0, 1), nal...)
	}
	var out bytes.Buffer
	info, err := MuxFragmentedMP4(context.Background(), &out, stream, MuxOptions{Codec: CodecH264, FrameRate: 25})
	if err != nil {
		t.Fatal(err)
	}
	f := readMuxed(t, out.Bytes())
	if info.SampleEntry != "avc3" || f.sampleEntry != "avc3" || info.CodecString[:5] != "avc3." {
		t.Fatalf("sample entry %s / %s", info.SampleEntry, f.sampleEntry)
	}
	inBand := 0
	for _, s := range f.samples {
		for _, nal := range s.nals {
			if nal[0]&0x1F == h264NALPPS {
				inBand++
			}
		}
	}
	if inBand < 2 {
		t.Fatalf("%d in-band PPS", inBand)
	}
}

func TestMuxRejectsInvalidInput(t *testing.T) {
	stream := fixture(t, "vt-h264.h264")
	ctx := context.Background()
	cases := []struct {
		name   string
		stream []byte
		opts   MuxOptions
		want   error
	}{
		{"frame rate", stream, MuxOptions{Codec: CodecH264}, ErrFrameRateRequired},
		{"codec", stream, MuxOptions{Codec: CodecMPEG2, FrameRate: 25}, ErrUnsupportedStream},
		{"no start code", []byte{1, 2, 3, 4}, MuxOptions{Codec: CodecH264, FrameRate: 25}, ErrMalformedStream},
		{"no parameter sets", []byte{0, 0, 1, 0x65, 0x88, 0x80}, MuxOptions{Codec: CodecH264, FrameRate: 25}, ErrMalformedStream},
		{"truncated SPS", []byte{0, 0, 1, 0x67, 0x64}, MuxOptions{Codec: CodecH264, FrameRate: 25}, ErrMalformedStream},
		{"HEVC as H.264", fixture(t, "vt-hevc.h265"), MuxOptions{Codec: CodecH264, FrameRate: 25}, ErrMalformedStream},
		{"limit", stream, MuxOptions{Codec: CodecH264, FrameRate: 25, Limits: Limits{MaxBytes: 100}}, ErrMediaTooLarge},
		{"NAL limit", stream, MuxOptions{Codec: CodecH264, FrameRate: 25, Limits: Limits{MaxFragments: 3}}, ErrTooManyFragments},
	}
	for _, tc := range cases {
		if _, err := MuxFragmentedMP4(ctx, &bytes.Buffer{}, tc.stream, tc.opts); !errors.Is(err, tc.want) {
			t.Errorf("%s: err = %v, want %v", tc.name, err, tc.want)
		}
	}
	cancelled, cancel := context.WithCancel(ctx)
	cancel()
	if _, err := MuxFragmentedMP4(cancelled, &bytes.Buffer{}, stream, MuxOptions{Codec: CodecH264, FrameRate: 25}); !errors.Is(err, context.Canceled) {
		t.Errorf("cancelled: err = %v", err)
	}
}

func TestMediaTiming(t *testing.T) {
	for _, tc := range []struct {
		rate          float64
		scale, sample uint32
	}{
		{25, 90000, 3600}, {30, 90000, 3000}, {60, 90000, 1500}, {12.5, 90000, 7200}, {24, 90000, 3750},
		{29.97, 30000, 1001}, {30000.0 / 1001, 30000, 1001}, {23.976, 24000, 1001}, {59.94, 60000, 1001},
		{7, 7000, 1000},
	} {
		if scale, sample := mediaTiming(tc.rate); scale != tc.scale || sample != tc.sample {
			t.Errorf("mediaTiming(%v) = %d/%d, want %d/%d", tc.rate, sample, scale, tc.sample, tc.scale)
		}
	}
}

func TestHEVCCodecString(t *testing.T) {
	// Main profile, compatible with profiles 1 and 2, level 3.1, progressive
	// and frame-only constraint flags: the ISO/IEC 14496-15 Annex E example.
	ptl := [12]byte{0x01, 0x60, 0, 0, 0, 0xB0, 0, 0, 0, 0, 0, 93}
	if got := hevcCodecString("hvc1", ptl); got != "hvc1.1.6.L93.B0" {
		t.Fatalf("got %s", got)
	}
	ptl = [12]byte{0x22 | 0x40, 0x20, 0, 0, 0, 0, 0, 0, 0, 0, 0, 120}
	if got := hevcCodecString("hev1", ptl); got != "hev1.A2.4.H120" {
		t.Fatalf("got %s", got)
	}
}

func TestH264PictureOrderCountWraps(t *testing.T) {
	sps := &h264SPS{log2MaxPocLsb: 4, log2MaxFrameNum: 4}
	var p h264POC
	var got []int64
	for i, lsb := range []uint32{0, 4, 8, 12, 0, 4} { // wraps at 16
		got = append(got, p.next(&h264Slice{sps: sps, idr: i == 0, refIdc: 1, pocLsb: lsb}))
	}
	if want := []int64{0, 4, 8, 12, 16, 20}; !equalInt64(got, want) {
		t.Fatalf("type 0 POCs %v, want %v", got, want)
	}
	sps2 := &h264SPS{pocType: 2, log2MaxFrameNum: 2}
	var q h264POC
	got = got[:0]
	for i, fn := range []uint32{0, 1, 2, 3, 3, 0} { // frame_num wraps at 4
		ref := uint32(1)
		if i == 3 {
			ref = 0
		}
		got = append(got, q.next(&h264Slice{sps: sps2, idr: i == 0, refIdc: ref, frameNum: fn}))
	}
	if want := []int64{0, 2, 4, 5, 6, 8}; !equalInt64(got, want) {
		t.Fatalf("type 2 POCs %v, want %v", got, want)
	}
}

func TestHEVCPictureOrderCountWrapsAndRestarts(t *testing.T) {
	sps := &hevcSPS{log2MaxPocLsb: 4}
	var p hevcPOC
	type pic struct {
		t   int
		lsb uint32
	}
	var got []int64
	var restarts []bool
	for _, pc := range []pic{{19, 0}, {1, 8}, {1, 14}, {1, 2}, {21, 6}, {19, 0}} {
		poc, restart := p.next(&hevcSlice{nalType: pc.t, pocLsb: pc.lsb, sps: sps})
		got, restarts = append(got, poc), append(restarts, restart)
	}
	if want := []int64{0, 8, 14, 18, 22, 0}; !equalInt64(got, want) {
		t.Fatalf("POCs %v, want %v", got, want)
	}
	if want := []bool{true, false, false, false, false, true}; !equalBools(restarts, want) {
		t.Fatalf("restarts %v", restarts)
	}
}

func ranks(v []int64) []int {
	idx := make([]int, len(v))
	for i := range idx {
		idx[i] = i
	}
	sort.SliceStable(idx, func(a, b int) bool { return v[idx[a]] < v[idx[b]] })
	out := make([]int, len(v))
	for rank, i := range idx {
		out[i] = rank
	}
	return out
}

func equalInts(a, b []int) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

func equalInt64(a, b []int64) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

func equalBools(a, b []bool) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

func FuzzMuxFragmentedMP4(f *testing.F) {
	for _, name := range []string{"vt-h264.h264", "vt-hevc.h265"} {
		data, err := os.ReadFile(filepath.Join("testdata", name))
		if err != nil {
			f.Fatal(err)
		}
		f.Add(data, name == "vt-hevc.h265")
	}
	f.Fuzz(func(t *testing.T, data []byte, hevc bool) {
		codec := CodecH264
		if hevc {
			codec = CodecHEVC
		}
		var out bytes.Buffer
		_, err := MuxFragmentedMP4(context.Background(), &out, data, MuxOptions{Codec: codec, FrameRate: 25})
		if err == nil {
			readMuxed(t, out.Bytes())
		} else if !errors.Is(err, ErrMalformedStream) && !errors.Is(err, ErrUnsupportedStream) && !errors.Is(err, ErrTooManyFragments) {
			t.Fatalf("untyped error %v", err)
		}
	})
}
