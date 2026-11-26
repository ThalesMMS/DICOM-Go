package j2kcore

import (
	"context"
	"errors"
	"testing"
)

func TestCodeBlockLimitPrecedesPacketReading(t *testing.T) {
	for name, precincts := range map[string][]byte{
		"default":  nil,
		"multiple": {0x22, 0x33, 0x44},
	} {
		t.Run(name, func(t *testing.T) {
			c := &Codestream{
				Width: 135, Height: 73, XOffset: 3, YOffset: 5,
				TileWidth: 64, TileHeight: 64,
				Components: []Component{{DX: 1, DY: 1, Depth: 8}, {DX: 1, DY: 1, Depth: 8}},
				Coding:     CodingStyle{Levels: 2, CodeBlockWidth: 16, CodeBlockHt: 8, UsePrecincts: precincts != nil, Precincts: precincts},
				TileComponentCoding: map[TileComponentKey]CodingStyle{
					{Tile: 1, Component: 1}: {Levels: 1, CodeBlockWidth: 4, CodeBlockHt: 8, UsePrecincts: precincts != nil, Precincts: precincts},
				},
			}
			maxBlocks := 0
			for tile := 0; tile < c.NumTiles(); tile++ {
				blocks := 0
				for comp := range c.Components {
					tc, err := c.TileComponentFor(tile, comp)
					if err != nil {
						t.Fatal(err)
					}
					for _, res := range tc.Resolutions {
						for _, band := range res.Bands {
							for _, precinct := range band.Precincts {
								blocks += len(precinct.Blocks)
							}
						}
					}
				}
				maxBlocks = maxInt(maxBlocks, blocks)
			}
			if err := (Limits{MaxCodeBlocksPerTile: maxBlocks}).check(c); err != nil {
				t.Fatalf("exact budget: %v", err)
			}
			// No packet bytes are supplied: the geometry must exhaust this budget
			// before the decoder allocates its output or tries to read packets.
			img, err := DecodeContext(context.Background(), c, Limits{MaxCodeBlocksPerTile: maxBlocks - 1})
			var limit *ErrLimit
			if img != nil || !errors.As(err, &limit) || limit.Resource != "code-blocks per tile" || limit.Value <= limit.Limit {
				t.Fatalf("decode: image=%v error=%v", img, err)
			}
		})
	}
}

func TestResolutionSupportsMultiplePrecincts(t *testing.T) {
	for _, tc := range []struct {
		width, height, x, y uint32
		precincts           int
	}{
		{32768, 1, 0, 0, 1},
		{32769, 1, 0, 0, 2},
		{32769, 1, 32767, 0, 2},
		{1, 32769, 0, 32767, 2},
		{32769, 1, 32768, 0, 1},
	} {
		c := &Codestream{
			Width: tc.width, Height: tc.height, XOffset: tc.x, YOffset: tc.y,
			TileWidth: tc.width, TileHeight: tc.height,
			Components: []Component{{DX: 1, DY: 1}},
			Coding:     CodingStyle{CodeBlockWidth: 64, CodeBlockHt: 64},
		}
		component, err := c.TileComponentFor(0, 0)
		if err != nil {
			t.Fatalf("%+v: %v", tc, err)
		}
		if got := component.Resolutions[0].Precincts(); got != tc.precincts {
			t.Errorf("%+v: %d precincts, want %d", tc, got, tc.precincts)
		}
	}
}
