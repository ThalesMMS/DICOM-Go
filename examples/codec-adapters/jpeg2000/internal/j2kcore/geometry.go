// Adapted from github.com/amrshadid/go-dicom compress/jpeg2000/geometry.go at
// commit daecea14be3fbc27300aff20912ed7c8c8a0d599 (v1.6.0, MIT License, see
// LICENSE.amrshadid-go-dicom and PROVENANCE.md in this directory).

package j2kcore

// The geometry of a tile, PS 15444-1 Annex B.
//
// A codestream's compressed bytes mean nothing without the grid they belong to:
// which samples a tile covers, how the wavelet splits them into subbands at
// each resolution, how a band is cut into code-blocks, and which precinct each
// block belongs to. Packet headers name code-blocks by their place in this
// structure, so it has to be built before a single packet can be read.
//
// All coordinates are on the reference grid (B.2): they are absolute, not
// relative to the tile, because the partitions that matter — precincts, code-
// blocks — are anchored at the origin of the grid rather than at the tile.

// Band types, in the order a packet lists them (B.10.2).
const (
	BandLL = 0
	BandHL = 1 // horizontally high-pass, vertically low-pass
	BandLH = 2
	BandHH = 3
)

// CodeBlock is one code-block's place in a band.
type CodeBlock struct {
	X0, Y0, X1, Y1 int
	// Index within the precinct, in raster order, which is the order a packet
	// header walks them in.
	Index int
}

// maxPrecinctsPerResolution bounds the precinct grid a header may declare
// before anything is allocated for it.
const maxPrecinctsPerResolution = 1 << 20

// Width and Height of a code-block in samples.
func (b CodeBlock) Width() int  { return b.X1 - b.X0 }
func (b CodeBlock) Height() int { return b.Y1 - b.Y0 }

// Band is one subband of one resolution.
type Band struct {
	Type           int
	X0, Y0, X1, Y1 int
	// Level is the decomposition level the band came from, counting from the
	// finest: nb in the standard's formulas.
	Level int
	// Precincts holds the band's part of each precinct of the resolution,
	// indexed by precinct number (B.6).
	Precincts []Precinct
}

// Precinct is one precinct's part of a band: its code-blocks in raster order,
// which is the order a packet header walks them in, and the shape of their
// grid, which the precinct's tag trees are built on (B.10.2).
type Precinct struct {
	BlocksWide, BlocksHigh int
	Blocks                 []CodeBlock
}

func (b Band) Width() int  { return b.X1 - b.X0 }
func (b Band) Height() int { return b.Y1 - b.Y0 }

// Resolution is one resolution level of a tile-component: the LL band alone at
// index 0, and the three detail bands that refine it at each index after.
type Resolution struct {
	Index          int
	X0, Y0, X1, Y1 int
	Bands          []Band

	// PPx and PPy are the precinct size as powers of two, and Precincts how
	// many the resolution is divided into.
	PPx, PPy               int
	PrecinctsWide          int
	PrecinctsHigh          int
	CodeBlockW, CodeBlockH int // the exponent actually used, after precincts
}

// Precincts is how many precincts this resolution holds.
func (r Resolution) Precincts() int { return r.PrecinctsWide * r.PrecinctsHigh }

// TileComponent is one component of one tile, decomposed into resolutions.
type TileComponent struct {
	Tile           int
	Component      int
	X0, Y0, X1, Y1 int
	Resolutions    []Resolution
}

func (t TileComponent) Width() int  { return t.X1 - t.X0 }
func (t TileComponent) Height() int { return t.Y1 - t.Y0 }

// TileBounds returns the region of the reference grid a tile covers (B.3):
// the tile's own rectangle clipped to the image.
func (c *Codestream) TileBounds(tile int) (x0, y0, x1, y1 int) {
	across := ceilDiv(c.Width-c.TileXOffset, c.TileWidth)
	if across == 0 {
		return 0, 0, 0, 0
	}
	p := tile % across
	q := tile / across

	x0 = maxInt(int(c.TileXOffset)+p*int(c.TileWidth), int(c.XOffset))
	y0 = maxInt(int(c.TileYOffset)+q*int(c.TileHeight), int(c.YOffset))
	x1 = minInt(int(c.TileXOffset)+(p+1)*int(c.TileWidth), int(c.Width))
	y1 = minInt(int(c.TileYOffset)+(q+1)*int(c.TileHeight), int(c.Height))
	return x0, y0, x1, y1
}

