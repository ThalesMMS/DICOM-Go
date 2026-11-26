package display

import (
	"encoding/binary"
	"image"
	"math"
)

// argbSamples implements retired PS3.3-2001 ARGB: the first component is a
// palette index, not opacity. A positive index overrides all three RGB planes.
func argbSamples(frame ColorFrame, emit func(i int, r, g, b uint16)) error {
	if frame.SamplesPerPixel != 4 {
		return frameValidationError("SamplesPerPixel", ErrUnsupportedColorLayout)
	}
	if frame.PlanarConfiguration != 0 && frame.PlanarConfiguration != 1 {
		return frameValidationError("PlanarConfiguration", ErrUnsupportedColorLayout)
	}
	format := frame.Format
	if format.BitsAllocated != 8 && format.BitsAllocated != 16 {
		return frameValidationError("BitsAllocated", ErrUnsupportedColorLayout)
	}
	if format.Signed {
		return frameValidationError("PixelRepresentation", ErrUnsupportedColorLayout)
	}
	width := format.BitsAllocated / 8
	layout, err := checkedFrameLayout(frame.Rows, frame.Columns, 4, width, 8)
	if err != nil {
		return err
	}
	if len(frame.Pixels) < layout.sourceBytes {
		return frameValidationError("Pixels", ErrPixelDataTooShort)
	}
	order := format.ByteOrder
	if order == nil {
		order = binary.LittleEndian
	}
	if format.BitsStored <= 0 || format.BitsStored > format.BitsAllocated {
		format.BitsStored, format.HighBit = format.BitsAllocated, format.BitsAllocated-1
	}
	reader := newStoredPixelReader(format, order)
	sample := func(i, channel int) int {
		index := 4*i + channel
		if frame.PlanarConfiguration == 1 {
			index = channel*layout.pixels + i
		}
		return int(reader.Value(frame.Pixels[index*width:]))
	}
	scale := 65535 / (math.Pow(2, float64(format.BitsStored)) - 1)
	for i := 0; i < layout.pixels; i++ {
		alpha := sample(i, 0)
		var rgb [3]uint16
		if alpha > 0 {
			palette := frame.Palette
			if palette == nil || palette.Red == nil || palette.Green == nil || palette.Blue == nil {
				return ErrMissingPalette
			}
			for channel, lut := range [3]*LUT{palette.Red, palette.Green, palette.Blue} {
				rgb[channel] = lut.entry16(lut.Lookup(alpha))
			}
		} else {
			for channel := range rgb {
				rgb[channel] = uint16(math.Round(clampFloat(float64(sample(i, channel+1))*scale, 0, 65535)))
			}
		}
		emit(i, rgb[0], rgb[1], rgb[2])
	}
	return nil
}

func renderARGB(frame ColorFrame) (*image.RGBA, error) {
	var out *image.RGBA
	err := argbSamples(frame, func(i int, r, g, b uint16) {
		if out == nil {
			out = image.NewRGBA(image.Rect(0, 0, frame.Columns, frame.Rows))
		}
		setRGBA(out, i, uint8(r>>8), uint8(g>>8), uint8(b>>8))
	})
	if err != nil {
		return nil, err
	}
	return out, nil
}

func renderARGB16(frame ColorFrame) (*image.RGBA64, error) {
	var out *image.RGBA64
	err := argbSamples(frame, func(i int, r, g, b uint16) {
		if out == nil {
			out = image.NewRGBA64(image.Rect(0, 0, frame.Columns, frame.Rows))
		}
		dst := out.Pix[i*8 : i*8+8]
		for c, v := range [3]uint16{r, g, b} {
			dst[2*c], dst[2*c+1] = uint8(v>>8), uint8(v)
		}
		dst[6], dst[7] = 255, 255
	})
	if err != nil {
		return nil, err
	}
	return out, nil
}
