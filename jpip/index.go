package jpip

import (
	"errors"
	"fmt"

	"github.com/ThalesMMS/dicom-go/pixeldata/encapsulated"
)

// IndexLimits bounds the memory and work of indexing one codestream. Zero
// fields use the defaults.
type IndexLimits struct {
	// MaxCodestreamBytes rejects larger codestreams (default 512 MiB).
	MaxCodestreamBytes int
	// MaxPackets bounds indexed packets; past it the codestream is still
	// served as tile data-bins (JPT-stream) but not as precincts (default 4 Mi).
	MaxPackets int
}

const (
	defaultMaxCodestreamBytes = 512 << 20
	defaultMaxPackets         = 4 << 20
)

func (l IndexLimits) normalized() IndexLimits {
	if l.MaxCodestreamBytes <= 0 {
		l.MaxCodestreamBytes = defaultMaxCodestreamBytes
	}
	if l.MaxPackets <= 0 {
		l.MaxPackets = defaultMaxPackets
	}
	return l
}

type byteSpan struct {
	start, end int
}

func (s byteSpan) len() int { return s.end - s.start }

// indexedPrecinct is one precinct data-bin: its packets in layer order.
type indexedPrecinct struct {
	id      uint64
	c, r, k int
	packets []byteSpan
	length  int
}

type indexedTile struct {
	geometry  tileGeometry
	parts     []encapsulated.J2KTilePart
	header    []byte // tile header data-bin
	length    int    // tile data-bin length (all tile-parts)
	precincts []indexedPrecinct
}

// CodestreamIndex locates the JPIP data-bins (ISO/IEC 15444-9 Annex A) of
// one JPEG 2000 codestream: the main header data-bin, one tile header and
// one tile data-bin per tile and, when the packets can be located, one
// precinct data-bin per precinct. It retains the codestream.
type CodestreamIndex struct {
	data   []byte
	layout encapsulated.J2KLayout
	size   imageSize
	tiles  []indexedTile
	ht     bool
	layers int
	levels int // smallest decomposition depth over tile-components
	// precincts reports whether precinct data-bins are available; when not,
	// precinctErr says why.
	precincts   bool
	precinctErr error
	usedPLT     bool
	packetCount int
}

// IndexCodestream indexes a JPEG 2000 Part 1 or HTJ2K (Part 15) codestream.
// Packet boundaries come from PLT marker segments when every tile-part has
// consistent ones, and otherwise from reading the Part 1 packet headers.
// Codestreams whose packet headers are relocated (PPM/PPT), or HTJ2K
// codestreams without PLT, are indexed without precinct data-bins and can be
// served only as tile data-bins.
func IndexCodestream(data []byte, limits IndexLimits) (*CodestreamIndex, error) {
	return indexCodestream(data, limits, true)
}

