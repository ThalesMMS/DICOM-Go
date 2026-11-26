package parser

import (
	"encoding/binary"
	"errors"
	"fmt"
	"io"

	"github.com/ThalesMMS/dicom-go/core"
	"github.com/ThalesMMS/dicom-go/dictionary"
	dicomenc "github.com/ThalesMMS/dicom-go/encoding"
)

// errDelimiterSkipped tells a caller of controlToken that a delimiter
// anomaly was skipped and the next token must be read.
var errDelimiterSkipped = errors.New("dicom: delimiter anomaly skipped")

func (r *Reader) controlToken(header core.ElementHeader) (Token, bool, error) {
	if !header.Tag.IsSequenceDelimiting() {
		return Token{}, false, nil
	}
	swapped := r.swappedItemPending
	r.swappedItemPending = false
	if r.seqRecovery.ReplaceWrongDelimiter {
		if tok, ok := r.replaceWrongDelimiter(header); ok {
			return tok, true, nil
		}
	}
	if r.recoverDelimiterAnomalies {
		skipped, err := r.recoverDelimiter(&header)
		if err != nil {
			return Token{}, true, err
		}
		if skipped {
			return Token{}, true, errDelimiterSkipped
		}
	}

	switch {
	case header.Tag.IsItem():
		if len(r.seqDelimiters) > 0 && r.seqDelimiters[len(r.seqDelimiters)-1].typ == seqTokenTypePixelSequence {
			if header.Length.IsUndefined() {
				return Token{}, true, &ParseError{
					Op:     OpReadValue,
					Offset: r.Position(),
					Tag:    header.Tag,
					VR:     header.VR,
					Length: header.Length,
					Err:    ErrUnsupportedUndefinedLength,
				}
			}
			tok, err := r.readPixelSequenceItemToken(header)
			return tok, true, err
		}
		if err := r.validateItemStart(header); err != nil {
			return Token{}, true, err
		}
		skip, err := r.pushContainer(seqTokenTypeItem, header, false)
		if err != nil {
			return Token{}, true, err
		}
		if swapped && !skip {
			r.seqDelimiters[len(r.seqDelimiters)-1].explicitBigEndian = true
		}
		r.delimiterCheckPending = true
		if skip {
			// The header ended at its container end: no item, and the
			// container closes here.
			tok, ok, err := r.updateSeqDelimiters()
			if !ok && err == nil {
				err = &ParseError{Op: OpReadValue, Offset: r.Position(), Err: fmt.Errorf("%w: container did not end after an empty item header", ErrContainerOverrun)}
			}
			return tok, true, err
		}
		return Token{Kind: TokenStartItem, Header: header}, true, nil
	case header.Tag.IsItemDelimitationItem():
		if err := validateDelimiterLength(header, r.Position()); err != nil {
			return Token{}, true, err
		}
		if err := r.popUndefinedLengthSequenceToken(header, seqTokenTypeItem, ErrUnexpectedItemDelimiter); err != nil {
			return Token{}, true, err
		}
		r.delimiterCheckPending = true
		return Token{Kind: TokenEndItem, Header: header}, true, nil
	case header.Tag.IsSequenceDelimitationItem():
		if err := validateDelimiterLength(header, r.Position()); err != nil {
			return Token{}, true, err
		}
		if len(r.seqDelimiters) > 0 && r.seqDelimiters[len(r.seqDelimiters)-1].typ == seqTokenTypePixelSequence {
			if err := r.popUndefinedLengthSequenceToken(header, seqTokenTypePixelSequence, ErrUnexpectedSequenceDelimiter); err != nil {
				return Token{}, true, err
			}
			r.pixelSequenceOffsetTablePending = false
			r.delimiterCheckPending = true
			return Token{Kind: TokenEndSequence, Header: header}, true, nil
		}
		if err := r.popUndefinedLengthSequenceToken(header, seqTokenTypeSequence, ErrUnexpectedSequenceDelimiter); err != nil {
			return Token{}, true, err
		}
		r.delimiterCheckPending = true
		return Token{Kind: TokenEndSequence, Header: header}, true, nil
	default:
		return Token{}, true, &ParseError{
			Op:     OpReadTag,
			Offset: r.Position(),
			Tag:    header.Tag,
			VR:     header.VR,
			Length: header.Length,
			Err:    fmt.Errorf("%w: %s", ErrUnexpectedSequenceControlTag, header.Tag),
		}
	}
}

