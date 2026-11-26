package main

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/ThalesMMS/dicom-go/core"
	"github.com/ThalesMMS/dicom-go/dictionary/std"
	"github.com/ThalesMMS/dicom-go/object"
	"github.com/ThalesMMS/dicom-go/transfer"
)

func dc1353Input(t *testing.T, dir, name string, syntax transfer.Syntax, extra ...core.Element) string {
	t.Helper()
	elems := []core.Element{{Header: core.ElementHeader{Tag: core.NewTag(0x0008, 0x0016), VR: core.VRUI}, Value: core.StringValue{"1.2.840.10008.5.1.4.1.1.7"}}, {Header: core.ElementHeader{Tag: core.NewTag(0x0008, 0x0018), VR: core.VRUI}, Value: core.StringValue{"1.2.826.0.1.3680043.10.1353.1"}}, {Header: core.ElementHeader{Tag: core.NewTag(0x0010, 0x0010), VR: core.VRPN}, Value: core.StringValue{"SYNTHETIC^OLD"}}}
	elems = append(elems, extra...)
	dataset := object.FromElements(elems, std.Dictionary)
	dataset.SetValueByteOrder(syntax.ByteOrder)
	file := &object.File{Dataset: dataset, TransferSyntax: syntax}
	var data bytes.Buffer
	if e := object.WriteFile(&data, file); e != nil {
		t.Fatal(e)
	}
	path := filepath.Join(dir, name)
	if e := os.WriteFile(path, data.Bytes(), 0o640); e != nil {
		t.Fatal(e)
	}
	return path
}
func dc1353Read(t *testing.T, path string) *object.File {
	t.Helper()
	f, e := object.OpenFile(path)
	if e != nil {
		t.Fatal(e)
	}
	t.Cleanup(func() { f.Close() })
	return f
}
func dc1353Run(t *testing.T, args ...string) int {
	t.Helper()
	var stderr bytes.Buffer
	c := run(args, &bytes.Buffer{}, &stderr)
	if c != 0 {
		t.Logf("exit %d: %s", c, stderr.String())
	}
	return c
}
func dc1353NoTemps(t *testing.T, dir string) {
	t.Helper()
	matches, e := filepath.Glob(filepath.Join(dir, ".dcmodify-*"))
	if e != nil || len(matches) != 0 {
		t.Fatalf("temps %v %v", matches, e)
	}
}

func TestDcmodifyPrivateFilterRealFilesAndPublicMeta(t *testing.T) {
	for _, syntax := range []transfer.Syntax{transfer.ExplicitVRLittleEndian, transfer.ExplicitVRBigEndian} {
		t.Run(syntax.UID, func(t *testing.T) {
			dir := t.TempDir()
			private := func(tag core.Tag, vr core.VR, value string) core.Element {
				return core.Element{Header: core.ElementHeader{Tag: tag, VR: vr}, Value: core.StringValue{value}}
			}
			input := dc1353Input(t, dir, "in.dcm", syntax,
				private(core.NewTag(0x0011, 0x0010), core.VRLO, "PRIVATE CREATOR"), private(core.NewTag(0x0011, 0x1010), core.VRLO, "PRIVATE VALUE"), private(core.NewTag(0x7776, 0x1010), core.VRLO, "PUBLIC UNKNOWN"),
				core.Element{Header: core.ElementHeader{Tag: core.NewTag(0x0008, 0x1110), VR: core.VRSQ}, Value: core.SequenceValue{Items: []core.DataSet{{Elements: []core.Element{private(core.NewTag(0x0011, 0x0010), core.VRLO, "PRIVATE CREATOR"), private(core.NewTag(0x0011, 0x1010), core.VRLO, "NESTED PRIVATE"), private(core.NewTag(0x0010, 0x0010), core.VRPN, "PUBLIC")}}}}},
				core.Element{Header: core.ElementHeader{Tag: core.NewTag(0x0011, 0x1020), VR: core.VRSQ}, Value: core.SequenceValue{Items: []core.DataSet{{Elements: []core.Element{private(core.NewTag(0x0010, 0x0010), core.VRPN, "WITH PRIVATE SQ")}}}}},
			)
			f := dc1353Read(t, input)
			f.Meta.Put(core.Element{Header: core.ElementHeader{Tag: core.NewTag(0x0002, 0x0100), VR: core.VRUI}, Value: core.StringValue{"1.2.3"}})
			f.Meta.Put(core.Element{Header: core.ElementHeader{Tag: core.NewTag(0x0002, 0x0102), VR: core.VROB}, Value: core.RawValue{1, 2}})
			var encoded bytes.Buffer
			if err := object.WriteFile(&encoded, f); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(input, encoded.Bytes(), 0o640); err != nil {
				t.Fatal(err)
			}
			if code := dc1353Run(t, "-ep", "-in-place", input); code != 0 {
				t.Fatal(code)
			}
			out := dc1353Read(t, input)
			if out.Dataset.Has(core.NewTag(0x0011, 0x0010)) || out.Dataset.Has(core.NewTag(0x0011, 0x1020)) || !out.Dataset.Has(core.NewTag(0x7776, 0x1010)) || !out.Meta.Has(core.NewTag(0x0002, 0x0102)) {
				t.Fatal("private/public classification not preserved")
			}
			items, _ := out.Dataset.GetSequence(core.NewTag(0x0008, 0x1110))
			if len(items) != 1 || items[0].Len() != 1 || !items[0].Has(core.NewTag(0x0010, 0x0010)) {
				t.Fatal("nested private removal incorrect")
			}
			before, _ := os.ReadFile(input)
			if code := dc1353Run(t, "-ep", "-max-edit-touches", "1", "-in-place", input); code != 1 {
				t.Fatal("private pass ignored conservative cap")
			}
			after, _ := os.ReadFile(input)
			if !bytes.Equal(before, after) {
				t.Fatal("private cap failure published")
			}
			dc1353NoTemps(t, dir)
		})
	}
}

