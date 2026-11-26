package parser

import (
	"errors"
	"fmt"
	"io"
	"math"

	"github.com/ThalesMMS/dicom-go/core"
	"github.com/ThalesMMS/dicom-go/dictionary"
)

// ErrMaxSelectiveTokensExceeded reports exhaustion of a selective reader's
// visited header/control budget, including work hidden by sequence pruning.
var ErrMaxSelectiveTokensExceeded = errors.New("dicom: selective reader token limit exceeded")

func (r *Reader) reserveSelectiveEvent(header core.ElementHeader, offset int64) error {
	s := r.selective
	// Unlimited callers check context before synthetic events and immediately
	// after header reads; no budget work is needed on that legacy path.
	if s.maxTokens == 0 {
		return nil
	}
	if err := s.ctx.Err(); err != nil {
		return err
	}
	if s.tokens >= s.maxTokens {
		return &ParseError{Op: OpReadTag, Offset: offset, Tag: header.Tag, VR: header.VR, Length: header.Length,
			Err: fmt.Errorf("%w: limit %d", ErrMaxSelectiveTokensExceeded, s.maxTokens)}
	}
	s.tokens++
	return nil
}

func (r *Reader) readSelectiveSkippedSequenceToken(header core.ElementHeader, offset int64) (Token, error) {
	if header.VR != core.VRSQ && !(header.VR == core.VRUN && header.Length.IsUndefined()) {
		return Token{}, r.selectiveDispositionError(header, offset)
	}
	if header.Length.IsDefined() {
		if err := r.skipOpaqueBody(header); err != nil {
			return Token{}, err
		}
	} else if err := r.skipUndefinedSequence(header); err != nil {
		return Token{}, err
	}
	r.delimiterCheckPending = true
	return Token{Kind: TokenElement, Header: header, Offset: offset,
		Element: core.Element{Header: header, Value: core.DiscardedValue{}}}, nil
}

// skipOpaqueBody admits the complete extent before moving into a body. Its
// descendants are deliberately not parsed, validated, counted or allocated.
func (r *Reader) skipOpaqueBody(header core.ElementHeader) error {
	if err := r.selective.ctx.Err(); err != nil {
		return err
	}
	if err := r.validateDefinedValueLength(header); err != nil {
		return err
	}
	if err := r.checkValueFits(header); err != nil {
		return err
	}
	n, offset := int64(header.Length), r.Position()
	if offset < 0 || n > math.MaxInt64-offset {
		return &ParseError{Op: OpReadValue, Offset: offset, Tag: header.Tag, VR: header.VR, Length: header.Length, Err: ErrMaxTotalBytesExceeded}
	}
	if r.maxTotalBytes > 0 && (offset > r.maxTotalBytes || n > r.maxTotalBytes-offset) {
		return &ParseError{Op: OpReadValue, Offset: offset, Tag: header.Tag, VR: header.VR, Length: header.Length, Err: ErrMaxTotalBytesExceeded}
	}
	if n == 0 {
		return nil
	}
	if seeker, ok := r.counter.r.(io.Seeker); ok {
		_, limitErr, err := r.selectiveSeekAvailable(seeker, n)
		if err == nil {
			err = limitErr
		}
		if err != nil {
			return &ParseError{Op: OpReadValue, Offset: offset, Tag: header.Tag, VR: header.VR, Length: header.Length, Err: err}
		}
		if _, err := seeker.Seek(n, io.SeekCurrent); err != nil {
			return &ParseError{Op: OpReadValue, Offset: offset, Tag: header.Tag, VR: header.VR, Length: header.Length, Err: err}
		}
		r.counter.pos += n
		return r.selective.ctx.Err()
	}
	var scratch [definedValueReadChunkSize]byte
	for n > 0 {
		if err := r.selective.ctx.Err(); err != nil {
			return err
		}
		count := int64(len(scratch))
		if count > n {
			count = n
		}
		if _, err := io.ReadFull(r.counter, scratch[:int(count)]); err != nil {
			return &ParseError{Op: OpReadValue, Offset: offset, Tag: header.Tag, VR: header.VR, Length: header.Length, Err: requiredReadError(err)}
		}
		n -= count
	}
	return nil
}

