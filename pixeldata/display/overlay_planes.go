package display

import (
	"encoding/binary"
	"errors"
	"fmt"
	"math"
	mathbits "math/bits"
	"strconv"
	"strings"

	"github.com/ThalesMMS/dicom-go/core"
	"github.com/ThalesMMS/dicom-go/object"
)

// ErrInvalidOverlay reports an overlay whose attributes or data cannot be
// used.
var ErrInvalidOverlay = errors.New("dicom: invalid overlay")

const (
	tagOverlayFrames      = uint16(0x0015)
	tagOverlayDescription = uint16(0x0022)
	tagOverlayType        = uint16(0x0040)
	tagOverlaySubtype     = uint16(0x0045)
	tagOverlayFrameOrigin = uint16(0x0051)
	tagOverlayBitPosition = uint16(0x0102)
	tagOverlayLabel       = uint16(0x1500)
	tagROIArea            = uint16(0x1301)
	tagROIMean            = uint16(0x1302)
	tagROIStandardDev     = uint16(0x1303)
)

var (
	tagImageRows           = core.NewTag(0x0028, 0x0010)
	tagImageColumns        = core.NewTag(0x0028, 0x0011)
	tagImageBitsAllocated  = core.NewTag(0x0028, 0x0100)
	tagImageBitsStored     = core.NewTag(0x0028, 0x0101)
	tagImageHighBit        = core.NewTag(0x0028, 0x0102)
	tagImageNumberOfFrames = core.NewTag(0x0028, 0x0008)
)

// Value ranges of the Overlay Plane attributes: Overlay Rows, Columns and
// Image Frame Origin are US, Overlay Origin is SS.
const (
	maxOverlayUS = 1<<16 - 1
	minOverlaySS = -(1 << 15)
	maxOverlaySS = 1<<15 - 1
)

// FrameCount is Number of Frames in Overlay (at least 1).
func (o Overlay) FrameCount() int {
	if o.Frames < 1 {
		return 1
	}
	return o.Frames
}

// Validate reports, wrapping ErrInvalidOverlay, an overlay that cannot be a
// DICOM overlay plane: Rows or Columns outside 1-65535, Overlay Origin
// outside the SS range, Image Frame Origin outside the US range, an embedded
// Bit Position outside a 16-bit sample, or Data shorter than
// Rows*Columns*FrameCount bits. OverlaysFromObject and EmbeddedOverlays
// return only overlays that pass it.
func (o Overlay) Validate() error {
	reason := ""
	switch {
	case o.Rows < 1 || o.Rows > maxOverlayUS || o.Columns < 1 || o.Columns > maxOverlayUS:
		reason = fmt.Sprintf("Overlay Rows/Columns %dx%d outside 1-%d", o.Rows, o.Columns, maxOverlayUS)
	case o.OriginRow < minOverlaySS || o.OriginRow > maxOverlaySS || o.OriginCol < minOverlaySS || o.OriginCol > maxOverlaySS:
		reason = fmt.Sprintf("Overlay Origin (%d,%d) outside the SS range", o.OriginRow, o.OriginCol)
	case o.FrameOrigin < 0 || o.FrameOrigin > maxOverlayUS:
		reason = fmt.Sprintf("Image Frame Origin %d outside the US range", o.FrameOrigin)
	case o.Embedded && (o.BitPosition < 0 || o.BitPosition > 15):
		reason = fmt.Sprintf("Overlay Bit Position %d outside a 16-bit sample", o.BitPosition)
	case !planesFit(len(o.Data), o.Rows, o.Columns, o.FrameCount()):
		reason = fmt.Sprintf("Overlay Data has %d bytes, too few for %d frame(s) of %dx%d bits", len(o.Data), o.FrameCount(), o.Rows, o.Columns)
	default:
		return nil
	}
	return fmt.Errorf("%w: group %#04x: %s", ErrInvalidOverlay, o.Group, reason)
}

// planeBits returns rows*columns and false when either is not positive or
// the product overflows.
func planeBits(rows, columns int) (uint64, bool) {
	if rows <= 0 || columns <= 0 {
		return 0, false
	}
	hi, plane := mathbits.Mul64(uint64(rows), uint64(columns))
	return plane, hi == 0
}

