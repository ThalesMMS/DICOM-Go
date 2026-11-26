package main

import (
	"bytes"
	"encoding/binary"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/ThalesMMS/dicom-go/core"
	"github.com/ThalesMMS/dicom-go/encapimage"
	"github.com/ThalesMMS/dicom-go/internal/dicomtest"
	"github.com/ThalesMMS/dicom-go/object"
	"github.com/ThalesMMS/dicom-go/transfer"
)

func TestJpg2dcmCopiesStudyAndStripsSegments(t *testing.T) {
	dir := t.TempDir()
	photo, err := os.ReadFile(filepath.Join("..", "..", "encapimage", "testdata", "baseline_422.jpg"))
	if err != nil {
		t.Fatal(err)
	}
	comment := []byte{0xff, 0xfe, 0, 0}
	binary.BigEndian.PutUint16(comment[2:], uint16(len("PRIVATE NOTE")+2))
	comment = append(comment, "PRIVATE NOTE"...)
	photo = append(append(append([]byte{}, photo[:2]...), comment...), photo[2:]...)
	input := filepath.Join(dir, "photo.jpg")
	if err := os.WriteFile(input, photo, 0o600); err != nil {
		t.Fatal(err)
	}
	study, err := dicomtest.Part10File(transfer.ExplicitVRLittleEndian,
		dicomtest.StringElement(core.NewTag(0x0008, 0x0005), core.VRCS, "ISO_IR 100"),
		dicomtest.StringElement(core.NewTag(0x0008, 0x0016), core.VRUI, "1.2.840.10008.5.1.4.1.1.7"),
		dicomtest.StringElement(core.NewTag(0x0008, 0x0018), core.VRUI, "1.2.826.0.1.3680043.10.1.985.1"),
		dicomtest.StringElement(core.NewTag(0x0010, 0x0010), core.VRPN, "M\xfcller^Ana"),
		dicomtest.StringElement(core.NewTag(0x0010, 0x0020), core.VRLO, "ID985"),
		dicomtest.StringElement(core.NewTag(0x0020, 0x000D), core.VRUI, "1.2.826.0.1.3680043.10.1.985"),
	)
	if err != nil {
		t.Fatal(err)
	}
	studyPath := filepath.Join(dir, "study.dcm")
	if err := os.WriteFile(studyPath, study, 0o600); err != nil {
		t.Fatal(err)
	}
	output := filepath.Join(dir, "photo.dcm")
	var stdout, stderr bytes.Buffer
	if code := run([]string{"-vlp", "-study-from", studyPath, input, output}, &stdout, &stderr); code != 0 {
		t.Fatalf("exit %d: %s", code, stderr.String())
	}
	if !strings.Contains(stdout.String(), "removed 1 COM segment(s)") || strings.Contains(stdout.String(), "PRIVATE") {
		t.Fatalf("summary %q", stdout.String())
	}
	file, err := object.OpenFile(output)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = file.Close() }()
	for tag, want := range map[core.Tag]string{
		core.NewTag(0x0008, 0x0016): encapimage.VLPhotographicImageStorage,
		core.NewTag(0x0010, 0x0010): "Müller^Ana",
		core.NewTag(0x0010, 0x0020): "ID985",
		core.NewTag(0x0020, 0x000D): "1.2.826.0.1.3680043.10.1.985",
		core.NewTag(0x0008, 0x0005): "ISO_IR 192",
	} {
		if got, _ := file.Dataset.GetString(tag); got != want {
			t.Fatalf("%s = %q, want %q", tag, got, want)
		}
	}
	if file.TransferSyntax.UID != transfer.JPEGBaseline.UID {
		t.Fatalf("transfer syntax %s", file.TransferSyntax.UID)
	}
}

func TestJpg2dcmRejectsProgressiveByDefault(t *testing.T) {
	input := filepath.Join("..", "..", "encapimage", "testdata", "progressive.jpg")
	output := filepath.Join(t.TempDir(), "out.dcm")
	var stdout, stderr bytes.Buffer
	if code := run([]string{input, output}, &stdout, &stderr); code != 1 || !strings.Contains(stderr.String(), "progressive") {
		t.Fatalf("exit %d: %s", code, stderr.String())
	}
	if code := run([]string{"-allow-progressive", input, output}, &stdout, &stderr); code != 0 {
		t.Fatalf("exit %d: %s", code, stderr.String())
	}
	if code := run([]string{input}, &stdout, &stderr); code != 2 {
		t.Fatalf("usage exit %d", code)
	}
}
