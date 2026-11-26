package object

import (
	"os"
	"path/filepath"
	"testing"
)

func TestSourceFileRetainsSelectedBytesAcrossPathReplacement(t *testing.T) {
	path := filepath.Join(t.TempDir(), "source.dcm")
	if err := os.WriteFile(path, []byte("selected"), 0600); err != nil {
		t.Fatal(err)
	}
	f, err := OpenSourceFile(path)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	if err := os.Rename(path, path+".held"); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte("replaced"), 0600); err != nil {
		t.Fatal(err)
	}
	data := make([]byte, 8)
	if _, err := f.ReadAt(data, 0); err != nil || string(data) != "selected" {
		t.Fatalf("selected source changed: %q %v", data, err)
	}
	if err := os.Remove(path + ".held"); err != nil {
		t.Fatal(err)
	}
	if _, err := f.ReadAt(data, 0); err != nil || string(data) != "selected" {
		t.Fatalf("removed source was lost: %q %v", data, err)
	}
}
