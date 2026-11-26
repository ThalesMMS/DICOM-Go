package video

import (
	"context"
	"encoding/binary"
	"slices"

	"github.com/ThalesMMS/dicom-go/transfer"
)

type importBox struct {
	kind                string
	start, payload, end int
}
type mp4Import struct {
	ctx         context.Context
	data        []byte
	boxes, nals int
	limits      ImportLimits
}

func (p *mp4Import) boxesAt(start, end int) ([]importBox, error) {
	var boxes []importBox
	for start < end {
		if e := p.ctx.Err(); e != nil {
			return nil, e
		}
		if p.boxes >= 4096 {
			return nil, ErrMediaTooLarge
		}
		p.boxes++
		if end-start < 8 {
			return nil, importError("truncated box")
		}
		size := uint64(binary.BigEndian.Uint32(p.data[start:]))
		header := 8
		if size == 1 {
			if end-start < 16 {
				return nil, ErrImportMedia
			}
			size = binary.BigEndian.Uint64(p.data[start+8:])
			header = 16
		}
		if size == 0 {
			size = uint64(end - start)
		}
		if size < uint64(header) || size > uint64(end-start) {
			return nil, ErrImportMedia
		}
		boxes = append(boxes, importBox{kind: string(p.data[start+4 : start+8]), start: start, payload: start + header, end: start + int(size)})
		start += int(size)
	}
	return boxes, nil
}
func oneBox(boxes []importBox, kind string) (importBox, error) {
	var found importBox
	n := 0
	for _, b := range boxes {
		if b.kind == kind {
			found = b
			n++
		}
	}
	if n != 1 {
		return importBox{}, importError("missing or duplicate " + kind)
	}
	return found, nil
}
func hasBox(boxes []importBox, kind string) bool {
	for _, b := range boxes {
		if b.kind == kind {
			return true
		}
	}
	return false
}
func (p *mp4Import) children(b importBox) ([]importBox, error) { return p.boxesAt(b.payload, b.end) }
func (p *mp4Import) body(b importBox) []byte                   { return p.data[b.payload:b.end] }
func (p *mp4Import) uniqueChild(parent importBox, kind string) (importBox, error) {
	b, e := p.children(parent)
	if e != nil {
		return importBox{}, e
	}
	return oneBox(b, kind)
}
func inspectMP4(ctx context.Context, data []byte, limits ImportLimits) (SourceInfo, error) {
	return inspectMP4Profile(ctx, data, limits, nil)
}

