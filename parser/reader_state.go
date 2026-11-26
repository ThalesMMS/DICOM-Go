package parser

import (
	"errors"
	"fmt"
	"io"

	"github.com/ThalesMMS/dicom-go/core"
	"github.com/ThalesMMS/dicom-go/validation"
)

func (r *Reader) Next() (Token, error) {
	if failure := r.incompleteSourceFailure(nil); failure != nil {
		return Token{}, failure
	}
	if err := r.rejectIncompleteCollectionMode(); err != nil {
		return Token{}, err
	}
	if r.incompleteTerminal {
		return Token{}, io.EOF
	}
	if r.privateInitError != nil {
		return Token{}, r.privateInitError
	}
	if err := r.encodedContextError(); err != nil {
		return Token{}, err
	}
	if r.selective != nil {
		return r.nextSelective()
	}
	if r.validationLifecycle != nil {
		for {
			tok, err := r.nextToken(true)
			if errors.Is(err, errLifecycleFiltered) {
				continue
			}
			return tok, err
		}
	}
	if tok, ok, err := r.updateSeqDelimiters(); ok || err != nil {
		return tok, err
	}
	if err := r.checkTotalBytes(); err != nil {
		return Token{}, err
	}
	headerOffset := r.Position()
	header, err := r.readHeader()
	if failure := r.incompleteSourceFailure(err); failure != nil {
		return Token{}, failure
	}
	if err != nil {
		if errors.Is(err, io.EOF) && len(r.seqDelimiters) > 0 {
			return Token{}, &ParseError{
				Op:     OpReadTag,
				Offset: r.Position(),
				Err:    io.ErrUnexpectedEOF,
			}
		}
		return Token{}, err
	}
	if tok, ok, err := r.controlToken(header); ok || err != nil {
		if errors.Is(err, errDelimiterSkipped) {
			return r.Next()
		}
		tok.Offset = headerOffset
		return tok, err
	}
	if len(r.seqDelimiters) > 0 && r.seqDelimiters[len(r.seqDelimiters)-1].typ == seqTokenTypePixelSequence {
		return Token{}, &ParseError{
			Op:     OpReadValue,
			Offset: r.Position(),
			Tag:    header.Tag,
			VR:     header.VR,
			Length: header.Length,
			Err:    fmt.Errorf("dicom: unexpected tag %s inside encapsulated Pixel Data", header.Tag),
		}
	}
	if tok, ok := r.seekUnselectedSequence(header, headerOffset); ok {
		return tok, nil
	}
	if header.VR == core.VRSQ || (header.VR == core.VRUN && header.Length.IsUndefined()) {
		if _, err := r.pushContainer(seqTokenTypeSequence, header, header.VR == core.VRUN); err != nil {
			return Token{}, err
		}
		r.delimiterCheckPending = true
		return Token{Kind: TokenStartSequence, Header: header, Offset: headerOffset}, nil
	}
	if (header.VR == core.VROB || header.VR == core.VROW) && header.Tag == core.TagPixelData && header.Length.IsUndefined() {
		if err := r.pushPixelSequence(header); err != nil {
			return Token{}, err
		}
		r.pixelSequenceOffsetTablePending = true
		r.pixelDataBytes = 0
		r.delimiterCheckPending = true
		if r.encapsulatedSink != nil {
			return r.streamEncodedPixelData(header, headerOffset)
		}
		return Token{Kind: TokenStartPixelSequence, Header: header, Offset: headerOffset}, nil
	}
	if header.Length.IsUndefined() {
		return Token{}, &ParseError{
			Op:     OpReadValue,
			Offset: r.Position(),
			Tag:    header.Tag,
			VR:     header.VR,
			Length: header.Length,
			Err:    ErrUnsupportedUndefinedLength,
		}
	}
	tok, err := r.readDefinedValueToken(header)
	if failure := r.incompleteSourceFailure(err); failure != nil {
		return Token{}, failure
	}
	tok.Offset = headerOffset
	return tok, err
}

