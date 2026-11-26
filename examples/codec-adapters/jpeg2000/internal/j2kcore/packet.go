// Adapted from github.com/amrshadid/go-dicom compress/jpeg2000/packet.go at
// commit daecea14be3fbc27300aff20912ed7c8c8a0d599 (v1.6.0, MIT License, see
// LICENSE.amrshadid-go-dicom and PROVENANCE.md in this directory).

package j2kcore

import (
	"context"
	"slices"
	"sort"
)

// Packet headers, PS 15444-1 Annex B.10.
//
// A tile-part's bytes are a sequence of packets. Each packet belongs to one
// (layer, resolution, component, precinct) and carries, for every code-block in
// that precinct: whether the block appears at all, how many of its leading bit
// planes are zero, how many coding passes it contributes, and how many bytes
// those passes occupy. The bytes themselves follow the header, unless the
// headers were packed into PPM or PPT segments (A.7.4, A.7.5), in which case
// they are read from there and the tile data holds the bodies alone.
//
// Nothing here decodes a coefficient. This is the layer that says which bytes
// belong to which code-block, so tier-1 can be handed one block's data.

// Segment is one codeword segment of a code-block: the bytes of the coding
// passes between two terminations (D.4). A block coded without code-block
// style options is a single segment.
type Segment struct {
	Data   []byte
	Passes int
}

// CodeBlockData is everything one code-block contributed across the layers of
// a tile: its place in the band, and the bytes tier-1 will decode.
type CodeBlockData struct {
	Block         CodeBlock
	BandType      int
	Resolution    int
	Component     int
	ZeroBitPlanes int // leading bit planes that are all zero (B.10.5)
	Passes        int
	Segments      []Segment
	// Style is the code-block style of the tile-component (Block* flags).
	Style byte
}

// TileData is one tile's code-blocks, in the order the geometry lists them.
type TileData struct {
	Tile       int
	Components []TileComponent
	Blocks     []CodeBlockData

	// BytesRead is how much of the tile's data the packets accounted for. It
	// should be all of it: the packet headers are self-describing, so a reader
	// that ends anywhere but the last byte has misread a field somewhere.
	BytesRead int
	TileBytes int
}

// blockState is what a packet header has said about one code-block so far. The
// state persists across layers: a block included in layer 0 is signaled with a
// single bit in layer 1, and its length field grows as its contributions do.
type blockState struct {
	included      bool
	zeroBitPlanes int
	lblock        int // bits in the length field, 3 to start (B.10.7.1)
	passes        int
	// kept counts the passes whose bytes were attached: all of them, unless
	// a packetFilter dropped the contributions of some packets.
	kept     int
	segments []Segment
	// open says the last segment was not terminated, so the next
	// contribution continues it.
	open bool
}

// precinctState holds the two tag trees one band of a precinct shares across
// its packets, and the state of its code-blocks.
type precinctState struct {
	inclusion *tagTree
	zeroBits  *tagTree
	blocks    []blockState
}

// maxPacketsPerTile bounds the packets a tile's headers may declare, unless
// its data is larger: every packet header takes at least a byte.
const maxPacketsPerTile = 1 << 20

// ReadTilePackets reads every packet of a tile and returns the code-blocks with
// their data gathered across layers.
//
// The tile-parts are concatenated first: the standard allows a packet to be
// split across tile-parts, and a decoder that read each part separately would
// stop at the seam.
func (c *Codestream) ReadTilePackets(tile int) (*TileData, error) {
	return c.readTilePackets(context.Background(), tile, nil)
}

// PacketSpan locates one packet of a tile whose headers are not packed: its
// SOP segment if any, its header (with EPH) and its body, as slices of the
// tile's concatenated data.
type PacketSpan struct {
	Layer, Resolution, Component, Precinct int
	SOP, Header, Body                      []byte
}

