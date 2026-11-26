package dicomdir

import (
	"bytes"
	"io"
	"os"
	"path/filepath"
	"slices"
	"testing"

	"github.com/ThalesMMS/dicom-go/core"
	"github.com/ThalesMMS/dicom-go/dictionary/std"
	"github.com/ThalesMMS/dicom-go/object"
	"github.com/ThalesMMS/dicom-go/transfer"
)

// DCMTK dcmdata/tests/tdicdir.cc rejects rooted Referenced File IDs.
// A matching file under the media root must never make such a record usable.
func TestReferenceAcceptanceDICOMDIRRejectsRootedWireFileID(t *testing.T) {
	for _, tc := range []struct {
		name string
		wire string
	}{
		{name: "rooted", wire: `\DICOM\IMAGE001`},
		{name: "UNC", wire: `\\DICOM\IMAGE001`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			root := t.TempDir()
			images := filepath.Join(root, "DICOM")
			if err := os.Mkdir(images, 0o700); err != nil {
				t.Fatal(err)
			}
			want := filepath.Join(images, "IMAGE002")
			if err := os.WriteFile(filepath.Join(images, "IMAGE001"), []byte("invalid reference candidate"), 0o600); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(want, []byte("valid reference"), 0o600); err != nil {
				t.Fatal(err)
			}
			dataset := object.FromElements([]core.Element{
				uiElement(testTagSOPClassUID, "1.2.840.10008.1.3.10"),
				uiElement(testTagSOPInstanceUID, "1.2.3.81.92"),
				directoryRecordSequence(directoryRecord(tc.wire), directoryRecord("DICOM", "IMAGE002")),
			}, std.Dictionary)
			var encoded bytes.Buffer
			if err := object.WriteFile(&encoded, &object.File{Dataset: dataset, TransferSyntax: transfer.ExplicitVRLittleEndian}); err != nil {
				t.Fatal(err)
			}
			path := filepath.Join(root, "DICOMDIR")
			if err := os.WriteFile(path, encoded.Bytes(), 0o600); err != nil {
				t.Fatal(err)
			}

			paths, err := ReferencedPaths(path)
			if err != nil || !slices.Equal(paths, []string{want}) {
				t.Errorf("ReferencedPaths() = %q, %v; want only %q", paths, err, want)
			}
			file, err := object.OpenFile(path)
			if err != nil {
				t.Fatal(err)
			}
			defer file.Close()
			var opened []string
			for _, ref := range References(file.Dataset) {
				referenced, resolved, err := OpenReferencedFile(root, ref)
				if err != nil {
					continue
				}
				body, readErr := io.ReadAll(referenced)
				closeErr := referenced.Close()
				if readErr != nil || closeErr != nil {
					t.Fatalf("reading %q: %v; closing: %v", resolved, readErr, closeErr)
				}
				opened = append(opened, resolved)
				if resolved == want && string(body) != "valid reference" {
					t.Errorf("opened expected path with unexpected contents %q", body)
				}
			}
			if !slices.Equal(opened, []string{want}) {
				t.Errorf("opened %q; want only %q", opened, want)
			}
		})
	}
}