func (r *Reader) nextToken(runLifecycle bool) (Token, error) {
	if tok, ok, err := r.updateSeqDelimiters(); ok || err != nil {
		return r.finishLifecycleToken(tok, err, runLifecycle)
	}
	if err := r.checkTotalBytes(); err != nil {
		return Token{}, err
	}
	headerOffset := r.Position()
	header, err := r.readHeader()
	if err != nil {
		if errors.Is(err, io.EOF) && len(r.seqDelimiters) > 0 {
			return Token{}, &ParseError{
				Op:     OpReadTag,
				Offset: r.Position(),
				Err:    io.ErrUnexpectedEOF,
			}
		}
		return Token{}, err
	}
	if tok, ok, err := r.controlToken(header); ok || err != nil {
		if errors.Is(err, errDelimiterSkipped) {
			return r.nextToken(runLifecycle)
		}
		tok.Offset = headerOffset
		return r.finishLifecycleToken(tok, err, runLifecycle)
	}
	if len(r.seqDelimiters) > 0 && r.seqDelimiters[len(r.seqDelimiters)-1].typ == seqTokenTypePixelSequence {
		return Token{}, &ParseError{
			Op:     OpReadValue,
			Offset: r.Position(),
			Tag:    header.Tag,
			VR:     header.VR,
			Length: header.Length,
			Err:    fmt.Errorf("dicom: unexpected tag %s inside encapsulated Pixel Data", header.Tag),
		}
	}
	headerResult, err := r.handleLifecycleHeader(header, headerOffset, runLifecycle)
	if err != nil {
		return Token{}, err
	}
	if headerResult.SkipValue || headerResult.DeferValue {
		if header.VR == core.VRSQ || header.Length.IsUndefined() {
			return Token{}, fmt.Errorf("%w: header hook can only skip or defer a defined-length primitive", validation.ErrHookAction)
		}
		tok, err := r.readLifecycleSkippedValueToken(header, headerResult.DeferValue)
		tok.Offset = headerOffset
		return r.finishLifecycleToken(tok, err, runLifecycle)
	}
	if header.VR == core.VRSQ || (header.VR == core.VRUN && header.Length.IsUndefined()) {
		if _, err := r.pushContainer(seqTokenTypeSequence, header, header.VR == core.VRUN); err != nil {
			return Token{}, err
		}
		r.delimiterCheckPending = true
		return r.finishLifecycleToken(Token{Kind: TokenStartSequence, Header: header, Offset: headerOffset}, nil, runLifecycle)
	}
	if (header.VR == core.VROB || header.VR == core.VROW) && header.Tag == core.TagPixelData && header.Length.IsUndefined() {
		if err := r.pushPixelSequence(header); err != nil {
			return Token{}, err
		}
		r.pixelSequenceOffsetTablePending = true
		r.pixelDataBytes = 0
		r.delimiterCheckPending = true
		if r.encapsulatedSink != nil {
			return r.streamEncodedPixelData(header, headerOffset)
		}
		return r.finishLifecycleToken(Token{Kind: TokenStartPixelSequence, Header: header, Offset: headerOffset}, nil, runLifecycle)
	}
	if header.Length.IsUndefined() {
		return Token{}, &ParseError{
			Op:     OpReadValue,
			Offset: r.Position(),
			Tag:    header.Tag,
			VR:     header.VR,
			Length: header.Length,
			Err:    ErrUnsupportedUndefinedLength,
		}
	}
	tok, err := r.readDefinedValueToken(header)
	tok.Offset = headerOffset
	return r.finishLifecycleToken(tok, err, runLifecycle)
}