func (r *Reader) readPixelSequenceItemToken(header core.ElementHeader) (Token, error) {
	if err := r.checkValueFits(header); err != nil {
		return Token{}, err
	}
	if err := r.reservePixelDataBytes(header); err != nil {
		return Token{}, err
	}
	if err := r.checkFragmentCountLimit(header); err != nil {
		return Token{}, err
	}

	if r.encodedPixelActive {
		return r.streamEncodedItem(header)
	}
	if r.skipPixelData || r.deferPixelData {
		valueOffset := r.Position()
		var err error
		if r.selective != nil && r.selective.skippingPixelSequence {
			if err = r.validateDefinedValueLength(header); err == nil {
				err = r.selectiveSkipN(valueOffset, header, int64(header.Length))
			}
		} else {
			err = r.skipN(valueOffset, header, int64(header.Length))
		}
		if err != nil {
			return Token{}, err
		}
		if r.pixelSequenceOffsetTablePending {
			r.pixelSequenceOffsetTablePending = false
		} else {
			r.fragmentCount++
		}
		r.elementCount++
		r.delimiterCheckPending = true
		return Token{
			Kind:   TokenElement,
			Header: header,
			Element: core.Element{
				Header: header,
				Value:  nil,
			},
		}, nil
	}

	// Fragment items must stay materialized so collectFragmentSequence can
	// distinguish the Basic Offset Table from encoded fragments.
	inlineThreshold := r.inlineThreshold
	r.inlineThreshold = 0
	tok, err := r.readDefinedValueToken(header)
	r.inlineThreshold = inlineThreshold
	if err != nil {
		return Token{}, err
	}
	if r.pixelSequenceOffsetTablePending {
		r.pixelSequenceOffsetTablePending = false
		return tok, nil
	}
	r.fragmentCount++
	return tok, nil
}

func (r *Reader) updateSeqDelimiters() (Token, bool, error) {
	if !r.delimiterCheckPending {
		return Token{}, false, nil
	}
	if len(r.seqDelimiters) == 0 {
		r.delimiterCheckPending = false
		return Token{}, false, nil
	}

	last := r.seqDelimiters[len(r.seqDelimiters)-1]
	if last.length.IsUndefined() {
		if tok, ok, err := r.closeAtContainerEnd(); ok || err != nil {
			return tok, true, err
		}
		r.delimiterCheckPending = false
		return Token{}, false, nil
	}

	endOffset := last.baseOffset + uint64(last.length)
	currentOffset := r.positionU64()
	switch {
	case currentOffset == endOffset:
		r.seqDelimiters = r.seqDelimiters[:len(r.seqDelimiters)-1]
		switch last.typ {
		case seqTokenTypeSequence:
			return Token{Kind: TokenEndSequence}, true, nil
		case seqTokenTypePixelSequence:
			r.pixelSequenceOffsetTablePending = false
			return Token{Kind: TokenEndSequence}, true, nil
		case seqTokenTypeItem:
			return Token{Kind: TokenEndItem}, true, nil
		default:
			return Token{}, true, &ParseError{
				Op:     OpReadValue,
				Offset: int64(currentOffset),
				Err:    fmt.Errorf("dicom: unknown sequence token type %d", last.typ),
			}
		}
	case currentOffset > endOffset:
		return Token{}, true, &ParseError{
			Op:     OpReadValue,
			Offset: int64(currentOffset),
			Length: last.length,
			Err:    fmt.Errorf("dicom: read past %s boundary: expected end at offset %d, got %d", last.typ, endOffset, currentOffset),
		}
	default:
		r.delimiterCheckPending = false
		return Token{}, false, nil
	}
}

