// Package wrappercli holds finite IO and process lifecycle for small tools.
package wrappercli

import (
	"bytes"
	"context"
	"errors"
	"flag"
	"fmt"
	"github.com/ThalesMMS/dicom-go/core"
	clidiag "github.com/ThalesMMS/dicom-go/internal/clidiag/file"
	"github.com/ThalesMMS/dicom-go/internal/nofollow"
	"github.com/ThalesMMS/dicom-go/internal/privatefs"
	"github.com/ThalesMMS/dicom-go/object"
	"github.com/ThalesMMS/dicom-go/transfer"
	"io"
	"os"
	"os/signal"
	"path/filepath"
)

const MaxInputBytes int64 = 64 << 20
const MaxFiles = 128

var ErrUsage = errors.New("invalid command arguments")

type Execute func(context.Context, string, []string, io.Writer, io.Writer) error

func Main(tool string, execute Execute) {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()
	os.Exit(Run(ctx, tool, os.Args[1:], os.Stdout, os.Stderr, execute))
}
func Run(ctx context.Context, tool string, args []string, out, errout io.Writer, execute Execute) int {
	err := execute(ctx, tool, args, out, errout)
	if err == nil || errors.Is(err, flag.ErrHelp) {
		return 0
	}
	clidiag.Fprintln(errout, tool, err)
	if errors.Is(err, ErrUsage) {
		return 2
	}
	return 1
}
func Flags(tool, usage string, errout io.Writer) *flag.FlagSet {
	f := flag.NewFlagSet(tool, flag.ContinueOnError)
	f.SetOutput(errout)
	f.Usage = func() { fmt.Fprintf(errout, "Usage: %s %s\n\nFlags:\n", tool, usage); f.PrintDefaults() }
	return f
}
func Parse(f *flag.FlagSet, args []string, min, max int) error {
	if err := f.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return err
		}
		return ErrUsage
	}
	if f.NArg() < min || f.NArg() > max {
		f.Usage()
		return ErrUsage
	}
	return nil
}
func Read(path string, max int64) ([]byte, error) {
	if max <= 0 || max > MaxInputBytes {
		return nil, ErrUsage
	}
	f, err := nofollow.OpenFile(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil {
		return nil, err
	}
	if !info.Mode().IsRegular() {
		return nil, errors.New("input must be a regular file")
	}
	if info.Size() > max {
		return nil, errors.New("input exceeds byte limit")
	}
	b, err := io.ReadAll(io.LimitReader(f, max+1))
	if err != nil {
		return nil, err
	}
	if int64(len(b)) > max {
		return nil, errors.New("input exceeds byte limit")
	}
	return b, nil
}
func ReadDICOM(path string, skipPixels bool) (*object.File, error) {
	b, err := Read(path, MaxInputBytes)
	if err != nil {
		return nil, err
	}
	return object.ReadFileWithOptions(bytes.NewReader(b), object.ReadFileOptions{MaxElementBytes: MaxInputBytes, MaxPixelDataBytes: MaxInputBytes, MaxTotalBytes: MaxInputBytes, MaxElements: 100000, MaxFragments: 100000, MaxSequenceDepth: 32, SkipPixelData: skipPixels})
}

// WriteNew publishes only after write/sync/close succeed. Link is create-only.
// The parent directory must be trusted; no descriptor-based symlink claim is made.
func WriteNew(path string, write func(io.Writer) error) error {
	temp, err := os.CreateTemp(filepath.Dir(path), ".dicom-go-wrapper-*")
	if err != nil {
		return err
	}
	defer os.Remove(temp.Name())
	if err := privatefs.Secure(temp.Name()); err != nil {
		temp.Close()
		return err
	}
	if err = write(&outputWriter{writer: temp, remaining: MaxInputBytes}); err != nil {
		temp.Close()
		return err
	}
	if err = temp.Sync(); err != nil {
		temp.Close()
		return err
	}
	if err = temp.Close(); err != nil {
		return err
	}
	return os.Link(temp.Name(), path)
}
func WriteFile(path string, file *object.File) error {
	return WriteNew(path, func(w io.Writer) error { return object.WriteFile(w, file) })
}
func DatasetFile(ds *object.Object) (*object.File, error) {
	f := &object.File{Dataset: ds, TransferSyntax: transfer.ExplicitVRLittleEndian}
	if err := f.RebuildFileMeta(); err != nil {
		return nil, err
	}
	return f, nil
}
func UID() (string, error) { return core.NewUID() }

type outputWriter struct {
	writer    io.Writer
	remaining int64
}

func (w *outputWriter) Write(p []byte) (int, error) {
	if int64(len(p)) > w.remaining {
		return 0, errors.New("output exceeds 64 MiB")
	}
	n, err := w.writer.Write(p)
	w.remaining -= int64(n)
	return n, err
}
