package display

import (
	"bytes"
	"encoding/binary"
	"errors"
	"image"
	"sort"
	"testing"

	"github.com/ThalesMMS/dicom-go/core"
	"github.com/ThalesMMS/dicom-go/dictionary/std"
	"github.com/ThalesMMS/dicom-go/object"
	"github.com/ThalesMMS/dicom-go/parser"
	"github.com/ThalesMMS/dicom-go/transfer"
)

// Bounds of FuzzPresentationFromObject: the input cap and the parser limits
// are the allocation guardrails for the data set itself; the invariants in
// checkFuzzLUT bound what the presentation readers build from it.
const (
	presentationFuzzMaxInput        = 256 << 10
	presentationFuzzMaxElementBytes = 64 << 10
	presentationFuzzMaxElements     = 1024
	presentationFuzzMaxDepth        = 8
)

// Flags of FuzzPresentationFromObject: the low two bits select the transfer
// syntax the input is parsed with, presentationFuzzLegacy reads legacy LUT
// encodings (LUTOptions.Legacy).
const (
	presentationFuzzExplicitLE = 0
	presentationFuzzImplicitLE = 1
	presentationFuzzExplicitBE = 2
	presentationFuzzLegacy     = 4
)

// fuzzElement is a data element for the seed encoder. Words holds 16-bit
// binary values (US, SS, OW) encoded in the syntax's byte order, Value holds
// bytes written verbatim (OB, text), and Items the items of a sequence.
type fuzzElement struct {
	Tag   core.Tag
	VR    string
	Words []uint16
	Value []byte
	Items [][]fuzzElement
}

func fzWords(group, element uint16, vr string, words ...uint16) fuzzElement {
	return fuzzElement{Tag: core.NewTag(group, element), VR: vr, Words: append([]uint16{}, words...)}
}

func fzBytes(group, element uint16, vr string, value []byte) fuzzElement {
	return fuzzElement{Tag: core.NewTag(group, element), VR: vr, Value: append([]byte{}, value...)}
}

// fzText is a text element padded to even length with a space.
func fzText(group, element uint16, vr, value string) fuzzElement {
	if len(value)%2 != 0 {
		value += " "
	}
	return fzBytes(group, element, vr, []byte(value))
}

func fzUID(group, element uint16, value string) fuzzElement {
	raw := []byte(value)
	if len(raw)%2 != 0 {
		raw = append(raw, 0)
	}
	return fzBytes(group, element, "UI", raw)
}

func fzSequence(group, element uint16, items ...[]fuzzElement) fuzzElement {
	return fuzzElement{Tag: core.NewTag(group, element), VR: "SQ", Items: items}
}

// fzLUTItem is a LUT sequence item: LUT Descriptor (numberOfEntries,
// firstMapped, bitsPerEntry) in descriptorVR and LUT Data in dataVR.
func fzLUTItem(descriptorVR string, entries, first, bits uint16, dataVR string, data ...uint16) []fuzzElement {
	return []fuzzElement{
		fzWords(0x0028, 0x3002, descriptorVR, entries, first, bits),
		fzWords(0x0028, 0x3006, dataVR, data...),
	}
}

// fzPacked8 packs 8-bit entries two per word, the first in the low byte.
func fzPacked8(entries ...uint8) []uint16 {
	out := make([]uint16, (len(entries)+1)/2)
	for i, entry := range entries {
		out[i/2] |= uint16(entry) << (8 * (i % 2))
	}
	return out
}

func fzRamp(count int, step uint16) []uint16 {
	out := make([]uint16, count)
	for i := range out {
		out[i] = uint16(i) * step
	}
	return out
}

// fzPalette is the three palette descriptors (VR descriptorVR) and, per
// channel, LUT Data (dataTag 0x120x) or Segmented LUT Data (0x122x).
func fzPalette(descriptorVR string, entries, first, bits uint16, dataElement uint16, red, green, blue []uint16) []fuzzElement {
	out := []fuzzElement{}
	for i, data := range [][]uint16{red, green, blue} {
		out = append(out, fzWords(0x0028, 0x1101+uint16(i), descriptorVR, entries, first, bits))
		if data != nil {
			out = append(out, fzWords(0x0028, dataElement+uint16(i), "OW", data...))
		}
	}
	return out
}

// fzOverlay is a 1-bit overlay group with Overlay Origin as signed values.
func fzOverlay(group, rows, columns uint16, originRow, originColumn int16, data []byte, extra ...fuzzElement) []fuzzElement {
	out := []fuzzElement{
		fzWords(group, 0x0010, "US", rows),
		fzWords(group, 0x0011, "US", columns),
		fzText(group, 0x0040, "CS", "G"),
		fzWords(group, 0x0050, "SS", uint16(originRow), uint16(originColumn)),
		fzWords(group, 0x0100, "US", 1),
		fzWords(group, 0x0102, "US", 0),
		fzBytes(group, 0x3000, "OB", data),
	}
	return append(out, extra...)
}

func fzShape(shapes string) fuzzElement { return fzText(0x0018, 0x1600, "CS", shapes) }

var fzLongLengthVRs = map[string]bool{
	"OB": true, "OD": true, "OF": true, "OL": true, "OV": true, "OW": true,
	"SQ": true, "SV": true, "UC": true, "UN": true, "UR": true, "UT": true, "UV": true,
}

