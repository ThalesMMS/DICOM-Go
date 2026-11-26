package dimse

import (
	"context"
	"errors"
	"fmt"
	"net"
	"os"
	"time"

	"github.com/ThalesMMS/dicom-go/net/ul"
	"github.com/ThalesMMS/dicom-go/object"
)

// storageCommitmentAwaitPoll bounds each idle read while a requestor waits for
// a same-association N-EVENT-REPORT, so the durable store is re-checked for a
// result delivered on a separate callback association.
const storageCommitmentAwaitPoll = 100 * time.Millisecond

// StorageCommitmentAwaitResult reports how AwaitResultOnAssociation ended.
type StorageCommitmentAwaitResult struct {
	// Transaction is the latest durable snapshot of the request.
	Transaction StorageCommitmentTransaction
	// PeerReleased reports that the committer released the association (the
	// A-RELEASE-RP has been sent); the caller must not release it again.
	PeerReleased bool
}

// AwaitResultOnAssociation lets a requestor keep the association that carried
// its N-ACTION open for at most wait and serve the committer's
// N-EVENT-REPORT on it (PS3.4 J.3.3: the committer may report on the original
// association while it is still open). It returns as soon as the transaction
// is terminal in the store, whether the result arrived here or on a separate
// callback association, when wait elapses, or when the committer releases.
//
// The association is borrowed and this call is its sole reader while active.
// Idle polling never consumes a partial PDU, so after a timeout the caller can
// still release the association normally. A command other than
// N-EVENT-REPORT is a protocol error. An expired wait is not an error: the
// returned transaction is simply not terminal yet.
func (w *StorageCommitmentWorkflow) AwaitResultOnAssociation(ctx context.Context, assoc *ul.Association, transactionUID string, wait time.Duration) (StorageCommitmentAwaitResult, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	if assoc == nil || wait < 0 {
		return StorageCommitmentAwaitResult{}, ErrStorageCommitmentInvalidRequest
	}
	transaction, err := w.storeGet(ctx, transactionUID)
	if err != nil {
		return StorageCommitmentAwaitResult{}, err
	}
	if transaction.Direction != StorageCommitmentDirectionRequestor {
		return StorageCommitmentAwaitResult{Transaction: transaction}, ErrStorageCommitmentTransactionConflict
	}
	releaseOperation, err := beginAssociationOperation(assoc)
	if err != nil {
		return StorageCommitmentAwaitResult{Transaction: transaction}, err
	}
	defer releaseOperation()
	deadline := time.Now().Add(wait)
	options := w.NormalizedOptions(assoc)
	for {
		if storageCommitmentTerminal(transaction.State) || !time.Now().Before(deadline) {
			return StorageCommitmentAwaitResult{Transaction: transaction}, nil
		}
		pcID, command, released, timedOut, err := awaitStorageCommitmentCommand(ctx, assoc, deadline)
		if err != nil {
			return StorageCommitmentAwaitResult{Transaction: transaction}, err
		}
		if released {
			latest, getErr := w.storeGet(ctx, transactionUID)
			if getErr == nil {
				transaction = latest
			}
			return StorageCommitmentAwaitResult{Transaction: transaction, PeerReleased: true}, getErr
		}
		if !timedOut {
			field, fieldErr := CommandUint16(command, CommandField)
			if fieldErr != nil {
				return StorageCommitmentAwaitResult{Transaction: transaction}, fieldErr
			}
			if field != NEventReportRQ {
				return StorageCommitmentAwaitResult{Transaction: transaction}, fmt.Errorf("dicom dimse: unexpected command field 0x%04X while awaiting Storage Commitment result", field)
			}
			if serveErr := ServeNormalizedCommand(ctx, assoc, pcID, command, options); serveErr != nil && !isHandledNormalizedSCPError(serveErr) {
				return StorageCommitmentAwaitResult{Transaction: transaction}, serveErr
			}
		}
		latest, err := w.storeGet(ctx, transactionUID)
		if err != nil {
			return StorageCommitmentAwaitResult{Transaction: transaction}, err
		}
		transaction = latest
	}
}

// awaitStorageCommitmentCommand polls for the first PDU of the next command
// with a short bound, then reads the complete command under ctx. Only an idle
// poll that consumed no complete PDU reports timedOut.
func awaitStorageCommitmentCommand(ctx context.Context, assoc *ul.Association, deadline time.Time) (byte, *object.Object, bool, bool, error) {
	values, err := takePDataCarryoverWithContext(ctx, assoc)
	if err != nil {
		return 0, nil, false, false, err
	}
	if len(values) == 0 {
		pollUntil := time.Now().Add(storageCommitmentAwaitPoll)
		if deadline.Before(pollUntil) {
			pollUntil = deadline
		}
		pollCtx, cancel := context.WithDeadline(ctx, pollUntil)
		pdu, receiveErr := assoc.Receive(pollCtx)
		// The connection deadline can fire just before the context timer, so
		// the poll bound is checked against the clock as well.
		pollExpired := pollCtx.Err() != nil || !time.Now().Before(pollUntil)
		cancel()
		if receiveErr != nil {
			if pollExpired && ctx.Err() == nil && isStorageCommitmentPollTimeout(receiveErr) {
				return 0, nil, false, true, nil
			}
			return 0, nil, false, false, receiveErr
		}
		switch pdu := pdu.(type) {
		case *ul.ReleaseRQ:
			return 0, nil, true, false, assoc.WritePDU(&ul.ReleaseRP{})
		case *ul.PDataTF:
			values = pdu.Values
		default:
			return 0, nil, false, false, ul.UnexpectedPDUError(pdu, "waiting for N-EVENT-REPORT or A-RELEASE-RQ")
		}
	}
	if err := storePDataCarryoverWithContext(ctx, assoc, values); err != nil {
		return 0, nil, false, false, err
	}
	pcID, command, released, err := receiveStorageCommandOrRelease(ul.WithActiveTransfer(ctx), assoc)
	return pcID, command, released, false, err
}

func isStorageCommitmentPollTimeout(err error) bool {
	if errors.Is(err, ul.ErrAssociationTimeout) || errors.Is(err, context.DeadlineExceeded) || errors.Is(err, os.ErrDeadlineExceeded) {
		return true
	}
	var netErr net.Error
	return errors.As(err, &netErr) && netErr.Timeout()
}
