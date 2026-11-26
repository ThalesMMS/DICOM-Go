package pixeldata

import (
	"context"
	"encoding/binary"
	"fmt"
	"math"

	"github.com/ThalesMMS/dicom-go/core"
	"github.com/ThalesMMS/dicom-go/object"
	"github.com/ThalesMMS/dicom-go/pixeldata/display"
	"github.com/ThalesMMS/dicom-go/transfer"
)

// NativePlanarRecovery declares the producer's actual native color layout.
// Sample values and byte counts cannot distinguish these layouts reliably.
type NativePlanarRecovery uint8

const (
	NativePlanarDeclared    NativePlanarRecovery = iota
	NativePlanarInterleaved                      // Each frame contains color-by-pixel samples.
	NativePlanarByFrame                          // Each frame contains consecutive component planes.
	// NativePlanarWholeImage is a nonstandard multi-frame layout: all frames
	// of component 1, then all frames of component 2, etc. It is normalized
	// to standard per-frame component planes, with Planar Configuration 1.
	NativePlanarWholeImage
)

const (
	DiagnosticNativePlanarOverride      = "native-planar-layout-overridden"
	DiagnosticNativeWholeImagePlanar    = "native-whole-image-planar-reordered"
	DiagnosticNativeOWByteSwap          = "native-ow-byte-swap"
	DiagnosticRLEReversedByteSegments   = "rle-reversed-byte-segments"
	DiagnosticWrongPaletteAttributeTags = "wrong-palette-attribute-tags"
	DiagnosticRetiredARGBToRGB          = "retired-argb-to-rgb"
	DiagnosticRetiredCMYKToRGB          = "retired-cmyk-additive-to-rgb"
	DiagnosticRetiredHSVToRGB           = "retired-hsv-full-range-to-rgb"
)

func requiresLegacyPixelRepair(r DecodeRecovery) bool {
	return r.NativePlanarLayout != NativePlanarDeclared || r.NativeOWByteSwap || r.RLEReversedByteSegments || r.WrongPaletteAttributeTags || r.ConvertRetiredColorModels
}

// Called only after the complete input/frame/pixel/native allocation admission.
func withLegacyPixelRepairs(ctx context.Context, dataset *object.Object, source transfer.Syntax, m Metadata, r DecodeRecovery) (*object.Object, Metadata, []string, error) {
	if !requiresLegacyPixelRepair(r) {
		return dataset, m, nil, nil
	}
	fail := func() (*object.Object, Metadata, []string, error) {
		return nil, Metadata{}, nil, fmt.Errorf("%w: unsupported legacy pixel repair layout", ErrInvalidMetadata)
	}
	if r.NativePlanarLayout > NativePlanarWholeImage {
		return fail()
	}
	if (r.NativePlanarLayout != NativePlanarDeclared || r.NativeOWByteSwap) && source.Encapsulated {
		return fail()
	}
	if err := ctx.Err(); err != nil {
		return nil, Metadata{}, nil, err
	}
	out := object.FromElements(dataset.Elements(), nil)
	out.SetValueByteOrder(dataset.ValueByteOrder())
	var diagnostics []string
	if r.WrongPaletteAttributeTags {
		changed, err := repairPaletteTags(out, m)
		if err != nil {
			return nil, Metadata{}, nil, err
		}
		if changed {
			diagnostics = append(diagnostics, DiagnosticWrongPaletteAttributeTags)
		}
	}
	if r.NativeOWByteSwap || r.NativePlanarLayout != NativePlanarDeclared {
		pixel, ok := out.Get(core.TagPixelData)
		raw, rawOK := pixel.RawBytes()
		size := m.TotalSize()
		if !ok || !rawOK || size <= 0 || size > int64(len(raw)) || int64(len(raw)) > size+size%2 || (m.BitsAllocated != 8 && m.BitsAllocated != 16) {
			return fail()
		}
		if r.NativeOWByteSwap {
			if pixel.VR() != core.VROW || len(raw)%2 != 0 {
				return fail()
			}
			swapped := make([]byte, len(raw))
			for i := 0; i < len(raw); i += 2 {
				if i%8192 == 0 {
					if err := ctx.Err(); err != nil {
						return nil, Metadata{}, nil, err
					}
				}
				swapped[i], swapped[i+1] = raw[i+1], raw[i]
			}
			raw = swapped
			diagnostics = append(diagnostics, DiagnosticNativeOWByteSwap)
		}
		if r.NativePlanarLayout != NativePlanarDeclared {
			if m.PixelRepresentation != 0 || m.SamplesPerPixel < 3 || m.SamplesPerPixel > 4 || m.SamplesPerPixel != photometricSamples(normalizedPhotometric(m.PhotometricInterpretation)) || m.BitsStored == 0 || m.BitsStored > m.BitsAllocated || m.HighBit != m.BitsStored-1 {
				return fail()
			}
			switch normalizedPhotometric(m.PhotometricInterpretation) {
			case "RGB", "YBR_FULL", "ARGB", "CMYK", "HSV":
			default:
				return fail()
			}
			planar := uint16(0)
			if r.NativePlanarLayout != NativePlanarInterleaved {
				planar = 1
			}
			if r.NativePlanarLayout == NativePlanarWholeImage {
				reordered := make([]byte, len(raw))
				plane := int(m.Rows) * int(m.Columns) * m.BytesPerSample()
				for f := 0; f < m.NumberOfFrames; f++ {
					for c := 0; c < int(m.SamplesPerPixel); c++ {
						from := (c*m.NumberOfFrames + f) * plane
						to := (f*int(m.SamplesPerPixel) + c) * plane
						for copied := 0; copied < plane; {
							if err := ctx.Err(); err != nil {
								return nil, Metadata{}, nil, err
							}
							n := min(8192, plane-copied)
							copy(reordered[to+copied:to+copied+n], raw[from+copied:from+copied+n])
							copied += n
						}
					}
				}
				raw = reordered
				diagnostics = append(diagnostics, DiagnosticNativeWholeImagePlanar)
			} else if !m.PlanarConfigurationPresent || m.PlanarConfiguration != planar {
				diagnostics = append(diagnostics, DiagnosticNativePlanarOverride)
			}
			m.PlanarConfiguration, m.PlanarConfigurationPresent = planar, true
			out.Put(uint16Element(tagPlanarConfiguration, planar, out.ValueByteOrder()))
		}
		out.Put(core.NewRawElement(core.TagPixelData, pixel.VR(), raw))
	}
	return out, m, diagnostics, nil
}