func inspectMP4Profile(ctx context.Context, data []byte, limits ImportLimits, profile *AVCProfile) (SourceInfo, error) {
	p := mp4Import{ctx: ctx, data: data, limits: limits}
	top, e := p.boxesAt(0, len(data))
	if e != nil {
		return SourceInfo{}, e
	}
	if hasBox(top, "moof") {
		return SourceInfo{}, importError("fragmented MP4")
	}
	ftyp, e := oneBox(top, "ftyp")
	if e != nil {
		return SourceInfo{}, e
	}
	if ftyp.start != 0 || len(p.body(ftyp)) < 8 || (len(p.body(ftyp))&3) != 0 {
		return SourceInfo{}, ErrImportMedia
	}
	brands := p.body(ftyp)
	known := false
	for i := 0; i < len(brands); i += 4 {
		if i == 4 {
			continue
		}
		switch string(brands[i : i+4]) {
		case "isom", "iso2", "mp41", "mp42", "avc1":
			known = true
		}
	}
	if !known {
		return SourceInfo{}, importError("unsupported MP4 brand")
	}
	moov, e := oneBox(top, "moov")
	if e != nil {
		return SourceInfo{}, e
	}
	movie, e := p.children(moov)
	if e != nil {
		return SourceInfo{}, e
	}
	trak, e := oneBox(movie, "trak")
	if e != nil {
		return SourceInfo{}, e
	}
	tracks, e := p.children(trak)
	if e != nil {
		return SourceInfo{}, e
	}
	if hasBox(tracks, "edts") {
		return SourceInfo{}, importError("edited MP4")
	}
	tkhd, e := oneBox(tracks, "tkhd")
	if e != nil {
		return SourceInfo{}, e
	}
	tk := p.body(tkhd)
	matrix := 40
	if len(tk) < 84 {
		return SourceInfo{}, ErrImportMedia
	}
	if tk[0] == 1 {
		matrix = 52
		if len(tk) < 96 {
			return SourceInfo{}, ErrImportMedia
		}
	} else if tk[0] != 0 {
		return SourceInfo{}, ErrImportMedia
	}
	for i, want := range []uint32{65536, 0, 0, 0, 65536, 0, 0, 0, 1 << 30} {
		if binary.BigEndian.Uint32(tk[matrix+i*4:]) != want {
			return SourceInfo{}, importError("rotated or scaled track")
		}
	}
	width, height := binary.BigEndian.Uint32(tk[matrix+36:]), binary.BigEndian.Uint32(tk[matrix+40:])
	if width&65535 != 0 || height&65535 != 0 {
		return SourceInfo{}, ErrImportMedia
	}
	mdia, e := oneBox(tracks, "mdia")
	if e != nil {
		return SourceInfo{}, e
	}
	media, e := p.children(mdia)
	if e != nil {
		return SourceInfo{}, e
	}
	hdlr, e := oneBox(media, "hdlr")
	if e != nil {
		return SourceInfo{}, e
	}
	h := p.body(hdlr)
	if len(h) < 12 || string(h[8:12]) != "vide" {
		return SourceInfo{}, importError("non-video track")
	}
	mdhd, e := oneBox(media, "mdhd")
	if e != nil {
		return SourceInfo{}, e
	}
	md := p.body(mdhd)
	var timescale uint32
	var duration uint64
	if len(md) < 20 {
		return SourceInfo{}, ErrImportMedia
	}
	if md[0] == 0 {
		timescale = binary.BigEndian.Uint32(md[12:])
		duration = uint64(binary.BigEndian.Uint32(md[16:]))
	} else if md[0] == 1 && len(md) >= 32 {
		timescale = binary.BigEndian.Uint32(md[20:])
		duration = binary.BigEndian.Uint64(md[24:])
	} else {
		return SourceInfo{}, ErrImportMedia
	}
	if timescale == 0 {
		return SourceInfo{}, ErrImportMedia
	}
	minf, e := oneBox(media, "minf")
	if e != nil {
		return SourceInfo{}, e
	}
	stbl, e := p.uniqueChild(minf, "stbl")
	if e != nil {
		return SourceInfo{}, e
	}
	tables, e := p.children(stbl)
	if e != nil {
		return SourceInfo{}, e
	}
	stsd, e := oneBox(tables, "stsd")
	if e != nil {
		return SourceInfo{}, e
	}
	sd := p.body(stsd)
	if len(sd) < 8 || binary.BigEndian.Uint32(sd[4:]) != 1 {
		return SourceInfo{}, ErrImportMedia
	}
	entries, e := p.boxesAt(stsd.payload+8, stsd.end)
	if e != nil || len(entries) != 1 || entries[0].kind != "avc1" {
		return SourceInfo{}, importError("unsupported MP4 sample entry")
	}
	entry := entries[0]
	visual := p.body(entry)
	if len(visual) < 78 {
		return SourceInfo{}, ErrImportMedia
	}
	columns, rows := int(binary.BigEndian.Uint16(visual[24:])), int(binary.BigEndian.Uint16(visual[26:]))
	if columns != int(width>>16) || rows != int(height>>16) {
		return SourceInfo{}, ErrImportMedia
	}
	config, e := p.boxesAt(entry.payload+78, entry.end)
	if e != nil {
		return SourceInfo{}, e
	}
	avcc, e := oneBox(config, "avcC")
	if e != nil {
		return SourceInfo{}, e
	}
	if hasBox(config, "pasp") {
		aspect, e := oneBox(config, "pasp")
		if e != nil {
			return SourceInfo{}, e
		}
		a := p.body(aspect)
		if len(a) != 8 || binary.BigEndian.Uint32(a) != 1 || binary.BigEndian.Uint32(a[4:]) != 1 {
			return SourceInfo{}, importError("non-square MP4 pixel aspect")
		}
	}
	lengthSize, e := p.inspectAVCC(p.body(avcc), columns, rows, profile)
	if e != nil {
		return SourceInfo{}, e
	}
	if profile != nil {
		if e = p.inspectAVCColour(config, profile); e != nil {
			return SourceInfo{}, e
		}
	}
	stsz, e := oneBox(tables, "stsz")
	if e != nil {
		return SourceInfo{}, e
	}
	sz := p.body(stsz)
	if len(sz) < 12 {
		return SourceInfo{}, ErrImportMedia
	}
	constant, n := binary.BigEndian.Uint32(sz[4:]), binary.BigEndian.Uint32(sz[8:])
	if n == 0 || uint64(n) > uint64(limits.MaxFrames) {
		return SourceInfo{}, ErrMediaTooLarge
	}
	if rows < 1 || columns < 1 || int64(rows)*int64(columns) > limits.MaxPixels || int64(rows)*int64(columns) > limits.MaxDecodedBytes/3/int64(n) {
		return SourceInfo{}, ErrMediaTooLarge
	}
	if constant == 0 && uint64(len(sz)) != 12+uint64(n)*4 || constant != 0 && len(sz) != 12 {
		return SourceInfo{}, ErrImportMedia
	}
	sizes := make([]uint32, int(n))
	for i := range sizes {
		sizes[i] = constant
		if constant == 0 {
			sizes[i] = binary.BigEndian.Uint32(sz[12+i*4:])
		}
		if sizes[i] == 0 {
			return SourceInfo{}, ErrImportMedia
		}
	}
	stts, e := oneBox(tables, "stts")
	if e != nil {
		return SourceInfo{}, e
	}
	ts := p.body(stts)
	if len(ts) < 8 {
		return SourceInfo{}, ErrImportMedia
	}
	count := uint64(binary.BigEndian.Uint32(ts[4:]))
	if count == 0 || count > uint64(limits.MaxFrames) || uint64(len(ts)) != 8+count*8 {
		return SourceInfo{}, ErrImportMedia
	}
	var frames uint64
	var delta uint32
	for i := uint64(0); i < count; i++ {
		samples := binary.BigEndian.Uint32(ts[8+i*8:])
		d := binary.BigEndian.Uint32(ts[12+i*8:])
		if samples == 0 || d == 0 || delta != 0 && delta != d {
			return SourceInfo{}, importError("nonconstant video timing")
		}
		delta = d
		frames += uint64(samples)
		if frames > uint64(limits.MaxFrames) {
			return SourceInfo{}, ErrMediaTooLarge
		}
	}
	macroblocks := uint64((columns+15)/16) * uint64((rows+15)/16)
	if columns > 4096 || rows > 4096 || macroblocks > 8192 || float64(macroblocks)*float64(timescale)/float64(delta) > 245760 {
		return SourceInfo{}, importError("AVC exceeds Level4.1 geometry/rate")
	}
	if frames != uint64(n) || duration != frames*uint64(delta) {
		return SourceInfo{}, importError("inconsistent frame timing")
	}
	if profile != nil {
		if e := p.inspectAVCPresentation(tables, n, delta); e != nil {
			return SourceInfo{}, e
		}
		numerator, denominator := normalizeAVCRate(timescale, delta)
		profile.Rows, profile.Columns, profile.Frames = rows, columns, int(n)
		profile.RateNumerator, profile.RateDenominator = numerator, denominator
		if e := profile.validate(); e != nil {
			return SourceInfo{}, e
		}
	}
	if e = p.inspectSamples(tables, top, sizes, lengthSize); e != nil {
		return SourceInfo{}, e
	}
	return SourceInfo{Rows: rows, Columns: columns, Frames: int(n), FrameRate: float64(timescale) / float64(delta), TransferSyntax: transfer.MPEG4HP41, Container: ContainerMP4, CompressionMethod: "ISO_14496_10"}, nil
}
func (p *mp4Import) inspectAVCC(b []byte, columns, rows int, profile *AVCProfile) (int, error) {
	if len(b) < 7 || b[0] != 1 || b[1] != 100 || !admittedAVCLevel(b[3]) || b[4]&0xfc != 0xfc || b[5]&0xe0 != 0xe0 {
		return 0, importError("AVC profile/level")
	}
	lengthSize := int(b[4]&3) + 1
	if lengthSize != 4 {
		return 0, importError("AVC NAL length width")
	}
	n := int(b[5] & 31)
	if n == 0 {
		return 0, ErrImportMedia
	}
	at := 6
	spss := make(map[uint32]*h264SPS)
	for i := 0; i < n; i++ {
		if at+2 > len(b) {
			return 0, ErrImportMedia
		}
		size := int(binary.BigEndian.Uint16(b[at:]))
		at += 2
		if size < 4 || size > len(b)-at || b[at]&31 != h264NALSPS {
			return 0, ErrImportMedia
		}
		s, e := parseH264SPS(b[at : at+size])
		if e != nil {
			return 0, e
		}
		if b[1] != b[at+1] || b[2] != b[at+2] || b[3] != b[at+3] {
			return 0, importError("AVC configuration/header mismatch")
		}
		if s.profile != 100 || !admittedAVCLevel(s.level) || s.chromaFormat != 1 || s.separateColourPlane || s.bitDepthLuma != 8 || s.bitDepthChroma != 8 || !s.frameMbsOnly || s.width != columns || s.height != rows {
			return 0, importError("AVC sample geometry/profile")
		}
		if e := inspectImportAVCAspect(s); e != nil {
			return 0, e
		}
		if profile != nil {
			colour, e := avcLimitedColour(s)
			if e != nil {
				return 0, e
			}
			if i > 0 && colour != profile.Colour {
				return 0, importError("changing AVC colour signal")
			}
			profile.Colour = colour
		}
		if _, exists := spss[s.id]; exists {
			return 0, ErrImportMedia
		}
		spss[s.id] = s
		at += size
	}
	if at >= len(b) {
		return 0, ErrImportMedia
	}
	n = int(b[at])
	at++
	if n == 0 {
		return 0, ErrImportMedia
	}
	for i := 0; i < n; i++ {
		if at+2 > len(b) {
			return 0, ErrImportMedia
		}
		size := int(binary.BigEndian.Uint16(b[at:]))
		at += 2
		if size < 2 || size > len(b)-at || b[at]&31 != h264NALPPS {
			return 0, ErrImportMedia
		}
		pps, e := parseH264PPS(b[at : at+size])
		if e != nil {
			return 0, e
		}
		if spss[pps.spsID] == nil {
			return 0, ErrImportMedia
		}
		at += size
	}
	// Optional AVC High Profile configuration extension is admitted only when
	// it agrees with the single 8-bit4:2:0 configuration and has no SPS extension.
	if at < len(b) {
		if len(b)-at != 4 || b[at]&3 != 1 || b[at+1]&7 != 0 || b[at+2]&7 != 0 || b[at+3] != 0 {
			return 0, importError("AVC configuration extension")
		}
	}
	return lengthSize, nil
}

