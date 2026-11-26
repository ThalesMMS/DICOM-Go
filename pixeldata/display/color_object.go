package display

import (
	"encoding/binary"
	"fmt"
	"math"
	"strings"

	"github.com/ThalesMMS/dicom-go/core"
	"github.com/ThalesMMS/dicom-go/object"
)

var (
	tagSamplesPerPixel     = core.NewTag(0x0028, 0x0002)
	tagPhotometric         = core.NewTag(0x0028, 0x0004)
	tagPlanarConfiguration = core.NewTag(0x0028, 0x0006)

	tagRedPaletteDesc   = core.NewTag(0x0028, 0x1101)
	tagGreenPaletteDesc = core.NewTag(0x0028, 0x1102)
	tagBluePaletteDesc  = core.NewTag(0x0028, 0x1103)
	tagRedPaletteData   = core.NewTag(0x0028, 0x1201)
	tagGreenPaletteData = core.NewTag(0x0028, 0x1202)
	tagBluePaletteData  = core.NewTag(0x0028, 0x1203)
	tagSegRedPalette    = core.NewTag(0x0028, 0x1221)
	tagSegGreenPalette  = core.NewTag(0x0028, 0x1222)
	tagSegBluePalette   = core.NewTag(0x0028, 0x1223)

	tagICCProfile = core.NewTag(0x0028, 0x2000)
	tagColorSpace = core.NewTag(0x0028, 0x2002)
)

// ColorMetadata exposes color-layout and color-management attributes without
// performing color management. The ICC profile and color space are reported as
// metadata only, preserving the lightweight default dependency policy.
type ColorMetadata struct {
	Photometric                string
	SamplesPerPixel            int
	PlanarConfiguration        int
	PlanarConfigurationPresent bool
	ColorSpace                 string
	// ICCProfile is the raw ICC Profile (0028,2000) bytes, if present.
	ICCProfile []byte
}

// ColorMetadataFromObject reads color layout and ICC/color-space metadata.
func ColorMetadataFromObject(obj *object.Object) ColorMetadata {
	var m ColorMetadata
	if obj == nil {
		return m
	}
	if p, ok := obj.GetString(tagPhotometric); ok {
		m.Photometric = strings.TrimSpace(p)
	}
	if spp, ok := rawUint16(obj, tagSamplesPerPixel); ok {
		m.SamplesPerPixel = int(spp)
	}
	if pc, ok := rawUint16(obj, tagPlanarConfiguration); ok {
		m.PlanarConfiguration = int(pc)
		m.PlanarConfigurationPresent = true
	}
	if cs, ok := obj.GetString(tagColorSpace); ok {
		m.ColorSpace = strings.TrimSpace(cs)
	}
	if icc, ok := obj.GetRaw(tagICCProfile); ok && len(icc) > 0 {
		m.ICCProfile = icc
	}
	return m
}

// PaletteFromObject reads the red/green/blue palette color LUTs. It returns
// (nil, nil) when the dataset has no palette LUTs, and an explicit error for
// malformed descriptors or LUT data. opts (at most one is used) selects the
// LUT encodings read; the palette data encodings are those of LUT Data.
// Segmented palette data (PS3.3 C.7.9.2) may expand at most one segment per
// data word plus one per entry; indirect segments that would re-walk more
// (empty) segments than that are refused with ErrInvalidLUTData.
func PaletteFromObject(obj *object.Object, opts ...LUTOptions) (*PaletteColorLUT, error) {
	if obj == nil {
		return nil, nil
	}

	options := lutOptions(opts)
	signed := pixelsSigned(obj)
	red, err := paletteLUT(obj, tagRedPaletteDesc, tagRedPaletteData, tagSegRedPalette, signed, options)
	if err != nil {
		return nil, err
	}
	green, err := paletteLUT(obj, tagGreenPaletteDesc, tagGreenPaletteData, tagSegGreenPalette, signed, options)
	if err != nil {
		return nil, err
	}
	blue, err := paletteLUT(obj, tagBluePaletteDesc, tagBluePaletteData, tagSegBluePalette, signed, options)
	if err != nil {
		return nil, err
	}
	if red == nil || green == nil || blue == nil {
		return nil, nil
	}
	return &PaletteColorLUT{Red: red, Green: green, Blue: blue}, nil
}

