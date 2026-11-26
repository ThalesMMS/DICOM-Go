package display

import (
	"math"
	"strings"

	"github.com/ThalesMMS/dicom-go/core"
	"github.com/ThalesMMS/dicom-go/internal/derivedio"
	"github.com/ThalesMMS/dicom-go/object"
)

var (
	tagRescaleIntercept    = core.NewTag(0x0028, 0x1052)
	tagRescaleSlope        = core.NewTag(0x0028, 0x1053)
	tagModalityLUTSequence = core.NewTag(0x0028, 0x3000)
	tagLUTDescriptor       = core.NewTag(0x0028, 0x3002)
	tagLUTData             = core.NewTag(0x0028, 0x3006)
	tagWindowCenter        = core.NewTag(0x0028, 0x1050)
	tagWindowWidth         = core.NewTag(0x0028, 0x1051)
	tagVOILUTFunction      = core.NewTag(0x0028, 0x1056)
	tagVOILUTSequence      = core.NewTag(0x0028, 0x3010)
	tagBitsStored          = core.NewTag(0x0028, 0x0101)
	tagPixelRepresentation = core.NewTag(0x0028, 0x0103)
)

// ModalityLUTFromObject builds the modality transform from a dataset. A
// Modality LUT Sequence (0028,3000) takes precedence over Rescale
// Slope/Intercept; if a Modality LUT Sequence is present but malformed, the
// linear rescale is used and the second return value reports the LUT error so
// callers can surface an explicit limitation instead of rendering wrong pixels.
// opts (at most one is used) selects the LUT encodings read. The LUT's first
// value mapped is signed when the stored pixels are (Pixel Representation 1),
// whatever the descriptor's VR (PS3.3 C.11.1.1.1).
func ModalityLUTFromObject(obj *object.Object, opts ...LUTOptions) (ModalityLUT, error) {
	return modalityLUTFrom(obj, pixelsSigned(obj), lutOptions(opts))
}

// modalityLUTFrom reads the modality transform of obj, a data set or a Pixel
// Value Transformation item; signedInput tells whether the stored pixels are
// signed.
func modalityLUTFrom(obj *object.Object, signedInput bool, opts LUTOptions) (ModalityLUT, error) {
	out := ModalityLUT{Slope: 1}
	if obj == nil {
		return out, nil
	}
	if slope, err := obj.GetFloat(tagRescaleSlope); err == nil && slope != 0 {
		out.Slope = slope
	}
	if intercept, err := obj.GetFloat(tagRescaleIntercept); err == nil {
		out.Intercept = intercept
	}

	items, ok := obj.GetSequence(tagModalityLUTSequence)
	if !ok || len(items) == 0 {
		return out, nil
	}
	lut, err := lutFromItem(items[0], signedInput, opts)
	if err != nil {
		return out, err
	}
	out.LUT = lut
	return out, nil
}

// VOIFromObject builds the VOI transform from a dataset. A VOI LUT Sequence
// (0028,3010) takes precedence over Window Center/Width. If a VOI LUT Sequence
// is present but malformed, the window (if any) is used and the LUT error is
// returned so callers can surface an explicit limitation. opts (at most one is
// used) selects the LUT encodings read. The LUT's first value mapped is signed
// when the VOI input may be negative (voiInputSigned).
func VOIFromObject(obj *object.Object, opts ...LUTOptions) (VOILUT, error) {
	return voiFrom(obj, voiInputSigned(obj, obj), lutOptions(opts))
}

// voiFrom reads the VOI transform of obj, a data set or a Frame VOI LUT item;
// signedInput tells whether its input may be negative.
func voiFrom(obj *object.Object, signedInput bool, opts LUTOptions) (VOILUT, error) {
	out := VOILUT{Function: voiFunctionFromObject(obj)}
	if obj == nil {
		return out, nil
	}
	if centers, err := obj.GetFloats(tagWindowCenter); err == nil && len(centers) > 0 {
		if widths, err := obj.GetFloats(tagWindowWidth); err == nil && len(widths) > 0 && validWindowWidth(out.Function, widths[0]) {
			out.Center = centers[0]
			out.Width = widths[0]
		}
	}

	items, ok := obj.GetSequence(tagVOILUTSequence)
	if !ok || len(items) == 0 {
		return out, nil
	}
	lut, err := lutFromItem(items[0], signedInput, opts)
	if err != nil {
		return out, err
	}
	out.LUT = lut
	return out, nil
}