// ReadAll returns non-SQ elements in encounter order. Sequence/item boundaries
// are ignored, so SQ nesting is lost; encapsulated Pixel Data is preserved as a
// FragmentSequence. Prefer ReadDataSet when callers need full structure.
func (r *Reader) ReadAll() (elements []core.Element, err error) {
	defer func() {
		err = r.closeSinks(err)
	}()
	for {
		tok, err := r.Next()
		if failure := r.incompleteSourceFailure(err); failure != nil {
			return nil, failure
		}
		if errors.Is(err, io.EOF) {
			return elements, r.incompleteCollectionError()
		}
		if err != nil {
			return nil, err
		}
		switch tok.Kind {
		case TokenElement:
			elements = append(elements, tok.Element)
		case TokenStartPixelSequence:
			if r.skipPixelData || r.deferPixelData {
				valueStart := r.Position()
				if err := r.discardFragmentSequence(tok.Header); err != nil {
					return nil, err
				}
				r.recordValueLocation(tok.Header, valueStart, r.Position()-valueStart)
				elements = append(elements, core.Element{
					Header: tok.Header,
					Value:  nil,
				})
				continue
			}
			fragments, err := r.collectFragmentSequence(tok.Header)
			if err != nil {
				return nil, err
			}
			elements = append(elements, core.Element{
				Header: tok.Header,
				Value:  fragments,
			})
		}
	}
}

// ReadDataSet materializes the token stream into an ordered in-memory data set,
// preserving nested SQ items as core.SequenceValue. This is the preferred API
// for callers that need full sequence structure; ReadAll only returns primitive
// element tokens and therefore discards sequence nesting.
func (r *Reader) ReadDataSet() (dataset core.DataSet, err error) {
	defer func() {
		err = r.closeSinks(err)
	}()
	if r.validationLifecycle == nil {
		elements, err := r.collectElementsDefault(false)
		if err != nil {
			return core.DataSet{}, err
		}
		return core.DataSet{Elements: elements}, r.incompleteCollectionError()
	}
	elements, err := r.collectElementsValidated(false)
	if err != nil {
		var parseErr *ParseError
		if errors.As(err, &parseErr) {
			r.validationLifecycle.operation.DiagnoseParsedPrefix(core.DataSet{Elements: elements})
		}
		return core.DataSet{}, err
	}
	dataset = core.DataSet{Elements: elements}
	result, validationErr := r.validationLifecycle.operation.ValidateParsedDataSet(dataset)
	return result.DataSet, validationErr
}

// ReadDataSetPartial reads like ReadDataSet but, when the stream fails, also
// returns completed elements and items inside interrupted sequences. An open
// item retains only its completed element prefix; incomplete SQ/item wrappers
// are identified by PartialDataSetError. Truncated primitives and incomplete
// encapsulated Pixel Data are dropped. A nil error means the data set is
// complete. Readers with validation lifecycle retain ReadDataSet's strict
// behavior, so no incomplete item or sequence is passed to completion hooks.
func (r *Reader) ReadDataSetPartial() (dataset core.DataSet, err error) {
	if r.allowIncompletePixelData {
		return core.DataSet{}, ErrIncompletePixelDataOptions
	}
	if r.validationLifecycle != nil {
		dataset, err = r.ReadDataSet()
		return dataset, err
	}
	defer func() {
		err = r.closeSinks(err)
	}()
	collector := partialCollector{reader: r}
	elements, err := collector.elements(false)
	return core.DataSet{Elements: elements}, err
}

func (r *Reader) collectElementsDefault(inItem bool) ([]core.Element, error) {
	return r.collectElementsPrefix(inItem, false)
}

