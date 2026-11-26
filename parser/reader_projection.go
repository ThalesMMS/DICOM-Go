package parser

import (
	"errors"

	"github.com/ThalesMMS/dicom-go/core"
	"github.com/ThalesMMS/dicom-go/dictionary"
)

var ErrDataSetProjectionOptions = errors.New("dicom: invalid metadata dataset projection options")

type dataSetProjection struct {
	tags          map[core.Tag]struct{}
	discarding    bool
	seekSequences bool
}

func (p *dataSetProjection) retains(tag core.Tag) bool {
	// Character-set and contextual VR interpretation of a selected subtree can
	// depend on root attributes encountered later. Creators also own resolution
	// of selected private root values. Keep their source values, not guesses.
	if dictionary.IsPrivateCreatorTag(tag) || tag == core.NewTag(0x0008, 0x0005) ||
		tag == core.NewTag(0x0028, 0x0103) || tag == core.NewTag(0x0028, 0x0100) ||
		tag == core.NewTag(0x0028, 0x3002) {
		return true
	}
	_, ok := p.tags[tag]
	return ok
}

// ReadDataSetProjected structurally parses the complete ordinary token stream
// while retaining selected root attributes and their complete sequence subtrees.
// Other root headers remain with core.DiscardedValue, preventing serialization
// or confusion with absent attributes. Unselected SQ/item collections are not
// allocated, but every descendant header, length and boundary is still parsed.
// Selection never assumes sorted tags or stops at Pixel Data. Character-set,
// contextual VR and private-creator dependencies are retained automatically.
//
// With SeekUnselectedSequences, unselected defined-length root sequences of a
// seekable source are passed by seek instead (see ReaderOptions).
//
// This metadata-only operation requires SkipPixelData and an untouched ordinary
// reader. Sinks, uniform validation, selective/prefix, partial/incomplete and
// deferred-value modes are excluded. It preserves ordinary structural parser
// checks and recovery diagnostics; it does not perform IOD/value validation.
func (r *Reader) ReadDataSetProjected(tags []core.Tag) (core.DataSet, error) {
	if r.projection != nil || r.selective != nil || r.validationLifecycle != nil ||
		r.frameSink != nil || r.encapsulatedSink != nil || !r.skipPixelData ||
		r.deferPixelData || r.deferWaveformData || r.inlineThreshold != 0 || len(r.bulkDataRules) != 0 ||
		r.allowIncompletePixelData || r.elementCount != 0 || len(r.seqDelimiters) != 0 {
		return core.DataSet{}, ErrDataSetProjectionOptions
	}
	projection := &dataSetProjection{tags: make(map[core.Tag]struct{}, len(tags)), seekSequences: r.seekUnselectedSequences}
	for _, tag := range tags {
		if tag.IsSequenceDelimiting() {
			return core.DataSet{}, ErrDataSetProjectionOptions
		}
		projection.tags[tag] = struct{}{}
	}
	r.projection = projection
	r.slurpEligible = false
	defer func() { r.projection = nil }()
	return r.ReadDataSet()
}

func (r *Reader) projectionDiscardsValue(header core.ElementHeader) bool {
	if r.projection == nil || dictionary.IsPrivateCreatorTag(header.Tag) {
		return false
	}
	return r.projection.discarding || len(r.seqDelimiters) == 0 && !r.projection.retains(header.Tag)
}

func (r *Reader) appendProjectedElement(elements []core.Element, element core.Element, inItem bool) []core.Element {
	if r.projection != nil {
		if r.projection.discarding {
			return elements
		}
		if !inItem && !r.projection.retains(element.Tag()) && !(isPixelDataValueTag(element.Tag()) && element.Value == nil) {
			element.Value = core.DiscardedValue{}
		}
	}
	return append(elements, element)
}

// seekUnselectedSequence passes an unselected defined-length root sequence by
// one seek (SeekUnselectedSequences). It reports false, leaving the reader
// untouched, whenever seekPast would not be indistinguishable from reading
// the bytes; the caller then parses the sequence, keeping every failure.
func (r *Reader) seekUnselectedSequence(header core.ElementHeader, headerOffset int64) (Token, bool) {
	if r.projection == nil || !r.projection.seekSequences || r.projection.discarding || len(r.seqDelimiters) != 0 ||
		header.VR != core.VRSQ || header.Length.IsUndefined() || r.projection.retains(header.Tag) {
		return Token{}, false
	}
	if !r.counter.seekPast(int64(header.Length), r.sourceEnd) {
		return Token{}, false
	}
	r.delimiterCheckPending = true
	return Token{Kind: TokenElement, Header: header, Offset: headerOffset, Element: core.Element{Header: header, Value: core.DiscardedValue{}}}, true
}
