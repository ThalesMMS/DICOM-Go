package display

import (
	"errors"
	"strings"

	"github.com/ThalesMMS/dicom-go/core"
	"github.com/ThalesMMS/dicom-go/object"
)

var (
	tagPixelValueTransformationSequence = core.NewTag(0x0028, 0x9145)
	tagFrameVOILUTSequence              = core.NewTag(0x0028, 0x9132)
	tagWindowCenterWidthExplanation     = core.NewTag(0x0028, 0x1055)
	tagLUTExplanation                   = core.NewTag(0x0028, 0x3003)
)

// ModalityLUTAt is ModalityLUTFromObject for the zero-based frameIndex of a
// multi-frame object: the Pixel Value Transformation macro (0028,9145) of the
// frame's Per-Frame Functional Groups item, then of the Shared Functional
// Groups, and the top-level attributes only when neither has it, which keeps
// single-frame and classic multi-frame objects unchanged. A macro present in
// both groups is used from the per-frame group and reported with
// object.ErrFunctionalGroupConflict. opts is as in ModalityLUTFromObject.
func ModalityLUTAt(obj *object.Object, frameIndex int, opts ...LUTOptions) (ModalityLUT, error) {
	item, _, conflict := obj.FunctionalGroupItem(frameIndex, tagPixelValueTransformationSequence)
	if item == nil {
		return ModalityLUTFromObject(obj, opts...)
	}
	out, err := modalityLUTFrom(item, pixelsSigned(obj), lutOptions(opts))
	return out, errors.Join(conflict, err)
}

// modalitySourceAt is the frame's Pixel Value Transformation item, or obj.
func modalitySourceAt(obj *object.Object, frameIndex int) *object.Object {
	if item, _, _ := obj.FunctionalGroupItem(frameIndex, tagPixelValueTransformationSequence); item != nil {
		return item
	}
	return obj
}

// VOIAt is VOIFromObject for the zero-based frameIndex: the Frame VOI LUT
// macro (0028,9132) of the frame's Per-Frame Functional Groups item, then of
// the Shared Functional Groups, and the top-level attributes only when
// neither has it. Conflicts are reported as in ModalityLUTAt, and opts is as
// in VOIFromObject.
func VOIAt(obj *object.Object, frameIndex int, opts ...LUTOptions) (VOILUT, error) {
	item, _, conflict := obj.FunctionalGroupItem(frameIndex, tagFrameVOILUTSequence)
	source := item
	if source == nil {
		source = obj
	}
	out, err := voiFrom(source, voiInputSigned(obj, modalitySourceAt(obj, frameIndex)), lutOptions(opts))
	return out, errors.Join(conflict, err)
}

// VOIsAt returns every VOI transform stored for the zero-based frameIndex,
// from the same source VOIAt uses: one entry per Window Center/Width pair with
// a width valid for the VOI function (validWindowWidth), then one per VOI LUT
// Sequence item, each with its explanation. VOIAt combines the first window
// with the first LUT, which takes precedence when present. A malformed LUT item is skipped and its error
// returned; opts is as in VOIFromObject.
func VOIsAt(obj *object.Object, frameIndex int, opts ...LUTOptions) ([]VOILUT, error) {
	source, _, conflict := obj.FunctionalGroupItem(frameIndex, tagFrameVOILUTSequence)
	if source == nil {
		source = obj
	}
	if source == nil {
		return nil, conflict
	}
	function := voiFunctionFromObject(source)
	var out []VOILUT
	centers, _ := source.GetFloats(tagWindowCenter)
	widths, _ := source.GetFloats(tagWindowWidth)
	explanations, _ := source.GetStrings(tagWindowCenterWidthExplanation)
	for i := 0; i < len(centers) && i < len(widths); i++ {
		if !validWindowWidth(function, widths[i]) {
			continue
		}
		voi := VOILUT{Center: centers[i], Width: widths[i], Function: function}
		if i < len(explanations) {
			voi.Explanation = strings.TrimSpace(explanations[i])
		}
		out = append(out, voi)
	}
	errs := []error{conflict}
	signedInput := voiInputSigned(obj, modalitySourceAt(obj, frameIndex))
	items, _ := source.GetSequence(tagVOILUTSequence)
	for _, item := range items {
		lut, err := lutFromItem(item, signedInput, lutOptions(opts))
		if err != nil {
			errs = append(errs, err)
			continue
		}
		voi := VOILUT{Function: function, LUT: lut}
		if explanation, ok := item.GetString(tagLUTExplanation); ok {
			voi.Explanation = strings.TrimSpace(explanation)
		}
		out = append(out, voi)
	}
	return out, errors.Join(errs...)
}