func (r *Reader) validateItemStart(header core.ElementHeader) error {
	if len(r.seqDelimiters) == 0 {
		return &ParseError{
			Op:     OpReadTag,
			Offset: r.Position(),
			Tag:    header.Tag,
			VR:     header.VR,
			Length: header.Length,
			Err:    fmt.Errorf("%w: %s", ErrUnexpectedSequenceControlTag, header.Tag),
		}
	}
	parent := r.seqDelimiters[len(r.seqDelimiters)-1]
	if parent.typ != seqTokenTypeSequence && parent.typ != seqTokenTypePixelSequence {
		return &ParseError{
			Op:     OpReadTag,
			Offset: r.Position(),
			Tag:    header.Tag,
			VR:     header.VR,
			Length: header.Length,
			Err:    fmt.Errorf("%w: %s", ErrUnexpectedSequenceControlTag, header.Tag),
		}
	}
	return nil
}

// pushContainer opens a sequence or item after checking its length against
// the enclosing defined-length container. skip reports an item header with no
// body left, which RecoverContainerOverruns does not record as an item.
func (r *Reader) pushContainer(typ seqTokenType, header core.ElementHeader, forceImplicitLittleEndian bool) (bool, error) {
	length, skip, err := r.fitChildLength(typ, header)
	if err != nil || skip {
		return skip, err
	}
	if typ == seqTokenTypeItem && len(r.seqDelimiters) > 0 {
		r.seqDelimiters[len(r.seqDelimiters)-1].items++
	}
	if err := r.pushSequenceTokenWithImplicitVRLittleEndian(typ, length, forceImplicitLittleEndian); err != nil {
		return false, err
	}
	if typ == seqTokenTypeSequence {
		r.seqDelimiters[len(r.seqDelimiters)-1].tag = header.Tag
	}
	return false, nil
}

// pushPixelSequence opens encapsulated Pixel Data; its fragments are checked
// against any enclosing container as values.
func (r *Reader) pushPixelSequence(header core.ElementHeader) error {
	if err := r.pushSequenceToken(seqTokenTypePixelSequence, header.Length); err != nil {
		return err
	}
	r.seqDelimiters[len(r.seqDelimiters)-1].tag = header.Tag
	return nil
}

func (r *Reader) pushSequenceToken(typ seqTokenType, length core.Length) error {
	return r.pushSequenceTokenWithImplicitVRLittleEndian(typ, length, false)
}

func (r *Reader) pushSequenceTokenWithImplicitVRLittleEndian(typ seqTokenType, length core.Length, force bool) error {
	if err := r.checkSequenceDepthLimit(length); err != nil {
		return err
	}
	implicitVRLittleEndian := r.implicitVRLittleEndianActive()
	if force {
		implicitVRLittleEndian = true
	}
	var privateScope *dictionary.PrivateReservations
	if typ == seqTokenTypeItem && r.privateRoot != nil {
		var err error
		privateScope, err = dictionary.NewPrivateReservations(r.maxPrivateCreators)
		if err != nil {
			return err
		}
	}
	index := len(r.seqDelimiters)
	definedIndex, itemIndex := -1, -1
	if index > 0 {
		parent := &r.seqDelimiters[index-1]
		definedIndex, itemIndex = parent.definedIndex, parent.itemIndex
	}
	if !length.IsUndefined() {
		definedIndex = index
	}
	if typ == seqTokenTypeItem {
		itemIndex = index
	}
	r.seqDelimiters = append(r.seqDelimiters, seqToken{
		privateScope:           privateScope,
		typ:                    typ,
		length:                 length,
		baseOffset:             r.positionU64(),
		implicitVRLittleEndian: implicitVRLittleEndian,
		explicitBigEndian:      !implicitVRLittleEndian && r.explicitBigEndianActive(),
		definedIndex:           definedIndex,
		itemIndex:              itemIndex,
	})
	return nil
}

// innermostItem returns the innermost open item frame, or nil.
func (r *Reader) innermostItem() *seqToken {
	n := len(r.seqDelimiters)
	if n == 0 {
		return nil
	}
	if i := r.seqDelimiters[n-1].itemIndex; i >= 0 {
		return &r.seqDelimiters[i]
	}
	return nil
}