// TileComponentFor builds the full geometry of one component of one tile.
func (c *Codestream) TileComponentFor(tile, component int) (TileComponent, error) {
	tc, err := c.tileComponentGeometry(tile, component)
	if err != nil {
		return TileComponent{}, err
	}
	for r := range tc.Resolutions {
		res := &tc.Resolutions[r]
		bandPPx, bandPPy := res.PPx, res.PPy
		if r > 0 {
			bandPPx, bandPPy = res.PPx-1, res.PPy-1
		}
		// The precinct grid of the bands is the resolution's, halved above
		// resolution 0 (B.6); it is anchored where the resolution's is.
		originX := floorDivPow2(res.X0, res.PPx) << bandPPx
		originY := floorDivPow2(res.Y0, res.PPy) << bandPPy
		for b := range res.Bands {
			band := &res.Bands[b]
			band.Precincts = make([]Precinct, res.Precincts())
			for k := range band.Precincts {
				px := originX + (k%res.PrecinctsWide)<<bandPPx
				py := originY + (k/res.PrecinctsWide)<<bandPPy
				band.Precincts[k] = precinctOf(*band, px, py, px+1<<bandPPx, py+1<<bandPPy, res.CodeBlockW, res.CodeBlockH)
			}
		}
	}
	return tc, nil
}

// tileComponentGeometry resolves band bounds without allocating code-blocks,
// so resource budgets can be checked before constructing packet state.
func (c *Codestream) tileComponentGeometry(tile, component int) (TileComponent, error) {
	if component < 0 || component >= len(c.Components) {
		return TileComponent{}, errorf("component %d of %d", component, len(c.Components))
	}
	if tile < 0 || tile >= c.NumTiles() {
		return TileComponent{}, errorf("tile %d of %d", tile, c.NumTiles())
	}

	tx0, ty0, tx1, ty1 := c.TileBounds(tile)
	comp := c.Components[component]
	tc := TileComponent{
		Tile:      tile,
		Component: component,
		// B.3: the component's own grid, after any subsampling.
		X0: ceilDivInt(tx0, comp.DX), Y0: ceilDivInt(ty0, comp.DY),
		X1: ceilDivInt(tx1, comp.DX), Y1: ceilDivInt(ty1, comp.DY),
	}

	style := c.CodingForTile(tile, component)
	levels := style.Levels

	for r := 0; r <= levels; r++ {
		// B.5: the resolution's grid is the component's, scaled down by the
		// decompositions still to be undone.
		shift := levels - r
		res := Resolution{
			Index: r,
			X0:    ceilDivPow2(tc.X0, shift), Y0: ceilDivPow2(tc.Y0, shift),
			X1: ceilDivPow2(tc.X1, shift), Y1: ceilDivPow2(tc.Y1, shift),
		}
		res.PPx, res.PPy = style.PrecinctExponents(r)

		// B.7: a code-block cannot be larger than a precinct, and at
		// resolutions above zero the precinct is halved on the way into the
		// bands, so the block exponent is one less.
		xcb, ycb := log2(style.CodeBlockWidth), log2(style.CodeBlockHt)
		bandPPx, bandPPy := res.PPx, res.PPy
		if r > 0 {
			bandPPx, bandPPy = res.PPx-1, res.PPy-1
		}
		res.CodeBlockW = minInt(xcb, bandPPx)
		res.CodeBlockH = minInt(ycb, bandPPy)

		if res.X1 > res.X0 && res.Y1 > res.Y0 {
			res.PrecinctsWide = ceilDivPow2(res.X1, res.PPx) - floorDivPow2(res.X0, res.PPx)
			res.PrecinctsHigh = ceilDivPow2(res.Y1, res.PPy) - floorDivPow2(res.Y0, res.PPy)
		}
		if count, ok := mulInt64(int64(res.PrecinctsWide), int64(res.PrecinctsHigh)); !ok || count > maxPrecinctsPerResolution {
			return TileComponent{}, &ErrLimit{Resource: "precincts per resolution", Value: count, Limit: maxPrecinctsPerResolution}
		}

		res.Bands = bandsOf(tc, r, levels)
		tc.Resolutions = append(tc.Resolutions, res)
	}
	return tc, nil
}

