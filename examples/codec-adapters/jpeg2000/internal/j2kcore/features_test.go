package j2kcore

import (
	"errors"
	"reflect"
	"testing"
)

func TestCodewordSegmentBoundaries(t *testing.T) {
	// Passes from 0: the first cleanup, then significance, refinement and
	// cleanup per bit plane.
	var bypassEnds, rawPasses []int
	for pass := 0; pass < 20; pass++ {
		if terminatesAfter(pass, BlockBypass) {
			bypassEnds = append(bypassEnds, pass)
		}
		if rawPass(pass, BlockBypass) {
			rawPasses = append(rawPasses, pass)
		}
		if !terminatesAfter(pass, BlockTermAll) || !terminatesAfter(pass, BlockTermAll|BlockBypass) {
			t.Fatalf("pass %d does not end a segment with termination on each pass", pass)
		}
		if terminatesAfter(pass, 0) || terminatesAfter(pass, BlockVSC|BlockReset|BlockSegSym|BlockPTerm) || rawPass(pass, BlockTermAll) {
			t.Fatalf("pass %d: segments or raw coding without the bypass or termination", pass)
		}
	}
	// The first ten passes (four bit planes) are one arithmetic segment; after
	// them each raw significance and refinement pair, and each cleanup, is one.
	if want := []int{9, 11, 12, 14, 15, 17, 18}; !reflect.DeepEqual(bypassEnds, want) {
		t.Fatalf("bypass segments end after %v, want %v", bypassEnds, want)
	}
	if want := []int{10, 11, 13, 14, 16, 17, 19}; !reflect.DeepEqual(rawPasses, want) {
		t.Fatalf("raw passes %v, want %v", rawPasses, want)
	}
}

func TestRawDecoderUnstuffsAfterFF(t *testing.T) {
	var r rawDecoder
	// 0xFF gives eight ones; the next byte holds seven data bits, its top
	// bit stuffed; past the end come ones.
	r.start([]byte{0xFF, 0x2A, 0x80})
	var bits []int
	for i := 0; i < 8+7+8+3; i++ {
		bits = append(bits, r.bit())
	}
	want := []int{1, 1, 1, 1, 1, 1, 1, 1, 0, 1, 0, 1, 0, 1, 0, 1, 0, 0, 0, 0, 0, 0, 0, 1, 1, 1}
	if !reflect.DeepEqual(bits, want) {
		t.Fatalf("bits %v, want %v", bits, want)
	}
}

func TestParsePOC(t *testing.T) {
	c := &Codestream{Components: make([]Component, 3)}
	changes, err := parsePOC(c, []byte{0, 1, 0, 2, 5, 0, ProgressionRPCL, 1, 0, 0, 7, 33, 3, ProgressionCPRL})
	if err != nil {
		t.Fatal(err)
	}
	want := []ProgressionChange{
		{ResolutionStart: 0, ComponentStart: 1, LayerEnd: 2, ResolutionEnd: 5, ComponentEnd: 256, Order: ProgressionRPCL},
		{ResolutionStart: 1, ComponentStart: 0, LayerEnd: 7, ResolutionEnd: 33, ComponentEnd: 3, Order: ProgressionCPRL},
	}
	if !reflect.DeepEqual(changes, want) {
		t.Fatalf("changes %+v, want %+v", changes, want)
	}
	for name, segment := range map[string][]byte{
		"partial entry": {0, 0, 0, 1, 5, 3},
		"bad order":     {0, 0, 0, 1, 5, 3, 5},
	} {
		if _, err := parsePOC(c, segment); err == nil {
			t.Errorf("%s: parsed", name)
		}
	}
	if _, err := parsePOC(&Codestream{}, []byte{0, 0, 0, 1, 5, 3, 0}); err == nil {
		t.Error("POC before SIZ parsed")
	}
}

func TestAssignPPM(t *testing.T) {
	c := &Codestream{
		TileParts: []TilePart{{Tile: 0}, {Tile: 1}},
		// Two PPM segments, the second continuing the first's run.
		ppm: map[int][]byte{0: {0, 0, 0, 2, 0xA, 0xB, 0, 0}, 1: {0, 1, 0xC}},
	}
	if err := assignPPM(c); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(c.TileParts[0].Headers, []byte{0xA, 0xB}) || !reflect.DeepEqual(c.TileParts[1].Headers, []byte{0xC}) || !c.TileParts[1].PackedHeaders {
		t.Fatalf("tile-parts %+v", c.TileParts)
	}
	short := &Codestream{TileParts: []TilePart{{}, {}}, ppm: map[int][]byte{0: {0, 0, 0, 1, 0xA}}}
	if err := assignPPM(short); err == nil {
		t.Fatal("PPM with runs for one of two tile-parts accepted")
	}
	both := &Codestream{TileParts: []TilePart{{PackedHeaders: true}}, ppm: map[int][]byte{0: {0, 0, 0, 0}}}
	if err := assignPPM(both); err == nil {
		t.Fatal("PPM and PPT together accepted")
	}
}

