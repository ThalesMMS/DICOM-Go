package dynamic

import (
	"testing"
	"time"

	"github.com/ThalesMMS/dicom-go/core"
	"github.com/ThalesMMS/dicom-go/internal/dicomtest"
	"github.com/ThalesMMS/dicom-go/object"
)

func TestFrameAcquisitionStampClassicAndEnhanced(t *testing.T) {
	text := func(tag core.Tag, vr core.VR, value string) core.Element {
		return core.NewRawElement(tag, vr, []byte(value))
	}
	frame := func(value string) core.DataSet {
		return core.DataSet{Elements: []core.Element{dicomtest.NewSequenceElement(tagFrameContentSequence, core.DataSet{
			Elements: []core.Element{text(tagFrameAcquisitionDateTime, core.VRDT, value)},
		})}}
	}
	for _, test := range []struct {
		name     string
		elements []core.Element
		index    int
		want     string
		zone     bool
	}{
		{"classic local", []core.Element{text(tagAcquisitionDate, core.VRDA, "20260630"), text(tagAcquisitionTime, core.VRTM, "093514.4")}, 0, "2026-06-30T09:35:14.4Z", false},
		{"classic offset", []core.Element{text(tagAcquisitionDateTime, core.VRDT, "20260630093514.4"), text(tagAcquisitionZone, core.VRSH, "-0300")}, 0, "2026-06-30T12:35:14.4Z", true},
		{"Enhanced frame 2", []core.Element{text(tagNumberOfFrames, core.VRIS, "2"), text(tagAcquisitionZone, core.VRSH, "-0300"), dicomtest.NewSequenceElement(tagPerFrameFunctionalGroups, frame("20260630093514"), frame("20260630093527+0200"))}, 1, "2026-06-30T07:35:27Z", true},
		{"whole-object date is not frame time", []core.Element{text(tagNumberOfFrames, core.VRIS, "2"), text(tagAcquisitionDateTime, core.VRDT, "20260630093514-0300")}, 1, "", false},
		{"minute precision", []core.Element{text(tagAcquisitionDateTime, core.VRDT, "202606300935")}, 0, "", false},
		{"conflicting groups", []core.Element{text(tagNumberOfFrames, core.VRIS, "2"), dicomtest.NewSequenceElement(tagSharedFunctionalGroups, frame("20260630093514")), dicomtest.NewSequenceElement(tagPerFrameFunctionalGroups, frame("20260630093514"), frame("20260630093527"))}, 1, "", false},
	} {
		t.Run(test.name, func(t *testing.T) {
			metadata, err := ReadFrameMetadata(&object.File{Dataset: object.FromElements(test.elements, nil)})
			if err != nil {
				t.Fatal(err)
			}
			got := metadata[test.index].AcquisitionStamp
			if test.want == "" {
				if !got.Time.IsZero() {
					t.Fatalf("unqualified timestamp accepted: %+v", got)
				}
				return
			}
			if got.Time.UTC().Format(time.RFC3339Nano) != test.want || got.UTCOffsetKnown != test.zone {
				t.Fatalf("timestamp = %+v, want %s known zone %v", got, test.want, test.zone)
			}
		})
	}
}