// encodeFuzzDataSet writes elements (sorted by tag) in Explicit VR Little or
// Big Endian or Implicit VR Little Endian; sequences and items use undefined
// lengths. It writes exactly what it is given, including odd lengths and VRs
// the dictionary does not allow, which the seeds rely on.
func encodeFuzzDataSet(elements []fuzzElement, flags uint8) []byte {
	var order binary.AppendByteOrder = binary.LittleEndian
	if flags&3 == presentationFuzzExplicitBE {
		order = binary.BigEndian
	}
	explicit := flags&3 != presentationFuzzImplicitLE
	var buf bytes.Buffer
	var write func([]fuzzElement)
	u16 := func(v uint16) { buf.Write(order.AppendUint16(nil, v)) }
	u32 := func(v uint32) { buf.Write(order.AppendUint32(nil, v)) }
	write = func(elements []fuzzElement) {
		sorted := append([]fuzzElement(nil), elements...)
		sort.SliceStable(sorted, func(i, j int) bool { return sorted[i].Tag.Less(sorted[j].Tag) })
		for _, element := range sorted {
			u16(uint16(element.Tag.Group))
			u16(uint16(element.Tag.Element))
			value := element.Value
			if element.Words != nil {
				value = nil
				for _, word := range element.Words {
					value = order.AppendUint16(value, word)
				}
			}
			length := uint32(len(value))
			if element.VR == "SQ" {
				length = 0xFFFFFFFF
			}
			switch {
			case !explicit:
				u32(length)
			case fzLongLengthVRs[element.VR]:
				buf.WriteString(element.VR)
				u16(0)
				u32(length)
			default:
				buf.WriteString(element.VR)
				u16(uint16(length))
			}
			if element.VR != "SQ" {
				buf.Write(value)
				continue
			}
			for _, item := range element.Items {
				u16(0xFFFE)
				u16(0xE000)
				u32(0xFFFFFFFF)
				write(item)
				u16(0xFFFE)
				u16(0xE00D)
				u32(0)
			}
			u16(0xFFFE)
			u16(0xE0DD)
			u32(0)
		}
	}
	write(elements)
	return buf.Bytes()
}

