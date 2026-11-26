package main

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"

	"github.com/ThalesMMS/dicom-go/internal/nofollow"
	"github.com/ThalesMMS/dicom-go/object"
	"github.com/ThalesMMS/dicom-go/transfer"
)

const maxExtractFiles = 10000
const maxExtractResponseBytes = int64(16 << 20)
const maxExtractTotalBytes = int64(256 << 20)

var errExtraction = errors.New("response extraction failed")

type findExtractor struct {
	dir   *os.File
	count int
	bytes int64
}

func newFindExtractor(path string) (*findExtractor, error) {
	dir, err := nofollow.OpenDirectory(path)
	if err != nil {
		return nil, errExtraction
	}
	return &findExtractor{dir: dir}, nil
}
func (x *findExtractor) Close() error { return x.dir.Close() }

// Write publishes raw ExplicitLE only. C-FIND Identifiers are not SOP
// instances, so neither a new SOP identity nor File Meta is fabricated.
func (x *findExtractor) Write(ctx context.Context, id *object.Object) (result error) {
	if err := ctx.Err(); err != nil {
		return err
	}
	if x.count >= maxExtractFiles {
		return errExtraction
	}
	var token [16]byte
	if _, err := rand.Read(token[:]); err != nil {
		return errExtraction
	}
	temporary := ".findscu-" + hex.EncodeToString(token[:]) + ".partial"
	f, err := nofollow.CreateAt(x.dir, temporary)
	if err != nil {
		return errExtraction
	}
	closed := false
	defer func() {
		if !closed {
			f.Close()
		}
		if err := nofollow.RemoveAt(x.dir, temporary); err != nil {
			result = errors.Join(result, errExtraction)
		}
	}()
	limit := maxExtractTotalBytes - x.bytes
	if limit > maxExtractResponseBytes {
		limit = maxExtractResponseBytes
	}
	w := &extractWriter{ctx: ctx, dst: f, limit: limit}
	if err := object.WriteDataSet(w, id, transfer.ExplicitVRLittleEndian); err != nil {
		return errors.Join(errExtraction, err)
	}
	if err := f.Sync(); err != nil {
		return errExtraction
	}
	info, err := f.Stat()
	if err != nil {
		return errExtraction
	}
	if err := f.Close(); err != nil {
		return errExtraction
	}
	closed = true
	if err := ctx.Err(); err != nil {
		return err
	}
	name := fmt.Sprintf("rsp%06d.dcm", x.count+1)
	published, err := nofollow.PublishClosedAt(x.dir, temporary, name, info)
	if published {
		x.count++
		x.bytes += w.n
	}
	if err != nil {
		return errExtraction
	}
	return nil
}

type extractWriter struct {
	ctx      context.Context
	dst      io.Writer
	limit, n int64
}

func (w *extractWriter) Write(p []byte) (int, error) {
	if err := w.ctx.Err(); err != nil {
		return 0, err
	}
	if int64(len(p)) > w.limit-w.n {
		return 0, errExtraction
	}
	// Bounded cancellation points for a large primitive value.
	total := 0
	for len(p) > 0 {
		if err := w.ctx.Err(); err != nil {
			return total, err
		}
		part := p
		if len(part) > 32<<10 {
			part = part[:32<<10]
		}
		n, err := w.dst.Write(part)
		w.n += int64(n)
		total += n
		p = p[n:]
		if err != nil {
			return total, err
		}
		if n != len(part) {
			return total, io.ErrShortWrite
		}
	}
	return total, nil
}
