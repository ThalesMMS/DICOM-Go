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

// A C-GET canceled while the requestor stores a sub-operation whose data set
// has arrived completes that sub-operation (#1994): the store handler runs to
// its end and the provider receives its real status, Success, instead of
// C000, before the cancellation ends the association with an A-ABORT.
func TestSendCGetWithProgressCompletesTheSubOperationInProgressWhenCanceled(t *testing.T) {
	testCGetCompletesTheSubOperationInProgress(t, false)
}

// The same when the operation's context is also the association's, as for a
// requestor whose association serves one operation, and its deadline passes
// while the object is stored: the response is still sent, though every
// write on the association would take that deadline (#2004).
func TestSendCGetWithProgressAnswersTheSubOperationInProgressWhenItsAssociationExpires(t *testing.T) {
	testCGetCompletesTheSubOperationInProgress(t, true)
}

func testCGetCompletesTheSubOperationInProgress(t *testing.T, associationDeadline bool) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	opCtx, cancelOp := context.WithCancel(ctx)
	if associationDeadline {
		opCtx, cancelOp = context.WithTimeout(ctx, time.Second)
	}
	defer cancelOp()

	listener, err := ul.Listen(ul.ListenOptions{Address: "127.0.0.1:0", Context: ctx})
	if err != nil {
		t.Fatalf("Listen() error = %v", err)
	}
	defer closeOrFail(t, "listener", listener)

	serverDone := make(chan error, 1)
	go func() {
		assoc, err := listener.AcceptAssociation(ul.AcceptOptions{
			AETitle:                   "GETSCP",
			Context:                   ctx,
			SupportedAbstractSyntaxes: []string{StudyRootGetSOPClassUID, cGetTestStorageSOPClassUID},
			SupportedTransferSyntaxes: []string{ul.ImplicitVRLittleEndian},
			RoleSelections: []ul.RoleSelectionItem{
				{SopClassUID: cGetTestStorageSOPClassUID, SCPRole: true},
			},
		})
		if err != nil {
			serverDone <- err
			return
		}
		defer func() { _ = assoc.Close() }()
		getPC, err := AcceptedContextForSOPClass(assoc, StudyRootGetSOPClassUID)
		if err != nil {
			serverDone <- err
			return
		}
		storePC, err := AcceptedContextForSOPClass(assoc, cGetTestStorageSOPClassUID)
		if err != nil {
			serverDone <- err
			return
		}
		if _, _, err := ReceiveCGetRequest(assoc, getPC.ID, transfer.ImplicitVRLittleEndian); err != nil {
			serverDone <- err
			return
		}
		if err := SendCStoreRequest(assoc, storePC.ID, CStoreRequest{
			AffectedSOPClassUID:    cGetTestStorageSOPClassUID,
			MessageID:              21,
			AffectedSOPInstanceUID: "1.2.3.4.5",
		}); err != nil {
			serverDone <- err
			return
		}
		if err := SendDataSet(assoc, storePC.ID, cGetStoredObject(), transfer.ImplicitVRLittleEndian); err != nil {
			serverDone <- err
			return
		}
		storeRsp, err := ReceiveCStoreResponse(assoc, storePC.ID)
		if err != nil {
			serverDone <- fmt.Errorf("the sub-operation in progress got no response: %w", err)
			return
		}
		if storeRsp.Status != StatusSuccess {
			serverDone <- fmt.Errorf("the sub-operation in progress was answered 0x%04X, want 0x0000", storeRsp.Status)
			return
		}
		_, err = assoc.ReadPDU()
		var abortErr *ul.AbortError
		if !errors.As(err, &abortErr) {
			serverDone <- fmt.Errorf("the association ended with %v, want an A-ABORT", err)
			return
		}
		serverDone <- nil
	}()

	assoc, err := ul.DialContext(ctx, listener.Addr().String(), ul.DialOptions{
		CalledAETitle:  "GETSCP",
		CallingAETitle: "GETSCU",
		Contexts: []ul.PresentationContext{
			StudyRootGetPresentationContext(),
			{AbstractSyntaxUID: cGetTestStorageSOPClassUID, TransferSyntaxUIDs: []string{ul.ImplicitVRLittleEndian}},
		},
		RoleSelections: []ul.RoleSelectionItem{
			{SopClassUID: cGetTestStorageSOPClassUID, SCPRole: true},
		},
	})
	if err != nil {
		t.Fatalf("DialContext() error = %v", err)
	}
	defer func() { _ = assoc.Close() }()
	if associationDeadline {
		assoc.Context = opCtx
	}
	getPC, err := AcceptedContextForSOPClass(assoc, StudyRootGetSOPClassUID)
	if err != nil {
		t.Fatalf("AcceptedContextForSOPClass() error = %v", err)
	}
	stored := false
	progress, err := SendCGetWithProgress(opCtx, assoc, getPC.ID, CGetRequest{
		AffectedSOPClassUID: StudyRootGetSOPClassUID,
		MessageID:           1,
	}, object.FromElements(cGetIdentifierElements(t), std.Dictionary), transfer.ImplicitVRLittleEndian, CGetStoreHandlerFunc(func(storeCtx context.Context, req CGetStoreRequestContext) (uint16, error) {
		// The operator cancels, or the deadline passes, while the object
		// is being stored. A handler that honors its context, as an
		// archive import does, would answer the failure of a canceled
		// store.
		if associationDeadline {
			<-opCtx.Done()
		} else {
			cancelOp()
		}
		if err := storeCtx.Err(); err != nil {
			return StatusCGetUnableToProcess, err
		}
		stored = req.DataSet != nil
		return StatusSuccess, nil
	}))
	if err != nil {
		t.Fatalf("SendCGetWithProgress() error = %v", err)
	}
	sawCanceled := false
	for event := range progress {
		if event.Err != nil && errors.Is(event.Err, opCtx.Err()) {
			sawCanceled = true
		}
	}
	if !sawCanceled {
		t.Fatalf("the canceled C-GET did not end with %v", opCtx.Err())
	}
	_ = assoc.Close()
	select {
	case err := <-serverDone:
		if err != nil {
			t.Errorf("provider: %v", err)
		}
	case <-ctx.Done():
		t.Error("the provider did not see the association end")
	}
	if !stored {
		t.Error("the sub-operation in progress was not stored")
	}
}

