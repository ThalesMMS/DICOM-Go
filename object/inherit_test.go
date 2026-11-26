package object

import (
	"bytes"
	"testing"
	"time"

	"github.com/ThalesMMS/dicom-go/core"
	"github.com/ThalesMMS/dicom-go/transfer"
)

func inheritSource() *Object {
	str := func(g, e uint16, vr core.VR, raw string) core.Element {
		return core.NewRawElement(core.NewTag(g, e), vr, []byte(raw))
	}
	item := core.DataSet{Elements: []core.Element{str(0x0010, 0x0020, core.VRLO, "OTHER-1 "), str(0x0010, 0x0021, core.VRLO, "H\xf4pital")}}
	return FromElements([]core.Element{
		str(0x0008, 0x0005, core.VRCS, "ISO_IR 100"),
		str(0x0008, 0x0020, core.VRDA, "20260101"),
		str(0x0008, 0x0050, core.VRSH, "ACC-9 "),
		str(0x0008, 0x0070, core.VRLO, "SCANNER "),
		str(0x0008, 0x0201, core.VRSH, "-0300 "),
		str(0x0010, 0x0010, core.VRPN, "M\xfcller^Hans"),
		str(0x0010, 0x0020, core.VRLO, "PID-7 "),
		{Header: core.ElementHeader{Tag: core.NewTag(0x0010, 0x1002), VR: core.VRSQ}, Value: core.SequenceValue{Items: []core.DataSet{item}}},
		str(0x0012, 0x0020, core.VRLO, "TRIAL-1 "),
		str(0x0020, 0x000d, core.VRUI, "1.2.3.4\x00"),
		str(0x0020, 0x0052, core.VRUI, "1.2.3.5\x00"),
	}, nil)
}

func reencode(t *testing.T, obj *Object) *Object {
	t.Helper()
	var b bytes.Buffer
	if err := WriteDataSet(&b, obj, transfer.ExplicitVRLittleEndian); err != nil {
		t.Fatal(err)
	}
	back, err := ReadDataSet(&b, transfer.ExplicitVRLittleEndian)
	if err != nil {
		t.Fatal(err)
	}
	return back
}

func TestInheritCommonModules(t *testing.T) {
	now := time.Date(2026, 9, 26, 15, 4, 5, 0, time.UTC)
	for _, declared := range []string{"", "ISO_IR 192"} {
		src := inheritSource()
		dst := New(nil)
		if declared != "" {
			dst.Put(core.Element{Header: core.ElementHeader{Tag: tagInheritCharacterSet, VR: core.VRCS}, Value: core.StringValue{declared}})
		}
		dst.Put(core.Element{Header: core.ElementHeader{Tag: core.NewTag(0x0020, 0x000d), VR: core.VRUI}, Value: core.StringValue{"2.9.9"}})
		if err := InheritCommonModules(dst, src, InheritOptions{Now: now}); err != nil {
			t.Fatal(err)
		}
		back := reencode(t, dst)
		get := func(g, e uint16) string {
			v, _ := back.LookupString(core.NewTag(g, e))
			return v
		}
		wantCharset := "ISO_IR 100"
		if declared != "" {
			wantCharset = declared
		}
		for _, c := range []struct {
			g, e uint16
			want string
		}{
			{0x0008, 0x0005, wantCharset},
			{0x0010, 0x0010, "Müller^Hans"}, {0x0010, 0x0020, "PID-7"}, {0x0008, 0x0050, "ACC-9"},
			{0x0012, 0x0020, "TRIAL-1"}, {0x0020, 0x0052, "1.2.3.5"},
			// dst's own Study Instance UID wins; the scanner is not copied.
			{0x0020, 0x000d, "2.9.9"}, {0x0008, 0x0070, "dicom-go"},
			// Created at 15:04:05 UTC, written in the source's -0300.
			{0x0008, 0x0012, "20260926"}, {0x0008, 0x0013, "120405"}, {0x0008, 0x0201, "-0300"},
			// Type 2 attributes neither object has are present and empty.
			{0x0010, 0x0030, ""}, {0x0008, 0x0090, ""}, {0x0020, 0x1040, ""},
		} {
			if got := get(c.g, c.e); got != c.want {
				t.Errorf("%q: (%04X,%04X) = %q, want %q", declared, c.g, c.e, got, c.want)
			}
		}
		for _, tag := range []core.Tag{core.NewTag(0x0010, 0x0030), core.NewTag(0x0010, 0x0040), core.NewTag(0x0008, 0x0030), core.NewTag(0x0020, 0x0010), core.NewTag(0x0018, 0x1000), core.NewTag(0x0018, 0x1020), core.NewTag(0x0008, 0x1090)} {
			if !back.Has(tag) {
				t.Errorf("%q: %s missing", declared, tag)
			}
		}
		items, _ := back.GetSequence(core.NewTag(0x0010, 0x1002))
		if len(items) != 1 {
			t.Fatalf("Other Patient IDs Sequence: %d items", len(items))
		}
		if v, _ := items[0].LookupString(core.NewTag(0x0010, 0x0021)); v != "Hôpital" {
			t.Errorf("%q: nested text %q", declared, v)
		}
	}
}

