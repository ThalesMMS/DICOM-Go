package object

import (
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"fmt"

	"github.com/ThalesMMS/dicom-go/core"
	"github.com/ThalesMMS/dicom-go/dictionary"
	"github.com/ThalesMMS/dicom-go/dictionary/std"
	"github.com/ThalesMMS/dicom-go/internal/valueencode"
	"github.com/ThalesMMS/dicom-go/parser"
	"github.com/ThalesMMS/dicom-go/transfer"
)

// ErrUNRecovery reports, under UNRecoveryOptions.Strict, a UN element whose
// known VR could not be applied safely.
var ErrUNRecovery = errors.New("dicom: UN element cannot be recovered")

// UNRecoveryError names the element and the reason; it never carries values.
type UNRecoveryError struct {
	Path   string
	Tag    core.Tag
	Reason UNReason
}

func (e *UNRecoveryError) Error() string {
	return fmt.Sprintf("%s: %s: %s", ErrUNRecovery, e.Path, e.Reason)
}

func (e *UNRecoveryError) Unwrap() error { return ErrUNRecovery }

// UNReason is the fixed vocabulary of UN recovery diagnostics.
type UNReason string

const (
	// Recovered elements.
	UNRecovered                  UNReason = "recovered"
	UNRecoveredSequence          UNReason = "recovered-sequence"
	UNRecoveredPrivateCreator    UNReason = "private-creator-encoded-as-UN"
	UNRecoveredLongShortVR       UNReason = "recovered-value-exceeds-16-bit-explicit-length"
	UNRecoveredUndefinedSequence UNReason = "recovered-undefined-length-sequence"

	// Elements kept as UN because nothing is known about them.
	UNUnknownAttribute  UNReason = "unknown-attribute"
	UNDictionaryUN      UNReason = "dictionary-vr-is-UN"
	UNPrivateNoCreator  UNReason = "private-creator-unresolved"
	UNPrivateNoCatalog  UNReason = "private-attribute-not-in-catalog"
	UNFileMetaGroup     UNReason = "file-meta-group-not-recovered"
	UNAmbiguousVR       UNReason = "ambiguous-vr"
	UNContextMissing    UNReason = "contextual-vr-without-context"
	UNDeferredValue     UNReason = "deferred-value"
	UNLengthMismatch    UNReason = "length-not-a-multiple-of-vr-size"
	UNInvalidSequence   UNReason = "invalid-sequence-payload"
	UNSequenceForScalar UNReason = "sequence-encoding-for-non-sequence-vr"
	UNLimitExceeded     UNReason = "recovery-limit-exceeded"
)

// unsafe reports reasons that Strict turns into errors: a VR is known but
// could not be applied. Unknown attributes are not recoverable at all and are
// kept even under Strict.
func (r UNReason) unsafe() bool {
	switch r {
	case UNAmbiguousVR, UNContextMissing, UNDeferredValue, UNLengthMismatch, UNInvalidSequence, UNSequenceForScalar, UNLimitExceeded, UNPrivateNoCreator:
		return true
	}
	return false
}

// UNDiagnostic records one UN element: its structural path, the resolved VR
// (UN when kept), and the reason. It contains no values.
type UNDiagnostic struct {
	Path      string
	Tag       core.Tag
	VR        core.VR
	Recovered bool
	Reason    UNReason
}

// UNRecoveryReport summarizes a RecoverUN call.
type UNRecoveryReport struct {
	Recovered            int
	Kept                 int
	Diagnostics          []UNDiagnostic
	DiagnosticsTruncated bool
}

