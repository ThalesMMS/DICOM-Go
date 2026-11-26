package dcmdump

import (
	"bufio"
	"compress/flate"
	"compress/zlib"
	"context"
	"errors"
	"io"
	"os"

	"github.com/ThalesMMS/dicom-go/internal/clireadopts"
	"github.com/ThalesMMS/dicom-go/object"
	"github.com/ThalesMMS/dicom-go/transfer"
)

type dumpContextReader struct {
	ctx    context.Context
	reader io.Reader
}

func (r dumpContextReader) Read(p []byte) (int, error) {
	if err := r.ctx.Err(); err != nil {
		return 0, err
	}
	return r.reader.Read(p)
}

func openInflatedForDump(ctx context.Context, source *os.File, read object.ReadFileOptions) (*object.File, string, bool, func(), error) {
	noop := func() {}
	if _, err := source.Seek(0, io.SeekStart); err != nil {
		return nil, "", false, noop, err
	}
	header, err := object.ReadPart10Header(source, read)
	if err != nil {
		return nil, "", false, noop, err
	}
	if !header.TransferSyntax.Deflated {
		return nil, "", false, noop, errors.New("inflated spool requires dataset Deflate")
	}
	br := bufio.NewReader(source)
	head, _ := br.Peek(2)
	wrapped := len(head) == 2 && head[0]&15 == 8 && head[0]>>4 <= 7 && head[1]&32 == 0 && (uint16(head[0])<<8|uint16(head[1]))%31 == 0
	var inflated io.ReadCloser
	if wrapped && read.AllowZlibDeflate {
		inflated, err = zlib.NewReader(br)
	} else {
		inflated = flate.NewReader(br)
		wrapped = false
	}
	if err != nil {
		return nil, "", false, noop, err
	}
	defer inflated.Close()
	spool, err := os.CreateTemp("", "dicom-go-dump-inflated-*")
	if err != nil {
		return nil, "", false, noop, err
	}
	remove := func() { _ = os.Remove(spool.Name()) }
	limit := read.MaxTotalBytes
	if limit <= 0 {
		limit = 1 << 30
	}
	n, err := io.CopyBuffer(spool, io.LimitReader(dumpContextReader{ctx, inflated}, limit), make([]byte, 64<<10))
	if err == nil {
		var extra [1]byte
		extraN, e := (dumpContextReader{ctx, inflated}).Read(extra[:])
		if extraN != 0 {
			err = errors.New("inflated dataset byte limit exceeded")
		} else if e != io.EOF {
			if e == nil {
				err = io.ErrNoProgress
			} else {
				err = e
			}
		}
	}
	if err != nil {
		spool.Close()
		remove()
		return nil, "", false, noop, err
	}
	if n == 0 {
		spool.Close()
		remove()
		return nil, "", false, noop, errors.New("empty inflated dataset")
	}
	if err := spool.Close(); err != nil {
		remove()
		return nil, "", false, noop, err
	}
	dataset, err := object.OpenDataSetWithOptions(spool.Name(), transfer.ExplicitVRLittleEndian, read)
	if err != nil {
		remove()
		return nil, "", false, noop, err
	}
	cleanup := func() { _ = dataset.Close(); remove() }
	return &object.File{Preamble: header.Preamble, Meta: header.Meta, Dataset: dataset, TransferSyntax: header.TransferSyntax}, spool.Name(), wrapped, cleanup, nil
}

func addInflatedWrapperReport(r *clireadopts.Report, wrapped bool, limit int) error {
	if !wrapped {
		return nil
	}
	if len(r.Events) >= limit {
		return clireadopts.ErrDiagnosticLimit
	}
	r.Events = append(r.Events, clireadopts.Event{Scope: "dataset", Path: "", Reason: "zlib-deflate-wrapper", Offset: -1})
	return nil
}
