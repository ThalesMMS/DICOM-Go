package netstore

import (
	"context"
	"errors"
	"github.com/ThalesMMS/dicom-go/object"
	"github.com/ThalesMMS/dicom-go/transfer"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"
)

func relativeSaveTest(ctx context.Context, root, relative string, ops saveOperations) (string, error) {
	s, err := beginStream(ctx, root, "1.2.3.4", ops)
	if err != nil {
		return "", err
	}
	if err = ops.write(s, part10File(validDataSet(), transfer.ExplicitVRLittleEndian, "1.2.3", "1.2.3.4")); err != nil {
		return "", s.abort(failure("write instance", err))
	}
	return s.CommitRelative(relative)
}
func relativeFiles(t *testing.T, root string) []string {
	t.Helper()
	var files []string
	err := filepath.WalkDir(root, func(p string, e os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if !e.IsDir() {
			files = append(files, p)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return files
}
func TestRelativeConcurrentPublicationNeverOverwrites(t *testing.T) {
	root := t.TempDir()
	const n = 16
	var wg sync.WaitGroup
	paths := make(chan string, n)
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			p, err := SavePart10RelativeWithOptions(context.Background(), root, "study/series/image.dcm", validDataSet(), transfer.ExplicitVRLittleEndian, SaveOptions{})
			if err != nil {
				t.Error(err)
				return
			}
			paths <- p
		}()
	}
	wg.Wait()
	close(paths)
	seen := map[string]bool{}
	for p := range paths {
		if seen[p] {
			t.Fatal("duplicate path")
		}
		seen[p] = true
		f, err := object.OpenFile(p)
		if err != nil {
			t.Fatal(err)
		}
		f.Close()
		if runtime.GOOS != "windows" {
			info, _ := os.Stat(p)
			if info.Mode().Perm() != 0600 {
				t.Fatalf("privacy %v", info.Mode())
			}
		}
	}
	if len(seen) != n || len(relativeFiles(t, root)) != n {
		t.Fatalf("published %d", len(seen))
	}
	for _, suffix := range []string{"", ".1", ".15"} {
		if !seen[filepath.Join(root, "study", "series", "image"+suffix+".dcm")] {
			t.Fatalf("collision suffix %q absent", suffix)
		}
	}
}
func TestRelativeRejectsUnsafeTargetsBeforePublication(t *testing.T) {
	for _, target := range []string{"../outside.dcm", "/absolute.dcm", "a/../b", "a//b", "CON/file", "a/" + strings.Repeat("x", 252), "a\\b"} {
		root := t.TempDir()
		if p, err := SavePart10RelativeWithOptions(context.Background(), root, target, validDataSet(), transfer.ExplicitVRLittleEndian, SaveOptions{}); p != "" || err == nil {
			t.Fatalf("accepted %q/%v", p, err)
		}
		if len(relativeFiles(t, root)) != 0 {
			t.Fatal("invalid target created files")
		}
	}
	root := t.TempDir()
	outside := t.TempDir()
	if err := os.Symlink(outside, filepath.Join(root, "study")); err != nil {
		t.Skipf("symlink: %v", err)
	}
	if p, err := SavePart10RelativeWithOptions(context.Background(), root, "study/image.dcm", validDataSet(), transfer.ExplicitVRLittleEndian, SaveOptions{}); p != "" || err == nil {
		t.Fatalf("followed link %q/%v", p, err)
	}
	if len(relativeFiles(t, outside)) != 0 {
		t.Fatal("outside changed")
	}
}
func TestRelativeFaultsAndPublishedOutcome(t *testing.T) {
	for _, stage := range []string{"write", "sync", "close", "create-parent-sync", "publish", "cancel-close", "post-publish", "publication-sync"} {
		t.Run(stage, func(t *testing.T) {
			root := t.TempDir()
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			ops := defaultSaveOperations()
			injected := errors.New("sensitive-remote-UID")
			if stage == "publication-sync" {
				if err := os.Mkdir(filepath.Join(root, "study"), 0700); err != nil {
					t.Fatal(err)
				}
			}
			switch stage {
			case "write":
				ops.write = func(w io.Writer, _ *object.File) error { w.Write([]byte("PARTIAL")); return injected }
			case "sync":
				ops.sync = func(*os.File) error { return injected }
			case "close":
				ops.close = func(f *os.File) error { return errors.Join(f.Close(), injected) }
			case "create-parent-sync", "publication-sync":
				if runtime.GOOS == "windows" {
					t.Skip("no directory fsync contract")
				}
				ops.syncDirectory = func(*os.File) error { return injected }
			case "publish":
				ops.publishRelative = func(*os.File, *os.File, string, string, os.FileInfo) (bool, error) { return false, injected }
			case "cancel-close":
				ops.close = func(f *os.File) error { cancel(); return f.Close() }
			case "post-publish":
				real := ops.publishRelative
				ops.publishRelative = func(a, b *os.File, s, d string, i os.FileInfo) (bool, error) {
					ok, err := real(a, b, s, d, i)
					if err != nil {
						return ok, err
					}
					return ok, injected
				}
			}
			p, err := relativeSaveTest(ctx, root, "study/image.dcm", ops)
			if err == nil {
				t.Fatal("fault accepted")
			}
			if strings.Contains(err.Error(), "sensitive") || strings.Contains(err.Error(), root) {
				t.Fatal("unredacted error")
			}
			published := stage == "post-publish" || stage == "publication-sync"
			var detail *Error
			if published {
				if p == "" || !errors.As(err, &detail) || !detail.Published || len(relativeFiles(t, root)) != 1 {
					t.Fatalf("lost published outcome %q/%v", p, err)
				}
			} else if p != "" || len(relativeFiles(t, root)) != 0 {
				t.Fatalf("failed store visible %q/%v", p, err)
			}
		})
	}
}
func TestRelativeChildIdentitySwapAndCollisions(t *testing.T) {
	root := t.TempDir()
	ops := defaultSaveOperations()
	realClose := ops.close
	ops.close = func(f *os.File) error {
		if err := realClose(f); err != nil {
			return err
		}
		if err := os.Rename(filepath.Join(root, "study"), filepath.Join(root, "moved")); err != nil {
			t.Fatal(err)
		}
		return os.Mkdir(filepath.Join(root, "study"), 0700)
	}
	p, err := relativeSaveTest(context.Background(), root, "study/image.dcm", ops)
	if p != "" || !errors.Is(err, ErrUnsafeDirectory) || len(relativeFiles(t, root)) != 0 {
		t.Fatalf("child swap %q/%v", p, err)
	}
	root = t.TempDir()
	ops = defaultSaveOperations()
	attempts := 0
	ops.publishRelative = func(*os.File, *os.File, string, string, os.FileInfo) (bool, error) {
		attempts++
		return false, os.ErrExist
	}
	p, err = relativeSaveTest(context.Background(), root, "study/image.dcm", ops)
	if p != "" || !errors.Is(err, ErrCollisionLimit) || attempts != 1000 || len(relativeFiles(t, root)) != 0 {
		t.Fatalf("collision %q/%v/%d", p, err, attempts)
	}
}
