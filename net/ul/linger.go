package ul

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"time"
)

// DefaultARTIMTimeout bounds how long a connection stays open after this
// side sent an A-ASSOCIATE-RJ or A-ABORT, waiting for the peer to read it
// and close (PS3.8 9.1.5, ARTIM; DCMTK waits in Sta13 the same way).
const DefaultARTIMTimeout = 2 * time.Second

// maxDrainBytes bounds the input discarded while waiting for the peer to
// close, so a peer that keeps sending cannot hold the connection. It has to
// cover what a peer in the middle of a retrieve sends before it reads the
// A-ABORT: with a megabyte, a provider streaming sub-operations on loopback
// was reset before it saw the abort. The ARTIM deadline still ends the wait.
const maxDrainBytes = 64 << 20

// ErrUnrecognizedPDU reports a PDU of an unknown type. It also satisfies
// errors.Is(err, ErrUnexpectedPDU).
var ErrUnrecognizedPDU = errors.New("dicom ul: unrecognized PDU type")

// UnexpectedPDUError describes a PDU received where the protocol does not
// allow it; an unknown PDU type is also ErrUnrecognizedPDU.
func UnexpectedPDUError(pdu PDU, while string) error {
	if _, unknown := pdu.(*UnknownPDU); unknown {
		return fmt.Errorf("%w (%w): got %s while %s", ErrUnexpectedPDU, ErrUnrecognizedPDU, pduName(pdu), while)
	}
	return fmt.Errorf("%w: got %s while %s", ErrUnexpectedPDU, pduName(pdu), while)
}

// ProviderAbortReason maps an upper-layer protocol error to the A-P-ABORT
// reason of PS3.8 Table 9-26 (source service-provider): 1
// unrecognized-PDU, 2 unexpected-PDU, 4 unrecognized-PDU-parameter, 6
// invalid-PDU-parameter-value. Errors that are not protocol violations
// (transport, timeouts, cancellation, application failures) report false.
func ProviderAbortReason(err error) (byte, bool) {
	switch {
	case err == nil:
		return 0, false
	case errors.Is(err, ErrUnrecognizedPDU), errors.Is(err, ErrUnsupportedPDU):
		return AbortReasonUnrecognizedPDU, true
	case errors.Is(err, ErrUnexpectedPDU):
		return AbortReasonUnexpectedPDU, true
	case errors.Is(err, ErrInvalidPDUItem), errors.Is(err, ErrInvalidUserItem):
		return AbortReasonUnrecognizedPDUParameter, true
	case errors.Is(err, ErrPDUTooLarge), errors.Is(err, ErrLengthOverflow), errors.Is(err, ErrInvalidPDUField),
		errors.Is(err, ErrMissingPDUField), errors.Is(err, ErrInvalidAEtitle), errors.Is(err, ErrInvalidPCID),
		errors.Is(err, ErrInvalidPDUSize), errors.Is(err, ErrInvalidPDU):
		return AbortReasonInvalidPDUParameter, true
	}
	return 0, false
}

// artimTimeout resolves an ARTIM option: zero is DefaultARTIMTimeout and a
// negative value closes at once.
func artimTimeout(configured time.Duration) time.Duration {
	switch {
	case configured == 0:
		return DefaultARTIMTimeout
	case configured < 0:
		return 0
	}
	return configured
}

// contextLimitReached reports whether ctx was canceled or its deadline has
// passed. A context.WithDeadline reports Err only once its timer has run,
// which can be after a socket deadline taken from Deadline expired: Err alone
// does not tell whether the caller's limit ended an operation.
func contextLimitReached(ctx context.Context) bool {
	if ctx == nil {
		return false
	}
	if ctx.Err() != nil {
		return true
	}
	deadline, ok := ctx.Deadline()
	return ok && !time.Now().Before(deadline)
}

// lingerClose half-closes conn, discards at most maxDrainBytes of input
// until the peer closes or the absolute ARTIM deadline passes, and closes
// it. Closing at once with unread input would make TCP send RST, and the
// peer could see a reset instead of the RJ or ABORT just written.
func lingerClose(conn net.Conn, artim time.Duration) error {
	if conn == nil {
		return nil
	}
	if artim > 0 {
		if halfCloser, ok := conn.(interface{ CloseWrite() error }); ok {
			_ = halfCloser.CloseWrite()
		}
		if conn.SetReadDeadline(time.Now().Add(artim)) == nil {
			_, _ = io.CopyN(io.Discard, conn, maxDrainBytes)
		}
	}
	return conn.Close()
}

// rejectAndClose sends an A-ASSOCIATE-RJ during negotiation and lingers.
func rejectAndClose(opts AcceptOptions, conn net.Conn, rj *AssociationRJ, operational *operationalState) error {
	notifyAssociationRejected(opts)
	notifyAssociationRejection(opts, conn, rj)
	writeErr := writeAssociationPDUObserved(opts.Context, conn, rj, operational)
	_ = lingerClose(conn, artimTimeout(opts.ARTIMTimeout))
	return writeErr
}

// providerAbortAndClose sends an A-P-ABORT with the reason of err, if err is
// a protocol violation, and lingers; other errors close at once.
func providerAbortAndClose(ctx context.Context, conn net.Conn, err error, artim time.Duration, operational *operationalState) {
	reason, ok := ProviderAbortReason(err)
	if !ok {
		_ = conn.Close()
		return
	}
	// A deadline can pass before the context reports it, so the abort gets
	// its own deadline whenever the caller's limit is reached.
	if contextLimitReached(ctx) {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(context.WithoutCancel(ctx), artim+time.Second)
		defer cancel()
	}
	_ = writeAssociationPDUObserved(ctx, conn, &AbortRQ{Source: AbortSourceServiceProvider, Reason: reason}, operational)
	_ = lingerClose(conn, artim)
}

// AbortForError aborts the association after err: a protocol violation
// (see ProviderAbortReason) is answered with an A-P-ABORT from the
// service-provider and its PS3.8 reason, anything else with an A-ABORT from
// the service-user, reason not specified.
func (a *Association) AbortForError(ctx context.Context, err error) error {
	if reason, ok := ProviderAbortReason(err); ok {
		return a.abort(ctx, AbortSourceServiceProvider, reason)
	}
	return a.abort(ctx, AbortSourceServiceUser, AbortReasonNotSpecified)
}

// closeLingering closes the association after this side sent an A-ABORT.
func (a *Association) closeLingering() error {
	if a == nil {
		return nil
	}
	a.connMu.Lock()
	conn := a.Conn
	a.Conn = nil
	a.connMu.Unlock()
	a.clearPDataCarryover()
	a.operational.close()
	return lingerClose(conn, a.artimTimeout)
}