// PacketSpans reads a tile's packets and returns where each lies, in
// codestream order. It is how a tool reorders or repacks packets.
func (c *Codestream) PacketSpans(tile int) ([]PacketSpan, error) {
	for _, part := range c.TileParts {
		if part.Tile == tile && part.PackedHeaders {
			return nil, errorf("tile %d has packed packet headers", tile)
		}
	}
	var spans []PacketSpan
	if _, err := c.readTilePackets(context.Background(), tile, &spans); err != nil {
		return nil, err
	}
	return spans, nil
}

// PacketOrder lists the (layer, resolution, component, precinct) of a
// tile's packets in the order its progression, or its POC, places them.
func (c *Codestream) PacketOrder(tile int) ([][4]int, error) {
	var comps []TileComponent
	for comp := range c.Components {
		tc, err := c.TileComponentFor(tile, comp)
		if err != nil {
			return nil, err
		}
		comps = append(comps, tc)
	}
	steps, err := c.packetSequence(tile, comps, 0)
	if err != nil {
		return nil, err
	}
	out := make([][4]int, len(steps))
	for i, s := range steps {
		out[i] = [4]int{int(s.layer), int(s.resolution), int(s.component), int(s.precinct)}
	}
	return out, nil
}

// packetFilter selects the packets whose bodies a decode needs: those of
// the first layers quality layers (all when 0) and of the resolutions a
// reduction by reduce keeps. The headers of the others are still read, as
// they carry the state later headers of the same precinct build on (B.10.4,
// B.10.7), but their bodies are not attached to any code-block; once no
// later packet of the progression is needed, reading stops.
type packetFilter struct {
	layers, reduce int
}

// needs reports whether a packet's body is decoded.
func (f packetFilter) needs(step packetStep, comps []TileComponent) bool {
	if f.layers > 0 && int(step.layer) >= f.layers {
		return false
	}
	comp := int(step.component)
	return comp < len(comps) && int(step.resolution) < len(comps[comp].Resolutions)-f.reduce
}

func (c *Codestream) readTilePackets(ctx context.Context, tile int, spans *[]PacketSpan) (*TileData, error) {
	return c.readTilePacketsFiltered(ctx, tile, spans, packetFilter{})
}

func (c *Codestream) readTilePacketsFiltered(ctx context.Context, tile int, spans *[]PacketSpan, filter packetFilter) (*TileData, error) {
	var body, headers []byte
	packed := false
	for _, part := range c.TileParts {
		if part.Tile != tile {
			continue
		}
		body = append(body, part.Data...)
		if part.PackedHeaders {
			packed = true
			headers = append(headers, part.Headers...)
		}
	}
	if len(body) == 0 && len(headers) == 0 {
		return nil, errorf("tile %d has no data", tile)
	}
	if !packed {
		headers = body
	}

	out := &TileData{Tile: tile}
	for comp := range c.Components {
		tc, err := c.TileComponentFor(tile, comp)
		if err != nil {
			return nil, err
		}
		out.Components = append(out.Components, tc)
	}
	steps, err := c.packetSequence(tile, out.Components, len(headers))
	if err != nil {
		return nil, err
	}
	if filter != (packetFilter{}) {
		// Nothing after the last needed packet changes a needed block.
		last := -1
		for i, step := range steps {
			if filter.needs(step, out.Components) {
				last = i
			}
		}
		steps = steps[:last+1]
	}

	states := map[[4]int]*precinctState{} // component, resolution, band, precinct
	var scratch []contribution            // reused by every packet
	hpos, bpos := 0, 0
	for _, step := range steps {
		if hpos >= len(headers) {
			// Layers may simply stop: an encoder writes as many as it wrote,
			// and the rest of the progression is empty.
			break
		}
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		b0 := bpos
		var headerStart, headerEnd int
		keep := filter == (packetFilter{}) || filter.needs(step, out.Components)
		hpos, bpos, headerStart, headerEnd, err = c.readPacket(headers, hpos, body, bpos, packed, tile, step, keep, states, out, &scratch)
		if err != nil {
			return nil, err
		}
		if spans != nil && !packed {
			*spans = append(*spans, PacketSpan{
				Layer: int(step.layer), Resolution: int(step.resolution), Component: int(step.component), Precinct: int(step.precinct),
				SOP: body[b0:headerStart], Header: body[headerStart:headerEnd], Body: body[headerEnd:bpos],
			})
		}

		if !packed {
			hpos = bpos
		}
	}

	out.BytesRead = bpos
	out.TileBytes = len(body)

	// The blocks in geometry order, which is the order tier-1 wants them.
	for _, key := range sortedKeys(states) {
		state := states[key]
		comp, res, band, precinct := key[0], key[1], key[2], key[3]
		bandInfo := out.Components[comp].Resolutions[res].Bands[band]
		style := c.CodingForTile(tile, comp).BlockStyle
		for i := range state.blocks {
			b := &state.blocks[i]
			if !b.included || len(b.segments) == 0 {
				continue
			}
			out.Blocks = append(out.Blocks, CodeBlockData{
				Block:         bandInfo.Precincts[precinct].Blocks[i],
				BandType:      bandInfo.Type,
				Resolution:    res,
				Component:     comp,
				ZeroBitPlanes: b.zeroBitPlanes,
				Passes:        b.kept,
				Segments:      b.segments,
				Style:         style,
			})
		}
	}
	return out, nil
}

