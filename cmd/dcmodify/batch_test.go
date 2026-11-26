package main

import (
	"bytes"
	"os"
	"path/filepath"
	"testing"

	"github.com/ThalesMMS/dicom-go/core"
	"github.com/ThalesMMS/dicom-go/internal/privatefs"
	"github.com/ThalesMMS/dicom-go/transfer"
)

func TestDcmodifyBatchUIDsPermissionsAndFailFirst(t *testing.T) {
	dir := t.TempDir()
	a := dc1353Input(t, dir, "a.dcm", transfer.ExplicitVRLittleEndian)
	b := dc1353Input(t, dir, "b.dcm", transfer.ExplicitVRLittleEndian)
	before, err := os.Stat(a)
	if err != nil {
		t.Fatal(err)
	}
	if c := dc1353Run(t, "-gst", "-gin", "-in-place", a, b); c != 0 {
		t.Fatal(c)
	}
	x := dc1353Read(t, a)
	y := dc1353Read(t, b)
	for _, tag := range []core.Tag{core.NewTag(0x0020, 0x000d), core.NewTag(0x0008, 0x0018)} {
		u, _ := x.Dataset.GetString(tag)
		v, _ := y.Dataset.GetString(tag)
		if u == v {
			t.Fatal("UID reused across batch")
		}
	}
	for _, p := range []string{a, b} {
		info, _ := os.Stat(p)
		if info.Mode().Perm() != before.Mode().Perm() {
			t.Fatal("permissions changed")
		}
	}
	bad := filepath.Join(dir, "bad.dcm")
	os.WriteFile(bad, []byte("not dicom"), 0o600)
	bBefore, _ := os.ReadFile(b)
	if c := dc1353Run(t, "-i", "PatientName=FIRST", "-in-place", a, bad, b); c != 1 {
		t.Fatal(c)
	}
	x = dc1353Read(t, a)
	name, _ := x.Dataset.GetString(core.NewTag(0x0010, 0x0010))
	if name != "FIRST" {
		t.Fatal(name)
	}
	bAfter, _ := os.ReadFile(b)
	if !bytes.Equal(bBefore, bAfter) {
		t.Fatal("processed after failure")
	}
	dc1353NoTemps(t, dir)
}
func TestDcmodifyBatchDuplicateAdmission(t *testing.T) {
	dir := t.TempDir()
	input := dc1353Input(t, dir, "in.dcm", transfer.ExplicitVRLittleEndian)
	before, _ := os.ReadFile(input)
	alias := filepath.Join(dir, "alias.dcm")
	if e := os.Link(input, alias); e != nil {
		t.Fatal(e)
	}
	symlink := filepath.Join(dir, "symlink.dcm")
	if e := os.Symlink(input, symlink); e != nil {
		t.Fatal(e)
	}
	for _, args := range [][]string{{"-gin", "-in-place", input, input}, {"-gin", "-in-place", input, alias}, {"-gin", "-in-place", input, symlink}, {"-gin", "-in-place", input, dir}, {"-gin", "-max-batch-files", "1", "-in-place", input, alias}, {"-gin", "-o", filepath.Join(dir, "out.dcm"), input, alias}} {
		if c := dc1353Run(t, args...); c == 0 {
			t.Fatal("duplicate admitted")
		}
	}
	after, _ := os.ReadFile(input)
	if !bytes.Equal(before, after) {
		t.Fatal("preflight modified input")
	}
}

func TestDcmodifyFiniteFlagAdmissionAndSourceCollisions(t *testing.T) {
	dir := t.TempDir()
	input := dc1353Input(t, dir, "in.dcm", transfer.ExplicitVRLittleEndian)
	before, _ := os.ReadFile(input)
	for _, flag := range []string{"-max-batch-files", "-max-value-file-bytes", "-max-value-source-bytes", "-max-edit-depth", "-max-edit-elements", "-max-edit-items", "-max-edit-visits", "-max-edit-matches", "-max-edit-path-bytes", "-max-edit-touches", "-max-edit-created-items", "-max-edit-value-bytes"} {
		for _, value := range []string{"0", "-1"} {
			if code := dc1353Run(t, "-gin", flag, value, "-in-place", input); code != 2 {
				t.Fatalf("non-positive cap accepted: %s", flag)
			}
		}
	}
	args := []string{"-gin", "-in-place"}
	for i := 0; i < 257; i++ {
		args = append(args, input)
	}
	if code := dc1353Run(t, args...); code != 2 {
		t.Fatal("257 operands admitted")
	}
	args = nil
	for i := 0; i < 4097; i++ {
		args = append(args, "-gin")
	}
	args = append(args, "-in-place", input)
	if code := dc1353Run(t, args...); code != 2 {
		t.Fatal("4097 actions admitted")
	}
	args = []string{"-gin"}
	for i := 0; i < 129; i++ {
		args = append(args, "-iff", "PatientName")
	}
	args = append(args, "-in-place", input)
	if code := dc1353Run(t, args...); code != 2 {
		t.Fatal("129 conditions admitted")
	}
	if code := dc1353Run(t, "-if", "Rows="+input, "-in-place", input); code != 1 {
		t.Fatal("value/input collision admitted")
	}
	script := filepath.Join(dir, "edits.script")
	if err := os.WriteFile(script, []byte("modify PatientName=X\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if code := dc1353Run(t, "-script", script, "-o", script, input); code != 1 {
		t.Fatal("script/output collision admitted")
	}
	after, _ := os.ReadFile(input)
	if !bytes.Equal(before, after) {
		t.Fatal("admission changed input")
	}
	dc1353NoTemps(t, dir)
}
func TestDcmodifyCreateOnlyAbsentAndRequiredIdentities(t *testing.T) {
	dir := t.TempDir()
	created := filepath.Join(dir, "new.dcm")
	if c := dc1353Run(t, "+fc", "-i", "SOPClassUID=1.2.840.10008.5.1.4.1.1.7", "-gin", "-in-place", created); c != 0 {
		t.Fatal(c)
	}
	f := dc1353Read(t, created)
	if f.TransferSyntax.UID != transfer.ExplicitVRLittleEndian.UID {
		t.Fatal("wrong new syntax")
	}
	if err := privatefs.Check(created); err != nil {
		t.Fatal(err)
	}
	missing := filepath.Join(dir, "missing.dcm")
	if c := dc1353Run(t, "+fc", "-i", "PatientName=X", "-in-place", missing); c != 1 {
		t.Fatal(c)
	}
	if _, e := os.Stat(missing); !os.IsNotExist(e) {
		t.Fatal("placeholder exists")
	}
	bad := filepath.Join(dir, "bad.dcm")
	os.WriteFile(bad, []byte("broken"), 0o600)
	if c := dc1353Run(t, "+fc", "-gin", "-in-place", bad); c != 1 {
		t.Fatal(c)
	}
	data, _ := os.ReadFile(bad)
	if string(data) != "broken" {
		t.Fatal("malformed existing overwritten")
	}
	if c := dc1353Run(t, "+fc", "-iff", "PatientID", "-gin", "-in-place", missing); c != 0 {
		t.Fatal(c)
	}
	if _, e := os.Stat(missing); !os.IsNotExist(e) {
		t.Fatal("false condition created input")
	}
	dc1353NoTemps(t, dir)
}