// UNRecoveryOptions bounds RecoverUN. Zero fields select the defaults of
// DefaultUNRecoveryOptions.
type UNRecoveryOptions struct {
	// Dictionary resolves known VRs; nil uses the object's dictionary, or the
	// standard dictionary. Private attributes resolve only through a
	// PrivateDataDictionary and a valid creator in the same data set.
	Dictionary dictionary.DataDictionary
	// MaxElementBytes limits each element parsed from a recovered sequence.
	MaxElementBytes int64
	// MaxElements limits the elements parsed from all recovered sequences.
	MaxElements int
	// MaxSequenceDepth limits sequence and item nesting, counted from the
	// object's top level.
	MaxSequenceDepth int
	// MaxPrivateCreators limits creator reservations per data set.
	MaxPrivateCreators int
	// MaxDiagnostics caps UNRecoveryReport.Diagnostics; counts stay exact.
	MaxDiagnostics int
	// Strict returns an error instead of keeping UN when a known VR cannot be
	// applied safely (ambiguous or missing context, bad length, invalid
	// sequence payload, deferred value, unresolved creator, limits).
	Strict bool
}

// DefaultUNRecoveryOptions returns the limits RecoverUN uses for zero fields.
func DefaultUNRecoveryOptions() UNRecoveryOptions {
	return UNRecoveryOptions{
		MaxElementBytes:    64 << 20,
		MaxElements:        100_000,
		MaxSequenceDepth:   64,
		MaxPrivateCreators: dictionary.DefaultMaxPrivateCreators,
		MaxDiagnostics:     256,
	}
}

// RecoverUN returns a copy of o in which UN elements with a known,
// unambiguous VR carry that VR (PS3.5 6.2.2). o is not modified, so writing o
// still reproduces the received UN bytes; writing the result is the explicit
// output transformation.
//
// UN values are Implicit VR Little Endian whatever the transfer syntax, so
// recovered binary values are converted to o's value byte order and recovered
// sequences are parsed as Implicit VR Little Endian items, within the limits
// of opts. Contextual VRs (US or SS, OB or OW, LUT Data) use only the
// normative context in the data set or its ancestors. A Private Creator
// encoded as UN becomes LO with its own diagnostic. Private attributes are
// recovered only through a PrivateDataDictionary and a valid creator of the
// same data set; items never inherit reservations. File Meta Information is a
// separate object and is never recovered. Anything that cannot be recovered
// safely stays UN with a diagnostic, or fails under Strict.
//
// Deferred values of o stay deferred: the result reads them through o's
// source, so o must stay open while the result is used.
func (o *Object) RecoverUN(ctx context.Context, opts UNRecoveryOptions) (*Object, UNRecoveryReport, error) {
	if o == nil {
		return nil, UNRecoveryReport{}, fmt.Errorf("dicom: nil object")
	}
	if ctx == nil {
		ctx = context.Background()
	}
	defaults := DefaultUNRecoveryOptions()
	if opts.MaxElementBytes < 0 || opts.MaxElements < 0 || opts.MaxSequenceDepth < 0 || opts.MaxPrivateCreators < 0 || opts.MaxDiagnostics < 0 {
		return nil, UNRecoveryReport{}, fmt.Errorf("dicom: UN recovery limits must not be negative")
	}
	if opts.MaxElementBytes == 0 {
		opts.MaxElementBytes = defaults.MaxElementBytes
	}
	if opts.MaxElements == 0 {
		opts.MaxElements = defaults.MaxElements
	}
	if opts.MaxSequenceDepth == 0 {
		opts.MaxSequenceDepth = defaults.MaxSequenceDepth
	}
	if opts.MaxPrivateCreators == 0 {
		opts.MaxPrivateCreators = defaults.MaxPrivateCreators
	}
	if opts.MaxDiagnostics == 0 {
		opts.MaxDiagnostics = defaults.MaxDiagnostics
	}
	dict := opts.Dictionary
	if dict == nil {
		dict = o.dict
	}
	if dict == nil {
		dict = std.Dictionary
	}
	r := &unRecoverer{ctx: ctx, dict: dict, order: o.ValueByteOrder(), opts: opts, budget: opts.MaxElements}
	elements, err := r.dataSet(o.Elements(), "", 0, nil)
	if err != nil {
		return nil, r.report, err
	}
	out := FromElementsWithTextOptions(elements, o.dict, o.text)
	out.SetValueByteOrder(o.ValueByteOrder())
	out.valueProvider = o.valueProvider
	return out, r.report, nil
}