// presentationFuzzSeeds are synthetic data sets written from PS3.3 C.11
// (Modality, VOI and Presentation LUTs), C.7.6.3.1.5 and C.7.9 (palette and
// segmented palette color LUTs), C.9 (overlays) and C.7.6.11 (display
// shutters). The malformed ones are modelled on the classes of defect fixed
// in DCMTK 87cccaa5 (8-bit LUT with an odd entry count), 99bae4af (partial
// palette), 601b227e / TALOS-2024-1957 (US/SS descriptor and US/OW LUT Data)
// and GDCM 21a79309 / TALOS-2024-1924 (LUT Data size that does not match the
// descriptor); none is taken from those projects' code or test files.
func presentationFuzzSeeds() map[string][]fuzzElement {
	ramp16 := fzRamp(4, 0x4000)
	unsignedCT := []fuzzElement{fzWords(0x0028, 0x0101, "US", 12), fzWords(0x0028, 0x0103, "US", 0)}
	signedCT := []fuzzElement{fzWords(0x0028, 0x0101, "US", 16), fzWords(0x0028, 0x0103, "US", 1)}
	with := func(parts ...[]fuzzElement) []fuzzElement {
		var out []fuzzElement
		for _, part := range parts {
			out = append(out, part...)
		}
		return out
	}
	modality := func(item []fuzzElement) []fuzzElement {
		return []fuzzElement{fzSequence(0x0028, 0x3000, item)}
	}
	voi := func(items ...[]fuzzElement) []fuzzElement {
		return []fuzzElement{fzSequence(0x0028, 0x3010, items...)}
	}
	presentation := func(item []fuzzElement) []fuzzElement {
		return []fuzzElement{fzSequence(0x2050, 0x0010, item)}
	}
	quadratic := make([]uint16, 0, 2*512+4*256)
	for i := 0; i < 512; i++ {
		quadratic = append(quadratic, 0, 0) // empty discrete segments
	}
	for i := 0; i < 256; i++ {
		quadratic = append(quadratic, 2, 512, 0, 0) // copy all of them
	}

	return map[string][]fuzzElement{
		// Modality LUT (PS3.3 C.11.1).
		"modality 16-bit":                  with(unsignedCT, modality(fzLUTItem("US", 4, 0, 16, "OW", ramp16...))),
		"modality 12-bit US data":          with(unsignedCT, modality(fzLUTItem("US", 3, 0, 12, "US", 0, 2048, 4095))),
		"modality 8-bit odd count packed":  with(unsignedCT, modality(fzLUTItem("US", 3, 0, 8, "OW", fzPacked8(10, 20, 30)...))),
		"modality 8-bit odd count words":   with(unsignedCT, modality(fzLUTItem("US", 3, 0, 8, "OW", 10, 20, 30))),
		"modality 8-bit odd byte length":   with(unsignedCT, modality([]fuzzElement{fzWords(0x0028, 0x3002, "US", 3, 0, 8), fzBytes(0x0028, 0x3006, "OB", []byte{10, 20, 30})})),
		"modality 8-bit single entry":      with(unsignedCT, modality(fzLUTItem("US", 1, 0, 8, "OW", 0x00FF))),
		"modality SS descriptor signed":    with(signedCT, modality(fzLUTItem("SS", 4, uint16(0xFC00), 16, "OW", ramp16...))),
		"modality US descriptor signed":    with(signedCT, modality(fzLUTItem("US", 4, uint16(0xFC00), 16, "US", ramp16...))),
		"modality SS descriptor SS data":   with(signedCT, modality(fzLUTItem("SS", 4, uint16(0x8000), 16, "SS", 0, 0x7FFF, 0x8000, 0xFFFF))),
		"modality OW descriptor":           with(unsignedCT, modality(fzLUTItem("OW", 4, 0, 16, "OW", ramp16...))),
		"modality descriptor two values":   with(unsignedCT, modality([]fuzzElement{fzWords(0x0028, 0x3002, "US", 4, 0), fzWords(0x0028, 0x3006, "OW", ramp16...)})),
		"modality descriptor odd length":   with(unsignedCT, modality([]fuzzElement{fzBytes(0x0028, 0x3002, "US", []byte{4, 0, 0, 0, 16}), fzWords(0x0028, 0x3006, "OW", ramp16...)})),
		"modality data shorter than count": with(unsignedCT, modality(fzLUTItem("US", 256, 0, 16, "OW", 1, 2))),
		"modality data longer than count":  with(unsignedCT, modality(fzLUTItem("US", 2, 0, 16, "OW", fzRamp(300, 7)...))),
		"modality zero entries short data": with(unsignedCT, modality(fzLUTItem("US", 0, 0, 16, "OW", 1, 2))),
		"modality zero entries 8-bit":      with(unsignedCT, modality(fzLUTItem("US", 0, 0, 8, "OW", fzPacked8(1, 2, 3)...))),
		"modality missing data":            with(unsignedCT, modality([]fuzzElement{fzWords(0x0028, 0x3002, "US", 4, 0, 16)})),
		"modality empty data":              with(unsignedCT, modality([]fuzzElement{fzWords(0x0028, 0x3002, "US", 4, 0, 16), fzBytes(0x0028, 0x3006, "OW", nil)})),
		"modality first mapped max":        with(unsignedCT, modality(fzLUTItem("US", 4, 0xFFFF, 16, "OW", ramp16...))),
		"modality first mapped min signed": with(signedCT, modality(fzLUTItem("SS", 4, 0x8000, 16, "OW", ramp16...))),
		"modality bits 16 of 8-bit values": with(unsignedCT, modality(fzLUTItem("US", 3, 0, 16, "OW", 0, 0x8080, 0x00FF))),
		"modality bits 8 of 16-bit values": with(unsignedCT, modality(fzLUTItem("US", 3, 0, 8, "OW", 0, 0x1234, 0xFFFF))),
		"modality bits 0":                  with(unsignedCT, modality(fzLUTItem("US", 2, 0, 0, "OW", 0, 1))),
		"modality bits 7":                  with(unsignedCT, modality(fzLUTItem("US", 3, 0, 7, "OW", 0, 300, 1000))),
		"modality bits 17":                 with(unsignedCT, modality(fzLUTItem("US", 2, 0, 17, "OW", 0, 100))),
		"modality bits 65535":              with(unsignedCT, modality(fzLUTItem("US", 2, 0, 0xFFFF, "OW", 0, 0xFFFF))),
		"modality entries above bits":      with(unsignedCT, modality(fzLUTItem("US", 2, 0, 12, "OW", 0, 5000))),
		"modality with rescale":            with(unsignedCT, []fuzzElement{fzText(0x0028, 0x1052, "DS", "-1024"), fzText(0x0028, 0x1053, "DS", "0")}, modality(fzLUTItem("US", 2, 0, 16, "OW", 0, 1))),
		"modality empty sequence":          with(unsignedCT, []fuzzElement{fzSequence(0x0028, 0x3000)}),
		"rescale only":                     with(unsignedCT, []fuzzElement{fzText(0x0028, 0x1052, "DS", "-1e308"), fzText(0x0028, 0x1053, "DS", "1e308")}),

		// VOI LUT (PS3.3 C.11.2).
		"voi window and two luts": with(unsignedCT, []fuzzElement{
			fzText(0x0028, 0x1050, "DS", "40\\400"), fzText(0x0028, 0x1051, "DS", "80\\0"),
			fzText(0x0028, 0x1055, "LO", "A\\B"),
		}, voi(fzLUTItem("US", 4, 0, 16, "OW", ramp16...), fzLUTItem("US", 3, 0, 8, "OW", fzPacked8(1, 2, 3)...))),
		"voi sigmoid zero width": with(unsignedCT, []fuzzElement{
			fzText(0x0028, 0x1050, "DS", "0"), fzText(0x0028, 0x1051, "DS", "0"), fzText(0x0028, 0x1056, "CS", "SIGMOID"),
		}),
		"voi linear exact":         with(unsignedCT, []fuzzElement{fzText(0x0028, 0x1050, "DS", "1e308"), fzText(0x0028, 0x1051, "DS", "1"), fzText(0x0028, 0x1056, "CS", "LINEAR_EXACT")}),
		"voi bad then good":        with(signedCT, voi(fzLUTItem("US", 9, 0, 16, "OW", 1), fzLUTItem("SS", 2, 0xFFFF, 16, "OW", 0, 1))),
		"voi rescale below zero":   with(unsignedCT, []fuzzElement{fzText(0x0028, 0x1052, "DS", "-1024")}, voi(fzLUTItem("US", 2, 0xFC00, 12, "OW", 0, 4095))),
		"voi data size mismatch 8": with(unsignedCT, voi(fzLUTItem("US", 5, 0, 8, "OW", fzPacked8(1, 2, 3, 4, 5, 6, 7, 8)...))),
		"functional groups": with(unsignedCT, []fuzzElement{
			fzSequence(0x5200, 0x9229, []fuzzElement{
				fzSequence(0x0028, 0x9145, fzLUTItem("US", 2, 0, 16, "OW", 0, 1)),
				fzSequence(0x0028, 0x9132, []fuzzElement{fzText(0x0028, 0x1050, "DS", "1"), fzText(0x0028, 0x1051, "DS", "2")}),
			}),
			fzSequence(0x5200, 0x9230, []fuzzElement{
				fzSequence(0x0028, 0x9132, append(fzLUTItem("US", 0, 0, 16, "OW", 1), fzSequence(0x0028, 0x3010, fzLUTItem("US", 2, 0, 16, "OW", 0, 1)))),
			}),
		}),
		"functional group pixel value transformation": with(signedCT, []fuzzElement{
			fzSequence(0x5200, 0x9230, []fuzzElement{
				fzSequence(0x0028, 0x9145, []fuzzElement{fzText(0x0028, 0x1052, "DS", "-1024"), fzText(0x0028, 0x1053, "DS", "1")}),
				fzSequence(0x0028, 0x9132, []fuzzElement{fzSequence(0x0028, 0x3010, fzLUTItem("US", 2, 0xFC00, 16, "OW", 0, 1))}),
			}),
		}),

		// Presentation LUT (PS3.3 C.11.6).
		"presentation lut 12-bit":       presentation(fzLUTItem("US", 256, 0, 12, "OW", fzRamp(256, 16)...)),
		"presentation lut 8-bit odd":    presentation(fzLUTItem("US", 255, 0, 8, "OW", fzPacked8(make([]uint8, 255)...)...)),
		"presentation lut SS first":     presentation(fzLUTItem("SS", 4, 0x8000, 16, "US", ramp16...)),
		"presentation lut short":        presentation(fzLUTItem("US", 4096, 0, 16, "OW", 1)),
		"presentation lut bad bits":     presentation(fzLUTItem("US", 4, 0, 1, "OW", ramp16...)),
		"presentation shape inverse":    {fzText(0x2050, 0x0020, "CS", "INVERSE")},
		"presentation shape identity":   {fzText(0x2050, 0x0020, "CS", "IDENTITY")},
		"presentation shape lin od":     {fzWords(0x2010, 0x0120, "US", 20), fzWords(0x2010, 0x0130, "US", 300), fzWords(0x2010, 0x015E, "US", 2000), fzWords(0x2010, 0x0160, "US", 10), fzText(0x2050, 0x0020, "CS", "LIN OD")},
		"presentation lin od extremes":  {fzWords(0x2010, 0x0120, "US", 0xFFFF), fzWords(0x2010, 0x0130, "US", 0xFFFF), fzWords(0x2010, 0x015E, "US", 0), fzWords(0x2010, 0x0160, "US", 0xFFFF), fzText(0x2050, 0x0020, "CS", "LIN OD")},
		"presentation lin od dark":      {fzWords(0x2010, 0x0120, "US", 0), fzWords(0x2010, 0x0130, "US", 0xFFFF), fzWords(0x2010, 0x015E, "US", 1), fzWords(0x2010, 0x0160, "US", 0), fzText(0x2050, 0x0020, "CS", "LIN OD")},
		"presentation lut and inverse":  append(presentation(fzLUTItem("US", 2, 0, 16, "OW", 0, 0xFFFF)), fzText(0x2050, 0x0020, "CS", "INVERSE")),
		"presentation state refused":    {fzUID(0x0008, 0x0016, "1.2.840.10008.5.1.4.1.1.11.1"), fzText(0x2050, 0x0020, "CS", "INVERSE")},
		"presentation lut empty item":   {fzSequence(0x2050, 0x0010, []fuzzElement{})},
		"presentation lut zero entries": presentation(fzLUTItem("US", 0, 0, 16, "OW", 0, 1, 2, 3)),

		// Palette color LUTs (PS3.3 C.7.6.3.1.5).
		"palette 16-bit":              with(unsignedCT, fzPalette("US", 4, 0, 16, 0x1201, ramp16, ramp16, ramp16)),
		"palette 8-bit odd count":     with(unsignedCT, fzPalette("US", 3, 0, 8, 0x1201, fzPacked8(1, 2, 3), fzPacked8(4, 5, 6), fzPacked8(7, 8, 9))),
		"palette SS descriptor":       with(signedCT, fzPalette("SS", 4, 0xFF00, 16, 0x1201, ramp16, ramp16, ramp16)),
		"palette partial blue absent": with(unsignedCT, fzPalette("US", 4, 0, 16, 0x1201, ramp16, ramp16, nil)[:4]),
		"palette partial data absent": with(unsignedCT, fzPalette("US", 4, 0, 16, 0x1201, ramp16, nil, ramp16)),
		"palette data without descriptor": with(unsignedCT, []fuzzElement{
			fzWords(0x0028, 0x1101, "US", 4, 0, 16), fzWords(0x0028, 0x1201, "OW", ramp16...),
			fzWords(0x0028, 0x1202, "OW", ramp16...), fzWords(0x0028, 0x1203, "OW", ramp16...),
		}),
		"palette channel lengths differ": with(unsignedCT, fzPalette("US", 256, 0, 16, 0x1201, fzRamp(256, 256), fzRamp(255, 256), fzRamp(1, 1))),
		"palette zero entries short":     with(unsignedCT, fzPalette("US", 0, 0, 16, 0x1201, ramp16, ramp16, ramp16)),
		"palette first mapped max":       with(unsignedCT, fzPalette("US", 4, 0xFFFF, 16, 0x1201, ramp16, ramp16, ramp16)),
		"palette bits 8 of 16-bit":       with(unsignedCT, fzPalette("US", 4, 0, 8, 0x1201, ramp16, ramp16, ramp16)),

		// Segmented palette color LUTs (PS3.3 C.7.9.2).
		"segmented valid": with(unsignedCT, fzPalette("US", 8, 0, 16, 0x1221,
			[]uint16{0, 2, 0, 100, 1, 3, 400, 2, 1, 0, 0},
			[]uint16{0, 1, 0, 1, 7, 0xFFFF},
			[]uint16{0, 8, 1, 2, 3, 4, 5, 6, 7, 8})),
		"segmented full range": with(unsignedCT, fzPalette("US", 0, 0, 16, 0x1221,
			[]uint16{0, 1, 0, 1, 0xFFFF, 0xFFFF}, []uint16{0, 1, 0xFFFF, 1, 0xFFFF, 0}, []uint16{0, 1, 0, 1, 0xFFFF, 0xFFFF})),
		"segmented length beyond data":  with(unsignedCT, fzPalette("US", 4, 0, 16, 0x1221, []uint16{0, 100, 1, 2}, []uint16{0, 4, 1, 2, 3, 4}, []uint16{0, 4, 1, 2, 3, 4})),
		"segmented length beyond count": with(unsignedCT, fzPalette("US", 2, 0, 16, 0x1221, []uint16{0, 4, 1, 2, 3, 4}, []uint16{0, 2, 1, 2}, []uint16{0, 2, 1, 2})),
		"segmented bad opcode":          with(unsignedCT, fzPalette("US", 2, 0, 16, 0x1221, []uint16{3, 2, 1, 2}, []uint16{0xFFFF, 2, 1, 2}, []uint16{0, 2, 1, 2})),
		"segmented linear first":        with(unsignedCT, fzPalette("US", 4, 0, 16, 0x1221, []uint16{1, 4, 255}, []uint16{0, 1, 0, 1, 3, 255}, []uint16{0, 4, 1, 2, 3, 4})),
		"segmented linear truncated":    with(unsignedCT, fzPalette("US", 4, 0, 16, 0x1221, []uint16{0, 1, 0, 1, 3}, []uint16{0, 1, 0, 1}, []uint16{0})),
		"segmented indirect self":       with(unsignedCT, fzPalette("US", 2, 0, 16, 0x1221, []uint16{2, 1, 0, 0}, []uint16{0, 2, 1, 2}, []uint16{0, 2, 1, 2})),
		"segmented indirect odd offset": with(unsignedCT, fzPalette("US", 2, 0, 16, 0x1221, []uint16{0, 1, 5, 2, 1, 1, 0}, []uint16{0, 2, 1, 2}, []uint16{0, 2, 1, 2})),
		"segmented indirect far offset": with(unsignedCT, fzPalette("US", 2, 0, 16, 0x1221, []uint16{0, 1, 5, 2, 1, 0xFFFE, 0xFFFF}, []uint16{0, 2, 1, 2}, []uint16{0, 2, 1, 2})),
		"segmented indirect truncated":  with(unsignedCT, fzPalette("US", 2, 0, 16, 0x1221, []uint16{0, 1, 5, 2, 1, 0}, []uint16{0, 2, 1, 2}, []uint16{0, 2, 1, 2})),
		"segmented indirect overrun":    with(unsignedCT, fzPalette("US", 4, 0, 16, 0x1221, []uint16{0, 2, 5, 6, 2, 3, 0, 0}, []uint16{0, 4, 1, 2, 3, 4}, []uint16{0, 4, 1, 2, 3, 4})),
		"segmented empty copies":        with(unsignedCT, fzPalette("US", 2, 0, 16, 0x1221, quadratic, []uint16{0, 2, 1, 2}, []uint16{0, 2, 1, 2})),
		"segmented odd byte length": with(unsignedCT, []fuzzElement{
			fzWords(0x0028, 0x1101, "US", 2, 0, 16), fzWords(0x0028, 0x1102, "US", 2, 0, 16), fzWords(0x0028, 0x1103, "US", 2, 0, 16),
			fzBytes(0x0028, 0x1221, "OW", []byte{0, 0, 2, 0, 1}), fzWords(0x0028, 0x1222, "OW", 0, 2, 1, 2), fzWords(0x0028, 0x1223, "OW", 0, 2, 1, 2),
		}),
		"segmented bits 8 of 16-bit": with(unsignedCT, fzPalette("US", 2, 0, 8, 0x1221, []uint16{0, 2, 0, 0x1234}, []uint16{0, 2, 1, 2}, []uint16{0, 2, 1, 2})),

		// Overlays (PS3.3 C.9).
		"overlay 4x4":           fzOverlay(0x6000, 4, 4, 1, 1, []byte{0xFF, 0x00}),
		"overlay maximal plane": fzOverlay(0x6000, 0xFFFF, 0xFFFF, 1, 1, []byte{0xFF, 0xFF}),
		"overlay short data":    fzOverlay(0x6002, 64, 64, -32768, 32767, []byte{0x01, 0x02}),
		"overlay frames":        fzOverlay(0x6004, 2, 2, 1, 1, []byte{0x0F, 0x00}, fzText(0x6004, 0x0015, "IS", "4611686018427387905"), fzWords(0x6004, 0x0051, "US", 0xFFFF)),
		"overlay zero rows":     fzOverlay(0x601E, 0, 8, 1, 1, []byte{0xFF, 0x00}),
		"overlay embedded":      fzOverlay(0x6006, 4, 4, 1, 1, []byte{0xFF, 0x00}, fzWords(0x6006, 0x0100, "US", 16)),
		"overlay short fields": {
			fzBytes(0x6008, 0x0010, "US", []byte{4}), fzBytes(0x6008, 0x0011, "US", nil),
			fzBytes(0x6008, 0x0050, "SS", []byte{1, 0}), fzBytes(0x6008, 0x3000, "OW", []byte{0xFF}),
		},

		// Display shutters (PS3.3 C.7.6.11).
		"shutter rectangle":          {fzShape("RECTANGULAR"), fzText(0x0018, 0x1602, "IS", "2"), fzText(0x0018, 0x1604, "IS", "12"), fzText(0x0018, 0x1606, "IS", "3"), fzText(0x0018, 0x1608, "IS", "14"), fzWords(0x0018, 0x1622, "US", 0x8000)},
		"shutter rectangle inverted": {fzShape("RECTANGULAR"), fzText(0x0018, 0x1602, "IS", "12"), fzText(0x0018, 0x1604, "IS", "2"), fzText(0x0018, 0x1606, "IS", "3"), fzText(0x0018, 0x1608, "IS", "14")},
		"shutter rectangle missing":  {fzShape("RECTANGULAR"), fzText(0x0018, 0x1602, "IS", "1")},
		"shutter circle":             {fzShape("CIRCULAR"), fzText(0x0018, 0x1610, "IS", "8\\8"), fzText(0x0018, 0x1612, "IS", "5")},
		"shutter circle zero radius": {fzShape("CIRCULAR"), fzText(0x0018, 0x1610, "IS", "8\\8"), fzText(0x0018, 0x1612, "IS", "0")},
		"shutter circle negative":    {fzShape("CIRCULAR"), fzText(0x0018, 0x1610, "IS", "8\\8"), fzText(0x0018, 0x1612, "IS", "-5")},
		"shutter circle one center":  {fzShape("CIRCULAR"), fzText(0x0018, 0x1610, "IS", "8"), fzText(0x0018, 0x1612, "IS", "5")},
		"shutter circle far center":  {fzShape("CIRCULAR"), fzText(0x0018, 0x1610, "IS", "999999999999\\-999999999999"), fzText(0x0018, 0x1612, "IS", "1")},
		"shutter circle huge radius": {fzShape("CIRCULAR"), fzText(0x0018, 0x1610, "IS", "1\\1"), fzText(0x0018, 0x1612, "IS", "999999999999")},
		"shutter polygon":            {fzShape("POLYGONAL"), fzText(0x0018, 0x1620, "IS", "1\\1\\16\\8\\1\\16")},
		"shutter polygon two points": {fzShape("POLYGONAL"), fzText(0x0018, 0x1620, "IS", "1\\1\\16\\16")},
		"shutter polygon collinear":  {fzShape("POLYGONAL"), fzText(0x0018, 0x1620, "IS", "1\\1\\8\\8\\16\\16")},
		"shutter polygon coincident": {fzShape("POLYGONAL"), fzText(0x0018, 0x1620, "IS", "4\\4\\4\\4\\4\\4")},
		"shutter polygon odd count":  {fzShape("POLYGONAL"), fzText(0x0018, 0x1620, "IS", "1\\1\\16\\8\\1")},
		"shutter polygon extremes":   {fzShape("POLYGONAL"), fzText(0x0018, 0x1620, "IS", "-999999999999\\-999999999999\\999999999999\\0\\0\\999999999999")},
		"shutter bitmap": with([]fuzzElement{fzShape("BITMAP"), fzWords(0x0018, 0x1623, "US", 0x6000)},
			fzOverlay(0x6000, 4, 4, 1, 1, []byte{0xFF, 0x00})),
		"shutter bitmap odd group":   {fzShape("BITMAP"), fzWords(0x0018, 0x1623, "US", 0x6001)},
		"shutter bitmap no overlay":  {fzShape("BITMAP"), fzWords(0x0018, 0x1623, "US", 0x6002)},
		"shutter bitmap short plane": with([]fuzzElement{fzShape("BITMAP"), fzWords(0x0018, 0x1623, "US", 0x6000)}, fzOverlay(0x6000, 0xFFFF, 0xFFFF, 1, 1, []byte{0xFF, 0x00})),
		"shutter without shape":      {fzText(0x0018, 0x1612, "IS", "5")},
		"shutter unknown shape":      {fzShape("ELLIPTICAL")},
		"shutter color two values":   {fzShape("CIRCULAR"), fzText(0x0018, 0x1610, "IS", "8\\8"), fzText(0x0018, 0x1612, "IS", "5"), fzWords(0x0018, 0x1624, "US", 1, 2)},
		"shutter four shapes": with([]fuzzElement{
			fzShape("RECTANGULAR\\CIRCULAR\\POLYGONAL\\BITMAP"),
			fzText(0x0018, 0x1602, "IS", "1"), fzText(0x0018, 0x1604, "IS", "16"), fzText(0x0018, 0x1606, "IS", "1"), fzText(0x0018, 0x1608, "IS", "16"),
			fzText(0x0018, 0x1610, "IS", "8\\8"), fzText(0x0018, 0x1612, "IS", "8"),
			fzText(0x0018, 0x1620, "IS", "1\\1\\16\\8\\1\\16"), fzWords(0x0018, 0x1623, "US", 0x6000),
			fzWords(0x0018, 0x1622, "US", 0xFFFF), fzWords(0x0018, 0x1624, "US", 0xFFFF, 0x8080, 0x8080),
		}, fzOverlay(0x6000, 16, 16, 1, 1, bytes.Repeat([]byte{0xAA}, 32))),
		"shutter three shapes and overlays": with([]fuzzElement{
			fzShape("RECTANGULAR\\CIRCULAR\\POLYGONAL"),
			fzText(0x0018, 0x1602, "IS", "1"), fzText(0x0018, 0x1604, "IS", "16"), fzText(0x0018, 0x1606, "IS", "1"), fzText(0x0018, 0x1608, "IS", "16"),
			fzText(0x0018, 0x1610, "IS", "8\\8"), fzText(0x0018, 0x1612, "IS", "8"),
			fzText(0x0018, 0x1620, "IS", "1\\1\\16\\8\\1\\16"),
		}, fzOverlay(0x6000, 16, 16, -4, -4, bytes.Repeat([]byte{0x55}, 32)), fzOverlay(0x6002, 3, 5, 15, 15, []byte{0xFF, 0x7F})),
	}
}

