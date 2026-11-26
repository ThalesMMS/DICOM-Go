package dimse

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/ThalesMMS/dicom-go/dictionary/std"
	"github.com/ThalesMMS/dicom-go/net/ul"
	"github.com/ThalesMMS/dicom-go/object"
	"github.com/ThalesMMS/dicom-go/transfer"
)

// A C-MOVE whose context is canceled tells the provider to stop: the
// sub-operations run on other associations and would otherwise go on.
func TestSendCMoveWithProgressSendsCCancelWhenItsContextEnds(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	listener, err := ul.Listen(ul.ListenOptions{Address: "127.0.0.1:0", Context: ctx})
	if err != nil {
		t.Fatalf("Listen() error = %v", err)
	}
	defer closeOrFail(t, "listener", listener)

	pendingSent := make(chan struct{})
	serverDone := make(chan error, 1)
	go func() {
		assoc, err := listener.AcceptAssociation(ul.AcceptOptions{
			AETitle:                   "MOVESCP",
			Context:                   ctx,
			SupportedAbstractSyntaxes: []string{StudyRootMoveSOPClassUID},
			SupportedTransferSyntaxes: []string{ul.ImplicitVRLittleEndian},
		})
		if err != nil {
			serverDone <- err
			return
		}
		defer func() { _ = assoc.Close() }()
		pc, err := AcceptedContextForSOPClass(assoc, StudyRootMoveSOPClassUID)
		if err != nil {
			serverDone <- err
			return
		}
		request, _, err := ReceiveCMoveRequestIdentifier(assoc, pc.ID, transfer.ImplicitVRLittleEndian)
		if err != nil {
			serverDone <- err
			return
		}
		remaining := uint16(10)
		if err := SendCMoveResponse(assoc, pc.ID, CMoveResponse{
			AffectedSOPClassUID: StudyRootMoveSOPClassUID, MessageIDBeingRespondedTo: request.MessageID,
			Status: StatusPending, NumberOfRemainingSuboperationsOrNil: &remaining,
		}); err != nil {
			serverDone <- err
			return
		}
		close(pendingSent)
		command, err := ReceiveCommandSet(assoc, pc.ID)
		if err != nil {
			serverDone <- fmt.Errorf("no command after the pending response: %w", err)
			return
		}
		cancelRequest, err := ParseCCancelRequest(command)
		if err != nil {
			serverDone <- err
			return
		}
		if cancelRequest.MessageIDBeingRespondedTo != request.MessageID {
			serverDone <- fmt.Errorf("C-CANCEL-RQ names message %d, want %d", cancelRequest.MessageIDBeingRespondedTo, request.MessageID)
			return
		}
		serverDone <- nil
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
		AffectedSOPClassUID: StudyRootMoveSOPClassUID, MessageID: 7, MoveDestination: "DEST",
	}, object.FromElements(cGetIdentifierElements(t), std.Dictionary), transfer.ImplicitVRLittleEndian)
	if err != nil {
		t.Fatalf("SendCMoveWithProgress() error = %v", err)
	}
	<-pendingSent
	sawCanceled := false
	for event := range progress {
		if event.Response != nil && !event.Final {
			cancelMove()
		}
		if event.Err != nil && errors.Is(event.Err, context.Canceled) {
			sawCanceled = true
		}
	}
	if !sawCanceled {
		t.Fatal("the canceled C-MOVE did not end with context.Canceled")
	}
	select {
	case err := <-serverDone:
		if err != nil {
			t.Fatalf("provider: %v", err)
		}
	case <-ctx.Done():
		t.Fatal("the provider did not receive a C-CANCEL-RQ")
	}
}
