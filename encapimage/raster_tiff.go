package encapimage

import (
	"context"
	"encoding/binary"
	"fmt"
)

// Independently derived from TIFF 6.0 sections 2-7, 8-10. No vendor decoder is
// incorporated. This initial archival subset uses unsigned uncompressed strips.
type tiffRasterPage struct {
	RasterPageInfo
	sourceRows, sourceColumns, rowsPerStrip int
	offsets, counts                         []uint32
	planar                                  int
}

func isTIFF(b []byte) bool { return len(b) >= 4 && (string(b[:2]) == "II" || string(b[:2]) == "MM") }

func inspectTIFF(ctx context.Context, data []byte, limits ImportLimits) ([]tiffRasterPage, binary.ByteOrder, error) {
	if len(data) < 8 {
		return nil, nil, ErrMalformedImage
	}
	var order binary.ByteOrder = binary.LittleEndian
	if string(data[:2]) == "MM" {
		order = binary.BigEndian
	}
	if order.Uint16(data[2:]) != 42 {
		return nil, nil, fmt.Errorf("%w: TIFF version (BigTIFF is not supported)", ErrUnsupportedImage)
	}
	offset := order.Uint32(data[4:])
	seen := make(map[uint32]bool)
	var pages []tiffRasterPage
	totalValues, totalEntries := 0, 0
	for offset != 0 {
		if err := ctx.Err(); err != nil {
			return nil, nil, err
		}
		if len(pages) >= limits.MaxFrames || len(pages) >= 4096 {
			return nil, nil, ErrImportLimit
		}
		if seen[offset] || offset < 8 || uint64(offset)+2 > uint64(len(data)) {
			return nil, nil, fmt.Errorf("%w: TIFF IFD offset/cycle", ErrMalformedImage)
		}
		seen[offset] = true
		n := int(order.Uint16(data[offset:]))
		totalEntries += n
		if n == 0 || n > 256 || totalEntries > 65536 {
			return nil, nil, ErrImportLimit
		}
		end := uint64(offset) + 2 + uint64(n)*12
		if end+4 > uint64(len(data)) {
			return nil, nil, ErrMalformedImage
		}
		tags := make(map[uint16][]uint32)
		last := uint16(0)
		for i := 0; i < n; i++ {
			if err := ctx.Err(); err != nil {
				return nil, nil, err
			}
			entry := data[int(offset)+2+i*12:]
			tag := order.Uint16(entry)
			if i > 0 && tag <= last {
				return nil, nil, fmt.Errorf("%w: TIFF duplicate/unsorted tags", ErrMalformedImage)
			}
			last = tag
			// Explicitly reject semantics that cannot be represented faithfully.
			switch tag {
			case 320, 322, 323, 324, 325, 330, 338, 34675, 529, 530, 531, 532:
				return nil, nil, fmt.Errorf("%w: TIFF tag %d (palette/tile/sub-IFD/alpha/ICC/YCbCr)", ErrUnsupportedImage, tag)
			}
			if !tiffStructuralTag(tag) {
				continue // descriptive/private metadata is never copied or interpreted.
			}
			typ, count := order.Uint16(entry[2:]), order.Uint32(entry[4:])
			if (typ != 3 && typ != 4) || count == 0 || count > 65536 {
				return nil, nil, fmt.Errorf("%w: TIFF tag %d type/count", ErrMalformedImage, tag)
			}
			shortOrLong := tag == 256 || tag == 257 || tag == 273 || tag == 278 || tag == 279
			if tag == 254 && typ != 4 || tag != 254 && !shortOrLong && typ != 3 {
				return nil, nil, fmt.Errorf("%w: TIFF tag %d has an invalid field type", ErrMalformedImage, tag)
			}
			if int(count) > 262144-totalValues {
				return nil, nil, ErrImportLimit
			}
			totalValues += int(count)
			width := uint64(2)
			if typ == 4 {
				width = 4
			}
			start := uint64(int(offset) + 2 + i*12 + 8)
			size := uint64(count) * width
			if size > 4 {
				start = uint64(order.Uint32(entry[8:]))
			}
			if start < 8 || start > uint64(len(data)) || size > uint64(len(data))-start {
				return nil, nil, ErrMalformedImage
			}
			values := make([]uint32, int(count))
			for j := range values {
				if j&1023 == 0 {
					if err := ctx.Err(); err != nil {
						return nil, nil, err
					}
				}
				at := int(start) + j*int(width)
				if typ == 3 {
					values[j] = uint32(order.Uint16(data[at:]))
				} else {
					values[j] = order.Uint32(data[at:])
				}
			}
			tags[tag] = values
		}
		p, err := admitTIFFPage(tags, len(pages)+1, len(data), limits)
		if err != nil {
			return nil, nil, err
		}
		pages = append(pages, p)
		offset = order.Uint32(data[int(end):])
	}
	if len(pages) == 0 {
		return nil, nil, ErrMalformedImage
	}
	return pages, order, nil
}