// TestInheritCommonModulesRepadsNULPaddedText checks that text a source pads
// with NUL is written padded with a space, while UIDs keep NUL padding.
func TestInheritCommonModulesRepadsNULPaddedText(t *testing.T) {
	src := FromElements([]core.Element{
		core.NewRawElement(core.NewTag(0x0008, 0x0030), core.VRTM, []byte("101010\x00\x00")),
		core.NewRawElement(core.NewTag(0x0010, 0x0040), core.VRCS, []byte("M\x00")),
		core.NewRawElement(core.NewTag(0x0020, 0x000d), core.VRUI, []byte("1.2.3.4\x00")),
	}, nil)
	dst := New(nil)
	if err := InheritCommonModules(dst, src, InheritOptions{}); err != nil {
		t.Fatal(err)
	}
	var b bytes.Buffer
	if err := WriteDataSet(&b, dst, transfer.ExplicitVRLittleEndian); err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"TM\x06\x00101010", "CS\x02\x00M ", "UI\x08\x001.2.3.4\x00"} {
		if !bytes.Contains(b.Bytes(), []byte(want)) {
			t.Errorf("encoded data set lacks %q", want)
		}
	}
	if bytes.Contains(b.Bytes(), []byte("M\x00")) {
		t.Error("Patient's Sex still padded with NUL")
	}
}

func TestInheritCommonModulesOptions(t *testing.T) {
	src := inheritSource()
	dst := New(nil)
	dst.Put(core.Element{Header: core.ElementHeader{Tag: core.NewTag(0x0010, 0x0020), VR: core.VRLO}, Value: core.StringValue{"MINE"}})
	if err := InheritCommonModules(dst, src, InheritOptions{Overwrite: true, Modules: InheritPatient}); err != nil {
		t.Fatal(err)
	}
	if v, _ := dst.LookupString(core.NewTag(0x0010, 0x0020)); v != "PID-7" {
		t.Fatalf("Overwrite kept %q", v)
	}
	if dst.Has(core.NewTag(0x0020, 0x000d)) || dst.Has(core.NewTag(0x0008, 0x0070)) || dst.Has(tagInstanceCreationDate) {
		t.Fatal("modules outside InheritPatient written")
	}
	// The copy is detached: changing src afterwards does not reach dst.
	src.Put(core.NewRawElement(core.NewTag(0x0010, 0x0010), core.VRPN, []byte("Other")))
	if v, _ := dst.LookupString(core.NewTag(0x0010, 0x0010)); v != "Müller^Hans" {
		t.Fatalf("dst changed with src: %q", v)
	}

	// Without a source the defaults still make the modules conformant.
	alone := New(nil)
	if err := InheritCommonModules(alone, nil, InheritOptions{Equipment: &Equipment{Manufacturer: "Twin Viewer", ModelName: "Twin", DeviceSerialNumber: "1", SoftwareVersions: "2"}}); err != nil {
		t.Fatal(err)
	}
	if uid, _ := alone.LookupString(core.NewTag(0x0020, 0x000d)); len(uid) < 6 || uid[:5] != "2.25." {
		t.Fatalf("generated Study Instance UID %q", uid)
	}
	if v, _ := alone.LookupString(core.NewTag(0x0008, 0x0070)); v != "Twin Viewer" {
		t.Fatalf("Manufacturer %q", v)
	}
	if alone.Has(core.NewTag(0x0020, 0x0052)) || alone.Has(core.NewTag(0x0020, 0x1040)) {
		t.Fatal("Frame of Reference written without a source")
	}
}

// TestDerivedCommonNowFixesInstanceCreation: an explicit Now makes the
// Instance Creation Date and Time reproducible.
func TestDerivedCommonNowFixesInstanceCreation(t *testing.T) {
	when := time.Date(2026, 6, 22, 10, 15, 30, 0, time.UTC)
	dst := New(nil)
	if err := (DerivedCommon{Now: when}).Apply(dst, InheritInstanceCreation); err != nil {
		t.Fatal(err)
	}
	date, _ := dst.GetString(tagInstanceCreationDate)
	clock, _ := dst.GetString(tagInstanceCreationTime)
	if date != "20260622" || clock != "101530" {
		t.Fatalf("instance creation %q %q", date, clock)
	}
}