// FuzzPresentationFromObject parses arbitrary bytes as a data set and builds
// every presentation input from it: Modality LUT, VOI LUT (also per frame),
// Presentation LUT and shape, palette and segmented palette color LUTs,
// display shutter and overlays, then applies them to small frames. Nothing may
// panic; errors must wrap the package's (or object's) typed errors; LUTs hold
// at most the declared 65536 entries or what the data holds, with entries
// inside their bits per entry; shutters and overlays pass their validation.
func FuzzPresentationFromObject(f *testing.F) {
	seeds := presentationFuzzSeeds()
	names := make([]string, 0, len(seeds))
	for name := range seeds {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		for _, flags := range []uint8{
			presentationFuzzExplicitLE, presentationFuzzExplicitLE | presentationFuzzLegacy,
			presentationFuzzImplicitLE, presentationFuzzExplicitBE,
		} {
			f.Add(encodeFuzzDataSet(seeds[name], flags), flags)
		}
	}
	f.Fuzz(func(t *testing.T, data []byte, flags uint8) {
		if len(data) > presentationFuzzMaxInput {
			return
		}
		fuzzPresentationDataSet(t, data, flags)
	})
}

func presentationFuzzSyntax(flags uint8) transfer.Syntax {
	switch flags & 3 {
	case presentationFuzzImplicitLE:
		return transfer.ImplicitVRLittleEndian
	case presentationFuzzExplicitBE:
		return transfer.ExplicitVRBigEndian
	default:
		return transfer.ExplicitVRLittleEndian
	}
}

