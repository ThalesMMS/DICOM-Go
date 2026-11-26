package dimse

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/ThalesMMS/dicom-go/net/ul"
)

// TestStorageCommitmentAwaitResultOnAssociation covers a requestor that keeps
// its N-ACTION association open: the committer either reports on it without
// releasing (the requestor then releases) or never reports, in which case the
// bounded wait expires and the association is still released cleanly.
func TestStorageCommitmentAwaitResultOnAssociation(t *testing.T) {
	cases := []struct {
		name         string
		deliver      bool
		failed       bool
		wantState    StorageCommitmentState
		wantFailures int
	}{
		{name: "same association success", deliver: true, wantState: StorageCommitmentStateResultReceived},
		{name: "same association partial failure", deliver: true, failed: true, wantState: StorageCommitmentStateResultReceived, wantFailures: 1},
		{name: "no report before wait expires", wantState: StorageCommitmentStateAccepted},
	}
	for i, tc := range cases {
		tc := tc
		transactionUID := "2.25.7701" + string(rune('0'+i))
		t.Run(tc.name, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()
			listener, err := ul.Listen(ul.ListenOptions{Address: "127.0.0.1:0", Context: ctx})
			if err != nil {
				t.Fatal(err)
			}
			defer listener.Close()
			committer := newStorageCommitmentTestWorkflow(t, StorageCommitmentWorkflowOptions{DefaultDeliveryMode: StorageCommitmentDeliverySameAssociation})
			serverDone := make(chan error, 1)
			go func() {
				assoc, err := listener.AcceptAssociation(ul.AcceptOptions{
					AETitle: "COMMITTER", Context: ctx,
					SupportedAbstractSyntaxes: []string{StorageCommitmentPushModelSOPClassUID},
					SupportedTransferSyntaxes: []string{ul.ExplicitVRLittleEndian, ul.ImplicitVRLittleEndian},
				})
				if err != nil {
					serverDone <- err
					return
				}
				defer assoc.Close()
				transaction, err := committer.ServeAction(ctx, assoc, 1)
				if err == nil && tc.deliver {
					result := StorageCommitmentResult{TransactionUID: transaction.TransactionUID, ReferencedSOPs: transaction.ReferencedSOPs}
					if tc.failed {
						failed := transaction.ReferencedSOPs[1]
						failed.FailureReason = StorageCommitmentFailureReasonNoSuchSOPInstance
						result = StorageCommitmentResult{TransactionUID: transaction.TransactionUID, ReferencedSOPs: transaction.ReferencedSOPs[:1], FailedSOPs: []StorageCommitmentSOPReference{failed}}
					}
					if _, err = committer.SetResult(ctx, result); err == nil {
						_, err = committer.DeliverOnAssociation(ctx, transaction.TransactionUID, assoc)
					}
				}
				if err == nil {
					// The committer keeps the association open; the requestor releases.
					err = ServeAssociation(ctx, assoc, AssociationSCPOptions{})
				}
				serverDone <- err
			}()
			assoc, err := ul.DialContext(ctx, listener.Addr().String(), ul.DialOptions{
				CalledAETitle: "COMMITTER", CallingAETitle: "REQUESTOR",
				Contexts: []ul.PresentationContext{{AbstractSyntaxUID: StorageCommitmentPushModelSOPClassUID, TransferSyntaxUIDs: []string{ul.ExplicitVRLittleEndian}}},
			})
			if err != nil {
				t.Fatal(err)
			}
			defer assoc.Close()
			requestStore := newStorageCommitmentTestStore(t)
			requestor := newStorageCommitmentTestWorkflow(t, StorageCommitmentWorkflowOptions{Store: requestStore})
			transaction, err := requestor.Request(ctx, assoc, storageCommitmentTestReferences, StorageCommitmentRequestOptions{TransactionUID: transactionUID})
			if err != nil {
				t.Fatalf("Request() error = %v", err)
			}
			wait := 3 * time.Second
			if !tc.deliver {
				wait = 250 * time.Millisecond
			}
			started := time.Now()
			awaited, err := requestor.AwaitResultOnAssociation(ctx, assoc, transaction.TransactionUID, wait)
			if err != nil {
				t.Fatalf("AwaitResultOnAssociation() error = %v", err)
			}
			if awaited.PeerReleased {
				t.Fatal("committer unexpectedly released the association")
			}
			if tc.deliver && time.Since(started) >= wait {
				t.Fatalf("await did not return when the result arrived (took %s)", time.Since(started))
			}
			if awaited.Transaction.State != tc.wantState {
				t.Fatalf("state = %q, want %q", awaited.Transaction.State, tc.wantState)
			}
			if tc.deliver && len(awaited.Transaction.Result.FailedSOPs) != tc.wantFailures {
				t.Fatalf("failed SOPs = %d, want %d", len(awaited.Transaction.Result.FailedSOPs), tc.wantFailures)
			}
			if err := assoc.Release(ctx); err != nil {
				t.Fatalf("Release() after await error = %v", err)
			}
			if err := <-serverDone; err != nil {
				t.Fatalf("committer error = %v", err)
			}
		})
	}
}

func TestStorageCommitmentAwaitResultOnAssociationReportsPeerRelease(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	listener, err := ul.Listen(ul.ListenOptions{Address: "127.0.0.1:0", Context: ctx})
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	committer := newStorageCommitmentTestWorkflow(t, StorageCommitmentWorkflowOptions{})
	serverDone := make(chan error, 1)
	go func() {
		assoc, err := listener.AcceptAssociation(ul.AcceptOptions{
			AETitle: "COMMITTER", Context: ctx,
			SupportedAbstractSyntaxes: []string{StorageCommitmentPushModelSOPClassUID},
			SupportedTransferSyntaxes: []string{ul.ExplicitVRLittleEndian},
		})
		if err != nil {
			serverDone <- err
			return
		}
		defer assoc.Close()
		if _, err = committer.ServeAction(ctx, assoc, 1); err == nil {
			err = assoc.Release(ctx)
		}
		serverDone <- err
	}()
	assoc, err := ul.DialContext(ctx, listener.Addr().String(), ul.DialOptions{
		CalledAETitle: "COMMITTER", CallingAETitle: "REQUESTOR",
		Contexts: []ul.PresentationContext{{AbstractSyntaxUID: StorageCommitmentPushModelSOPClassUID, TransferSyntaxUIDs: []string{ul.ExplicitVRLittleEndian}}},
	})
	if err != nil {
		t.Fatal(err)
	}
	defer assoc.Close()
	requestor := newStorageCommitmentTestWorkflow(t, StorageCommitmentWorkflowOptions{})
	transaction, err := requestor.Request(ctx, assoc, storageCommitmentTestReferences, StorageCommitmentRequestOptions{TransactionUID: "2.25.7709"})
	if err != nil {
		t.Fatal(err)
	}
	awaited, err := requestor.AwaitResultOnAssociation(ctx, assoc, transaction.TransactionUID, 3*time.Second)
	if err != nil {
		t.Fatalf("AwaitResultOnAssociation() error = %v", err)
	}
	if !awaited.PeerReleased || awaited.Transaction.State != StorageCommitmentStateAccepted {
		t.Fatalf("await = %+v", awaited)
	}
	if err := <-serverDone; err != nil {
		t.Fatalf("committer error = %v", err)
	}
	if _, err := requestor.AwaitResultOnAssociation(ctx, nil, transaction.TransactionUID, time.Second); !errors.Is(err, ErrStorageCommitmentInvalidRequest) {
		t.Fatalf("nil association error = %v", err)
	}
}
