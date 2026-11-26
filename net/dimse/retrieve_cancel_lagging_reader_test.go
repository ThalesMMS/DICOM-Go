package dimse

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/ThalesMMS/dicom-go/dictionary/std"
	"github.com/ThalesMMS/dicom-go/net/ul"
	"github.com/ThalesMMS/dicom-go/object"
	"github.com/ThalesMMS/dicom-go/transfer"
)

// providerSaw is what a test provider received from the requestor after its
// responses, until the association ended.
type providerSaw struct {
	cancel, release, abort bool
	err                    error // how the association ended when not by an A-ABORT
}

// watchRequestorEnd reads the requestor's PDUs on the provider side, answering
// nothing, until the association ends.
func watchRequestorEnd(assoc *ul.Association, pcID byte, messageID uint16) providerSaw {
	var saw providerSaw
	for {
		pdu, err := assoc.ReadPDU()
		var abort *ul.AbortError
		switch {
		case errors.As(err, &abort):
			saw.abort = true
			return saw
		case err != nil:
			saw.err = err
			return saw
		}
		switch pdu := pdu.(type) {
		case *ul.ReleaseRQ:
			saw.release = true
		case *ul.PDataTF:
			for _, value := range pdu.Values {
				if !value.IsCommand || value.PresentationContextID != pcID {
					continue
				}
				if command, err := DecodeCommandSet(value.Data); err == nil {
					if request, err := ParseCCancelRequest(command); err == nil && request.MessageIDBeingRespondedTo == messageID {
						saw.cancel = true
					}
				}
			}
		}
	}
}

// retrieveCancelMoment is when a lagging-reader test cancels the operation.
type retrieveCancelMoment int

const (
	// The requestor waits for the provider's next response; the one before
	// sits unread in the progress channel.
	cancelWhileWaitingForAResponse retrieveCancelMoment = iota
	// The requestor waits to hand over a pending response; the one before
	// sits unread in the progress channel.
	cancelWhileHandingOverAPendingResponse
	// The requestor waits to hand over the final response.
	cancelWhileHandingOverTheFinalResponse
)

// responsesFor is the status of each response the provider sends before it
// only reads, for a moment of the cancellation.
func responsesFor(moment retrieveCancelMoment) []uint16 {
	switch moment {
	case cancelWhileHandingOverAPendingResponse:
		return []uint16{StatusPending, StatusPending}
	case cancelWhileHandingOverTheFinalResponse:
		return []uint16{StatusPending, StatusSuccess}
	default:
		return []uint16{StatusPending}
	}
}

// handOverSignal makes the requestor report each time it starts to wait for
// its reader, until the test ends.
func handOverSignal(t *testing.T) <-chan struct{} {
	t.Helper()
	blocked := make(chan struct{}, 8)
	hook := func() {
		select {
		case blocked <- struct{}{}:
		default:
		}
	}
	testHookRetrieveHandOverBlocked.Store(&hook)
	t.Cleanup(func() { testHookRetrieveHandOverBlocked.Store(nil) })
	return blocked
}

// awaitCancelMoment returns once the requestor is at moment. The reader has
// not read anything.
func awaitCancelMoment[T any](t *testing.T, ctx context.Context, moment retrieveCancelMoment, progress <-chan T, blocked <-chan struct{}) {
	t.Helper()
	if moment == cancelWhileWaitingForAResponse {
		// The first response took the free slot; the requestor went back to
		// read the next one, which the provider never sends.
		for len(progress) == 0 {
			select {
			case <-ctx.Done():
				t.Fatal("the first response never reached the progress channel")
			case <-time.After(time.Millisecond):
			}
		}
		return
	}
	select {
	case <-blocked:
	case <-ctx.Done():
		t.Fatal("the requestor never waited to hand over a response")
	}
}

// awaitOperationEnd returns once the requestor's goroutine has ended, so the
// reader reads only after the requestor delivered its last event.
func awaitOperationEnd(t *testing.T, ctx context.Context, assoc *ul.Association) {
	t.Helper()
	for !assoc.TryBeginOperation() {
		select {
		case <-ctx.Done():
			t.Fatal("the retrieve did not end")
		case <-time.After(time.Millisecond):
		}
	}
	assoc.EndOperation()
}

