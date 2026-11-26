package encapimage

import (
	"context"
	"encoding/binary"
	"fmt"
	"image"
	"image/color"

	"github.com/ThalesMMS/dicom-go/core"
	"github.com/ThalesMMS/dicom-go/object"
)

const TIFF Format = "tiff"

// RasterPageInfo describes normalized display dimensions without assuming
// physical geometry. Number is one-based and refers to the source IFD order.
type RasterPageInfo struct {
	Number, Rows, Columns, Samples, BitsStored int
	Orientation, PlanarConfiguration           int
	PhotometricInterpretation                  string
}

type RasterInfo struct {
	Format Format
	Pages  []RasterPageInfo
}

// RasterChannel selects exact component samples, never a luminance conversion.
type RasterChannel string

const (
	RasterAll   RasterChannel = "all"
	RasterRed   RasterChannel = "red"
	RasterGreen RasterChannel = "green"
	RasterBlue  RasterChannel = "blue"
)

// RasterPage owns canonical interleaved little-endian unsigned stored samples.
type RasterPage struct {
	RasterPageInfo
	Data []byte
}

func rasterAdmission(ctx context.Context, data []byte, limits ImportLimits) (context.Context, ImportLimits, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	if err := ctx.Err(); err != nil {
		return ctx, limits, err
	}
	l, err := limits.normalized()
	if err != nil {
		return ctx, l, err
	}
	if int64(len(data)) > l.MaxInputBytes {
		return ctx, l, ErrImportLimit
	}
	return ctx, l, nil
}

// InspectRaster validates the finite TIFF subset without allocating its pixel
// planes. PNG/BMP/PNM/PGX reuse the existing bounded native import decoders.
// JPEG remains the separate codestream-preserving photographic import API.
func InspectRaster(ctx context.Context, data []byte, limits ImportLimits) (RasterInfo, error) {
	ctx, limits, err := rasterAdmission(ctx, data, limits)
	if err != nil {
		return RasterInfo{}, err
	}
	if isTIFF(data) {
		pages, _, err := inspectTIFF(ctx, data, limits)
		if err != nil {
			return RasterInfo{}, err
		}
		info := RasterInfo{Format: TIFF, Pages: make([]RasterPageInfo, len(pages))}
		for i := range pages {
			info.Pages[i] = pages[i].RasterPageInfo
		}
		if err := ctx.Err(); err != nil {
			return RasterInfo{}, err
		}
		return info, nil
	}
	f, err := readDesktopRaster(ctx, data, limits)
	if err != nil {
		return RasterInfo{}, err
	}
	if err := ctx.Err(); err != nil {
		return RasterInfo{}, err
	}
	return RasterInfo{Format: f.format, Pages: []RasterPageInfo{rasterInfo(f)}}, nil
}

func readDesktopRaster(ctx context.Context, data []byte, limits ImportLimits) (sourceFrame, error) {
	format := detectImage(data)
	if format != PNG && format != BMP && format != PNM && format != PGX {
		return sourceFrame{}, fmt.Errorf("%w: desktop raster format", ErrUnsupportedImage)
	}
	if format == PNG {
		// The native importer does not perform ICC color management. Refuse an
		// embedded profile rather than silently dropping its color semantics.
		for pos := 8; pos+12 <= len(data); {
			if err := ctx.Err(); err != nil {
				return sourceFrame{}, err
			}
			n := uint64(binary.BigEndian.Uint32(data[pos:]))
			if n > uint64(len(data)-pos-12) {
				return sourceFrame{}, ErrMalformedImage
			}
			if string(data[pos+4:pos+8]) == "iCCP" {
				return sourceFrame{}, fmt.Errorf("%w: PNG ICC profile", ErrUnsupportedImage)
			}
			pos += int(n) + 12
		}
	}
	f, err := readImage(ctx, data, format, ImageOptions{}, limits)
	if err == nil && f.signed != 0 {
		return sourceFrame{}, fmt.Errorf("%w: signed desktop raster", ErrUnsupportedImage)
	}
	return f, err
}

func rasterInfo(f sourceFrame) RasterPageInfo {
	return RasterPageInfo{Number: 1, Rows: f.rows, Columns: f.columns, Samples: f.samples,
		BitsStored: f.bits, Orientation: 1, PlanarConfiguration: 1, PhotometricInterpretation: f.photometric}
}

// DecodeRasterPage preserves sample precision and source page identity. A
// component selection is explicit and preserves that component's exact values.
func DecodeRasterPage(ctx context.Context, data []byte, page int, channel RasterChannel, limits ImportLimits) (RasterPage, error) {
	ctx, limits, err := rasterAdmission(ctx, data, limits)
	if err != nil {
		return RasterPage{}, err
	}
	if channel == "" {
		channel = RasterAll
	}
	if channel != RasterAll && channel != RasterRed && channel != RasterGreen && channel != RasterBlue {
		return RasterPage{}, ErrInvalidOption
	}
	var result RasterPage
	if isTIFF(data) {
		pages, order, err := inspectTIFF(ctx, data, limits)
		if err != nil {
			return result, err
		}
		if page < 1 || page > len(pages) {
			return result, ErrInvalidOption
		}
		result, err = decodeTIFFPage(ctx, data, pages[page-1], order, channel)
		if err != nil {
			return RasterPage{}, err
		}
	} else {
		if page != 1 {
			return result, ErrInvalidOption
		}
		f, err := readDesktopRaster(ctx, data, limits)
		if err != nil {
			return result, err
		}
		result = RasterPage{RasterPageInfo: rasterInfo(f), Data: f.data}
		if channel != RasterAll {
			if result.Samples != 3 {
				return RasterPage{}, fmt.Errorf("%w: component selection requires RGB", ErrInvalidOption)
			}
			width := (result.BitsStored + 7) / 8
			component := map[RasterChannel]int{RasterRed: 0, RasterGreen: 1, RasterBlue: 2}[channel]
			// Compact in place; no second full sample allocation.
			for at := 0; at < result.Rows*result.Columns; at++ {
				if at&4095 == 0 {
					if err := ctx.Err(); err != nil {
						return RasterPage{}, err
					}
				}
				copy(result.Data[at*width:], result.Data[(at*3+component)*width:(at*3+component+1)*width])
			}
			result.Data = result.Data[:result.Rows*result.Columns*width]
			result.Samples = 1
			result.PhotometricInterpretation = "MONOCHROME2"
		}
	}
	if err := ctx.Err(); err != nil {
		return RasterPage{}, err
	}
	return result, nil
}

