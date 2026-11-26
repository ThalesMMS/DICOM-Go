package dicomcolor

import "testing"

// The neutral PCS endpoints and color cube follow the behaviors checked by
// DCMTK dcmpstat_graphic_layer_rgb_cielab (dcmpstat/tests/tgl.cc) for 8-bit sRGB.
func TestReferenceDICOMCIELabEightBitColorCube(t *testing.T) {
	t.Run("neutral endpoints", func(t *testing.T) {
		if got := RGBToCIELab([3]int{}); got != [3]uint16{0, 0x8080, 0x8080} {
			t.Errorf("black encoded as %#04x, want [0x0000 0x8080 0x8080]", got)
		}
		for _, test := range []struct {
			rgb [3]int
			lab [3]uint16
		}{
			{[3]int{0, 0, 0}, [3]uint16{0, 0x8080, 0x8080}},
			{[3]int{255, 255, 255}, [3]uint16{0xffff, 0x8080, 0x8080}},
		} {
			if got := CIELabToRGB(test.lab); got != test.rgb {
				t.Errorf("decode %#04x: got %v, want %v", test.lab, got, test.rgb)
			}
		}
	})
	t.Run("out of gamut clips saturated channels", func(t *testing.T) {
		// L*=100, a*=b*=-128 requires more green and blue than sRGB can represent.
		if got := CIELabToRGB([3]uint16{0xffff, 0, 0}); got[1] != 255 || got[2] != 255 || got[0] < 0 || got[0] > 255 {
			t.Fatalf("light out-of-gamut CIELab decoded as %v", got)
		}
	})
	t.Run("exact sRGB round trip", func(t *testing.T) {
		for r := 0; r <= 255; r += 15 {
			for g := 0; g <= 255; g += 15 {
				for b := 0; b <= 255; b += 15 {
					want := [3]int{r, g, b}
					pcs := RGBToCIELab(want)
					if got := CIELabToRGB(pcs); got != want {
						t.Fatalf("sRGB %v through PCS %#04x returned %v", want, pcs, got)
					}
				}
			}
		}
	})
}
