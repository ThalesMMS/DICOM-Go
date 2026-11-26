package rtstruct

import (
	"bytes"
	"errors"
	"fmt"
	"math"
	"testing"

	"github.com/ThalesMMS/dicom-go/object"
	"github.com/ThalesMMS/dicom-go/render"
	dicomroi "github.com/ThalesMMS/dicom-go/roi"
)

func rasterROITestMask(columns, rows int) *dicomroi.RasterMask {
	// A ring with a hole, a separate blob and a diagonal pair of pixels.
	mask := dicomroi.NewRasterMask(columns, rows)
	for y := 2; y < 11; y++ {
		for x := 2; x < 11; x++ {
			if x < 5 || x > 7 || y < 5 || y > 7 {
				mask.Set(x, y, true)
			}
		}
	}
	for y := 3; y < 6; y++ {
		for x := 13; x < 17; x++ {
			mask.Set(x, y, true)
		}
	}
	mask.Set(14, 10, true)
	mask.Set(15, 11, true)
	return mask
}

func rasterROITestRefs(sopInstanceUID string) []ReferencedImage {
	return []ReferencedImage{{
		StudyInstanceUID:  "1.2.826.0.1.3680043.9.7433.1075.1",
		SeriesInstanceUID: "1.2.826.0.1.3680043.9.7433.1075.2",
		SOPClassUID:       "1.2.840.10008.5.1.4.1.1.2",
		SOPInstanceUID:    sopInstanceUID,
	}}
}

func writeAndReadStructureSet(t *testing.T, set *StructureSet) *StructureSet {
	t.Helper()
	file, err := Write(set)
	if err != nil {
		t.Fatalf("Write: %v", err)
	}
	var encoded bytes.Buffer
	if err := object.WriteFile(&encoded, file); err != nil {
		t.Fatalf("object.WriteFile: %v", err)
	}
	readFile, err := object.ReadFile(bytes.NewReader(encoded.Bytes()))
	if err != nil {
		t.Fatalf("object.ReadFile: %v", err)
	}
	out, err := Read(readFile.Dataset)
	if err != nil {
		t.Fatalf("Read: %v", err)
	}
	return out
}

// TestFromVectorROIsWritesRasterMasksAsXORContoursThatRoundTrip checks the
// raster → RTSTRUCT → raster round trip of #1075 on an anisotropic, offset
// grid: the Part 10 object read back rasterizes to the very same masks.
func TestFromVectorROIsWritesRasterMasksAsXORContoursThatRoundTrip(t *testing.T) {
	const columns, rows = 20, 14
	geometry := subPixelTestGeometry(render.Vec3{X: -10, Y: 5, Z: 30}, 0.7, 1.3, 2.5, columns, rows, 2)
	masks := []*dicomroi.RasterMask{rasterROITestMask(columns, rows), dicomroi.NewRasterMask(columns, rows)}
	masks[1].Set(4, 4, true)
	var slices []RasterROIContour
	for index, mask := range masks {
		slices = append(slices, RasterROIContour{
			Mask: mask, Geometry: geometry.Slices[index],
			ReferencedImages: []ReferencedImage{{
				StudyInstanceUID:  "1.2.826.0.1.3680043.9.7433.1075.1",
				SeriesInstanceUID: "1.2.826.0.1.3680043.9.7433.1075.2",
				SOPClassUID:       "1.2.840.10008.5.1.4.1.1.2",
				SOPInstanceUID:    fmt.Sprintf("1.2.826.0.1.3680043.9.7433.1075.3.%d", index+1),
			}},
		})
	}
	set, err := FromVectorROIs(FromVectorROIsOptions{
		SOPClassUID:         RTStructureSetStorage,
		SOPInstanceUID:      "1.2.826.0.1.3680043.9.7433.1075.4",
		StudyInstanceUID:    "1.2.826.0.1.3680043.9.7433.1075.1",
		SeriesInstanceUID:   "1.2.826.0.1.3680043.9.7433.1075.5",
		FrameOfReferenceUID: "1.2.826.0.1.3680043.9.7433.1075.6",
		ROIs:                []VectorROI{{Number: 1, Name: "Brush", Masks: slices}},
	})
	if err != nil {
		t.Fatalf("FromVectorROIs: %v", err)
	}
	for _, contour := range set.ROIs[0].Contours {
		if contour.GeometricType != ContourClosedPlanarXOR {
			t.Fatalf("contour type = %q, want CLOSEDPLANAR_XOR", contour.GeometricType)
		}
	}
	roundTrip := writeAndReadStructureSet(t, set)
	segmentation, err := Rasterize(roundTrip, RasterizeOptions{ROINumber: 1, Geometry: geometry, Columns: columns, Rows: rows})
	if err != nil {
		t.Fatalf("Rasterize: %v", err)
	}
	for index, want := range masks {
		got := segmentation.Mask(index)
		for y := 0; y < rows; y++ {
			for x := 0; x < columns; x++ {
				if got.Get(x, y) != want.Get(x, y) {
					t.Fatalf("slice %d voxel (%d,%d) = %v, want %v", index, x, y, got.Get(x, y), want.Get(x, y))
				}
			}
		}
	}
}

