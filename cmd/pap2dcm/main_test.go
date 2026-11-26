package main

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/ThalesMMS/dicom-go/object"
	"github.com/ThalesMMS/dicom-go/pixeldata"
)

// TestConvertGDCMPapyrusFile runs the command on the gdcmData Papyrus file
// (opt-in: DICOMGO_GDCMDATA_ROOT).
func TestConvertGDCMPapyrusFile(t *testing.T) {
	root := os.Getenv("DICOMGO_GDCMDATA_ROOT")
	if root == "" {
		t.Skip("opt-in: DICOMGO_GDCMDATA_ROOT")
	}
	input := filepath.Join(root, "PET-cardio-Multiframe-Papyrus.dcm")
	output := filepath.Join(t.TempDir(), "out")
	var stdout, stderr bytes.Buffer
	if code := run([]string{input, output}, &stdout, &stderr); code != 0 {
		t.Fatalf("exit %d: %s", code, stderr.String())
	}
	if !strings.Contains(stdout.String(), "9 images") || !strings.Contains(stdout.String(), "reserved UID") {
		t.Fatalf("stdout:\n%s", stdout.String())
	}
	for i := 1; i <= 9; i++ {
		file, err := object.OpenFile(filepath.Join(output, "IMG000"+string(rune('0'+i))+".dcm"))
		if err != nil {
			t.Fatal(err)
		}
		_, err = pixeldata.ExtractNativeFrames(file.Dataset)
		_ = file.Close()
		if err != nil {
			t.Fatalf("image %d: %v", i, err)
		}
	}
	// Existing files are never replaced.
	stdout.Reset()
	if code := run([]string{input, output}, &stdout, &stderr); code != 1 {
		t.Fatalf("second run exit %d", code)
	}
}

func TestUsageAndNonPapyrusInput(t *testing.T) {
	var stdout, stderr bytes.Buffer
	if code := run([]string{"only-one"}, &stdout, &stderr); code != 2 {
		t.Fatalf("usage exit %d", code)
	}
	input := filepath.Join(t.TempDir(), "missing.dcm")
	if code := run([]string{input, t.TempDir()}, &stdout, &stderr); code != 1 {
		t.Fatalf("missing input exit %d", code)
	}
}
