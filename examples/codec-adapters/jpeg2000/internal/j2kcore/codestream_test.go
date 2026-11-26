package j2kcore

import (
	"reflect"
	"testing"
)

func TestCodingForTileResolvesCODFieldsAndComponentPrecedence(t *testing.T) {
	mainCOD := CodingStyle{Progression: ProgressionLRCP, Layers: 2, MCT: true, SOP: true, CodeBlockWidth: 64}
	tileCOD := CodingStyle{Progression: ProgressionRLCP, Layers: 3, EPH: true, CodeBlockWidth: 32}
	mainCOC := CodingStyle{Levels: 1, CodeBlockWidth: 16}
	tileCOC := CodingStyle{Levels: 2, CodeBlockWidth: 8}
	for _, tc := range []struct {
		name                      string
		mainCOC, tileCOD, tileCOC bool
		wantCOD, wantSP           CodingStyle
	}{
		{"main COD", false, false, false, mainCOD, mainCOD},
		{"main COC", true, false, false, mainCOD, mainCOC},
		{"tile COD over main COC", true, true, false, tileCOD, tileCOD},
		{"tile COC with main COD", true, false, true, mainCOD, tileCOC},
		{"tile COC with tile COD", true, true, true, tileCOD, tileCOC},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c := &Codestream{Coding: mainCOD}
			if tc.mainCOC {
				c.ComponentCoding = map[int]CodingStyle{0: mainCOC}
			}
			if tc.tileCOD {
				c.TileCoding = map[int]CodingStyle{0: tileCOD}
			}
			if tc.tileCOC {
				c.TileComponentCoding = map[TileComponentKey]CodingStyle{{Tile: 0, Component: 0}: tileCOC}
			}
			want := tc.wantSP
			want.Progression, want.Layers, want.MCT = tc.wantCOD.Progression, tc.wantCOD.Layers, tc.wantCOD.MCT
			want.SOP, want.EPH = tc.wantCOD.SOP, tc.wantCOD.EPH
			if got := c.CodingForTile(0, 0); !reflect.DeepEqual(got, want) {
				t.Fatalf("coding = %+v, want %+v", got, want)
			}
		})
	}
}
