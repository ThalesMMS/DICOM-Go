package nofollow

import (
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"testing"
)

func TestDirectoryAtPrivateCreationAndPublishBetween(t *testing.T) {
	root := t.TempDir()
	parent, err := OpenDirectory(root)
	if err != nil {
		t.Fatal(err)
	}
	defer parent.Close()
	child, created, err := OpenDirectoryAt(parent, "nested", true)
	if err != nil || !created {
		t.Fatalf("create %v/%v", created, err)
	}
	defer child.Close()
	again, created, err := OpenDirectoryAt(parent, "nested", true)
	if err != nil || created {
		t.Fatalf("reopen %v/%v", created, err)
	}
	defer again.Close()
	a, _ := child.Stat()
	b, _ := again.Stat()
	if !os.SameFile(a, b) {
		t.Fatal("child identity changed")
	}
	if runtime.GOOS != "windows" && a.Mode().Perm() != 0700 {
		t.Fatalf("child privacy %v", a.Mode())
	}
	temp, err := CreateAt(parent, "private")
	if err != nil {
		t.Fatal(err)
	}
	if _, err = temp.WriteString("COMPLETE"); err != nil {
		t.Fatal(err)
	}
	if err = temp.Sync(); err != nil {
		t.Fatal(err)
	}
	info, err := temp.Stat()
	if err != nil {
		t.Fatal(err)
	}
	if err = temp.Close(); err != nil {
		t.Fatal(err)
	}
	published, err := PublishClosedBetweenAt(parent, child, "private", "result", info)
	if err != nil || !published {
		t.Fatalf("publication %v/%v", published, err)
	}
	if b, err := os.ReadFile(filepath.Join(root, "nested", "result")); err != nil || string(b) != "COMPLETE" {
		t.Fatalf("result %q/%v", b, err)
	}
	// Existing targets never change, including links when the platform permits them.
	temp, err = CreateAt(parent, "second")
	if err != nil {
		t.Fatal(err)
	}
	info, _ = temp.Stat()
	temp.Close()
	if published, err = PublishClosedBetweenAt(parent, child, "second", "result", info); published || !errors.Is(err, os.ErrExist) {
		t.Fatalf("overwrite %v/%v", published, err)
	}
}
func TestDirectoryAtRejectsNamesAndLinks(t *testing.T) {
	root := t.TempDir()
	parent, err := OpenDirectory(root)
	if err != nil {
		t.Fatal(err)
	}
	defer parent.Close()
	for _, name := range []string{"", ".", "..", "a/b"} {
		if f, _, err := OpenDirectoryAt(parent, name, true); err == nil {
			f.Close()
			t.Fatalf("accepted %q", name)
		}
	}
	if err = os.WriteFile(filepath.Join(root, "file"), []byte("KEEP"), 0600); err != nil {
		t.Fatal(err)
	}
	if f, _, err := OpenDirectoryAt(parent, "file", true); err == nil {
		f.Close()
		t.Fatal("opened file as directory")
	}
	outside := t.TempDir()
	if err = os.Symlink(outside, filepath.Join(root, "link")); err != nil {
		t.Skipf("symlink privilege: %v", err)
	}
	for _, create := range []bool{false, true} {
		if f, _, err := OpenDirectoryAt(parent, "link", create); err == nil {
			f.Close()
			t.Fatal("followed child link")
		}
	}
	if entries, err := os.ReadDir(outside); err != nil || len(entries) != 0 {
		t.Fatalf("outside changed %v/%v", entries, err)
	}
}