func TestDcmodifyPrivateRemovalWithExplicitOriginalAttributes(t *testing.T) {
	dir := t.TempDir()
	creator := core.NewTag(0x0011, 0x0010)
	private := core.NewTag(0x0011, 0x1010)
	input := dc1353Input(t, dir, "in.dcm", transfer.ExplicitVRLittleEndian,
		core.Element{Header: core.ElementHeader{Tag: creator, VR: core.VRLO}, Value: core.StringValue{"AUDIT CREATOR"}},
		core.Element{Header: core.ElementHeader{Tag: private, VR: core.VRLO}, Value: core.StringValue{"AUDIT PREVIOUS"}},
	)
	output := filepath.Join(dir, "out.dcm")
	before, _ := os.ReadFile(input)
	if code := dc1353Run(t, "-ep", "-original-attributes", "correct", "-o", output, input); code != 0 {
		t.Fatal(code)
	}
	f := dc1353Read(t, output)
	if f.Dataset.Has(creator) || f.Dataset.Has(private) {
		t.Fatal("current private attributes retained")
	}
	audit, ok := f.Dataset.GetSequence(core.NewTag(0x0400, 0x0561))
	if !ok || len(audit) != 1 {
		t.Fatal("explicit Original Attributes record missing")
	}
	previous, ok := audit[0].GetSequence(core.NewTag(0x0400, 0x0550))
	if !ok || len(previous) != 1 || !previous[0].Has(creator) {
		t.Fatal("legacy audit content refiltered")
	}
	if value, _ := previous[0].GetString(private); value != "AUDIT PREVIOUS" {
		t.Fatal("previous private value not preserved in explicit audit")
	}
	after, _ := os.ReadFile(input)
	if !bytes.Equal(before, after) {
		t.Fatal("audit output altered source")
	}
	dc1353NoTemps(t, dir)
}

