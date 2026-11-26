package object

import (
	"context"
	"errors"
	"fmt"

	"github.com/ThalesMMS/dicom-go/core"
	"github.com/ThalesMMS/dicom-go/parser"
	"github.com/ThalesMMS/dicom-go/validation"
)

// ErrPartialRead marks an incomplete File: completed elements before a parse
// failure (AllowPartialRead), or a deliberate StopBeforeTag header boundary.
var ErrPartialRead = errors.New("dicom: partial read: the data set ends early or is malformed")

// ErrPartialFile refuses operations that require a complete File.
var ErrPartialFile = errors.New("dicom: file is the result of a partial read")

// PartialRead describes where an incomplete read stopped. It holds offsets, a
// tag and the cause (parse failure or ReadStopError), never values.
type PartialRead struct {
	// Offset is the absolute stream offset of the failure when known, else -1.
	Offset int64
	// Tag is the element being read at the failure, when known.
	Tag core.Tag
	// Elements is the number of complete top-level elements kept.
	Elements int
	// RetainedElements includes incomplete top-level sequence prefixes too.
	RetainedElements int
	// Path identifies the failed attribute or innermost open container. It
	// contains only tags and zero-based item indices, never values.
	Path validation.Path
	// Containers identifies incomplete SQ/item wrappers and whether each
	// prefix was retained. Completed items do not appear in this list.
	Containers []parser.PartialReadContainer
	// Err is the read error that ended the data set.
	Err error
}

// PartialReadError is returned with the partial File.
type PartialReadError struct {
	Read PartialRead
}

func (e *PartialReadError) Error() string {
	if e == nil {
		return ErrPartialRead.Error()
	}
	return fmt.Sprintf("%s: kept %d complete elements (%d root entries) before offset %d (%s), path %s: %v",
		ErrPartialRead, e.Read.Elements, e.Read.RetainedElements, e.Read.Offset, e.Read.Tag, e.Read.Path, e.Read.Err)
}

// Unwrap exposes ErrPartialRead and the underlying read error.
func (e *PartialReadError) Unwrap() []error {
	if e == nil {
		return []error{ErrPartialRead}
	}
	return []error{ErrPartialRead, e.Read.Err}
}

// PartialRead reports whether f came from a partial read, and where it
// stopped.
func (f *File) PartialRead() (PartialRead, bool) {
	if f == nil || f.partialRead == nil {
		return PartialRead{}, false
	}
	return clonePartialRead(*f.partialRead), true
}

func clonePartialRead(p PartialRead) PartialRead {
	p.Path = p.Path.Clone()
	p.Containers = append([]parser.PartialReadContainer(nil), p.Containers...)
	return p
}

func newPartialRead(err error, elements int) *PartialRead {
	p := &PartialRead{Offset: -1, Elements: elements, RetainedElements: elements, Err: err}
	var pe *parser.ParseError
	if errors.As(err, &pe) {
		p.Offset, p.Tag = pe.Offset, pe.Tag
	}
	var partial *parser.PartialDataSetError
	if errors.As(err, &partial) {
		p.Path = partial.Path.Clone()
		p.Containers = append([]parser.PartialReadContainer(nil), partial.Containers...)
		if len(p.Containers) > 0 && p.Containers[0].Retained {
			p.Elements-- // The interrupted root SQ is a prefix, not complete.
		}
	}
	return p
}

func isContextError(err error) bool {
	return errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded)
}
