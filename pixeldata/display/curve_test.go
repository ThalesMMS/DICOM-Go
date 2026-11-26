package display

import (
	"bytes"
	"crypto/md5"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"image"
	"math"
	"os"
	"path/filepath"
	"testing"

	"github.com/ThalesMMS/dicom-go/core"
	"github.com/ThalesMMS/dicom-go/object"
	"github.com/ThalesMMS/dicom-go/transfer"
)

func curveUS(values ...uint16) []byte {
	var out []byte
	for _, v := range values {
		out = binary.LittleEndian.AppendUint16(out, v)
	}
	return out
}

// polyCurveObject is a synthetic image with a POLY curve in group 5000 and an
// ROI curve in group 5002, both in pixel (PIXL) coordinates.
func polyCurveObject() *object.Object {
	return object.FromElements([]core.Element{
		ovUS(0x5000, 0x0005, 2), ovUS(0x5000, 0x0010, 3),
		ovText(0x5000, 0x0020, core.VRCS, "POLY"), ovText(0x5000, 0x0022, core.VRLO, "caliper"),
		core.Element{Header: core.ElementHeader{Tag: core.NewTag(0x5000, 0x0030), VR: core.VRSH}, Value: core.StringValue{"PIXL", "PIXL"}},
		ovUS(0x5000, 0x0103, CurveValuesUS),
		ovText(0x5000, 0x2500, core.VRLO, "LINE"),
		// (x,y) = (column,row), 1-based: (2,2) -> (6,2) -> (6,5).
		core.NewRawElement(core.NewTag(0x5000, 0x3000), core.VROW, curveUS(2, 2, 6, 2, 6, 5)),

		ovUS(0x5002, 0x0005, 2), ovUS(0x5002, 0x0010, 3),
		ovText(0x5002, 0x0020, core.VRCS, "ROI"), ovUS(0x5002, 0x0103, CurveValuesSS),
		core.NewRawElement(core.NewTag(0x5002, 0x3000), core.VROW, curveUS(1, 8, 3, 8, 1, 10)),
	}, nil)
}

func TestCurvesFromObjectDecodesPolyAndROI(t *testing.T) {
	curves, err := CurvesFromObject(polyCurveObject())
	if err != nil || len(curves) != 2 {
		t.Fatalf("curves %+v err %v", curves, err)
	}
	poly := curves[0]
	if poly.Group != 0x5000 || poly.TypeOfData != "POLY" || poly.Dimensions != 2 || poly.Description != "caliper" ||
		poly.Label != "LINE" || len(poly.AxisUnits) != 2 || poly.AxisUnits[0] != "PIXL" || !poly.IsImageGraphic() || poly.Closed() {
		t.Fatalf("poly %+v", poly)
	}
	points, ok := poly.ImagePoints()
	want := []CurvePoint{{1, 1}, {5, 1}, {5, 4}}
	if !ok || len(points) != len(want) {
		t.Fatalf("points %v", points)
	}
	for i := range want {
		if points[i] != want[i] {
			t.Fatalf("point %d = %v, want %v", i, points[i], want[i])
		}
	}
	if roi := curves[1]; roi.TypeOfData != "ROI" || !roi.Closed() || roi.ValueRepresentation != CurveValuesSS {
		t.Fatalf("roi %+v", roi)
	}
}

// TestPolyCurveDrawsAtImageCoordinates is the acceptance fixture: the POLY
// curve sets exactly the pixels of its two segments, the ROI outline is
// closed, and nothing else is drawn.
func TestPolyCurveDrawsAtImageCoordinates(t *testing.T) {
	curves, err := CurvesFromObject(polyCurveObject())
	if err != nil {
		t.Fatal(err)
	}
	img := image.NewGray(image.Rect(0, 0, 8, 12))
	for _, curve := range curves {
		curve.Composite(img, 255)
	}
	want := map[image.Point]bool{}
	for x := 1; x <= 5; x++ { // row 1, columns 1-5
		want[image.Pt(x, 1)] = true
	}
	for y := 1; y <= 4; y++ { // column 5, rows 1-4
		want[image.Pt(5, y)] = true
	}
	// ROI triangle (0,7) (2,7) (0,9), closed back to (0,7).
	for _, p := range []image.Point{{0, 7}, {1, 7}, {2, 7}, {1, 8}, {0, 9}, {0, 8}} {
		want[p] = true
	}
	for y := 0; y < 12; y++ {
		for x := 0; x < 8; x++ {
			if got := img.GrayAt(x, y).Y == 255; got != want[image.Pt(x, y)] {
				t.Errorf("pixel (%d,%d) set=%v, want %v", x, y, got, want[image.Pt(x, y)])
			}
		}
	}
}

