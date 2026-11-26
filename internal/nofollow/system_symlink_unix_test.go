//go:build aix || darwin || dragonfly || freebsd || linux || netbsd || openbsd || solaris

package nofollow

import (
	"os"
	"path/filepath"
	"syscall"
	"testing"
)

// rootOwnedSymlink reports whether path is a symbolic link owned by root, such
// as macOS /tmp and /var.
func rootOwnedSymlink(path string) bool {
	info, err := os.Lstat(path)
	if err != nil || info.Mode()&os.ModeSymlink == 0 {
		return false
	}
	stat, ok := info.Sys().(*syscall.Stat_t)
	return ok && stat.Uid == 0
}

func TestOpenFollowsRootOwnedSystemSymlinks(t *testing.T) {
	if !rootOwnedSymlink("/tmp") {
		t.Skip("/tmp is not a root-owned symlink on this system")
	}
	dir, err := os.MkdirTemp("/tmp", "nofollow-system-")
	if err != nil {
		t.Skipf("cannot create a directory under /tmp: %v", err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	if err := os.WriteFile(filepath.Join(dir, "f"), []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	parent, err := OpenDirectory(dir)
	if err != nil {
		t.Fatalf("OpenDirectory(%s) through the /tmp system symlink: %v", dir, err)
	}
	defer parent.Close()
	file, err := OpenFile(filepath.Join(dir, "f"))
	if err != nil {
		t.Fatalf("OpenFile through the /tmp system symlink: %v", err)
	}
	_ = file.Close()
}

func TestOpenRefusesUserOwnedSymlinks(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("symlinks created by root are system symlinks by definition")
	}
	base := t.TempDir()
	real := filepath.Join(base, "real")
	if err := os.Mkdir(real, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(real, "f"), []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	alias := filepath.Join(base, "alias")
	if err := os.Symlink(real, alias); err != nil {
		t.Skipf("symlink unavailable: %v", err)
	}
	if file, err := OpenDirectory(alias); err == nil {
		_ = file.Close()
		t.Fatal("OpenDirectory followed a user-owned directory symlink")
	}
	if file, err := OpenFile(filepath.Join(alias, "f")); err == nil {
		_ = file.Close()
		t.Fatal("OpenFile followed a user-owned ancestor symlink")
	}
	if err := os.Symlink(filepath.Join(real, "f"), filepath.Join(real, "link")); err != nil {
		t.Fatal(err)
	}
	if file, err := OpenFile(filepath.Join(real, "link")); err == nil {
		_ = file.Close()
		t.Fatal("OpenFile followed a final-component symlink")
	}
}
