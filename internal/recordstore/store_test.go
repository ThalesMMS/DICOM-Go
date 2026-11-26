package recordstore

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
)

var testOptions = Options{MaxRecords: 4, MaxRecordBytes: 128, MaxTotalBytes: 256}

func key(n int) string { return strings.Repeat(string(rune('a'+n)), 64) + ".json" }
func openTest(t *testing.T, dir string) *Store {
	t.Helper()
	s, err := Open(context.Background(), dir, testOptions)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	return s
}

func TestStoreReplayCASQuotaAndOwnership(t *testing.T) {
	ctx := context.Background()
	dir := filepath.Join(t.TempDir(), "records")
	s := openTest(t, dir)
	if _, err := Open(ctx, dir, testOptions); !errors.Is(err, ErrUnavailable) {
		t.Fatalf("second publisher: %v", err)
	}
	created, err := s.Create(ctx, key(0), []byte("old"))
	if err != nil || !created {
		t.Fatalf("create %v %v", created, err)
	}
	if created, err := s.Create(ctx, key(0), []byte("different")); err != nil || created {
		t.Fatalf("duplicate %v %v", created, err)
	}
	if err := s.Replace(ctx, key(0), []byte("new")); err != nil {
		t.Fatal(err)
	}
	if err := s.Replace(ctx, key(1), []byte("x")); !errors.Is(err, ErrNotFound) {
		t.Fatalf("replace missing %v", err)
	}
	if _, err := s.Create(ctx, "../escape", []byte("x")); !errors.Is(err, ErrInvalid) {
		t.Fatal(err)
	}
	if _, err := s.Create(ctx, key(1), make([]byte, 129)); !errors.Is(err, ErrLimit) {
		t.Fatal(err)
	}
	for _, k := range []string{key(1), key(2)} {
		if _, err := s.Create(ctx, k, make([]byte, 126)); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := s.Create(ctx, key(3), []byte("too big")); !errors.Is(err, ErrLimit) {
		t.Fatal(err)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	s = openTest(t, dir)
	data, err := s.Read(ctx, key(0))
	if err != nil || string(data) != "new" {
		t.Fatalf("replay %q %v", data, err)
	}
	data[0] = 'X'
	again, _ := s.Read(ctx, key(0))
	if string(again) != "new" {
		t.Fatal("borrowed bytes")
	}
	if err := s.Delete(ctx, key(1)); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Create(ctx, key(3), []byte("reclaimed")); err != nil {
		t.Fatal(err)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Keys(ctx); !errors.Is(err, ErrUnavailable) {
		t.Fatal(err)
	}
}

func TestStorePublicationFailures(t *testing.T) {
	for _, stage := range []string{"file-sync", "directory-sync"} {
		t.Run(stage, func(t *testing.T) {
			ctx := context.Background()
			dir := t.TempDir()
			s := openTest(t, dir)
			failure := errors.New("injected IO failure")
			if stage == "file-sync" {
				s.syncFile = func(*os.File) error { return failure }
			} else {
				s.syncDir = func(*os.File) error { return failure }
			}
			if _, err := s.Create(ctx, key(0), []byte("complete")); !errors.Is(err, ErrUnavailable) {
				t.Fatal(err)
			}
			_, statErr := os.Stat(filepath.Join(dir, key(0)))
			if stage == "file-sync" {
				if !errors.Is(statErr, os.ErrNotExist) {
					t.Fatalf("published before sync: %v", statErr)
				}
				if _, err := s.Keys(ctx); err != nil {
					t.Fatal(err)
				}
			} else {
				if statErr != nil {
					t.Fatal(statErr)
				}
				if _, err := s.Keys(ctx); !errors.Is(err, ErrUnavailable) {
					t.Fatalf("not poisoned: %v", err)
				}
			}
			s.Close()
			reopened := openTest(t, dir)
			keys, err := reopened.Keys(ctx)
			if err != nil {
				t.Fatal(err)
			}
			want := 0
			if stage == "directory-sync" {
				want = 1
			}
			if len(keys) != want {
				t.Fatalf("restart records %d want %d", len(keys), want)
			}
			entries, _ := os.ReadDir(dir)
			for _, entry := range entries {
				if strings.HasPrefix(entry.Name(), tempPrefix) {
					t.Fatal("temporary leaked")
				}
			}
		})
	}
}

func TestStoreConcurrentCreateAndCancellation(t *testing.T) {
	s := openTest(t, t.TempDir())
	ctx := context.Background()
	var wg sync.WaitGroup
	var created int
	var mu sync.Mutex
	for i := 0; i < 12; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			ok, err := s.Create(ctx, key(0), []byte("one"))
			if err != nil {
				t.Error(err)
			}
			if ok {
				mu.Lock()
				created++
				mu.Unlock()
			}
		}()
	}
	wg.Wait()
	if created != 1 {
		t.Fatal(created)
	}
	canceled, cancel := context.WithCancel(ctx)
	cancel()
	if _, err := s.Create(canceled, key(1), []byte("x")); !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
}

func TestStoreRefusesMalformedEntriesAndOutsideSymlink(t *testing.T) {
	for _, kind := range []string{"symlink", "directory", "oversized", "unexpected", "bad-temp"} {
		t.Run(kind, func(t *testing.T) {
			dir := t.TempDir()
			path := filepath.Join(dir, key(0))
			switch kind {
			case "symlink":
				if err := os.Symlink(filepath.Join(t.TempDir(), "missing"), path); err != nil {
					t.Skip("symlink creation unavailable")
				}
			case "directory":
				os.Mkdir(path, 0700)
			case "oversized":
				os.WriteFile(path, make([]byte, 129), 0600)
			case "unexpected":
				os.WriteFile(filepath.Join(dir, "unknown"), []byte("x"), 0600)
			case "bad-temp":
				os.WriteFile(filepath.Join(dir, tempPrefix+strings.Repeat("Z", 32)), []byte("x"), 0600)
			}
			if s, err := Open(context.Background(), dir, testOptions); err == nil {
				s.Close()
				t.Fatal("unsafe entry admitted")
			}
		})
	}
	outer := t.TempDir()
	outside := t.TempDir()
	alias := filepath.Join(outer, "alias")
	if err := os.Symlink(outside, alias); err != nil {
		t.Skip("symlink unavailable")
	}
	if s, err := Open(context.Background(), filepath.Join(alias, "must-not-create"), testOptions); err == nil {
		s.Close()
		t.Fatal("symlink parent admitted")
	}
	if _, err := os.Stat(filepath.Join(outside, "must-not-create")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("outside side effect: %v", err)
	}
}

func TestStoreUnpublishedTempAndRootReplacement(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "records")
	s := openTest(t, dir)
	s.Close()
	temp := filepath.Join(dir, tempPrefix+strings.Repeat("0", 32))
	if err := os.WriteFile(temp, []byte("incomplete"), 0600); err != nil {
		t.Fatal(err)
	}
	s = openTest(t, dir)
	if _, err := os.Stat(temp); !errors.Is(err, os.ErrNotExist) {
		t.Fatal(err)
	}
	if err := os.Rename(dir, dir+"-old"); err != nil {
		t.Skip("directory rename unavailable")
	}
	if err := os.Mkdir(dir, 0700); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Create(context.Background(), key(0), []byte("x")); !errors.Is(err, ErrUnavailable) {
		t.Fatalf("root replacement %v", err)
	}
}