func fuzzPresentationDataSet(t *testing.T, data []byte, flags uint8) {
	t.Helper()
	obj, err := object.ReadDataSetWithOptions(bytes.NewReader(data), presentationFuzzSyntax(flags), object.ReadFileOptions{
		Dictionary:       std.Dictionary,
		MaxElementBytes:  presentationFuzzMaxElementBytes,
		MaxTotalBytes:    presentationFuzzMaxInput,
		MaxSequenceDepth: presentationFuzzMaxDepth,
		MaxElements:      presentationFuzzMaxElements,
		OddLengthPolicy:  parser.AcceptOddLength,
	})
	if err != nil || obj == nil {
		return
	}
	opts := LUTOptions{Legacy: flags&presentationFuzzLegacy != 0}
	limit := len(data)

	modality, err := ModalityLUTFromObject(obj, opts)
	checkFuzzError(t, "ModalityLUTFromObject", err)
	checkFuzzLUT(t, "modality", modality.LUT, limit)
	voi, err := VOIFromObject(obj, opts)
	checkFuzzError(t, "VOIFromObject", err)
	checkFuzzLUT(t, "VOI", voi.LUT, limit)
	for frame := -1; frame < 2; frame++ {
		m, err := ModalityLUTAt(obj, frame, opts)
		checkFuzzError(t, "ModalityLUTAt", err)
		checkFuzzLUT(t, "frame modality", m.LUT, limit)
		v, err := VOIAt(obj, frame, opts)
		checkFuzzError(t, "VOIAt", err)
		checkFuzzLUT(t, "frame VOI", v.LUT, limit)
		all, err := VOIsAt(obj, frame, opts)
		checkFuzzError(t, "VOIsAt", err)
		for _, v := range all {
			checkFuzzLUT(t, "frame VOIs", v.LUT, limit)
		}
	}
	signed := pixelsSigned(obj)
	gray := Frame{Rows: 4, Columns: 4, Pixels: fuzzFramePixels(), Format: PixelFormat{BitsAllocated: 16, BitsStored: 16, HighBit: 15, Signed: signed}, Modality: modality, VOI: voi}
	_, _ = RenderGray(gray)
	_, _ = RenderGray16(gray)

	palette, err := PaletteFromObject(obj, opts)
	checkFuzzError(t, "PaletteFromObject", err)
	if palette != nil {
		for _, lut := range []*LUT{palette.Red, palette.Green, palette.Blue} {
			checkFuzzLUT(t, "palette", lut, limit)
		}
		color := ColorFrame{Rows: 4, Columns: 4, Pixels: fuzzFramePixels(), Photometric: "PALETTE COLOR", SamplesPerPixel: 1, Format: gray.Format, Palette: palette}
		if _, err := RenderColor(color); err != nil {
			t.Fatalf("RenderColor with a read palette: %v", err)
		}
		if _, err := RenderColor16(color); err != nil {
			t.Fatalf("RenderColor16 with a read palette: %v", err)
		}
		_, _, _ = palette.At(-1 << 20)
	}

	pres, err := PresentationFromObject(obj, opts)
	checkFuzzError(t, "PresentationFromObject", err)
	checkFuzzLUT(t, "presentation", pres.LUT, limit)
	if err := ValidateShutter(pres.Shutter); err != nil {
		t.Fatalf("PresentationFromObject shutter fails ValidateShutter: %v", err)
	}
	for _, overlay := range pres.Overlays {
		if err := overlay.Validate(); err != nil {
			t.Fatalf("PresentationFromObject overlay fails Validate: %v", err)
		}
		if len(overlay.Data) > limit {
			t.Fatalf("overlay data grew to %d bytes from a %d-byte input", len(overlay.Data), limit)
		}
		for frame := 0; frame < 3; frame++ {
			if plane, ok := overlay.Frame(frame); ok && len(plane.Data) > len(overlay.Data) {
				t.Fatalf("overlay frame %d grew to %d bytes from %d", frame, len(plane.Data), len(overlay.Data))
			}
		}
	}
	shutter, err := ShutterFromObject(obj)
	checkFuzzError(t, "ShutterFromObject", err)
	if err := ValidateShutter(shutter); err != nil {
		t.Fatalf("ShutterFromObject fails ValidateShutter: %v", err)
	}
	overlays, err := OverlaysFromObject(obj)
	checkFuzzError(t, "OverlaysFromObject", err)
	for _, overlay := range overlays {
		if err := overlay.Validate(); err != nil {
			t.Fatalf("OverlaysFromObject overlay fails Validate: %v", err)
		}
	}

	// Apply the presentation to small frames, with the overlays of group
	// 0x6000 also taken raw (unvalidated), as a caller building its own
	// Overlay would: burn-in work must stay bounded by the image.
	pres.Overlays = append(pres.Overlays, fuzzRawOverlay(obj))
	img := image.NewGray(image.Rect(0, 0, 16, 16))
	img16 := image.NewGray16(image.Rect(0, 0, 16, 16))
	for i := range img.Pix {
		img.Pix[i] = uint8(i)
	}
	for i := range img16.Pix {
		img16.Pix[i] = uint8(i * 7)
	}
	pres.Apply(img)
	pres.ApplyGray16(img16)
	if pres.Shutter != nil {
		clone := pres.Shutter.Clone()
		_ = clone.ApplyTo(image.NewRGBA(image.Rect(2, 2, 10, 10)))
		_ = clone.ApplyTo(img)
	}
}

