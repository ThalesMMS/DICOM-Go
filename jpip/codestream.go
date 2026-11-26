package jpip

import (
	"encoding/binary"
	"errors"
	"fmt"
)

// This file reads the coding parameters of a JPEG 2000 codestream (ISO/IEC
// 15444-1 Annex A) and derives the tile, resolution, sub-band and precinct
// geometry of Annex B that data-bin indexing needs. It never decodes
// coefficients.

// ErrMalformedCodestream reports a JPEG 2000 codestream the indexer cannot
// describe: invalid marker segments, inconsistent geometry, or packets that do
// not match the declared progression.
var ErrMalformedCodestream = errors.New("dicom: malformed JPEG 2000 codestream for JPIP")

// ErrIndexLimit reports a codestream whose index would exceed a configured
// bound.
var ErrIndexLimit = errors.New("dicom: JPEG 2000 codestream exceeds JPIP index limits")

const (
	markerSOC = 0xff4f
	markerCAP = 0xff50
	markerSIZ = 0xff51
	markerCOD = 0xff52
	markerCOC = 0xff53
	markerTLM = 0xff55
	markerPLM = 0xff57
	markerPLT = 0xff58
	markerQCD = 0xff5c
	markerQCC = 0xff5d
	markerRGN = 0xff5e
	markerPOC = 0xff5f
	markerPPM = 0xff60
	markerPPT = 0xff61
	markerCRG = 0xff63
	markerCOM = 0xff64
	markerDFS = 0xff72
	markerADS = 0xff73
	markerSOT = 0xff90
	markerSOP = 0xff91
	markerEPH = 0xff92
	markerSOD = 0xff93
	markerEOC = 0xffd9

	progressionLRCP = 0
	progressionRLCP = 1
	progressionRPCL = 2
	progressionPCRL = 3
	progressionCPRL = 4

	scodPrecincts = 0x01
	scodSOP       = 0x02
	scodEPH       = 0x04
	// Part 2 code-block anchor offsets (ISO/IEC 15444-2 A.2.3).
	scodPart2Anchors = 0x18

	cmodeBypass  = 0x01
	cmodeTermAll = 0x04
	cmodeHT      = 0x40

	maxDecompositionLevels = 32
	maxComponents          = 16384
)

func malformed(format string, args ...any) error {
	return fmt.Errorf("%w: "+format, append([]any{ErrMalformedCodestream}, args...)...)
}

// segment is one marker segment; payload excludes the marker and length.
type segment struct {
	marker  uint16
	start   int // offset of the marker inside the walked buffer
	end     int // offset just past the segment
	payload []byte
}

// walkSegments splits a run of marker segments. SOC, which has no length, is
// reported with an empty payload.
func walkSegments(data []byte, fn func(segment) error) error {
	for at := 0; at < len(data); {
		if at+2 > len(data) || data[at] != 0xff {
			return malformed("expected a marker")
		}
		marker := binary.BigEndian.Uint16(data[at:])
		if marker == markerSOC {
			if err := fn(segment{marker: marker, start: at, end: at + 2}); err != nil {
				return err
			}
			at += 2
			continue
		}
		if at+4 > len(data) {
			return malformed("truncated marker segment")
		}
		length := int(binary.BigEndian.Uint16(data[at+2:]))
		if length < 2 || at+2+length > len(data) {
			return malformed("marker segment length out of range")
		}
		if err := fn(segment{marker: marker, start: at, end: at + 2 + length, payload: data[at+4 : at+2+length]}); err != nil {
			return err
		}
		at += 2 + length
	}
	return nil
}

type componentSize struct {
	xr, yr int64
}

type imageSize struct {
	x1, y1, x0, y0     int64 // Xsiz, Ysiz, XOsiz, YOsiz
	tw, th, tx0, ty0   int64 // XTsiz, YTsiz, XTOsiz, YTOsiz
	comps              []componentSize
	tilesX, tilesY     int
	componentFieldSize int // bytes of a component index in COC/QCC/RGN/POC
}

func (s imageSize) numTiles() int { return s.tilesX * s.tilesY }

