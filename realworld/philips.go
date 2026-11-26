package realworld

import (
	"strings"

	"github.com/ThalesMMS/dicom-go/core"
	"github.com/ThalesMMS/dicom-go/object"
)

// Philips MR private scaling elements (group 2005), by creator and offset.
// Definitions: GDCM Source/DataDictionary/gdcmPrivateDefaultDicts.cxx
// (commit 2cd05d13, BSD-3-Clause), "Rescale Intercept Philips" and "Rescale
// Slope Philips" (DS) and "ScaleSlope" (FL, marked uncertain there); their
// use follows Chenevert et al., "Errors in Quantitative Image Analysis due to
// Platform-Dependent Image Scaling", Transl Oncol 2014 (PMC3998685).
const (
	PhilipsGroup                  = 0x2005
	PhilipsRescaleCreator         = "Philips MR Imaging DD 005"
	PhilipsRescaleInterceptOffset = 0x09
	PhilipsRescaleSlopeOffset     = 0x0A
	PhilipsScaleSlopeCreator      = "Philips MR Imaging DD 001"
	PhilipsScaleSlopeOffset       = 0x0E
)

// PhilipsScale is the Philips MR private scaling of one frame. It is vendor
// private data, read only on request and never applied to display.
type PhilipsScale struct {
	// RescaleIntercept and RescaleSlope are the private rescale (DD 005
	// xx09/xx0A): stored * RescaleSlope + RescaleIntercept is the value the
	// scanner displays.
	RescaleIntercept float64
	RescaleSlope     float64
	// ScaleSlope is the private Scale Slope (DD 001 xx0E); HasScaleSlope
	// reports whether it was present and nonzero.
	ScaleSlope    float64
	HasScaleSlope bool
}

// DisplayValue is stored * RescaleSlope + RescaleIntercept.
func (s PhilipsScale) DisplayValue(stored float64) float64 {
	return stored*s.RescaleSlope + s.RescaleIntercept
}

// FloatingPointValue is the Philips floating point value, DisplayValue /
// (RescaleSlope * ScaleSlope), which removes the scanner's per-image scaling
// for quantitative comparison. It is false without a Scale Slope.
func (s PhilipsScale) FloatingPointValue(stored float64) (float64, bool) {
	if !s.HasScaleSlope || s.RescaleSlope == 0 {
		return 0, false
	}
	return s.DisplayValue(stored) / (s.RescaleSlope * s.ScaleSlope), true
}

// PhilipsScaleAt reads the Philips private scaling for the zero-based
// frameIndex from the frame's Per-Frame Functional Groups item, where
// Philips enhanced MR stores it, or from the top level. The private creator
// is matched exactly, so other vendors' group 2005 data is never used. It
// is false when the private rescale is absent.
func PhilipsScaleAt(obj *object.Object, frameIndex int) (PhilipsScale, bool) {
	sources := []*object.Object{obj}
	if frames, ok := obj.GetSequence(tagPerFrameFunctionalGroups); ok && frameIndex >= 0 && frameIndex < len(frames) {
		sources = append([]*object.Object{frames[frameIndex]}, sources...)
	}
	for _, source := range sources {
		if scale, ok := philipsScaleIn(source); ok {
			return scale, true
		}
	}
	return PhilipsScale{}, false
}

func philipsScaleIn(o *object.Object) (PhilipsScale, bool) {
	if o == nil {
		return PhilipsScale{}, false
	}
	intercept, okIntercept := privateNumber(o, PhilipsRescaleCreator, PhilipsRescaleInterceptOffset, core.VRDS)
	slope, okSlope := privateNumber(o, PhilipsRescaleCreator, PhilipsRescaleSlopeOffset, core.VRDS)
	if !okIntercept || !okSlope {
		return PhilipsScale{}, false
	}
	if slope == 0 {
		slope = 1
	}
	scale := PhilipsScale{RescaleIntercept: intercept, RescaleSlope: slope}
	if ss, ok := privateNumber(o, PhilipsScaleSlopeCreator, PhilipsScaleSlopeOffset, core.VRFL); ok && ss != 0 {
		scale.ScaleSlope, scale.HasScaleSlope = ss, true
	}
	return scale, true
}

func privateNumber(o *object.Object, creator string, offset uint8, vr core.VR) (float64, bool) {
	tag, ok := privateTag(o, creator, offset)
	if !ok {
		return 0, false
	}
	values, err := numbersAs(o, tag, vr)
	if err != nil || len(values) == 0 {
		return 0, false
	}
	return values[0], true
}

// privateTag finds the element of creator at offset in group 2005. Philips
// writes both spellings of its creators (DCMTK private.dic lists "Philips MR
// Imaging DD 001" and "PHILIPS MR IMAGING DD 001"). Creators read as UN, as
// Implicit VR files without a private dictionary produce them (#1034), are
// matched by their text; a creator reserved in two blocks is ambiguous.
func privateTag(o *object.Object, creator string, offset uint8) (core.Tag, bool) {
	for _, name := range []string{creator, strings.ToUpper(creator)} {
		if tag, err := o.PrivateTag(PhilipsGroup, name, offset); err == nil {
			return tag, true
		}
	}
	block := -1
	for _, element := range o.Elements() {
		tag := element.Tag()
		if tag.Group != PhilipsGroup || tag.Element < 0x0010 || tag.Element > 0x00FF {
			continue
		}
		values, err := stringValues(o, element)
		if err != nil || len(values) != 1 {
			continue
		}
		if name := strings.TrimSpace(values[0]); name == creator || name == strings.ToUpper(creator) {
			if block >= 0 {
				return core.Tag{}, false
			}
			block = int(tag.Element)
		}
	}
	if block < 0 {
		return core.Tag{}, false
	}
	return core.NewTag(PhilipsGroup, uint16(block)<<8|uint16(offset)), true
}
