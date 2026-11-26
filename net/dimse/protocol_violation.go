package dimse

import (
	"errors"
	"fmt"
)

// ErrProtocolViolation classifies a response that the peer may not send for
// the request in progress: another service's command, a response to a
// different Message ID, or a pending C-FIND response without its Identifier.
// errors.Is reports it for the errors the service users return in those
// cases; their messages keep the detail.
var ErrProtocolViolation = errors.New("dicom dimse: protocol violation")

// ErrCEchoStatus classifies a C-ECHO response whose status is not Success.
var ErrCEchoStatus = errors.New("dicom dimse: C-ECHO status")

type classifiedError struct {
	class   error
	message string
}

func (e *classifiedError) Error() string { return e.message }

func (e *classifiedError) Is(target error) bool { return target == e.class }

func protocolViolationf(format string, args ...any) error {
	return &classifiedError{class: ErrProtocolViolation, message: fmt.Sprintf(format, args...)}
}
