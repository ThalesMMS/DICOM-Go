package display

import (
	"encoding/binary"
	"errors"
	"testing"

	"github.com/ThalesMMS/dicom-go/transfer"
)

func TestYBRPartialConversion(t *testing.T) {
	// BT.601 partial range: Y 16 is black, 235 white; chroma 128 is neutral.
	cases := []struct {
		y, cb, cr uint8
		want      [3]uint8
	}{
		{16, 128, 128, [3]uint8{0, 0, 0}},
		{235, 128, 128, [3]uint8{255, 255, 255}},
		{82, 90, 240, [3]uint8{255, 0, 0}},  // BT.601 red
		{145, 54, 34, [3]uint8{0, 255, 0}},  // green
		{41, 240, 110, [3]uint8{0, 0, 255}}, // blue
	}
	for _, tc := range cases {
		frame := ColorFrame{Rows: 1, Columns: 2, Photometric: "YBR_PARTIAL_422", SamplesPerPixel: 3, Format: PixelFormat{BitsAllocated: 8}, Pixels: []byte{tc.y, tc.y, tc.cb, tc.cr}}
		img, err := RenderColor(frame)
		if err != nil {
			t.Fatal(err)
		}
		for x := 0; x < 2; x++ {
			got := [3]uint8{img.Pix[4*x], img.Pix[4*x+1], img.Pix[4*x+2]}
			for c := range got {
				if d := int(got[c]) - int(tc.want[c]); d < -2 || d > 2 {
					t.Fatalf("YCbCr %d/%d/%d pixel %d: %v, want %v", tc.y, tc.cb, tc.cr, x, got, tc.want)
				}
			}
		}
	}
	// A decoder returns 4:2:0 or 4:2:2 at full resolution.
	full := ColorFrame{Rows: 1, Columns: 1, Photometric: "YBR_PARTIAL_420", SamplesPerPixel: 3, Format: PixelFormat{BitsAllocated: 8}, Pixels: []byte{235, 128, 128}}
	if img, err := RenderColor(full); err != nil || img.Pix[0] != 255 {
		t.Fatalf("decoded 4:2:0: %v %v", img, err)
	}
	full.Photometric = "YBR_PARTIAL_422"
	if img, err := RenderColor(full); err != nil || img.Pix[1] != 255 {
		t.Fatalf("decoded 4:2:2: %v %v", img, err)
	}
}

