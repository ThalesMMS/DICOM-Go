package encapdoc

import (
	"bytes"
	"context"
	"fmt"
	"io"

	"github.com/ThalesMMS/dicom-go/object"
	"github.com/ThalesMMS/dicom-go/parser"
)

const documentChunkSize = 32 << 10

// CopyDocumentTo streams the document in a supported Encapsulated Document
// dataset. It validates the declared unpadded length before writing and uses
// Extract's format-specific padding rules if the declaration is absent.
// maxBytes limits the output (zero means no caller-supplied limit).
//
// Like Extract, this recovers the payload independently of MIME or content
// validation. Use InspectPayload for content inspection or Object.CopyValueTo
// to recover the complete stored value, including any padding.
//
// The original source of a deferred value must remain open and stable during
// replay. Reads on that source must not run concurrently. On cancellation, a
// source read error, or a destination write error, the returned count is the
// number of bytes delivered. The caller owns destination publication.
func CopyDocumentTo(ctx context.Context, obj *object.Object, dst io.Writer, maxBytes int64) (int64, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	if err := ctx.Err(); err != nil {
		return 0, err
	}
	if dst == nil || maxBytes < 0 {
		return 0, fmt.Errorf("encapdoc: destination writer and nonnegative byte limit required")
	}
	k, err := documentKind(obj)
	if err != nil {
		return 0, err
	}
	info, err := InspectDocumentLength(ctx, obj)
	if err != nil {
		return 0, err
	}
	element, _ := obj.Get(tagEncapsulatedDocument)
	raw, resident := element.RawBytes()
	length := info.DeclaredBytes
	path := []parser.BulkDataPathStep{{Tag: tagEncapsulatedDocument, ItemIndex: -1}}
	if !info.HasDeclaration {
		switch {
		case resident:
			length = int64(len(unpad(k, raw)))
		case info.StoredBytes > 0:
			length, err = deferredDocumentLength(ctx, obj, path, k, info.StoredBytes)
			if err != nil {
				return 0, fmt.Errorf("encapdoc: infer document length: %w", err)
			}
		}
	}
	if err := ctx.Err(); err != nil {
		return 0, err
	}
	if maxBytes > 0 && length > maxBytes {
		return 0, fmt.Errorf("%w: %d bytes", ErrEncapsulatedDocumentTooLarge, length)
	}
	if length == 0 {
		return 0, nil
	}
	if !resident {
		n, err := obj.CopyBulkDataRangeTo(ctx, path, 0, length, dst)
		if err != nil {
			return n, fmt.Errorf("encapdoc: replay document payload: %w", err)
		}
		return n, ctx.Err()
	}
	var written int64
	for written < length {
		chunk := raw[written:min(length, written+documentChunkSize)]
		n, err := dst.Write(chunk)
		written += int64(n)
		if err != nil {
			return written, err
		}
		if n != len(chunk) {
			return written, io.ErrShortWrite
		}
		if err := ctx.Err(); err != nil {
			return written, err
		}
	}
	return written, nil
}

func deferredDocumentLength(ctx context.Context, obj *object.Object, path []parser.BulkDataPathStep, k kind, stored int64) (int64, error) {
	if k.sopClassUID == EncapsulatedPDFStorage {
		probe := new(pdfLengthProbe)
		if _, err := obj.CopyBulkDataRangeTo(ctx, path, 0, stored, probe); err != nil {
			return 0, err
		}
		if probe.eofEnd > 0 {
			return probe.eofEnd, nil
		}
		return probe.nonNUL, nil
	}
	var buf bytes.Buffer
	const stlHeaderSize = 84
	if k.sopClassUID == EncapsulatedSTLStorage && stored >= stlHeaderSize {
		if _, err := obj.CopyBulkDataRangeTo(ctx, path, 0, stlHeaderSize, &buf); err != nil {
			return 0, err
		}
		if size, ok := binarySTLLength(buf.Bytes()); ok && size <= stored && stored-size <= 1 {
			return size, nil
		}
		buf.Reset()
	}
	for end := stored; end > 0; {
		start := max(0, end-documentChunkSize)
		if _, err := obj.CopyBulkDataRangeTo(ctx, path, start, end-start, &buf); err != nil {
			return 0, err
		}
		if remaining := bytes.TrimRight(buf.Bytes(), "\x00"); len(remaining) > 0 {
			return start + int64(len(remaining)), nil
		}
		buf.Reset()
		end = start
	}
	return 0, nil
}

// pdfLengthProbe tracks the last PDF EOF marker and immediately following
// line endings, including a marker or a line ending split across chunks.
type pdfLengthProbe struct {
	offset, nonNUL, eofEnd int64
	tail                   [len(pdfEOF) - 1]byte
	tailSize               int
}

func (p *pdfLengthProbe) Write(chunk []byte) (int, error) {
	if size := len(bytes.TrimRight(chunk, "\x00")); size > 0 {
		p.nonNUL = p.offset + int64(size)
	}
	var boundary [2 * (len(pdfEOF) - 1)]byte
	copy(boundary[:], p.tail[:p.tailSize])
	prefixSize := copy(boundary[p.tailSize:], chunk[:min(len(chunk), len(p.tail))])
	window := boundary[:p.tailSize+prefixSize]

	tailStart := -1
	if start := bytes.LastIndex(chunk, []byte(pdfEOF)); start >= 0 {
		tailStart = start + len(pdfEOF)
	} else if start := bytes.LastIndex(window, []byte(pdfEOF)); start >= 0 {
		tailStart = start + len(pdfEOF) - p.tailSize
	} else if p.eofEnd > 0 && p.eofEnd == p.offset {
		tailStart = 0
	}
	if tailStart >= 0 {
		lineEnding := len(chunk[tailStart:]) - len(bytes.TrimLeft(chunk[tailStart:], "\r\n"))
		p.eofEnd = p.offset + int64(tailStart+lineEnding)
	}
	if len(chunk) >= len(p.tail) {
		p.tailSize = copy(p.tail[:], chunk[len(chunk)-len(p.tail):])
	} else {
		p.tailSize = copy(p.tail[:], window[max(0, len(window)-len(p.tail)):])
	}
	p.offset += int64(len(chunk))
	return len(chunk), nil
}