func repairPaletteTags(out *object.Object, m Metadata) (bool, error) {
	var wrong, canonical []core.Tag
	for c := uint16(1); c <= 3; c++ {
		wrong = append(wrong, core.NewTag(0x0028, 0x1110+c), core.NewTag(0x0028, 0x1210+c))
		canonical = append(canonical, core.NewTag(0x0028, 0x1100+c), core.NewTag(0x0028, 0x1200+c))
	}
	count := 0
	for _, tag := range wrong {
		if out.Has(tag) {
			count++
		}
	}
	if count == 0 {
		return false, nil
	}
	if count != 6 || normalizedPhotometric(m.PhotometricInterpretation) != "PALETTE COLOR" && normalizedPhotometric(m.PhotometricInterpretation) != "ARGB" {
		return false, fmt.Errorf("%w: incomplete legacy palette tag set", ErrInvalidMetadata)
	}
	for c, tag := range canonical {
		if out.Has(tag) || out.Has(core.NewTag(0x0028, 0x1221+uint16(c/2))) {
			return false, fmt.Errorf("%w: conflicting palette tag sets", ErrInvalidMetadata)
		}
	}
	for i, tag := range wrong {
		elem, _ := out.Get(tag)
		elem.Header.Tag = canonical[i]
		out.Remove(tag)
		out.Put(elem)
	}
	if err := boundedLegacyPalette(out); err != nil {
		return false, err
	}
	if palette, err := display.PaletteFromObject(out); err != nil || palette == nil {
		return false, fmt.Errorf("%w: malformed legacy palette", ErrInvalidMetadata)
	}
	return true, nil
}

func isRetiredColor(name string) bool {
	switch normalizedPhotometric(name) {
	case "ARGB", "CMYK", "HSV":
		return true
	}
	return false
}