func (r *Reader) collectElementsPrefix(inItem, allowRootStop bool) ([]core.Element, error) {
	if inItem && r.inlineThreshold > 0 && !r.deferNestedValues {
		inlineThreshold := r.inlineThreshold
		r.inlineThreshold = 0
		defer func() {
			r.inlineThreshold = inlineThreshold
		}()
	}

	var elements []core.Element
	for {
		tok, err := r.Next()
		if failure := r.incompleteSourceFailure(err); failure != nil {
			return nil, failure
		}
		if errors.Is(err, io.EOF) {
			if inItem {
				return elements, &ParseError{
					Op:     OpReadValue,
					Offset: r.Position(),
					Err:    io.ErrUnexpectedEOF,
				}
			}
			return elements, nil
		}
		if err != nil {
			return elements, err
		}

		switch tok.Kind {
		case TokenStop:
			if allowRootStop && !inItem {
				return elements, &prefixStop{ReadStop{Stopped: true, Header: tok.Header, Offset: tok.Offset}}
			}
			return elements, unexpectedCollectorToken(tok, r.Position(), "complete dataset")
		case TokenElement:
			elements = r.appendProjectedElement(elements, tok.Element, inItem)
		case TokenStartSequence:
			discard := r.projection != nil && !inItem && !r.projection.retains(tok.Header.Tag)
			if discard {
				r.projection.discarding = true
			}
			seq, err := r.collectSequenceDefault(tok.Header)
			if discard {
				r.projection.discarding = false
			}
			if err != nil {
				return elements, err
			}
			elements = r.appendProjectedElement(elements, core.Element{
				Header: tok.Header,
				Value:  seq,
			}, inItem)
		case TokenStartPixelSequence:
			if r.skipPixelData || r.deferPixelData {
				valueStart := r.Position()
				if err := r.discardFragmentSequence(tok.Header); err != nil {
					return elements, err
				}
				r.recordValueLocation(tok.Header, valueStart, r.Position()-valueStart)
				elements = r.appendProjectedElement(elements, core.Element{
					Header: tok.Header,
					Value:  nil,
				}, inItem)
				continue
			}
			fragments, err := r.collectFragmentSequence(tok.Header)
			if err != nil {
				return elements, err
			}
			elements = r.appendProjectedElement(elements, core.Element{
				Header: tok.Header,
				Value:  fragments,
			}, inItem)
		case TokenEndItem:
			if !inItem {
				return elements, unexpectedCollectorToken(tok, r.Position(), "top-level dataset")
			}
			return elements, nil
		case TokenStartItem:
			return elements, unexpectedCollectorToken(tok, r.Position(), "dataset")
		case TokenEndSequence:
			return elements, unexpectedCollectorToken(tok, r.Position(), "dataset")
		default:
			return elements, &ParseError{
				Op:     OpReadValue,
				Offset: r.Position(),
				Err:    fmt.Errorf("dicom: unsupported token kind %d while collecting dataset", tok.Kind),
			}
		}
	}
}

func (r *Reader) collectSequenceDefault(header core.ElementHeader) (core.SequenceValue, error) {
	var items []core.DataSet
	for {
		tok, err := r.Next()
		if errors.Is(err, io.EOF) {
			return core.SequenceValue{}, &ParseError{
				Op:     OpReadValue,
				Offset: r.Position(),
				Tag:    header.Tag,
				VR:     header.VR,
				Length: header.Length,
				Err:    io.ErrUnexpectedEOF,
			}
		}
		if err != nil {
			return core.SequenceValue{}, err
		}

		switch tok.Kind {
		case TokenStartItem:
			elements, err := r.collectElementsDefault(true)
			if err != nil {
				return core.SequenceValue{}, err
			}
			if r.projection == nil || !r.projection.discarding {
				items = append(items, core.DataSet{Elements: elements, ItemOffset: tok.Offset, ItemOffsetSet: true})
			}
		case TokenEndSequence:
			return core.SequenceValue{Items: items}, nil
		default:
			return core.SequenceValue{}, unexpectedCollectorToken(tok, r.Position(), fmt.Sprintf("sequence %s", header.Tag))
		}
	}
}

