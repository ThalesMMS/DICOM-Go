package privatefs

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
)

func TestPrivateDirectoryAndAtomicReplacement(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "spool")
	if err := CreateDirectory(dir); err != nil {
		t.Fatal(err)
	}
	if err := Check(dir); err != nil {
		t.Fatal(err)
	}
	if err := CreateDirectory(dir); err != nil {
		t.Fatal("private directory could not be reopened", err)
	}
	target := filepath.Join(dir, "state")
	for _, contents := range []string{"first", "replacement"} {
		file, err := os.CreateTemp(dir, "pending-*")
		if err != nil {
			t.Fatal(err)
		}
		path := file.Name()
		if err := Secure(path); err != nil {
			file.Close()
			t.Fatal(err)
		}
		if err := Check(path); err != nil {
			file.Close()
			t.Fatal("temporary data must be protected before writing", err)
		}
		_, writeErr := file.WriteString(contents)
		err = errors.Join(writeErr, file.Sync(), file.Close())
		if err != nil {
			t.Fatal(err)
		}
		if err := Replace(path, target); err != nil {
			t.Fatal(err)
		}
		if err := SyncDirectory(dir); err != nil {
			t.Fatal(err)
		}
		if err := Check(target); err != nil {
			t.Fatal("publication lost native privacy", err)
		}
		got, err := os.ReadFile(target)
		if err != nil || string(got) != contents {
			t.Fatalf("published %q: %v", got, err)
		}
	}
	if err := CreateDirectory(target); err == nil {
		t.Fatal("regular file admitted as a directory")
	}
}
