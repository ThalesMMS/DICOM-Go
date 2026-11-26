package render

import (
	"math"
	"slices"
	"testing"
)

// TestSingleValuePixelSpacingIsIsotropic: one Pixel Spacing value is used for
// rows and columns in the volume, the geometry and the 3D cursor (#1112),
// and a stack without one is flagged instead of silently measured at 1 mm.
func TestSingleValuePixelSpacingIsIsotropic(t *testing.T) {
	stack := gradientColumnStackWithSpacing(8, 8, 4, 0.5, 0.5, 2)
	stack.PixelSpacing = []float64{0.5}
	vol, err := BuildVolume(stack)
	if err != nil {
		t.Fatal(err)
	}
	if vol.RowSpacing != 0.5 || vol.ColSpacing != 0.5 {
		t.Fatalf("volume spacing row %v col %v, want 0.5 both", vol.RowSpacing, vol.ColSpacing)
	}
	assessment := stack.GeometryAssessment()
	if slices.Contains(assessment.Geometry.Issues, GeometryIssueMissingPixelSpacing) {
		t.Fatalf("issues %v", assessment.Geometry.Issues)
	}
	point, ok := PanelClickToWorld(stack, MPRPlaneAxial, 1, 4, 2)
	if !ok || point.X != 2 || point.Y != 1 || math.Abs(point.Z-2) > 1e-12 {
		t.Fatalf("PanelClickToWorld = %+v %v, want (2, 1, 2)", point, ok)
	}

	// The per-frame value serves when the series has none.
	stack.PixelSpacing = nil
	for _, frame := range stack.Frames {
		frame.PixelSpacing = []float64{0.25}
	}
	if vol, err := BuildVolume(stack); err != nil || vol.RowSpacing != 0.25 || vol.ColSpacing != 0.25 {
		t.Fatalf("per-frame single value: %v, row %v col %v", err, vol.RowSpacing, vol.ColSpacing)
	}

	for _, frame := range stack.Frames {
		frame.PixelSpacing = nil
	}
	assessment = stack.GeometryAssessment()
	if !slices.Contains(assessment.Geometry.Issues, GeometryIssueMissingPixelSpacing) {
		t.Fatalf("missing spacing not reported: %v", assessment.Geometry.Issues)
	}
	if assessment.Disposition != GeometryRegularFastPath || assessment.FallbackReason != GeometryIssueNone {
		t.Fatalf("missing spacing changed the disposition: %v %q", assessment.Disposition, assessment.FallbackReason)
	}
	if _, ok := PanelClickToWorld(stack, MPRPlaneAxial, 1, 4, 2); ok {
		t.Fatal("PanelClickToWorld assumed a spacing")
	}
}
