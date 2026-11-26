package dimse

import (
	"context"
	"errors"
	"io"
	"net"
	"testing"
	"time"

	"github.com/ThalesMMS/dicom-go/net/ul"
)

// TestStorageCommitmentServeListenerSurvivesInvalidNegotiation sends a
// connection that never negotiates (bytes that are not an A-ASSOCIATE-RQ) to
// the callback receiver and then delivers a result on a valid callback
// association to the same receiver.
func TestStorageCommitmentServeListenerSurvivesInvalidNegotiation(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	listener, err := ul.Listen(ul.ListenOptions{Address: "127.0.0.1:0", Context: ctx})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = listener.Close() }()

	transactionUID := "2.25.2020"
	requestorStore := newStorageCommitmentTestStore(t)
	requestTransaction := StorageCommitmentTransaction{
		TransactionUID: transactionUID, Direction: StorageCommitmentDirectionRequestor,
		State: StorageCommitmentStateAccepted, DeliveryMode: StorageCommitmentDeliveryCallback,
		RequestorAETitle: "REQUESTOR", CommitterAETitle: "COMMITTER",
		ReferencedSOPs: append([]StorageCommitmentSOPReference(nil), storageCommitmentTestReferences...),
		CreatedAt:      time.Now(), UpdatedAt: time.Now(),
	}
	requestTransaction.RequestDigest = storageCommitmentRequestDigest(requestTransaction)
	if created, err := requestorStore.Create(ctx, requestTransaction); err != nil || !created {
		t.Fatalf("requestor Create() = %v, %v", created, err)
	}
	requestor := newStorageCommitmentTestWorkflow(t, StorageCommitmentWorkflowOptions{Store: requestorStore})

	reported := make(chan error, 8)
	serveCtx, stopServing := context.WithCancel(ctx)
	defer stopServing()
	serveDone := make(chan error, 1)
	go func() {
		serveDone <- requestor.ServeListener(serveCtx, listener, StorageCommitmentListenerOptions{
			Accept: ul.AcceptOptions{AETitle: "REQUESTOR", RequireMatchingCalledAE: true},
			OnError: func(err error) {
				select {
				case reported <- err:
				default:
				}
			},
		})
	}()

	garbage, err := (&net.Dialer{}).DialContext(ctx, "tcp", listener.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	deadline, _ := ctx.Deadline()
	_ = garbage.SetDeadline(deadline)
	if _, err := garbage.Write([]byte("GET / HTTP/1.1\r\nHost: dicom\r\n\r\n")); err != nil {
		t.Fatal(err)
	}
	// The receiver answers with an A-P-ABORT and half-closes; once this side
	// closes too, the negotiation failure is reported.
	_, _ = io.Copy(io.Discard, garbage)
	_ = garbage.Close()
	select {
	case err := <-reported:
		var deliveryErr *StorageCommitmentDeliveryError
		if !errors.As(err, &deliveryErr) || deliveryErr.Class != StorageCommitmentFailureProtocol {
			t.Fatalf("reported error = %v, want a protocol StorageCommitmentDeliveryError", err)
		}
	case err := <-serveDone:
		t.Fatalf("ServeListener() returned %v after one invalid negotiation", err)
	case <-ctx.Done():
		t.Fatal("invalid negotiation was not reported")
	}

	committerStore := newStorageCommitmentTestStore(t)
	committer := newStorageCommitmentTestWorkflow(t, StorageCommitmentWorkflowOptions{
		Store: committerStore,
		Resolver: StorageCommitmentCallbackResolverFunc(func(context.Context, StorageCommitmentCallbackRequest) (StorageCommitmentCallbackTarget, error) {
			return StorageCommitmentCallbackTarget{
				Address:     listener.Addr().String(),
				DialOptions: ul.DialOptions{CallingAETitle: "COMMITTER", CalledAETitle: "REQUESTOR"},
			}, nil
		}),
	})
	commitTransaction := acceptStorageCommitmentTestRequest(t, committer, transactionUID)
	if _, err := committer.SetResult(ctx, StorageCommitmentResult{
		TransactionUID: transactionUID,
		ReferencedSOPs: append([]StorageCommitmentSOPReference(nil), storageCommitmentTestReferences...),
	}); err != nil {
		t.Fatal(err)
	}
	delivered, err := committer.DeliverCallback(ctx, commitTransaction.TransactionUID)
	if err != nil || delivered.State != StorageCommitmentStateDelivered {
		t.Fatalf("DeliverCallback() after an invalid negotiation = %#v, %v", delivered, err)
	}
	received, err := requestor.Wait(ctx, transactionUID, 5*time.Millisecond)
	if err != nil || received.State != StorageCommitmentStateResultReceived || received.Result == nil || len(received.Result.ReferencedSOPs) != len(storageCommitmentTestReferences) {
		t.Fatalf("received = %#v, %v", received, err)
	}

	stopServing()
	if err := <-serveDone; !errors.Is(err, context.Canceled) {
		t.Fatalf("ServeListener() error = %v, want context.Canceled", err)
	}
	select {
	case err := <-reported:
		t.Fatalf("unexpected reported error after the valid association: %v", err)
	default:
	}
}

// TestStorageCommitmentServeListenerRejectsNegativeTimeouts keeps the
// options check that used to happen before the first accept.
func TestStorageCommitmentServeListenerRejectsNegativeTimeouts(t *testing.T) {
	listener, err := ul.Listen(ul.ListenOptions{Address: "127.0.0.1:0"})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = listener.Close() }()
	workflow := newStorageCommitmentTestWorkflow(t, StorageCommitmentWorkflowOptions{Store: newStorageCommitmentTestStore(t)})
	err = workflow.ServeListener(context.Background(), listener, StorageCommitmentListenerOptions{
		Accept: ul.AcceptOptions{NegotiationTimeout: -time.Second},
	})
	if !errors.Is(err, ErrStorageCommitmentInvalidRequest) {
		t.Fatalf("ServeListener() error = %v, want ErrStorageCommitmentInvalidRequest", err)
	}
}
