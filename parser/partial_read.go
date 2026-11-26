package parser

import (
	"errors"
	"fmt"
	"io"

	"github.com/ThalesMMS/dicom-go/core"
	"github.com/ThalesMMS/dicom-go/validation"
)

// PartialReadContainer identifies an incomplete container at the first error.
// Entries are ordered outermost to innermost and contain no element values.
type PartialReadContainer struct {
	Tag core.Tag
	// ItemIndex is -1 for a sequence, or the zero-based index of an item in it.
	ItemIndex int
	// Offset is the absolute offset of the successfully decoded container header.
	Offset int64
	// Retained means the result includes this incomplete container's prefix.
	// It does not mean that the container closed or has a complete source span.
	Retained bool
}

// PartialDataSetError describes the incomplete result of ReadDataSetPartial.
// Err is the original failure; errors.Is/As still expose its cause. Path ends
// at the failed attribute when its tag is known, else at the open container.
// Containers distinguishes incomplete prefixes from genuinely completed items.
type PartialDataSetError struct {
	Path       validation.Path
	Containers []PartialReadContainer
	Err        error
}

func (e *PartialDataSetError) Error() string {
	if e == nil {
		return "dicom: incomplete data set"
	}
	return fmt.Sprintf("dicom: incomplete data set at %s: %v", e.Path, e.Err)
}

func (e *PartialDataSetError) Unwrap() error {
	if e == nil {
		return nil
	}
	return e.Err
}

// partialCollector changes only materialization on failure, never token
// framing. A single diagnostic snapshot is captured before recursive unwind.
type partialCollector struct {
	reader *Reader
	frames []PartialReadContainer
}

func (c *partialCollector) fail(err error) error {
	return c.failAt(err, core.Tag{})
}

func (c *partialCollector) failAt(err error, leaf core.Tag) error {
	var partial *PartialDataSetError
	if errors.As(err, &partial) {
		return err
	}
	path := make(validation.Path, 0, len(c.frames)/2+1)
	for _, frame := range c.frames {
		if frame.ItemIndex < 0 {
			path = append(path, validation.PathStep{Tag: frame.Tag, ItemIndex: -1})
		} else {
			path[len(path)-1].ItemIndex = frame.ItemIndex
		}
	}
	var pe *ParseError
	if leaf == (core.Tag{}) && errors.As(err, &pe) {
		leaf = pe.Tag
	}
	if leaf != (core.Tag{}) {
		// A sequence-level EOF already ends at this sequence. Attribute and
		// actual control-tag errors retain their distinct final path step.
		if len(path) == 0 || path[len(path)-1].Tag != leaf || path[len(path)-1].ItemIndex >= 0 {
			path = append(path, validation.PathStep{Tag: leaf, ItemIndex: -1})
		}
	}
	return &PartialDataSetError{Path: path, Containers: append([]PartialReadContainer(nil), c.frames...), Err: err}
}

func (c *partialCollector) retain(err error, index int) {
	var partial *PartialDataSetError
	if errors.As(err, &partial) && index < len(partial.Containers) {
		partial.Containers[index].Retained = true
	}
}

func (c *partialCollector) elements(inItem bool) ([]core.Element, error) {
	r := c.reader
	if inItem && r.inlineThreshold > 0 && !r.deferNestedValues {
		threshold := r.inlineThreshold
		r.inlineThreshold = 0
		defer func() { r.inlineThreshold = threshold }()
	}
	var elements []core.Element
	for {
		tok, err := r.Next()
		if errors.Is(err, io.EOF) {
			if !inItem {
				return elements, nil
			}
			err = &ParseError{Op: OpReadValue, Offset: r.Position(), Err: io.ErrUnexpectedEOF}
		}
		if err != nil {
			return elements, c.fail(err)
		}
		switch tok.Kind {
		case TokenElement:
			elements = append(elements, tok.Element)
		case TokenStartSequence:
			index := len(c.frames)
			c.frames = append(c.frames, PartialReadContainer{Tag: tok.Header.Tag, ItemIndex: -1, Offset: tok.Offset})
			seq, err := c.sequence(tok.Header)
			if err == nil || len(seq.Items) > 0 {
				elements = append(elements, core.Element{Header: tok.Header, Value: seq})
				if err != nil {
					c.retain(err, index)
				}
			}
			c.frames = c.frames[:index]
			if err != nil {
				return elements, err
			}
		case TokenStartPixelSequence:
			if r.skipPixelData || r.deferPixelData {
				start := r.Position()
				if err := r.discardFragmentSequence(tok.Header); err != nil {
					return elements, c.failAt(err, tok.Header.Tag)
				}
				r.recordValueLocation(tok.Header, start, r.Position()-start)
				elements = append(elements, core.Element{Header: tok.Header})
			} else {
				fragments, err := r.collectFragmentSequence(tok.Header)
				if err != nil {
					return elements, c.failAt(err, tok.Header.Tag)
				}
				elements = append(elements, core.Element{Header: tok.Header, Value: fragments})
			}
		case TokenEndItem:
			if inItem {
				return elements, nil
			}
			return elements, c.fail(unexpectedCollectorToken(tok, r.Position(), "top-level dataset"))
		default:
			return elements, c.fail(unexpectedCollectorToken(tok, r.Position(), "dataset"))
		}
	}
}

func (c *partialCollector) sequence(header core.ElementHeader) (core.SequenceValue, error) {
	r := c.reader
	var items []core.DataSet
	for {
		tok, err := r.Next()
		if errors.Is(err, io.EOF) {
			err = &ParseError{Op: OpReadValue, Offset: r.Position(), Tag: header.Tag, VR: header.VR, Length: header.Length, Err: io.ErrUnexpectedEOF}
		}
		if err != nil {
			return core.SequenceValue{Items: items}, c.fail(err)
		}
		switch tok.Kind {
		case TokenStartItem:
			index := len(c.frames)
			c.frames = append(c.frames, PartialReadContainer{Tag: header.Tag, ItemIndex: len(items), Offset: tok.Offset})
			elements, err := c.elements(true)
			if err == nil || len(elements) > 0 {
				items = append(items, core.DataSet{Elements: elements, ItemOffset: tok.Offset, ItemOffsetSet: true})
				if err != nil {
					c.retain(err, index)
				}
			}
			c.frames = c.frames[:index]
			if err != nil {
				return core.SequenceValue{Items: items}, err
			}
		case TokenEndSequence:
			return core.SequenceValue{Items: items}, nil
		default:
			return core.SequenceValue{Items: items}, c.fail(unexpectedCollectorToken(tok, r.Position(), fmt.Sprintf("sequence %s", header.Tag)))
		}
	}
}