func tiffStructuralTag(tag uint16) bool {
	switch tag {
	case 254, 256, 257, 258, 259, 262, 266, 273, 274, 277, 278, 279, 284, 317, 339:
		return true
	}
	return false
}

func admitTIFFPage(tags map[uint16][]uint32, number, bytes int, limits ImportLimits) (tiffRasterPage, error) {
	p := tiffRasterPage{}
	malformed := false
	scalar := func(tag uint16, def uint32) uint32 {
		v := tags[tag]
		if len(v) == 0 {
			return def
		}
		if len(v) != 1 {
			malformed = true
			return def
		}
		return v[0]
	}
	w, h := scalar(256, 0), scalar(257, 0)
	samples, photo := scalar(277, 1), scalar(262, 99)
	bits := tags[258]
	if w == 0 || h == 0 || w > 65535 || h > 65535 || (samples != 1 && samples != 3) || len(bits) != int(samples) {
		return p, fmt.Errorf("%w: TIFF dimensions/components/bits", ErrUnsupportedImage)
	}
	for _, bit := range bits {
		if bit != bits[0] || (bit != 8 && bit != 16) {
			return p, fmt.Errorf("%w: TIFF bit depth", ErrUnsupportedImage)
		}
	}
	if (samples == 1 && photo != 0 && photo != 1) || (samples == 3 && photo != 2) {
		return p, fmt.Errorf("%w: TIFF photometric interpretation", ErrUnsupportedImage)
	}
	if scalar(259, 1) != 1 || scalar(266, 1) != 1 || scalar(317, 1) != 1 || scalar(254, 0)&^uint32(2) != 0 {
		return p, fmt.Errorf("%w: TIFF compression/fill/predictor/subfile", ErrUnsupportedImage)
	}
	if formats := tags[339]; len(formats) > 0 {
		if len(formats) != int(samples) {
			return p, ErrMalformedImage
		}
		for _, f := range formats {
			if f != 1 {
				return p, fmt.Errorf("%w: TIFF signed/floating samples", ErrUnsupportedImage)
			}
		}
	}
	orientation, planar, stripRows := scalar(274, 1), scalar(284, 1), scalar(278, h)
	if malformed || orientation < 1 || orientation > 8 || planar < 1 || planar > 2 || stripRows == 0 {
		return p, ErrMalformedImage
	}
	if stripRows > h {
		stripRows = h
	}
	p = tiffRasterPage{RasterPageInfo: RasterPageInfo{Number: number, Rows: int(h), Columns: int(w), Samples: int(samples), BitsStored: int(bits[0]), Orientation: int(orientation), PlanarConfiguration: int(planar), PhotometricInterpretation: "MONOCHROME2"}, sourceRows: int(h), sourceColumns: int(w), rowsPerStrip: int(stripRows), planar: int(planar), offsets: tags[273], counts: tags[279]}
	if photo == 0 {
		p.PhotometricInterpretation = "MONOCHROME1"
	} else if photo == 2 {
		p.PhotometricInterpretation = "RGB"
	}
	if orientation >= 5 {
		p.Rows, p.Columns = p.Columns, p.Rows
	}
	if _, err := admitImage(imageLayout{rows: p.Rows, columns: p.Columns, samples: p.Samples, bits: p.BitsStored}, limits); err != nil {
		return tiffRasterPage{}, err
	}
	stripsPerPlane := (p.sourceRows + p.rowsPerStrip - 1) / p.rowsPerStrip
	count := stripsPerPlane
	if planar == 2 {
		count *= p.Samples
	}
	if len(p.offsets) != count || len(p.counts) != count || count > 65536 {
		return tiffRasterPage{}, fmt.Errorf("%w: TIFF strip count", ErrMalformedImage)
	}
	for i := 0; i < count; i++ {
		rows := min(p.rowsPerStrip, p.sourceRows-(i%stripsPerPlane)*p.rowsPerStrip)
		components := p.Samples
		if planar == 2 {
			components = 1
		}
		want := uint64(rows) * uint64(p.sourceColumns) * uint64(components) * uint64(p.BitsStored/8)
		offset, size := uint64(p.offsets[i]), uint64(p.counts[i])
		if size != want || offset < 8 || offset > uint64(bytes) || size > uint64(bytes)-offset {
			return tiffRasterPage{}, fmt.Errorf("%w: TIFF strip span", ErrMalformedImage)
		}
	}
	return p, nil
}

