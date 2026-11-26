package hl7

import (
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"strconv"
	"time"
)

// AckCode is MSA-1 in original or enhanced acknowledgment mode (HL7 table 0008).
type AckCode string

const (
	// AckAccept is AA: the message was processed.
	AckAccept AckCode = "AA"
	// AckError is AE: the message was refused for an error in its content.
	AckError AckCode = "AE"
	// AckReject is AR: the message was refused for its type, version or
	// processing ID, or for a receiver failure unrelated to its content.
	AckReject AckCode = "AR"
	// AckCommitAccept is CA, used after the receiver accepts responsibility
	// for a message in enhanced mode.
	AckCommitAccept AckCode = "CA"
	// AckCommitError is CE, a failure to accept the message in enhanced mode.
	AckCommitError AckCode = "CE"
	// AckCommitReject is CR, used for unsupported message type, processing ID
	// or version in enhanced mode.
	AckCommitReject AckCode = "CR"
)

// Error codes of HL7 table 0357 (ERR-3).
const (
	ErrorCodeSegmentSequence          = "100"
	ErrorCodeRequiredFieldMissing     = "101"
	ErrorCodeDataType                 = "102"
	ErrorCodeTableValueNotFound       = "103"
	ErrorCodeUnsupportedMessageType   = "200"
	ErrorCodeUnsupportedEventCode     = "201"
	ErrorCodeUnsupportedProcessingID  = "202"
	ErrorCodeUnsupportedVersionID     = "203"
	ErrorCodeUnknownKeyIdentifier     = "204"
	ErrorCodeDuplicateKeyIdentifier   = "205"
	ErrorCodeApplicationRecordLocked  = "206"
	ErrorCodeApplicationInternalError = "207"
)

// ErrorLocation is ERR-2 (ERL): where in the message the error lies.
type ErrorLocation struct {
	Segment   string
	Sequence  int // 1-based occurrence of the segment
	Field     int
	Component int
}

func (l ErrorLocation) String() string {
	if l.Segment == "" {
		return ""
	}
	s := fmt.Sprintf("%s-%d", l.Segment, l.Field)
	if l.Sequence > 1 {
		s = fmt.Sprintf("%s[%d]-%d", l.Segment, l.Sequence, l.Field)
	}
	if l.Component > 0 {
		s += fmt.Sprintf(".%d", l.Component)
	}
	return s
}

// Error is an application-level refusal that maps onto an acknowledgment:
// its HL7 error code, location and whether it rejects (AR) rather than
// errors (AE). Its text names the location, never values.
type Error struct {
	Code     string
	Location ErrorLocation
	Reject   bool
	Err      error
}

func (e *Error) Error() string {
	location := e.Location.String()
	if location != "" {
		location = " at " + location
	}
	return fmt.Sprintf("hl7: error %s%s: %v", e.Code, location, e.Err)
}

func (e *Error) Unwrap() error { return e.Err }

// AckOptions complete an acknowledgment.
type AckOptions struct {
	// Text is MSA-3; it reaches the sender and must not carry patient data.
	Text string
	// ErrorCode is ERR-3 for error and reject codes. The default is 207
	// (application internal error).
	ErrorCode string
	// ErrorLocation is ERR-2.
	ErrorLocation ErrorLocation
	// ControlID is the acknowledgment's MSH-10; default a random ID.
	ControlID string
	// Time is MSH-7; default now.
	Time time.Time
}

// NewACK builds a general acknowledgment. The MSH swaps sender and receiver
// and preserves the version, processing ID and character set. MSA carries
// the supplied code and the original control ID. Error and reject codes add
// an ERR segment in the HL7 v2.5 layout.
func NewACK(original *Message, code AckCode, options AckOptions) *Message {
	header := original.Header()
	now := options.Time
	if now.IsZero() {
		now = time.Now()
	}
	controlID := options.ControlID
	if controlID == "" {
		controlID = newControlID()
	}
	version := header.Version
	if version == "" {
		version = "2.5.1"
	}
	processing := header.ProcessingID
	if processing == "" {
		processing = "P"
	}
	ack := NewMessage(MessageHeader{
		SendingApplication: header.ReceivingApplication, SendingFacility: header.ReceivingFacility,
		ReceivingApplication: header.SendingApplication, ReceivingFacility: header.SendingFacility,
		DateTime:     now.Format("20060102150405"),
		Type:         MessageType{Code: "ACK", Trigger: header.Type.Trigger, Structure: "ACK"},
		ControlID:    controlID,
		ProcessingID: processing,
		Version:      version,
		CharacterSet: header.CharacterSet,
	})
	msa := ack.Add("MSA")
	msa.SetValue(1, string(code))
	msa.SetValue(2, header.ControlID)
	if options.Text != "" {
		msa.SetValue(3, options.Text)
	}
	if code != AckAccept && code != AckCommitAccept {
		errorCode := options.ErrorCode
		if errorCode == "" {
			errorCode = ErrorCodeApplicationInternalError
		}
		err := ack.Add("ERR")
		if l := options.ErrorLocation; l.Segment != "" {
			sequence := l.Sequence
			if sequence < 1 {
				sequence = 1
			}
			location := []string{l.Segment, strconv.Itoa(sequence), strconv.Itoa(l.Field)}
			if l.Component > 0 {
				location = append(location, "", strconv.Itoa(l.Component))
			}
			err.SetField(2, Components(location...))
		}
		err.SetField(3, Components(errorCode, errorCodeText(errorCode), "HL70357"))
		err.SetValue(4, "E")
	}
	return ack
}

