package gsps

import (
	"errors"
	"image"
	"reflect"
	"testing"
)

func TestCompoundRenderingChoosesTheExactLinkedAlternative(t *testing.T) {
	const imageUID = "1.2.3.4"
	validRef := []ReferencedImage{{SOPInstanceUID: imageUID, Frames: []int{1}}}
	invalidRef := []ReferencedImage{{SOPInstanceUID: imageUID, Frames: []int{2}}}
	filled := false
	valid := &CompoundGraphic{Type: CompoundTypeRectangle, Units: AnnotationUnitsPixel, Filled: &filled,
		Points: []Point2D{{4, 4}, {10, 10}}}
	invalid := &CompoundGraphic{Type: CompoundTypeRectangle, Units: AnnotationUnitsPixel,
		Points: []Point2D{{4, 4}, {10, 10}}}
	private := &CompoundGraphic{Type: "VENDOR_SHAPE", Units: AnnotationUnitsPixel, Points: []Point2D{{4, 4}}}
	state := &State{
		ReferencedImages: []ReferencedImage{{SOPInstanceUID: imageUID}},
		GraphicAnnotations: []GraphicAnnotation{
			{LayerName: "TEST", GroupIndex: 1, Compound: valid, HasCompoundGraphicID: true, ReferencedImages: validRef},
			{LayerName: "TEST", GroupIndex: 1, HasCompoundGraphicID: true, ReferencedImages: validRef, Polyline: []Point2D{{15, 16}, {22, 16}}},
			{LayerName: "TEST", GroupIndex: 2, Compound: invalid, CompoundGraphicID: 4, ReferencedImages: invalidRef},
			{LayerName: "TEST", GroupIndex: 2, CompoundGraphicID: 4, ReferencedImages: invalidRef, Polyline: []Point2D{{15, 16}, {22, 16}}},
			{LayerName: "TEST", GroupIndex: 3, Compound: private, CompoundGraphicID: 5, ReferencedImages: invalidRef},
			{LayerName: "TEST", GroupIndex: 3, CompoundGraphicID: 5, ReferencedImages: invalidRef, Polyline: []Point2D{{15, 25}, {22, 25}}},
			{LayerName: "TEST", ReferencedImages: validRef, Polyline: []Point2D{{15, 21}, {22, 21}}},
		},
	}
	frame := blackFrame(32, 40)
	source := append([]byte(nil), frame.Pixels...)
	for _, tc := range []struct {
		frame                    int
		primary, fallback, plain uint8
		privateFallback          uint8
	}{
		{frame: 1, primary: 255, plain: 255},
		{frame: 2, fallback: 255, privateFallback: 255},
	} {
		target := ImageState{SOPInstanceUID: imageUID, FrameNumber: tc.frame, Rows: 32, Columns: 40}
		out, err := Render(frame, state, target, RenderOptions{})
		if err != nil {
			t.Fatal(err)
		}
		overlay, err := RenderAnnotationOverlay(state, target, out.Bounds(), samePoint)
		if err != nil {
			t.Fatal(err)
		}
		for p, want := range map[image.Point]uint8{
			{X: 7, Y: 4}: tc.primary, {X: 18, Y: 16}: tc.fallback,
			{X: 18, Y: 21}: tc.plain, {X: 18, Y: 25}: tc.privateFallback,
		} {
			if gray, alpha := out.GrayAt(p.X, p.Y).Y, overlay.NRGBAAt(p.X, p.Y).A; gray != want || alpha != want {
				t.Fatalf("frame %d at %v: gray/alpha %d/%d, want %d", tc.frame, p, gray, alpha, want)
			}
		}
	}
	if _, err := RenderAnnotationOverlay(state, ImageState{SOPInstanceUID: "1.2.3.9", FrameNumber: 1, Rows: 32, Columns: 40},
		image.Rect(0, 0, 40, 32), samePoint); !errors.Is(err, ErrMissingReference) {
		t.Fatalf("unrelated SOP overlay error = %v", err)
	}
	if !reflect.DeepEqual(frame.Pixels, source) {
		t.Fatal("annotation rendering changed source pixels")
	}
}

