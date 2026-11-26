package siemens

import (
	"errors"
	"testing"
)

const testProtocol = `<XProtocol> { }
### ASCCONV BEGIN object=MrProtDataImpl@MrProtocolData version=51130001 ###
ulVersion                                = 0x14b44b6
tProtocolName                            = ""ep2d_bold # moco""
sSliceArray.asSlice[0].sPosition.dSag    = -3.7	# comment
sSliceArray.asSlice[0].sPosition.dTra    = -38.5
sSliceArray.asSlice[0].sNormal.dCor      = -0.31
sSliceArray.asSlice[0].sNormal.dTra      = 0.95
sSliceArray.asSlice[0].dThickness        = 4.9
sSliceArray.asSlice[1].sPosition.dTra    = -32.4
sSliceArray.asSlice[1].sNormal.dTra      = 1
sSliceArray.asSlice[1].dInPlaneRot       = 0.5
sSliceArray.lSize                        = 2
not an entry
### ASCCONV END ###
sSliceArray.lSize = 99
`

func TestParseASCCONV(t *testing.T) {
	p, err := ParseASCCONV(testProtocol)
	if err != nil {
		t.Fatal(err)
	}
	if p.Len() != 11 {
		t.Fatalf("%d entries", p.Len())
	}
	if name, ok := p.String("tProtocolName"); !ok || name != "ep2d_bold # moco" {
		t.Fatalf("tProtocolName = %q", name)
	}
	if v, ok, err := p.Int("ulVersion"); !ok || err != nil || v != 0x14b44b6 {
		t.Fatalf("ulVersion = %d, %v", v, err)
	}
	if v, ok, err := p.Float64("sSliceArray.asSlice[0].sPosition.dSag"); !ok || err != nil || v != -3.7 {
		t.Fatalf("dSag = %v, %v", v, err)
	}
	slices, err := p.SliceArray()
	if err != nil || len(slices) != 2 {
		t.Fatalf("slices %v, %v", slices, err)
	}
	if slices[0].Position != [3]float64{-3.7, 0, -38.5} || slices[0].Normal != [3]float64{0, -0.31, 0.95} || slices[0].Thickness != 4.9 {
		t.Fatalf("slice 0 = %+v", slices[0])
	}
	if slices[1].Position != [3]float64{0, 0, -32.4} || slices[1].InPlaneRotation != 0.5 {
		t.Fatalf("slice 1 = %+v", slices[1])
	}
}

func TestParseASCCONVMissingBlock(t *testing.T) {
	for _, text := range []string{"", "no protocol", "### ASCCONV BEGIN ###\nkey = 1\n"} {
		if _, err := ParseASCCONV(text); !errors.Is(err, ErrNoASCCONV) {
			t.Errorf("%q: %v", text, err)
		}
	}
	p, err := ParseASCCONV("### ASCCONV BEGIN ###\n### ASCCONV END ###")
	if err != nil || p.Len() != 0 {
		t.Fatalf("empty block: %v", err)
	}
	if slices, err := p.SliceArray(); err != nil || slices != nil {
		t.Fatalf("no slice array: %v %v", slices, err)
	}
	huge, _ := ParseASCCONV("### ASCCONV BEGIN ###\nsSliceArray.lSize = 100000\n### ASCCONV END ###")
	if _, err := huge.SliceArray(); err == nil {
		t.Fatal("accepted a huge slice array")
	}
}

func FuzzParseASCCONV(f *testing.F) {
	f.Add(testProtocol)
	f.Fuzz(func(t *testing.T, text string) {
		p, err := ParseASCCONV(text)
		if err != nil {
			return
		}
		_, _ = p.SliceArray()
	})
}