func decodeTIFFPage(ctx context.Context, data []byte, p tiffRasterPage, order binary.ByteOrder, channel RasterChannel) (RasterPage, error) {
	if channel != RasterAll && p.Samples != 3 {
		return RasterPage{}, fmt.Errorf("%w: component selection requires RGB", ErrInvalidOption)
	}
	out := RasterPage{RasterPageInfo: p.RasterPageInfo}
	selected := -1
	if channel != RasterAll {
		selected = map[RasterChannel]int{RasterRed: 0, RasterGreen: 1, RasterBlue: 2}[channel]
		out.Samples = 1
		out.PhotometricInterpretation = "MONOCHROME2"
	}
	width := p.BitsStored / 8
	if err := ctx.Err(); err != nil {
		return RasterPage{}, err
	}
	out.Data = make([]byte, out.Rows*out.Columns*out.Samples*width)
	stripsPerPlane := (p.sourceRows + p.rowsPerStrip - 1) / p.rowsPerStrip
	for y := 0; y < p.sourceRows; y++ {
		if err := ctx.Err(); err != nil {
			return RasterPage{}, err
		}
		for x := 0; x < p.sourceColumns; x++ {
			if x&4095 == 0 {
				if err := ctx.Err(); err != nil {
					return RasterPage{}, err
				}
			}
			dx, dy := x, y
			switch p.Orientation {
			case 2:
				dx = p.sourceColumns - 1 - x
			case 3:
				dx, dy = p.sourceColumns-1-x, p.sourceRows-1-y
			case 4:
				dy = p.sourceRows - 1 - y
			case 5:
				dx, dy = y, x
			case 6:
				dx, dy = p.sourceRows-1-y, x
			case 7:
				dx, dy = p.sourceRows-1-y, p.sourceColumns-1-x
			case 8:
				dx, dy = y, p.sourceColumns-1-x
			}
			for c := 0; c < out.Samples; c++ {
				sourceC := c
				if selected >= 0 {
					sourceC = selected
				}
				strip := y / p.rowsPerStrip
				at := ((y%p.rowsPerStrip)*p.sourceColumns+x)*p.Samples + sourceC
				if p.planar == 2 {
					strip += sourceC * stripsPerPlane
					at = (y%p.rowsPerStrip)*p.sourceColumns + x
				}
				at = int(p.offsets[strip]) + at*width
				dest := ((dy*out.Columns+dx)*out.Samples + c) * width
				if width == 1 {
					out.Data[dest] = data[at]
				} else {
					binary.LittleEndian.PutUint16(out.Data[dest:], order.Uint16(data[at:]))
				}
			}
		}
	}
	return out, nil
}
