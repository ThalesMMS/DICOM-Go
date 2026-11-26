package rtdose

import (
	"bytes"
	"errors"
	"reflect"
	"strings"
	"testing"

	"github.com/ThalesMMS/dicom-go/core"
	"github.com/ThalesMMS/dicom-go/internal/derivedio"
	"github.com/ThalesMMS/dicom-go/object"
	"github.com/ThalesMMS/dicom-go/rtstruct"
)

func TestReadEmbeddedDVHsWithoutDoseGridKeepsValidSiblings(t *testing.T) {
	good := testEmbeddedDVHItem(DoseUnitsRelative, DoseTypeEffective, 2, []float64{1, 80, 2, 40})
	good = derivedio.DataSet(append(good.Elements,
		derivedio.DS(tagDVHMinimumDose),
		derivedio.DS(tagDVHMaximumDose),
		derivedio.DS(tagDVHMeanDose),
	)...)
	wrongCount := testEmbeddedDVHItem(DoseUnitsGray, DoseTypePhysical, 1, []float64{1, 80, 2})
	wrongVolume := testEmbeddedDVHItem(DoseUnitsGray, DoseTypePhysical, 1, []float64{1, -1})
	wrongUnits := testEmbeddedDVHItem("OTHER", DoseTypePhysical, 1, []float64{1, 40})
	missingType := testEmbeddedDVHItem(DoseUnitsGray, "", 1, []float64{1, 40})
	nonFinite := testEmbeddedDVHItem(DoseUnitsGray, DoseTypePhysical, 1, []float64{1, 40})
	for i, element := range nonFinite.Elements {
		if element.Tag() == tagDVHData {
			nonFinite.Elements[i] = core.NewRawElement(tagDVHData, core.VRDS, []byte("Inf\\1 "))
		}
	}
	invalidStatistic := testEmbeddedDVHItem(DoseUnitsGray, DoseTypePhysical, 1, []float64{1, 40})
	invalidStatistic = derivedio.DataSet(append(invalidStatistic.Elements,
		core.NewRawElement(tagDVHMeanDose, core.VRDS, []byte("NaN ")))...)
	ambiguousUnits := testEmbeddedDVHItem("GY\\RELATIVE", DoseTypePhysical, 1, []float64{1, 40})
	overBudget := testEmbeddedDVHItem(DoseUnitsGray, DoseTypePhysical, maxDVHBinsPerItem+1, []float64{1, 40})
	last := testEmbeddedDVHItem(DoseUnitsGray, DoseTypePhysical, 1, []float64{2, 5})
	set := testDVHStructureSet(rtstruct.RTStructureSetStorage, "1.2.333.44")
	file := readTestEmbeddedDVHFile(t, set, derivedio.Seq(tagDVHSequence,
		good, wrongCount, wrongVolume, wrongUnits, missingType, nonFinite, invalidStatistic, ambiguousUnits, overBudget, last))
	if file.Dataset.Has(derivedio.TagPixelData) {
		t.Fatal("DVH-only fixture contains Pixel Data")
	}
	report, err := ReadEmbeddedDVHs(file.Dataset)
	if err != nil {
		t.Fatal(err)
	}
	wantRef := Reference{SOPClassUID: rtstruct.RTStructureSetStorage, SOPInstanceUID: "1.2.333.44"}
	if report.StructureSet != wantRef || len(report.Histograms) != 2 {
		t.Fatalf("report = %+v", report)
	}
	wantDiagnostics := []DVHItemDiagnostic{
		{Item: 2, Tag: tagDVHData}, {Item: 3, Tag: tagDVHData}, {Item: 4, Tag: tagDoseUnits},
		{Item: 5, Tag: tagDoseType}, {Item: 6, Tag: tagDVHData}, {Item: 7, Tag: tagDVHMeanDose},
		{Item: 8, Tag: tagDoseUnits}, {Item: 9, Tag: tagDVHNumberOfBins},
	}
	if !reflect.DeepEqual(report.Diagnostics, wantDiagnostics) {
		t.Fatalf("diagnostics = %+v, want %+v", report.Diagnostics, wantDiagnostics)
	}
	first := report.Histograms[0]
	wantROIs := []DVHROIContribution{{Number: 7, Type: "INCLUDED"}, {Number: 8, Type: "EXCLUDED"}}
	if first.DoseUnits != DoseUnitsRelative || first.DoseType != DoseTypeEffective ||
		first.DoseScaling != 0.02 || first.NumberOfBins != 2 ||
		!reflect.DeepEqual(first.Data, []float64{1, 80, 2, 40}) ||
		!reflect.DeepEqual(first.ROIContributions, wantROIs) ||
		!reflect.DeepEqual(report.Histograms[1].Data, []float64{2, 5}) {
		t.Fatalf("histograms = %+v", report.Histograms)
	}
	if grid, err := Read(file); grid != nil || !errors.Is(err, ErrInvalidObject) {
		t.Fatalf("strict grid read = %+v, %v", grid, err)
	}
}