func indexCodestream(data []byte, limits IndexLimits, usePLT bool) (*CodestreamIndex, error) {
	limits = limits.normalized()
	if len(data) > limits.MaxCodestreamBytes {
		return nil, fmt.Errorf("%w: codestream size", ErrIndexLimit)
	}
	layout, err := encapsulated.ParseJ2KLayout(data)
	if err != nil {
		return nil, errors.Join(ErrMalformedCodestream, err)
	}
	main, size, err := parseHeader(data[:layout.MainHeaderEnd], nil, true)
	if err != nil {
		return nil, err
	}
	if main.cod == nil {
		return nil, malformed("missing COD")
	}
	index := &CodestreamIndex{data: data, layout: layout, size: size, tiles: make([]indexedTile, size.numTiles())}
	for _, part := range layout.TileParts {
		if part.Tile >= len(index.tiles) {
			return nil, malformed("tile index out of range")
		}
		tile := &index.tiles[part.Tile]
		if len(tile.parts) > 0 && part.Part != tile.parts[len(tile.parts)-1].Part+1 || len(tile.parts) == 0 && part.Part != 0 {
			return nil, malformed("tile-parts out of order")
		}
		tile.parts = append(tile.parts, part)
		tile.length += part.End - part.Start
	}
	index.levels = maxDecompositionLevels
	tileParams := make([]headerParameters, len(index.tiles))
	for t := range index.tiles {
		tile := &index.tiles[t]
		if len(tile.parts) == 0 {
			return nil, malformed("missing tile")
		}
		headers := make([]byte, 0)
		pltComplete := true
		var plt []uint64
		for _, part := range tile.parts {
			raw := data[part.Start+12 : part.HeaderEnd-2]
			params, _, err := parseHeader(raw, &size, false)
			if err != nil {
				return nil, err
			}
			if !params.pltValid {
				pltComplete = false
			}
			plt = append(plt, params.pltLengths...)
			err = walkSegments(raw, func(s segment) error {
				if s.marker != markerPLT {
					headers = append(headers, raw[s.start:s.end]...)
				}
				return nil
			})
			if err != nil {
				return nil, err
			}
		}
		params, _, err := parseHeader(headers, &size, false)
		if err != nil {
			return nil, err
		}
		params.pltLengths, params.pltValid = plt, pltComplete
		tileParams[t] = params
		tile.header = headers
		style, comps, poc, err := codingParameters(size, main, params)
		if err != nil {
			return nil, err
		}
		geometry, err := newTileGeometry(size, t, style, comps, poc)
		if err != nil {
			return nil, err
		}
		tile.geometry = geometry
		index.ht = index.ht || geometry.ht
		index.layers = max(index.layers, geometry.layers)
		for _, comp := range geometry.components {
			index.levels = min(index.levels, comp.coding.levels)
		}
		switch {
		case main.packed || params.packed:
			index.precinctErr = errors.New("packet headers are relocated by PPM or PPT")
		case main.extended || params.extended:
			index.precinctErr = errors.New("Part 2 decomposition or code-block anchors are not indexed")
		}
	}
	if index.precinctErr == nil {
		index.precinctErr = index.indexPrecincts(tileParams, limits, usePLT)
	}
	index.precincts = index.precinctErr == nil
	if !index.precincts {
		for t := range index.tiles {
			index.tiles[t].precincts = nil
		}
	}
	return index, nil
}

func (x *CodestreamIndex) indexPrecincts(params []headerParameters, limits IndexLimits, usePLT bool) error {
	allPLT := usePLT
	for t := range x.tiles {
		allPLT = allPLT && params[t].pltValid
	}
	if allPLT {
		if err := x.indexWithPLT(params, limits); err == nil {
			x.usedPLT = true
			return nil
		} else if x.ht {
			return err
		}
	}
	if x.ht {
		return errors.New("HTJ2K packets are located only through PLT marker segments")
	}
	return x.indexWithPacketHeaders(limits)
}

// packetCursor walks a tile's packet data across its tile-parts.
type packetCursor struct {
	parts []encapsulated.J2KTilePart
	part  int
	pos   int
}

func newPacketCursor(parts []encapsulated.J2KTilePart) *packetCursor {
	c := &packetCursor{parts: parts}
	c.pos = parts[0].HeaderEnd
	c.skipEmpty()
	return c
}

func (c *packetCursor) skipEmpty() {
	for c.part < len(c.parts) && c.pos >= c.parts[c.part].End {
		c.part++
		if c.part < len(c.parts) {
			c.pos = c.parts[c.part].HeaderEnd
		}
	}
}

func (c *packetCursor) done() bool { return c.part >= len(c.parts) }

// limit is the end of the tile-part holding the next packet.
func (c *packetCursor) limit() int { return c.parts[c.part].End }

func (x *CodestreamIndex) newPrecincts(t int) {
	tile := &x.tiles[t]
	g := &tile.geometry
	tile.precincts = make([]indexedPrecinct, g.precinctCount)
	for c := range g.components {
		for r := range g.components[c].resolutions {
			res := &g.components[c].resolutions[r]
			for k := 0; k < res.precincts(); k++ {
				i := g.precinctIndex(c, r, k)
				tile.precincts[i] = indexedPrecinct{
					id: precinctBinID(t, c, res.precinctBase+k, len(g.components), len(x.tiles)),
					c:  c, r: r, k: k,
				}
			}
		}
	}
}

