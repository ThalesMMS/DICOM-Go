package jpegxladapter

import (
	"slices"
	"testing"

	"github.com/ThalesMMS/dicom-go/pixeldata"
)

// Signed monochrome samples are stored offset-binary in the codestream
// (value + 2^(BitsStored-1)), as by the base module's pixeldata/jpegxl.
func TestUnshiftSignedRestoresTwosComplement(t *testing.T) {
	m := pixeldata.Metadata{BitsAllocated: 16, BitsStored: 12, PixelRepresentation: 1}
	frame := []byte{0x00, 0x00, 0xff, 0x07, 0x00, 0x08, 0xff, 0x0f} // 0, 2047, 2048, 4095
	unshiftSigned(frame, m)
	want := []byte{0x00, 0xf8, 0xff, 0xff, 0x00, 0x00, 0xff, 0x07} // -2048, -1, 0, 2047
	if !slices.Equal(frame, want) {
		t.Fatalf("16-bit = % x, want % x", frame, want)
	}

	m8 := pixeldata.Metadata{BitsAllocated: 8, BitsStored: 8, PixelRepresentation: 1}
	frame8 := []byte{0, 127, 128, 255}
	unshiftSigned(frame8, m8)
	if want := []byte{0x80, 0xff, 0x00, 0x7f}; !slices.Equal(frame8, want) {
		t.Fatalf("8-bit = % x, want % x", frame8, want)
	}

	unsigned := []byte{1, 2}
	unshiftSigned(unsigned, pixeldata.Metadata{BitsAllocated: 8, BitsStored: 8})
	if !slices.Equal(unsigned, []byte{1, 2}) {
		t.Fatal("unsigned samples changed")
	}
}
