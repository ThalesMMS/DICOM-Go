package object_test

import (
	"bytes"
	"errors"
	"testing"

	"github.com/ThalesMMS/dicom-go/core"
	"github.com/ThalesMMS/dicom-go/dictionary/std"
	"github.com/ThalesMMS/dicom-go/object"
	"github.com/ThalesMMS/dicom-go/transfer"
)

var (
	uidTagSOPClass    = core.NewTag(0x0008, 0x0016)
	uidTagSOPInstance = core.NewTag(0x0008, 0x0018)
	uidTagReferenced  = core.NewTag(0x0008, 0x1155)
	uidTagMediaInst   = core.NewTag(0x0002, 0x0003)
)

func uidText(tag core.Tag, values ...string) core.Element {
	return core.Element{Header: core.ElementHeader{Tag: tag, VR: core.VRUI}, Value: core.StringValue(values)}
}

// Conformant authored UIDs, NUL padding, empty and multi-valued UI are
// written and read back unchanged.
func TestAuthoredConformantUIDsAreWritten(t *testing.T) {
	for _, values := range [][]string{
		{"1.2.840.10008.1.2.1\x00"},
		{"0.0"},
		{"2.25.339687224046345417426104158208217577001"},
		{""},
		{"1.2.3", "2.25.1"},
	} {
		obj := object.FromElements([]core.Element{
			uidText(uidTagSOPClass, "1.2.840.10008.5.1.4.1.1.7"),
			uidText(uidTagSOPInstance, "1.2.826.0.1.3680043.10.543.1"),
			uidText(uidTagReferenced, values...),
		}, std.Dictionary)
		var out bytes.Buffer
		if err := object.WriteFile(&out, &object.File{Dataset: obj, TransferSyntax: transfer.ExplicitVRLittleEndian}); err != nil {
			t.Fatalf("%q: %v", values, err)
		}
		file, err := object.ReadFile(&out)
		if err != nil {
			t.Fatal(err)
		}
		got, _ := file.Dataset.GetStrings(uidTagReferenced)
		if len(values) == 1 && values[0] == "" {
			if len(got) > 1 || (len(got) == 1 && got[0] != "") {
				t.Fatalf("empty UI read back as %q", got)
			}
			continue
		}
		if len(got) != len(values) || got[len(got)-1] != trimNUL(values[len(values)-1]) {
			t.Fatalf("%q read back as %q", values, got)
		}
	}
}

func trimNUL(s string) string { return string(bytes.TrimRight([]byte(s), "\x00 ")) }

// A file whose UIDs do not conform is still read unchanged and can be
// rewritten and cloned: read values stay raw, and File Meta derived from them
// keeps their bytes. Authoring the same identity as text is refused.
func TestLegacyUIDsAreReadAndRewrittenUnchanged(t *testing.T) {
	const legacy = "1.2.826.0.1.03.RTPLAN"
	raw := func(tag core.Tag, value string) core.Element {
		b := []byte(value)
		if len(b)%2 != 0 {
			b = append(b, 0)
		}
		return core.NewRawElement(tag, core.VRUI, b)
	}
	source := object.FromElements([]core.Element{
		raw(uidTagSOPClass, "1.2.840.10008.5.1.4.1.1.7"),
		raw(uidTagSOPInstance, legacy),
		raw(uidTagReferenced, legacy),
	}, std.Dictionary)
	var first bytes.Buffer
	if err := object.WriteFile(&first, &object.File{Dataset: source, TransferSyntax: transfer.ExplicitVRLittleEndian}); err != nil {
		t.Fatalf("legacy raw UIDs refused: %v", err)
	}
	file, err := object.ReadFile(bytes.NewReader(first.Bytes()))
	if err != nil {
		t.Fatal(err)
	}
	for _, tag := range []core.Tag{uidTagSOPInstance, uidTagReferenced} {
		if got, _ := file.Dataset.GetString(tag); got != legacy {
			t.Fatalf("%s read as %q", tag, got)
		}
	}
	if got, _ := file.Meta.GetString(uidTagMediaInst); got != legacy {
		t.Fatalf("media storage SOP instance read as %q", got)
	}

	var second bytes.Buffer
	if err := object.WriteFile(&second, file); err != nil {
		t.Fatalf("rewrite of a read legacy file: %v", err)
	}
	if !bytes.Equal(first.Bytes(), second.Bytes()) {
		t.Fatal("rewrite changed the legacy file")
	}
	clone, err := object.CloneFile(file)
	if err != nil {
		t.Fatalf("clone of a read legacy file: %v", err)
	}
	if err := clone.RebuildFileMeta(); err != nil {
		t.Fatalf("meta rebuild from read UIDs: %v", err)
	}

	authored := object.FromElements([]core.Element{
		uidText(uidTagSOPClass, "1.2.840.10008.5.1.4.1.1.7"),
		uidText(uidTagSOPInstance, legacy),
	}, std.Dictionary)
	err = object.WriteFile(&bytes.Buffer{}, &object.File{Dataset: authored, TransferSyntax: transfer.ExplicitVRLittleEndian})
	if !errors.Is(err, core.ErrValueNotRepresentable) {
		t.Fatalf("authored invalid UID gave %v", err)
	}
}