// Preview scales only the presentation to at most edge pixels per dimension.
// Its 8-bit display conversion never changes the stored samples used by import.
func (p RasterPage) Preview(ctx context.Context, edge int) (image.Image, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	if edge < 1 || edge > 1024 || p.Rows < 1 || p.Columns < 1 || p.Rows > 65535 || p.Columns > 65535 || (p.Samples != 1 && p.Samples != 3) || p.BitsStored < 1 || p.BitsStored > 16 {
		return nil, ErrInvalidOption
	}
	if (p.PhotometricInterpretation != "MONOCHROME1" && p.PhotometricInterpretation != "MONOCHROME2" && p.PhotometricInterpretation != "RGB") || (p.PhotometricInterpretation == "RGB") != (p.Samples == 3) {
		return nil, ErrUnsupportedImage
	}
	width := (p.BitsStored + 7) / 8
	if int64(p.Rows)*int64(p.Columns)*int64(p.Samples)*int64(width) != int64(len(p.Data)) {
		return nil, ErrMalformedImage
	}
	w, h := p.Columns, p.Rows
	if max(w, h) > edge {
		w = max(1, w*edge/max(p.Columns, p.Rows))
		h = max(1, h*edge/max(p.Columns, p.Rows))
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	out := image.NewRGBA(image.Rect(0, 0, w, h))
	sample := func(at int) uint8 {
		v := uint32(p.Data[at])
		if width == 2 {
			v = uint32(binary.LittleEndian.Uint16(p.Data[at:]))
		}
		v = v * 255 / ((1 << uint(p.BitsStored)) - 1)
		if p.PhotometricInterpretation == "MONOCHROME1" {
			v = 255 - v
		}
		return uint8(v)
	}
	for y := 0; y < h; y++ {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		for x := 0; x < w; x++ {
			at := ((y*p.Rows/h)*p.Columns + x*p.Columns/w) * p.Samples * width
			r := sample(at)
			c := color.RGBA{R: r, G: r, B: r, A: 255}
			if p.Samples == 3 {
				c.G, c.B = sample(at+width), sample(at+2*width)
			}
			out.SetRGBA(x, y, c)
		}
	}
	return out, nil
}

// BuildRasterPage reuses the native image constructor, with one SC object per
// page. This supports heterogeneous page sizes and exact RGB16 without claiming
// the RGB8-only Multi-frame True Color SC IOD. New identities follow ImageOptions.
func BuildRasterPage(ctx context.Context, page RasterPage, opts ImageOptions) (*object.File, ImportReport, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	if page.PhotometricInterpretation != "MONOCHROME1" && page.PhotometricInterpretation != "MONOCHROME2" && page.PhotometricInterpretation != "RGB" {
		return nil, ImportReport{}, ErrUnsupportedImage
	}
	if (page.PhotometricInterpretation == "RGB") != (page.Samples == 3) {
		return nil, ImportReport{}, ErrUnsupportedImage
	}
	limits, err := opts.Limits.normalized()
	if err != nil {
		return nil, ImportReport{}, err
	}
	if _, err := admitImage(imageLayout{rows: page.Rows, columns: page.Columns, samples: page.Samples, bits: page.BitsStored}, limits); err != nil {
		return nil, ImportReport{}, err
	}
	if opts.SOPClassUID != "" && opts.SOPClassUID != SecondaryCaptureImageStorage {
		return nil, ImportReport{}, ErrUnsupportedSOPClass
	}
	opts.Format = Raw
	opts.Raw = RawOptions{Rows: page.Rows, Columns: page.Columns, Frames: 1, Samples: page.Samples,
		BitsAllocated: 8, BitsStored: page.BitsStored}
	if page.BitsStored > 8 {
		opts.Raw.BitsAllocated = 16
	}
	opts.ImageType = `DERIVED\SECONDARY`
	opts.SOPClassUID = SecondaryCaptureImageStorage
	f, report, err := FromImages(ctx, [][]byte{page.Data}, opts)
	if err != nil {
		return nil, ImportReport{}, err
	}
	if page.PhotometricInterpretation == "MONOCHROME1" {
		f.Dataset.Put(stringElement(tagPhotometric, core.VRCS, "MONOCHROME1"))
	}
	if err := checkImported(ctx, f); err != nil {
		return nil, ImportReport{}, err
	}
	report.PhotometricInterpretation = page.PhotometricInterpretation
	return f, report, nil
}
