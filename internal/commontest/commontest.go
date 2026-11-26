// Package commontest checks, in writer tests, that derived objects carry the
// common modules object.InheritCommonModules writes.
package commontest

import (
	"bytes"
	"strings"
	"testing"

	"github.com/ThalesMMS/dicom-go/core"
	"github.com/ThalesMMS/dicom-go/object"
	"github.com/ThalesMMS/dicom-go/transfer"
	"github.com/ThalesMMS/dicom-go/validation/iod"
)

// FrameOfReferenceUID is the Frame of Reference of Source.
const FrameOfReferenceUID = "1.2.826.0.1.3680043.10.1.1047.9"

// Source is a synthetic image of a patient and study (no PHI) in ISO_IR 100.
func Source() *object.Object {
	text := func(g, e uint16, vr core.VR, raw string) core.Element {
		return core.NewRawElement(core.NewTag(g, e), vr, []byte(raw))
	}
	return object.FromElements([]core.Element{
		text(0x0008, 0x0005, core.VRCS, "ISO_IR 100"),
		text(0x0008, 0x0020, core.VRDA, "20260101"),
		text(0x0008, 0x0050, core.VRSH, "ACC-1047"),
		text(0x0008, 0x0070, core.VRLO, "SCANNER "),
		text(0x0010, 0x0010, core.VRPN, "Synth\xe9tique^Ana "),
		text(0x0010, 0x0020, core.VRLO, "SYN-1047"),
		text(0x0010, 0x0040, core.VRCS, "O "),
		text(0x0020, 0x000d, core.VRUI, "1.2.826.0.1.3680043.10.1.1047.1\x00"),
		text(0x0020, 0x0052, core.VRUI, FrameOfReferenceUID+"\x00"),
	}, nil)
}

// Check encodes obj and verifies the Patient and Study modules copied from
// Source, the dicom-go equipment, the instance creation date, the Type 2
// attributes Source lacks, the Frame of Reference only when the IOD has the
// module, and that obj has no IOD error (validation/iod).
func Check(t testing.TB, obj *object.Object, frameOfReference bool) {
	t.Helper()
	// Only the modules are checked; Pixel Data (maybe encapsulated) is left out.
	header := object.FromElements(obj.Elements(), nil)
	header.Remove(core.TagPixelData)
	var encoded bytes.Buffer
	if err := object.WriteDataSet(&encoded, header, transfer.ExplicitVRLittleEndian); err != nil {
		t.Fatalf("encode: %v", err)
	}
	back, err := object.ReadDataSet(&encoded, transfer.ExplicitVRLittleEndian)
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	for tag, want := range map[core.Tag]string{
		core.NewTag(0x0010, 0x0010): "Synthétique^Ana", core.NewTag(0x0010, 0x0020): "SYN-1047",
		core.NewTag(0x0010, 0x0040): "O", core.NewTag(0x0008, 0x0020): "20260101",
		core.NewTag(0x0008, 0x0050): "ACC-1047", core.NewTag(0x0008, 0x0070): "dicom-go",
	} {
		if got, _ := back.LookupString(tag); strings.TrimSpace(got) != want {
			t.Errorf("%s = %q, want %q", tag, got, want)
		}
	}
	for _, tag := range []core.Tag{
		core.NewTag(0x0010, 0x0030), core.NewTag(0x0008, 0x0030), core.NewTag(0x0008, 0x0090), core.NewTag(0x0020, 0x0010),
		core.NewTag(0x0020, 0x000d), core.NewTag(0x0008, 0x1090), core.NewTag(0x0018, 0x1000), core.NewTag(0x0018, 0x1020),
		core.NewTag(0x0008, 0x0012), core.NewTag(0x0008, 0x0013),
	} {
		if !back.Has(tag) {
			t.Errorf("%s missing", tag)
		}
	}
	// The object meets its IOD's Types (validation/iod).
	if report, err := iod.Validate(obj, iod.Options{}); err != nil {
		t.Errorf("IOD validation: %v", err)
	} else {
		for _, f := range report.Errors() {
			t.Errorf("%s: %s", report.IOD, f)
		}
	}
	if frameOfReference != back.Has(core.NewTag(0x0020, 0x0052)) {
		t.Errorf("Frame of Reference UID present %v, want %v", back.Has(core.NewTag(0x0020, 0x0052)), frameOfReference)
	}
}
