package object

import (
	"fmt"
	"io"

	"github.com/ThalesMMS/dicom-go/core"
	"github.com/ThalesMMS/dicom-go/parser"
)

// readerValueProvider replays a seekable source to stream raw value bytes for
// elements that were not materialized in memory.
//
// The parser records value offsets for skipped/deferred values while reading.
// CopyValueTo uses those offsets directly when available and falls back to a
// tag scan for readers created before an offset was recorded.
//
// Limitations:
//   - Only works when the original input passed to object.ReadFileWithOptions is
//     an io.ReadSeeker (e.g., *os.File).
//   - Supports defined-length primitive values and undefined-length encapsulated
//     Pixel Data values. General SQ replay is a permanent, intentional
//     non-goal (see docs/CONFORMANCE.md "Known Limitations" and #278);
//     sequence item values are always materialized instead.
//
// NOTE: This is an internal helper; the public entrypoint is Object.CopyValueTo.

type readerValueProvider struct {
	reader *parser.Reader
	// bulkItemBindings retains only ancestor items containing deferred bulk.
	// Original non-nil item addresses bind references to this parsed source.
	bulkItemBindings map[*core.DataSet]struct{}
	// swapWidth, when set, gives the unit whose bytes are reversed while a
	// value of a legacy Big Endian encoding is copied (0 or 1: unchanged).
	swapWidth func(core.Tag) int
}

// CopyValueTo serves the top-level object only. Values nested in sequence
// items carry their Item offset, so a nested element with the same tag (for
// example Pixel Data inside an Icon Image Sequence item) never makes the
// top-level value ambiguous or replaces it.
func (p *readerValueProvider) CopyValueTo(tag core.Tag, w io.Writer) (int64, error) {
	if p == nil || p.reader == nil {
		return 0, fmt.Errorf("dicom: nil value provider")
	}
	if p.swapWidth != nil {
		if width := p.swapWidth(tag); width > 1 {
			rw := &reversingWriter{w: w, width: width}
			n, err := p.copyValueTo(tag, rw)
			if flushErr := rw.flush(); err == nil {
				err = flushErr
			}
			return n, err
		}
	}
	return p.copyValueTo(tag, w)
}

func (p *readerValueProvider) copyValueTo(tag core.Tag, w io.Writer) (int64, error) {
	all := p.reader.ValueLocations(tag)
	var root []parser.ValueLocation
	for _, location := range all {
		if !location.ItemOffsetSet {
			root = append(root, location)
		}
	}
	switch {
	case len(root) == 1:
		return p.reader.CopyElementValueAt(root[0], w)
	case len(root) > 1:
		return 0, fmt.Errorf("dicom: value location for element %s is ambiguous due to duplicate tags", tag)
	case len(all) > 0:
		return 0, fmt.Errorf("dicom: no top-level value location for element %s", tag)
	default:
		return p.reader.CopyElementValueTo(tag, w)
	}
}

// reversingWriter reverses the bytes of every width-sized unit written
// through it, across Write boundaries; flush writes a trailing partial unit
// unchanged.
// reversingWriter reverses the byte order of each width-byte unit it streams.
// It reuses one bounded scratch buffer, so streaming a large deferred value
// (Big Endian Pixel Data) allocates neither the value nor a copy per write.
type reversingWriter struct {
	w       io.Writer
	width   int
	pending []byte
	scratch []byte
}

// reversingWriterChunk bounds the scratch buffer of a reversingWriter.
const reversingWriterChunk = 64 << 10

func (r *reversingWriter) Write(p []byte) (int, error) {
	written := len(p)
	if len(r.pending) > 0 {
		need := r.width - len(r.pending)
		if need > len(p) {
			r.pending = append(r.pending, p...)
			return written, nil
		}
		r.pending = append(r.pending, p[:need]...)
		p = p[need:]
		if err := r.writeReversed(r.pending); err != nil {
			return 0, err
		}
		r.pending = r.pending[:0]
	}
	whole := len(p) - len(p)%r.width
	if whole > 0 {
		if err := r.writeReversed(p[:whole]); err != nil {
			return 0, err
		}
	}
	r.pending = append(r.pending[:0], p[whole:]...)
	return written, nil
}