// ACKFor acknowledges original after processing that returned err: AA for
// nil, otherwise AE, or AR when err is an *Error with Reject, with the
// *Error's code and location. The MSA-3 text is options.Text or the
// generic text of the code, never err's message.
func ACKFor(original *Message, err error, options AckOptions) *Message {
	if err == nil {
		return NewACK(original, AckAccept, options)
	}
	code := AckError
	var hl7Err *Error
	if errors.As(err, &hl7Err) {
		if hl7Err.Reject {
			code = AckReject
		}
		if options.ErrorCode == "" {
			options.ErrorCode = hl7Err.Code
		}
		if options.ErrorLocation.Segment == "" {
			options.ErrorLocation = hl7Err.Location
		}
	}
	if options.ErrorCode == "" {
		options.ErrorCode = ErrorCodeApplicationInternalError
	}
	if options.Text == "" {
		options.Text = errorCodeText(options.ErrorCode)
	}
	return NewACK(original, code, options)
}

func errorCodeText(code string) string {
	switch code {
	case ErrorCodeSegmentSequence:
		return "Segment sequence error"
	case ErrorCodeRequiredFieldMissing:
		return "Required field missing"
	case ErrorCodeDataType:
		return "Data type error"
	case ErrorCodeTableValueNotFound:
		return "Table value not found"
	case ErrorCodeUnsupportedMessageType:
		return "Unsupported message type"
	case ErrorCodeUnsupportedEventCode:
		return "Unsupported event code"
	case ErrorCodeUnsupportedProcessingID:
		return "Unsupported processing id"
	case ErrorCodeUnsupportedVersionID:
		return "Unsupported version id"
	case ErrorCodeUnknownKeyIdentifier:
		return "Unknown key identifier"
	case ErrorCodeDuplicateKeyIdentifier:
		return "Duplicate key identifier"
	case ErrorCodeApplicationRecordLocked:
		return "Application record locked"
	default:
		return "Application internal error"
	}
}

func newControlID() string {
	var b [10]byte
	if _, err := rand.Read(b[:]); err != nil {
		return strconv.FormatInt(time.Now().UnixNano(), 36)
	}
	return hex.EncodeToString(b[:])
}

// Acknowledgment is a parsed MSA (and ERR) segment.
type Acknowledgment struct {
	Code      AckCode
	ControlID string // MSA-2, the acknowledged message's MSH-10
	Text      string // MSA-3
	ErrorCode string // ERR-3.1, or the v2.3 ERR-1 code
}

// ParseAcknowledgment reads the MSA and first ERR segments of a response.
func ParseAcknowledgment(m *Message) (Acknowledgment, error) {
	msa, ok := m.Segment("MSA")
	if !ok {
		return Acknowledgment{}, &Error{Code: ErrorCodeSegmentSequence, Location: ErrorLocation{Segment: "MSA", Sequence: 1}, Err: errors.New("no MSA segment")}
	}
	ack := Acknowledgment{Code: AckCode(msa.Field(1).String()), ControlID: msa.Field(2).String(), Text: msa.Field(3).String()}
	switch ack.Code {
	case AckAccept, AckError, AckReject, AckCommitAccept, AckCommitError, AckCommitReject:
	default:
		return Acknowledgment{}, &Error{Code: ErrorCodeTableValueNotFound, Location: ErrorLocation{Segment: "MSA", Sequence: 1, Field: 1}, Err: errors.New("unknown acknowledgment code")}
	}
	if errSegment, ok := m.Segment("ERR"); ok {
		ack.ErrorCode = errSegment.Value(3, 1, 1)
		if ack.ErrorCode == "" {
			// HL7 v2.3/2.4: ERR-1 is segment^sequence^field^code (CE).
			ack.ErrorCode = errSegment.Value(1, 4, 1)
		}
	}
	return ack, nil
}