// paletteLUT reads one palette color LUT (descriptor + data). It returns
// (nil, nil) when the descriptor and data are absent.
func paletteLUT(obj *object.Object, descTag, dataTag, segmentedDataTag core.Tag, signedInput bool, opts LUTOptions) (*LUT, error) {
	elem, ok := obj.Get(descTag)
	if !ok {
		if _, hasData := obj.GetRaw(dataTag); hasData {
			return nil, ErrInvalidLUTDescriptor
		}
		if _, hasSegmentedData := obj.GetRaw(segmentedDataTag); hasSegmentedData {
			return nil, ErrInvalidLUTDescriptor
		}
		return nil, nil
	}
	descRaw, ok := elem.RawBytes()
	if !ok || len(descRaw) < 6 {
		return nil, ErrInvalidLUTDescriptor
	}
	order := obj.ValueByteOrder()
	numberOfEntries := int(order.Uint16(descRaw[0:2]))
	bitsPerEntry := int(order.Uint16(descRaw[4:6]))
	firstMapped := int(order.Uint16(descRaw[2:4]))
	if elem.VR() == core.VRSS || signedInput {
		firstMapped = int(int16(order.Uint16(descRaw[2:4])))
	}

	if dataRaw, ok := obj.GetRaw(dataTag); ok && len(dataRaw) > 0 {
		return lutFromData(dataRaw, order, numberOfEntries, firstMapped, bitsPerEntry, opts)
	}
	segmentedRaw, ok := obj.GetRaw(segmentedDataTag)
	if !ok || len(segmentedRaw) == 0 {
		return nil, ErrInvalidLUTData
	}
	entries, err := segmentedPaletteEntries(segmentedRaw, numberOfEntries, order)
	if err != nil {
		return nil, err
	}
	count := numberOfEntries
	if count == 0 {
		count = 1 << 16
	}
	if len(entries) != count {
		return nil, fmt.Errorf("%w: segmented palette expands to %d entries, want %d", ErrInvalidLUTData, len(entries), count)
	}
	// Segmented data is always in words; its entries are checked against the
	// depth like word-encoded LUT Data.
	entries, bitsPerEntry, adjustments, err := wordLUTDepth(entries, bitsPerEntry, nil, opts, false)
	if err != nil {
		return nil, err
	}
	return &LUT{FirstMapped: firstMapped, BitsPerEntry: bitsPerEntry, Entries: entries, Adjustments: adjustments}, nil
}

func segmentedPaletteEntries(data []byte, numberOfEntries int, order binary.ByteOrder) ([]uint16, error) {
	if len(data)%2 != 0 {
		return nil, ErrInvalidLUTData
	}
	if order == nil {
		order = binary.LittleEndian
	}
	count := numberOfEntries
	if count == 0 {
		count = 1 << 16
	}
	words := make([]uint16, len(data)/2)
	for i := range words {
		words[i] = order.Uint16(data[i*2 : i*2+2])
	}

	// Every segment read directly spans at least two words, and every copied
	// segment that adds entries adds at least one of count, so a conformant
	// table expands at most len(words)/2+count segments. The budget leaves
	// len(words)/2 more for empty copied segments and refuses indirect
	// segments that would re-walk empty segments quadratically often.
	expander := segmentedPaletteExpander{words: words, count: count, budget: len(words) + count}
	entries := make([]uint16, 0, count)
	for i := 0; i < len(words); {
		next, err := expander.expandSegment(i, &entries, true)
		if err != nil {
			return nil, err
		}
		i = next
	}
	return entries, nil
}

