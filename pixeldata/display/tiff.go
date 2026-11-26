package display

import (
	"encoding/binary"
	"errors"
	"fmt"
	"image"
	"io"
)

var ErrUnsupportedTIFFImage = errors.New("dicom/display: unsupported TIFF image")

// EncodeTIFF writes a little-endian, uncompressed, single-strip TIFF
// containing Gray (8 bits), Gray16 (16 bits), or opaque RGBA (8-bit RGB).
// Gray16 samples are written directly; they are never reduced to eight bits.
// No resolution or patient metadata is invented. BigTIFF, palettes, alpha,
// multiple pages and compressed strips are outside this encoder's scope.
func EncodeTIFF(w io.Writer, img image.Image) error {
	if img == nil {
		return fmt.Errorf("%w: nil image", ErrUnsupportedTIFFImage)
	}
	b := img.Bounds()
	width, height := b.Dx(), b.Dy()
	if !checkedExportSize(width, height) {
		return fmt.Errorf("%w: size exceeds %d pixels", ErrUnsupportedTIFFImage, MaxExportPixels)
	}
	bits, samples, photo := uint32(8), uint32(1), uint32(1)
	switch source := img.(type) {
	case *image.Gray:
	case *image.Gray16:
		bits = 16
	case *image.RGBA:
		samples, photo = 3, 2
		for y := b.Min.Y; y < b.Max.Y; y++ {
			for x := b.Min.X; x < b.Max.X; x++ {
				if source.RGBAAt(x, y).A != 255 {
					return fmt.Errorf("%w: RGBA must be opaque", ErrUnsupportedTIFFImage)
				}
			}
		}
	default:
		return fmt.Errorf("%w: need Gray, Gray16 or opaque RGBA", ErrUnsupportedTIFFImage)
	}
	const directorySize = 2 + 10*12 + 4
	pixelOffset := uint32(8 + directorySize)
	bitsValue := bits
	if samples == 3 {
		bitsValue = pixelOffset
		pixelOffset += 6
	}
	header := make([]byte, int(pixelOffset))
	copy(header, []byte{'I', 'I', 42, 0, 8, 0, 0, 0})
	binary.LittleEndian.PutUint16(header[8:10], 10)
	entries := [10][4]uint32{
		{256, 4, 1, uint32(width)}, {257, 4, 1, uint32(height)}, {258, 3, samples, bitsValue},
		{259, 3, 1, 1}, {262, 3, 1, photo}, {273, 4, 1, pixelOffset}, {277, 3, 1, samples},
		{278, 4, 1, uint32(height)}, {279, 4, 1, uint32(width) * uint32(height) * samples * (bits / 8)}, {284, 3, 1, 1},
	}
	for i, entry := range entries {
		o := 10 + i*12
		binary.LittleEndian.PutUint16(header[o:o+2], uint16(entry[0]))
		binary.LittleEndian.PutUint16(header[o+2:o+4], uint16(entry[1]))
		binary.LittleEndian.PutUint32(header[o+4:o+8], entry[2])
		binary.LittleEndian.PutUint32(header[o+8:o+12], entry[3])
	}
	if samples == 3 {
		for i := 0; i < 3; i++ {
			binary.LittleEndian.PutUint16(header[8+directorySize+2*i:], 8)
		}
	}
	if err := writeTIFFBytes(w, header); err != nil {
		return err
	}
	row := make([]byte, width*int(samples)*int(bits/8))
	for y := b.Min.Y; y < b.Max.Y; y++ {
		for x := b.Min.X; x < b.Max.X; x++ {
			o := (x - b.Min.X) * int(samples) * int(bits/8)
			switch source := img.(type) {
			case *image.Gray:
				row[o] = source.GrayAt(x, y).Y
			case *image.Gray16:
				binary.LittleEndian.PutUint16(row[o:o+2], source.Gray16At(x, y).Y)
			case *image.RGBA:
				p := source.RGBAAt(x, y)
				row[o], row[o+1], row[o+2] = p.R, p.G, p.B
			}
		}
		if err := writeTIFFBytes(w, row); err != nil {
			return err
		}
	}
	return nil
}

func writeTIFFBytes(w io.Writer, p []byte) error {
	n, err := w.Write(p)
	if err == nil && n != len(p) {
		return io.ErrShortWrite
	}
	return err
}