// planesFit reports whether dataBytes bytes hold frames planes of
// rows*columns bits, without overflowing whatever the inputs.
func planesFit(dataBytes, rows, columns, frames int) bool {
	plane, ok := planeBits(rows, columns)
	if !ok || frames <= 0 || dataBytes < 0 {
		return false
	}
	hi, total := mathbits.Mul64(plane, uint64(frames))
	return hi == 0 && total <= uint64(dataBytes)*8
}

// Frame returns overlay frame index (zero-based) as a single-frame overlay
// whose Data starts at that frame's first bit; frames are packed back to
// back without padding (PS3.5 8.1.2). It reports false for a frame that Data
// does not hold in full, so the copy never exceeds Data.
func (o Overlay) Frame(index int) (Overlay, bool) {
	if index < 0 || index >= o.FrameCount() || !planesFit(len(o.Data), o.Rows, o.Columns, index+1) {
		return Overlay{}, false
	}
	plane, _ := planeBits(o.Rows, o.Columns)
	start := plane * uint64(index)
	out := o
	out.Frames, out.FrameOrigin = 1, 1
	// planesFit bounds the plane and its end by the data length.
	out.Data = make([]byte, int((plane+7)/8))
	first, shift := int(start/8), uint(start%8)
	for j := range out.Data {
		b := o.Data[first+j] >> shift
		if shift != 0 && first+j+1 < len(o.Data) {
			b |= o.Data[first+j+1] << (8 - shift)
		}
		out.Data[j] = b
	}
	if rest := plane % 8; rest != 0 {
		out.Data[len(out.Data)-1] &= byte(1)<<rest - 1
	}
	return out, true
}

// ForImageFrame returns the overlay frame that applies to the zero-based
// image frame, using Image Frame Origin (60xx,0051), and false when the
// overlay does not cover that frame.
func (o Overlay) ForImageFrame(imageFrame int) (Overlay, bool) {
	origin := o.FrameOrigin
	if origin < 1 {
		origin = 1
	}
	return o.Frame(imageFrame - (origin - 1))
}

// Unpacked returns the first overlay frame as Rows*Columns bytes, 255 where
// the bit is set and 0 elsewhere (the GDCM Overlay::GetUnpackBuffer layout).
// It returns nil unless Data holds the whole frame, so the buffer is never
// more than eight times the size of Data.
func (o Overlay) Unpacked() []byte {
	plane, _ := planeBits(o.Rows, o.Columns)
	if !planesFit(len(o.Data), o.Rows, o.Columns, 1) || plane > math.MaxInt {
		return nil
	}
	out := make([]byte, int(plane))
	for i := range out {
		if o.Data[i/8]&(1<<uint(i%8)) != 0 {
			out[i] = 255
		}
	}
	return out
}

// describeOverlay fills the descriptive and ROI attributes of a group.
func describeOverlay(obj *object.Object, group uint16, overlay *Overlay) {
	overlay.Group = group
	overlay.Type = strings.ToUpper(overlayString(obj, group, tagOverlayType))
	overlay.Subtype = overlayString(obj, group, tagOverlaySubtype)
	overlay.Label = overlayString(obj, group, tagOverlayLabel)
	overlay.Description = overlayString(obj, group, tagOverlayDescription)
	overlay.Frames = 1
	if frames, err := strconv.Atoi(overlayString(obj, group, tagOverlayFrames)); err == nil && frames > 0 {
		overlay.Frames = frames
	}
	overlay.FrameOrigin = 1
	if origin, ok := overlayUint16(obj, group, tagOverlayFrameOrigin); ok && origin > 0 {
		overlay.FrameOrigin = int(origin)
	}
	area, errArea := strconv.Atoi(overlayString(obj, group, tagROIArea))
	mean, errMean := strconv.ParseFloat(overlayString(obj, group, tagROIMean), 64)
	sd, errSD := strconv.ParseFloat(overlayString(obj, group, tagROIStandardDev), 64)
	if errArea == nil || errMean == nil || errSD == nil {
		overlay.HasROIStatistics = true
		overlay.ROIArea, overlay.ROIMean, overlay.ROIStandardDeviation = area, mean, sd
	}
}

