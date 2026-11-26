package jpip

// responsePlan decides which data-bin ranges a view-window needs and in what
// order: headers first, then precinct packets from the coarsest resolution
// and lowest layer up (JPP), or whole tiles (JPT).
type responsePlan struct {
	index          *CodestreamIndex
	stream         uint64
	kind           string
	discard        int
	x0, y0, x1, y1 int64 // region on the reference grid
	comps          []bool
	layers         int
	fsizX, fsizY   int64
	whole          bool
}

type planChunk struct {
	key       dataBinKey
	offset    int64
	data      []byte
	binLength int64
}

func newPlan(index *CodestreamIndex, stream uint64, kind string, w viewWindow) *responsePlan {
	size := index.size
	p := &responsePlan{
		index: index, stream: stream, kind: kind, comps: w.comps,
		x0: size.x0, y0: size.y0, x1: size.x1, y1: size.y1,
		layers: index.layers,
	}
	if w.layers >= 0 && w.layers < p.layers {
		p.layers = w.layers
	}
	if w.explicitSize {
		p.discard = chooseDiscardLevels(size, index.levels, w.fx, w.fy, w.round)
		d := uint(p.discard)
		ox := ceilDiv(size.x0, 1<<d)
		oy := ceilDiv(size.y0, 1<<d)
		if w.hasOffset || w.hasRegion {
			rx0, ry0 := (ox+w.ox)<<d, (oy+w.oy)<<d
			rx1, ry1 := size.x1, size.y1
			if w.hasRegion {
				rx1, ry1 = (ox+w.ox+w.sx)<<d, (oy+w.oy+w.sy)<<d
			}
			p.x0, p.y0 = max(rx0, size.x0), max(ry0, size.y0)
			p.x1, p.y1 = min(rx1, size.x1), min(ry1, size.y1)
		}
	}
	p.fsizX, p.fsizY = reducedSize(size, p.discard)
	allComps := true
	for c := range size.comps {
		allComps = allComps && p.component(c)
	}
	p.whole = p.discard == 0 && allComps && p.layers >= index.layers &&
		p.x0 == size.x0 && p.y0 == size.y0 && p.x1 == size.x1 && p.y1 == size.y1
	return p
}

func reducedSize(size imageSize, d int) (int64, int64) {
	scale := int64(1) << uint(d)
	return ceilDiv(size.x1, scale) - ceilDiv(size.x0, scale), ceilDiv(size.y1, scale) - ceilDiv(size.y0, scale)
}

// chooseDiscardLevels maps fsiz to a number of discarded resolution levels
// (ISO/IEC 15444-9 C.4.2): round-down keeps the largest size that fits,
// round-up the smallest that covers, closest the nearest.
func chooseDiscardLevels(size imageSize, levels int, fx, fy int64, round string) int {
	switch round {
	case "round-up":
		for d := levels; d >= 0; d-- {
			if w, h := reducedSize(size, d); w >= fx && h >= fy {
				return d
			}
		}
		return 0
	case "closest":
		best, bestDistance := 0, int64(-1)
		for d := 0; d <= levels; d++ {
			w, h := reducedSize(size, d)
			distance := abs64(w-fx) + abs64(h-fy)
			if bestDistance < 0 || distance < bestDistance {
				best, bestDistance = d, distance
			}
		}
		return best
	default:
		for d := 0; d <= levels; d++ {
			if w, h := reducedSize(size, d); w <= fx && h <= fy {
				return d
			}
		}
		return levels
	}
}

func abs64(v int64) int64 {
	if v < 0 {
		return -v
	}
	return v
}

func (p *responsePlan) component(c int) bool {
	return p.comps == nil || c < len(p.comps) && p.comps[c]
}

func (p *responsePlan) intersects(x0, y0, x1, y1 int64) bool {
	return x0 < p.x1 && p.x0 < x1 && y0 < p.y1 && p.y0 < y1
}

