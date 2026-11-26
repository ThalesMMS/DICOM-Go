package parser

import (
	"bytes"
	"encoding/binary"
	"errors"
	"fmt"
	"io"

	"github.com/ThalesMMS/dicom-go/core"
	"github.com/ThalesMMS/dicom-go/dictionary"
	dicomenc "github.com/ThalesMMS/dicom-go/encoding"
)

func (r *Reader) readHeader() (core.ElementHeader, error) {
	headerOffset := r.Position()
	header, err := r.readHeaderFenced()
	if err != nil || r.seqRecovery == (SequenceRecoveryOptions{}) || header.Tag.IsSequenceDelimiting() {
		return header, err
	}
	return r.applySequenceRecovery(header, headerOffset)
}

func (r *Reader) readHeaderFenced() (result core.ElementHeader, err error) {
	r.lastReservedNonZero = false
	headerOffset := r.Position()
	// A header never reads past its enclosing defined-length container.
	if _, end, ok := r.definedContainer(); ok {
		r.counter.fence, r.counter.fenceHit = int64(end), false
		defer func() { r.counter.fence = 0 }()
	}
	r.beginHeaderInput()
	defer func() {
		if restoreErr := r.endHeaderInput(); restoreErr != nil {
			result = core.ElementHeader{}
			err = errors.Join(err, fmt.Errorf("restore header source position: %w", restoreErr))
		}
	}()
	header, err := r.readHeaderBytes(headerOffset)
	if err != nil && r.counter.fenceHit {
		return core.ElementHeader{}, &ParseError{
			Op:     OpReadTag,
			Offset: headerOffset,
			Err:    fmt.Errorf("%w: element header crosses the container end", ErrContainerOverrun),
		}
	}
	return header, err
}

func (r *Reader) readHeaderBytes(headerOffset int64) (core.ElementHeader, error) {
	decoder := r.currentDecoder()
	tag, err := decoder.ReadTag(r.headerReader())
	if err != nil {
		err = normalizeReadError(headerOffset, r.Position(), err)
		if r.Position() == headerOffset && errors.Is(err, io.EOF) {
			return core.ElementHeader{}, io.EOF
		}
		return core.ElementHeader{}, &ParseError{
			Op:     OpReadTag,
			Offset: headerOffset,
			Err:    err,
		}
	}
	if r.recoverDelimiterAnomalies && tag.Group == 0xfeff && len(r.seqDelimiters) > 0 {
		if swapped, ok := swappedControlTag(tag); ok {
			if err := r.countDelimiterRecovery(headerOffset); err != nil {
				return core.ElementHeader{}, err
			}
			r.recordBoundary(BoundaryRecovery{Offset: headerOffset, DeclaredLength: -1, AvailableLength: -1, Reason: BoundarySwappedItemTag})
			tag = swapped
			decoder.Order = binary.BigEndian
			r.swappedItemPending = tag.IsItem()
		}
	}
	if tag.IsSequenceDelimiting() {
		length, err := r.readLongLengthWithDecoder(tag, core.VRUN, decoder)
		if err != nil {
			return core.ElementHeader{}, err
		}
		return core.ElementHeader{Tag: tag, VR: core.VRUN, Length: length, LengthSet: true}, nil
	}
	var header core.ElementHeader
	if r.syntax.ExplicitVR && !r.implicitVRLittleEndianActive() {
		header, err = r.readExplicitHeader(tag)
	} else {
		header, err = r.readImplicitHeader(tag)
	}
	// A malformed SQ/UN creator may never enter the primitive-value path.
	// Record its invalid reservation before entering any child item scope.
	if err == nil && header.VR != core.VRLO {
		err = r.capturePrivateReservation(core.Element{Header: header})
	}
	return header, err
}