// packetStep names one packet's place in the progression.
type packetStep struct {
	layer      int32
	resolution int32
	component  int32
	precinct   int32
	// x and y are where the precinct starts on the reference grid, clipped to
	// the tile, which orders the position-driven progressions (B.12.1.3-5).
	x, y int32
}

// packetSequence lists the packets of a tile in the order they appear (B.12):
// the progression of the tile's coding style, or the volumes of its POC
// entries in turn, each skipping the packets an earlier one placed (A.6.6).
func (c *Codestream) packetSequence(tile int, comps []TileComponent, streamBytes int) ([]packetStep, error) {
	style := c.CodingForTile(tile, 0)
	changes := c.ProgressionChangesForTile(tile)
	if len(changes) == 0 {
		changes = []ProgressionChange{{ResolutionEnd: 33, ComponentEnd: len(comps), LayerEnd: style.Layers, Order: style.Progression}}
	}

	total := int64(0)
	for _, tc := range comps {
		for _, res := range tc.Resolutions {
			total += int64(res.Precincts())
		}
	}
	total *= int64(style.Layers)
	if limit := int64(maxInt(maxPacketsPerTile, streamBytes)); total > limit {
		return nil, &ErrLimit{Resource: "packets per tile", Value: total, Limit: limit}
	}

	tx0, ty0, _, _ := c.TileBounds(tile)
	// Only POC volumes can overlap, so only they need to know what was placed.
	var emitted map[packetStep]bool
	if len(changes) > 1 {
		emitted = map[packetStep]bool{}
	}
	var steps []packetStep
	for _, change := range changes {
		var volume []packetStep
		for comp := maxInt(change.ComponentStart, 0); comp < minInt(change.ComponentEnd, len(comps)); comp++ {
			tc := comps[comp]
			levels := len(tc.Resolutions) - 1
			dx, dy := c.Components[comp].DX, c.Components[comp].DY
			for r := maxInt(change.ResolutionStart, 0); r < minInt(change.ResolutionEnd, len(tc.Resolutions)); r++ {
				res := tc.Resolutions[r]
				for p := 0; p < res.Precincts(); p++ {
					// The precinct's origin on the reference grid, or the tile's
					// edge for a precinct that begins before it.
					px := (floorDivPow2(res.X0, res.PPx) + p%res.PrecinctsWide) << res.PPx
					py := (floorDivPow2(res.Y0, res.PPy) + p/res.PrecinctsWide) << res.PPy
					x := maxInt(px*dx<<(levels-r), tx0)
					y := maxInt(py*dy<<(levels-r), ty0)
					for l := 0; l < minInt(change.LayerEnd, style.Layers); l++ {
						step := packetStep{layer: int32(l), resolution: int32(r), component: int32(comp), precinct: int32(p), x: int32(x), y: int32(y)}
						if emitted != nil {
							key := step
							key.x, key.y = 0, 0
							if emitted[key] {
								continue
							}
							emitted[key] = true
						}
						volume = append(volume, step)
					}
				}
			}
		}
		sortProgression(volume, change.Order)
		steps = append(steps, volume...)
	}
	return steps, nil
}