func parseSIZ(p []byte) (imageSize, error) {
	if len(p) < 36 {
		return imageSize{}, malformed("short SIZ")
	}
	u32 := func(at int) int64 { return int64(binary.BigEndian.Uint32(p[at:])) }
	s := imageSize{
		x1: u32(2), y1: u32(6), x0: u32(10), y0: u32(14),
		tw: u32(18), th: u32(22), tx0: u32(26), ty0: u32(30),
	}
	csiz := int(binary.BigEndian.Uint16(p[34:]))
	if csiz < 1 || csiz > maxComponents || len(p) != 36+3*csiz {
		return imageSize{}, malformed("SIZ component count")
	}
	if s.x1 <= s.x0 || s.y1 <= s.y0 || s.tw == 0 || s.th == 0 ||
		s.tx0 > s.x0 || s.ty0 > s.y0 || s.tx0+s.tw <= s.x0 || s.ty0+s.th <= s.y0 {
		return imageSize{}, malformed("SIZ geometry")
	}
	for c := 0; c < csiz; c++ {
		xr, yr := int64(p[37+3*c]), int64(p[38+3*c])
		if xr == 0 || yr == 0 {
			return imageSize{}, malformed("SIZ sub-sampling")
		}
		s.comps = append(s.comps, componentSize{xr: xr, yr: yr})
	}
	tilesX := ceilDiv(s.x1-s.tx0, s.tw)
	tilesY := ceilDiv(s.y1-s.ty0, s.th)
	if tilesX*tilesY > 65535 {
		return imageSize{}, malformed("too many tiles")
	}
	s.tilesX, s.tilesY = int(tilesX), int(tilesY)
	s.componentFieldSize = 1
	if csiz > 256 {
		s.componentFieldSize = 2
	}
	return s, nil
}

// componentCoding holds the SPcod/SPcoc parameters of one component.
type componentCoding struct {
	levels   int
	xcb, ycb int // code-block exponents
	cmodes   byte
	ppx, ppy []int // per resolution, 0..levels
}

type codingStyle struct {
	scod        byte
	progression byte
	layers      int
	component   componentCoding
}

func parseComponentCoding(p []byte, precincts bool) (componentCoding, int, error) {
	if len(p) < 5 {
		return componentCoding{}, 0, malformed("short coding style")
	}
	cc := componentCoding{levels: int(p[0]), xcb: int(p[1]) + 2, ycb: int(p[2]) + 2, cmodes: p[3]}
	if cc.levels > maxDecompositionLevels || cc.xcb > 10 || cc.ycb > 10 || cc.xcb+cc.ycb > 12 {
		return componentCoding{}, 0, malformed("coding style parameters")
	}
	used := 5
	cc.ppx = make([]int, cc.levels+1)
	cc.ppy = make([]int, cc.levels+1)
	for r := 0; r <= cc.levels; r++ {
		cc.ppx[r], cc.ppy[r] = 15, 15
		if !precincts {
			continue
		}
		if used >= len(p) {
			return componentCoding{}, 0, malformed("short precinct sizes")
		}
		cc.ppx[r], cc.ppy[r] = int(p[used]&0x0f), int(p[used]>>4)
		used++
		if r > 0 && (cc.ppx[r] == 0 || cc.ppy[r] == 0) {
			return componentCoding{}, 0, malformed("precinct size")
		}
	}
	return cc, used, nil
}

func parseCOD(p []byte) (codingStyle, error) {
	if len(p) < 4 {
		return codingStyle{}, malformed("short COD")
	}
	cs := codingStyle{scod: p[0], progression: p[1], layers: int(binary.BigEndian.Uint16(p[2:]))}
	if cs.progression > progressionCPRL || cs.layers == 0 {
		return codingStyle{}, malformed("COD progression or layers")
	}
	cc, used, err := parseComponentCoding(p[5:], cs.scod&scodPrecincts != 0)
	if err != nil {
		return codingStyle{}, err
	}
	if 5+used != len(p) {
		return codingStyle{}, malformed("COD length")
	}
	cs.component = cc
	return cs, nil
}