func TestCompoundRenderingAllowsAllStandardShapes(t *testing.T) {
	ends := []Point2D{{.20, .52}, {.80, .52}}
	center := Point2D{X: .5, Y: .52}
	var gap, visibility = .1, .65
	filled, labels := false, true
	tests := []CompoundGraphic{
		{Type: CompoundTypeMultiline, Points: []Point2D{{.2, .2}, {.8, .3}, {.3, .6}, {.7, .75}}},
		{Type: CompoundTypeInfiniteLine, Points: ends, GapLength: &gap, RotationPoint: &center},
		{Type: CompoundTypeCutline, Points: ends, GapLength: &gap, RotationPoint: &center},
		{Type: CompoundTypeRangeLine, Points: ends},
		{Type: CompoundTypeRuler, Points: ends, TickAlignment: "CENTER", TickLabelAlignment: "BOTTOM", ShowTickLabel: &labels},
		{Type: CompoundTypeAxis, Points: ends, TickAlignment: "TOP", TickLabelAlignment: "TOP", ShowTickLabel: &labels,
			MajorTicks: []CompoundTick{{Position: 0, Label: "A"}, {Position: 1, Label: "B"}}},
		{Type: CompoundTypeCrosshair, Points: []Point2D{center}, GapLength: &gap, DiameterOfVisibility: &visibility, TickAlignment: "CENTER", TickLabelAlignment: "BOTTOM", ShowTickLabel: &labels},
		{Type: CompoundTypeArrow, Points: ends},
		{Type: CompoundTypeRectangle, Points: []Point2D{{.25, .25}, {.75, .75}}, Filled: &filled},
		{Type: CompoundTypeEllipse, Points: []Point2D{{.25, .25}, {.75, .75}}, Filled: &filled},
	}
	target := ImageState{Rows: 64, Columns: 64}
	for _, test := range tests {
		t.Run(test.Type, func(t *testing.T) {
			test.Units = AnnotationUnitsDisplay
			state := &State{GraphicAnnotations: []GraphicAnnotation{
				{LayerName: "TEST", CompoundGraphicID: 2, Compound: &test},
				{LayerName: "TEST", CompoundGraphicID: 2, Polyline: []Point2D{{2, 2}, {5, 2}}},
			}}
			out, err := RenderAnnotationOverlay(state, target, image.Rect(0, 0, 64, 64), samePoint)
			if err != nil {
				t.Fatal(err)
			}
			visible := 0
			for i := 3; i < len(out.Pix); i += 4 {
				if out.Pix[i] != 0 {
					visible++
				}
			}
			if visible == 0 || out.NRGBAAt(3, 2).A != 0 {
				t.Fatalf("rendered %d pixels, fallback alpha=%d", visible, out.NRGBAAt(3, 2).A)
			}
		})
	}
}

func TestCompoundRenderingKeepsAlternativesForAmbiguousLinks(t *testing.T) {
	outline := false
	shape := func(group int) GraphicAnnotation {
		return GraphicAnnotation{LayerName: "TEST", GroupIndex: group, CompoundGraphicID: 9,
			Compound: &CompoundGraphic{Type: CompoundTypeRectangle, Units: AnnotationUnitsPixel,
				Points: []Point2D{{4, 4}, {10, 10}}, Filled: &outline}}
	}
	fallback := GraphicAnnotation{
		LayerName: "TEST", GroupIndex: 2, CompoundGraphicID: 9, Polyline: []Point2D{{15, 16}, {22, 16}},
	}
	for _, tc := range []struct {
		name  string
		items []GraphicAnnotation
	}{
		{name: "mismatched annotation item", items: []GraphicAnnotation{shape(1), fallback}},
		{name: "repeated ID", items: []GraphicAnnotation{shape(1), shape(2), fallback}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			state := &State{GraphicAnnotations: tc.items}
			target := ImageState{Rows: 32, Columns: 40}
			result, err := Apply(state, target)
			if err != nil || len(result.GraphicAnnotations) != 1 || result.GraphicAnnotations[0].Compound != nil {
				t.Fatalf("selection = %+v, error=%v", result.GraphicAnnotations, err)
			}
			out, err := RenderAnnotationOverlay(state, target, image.Rect(0, 0, 40, 32), samePoint)
			if err != nil {
				t.Fatal(err)
			}
			if out.NRGBAAt(7, 4).A != 0 || out.NRGBAAt(18, 16).A != 255 {
				t.Fatalf("ambiguous appearances were combined: primary=%v alternative=%v",
					out.NRGBAAt(7, 4), out.NRGBAAt(18, 16))
			}
		})
	}
}