// each yields the planned chunks until fn returns false.
func (p *responsePlan) each(fn func(planChunk) bool) {
	x := p.index
	main := x.mainHeader()
	if !fn(planChunk{key: dataBinKey{p.stream, ClassMainHeader, 0}, data: main, binLength: int64(len(main))}) {
		return
	}
	// A raw codestream has no metadata; an empty metadata-bin 0 says so.
	if !fn(planChunk{key: dataBinKey{p.stream, ClassMetadata, 0}}) {
		return
	}
	var tiles []int
	for t := range x.tiles {
		g := &x.tiles[t].geometry
		if p.intersects(g.x0, g.y0, g.x1, g.y1) {
			tiles = append(tiles, t)
		}
	}
	if p.kind == "jpt-stream" {
		for _, t := range tiles {
			tile := &x.tiles[t]
			offset := int64(0)
			for _, part := range tile.parts {
				chunk := planChunk{
					key: dataBinKey{p.stream, ClassTile, uint64(t)}, offset: offset,
					data: x.data[part.Start:part.End], binLength: int64(tile.length),
				}
				if !fn(chunk) {
					return
				}
				offset += int64(part.End - part.Start)
			}
		}
		return
	}
	for _, t := range tiles {
		header := x.tiles[t].header
		if !fn(planChunk{key: dataBinKey{p.stream, ClassTileHeader, uint64(t)}, data: header, binLength: int64(len(header))}) {
			return
		}
	}
	type selected struct {
		precinct *indexedPrecinct
		offset   int64
	}
	byResolution := make([][]selected, maxDecompositionLevels+1)
	for _, t := range tiles {
		tile := &x.tiles[t]
		g := &tile.geometry
		for i := range tile.precincts {
			prec := &tile.precincts[i]
			comp := &g.components[prec.c]
			if !p.component(prec.c) || prec.r > max(comp.coding.levels-p.discard, 0) {
				continue
			}
			if !p.intersects(g.precinctBounds(prec.c, prec.r, prec.k)) {
				continue
			}
			byResolution[prec.r] = append(byResolution[prec.r], selected{precinct: prec})
		}
	}
	for r := range byResolution {
		group := byResolution[r]
		for l := 0; l < p.layers; l++ {
			for i := range group {
				sel := &group[i]
				if l >= len(sel.precinct.packets) {
					continue
				}
				span := sel.precinct.packets[l]
				chunk := planChunk{
					key:    dataBinKey{p.stream, ClassPrecinct, sel.precinct.id},
					offset: sel.offset, data: x.data[span.start:span.end],
					binLength: int64(sel.precinct.length),
				}
				sel.offset += int64(span.len())
				if !fn(chunk) {
					return
				}
			}
		}
	}
}

type responseBody struct {
	data     []byte
	messages int
	eor      EORReason
}

// write serializes the plan, skipping ranges the cache model already holds
// and recording what it sends. length is the client's len (negative when
// absent); maxBytes is the server's response limit.
func (p *responsePlan) write(model map[dataBinKey]int64, length, maxBytes int) responseBody {
	limit, reason := maxBytes-3, EORResponseLimitReached
	if length >= 0 && length < limit {
		limit, reason = length, EORByteLimitReached
	}
	var body responseBody
	stopped := false
	p.each(func(c planChunk) bool {
		sent, known := model[c.key]
		end := c.offset + int64(len(c.data))
		if known && sent >= end {
			return true
		}
		if sent > c.offset {
			c.data = c.data[sent-c.offset:]
			c.offset = sent
		}
		m := Message{Class: c.key.class, Stream: c.key.stream, ID: c.key.id, Offset: uint64(c.offset)}
		m.Complete = end == c.binLength
		header := len(appendMessageHeader(nil, m, len(c.data)))
		if len(body.data)+header+len(c.data) > limit {
			available := limit - len(body.data) - header
			if available > 0 {
				m.Complete = false
				m.Data = c.data[:available]
				body.data = AppendMessage(body.data, m)
				body.messages++
				model[c.key] = c.offset + int64(available)
			}
			stopped = true
			return false
		}
		m.Data = c.data
		body.data = AppendMessage(body.data, m)
		body.messages++
		model[c.key] = end
		return true
	})
	switch {
	case stopped:
		body.eor = reason
	case p.whole:
		body.eor = EORImageDone
	default:
		body.eor = EORWindowDone
	}
	body.data = AppendEOR(body.data, body.eor)
	return body
}