func voiFunctionFromObject(obj *object.Object) VOIFunction {
	if obj == nil {
		return VOILinear
	}
	value, ok := obj.GetString(tagVOILUTFunction)
	if !ok {
		return VOILinear
	}
	switch strings.ToUpper(strings.TrimSpace(value)) {
	case "SIGMOID":
		return VOISigmoid
	case "LINEAR_EXACT":
		return VOILinearExact
	default:
		return VOILinear
	}
}

// pixelsSigned reports Pixel Representation 1 (signed stored pixels).
func pixelsSigned(obj *object.Object) bool {
	return obj != nil && derivedio.Int(obj, tagPixelRepresentation) == 1
}

// voiInputSigned reports whether the input of a VOI LUT may be negative,
// which makes its first value mapped signed (PS3.3 C.11.2.1.1): never after a
// Modality LUT, whose output is unsigned; when the rescale of the stored
// range of root (Bits Stored, Pixel Representation) reaches below zero; and
// otherwise when the stored pixels are signed. modality holds the rescale or
// Modality LUT (root itself, or the frame's Pixel Value Transformation item).
func voiInputSigned(root, modality *object.Object) bool {
	if modality == nil {
		return pixelsSigned(root)
	}
	if items, ok := modality.GetSequence(tagModalityLUTSequence); ok && len(items) > 0 {
		return false
	}
	slope, intercept := 1.0, 0.0
	if value, err := modality.GetFloat(tagRescaleSlope); err == nil && value != 0 {
		slope = value
	}
	if value, err := modality.GetFloat(tagRescaleIntercept); err == nil {
		intercept = value
	}
	bitsStored := 16
	if root != nil {
		if value := derivedio.Int(root, tagBitsStored); value > 0 && value <= 32 {
			bitsStored = value
		}
	}
	low, high := 0.0, math.Exp2(float64(bitsStored))-1
	if pixelsSigned(root) {
		low, high = -math.Exp2(float64(bitsStored-1)), math.Exp2(float64(bitsStored-1))-1
	}
	return math.Min(slope*low, slope*high)+intercept < 0
}

// LUTFromItem reads the LUT Descriptor and LUT Data of a LUT sequence item,
// such as a Presentation LUT Sequence item of a presentation state;
// signedInput and opts are as in lutFromItem.
func LUTFromItem(item *object.Object, signedInput bool, opts ...LUTOptions) (*LUT, error) {
	return lutFromItem(item, signedInput, lutOptions(opts))
}

// lutFromItem reads a LUT Descriptor (0028,3002) and LUT Data (0028,3006) from a
// LUT sequence item, decoding the data as LUTOptions describes.
//
// LUT Descriptor and LUT Data are binary US/SS/OW values. The descriptor's
// middle value (the first input value mapped) is signed when the descriptor
// VR is SS or when the values the LUT maps may be negative (signedInput), as
// the standard ties that value's VR to the LUT's input rather than to how an
// encoder labelled it.
func lutFromItem(item *object.Object, signedInput bool, opts LUTOptions) (*LUT, error) {
	if item == nil {
		return nil, ErrInvalidLUTDescriptor
	}
	elem, ok := item.Get(tagLUTDescriptor)
	if !ok {
		return nil, ErrInvalidLUTDescriptor
	}
	descRaw, ok := elem.RawBytes()
	if !ok || len(descRaw) < 6 {
		return nil, ErrInvalidLUTDescriptor
	}
	order := item.ValueByteOrder()
	numberOfEntries := int(order.Uint16(descRaw[0:2]))
	bitsPerEntry := int(order.Uint16(descRaw[4:6]))
	firstMapped := int(order.Uint16(descRaw[2:4]))
	if elem.VR() == core.VRSS || signedInput {
		firstMapped = int(int16(order.Uint16(descRaw[2:4])))
	}

	raw, ok := item.GetRaw(tagLUTData)
	if !ok || len(raw) == 0 {
		return nil, ErrInvalidLUTData
	}
	return lutFromData(raw, order, numberOfEntries, firstMapped, bitsPerEntry, opts)
}
