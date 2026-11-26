package main

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/ThalesMMS/dicom-go/core"
	"github.com/ThalesMMS/dicom-go/dicomdir"
	"github.com/ThalesMMS/dicom-go/object"
	"github.com/ThalesMMS/dicom-go/transfer"
)

func str(group, element uint16, vr core.VR, value string) core.Element {
	return core.Element{Header: core.ElementHeader{Tag: core.NewTag(group, element), VR: vr}, Value: core.StringValue{value}}
}

// writeImage writes a synthetic Secondary Capture image at root/components.
func writeImage(t *testing.T, root, sopUID, instance string, components ...string) {
	t.Helper()
	path := filepath.Join(append([]string{root}, components...)...)
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	elements := []core.Element{
		str(0x0008, 0x0016, core.VRUI, "1.2.840.10008.5.1.4.1.1.7"),
		str(0x0008, 0x0018, core.VRUI, sopUID),
		str(0x0008, 0x0020, core.VRDA, "20240115"),
		str(0x0008, 0x0030, core.VRTM, "101500"),
		str(0x0008, 0x0050, core.VRSH, ""),
		str(0x0008, 0x0060, core.VRCS, "OT"),
		str(0x0010, 0x0010, core.VRPN, "SYNTHETIC^PERSON"),
		str(0x0010, 0x0020, core.VRLO, "SYN0001"),
		str(0x0020, 0x000D, core.VRUI, "1.2.826.0.1.3680043.10.1055.10"),
		str(0x0020, 0x000E, core.VRUI, "1.2.826.0.1.3680043.10.1055.11"),
		str(0x0020, 0x0010, core.VRSH, "1"),
		str(0x0020, 0x0011, core.VRIS, "1"),
		str(0x0020, 0x0013, core.VRIS, instance),
		core.NewRawElement(core.TagPixelData, core.VROB, []byte{1, 2}),
	}
	var encoded bytes.Buffer
	if err := object.WriteFile(&encoded, &object.File{Dataset: object.FromElements(elements, nil), TransferSyntax: transfer.ExplicitVRLittleEndian}); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, encoded.Bytes(), 0o600); err != nil {
		t.Fatal(err)
	}
}

func mediaRoot(t *testing.T) string {
	t.Helper()
	root := t.TempDir()
	writeImage(t, root, "1.2.826.0.1.3680043.10.1055.12", "1", "PAT1", "STUDY1", "SER1", "IMG1")
	writeImage(t, root, "1.2.826.0.1.3680043.10.1055.13", "2", "PAT1", "STUDY1", "SER1", "IMG2")
	return root
}

func runGendir(args ...string) (int, string, string) {
	var stdout, stderr bytes.Buffer
	code := run(args, &stdout, &stderr)
	return code, stdout.String(), stderr.String()
}

// TestGendirPublishesTheDICOMDIR: the DICOMDIR at the root references both
// images, with the File-set ID, and parses back.
func TestGendirPublishesTheDICOMDIR(t *testing.T) {
	root := mediaRoot(t)
	code, stdout, stderr := runGendir("-fileset-id", "MEDIA_01", "-profile", "std-gen-cd", root)
	if code != 0 || !strings.Contains(stdout, "2 file(s), 0 skipped") {
		t.Fatalf("code %d stdout %q stderr %q", code, stdout, stderr)
	}
	set, err := dicomdir.ParseFileSet(filepath.Join(root, "DICOMDIR"))
	if err != nil {
		t.Fatal(err)
	}
	if got := len(set.Records()); got != 2 || set.FileSetID() != "MEDIA_01" {
		t.Fatalf("%d records, File-set ID %q", got, set.FileSetID())
	}
}

// TestGendirSkipsOrRefusesUnreferenceableFiles: a file whose name is not a
// File ID fails the run unless -skip-invalid reports and skips it; -o
// writes elsewhere and leaves the root untouched.
func TestGendirSkipsOrRefusesUnreferenceableFiles(t *testing.T) {
	root := mediaRoot(t)
	writeImage(t, root, "1.2.826.0.1.3680043.10.1055.14", "3", "PAT1", "image.dcm")
	if code, _, _ := runGendir(root); code != 1 {
		t.Fatalf("invalid file name: code %d, want 1", code)
	}
	if _, err := os.Stat(filepath.Join(root, "DICOMDIR")); !os.IsNotExist(err) {
		t.Fatal("a refused run published a DICOMDIR")
	}
	output := filepath.Join(t.TempDir(), "preview.dir")
	code, stdout, stderr := runGendir("-skip-invalid", "-o", output, root)
	if code != 0 || !strings.Contains(stdout, "2 file(s), 1 skipped") || !strings.Contains(stderr, "invalid_file_id") {
		t.Fatalf("code %d stdout %q stderr %q", code, stdout, stderr)
	}
	if info, err := os.Stat(output); err != nil || info.Size() == 0 {
		t.Fatalf("output %v, %v", info, err)
	}
	if _, err := os.Stat(filepath.Join(root, "DICOMDIR")); !os.IsNotExist(err) {
		t.Fatal("-o also published at the root")
	}
}

func TestGendirUsageErrors(t *testing.T) {
	root := mediaRoot(t)
	for _, args := range [][]string{{"-profile", "NO-SUCH", root}, {"-fileset-id", "lower case", root}, {}} {
		if code, _, _ := runGendir(args...); code != 2 {
			t.Fatalf("%v: code %d, want 2", args, code)
		}
	}
	if code, _, stderr := runGendir("-h"); code != 0 || !strings.Contains(stderr, "Usage: dcmgendir") {
		t.Fatalf("-h: %d %q", code, stderr)
	}
}

// TestGendirInventsMissingKeysOnRequest: an image without Study ID is
// refused with a hint, and -invent references it.
func TestGendirInventsMissingKeysOnRequest(t *testing.T) {
	root := t.TempDir()
	writeImage(t, root, "1.2.826.0.1.3680043.10.1055.15", "1", "PAT1", "IMG1")
	path := filepath.Join(root, "PAT1", "IMG1")
	file, err := object.OpenFile(path)
	if err != nil {
		t.Fatal(err)
	}
	file.Dataset.Remove(core.NewTag(0x0020, 0x0010))
	var encoded bytes.Buffer
	if err := object.WriteFile(&encoded, file); err != nil {
		t.Fatal(err)
	}
	_ = file.Close()
	if err := os.WriteFile(path, encoded.Bytes(), 0o600); err != nil {
		t.Fatal(err)
	}
	if code, _, stderr := runGendir(root); code != 1 || !strings.Contains(stderr, "missing_required_key") || !strings.Contains(stderr, "-invent fills missing keys") {
		t.Fatalf("code %d stderr %q", code, stderr)
	}
	code, stdout, stderr := runGendir("-invent", root)
	if code != 0 || !strings.Contains(stdout, "1 file(s), 0 skipped, 1 with invented keys") {
		t.Fatalf("-invent: code %d stdout %q stderr %q", code, stdout, stderr)
	}
	if _, err := dicomdir.ParseFileSet(filepath.Join(root, "DICOMDIR")); err != nil {
		t.Fatalf("the DICOMDIR with invented keys does not parse: %v", err)
	}
}
