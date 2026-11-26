package parser

import (
	"encoding/binary"
	"io"

	"github.com/ThalesMMS/dicom-go/core"
)

// correctGETextLength recognizes the GE 13-for-10 text-length defect. This
// is deliberately narrower than replacing every implicit VL=13: the stream
// itself must distinguish the two boundaries. No value is logged or matched
// against vendor-specific text. The fixed lookahead also works without Seek.
func (r *Reader) correctGETextLength(tag core.Tag, vr core.VR) (bool, error) {
	if r.syntax.ExplicitVR || r.currentDecoder().Order != binary.LittleEndian ||
		!r.geTextLengthEligible(tag, vr) {
		return false, nil
	}

	valueOffset := r.Position()
	remaining, bounded := r.geTextLengthRemaining(valueOffset)
	// GE also wrote chains of VL=13 text values (including DS). Every link
	// must be unambiguous and end at a normal, defined even-length header.
	// These limits are independent of the lengths written in the input.
	const maxLookahead = 10 + 8 + 64
	const maxChain = 4
	peek := maxLookahead
	if bounded && remaining < int64(peek) {
		peek = int(max(remaining, 0))
	}
	if peek < 10+8 {
		return false, nil
	}
	var ahead [maxLookahead]byte
	n, err := io.ReadFull(r.counter, ahead[:peek])
	r.counter.unread(ahead[:n])
	if err != nil && err != io.EOF && err != io.ErrUnexpectedEOF {
		return false, &ParseError{Op: OpReadValue, Offset: valueOffset, Tag: tag, VR: vr, Err: err}
	}
	if n < peek {
		// The source ended earlier than its advertised size (for example a
		// file truncated after construction). Use the observed end instead.
		remaining, bounded = int64(n), true
	}
	if n < 10+8 {
		return false, nil
	}
	current, currentVR, at := tag, vr, 0
	terminal := false
	for link := 0; link < maxChain; link++ {
		if at+18 > n || !r.geTextLengthEligible(current, currentVR) {
			return false, nil
		}
		for _, b := range ahead[at : at+10] {
			if b < 0x20 || b > 0x7e {
				return false, nil
			}
		}
		next, nextVR, ok := r.geTextLengthPublicTag(ahead[at+10 : at+18])
		if !ok || !current.Less(next) || nextVR == core.VRSQ {
			return false, nil
		}
		// A known tag at the declared boundary is sufficient uncertainty to
		// refuse a link, even if its length bytes are absent or malformed.
		if r.geTextLengthAmbiguousTag(ahead[at+13 : min(at+21, n)]) {
			return false, nil
		}
		length := binary.LittleEndian.Uint32(ahead[at+14 : at+18])
		if length == 13 {
			current, currentVR, at = next, nextVR, at+18
			continue
		}
		if length == uint32(core.UndefinedLength) || length&1 != 0 ||
			r.maxElementBytes > 0 && int64(length) > r.maxElementBytes {
			return false, nil
		}
		if bounded {
			if int64(length) > remaining-int64(at+18) {
				return false, nil
			}
		}
		// A fence or byte budget is only an upper bound, not evidence that
		// the bytes exist. Only a sized source permits a terminal value to
		// extend past the buffered lookahead.
		if r.sourceEnd <= 0 && int64(length) > int64(n)-(int64(at)+18) {
			return false, nil
		}
		terminal = true
		break
	}
	if !terminal {
		return false, nil
	}
	r.recordBoundaryAt(BoundaryRecovery{
		Offset: valueOffset, DeclaredLength: 13, AvailableLength: 10,
		Reason: BoundaryGETextLength, DeclaredVR: vr, UsedVR: vr,
	}, func() string { return r.childPath(tag) })
	return true, nil
}

// Ambiguity needs only a known public tag, including contextual/alternative
// VRs. Choosing a unique VR is required to authorize +10, not to exclude +13.
func (r *Reader) geTextLengthAmbiguousTag(b []byte) bool {
	tag := core.NewTag(binary.LittleEndian.Uint16(b[:2]), binary.LittleEndian.Uint16(b[2:4]))
	if tag.IsPrivate() || tag.IsSequenceDelimiting() || r.dict == nil {
		return false
	}
	if _, known := r.dict.ByTag(tag); known {
		return true
	}
	if tag.IsGroupLength() {
		_, _, plausible := r.geTextLengthPublicTag(b)
		return plausible
	}
	return false
}

func (r *Reader) geTextLengthEligible(tag core.Tag, vr core.VR) bool {
	if tag.IsPrivate() || tag.IsGroupLength() || !vr.IsStringLike() ||
		tag == core.NewTag(0x0008, 0x0070) || tag == core.NewTag(0x0008, 0x0080) ||
		r.maxElementBytes > 0 && r.maxElementBytes < 10 {
		return false
	}
	dictVR, ok := r.singleDictionaryVR(tag)
	return ok && dictVR == vr
}

// geTextLengthRemaining combines every known bound in stream coordinates.
func (r *Reader) geTextLengthRemaining(offset int64) (int64, bool) {
	end := int64(0)
	for _, bound := range []int64{r.sourceEnd, r.counter.fence, r.maxTotalBytes} {
		if bound > 0 && (end == 0 || bound < end) {
			end = bound
		}
	}
	return end - offset, end != 0
}

func (r *Reader) geTextLengthPublicTag(b []byte) (core.Tag, core.VR, bool) {
	tag := core.NewTag(binary.LittleEndian.Uint16(b[:2]), binary.LittleEndian.Uint16(b[2:4]))
	if tag.Group < 0x0008 || tag.IsPrivate() || tag.IsSequenceDelimiting() {
		return tag, "", false
	}
	if tag.IsGroupLength() {
		// Retired data-set group lengths are UL. Recognize only a public
		// group anchored by its standard (gggg,0010) dictionary entry; this
		// and a complete VL=4 header exclude arbitrary overlapping bytes
		// such as (0810,0000) or (0400,0000) with a huge false length.
		if len(b) < 8 || binary.LittleEndian.Uint32(b[4:8]) != 4 {
			return tag, "", false
		}
		_, known := r.singleDictionaryVR(core.NewTag(tag.Group, 0x0010))
		return tag, core.VRUL, known
	}
	vr, ok := r.singleDictionaryVR(tag)
	return tag, vr, ok
}