func (x *CodestreamIndex) addPacket(t int, c, r, k int, span byteSpan, limits IndexLimits) error {
	x.packetCount++
	if x.packetCount > limits.MaxPackets {
		return fmt.Errorf("%w: packet count", ErrIndexLimit)
	}
	tile := &x.tiles[t]
	p := &tile.precincts[tile.geometry.precinctIndex(c, r, k)]
	p.packets = append(p.packets, span)
	p.length += span.len()
	return nil
}

func (x *CodestreamIndex) indexWithPLT(params []headerParameters, limits IndexLimits) error {
	x.packetCount = 0
	for t := range x.tiles {
		tile := &x.tiles[t]
		x.newPrecincts(t)
		lengths := params[t].pltLengths
		cursor := newPacketCursor(tile.parts)
		n := 0
		err := tile.geometry.forEachPacket(tile.geometry.progression, tile.geometry.poc, func(l, r, c, k int) error {
			if cursor.done() || n >= len(lengths) {
				return malformed("fewer packets than the progression requires")
			}
			length := lengths[n]
			n++
			if length > uint64(cursor.limit()-cursor.pos) {
				return malformed("PLT length crosses a tile-part")
			}
			span := byteSpan{cursor.pos, cursor.pos + int(length)}
			cursor.pos = span.end
			cursor.skipEmpty()
			return x.addPacket(t, c, r, k, span, limits)
		})
		if err != nil {
			return err
		}
		if !cursor.done() || n != len(lengths) {
			return malformed("PLT lengths do not match the tile data")
		}
	}
	return nil
}

func (x *CodestreamIndex) indexWithPacketHeaders(limits IndexLimits) error {
	x.packetCount = 0
	for t := range x.tiles {
		tile := &x.tiles[t]
		g := &tile.geometry
		x.newPrecincts(t)
		states := make([]*precinctState, g.precinctCount)
		cursor := newPacketCursor(tile.parts)
		err := g.forEachPacket(g.progression, g.poc, func(l, r, c, k int) error {
			if cursor.done() {
				return malformed("fewer packets than the progression requires")
			}
			i := g.precinctIndex(c, r, k)
			if states[i] == nil {
				states[i] = newPrecinctState(g.codeBlockGrids(c, r, k))
			}
			end, err := measurePacket(x.data[:cursor.limit()], cursor.pos, states[i], l, g.scod, g.components[c].coding.cmodes)
			if err != nil {
				return errors.Join(ErrMalformedCodestream, err)
			}
			span := byteSpan{cursor.pos, end}
			cursor.pos = end
			cursor.skipEmpty()
			return x.addPacket(t, c, r, k, span, limits)
		})
		if err != nil {
			return err
		}
		if !cursor.done() {
			return malformed("tile data after the last packet")
		}
	}
	return nil
}

// Precincts reports whether precinct data-bins (JPP-stream) are available.
func (x *CodestreamIndex) Precincts() bool { return x != nil && x.precincts }

// HTJ2K reports whether any tile-component uses the HT block coder.
func (x *CodestreamIndex) HTJ2K() bool { return x != nil && x.ht }

// Size returns the full image size on the reference grid.
func (x *CodestreamIndex) Size() (width, height int) {
	return int(x.size.x1 - x.size.x0), int(x.size.y1 - x.size.y0)
}

// ReducedSize returns the frame size, on the reference grid, after
// discarding the given number of resolution levels: the fsiz a client sends
// to receive that resolution (ISO/IEC 15444-9 C.4.2).
func (x *CodestreamIndex) ReducedSize(discard int) (width, height int) {
	w, h := reducedSize(x.size, max(discard, 0))
	return int(w), int(h)
}

// DiscardLevels returns the largest number of resolution levels every
// tile-component can discard.
func (x *CodestreamIndex) DiscardLevels() int { return x.levels }

// Layers returns the largest number of quality layers over the tiles.
func (x *CodestreamIndex) Layers() int { return x.layers }

// mainHeader is the main header data-bin: SOC through the last main header
// marker segment.
func (x *CodestreamIndex) mainHeader() []byte { return x.data[:x.layout.MainHeaderEnd] }
