package main

import (
	"encoding/binary"
	"image"
	"os"
	"path/filepath"
	"testing"
)

func TestPNGAndTIFF16PreserveAdjacentSamples(t *testing.T) {
	dir := t.TempDir()
	in := writeFile(t, dir, "image.dcm", precisionImage(t, []uint16{0, 1, 0x1234, 65535}))
	png := filepath.Join(dir, "out.png")
	if code, _, stderr := runTool("-bits", "16", "-window", "32768,65536", in, png); code != 0 {
		t.Fatal(stderr)
	}
	g := readPNG(t, png).(*image.Gray16)
	for i, v := range []uint16{0, 1, 0x1234, 65535} {
		if got := g.Gray16At(i, 0).Y; got != v {
			t.Fatalf("PNG sample %d=%d want %d", i, got, v)
		}
	}
	for _, ext := range []string{".tif", ".tiff"} {
		path := filepath.Join(dir, "out"+ext)
		if code, _, stderr := runTool("-bits", "16", "-window", "32768,65536", in, path); code != 0 {
			t.Fatal(stderr)
		}
		b, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		directory := int(binary.LittleEndian.Uint32(b[4:8]))
		count := int(binary.LittleEndian.Uint16(b[directory:]))
		offset := 0
		for i := 0; i < count; i++ {
			e := b[directory+2+12*i:]
			if binary.LittleEndian.Uint16(e) == 273 {
				offset = int(binary.LittleEndian.Uint32(e[8:]))
			}
		}
		if offset == 0 {
			t.Fatal("missing strip")
		}
		for i, v := range []uint16{0, 1, 0x1234, 65535} {
			if got := binary.LittleEndian.Uint16(b[offset+2*i:]); got != v {
				t.Fatalf("TIFF sample %d=%d want %d", i, got, v)
			}
		}
	}
}

func TestCLITransformsAndRegion(t *testing.T) {
	dir := t.TempDir()
	in := writeFile(t, dir, "image.dcm", precisionImage(t, []uint16{0, 1, 2, 3}))
	out := filepath.Join(dir, "out.png")
	if code, _, stderr := runTool("-window", "2,4", "-rotate", "90", "-flip", "vertical", "-scale", "2", in, out); code != 0 {
		t.Fatal(stderr)
	}
	g := readPNG(t, out).(*image.Gray)
	if g.Bounds() != image.Rect(0, 0, 2, 8) || g.GrayAt(0, 0).Y != 255 || g.GrayAt(1, 7).Y != 0 {
		t.Fatalf("CLI transform %v %v", g.Bounds(), g.Pix)
	}
	if code, _, stderr := runTool("-size", "2,2", in, out); code != 0 {
		t.Fatal(stderr)
	}
	if got := readPNG(t, out).Bounds(); got != image.Rect(0, 0, 2, 2) {
		t.Fatal(got)
	}
	if code, _, stderr := runTool("-window-region", "1,0,2,1", in, out); code != 0 {
		t.Fatal(stderr)
	}
	g = readPNG(t, out).(*image.Gray)
	if g.GrayAt(1, 0).Y != 0 || g.GrayAt(2, 0).Y != 255 {
		t.Fatalf("region window %v", g.Pix)
	}
	if code, _, _ := runTool("-window-region", "3,0,2,1", in, out); code != 1 {
		t.Fatal("invalid region accepted")
	}
}