func (p *mp4Import) inspectAVCColour(config []importBox, profile *AVCProfile) error {
	if !hasBox(config, "colr") {
		return nil
	}
	colour, err := oneBox(config, "colr")
	if err != nil {
		return err
	}
	body := p.body(colour)
	if len(body) != 10 && len(body) != 11 {
		return importError("unsupported MP4 colour signal")
	}
	switch string(body[:4]) {
	case "nclc":
		if len(body) != 10 {
			return ErrImportMedia
		}
	case "nclx":
		if len(body) != 11 || body[10] != 0 {
			return importError("full-range MP4 colour")
		}
	default:
		return importError("unsupported MP4 colour signal")
	}
	for i := range profile.Colour {
		value := binary.BigEndian.Uint16(body[4+2*i:])
		if value > 255 || (profile.Colour[i] != 2 && uint16(profile.Colour[i]) != value) {
			return importError("conflicting AVC and MP4 colour signals")
		}
		profile.Colour[i] = uint8(value)
	}
	return nil
}

// inspectAVCPresentation admits only a complete constant-spaced PTS lattice.
// Decode-order samples are allowed to arrive in a different order.
func (p *mp4Import) inspectAVCPresentation(tables []importBox, frames, delta uint32) error {
	if !hasBox(tables, "ctts") {
		return nil
	}
	ctts, err := oneBox(tables, "ctts")
	if err != nil {
		return err
	}
	table := p.body(ctts)
	if len(table) < 8 || (table[0] != 0 && table[0] != 1) {
		return ErrImportMedia
	}
	count := binary.BigEndian.Uint32(table[4:])
	if count == 0 || count > frames || uint64(len(table)) != 8+8*uint64(count) {
		return ErrImportMedia
	}
	presentation := make([]int64, 0, frames)
	for i := uint32(0); i < count; i++ {
		if err := p.ctx.Err(); err != nil {
			return err
		}
		at := 8 + 8*i
		samples := binary.BigEndian.Uint32(table[at:])
		offset := int64(binary.BigEndian.Uint32(table[at+4:]))
		if table[0] == 1 {
			offset = int64(int32(offset))
		}
		if samples == 0 || samples > frames-uint32(len(presentation)) {
			return ErrImportMedia
		}
		for j := uint32(0); j < samples; j++ {
			presentation = append(presentation, int64(len(presentation))*int64(delta)+offset)
		}
	}
	if uint32(len(presentation)) != frames {
		return ErrImportMedia
	}
	slices.Sort(presentation)
	for i, pts := range presentation {
		if pts-presentation[0] != int64(i)*int64(delta) {
			return importError("nonconstant presentation timing")
		}
	}
	return p.ctx.Err()
}
func (p *mp4Import) inspectSamples(tables, top []importBox, sizes []uint32, lengthSize int) error {
	stsc, e := oneBox(tables, "stsc")
	if e != nil {
		return e
	}
	sc := p.body(stsc)
	if len(sc) < 8 {
		return ErrImportMedia
	}
	n := uint64(binary.BigEndian.Uint32(sc[4:]))
	if n == 0 || n > uint64(len(sizes)) || uint64(len(sc)) != 8+n*12 {
		return ErrImportMedia
	}
	var offsets []uint64
	var off importBox
	width := 4
	if hasBox(tables, "stco") && hasBox(tables, "co64") {
		return ErrImportMedia
	}
	kind := "stco"
	if hasBox(tables, "co64") {
		kind = "co64"
		width = 8
	}
	off, e = oneBox(tables, kind)
	if e != nil {
		return e
	}
	co := p.body(off)
	if len(co) < 8 {
		return ErrImportMedia
	}
	chunks := uint64(binary.BigEndian.Uint32(co[4:]))
	if chunks == 0 || chunks > uint64(len(sizes)) || uint64(len(co)) != 8+chunks*uint64(width) {
		return ErrImportMedia
	}
	offsets = make([]uint64, int(chunks))
	for i := range offsets {
		if width == 4 {
			offsets[i] = uint64(binary.BigEndian.Uint32(co[8+i*4:]))
		} else {
			offsets[i] = binary.BigEndian.Uint64(co[8+i*8:])
		}
	}
	firsts := make([]uint32, int(n))
	per := make([]uint32, int(n))
	for i := range firsts {
		firsts[i] = binary.BigEndian.Uint32(sc[8+i*12:])
		per[i] = binary.BigEndian.Uint32(sc[12+i*12:])
		desc := binary.BigEndian.Uint32(sc[16+i*12:])
		if firsts[i] < 1 || uint64(firsts[i]) > chunks || i == 0 && firsts[i] != 1 || i > 0 && firsts[i] <= firsts[i-1] || per[i] == 0 || desc != 1 {
			return ErrImportMedia
		}
	}
	index, entry := 0, 0
	var previousEnd uint64
	for c, offset := range offsets {
		if e := p.ctx.Err(); e != nil {
			return e
		}
		for entry+1 < len(firsts) && firsts[entry+1] <= uint32(c+1) {
			entry++
		}
		if offset < previousEnd {
			return importError("overlapping video samples")
		}
		for j := uint32(0); j < per[entry]; j++ {
			if index >= len(sizes) || offset > uint64(len(p.data)) || uint64(sizes[index]) > uint64(len(p.data))-offset {
				return ErrImportMedia
			}
			end := offset + uint64(sizes[index])
			inMedia := false
			for _, m := range top {
				if m.kind == "mdat" && offset >= uint64(m.payload) && end <= uint64(m.end) {
					inMedia = true
					break
				}
			}
			if !inMedia {
				return importError("sample outside mdat")
			}
			if e = p.inspectAVCSample(p.data[int(offset):int(end)], lengthSize); e != nil {
				return e
			}
			offset = end
			index++
		}
		previousEnd = offset
	}
	if index != len(sizes) {
		return ErrImportMedia
	}
	return nil
}
func (p *mp4Import) inspectAVCSample(b []byte, width int) error {
	coded := false
	for at := 0; at < len(b); {
		p.nals++
		if p.nals > DefaultMaxFragments {
			return ErrTooManyFragments
		}
		if e := p.ctx.Err(); e != nil {
			return e
		}
		if len(b)-at < width {
			return ErrImportMedia
		}
		n := uint64(binary.BigEndian.Uint32(b[at:]))
		at += width
		if n == 0 || n > uint64(len(b)-at) || b[at]&128 != 0 {
			return ErrImportMedia
		}
		kind := b[at] & 31
		if kind == h264NALSPS || kind == h264NALPPS {
			return importError("in-band AVC configuration change")
		}
		if kind == h264NALSlice || kind == h264NALIDR {
			coded = true
		}
		at += int(n)
	}
	if !coded {
		return importError("sample without coded picture")
	}
	return nil
}

func admittedAVCLevel(level byte) bool {
	switch level {
	case 10, 11, 12, 13, 20, 21, 22, 30, 31, 32, 40, 41:
		return true
	}
	return false
}

func inspectImportAVCAspect(s *h264SPS) error {
	r := newBitReader(rbsp(s.raw[1:]))
	r.pos = s.vuiBitOffset
	if r.flag() && r.flag() {
		if idc := r.u(8); idc != 1 {
			return importError("AVC non-square sample aspect")
		}
	}
	if r.err != nil {
		return r.err
	}
	remainingOne := false
	for pos := r.pos; pos < len(r.data)*8; pos++ {
		if r.data[pos/8]&(1<<uint(7-pos%8)) != 0 {
			remainingOne = true
			break
		}
	}
	if !remainingOne {
		return importError("truncated AVC VUI/trailing bits")
	}
	return nil
}