// precinctCodestream is a one-tile, one-component header with user-defined
// precincts, 32x32 code-blocks and three decomposition levels.
func precinctCodestream(width, height, x0, y0 uint32, precincts []byte) *Codestream {
	return &Codestream{
		Width: width, Height: height, XOffset: x0, YOffset: y0,
		TileWidth: width, TileHeight: height,
		Components: []Component{{Depth: 8, DX: 1, DY: 1}},
		Coding: CodingStyle{
			Layers: 1, Levels: 3, CodeBlockWidth: 32, CodeBlockHt: 32, Wavelet: Wavelet53Reversible,
			UsePrecincts: precincts != nil, Precincts: precincts,
		},
	}
}

func TestPrecinctsPartitionEveryBand(t *testing.T) {
	c := precinctCodestream(203, 150, 7, 3, []byte{0x33, 0x44, 0x45, 0x65})
	tc, err := c.TileComponentFor(0, 0)
	if err != nil {
		t.Fatal(err)
	}
	for r, res := range tc.Resolutions {
		ppx, ppy := c.Coding.PrecinctExponents(r)
		if res.PPx != ppx || res.PPy != ppy {
			t.Fatalf("resolution %d precincts 2^%d x 2^%d, want 2^%d x 2^%d", r, res.PPx, res.PPy, ppx, ppy)
		}
		for _, band := range res.Bands {
			covered := 0
			for _, p := range band.Precincts {
				if len(p.Blocks) != p.BlocksWide*p.BlocksHigh {
					t.Fatalf("resolution %d band %d: %d blocks for a %dx%d grid", r, band.Type, len(p.Blocks), p.BlocksWide, p.BlocksHigh)
				}
				for i, b := range p.Blocks {
					if b.Index != i || b.Width() <= 0 || b.Height() <= 0 || b.Width() > 1<<res.CodeBlockW || b.Height() > 1<<res.CodeBlockH {
						t.Fatalf("resolution %d band %d block %+v", r, band.Type, b)
					}
					if b.X0 < band.X0 || b.Y0 < band.Y0 || b.X1 > band.X1 || b.Y1 > band.Y1 {
						t.Fatalf("resolution %d band %d block %+v outside the band", r, band.Type, b)
					}
					covered += b.Width() * b.Height()
				}
			}
			if covered != band.Width()*band.Height() {
				t.Fatalf("resolution %d band %d: blocks cover %d of %d samples", r, band.Type, covered, band.Width()*band.Height())
			}
		}
	}
	// Default precincts: one per resolution.
	tc, err = precinctCodestream(203, 150, 7, 3, nil).TileComponentFor(0, 0)
	if err != nil {
		t.Fatal(err)
	}
	for r, res := range tc.Resolutions {
		if res.Precincts() != 1 {
			t.Fatalf("resolution %d has %d default precincts", r, res.Precincts())
		}
	}
}

func TestPrecinctAndPacketLimits(t *testing.T) {
	// 1x1 precincts at resolution 0 of a large image.
	c := precinctCodestream(1<<15, 1<<15, 0, 0, []byte{0x00, 0x11, 0x11, 0x11})
	var limit *ErrLimit
	if _, err := c.TileComponentFor(0, 0); !errors.As(err, &limit) || limit.Resource != "precincts per resolution" {
		t.Fatalf("precinct limit: %v", err)
	}
	// Many small precincts over many layers: more packets than the budget.
	c = precinctCodestream(1<<12, 1<<12, 0, 0, []byte{0x22, 0x22, 0x22, 0x22})
	c.Coding.Layers = 200
	tc, err := c.TileComponentFor(0, 0)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := c.packetSequence(0, []TileComponent{tc}, 100); !errors.As(err, &limit) || limit.Resource != "packets per tile" {
		t.Fatalf("packet limit: %v", err)
	}
}

