package encapimage

import (
	"bytes"
	"context"
	"encoding/binary"
	"fmt"
	"github.com/ThalesMMS/dicom-go/transfer"
	"image/color"
	"image/png"
)

func readRaster(ctx context.Context, b []byte, format Format, limits ImportLimits) (sourceFrame, error) {
	if format == BMP {
		return readBMP(ctx, b, limits)
	}
	cfg, err := png.DecodeConfig(pngContextReader{ctx, bytes.NewReader(b)})
	if e := ctx.Err(); e != nil {
		return sourceFrame{}, e
	}
	if err != nil {
		return sourceFrame{}, fmt.Errorf("%w: PNG header", ErrMalformedImage)
	}
	l := imageLayout{rows: cfg.Height, columns: cfg.Width, samples: 3, bits: 8, lossless: true, process: "native", photometric: "RGB"}
	if cfg.ColorModel == color.GrayModel || cfg.ColorModel == color.Gray16Model {
		l.samples = 1
		l.photometric = "MONOCHROME2"
	}
	if cfg.ColorModel == color.Gray16Model || cfg.ColorModel == color.RGBA64Model || cfg.ColorModel == color.NRGBA64Model {
		l.bits = 16
	}
	n, err := admitImage(l, limits)
	if err != nil {
		return sourceFrame{}, err
	}
	// Conservative standard decoder pixel storage preflight, separate from RSS.
	if int64(l.rows)*int64(l.columns) > limits.MaxDecodedBytes/8 {
		return sourceFrame{}, ErrImportLimit
	}
	if err := ctx.Err(); err != nil {
		return sourceFrame{}, err
	}
	img, err := png.Decode(pngContextReader{ctx, bytes.NewReader(b)})
	if e := ctx.Err(); e != nil {
		return sourceFrame{}, e
	}
	if err != nil {
		return sourceFrame{}, fmt.Errorf("%w: PNG decode", ErrMalformedImage)
	}
	if img.Bounds().Dx() != l.columns || img.Bounds().Dy() != l.rows {
		return sourceFrame{}, ErrMalformedImage
	}
	data := make([]byte, int(n))
	pos := 0
	put := func(v uint32) {
		if l.bits == 16 {
			binary.LittleEndian.PutUint16(data[pos:], uint16(v))
			pos += 2
		} else {
			data[pos] = byte(v >> 8)
			pos++
		}
	}
	for y := 0; y < l.rows; y++ {
		if err := ctx.Err(); err != nil {
			return sourceFrame{}, err
		}
		for x := 0; x < l.columns; x++ {
			red, green, blue, alpha := img.At(x, y).RGBA()
			if alpha != 65535 {
				return sourceFrame{}, fmt.Errorf("%w: transparent alpha", ErrUnsupportedImage)
			}
			put(red)
			if l.samples == 3 {
				put(green)
				put(blue)
			}
		}
	}
	return sourceFrame{imageLayout: l, data: data, syntax: transfer.ExplicitVRLittleEndian, format: PNG}, nil
}
func readBMP(ctx context.Context, b []byte, limits ImportLimits) (sourceFrame, error) {
	if len(b) < 54 || string(b[:2]) != "BM" {
		return sourceFrame{}, ErrMalformedImage
	}
	offset := uint64(binary.LittleEndian.Uint32(b[10:]))
	dib := uint64(binary.LittleEndian.Uint32(b[14:]))
	if dib != 40 {
		return sourceFrame{}, fmt.Errorf("%w: BMP DIB", ErrUnsupportedImage)
	}
	if binary.LittleEndian.Uint32(b[2:]) != uint32(len(b)) || offset < 54 || offset > uint64(len(b)) || binary.LittleEndian.Uint16(b[26:]) != 1 {
		return sourceFrame{}, ErrMalformedImage
	}
	w, h := int64(int32(binary.LittleEndian.Uint32(b[18:]))), int64(int32(binary.LittleEndian.Uint32(b[22:])))
	top := h < 0
	if top {
		h = -h
	}
	bits := int(binary.LittleEndian.Uint16(b[28:]))
	if (bits != 8 && bits != 24 && bits != 32) || binary.LittleEndian.Uint32(b[30:]) != 0 {
		return sourceFrame{}, fmt.Errorf("%w: BMP compression/bit depth", ErrUnsupportedImage)
	}
	l := imageLayout{rows: int(h), columns: int(w), samples: 3, bits: 8, lossless: true, process: "native", photometric: "RGB"}
	n, err := admitImage(l, limits)
	if err != nil {
		return sourceFrame{}, err
	}
	row := ((uint64(w)*uint64(bits) + 31) / 32) * 4
	if row*uint64(h) > uint64(len(b))-offset || offset+row*uint64(h) != uint64(len(b)) {
		return sourceFrame{}, ErrMalformedImage
	}
	var palette []byte
	if bits == 8 {
		count := uint64(binary.LittleEndian.Uint32(b[46:]))
		if count == 0 {
			count = 256
		}
		if count > 256 || 54+count*4 > offset {
			return sourceFrame{}, ErrMalformedImage
		}
		palette = b[54 : 54+count*4]
	}
	data := make([]byte, int(n))
	for y := 0; y < l.rows; y++ {
		if err := ctx.Err(); err != nil {
			return sourceFrame{}, err
		}
		sourceY := y
		if !top {
			sourceY = l.rows - 1 - y
		}
		line := b[int(offset)+sourceY*int(row):]
		for x := 0; x < l.columns; x++ {
			var pix []byte
			if bits == 8 {
				index := int(line[x]) * 4
				if index+4 > len(palette) {
					return sourceFrame{}, ErrMalformedImage
				}
				pix = palette[index : index+4]
			} else {
				start := x * (bits / 8)
				pix = line[start : start+bits/8]
			}
			at := (y*l.columns + x) * 3
			data[at], data[at+1], data[at+2] = pix[2], pix[1], pix[0]
		}
	}
	return sourceFrame{imageLayout: l, data: data, syntax: transfer.ExplicitVRLittleEndian, format: BMP}, nil
}

// pngContextReader polls at decoder reads. CPU work over already buffered
// compressed data remains cooperatively bounded by the admitted image size.
type pngContextReader struct {
	ctx    context.Context
	reader *bytes.Reader
}

func (r pngContextReader) Read(p []byte) (int, error) {
	if e := r.ctx.Err(); e != nil {
		return 0, e
	}
	return r.reader.Read(p)
}
