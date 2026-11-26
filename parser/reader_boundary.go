package parser

import (
	"errors"
	"fmt"
	"strings"

	"github.com/ThalesMMS/dicom-go/core"
)

// ErrContainerOverrun reports an item, sequence or value whose declared length
// runs past the end of its enclosing defined-length sequence or item, or an
// undefined-length child left open at that end. The reader stops before
// consuming any byte outside the container.
var ErrContainerOverrun = errors.New("dicom: length exceeds the enclosing sequence or item")

// BoundaryReason names a recovered container anomaly.
type BoundaryReason string

const (
	// BoundaryItemOverrun: a defined-length item was declared longer than the
	// bytes left in its container; it was read to the container end.
	BoundaryItemOverrun BoundaryReason = "item-overruns-container"
	// BoundarySequenceOverrun: the same for a defined-length sequence.
	BoundarySequenceOverrun BoundaryReason = "sequence-overruns-container"
	// BoundaryItemHeaderAtEnd: an item header ended exactly at the container
	// end, with no body; no item was recorded.
	BoundaryItemHeaderAtEnd BoundaryReason = "item-header-at-container-end"
	// BoundaryMissingDelimiter: an undefined-length item or sequence was still
	// open when its container ended; it was closed there.
	BoundaryMissingDelimiter BoundaryReason = "missing-delimiter-at-container-end"
	// BoundaryImplicitVRElement: an element with invalid VR bytes in an
	// Explicit VR data set was read as Implicit VR (RecoverImplicitVRElements).
	BoundaryImplicitVRElement BoundaryReason = "implicit-vr-element"
	// Delimiter repairs made by RecoverDelimiterAnomalies.
	BoundarySwappedItemTag         BoundaryReason = "swapped-item-tag"
	BoundaryDelimiterLengthIgnored BoundaryReason = "delimiter-length-ignored"
	BoundaryOrphanDelimiter        BoundaryReason = "orphan-delimiter-skipped"
	BoundaryItemGroupLength        BoundaryReason = "item-group-length-skipped"
	// VR repairs made by VRRecoveryOptions.
	BoundaryOXPixelData           BoundaryReason = "ox-pixel-data-read-as-ow"
	BoundaryDictionaryVR          BoundaryReason = "dictionary-vr-preferred"
	BoundaryDictionaryLengthField BoundaryReason = "dictionary-length-field-size"
	BoundaryPrivateULLength       BoundaryReason = "private-ul-length-6-read-as-4"
	BoundaryShortLengthUN         BoundaryReason = "short-length-un"
	// BoundaryGETextLength: a GE implicit-VR text value declared as 13 bytes
	// was read as 10 after bounded, unambiguous header checks.
	BoundaryGETextLength BoundaryReason = "ge-text-length-13-read-as-10"
	// Sequence repairs made by SequenceRecoveryOptions.
	BoundaryOBOWAsSequence         BoundaryReason = "undefined-length-ob-ow-read-as-sq"
	BoundaryVOILUTAsSequence       BoundaryReason = "voi-lut-sequence-read-as-sq"
	BoundaryWrongDelimiterReplaced BoundaryReason = "wrong-delimiter-replaced"
	BoundaryPrivateSequenceBytes   BoundaryReason = "private-sequence-kept-as-bytes"
	// BoundaryImplicitVRFileMeta: group 0002 was encoded in Implicit VR
	// Little Endian and read with the dictionary (AllowImplicitVRFileMeta).
	BoundaryImplicitVRFileMeta BoundaryReason = "implicit-vr-file-meta"
	// BoundaryZlibDeflate: a Deflated data set carried a zlib (RFC 1950)
	// wrapper instead of raw deflate (object.ReadFileOptions.AllowZlibDeflate).
	BoundaryZlibDeflate BoundaryReason = "zlib-deflate-wrapper"
)