type unRecoverer struct {
	ctx    context.Context
	dict   dictionary.DataDictionary
	order  binary.ByteOrder
	opts   UNRecoveryOptions
	budget int
	report UNRecoveryReport
}

func unPath(parent string, tag core.Tag) string {
	if parent == "" {
		return tag.String()
	}
	return parent + "." + tag.String()
}

// note records a diagnostic and, under Strict, turns an unsafe reason into
// an error.
func (r *unRecoverer) note(path string, tag core.Tag, vr core.VR, reason UNReason) error {
	recovered := vr != core.VRUN
	if recovered {
		r.report.Recovered++
	} else {
		r.report.Kept++
	}
	if len(r.report.Diagnostics) < r.opts.MaxDiagnostics {
		r.report.Diagnostics = append(r.report.Diagnostics, UNDiagnostic{Path: path, Tag: tag, VR: vr, Recovered: recovered, Reason: reason})
	} else {
		r.report.DiagnosticsTruncated = true
	}
	if !recovered && r.opts.Strict && reason.unsafe() {
		return &UNRecoveryError{Path: path, Tag: tag, Reason: reason}
	}
	return nil
}

// dataSet recovers one data set. Creators come first, then primitives whose
// VR is fixed, then contextual VRs (which may depend on a recovered Pixel
// Representation), then sequences with this data set as their context.
func (r *unRecoverer) dataSet(elements []core.Element, path string, depth int, ancestors []pixelRepresentationScope) ([]core.Element, error) {
	out := append([]core.Element(nil), elements...)
	for i, element := range out {
		if element.VR() != core.VRUN || !dictionary.IsPrivateCreatorTag(element.Tag()) {
			continue
		}
		if _, ok := element.Value.(core.RawValue); !ok {
			continue
		}
		element.Header.VR = core.VRLO
		out[i] = element
		if err := r.note(unPath(path, element.Tag()), element.Tag(), core.VRLO, UNRecoveredPrivateCreator); err != nil {
			return nil, err
		}
	}
	reservations, _, err := dictionary.PrivateReservationsFromElements(out, r.opts.MaxPrivateCreators)
	if err != nil {
		return nil, err
	}
	pending := make([]bool, len(out))
	for i, element := range out {
		if err := r.ctx.Err(); err != nil {
			return nil, err
		}
		if element.VR() != core.VRUN || dictionary.IsPrivateCreatorTag(element.Tag()) {
			continue
		}
		recovered, deferred, err := r.primitive(element, out, path, depth, reservations, nil, false)
		if err != nil {
			return nil, err
		}
		pending[i] = deferred
		out[i] = recovered
	}
	scope := append([]pixelRepresentationScope{contextualVRResolver{order: r.order}.pixelRepresentation(out)}, ancestors...)
	for i, element := range out {
		if !pending[i] {
			continue
		}
		recovered, _, err := r.primitive(element, out, path, depth, reservations, scope, true)
		if err != nil {
			return nil, err
		}
		out[i] = recovered
	}
	for i, element := range out {
		if err := r.ctx.Err(); err != nil {
			return nil, err
		}
		sequence, ok := element.Value.(core.SequenceValue)
		if !ok {
			continue
		}
		elementPath := unPath(path, element.Tag())
		if depth+1 > r.opts.MaxSequenceDepth {
			if err := r.note(elementPath, element.Tag(), core.VRUN, UNLimitExceeded); err != nil {
				return nil, err
			}
			continue
		}
		items := make([]core.DataSet, len(sequence.Items))
		for j, item := range sequence.Items {
			recovered, err := r.dataSet(item.Elements, fmt.Sprintf("%s[%d]", elementPath, j), depth+1, scope)
			if err != nil {
				return nil, err
			}
			items[j] = item
			items[j].Elements = recovered
		}
		element.Value = core.SequenceValue{Items: items}
		out[i] = element
	}
	return out, nil
}