// fuzzRawOverlay is the 1-bit overlay of group 0x6000 as its attributes
// declare it, without Overlay.Validate.
func fuzzRawOverlay(obj *object.Object) Overlay {
	rows, _ := overlayUint16(obj, 0x6000, tagOverlayRows)
	columns, _ := overlayUint16(obj, 0x6000, tagOverlayColumns)
	originRow, originColumn := overlayOrigin(obj, 0x6000)
	data, _ := obj.GetRaw(core.NewTag(0x6000, tagOverlayData))
	return Overlay{Rows: int(rows), Columns: int(columns), OriginRow: originRow, OriginCol: originColumn, Data: data}
}

// fuzzFramePixels is a 4x4 16-bit frame spanning the unsigned and signed
// extremes, so LUT lookups clamp on both sides.
func fuzzFramePixels() []byte {
	values := []uint16{0, 1, 2, 3, 0x00FF, 0x0100, 0x0FFF, 0x1000, 0x7FFE, 0x7FFF, 0x8000, 0x8001, 0xC000, 0xFC00, 0xFFFE, 0xFFFF}
	out := make([]byte, 0, 2*len(values))
	for _, value := range values {
		out = binary.LittleEndian.AppendUint16(out, value)
	}
	return out
}

// presentationFuzzErrors are the errors the presentation readers may return.
var presentationFuzzErrors = []error{
	ErrInvalidLUTDescriptor, ErrInvalidLUTData, ErrLegacyLUTEncoding, ErrUnsupportedSegmentedPalette,
	ErrUnsupportedPresentationLUT, ErrUnsupportedGSPS, ErrInvalidShutter,
	ErrUnsupportedShutter, ErrInvalidOverlay, object.ErrFunctionalGroupConflict,
}

