package main

import (
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"

	"github.com/ThalesMMS/dicom-go/encapimage"
	"github.com/ThalesMMS/dicom-go/internal/clifile"
	"github.com/ThalesMMS/dicom-go/object"
)

func samePath(a, b string) bool {
	aa, e := filepath.Abs(a)
	bb, f := filepath.Abs(b)
	return e == nil && f == nil && filepath.Clean(aa) == filepath.Clean(bb) || clifile.SameFile(a, b)
}
func readInput(ctx context.Context, path string, maxBytes int64) ([]byte, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	initial, e := os.Stat(path)
	if e != nil {
		return nil, e
	}
	if !initial.Mode().IsRegular() {
		return nil, errors.New("img2dcm: input must be a regular file")
	}
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil {
		return nil, err
	}
	if !info.Mode().IsRegular() {
		return nil, errors.New("img2dcm: input must be a regular file")
	}
	if info.Size() < 0 || info.Size() > maxBytes {
		return nil, encapimage.ErrImportLimit
	}
	data := make([]byte, int(info.Size()))
	for at := 0; at < len(data); {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		end := at + 32<<10
		if end > len(data) {
			end = len(data)
		}
		n, err := io.ReadFull(f, data[at:end])
		at += n
		if err != nil {
			return nil, err
		}
	}
	var extra [1]byte
	if n, err := f.Read(extra[:]); n != 0 || err != io.EOF {
		return nil, errors.New("img2dcm: input changed while reading")
	}
	return data, ctx.Err()
}

type contextSource struct {
	ctx context.Context
	f   *os.File
}

func (s contextSource) Read(p []byte) (int, error) {
	if err := s.ctx.Err(); err != nil {
		return 0, err
	}
	return s.f.Read(p)
}
func (s contextSource) Seek(offset int64, whence int) (int64, error) {
	if err := s.ctx.Err(); err != nil {
		return 0, err
	}
	return s.f.Seek(offset, whence)
}
func readSource(ctx context.Context, path string) (*object.File, func(), error) {
	initial, e := os.Stat(path)
	if e != nil {
		return nil, nil, e
	}
	if !initial.Mode().IsRegular() {
		return nil, nil, errors.New("img2dcm: source must be a regular file")
	}
	f, err := os.Open(path)
	if err != nil {
		return nil, nil, err
	}
	closeSource := func() { _ = f.Close() }
	info, err := f.Stat()
	if err != nil {
		closeSource()
		return nil, nil, err
	}
	if !info.Mode().IsRegular() || info.Size() > 256<<20 {
		closeSource()
		return nil, nil, encapimage.ErrImportLimit
	}
	source, err := object.ReadFileWithOptions(contextSource{ctx: ctx, f: f}, object.ReadFileOptions{MaxElementBytes: 16 << 20, MaxPixelDataBytes: 256 << 20, MaxTotalBytes: 256 << 20, MaxSequenceDepth: 32, MaxElements: 100000, MaxFragments: 4096, DeferPixelData: true})
	if err != nil {
		closeSource()
		return nil, nil, err
	}
	return source, closeSource, nil
}
func objectCharsetOptions() object.CharsetConversionOptions { return object.CharsetConversionOptions{} }

type contextOutput struct {
	ctx context.Context
	w   io.Writer
}

func (w contextOutput) Write(p []byte) (int, error) {
	if err := w.ctx.Err(); err != nil {
		return 0, err
	}
	return w.w.Write(p)
}
func writePart10Context(ctx context.Context, w io.Writer, f *object.File) error {
	if err := object.WriteFile(contextOutput{ctx: ctx, w: w}, f); err != nil {
		return err
	}
	return ctx.Err()
}
