package main

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"runtime"

	"github.com/ThalesMMS/dicom-go/internal/nofollow"
	"github.com/ThalesMMS/dicom-go/object"
	"github.com/ThalesMMS/dicom-go/pixeldata"
	"github.com/ThalesMMS/dicom-go/transfer"
)

type datasetContextReader struct {
	ctx context.Context
	r   io.Reader
}

func (r datasetContextReader) Read(p []byte) (int, error) {
	if err := r.ctx.Err(); err != nil {
		return 0, err
	}
	return r.r.Read(p)
}

// The precursor has already passed TranscodePath's structural and pixel checks.
// Dataset extraction preserves its encoded body, including raw Deflate padding.
func transcodeDatasetOnly(ctx context.Context, source, destination string, target transfer.Syntax, opts pixeldata.TranscodeOptions) (pixeldata.TranscodeReport, error) {
	empty := pixeldata.TranscodeReport{}
	if err := ctx.Err(); err != nil {
		return empty, err
	}
	if opts.WriteOptions.Padding.Alignment != 0 {
		return empty, errors.New("dataset-only cannot use Part 10 alignment")
	}
	parentPath := filepath.Dir(destination)
	name := filepath.Base(destination)
	if !nofollow.ValidName(name) {
		return empty, pixeldata.ErrTranscodeDestinationUnsafe
	}
	parent, err := nofollow.OpenDirectory(parentPath)
	if err != nil {
		return empty, err
	}
	defer parent.Close()
	existing, err := nofollow.OpenAt(parent, name)
	if err == nil {
		existing.Close()
		return empty, pixeldata.ErrTranscodeDestinationUnsafe
	}
	if !errors.Is(err, os.ErrNotExist) {
		return empty, pixeldata.ErrTranscodeDestinationUnsafe
	}
	work, err := os.MkdirTemp(parentPath, ".dcmconv-dataset-*")
	if err != nil {
		return empty, err
	}
	defer os.RemoveAll(work)
	stagedPath := filepath.Join(work, "verified.dcm")
	report, err := pixeldata.TranscodePath(ctx, source, stagedPath, target, opts)
	if err != nil {
		return empty, err
	}
	staged, err := nofollow.OpenFile(stagedPath)
	if err != nil {
		return empty, err
	}
	defer staged.Close()
	limits, err := pixeldata.ResolveTranscodeLimits(opts.Limits)
	if err != nil {
		return empty, err
	}
	header, err := object.ReadPart10Header(staged, object.ReadFileOptions{MaxTotalBytes: limits.MaxOutputBytes, MaxElementBytes: limits.MaxOutputBytes, MaxElements: limits.MaxElements})
	if err != nil {
		return empty, err
	}
	info, err := staged.Stat()
	if err != nil {
		return empty, err
	}
	length := info.Size() - header.DataSetOffset
	if header.TransferSyntax.UID != target.UID || length <= 0 || length > limits.MaxOutputBytes {
		return empty, pixeldata.ErrTranscodeTransaction
	}
	var nonce [16]byte
	if _, err := rand.Read(nonce[:]); err != nil {
		return empty, err
	}
	temporary := ".dcmconv-dataset-" + hex.EncodeToString(nonce[:])
	out, err := nofollow.CreateAt(parent, temporary)
	if err != nil {
		return empty, err
	}
	defer nofollow.RemoveAt(parent, temporary)
	open := true
	defer func() {
		if open {
			out.Close()
		}
	}()
	digest := sha256.New()
	buffer := make([]byte, 64<<10)
	n, err := io.CopyBuffer(io.MultiWriter(out, digest), datasetContextReader{ctx, io.NewSectionReader(staged, header.DataSetOffset, length)}, buffer)
	if err != nil {
		return empty, err
	}
	if n != length {
		return empty, io.ErrUnexpectedEOF
	}
	report.FileSynced = false
	report.DirectorySynced = false
	if opts.Durability == pixeldata.TranscodeDurable {
		if err := out.Sync(); err != nil {
			return empty, err
		}
		report.FileSynced = true
	}
	expected, err := out.Stat()
	if err != nil {
		return empty, err
	}
	if err := out.Close(); err != nil {
		return empty, err
	}
	open = false
	check, err := nofollow.OpenAt(parent, temporary)
	if err != nil {
		return empty, err
	}
	checkDigest := sha256.New()
	copied, checkErr := io.CopyBuffer(checkDigest, datasetContextReader{ctx, check}, buffer)
	closeErr := check.Close()
	if checkErr != nil {
		return empty, checkErr
	}
	if closeErr != nil {
		return empty, closeErr
	}
	if copied != length || hex.EncodeToString(digest.Sum(nil)) != hex.EncodeToString(checkDigest.Sum(nil)) {
		return empty, pixeldata.ErrTranscodeTransaction
	}
	if err := ctx.Err(); err != nil {
		return empty, err
	}
	published, err := nofollow.PublishClosedAt(parent, temporary, name, expected)
	if err != nil {
		if published {
			return report, fmt.Errorf("dataset output was published; publication completion failed: %w", err)
		}
		return empty, err
	}
	if !published {
		return empty, pixeldata.ErrTranscodeTransaction
	}
	if opts.Durability == pixeldata.TranscodeDurable && runtime.GOOS != "windows" {
		if err := parent.Sync(); err != nil {
			return report, fmt.Errorf("dataset output was published; directory sync failed: %w", err)
		}
		report.DirectorySynced = true
	}
	return report, nil
}