// checkFuzzError fails when err, or any error it joins, wraps none of
// presentationFuzzErrors.
func checkFuzzError(t *testing.T, name string, err error) {
	t.Helper()
	if err == nil {
		return
	}
	if joined, ok := err.(interface{ Unwrap() []error }); ok {
		for _, part := range joined.Unwrap() {
			checkFuzzError(t, name, part)
		}
		return
	}
	for _, typed := range presentationFuzzErrors {
		if errors.Is(err, typed) {
			return
		}
	}
	t.Fatalf("%s: untyped error %v", name, err)
}

// checkFuzzLUT fails for a LUT larger than both the 65536 entries a
// descriptor can declare and the data an input of limit bytes holds, or with
// entries outside its bits per entry.
func checkFuzzLUT(t *testing.T, name string, lut *LUT, limit int) {
	t.Helper()
	if lut == nil {
		return
	}
	if len(lut.Entries) == 0 {
		t.Fatalf("%s LUT has no entries", name)
	}
	if len(lut.Entries) > 1<<16 && len(lut.Entries) > limit/2 {
		t.Fatalf("%s LUT has %d entries from a %d-byte input", name, len(lut.Entries), limit)
	}
	if lut.BitsPerEntry < 8 || lut.BitsPerEntry > 16 {
		t.Fatalf("%s LUT bits per entry %d", name, lut.BitsPerEntry)
	}
	if largest := maxEntry(lut.Entries); largest > lut.outputMax() {
		t.Fatalf("%s LUT entry %d exceeds %d bits", name, largest, lut.BitsPerEntry)
	}
}

// TestPresentationFuzzSeedsParse keeps the seeds meaningful: every seed must
// parse in each syntax it is written in, so the fuzzer starts from real data
// sets rather than bytes the parser rejects.
func TestPresentationFuzzSeedsParse(t *testing.T) {
	for name, elements := range presentationFuzzSeeds() {
		for _, flags := range []uint8{presentationFuzzExplicitLE, presentationFuzzImplicitLE, presentationFuzzExplicitBE} {
			data := encodeFuzzDataSet(elements, flags)
			obj, err := object.ReadDataSetWithOptions(bytes.NewReader(data), presentationFuzzSyntax(flags), object.ReadFileOptions{Dictionary: std.Dictionary, OddLengthPolicy: parser.AcceptOddLength})
			if err != nil || obj == nil {
				t.Errorf("%s (flags %d): %v", name, flags, err)
			}
		}
	}
}
