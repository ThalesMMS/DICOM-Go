package jpip

import (
	"errors"
	"fmt"
)

// DataBinClass is a JPIP data-bin class (ISO/IEC 15444-9 A.2.2, Table A.2).
type DataBinClass uint8

const (
	ClassPrecinct           DataBinClass = 0
	ClassExtendedPrecinct   DataBinClass = 1
	ClassTileHeader         DataBinClass = 2
	ClassTile               DataBinClass = 4
	ClassExtendedTile       DataBinClass = 5
	ClassMainHeader         DataBinClass = 6
	ClassMetadata           DataBinClass = 8
	maxSupportedDataBinCode              = 8
)

// EORReason is the reason code of an End Of Response message (ISO/IEC
// 15444-9 D.3, Table D.2).
type EORReason uint8

const (
	EORImageDone            EORReason = 1
	EORWindowDone           EORReason = 2
	EORWindowChange         EORReason = 3
	EORByteLimitReached     EORReason = 4
	EORQualityLimitReached  EORReason = 5
	EORSessionLimitReached  EORReason = 6
	EORResponseLimitReached EORReason = 7
	EORNonSpecified         EORReason = 0xff
)

// Message is one JPP-stream or JPT-stream message: a contiguous range of one
// data-bin (ISO/IEC 15444-9 Annex A). Complete reports that the range ends
// the data-bin.
type Message struct {
	Class    DataBinClass
	Stream   uint64 // CSn, the codestream identifier
	ID       uint64 // in-class identifier
	Offset   uint64
	Data     []byte
	Complete bool
	Aux      uint64 // only for extended classes
}

// Stream is a parsed JPIP response body.
type Stream struct {
	Messages []Message
	// EOR is the End Of Response reason; HasEOR is false when the body ended
	// without one (a truncated or still-open response).
	EOR    EORReason
	HasEOR bool
}

// ErrMalformedStream reports a JPP/JPT stream that violates ISO/IEC 15444-9
// Annex A message syntax.
var ErrMalformedStream = errors.New("dicom: malformed JPIP data-bin stream")

const maxVBASBytes = 9 // 63 value bits

// appendVBAS appends a variable-length byte-aligned segment (ISO/IEC
// 15444-9 A.2.1): big-endian 7-bit groups, all but the last with bit 7 set.
func appendVBAS(out []byte, value uint64) []byte {
	n := 1
	for v := value >> 7; v != 0; v >>= 7 {
		n++
	}
	for i := n - 1; i >= 0; i-- {
		b := byte(value>>(7*uint(i))) & 0x7f
		if i != 0 {
			b |= 0x80
		}
		out = append(out, b)
	}
	return out
}

// appendMessageHeader writes a header that names its class and codestream
// explicitly, so every message can be read on its own.
func appendMessageHeader(out []byte, m Message, length int) []byte {
	// Bin-ID: bit 7 continuation, bits 6-5 indicator (3 = class and CSn
	// follow), bit 4 completeness, then the in-class ID in big-endian
	// 4+7+7... bit groups.
	groups := 0
	for v := m.ID >> 4; v != 0; v >>= 7 {
		groups++
	}
	first := byte(3<<5) | byte(m.ID>>(7*uint(groups)))&0x0f
	if m.Complete {
		first |= 0x10
	}
	if groups > 0 {
		first |= 0x80
	}
	out = append(out, first)
	for i := groups - 1; i >= 0; i-- {
		b := byte(m.ID>>(7*uint(i))) & 0x7f
		if i != 0 {
			b |= 0x80
		}
		out = append(out, b)
	}
	out = appendVBAS(out, uint64(m.Class))
	out = appendVBAS(out, m.Stream)
	out = appendVBAS(out, m.Offset)
	out = appendVBAS(out, uint64(length))
	if m.Class == ClassExtendedPrecinct || m.Class == ClassExtendedTile {
		out = appendVBAS(out, m.Aux)
	}
	return out
}