func TestColorAboveEightBits(t *testing.T) {
	le16 := func(values ...uint16) []byte {
		out := make([]byte, 2*len(values))
		for i, v := range values {
			binary.LittleEndian.PutUint16(out[2*i:], v)
		}
		return out
	}
	// 12-bit RGB: 4095 is full scale, 2048 about half.
	rgb := ColorFrame{Rows: 1, Columns: 2, Photometric: "RGB", SamplesPerPixel: 3, Format: PixelFormat{BitsAllocated: 16, BitsStored: 12, HighBit: 11}, Pixels: le16(4095, 2048, 0, 1, 2, 3)}
	img, err := RenderColor(rgb)
	if err != nil {
		t.Fatal(err)
	}
	if img.Pix[0] != 255 || img.Pix[1] != 128 || img.Pix[2] != 0 {
		t.Fatalf("12-bit RGB to 8 bits: %v", img.Pix[:4])
	}
	wide, err := RenderColor16(rgb)
	if err != nil {
		t.Fatal(err)
	}
	channel := func(i int) uint16 { return uint16(wide.Pix[2*i])<<8 | uint16(wide.Pix[2*i+1]) }
	if channel(0) != 65535 || channel(1) != 32776 || channel(4) != 16 || channel(5) != 32 || channel(6) != 48 {
		t.Fatalf("12-bit RGB to 16 bits: %d %d %d %d %d", channel(0), channel(1), channel(4), channel(5), channel(6))
	}
	// Bits above Bits Stored are ignored.
	rgb.Pixels = le16(0xF000|4095, 0, 0, 0, 0, 0)
	if img, _ := RenderColor(rgb); img.Pix[0] != 255 {
		t.Fatalf("high bits leaked: %v", img.Pix[:4])
	}
	// 16-bit planar YBR_FULL: neutral chroma is 32768.
	ybr := ColorFrame{Rows: 1, Columns: 1, Photometric: "YBR_FULL", SamplesPerPixel: 3, PlanarConfiguration: 1, Format: PixelFormat{BitsAllocated: 16, BitsStored: 16, HighBit: 15}, Pixels: le16(40000, 32768, 32768)}
	wide, err = RenderColor16(ybr)
	if err != nil {
		t.Fatal(err)
	}
	if channel(0) != 40000 || channel(1) != 40000 || channel(2) != 40000 {
		t.Fatalf("16-bit YBR_FULL gray: %d %d %d", channel(0), channel(1), channel(2))
	}
	// Big Endian 16-bit YBR_FULL_422.
	be := make([]byte, 8)
	for i, v := range []uint16{65535, 0, 32768, 32768} {
		binary.BigEndian.PutUint16(be[2*i:], v)
	}
	ybr422 := ColorFrame{Rows: 1, Columns: 2, Photometric: "YBR_FULL_422", SamplesPerPixel: 3, Format: PixelFormat{BitsAllocated: 16, BitsStored: 16, HighBit: 15, ByteOrder: binary.BigEndian}, Pixels: be}
	if img, err := RenderColor(ybr422); err != nil || img.Pix[0] != 255 || img.Pix[4] != 0 {
		t.Fatalf("16-bit 4:2:2: %v %v", img, err)
	}
	signed := rgb
	signed.Format.Signed = true
	if _, err := RenderColor(signed); !errors.Is(err, ErrUnsupportedColorLayout) {
		t.Fatalf("signed color: %v", err)
	}
}

func TestRenderColor16Palette(t *testing.T) {
	lut8, _ := NewLUT([]int{2, 0, 8}, []uint16{0, 255})
	lut16, _ := NewLUT([]int{2, 0, 16}, []uint16{0, 1000})
	frame := ColorFrame{Rows: 1, Columns: 2, Photometric: "PALETTE COLOR", SamplesPerPixel: 1, Format: PixelFormat{BitsAllocated: 8, BitsStored: 8, HighBit: 7}, Pixels: []byte{0, 1}, Palette: &PaletteColorLUT{Red: lut8, Green: lut16, Blue: lut8}}
	img, err := RenderColor16(frame)
	if err != nil {
		t.Fatal(err)
	}
	if got := uint16(img.Pix[8])<<8 | uint16(img.Pix[9]); got != 65535 {
		t.Fatalf("8-bit palette red = %d", got)
	}
	if got := uint16(img.Pix[10])<<8 | uint16(img.Pix[11]); got != 1000 {
		t.Fatalf("16-bit palette green = %d", got)
	}
	frame.Palette = nil
	if _, err := RenderColor16(frame); !errors.Is(err, ErrMissingPalette) {
		t.Fatalf("missing palette: %v", err)
	}
}

func TestValidateColorPhotometric(t *testing.T) {
	cases := []struct {
		photometric, syntax string
		ok                  bool
	}{
		{"YBR_PARTIAL_420", transfer.ExplicitVRLittleEndian.UID, false},
		{"YBR_PARTIAL_420", transfer.MPEG2MPML.UID, true},
		{"YBR_PARTIAL_420", transfer.HEVCMP51.UID, true},
		{"YBR_ICT", transfer.JPEG2000.UID, true},
		{"YBR_RCT", transfer.ExplicitVRLittleEndian.UID, false},
		{"YBR_PARTIAL_422", transfer.ExplicitVRLittleEndian.UID, true},
		{"RGB", transfer.ExplicitVRLittleEndian.UID, true},
	}
	for _, tc := range cases {
		err := ValidateColorPhotometric(tc.photometric, tc.syntax)
		if (err == nil) != tc.ok || (err != nil && !errors.Is(err, ErrUnsupportedColorPhotometric)) {
			t.Errorf("%s with %s: %v", tc.photometric, tc.syntax, err)
		}
	}
}
