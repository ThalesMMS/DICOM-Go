package dicomdir

import (
	"bytes"
	"errors"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"testing"

	"github.com/ThalesMMS/dicom-go/core"
	"github.com/ThalesMMS/dicom-go/dictionary/std"
	"github.com/ThalesMMS/dicom-go/object"
	"github.com/ThalesMMS/dicom-go/transfer"
)

// Inspired by dcmdata_referencedFileIDSafety in DCMTK's dcmdata/tests/tdicdir.cc.
func TestDCMTKReferencePortableFileID(t *testing.T) {
	tests := []struct {
		name  string
		input string
		want  FileID
	}{
		{name: "numeric component", input: `7`, want: FileID{"7"}},
		{name: "underscore component", input: `_`, want: FileID{"_"}},
		{name: "full width at both levels", input: `A012345_\IMG_0012`, want: FileID{"A012345_", "IMG_0012"}},
		{name: "reserved name in directory position", input: `DICOMDIR\OBJ_01`, want: FileID{"DICOMDIR", "OBJ_01"}},
		{name: "wire padding", input: `DIR_01\OBJ_01 `, want: FileID{"DIR_01", "OBJ_01"}},
		{name: "leading current directory", input: `.\OBJ_01`},
		{name: "rooted Windows path", input: `\DIR_01\OBJ_01`},
		{name: "terminal parent directory", input: `DIR_01\..`},
		{name: "empty middle component", input: `DIR_01\\OBJ_01`},
		{name: "empty final component", input: `DIR_01\OBJ_01\`},
		{name: "space in component", input: `DIR_01\OBJ 01`},
		{name: "space before separator", input: `DIR_01 \OBJ_01`},
		{name: "literal wildcard", input: `DIR_01\O*J_01`},
		{name: "dotted extension", input: `DIR_01\OBJ.dcm`},
		{name: "optical media version suffix", input: `DIR_01\OBJ_01;1`},
		{name: "hyphen", input: `DIR_01\OBJ-01`},
		{name: "mixed case", input: `DIR_01\Obj_01`},
		{name: "Unicode letter", input: `DIR_01\ÓBJ_01`},
		{name: "embedded NUL", input: "DIR_01\\OBJ_\x0001"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			got, err := ParseFileID(test.input)
			if test.want == nil {
				if got != nil || !errors.Is(err, ErrInvalidFileID) {
					t.Fatalf("ParseFileID(%q) = %q, %v; want nil, ErrInvalidFileID", test.input, got, err)
				}
				return
			}
			if err != nil || !reflect.DeepEqual(got, test.want) {
				t.Fatalf("ParseFileID(%q) = %q, %v; want %q", test.input, got, err, test.want)
			}
		})
	}
}

func TestDCMTKReferenceMixedMediaFileIDs(t *testing.T) {
	parent := t.TempDir()
	root := filepath.Join(parent, "media")
	if err := os.MkdirAll(filepath.Join(root, "dir_01"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(parent, "OUTSIDE"), []byte("outside medium"), 0o600); err != nil {
		t.Fatal(err)
	}
	for path, content := range map[string]string{"dir_01/obj_01": "lowercase file", "UPPER_2": "uppercase file"} {
		if err := os.WriteFile(filepath.Join(root, filepath.FromSlash(path)), []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	records := []struct {
		name    string
		fileID  []string
		path    string
		content string
	}{
		{name: "lowercase relative file", fileID: []string{"dir_01", "obj_01"}, path: "dir_01/obj_01", content: "lowercase file"},
		{name: "leading traversal", fileID: []string{"..", "OUTSIDE"}},
		{name: "embedded traversal", fileID: []string{"dir_01", "..", "..", "OUTSIDE"}},
		{name: "terminal traversal", fileID: []string{"dir_01", ".."}},
		{name: "drive-relative Windows path", fileID: []string{`C:OUTSIDE`}},
		{name: "slash-delimited path", fileID: []string{"dir_01/obj_01"}},
		{name: "reserved Windows device", fileID: []string{"dir_01", "NuL.txt"}},
		{name: "alternate stream", fileID: []string{"dir_01", "obj_01:stream"}},
		{name: "following valid record", fileID: []string{"UPPER_2"}, path: "UPPER_2", content: "uppercase file"},
	}
	items := make([]*object.Object, 0, len(records))
	for _, record := range records {
		items = append(items, directoryRecord(record.fileID...))
	}
	dataset := object.FromElements([]core.Element{
		uiElement(testTagSOPClassUID, "1.2.840.10008.1.3.10"),
		uiElement(testTagSOPInstanceUID, "1.2.3.81.91"),
		directoryRecordSequence(items...),
	}, std.Dictionary)
	var encoded bytes.Buffer
	if err := object.WriteFile(&encoded, &object.File{Dataset: dataset, TransferSyntax: transfer.ExplicitVRLittleEndian}); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(root, "DICOMDIR")
	if err := os.WriteFile(path, encoded.Bytes(), 0o600); err != nil {
		t.Fatal(err)
	}

	t.Run("enumeration returns only contained files in record order", func(t *testing.T) {
		got, err := ReferencedPaths(path)
		want := []string{filepath.Join(root, "dir_01", "obj_01"), filepath.Join(root, "UPPER_2")}
		if err != nil || !reflect.DeepEqual(got, want) {
			t.Fatalf("ReferencedPaths() = %q, %v; want %q", got, err, want)
		}
	})
	for _, record := range records {
		t.Run(record.name, func(t *testing.T) {
			file, path, err := OpenReferencedFile(root, Reference{FileID: record.fileID})
			if file != nil {
				defer file.Close()
			}
			if record.path == "" {
				if err == nil || file != nil || path != "" {
					t.Fatalf("OpenReferencedFile(%q) returned path %q and error %v; want rejection", record.fileID, path, err)
				}
				return
			}
			want := filepath.Join(root, filepath.FromSlash(record.path))
			if err != nil || file == nil || path != want {
				t.Fatalf("OpenReferencedFile(%q) returned path %q and error %v; want %q", record.fileID, path, err, want)
			}
			got, err := io.ReadAll(file)
			if err != nil || string(got) != record.content {
				t.Fatalf("read file = %q, %v; want %q", got, err, record.content)
			}
		})
	}
}
