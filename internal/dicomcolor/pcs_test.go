package dicomcolor

import "testing"

func TestDICOMCIELabReferenceColors(t *testing.T) {
	// The CIELab words for the primaries are the DICOM quantization of
	// CSS Color 4's D50 values used by DCMTK's dcmiod color conversion test.
	for _, tc := range []struct {
		name string
		rgb  [3]int
		lab  [3]uint16
	}{
		{"black", [3]int{0, 0, 0}, [3]uint16{0x0000, 0x8080, 0x8080}},
		{"white", [3]int{255, 255, 255}, [3]uint16{0xffff, 0x8080, 0x8080}},
		{"red", [3]int{255, 0, 0}, [3]uint16{0x8afb, 0xd19f, 0xc6aa}},
		{"green", [3]int{0, 255, 0}, [3]uint16{0xe0d0, 0x30eb, 0xd1d0}},
		{"blue", [3]int{0, 0, 255}, [3]uint16{0x4bb2, 0xc50e, 0x1008}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := RGBToCIELab(tc.rgb); got != tc.lab {
				t.Errorf("RGBToCIELab(%v) = %#04x, want %#04x", tc.rgb, got, tc.lab)
			}
			if got := CIELabToRGB(tc.lab); got != tc.rgb {
				t.Errorf("CIELabToRGB(%#04x) = %v, want %v", tc.lab, got, tc.rgb)
			}
		})
	}
}

func TestDICOMCIELabNeutralGrays(t *testing.T) {
	for value := 0; value <= 255; value++ {
		gray := [3]int{value, value, value}
		lab := RGBToCIELab(gray)
		if lab[1] != 0x8080 || lab[2] != 0x8080 {
			t.Errorf("gray %d encoded with chroma %#04x", value, lab[1:])
		}
		if got := CIELabToRGB(lab); got != gray {
			t.Errorf("gray %d round trip = %v", value, got)
		}
	}
}
