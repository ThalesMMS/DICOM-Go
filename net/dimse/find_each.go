package dimse

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/ThalesMMS/dicom-go/object"
)

// DefaultFindCancelDrainTimeout bounds the wait for the final response after
// FindEach sends a C-CANCEL-RQ, when FindEachOptions leaves it zero.
const DefaultFindCancelDrainTimeout = 10 * time.Second

// ErrFindCancelNotAnswered reports a peer that did not send the final C-FIND
// response within the drain timeout after a C-CANCEL-RQ. The association
// state is uncertain: the caller should abort it.
var ErrFindCancelNotAnswered = errors.New("dicom dimse: no final C-FIND response after C-CANCEL")

// FindEachOptions configure FindEach.
type FindEachOptions struct {
	OperationOptions
	// CancelDrainTimeout bounds the wait, after a C-CANCEL-RQ, for the final
	// response; zero uses DefaultFindCancelDrainTimeout. The operation
	// context still applies.
	CancelDrainTimeout time.Duration
}

// FindEachOutcome describes how a FindEach operation ended.
type FindEachOutcome struct {
	// Final is the final C-FIND response, nil when none arrived.
	Final *CFindResponse
	// Pending counts the pending responses passed to the callback.
	Pending int
	// CancelSent reports that the callback stopped the operation and a
	// C-CANCEL-RQ was sent for it.
	CancelSent bool
	// Discarded counts the pending responses that arrived after the
	// C-CANCEL-RQ; they are read and dropped, never passed to the callback.
	Discarded int
}

// FindEach executes a C-FIND exchange and calls each, in the caller's
// goroutine, for every pending response in order. When each returns false
// FindEach sends a C-CANCEL-RQ for the request's Message ID (PS3.7 9.3.2.3),
// reads and discards the pending responses the peer had already sent, and
// returns once the final response (normally Cancel, FE00) arrives. A final
// response after a C-CANCEL-RQ is not an error, whatever its status.
//
// Without a cancel, a final Failure, Cancel or invalid status is returned as
// the same error FindWithOptions reports, with Final set. Transport errors
// and timeouts follow the OperationOptions error policy (default: leave the
// association open for the caller to release or abort).
func (c *FindClient) FindEach(options FindEachOptions, req CFindRequest, identifier *object.Object, each func(FindResult) bool) (FindEachOutcome, error) {
	var outcome FindEachOutcome
	normalizedSOPClassUID, err := c.validate(req)
	if err != nil {
		return outcome, err
	}
	if each == nil {
		return outcome, fmt.Errorf("dicom dimse: nil C-FIND response callback")
	}
	if identifier == nil {
		return outcome, fmt.Errorf("dicom dimse: nil C-FIND identifier")
	}
	req.AffectedSOPClassUID = normalizedSOPClassUID
	options.OperationOptions = operationOptionsWithDefaultPolicy(options.OperationOptions, OperationErrorPolicyLeaveOpen)
	if options.CancelDrainTimeout <= 0 {
		options.CancelDrainTimeout = DefaultFindCancelDrainTimeout
	}
	policy := options.ErrorPolicy
	fail := func(err error, uncertain bool) (FindEachOutcome, error) {
		if !uncertain {
			return outcome, newOperationError("C-FIND", err, false)
		}
		return outcome, applyOperationErrorPolicy(c.Assoc, policy, newOperationError("C-FIND", err, true))
	}

	ctx, cancel := operationContext(options.OperationOptions)
	defer cancel()
	if err := ctx.Err(); err != nil {
		return fail(err, false)
	}
	releaseOperation, err := beginAssociationOperation(c.Assoc)
	if err != nil {
		return fail(err, false)
	}
	defer releaseOperation()

	if err := SendCommandSetWithContext(ctx, c.Assoc, c.PresentationCtxID, req.CommandSet()); err != nil {
		return fail(err, true)
	}
	if err := SendDataSetWithContext(ctx, c.Assoc, c.PresentationCtxID, identifier, c.IdentifierSyntax); err != nil {
		return fail(err, true)
	}

	for {
		if err := ctx.Err(); err != nil {
			return fail(err, true)
		}
		responseCtx, responseCancel := operationResponseContext(ctx, options.ResponseTimeout)
		rsp, id, err := receiveCFindResponseWithContext(responseCtx, c.Assoc, c.PresentationCtxID, c.IdentifierSyntax)
		responseCancel()
		if err != nil {
			return fail(err, true)
		}
		if rsp.MessageIDBeingRespondedTo != req.MessageID {
			return fail(protocolViolationf("dicom dimse: C-FIND response message ID %d, want %d", rsp.MessageIDBeingRespondedTo, req.MessageID), true)
		}
		if id == nil && CategorizeCFindStatus(rsp.Status) == CFindStatusPending {
			// PS3.4 C.4.1.3: every pending response carries one match.
			return fail(protocolViolationf("dicom dimse: pending C-FIND response without an Identifier"), true)
		}
		switch CategorizeCFindStatus(rsp.Status) {
		case CFindStatusPending:
			outcome.Pending++
			if each(FindResult{Response: rsp, Identifier: id}) {
				continue
			}
			return c.cancelFindEach(ctx, options, req, outcome)
		case CFindStatusSuccess:
			outcome.Final = rsp
			return outcome, nil
		default:
			outcome.Final = rsp
			return outcome, CFindStatusError(rsp)
		}
	}
}

// cancelFindEach sends the C-CANCEL-RQ and drains the operation to its final
// response.
func (c *FindClient) cancelFindEach(ctx context.Context, options FindEachOptions, req CFindRequest, outcome FindEachOutcome) (FindEachOutcome, error) {
	drainCtx, cancel := context.WithTimeout(ctx, options.CancelDrainTimeout)
	defer cancel()
	if err := SendCCancelRequestWithContext(drainCtx, c.Assoc, c.PresentationCtxID, CCancelRequest{MessageIDBeingRespondedTo: req.MessageID}); err != nil {
		return outcome, applyOperationErrorPolicy(c.Assoc, options.ErrorPolicy, newOperationError("C-FIND", err, true))
	}
	outcome.CancelSent = true
	for {
		responseCtx, responseCancel := operationResponseContext(drainCtx, options.ResponseTimeout)
		rsp, _, err := receiveCFindResponseWithContext(responseCtx, c.Assoc, c.PresentationCtxID, c.IdentifierSyntax)
		responseCancel()
		if err != nil {
			if drainExpired(drainCtx) && ctx.Err() == nil {
				err = fmt.Errorf("%w within %s: %w", ErrFindCancelNotAnswered, options.CancelDrainTimeout, err)
			}
			return outcome, applyOperationErrorPolicy(c.Assoc, options.ErrorPolicy, newOperationError("C-FIND", err, true))
		}
		if CategorizeCFindStatus(rsp.Status) == CFindStatusPending {
			outcome.Discarded++
			continue
		}
		outcome.Final = rsp
		return outcome, nil
	}
}

// drainExpired reports whether the drain deadline has passed. The read that
// failed carries the same deadline on its socket, which can fire before the
// context's own timer marks it done, so the clock is checked too.
func drainExpired(drainCtx context.Context) bool {
	if drainCtx.Err() != nil {
		return true
	}
	deadline, ok := drainCtx.Deadline()
	return ok && !time.Now().Before(deadline)
}
