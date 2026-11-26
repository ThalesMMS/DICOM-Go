package encapimage

import (
	"bytes"
	"context"
	"encoding/binary"
	"testing"
)

func bmpInput(bits int, top bool) []byte {
	w, h := 2, 2
	stride := ((w*bits + 31) / 32) * 4
	off := 54
	if bits == 8 {
		off += 8
	}
	b := make([]byte, off+h*stride)
	copy(b, "BM")
	binary.LittleEndian.PutUint32(b[2:], uint32(len(b)))
	binary.LittleEndian.PutUint32(b[10:], uint32(off))
	binary.LittleEndian.PutUint32(b[14:], 40)
	binary.LittleEndian.PutUint32(b[18:], 2)
	height := int32(2)
	if top {
		height = -2
	}
	binary.LittleEndian.PutUint32(b[22:], uint32(height))
	binary.LittleEndian.PutUint16(b[26:], 1)
	binary.LittleEndian.PutUint16(b[28:], uint16(bits))
	if bits == 8 {
		binary.LittleEndian.PutUint32(b[46:], 2)
		copy(b[54:], []byte{0, 0, 255, 0, 255, 0, 0, 0})
	}
	for y := 0; y < h; y++ {
		sy := y
		if !top {
			sy = h - 1 - y
		}
		line := b[off+sy*stride:]
		for x := 0; x < w; x++ {
			red := (x+y)%2 == 0
			if bits == 8 {
				if !red {
					line[x] = 1
				}
			} else {
				at := x * (bits / 8)
				if red {
					line[at+2] = 255
				} else {
					line[at] = 255
				}
			}
		}
	}
	return b
}
func TestFromImagesBMPOrientationPaletteAndBGRX(t *testing.T) {
	want := []byte{255, 0, 0, 0, 0, 255, 0, 0, 255, 255, 0, 0}
	for _, bits := range []int{8, 24, 32} {
		for _, top := range []bool{false, true} {
			f, r := imported(t, [][]byte{bmpInput(bits, top)}, ImageOptions{})
			if r.Format != BMP || !bytes.Equal(nativeBytes(t, part10Roundtrip(t, f)), want) {
				t.Fatalf("BMP bits=%d top=%t", bits, top)
			}
		}
	}
}
func TestFromImagesPNMExactASCIIAndBinary(t *testing.T) {
	cases := []struct {
		name       string
		data, want []byte
		bits       int
	}{
		{"p2", []byte("P2\n# own samples\n2 2\n255\n0 23 128 255\n"), []byte{0, 23, 128, 255}, 8},
		{"p3", []byte("P3 2 1 255\n1 2 3 254 253 252"), []byte{1, 2, 3, 254, 253, 252}, 8},
		{"p5-first-whitespace", append([]byte("P5\n2 1\n255\n"), 10, 35), []byte{10, 35}, 8},
		{"p6-16", append([]byte("P6\n1 1\n65535\n"), 0x12, 0x34, 0xff, 0xff, 0, 1), []byte{0x34, 0x12, 0xff, 0xff, 1, 0}, 16},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f, r := imported(t, [][]byte{tc.data}, ImageOptions{})
			if r.BitsStored != tc.bits || !bytes.Equal(nativeBytes(t, part10Roundtrip(t, f))[:len(tc.want)], tc.want) {
				t.Fatal("portable samples changed")
			}
		})
	}
}
func TestFromImagesPGXPrecisionSignedAndEndian(t *testing.T) {
	for _, tc := range []struct {
		data, want []byte
		bits       int
		signed     int
	}{
		{append([]byte("PG ML + 12 2 1\n"), 0x01, 0x23, 0x0f, 0xff), []byte{0x23, 1, 0xff, 0x0f}, 12, 0},
		{append([]byte("PG LM - 16 2 1\n"), 0xff, 0xff, 0x00, 0x80), []byte{0xff, 0xff, 0x00, 0x80}, 16, 1},
	} {
		f, r := imported(t, [][]byte{tc.data}, ImageOptions{})
		if r.Format != PGX || r.BitsStored != tc.bits || getUS(t, f.Dataset, tagPixelRepresentation) != tc.signed || !bytes.Equal(nativeBytes(t, part10Roundtrip(t, f)), tc.want) {
			t.Fatal("PGX samples changed")
		}
	}
}
func TestFromImagesRawOffsetEndianAllocatedAndFrames(t *testing.T) {
	b := []byte{99, 98, 0, 23, 0, 255, 0, 1, 0, 128}
	o := ImageOptions{Format: Raw, Raw: RawOptions{Rows: 1, Columns: 2, Frames: 2, Samples: 1, BitsAllocated: 16, BitsStored: 12, ByteOrder: "big", Offset: 2}}
	f, r := imported(t, [][]byte{b}, o)
	if r.Frames != 2 || getUS(t, f.Dataset, tagBitsAllocated) != 16 || getString(f.Dataset, tagSOPClassUID) != MultiFrameGrayscaleWordSC || !bytes.Equal(nativeBytes(t, part10Roundtrip(t, f)), []byte{23, 0, 255, 0, 1, 0, 128, 0}) {
		t.Fatalf("raw layout/report: %+v", r)
	}
}
func TestFromImagesMalformedNativeFormatsFailClosed(t *testing.T) {
	cases := [][]byte{[]byte("P1 1 1\n1"), []byte("P5 1 1 31\n\x01"), []byte("P5 2 1 255\n\x01"), []byte("P2 1 1 255\n256"), []byte("PG ML + 17 1 1\n\x00\x00"), []byte("BM")}
	for _, data := range cases {
		f, r, e := FromImages(context.Background(), [][]byte{data}, ImageOptions{})
		if e == nil || f != nil || r.Frames != 0 {
			t.Fatalf("malformed input accepted: %q", data)
		}
	}
}