func parseCOC(p []byte, size imageSize) (int, componentCoding, error) {
	n := size.componentFieldSize
	if len(p) < n+1 {
		return 0, componentCoding{}, malformed("short COC")
	}
	c := int(p[0])
	if n == 2 {
		c = int(binary.BigEndian.Uint16(p))
	}
	if c >= len(size.comps) {
		return 0, componentCoding{}, malformed("COC component")
	}
	cc, used, err := parseComponentCoding(p[n+1:], p[n]&scodPrecincts != 0)
	if err != nil {
		return 0, componentCoding{}, err
	}
	if n+1+used != len(p) {
		return 0, componentCoding{}, malformed("COC length")
	}
	return c, cc, nil
}

type progressionChange struct {
	rs, cs, lye, re, ce int
	order               byte
}

func parsePOC(p []byte, size imageSize) ([]progressionChange, error) {
	n := size.componentFieldSize
	entry := 5 + 2*n
	if len(p) == 0 || len(p)%entry != 0 {
		return nil, malformed("POC length")
	}
	var out []progressionChange
	for at := 0; at < len(p); at += entry {
		e := p[at:]
		var change progressionChange
		change.rs = int(e[0])
		if n == 1 {
			change.cs = int(e[1])
		} else {
			change.cs = int(binary.BigEndian.Uint16(e[1:]))
		}
		change.lye = int(binary.BigEndian.Uint16(e[1+n:]))
		change.re = int(e[3+n])
		if n == 1 {
			change.ce = int(e[4+n])
			if change.ce == 0 {
				change.ce = 256
			}
		} else {
			change.ce = int(binary.BigEndian.Uint16(e[4+n:]))
		}
		change.order = e[4+2*n]
		if change.order > progressionCPRL {
			return nil, malformed("POC progression")
		}
		out = append(out, change)
	}
	return out, nil
}

// headerParameters collects the parameters of one header (main or tile).
type headerParameters struct {
	cod        *codingStyle
	coc        map[int]componentCoding
	poc        []progressionChange
	packed     bool // PPM or PPT relocates packet headers
	extended   bool // Part 2 decomposition or code-block anchors change the geometry
	ht         bool
	pltLengths []uint64
	pltValid   bool
}

func parseHeader(data []byte, size *imageSize, main bool) (headerParameters, imageSize, error) {
	params := headerParameters{coc: map[int]componentCoding{}, pltValid: true}
	var siz imageSize
	if size != nil {
		siz = *size
	}
	sawSIZ := size != nil
	pltSeen := false
	err := walkSegments(data, func(s segment) error {
		switch s.marker {
		case markerSOC:
			if !main || s.start != 0 {
				return malformed("unexpected SOC")
			}
		case markerSIZ:
			if !main || sawSIZ {
				return malformed("unexpected SIZ")
			}
			parsed, err := parseSIZ(s.payload)
			if err != nil {
				return err
			}
			siz, sawSIZ = parsed, true
		case markerCOD:
			if params.cod != nil {
				return malformed("duplicate COD")
			}
			cod, err := parseCOD(s.payload)
			if err != nil {
				return err
			}
			params.cod = &cod
			if cod.scod&scodPart2Anchors != 0 {
				params.extended = true
			}
		case markerCOC:
			if !sawSIZ {
				return malformed("COC before SIZ")
			}
			c, cc, err := parseCOC(s.payload, siz)
			if err != nil {
				return err
			}
			params.coc[c] = cc
		case markerPOC:
			if !sawSIZ {
				return malformed("POC before SIZ")
			}
			changes, err := parsePOC(s.payload, siz)
			if err != nil {
				return err
			}
			params.poc = append(params.poc, changes...)
		case markerPPM, markerPPT:
			params.packed = true
		case markerDFS, markerADS:
			params.extended = true
		case markerPLT:
			pltSeen = true
			lengths, ok := parsePLT(s.payload)
			if !ok {
				params.pltValid = false
			}
			params.pltLengths = append(params.pltLengths, lengths...)
		case markerSOT, markerSOD, markerEOC, markerSOP, markerEPH:
			return malformed("unexpected delimiting marker in a header")
		}
		return nil
	})
	if err != nil {
		return headerParameters{}, imageSize{}, err
	}
	if main && !sawSIZ {
		return headerParameters{}, imageSize{}, malformed("missing SIZ")
	}
	if !pltSeen {
		params.pltValid = false
	}
	return params, siz, nil
}

