package display

import (
	"encoding/binary"
	"errors"
	"image"
	"strings"
	"testing"
	"time"

	"github.com/ThalesMMS/dicom-go/core"
	"github.com/ThalesMMS/dicom-go/object"
)

// TestApplyGray16OverlayWorkIsBoundedByImage is the FuzzPresentationFromObject
// seed "overlay maximal plane": ApplyGray16 walked every declared bit of a
// 65535x65535 overlay carrying two bytes (4.3e9 bit tests per overlay) instead
// of the part that overlaps the image and the data, as Composite does.
func TestApplyGray16OverlayWorkIsBoundedByImage(t *testing.T) {
	origins := [][2]int{{1, 1}, {-32768, -32768}, {32767, 32767}, {-32768, 1}, {1, 32767}}
	pres := Presentation{}
	for _, origin := range origins {
		pres.Overlays = append(pres.Overlays, Overlay{Rows: 65535, Columns: 65535, OriginRow: origin[0], OriginCol: origin[1], Data: []byte{0xFF, 0xFF}})
	}
	img := image.NewGray16(image.Rect(0, 0, 64, 64))
	start := time.Now()
	pres.ApplyGray16(img)
	if elapsed := time.Since(start); elapsed > 5*time.Second {
		t.Fatalf("five huge overlays took %v", elapsed)
	}
	// Only the origin (1,1) overlay reaches the image: the first 16 bits of
	// its first row.
	for x := 0; x < 64; x++ {
		want := uint16(0)
		if x < 16 {
			want = 0xFFFF
		}
		if got := img.Gray16At(x, 0).Y; got != want {
			t.Fatalf("pixel (%d,0) = %#x, want %#x", x, got, want)
		}
		if got := img.Gray16At(x, 1).Y; got != 0 {
			t.Fatalf("pixel (%d,1) = %#x, want 0", x, got)
		}
	}
}

// TestApplyGray16OverlaysMatchComposite checks that the 16-bit burn-in marks
// exactly the pixels Composite marks, including clipped and offset planes.
func TestApplyGray16OverlaysMatchComposite(t *testing.T) {
	overlays := []Overlay{
		{Rows: 4, Columns: 4, OriginRow: 1, OriginCol: 1, Data: []byte{0xA5, 0x5A}},
		{Rows: 5, Columns: 3, OriginRow: -1, OriginCol: 7, Data: []byte{0xFF, 0x7F}},
		{Rows: 3, Columns: 11, OriginRow: 6, OriginCol: -4, Data: []byte{0x0F, 0xF0, 0x33, 0xCC, 0x01}},
		{Rows: 9, Columns: 9, OriginRow: 0, OriginCol: 0, Data: []byte{0xFF}},
	}
	for i, overlay := range overlays {
		gray := image.NewGray(image.Rect(0, 0, 8, 8))
		overlay.Composite(gray, 255)
		gray16 := image.NewGray16(image.Rect(0, 0, 8, 8))
		Presentation{Overlays: []Overlay{overlay}}.ApplyGray16(gray16)
		for y := 0; y < 8; y++ {
			for x := 0; x < 8; x++ {
				set, set16 := gray.GrayAt(x, y).Y == 255, gray16.Gray16At(x, y).Y == 0xFFFF
				if set != set16 {
					t.Fatalf("overlay %d pixel (%d,%d): Composite %v, ApplyGray16 %v", i, x, y, set, set16)
				}
			}
		}
	}
}

// segmentedEmptyCopies is Segmented Palette Color LUT Data of empty discrete
// segments followed by indirect segments that each copy all of them, ending
// with a two-entry discrete segment: conformant in form, but the copies
// re-walk the empty segments empties*copies times.
func segmentedEmptyCopies(empties, copies int) []uint16 {
	words := make([]uint16, 0, 2*empties+4*copies+4)
	for i := 0; i < empties; i++ {
		words = append(words, 0, 0)
	}
	for i := 0; i < copies; i++ {
		words = append(words, 2, uint16(empties), 0, 0)
	}
	return append(words, 0, 2, 100, 200)
}