func TestProgressionOrders(t *testing.T) {
	// Two components, two resolutions, two layers; resolution 1 has two
	// precincts side by side.
	steps := []packetStep{}
	for l := int32(0); l < 2; l++ {
		for c := int32(0); c < 2; c++ {
			steps = append(steps, packetStep{layer: l, resolution: 0, component: c, precinct: 0, x: 0, y: 0})
			steps = append(steps, packetStep{layer: l, resolution: 1, component: c, precinct: 0, x: 0, y: 0})
			steps = append(steps, packetStep{layer: l, resolution: 1, component: c, precinct: 1, x: 8, y: 0})
		}
	}
	label := func(s []packetStep) []string {
		out := make([]string, len(s))
		for i, p := range s {
			out[i] = string(rune('0'+p.layer)) + string(rune('0'+p.resolution)) + string(rune('0'+p.component)) + string(rune('0'+p.precinct))
		}
		return out
	}
	for order, want := range map[int][]string{
		// layer resolution component precinct
		ProgressionLRCP: {"0000", "0010", "0100", "0101", "0110", "0111", "1000", "1010", "1100", "1101", "1110", "1111"},
		ProgressionRLCP: {"0000", "0010", "1000", "1010", "0100", "0101", "0110", "0111", "1100", "1101", "1110", "1111"},
		ProgressionRPCL: {"0000", "1000", "0010", "1010", "0100", "1100", "0110", "1110", "0101", "1101", "0111", "1111"},
		ProgressionPCRL: {"0000", "1000", "0100", "1100", "0010", "1010", "0110", "1110", "0101", "1101", "0111", "1111"},
		ProgressionCPRL: {"0000", "1000", "0100", "1100", "0101", "1101", "0010", "1010", "0110", "1110", "0111", "1111"},
	} {
		sorted := append([]packetStep(nil), steps...)
		sortProgression(sorted, order)
		if got := label(sorted); !reflect.DeepEqual(got, want) {
			t.Errorf("order %d: %v, want %v", order, got, want)
		}
	}
}

func TestDescaleROI(t *testing.T) {
	// Values in halves, shift 3: the region starts at a magnitude of 2^3,
	// 16 halves.
	got := []int32{
		15, -15, // background, the largest: unchanged
		1, 0, // background
		16<<1 | 1, -(16<<1 | 1), // 16 decoded through every plane: the estimate is restored
		17 << 4,   // 17, truncated with its estimate above the discarded planes: kept
		40<<4 | 8, // 40, truncated at the last plane the shift keeps
	}
	want := []int32{15, -15, 1, 0, 2<<1 | 1, -(2<<1 | 1), 17 << 1, 40<<1 | 1}
	descaleROI(got, 3)
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("descaled %v, want %v", got, want)
	}
	large := []int32{1 << 29, -(1 << 29)}
	descaleROI(large, 30)
	if large[0] != 1<<29 || large[1] != -(1<<29) {
		t.Fatalf("a shift of 30 changed background values: %v", large)
	}
}

func TestParseRGN(t *testing.T) {
	c := &Codestream{Components: make([]Component, 3)}
	if component, shift, err := parseRGN(c, []byte{2, 0, 7}); err != nil || component != 2 || shift != 7 {
		t.Fatalf("RGN: component %d shift %d, %v", component, shift, err)
	}
	wide := &Codestream{Components: make([]Component, 300)}
	if component, shift, err := parseRGN(wide, []byte{0x01, 0x10, 0, 9}); err != nil || component != 272 || shift != 9 {
		t.Fatalf("RGN with a two-byte index: component %d shift %d, %v", component, shift, err)
	}
	var unsupported *ErrUnsupported
	if _, _, err := parseRGN(c, []byte{0, 1, 7}); !errors.As(err, &unsupported) {
		t.Fatalf("RGN style 1: %v, want ErrUnsupported", err)
	}
	for name, segment := range map[string][]byte{
		"component out of range": {3, 0, 7},
		"short":                  {0, 0},
		"long":                   {0, 0, 7, 0},
	} {
		if _, _, err := parseRGN(c, segment); err == nil || errors.As(err, &unsupported) {
			t.Fatalf("%s: %v, want a malformed-segment error", name, err)
		}
	}
	if _, _, err := parseRGN(&Codestream{}, []byte{0, 0, 7}); err == nil {
		t.Fatal("RGN before SIZ was accepted")
	}
}

func TestOutputSizeWithSubsamplingAndReduction(t *testing.T) {
	c := &Codestream{Width: 73, Height: 45, XOffset: 3, YOffset: 1, Components: []Component{{DX: 2, DY: 3}, {DX: 4, DY: 3}}}
	for reduce, want := range [][2]int{{35, 14}, {18, 7}, {9, 3}} {
		w, h := c.OutputSize(reduce)
		if w != want[0] || h != want[1] {
			t.Errorf("reduce %d: %dx%d, want %dx%d", reduce, w, h, want[0], want[1])
		}
	}
}