func retiredColorFrames(ctx context.Context, dataset *object.Object, source transfer.Syntax, frames Frames, m Metadata) (Frames, Metadata, string, error) {
	name := normalizedPhotometric(m.PhotometricInterpretation)
	if !isRetiredColor(name) {
		return frames, m, "", nil
	}
	fail := func() (Frames, Metadata, string, error) {
		return Frames{}, Metadata{}, "", fmt.Errorf("%w: unsupported retired color normalization layout", ErrUnsupportedDecompression)
	}
	channels := 4
	code := DiagnosticRetiredARGBToRGB
	if name == "HSV" {
		channels, code = 3, DiagnosticRetiredHSVToRGB
	} else if name == "CMYK" {
		code = DiagnosticRetiredCMYKToRGB
	}
	if source.Encapsulated || m.PixelRepresentation != 0 || m.SamplesPerPixel != uint16(channels) || (m.BitsAllocated != 8 && m.BitsAllocated != 16) || m.BitsStored != m.BitsAllocated || m.HighBit != m.BitsStored-1 || !m.PlanarConfigurationPresent || m.PlanarConfiguration > 1 {
		return fail()
	}
	var palette *display.PaletteColorLUT
	if name == "ARGB" {
		var err error
		if err := boundedLegacyPalette(dataset); err != nil {
			return Frames{}, Metadata{}, "", err
		}
		palette, err = display.PaletteFromObject(dataset)
		if err != nil {
			return Frames{}, Metadata{}, "", err
		}
	}
	pixels, width := int(m.Rows)*int(m.Columns), m.BytesPerSample()
	maximum := float64(uint32(1)<<m.BitsStored - 1)
	out := Frames{Rows: frames.Rows, Columns: frames.Columns, Data: make([][]byte, len(frames.Data)), Diagnostics: frames.Diagnostics, PhotometricInterpretation: "RGB"}
	for f, raw := range frames.Data {
		if len(raw) != pixels*channels*width {
			return fail()
		}
		rgb := make([]byte, pixels*3*width)
		sample := func(i, c int) float64 {
			pos := i*channels + c
			if m.PlanarConfiguration == 1 {
				pos = c*pixels + i
			}
			if width == 1 {
				return float64(raw[pos])
			}
			return float64(binary.LittleEndian.Uint16(raw[pos*2:]))
		}
		for i := 0; i < pixels; i++ {
			if i%4096 == 0 {
				if err := ctx.Err(); err != nil {
					return Frames{}, Metadata{}, "", err
				}
			}
			var values [3]float64
			switch name {
			case "ARGB":
				alpha := int(sample(i, 0))
				if alpha > 0 {
					if palette == nil {
						return Frames{}, Metadata{}, "", display.ErrMissingPalette
					}
					for c, lut := range [3]*display.LUT{palette.Red, palette.Green, palette.Blue} {
						top := uint32(1)<<lut.BitsPerEntry - 1
						value16 := (uint32(lut.Lookup(alpha))*65535 + top/2) / top
						if width == 1 {
							values[c] = float64(value16 >> 8)
						} else {
							values[c] = float64(value16)
						}
					}
				} else {
					for c := range values {
						values[c] = sample(i, c+1)
					}
				}
			case "CMYK":
				for c := range values {
					values[c] = max(0, maximum-sample(i, c)-sample(i, 3))
				}
			case "HSV":
				h, s, v := sample(i, 0)*6/(maximum+1), sample(i, 1)/maximum, sample(i, 2)
				chroma := v * s
				x := chroma * (1 - math.Abs(math.Mod(h, 2)-1))
				switch int(h) % 6 {
				case 0:
					values = [3]float64{chroma, x, 0}
				case 1:
					values = [3]float64{x, chroma, 0}
				case 2:
					values = [3]float64{0, chroma, x}
				case 3:
					values = [3]float64{0, x, chroma}
				case 4:
					values = [3]float64{x, 0, chroma}
				case 5:
					values = [3]float64{chroma, 0, x}
				}
				for c := range values {
					values[c] += v - chroma
				}
			}
			for c, value := range values {
				value = math.Floor(value)
				if width == 1 {
					rgb[3*i+c] = byte(value)
				} else {
					binary.LittleEndian.PutUint16(rgb[(3*i+c)*2:], uint16(value))
				}
			}
		}
		out.Data[f] = rgb
	}
	m.SamplesPerPixel, m.PhotometricInterpretation, m.PlanarConfiguration, m.PlanarConfigurationPresent = 3, "RGB", 0, true
	return out, m, code, ctx.Err()
}

func removeRetiredColorAttributes(out *object.Object) {
	for _, tag := range append(append(append([]core.Tag{}, paletteDescriptorTags[:]...), paletteDataTags[:]...), segmentedPaletteTags[:]...) {
		out.Remove(tag)
	}
	for _, tag := range nativeRewritePaletteFreeAttrs {
		out.Remove(tag)
	}
	out.Remove(core.NewTag(0x0028, 0x2000))
	out.Remove(core.NewTag(0x0028, 0x2002))
}

// LUT indices are 16 bits. Check encoded attributes before the display reader
// allocates words/entries, including a finite allowance for segmented tables.
func boundedLegacyPalette(dataset *object.Object) error {
	for _, family := range []struct {
		tags  [3]core.Tag
		limit int
	}{
		{paletteDescriptorTags, 6}, {paletteDataTags, 2 * 65536}, {segmentedPaletteTags, 8 * (65536 + 2)},
	} {
		for _, tag := range family.tags {
			if elem, present := dataset.Get(tag); present {
				if raw, ok := elem.RawBytes(); !ok || len(raw) > family.limit {
					return fmt.Errorf("%w: legacy palette exceeds bounded profile", ErrUnsupportedDecompression)
				}
			}
		}
	}
	return nil
}