func TestFromVectorROIsSmoothedRasterMaskStaysWithinAPixel(t *testing.T) {
	const columns, rows = 40, 40
	geometry := subPixelTestGeometry(render.Vec3{}, 1, 1, 1, columns, rows, 1)
	mask := dicomroi.NewRasterMask(columns, rows)
	const r = 12.0
	for y := 0; y < rows; y++ {
		for x := 0; x < columns; x++ {
			if math.Hypot(float64(x)-19.5, float64(y)-19.5) <= r {
				mask.Set(x, y, true)
			}
		}
	}
	set, err := FromVectorROIs(FromVectorROIsOptions{
		SOPClassUID:         RTStructureSetStorage,
		SOPInstanceUID:      "1.2.826.0.1.3680043.9.7433.1075.7",
		FrameOfReferenceUID: "1.2.826.0.1.3680043.9.7433.1075.8",
		ROIs: []VectorROI{{Number: 1, Name: "Smooth", Masks: []RasterROIContour{{
			Mask: mask, Geometry: geometry.Slices[0], SmoothSigma: 1,
			ReferencedImages: rasterROITestRefs("1.2.826.0.1.3680043.9.7433.1075.9"),
		}}}},
	})
	if err != nil {
		t.Fatalf("FromVectorROIs: %v", err)
	}
	if len(set.ROIs[0].Contours) != 1 {
		t.Fatalf("contours = %d, want one smooth boundary", len(set.ROIs[0].Contours))
	}
	for _, point := range set.ROIs[0].Contours[0].Points {
		if d := math.Hypot(point.X-19.5, point.Y-19.5); math.Abs(d-r) > 1 {
			t.Fatalf("smoothed boundary point %+v is %.2f mm from the centre, want %.1f ± 1", point, d, r)
		}
	}
}

func TestFromVectorROIsRejectsInvalidRasterMasks(t *testing.T) {
	geometry := subPixelTestGeometry(render.Vec3{}, 1, 1, 1, 8, 8, 1).Slices[0]
	refs := rasterROITestRefs("1.2.826.0.1.3680043.9.7433.1075.10")
	full := dicomroi.NewRasterMask(8, 8)
	full.Set(2, 2, true)
	square := dicomroi.VectorROI{Shape: dicomroi.ROIRectangle, SubpixelPoints: []dicomroi.Point2D{{X: 1, Y: 1}, {X: 3, Y: 3}}}
	cases := map[string]VectorROI{
		"empty":    {Number: 1, Name: "Empty", Masks: []RasterROIContour{{Mask: dicomroi.NewRasterMask(8, 8), Geometry: geometry, ReferencedImages: refs}}},
		"size":     {Number: 1, Name: "Size", Masks: []RasterROIContour{{Mask: dicomroi.NewRasterMask(4, 8), Geometry: geometry, ReferencedImages: refs}}},
		"nil":      {Number: 1, Name: "Nil", Masks: []RasterROIContour{{Geometry: geometry, ReferencedImages: refs}}},
		"mixed":    {Number: 1, Name: "Mixed", Masks: []RasterROIContour{{Mask: full, Geometry: geometry, ReferencedImages: refs}}, Contours: []VectorROIContour{{ROI: square, Geometry: geometry, ReferencedImages: refs}}},
		"no-image": {Number: 1, Name: "Unreferenced", Masks: []RasterROIContour{{Mask: full, Geometry: geometry}}},
	}
	for name, item := range cases {
		_, err := FromVectorROIs(FromVectorROIsOptions{
			SOPClassUID:         RTStructureSetStorage,
			SOPInstanceUID:      "1.2.826.0.1.3680043.9.7433.1075.11",
			FrameOfReferenceUID: "1.2.826.0.1.3680043.9.7433.1075.12",
			ROIs:                []VectorROI{item},
		})
		want := ErrGeometryMismatch
		if name == "no-image" {
			want = ErrMissingReference
		}
		if !errors.Is(err, want) {
			t.Fatalf("%s: error = %v, want %v", name, err, want)
		}
	}
}
