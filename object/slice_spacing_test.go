package object

import (
	"math"
	"testing"

	"github.com/ThalesMMS/dicom-go/core"
	"github.com/ThalesMMS/dicom-go/dictionary/std"
)

func TestResolveSliceSpacingPrecedence(t *testing.T) {
	for _, tc := range []struct {
		name               string
		spacing, thickness float64
		want               SliceSpacing
	}{
		{"spacing between slices wins over thickness", 3, 5, SliceSpacing{Value: 3, Source: SliceSpacingFromSpacingBetweenSlices}},
		{"negative spacing keeps its magnitude", -2.5, 5, SliceSpacing{Value: 2.5, Source: SliceSpacingFromSpacingBetweenSlices, Negative: true}},
		{"thickness when spacing is absent", 0, 5, SliceSpacing{Value: 5, Source: SliceSpacingFromSliceThickness}},
		{"thickness when spacing is not finite", math.NaN(), 2, SliceSpacing{Value: 2, Source: SliceSpacingFromSliceThickness}},
		{"thickness when spacing is infinite", math.Inf(1), 2, SliceSpacing{Value: 2, Source: SliceSpacingFromSliceThickness}},
		{"default when neither is usable", 0, 0, SliceSpacing{Value: 1, Source: SliceSpacingDefault}},
		{"negative thickness is unusable", 0, -4, SliceSpacing{Value: 1, Source: SliceSpacingDefault}},
		{"non-finite thickness is unusable", 0, math.Inf(1), SliceSpacing{Value: 1, Source: SliceSpacingDefault}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := ResolveSliceSpacing(tc.spacing, tc.thickness); got != tc.want {
				t.Fatalf("ResolveSliceSpacing(%v, %v) = %+v, want %+v", tc.spacing, tc.thickness, got, tc.want)
			}
		})
	}
}

// TestFrameGeometrySliceSpacingPrecedence covers the ITK 9467d94115 /
// dbd46b48bf cases: a top-level Spacing Between Slices and one in the shared
// Pixel Measures both win over Slice Thickness.
func TestFrameGeometrySliceSpacingPrecedence(t *testing.T) {
	measures := func(elements ...core.Element) core.Element {
		return geometrySequence(tagSharedFunctionalGroups, core.DataSet{Elements: []core.Element{
			geometrySequence(tagPixelMeasuresSequence, core.DataSet{Elements: elements}),
		}})
	}
	for _, tc := range []struct {
		name     string
		elements []core.Element
		want     SliceSpacing
	}{
		{"top-level spacing between slices", []core.Element{
			dsMulti(tagSliceThickness, "5"), dsMulti(tagSpacingBetweenSlices, "3"),
		}, SliceSpacing{Value: 3, Source: SliceSpacingFromSpacingBetweenSlices}},
		{"top-level thickness only", []core.Element{
			dsMulti(tagSliceThickness, "5"),
		}, SliceSpacing{Value: 5, Source: SliceSpacingFromSliceThickness}},
		{"shared pixel measures spacing between slices", []core.Element{
			measures(dsMulti(tagSliceThickness, "1.2"), dsMulti(tagSpacingBetweenSlices, "0.6")),
		}, SliceSpacing{Value: 0.6, Source: SliceSpacingFromSpacingBetweenSlices}},
		{"shared spacing over top-level thickness", []core.Element{
			dsMulti(tagSliceThickness, "5"), measures(dsMulti(tagSpacingBetweenSlices, "2")),
		}, SliceSpacing{Value: 2, Source: SliceSpacingFromSpacingBetweenSlices}},
		{"nothing", nil, SliceSpacing{Value: 1, Source: SliceSpacingDefault}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			obj := FromElements(tc.elements, std.Dictionary)
			if got := obj.FrameGeometryAt(0).SliceSpacing(); got != tc.want {
				t.Fatalf("SliceSpacing() = %+v, want %+v", got, tc.want)
			}
		})
	}
}