// itemOffset returns the offset of the innermost enclosing Item header, found
// through the cached item indexes rather than a scan of the stack.
func (r *Reader) itemOffset() (int64, bool) {
	n := len(r.seqDelimiters)
	if n == 0 {
		return 0, false
	}
	for i := r.seqDelimiters[n-1].itemIndex; i >= 0; {
		frame := &r.seqDelimiters[i]
		if frame.baseOffset >= 8 {
			return int64(frame.baseOffset - 8), true
		}
		if i == 0 {
			break
		}
		i = r.seqDelimiters[i-1].itemIndex
	}
	return 0, false
}

func (r *Reader) implicitVRLittleEndianActive() bool {
	if len(r.seqDelimiters) == 0 {
		return false
	}
	return r.seqDelimiters[len(r.seqDelimiters)-1].implicitVRLittleEndian
}

func (r *Reader) explicitBigEndianActive() bool {
	if len(r.seqDelimiters) == 0 {
		return false
	}
	return r.seqDelimiters[len(r.seqDelimiters)-1].explicitBigEndian
}

func (r *Reader) currentDecoder() dicomenc.BasicDecoder {
	if r.implicitVRLittleEndianActive() {
		return dicomenc.NewBasicDecoder(binary.LittleEndian)
	}
	if r.explicitBigEndianActive() {
		return dicomenc.NewBasicDecoder(binary.BigEndian)
	}
	return r.dec
}

func (r *Reader) popUndefinedLengthSequenceToken(header core.ElementHeader, want seqTokenType, errType error) error {
	if len(r.seqDelimiters) == 0 {
		return &ParseError{
			Op:     OpReadTag,
			Offset: r.Position(),
			Tag:    header.Tag,
			VR:     header.VR,
			Length: header.Length,
			Err:    errType,
		}
	}
	last := r.seqDelimiters[len(r.seqDelimiters)-1]
	if last.typ != want || last.length.IsDefined() {
		return &ParseError{
			Op:     OpReadTag,
			Offset: r.Position(),
			Tag:    header.Tag,
			VR:     header.VR,
			Length: header.Length,
			Err:    errType,
		}
	}
	r.seqDelimiters = r.seqDelimiters[:len(r.seqDelimiters)-1]
	return nil
}

func (r *Reader) positionU64() uint64 {
	pos := r.Position()
	if pos < 0 {
		return 0
	}
	return uint64(pos)
}

func validateDelimiterLength(header core.ElementHeader, offset int64) error {
	if header.Length == 0 {
		return nil
	}
	return &ParseError{
		Op:     OpReadLength,
		Offset: offset,
		Tag:    header.Tag,
		VR:     header.VR,
		Length: header.Length,
		Err:    fmt.Errorf("%w for %s", ErrUnexpectedDelimiterLength, header.Tag),
	}
}

func normalizeReadError(start, end int64, err error) error {
	if err == nil {
		return nil
	}
	if end > start && errors.Is(err, io.EOF) && !errors.Is(err, io.ErrUnexpectedEOF) {
		return io.ErrUnexpectedEOF
	}
	return err
}

// EOF is normal only before the first byte of a new element's tag. Once a
// header field or declared value is required, even a zero-byte EOF is truncation.
func requiredReadError(err error) error {
	if errors.Is(err, io.EOF) {
		return io.ErrUnexpectedEOF
	}
	return err
}