// parsePLT reads the packet lengths of one PLT segment (ISO/IEC 15444-1
// A.7.3). A length must not straddle segments.
func parsePLT(p []byte) ([]uint64, bool) {
	if len(p) < 1 {
		return nil, false
	}
	var lengths []uint64
	var value uint64
	open := false
	for _, b := range p[1:] {
		if value>>56 != 0 {
			return nil, false
		}
		value = value<<7 | uint64(b&0x7f)
		open = b&0x80 != 0
		if !open {
			lengths = append(lengths, value)
			value = 0
		}
	}
	return lengths, !open
}

// bandGeometry is one sub-band of a resolution, in its own coordinates.
type bandGeometry struct {
	x0, y0, x1, y1 int64
}

func (b bandGeometry) empty() bool { return b.x0 >= b.x1 || b.y0 >= b.y1 }

type resolutionGeometry struct {
	x0, y0, x1, y1 int64
	ppx, ppy       int
	pw, ph         int
	precinctBase   int // precincts of lower resolutions of this component
	bands          []bandGeometry
}

func (r resolutionGeometry) precincts() int { return r.pw * r.ph }

type componentGeometry struct {
	x0, y0, x1, y1 int64
	xr, yr         int64
	coding         componentCoding
	resolutions    []resolutionGeometry
	precinctBase   int // precincts of lower components of this tile
	precinctCount  int
}

type tileGeometry struct {
	index          int
	x0, y0, x1, y1 int64
	scod           byte
	progression    byte
	layers         int
	poc            []progressionChange
	components     []componentGeometry
	precinctCount  int
	ht             bool
}

// codingParameters resolves the parameters that apply inside one tile
// (ISO/IEC 15444-1 A.6.1 precedence: tile COC, tile COD, main COC, main COD).
func codingParameters(size imageSize, main, tile headerParameters) (codingStyle, []componentCoding, []progressionChange, error) {
	if main.cod == nil {
		return codingStyle{}, nil, nil, malformed("missing COD")
	}
	style := *main.cod
	if tile.cod != nil {
		style = *tile.cod
	}
	comps := make([]componentCoding, len(size.comps))
	for c := range comps {
		switch cc, ok := tile.coc[c]; {
		case ok:
			comps[c] = cc
		case tile.cod != nil:
			comps[c] = tile.cod.component
		default:
			if cc, ok := main.coc[c]; ok {
				comps[c] = cc
			} else {
				comps[c] = main.cod.component
			}
		}
	}
	poc := main.poc
	if len(tile.poc) > 0 {
		poc = tile.poc
	}
	return style, comps, poc, nil
}

