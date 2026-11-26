package parser

import (
	"bytes"
	"errors"
	"fmt"
	"io"

	"github.com/ThalesMMS/dicom-go/core"
	"github.com/ThalesMMS/dicom-go/transfer"
)

var ErrIncompletePixelData = errors.New("dicom: incomplete Pixel Data")
var ErrIncompletePixelDataOptions = errors.New("dicom: incompatible incomplete Pixel Data recovery options")

const MaxIncompletePixelDataBytes int64 = 128 << 20
const MaxIncompletePixelDataFragments = 100000

// IncompletePixelData records a terminal interrupted integer Pixel Data value.
// It contains structural counts only. Retained Items need not contain complete
// codestreams: a decoder must separately qualify every returned image frame.
type IncompletePixelData struct {
	Kind                                  string
	ValueOffset, FailureOffset            int64
	DeclaredBytes, AvailableBytes         int64
	RetainedFragments, DiscardedFragments int
	DiscardedBytes                        int64
	Cause                                 error
}

// IncompletePixelDataError preserves both the classification and original cause.
type IncompletePixelDataError struct{ Read IncompletePixelData }

func (e *IncompletePixelDataError) Error() string { return ErrIncompletePixelData.Error() }
func (e *IncompletePixelDataError) Unwrap() []error {
	if e == nil {
		return []error{ErrIncompletePixelData}
	}
	return []error{ErrIncompletePixelData, e.Read.Cause}
}
func (r *Reader) IncompletePixelData() (IncompletePixelData, bool) {
	if r == nil || r.incompletePixelData == nil {
		return IncompletePixelData{}, false
	}
	return *r.incompletePixelData, true
}

func incompleteReadOptions(o ReaderOptions, syntax transfer.Syntax) (ReaderOptions, error) {
	if !o.AllowIncompletePixelData {
		return o, nil
	}
	_, legacy := transfer.LegacySyntax(syntax.UID)
	if syntax.Deflated || legacy || o.RecoverContainerOverruns || o.RecoverImplicitVRElements || o.RecoverDelimiterAnomalies || o.SequenceRecovery != (SequenceRecoveryOptions{}) || o.VRRecovery != (VRRecoveryOptions{}) || o.SkipPixelData || o.DeferPixelData || o.FrameSink != nil || o.EncapsulatedSink != nil || o.InlineValueBytesThreshold != 0 || o.BulkDataDescriptor != nil {
		return o, ErrIncompletePixelDataOptions
	}
	if o.MaxElementBytes < 0 || o.MaxPixelDataBytes < 0 || o.MaxTotalBytes < 0 || o.MaxElements < 0 || o.MaxFragments < 0 || o.MaxSequenceDepth < 0 {
		return o, ErrIncompletePixelDataOptions
	}
	if o.MaxPixelDataBytes == 0 || o.MaxPixelDataBytes > MaxIncompletePixelDataBytes {
		o.MaxPixelDataBytes = MaxIncompletePixelDataBytes
	}
	if o.MaxFragments == 0 || o.MaxFragments > MaxIncompletePixelDataFragments {
		o.MaxFragments = MaxIncompletePixelDataFragments
	}
	if o.MaxElementBytes == 0 {
		o.MaxElementBytes = 64 << 20
	}
	if o.MaxTotalBytes == 0 {
		o.MaxTotalBytes = 512 << 20
	}
	if o.MaxElements == 0 {
		o.MaxElements = 1000000
	}
	return o, nil
}

func terminalEOFCause(err error) bool {
	for err != nil {
		if err == io.EOF || err == io.ErrUnexpectedEOF {
			return true
		}
		next, ok := err.(interface{ Unwrap() error })
		if !ok {
			return false
		}
		err = next.Unwrap()
	}
	return false
}
func (r *Reader) incompleteSourceFailure(err error) error {
	if r.allowIncompletePixelData && r.counter.firstFailure != nil {
		return errors.Join(err, r.counter.firstFailure)
	}
	return nil
}

func (r *Reader) readIncompleteNativeToken(header core.ElementHeader, offset int64) (Token, error) {
	// Existing admission has checked the declared size. Incremental bounded
	// reads retain only bytes physically present and preserve non-EOF errors.
	var out bytes.Buffer
	left := int64(header.Length)
	buffer := make([]byte, int(min(left, 32<<10)))
	for left > 0 {
		n, err := io.ReadFull(r.counter, buffer[:int(min(left, int64(len(buffer))))])
		if failure := r.incompleteSourceFailure(err); failure != nil {
			return Token{}, &ParseError{Op: OpReadValue, Offset: offset, Tag: header.Tag, VR: header.VR, Length: header.Length, Err: failure}
		}
		if n > 0 {
			out.Write(buffer[:n])
			left -= int64(n)
		}
		if err != nil {
			if !terminalEOFCause(err) {
				return Token{}, &ParseError{Op: OpReadValue, Offset: offset, Tag: header.Tag, VR: header.VR, Length: header.Length, Err: err}
			}
			cause := r.valueReadError(offset, header, err)
			if !terminalEOFCause(cause) || r.counter.fenceHit || r.sourceEnd > 0 && r.Position() != r.sourceEnd {
				return Token{}, cause
			}
			r.incompletePixelData = &IncompletePixelData{Kind: "native-value-eof", ValueOffset: offset, FailureOffset: r.Position(), DeclaredBytes: int64(header.Length), AvailableBytes: int64(out.Len()), Cause: cause}
			r.incompleteTerminal = true
			break
		}
	}
	data := r.toStreamByteOrder(header.VR, out.Bytes())
	r.elementCount++
	r.delimiterCheckPending = true
	return Token{Kind: TokenElement, Header: header, Element: core.Element{Header: header, Value: core.RawValue(data)}}, nil
}