// recoverDelimiter applies RecoverDelimiterAnomalies to a control element.
// It clears a nonzero delimiter length in place and reports whether the
// element was skipped entirely.
func (r *Reader) recoverDelimiter(header *core.ElementHeader) (bool, error) {
	offset := r.Position() - 8
	var top *seqToken
	if n := len(r.seqDelimiters); n > 0 {
		top = &r.seqDelimiters[n-1]
	}
	if top != nil && top.typ == seqTokenTypePixelSequence {
		return false, nil
	}
	switch {
	case header.Tag.IsItem() && top != nil && top.typ == seqTokenTypeItem && !top.length.IsUndefined() &&
		r.positionU64() == top.baseOffset+uint64(top.length):
		// An item header nested directly in an item and ending exactly at
		// that item's defined end carries no content: skip it.
		if err := r.countDelimiterRecovery(offset); err != nil {
			return false, err
		}
		declared := int64(header.Length)
		if header.Length.IsUndefined() {
			declared = -1
		}
		r.recordBoundary(BoundaryRecovery{Offset: offset, DeclaredLength: declared, AvailableLength: 0, Reason: BoundaryOrphanDelimiter})
		r.delimiterCheckPending = true
		return true, nil
	case header.Tag.Group == 0xfffe && header.Tag.Element == 0x0000:
		// Explicit "UL" + 16-bit length, or an Implicit 32-bit length.
		raw := uint32(header.Length)
		n := raw
		if byte(raw) == 0x55 && byte(raw>>8) == 0x4c { // "UL"
			n = raw >> 16
		}
		if n != 4 || top == nil {
			return false, nil
		}
		if err := r.countDelimiterRecovery(offset); err != nil {
			return false, err
		}
		if _, err := io.CopyN(io.Discard, r.counter, 4); err != nil {
			return false, &ParseError{Op: OpReadValue, Offset: r.Position(), Tag: header.Tag, Err: requiredReadError(err)}
		}
		r.recordBoundary(BoundaryRecovery{Offset: offset, DeclaredLength: 4, AvailableLength: 4, Reason: BoundaryItemGroupLength})
		r.delimiterCheckPending = true
		return true, nil
	case header.Tag.IsItemDelimitationItem(), header.Tag.IsSequenceDelimitationItem():
		want := seqTokenTypeItem
		if header.Tag.IsSequenceDelimitationItem() {
			want = seqTokenTypeSequence
		}
		if top == nil || top.typ != want || !top.length.IsUndefined() {
			if err := r.countDelimiterRecovery(offset); err != nil {
				return false, err
			}
			declared := int64(header.Length)
			if header.Length.IsUndefined() {
				declared = -1
			}
			r.recordBoundary(BoundaryRecovery{Offset: offset, DeclaredLength: declared, AvailableLength: 0, Reason: BoundaryOrphanDelimiter})
			r.delimiterCheckPending = true
			return true, nil
		}
		if header.Length != 0 {
			if err := r.countDelimiterRecovery(offset); err != nil {
				return false, err
			}
			declared := int64(header.Length)
			if header.Length.IsUndefined() {
				declared = -1
			}
			r.recordBoundary(BoundaryRecovery{Offset: offset, DeclaredLength: declared, AvailableLength: 0, Reason: BoundaryDelimiterLengthIgnored})
			header.Length = 0
		}
	}
	return false, nil
}

// replaceWrongDelimiter implements SequenceRecoveryOptions.ReplaceWrongDelimiter.
func (r *Reader) replaceWrongDelimiter(header core.ElementHeader) (Token, bool) {
	n := len(r.seqDelimiters)
	if n == 0 || header.Length != 0 {
		return Token{}, false
	}
	top := r.seqDelimiters[n-1]
	offset := r.Position() - 8
	switch {
	case header.Tag.IsSequenceDelimitationItem() && top.typ == seqTokenTypeItem && top.length.IsUndefined():
		// DCMTK dcmReplaceWrongDelimitationItem: read as the Item
		// Delimitation Item expected here; the sequence stays open.
		r.recordBoundary(BoundaryRecovery{Offset: offset, DeclaredLength: 0, AvailableLength: 0, Reason: BoundaryWrongDelimiterReplaced})
		r.seqDelimiters = r.seqDelimiters[:n-1]
		r.delimiterCheckPending = true
		return Token{Kind: TokenEndItem, Header: header}, true
	case header.Tag.IsItemDelimitationItem() && top.typ == seqTokenTypeSequence && top.length.IsUndefined():
		r.recordBoundary(BoundaryRecovery{Offset: offset, DeclaredLength: 0, AvailableLength: 0, Reason: BoundaryWrongDelimiterReplaced})
		r.seqDelimiters = r.seqDelimiters[:n-1]
		r.delimiterCheckPending = true
		return Token{Kind: TokenEndSequence, Header: header}, true
	}
	return Token{}, false
}
