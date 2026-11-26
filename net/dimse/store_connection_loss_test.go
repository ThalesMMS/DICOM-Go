package dimse

import (
	"context"
	"errors"
	"fmt"
	"io"
	"syscall"
	"testing"
	"time"

	"github.com/ThalesMMS/dicom-go/internal/dicomtest"
	"github.com/ThalesMMS/dicom-go/net/ul"
	"github.com/ThalesMMS/dicom-go/transfer"
)

// An uncertain C-STORE keeps how the peer ended the association, an A-ABORT,
// a reset or a close, as a value-free cause next to ErrStoreUncertain.
func TestUncertainStoreErrorNamesTheLostConnection(t *testing.T) {
	for _, tc := range []struct {
		name string
		err  error
		want error
	}{
		{"A-ABORT", fmt.Errorf("read 10.0.0.7:104: %w", &ul.AbortError{Source: 2, Reason: 0}), ul.ErrAssociationAborted},
		{"reset", fmt.Errorf("write 10.0.0.7:104: %w", syscall.Errno(10054)), syscall.Errno(10054)},
		{"closed", fmt.Errorf("read PDU header: %w", io.EOF), io.ErrUnexpectedEOF},
	} {
		err := newStoreError("transfer", 1, uncertainStoreCause(tc.err), true)
		if !errors.Is(err, ErrStoreUncertain) || !errors.Is(err, tc.want) {
			t.Errorf("%s: error %v is not ErrStoreUncertain with %v", tc.name, err, tc.want)
		}
		if errors.Is(err, ErrStoreTimeout) {
			t.Errorf("%s: a lost connection is named as a timeout", tc.name)
		}
		if err.Error() != "dicom dimse: C-STORE batch transfer failed" {
			t.Errorf("%s: text = %q carries the transport error", tc.name, err.Error())
		}
	}
	var abort *ul.AbortError
	if err := newStoreError("transfer", 1, uncertainStoreCause(&ul.AbortError{Source: 2, Reason: 6}), true); !errors.As(err, &abort) || abort.Source != 2 || abort.Reason != 6 {
		t.Fatalf("the A-ABORT source and reason are lost: %v", err)
	}
	if err := newStoreError("transfer", 1, uncertainStoreCause(errors.New("unrelated")), true); valueFreeConnectionLoss(err) != nil {
		t.Fatalf("an unrelated error is named as a lost connection: %v", err)
	}
}

// The items a session gives up on after its associations kept being lost
// carry the cause of the last loss, without being uncertain themselves.
func TestReconnectFailureNamesTheLostAssociation(t *testing.T) {
	result := &StoreBatchResult{Items: []StoreItemResult{
		{SourceIndex: 0, Outcome: StoreOutcomeSuccess},
		{SourceIndex: 1, Outcome: StoreOutcomeUnknown, Err: newStoreError("transfer", 1, uncertainStoreCause(syscall.Errno(10054)), true)},
		{SourceIndex: 2, Outcome: StoreOutcomeUnknown, Err: newStoreError("transfer", 2, uncertainStoreCause(context.DeadlineExceeded), true)},
		{SourceIndex: 3, Outcome: StoreOutcomeNotSent},
	}}
	attempted := []StorePlannedItem{{SourceIndex: 0}, {SourceIndex: 1}, {SourceIndex: 2}}
	for _, tc := range []struct {
		name      string
		attempted []StorePlannedItem
		want      error
	}{
		{"timed out last", attempted, ErrStoreTimeout},
		{"reset last", attempted[:2], syscall.Errno(10054)},
	} {
		err := newStoreError("reconnect", 3, lostAssociationCause(result, tc.attempted), false)
		if !errors.Is(err, ErrStoreAssociation) || !errors.Is(err, tc.want) {
			t.Errorf("%s: error %v is not ErrStoreAssociation with %v", tc.name, err, tc.want)
		}
		if errors.Is(err, ErrStoreUncertain) {
			t.Errorf("%s: an item that was never sent is uncertain", tc.name)
		}
	}
	if err := newStoreError("reconnect", 3, lostAssociationCause(result, attempted[:1]), false); !errors.Is(err, ErrStoreAssociation) || valueFreeConnectionLoss(err) != nil || errors.Is(err, ErrStoreTimeout) {
		t.Fatalf("without an uncertain store the error is %v", err)
	}
}

