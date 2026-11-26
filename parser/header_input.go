package parser

import (
	"bytes"
	"io"
	"os"
	"strings"
)

// headerInput stages bytes of the current header. Each fill stays inside the
// minimum header size or a field already justified by a parsed VR. Values,
// including deferred and skipped payloads, remain outside the staged bytes.
type headerInput struct {
	source *countingReader
	data   [8]byte
	off    int
	n      int
	err    error
	active bool
}

func headerInputSource(source io.Reader, sourceEnd int64) bool {
	if sourceEnd <= 0 {
		return false
	}
	// A generic Reader may attach effects or an n+err outcome to individual
	// field reads. Knowing its size or having Seek/ReadAt does not authorize a
	// change to that contract. These concrete sources have ordinary byte/EOF
	// semantics. A regular os.File has already been established by sourceEnd.
	switch source.(type) {
	case *os.File, *bytes.Reader, *strings.Reader:
		return true
	default:
		return false
	}
}

func (r *Reader) beginHeaderInput() {
	r.headerInput.active = false
	if !r.headerInputEnabled || len(r.counter.pending) != 0 ||
		r.recoverDelimiterAnomalies || r.recoverImplicitVRElements ||
		r.vrRecovery != (VRRecoveryOptions{}) || r.seqRecovery != (SequenceRecoveryOptions{}) {
		return
	}
	if _, slurped := r.counter.r.(*sliceSource); slurped {
		return
	}
	r.headerInput.source = r.counter
	r.headerInput.active = true
	prefix := 4 // Do not move past the tag before an implicit dictionary lookup.
	if r.syntax.ExplicitVR && !r.implicitVRLittleEndianActive() {
		prefix = 6 // Tag + VR, or tag + first two bytes of an Item's length.
		counter := r.counter
		// A wider read near a fence can change which error wins, even
		// when an invalid VR would have stopped before reaching that fence.
		if r.headerInputFile && r.sourceEnd-counter.pos >= 8 &&
			(counter.fence <= 0 || counter.fence-counter.pos >= 8) &&
			(counter.maxTotalBytes <= 0 || counter.maxTotalBytes-counter.pos >= 8) {
			prefix = 8
		}
	}
	r.headerInput.fill(prefix)
}

func (r *Reader) endHeaderInput() error {
	input := &r.headerInput
	var err error
	if unread := input.n - input.off; unread > 0 {
		// The wider file stage leaves bytes unconsumed on an invalid VR.
		// The staged reads already account for the logical position.
		_, err = input.source.r.(io.Seeker).Seek(-int64(unread), io.SeekCurrent)
	}
	input.active = false
	input.source = nil
	input.off, input.n = 0, 0
	input.err = nil
	return err
}

func (r *Reader) headerReader() io.Reader {
	if r.headerInput.active {
		return &r.headerInput
	}
	return r.counter
}

func (h *headerInput) fill(length int) {
	// Refill only after the parser consumes the previous stage.
	if h.off != h.n || h.err != nil {
		return
	}
	h.off, h.n = 0, 0
	h.n, h.err = io.ReadFull(h.source, h.data[:length])
	if h.err == io.ErrUnexpectedEOF {
		// ReadFull describes the entire staging read. Individual fields must
		// still see EOF at an exact field boundary, or UnexpectedEOF only after
		// they consumed a partial field (ReadFull performs that conversion).
		h.err = io.EOF
	}
	// The physical read is already complete, but fields consume the staged
	// bytes through the same logical counter, preserving per-field offsets.
	h.source.pos -= int64(h.n)
}

func (h *headerInput) Read(p []byte) (int, error) {
	if len(p) == 0 {
		return 0, nil
	}
	if h.off < h.n {
		n := copy(p, h.data[h.off:h.n])
		h.off += n
		h.source.pos += int64(n)
		return n, nil
	}
	if h.err != nil {
		return 0, h.err
	}
	return h.source.Read(p)
}
