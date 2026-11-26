package wrappercli

import (
	"bytes"
	"errors"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"testing"
	"time"

	"github.com/ThalesMMS/dicom-go/internal/privatefs"
)

func TestWriteNewNoOverwriteAndCleanup(t *testing.T) {
	dir := t.TempDir()
	target := filepath.Join(dir, "out")
	if err := WriteNew(target, func(w io.Writer) error { _, err := w.Write([]byte("complete")); return err }); err != nil {
		t.Fatal(err)
	}
	info, _ := os.Stat(target)
	if privatefs.Check(target) != nil {
		t.Fatalf("mode %o", info.Mode().Perm())
	}
	if err := WriteNew(target, func(w io.Writer) error { _, err := w.Write([]byte("replacement")); return err }); err == nil {
		t.Fatal("overwritten")
	}
	data, _ := os.ReadFile(target)
	if string(data) != "complete" {
		t.Fatal("old output changed")
	}
	bad := filepath.Join(dir, "bad")
	if err := WriteNew(bad, func(w io.Writer) error { w.Write([]byte("partial")); return errors.New("synthetic failure") }); err == nil {
		t.Fatal("accepted writer error")
	}
	if _, err := os.Stat(bad); !os.IsNotExist(err) {
		t.Fatal("partial output published")
	}
	entries, _ := os.ReadDir(dir)
	if len(entries) != 1 {
		t.Fatalf("staging leaked: %d entries", len(entries))
	}
}
func TestInputFileAndActualByteLimits(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "in")
	os.WriteFile(path, []byte("abc"), 0600)
	if _, err := Read(path, 2); err == nil {
		t.Fatal("accepted oversized actual bytes")
	}
	if _, err := Read(dir, 3); err == nil {
		t.Fatal("accepted directory")
	}
	var b bytes.Buffer
	w := &outputWriter{writer: &b, remaining: 2}
	if _, err := w.Write([]byte("abc")); err == nil || b.Len() != 0 {
		t.Fatal("output quota wrote partial chunk")
	}
}

func TestReadFIFORefusedWithoutWriter(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("Unix FIFO qualification")
	}
	fifo := filepath.Join(t.TempDir(), "fifo")
	if err := exec.Command("mkfifo", fifo).Run(); err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() { _, err := Read(fifo, 16); done <- err }()
	select {
	case err := <-done:
		if err == nil {
			t.Fatal("FIFO admitted")
		}
	case <-time.After(200 * time.Millisecond):
		// Release a pre-fix blocking reader so regression runs leave no goroutine.
		writer, err := os.OpenFile(fifo, os.O_WRONLY, 0600)
		if err == nil {
			writer.Close()
		}
		<-done
		t.Fatal("FIFO open blocked before regular-file admission")
	}
}