// sortProgression orders one volume of packets by a progression's nesting,
// outermost first (B.12.1).
func sortProgression(steps []packetStep, order int) {
	key := func(s packetStep) [5]int32 {
		switch order {
		case ProgressionRLCP:
			return [5]int32{s.resolution, s.layer, s.component, s.precinct, 0}
		case ProgressionRPCL:
			return [5]int32{s.resolution, s.y, s.x, s.component, s.layer}
		case ProgressionPCRL:
			return [5]int32{s.y, s.x, s.component, s.resolution, s.layer}
		case ProgressionCPRL:
			return [5]int32{s.component, s.y, s.x, s.resolution, s.layer}
		default: // LRCP
			return [5]int32{s.layer, s.resolution, s.component, s.precinct, 0}
		}
	}
	sort.SliceStable(steps, func(i, j int) bool {
		a, b := key(steps[i]), key(steps[j])
		for k := range a {
			if a[k] != b[k] {
				return a[k] < b[k]
			}
		}
		return false
	})
}

// contribution is one codeword segment's part of a packet body.
type contribution struct {
	key       [4]int
	block     int
	length    int
	passes    int
	lastPass  int // the index of the part's last coding pass in its block
	blockTerm bool
}

// readPacket reads one packet's header and body, returning where the next
// packet's header and body begin, and where its header began and ended.
// Without packed headers the two are the same stream. Unless keep, the body
// is skipped rather than attached to its code-blocks.
func (c *Codestream) readPacket(headers []byte, hpos int, body []byte, bpos int, packed bool, tile int, step packetStep, keep bool,
	states map[[4]int]*precinctState, out *TileData, scratch *[]contribution) (int, int, int, int, error) {

	comp, resolution, precinct := int(step.component), int(step.resolution), int(step.precinct)
	style := c.CodingForTile(tile, comp)
	tc := out.Components[comp]
	if resolution >= len(tc.Resolutions) {
		return hpos, bpos, hpos, hpos, nil // this component has fewer resolutions than another
	}
	res := tc.Resolutions[resolution]

	// An SOP marker may precede any packet in the tile data (A.8.1). It
	// carries a sequence number this decoder does not need, so it is skipped.
	if style.SOP && bpos+6 <= len(body) && be16(body[bpos:]) == 0xFF91 {
		bpos += 6
		if !packed {
			hpos = bpos
		}
	}

	headerStart := hpos
	r := newBitReader(headers[hpos:])
	nonEmpty, err := r.readBit()
	if err != nil {
		return 0, 0, 0, 0, err
	}

	contributions := (*scratch)[:0]
	defer func() { *scratch = contributions[:0] }()
	if nonEmpty == 1 {
		for bandIndex, band := range res.Bands {
			if precinct >= len(band.Precincts) || len(band.Precincts[precinct].Blocks) == 0 {
				continue
			}
			area := band.Precincts[precinct]
			key := [4]int{comp, resolution, bandIndex, precinct}
			state := states[key]
			if state == nil {
				// The trees are sized by the code-block grid of this precinct.
				state = &precinctState{
					inclusion: newTagTree(area.BlocksWide, area.BlocksHigh),
					zeroBits:  newTagTree(area.BlocksWide, area.BlocksHigh),
					blocks:    make([]blockState, len(area.Blocks)),
				}
				states[key] = state
			}

			for i := range area.Blocks {
				b := &state.blocks[i]
				included, err := readInclusion(r, state, b, i, int(step.layer))
				if err != nil {
					return 0, 0, 0, 0, err
				}
				if !included {
					continue
				}
				if !b.included {
					zero, err := readZeroBitPlanes(r, state, i)
					if err != nil {
						return 0, 0, 0, 0, err
					}
					b.zeroBitPlanes = zero
					b.lblock = 3
					b.included = true
				}

				passes, err := readPassCount(r)
				if err != nil {
					return 0, 0, 0, 0, err
				}
				if style.BlockStyle&BlockHT != 0 && b.passes+passes > 1 {
					// Their SigProp and MagRef passes, or placeholder passes,
					// take a second codeword segment this reader does not split.
					return 0, 0, 0, 0, &ErrUnsupported{Feature: "HT refinement or placeholder passes"}
				}
				if err := readLblockIncrement(r, b); err != nil {
					return 0, 0, 0, 0, err
				}
				// B.10.7.2: one length per codeword segment the new passes
				// reach, each in Lblock + log2(its passes) bits.
				first, last := b.passes, b.passes+passes-1
				parts := len(contributions)
				for start := first; start <= last; {
					end := start
					for end < last && !terminatesAfter(end, style.BlockStyle) {
						end++
					}
					length, err := r.readBits(b.lblock + log2(end-start+1))
					if err != nil {
						return 0, 0, 0, 0, err
					}
					contributions = append(contributions, contribution{
						key: key, block: i, length: int(length), passes: end - start + 1,
						lastPass: end, blockTerm: terminatesAfter(end, style.BlockStyle),
					})
					start = end + 1
				}
				// Room for the segments these parts may start, so that a
				// block terminated on every pass grows its list once.
				b.segments = slices.Grow(b.segments, len(contributions)-parts)
				b.passes += passes
			}
		}
	}

	r.align()
	hpos += r.offset()

	// An EPH marker closes the header when the coding style says so (A.8.2).
	if style.EPH && hpos+2 <= len(headers) && be16(headers[hpos:]) == 0xFF92 {
		hpos += 2
	}
	headerEnd := hpos
	if !packed {
		bpos = hpos
	}

	for _, part := range contributions {
		if part.length > len(body)-bpos {
			return 0, 0, 0, 0, errorf("packet body runs past the tile: %d bytes with %d left",
				part.length, len(body)-bpos)
		}
		block := &states[part.key].blocks[part.block]
		data := body[bpos : bpos+part.length]
		if !keep {
			bpos += part.length
			continue
		}
		block.kept += part.passes
		if block.open && len(block.segments) > 0 {
			last := &block.segments[len(block.segments)-1]
			last.Data = append(last.Data, data...)
			last.Passes += part.passes
		} else {
			// The tile's own bytes, capped so that a later contribution to
			// this segment copies them rather than write into the next.
			block.segments = append(block.segments, Segment{Data: data[:len(data):len(data)], Passes: part.passes})
		}
		block.open = !part.blockTerm
		bpos += part.length
	}
	return hpos, bpos, headerStart, headerEnd, nil
}