// segmentedPaletteSegmentEnd validates the wire extent of one segment and
// returns the index immediately after it.
func segmentedPaletteSegmentEnd(words []uint16, index int) (int, error) {
	if index < 0 || index+1 >= len(words) {
		return 0, ErrInvalidLUTData
	}
	end := index + 2
	switch opcode := words[index]; opcode {
	case 0:
		end += int(words[index+1])
	case 1:
		end++
	case 2:
		end += 2
	default:
		return 0, fmt.Errorf("%w: segmented palette opcode %d", ErrInvalidLUTData, opcode)
	}
	if end > len(words) {
		return 0, ErrInvalidLUTData
	}
	return end, nil
}

type segmentedPaletteExpander struct {
	words []uint16
	count int
	// budget is the number of segments still allowed to be expanded.
	budget int
	// segmentStarts stores one bit per wire word and is allocated for indirect palettes.
	segmentStarts []byte
}

func (e *segmentedPaletteExpander) expandSegment(index int, entries *[]uint16, allowIndirect bool) (int, error) {
	next, err := segmentedPaletteSegmentEnd(e.words, index)
	if err != nil {
		return 0, err
	}
	if e.budget--; e.budget < 0 {
		return 0, fmt.Errorf("%w: segmented palette expands more segments than its %d words and %d entries allow", ErrInvalidLUTData, len(e.words), e.count)
	}
	opcode := e.words[index]
	length := int(e.words[index+1])
	dataIndex := index + 2

	switch opcode {
	case 0:
		if len(*entries)+length > e.count {
			return 0, ErrInvalidLUTData
		}
		*entries = append(*entries, e.words[dataIndex:next]...)
	case 1:
		if len(*entries)+length > e.count {
			return 0, ErrInvalidLUTData
		}
		if length == 0 {
			return next, nil
		}
		if len(*entries) == 0 {
			return 0, ErrInvalidLUTData
		}
		start, end := (*entries)[len(*entries)-1], e.words[dataIndex]
		for step := 1; step <= length; step++ {
			*entries = append(*entries, interpolateSegmentValue(start, end, step, length))
		}
	case 2:
		if !allowIndirect {
			return 0, fmt.Errorf("%w: indirect segment cannot copy another indirect segment", ErrUnsupportedSegmentedPalette)
		}
		offset := uint32(e.words[dataIndex]) | uint32(e.words[dataIndex+1])<<16
		copyIndex := int(offset / 2)
		if offset%2 != 0 || copyIndex >= len(e.words) {
			return 0, ErrInvalidLUTData
		}
		if e.segmentStarts == nil {
			// An indirect segment can refer forward. Index the top-level
			// starts once so data words cannot be mistaken for opcodes.
			e.segmentStarts = make([]byte, (len(e.words)+7)/8)
			for i := 0; i < len(e.words); {
				e.segmentStarts[i/8] |= 1 << (i % 8)
				end, err := segmentedPaletteSegmentEnd(e.words, i)
				if err != nil {
					return 0, err
				}
				i = end
			}
		}
		if e.segmentStarts[copyIndex/8]&(1<<(copyIndex%8)) == 0 {
			return 0, fmt.Errorf("%w: indirect offset %d is not a segment start", ErrInvalidLUTData, offset)
		}
		for n := 0; n < length; n++ {
			end, err := e.expandSegment(copyIndex, entries, false)
			if err != nil {
				return 0, err
			}
			copyIndex = end
		}
	}
	return next, nil
}

func interpolateSegmentValue(start, end uint16, step, length int) uint16 {
	if length <= 0 {
		return start
	}
	value := float64(start) + (float64(end)-float64(start))*float64(step)/float64(length)
	return uint16(math.Round(value))
}

// rawUint16 reads a binary US attribute using the object's value byte order.
func rawUint16(obj *object.Object, tag core.Tag) (uint16, bool) {
	raw, ok := obj.GetRaw(tag)
	if !ok || len(raw) < 2 {
		return 0, false
	}
	return obj.ValueByteOrder().Uint16(raw[:2]), true
}