func overlayString(obj *object.Object, group, element uint16) string {
	value, _ := obj.GetString(core.NewTag(group, element))
	return strings.TrimSpace(value)
}

// overlayDataLittleEndian returns Overlay Data as bytes packed least
// significant bit first. OW data of a Big Endian data set holds 16-bit words
// in Big Endian order and is swapped.
func overlayDataLittleEndian(obj *object.Object, group uint16, data []byte) []byte {
	element, ok := obj.Get(core.NewTag(group, tagOverlayData))
	if !ok || element.VR() != core.VROW || obj.ValueByteOrder() != binary.BigEndian {
		return data
	}
	out := make([]byte, len(data))
	copy(out, data)
	for i := 0; i+1 < len(out); i += 2 {
		out[i], out[i+1] = out[i+1], out[i]
	}
	return out
}

// EmbeddedOverlays extracts the overlays that live in unused bits of the
// image samples (Overlay Bits Allocated greater than 1, no Overlay Data; a
// retired encoding still found in older files). native holds the decoded
// native pixel samples of all frames, as pixeldata decompression or frame
// extraction returns them, in order. Each overlay covers Number of Frames in
// Overlay frames from Image Frame Origin, or every image frame.
func EmbeddedOverlays(obj *object.Object, native []byte, order binary.ByteOrder) ([]Overlay, error) {
	if obj == nil {
		return nil, nil
	}
	if order == nil {
		order = binary.LittleEndian
	}
	rows, okRows := rawUint16(obj, tagImageRows)
	columns, okColumns := rawUint16(obj, tagImageColumns)
	bitsAllocated, okBits := rawUint16(obj, tagImageBitsAllocated)
	imageFrames := 1
	if value, ok := obj.GetString(tagImageNumberOfFrames); ok {
		if n, err := strconv.Atoi(strings.TrimSpace(value)); err == nil && n > 0 {
			imageFrames = n
		}
	}
	var out []Overlay
	for group := overlayGroupFirst; group <= overlayGroupLast; group += 2 {
		overlayBits, ok := overlayUint16(obj, group, tagOverlayBitsAlloc)
		if !ok || overlayBits <= 1 || obj.Has(core.NewTag(group, tagOverlayData)) {
			continue
		}
		if !okRows || !okColumns || !okBits || rows == 0 || columns == 0 || (bitsAllocated != 8 && bitsAllocated != 16) {
			return out, fmt.Errorf("%w: embedded overlay %#04x needs non-empty 8- or 16-bit image samples", ErrInvalidOverlay, group)
		}
		position, ok := overlayUint16(obj, group, tagOverlayBitPosition)
		if !ok || position >= bitsAllocated {
			return out, fmt.Errorf("%w: embedded overlay %#04x bit position", ErrInvalidOverlay, group)
		}
		if low, high, ok := storedBits(obj); ok && position >= low && position <= high {
			return out, fmt.Errorf("%w: embedded overlay %#04x bit position %d lies in the stored bits %d-%d", ErrInvalidOverlay, group, position, low, high)
		}
		overlay := Overlay{Rows: int(rows), Columns: int(columns), Embedded: true, BitPosition: int(position)}
		overlay.OriginRow, overlay.OriginCol = overlayOrigin(obj, group)
		describeOverlay(obj, group, &overlay)
		if !obj.Has(core.NewTag(group, tagOverlayFrames)) {
			overlay.Frames = imageFrames
		}
		// Number of Frames in Overlay and Image Frame Origin come from the
		// data set; bound them by the samples present before any product
		// or allocation, so no value can overflow or over-allocate.
		width := int(bitsAllocated) / 8
		samples := uint64(len(native) / width)
		plane := uint64(rows) * uint64(columns)
		first := uint64(overlay.FrameOrigin-1) * plane
		hi, total := mathbits.Mul64(uint64(overlay.FrameCount()), plane)
		if hi != 0 || first > samples || total > samples-first {
			return out, fmt.Errorf("%w: embedded overlay %#04x covers %d frame(s) from image frame %d, pixel data has %d samples of %d per frame",
				ErrInvalidOverlay, group, overlay.FrameCount(), overlay.FrameOrigin, samples, plane)
		}
		overlay.Data = make([]byte, int((total+7)/8))
		base := int(first)
		for i := 0; i < int(total); i++ {
			offset := (base + i) * width
			var sample uint16
			if width == 2 {
				sample = order.Uint16(native[offset:])
			} else {
				sample = uint16(native[offset])
			}
			if sample&(1<<position) != 0 {
				overlay.Data[i/8] |= 1 << uint(i%8)
			}
		}
		if err := overlay.Validate(); err != nil {
			return out, err
		}
		out = append(out, overlay)
	}
	return out, nil
}