// terminatesAfter says whether the codeword segment ends with coding pass
// pass of a block (passes numbered from 0, the first cleanup pass): after
// every pass with termination on each pass; with the arithmetic coding
// bypass alone, after the tenth pass, which ends the fourth bit plane, and
// then after each raw significance and refinement pair and each cleanup pass
// (D.6, Table D.9).
func terminatesAfter(pass int, style byte) bool {
	switch {
	case style&BlockTermAll != 0:
		return true
	case style&BlockBypass != 0:
		if pass < 10 {
			return pass == 9
		}
		return (pass-10)%3 != 0
	}
	return false
}

// rawPass says whether a coding pass is read without the arithmetic coder:
// with the bypass, the significance and refinement passes after the fourth
// bit plane (D.6).
func rawPass(pass int, style byte) bool {
	return style&BlockBypass != 0 && pass >= 10 && (pass-10)%3 != 2
}

// readInclusion says whether a code-block contributes to this layer (B.10.4).
// A block that has never appeared is signaled through the inclusion tag tree,
// whose value is the first layer it appears in; one that has appeared before
// takes a single bit.
func readInclusion(r *bitReader, state *precinctState, b *blockState, index, layer int) (bool, error) {
	if b.included {
		bit, err := r.readBit()
		return bit == 1, err
	}
	value, known, err := state.inclusion.decode(r, index, int32(layer)+1)
	if err != nil {
		return false, err
	}
	return known && value <= int32(layer), nil
}