func (r *Reader) readExplicitHeader(tag core.Tag) (core.ElementHeader, error) {
	var vr core.VR
	var length core.Length

	var vrBytes [2]byte
	vrOffset := r.Position()
	if _, err := io.ReadFull(r.headerReader(), vrBytes[:]); err != nil {
		return core.ElementHeader{}, &ParseError{
			Op:     OpReadVR,
			Offset: vrOffset,
			Tag:    tag,
			Err:    requiredReadError(err),
		}
	}
	parsedVR, err := core.ParseVR(string(vrBytes[:]))
	if err != nil && r.vrRecovery.MapOXToOW && tag == core.TagPixelData && vrBytes == [2]byte{'O', 'X'} {
		parsedVR, err = core.VROW, nil
		r.recordVRRepair(tag, vrOffset-4, core.VR("OX"), core.VROW, BoundaryOXPixelData)
	}
	if err != nil && r.recoverImplicitVRElements {
		if header, ok, rerr := r.readImplicitAfterInvalidVR(tag, vrBytes, vrOffset); rerr != nil || ok {
			return header, rerr
		}
	}
	if err != nil {
		return core.ElementHeader{}, &ParseError{
			Op:     OpReadVR,
			Offset: vrOffset,
			Tag:    tag,
			Err:    err,
		}
	}
	vr = parsedVR
	lengthVR := vr
	if (r.vrRecovery.PreferDictionaryVR || r.vrRecovery.PreferDictionaryLengthFieldSize) && !tag.IsPrivate() {
		if dictVR, ok := r.singleDictionaryVR(tag); ok && dictVR != vr {
			if r.vrRecovery.PreferDictionaryLengthFieldSize && dictVR.UsesLongExplicitLength() != vr.UsesLongExplicitLength() {
				lengthVR = dictVR
				r.recordVRRepair(tag, vrOffset-4, vr, dictVR, BoundaryDictionaryLengthField)
			}
			if r.vrRecovery.PreferDictionaryVR {
				r.recordVRRepair(tag, vrOffset-4, vr, dictVR, BoundaryDictionaryVR)
				vr = dictVR
			}
		}
	}
	if lengthVR.UsesLongExplicitLength() {
		// In lenient mode both reserved bytes and the four-byte length must be
		// consumed. Strict validation stops before the length on bad reserved
		// bytes, so that mode deliberately keeps the separate reads.
		if !r.strictReservedBytes && r.headerInput.active {
			r.headerInput.fill(6)
		}
		var reserved [2]byte
		reservedOffset := r.Position()
		if _, err := io.ReadFull(r.headerReader(), reserved[:]); err != nil {
			return core.ElementHeader{}, &ParseError{
				Op:     OpReadReserved,
				Offset: reservedOffset,
				Tag:    tag,
				VR:     vr,
				Err:    requiredReadError(err),
			}
		}
		if r.strictReservedBytes && reserved != [2]byte{} {
			return core.ElementHeader{}, &ParseError{
				Op:     OpValidateReserved,
				Offset: reservedOffset,
				Tag:    tag,
				VR:     vr,
				Err:    fmt.Errorf("%w: got %02X%02X", ErrNonZeroReservedBytes, reserved[0], reserved[1]),
			}
		}
		if r.vrRecovery.ShortLengthUN && vr == core.VRUN {
			if header, ok, err := r.readShortLengthUN(tag, vrOffset, reserved); ok || err != nil {
				return header, err
			}
		}
		if reserved != [2]byte{} {
			r.lastReservedNonZero = true
			r.lastReservedOffset = reservedOffset
		}
		length, err = r.readLongLength(tag, vr)
		if err != nil {
			return core.ElementHeader{}, err
		}
	} else {
		lengthOffset := r.Position()
		u16, err := r.currentDecoder().ReadU16(r.headerReader())
		if err != nil {
			return core.ElementHeader{}, &ParseError{
				Op:     OpReadLength,
				Offset: lengthOffset,
				Tag:    tag,
				VR:     vr,
				Err:    requiredReadError(err),
			}
		}
		length = core.Length(u16)
		if length == 6 && vr == core.VRUL && tag.IsPrivate() && r.vrRecovery.CorrectPrivateULLength {
			r.recordVRRepair(tag, vrOffset-4, vr, vr, BoundaryPrivateULLength)
			length = 4
		}
	}
	return core.ElementHeader{Tag: tag, VR: vr, Length: length, LengthSet: true}, nil
}

func (r *Reader) readImplicitHeader(tag core.Tag) (core.ElementHeader, error) {
	vr := r.lookupImplicitVR(tag)
	length, err := r.readLongLength(tag, vr)
	if err != nil {
		return core.ElementHeader{}, err
	}
	if length == 13 && r.vrRecovery.CorrectGETextLength {
		corrected, err := r.correctGETextLength(tag, vr)
		if err != nil {
			return core.ElementHeader{}, err
		}
		if corrected {
			length = 10
		}
	}
	return core.ElementHeader{Tag: tag, VR: implicitLengthVR(tag, vr, length), Length: length, LengthSet: true}, nil
}

func (r *Reader) readLongLength(tag core.Tag, vr core.VR) (core.Length, error) {
	return r.readLongLengthWithDecoder(tag, vr, r.currentDecoder())
}

