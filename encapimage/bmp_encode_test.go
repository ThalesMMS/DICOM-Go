package encapimage

import (
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"image"
	"image/color"
	"io"
	"math"
	"testing"
)

func TestEncodeBMP24BGRRowsPaddingAndAlpha(t *testing.T) {
	img := image.NewNRGBA(image.Rect(5, 7, 8, 9))
	img.SetNRGBA(5, 7, color.NRGBA{R: 200, G: 100, B: 50, A: 0})
	img.SetNRGBA(6, 7, color.NRGBA{R: 255, A: 128})
	img.SetNRGBA(7, 7, color.NRGBA{R: 1, G: 2, B: 3, A: 255})
	img.SetNRGBA(5, 8, color.NRGBA{R: 4, G: 5, B: 6, A: 255})
	img.SetNRGBA(6, 8, color.NRGBA{R: 7, G: 8, B: 9, A: 255})
	img.SetNRGBA(7, 8, color.NRGBA{R: 10, G: 11, B: 12, A: 255})

	var buf bytes.Buffer
	if err := EncodeBMP24(context.Background(), &buf, img); err != nil {
		t.Fatal(err)
	}
	data := buf.Bytes()
	if len(data) != 54+2*12 || string(data[:2]) != "BM" ||
		binary.LittleEndian.Uint32(data[2:6]) != uint32(len(data)) ||
		binary.LittleEndian.Uint32(data[10:14]) != 54 ||
		binary.LittleEndian.Uint32(data[14:18]) != 40 ||
		binary.LittleEndian.Uint32(data[18:22]) != 3 || binary.LittleEndian.Uint32(data[22:26]) != 2 ||
		binary.LittleEndian.Uint16(data[26:28]) != 1 || binary.LittleEndian.Uint16(data[28:30]) != 24 ||
		binary.LittleEndian.Uint32(data[30:34]) != 0 || binary.LittleEndian.Uint32(data[34:38]) != 24 {
		t.Fatalf("unexpected BMP header: %x", data)
	}
	want := []byte{
		6, 5, 4, 9, 8, 7, 12, 11, 10, 0, 0, 0,
		0, 0, 0, 0, 0, 128, 3, 2, 1, 0, 0, 0,
	}
	if !bytes.Equal(data[54:], want) {
		t.Fatalf("BMP BGR rows = %v, want %v", data[54:], want)
	}
}

type bmpWriterFunc func([]byte) (int, error)

func (fn bmpWriterFunc) Write(p []byte) (int, error) { return fn(p) }

type headerOnlyImage struct {
	image.Image
	bounds image.Rectangle
}

func (img headerOnlyImage) Bounds() image.Rectangle { return img.bounds }

func TestEncodeBMP24RefusesOversizedHeaderAndPropagatesWriterFailure(t *testing.T) {
	for _, bounds := range []image.Rectangle{
		{Min: image.Pt(math.MinInt, 0), Max: image.Pt(math.MaxInt, 1)},
		image.Rect(0, 0, math.MaxInt32, 1),
		image.Rect(0, 0, 65535, 65535),
	} {
		var buf bytes.Buffer
		if err := EncodeBMP24(context.Background(), &buf, headerOnlyImage{bounds: bounds}); !errors.Is(err, ErrUnsupportedImage) || buf.Len() != 0 {
			t.Fatalf("oversized BMP = %d bytes, %v", buf.Len(), err)
		}
	}
	img := image.NewGray(image.Rect(0, 0, 5, 2))
	short := bmpWriterFunc(func(p []byte) (int, error) { return len(p) - 1, nil })
	if err := EncodeBMP24(context.Background(), short, img); !errors.Is(err, io.ErrShortWrite) {
		t.Fatalf("short write = %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	writes := 0
	writer := bmpWriterFunc(func(p []byte) (int, error) {
		writes++
		if writes == 2 {
			cancel()
		}
		return len(p), nil
	})
	if err := EncodeBMP24(ctx, writer, img); !errors.Is(err, context.Canceled) || writes != 2 {
		t.Fatalf("cancelled encode = %d writes, %v", writes, err)
	}
}