// writeReversed writes data, a whole number of units, reversed unit by unit
// through the scratch buffer.
func (r *reversingWriter) writeReversed(data []byte) error {
	chunk := reversingWriterChunk - reversingWriterChunk%r.width
	if chunk <= 0 {
		chunk = r.width
	}
	if cap(r.scratch) < min(len(data), chunk) {
		r.scratch = make([]byte, min(len(data), chunk))
	}
	for len(data) > 0 {
		n := min(len(data), chunk)
		out := r.scratch[:n]
		for unit := 0; unit < n; unit += r.width {
			for index := 0; index < r.width; index++ {
				out[unit+index] = data[unit+r.width-1-index]
			}
		}
		if _, err := r.w.Write(out); err != nil {
			return err
		}
		data = data[n:]
	}
	return nil
}

func (r *reversingWriter) flush() error {
	if len(r.pending) == 0 {
		return nil
	}
	_, err := r.w.Write(r.pending)
	r.pending = nil
	return err
}

// copyNestedValueTo streams the deferred value of tag in the sequence item
// that starts at itemOffset.
func (p *readerValueProvider) copyNestedValueTo(itemOffset int64, tag core.Tag, w io.Writer) (int64, error) {
	if p == nil || p.reader == nil {
		return 0, fmt.Errorf("dicom: nil value provider")
	}
	var found []parser.ValueLocation
	for _, location := range p.reader.ValueLocations(tag) {
		if location.ItemOffsetSet && location.ItemOffset == itemOffset {
			found = append(found, location)
		}
	}
	switch len(found) {
	case 0:
		return 0, fmt.Errorf("%w: no value location for element %s in the item at offset %d", ErrDeferredSequenceValueWrite, tag, itemOffset)
	case 1:
	default:
		return 0, fmt.Errorf("dicom: value location for element %s is ambiguous due to duplicate tags in the item at offset %d", tag, itemOffset)
	}
	if p.swapWidth != nil {
		if width := p.swapWidth(tag); width > 1 {
			rw := &reversingWriter{w: w, width: width}
			n, err := p.reader.CopyElementValueAt(found[0], rw)
			if flushErr := rw.flush(); err == nil {
				err = flushErr
			}
			return n, err
		}
	}
	return p.reader.CopyElementValueAt(found[0], w)
}

// isNestedDeferred reports a primitive element whose value was left on the
// source: a nil Value with a positive defined length.
func isNestedDeferred(el core.Element) bool {
	return el.Value == nil && el.VR() != core.VRSQ && el.Header.HasLength() &&
		!el.Header.Length.IsUndefined() && el.Header.Length > 0
}

// CopyNestedValueTo streams the value of tag in item, a sequence item of
// this object at any depth as GetSequence returns it; its ItemOffset binds
// it to the source. A materialized raw value is copied from memory; a
// deferred one (ReadFileOptions.DeferNestedValues or DeferWaveformData) is
// replayed from the seekable source.
func (o *Object) CopyNestedValueTo(item *Object, tag core.Tag, w io.Writer) (int64, error) {
	if o == nil || item == nil {
		return 0, fmt.Errorf("dicom: nil object")
	}
	element, ok := item.Get(tag)
	if !ok {
		return 0, fmt.Errorf("dicom: missing element %s in sequence item", tag)
	}
	offset, offsetSet := item.ItemOffset()
	return o.copyNestedValueTo(offset, offsetSet, element, w)
}

