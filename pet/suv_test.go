package pet

import (
	"math"
	"testing"
	"time"

	"github.com/ThalesMMS/dicom-go/core"
	"github.com/ThalesMMS/dicom-go/dictionary/std"
	dicomtags "github.com/ThalesMMS/dicom-go/dictionary/tags"
	"github.com/ThalesMMS/dicom-go/object"
)

func TestMetadataSUVbwReferenceWithDecayToAcquisitionStart(t *testing.T) {
	// Reference phantom: 400 MBq at administration, one 3600 s half-life later
	// the activity is 200 MBq. A 2000 Bq/ml voxel in a 50 kg patient is:
	// 2000 * 50000 / 200000000 = 0.5 SUVbw.
	dataset := petSUVTestDataset(
		stringTestElement(dicomtags.AcquisitionDateTime, core.VRDT, "20260709110000-0300"),
		petIsotopeSequence(
			stringTestElement(dicomtags.RadiopharmaceuticalStartDateTime, core.VRDT, "20260709100000-0300"),
			stringTestElement(dicomtags.RadionuclideTotalDose, core.VRDS, "400000000"),
			stringTestElement(dicomtags.RadionuclideHalfLife, core.VRDS, "3600"),
		),
	)
	meta := ParseClassicMetadata(dataset)
	if meta == nil {
		t.Fatal("ParseClassicMetadata() = nil")
	}
	if !meta.AdministrationTimeOK || !meta.AcquisitionTimeOK {
		t.Fatalf("parsed times = administration %v acquisition %v, want both valid", meta.AdministrationTimeOK, meta.AcquisitionTimeOK)
	}
	if got, want := meta.AcquisitionTime.Sub(meta.AdministrationTime), time.Hour; got != want {
		t.Fatalf("acquisition-administration = %v, want %v", got, want)
	}
	got, ok := meta.SUVbw(2000)
	if !ok {
		t.Fatal("SUVbw() unavailable with complete reference metadata")
	}
	if math.Abs(got-0.5) > 1e-12 {
		t.Fatalf("SUVbw() = %.12f, want 0.5", got)
	}
}

func TestMetadataUsesAcquisitionDateForDeprecatedAdministrationTime(t *testing.T) {
	dataset := petSUVTestDataset(
		stringTestElement(dicomtags.AcquisitionDate, core.VRDA, "20260709"),
		stringTestElement(dicomtags.AcquisitionTime, core.VRTM, "110000"),
		petIsotopeSequence(
			stringTestElement(dicomtags.RadiopharmaceuticalStartTime, core.VRTM, "100000"),
			stringTestElement(dicomtags.RadionuclideTotalDose, core.VRDS, "400000000"),
			stringTestElement(dicomtags.RadionuclideHalfLife, core.VRDS, "3600"),
		),
	)
	meta := ParseClassicMetadata(dataset)
	if meta == nil || !meta.AdministrationTimeOK || !meta.AcquisitionTimeOK {
		t.Fatalf("fallback TM metadata = %#v, want complete times", meta)
	}
	if got := meta.AcquisitionTime.Sub(meta.AdministrationTime); got != time.Hour {
		t.Fatalf("fallback TM elapsed = %v, want 1h", got)
	}
}

func TestMetadataRejectsMixedKnownAndUnknownTimeOffsets(t *testing.T) {
	dataset := petSUVTestDataset(
		stringTestElement(dicomtags.AcquisitionDateTime, core.VRDT, "20260709110000-0300"),
		petIsotopeSequence(
			stringTestElement(dicomtags.RadiopharmaceuticalStartDateTime, core.VRDT, "20260709100000"),
			stringTestElement(dicomtags.RadionuclideTotalDose, core.VRDS, "400000000"),
			stringTestElement(dicomtags.RadionuclideHalfLife, core.VRDS, "3600"),
		),
	)
	meta := ParseClassicMetadata(dataset)
	if meta == nil || !meta.AcquisitionTimeOK {
		t.Fatalf("acquisition metadata = %#v, want known acquisition time", meta)
	}
	if meta.AdministrationTimeOK {
		t.Fatal("administration time without an offset was accepted against an explicitly offset acquisition time")
	}
	if got, ok := meta.SUVbw(2000); ok {
		t.Fatalf("SUVbw() = %v, true for ambiguous time bases", got)
	}
}