// primitive resolves one UN element. In the first pass (contextual false) a
// contextual VR is deferred to the second pass, which has the data set's
// Pixel Representation scope.
func (r *unRecoverer) primitive(element core.Element, siblings []core.Element, path string, depth int, reservations *dictionary.PrivateReservations, scope []pixelRepresentationScope, contextual bool) (core.Element, bool, error) {
	tag := element.Tag()
	elementPath := unPath(path, tag)
	keep := func(reason UNReason) (core.Element, bool, error) {
		return element, false, r.note(elementPath, tag, core.VRUN, reason)
	}
	if tag.Group == 0x0002 {
		return keep(UNFileMetaGroup)
	}
	vr, reason, context := r.lookup(tag, reservations)
	if reason != "" {
		return keep(reason)
	}
	if context != "" {
		if !contextual {
			return element, true, nil
		}
		resolver := contextualVRResolver{dict: r.dict, order: r.order}
		resolved, err := resolver.resolve(element, context, siblings, scope)
		if err != nil {
			return keep(UNContextMissing)
		}
		vr = resolved
	}
	switch value := element.Value.(type) {
	case nil:
		return keep(UNDeferredValue)
	case core.SequenceValue:
		// Undefined-length UN, already parsed as Implicit VR Little Endian
		// items by the reader.
		if vr != core.VRSQ {
			return keep(UNSequenceForScalar)
		}
		element.Header.VR = core.VRSQ
		return element, false, r.note(elementPath, tag, core.VRSQ, UNRecoveredUndefinedSequence)
	case core.RawValue:
		if vr == core.VRSQ {
			sequence, reason := r.sequence(tag, value, depth)
			if reason != "" {
				return keep(reason)
			}
			element.Header.VR = core.VRSQ
			element.Value = sequence
			return element, false, r.note(elementPath, tag, core.VRSQ, UNRecoveredSequence)
		}
		unit := vrUnitSize(vr)
		if len(value)%unit != 0 {
			return keep(UNLengthMismatch)
		}
		data := []byte(value)
		if width := r.swapWidth(tag, vr, siblings); width > 1 && r.order == binary.BigEndian {
			if len(data)%width != 0 {
				return keep(UNLengthMismatch)
			}
			data = valueencode.ReverseFixedWidth(data, width)
		}
		element.Header.VR = vr
		element.Value = core.RawValue(data)
		reason := UNRecovered
		if !vr.UsesLongExplicitLength() && len(value) > 0xFFFF {
			reason = UNRecoveredLongShortVR
		}
		return element, false, r.note(elementPath, tag, vr, reason)
	default:
		return keep(UNAmbiguousVR)
	}
}

// lookup returns the VR known for tag, the contextual token when the VR
// depends on the data set, or the reason nothing can be recovered.
func (r *unRecoverer) lookup(tag core.Tag, reservations *dictionary.PrivateReservations) (core.VR, UNReason, dictionary.ContextualVR) {
	if dictionary.IsPrivateDataTag(tag) {
		creator, err := reservations.Creator(tag)
		if err != nil {
			return "", UNPrivateNoCreator, ""
		}
		entry, ok := dictionary.LookupScopedEntry(r.dict, tag, creator)
		if !ok || entry.VR == "" {
			return "", UNPrivateNoCatalog, ""
		}
		if entry.VR == core.VRUN {
			return "", UNDictionaryUN, ""
		}
		return entry.VR, "", ""
	}
	spec, ok := dictionary.LookupVRSpec(r.dict, tag)
	if !ok {
		return "", UNUnknownAttribute, ""
	}
	switch context := spec.Context(); context {
	case "":
	case dictionary.ContextualVRUP:
		return core.VRUL, "", ""
	default:
		return "", "", context
	}
	values := spec.Values()
	if len(values) != 1 {
		return "", UNAmbiguousVR, ""
	}
	if values[0] == core.VRUN {
		return "", UNDictionaryUN, ""
	}
	return values[0], "", ""
}

