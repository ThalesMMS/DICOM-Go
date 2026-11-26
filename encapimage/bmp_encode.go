package encapimage

import (
	"context"
	"encoding/binary"
	"fmt"
	"image"
	"io"
	"math"
)

// EncodeBMP24 writes an uncompressed Windows BMP with 24-bit BGR pixels and
// bottom-up, four-byte-aligned rows. Alpha is composited on black using the
// premultiplied channels returned by color.Color.RGBA. Input is read in bounded
// chunks; callers needing atomic file publication should stage the output.
func EncodeBMP24(ctx context.Context, w io.Writer, img image.Image) error {
	if ctx == nil {
		ctx = context.Background()
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	if w == nil || img == nil || img.Bounds().Empty() {
		return fmt.Errorf("%w: BMP requires an image and destination", ErrMalformedImage)
	}

	bounds := img.Bounds()
	width := uint64(bounds.Max.X) - uint64(bounds.Min.X)
	height := uint64(bounds.Max.Y) - uint64(bounds.Min.Y)
	// Width and height are signed int32 in BITMAPINFOHEADER. File size is a
	// uint32 in BITMAPFILEHEADER, and the bounded product fits in uint64.
	if width > math.MaxInt32 || height > math.MaxInt32 {
		return fmt.Errorf("%w: BMP dimensions exceed the format limit", ErrUnsupportedImage)
	}
	stride := (width*3 + 3) &^ 3
	pixelBytes := stride * height
	const headerSize = 14 + 40
	if pixelBytes > math.MaxUint32-headerSize {
		return fmt.Errorf("%w: BMP file size exceeds the format limit", ErrUnsupportedImage)
	}

	var header [headerSize]byte
	copy(header[:2], "BM")
	binary.LittleEndian.PutUint32(header[2:6], uint32(headerSize+pixelBytes))
	binary.LittleEndian.PutUint32(header[10:14], headerSize)
	binary.LittleEndian.PutUint32(header[14:18], 40)
	binary.LittleEndian.PutUint32(header[18:22], uint32(width))
	binary.LittleEndian.PutUint32(header[22:26], uint32(height))
	binary.LittleEndian.PutUint16(header[26:28], 1)
	binary.LittleEndian.PutUint16(header[28:30], 24)
	binary.LittleEndian.PutUint32(header[34:38], uint32(pixelBytes))
	if err := writeBMPBytes(w, header[:]); err != nil {
		return err
	}

	const chunkPixels = 4096
	var chunk [chunkPixels*3 + 3]byte
	padding := int(stride - width*3)
	for row := int(height) - 1; row >= 0; row-- {
		y := bounds.Min.Y + row
		for offset := 0; offset < int(width); {
			if err := ctx.Err(); err != nil {
				return err
			}
			count := min(chunkPixels, int(width)-offset)
			x := bounds.Min.X + offset
			for i := range count {
				red, green, blue, _ := img.At(x+i, y).RGBA()
				chunk[i*3], chunk[i*3+1], chunk[i*3+2] = uint8(blue>>8), uint8(green>>8), uint8(red>>8)
			}
			offset += count
			n := count * 3
			if offset == int(width) {
				clear(chunk[n : n+padding])
				n += padding
			}
			if err := writeBMPBytes(w, chunk[:n]); err != nil {
				return err
			}
		}
	}
	return ctx.Err()
}

func writeBMPBytes(w io.Writer, bytes []byte) error {
	n, err := w.Write(bytes)
	if err == nil && n != len(bytes) {
		return io.ErrShortWrite
	}
	return err
}