func newTileGeometry(size imageSize, index int, style codingStyle, comps []componentCoding, poc []progressionChange) (tileGeometry, error) {
	p, q := int64(index%size.tilesX), int64(index/size.tilesX)
	t := tileGeometry{
		index:       index,
		x0:          max(size.tx0+p*size.tw, size.x0),
		y0:          max(size.ty0+q*size.th, size.y0),
		x1:          min(size.tx0+(p+1)*size.tw, size.x1),
		y1:          min(size.ty0+(q+1)*size.th, size.y1),
		scod:        style.scod,
		progression: style.progression,
		layers:      style.layers,
		poc:         poc,
	}
	for c, cs := range size.comps {
		coding := comps[c]
		if coding.cmodes&cmodeHT != 0 {
			t.ht = true
		}
		comp := componentGeometry{
			x0: ceilDiv(t.x0, cs.xr), y0: ceilDiv(t.y0, cs.yr),
			x1: ceilDiv(t.x1, cs.xr), y1: ceilDiv(t.y1, cs.yr),
			xr: cs.xr, yr: cs.yr, coding: coding, precinctBase: t.precinctCount,
		}
		base := 0
		for r := 0; r <= coding.levels; r++ {
			n := uint(coding.levels - r)
			res := resolutionGeometry{
				x0: ceilDiv(comp.x0, 1<<n), y0: ceilDiv(comp.y0, 1<<n),
				x1: ceilDiv(comp.x1, 1<<n), y1: ceilDiv(comp.y1, 1<<n),
				ppx: coding.ppx[r], ppy: coding.ppy[r], precinctBase: base,
			}
			if res.x1 > res.x0 && res.y1 > res.y0 {
				pw := ceilDiv(res.x1, 1<<uint(res.ppx)) - floorDiv(res.x0, 1<<uint(res.ppx))
				ph := ceilDiv(res.y1, 1<<uint(res.ppy)) - floorDiv(res.y0, 1<<uint(res.ppy))
				if pw*ph > 1<<24 {
					return tileGeometry{}, fmt.Errorf("%w: precinct count", ErrIndexLimit)
				}
				res.pw, res.ph = int(pw), int(ph)
			}
			if r == 0 {
				res.bands = []bandGeometry{bandOf(comp, coding.levels, 0, 0)}
			} else {
				nb := coding.levels - r + 1
				res.bands = []bandGeometry{
					bandOf(comp, nb, 1, 0), // HL
					bandOf(comp, nb, 0, 1), // LH
					bandOf(comp, nb, 1, 1), // HH
				}
			}
			base += res.precincts()
			comp.resolutions = append(comp.resolutions, res)
		}
		comp.precinctCount = base
		t.precinctCount += base
		if t.precinctCount > 1<<24 {
			return tileGeometry{}, fmt.Errorf("%w: precinct count", ErrIndexLimit)
		}
		t.components = append(t.components, comp)
	}
	return t, nil
}

// bandOf applies ISO/IEC 15444-1 equation B-15.
func bandOf(comp componentGeometry, nb int, xo, yo int64) bandGeometry {
	if nb == 0 {
		return bandGeometry{x0: comp.x0, y0: comp.y0, x1: comp.x1, y1: comp.y1}
	}
	half := int64(1) << uint(nb-1)
	scale := int64(1) << uint(nb)
	return bandGeometry{
		x0: ceilDiv(comp.x0-half*xo, scale), y0: ceilDiv(comp.y0-half*yo, scale),
		x1: ceilDiv(comp.x1-half*xo, scale), y1: ceilDiv(comp.y1-half*yo, scale),
	}
}

// codeBlockGrid is the code-block partition of one band inside one precinct.
type codeBlockGrid struct {
	cw, ch int
}

func (t *tileGeometry) codeBlockGrids(c, r, k int) []codeBlockGrid {
	comp := &t.components[c]
	res := &comp.resolutions[r]
	px, py := int64(k%res.pw), int64(k/res.pw)
	pbx, pby := res.ppx, res.ppy
	originX := floorDiv(res.x0, 1<<uint(res.ppx)) << uint(res.ppx)
	originY := floorDiv(res.y0, 1<<uint(res.ppy)) << uint(res.ppy)
	if r > 0 {
		pbx, pby = pbx-1, pby-1
		originX, originY = originX/2, originY/2
	}
	xcb, ycb := min(comp.coding.xcb, pbx), min(comp.coding.ycb, pby)
	grids := make([]codeBlockGrid, len(res.bands))
	for b, band := range res.bands {
		if band.empty() {
			continue
		}
		x0 := max(originX+px<<uint(pbx), band.x0)
		y0 := max(originY+py<<uint(pby), band.y0)
		x1 := min(originX+(px+1)<<uint(pbx), band.x1)
		y1 := min(originY+(py+1)<<uint(pby), band.y1)
		if x1 <= x0 || y1 <= y0 {
			continue
		}
		grids[b] = codeBlockGrid{
			cw: int(ceilDiv(x1, 1<<uint(xcb)) - floorDiv(x0, 1<<uint(xcb))),
			ch: int(ceilDiv(y1, 1<<uint(ycb)) - floorDiv(y0, 1<<uint(ycb))),
		}
	}
	return grids
}