// A C-MOVE canceled while its reader lags ends with the cancellation as its
// last event, after a C-CANCEL and an A-ABORT, whether the requestor was
// waiting for the next response or for the reader to take one: the channel
// never ends after a pending response the reader would take for the final
// one, answered by an A-RELEASE-RQ in the middle of the operation (#2004).
// The final response, once received, is handed over even when the
// cancellation comes meanwhile, with neither a C-CANCEL nor an A-ABORT.
func TestSendCMoveWithProgressCanceledWhileItsReaderLags(t *testing.T) {
	for _, tc := range []struct {
		name   string
		moment retrieveCancelMoment
	}{
		{"waiting-for-a-response", cancelWhileWaitingForAResponse},
		{"handing-over-a-pending-response", cancelWhileHandingOverAPendingResponse},
		{"handing-over-the-final-response", cancelWhileHandingOverTheFinalResponse},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()
			blocked := handOverSignal(t)
			listener, err := ul.Listen(ul.ListenOptions{Address: "127.0.0.1:0", Context: ctx})
			if err != nil {
				t.Fatalf("Listen() error = %v", err)
			}
			defer closeOrFail(t, "listener", listener)

			provider := make(chan providerSaw, 1)
			go func() {
				assoc, err := listener.AcceptAssociation(ul.AcceptOptions{
					AETitle: "MOVESCP", Context: ctx,
					SupportedAbstractSyntaxes: []string{StudyRootMoveSOPClassUID},
					SupportedTransferSyntaxes: []string{ul.ImplicitVRLittleEndian},
				})
				if err != nil {
					provider <- providerSaw{err: err}
					return
				}
				defer func() { _ = assoc.Close() }()
				pc, err := AcceptedContextForSOPClass(assoc, StudyRootMoveSOPClassUID)
				if err != nil {
					provider <- providerSaw{err: err}
					return
				}
				request, _, err := ReceiveCMoveRequestIdentifier(assoc, pc.ID, transfer.ImplicitVRLittleEndian)
				if err != nil {
					provider <- providerSaw{err: err}
					return
				}
				for index, status := range responsesFor(tc.moment) {
					completed, remaining := uint16(index+1), uint16(99-index)
					if status != StatusPending {
						remaining = 0
					}
					if err := SendCMoveResponse(assoc, pc.ID, CMoveResponse{
						AffectedSOPClassUID: StudyRootMoveSOPClassUID, MessageIDBeingRespondedTo: request.MessageID,
						Status: status, NumberOfRemainingSuboperationsOrNil: &remaining, NumberOfCompletedSuboperationsOrNil: &completed,
					}); err != nil {
						provider <- providerSaw{err: err}
						return
					}
				}
				provider <- watchRequestorEnd(assoc, pc.ID, request.MessageID)
			}()

			assoc, err := ul.DialContext(ctx, listener.Addr().String(), ul.DialOptions{
				CalledAETitle: "MOVESCP", CallingAETitle: "MOVESCU",
				Contexts: []ul.PresentationContext{StudyRootMovePresentationContext()},
			})
			if err != nil {
				t.Fatalf("DialContext() error = %v", err)
			}
			defer func() { _ = assoc.Close() }()
			pc, err := AcceptedContextForSOPClass(assoc, StudyRootMoveSOPClassUID)
			if err != nil {
				t.Fatal(err)
			}
			moveCtx, cancelMove := context.WithCancel(ctx)
			defer cancelMove()
			progress, err := SendCMoveWithProgress(moveCtx, assoc, pc.ID, CMoveRequest{
				AffectedSOPClassUID: StudyRootMoveSOPClassUID, MessageID: 9, MoveDestination: "DEST",
			}, object.FromElements(cGetIdentifierElements(t), std.Dictionary), transfer.ImplicitVRLittleEndian)
			if err != nil {
				t.Fatalf("SendCMoveWithProgress() error = %v", err)
			}
			awaitCancelMoment(t, ctx, tc.moment, progress, blocked)
			canceled := time.Now()
			cancelMove()
			awaitOperationEnd(t, ctx, assoc)
			if elapsed := time.Since(canceled); elapsed > 5*time.Second {
				t.Errorf("the canceled C-MOVE ended %s after its cancellation", elapsed.Round(time.Millisecond))
			}
			var events []CMoveProgress
			for event := range progress {
				events = append(events, event)
			}
			_ = assoc.Close()
			last := events[len(events)-1]
			var saw providerSaw
			select {
			case saw = <-provider:
			case <-ctx.Done():
				t.Fatal("the provider did not see the end of the association")
			}
			if tc.moment == cancelWhileHandingOverTheFinalResponse {
				if last.Err != nil || !last.Final || last.StatusClass != CMoveStatusSuccess {
					t.Fatalf("the last event = %+v, want the final Success response", last)
				}
				if saw.cancel || saw.abort {
					t.Errorf("the provider of a finished C-MOVE saw C-CANCEL=%t A-ABORT=%t", saw.cancel, saw.abort)
				}
				return
			}
			if last.Err == nil || !errors.Is(last.Err, context.Canceled) {
				t.Fatalf("the last of %d events = %+v, want the cancellation", len(events), last)
			}
			if saw.release || !saw.cancel || !saw.abort {
				t.Errorf("the provider saw A-RELEASE-RQ=%t C-CANCEL=%t A-ABORT=%t (end %v), want a C-CANCEL and an A-ABORT only", saw.release, saw.cancel, saw.abort, saw.err)
			}
		})
	}
}