// skipUndefinedSequence reuses the ordinary container/decoder/private scope
// state without exposing hidden callbacks or paths. Only undefined containers
// require visited headers; defined children are opaque admitted extents.
func (r *Reader) skipUndefinedSequence(header core.ElementHeader) error {
	baseDepth := len(r.seqDelimiters)
	if _, err := r.pushContainer(seqTokenTypeSequence, header, header.VR == core.VRUN); err != nil {
		return err
	}
	r.delimiterCheckPending = true
	oldSkip, oldSkipping := r.skipPixelData, r.selective.skippingPixelSequence
	r.skipPixelData, r.selective.skippingPixelSequence = true, true
	defer func() {
		r.skipPixelData, r.selective.skippingPixelSequence = oldSkip, oldSkipping
	}()
	for len(r.seqDelimiters) > baseDepth {
		if err := r.selective.ctx.Err(); err != nil {
			return err
		}
		if tok, ok, err := r.updateSeqDelimiters(); ok || err != nil {
			if err != nil {
				return err
			}
			if err := r.reserveSelectiveEvent(tok.Header, r.Position()); err != nil {
				return err
			}
			continue
		}
		if err := r.checkTotalBytes(); err != nil {
			return err
		}
		offset := r.Position()
		h, err := r.readHeader()
		if err != nil {
			if errors.Is(err, io.EOF) {
				return &ParseError{Op: OpReadTag, Offset: offset, Err: io.ErrUnexpectedEOF}
			}
			return err
		}
		if r.selective.maxTokens == 0 {
			if err := r.selective.ctx.Err(); err != nil {
				return err
			}
		}
		if err := r.reserveSelectiveEvent(h, offset); err != nil {
			return err
		}
		if h.Tag.IsItem() && h.Length.IsDefined() && r.seqDelimiters[len(r.seqDelimiters)-1].typ != seqTokenTypePixelSequence {
			if err := r.validateItemStart(h); err != nil {
				return err
			}
			if err := r.checkSequenceDepthLimit(h.Length); err != nil {
				return err
			}
			if err := r.skipOpaqueBody(h); err != nil {
				return err
			}
			r.seqDelimiters[len(r.seqDelimiters)-1].items++
			r.delimiterCheckPending = true
			continue
		}
		if _, ok, err := r.controlToken(h); ok || err != nil {
			if errors.Is(err, errDelimiterSkipped) {
				continue
			}
			if err != nil {
				return err
			}
			continue
		}
		parent := r.seqDelimiters[len(r.seqDelimiters)-1]
		if parent.typ != seqTokenTypeItem {
			return &ParseError{Op: OpReadTag, Offset: offset, Tag: h.Tag, VR: h.VR, Length: h.Length, Err: ErrUnexpectedSequenceControlTag}
		}
		if h.VR == core.VRSQ || (h.VR == core.VRUN && h.Length.IsUndefined()) {
			if h.Length.IsDefined() {
				if err := r.skipOpaqueBody(h); err != nil {
					return err
				}
			} else if _, err := r.pushContainer(seqTokenTypeSequence, h, h.VR == core.VRUN); err != nil {
				return err
			}
			r.delimiterCheckPending = true
			continue
		}
		if isEncapsulatedPixelDataHeader(h) {
			if err := r.pushPixelSequence(h); err != nil {
				return err
			}
			r.pixelSequenceOffsetTablePending, r.delimiterCheckPending = true, true
			r.pixelDataBytes = 0
			continue
		}
		if h.Length.IsUndefined() {
			return &ParseError{Op: OpReadValue, Offset: offset, Tag: h.Tag, VR: h.VR, Length: h.Length, Err: ErrUnsupportedUndefinedLength}
		}
		if err := r.checkValueFits(h); err != nil {
			return err
		}
		if r.privateRoot != nil && dictionary.IsPrivateCreatorTag(h.Tag) {
			if _, err := r.readDefinedValueToken(h); err != nil {
				return err
			}
		} else if _, err := r.readSelectiveSkippedValueToken(h); err != nil {
			return err
		}
	}
	return nil
}