// DefaultMaxBoundaryRecoveries caps BoundaryRecoveries when the option is 0.
const DefaultMaxBoundaryRecoveries = 128

// BoundaryRecovery describes one anomaly RecoverContainerOverruns repaired.
// Path is structural, for example "(0004,1220)[51]"; Offset is the absolute
// offset of the value, or of the position where the container ended;
// DeclaredLength is -1 for undefined lengths; AvailableLength is what the
// container had left. No values are recorded.
type BoundaryRecovery struct {
	Path            string
	Offset          int64
	DeclaredLength  int64
	AvailableLength int64
	Reason          BoundaryReason
	// DeclaredVR and UsedVR are set by VR repairs (VRRecoveryOptions): the
	// VR bytes written in the file and the VR the element was read with.
	DeclaredVR core.VR
	UsedVR     core.VR
}

// BoundaryRecoveries returns the anomalies repaired so far and whether more
// were repaired than recorded. Call it after the read completes: it returns
// the reader's final list, not a snapshot taken while parsing.
func (r *Reader) BoundaryRecoveries() ([]BoundaryRecovery, bool) {
	return append([]BoundaryRecovery(nil), r.boundaryRecoveries...), r.boundaryRecoveriesTruncated
}

func (r *Reader) recordBoundary(recovery BoundaryRecovery) {
	r.recordBoundaryAt(recovery, nil)
}

// recordBoundaryAt records recovery with the path returned by path, or the
// current structural path when path is nil and recovery.Path is empty. The
// path costs O(depth) to render, so it is built only when the recovery is
// actually kept.
func (r *Reader) recordBoundaryAt(recovery BoundaryRecovery, path func() string) {
	limit := r.maxBoundaryRecoveries
	if limit <= 0 {
		limit = DefaultMaxBoundaryRecoveries
	}
	if len(r.boundaryRecoveries) >= limit {
		r.boundaryRecoveriesTruncated = true
		return
	}
	switch {
	case path != nil:
		recovery.Path = path()
	case recovery.Path == "":
		recovery.Path = r.structuralPath()
	}
	r.boundaryRecoveries = append(r.boundaryRecoveries, recovery)
}

// childPath renders the structural path of a child element of the innermost
// open item or data set, e.g. "(0008,1115)[0].(0028,3010)".
func (r *Reader) childPath(tag core.Tag) string {
	path := r.structuralPath()
	if path != "" {
		path += "."
	}
	return path + tag.String()
}

// structuralPath renders the open sequences and items, e.g. "(0008,1115)[0]".
func (r *Reader) structuralPath() string {
	var b strings.Builder
	for i, token := range r.seqDelimiters {
		if token.typ != seqTokenTypeSequence && token.typ != seqTokenTypePixelSequence {
			continue
		}
		if b.Len() > 0 {
			b.WriteByte('.')
		}
		b.WriteString(token.tag.String())
		if i+1 < len(r.seqDelimiters) && r.seqDelimiters[i+1].typ == seqTokenTypeItem {
			fmt.Fprintf(&b, "[%d]", token.items-1)
		}
	}
	return b.String()
}

// definedContainer returns the index and end offset of the innermost open
// sequence or item with a defined length. It reads the index cached on the top
// frame, so it costs O(1) at any nesting depth.
func (r *Reader) definedContainer() (int, uint64, bool) {
	n := len(r.seqDelimiters)
	if n == 0 {
		return 0, 0, false
	}
	i := r.seqDelimiters[n-1].definedIndex
	if i < 0 {
		return 0, 0, false
	}
	token := &r.seqDelimiters[i]
	return i, token.baseOffset + uint64(token.length), true
}

// remainingInContainer is the number of bytes left before the innermost
// defined container ends; ok is false when no container bounds the value.
func (r *Reader) remainingInContainer() (uint64, bool) {
	_, end, ok := r.definedContainer()
	if !ok {
		return 0, false
	}
	position := r.positionU64()
	if position >= end {
		return 0, true
	}
	return end - position, true
}

