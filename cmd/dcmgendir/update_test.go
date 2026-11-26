package main

import (
	"bytes"
	"os"
	"path/filepath"
	"testing"

	"github.com/ThalesMMS/dicom-go/dicomdir"
)

func TestGendirAppendRefreshSubset(t *testing.T) {
	root := mediaRoot(t)
	if code, _, err := runGendir("-fileset-id", "KEEP_ID", root); code != 0 {
		t.Fatal(err)
	}
	path := filepath.Join(root, "DICOMDIR")
	original, err := dicomdir.ParseFileSet(path)
	if err != nil {
		t.Fatal(err)
	}
	writeImage(t, root, "1.2.826.0.1.3680043.10.1055.99", "3", "PAT1", "STUDY1", "SER1", "IMG3")
	if code, _, err := runGendir("-append", "-file", "PAT1/STUDY1/SER1/IMG3", root); code != 0 {
		t.Fatal(err)
	}
	appended, err := dicomdir.ParseFileSet(path)
	if err != nil {
		t.Fatal(err)
	}
	if len(appended.Records()) != 3 || appended.FileSetUID() != original.FileSetUID() || appended.FileSetID() != "KEEP_ID" {
		t.Fatal("identity/count")
	}
	writeImage(t, root, "1.2.826.0.1.3680043.10.1055.99", "12345", "PAT1", "STUDY1", "SER1", "IMG3")
	before, _ := os.ReadFile(path)
	if code, _, _ := runGendir("-append", root); code != 1 {
		t.Fatalf("changed append %d", code)
	}
	after, _ := os.ReadFile(path)
	if !bytes.Equal(before, after) {
		t.Fatal("failure publication")
	}
	if code, _, err := runGendir("-update", "-file", "PAT1\\STUDY1\\SER1\\IMG3", root); code != 0 {
		t.Fatal(err)
	}
	next, err := dicomdir.ParseFileSet(path)
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, r := range next.Records() {
		if r.InstanceNumber == "12345" {
			found = true
		}
	}
	if !found {
		t.Fatal("no refresh")
	}
}
func TestGendirUpdateUsageAndFailClosed(t *testing.T) {
	root := mediaRoot(t)
	for _, args := range [][]string{{"-append", "-update", root}, {"-append", "-fileset-id", "REPLACE", root}, {"-update", "-skip-invalid", root}, {"-file", "bad.dcm", root}, {"-file", "IMG1", "-file", "IMG1", root}} {
		if code, _, _ := runGendir(args...); code != 2 {
			t.Fatalf("%v code %d", args, code)
		}
	}
	if code, _, _ := runGendir("-append", root); code != 1 {
		t.Fatal("missing imported directory accepted")
	}
	if code, _, err := runGendir(root); code != 0 {
		t.Fatal(err)
	}
	before, _ := os.ReadFile(filepath.Join(root, "DICOMDIR"))
	if err := os.Remove(filepath.Join(root, "PAT1", "STUDY1", "SER1", "IMG2")); err != nil {
		t.Fatal(err)
	}
	if code, _, _ := runGendir("-append", "-skip-invalid", "-file", "PAT1/STUDY1/SER1/IMG1", root); code != 1 {
		t.Fatal("missing unselected source accepted")
	}
	after, _ := os.ReadFile(filepath.Join(root, "DICOMDIR"))
	if !bytes.Equal(before, after) {
		t.Fatal("missing source removed implicitly")
	}
}
