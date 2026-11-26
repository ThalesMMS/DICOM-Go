package clinicalrelation_test

import (
	"reflect"
	"testing"

	"github.com/ThalesMMS/dicom-go/clinicalrelation"
	"github.com/ThalesMMS/dicom-go/core"
	"github.com/ThalesMMS/dicom-go/dictionary/std"
	"github.com/ThalesMMS/dicom-go/dictionary/tags"
	"github.com/ThalesMMS/dicom-go/dictionary/uid"
	"github.com/ThalesMMS/dicom-go/gsps"
	"github.com/ThalesMMS/dicom-go/internal/dicomtest"
	"github.com/ThalesMMS/dicom-go/object"
	"github.com/ThalesMMS/dicom-go/parametricmap"
	"github.com/ThalesMMS/dicom-go/rtdose"
	"github.com/ThalesMMS/dicom-go/seg"
	"github.com/ThalesMMS/dicom-go/spatialreg"
)

// Every Storage SOP Class either resolves from its identity alone, with the
// same result whatever other attributes the object carries, or is read by a
// class-specific reader that an identity-only data set does not satisfy.
func TestResolvesFromIdentityMatchesResolve(t *testing.T) {
	const (
		studyUID  = "1.2.826.0.1.3680043.10.543.1619.1"
		seriesUID = "1.2.826.0.1.3680043.10.543.1619.2"
		sopUID    = "1.2.826.0.1.3680043.10.543.1619.3"
		frameUID  = "1.2.826.0.1.3680043.10.543.1619.4"
	)
	parsed := 0
	for _, class := range uid.StorageSOPClassUIDs() {
		identity := []core.Element{
			dicomtest.StringElement(core.NewTag(0x0008, 0x0016), core.VRUI, class),
			dicomtest.StringElement(core.NewTag(0x0008, 0x0018), core.VRUI, sopUID),
			dicomtest.StringElement(core.NewTag(0x0020, 0x000D), core.VRUI, studyUID),
			dicomtest.StringElement(core.NewTag(0x0020, 0x000E), core.VRUI, seriesUID),
			dicomtest.StringElement(core.NewTag(0x0020, 0x0052), core.VRUI, frameUID),
		}
		reference := object.FromElements([]core.Element{
			dicomtest.StringElement(core.NewTag(0x0008, 0x1150), core.VRUI, "1.2.840.10008.5.1.4.1.1.2"),
			dicomtest.StringElement(core.NewTag(0x0008, 0x1155), core.VRUI, "1.2.826.0.1.3680043.10.543.1619.5"),
		}, std.Dictionary)
		complete := append(append([]core.Element(nil), identity...),
			object.SequenceElement(core.NewTag(0x0008, 0x1140), reference),
			dicomtest.StringElement(core.NewTag(0x0008, 0x0060), core.VRCS, "OT"),
		)
		want := clinicalrelation.Identity{SOPClassUID: class, SOPInstanceUID: sopUID, StudyInstanceUID: studyUID, SeriesInstanceUID: seriesUID}
		fromIdentity, identityErr := clinicalrelation.Resolve(object.FromElements(identity, std.Dictionary), want)
		if !clinicalrelation.ResolvesFromIdentity(class) {
			parsed++
			if identityErr == nil && fromIdentity.Kind == clinicalrelation.ObjectKindImage {
				t.Errorf("%s resolves as an image from its identity, but ResolvesFromIdentity is false", class)
			}
			continue
		}
		fromComplete, completeErr := clinicalrelation.Resolve(object.FromElements(complete, std.Dictionary), want)
		if identityErr != nil || completeErr != nil {
			t.Errorf("%s: Resolve errors = %v / %v", class, identityErr, completeErr)
			continue
		}
		if fromIdentity.Kind != clinicalrelation.ObjectKindImage || fromIdentity.FrameOfReferenceUID != frameUID || len(fromIdentity.References) != 0 {
			t.Errorf("%s: identity resolution = %+v", class, fromIdentity)
		}
		if !reflect.DeepEqual(fromComplete, fromIdentity) {
			t.Errorf("%s: complete resolution %+v differs from identity resolution %+v", class, fromComplete, fromIdentity)
		}
	}
	if parsed != 17 {
		t.Fatalf("class-specific readers cover %d Storage SOP Classes, want 17", parsed)
	}
	if !clinicalrelation.ResolvesFromIdentity("") || clinicalrelation.ResolvesFromIdentity(" 1.2.840.10008.5.1.4.1.1.66.3\x00") {
		t.Fatal("ResolvesFromIdentity does not normalize the SOP Class UID")
	}
}

func TestReadRequirementForSOPClass(t *testing.T) {
	for _, test := range []struct {
		name string
		uid  string
		want clinicalrelation.ReadRequirement
	}{
		{"binary or fractional SEG", seg.SegmentationStorage, clinicalrelation.ReadPixelData},
		{"label map SEG", seg.LabelMapSegmentationStorage, clinicalrelation.ReadPixelData},
		{"RT Dose", rtdose.RTDoseStorage, clinicalrelation.ReadPixelData},
		{"Parametric Map", parametricmap.ParametricMapStorage, clinicalrelation.ReadPixelData},
		{"padded SEG UID", " " + seg.SegmentationStorage + "\x00", clinicalrelation.ReadPixelData},
		{"GSPS", gsps.GrayscaleSoftcopyPresentationStateStorage, clinicalrelation.ReadCompleteModules},
		{"deformable registration", spatialreg.DeformableSpatialRegistrationStorage, clinicalrelation.ReadCompleteModules},
		{"enhanced CT", "1.2.840.10008.5.1.4.1.1.2.1", clinicalrelation.ReadIdentity},
		{"waveform", "1.2.840.10008.5.1.4.1.1.9.4.1", clinicalrelation.ReadIdentity},
		{"encapsulated PDF", "1.2.840.10008.5.1.4.1.1.104.1", clinicalrelation.ReadIdentity},
		{"unknown private class", "2.25.1663", clinicalrelation.ReadIdentity},
		{"missing class", "", clinicalrelation.ReadIdentity},
	} {
		t.Run(test.name, func(t *testing.T) {
			if got := clinicalrelation.ReadRequirementForSOPClass(test.uid); got != test.want {
				t.Fatalf("read requirement = %v, want %v", got, test.want)
			}
		})
	}
}

func TestResolveUnknownAndMissingClassFromIdentity(t *testing.T) {
	for _, test := range []struct {
		uid  string
		want clinicalrelation.ObjectKind
	}{
		{"2.25.1663", clinicalrelation.ObjectKindImage},
		{"", clinicalrelation.ObjectKindOther},
	} {
		dataset := object.FromElements([]core.Element{
			dicomtest.StringElement(tags.FrameOfReferenceUID, core.VRUI, "2.25.42"),
		}, std.Dictionary)
		result, err := clinicalrelation.Resolve(dataset, clinicalrelation.Identity{SOPClassUID: test.uid})
		if err != nil || result.Kind != test.want || result.SOPClassUID != test.uid ||
			result.FrameOfReferenceUID != "2.25.42" || len(result.References) != 0 {
			t.Fatalf("Resolve(%q) = %+v, %v", test.uid, result, err)
		}
	}
}