// readZeroBitPlanes reads how many leading bit planes of a newly included block
// are all zero (B.10.5). The tag tree is read until the value is final, which
// is what distinguishes it from the inclusion tree's threshold read.
func readZeroBitPlanes(r *bitReader, state *precinctState, index int) (int, error) {
	for threshold := int32(1); threshold < 74; threshold++ {
		value, known, err := state.zeroBits.decode(r, index, threshold)
		if err != nil {
			return 0, err
		}
		if known {
			return int(value), nil
		}
	}
	return 0, errorf("a code-block claims more than 73 zero bit planes")
}

// readPassCount reads the number of coding passes this contribution carries
// (Table B.4). A 0 ends the code, so one pass costs one bit, and the code grows
// in three steps to 164 — every pass a 37-bit-plane block could have.
func readPassCount(r *bitReader) (int, error) {
	first, err := r.readBit()
	if err != nil {
		return 0, err
	}
	if first == 0 {
		return 1, nil
	}
	second, err := r.readBit()
	if err != nil {
		return 0, err
	}
	if second == 0 {
		return 2, nil
	}

	twoBits, err := r.readBits(2)
	if err != nil {
		return 0, err
	}
	if twoBits != 3 {
		return 3 + int(twoBits), nil
	}

	fiveBits, err := r.readBits(5)
	if err != nil {
		return 0, err
	}
	if fiveBits != 31 {
		return 6 + int(fiveBits), nil
	}

	sevenBits, err := r.readBits(7)
	if err != nil {
		return 0, err
	}
	return 37 + int(sevenBits), nil
}

// readLblockIncrement reads the comma code that widens a block's length
// fields (B.10.7.1): the field starts at three bits and grows by the number
// of 1 bits before a 0, so a block whose contributions get longer does not
// pay for a wide field in every packet. The width also grows with the number
// of passes each length covers, since more passes can carry more bytes.
func readLblockIncrement(r *bitReader, b *blockState) error {
	for {
		bit, err := r.readBit()
		if err != nil {
			return err
		}
		if bit == 0 {
			return nil
		}
		b.lblock++
		if b.lblock > 32 {
			return errorf("a code-block length field grew past 32 bits")
		}
	}
}

// sortedKeys lists the precinct keys in component, resolution, band, precinct
// order, so the blocks come out in the order the geometry lists them.
func sortedKeys(states map[[4]int]*precinctState) [][4]int {
	keys := make([][4]int, 0, len(states))
	for key := range states {
		keys = append(keys, key)
	}
	sort.Slice(keys, func(i, j int) bool { return less(keys[i], keys[j]) })
	return keys
}

func less(a, b [4]int) bool {
	for i := range a {
		if a[i] != b[i] {
			return a[i] < b[i]
		}
	}
	return false
}

// ReadTilePacketsForTest exposes ReadTilePackets to tests in this package that
// need to drive it with a modified codestream.
func ReadTilePacketsForTest(c *Codestream, tile int) (*TileData, error) {
	return c.ReadTilePackets(tile)
}