// sequence parses a defined-length UN payload as Implicit VR Little Endian
// items. The payload is isolated behind an undefined-length UN wrapper in the
// object's own explicit VR byte order, so the reader converts nested binary
// values to that order, applies the configured limits, and can never read
// beyond the element: the reader's source is exactly the wrapper.
func (r *unRecoverer) sequence(tag core.Tag, payload []byte, depth int) (core.SequenceValue, UNReason) {
	if r.budget <= 0 {
		return core.SequenceValue{}, UNLimitExceeded
	}
	syntax := transfer.ExplicitVRLittleEndian
	if r.order == binary.BigEndian {
		syntax = transfer.ExplicitVRBigEndian
	}
	var stream bytes.Buffer
	header := make([]byte, 12)
	syntax.ByteOrder.PutUint16(header[0:], tag.Group)
	syntax.ByteOrder.PutUint16(header[2:], tag.Element)
	copy(header[4:], "UN")
	syntax.ByteOrder.PutUint32(header[8:], 0xFFFFFFFF)
	stream.Write(header)
	stream.Write(payload)
	// The sequence delimiter is read inside the Implicit VR Little Endian
	// scope.
	stream.Write([]byte{0xFE, 0xFF, 0xDD, 0xE0, 0, 0, 0, 0})
	remainingDepth := r.opts.MaxSequenceDepth - depth
	if remainingDepth < 2 {
		return core.SequenceValue{}, UNLimitExceeded
	}
	// No element can be larger than the payload, so a declared length beyond
	// it is structural damage, not the caller's element limit.
	elementLimit := min(r.opts.MaxElementBytes, int64(len(payload)))
	parsed, err := ReadDataSetWithOptions(bytes.NewReader(stream.Bytes()), syntax, ReadFileOptions{
		Dictionary:       r.dict,
		MaxElementBytes:  elementLimit,
		MaxElements:      r.budget,
		MaxSequenceDepth: remainingDepth,
	})
	if err != nil {
		if errors.Is(err, parser.ErrMaxDepthExceeded) || errors.Is(err, parser.ErrMaxElementsExceeded) ||
			errors.Is(err, parser.ErrMaxElementBytesExceeded) && r.opts.MaxElementBytes < int64(len(payload)) {
			return core.SequenceValue{}, UNLimitExceeded
		}
		return core.SequenceValue{}, UNInvalidSequence
	}
	elements := parsed.Elements()
	if len(elements) != 1 {
		return core.SequenceValue{}, UNInvalidSequence
	}
	sequence, ok := elements[0].Value.(core.SequenceValue)
	if !ok {
		return core.SequenceValue{}, UNInvalidSequence
	}
	r.budget -= countElements(sequence)
	return sequence, ""
}

func countElements(sequence core.SequenceValue) int {
	n := 0
	for _, item := range sequence.Items {
		for _, element := range item.Elements {
			n++
			if nested, ok := element.Value.(core.SequenceValue); ok {
				n += countElements(nested)
			}
		}
	}
	return n
}

// swapWidth is the unit a byte-order change reverses. Native OW Pixel Data is
// reversed at its sample width (Bits Allocated 32 or 64), as in PS3.5 8.
func (r *unRecoverer) swapWidth(tag core.Tag, vr core.VR, siblings []core.Element) int {
	width := valueencode.ByteOrderWidth(vr)
	if tag != core.TagPixelData || vr != core.VROW {
		return width
	}
	bits, ok, err := contextualVRResolver{order: r.order}.uint16Value(siblings, tagBitsAllocatedVR, 0)
	if err == nil && ok && (bits == 32 || bits == 64) {
		return int(bits / 8)
	}
	return width
}

// vrUnitSize is the size every value of vr is a multiple of.
func vrUnitSize(vr core.VR) int {
	if vr == core.VRAT {
		return 4
	}
	return valueencode.ByteOrderWidth(vr)
}
