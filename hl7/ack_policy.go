package hl7

import "errors"

// AckCondition identifies an acknowledgment request in HL7 table 0155.
// The zero value means the corresponding MSH field was omitted or null.
type AckCondition string

const (
	AckAlways    AckCondition = "AL"
	AckNever     AckCondition = "NE"
	AckOnError   AckCondition = "ER"
	AckOnSuccess AckCondition = "SU"
)

// AckPolicy is the pair of acknowledgment requests in MSH-15 and MSH-16.
// Its zero value uses original mode, which calls for an application response.
type AckPolicy struct {
	Accept      AckCondition // MSH-15, after deciding whether to accept the message
	Application AckCondition // MSH-16, after processing the message
}

// ReadAckPolicy reads and validates both acknowledgment conditions. If only
// one is present, the omitted or null condition requests no ACK in that phase.
func ReadAckPolicy(message *Message) (AckPolicy, error) {
	if message == nil || len(message.Segments) == 0 || message.Segments[0].Name != "MSH" {
		return AckPolicy{}, &Error{
			Code: ErrorCodeSegmentSequence, Location: ErrorLocation{Segment: "MSH", Sequence: 1},
			Err: errors.New("missing initial MSH segment"),
		}
	}
	msh := message.Segments[0]
	accept, err := readAckCondition(msh, 15)
	if err != nil {
		return AckPolicy{}, err
	}
	application, err := readAckCondition(msh, 16)
	if err != nil {
		return AckPolicy{}, err
	}
	return AckPolicy{Accept: accept, Application: application}, nil
}

func readAckCondition(msh Segment, number int) (AckCondition, error) {
	field := msh.Field(number)
	if len(field) == 0 {
		return "", nil
	}
	if len(field) == 1 && len(field[0]) == 1 && len(field[0][0]) == 1 {
		condition := AckCondition(field.String())
		switch condition {
		case "", Null:
			return "", nil
		case AckAlways, AckNever, AckOnError, AckOnSuccess:
			return condition, nil
		}
	}
	return "", &Error{
		Code: ErrorCodeTableValueNotFound, Location: ErrorLocation{Segment: "MSH", Sequence: 1, Field: number},
		Err: errors.New("invalid acknowledgment condition"),
	}
}

// Enhanced reports whether at least one acknowledgment condition is present.
func (p AckPolicy) Enhanced() bool {
	return p.Accept != "" || p.Application != ""
}

// Wants reports whether the message requests the ACK for the given outcome.
// The caller determines the outcome after the relevant storage or processing.
func (p AckPolicy) Wants(code AckCode) bool {
	switch code {
	case AckCommitAccept, AckCommitError, AckCommitReject:
		return p.Accept.wants(code == AckCommitAccept)
	case AckAccept, AckError, AckReject:
		return !p.Enhanced() || p.Application.wants(code == AckAccept)
	default:
		return false
	}
}

func (c AckCondition) wants(success bool) bool {
	return c == AckAlways || (c == AckOnSuccess && success) || (c == AckOnError && !success)
}
