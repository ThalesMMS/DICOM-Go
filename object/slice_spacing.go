package object

import "math"

// SliceSpacingSource names where a through-plane slice spacing came from.
type SliceSpacingSource string

const (
	// SliceSpacingUnknown: no spacing was resolved.
	SliceSpacingUnknown SliceSpacingSource = ""
	// SliceSpacingFromPositions: measured between the patient positions of
	// adjacent slices. Volume builders prefer it over any attribute.
	SliceSpacingFromPositions SliceSpacingSource = "positions"
	// SliceSpacingFromSpacingBetweenSlices: Spacing Between Slices
	// (0018,0088), the centre-to-centre distance of adjacent slices.
	SliceSpacingFromSpacingBetweenSlices SliceSpacingSource = "spacing-between-slices"
	// SliceSpacingFromSliceThickness: Slice Thickness (0018,0050). It is the
	// slab the signal comes from, not a distance between slices, and serves
	// only as a proxy when Spacing Between Slices is absent.
	SliceSpacingFromSliceThickness SliceSpacingSource = "slice-thickness"
	// SliceSpacingDefault: neither attribute is usable; 1 mm is assumed.
	SliceSpacingDefault SliceSpacingSource = "default"
)

// DefaultSliceSpacingMM is the spacing ResolveSliceSpacing reports when no
// attribute is usable.
const DefaultSliceSpacingMM = 1.0

// SliceSpacing is a resolved through-plane spacing and its provenance.
type SliceSpacing struct {
	// Value is the positive spacing in mm.
	Value  float64
	Source SliceSpacingSource
	// Negative reports a negative Spacing Between Slices; Value is its
	// magnitude. Callers placing slices keep the sign (see FrameGeometryAt);
	// callers sizing a voxel use Value.
	Negative bool
}

// ResolveSliceSpacing is the one precedence rule for the spacing of a slice
// when the positions of neighbouring slices do not measure it: Spacing
// Between Slices (0018,0088), then Slice Thickness (0018,0050), then 1 mm.
// Zero stands for an absent value. A zero or non-finite value is skipped, as
// is a non-positive Slice Thickness; a negative Spacing Between Slices
// contributes its magnitude and sets Negative.
//
// The order follows ITK's DICOM readers (Apache-2.0): commit 9467d94115
// stopped taking the z spacing from Slice Thickness when Spacing Between
// Slices is present, dbd46b48bf applied the same precedence to the Pixel
// Measures of enhanced multi-frame objects, and 296bed66ff falls back to 1
// instead of a zero spacing. This is an independent Go implementation of that
// rule; no ITK code is copied.
func ResolveSliceSpacing(spacingBetweenSlices, sliceThickness float64) SliceSpacing {
	if spacingBetweenSlices != 0 && finiteFloat(spacingBetweenSlices) {
		return SliceSpacing{
			Value:    math.Abs(spacingBetweenSlices),
			Source:   SliceSpacingFromSpacingBetweenSlices,
			Negative: spacingBetweenSlices < 0,
		}
	}
	if sliceThickness > 0 && finiteFloat(sliceThickness) {
		return SliceSpacing{Value: sliceThickness, Source: SliceSpacingFromSliceThickness}
	}
	return SliceSpacing{Value: DefaultSliceSpacingMM, Source: SliceSpacingDefault}
}

// SliceSpacing applies ResolveSliceSpacing to the frame's Spacing Between
// Slices and Slice Thickness (top level, then shared and per-frame Pixel
// Measures, as FrameGeometryAt resolves them).
func (g FrameGeometry) SliceSpacing() SliceSpacing {
	spacing, thickness := 0.0, 0.0
	if g.HasSpacingBetweenSlices {
		spacing = g.SpacingBetweenSlices
	}
	if g.HasSliceThickness {
		thickness = g.SliceThickness
	}
	return ResolveSliceSpacing(spacing, thickness)
}

func finiteFloat(value float64) bool {
	return !math.IsNaN(value) && !math.IsInf(value, 0)
}
