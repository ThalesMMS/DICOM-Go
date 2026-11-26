package mllp

import (
	"context"
	"errors"
	"fmt"
	"net"
	"time"
)

// Release selects a wire profile. Zero is the existing MLLP1 profile.
type Release uint8

const (
	MLLP1 Release = iota
	// MLLP2 adds framed, one-byte transport ACK/NAK confirmations in both
	// directions. These confirmations are independent of application HL7 ACKs.
	MLLP2
)

const (
	transportACK byte = 0x06
	transportNAK byte = 0x15
)

var (
	ErrUnsupportedRelease    = errors.New("mllp: unsupported release")
	ErrTransportNAK          = errors.New("mllp: transport frame rejected")
	ErrTransportConfirmation = errors.New("mllp: malformed transport confirmation")
)

// ExchangePhase identifies an MLLP2 operation without exposing message values.
type ExchangePhase string

const (
	PhaseWriteRequest         ExchangePhase = "write request"
	PhaseRequestConfirmation  ExchangePhase = "request transport confirmation"
	PhaseReadResponse         ExchangePhase = "read application response"
	PhaseResponseConfirmation ExchangePhase = "response transport confirmation"
	PhaseParseResponse        ExchangePhase = "parse application response"
	PhaseCorrelateResponse    ExchangePhase = "correlate application response"
)

// RequestTransportState describes knowledge of the request's transport
// confirmation. Acknowledged does not assert application acceptance or storage.
type RequestTransportState string

const (
	RequestNotSent      RequestTransportState = "not sent"
	RequestUnknown      RequestTransportState = "unknown"
	RequestAcknowledged RequestTransportState = "acknowledged"
	RequestRejected     RequestTransportState = "rejected"
)

// ExchangeError separates an MLLP2 phase and request transport result from
// application AA/AE/AR. Unwrap retains timeout, cancellation and framing causes.
// No automatic retransmission follows this error, including a received NAK.
type ExchangeError struct {
	Phase        ExchangePhase
	RequestState RequestTransportState
	Err          error
}

func (e *ExchangeError) Error() string {
	return "mllp: MLLP2 exchange failed (phase=" + string(e.Phase) + ", request transport=" + string(e.RequestState) + ")"
}
func (e *ExchangeError) Unwrap() error { return e.Err }

func (r Release) valid() bool { return r == MLLP1 || r == MLLP2 }

func boundedDeadline(ctx context.Context, duration time.Duration, ceiling time.Time) time.Time {
	deadline := time.Now().Add(duration)
	if !ceiling.IsZero() && ceiling.Before(deadline) {
		deadline = ceiling
	}
	if d, ok := ctx.Deadline(); ok && d.Before(deadline) {
		deadline = d
	}
	return deadline
}

// Closing, instead of merely advancing a deadline, prevents another phase
// from undoing cancellation by setting its own future deadline. Join the
// callback before a client operation releases its serialization gate.
func closeOnCancel(ctx context.Context, conn net.Conn) func() {
	done := make(chan struct{})
	stop := context.AfterFunc(ctx, func() {
		_ = conn.Close()
		close(done)
	})
	return func() {
		if !stop() {
			<-done
		}
	}
}

func (r *Reader) readConfirmation() error {
	// Server frame-start callbacks must not replace this phase's deadline.
	onStart := r.onStart
	r.onStart = nil
	defer func() { r.onStart = onStart }()
	payload, err := r.readFrame(1)
	if err != nil {
		if errors.Is(err, ErrFraming) || errors.Is(err, ErrFrameTooLarge) {
			return errors.Join(ErrTransportConfirmation, err)
		}
		return err
	}
	switch payload[0] {
	case transportACK:
		return nil
	case transportNAK:
		return ErrTransportNAK
	default:
		return ErrTransportConfirmation
	}
}

func writeConfirmation(conn net.Conn, accepted bool) error {
	value := transportNAK
	if accepted {
		value = transportACK
	}
	return WriteFrame(conn, []byte{value})
}

func exchangeError(ctx context.Context, phase ExchangePhase, state RequestTransportState, err error) error {
	if ctx.Err() != nil {
		err = errors.Join(ctx.Err(), err)
	}
	return &ExchangeError{Phase: phase, RequestState: state, Err: err}
}

func transportPhaseError(phase string, err error) error {
	return fmt.Errorf("mllp: %s: %w", phase, err)
}