func TestMetadataSUVbwFailClosed(t *testing.T) {
	base := Metadata{
		Units:                petUnitsBQML,
		DecayCorrection:      petDecayCorrectionStart,
		CorrectedImage:       []string{"ATTN", petCorrectedImageDecay},
		PatientWeightKg:      50,
		TotalDoseBq:          400_000_000,
		HalfLifeSeconds:      3600,
		AdministrationTime:   time.Date(2026, 7, 9, 10, 0, 0, 0, time.UTC),
		AdministrationTimeOK: true,
		AcquisitionTime:      time.Date(2026, 7, 9, 11, 0, 0, 0, time.UTC),
		AcquisitionTimeOK:    true,
	}
	tests := []struct {
		name   string
		mutate func(*Metadata)
	}{
		{name: "missing weight", mutate: func(m *Metadata) { m.PatientWeightKg = 0 }},
		{name: "missing dose", mutate: func(m *Metadata) { m.TotalDoseBq = 0 }},
		{name: "missing half life", mutate: func(m *Metadata) { m.HalfLifeSeconds = 0 }},
		{name: "missing administration time", mutate: func(m *Metadata) { m.AdministrationTimeOK = false }},
		{name: "missing acquisition time", mutate: func(m *Metadata) { m.AcquisitionTimeOK = false }},
		{name: "negative elapsed", mutate: func(m *Metadata) { m.AcquisitionTime = m.AdministrationTime.Add(-time.Minute) }},
		{name: "not decay corrected", mutate: func(m *Metadata) { m.CorrectedImage = []string{"ATTN"} }},
		{name: "ambiguous decay reference", mutate: func(m *Metadata) { m.DecayCorrection = "NONE" }},
		{name: "unknown units", mutate: func(m *Metadata) { m.Units = "CNTS" }},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			meta := base
			meta.CorrectedImage = append([]string(nil), base.CorrectedImage...)
			tt.mutate(&meta)
			if got, ok := meta.SUVbw(2000); ok {
				t.Fatalf("SUVbw() = %v, true; want unavailable", got)
			}
		})
	}
}

func TestMetadataSUVbwDirectGMLRequiresBodyWeightType(t *testing.T) {
	for _, tt := range []struct {
		suvType string
		wantOK  bool
	}{
		{suvType: "", wantOK: true},
		{suvType: "BW", wantOK: true},
		{suvType: "LBM", wantOK: false},
	} {
		meta := Metadata{Units: petUnitsGML, SUVType: tt.suvType}
		got, ok := meta.SUVbw(3.25)
		if ok != tt.wantOK {
			t.Fatalf("SUVbw(GML, type %q) ok = %v, want %v", tt.suvType, ok, tt.wantOK)
		}
		if ok && got != 3.25 {
			t.Fatalf("SUVbw(GML, type %q) = %v, want stored SUV 3.25", tt.suvType, got)
		}
	}
}

func petSUVTestDataset(extra ...core.Element) *object.Object {
	correctedImage := stringTestElement(dicomtags.CorrectedImage, core.VRCS, "")
	correctedImage.Value = core.StringValue{"ATTN", petCorrectedImageDecay}
	elements := []core.Element{
		stringTestElement(dicomtags.Modality, core.VRCS, "PT"),
		stringTestElement(dicomtags.Units, core.VRCS, petUnitsBQML),
		stringTestElement(dicomtags.DecayCorrection, core.VRCS, petDecayCorrectionStart),
		correctedImage,
		stringTestElement(dicomtags.PatientWeight, core.VRDS, "50"),
	}
	elements = append(elements, extra...)
	return object.FromElements(elements, std.Dictionary)
}

func petIsotopeSequence(elements ...core.Element) core.Element {
	return core.Element{
		Header: core.ElementHeader{Tag: dicomtags.RadiopharmaceuticalInformationSequence, VR: core.VRSQ},
		Value:  core.SequenceValue{Items: []core.DataSet{{Elements: elements}}},
	}
}