func TestCurveCompositeClipsFarCoordinates(t *testing.T) {
	obj := object.FromElements([]core.Element{
		ovUS(0x5000, 0x0005, 2), ovUS(0x5000, 0x0010, 2), ovText(0x5000, 0x0020, core.VRCS, "POLY"),
		ovUS(0x5000, 0x0103, CurveValuesFD),
		core.NewRawElement(core.NewTag(0x5000, 0x3000), core.VROB, func() []byte {
			var out []byte
			for _, v := range []float64{-1e12, 3, 1e12, 3} {
				out = binary.LittleEndian.AppendUint64(out, math.Float64bits(v))
			}
			return out
		}()),
	}, nil)
	curves, err := CurvesFromObject(obj)
	if err != nil || len(curves) != 1 {
		t.Fatalf("curves %v err %v", curves, err)
	}
	img := image.NewGray(image.Rect(0, 0, 4, 4))
	curves[0].Composite(img, 9)
	for x := 0; x < 4; x++ {
		if img.GrayAt(x, 2).Y != 9 || img.GrayAt(x, 1).Y != 0 {
			t.Fatalf("row 2 not drawn across the image: %v", img.Pix)
		}
	}
}

func TestCurveIntervalSpacingAndNonGraphicTypes(t *testing.T) {
	// A 2-D PHYSIO curve whose X axis uses interval spacing: start 10,
	// step 5; only Y is stored.
	obj := object.FromElements([]core.Element{
		ovUS(0x5004, 0x0005, 2), ovUS(0x5004, 0x0010, 3), ovText(0x5004, 0x0020, core.VRCS, "PHYSIO"),
		ovUS(0x5004, 0x0103, CurveValuesUS),
		core.NewRawElement(core.NewTag(0x5004, 0x0110), core.VRUS, curveUS(0, 1)),
		ovUS(0x5004, 0x0112, 10), ovUS(0x5004, 0x0114, 5),
		core.NewRawElement(core.NewTag(0x5004, 0x3000), core.VROW, curveUS(7, 8, 9)),
	}, nil)
	curves, err := CurvesFromObject(obj)
	if err != nil || len(curves) != 1 {
		t.Fatalf("curves %v err %v", curves, err)
	}
	want := [][]float64{{10, 7}, {15, 8}, {20, 9}}
	for i, tuple := range curves[0].Points {
		if tuple[0] != want[i][0] || tuple[1] != want[i][1] {
			t.Fatalf("points %v, want %v", curves[0].Points, want)
		}
	}
	if curves[0].IsImageGraphic() {
		t.Fatal("PHYSIO curve treated as an image graphic")
	}
	img := image.NewGray(image.Rect(0, 0, 4, 4))
	curves[0].Composite(img, 1)
	for _, v := range img.Pix {
		if v != 0 {
			t.Fatal("non-graphic curve drawn")
		}
	}
}

func TestCurveNonPixelUnitsAreNotDrawn(t *testing.T) {
	obj := object.FromElements([]core.Element{
		ovUS(0x5000, 0x0005, 2), ovUS(0x5000, 0x0010, 1), ovText(0x5000, 0x0020, core.VRCS, "POLY"),
		core.Element{Header: core.ElementHeader{Tag: core.NewTag(0x5000, 0x0030), VR: core.VRSH}, Value: core.StringValue{"MM", "MM"}},
		ovUS(0x5000, 0x0103, CurveValuesUS),
		core.NewRawElement(core.NewTag(0x5000, 0x3000), core.VROW, curveUS(1, 1)),
	}, nil)
	curves, _ := CurvesFromObject(obj)
	if len(curves) != 1 || curves[0].IsImageGraphic() {
		t.Fatalf("millimetre POLY curve treated as pixel graphic: %+v", curves)
	}
}

func TestCurvesFromObjectReportsInvalidGroups(t *testing.T) {
	obj := object.FromElements([]core.Element{
		// Too little Curve Data for 3 points of 2 US values.
		ovUS(0x5000, 0x0005, 2), ovUS(0x5000, 0x0010, 3), ovUS(0x5000, 0x0103, CurveValuesUS),
		core.NewRawElement(core.NewTag(0x5000, 0x3000), core.VROW, curveUS(1, 2, 3)),
		// Unknown Data Value Representation.
		ovUS(0x5002, 0x0005, 1), ovUS(0x5002, 0x0010, 1), ovUS(0x5002, 0x0103, 9),
		core.NewRawElement(core.NewTag(0x5002, 0x3000), core.VROW, curveUS(1)),
		// Missing Curve Dimensions.
		ovUS(0x5004, 0x0010, 1), ovUS(0x5004, 0x0103, 0),
		core.NewRawElement(core.NewTag(0x5004, 0x3000), core.VROW, curveUS(1)),
		// Valid 1-D curve.
		ovUS(0x5006, 0x0005, 1), ovUS(0x5006, 0x0010, 2), ovText(0x5006, 0x0020, core.VRCS, "TAC"), ovUS(0x5006, 0x0103, CurveValuesSL),
		core.NewRawElement(core.NewTag(0x5006, 0x3000), core.VROB, binary.LittleEndian.AppendUint32(binary.LittleEndian.AppendUint32(nil, 4), uint32(0xFFFFFFFF))),
	}, nil)
	curves, err := CurvesFromObject(obj)
	if !errors.Is(err, ErrInvalidCurve) {
		t.Fatalf("err %v", err)
	}
	if len(curves) != 1 || curves[0].Group != 0x5006 || curves[0].Points[1][0] != -1 {
		t.Fatalf("curves %+v", curves)
	}
}