func (r *Reader) readLongLengthWithDecoder(tag core.Tag, vr core.VR, decoder dicomenc.BasicDecoder) (core.Length, error) {
	lengthOffset := r.Position()
	u32, err := decoder.ReadU32(r.headerReader())
	if err != nil {
		return 0, &ParseError{
			Op:     OpReadLength,
			Offset: lengthOffset,
			Tag:    tag,
			VR:     vr,
			Err:    requiredReadError(err),
		}
	}
	return core.Length(u32), nil
}

// lookupImplicitVR resolves a tag's VR for implicit-VR transfer syntaxes in
// this order:
//  1. Hard-coded special cases for Pixel Data and Overlay Data, both of which
//     are treated as OW for parser safety.
//  2. Private Creators (gggg,0010-00FF) are LO, the VR PS3.5 7.8.1 fixes for
//     them, with or without a private dictionary.
//  3. Dictionary lookup via dictionary.LookupVR.
//  4. Fallback to UN when no dictionary entry is available.
//
// Private data elements are typically absent from the standard dictionary,
// so they usually resolve to UN unless callers provide a custom dictionary. A defined-
// length UN value is preserved as raw bytes. An undefined-length UN value is
// parsed through the sequence item/delimiter grammar with a
// scoped Implicit VR Little Endian override while preserving VR UN in the
// in-memory header. An opt-in PrivateDataDictionary resolves creator-relative
// definitions against this dataset/item's reservations, without inheritance.
func (r *Reader) lookupImplicitVR(tag core.Tag) core.VR {
	if tag == core.TagPixelData {
		return core.VROW
	}
	if tag.Element == 0x3000 && tag.Group>>8 == 0x60 && tag.Group&1 == 0 {
		return core.VROW
	}
	if r.privateRoot != nil && tag.IsPrivate() || dictionary.IsPrivateCreatorTag(tag) {
		return r.lookupPrivateVR(tag)
	}
	return dictionary.LookupVR(r.dict, tag)
}

// implicitLengthVR keeps an undefined-length Private Creator UN, so that
// malformed input still goes through the sequence grammar rather than
// failing as an undefined-length LO.
func implicitLengthVR(tag core.Tag, vr core.VR, length core.Length) core.VR {
	if length.IsUndefined() && vr == core.VRLO && dictionary.IsPrivateCreatorTag(tag) {
		return core.VRUN
	}
	return vr
}

// readImplicitAfterInvalidVR reinterprets the two VR bytes and the next two
// bytes as an Implicit VR length. ok is false (and nothing is reported) when
// the length is implausible, so the caller keeps the strict error.
func (r *Reader) readImplicitAfterInvalidVR(tag core.Tag, vrBytes [2]byte, vrOffset int64) (core.ElementHeader, bool, error) {
	if tag == core.TagPixelData || !r.syntax.IsLittleEndian() {
		return core.ElementHeader{}, false, nil
	}
	var rest [2]byte
	if _, err := io.ReadFull(r.counter, rest[:]); err != nil {
		return core.ElementHeader{}, false, nil
	}
	length := core.Length(uint32(vrBytes[0]) | uint32(vrBytes[1])<<8 | uint32(rest[0])<<16 | uint32(rest[1])<<24)
	if !length.IsUndefined() {
		if remaining, bounded := r.remainingInContainer(); bounded && uint64(length) > remaining {
			return core.ElementHeader{}, false, nil
		}
		if r.sourceEnd > 0 && int64(length) > r.sourceEnd-r.Position() {
			return core.ElementHeader{}, false, nil
		}
	}
	limit := r.maxImplicitVRElements
	if limit <= 0 {
		limit = DefaultMaxImplicitVRElements
	}
	if r.implicitVRElements >= limit {
		return core.ElementHeader{}, false, &ParseError{Op: OpReadVR, Offset: vrOffset, Tag: tag, Err: ErrMaxImplicitVRElements}
	}
	r.implicitVRElements++
	vr := implicitLengthVR(tag, r.lookupImplicitVR(tag), length)
	declared := int64(length)
	if length.IsUndefined() {
		declared = -1
	}
	r.recordBoundaryAt(BoundaryRecovery{Offset: vrOffset - 4, DeclaredLength: declared, AvailableLength: declared, Reason: BoundaryImplicitVRElement},
		func() string { return r.childPath(tag) })
	return core.ElementHeader{Tag: tag, VR: vr, Length: length, LengthSet: true}, true, nil
}

