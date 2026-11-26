package dicomcolor

import "testing"

// DCMTK dcmiod/tests/tcielabutil.cc checks the sRGB white endpoint against
// the exact DICOM encoding of the neutral ICC PCS white point.
func TestReferenceAcceptanceDICOMCIELabWhitePoint(t *testing.T) {
	const neutral = 0x8080
	want := [3]uint16{0xffff, neutral, neutral}
	if got := RGBToCIELab([3]int{255, 255, 255}); got != want {
		t.Fatalf("sRGB white encoded as %#04x, want %#04x", got, want)
	}
}