func (r *Reader) collectElementsValidated(inItem bool) ([]core.Element, error) {
	if inItem && r.inlineThreshold > 0 && !r.deferNestedValues {
		inlineThreshold := r.inlineThreshold
		r.inlineThreshold = 0
		defer func() {
			r.inlineThreshold = inlineThreshold
		}()
	}

	var elements []core.Element
	for {
		tok, err := r.Next()
		if errors.Is(err, io.EOF) {
			if inItem {
				return elements, &ParseError{
					Op:     OpReadValue,
					Offset: r.Position(),
					Err:    io.ErrUnexpectedEOF,
				}
			}
			return elements, nil
		}
		if err != nil {
			return elements, err
		}

		switch tok.Kind {
		case TokenElement:
			elements = append(elements, tok.Element)
		case TokenStartSequence:
			path := r.lifecycleElementPath(tok.Header.Tag)
			seq, err := r.collectSequenceValidated(tok.Header, path)
			element := core.Element{Header: tok.Header, Value: seq}
			if err != nil {
				// Keep complete descendants for the bounded diagnostics pass.
				// The incomplete sequence never reaches a completion hook.
				if len(seq.Items) > 0 {
					elements = append(elements, element)
				}
				return elements, err
			}
			element, filtered, err := r.handleCompletedElement(path, tok.Offset, validation.HookSequenceComplete, element)
			if err != nil {
				return elements, err
			}
			if !filtered {
				elements = append(elements, element)
			}
		case TokenStartPixelSequence:
			path := r.lifecycleElementPath(tok.Header.Tag)
			if r.skipPixelData || r.deferPixelData {
				valueStart := r.Position()
				if err := r.discardFragmentSequence(tok.Header); err != nil {
					return elements, err
				}
				r.recordValueLocation(tok.Header, valueStart, r.Position()-valueStart)
				element := core.Element{
					Header: tok.Header,
					Value:  nil,
				}
				element, filtered, hookErr := r.handleCompletedElement(path, tok.Offset, "", element)
				if hookErr != nil {
					return elements, hookErr
				}
				if !filtered {
					elements = append(elements, element)
				}
				continue
			}
			fragments, err := r.collectFragmentSequence(tok.Header)
			if err != nil {
				return elements, err
			}
			element := core.Element{
				Header: tok.Header,
				Value:  fragments,
			}
			element, filtered, hookErr := r.handleCompletedElement(path, tok.Offset, "", element)
			if hookErr != nil {
				return elements, hookErr
			}
			if !filtered {
				elements = append(elements, element)
			}
		case TokenEndItem:
			if !inItem {
				return elements, unexpectedCollectorToken(tok, r.Position(), "top-level dataset")
			}
			return elements, nil
		case TokenStartItem:
			return elements, unexpectedCollectorToken(tok, r.Position(), "dataset")
		case TokenEndSequence:
			return elements, unexpectedCollectorToken(tok, r.Position(), "dataset")
		default:
			return elements, &ParseError{
				Op:     OpReadValue,
				Offset: r.Position(),
				Err:    fmt.Errorf("dicom: unsupported token kind %d while collecting dataset", tok.Kind),
			}
		}
	}
}

func (r *Reader) collectSequenceValidated(header core.ElementHeader, sequencePath validation.Path) (core.SequenceValue, error) {
	var items []core.DataSet
	for {
		tok, err := r.Next()
		if errors.Is(err, io.EOF) {
			return core.SequenceValue{Items: items}, &ParseError{
				Op:     OpReadValue,
				Offset: r.Position(),
				Tag:    header.Tag,
				VR:     header.VR,
				Length: header.Length,
				Err:    io.ErrUnexpectedEOF,
			}
		}
		if err != nil {
			return core.SequenceValue{Items: items}, err
		}

		switch tok.Kind {
		case TokenStartItem:
			elements, err := r.collectElementsValidated(true)
			item := core.DataSet{Elements: elements, ItemOffset: tok.Offset, ItemOffsetSet: true}
			if err != nil {
				if len(elements) > 0 {
					items = append(items, item)
				}
				return core.SequenceValue{Items: items}, err
			}
			if r.validationLifecycle != nil && r.validationSuppressed == 0 {
				itemPath := sequencePath.Clone()
				itemPath[len(itemPath)-1].ItemIndex = len(items)
				if _, err := r.validationLifecycle.operation.Handle(validation.HookEvent{Point: validation.HookItemComplete, Path: itemPath, DataSet: &item, Offset: tok.Offset, OffsetSet: true}); err != nil {
					return core.SequenceValue{Items: items}, err
				}
			}
			items = append(items, item)
		case TokenEndSequence:
			return core.SequenceValue{Items: items}, nil
		default:
			return core.SequenceValue{Items: items}, unexpectedCollectorToken(tok, r.Position(), fmt.Sprintf("sequence %s", header.Tag))
		}
	}
}