// precinctOf cuts the part of a band inside a precinct into code-blocks
// (B.7). The code-block grid is anchored at the origin, and a precinct holds
// whole cells of it, so every block belongs to exactly one precinct.
func precinctOf(band Band, px0, py0, px1, py1, xcb, ycb int) Precinct {
	x0, y0 := maxInt(band.X0, px0), maxInt(band.Y0, py0)
	x1, y1 := minInt(band.X1, px1), minInt(band.Y1, py1)
	if x1 <= x0 || y1 <= y0 {
		return Precinct{}
	}
	w, h := 1<<xcb, 1<<ycb
	var p Precinct
	for y := floorTo(y0, h); y < y1; y += h {
		p.BlocksHigh++
		p.BlocksWide = 0
		for x := floorTo(x0, w); x < x1; x += w {
			p.BlocksWide++
			p.Blocks = append(p.Blocks, CodeBlock{
				X0: maxInt(x, x0), Y0: maxInt(y, y0),
				X1: minInt(x+w, x1), Y1: minInt(y+h, y1),
				Index: len(p.Blocks),
			})
		}
	}
	return p
}

// bandsOf gives the subbands of one resolution (B.5, Table B.1): the LL band
// alone at resolution 0, and HL, LH, HH at every resolution above it.
func bandsOf(tc TileComponent, r, levels int) []Band {
	if r == 0 {
		nb := levels
		return []Band{{
			Type: BandLL, Level: nb,
			X0: bandCoord(tc.X0, nb, 0), Y0: bandCoord(tc.Y0, nb, 0),
			X1: bandCoord(tc.X1, nb, 0), Y1: bandCoord(tc.Y1, nb, 0),
		}}
	}

	nb := levels - r + 1
	out := make([]Band, 0, 3)
	for _, t := range []struct {
		kind     int
		xob, yob int
	}{{BandHL, 1, 0}, {BandLH, 0, 1}, {BandHH, 1, 1}} {
		out = append(out, Band{
			Type: t.kind, Level: nb,
			X0: bandCoord(tc.X0, nb, t.xob), Y0: bandCoord(tc.Y0, nb, t.yob),
			X1: bandCoord(tc.X1, nb, t.xob), Y1: bandCoord(tc.Y1, nb, t.yob),
		})
	}
	return out
}

// bandCoord is the standard's band coordinate formula (B-15):
// ceil((x - 2^(nb-1) * o) / 2^nb), where o is 0 or 1 for the low or high half.
func bandCoord(x, nb, o int) int {
	if nb == 0 {
		return x
	}
	return ceilDivInt(x-(1<<(nb-1))*o, 1<<nb)
}

func ceilDivInt(a, b int) int {
	if b == 0 {
		return 0
	}
	if a >= 0 {
		return (a + b - 1) / b
	}
	// Go truncates toward zero; the standard's ceil is toward positive
	// infinity, and band coordinates can be negative before the tile offset is
	// taken off.
	return -((-a) / b)
}

func ceilDivPow2(a, shift int) int { return ceilDivInt(a, 1<<shift) }

func floorDivPow2(a, shift int) int {
	if a >= 0 {
		return a >> shift
	}
	return -((-a + (1 << shift) - 1) >> shift)
}

// floorTo rounds down to a multiple of size, which is where the partition grid
// line before x sits.
func floorTo(x, size int) int {
	if x >= 0 {
		return x - x%size
	}
	return -(((-x) + size - 1) / size) * size
}

func log2(v int) int {
	n := 0
	for v > 1 {
		v >>= 1
		n++
	}
	return n
}

func minInt(a, b int) int {
	if a < b {
		return a
	}
	return b
}

func maxInt(a, b int) int {
	if a > b {
		return a
	}
	return b
}