func (o *Object) copyNestedValueTo(itemOffset int64, itemOffsetSet bool, element core.Element, w io.Writer) (int64, error) {
	tag := element.Tag()
	if raw, ok := element.RawBytes(); ok {
		n, err := w.Write(raw)
		return int64(n), err
	}
	if element.Value != nil {
		return 0, fmt.Errorf("dicom: element %s value is not a raw byte payload", tag)
	}
	if !itemOffsetSet {
		return 0, fmt.Errorf("%w: element %s is in an item without a source offset", ErrDeferredSequenceValueWrite, tag)
	}
	provider, ok := o.nestedValueReader()
	if !ok {
		return 0, fmt.Errorf("dicom: no value provider available for element %s", tag)
	}
	return provider.copyNestedValueTo(itemOffset, tag, w)
}

// nestedValueReader is the reader provider that holds the values deferred in
// o's sequence items, also when o is the data set WriteFileFrames writes,
// whose provider serves Pixel Data from frames and the rest from the source.
func (o *Object) nestedValueReader() (*readerValueProvider, bool) {
	if o == nil {
		return nil, false
	}
	provider := o.valueProvider
	if frames, ok := provider.(*frameValueProvider); ok && frames != nil {
		provider = frames.base
	}
	reader, ok := provider.(*readerValueProvider)
	return reader, ok && reader != nil
}

// nestedDeferredValue is the parser.WriterOptions.DeferredValue of an
// object: it streams the deferred values nested in its items.
func (o *Object) nestedDeferredValue(item core.DataSet, el core.Element) (func(io.Writer) (int64, error), bool) {
	if !isNestedDeferred(el) {
		return nil, false
	}
	if _, ok := o.nestedValueReader(); !ok {
		return nil, false
	}
	return func(w io.Writer) (int64, error) {
		return o.copyNestedValueTo(item.ItemOffset, item.ItemOffsetSet, el, w)
	}, true
}

// encodedValueLengthProvider exposes metadata without consuming or acquiring
// source ownership. Generic providers may deliberately not implement it.
type encodedValueLengthProvider interface{ encodedValueLength(core.Tag) (int64, error) }

func (o *Object) deferredEncodedLength(el core.Element) (int64, error) {
	if el.Header.HasLength() && !el.Header.Length.IsUndefined() {
		return int64(el.Header.Length), nil
	}
	if o == nil {
		return 0, parser.ErrEncodedLengthUnavailable
	}
	if provider, ok := o.valueProvider.(encodedValueLengthProvider); ok {
		return provider.encodedValueLength(el.Tag())
	}
	return 0, parser.ErrEncodedLengthUnavailable
}

func (p *readerValueProvider) encodedValueLength(tag core.Tag) (int64, error) {
	if p == nil || p.reader == nil {
		return 0, parser.ErrEncodedLengthUnavailable
	}
	var found parser.ValueLocation
	count := 0
	for _, location := range p.reader.ValueLocations(tag) {
		if !location.ItemOffsetSet {
			found = location
			count++
		}
	}
	if count != 1 {
		return 0, fmt.Errorf("%w: root value location is unavailable or ambiguous", parser.ErrEncodedLengthUnavailable)
	}
	if found.Length < 0 {
		return 0, parser.ErrEncodedLengthUnavailable
	}
	return found.Length, nil
}

func (o *Object) nestedDeferredLength(item core.DataSet, el core.Element) (int64, error) {
	if el.Header.HasLength() && !el.Header.Length.IsUndefined() {
		return int64(el.Header.Length), nil
	}
	if o == nil || !item.ItemOffsetSet {
		return 0, parser.ErrEncodedLengthUnavailable
	}
	provider, ok := o.nestedValueReader()
	if !ok || provider.reader == nil {
		return 0, parser.ErrEncodedLengthUnavailable
	}
	var found parser.ValueLocation
	count := 0
	for _, location := range provider.reader.ValueLocations(el.Tag()) {
		if location.ItemOffsetSet && location.ItemOffset == item.ItemOffset {
			found = location
			count++
		}
	}
	if count != 1 || found.Length < 0 {
		return 0, parser.ErrEncodedLengthUnavailable
	}
	return found.Length, nil
}
