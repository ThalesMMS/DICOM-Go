package gsps

import (
	"bytes"
	"errors"
	"image"
	"testing"

	"github.com/ThalesMMS/dicom-go/object"
	"github.com/ThalesMMS/dicom-go/pixeldata/display"
)

// A shared GSPS can give two frames of the same SOP different display areas,
// window/level and graphic content. Neither applying nor rejecting a state may
// edit the referenced frame's stored samples.
func TestReferenceGSPSScopesPresentationAndPreservesSourcePixels(t *testing.T) {
	all := ReferencedImage{
		SeriesInstanceUID: "2.25.651",
		SOPClassUID:       "1.2.840.10008.5.1.4.1.1.2.1",
		SOPInstanceUID:    "2.25.652",
		Frames:            []int{1, 3},
	}
	third := all
	third.Frames = []int{3}
	source := &State{
		SOPInstanceUID: "2.25.653", StudyInstanceUID: "2.25.654", SeriesInstanceUID: "2.25.655",
		ReferencedImages: []ReferencedImage{all},
		DisplayedAreas: []DisplayedArea{
			{Defined: true, TopLeftX: 1, TopLeftY: 1, BottomRightX: 9, BottomRightY: 7},
			{Defined: true, TopLeftX: 3, TopLeftY: 2, BottomRightX: 8, BottomRightY: 5,
				PresentationSizeMode: "MAGNIFY", MagnificationRatio: 2, ReferencedImages: []ReferencedImage{third}},
		},
		SpatialTransform: SpatialTransform{RotationDegrees: 90, FlipHorizontal: true},
		SoftcopyVOIs: []SoftcopyVOI{
			{WindowCenter: 100, WindowWidth: 180},
			{WindowCenter: 60, WindowWidth: 75, ReferencedImages: []ReferencedImage{third}},
		},
		GraphicAnnotations: []GraphicAnnotation{{
			LayerName: "MARKERS", GraphicType: GraphicTypePoint, GraphicUnits: AnnotationUnitsDisplay,
			Polyline: []Point2D{{X: 0.5, Y: 0.5}}, ReferencedImages: []ReferencedImage{third},
		}},
	}
	file, err := Write(source)
	if err != nil {
		t.Fatal(err)
	}
	defer file.Close()
	var wire bytes.Buffer
	if err := object.WriteFile(&wire, file); err != nil {
		t.Fatal(err)
	}
	decoded, err := object.ReadFile(&wire)
	if err != nil {
		t.Fatal(err)
	}
	defer decoded.Close()
	state, err := Read(decoded.Dataset)
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name          string
		instance      string
		frame         int
		center, width float64
		magnify       float64
		extent        image.Point
		wantPoint     *Point2D
		wantErr       error
	}{
		{name: "all frame defaults", instance: all.SOPInstanceUID, frame: 1, center: 100, width: 180, extent: image.Pt(7, 9)},
		{name: "specific frame override", instance: all.SOPInstanceUID, frame: 3, center: 60, width: 75, magnify: 2,
			extent: image.Pt(4, 6), wantPoint: &Point2D{X: 5, Y: 3}},
		{name: "unselected frame", instance: all.SOPInstanceUID, frame: 2, wantErr: ErrMissingReference},
		{name: "wrong SOP with selected frame number", instance: "2.25.656", frame: 3, wantErr: ErrMissingReference},
	} {
		t.Run(tc.name, func(t *testing.T) {
			pixels := make([]byte, 7*9)
			for i := range pixels {
				pixels[i] = byte(i * 3)
			}
			before := bytes.Clone(pixels)
			frame := display.Frame{Rows: 7, Columns: 9, Pixels: pixels,
				Format: display.PixelFormat{BitsAllocated: 8, BitsStored: 8, HighBit: 7}}
			target := ImageState{SOPInstanceUID: tc.instance, FrameNumber: tc.frame, Rows: frame.Rows, Columns: frame.Columns}
			applied, applyErr := Apply(state, target)
			rendered, renderErr := Render(frame, state, target, RenderOptions{})
			if !bytes.Equal(frame.Pixels, before) {
				t.Fatal("GSPS changed source samples")
			}
			if tc.wantErr != nil {
				if !errors.Is(applyErr, tc.wantErr) || !errors.Is(renderErr, tc.wantErr) || rendered != nil {
					t.Fatalf("Apply error = %v; Render = %v, %v; want %v", applyErr, rendered, renderErr, tc.wantErr)
				}
				return
			}
			if applyErr != nil || renderErr != nil {
				t.Fatalf("Apply error = %v; Render error = %v", applyErr, renderErr)
			}
			if applied.WindowCenter != tc.center || applied.WindowWidth != tc.width ||
				applied.RotationDegrees != 90 || !applied.FlipHorizontal ||
				applied.DisplayedArea.MagnificationRatio != tc.magnify || rendered.Bounds().Size() != tc.extent {
				t.Fatalf("applied = %+v, rendered bounds = %v", applied, rendered.Bounds())
			}
			if tc.wantPoint == nil {
				if len(applied.GraphicAnnotations) != 0 {
					t.Fatalf("unscoped frame acquired annotations: %+v", applied.GraphicAnnotations)
				}
				return
			}
			if len(applied.GraphicAnnotations) != 1 {
				t.Fatalf("applied annotations = %+v, want one point", applied.GraphicAnnotations)
			}
			mark := applied.GraphicAnnotations[0]
			if mark.GraphicType != GraphicTypePoint || mark.GraphicUnits != AnnotationUnitsPixel ||
				len(mark.Polyline) != 1 || mark.Polyline[0] != *tc.wantPoint {
				t.Fatalf("annotation = %+v, want pixel point %v", mark, *tc.wantPoint)
			}
		})
	}
}
