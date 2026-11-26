package display

import (
	"image"
	"math"
	"strconv"
	"strings"

	"github.com/ThalesMMS/dicom-go/core"
	"github.com/ThalesMMS/dicom-go/object"

	"github.com/ThalesMMS/dicom-go/internal/gridindex"
)

// PresentationLUTShapeLinOD is the Presentation LUT Shape that makes the
// input linear in optical density on a print (PS3.3 C.11.6.1.2).
const PresentationLUTShapeLinOD = "LIN OD"

var (
	tagMinDensity            = core.NewTag(0x2010, 0x0120)
	tagMaxDensity            = core.NewTag(0x2010, 0x0130)
	tagIllumination          = core.NewTag(0x2010, 0x015E)
	tagReflectedAmbientLight = core.NewTag(0x2010, 0x0160)
)

// Unit maps one modality value to the normalized display range [0,1] through
// the configured VOI LUT or window, without 8-bit quantization.
func (m VOIByteMapper) Unit(value float64) float64 {
	if m.voi.LUT != nil {
		value = float64(m.voi.LUT.Lookup(gridindex.Nearest(value)))
	}
	if m.rangeSpan > 0 {
		return clampFloat((value-m.rangeMin)/m.rangeSpan, 0, 1)
	}
	return m.voi.WindowUnit(value)
}

// RenderGray16 is RenderGray with 16-bit output: the VOI output as P-values
// 0..65535 in the MONOCHROME2 convention, for calibrated displays, printers
// and exports that keep more than 8 bits.
func RenderGray16(frame Frame) (*image.Gray16, error) {
	if frame.Format.BitsAllocated != 8 && frame.Format.BitsAllocated != 16 {
		return nil, frameValidationError("BitsAllocated", ErrUnsupportedBitsAllocated)
	}
	values, err := DecodeModality(frame)
	if err != nil {
		return nil, err
	}
	reader := newStoredPixelReader(frame.Format, frame.Format.ByteOrder)
	mapper := frameVOIMapper(frame.VOI, frame.Modality, &reader)
	out := image.NewGray16(image.Rect(0, 0, frame.Columns, frame.Rows))
	for i, value := range values {
		p := uint16(math.Round(mapper.Unit(value) * 65535))
		out.Pix[2*i], out.Pix[2*i+1] = uint8(p>>8), uint8(p)
	}
	return out, nil
}

// ApplyGray16 runs the presentation stages over a 16-bit P-value image in
// place: the Presentation LUT is indexed by the input scaled to its entries,
// the shutter writes its P-value and overlays burn the maximum value.
func (p Presentation) ApplyGray16(img *image.Gray16) {
	if img == nil {
		return
	}
	bounds := img.Bounds()
	for y := bounds.Min.Y; y < bounds.Max.Y; y++ {
		for x := bounds.Min.X; x < bounds.Max.X; x++ {
			i := img.PixOffset(x, y)
			value := uint16(img.Pix[i])<<8 | uint16(img.Pix[i+1])
			if p.LUT != nil && len(p.LUT.Entries) > 0 {
				index := gridindex.Nearest(float64(value) * float64(len(p.LUT.Entries)-1) / 65535)
				value = p.LUT.entry16(p.LUT.Entries[index])
			}
			if p.Invert {
				value = 65535 - value
			}
			img.Pix[i], img.Pix[i+1] = uint8(value>>8), uint8(value)
		}
	}
	if s := p.Shutter; s != nil && (s.Rectangle != nil || s.Circle != nil || s.Polygon != nil || s.Bitmap != nil) {
		masked := uint16(s.PresentationValue) * 257
		if s.PresentationPValueDefined {
			masked = s.PresentationPValue
		}
		for row := 0; row < bounds.Dy(); row++ {
			for col := 0; col < bounds.Dx(); col++ {
				if !s.visible(row, col) {
					i := img.PixOffset(bounds.Min.X+col, bounds.Min.Y+row)
					img.Pix[i], img.Pix[i+1] = uint8(masked>>8), uint8(masked)
				}
			}
		}
	}
	for _, overlay := range p.Overlays {
		// As in Overlay.Composite, only the part of the plane that overlaps
		// both the image and Data is visited, so the work is bounded by the
		// image whatever Overlay Rows and Columns declare.
		rowLo, rowHi := overlaySpan(overlay.OriginRow, overlay.dataRows(), bounds.Dy())
		colLo, colHi := overlaySpan(overlay.OriginCol, overlay.Columns, bounds.Dx())
		for r := rowLo; r < rowHi; r++ {
			for c := colLo; c < colHi; c++ {
				if !overlay.isSet(r, c) {
					continue
				}
				i := img.PixOffset(bounds.Min.X+overlay.OriginCol-1+c, bounds.Min.Y+overlay.OriginRow-1+r)
				img.Pix[i], img.Pix[i+1] = 0xFF, 0xFF
			}
		}
	}
}

// PrintDensityFromObject reads Min Density, Max Density (hundredths of OD),
// Illumination and Reflected Ambient Light (cd/m²) of a Basic Film Box or
// Presentation LUT, defaulting absent values to DefaultPrintDensity.
func PrintDensityFromObject(obj *object.Object) PrintDensity {
	out := DefaultPrintDensity
	if obj == nil {
		return out
	}
	read := func(tag core.Tag) (float64, bool) {
		if raw, ok := obj.GetRaw(tag); ok && len(raw) >= 2 {
			return float64(obj.ValueByteOrder().Uint16(raw)), true
		}
		if text, ok := obj.GetString(tag); ok {
			if value, err := strconv.ParseFloat(strings.TrimSpace(text), 64); err == nil {
				return value, true
			}
		}
		return 0, false
	}
	if value, ok := read(tagMinDensity); ok {
		out.MinDensity = value / 100
	}
	if value, ok := read(tagMaxDensity); ok && value > 0 {
		out.MaxDensity = value / 100
	}
	if value, ok := read(tagIllumination); ok && value > 0 {
		out.Illumination = value
	}
	if value, ok := read(tagReflectedAmbientLight); ok {
		out.ReflectedAmbientLight = value
	}
	return out.normalized()
}