func stringTestElement(tag core.Tag, vr core.VR, value string) core.Element {
	return core.Element{
		Header: core.ElementHeader{Tag: tag, VR: vr},
		Value:  core.StringValue{value},
	}
}

func TestParseClassicMetadataSUVbwAtAdministration(t *testing.T) {
	dataset := petSUVTestDataset(petIsotopeSequence(
		stringTestElement(dicomtags.RadionuclideTotalDose, core.VRDS, "400000000"),
	))
	dataset.Put(stringTestElement(dicomtags.DecayCorrection, core.VRCS, "ADMIN"))
	meta := ParseClassicMetadata(dataset)
	if meta == nil || meta.AdministrationTimeOK || meta.AcquisitionTimeOK {
		t.Fatalf("expected calibration without timestamps, got %#v", meta)
	}
	if got, ok := meta.SUVbw(2000); !ok || got != 0.25 {
		t.Fatalf("SUVbw(2000) = (%v, %v), want (0.25, true)", got, ok)
	}
	for _, input := range []float64{math.NaN(), math.Inf(1), math.Inf(-1), -1, math.MaxFloat64} {
		if got, ok := meta.SUVbw(input); ok {
			t.Errorf("SUVbw(%v) = %v, want unavailable", input, got)
		}
	}
}

func TestParseClassicMetadataRejectsUnusableDose(t *testing.T) {
	dose := stringTestElement(dicomtags.RadionuclideTotalDose, core.VRDS, "400000000")
	ambiguousDose := dose
	ambiguousDose.Value = core.StringValue{"400000000", "800000000"}
	multipleIsotopes := petIsotopeSequence(dose)
	multipleIsotopes.Value = core.SequenceValue{Items: []core.DataSet{
		{Elements: []core.Element{dose}},
		{Elements: []core.Element{dose}},
	}}
	for _, tt := range []struct {
		name   string
		extras []core.Element
	}{
		{name: "missing item"},
		{name: "multiple items", extras: []core.Element{multipleIsotopes}},
		{name: "multiple dose values", extras: []core.Element{petIsotopeSequence(ambiguousDose)}},
		{name: "infinite dose", extras: []core.Element{petIsotopeSequence(stringTestElement(dicomtags.RadionuclideTotalDose, core.VRDS, "Inf"))}},
		{name: "negative dose", extras: []core.Element{petIsotopeSequence(stringTestElement(dicomtags.RadionuclideTotalDose, core.VRDS, "-1"))}},
	} {
		t.Run(tt.name, func(t *testing.T) {
			dataset := petSUVTestDataset(tt.extras...)
			dataset.Put(stringTestElement(dicomtags.DecayCorrection, core.VRCS, "ADMIN"))
			meta := ParseClassicMetadata(dataset)
			if got, ok := meta.SUVbw(2000); meta == nil || ok {
				t.Fatalf("metadata %v SUVbw = (%v, %v), want present metadata with unavailable SUVbw", meta, got, ok)
			}
		})
	}
}

func TestParseClassicMetadataAppliesDatasetTimezoneAcrossDates(t *testing.T) {
	dataset := petSUVTestDataset(
		stringTestElement(dicomtags.TimezoneOffsetFromUTC, core.VRSH, "-0300"),
		stringTestElement(dicomtags.AcquisitionDate, core.VRDA, "20260710"),
		stringTestElement(dicomtags.AcquisitionTime, core.VRTM, "0030"),
		petIsotopeSequence(
			stringTestElement(dicomtags.RadiopharmaceuticalStartDateTime, core.VRDT, "202607100230+0000"),
			stringTestElement(dicomtags.RadionuclideTotalDose, core.VRDS, "400000000"),
			stringTestElement(dicomtags.RadionuclideHalfLife, core.VRDS, "3600"),
		),
	)
	meta := ParseClassicMetadata(dataset)
	if meta == nil || !meta.AdministrationTimeOK || !meta.AcquisitionTimeOK {
		t.Fatalf("calibration = %#v, want known compatible time bases", meta)
	}
	if got, ok := meta.SUVbw(2000); !ok || math.Abs(got-0.5) > 1e-12 {
		t.Fatalf("SUVbw(2000) = (%v, %v), want (0.5, true)", got, ok)
	}
}