func (r *Reader) collectFragmentSequence(header core.ElementHeader) (core.FragmentSequence, error) {
	var value core.FragmentSequence
	var sawOffsetTable bool
	valueOffset := r.Position()

	for {
		start := r.Position()
		tok, err := r.Next()
		if failure := r.incompleteSourceFailure(err); failure != nil {
			return core.FragmentSequence{}, failure
		}
		if err != nil {
			if recovered, ok := r.finishIncompleteFragments(header, start, valueOffset, value, sawOffsetTable, err); ok {
				return recovered, nil
			}
			if failure := r.incompleteSourceFailure(err); failure != nil {
				return core.FragmentSequence{}, failure
			}
		}
		if errors.Is(err, io.EOF) {
			return core.FragmentSequence{}, &ParseError{
				Op:     OpReadValue,
				Offset: r.Position(),
				Tag:    header.Tag,
				VR:     header.VR,
				Length: header.Length,
				Err:    io.ErrUnexpectedEOF,
			}
		}
		if err != nil {
			return core.FragmentSequence{}, err
		}

		switch tok.Kind {
		case TokenElement:
			raw, ok := tok.Element.RawBytes()
			if !ok {
				return core.FragmentSequence{}, unexpectedCollectorToken(tok, r.Position(), fmt.Sprintf("pixel sequence %s", header.Tag))
			}
			if !sawOffsetTable {
				value.OffsetTable = core.CloneBytes(raw)
				sawOffsetTable = true
				continue
			}
			value.Fragments = append(value.Fragments, core.CloneBytes(raw))
		case TokenEndSequence:
			if !sawOffsetTable {
				return core.FragmentSequence{}, &ParseError{
					Op:     OpReadValue,
					Offset: r.Position(),
					Tag:    header.Tag,
					VR:     header.VR,
					Length: header.Length,
					Err:    ErrMissingBasicOffsetTable,
				}
			}
			return value, nil
		default:
			return core.FragmentSequence{}, unexpectedCollectorToken(tok, r.Position(), fmt.Sprintf("pixel sequence %s", header.Tag))
		}
	}
}

func (r *Reader) discardFragmentSequence(header core.ElementHeader) error {
	for {
		tok, err := r.Next()
		if errors.Is(err, io.EOF) {
			return &ParseError{
				Op:     OpReadValue,
				Offset: r.Position(),
				Tag:    header.Tag,
				VR:     header.VR,
				Length: header.Length,
				Err:    io.ErrUnexpectedEOF,
			}
		}
		if err != nil {
			return err
		}

		switch tok.Kind {
		case TokenElement:
			continue
		case TokenEndSequence:
			return nil
		default:
			return unexpectedCollectorToken(tok, r.Position(), fmt.Sprintf("pixel sequence %s", header.Tag))
		}
	}
}

func unexpectedCollectorToken(tok Token, offset int64, context string) error {
	return &ParseError{
		Op:     OpReadValue,
		Offset: offset,
		Tag:    tok.Header.Tag,
		VR:     tok.Header.VR,
		Length: tok.Header.Length,
		Err:    fmt.Errorf("dicom: unexpected %s while collecting %s", tok.Kind, context),
	}
}
