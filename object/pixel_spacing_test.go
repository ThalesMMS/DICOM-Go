package object

import (
	"math"
	"testing"

	"github.com/ThalesMMS/dicom-go/core"
)

func spacingTestElement(tag core.Tag, vr core.VR, values ...string) core.Element {
	return core.Element{Header: core.ElementHeader{Tag: tag, VR: vr}, Value: core.StringValue(values)}
}

func spacingGroup(tag core.Tag, items ...core.DataSet) core.Element {
	return core.Element{Header: core.ElementHeader{Tag: tag, VR: core.VRSQ}, Value: core.SequenceValue{Items: items}}
}

func macroItem(macro, tag core.Tag, values ...string) core.DataSet {
	return core.DataSet{Elements: []core.Element{spacingGroup(macro, core.DataSet{Elements: []core.Element{
		spacingTestElement(tag, core.VRDS, values...)}})}}
}

func TestPixelSpacingAtByModality(t *testing.T) {
	modality := func(m string) core.Element { return spacingTestElement(tagModality, core.VRCS, m) }
	sop := func(uid string) core.Element { return spacingTestElement(tagSOPClassUIDSpacing, core.VRUI, uid) }
	ps := func(v ...string) core.Element { return spacingTestElement(tagPixelSpacing, core.VRDS, v...) }
	imager := func(v ...string) core.Element { return spacingTestElement(tagImagerPixelSpacing, core.VRDS, v...) }
	calibration := spacingTestElement(tagPixelSpacingCalibrationType, core.VRCS, "GEOMETRY")

	cases := []struct {
		name     string
		elements []core.Element
		frame    int
		semantic PixelSpacingSemantic
		tag      core.Tag
		source   FunctionalGroupSource
		row, col float64
	}{
		{"CT", []core.Element{modality("CT"), ps("0.7", "0.8")}, 0, SpacingPatient, tagPixelSpacing, FunctionalGroupNone, 0.7, 0.8},
		{"MR", []core.Element{modality("MR"), ps("1", "1")}, 0, SpacingPatient, tagPixelSpacing, FunctionalGroupNone, 1, 1},
		{"Enhanced MR per-frame", []core.Element{modality("MR"), ps("9", "9"),
			spacingGroup(tagSharedFunctionalGroups, macroItem(tagPixelMeasuresSequence, tagPixelSpacing, "0.5", "0.5")),
			spacingGroup(tagPerFrameFunctionalGroups, core.DataSet{}, macroItem(tagPixelMeasuresSequence, tagPixelSpacing, "0.25", "0.3"))},
			1, SpacingPatient, tagPixelSpacing, FunctionalGroupPerFrame, 0.25, 0.3},
		{"Enhanced MR shared", []core.Element{modality("MR"),
			spacingGroup(tagSharedFunctionalGroups, macroItem(tagPixelMeasuresSequence, tagPixelSpacing, "0.5", "0.5"))},
			0, SpacingPatient, tagPixelSpacing, FunctionalGroupShared, 0.5, 0.5},
		{"CR detector", []core.Element{modality("CR"), imager("0.1", "0.1")}, 0, SpacingDetector, tagImagerPixelSpacing, FunctionalGroupNone, 0.1, 0.1},
		{"DX copy of imager", []core.Element{modality("DX"), ps("0.143", "0.143"), imager("0.143", "0.143")}, 0, SpacingDetector, tagPixelSpacing, FunctionalGroupNone, 0.143, 0.143},
		{"DX corrected, no calibration type", []core.Element{modality("DX"), ps("0.13", "0.13"), imager("0.143", "0.143")}, 0, SpacingUnknown, tagPixelSpacing, FunctionalGroupNone, 0.13, 0.13},
		{"DX calibrated", []core.Element{modality("DX"), ps("0.13", "0.13"), imager("0.143", "0.143"), calibration}, 0, SpacingCalibrated, tagPixelSpacing, FunctionalGroupNone, 0.13, 0.13},
		{"MG detector", []core.Element{modality("MG"), imager("0.07", "0.07")}, 0, SpacingDetector, tagImagerPixelSpacing, FunctionalGroupNone, 0.07, 0.07},
		{"XA detector", []core.Element{modality("XA"), imager("0.2", "0.2")}, 0, SpacingDetector, tagImagerPixelSpacing, FunctionalGroupNone, 0.2, 0.2},
		{"RF detector", []core.Element{modality("RF"), imager("0.3", "0.3")}, 0, SpacingDetector, tagImagerPixelSpacing, FunctionalGroupNone, 0.3, 0.3},
		{"IO unknown", []core.Element{modality("IO"), ps("0.02", "0.02")}, 0, SpacingUnknown, tagPixelSpacing, FunctionalGroupNone, 0.02, 0.02},
		{"Enhanced XA", []core.Element{sop("1.2.840.10008.5.1.4.1.1.12.1.1"), modality("XA"),
			spacingGroup(tagSharedFunctionalGroups, macroItem(tagXRayFramePixelDataPropertiesSeq, tagImagerPixelSpacing, "0.15", "0.15"))},
			0, SpacingDetector, tagImagerPixelSpacing, FunctionalGroupShared, 0.15, 0.15},
		{"RT Image", []core.Element{sop("1.2.840.10008.5.1.4.1.1.481.1"), modality("RTIMAGE"),
			spacingTestElement(tagRTImagePlanePixelSpacing, core.VRDS, "0.4", "0.4")}, 0, SpacingRTImagePlane, tagRTImagePlanePixelSpacing, FunctionalGroupNone, 0.4, 0.4},
		{"SC scanned", []core.Element{sop("1.2.840.10008.5.1.4.1.1.7"), modality("OT"),
			spacingTestElement(tagNominalScannedPixelSpacing, core.VRDS, "0.05", "0.05")}, 0, SpacingScanned, tagNominalScannedPixelSpacing, FunctionalGroupNone, 0.05, 0.05},
		{"SC calibrated", []core.Element{sop("1.2.840.10008.5.1.4.1.1.7"), ps("0.1", "0.1"), calibration,
			spacingTestElement(tagNominalScannedPixelSpacing, core.VRDS, "0.05", "0.05")}, 0, SpacingCalibrated, tagPixelSpacing, FunctionalGroupNone, 0.1, 0.1},
		{"SC plain", []core.Element{sop("1.2.840.10008.5.1.4.1.1.7"), ps("0.1", "0.1")}, 0, SpacingUnknown, tagPixelSpacing, FunctionalGroupNone, 0.1, 0.1},
		{"US", []core.Element{modality("US"), ps("0.1", "0.1")}, 0, SpacingUnknown, tagPixelSpacing, FunctionalGroupNone, 0.1, 0.1},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := FromElements(c.elements, nil).PixelSpacingAt(c.frame)
			if got.Semantic != c.semantic || got.Tag != c.tag || got.Source != c.source || got.RowSpacing != c.row || got.ColumnSpacing != c.col || !got.Valid() {
				t.Fatalf("got %+v", got)
			}
		})
	}
	if got := FromElements([]core.Element{modality("CR")}, nil).PixelSpacingAt(0); got.Valid() {
		t.Fatalf("no spacing resolved as %+v", got)
	}
}

func TestEstimatedPatientSpacingIsExplicit(t *testing.T) {
	obj := FromElements([]core.Element{
		spacingTestElement(tagModality, core.VRCS, "MG"),
		spacingTestElement(tagImagerPixelSpacing, core.VRDS, "0.1", "0.1"),
		spacingTestElement(tagEstimatedMagnificationFactor, core.VRDS, "1.25"),
	}, nil)
	got := obj.PixelSpacingAt(0)
	if got.Semantic != SpacingDetector || got.RowSpacing != 0.1 || got.MagnificationFactor != 1.25 {
		t.Fatalf("detector spacing corrected silently: %+v", got)
	}
	row, col, ok := got.EstimatedPatientSpacing()
	if !ok || math.Abs(row-0.08) > 1e-12 || math.Abs(col-0.08) > 1e-12 {
		t.Fatalf("estimate %v %v %v", row, col, ok)
	}
	calibrated := PixelSpacingResolution{Semantic: SpacingCalibrated, RowSpacing: 1, ColumnSpacing: 1, MagnificationFactor: 2}
	if _, _, ok := calibrated.EstimatedPatientSpacing(); ok {
		t.Fatal("calibrated spacing estimated again")
	}
}