// precinctBounds returns the reference-grid rectangle a precinct covers.
func (t *tileGeometry) precinctBounds(c, r, k int) (x0, y0, x1, y1 int64) {
	comp := &t.components[c]
	res := &comp.resolutions[r]
	px, py := int64(k%res.pw), int64(k/res.pw)
	rx0 := max((floorDiv(res.x0, 1<<uint(res.ppx))+px)<<uint(res.ppx), res.x0)
	ry0 := max((floorDiv(res.y0, 1<<uint(res.ppy))+py)<<uint(res.ppy), res.y0)
	rx1 := min((floorDiv(res.x0, 1<<uint(res.ppx))+px+1)<<uint(res.ppx), res.x1)
	ry1 := min((floorDiv(res.y0, 1<<uint(res.ppy))+py+1)<<uint(res.ppy), res.y1)
	n := uint(comp.coding.levels - r)
	return rx0 << n * comp.xr, ry0 << n * comp.yr, rx1 << n * comp.xr, ry1 << n * comp.yr
}

// precinctIndex is the position of (c, r, k) among the tile's precincts.
func (t *tileGeometry) precinctIndex(c, r, k int) int {
	comp := &t.components[c]
	return comp.precinctBase + comp.resolutions[r].precinctBase + k
}

// precinctBinID is the in-class identifier of ISO/IEC 15444-9 A.3.2.1:
// I = t + (c + s * Csiz) * numTiles, with s the precinct's sequence number
// within its tile-component.
func precinctBinID(t, c, s, numComponents, numTiles int) uint64 {
	return uint64(t) + (uint64(c)+uint64(s)*uint64(numComponents))*uint64(numTiles)
}

// packetVisitor receives packets in progression order.
type packetVisitor func(layer, r, c, k int) error

// forEachPacket enumerates the packets of a tile in the order its
// progression (COD, or POC when present) places them in the codestream
// (ISO/IEC 15444-1 B.12). A packet already visited by an earlier progression
// change is skipped, so each (layer, precinct) is reported once.
func (t *tileGeometry) forEachPacket(order byte, poc []progressionChange, visit packetVisitor) error {
	maxRes := 0
	for _, comp := range t.components {
		maxRes = max(maxRes, comp.coding.levels+1)
	}
	changes := poc
	if len(changes) == 0 {
		changes = []progressionChange{{rs: 0, cs: 0, lye: t.layers, re: maxRes, ce: len(t.components), order: order}}
	}
	next := make([]int32, t.precinctCount)
	emit := func(l, r, c, k int) error {
		i := t.precinctIndex(c, r, k)
		switch {
		case int(next[i]) > l:
			return nil
		case int(next[i]) < l:
			return malformed("progression skips a layer")
		}
		next[i]++
		return visit(l, r, c, k)
	}
	for _, change := range changes {
		lye := min(change.lye, t.layers)
		ce := min(change.ce, len(t.components))
		cs, rs, re := change.cs, change.rs, min(change.re, maxRes)
		if cs >= ce || rs >= re || lye <= 0 {
			continue
		}
		var err error
		switch change.order {
		case progressionLRCP:
			for l := 0; l < lye && err == nil; l++ {
				for r := rs; r < re && err == nil; r++ {
					err = t.eachComponentPrecinct(cs, ce, r, func(c, k int) error { return emit(l, r, c, k) })
				}
			}
		case progressionRLCP:
			for r := rs; r < re && err == nil; r++ {
				for l := 0; l < lye && err == nil; l++ {
					err = t.eachComponentPrecinct(cs, ce, r, func(c, k int) error { return emit(l, r, c, k) })
				}
			}
		case progressionRPCL:
			for r := rs; r < re && err == nil; r++ {
				err = t.eachPosition(cs, ce, r, r+1, func(x, y int64) error {
					for c := cs; c < ce; c++ {
						if k, ok := t.precinctAt(c, r, x, y); ok {
							for l := 0; l < lye; l++ {
								if err := emit(l, r, c, k); err != nil {
									return err
								}
							}
						}
					}
					return nil
				})
			}
		case progressionPCRL:
			err = t.eachPosition(cs, ce, rs, re, func(x, y int64) error {
				for c := cs; c < ce; c++ {
					for r := rs; r < re; r++ {
						if k, ok := t.precinctAt(c, r, x, y); ok {
							for l := 0; l < lye; l++ {
								if err := emit(l, r, c, k); err != nil {
									return err
								}
							}
						}
					}
				}
				return nil
			})
		case progressionCPRL:
			for c := cs; c < ce && err == nil; c++ {
				err = t.eachPosition(c, c+1, rs, re, func(x, y int64) error {
					for r := rs; r < re; r++ {
						if k, ok := t.precinctAt(c, r, x, y); ok {
							for l := 0; l < lye; l++ {
								if err := emit(l, r, c, k); err != nil {
									return err
								}
							}
						}
					}
					return nil
				})
			}
		}
		if err != nil {
			return err
		}
	}
	return nil
}

