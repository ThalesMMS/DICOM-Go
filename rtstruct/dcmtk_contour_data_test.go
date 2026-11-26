package rtstruct

import (
	"bytes"
	"reflect"
	"testing"

	"github.com/ThalesMMS/dicom-go/core"
	"github.com/ThalesMMS/dicom-go/internal/derivedio"
	"github.com/ThalesMMS/dicom-go/object"
)

// DCMTK dcmrt/tests/tcontour.cc checks the order and size of a nested Contour
// Data vector. Exercise decimal decoding and ROI-number binding across two
// independently ordered sequences through Part 10 and the RTSTRUCT writer.
func TestDCMTKReferenceRTStructureSetContourData(t *testing.T) {
	const (
		instanceUID = "2.25.126900000000000000000000000005001"
		studyUID    = "2.25.126900000000000000000000000005002"
		seriesUID   = "2.25.126900000000000000000000000005003"
		frameUID    = "2.25.126900000000000000000000000005004"
	)
	dataset := derivedio.Object(
		derivedio.UI(derivedio.TagSOPClassUID, RTStructureSetStorage),
		derivedio.UI(derivedio.TagSOPInstanceUID, instanceUID),
		derivedio.UI(derivedio.TagStudyInstanceUID, studyUID),
		derivedio.UI(derivedio.TagSeriesInstanceUID, seriesUID),
		derivedio.Seq(tagReferencedFrameOfReferenceSeq, derivedio.DataSet(
			derivedio.UI(derivedio.TagFrameOfReferenceUID, frameUID),
		)),
		derivedio.Seq(tagStructureSetROISequence,
			derivedio.DataSet(derivedio.IS(tagROINumber, 14), derivedio.LO(tagROIName, "Trace")),
			derivedio.DataSet(derivedio.IS(tagROINumber, 7), derivedio.LO(tagROIName, "Landmark")),
		),
		derivedio.Seq(tagROIContourSequence,
			derivedio.DataSet(
				derivedio.IS(tagReferencedROINumber, 7),
				derivedio.IS(tagROIDisplayColor, 20, 40, 60),
				derivedio.Seq(tagContourSequence, derivedio.DataSet(
					derivedio.CS(tagContourGeometricType, ContourPoint),
					derivedio.IS(tagNumberOfContourPoints, 1),
					derivedio.Strings(tagContourData, core.VRDS, []string{"-24.125", "8.5", "+15"}),
				)),
			),
			derivedio.DataSet(
				derivedio.IS(tagReferencedROINumber, 14),
				derivedio.IS(tagROIDisplayColor, 4, 2, 1),
				derivedio.Seq(tagContourSequence,
					derivedio.DataSet(
						derivedio.CS(tagContourGeometricType, ContourOpenNonPlanar),
						derivedio.IS(tagNumberOfContourPoints, 2),
						derivedio.Strings(tagContourData, core.VRDS, []string{"1.25E+2", "-30.5", ".125", "-14.5", "5e-1", "27.75"}),
					),
					derivedio.DataSet(
						derivedio.CS(tagContourGeometricType, ContourOpenPlanar),
						derivedio.IS(tagNumberOfContourPoints, 2),
						derivedio.Strings(tagContourData, core.VRDS, []string{"3", "-7.75", "0", "64", "120.25", "0"}),
					),
				),
			),
		),
	)
	input, err := derivedio.File(RTStructureSetStorage, instanceUID, dataset)
	if err != nil {
		t.Fatal(err)
	}
	want := []ROI{
		{Number: 7, Name: "Landmark", ColorRGB: [3]int{20, 40, 60}, Contours: []Contour{
			{GeometricType: ContourPoint, Points: []Point3D{{X: -24.125, Y: 8.5, Z: 15}}},
		}},
		{Number: 14, Name: "Trace", ColorRGB: [3]int{4, 2, 1}, Contours: []Contour{
			{GeometricType: ContourOpenNonPlanar, Points: []Point3D{{X: 125, Y: -30.5, Z: 0.125}, {X: -14.5, Y: 0.5, Z: 27.75}}},
			{GeometricType: ContourOpenPlanar, Points: []Point3D{{X: 3, Y: -7.75, Z: 0}, {X: 64, Y: 120.25, Z: 0}}},
		}},
	}
	readPart10 := func(t *testing.T, file *object.File) *StructureSet {
		t.Helper()
		var buf bytes.Buffer
		if err := object.WriteFile(&buf, file); err != nil {
			t.Fatal(err)
		}
		parsed, err := object.ReadFile(bytes.NewReader(buf.Bytes()))
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() {
			if err := parsed.Close(); err != nil {
				t.Error(err)
			}
		})
		set, err := Read(parsed.Dataset)
		if err != nil {
			t.Fatal(err)
		}
		if set.FrameOfReferenceUID != frameUID || !reflect.DeepEqual(set.ROIs, want) {
			t.Fatalf("frame = %q, ROIs = %#v, want %q and %#v", set.FrameOfReferenceUID, set.ROIs, frameUID, want)
		}
		return set
	}
	set := readPart10(t, input)
	output, err := Write(set)
	if err != nil {
		t.Fatal(err)
	}
	readPart10(t, output)
}