// storedBits returns the sample bit range [low, high] that Bits Stored and
// High Bit give to pixel values, when both are present and consistent.
func storedBits(obj *object.Object) (low, high uint16, ok bool) {
	stored, okStored := rawUint16(obj, tagImageBitsStored)
	high, okHigh := rawUint16(obj, tagImageHighBit)
	if !okStored || !okHigh || stored == 0 || high+1 < stored {
		return 0, 0, false
	}
	return high + 1 - stored, high, true
}

// EmbeddedOverlaysForFrame is EmbeddedOverlays for one decoded image frame:
// frameSamples holds the native samples of the zero-based imageFrame only,
// and every returned overlay is the single plane that applies to it
// (overlays whose frames do not cover imageFrame are omitted).
func EmbeddedOverlaysForFrame(obj *object.Object, frameSamples []byte, order binary.ByteOrder, imageFrame int) ([]Overlay, error) {
	if obj == nil || imageFrame < 0 {
		return nil, nil
	}
	rows, okRows := rawUint16(obj, tagImageRows)
	columns, okColumns := rawUint16(obj, tagImageColumns)
	bitsAllocated, okBits := rawUint16(obj, tagImageBitsAllocated)
	if !okRows || !okColumns || !okBits || (bitsAllocated != 8 && bitsAllocated != 16) {
		return nil, nil
	}
	need := uint64(rows) * uint64(columns) * uint64(bitsAllocated/8)
	if uint64(len(frameSamples)) < need {
		return nil, fmt.Errorf("%w: frame has %d bytes, needs %d", ErrInvalidOverlay, len(frameSamples), need)
	}
	// Extract from a single-frame view of the object: the frame's samples
	// stand for frame 1, and frame coverage is checked here.
	var out []Overlay
	single := make([]byte, int(need))
	copy(single, frameSamples)
	all, err := EmbeddedOverlays(singleFrameView(obj), single, order)
	if err != nil {
		return nil, err
	}
	for _, overlay := range all {
		frames, origin := embeddedCoverage(obj, overlay.Group)
		index := imageFrame - (origin - 1)
		if frames > 0 && (index < 0 || index >= frames) {
			continue
		}
		overlay.Frames, overlay.FrameOrigin = 1, 1
		out = append(out, overlay)
	}
	return out, nil
}

// singleFrameView presents obj as a one-frame image whose overlays start at
// frame 1, so EmbeddedOverlays reads one frame of samples.
func singleFrameView(obj *object.Object) *object.Object {
	var elements []core.Element
	for _, element := range obj.Elements() {
		tag := element.Tag()
		if tag == tagImageNumberOfFrames {
			continue
		}
		if tag.Group >= overlayGroupFirst && tag.Group <= overlayGroupLast && (tag.Element == tagOverlayFrames || tag.Element == tagOverlayFrameOrigin) {
			continue
		}
		if tag == core.TagPixelData {
			continue
		}
		elements = append(elements, element)
	}
	view := object.FromElements(elements, nil)
	view.SetValueByteOrder(obj.ValueByteOrder())
	return view
}

// embeddedCoverage returns Number of Frames in Overlay (0 when absent: all
// frames) and Image Frame Origin (1 when absent) of an overlay group.
func embeddedCoverage(obj *object.Object, group uint16) (frames, origin int) {
	origin = 1
	if value, err := strconv.Atoi(overlayString(obj, group, tagOverlayFrames)); err == nil && value > 0 {
		frames = value
	}
	if value, ok := overlayUint16(obj, group, tagOverlayFrameOrigin); ok && value > 0 {
		origin = int(value)
	}
	return frames, origin
}