func (t *tileGeometry) eachComponentPrecinct(cs, ce, r int, fn func(c, k int) error) error {
	for c := cs; c < ce; c++ {
		comp := &t.components[c]
		if r > comp.coding.levels {
			continue
		}
		for k := 0; k < comp.resolutions[r].precincts(); k++ {
			if err := fn(c, k); err != nil {
				return err
			}
		}
	}
	return nil
}

// eachPosition visits, in raster order, every reference-grid position of the
// tile where a precinct of a component in [cs, ce) at a resolution in
// [rs, re) can start. Candidate positions are the tile origin and the
// multiples of XRsiz*2^(PPx+NL-r) (and likewise vertically), which is where
// the conditions of ISO/IEC 15444-1 B.12.1.3 can hold.
func (t *tileGeometry) eachPosition(cs, ce, rs, re int, fn func(x, y int64) error) error {
	step := func(v, limit int64, vertical bool) int64 {
		next := limit
		for c := cs; c < ce; c++ {
			comp := &t.components[c]
			for r := rs; r < min(re, comp.coding.levels+1); r++ {
				res := &comp.resolutions[r]
				if res.precincts() == 0 {
					continue
				}
				n := uint(comp.coding.levels - r)
				m := comp.xr << (uint(res.ppx) + n)
				if vertical {
					m = comp.yr << (uint(res.ppy) + n)
				}
				next = min(next, (floorDiv(v, m)+1)*m)
			}
		}
		return next
	}
	for y := t.y0; y < t.y1; y = step(y, t.y1, true) {
		for x := t.x0; x < t.x1; x = step(x, t.x1, false) {
			if err := fn(x, y); err != nil {
				return err
			}
		}
	}
	return nil
}

// precinctAt applies the position test of ISO/IEC 15444-1 B.12.1.3 and
// returns the precinct of component c at resolution r that starts at (x, y).
func (t *tileGeometry) precinctAt(c, r int, x, y int64) (int, bool) {
	comp := &t.components[c]
	if r > comp.coding.levels {
		return 0, false
	}
	res := &comp.resolutions[r]
	if res.precincts() == 0 {
		return 0, false
	}
	n := uint(comp.coding.levels - r)
	if !(y%(comp.yr<<(uint(res.ppy)+n)) == 0 || y == t.y0 && res.y0%(1<<uint(res.ppy)) != 0) {
		return 0, false
	}
	if !(x%(comp.xr<<(uint(res.ppx)+n)) == 0 || x == t.x0 && res.x0%(1<<uint(res.ppx)) != 0) {
		return 0, false
	}
	px := floorDiv(ceilDiv(x, comp.xr<<n), 1<<uint(res.ppx)) - floorDiv(res.x0, 1<<uint(res.ppx))
	py := floorDiv(ceilDiv(y, comp.yr<<n), 1<<uint(res.ppy)) - floorDiv(res.y0, 1<<uint(res.ppy))
	if px < 0 || py < 0 || px >= int64(res.pw) || py >= int64(res.ph) {
		return 0, false
	}
	return int(px) + int(py)*res.pw, true
}

func ceilDiv(a, b int64) int64 {
	q := a / b
	if a%b != 0 && a > 0 {
		q++
	}
	return q
}

func floorDiv(a, b int64) int64 {
	q := a / b
	if a%b != 0 && a < 0 {
		q--
	}
	return q
}