func TestReadEmbeddedDVHsRejectsInvalidModuleReferences(t *testing.T) {
	sequence := derivedio.Seq(tagDVHSequence,
		testEmbeddedDVHItem(DoseUnitsGray, DoseTypePhysical, 1, []float64{1, 2}))
	goodRef := testDVHStructureSet(rtstruct.RTStructureSetStorage, "1.2.3")
	for _, test := range []struct {
		name     string
		elements []core.Element
	}{
		{name: "no sequence", elements: []core.Element{goodRef}},
		{name: "no structure set", elements: []core.Element{sequence}},
		{name: "wrong structure set class", elements: []core.Element{testDVHStructureSet(RTDoseStorage, "1.2.3"), sequence}},
		{name: "empty structure set identity", elements: []core.Element{testDVHStructureSet(rtstruct.RTStructureSetStorage, ""), sequence}},
		{name: "ambiguous structure set identity", elements: []core.Element{testDVHStructureSet(rtstruct.RTStructureSetStorage, "1.2.3\\1.2.4"), sequence}},
		{name: "empty sequence", elements: []core.Element{goodRef, derivedio.Seq(tagDVHSequence)}},
	} {
		t.Run(test.name, func(t *testing.T) {
			file := readTestEmbeddedDVHFile(t, test.elements...)
			report, err := ReadEmbeddedDVHs(file.Dataset)
			if !errors.Is(err, ErrInvalidObject) || len(report.Histograms) > 0 || report.StructureSet != (Reference{}) {
				t.Fatalf("report = %+v, err = %v", report, err)
			}
		})
	}
}

func TestReadEmbeddedDVHsSupportsLongUNPairs(t *testing.T) {
	const bins = 16500
	item := testEmbeddedDVHItem(DoseUnitsGray, DoseTypePhysical, bins, []float64{1, 2})
	malformed := testEmbeddedDVHItem(DoseUnitsGray, DoseTypePhysical, bins, []float64{1, 2})
	for i, element := range item.Elements {
		if element.Tag() == tagDVHData {
			raw := strings.Repeat("1\\2\\", bins-1) + "1\\2 "
			item.Elements[i] = core.NewRawElement(tagDVHData, core.VRUN, []byte(raw))
			malformed.Elements[i] = core.NewRawElement(tagDVHData, core.VRUN, []byte(strings.Repeat("\\", len(raw))))
		}
	}
	file := readTestEmbeddedDVHFile(t, testDVHStructureSet(rtstruct.RTStructureSetStorage, "1.2.3"),
		derivedio.Seq(tagDVHSequence, malformed, item))
	report, err := ReadEmbeddedDVHs(file.Dataset)
	if err != nil || !reflect.DeepEqual(report.Diagnostics, []DVHItemDiagnostic{{Item: 1, Tag: tagDVHData}}) || len(report.Histograms) != 1 {
		t.Fatalf("histograms = %d, diagnostics = %+v, err = %v", len(report.Histograms), report.Diagnostics, err)
	}
	got := report.Histograms[0]
	if got.NumberOfBins != bins || len(got.Data) != bins*2 || got.Data[len(got.Data)-1] != 2 {
		t.Fatalf("histogram bins = %d, values = %d", got.NumberOfBins, len(got.Data))
	}
}

func testEmbeddedDVHItem(units, doseType string, bins int, values []float64) core.DataSet {
	return derivedio.DataSet(
		derivedio.CS(tagDVHType, "CUMULATIVE"),
		derivedio.CS(tagDoseUnits, units),
		derivedio.CS(tagDoseType, doseType),
		derivedio.CS(tagDVHVolumeUnits, "PERCENT"),
		derivedio.IS(tagDVHNumberOfBins, bins),
		derivedio.DS(tagDVHDoseScaling, 0.02),
		derivedio.DS(tagDVHData, values...),
		derivedio.Seq(tagDVHReferencedROISequence,
			derivedio.DataSet(derivedio.IS(tagReferencedROINumber, 7), derivedio.CS(tagDVHROIContributionType, "INCLUDED")),
			derivedio.DataSet(derivedio.IS(tagReferencedROINumber, 8), derivedio.CS(tagDVHROIContributionType, "EXCLUDED")),
		),
	)
}

func testDVHStructureSet(class, instance string) core.Element {
	return derivedio.Seq(tagReferencedStructureSetSeq, derivedio.DataSet(
		derivedio.UI(derivedio.TagRefSOPClassUID, class),
		derivedio.UI(derivedio.TagRefSOPInstanceUID, instance),
	))
}

func readTestEmbeddedDVHFile(t *testing.T, elements ...core.Element) *object.File {
	t.Helper()
	const uid = "1.2.333.1"
	obj := derivedio.Object(append([]core.Element{
		derivedio.UI(derivedio.TagSOPClassUID, RTDoseStorage),
		derivedio.UI(derivedio.TagSOPInstanceUID, uid),
		derivedio.UI(derivedio.TagStudyInstanceUID, "1.2.333.2"),
		derivedio.UI(derivedio.TagSeriesInstanceUID, "1.2.333.3"),
		derivedio.UI(derivedio.TagFrameOfReferenceUID, "1.2.333.4"),
		derivedio.CS(derivedio.TagModality, "RTDOSE"),
		derivedio.CS(tagDoseUnits, DoseUnitsGray),
		derivedio.CS(tagDoseType, DoseTypePhysical),
		derivedio.CS(tagDoseSummationType, "PLAN"),
	}, elements...)...)
	file, err := derivedio.File(RTDoseStorage, uid, obj)
	if err != nil {
		t.Fatal(err)
	}
	var encoded bytes.Buffer
	if err := object.WriteFile(&encoded, file); err != nil {
		t.Fatal(err)
	}
	read, err := object.ReadFile(bytes.NewReader(encoded.Bytes()))
	if err != nil {
		t.Fatal(err)
	}
	return read
}