func (r *Reader) finishIncompleteFragments(header core.ElementHeader, start, valueOffset int64, value core.FragmentSequence, sawBOT bool, err error) (core.FragmentSequence, bool) {
	if !r.allowIncompletePixelData || !sawBOT || len(value.Fragments) == 0 || len(r.seqDelimiters) != 1 || r.seqDelimiters[0].typ != seqTokenTypePixelSequence || r.counter.fenceHit {
		return core.FragmentSequence{}, false
	}
	kind := ""
	discarded := int64(0)
	dropped := 0
	var pe *ParseError
	if terminalEOFCause(err) && terminalEOFCause(r.counter.lastError) {
		if r.sourceEnd > 0 && r.Position() != r.sourceEnd {
			return core.FragmentSequence{}, false
		}
		if errors.As(err, &pe) && pe.Tag.IsItem() && pe.Op == OpReadValue {
			if pe.Length.IsUndefined() {
				return core.FragmentSequence{}, false
			}
			kind = "encapsulated-fragment-eof"
			dropped = 1
			discarded = r.Position() - start - 8
			if discarded < 0 {
				return core.FragmentSequence{}, false
			}
		} else {
			discarded = r.Position() - start
			if discarded < 0 || discarded > 7 {
				return core.FragmentSequence{}, false
			}
			// An identified ordinary data element, malformed delimiter, or private
			// tag must never be discarded as an incomplete control header.
			if pe != nil && pe.Tag != (core.Tag{}) && !pe.Tag.IsItem() && !pe.Tag.IsSequenceDelimitationItem() {
				return core.FragmentSequence{}, false
			}
			kind = "encapsulated-delimiter-eof"
		}
	} else if errors.As(err, &pe) && pe.Op == OpReadVR && r.sourceEnd-start == 8 && r.Position()-start == 6 && !pe.Tag.IsPrivate() {
		// The eight-byte garbage terminator is admitted only from a sized stable
		// ReaderAt; do not infer EOF by scanning an unbounded streaming source.
		if r.dict == nil {
			return core.FragmentSequence{}, false
		}
		if _, known := r.dict.ByTag(pe.Tag); known {
			return core.FragmentSequence{}, false
		}
		src, ok := r.counter.r.(io.ReaderAt)
		if !ok {
			return core.FragmentSequence{}, false
		}
		var tail [8]byte
		n, re := src.ReadAt(tail[:], start-r.baseOffset+r.rootOffset)
		if n != 8 || (re != nil && re != io.EOF) {
			if re != nil && r.counter.firstFailure == nil {
				r.counter.firstFailure = re
			}
			return core.FragmentSequence{}, false
		}
		if _, re = core.ParseVR(string(tail[4:6])); re == nil {
			return core.FragmentSequence{}, false
		}
		var rest [2]byte
		if _, re = io.ReadFull(r.counter, rest[:]); re != nil {
			return core.FragmentSequence{}, false
		}
		var probe [1]byte
		n, re = r.counter.Read(probe[:])
		if n != 0 || re != io.EOF {
			return core.FragmentSequence{}, false
		}
		kind = "encapsulated-garbage-terminator"
		discarded = 8
	} else {
		return core.FragmentSequence{}, false
	}
	if r.incompleteSourceFailure(err) != nil {
		return core.FragmentSequence{}, false
	}
	var available int64
	available = int64(len(value.OffsetTable))
	for _, f := range value.Fragments {
		available += int64(len(f))
	}
	r.incompletePixelData = &IncompletePixelData{Kind: kind, ValueOffset: valueOffset, FailureOffset: start, DeclaredBytes: -1, AvailableBytes: available, RetainedFragments: len(value.Fragments), DiscardedFragments: dropped, DiscardedBytes: discarded, Cause: err}
	r.seqDelimiters = nil
	r.pixelSequenceOffsetTablePending = false
	r.delimiterCheckPending = false
	r.incompleteTerminal = true
	return value, true
}

func (r *Reader) incompleteCollectionError() error {
	if info, ok := r.IncompletePixelData(); ok {
		return &IncompletePixelDataError{Read: info}
	}
	return nil
}

func (r *Reader) rejectIncompleteCollectionMode() error {
	if r.allowIncompletePixelData && (r.validationLifecycle != nil || r.selective != nil || r.stopBeforePixelData) {
		return fmt.Errorf("%w: selective or validated collector", ErrIncompletePixelDataOptions)
	}
	return nil
}

// Item bytes are admitted by the existing cumulative Pixel Data budget first.
// Even a capped declared VL must not cause a full allocation before EOF.
func (r *Reader) readIncompleteItemBytes(header core.ElementHeader, offset int64) ([]byte, error) {
	var out bytes.Buffer
	left := int64(header.Length)
	scratch := make([]byte, int(min(left, 32<<10)))
	for left > 0 {
		n, err := io.ReadFull(r.counter, scratch[:int(min(left, int64(len(scratch))))])
		if failure := r.incompleteSourceFailure(err); failure != nil {
			return nil, &ParseError{Op: OpReadValue, Offset: offset, Tag: header.Tag, VR: header.VR, Length: header.Length, Err: failure}
		}
		if n > 0 {
			out.Write(scratch[:n])
			left -= int64(n)
		}
		if err != nil {
			return nil, r.valueReadError(offset, header, err)
		}
	}
	return out.Bytes(), nil
}