func TestDcmodifyParityUIDOrderMetaAndSyntax(t *testing.T) {
	for _, syntax := range []transfer.Syntax{transfer.ExplicitVRLittleEndian, transfer.ImplicitVRLittleEndian, transfer.ExplicitVRBigEndian, transfer.DeflatedExplicitVRLittleEndian} {
		t.Run(syntax.UID, func(t *testing.T) {
			dir := t.TempDir()
			input := dc1353Input(t, dir, "in.dcm", syntax,
				core.Element{Header: core.ElementHeader{Tag: core.NewTag(0x0020, 0x0052), VR: core.VRUI}, Value: core.StringValue{"1.2.99.10"}},
				core.Element{Header: core.ElementHeader{Tag: core.NewTag(0x0008, 0x1110), VR: core.VRSQ}, Value: core.SequenceValue{Items: []core.DataSet{{Elements: []core.Element{
					{Header: core.ElementHeader{Tag: core.NewTag(0x0008, 0x1155), VR: core.VRUI}, Value: core.StringValue{"1.2.826.0.1.3680043.10.1353.1"}},
				}}}}},
			)
			output := filepath.Join(dir, "out.dcm")
			before, _ := os.ReadFile(input)
			if c := dc1353Run(t, "-gst", "-gse", "-gin", "-i", "SOPInstanceUID=1.2.99.1", "-o", output, input); c != 0 {
				t.Fatal(c)
			}
			f := dc1353Read(t, output)
			if f.TransferSyntax.UID != syntax.UID {
				t.Fatal(f.TransferSyntax.UID)
			}
			for _, tag := range []core.Tag{core.NewTag(0x0020, 0x000d), core.NewTag(0x0020, 0x000e)} {
				v, _ := f.Dataset.GetString(tag)
				if !core.IsValidUID(v) || !strings.HasPrefix(v, "2.25.") {
					t.Fatal("generated UID invalid")
				}
			}
			v, _ := f.Dataset.GetString(core.NewTag(0x0008, 0x0018))
			meta, _ := f.Meta.GetString(core.NewTag(0x0002, 0x0003))
			if v != "1.2.99.1" || meta != v {
				t.Fatal("instance/meta differ")
			}
			frame, _ := f.Dataset.GetString(core.NewTag(0x0020, 0x0052))
			references, _ := f.Dataset.GetSequence(core.NewTag(0x0008, 0x1110))
			if len(references) != 1 {
				t.Fatal("reference sequence altered")
			}
			ref, _ := references[0].GetString(core.NewTag(0x0008, 0x1155))
			if frame != "1.2.99.10" || ref != "1.2.826.0.1.3680043.10.1353.1" {
				t.Fatal("non-selected UID rewritten")
			}
			after, _ := os.ReadFile(input)
			if !bytes.Equal(before, after) {
				t.Fatal("source changed")
			}
		})
	}
}
func TestDcmodifyParityConditionsBeforeUIDAndMissingPolicy(t *testing.T) {
	dir := t.TempDir()
	input := dc1353Input(t, dir, "in.dcm", transfer.ExplicitVRLittleEndian)
	output := filepath.Join(dir, "out.dcm")
	if c := dc1353Run(t, "-iff", "SOPInstanceUID=1.2.826.0.1.3680043.10.1353.1", "-gin", "-o", output, input); c != 0 {
		t.Fatal(c)
	}
	os.Remove(output)
	before, _ := os.ReadFile(input)
	if c := dc1353Run(t, "-iff", "PatientName=OTHER", "-gin", "-o", output, input); c != 0 {
		t.Fatal(c)
	}
	if _, e := os.Stat(output); !os.IsNotExist(e) {
		t.Fatal("false condition published output")
	}
	if c := dc1353Run(t, "-imt", "-ma", "PatientID=X", "-ea", "AccessionNumber", "-m", "PatientID=X", "-e", "AccessionNumber", "-i", "PatientName=LAST", "-in-place", input); c != 0 {
		t.Fatal(c)
	}
	after, _ := os.ReadFile(input)
	if bytes.Equal(before, after) {
		t.Fatal("final insert missing")
	}
	dc1353NoTemps(t, dir)
}
func TestDcmodifyParityFixedErrorsEntropyAndScriptCaps(t *testing.T) {
	dir := t.TempDir()
	input := dc1353Input(t, dir, "in.dcm", transfer.ExplicitVRLittleEndian)
	output := filepath.Join(dir, "out.dcm")
	var stderr bytes.Buffer
	if c := run([]string{"-iff", "NOT_A_TAG=SECRET_UID_1353", "-gin", "-o", output, input}, &bytes.Buffer{}, &stderr); c != 2 || strings.Contains(stderr.String(), "SECRET_UID_1353") {
		t.Fatalf("%d %s", c, stderr.String())
	}
	if c := runContext(context.Background(), []string{"-gin", "-o", output, input}, &bytes.Buffer{}, &stderr, func() (string, error) { return "", errors.New("SECRET_ENTROPY_1353") }); c != 1 || strings.Contains(stderr.String(), "SECRET_ENTROPY_1353") {
		t.Fatalf("%d %s", c, stderr.String())
	}
	script := filepath.Join(dir, "edits.txt")
	if e := os.WriteFile(script, bytes.Repeat([]byte{' '}, (1<<20)+1), 0o600); e != nil {
		t.Fatal(e)
	}
	if c := dc1353Run(t, "-script", script, "-o", output, input); c != 2 {
		t.Fatal(c)
	}
	if _, e := os.Stat(output); !os.IsNotExist(e) {
		t.Fatal("failure published output")
	}
	dc1353NoTemps(t, dir)
}
func TestDcmodifyParityRecursiveWrittenOutput(t *testing.T) {
	dir := t.TempDir()
	input := dc1353Input(t, dir, "in.dcm", transfer.ExplicitVRLittleEndian)
	if c := dc1353Run(t, "-i", "ReferencedStudySequence[0].PatientName=ONE", "-i", "ReferencedStudySequence[1].ReferencedStudySequence[0].PatientName=TWO", "-ma", "PatientName=ALL", "-in-place", input); c != 0 {
		t.Fatal(c)
	}
	f := dc1353Read(t, input)
	n := 0
	if e := f.Dataset.Walk(func(_ []core.Tag, el core.Element) error {
		if el.Tag() == core.NewTag(0x0010, 0x0010) {
			n++
			if el.StringValue() != "ALL" {
				t.Fatal("nested modification missing")
			}
		}
		return nil
	}); e != nil {
		t.Fatal(e)
	}
	if n != 3 {
		t.Fatal(n)
	}
	if c := dc1353Run(t, "-ea", "ReferencedStudySequence", "-in-place", input); c != 0 {
		t.Fatal(c)
	}
	f = dc1353Read(t, input)
	if f.Dataset.Has(core.NewTag(0x0008, 0x1110)) {
		t.Fatal("sequence not removed")
	}
}
