package encapimage

import (
	"context"
	"github.com/ThalesMMS/dicom-go/core"
	"github.com/ThalesMMS/dicom-go/dictionary/std"
	"github.com/ThalesMMS/dicom-go/object"
	"testing"
)

func TestFromImagesMultiFrameIODFamiliesAndFrameIncrement(t *testing.T) {
	cases := []struct {
		data  []byte
		class string
	}{
		{[]byte("P5 2 1 255\n\x00\xff"), MultiFrameGrayscaleByteSC},
		{[]byte("P5 2 1 65535\n\x00\x00\xff\xff"), MultiFrameGrayscaleWordSC},
		{[]byte("P6 1 1 255\n\x01\x02\x03"), MultiFrameTrueColorSC},
	}
	for _, tc := range cases {
		f, r := imported(t, [][]byte{tc.data, tc.data}, ImageOptions{})
		if r.Frames != 2 || getString(f.Dataset, tagSOPClassUID) != tc.class || getString(f.Dataset, core.NewTag(0x0028, 0x0008)) != "2" {
			t.Fatalf("MF: %+v", r)
		}
		e, ok := f.Dataset.Get(core.NewTag(0x0028, 0x0009))
		if !ok || len(e.Value.(core.TagValue)) != 1 || e.Value.(core.TagValue)[0] != core.NewTag(0x0018, 0x2001) {
			t.Fatal("Frame Increment Pointer")
		}
		part10Roundtrip(t, f)
	}
}
func TestFromImagesVLPhotographicAndUnsupportedMultiLayouts(t *testing.T) {
	f, _ := imported(t, [][]byte{grayInput(t)}, ImageOptions{JPEGOptions: JPEGOptions{SOPClassUID: VLPhotographicImageStorage}})
	if getString(f.Dataset, tagSOPClassUID) != VLPhotographicImageStorage {
		t.Fatal("wrong VLP class")
	}
	for _, o := range []ImageOptions{
		{JPEGOptions: JPEGOptions{SOPClassUID: SecondaryCaptureImageStorage}},
		{JPEGOptions: JPEGOptions{SOPClassUID: VLPhotographicImageStorage}},
	} {
		if f, _, e := FromImages(context.Background(), [][]byte{grayInput(t), grayInput(t)}, o); e == nil || f != nil {
			t.Fatal("single-frame IOD accepted multi-frame")
		}
	}
}
func TestFromImagesSeriesInheritanceNewSOPAndExplicitPrecedence(t *testing.T) {
	source := object.FromElements([]core.Element{
		stringElement(tagSOPInstanceUID, core.VRUI, "1.2.3.4"), stringElement(tagStudyInstanceUID, core.VRUI, "1.2.3"), stringElement(tagSeriesInstanceUID, core.VRUI, "1.2.3.5"), stringElement(tagPatientName, core.VRPN, "Synthetic^Patient"), stringElement(tagSeriesNumber, core.VRIS, "7"), stringElement(tagSeriesDescription, core.VRLO, "Original series"), stringElement(tagModality, core.VRCS, "OT"),
	}, std.Dictionary)
	f, r := imported(t, [][]byte{grayInput(t)}, ImageOptions{SeriesSource: source, JPEGOptions: JPEGOptions{SeriesDescription: "Explicit series"}})
	for tag, want := range map[core.Tag]string{tagStudyInstanceUID: "1.2.3", tagSeriesInstanceUID: "1.2.3.5", tagPatientName: "Synthetic^Patient", tagSeriesDescription: "Explicit series", tagSeriesNumber: "7"} {
		if getString(f.Dataset, tag) != want {
			t.Fatalf("%s inheritance", tag)
		}
	}
	if getString(f.Dataset, tagSOPInstanceUID) == "1.2.3.4" || len(r.GeneratedUIDs) != 1 || r.GeneratedUIDs[0] != "SOPInstanceUID" {
		t.Fatalf("identity/report: %+v", r)
	}
	if getString(source, tagSeriesDescription) != "Original series" {
		t.Fatal("source mutated")
	}
	if f, _, e := FromImages(context.Background(), [][]byte{grayInput(t)}, ImageOptions{SeriesSource: source, JPEGOptions: JPEGOptions{StudyInstanceUID: "1.2.9"}}); e == nil || f != nil {
		t.Fatal("inherited series with mismatching study accepted")
	}
}

func TestFromImagesSourceSOPMustNotBeReused(t *testing.T) {
	source := object.FromElements([]core.Element{stringElement(tagSOPInstanceUID, core.VRUI, "1.2.3.4"), stringElement(tagStudyInstanceUID, core.VRUI, "1.2.3"), stringElement(tagSeriesInstanceUID, core.VRUI, "1.2.3.5")}, std.Dictionary)
	if f, _, e := FromImages(context.Background(), [][]byte{grayInput(t)}, ImageOptions{SeriesSource: source, JPEGOptions: JPEGOptions{SOPInstanceUID: "1.2.3.4"}}); e == nil || f != nil {
		t.Fatal("source SOP reused")
	}
}

func TestFromImagesStudySourceCharsetConversionAndExplicitUTF8(t *testing.T) {
	source := object.FromElements([]core.Element{stringElement(tagSpecificCharacterSet, core.VRCS, "ISO_IR 100"), core.NewRawElement(tagPatientName, core.VRPN, []byte{'A', 'n', 'd', 'r', 0xe9, ' '}), stringElement(tagStudyInstanceUID, core.VRUI, "1.2.3")}, std.Dictionary)
	f, _, e := FromImages(context.Background(), [][]byte{grayInput(t)}, ImageOptions{JPEGOptions: JPEGOptions{PatientID: "Δ", Common: object.DerivedCommon{Source: source}}})
	if e != nil {
		t.Fatal(e)
	}
	out := part10Roundtrip(t, f)
	if getString(out.Dataset, tagPatientName) != "André" || getString(out.Dataset, tagPatientID) != "Δ" || getString(out.Dataset, tagSpecificCharacterSet) != "ISO_IR 192" {
		t.Fatalf("source charset/explicit fields disagree: name=%q", getString(out.Dataset, tagPatientName))
	}
	if getString(source, tagSpecificCharacterSet) != "ISO_IR 100" {
		t.Fatal("source mutated")
	}
}