// TestSegmentedPaletteEmptyCopiesBounded is the FuzzPresentationFromObject
// seed "segmented empty copies" at full element size: indirect segments that
// copy empty segments made the expansion quadratic in the data (8192 x 4096
// segment walks from 64 KiB; minutes for a larger element). Expansion is now
// limited to len(words)+count segments and the table is refused.
func TestSegmentedPaletteEmptyCopiesBounded(t *testing.T) {
	words := segmentedEmptyCopies(8192, 4095)
	data := make([]byte, 2*len(words))
	for i, word := range words {
		binary.LittleEndian.PutUint16(data[2*i:], word)
	}
	start := time.Now()
	_, err := segmentedPaletteEntries(data, 2, binary.LittleEndian)
	if !errors.Is(err, ErrInvalidLUTData) {
		t.Fatalf("err = %v, want ErrInvalidLUTData", err)
	}
	if elapsed := time.Since(start); elapsed > time.Second {
		t.Fatalf("expansion took %v", elapsed)
	}

	// Indirect segments that add entries stay within the budget.
	words = []uint16{0, 1, 5}
	for i := 0; i < 1000; i++ {
		words = append(words, 2, 1, 0, 0)
	}
	data = make([]byte, 2*len(words))
	for i, word := range words {
		binary.LittleEndian.PutUint16(data[2*i:], word)
	}
	entries, err := segmentedPaletteEntries(data, 1001, binary.LittleEndian)
	if err != nil || len(entries) != 1001 || entries[1000] != 5 {
		t.Fatalf("entries = %d, err = %v", len(entries), err)
	}
}

// TestCircleShutterFarGeometry: Center of Circular Shutter and Radius are IS
// values of up to 12 digits, and the squared distance or radius overflowed
// int. A center 2^32 rows away made the pixel under it visible (its squared
// distance wrapped to 0), and a radius of 2^32 masked the whole image.
func TestCircleShutterFarGeometry(t *testing.T) {
	shutter := func(t *testing.T, center, radius string) *Shutter {
		t.Helper()
		obj := object.FromElements([]core.Element{
			ovText(0x0018, 0x1600, core.VRCS, "CIRCULAR"),
			{Header: core.ElementHeader{Tag: tagShutterCircleCtr, VR: core.VRIS}, Value: core.StringValue(strings.Split(center, "\\"))},
			ovText(0x0018, 0x1612, core.VRIS, radius),
		}, nil)
		sh, err := ShutterFromObject(obj)
		if err != nil {
			t.Fatal(err)
		}
		return sh
	}
	cases := []struct {
		name, center, radius string
		wantVisible          bool
	}{
		{name: "center 2^32 rows away", center: "-4294967295\\1", radius: "1"},
		{name: "center 2^32 columns away", center: "1\\4294967297", radius: "1"},
		{name: "far diagonal center", center: "-999999999999\\999999999999", radius: "1"},
		{name: "radius 2^32", center: "1\\1", radius: "4294967296", wantVisible: true},
		{name: "largest IS radius", center: "-99999999999\\1", radius: "999999999999", wantVisible: true},
		{name: "near center", center: "1\\1", radius: "1", wantVisible: true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			sh := shutter(t, tc.center, tc.radius)
			img := image.NewGray(image.Rect(0, 0, 1, 1))
			img.Pix[0] = 200
			sh.Apply(img)
			if visible := img.Pix[0] == 200; visible != tc.wantVisible {
				t.Fatalf("pixel (0,0) visible = %v, want %v", visible, tc.wantVisible)
			}
		})
	}
	if !(CircleShutter{CenterRow: 1, CenterCol: 1, Radius: -2}).inside(1, 1) {
		t.Fatal("a negative radius no longer acts as its magnitude")
	}
	if (CircleShutter{CenterRow: -1 << 63, CenterCol: 1 << 62, Radius: 1<<63 - 1}).inside(0, 0) {
		t.Fatal("extreme center reported inside")
	}
}
