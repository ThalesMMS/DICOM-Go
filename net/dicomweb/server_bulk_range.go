package dicomweb

import (
	"context"
	"io"
	"math"
	"net/http"
	"strconv"
	"strings"
)

type bulkByteRange struct {
	start, length int64
	satisfiable   bool
}

// parseBulkRange implements one byte range. The bounded adapter deliberately
// ignores malformed/multiple/unknown units. Decimal numbers saturate safely:
// a huge start is unsatisfiable; a huge end/suffix clamps to the finite value.
func parseBulkRange(header string, size int64) (bulkByteRange, bool) {
	unit, value, ok := strings.Cut(strings.TrimSpace(header), "=")
	if !ok || !strings.EqualFold(strings.TrimSpace(unit), "bytes") || strings.Contains(value, ",") || size <= 0 {
		return bulkByteRange{}, false
	}
	value = strings.TrimSpace(value)
	first, last, ok := strings.Cut(value, "-")
	if !ok || strings.Contains(last, "-") {
		return bulkByteRange{}, false
	}
	if first == "" {
		suffix, ok := bulkRangeNumber(last)
		if !ok {
			return bulkByteRange{}, false
		}
		if suffix == 0 {
			return bulkByteRange{}, true
		}
		length := min(suffix, size)
		return bulkByteRange{start: size - length, length: length, satisfiable: true}, true
	}
	start, ok := bulkRangeNumber(first)
	if !ok {
		return bulkByteRange{}, false
	}
	end := size - 1
	if last != "" {
		var valid bool
		end, valid = bulkRangeNumber(last)
		if !valid || end < start {
			return bulkByteRange{}, false
		}
	}
	if start >= size {
		return bulkByteRange{}, true
	}
	end = min(end, size-1)
	return bulkByteRange{start: start, length: end - start + 1, satisfiable: true}, true
}
func bulkRangeNumber(raw string) (int64, bool) {
	if raw == "" {
		return 0, false
	}
	var n int64
	for i := range raw {
		c := raw[i]
		if c < '0' || c > '9' {
			return 0, false
		}
		digit := int64(c - '0')
		if n > (math.MaxInt64-digit)/10 {
			n = math.MaxInt64
		} else {
			n = n*10 + digit
		}
	}
	return n, true
}

type bulkContextReader struct {
	ctx    context.Context
	reader io.Reader
}

func (r bulkContextReader) Read(p []byte) (int, error) {
	if err := CheckReadLease(r.ctx); err != nil {
		return 0, err
	}
	n, e := r.reader.Read(p)
	if err := CheckReadLease(r.ctx); err != nil {
		return 0, err
	}
	return n, e
}

// Reader ownership is held by serveBulkData's withReaderClosed. Range access
// never probes or materializes the stream, infers physical offsets, or closes
// ReaderAt separately. The backend promises a stable logical representation.
func (s *Server) serveBulkRange(w *serverResponseWriter, r *http.Request, part BulkDataPart, contentType string) error {
	if err := CheckReadLease(r.Context()); err != nil {
		return err
	}
	w.Header().Set("Content-Type", contentType)
	if part.Size < 0 || part.ReaderAt == nil {
		w.Header().Set("Accept-Ranges", "none")
		_, err := copyBounded(r.Context(), w, part.Reader, min(s.limits.MaxPartBytes, w.remaining))
		return err
	}
	selected := bulkByteRange{length: part.Size, satisfiable: true}
	partial := false
	if len(r.Header.Values("If-Range")) == 0 && len(r.Header.Values("Range")) <= 1 {
		if parsed, ok := parseBulkRange(r.Header.Get("Range"), part.Size); ok {
			selected, partial = parsed, true
		}
	}
	w.Header().Set("Accept-Ranges", "bytes")
	if partial && !selected.satisfiable {
		if err := CheckReadLease(r.Context()); err != nil {
			return err
		}
		w.Header().Set("Content-Range", "bytes */"+strconv.FormatInt(part.Size, 10))
		w.Header().Set("Content-Length", "0")
		w.WriteHeader(http.StatusRequestedRangeNotSatisfiable)
		return nil
	}
	if selected.length > s.limits.MaxPartBytes || selected.length > w.remaining {
		return ErrResourceLimit
	}
	w.Header().Set("Content-Length", strconv.FormatInt(selected.length, 10))
	if partial {
		if err := CheckReadLease(r.Context()); err != nil {
			return err
		}
		w.Header().Set("Content-Range", "bytes "+strconv.FormatInt(selected.start, 10)+"-"+strconv.FormatInt(selected.start+selected.length-1, 10)+"/"+strconv.FormatInt(part.Size, 10))
		w.WriteHeader(http.StatusPartialContent)
	}
	section := io.NewSectionReader(part.ReaderAt, selected.start, selected.length)
	written, err := io.CopyBuffer(w, bulkContextReader{ctx: r.Context(), reader: section}, make([]byte, 32<<10))
	if err != nil {
		return err
	}
	if written != selected.length {
		return io.ErrUnexpectedEOF
	}
	return nil
}
