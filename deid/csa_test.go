package deid

import (
	"bytes"
	"context"
	"strings"
	"testing"

	"github.com/ThalesMMS/dicom-go/core"
	"github.com/ThalesMMS/dicom-go/object"
	"github.com/ThalesMMS/dicom-go/siemens"
)

var (
	tagCSACreator = core.NewTag(0x0029, 0x0010)
	tagCSAImage   = core.NewTag(0x0029, 0x1010)
	tagCSASeries  = core.NewTag(0x0029, 0x1020)
)

// dirtyCSA returns an SV10 header whose first element name is followed by
// leftover text, and the same header as ScrubCSA should leave it.
func dirtyCSA(t *testing.T) (dirty, clean []byte) {
	t.Helper()
	clean, err := siemens.EncodeCSA([]siemens.CSAElement{{Name: "EchoLinePosition", VM: 1, VR: "IS", SyngoDT: 6, Items: 1, Values: []string{"128"}}})
	if err != nil {
		t.Fatal(err)
	}
	dirty = bytes.Clone(clean)
	copy(dirty[16+len("EchoLinePosition")+1:], "Doe^Jane")
	return dirty, clean
}

func csaObject(t *testing.T) (*object.Object, []byte) {
	t.Helper()
	dirty, clean := dirtyCSA(t)
	return object.FromElements([]core.Element{
		core.NewRawElement(tagPatientName, core.VRPN, []byte("Doe^Jane")),
		core.NewRawElement(tagCSACreator, core.VRLO, []byte(csaCreator)),
		core.NewRawElement(tagCSAImage, core.VROB, dirty),
		// Not a CSA header in any format: it cannot be scrubbed.
		core.NewRawElement(tagCSASeries, core.VROB, []byte("SV10\x04\x03\x02\x01garbage!")),
	}, nil), clean
}

func TestAnonymizeScrubsRetainedCSAHeaders(t *testing.T) {
	obj, clean := csaObject(t)
	report, err := AnonymizeObjectWithReport(obj, Options{PrivateTagPolicy: RetainPrivateTags}, nil)
	if err != nil {
		t.Fatal(err)
	}
	got, _ := obj.GetRaw(tagCSAImage)
	if !bytes.Equal(got, clean) || obj.Has(tagCSASeries) {
		t.Fatalf("image header scrubbed %v, unscrubbable series header kept %v", bytes.Equal(got, clean), obj.Has(tagCSASeries))
	}
	if report.CSA != (CSAScrubReport{Scrubbed: 1, BytesCleared: len("Doe^Jane"), Unscrubbable: 1}) {
		t.Fatalf("report %+v", report.CSA)
	}
	// Removed private attributes are not scrubbed or counted.
	obj, _ = csaObject(t)
	if report, err := AnonymizeObjectWithReport(obj, Options{}, nil); err != nil || report.CSA != (CSAScrubReport{}) || obj.Has(tagCSAImage) {
		t.Fatalf("removed: %+v %v", report.CSA, err)
	}
}

func TestBasicProfileScrubsSafePrivateCSAHeaders(t *testing.T) {
	registry, err := NewSafePrivateRegistry(SafePrivateProvenance{Version: "csa-2026-09", Checksum: strings.Repeat("b", 64)}, []SafePrivateRule{
		{Tag: tagCSAImage, Creator: csaCreator, VR: core.VROB, VM: 1, Action: ProfileActionKeep},
		{Tag: tagCSASeries, Creator: csaCreator, VR: core.VROB, VM: 1, Action: ProfileActionKeep},
	})
	if err != nil {
		t.Fatal(err)
	}
	obj, clean := csaObject(t)
	options := DefaultBasicProfileOptions()
	options.SelectedOptions = []ProfileOption{ProfileOptionRetainSafePrivate}
	options.SafePrivateRegistry = registry
	plan, err := PlanBasicProfile(context.Background(), obj, options, nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := plan.Apply(context.Background(), obj); err != nil {
		t.Fatal(err)
	}
	got, _ := obj.GetRaw(tagCSAImage)
	if !bytes.Equal(got, clean) || obj.Has(tagCSASeries) || !obj.Has(tagCSACreator) {
		t.Fatalf("image header scrubbed %v, series kept %v, creator kept %v", bytes.Equal(got, clean), obj.Has(tagCSASeries), obj.Has(tagCSACreator))
	}
	report := plan.Report()
	if report.CSA != (CSAScrubReport{Scrubbed: 1, BytesCleared: len("Doe^Jane"), Unscrubbable: 1}) {
		t.Fatalf("report %+v", report.CSA)
	}
	reasons := map[core.Tag]string{}
	for _, action := range report.Actions {
		reasons[action.Tag] = action.Reason
	}
	if reasons[tagCSAImage] != "safe-private-csa-scrubbed" || reasons[tagCSASeries] != "safe-private-csa-unscrubbable" {
		t.Fatalf("reasons %v", reasons)
	}
}
