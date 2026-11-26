package dicomconv

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"syscall"
	"testing"
)

// errPrivilegeNotHeld is Windows ERROR_PRIVILEGE_NOT_HELD.
const errPrivilegeNotHeld = syscall.Errno(1314)

// linkOrSkip creates a symbolic link, skipping only when Windows refuses the
// privilege (no Developer Mode or elevation).
func linkOrSkip(t *testing.T, target, link string) {
	t.Helper()
	if err := os.Symlink(target, link); err != nil {
		if runtime.GOOS == "windows" && errors.Is(err, errPrivilegeNotHeld) {
			t.Skipf("creating symbolic links needs Developer Mode or elevation: %v", err)
		}
		t.Fatal(err)
	}
}

func TestBulkDataURIRejectsLinksLeavingTheDocumentDirectory(t *testing.T) {
	ctx := context.Background()
	dir, outside := t.TempDir(), t.TempDir()
	if err := os.WriteFile(filepath.Join(outside, "secret.bin"), []byte{1, 2, 3, 4}, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "inside.bin"), []byte{5, 6, 7, 8}, 0o600); err != nil {
		t.Fatal(err)
	}
	uris := []string{"missing.bin"}
	if runtime.GOOS == "windows" {
		// A directory junction needs no privilege.
		if out, err := exec.Command("cmd", "/c", "mklink", "/J", filepath.Join(dir, "junction"), outside).CombinedOutput(); err != nil {
			t.Fatalf("mklink /J: %v %s", err, out)
		}
		uris = append(uris, "junction/secret.bin")
	}
	check := func(uris []string) {
		t.Helper()
		for _, uri := range uris {
			if _, err := resolveBulkPath(dir, uri); !errors.Is(err, ErrBulkDataURI) {
				t.Errorf("%q resolved: %v", uri, err)
			}
			document := `{"00291001":{"vr":"OB","BulkDataURI":"` + uri + `"}}`
			if _, err := Decode(ctx, []byte(document), JSON, DecodeOptions{BaseDir: dir}); !errors.Is(err, ErrBulkDataURI) {
				t.Errorf("JSON %q: %v", uri, err)
			}
			xmlDocument := `<?xml version="1.0" encoding="UTF-8"?><NativeDicomModel xml:space="preserve" xmlns="http://dicom.nema.org/PS3.19/models/NativeDICOM"><DicomAttribute tag="00420011" vr="OB" keyword="EncapsulatedDocument"><BulkData uri="` + uri + `"/></DicomAttribute></NativeDicomModel>`
			if _, err := Decode(ctx, []byte(xmlDocument), XML, DecodeOptions{BaseDir: dir}); !errors.Is(err, ErrBulkDataURI) {
				t.Errorf("XML %q: %v", uri, err)
			}
		}
	}
	check(uris)
	linkOrSkip(t, filepath.Join(outside, "secret.bin"), filepath.Join(dir, "file-link.bin"))
	linkOrSkip(t, outside, filepath.Join(dir, "dir-link"))
	linkOrSkip(t, filepath.Join(dir, "inside.bin"), filepath.Join(dir, "inside-link.bin"))
	check([]string{"file-link.bin", "dir-link/secret.bin", "dir-link/missing.bin"})
	for _, uri := range []string{"inside.bin", "inside-link.bin"} {
		resolved, err := resolveBulkPath(dir, uri)
		if err != nil {
			t.Fatalf("%q: %v", uri, err)
		}
		if content, err := os.ReadFile(resolved); err != nil || string(content) != "\x05\x06\x07\x08" {
			t.Fatalf("%q: %v %v", uri, content, err)
		}
	}
}

// TestBulkDataFilesHonourMaxBulkBytes checks that both converter paths refuse
// a BulkData file above MaxBulkBytes (#2043).
func TestBulkDataFilesHonourMaxBulkBytes(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "bulk.bin"), []byte{1, 2, 3, 4, 5, 6, 7, 8}, 0o600); err != nil {
		t.Fatal(err)
	}
	jsonDocument := `{"00291001":{"vr":"OB","BulkDataURI":"bulk.bin"}}`
	xmlDocument := `<?xml version="1.0" encoding="UTF-8"?><NativeDicomModel xml:space="preserve" xmlns="http://dicom.nema.org/PS3.19/models/NativeDICOM"><DicomAttribute tag="00420011" vr="OB" keyword="EncapsulatedDocument"><BulkData uri="bulk.bin"/></DicomAttribute></NativeDicomModel>`
	for _, tc := range []struct {
		name     string
		document string
		format   Format
	}{{"JSON", jsonDocument, JSON}, {"XML", xmlDocument, XML}} {
		if _, err := Decode(ctx, []byte(tc.document), tc.format, DecodeOptions{BaseDir: dir, MaxBulkBytes: 4}); !errors.Is(err, ErrBulkDataURI) {
			t.Errorf("%s with MaxBulkBytes 4: %v, want ErrBulkDataURI", tc.name, err)
		}
		if _, err := Decode(ctx, []byte(tc.document), tc.format, DecodeOptions{BaseDir: dir, MaxBulkBytes: 8}); err != nil {
			t.Errorf("%s with MaxBulkBytes 8: %v", tc.name, err)
		}
	}
}