// AppendMessage appends one message (header and data) to out.
func AppendMessage(out []byte, m Message) []byte {
	out = appendMessageHeader(out, m, len(m.Data))
	return append(out, m.Data...)
}

// AppendEOR appends an End Of Response message with an empty body.
func AppendEOR(out []byte, reason EORReason) []byte {
	return append(out, 0x00, byte(reason), 0x00)
}

// ParseStream parses a JPP-stream or JPT-stream response body. Message data
// alias body. Messages after an EOR message are rejected.
func ParseStream(body []byte) (Stream, error) {
	var stream Stream
	var class DataBinClass
	var csn uint64
	havePrevious := false
	at := 0
	readVBAS := func() (uint64, error) {
		var value uint64
		for i := 0; ; i++ {
			if at >= len(body) || i == maxVBASBytes {
				return 0, fmt.Errorf("%w: truncated or oversized VBAS", ErrMalformedStream)
			}
			b := body[at]
			at++
			value = value<<7 | uint64(b&0x7f)
			if b&0x80 == 0 {
				return value, nil
			}
		}
	}
	readClass := func() (DataBinClass, error) {
		value, err := readVBAS()
		if err != nil {
			return 0, err
		}
		if value > maxSupportedDataBinCode || value == 3 || value == 7 {
			return 0, fmt.Errorf("%w: unknown data-bin class", ErrMalformedStream)
		}
		return DataBinClass(value), nil
	}
	for at < len(body) {
		if stream.HasEOR {
			return Stream{}, fmt.Errorf("%w: data after EOR", ErrMalformedStream)
		}
		first := body[at]
		if first == 0 {
			if at+2 > len(body) {
				return Stream{}, fmt.Errorf("%w: truncated EOR", ErrMalformedStream)
			}
			stream.EOR = EORReason(body[at+1])
			at += 2
			length, err := readVBAS()
			if err != nil {
				return Stream{}, err
			}
			if length > uint64(len(body)-at) {
				return Stream{}, fmt.Errorf("%w: truncated EOR body", ErrMalformedStream)
			}
			at += int(length)
			stream.HasEOR = true
			continue
		}
		at++
		indicator := (first >> 5) & 3
		if indicator == 0 {
			return Stream{}, fmt.Errorf("%w: prohibited Bin-ID indicator", ErrMalformedStream)
		}
		m := Message{Complete: first&0x10 != 0, ID: uint64(first & 0x0f)}
		for b := first; b&0x80 != 0; {
			if at >= len(body) || m.ID>>(63-7) != 0 {
				return Stream{}, fmt.Errorf("%w: truncated or oversized Bin-ID", ErrMalformedStream)
			}
			b = body[at]
			at++
			m.ID = m.ID<<7 | uint64(b&0x7f)
		}
		switch indicator {
		case 1:
			if !havePrevious {
				return Stream{}, fmt.Errorf("%w: message omits class before any class was sent", ErrMalformedStream)
			}
		case 2:
			if !havePrevious {
				return Stream{}, fmt.Errorf("%w: message omits CSn before any CSn was sent", ErrMalformedStream)
			}
			value, err := readClass()
			if err != nil {
				return Stream{}, err
			}
			class = value
		case 3:
			value, err := readClass()
			if err != nil {
				return Stream{}, err
			}
			class = value
			if csn, err = readVBAS(); err != nil {
				return Stream{}, err
			}
		}
		havePrevious = true
		m.Class, m.Stream = class, csn
		offset, err := readVBAS()
		if err != nil {
			return Stream{}, err
		}
		length, err := readVBAS()
		if err != nil {
			return Stream{}, err
		}
		if class == ClassExtendedPrecinct || class == ClassExtendedTile {
			if m.Aux, err = readVBAS(); err != nil {
				return Stream{}, err
			}
		}
		if length > uint64(len(body)-at) {
			return Stream{}, fmt.Errorf("%w: truncated message body", ErrMalformedStream)
		}
		m.Offset = offset
		m.Data = body[at : at+int(length) : at+int(length)]
		at += int(length)
		stream.Messages = append(stream.Messages, m)
	}
	return stream, nil
}