// swappedControlTag maps a byte-swapped Item, Item Delimitation or Sequence
// Delimitation tag to the correct tag.
func swappedControlTag(tag core.Tag) (core.Tag, bool) {
	switch tag.Element {
	case 0x00e0:
		return core.NewTag(0xfffe, 0xe000), true
	case 0x0de0:
		return core.NewTag(0xfffe, 0xe00d), true
	case 0xdde0:
		return core.NewTag(0xfffe, 0xe0dd), true
	}
	return tag, false
}

func (r *Reader) countDelimiterRecovery(offset int64) error {
	limit := r.maxDelimiterRecoveries
	if limit <= 0 {
		limit = DefaultMaxDelimiterRecoveries
	}
	if r.delimiterRecoveries >= limit {
		return &ParseError{Op: OpReadTag, Offset: offset, Err: ErrMaxDelimiterRecoveries}
	}
	r.delimiterRecoveries++
	return nil
}

// shortLength decodes a 16-bit length in the active byte order.
func (r *Reader) shortLength(b [2]byte) uint16 {
	if !r.syntax.IsLittleEndian() || r.explicitBigEndianActive() {
		return binary.BigEndian.Uint16(b[:])
	}
	return binary.LittleEndian.Uint16(b[:])
}

// singleDictionaryVR returns a public element's dictionary VR when the
// dictionary names exactly one.
func (r *Reader) singleDictionaryVR(tag core.Tag) (core.VR, bool) {
	spec, ok := dictionary.LookupVRSpec(r.dict, tag)
	if !ok {
		return "", false
	}
	values := spec.Values()
	if len(values) != 1 || values[0] == core.VRUN {
		return "", false
	}
	return values[0], true
}

func (r *Reader) recordVRRepair(tag core.Tag, offset int64, declared, used core.VR, reason BoundaryReason) {
	r.recordBoundaryAt(BoundaryRecovery{Offset: offset, DeclaredLength: -1, AvailableLength: -1,
		Reason: reason, DeclaredVR: declared, UsedVR: used}, func() string { return r.childPath(tag) })
}

// readShortLengthUN recognizes UN written by GDCM 1.x with a 16-bit length
// in the reserved bytes: the reserved bytes are nonzero, or the 32-bit length
// cannot fit the source while the 16-bit one does. The four bytes read ahead
// are returned to the stream. ok is false when the element looks standard.
func (r *Reader) readShortLengthUN(tag core.Tag, vrOffset int64, reserved [2]byte) (core.ElementHeader, bool, error) {
	if reserved != [2]byte{} {
		// Nonzero reserved bytes are the 16-bit length itself.
		short := r.shortLength(reserved)
		if left, bounded := r.remainingInContainer(); bounded && uint64(short) > left {
			return core.ElementHeader{}, false, nil
		}
		if r.sourceEnd > 0 && int64(short) > r.sourceEnd-r.Position() {
			return core.ElementHeader{}, false, nil
		}
		r.recordVRRepair(tag, vrOffset-4, core.VRUN, core.VRUN, BoundaryShortLengthUN)
		return core.ElementHeader{Tag: tag, VR: core.VRUN, Length: core.Length(short), LengthSet: true}, true, nil
	}
	var ahead [4]byte
	if _, err := io.ReadFull(r.counter, ahead[:]); err != nil {
		return core.ElementHeader{}, true, &ParseError{Op: OpReadLength, Offset: r.Position(), Tag: tag, VR: core.VRUN, Err: requiredReadError(err)}
	}
	long := uint64(binary.LittleEndian.Uint32(ahead[:]))
	short := uint64(r.shortLength(reserved))
	if !r.syntax.IsLittleEndian() || r.explicitBigEndianActive() {
		long = uint64(binary.BigEndian.Uint32(ahead[:]))
	}
	remaining := int64(-1)
	if r.sourceEnd > 0 {
		remaining = r.sourceEnd - r.Position()
	}
	if left, bounded := r.remainingInContainer(); bounded && (remaining < 0 || int64(left) < remaining) {
		remaining = int64(left)
	}
	fitsShort := remaining >= 0 && int64(short) <= remaining+4
	implausibleLong := long != uint64(core.UndefinedLength) && remaining >= 0 && int64(long) > remaining
	if !implausibleLong || !fitsShort {
		// Standard element: consume the 32-bit length as usual.
		return core.ElementHeader{Tag: tag, VR: core.VRUN, Length: core.Length(long), LengthSet: true}, true, nil
	}
	r.counter.unread(ahead[:])
	r.recordVRRepair(tag, vrOffset-4, core.VRUN, core.VRUN, BoundaryShortLengthUN)
	return core.ElementHeader{Tag: tag, VR: core.VRUN, Length: core.Length(short), LengthSet: true}, true, nil
}