func TestCurveDataBigEndian(t *testing.T) {
	obj := object.FromElements([]core.Element{
		core.NewRawElement(core.NewTag(0x5000, 0x0005), core.VRUS, binary.BigEndian.AppendUint16(nil, 1)),
		core.NewRawElement(core.NewTag(0x5000, 0x0010), core.VRUS, binary.BigEndian.AppendUint16(nil, 2)),
		core.NewRawElement(core.NewTag(0x5000, 0x0103), core.VRUS, binary.BigEndian.AppendUint16(nil, CurveValuesSS)),
		core.NewRawElement(core.NewTag(0x5000, 0x3000), core.VROW, binary.BigEndian.AppendUint16(binary.BigEndian.AppendUint16(nil, 300), uint16(0xFFFE))),
	}, nil)
	obj.SetValueByteOrder(binary.BigEndian)
	curves, err := CurvesFromObject(obj)
	if err != nil || len(curves) != 1 || curves[0].Points[0][0] != 300 || curves[0].Points[1][0] != -2 {
		t.Fatalf("curves %+v err %v", curves, err)
	}
}

// TestCurvesSurviveImplicitAndExplicitEncoding writes the POLY fixture in
// Implicit and Explicit VR Little Endian, reads it back, and finds the same
// vertices: Curve Data resolves to OB or OW by context. Big Endian values are
// covered by TestCurveDataBigEndian.
func TestCurvesSurviveImplicitAndExplicitEncoding(t *testing.T) {
	for _, syntax := range []transfer.Syntax{transfer.ImplicitVRLittleEndian, transfer.ExplicitVRLittleEndian} {
		var buf bytes.Buffer
		if err := object.WriteDataSet(&buf, polyCurveObject(), syntax); err != nil {
			t.Fatalf("%s write: %v", syntax.Name, err)
		}
		read, err := object.ReadDataSet(&buf, syntax)
		if err != nil {
			t.Fatalf("%s read: %v", syntax.Name, err)
		}
		curves, err := CurvesFromObject(read)
		if err != nil || len(curves) != 2 {
			t.Fatalf("%s: curves %+v err %v", syntax.Name, curves, err)
		}
		points, ok := curves[0].ImagePoints()
		if !ok || len(points) != 3 || points[2] != (CurvePoint{X: 5, Y: 4}) || curves[1].Points[2][1] != 10 {
			t.Fatalf("%s: points %v roi %v", syntax.Name, points, curves[1].Points)
		}
	}
}

// TestCurvesMatchGDCM checks the PHYSIO curves of the gdcmData XA files
// against GDCM TestCurve2.cxx: dimensions, point count, type, data value
// representation, and the MD5 of the float32 (x, y, z) triplets GDCM's
// Curve::GetAsPoints produces for a one-dimensional US curve (x = value).
func TestCurvesMatchGDCM(t *testing.T) {
	root := os.Getenv("DICOMGO_GDCMDATA_ROOT")
	if root == "" {
		t.Skip("set DICOMGO_GDCMDATA_ROOT to a separately obtained GDCMData checkout")
	}
	for name, want := range map[string]struct {
		points int
		md5    string
	}{
		"GE_DLX-8-MONO2-Multiframe-Jpeg_Lossless.dcm": {1126, "0fee912671ae158390efc7b49fe39f9b"},
		"GE_DLX-8-MONO2-Multiframe.dcm":               {969, "b46d2c6eed2944f1e16c46125022d2d4"},
	} {
		f, err := object.OpenFileWithOptions(filepath.Join(root, name), object.ReadFileOptions{RecoverDelimiterAnomalies: true})
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		curves, err := CurvesFromObject(f.Dataset)
		if err != nil || len(curves) != 1 {
			t.Fatalf("%s: curves %d err %v", name, len(curves), err)
		}
		curve := curves[0]
		if curve.Dimensions != 1 || len(curve.Points) != want.points || curve.TypeOfData != "PHYSIO" || curve.ValueRepresentation != CurveValuesUS || curve.IsImageGraphic() {
			t.Fatalf("%s: %+v", name, curve)
		}
		var triplets []byte
		for _, tuple := range curve.Points {
			triplets = binary.LittleEndian.AppendUint32(triplets, math.Float32bits(float32(tuple[0])))
			triplets = binary.LittleEndian.AppendUint32(triplets, 0)
			triplets = binary.LittleEndian.AppendUint32(triplets, 0)
		}
		sum := md5.Sum(triplets)
		if got := hex.EncodeToString(sum[:]); got != want.md5 {
			t.Errorf("%s: points MD5 %s, GDCM %s", name, got, want.md5)
		}
	}
}