// A C-GET canceled while a sub-operation is arriving still ends with an
// A-ABORT. The cancellation interrupts the read of the C-STORE data set, and
// the provider must learn that the requestor left instead of seeing the
// connection close.
func TestSendCGetWithProgressAbortsWhenCanceledInsideASubOperation(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	opCtx, cancelOp := context.WithCancel(ctx)
	defer cancelOp()

	listener, err := ul.Listen(ul.ListenOptions{Address: "127.0.0.1:0", Context: ctx})
	if err != nil {
		t.Fatalf("Listen() error = %v", err)
	}
	defer closeOrFail(t, "listener", listener)

	commandSent := make(chan struct{})
	serverDone := make(chan error, 1)
	go func() {
		assoc, err := listener.AcceptAssociation(ul.AcceptOptions{
			AETitle:                   "GETSCP",
			Context:                   ctx,
			SupportedAbstractSyntaxes: []string{StudyRootGetSOPClassUID, cGetTestStorageSOPClassUID},
			SupportedTransferSyntaxes: []string{ul.ImplicitVRLittleEndian},
			RoleSelections: []ul.RoleSelectionItem{
				{SopClassUID: cGetTestStorageSOPClassUID, SCPRole: true},
			},
		})
		if err != nil {
			serverDone <- err
			return
		}
		defer func() { _ = assoc.Close() }()
		getPC, err := AcceptedContextForSOPClass(assoc, StudyRootGetSOPClassUID)
		if err != nil {
			serverDone <- err
			return
		}
		storePC, err := AcceptedContextForSOPClass(assoc, cGetTestStorageSOPClassUID)
		if err != nil {
			serverDone <- err
			return
		}
		if _, _, err := ReceiveCGetRequest(assoc, getPC.ID, transfer.ImplicitVRLittleEndian); err != nil {
			serverDone <- err
			return
		}
		// The command of a sub-operation whose data set never comes.
		if err := SendCStoreRequest(assoc, storePC.ID, CStoreRequest{
			AffectedSOPClassUID:    cGetTestStorageSOPClassUID,
			MessageID:              21,
			AffectedSOPInstanceUID: "1.2.3.4.5",
		}); err != nil {
			serverDone <- err
			return
		}
		close(commandSent)
		_, err = assoc.ReadPDU()
		var abortErr *ul.AbortError
		if !errors.As(err, &abortErr) {
			serverDone <- fmt.Errorf("the association ended with %v, want an A-ABORT", err)
			return
		}
		serverDone <- nil
	}()

	assoc, err := ul.DialContext(ctx, listener.Addr().String(), ul.DialOptions{
		CalledAETitle:  "GETSCP",
		CallingAETitle: "GETSCU",
		Contexts: []ul.PresentationContext{
			StudyRootGetPresentationContext(),
			{AbstractSyntaxUID: cGetTestStorageSOPClassUID, TransferSyntaxUIDs: []string{ul.ImplicitVRLittleEndian}},
		},
		RoleSelections: []ul.RoleSelectionItem{
			{SopClassUID: cGetTestStorageSOPClassUID, SCPRole: true},
		},
	})
	if err != nil {
		t.Fatalf("DialContext() error = %v", err)
	}
	defer func() { _ = assoc.Close() }()
	getPC, err := AcceptedContextForSOPClass(assoc, StudyRootGetSOPClassUID)
	if err != nil {
		t.Fatalf("AcceptedContextForSOPClass() error = %v", err)
	}
	progress, err := SendCGetWithProgress(opCtx, assoc, getPC.ID, CGetRequest{
		AffectedSOPClassUID: StudyRootGetSOPClassUID,
		MessageID:           1,
	}, object.FromElements(cGetIdentifierElements(t), std.Dictionary), transfer.ImplicitVRLittleEndian, CGetStoreHandlerFunc(func(context.Context, CGetStoreRequestContext) (uint16, error) {
		return StatusSuccess, nil
	}))
	if err != nil {
		t.Fatalf("SendCGetWithProgress() error = %v", err)
	}
	<-commandSent
	// The requestor is now inside the sub-operation, waiting for its data set.
	time.Sleep(100 * time.Millisecond)
	cancelOp()
	sawCanceled := false
	for event := range progress {
		if event.Err != nil && errors.Is(event.Err, context.Canceled) {
			sawCanceled = true
		}
	}
	if !sawCanceled {
		t.Fatal("the canceled C-GET did not end with context.Canceled")
	}
	// The caller closes the association after the final event, as the
	// application does.
	_ = assoc.Close()
	select {
	case err := <-serverDone:
		if err != nil {
			t.Fatalf("provider: %v", err)
		}
	case <-ctx.Done():
		t.Fatal("the provider did not see the association end")
	}
}