func (r *Reader) overrunError(header core.ElementHeader, available uint64) error {
	return &ParseError{
		Op:     OpReadValue,
		Offset: r.Position(),
		Tag:    header.Tag,
		VR:     header.VR,
		Length: header.Length,
		Err:    fmt.Errorf("%w: %d bytes declared, %d left", ErrContainerOverrun, uint64(header.Length), available),
	}
}

// checkValueFits refuses a primitive value or fragment that would cross its
// container end, in every mode: a truncated value is never accepted.
func (r *Reader) checkValueFits(header core.ElementHeader) error {
	if header.Length.IsUndefined() {
		return nil
	}
	if available, ok := r.remainingInContainer(); ok && uint64(header.Length) > available {
		return r.overrunError(header, available)
	}
	return nil
}

// fitChildLength checks a defined-length sequence or item against its
// container. Strict readers fail; RecoverContainerOverruns clamps the length
// to the bytes left. skip reports an item header with nothing left, which is
// not an item.
func (r *Reader) fitChildLength(typ seqTokenType, header core.ElementHeader) (length core.Length, skip bool, err error) {
	length = header.Length
	if length.IsUndefined() {
		return length, false, nil
	}
	available, ok := r.remainingInContainer()
	if !ok || uint64(length) <= available {
		return length, false, nil
	}
	if !r.recoverContainerOverruns {
		return 0, false, r.overrunError(header, available)
	}
	// The path names the child being opened: the next item index of the
	// enclosing sequence, or the sequence's own tag.
	recovery := BoundaryRecovery{Offset: r.Position(), DeclaredLength: int64(uint64(length)), AvailableLength: int64(available), Reason: BoundaryItemOverrun}
	path := func() string {
		if typ == seqTokenTypeSequence {
			return r.childPath(header.Tag)
		}
		path := r.structuralPath()
		if len(r.seqDelimiters) > 0 {
			path += fmt.Sprintf("[%d]", r.seqDelimiters[len(r.seqDelimiters)-1].items)
		}
		return path
	}
	if typ == seqTokenTypeSequence {
		recovery.Reason = BoundarySequenceOverrun
	}
	if available == 0 {
		if typ != seqTokenTypeItem {
			return 0, false, r.overrunError(header, available)
		}
		recovery.Reason = BoundaryItemHeaderAtEnd
		r.recordBoundaryAt(recovery, path)
		return 0, true, nil
	}
	r.recordBoundaryAt(recovery, path)
	return core.Length(available), false, nil
}

// closeAtContainerEnd handles an undefined-length sequence or item that is
// still open when an enclosing defined-length container ends. Strict readers
// fail; RecoverContainerOverruns closes it there.
func (r *Reader) closeAtContainerEnd() (Token, bool, error) {
	index, end, ok := r.definedContainer()
	top := len(r.seqDelimiters) - 1
	if !ok || index == top {
		return Token{}, false, nil
	}
	position := r.positionU64()
	if position < end {
		return Token{}, false, nil
	}
	last := r.seqDelimiters[top]
	if position > end || !r.recoverContainerOverruns {
		return Token{}, true, &ParseError{
			Op:     OpReadValue,
			Offset: int64(position),
			Err:    fmt.Errorf("%w: undefined-length %s open at the end of its container (offset %d)", ErrContainerOverrun, last.typ, end),
		}
	}
	r.recordBoundary(BoundaryRecovery{Offset: int64(position), DeclaredLength: -1, Reason: BoundaryMissingDelimiter})
	r.seqDelimiters = r.seqDelimiters[:top]
	switch last.typ {
	case seqTokenTypeItem:
		return Token{Kind: TokenEndItem}, true, nil
	case seqTokenTypePixelSequence:
		r.pixelSequenceOffsetTablePending = false
	}
	return Token{Kind: TokenEndSequence}, true, nil
}