// applySequenceRecovery rewrites a header according to SequenceRecovery.
func (r *Reader) applySequenceRecovery(header core.ElementHeader, headerOffset int64) (core.ElementHeader, error) {
	// The path is rendered only when a repair is recorded: building it for
	// every header would cost O(depth) per element.
	tag := header.Tag
	record := func(reason BoundaryReason, used core.VR) {
		declared := int64(header.Length)
		if header.Length.IsUndefined() {
			declared = -1
		}
		r.recordBoundaryAt(BoundaryRecovery{Offset: headerOffset, DeclaredLength: declared, AvailableLength: -1,
			Reason: reason, DeclaredVR: header.VR, UsedVR: used}, func() string { return r.childPath(tag) })
	}
	switch {
	case r.seqRecovery.VOILUTSequenceOWAsSQ && header.Tag == core.NewTag(0x0028, 0x3010) && header.VR != core.VRSQ:
		record(BoundaryVOILUTAsSequence, core.VRSQ)
		header.VR = core.VRSQ
	case r.seqRecovery.UndefinedLengthOBOWAsSQ && header.Length.IsUndefined() && header.Tag != core.TagPixelData &&
		(header.VR == core.VROB || header.VR == core.VROW):
		item, err := r.nextIsItem()
		if err != nil {
			return header, err
		}
		if item {
			record(BoundaryOBOWAsSequence, core.VRSQ)
			header.VR = core.VRSQ
		}
	case r.seqRecovery.PrivateSequenceAsBytes && header.VR == core.VRSQ && header.Tag.IsPrivate() &&
		header.Length.IsDefined() && uint64(header.Length) <= maxPrivateSequenceCheckBytes:
		ok, err := r.privateSequenceParses(header)
		if err != nil {
			return header, err
		}
		if !ok {
			record(BoundaryPrivateSequenceBytes, core.VRUN)
			header.VR = core.VRUN
		}
	}
	return header, nil
}

// nextIsItem reports whether the next four bytes are an Item tag, leaving the
// stream unchanged.
func (r *Reader) nextIsItem() (bool, error) {
	var ahead [4]byte
	n, err := io.ReadFull(r.counter, ahead[:])
	r.counter.unread(ahead[:n])
	if err != nil {
		return false, nil
	}
	group, element := binary.LittleEndian.Uint16(ahead[:2]), binary.LittleEndian.Uint16(ahead[2:])
	if !r.syntax.IsLittleEndian() || r.explicitBigEndianActive() {
		group, element = binary.BigEndian.Uint16(ahead[:2]), binary.BigEndian.Uint16(ahead[2:])
	}
	return group == 0xfffe && element == 0xe000, nil
}

// privateSequenceParses buffers a defined-length private SQ value, parses it
// with a detached reader and returns the bytes to the stream either way.
func (r *Reader) privateSequenceParses(header core.ElementHeader) (bool, error) {
	valueOffset := r.Position()
	value := make([]byte, int(header.Length))
	if _, err := io.ReadFull(r.counter, value); err != nil {
		return false, &ParseError{Op: OpReadValue, Offset: valueOffset, Tag: header.Tag, VR: header.VR, Length: header.Length, Err: requiredReadError(err)}
	}
	defer r.counter.unread(value)
	sub := NewReader(bytes.NewReader(value), r.syntax, ReaderOptions{
		Dictionary: r.dict, BaseOffset: valueOffset, MaxSequenceDepth: r.maxSequenceDepth,
		MaxElements: r.maxElements, RecoverDelimiterAnomalies: r.recoverDelimiterAnomalies,
		RecoverImplicitVRElements: r.recoverImplicitVRElements, VRRecovery: r.vrRecovery,
	})
	if err := sub.pushSequenceToken(seqTokenTypeSequence, header.Length); err != nil {
		return false, nil
	}
	sub.delimiterCheckPending = true
	for {
		tok, err := sub.Next()
		if err != nil {
			return false, nil
		}
		if tok.Kind == TokenEndSequence && len(sub.seqDelimiters) == 0 {
			return sub.Position() == valueOffset+int64(header.Length), nil
		}
	}
}
