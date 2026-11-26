package display

import (
	"bytes"
	"encoding/binary"
	"errors"
	"image"
	"image/color"
	"io"
	"testing"
)

func TestTIFFHasActualGray16SamplesAndSubimageStride(t *testing.T) {
	src := image.NewGray16(image.Rect(0, 0, 4, 3))
	src.SetGray16(1, 1, color.Gray16{Y: 1})
	src.SetGray16(2, 1, color.Gray16{Y: 65534})
	src.SetGray16(1, 2, color.Gray16{Y: 0x1234})
	src.SetGray16(2, 2, color.Gray16{Y: 0xFEDC})
	var out bytes.Buffer
	if err := EncodeTIFF(&out, src.SubImage(image.Rect(1, 1, 3, 3))); err != nil {
		t.Fatal(err)
	}
	b := out.Bytes()
	if !bytes.Equal(b[:4], []byte{'I', 'I', 42, 0}) {
		t.Fatal("TIFF header")
	}
	order := binary.LittleEndian
	directory := int(order.Uint32(b[4:8]))
	count := int(order.Uint16(b[directory:]))
	tags := map[uint16]uint32{}
	for i := 0; i < count; i++ {
		entry := b[directory+2+12*i:]
		tags[order.Uint16(entry)] = order.Uint32(entry[8:])
	}
	if tags[256] != 2 || tags[257] != 2 || tags[258] != 16 || tags[259] != 1 || tags[262] != 1 || tags[277] != 1 || tags[279] != 8 {
		t.Fatalf("tags %v", tags)
	}
	offset := int(tags[273])
	for i, v := range []uint16{1, 65534, 0x1234, 0xFEDC} {
		if got := order.Uint16(b[offset+2*i:]); got != v {
			t.Fatalf("sample %d=%d want %d", i, got, v)
		}
	}
}

type tiffShortWriter struct{}

func (tiffShortWriter) Write(p []byte) (int, error) { return len(p) - 1, nil }

type tiffErrorWriter struct{ err error }

func (w tiffErrorWriter) Write([]byte) (int, error) { return 0, w.err }

func TestTIFFFormatsAndWriteErrors(t *testing.T) {
	for _, img := range []image.Image{image.NewGray(image.Rect(0, 0, 1, 1)), func() image.Image {
		r := image.NewRGBA(image.Rect(0, 0, 1, 1))
		r.SetRGBA(0, 0, color.RGBA{R: 1, G: 2, B: 3, A: 255})
		return r
	}()} {
		var out bytes.Buffer
		if err := EncodeTIFF(&out, img); err != nil {
			t.Fatal(err)
		}
		if err := EncodeTIFF(tiffShortWriter{}, img); !errors.Is(err, io.ErrShortWrite) {
			t.Fatal(err)
		}
		sentinel := errors.New("disk full")
		if err := EncodeTIFF(tiffErrorWriter{sentinel}, img); !errors.Is(err, sentinel) {
			t.Fatal(err)
		}
	}
	for _, img := range []image.Image{nil, image.NewGray(image.Rect(0, 0, 0, 0)), image.NewRGBA(image.Rect(0, 0, 1, 1)), image.NewNRGBA(image.Rect(0, 0, 1, 1))} {
		if err := EncodeTIFF(io.Discard, img); !errors.Is(err, ErrUnsupportedTIFFImage) {
			t.Fatalf("%T: %v", img, err)
		}
	}
}