func TestCompoundRenderingUsesDisplayedAreaAndDetachedSnapshot(t *testing.T) {
	filled := true
	alpha := 0.5
	white := [3]uint16{65535, 32896, 32896}
	state := &State{
		SpatialTransform: SpatialTransform{RotationDegrees: 90, FlipHorizontal: true},
		DisplayedArea:    DisplayedArea{Defined: true, TopLeftX: 5, TopLeftY: 3, BottomRightX: 16, BottomRightY: 12},
		GraphicAnnotations: []GraphicAnnotation{{
			CompoundGraphicID: 1,
			Compound: &CompoundGraphic{
				Type: CompoundTypeRectangle, Units: AnnotationUnitsDisplay,
				Points: []Point2D{{.25, .25}, {.75, .75}}, Filled: &filled,
				FillStyle: &CompoundFillStyle{Mode: "SOLID", OnColor: &white, OnOpacity: &alpha},
			},
		}},
	}
	snapshot, err := CaptureAnnotationState(state)
	if err != nil {
		t.Fatal(err)
	}
	// Neither caller edits to the style nor clearing geometry affect the
	// previously captured annotation on the requested frame.
	alpha = 0
	state.DisplayedArea = DisplayedArea{}
	state.GraphicAnnotations[0].Compound.Points[0] = Point2D{X: .9, Y: .9}
	target := ImageState{Rows: 20, Columns: 24}
	// A clockwise quarter turn followed by a horizontal flip swaps the
	// original axes; the chosen image area maps to output [2,12)×[4,16).
	overlay, err := RenderAnnotationOverlay(snapshot, target, image.Rect(0, 0, 20, 24),
		func(p Point2D) Point2D { return Point2D{X: p.Y, Y: p.X} })
	if err != nil {
		t.Fatal(err)
	}
	center := overlay.NRGBAAt(7, 10)
	if center.A != 128 || center.R < 245 || center.G < 245 || center.B < 245 || overlay.NRGBAAt(14, 10).A != 0 {
		t.Fatalf("center=%+v outside=%+v", center, overlay.NRGBAAt(14, 10))
	}
}

func TestCompoundCutlineZeroGapKeepsTwoArrows(t *testing.T) {
	zero := 0.0
	center := Point2D{X: 32.5, Y: 32.5}
	graphic := &CompoundGraphic{Type: CompoundTypeCutline, Units: AnnotationUnitsPixel,
		Points: []Point2D{{12, 32.5}, {50, 32.5}}, RotationPoint: &center, GapLength: &zero}
	plan, err := projectCompound(graphic, func(p Point2D, _ string) (Point2D, error) { return p, nil }, image.Rect(0, 0, 64, 64))
	if err != nil {
		t.Fatal(err)
	}
	if len(plan.paths) != 6 || plan.paths[2][1].X >= center.X || plan.paths[4][1].X <= center.X {
		t.Fatalf("expected arrows on opposite halves, got %v", plan.paths)
	}
}

func samePoint(p Point2D) Point2D { return p }