// An uncertain store cut short by a local resource limit belongs to that item
// alone: the items given up after it were never transferred and fail as an
// association failure, not as a resource limit.
func TestReconnectFailureDoesNotInheritAResourceLimit(t *testing.T) {
	limited := newStoreError("transfer", 1, ErrStoreResourceLimit, true)
	result := &StoreBatchResult{Items: []StoreItemResult{
		{SourceIndex: 0, Outcome: StoreOutcomeUnknown, Err: newStoreError("transfer", 0, uncertainStoreCause(syscall.Errno(10054)), true)},
		{SourceIndex: 1, Outcome: StoreOutcomeUnknown, Err: limited},
		{SourceIndex: 2, Outcome: StoreOutcomeNotSent},
	}}
	if !errors.Is(limited, ErrStoreResourceLimit) {
		t.Fatalf("the limited item error = %v, want ErrStoreResourceLimit", limited)
	}
	err := newStoreError("reconnect", 2, lostAssociationCause(result, []StorePlannedItem{{SourceIndex: 0}, {SourceIndex: 1}}), false)
	if !errors.Is(err, ErrStoreAssociation) {
		t.Fatalf("error = %v, want ErrStoreAssociation", err)
	}
	if errors.Is(err, ErrStoreResourceLimit) || errors.Is(err, ErrStoreUncertain) {
		t.Fatalf("error = %v inherits the resource limit or the uncertainty of another item", err)
	}
	if lost := valueFreeConnectionLoss(err); lost != nil {
		t.Fatalf("error = %v names %v, which was not the last loss", err, lost)
	}
	var storeErr *StoreError
	if !errors.As(err, &storeErr) || storeErr.Stage != "reconnect" || storeErr.Uncertain {
		t.Fatalf("error = %#v, want a certain reconnect StoreError", err)
	}
}

// A peer that answers a C-STORE with an A-ABORT, or drops the connection,
// leaves an item whose error says so.
func TestStoreSessionNamesTheWayThePeerEndedAnUncertainStore(t *testing.T) {
	for _, tc := range []struct {
		name  string
		end   func(*ul.Association)
		named func(error) bool
	}{
		{"A-ABORT", func(assoc *ul.Association) { _ = assoc.Abort(0) }, func(err error) bool { return errors.Is(err, ul.ErrAssociationAborted) }},
		{"closed connection", func(assoc *ul.Association) { _ = assoc.Close() }, func(err error) bool {
			var errno syscall.Errno
			return errors.Is(err, io.ErrUnexpectedEOF) || errors.As(err, &errno)
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			fixture := readIntegrationFixture(t)
			descriptor, err := descriptorFromFile(fixture, 1, "", true)
			if err != nil {
				t.Fatalf("descriptorFromFile() error = %v", err)
			}
			source := &trackingStoreSource{descriptor: descriptor, dataSet: fixture.Dataset}
			listener, err := ul.Listen(ul.ListenOptions{Address: "127.0.0.1:0", Context: ctx})
			if err != nil {
				t.Fatalf("Listen() error = %v", err)
			}
			defer func() { _ = listener.Close() }()
			serverDone := make(chan error, 1)
			go func() {
				assoc, err := listener.AcceptAssociation(ul.AcceptOptions{
					AETitle: "STORESCP", Context: ctx,
					SupportedAbstractSyntaxes: []string{dicomtest.TestSOPClassUID},
					SupportedTransferSyntaxes: []string{transfer.ExplicitVRLittleEndian.UID},
				})
				if err != nil {
					serverDone <- err
					return
				}
				pcID, _, err := ReceiveCStoreRequestAny(assoc)
				if err == nil {
					_, err = ReceiveDataSet(assoc, pcID, transfer.ExplicitVRLittleEndian)
				}
				tc.end(assoc)
				serverDone <- err
			}()

			session, err := NewStoreSession(listener.Addr().String(), StoreSessionOptions{
				DialOptions:     ul.DialOptions{CalledAETitle: "STORESCP", CallingAETitle: "STORESCU"},
				ContinueOnError: true,
			})
			if err != nil {
				t.Fatalf("NewStoreSession() error = %v", err)
			}
			result, _ := session.StoreBatch(ctx, []StoreSource{source})
			if err := <-serverDone; err != nil {
				t.Fatalf("SCP error = %v", err)
			}
			if len(result.Items) != 1 || result.Items[0].Outcome != StoreOutcomeUnknown {
				t.Fatalf("items = %+v, want one unknown outcome", result.Items)
			}
			itemErr := result.Items[0].Err
			if !errors.Is(itemErr, ErrStoreUncertain) || !tc.named(itemErr) {
				t.Fatalf("item error = %v, want ErrStoreUncertain naming the %s", itemErr, tc.name)
			}
			if errors.Is(itemErr, ErrStoreTimeout) {
				t.Fatalf("item error = %v names a timeout", itemErr)
			}
		})
	}
}