// The C-GET counterpart: canceled while its reader lags, it ends with the
// cancellation after an A-ABORT; a final response already received is
// handed over (#2004).
func TestSendCGetWithProgressCanceledWhileItsReaderLags(t *testing.T) {
	for _, tc := range []struct {
		name   string
		moment retrieveCancelMoment
	}{
		{"waiting-for-a-response", cancelWhileWaitingForAResponse},
		{"handing-over-a-pending-response", cancelWhileHandingOverAPendingResponse},
		{"handing-over-the-final-response", cancelWhileHandingOverTheFinalResponse},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()
			blocked := handOverSignal(t)
			listener, err := ul.Listen(ul.ListenOptions{Address: "127.0.0.1:0", Context: ctx})
			if err != nil {
				t.Fatalf("Listen() error = %v", err)
			}
			defer closeOrFail(t, "listener", listener)

			provider := make(chan providerSaw, 1)
			go func() {
				assoc, err := listener.AcceptAssociation(ul.AcceptOptions{
					AETitle: "GETSCP", Context: ctx,
					SupportedAbstractSyntaxes: []string{StudyRootGetSOPClassUID, cGetTestStorageSOPClassUID},
					SupportedTransferSyntaxes: []string{ul.ImplicitVRLittleEndian},
					RoleSelections:            []ul.RoleSelectionItem{{SopClassUID: cGetTestStorageSOPClassUID, SCPRole: true}},
				})
				if err != nil {
					provider <- providerSaw{err: err}
					return
				}
				defer func() { _ = assoc.Close() }()
				pc, err := AcceptedContextForSOPClass(assoc, StudyRootGetSOPClassUID)
				if err != nil {
					provider <- providerSaw{err: err}
					return
				}
				request, _, err := ReceiveCGetRequest(assoc, pc.ID, transfer.ImplicitVRLittleEndian)
				if err != nil {
					provider <- providerSaw{err: err}
					return
				}
				for index, status := range responsesFor(tc.moment) {
					completed, remaining := uint16(index+1), uint16(99-index)
					if status != StatusPending {
						remaining = 0
					}
					if err := SendCGetResponse(assoc, pc.ID, CGetResponse{
						AffectedSOPClassUID: StudyRootGetSOPClassUID, MessageIDBeingRespondedTo: request.MessageID,
						Status: status, NumberOfRemainingSuboperationsOrNil: &remaining, NumberOfCompletedSuboperationsOrNil: &completed,
					}); err != nil {
						provider <- providerSaw{err: err}
						return
					}
				}
				provider <- watchRequestorEnd(assoc, pc.ID, request.MessageID)
			}()

			assoc, err := ul.DialContext(ctx, listener.Addr().String(), ul.DialOptions{
				CalledAETitle: "GETSCP", CallingAETitle: "GETSCU",
				Contexts: []ul.PresentationContext{
					StudyRootGetPresentationContext(),
					{AbstractSyntaxUID: cGetTestStorageSOPClassUID, TransferSyntaxUIDs: []string{ul.ImplicitVRLittleEndian}},
				},
				RoleSelections: []ul.RoleSelectionItem{{SopClassUID: cGetTestStorageSOPClassUID, SCPRole: true}},
			})
			if err != nil {
				t.Fatalf("DialContext() error = %v", err)
			}
			defer func() { _ = assoc.Close() }()
			pc, err := AcceptedContextForSOPClass(assoc, StudyRootGetSOPClassUID)
			if err != nil {
				t.Fatal(err)
			}
			getCtx, cancelGet := context.WithCancel(ctx)
			defer cancelGet()
			progress, err := SendCGetWithProgress(getCtx, assoc, pc.ID, CGetRequest{
				AffectedSOPClassUID: StudyRootGetSOPClassUID, MessageID: 9,
			}, object.FromElements(cGetIdentifierElements(t), std.Dictionary), transfer.ImplicitVRLittleEndian,
				CGetStoreHandlerFunc(func(context.Context, CGetStoreRequestContext) (uint16, error) { return StatusSuccess, nil }))
			if err != nil {
				t.Fatalf("SendCGetWithProgress() error = %v", err)
			}
			awaitCancelMoment(t, ctx, tc.moment, progress, blocked)
			canceled := time.Now()
			cancelGet()
			awaitOperationEnd(t, ctx, assoc)
			if elapsed := time.Since(canceled); elapsed > 5*time.Second {
				t.Errorf("the canceled C-GET ended %s after its cancellation", elapsed.Round(time.Millisecond))
			}
			var events []CGetProgress
			for event := range progress {
				events = append(events, event)
			}
			_ = assoc.Close()
			last := events[len(events)-1]
			var saw providerSaw
			select {
			case saw = <-provider:
			case <-ctx.Done():
				t.Fatal("the provider did not see the end of the association")
			}
			if tc.moment == cancelWhileHandingOverTheFinalResponse {
				if last.Err != nil || !last.Final || last.StatusClass != CMoveStatusSuccess {
					t.Fatalf("the last event = %+v, want the final Success response", last)
				}
				if saw.abort {
					t.Error("the provider of a finished C-GET saw an A-ABORT")
				}
				return
			}
			if last.Err == nil || !errors.Is(last.Err, context.Canceled) {
				t.Fatalf("the last of %d events = %+v, want the cancellation", len(events), last)
			}
			if saw.release || !saw.abort {
				t.Errorf("the provider saw A-RELEASE-RQ=%t A-ABORT=%t (end %v), want an A-ABORT only", saw.release, saw.abort, saw.err)
			}
		})
	}
}
