package rtstruct

import (
	"errors"
	"math"
	"testing"

	"github.com/ThalesMMS/dicom-go/render"
	dicomroi "github.com/ThalesMMS/dicom-go/roi"
)

func TestPreviewSlicePlacesObliqueContourBySOPBeforePlaneDistance(t *testing.T) {
	const rootHalf = math.Sqrt2 / 2
	plane := render.SliceGeometry{
		Origin:     render.Vec3{X: 10, Y: 20, Z: 30},
		RowDir:     render.Vec3{X: 1},
		ColDir:     render.Vec3{Y: rootHalf, Z: rootHalf},
		Normal:     render.Vec3{Y: -rootHalf, Z: rootHalf},
		RowSpacing: 2, ColSpacing: 0.5, Rows: 4, Columns: 4,
	}
	outline := []dicomroi.Point2D{{X: 0.6, Y: 0.6}, {X: 2.4, Y: 0.6}, {X: 2.4, Y: 2.4}, {X: 0.6, Y: 2.4}}
	nearbyPlane := plane
	nearbyPlane.Origin = plane.Origin.Add(plane.Normal.Scale(4))
	contour := Contour{
		GeometricType:    ContourClosedPlanar,
		Points:           imagePointsToPatient(outline, nearbyPlane),
		ReferencedImages: []ReferencedImage{{SOPInstanceUID: "referenced"}},
	}

	preview := PreviewSlice([]Contour{contour}, SlicePreviewOptions{
		SOPInstanceUID: "referenced", Geometry: plane, PlaneToleranceMM: 0.5,
	})
	if preview.PartialGeometry || preview.MissingGeometry || len(preview.Contours) != 1 || preview.Mask == nil {
		t.Fatalf("referenced preview = %+v", preview)
	}
	for i, want := range outline {
		got := preview.Contours[0][i]
		if math.Abs(got.X-want.X) > 1e-12 || math.Abs(got.Y-want.Y) > 1e-12 {
			t.Fatalf("projected point %d = %+v, want %+v", i, got, want)
		}
	}
	if got := preview.Mask.Count(); got != 4 || !preview.Mask.Get(1, 1) || !preview.Mask.Get(2, 2) {
		t.Fatalf("pixel-centre fill count = %d, want four pixels at columns and rows 1 and 2", got)
	}
	wrongSOP := PreviewSlice([]Contour{contour}, SlicePreviewOptions{
		SOPInstanceUID: "nearest", Geometry: nearbyPlane, PlaneToleranceMM: 0.5,
	})
	if len(wrongSOP.Contours) != 0 || wrongSOP.Mask != nil {
		t.Fatalf("preview used plane geometry despite a nonmatching image reference: %+v", wrongSOP)
	}

	set := &StructureSet{ROIs: []ROI{{Number: 1, Contours: []Contour{contour}}}}
	geometry := render.BuildVolumeGeometry([]render.SliceGeometry{plane, nearbyPlane}, render.DefaultGeometryTolerances())
	segmentation, err := Rasterize(set, RasterizeOptions{ROINumber: 1, Geometry: geometry, Rows: 4, Columns: 4})
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := segmentation.MaskAt(0); ok {
		t.Fatal("strict rasterization used the SOP reference instead of the nearest plane")
	}
	if mask, ok := segmentation.MaskAt(1); !ok || mask.Count() != 4 {
		t.Fatalf("nearest plane mask = %v, %v, want four pixels", mask, ok)
	}
}

func TestPreviewSlicePlaneToleranceAndReferencedXYRecovery(t *testing.T) {
	slice := testRTGeometry().Slices[0]
	options := SlicePreviewOptions{SOPInstanceUID: "image", Geometry: slice, PlaneToleranceMM: 0.5}
	contour := Contour{
		GeometricType: ContourClosedPlanar,
		Points:        []Point3D{{X: 1, Y: 1, Z: 0.5}, {X: 3, Y: 1, Z: 0.5}, {X: 3, Y: 3, Z: 0.5}},
	}
	if preview := PreviewSlice([]Contour{contour}, options); len(preview.Contours) != 1 || preview.MissingGeometry {
		t.Fatalf("unreferenced contour at plane tolerance = %+v", preview)
	}
	contour.Points[2].Z = math.Nextafter(0.5, 1)
	if preview := PreviewSlice([]Contour{contour}, options); len(preview.Contours) != 0 {
		t.Fatalf("unreferenced contour with one off-plane point = %+v", preview)
	}
	options.Geometry = render.SliceGeometry{Rows: 4, Columns: 4}
	if preview := PreviewSlice([]Contour{contour}, options); len(preview.Contours) != 0 {
		t.Fatalf("unreferenced contour on a slice without geometry = %+v", preview)
	}
	contour.ReferencedImages = []ReferencedImage{{SOPInstanceUID: "image"}}
	preview := PreviewSlice([]Contour{contour}, options)
	if !preview.MissingGeometry || len(preview.Contours) != 1 || preview.Mask == nil ||
		preview.Contours[0][0] != (dicomroi.Point2D{X: 1, Y: 1}) {
		t.Fatalf("referenced XY recovery = %+v", preview)
	}
	options.Geometry.Rows = 0
	preview = PreviewSlice([]Contour{contour}, options)
	if !preview.MissingGeometry || len(preview.Contours) != 1 || preview.Mask != nil {
		t.Fatalf("referenced outline without a raster grid = %+v", preview)
	}
}

func TestPreviewSliceKeepsXORHolesWhenSkippingOtherContourTypes(t *testing.T) {
	box := func(geometricType string, min, max float64) Contour {
		return Contour{GeometricType: geometricType, Points: []Point3D{
			{X: min, Y: min}, {X: max, Y: min}, {X: max, Y: max}, {X: min, Y: max},
		}}
	}
	geometry := testRTGeometry()
	contours := []Contour{
		box(ContourClosedPlanarXOR, 0, 4),
		box(ContourClosedPlanarXOR, 1, 3),
		box(ContourClosedPlanar, 0, 4),
		box(ContourOpenPlanar, 0, 4),
	}
	preview := PreviewSlice(contours, SlicePreviewOptions{Geometry: geometry.Slices[0]})
	if !preview.PartialGeometry || preview.MissingGeometry || len(preview.Contours) != 2 || preview.Mask == nil {
		t.Fatalf("mixed contour preview = %+v", preview)
	}
	if preview.Mask.Count() != 12 || preview.Mask.Get(1, 1) || !preview.Mask.Get(0, 0) {
		t.Fatalf("XOR mask count = %d, want 12 with centre hole", preview.Mask.Count())
	}

	roi := ROI{Number: 1, Contours: contours}
	set := &StructureSet{ROIs: []ROI{roi}}
	opts := RasterizeOptions{ROINumber: 1, Geometry: geometry, Rows: 8, Columns: 8}
	if _, err := Rasterize(set, opts); !errors.Is(err, ErrUnsupportedContourType) {
		t.Fatalf("strict rasterization error = %v, want ErrUnsupportedContourType", err)
	}
	set.ROIs[0].Contours = contours[:3]
	if _, err := Rasterize(set, opts); !errors.Is(err, ErrGeometryMismatch) {
		t.Fatalf("strict rasterization error = %v, want ErrGeometryMismatch", err)
	}
}
