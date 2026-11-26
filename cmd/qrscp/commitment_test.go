package main

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/ThalesMMS/dicom-go/internal/dimsetest"
	"github.com/ThalesMMS/dicom-go/net/dimse"
	"github.com/ThalesMMS/dicom-go/net/ul"
)

type commitmentPeer struct {
	workflow *dimse.StorageCommitmentWorkflow
	store    *dimse.MemoryStorageCommitmentStore
	address  string
}

func startCommitmentPeer(t *testing.T, ctx context.Context) commitmentPeer {
	t.Helper()
	store, err := dimse.NewMemoryStorageCommitmentStore(32)
	if err != nil {
		t.Fatal(err)
	}
	workflow, err := dimse.NewStorageCommitmentWorkflow(dimse.StorageCommitmentWorkflowOptions{Store: store})
	if err != nil {
		t.Fatal(err)
	}
	listener, err := ul.Listen(ul.ListenOptions{Address: "127.0.0.1:0", Context: ctx})
	if err != nil {
		t.Fatal(err)
	}
	serveCtx, stop := context.WithCancel(ctx)
	done := make(chan struct{})
	go func() {
		defer close(done)
		_ = workflow.ServeListener(serveCtx, listener, dimse.StorageCommitmentListenerOptions{Accept: ul.AcceptOptions{AETitle: "COMMITSCU", RequireMatchingCalledAE: true, SupportedAbstractSyntaxes: []string{dimse.StorageCommitmentPushModelSOPClassUID}, SupportedTransferSyntaxes: []string{ul.ImplicitVRLittleEndian, ul.ExplicitVRLittleEndian}, RoleSelections: []ul.RoleSelectionItem{{SopClassUID: dimse.StorageCommitmentPushModelSOPClassUID, SCPRole: true}}}})
	}()
	t.Cleanup(func() {
		stop()
		listener.Close()
		select {
		case <-done:
		case <-time.After(10 * time.Second):
			t.Error("commitment callback shutdown timeout")
		}
	})
	return commitmentPeer{workflow, store, listener.Addr().String()}
}
func commitmentRequest(t *testing.T, ctx context.Context, workflow *dimse.StorageCommitmentWorkflow, address, uid string, refs []dimse.StorageCommitmentSOPReference) *ul.Association {
	t.Helper()
	assoc, err := ul.DialContext(ctx, address, ul.DialOptions{CalledAETitle: "QRSCP", CallingAETitle: "COMMITSCU", Contexts: []ul.PresentationContext{{AbstractSyntaxUID: dimse.StorageCommitmentPushModelSOPClassUID, TransferSyntaxUIDs: []string{ul.ImplicitVRLittleEndian}}}})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { assoc.Close() })
	if _, err := workflow.Request(ctx, assoc, refs, dimse.StorageCommitmentRequestOptions{TransactionUID: uid}); err != nil {
		t.Fatal(err)
	}
	return assoc
}

func TestWritableCommitmentCallbackAndPersistedResult(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	peer := startCommitmentPeer(t, ctx)
	opts := writableTestOptions(t, t.TempDir(), "-storage-commitment", "-commitment-callback", "COMMITSCU="+peer.address)
	address, stop := startWritableQRSCP(t, opts)
	uid := seriesA + ".301"
	if status := networkStore(t, address, uid); status != dimse.StatusSuccess {
		t.Fatal(status)
	}
	transactionUID := "2.25.1357301"
	refs := []dimse.StorageCommitmentSOPReference{{SOPClassUID: dimsetest.SecondaryCaptureSOPClassUID, SOPInstanceUID: uid}, {SOPClassUID: dimsetest.SecondaryCaptureSOPClassUID, SOPInstanceUID: "2.25.404"}}
	assoc := commitmentRequest(t, ctx, peer.workflow, address, transactionUID, refs)
	assoc.Release(ctx)
	transaction, err := peer.workflow.Wait(ctx, transactionUID, 10*time.Millisecond)
	if err != nil || transaction.Result == nil || len(transaction.Result.ReferencedSOPs) != 1 || len(transaction.Result.FailedSOPs) != 1 || transaction.Result.FailedSOPs[0].FailureReason != dimse.StorageCommitmentFailureReasonNoSuchSOPInstance {
		t.Fatalf("callback %+v %v", transaction, err)
	}
	unknown, err := ul.DialContext(ctx, address, ul.DialOptions{CalledAETitle: "QRSCP", CallingAETitle: "UNCONFIGURED", Contexts: []ul.PresentationContext{{AbstractSyntaxUID: dimse.StorageCommitmentPushModelSOPClassUID, TransferSyntaxUIDs: []string{ul.ImplicitVRLittleEndian}}}})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := peer.workflow.Request(ctx, unknown, refs, dimse.StorageCommitmentRequestOptions{TransactionUID: "2.25.1357302"}); err == nil {
		t.Error("N-ACTION without callback route admitted")
	}
	unknown.Close()
	stop()
	store, err := dimse.NewFileStorageCommitmentStore(filepath.Join(opts.writable.root, ".qrscp-commitment"), dimse.FileStorageCommitmentStoreOptions{})
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	persisted, err := store.Get(ctx, transactionUID)
	if err != nil || persisted.Result == nil || len(persisted.Result.ReferencedSOPs) != 1 || persisted.Version < 4 {
		t.Fatalf("persisted %+v %v", persisted, err)
	}
}

func TestWritableCommitmentFailureReasons(t *testing.T) {
	ctx := context.Background()
	opts := writableTestOptions(t, t.TempDir(), "-storage-commitment", "-stgcmt-same-assoc")
	a := openArchiveTest(t, opts)
	uid := seriesA + ".331"
	if _, err := a.Store(ctx, storeRequest(writableData(t, uid)), "QRSCP", "TESTSCU"); err != nil {
		t.Fatal(err)
	}
	c, err := newArchiveCommitment(a, "QRSCP", opts.writable)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	for _, test := range []struct {
		class, uid string
		want       uint16
	}{{"2.25.999", uid, dimse.StorageCommitmentFailureReasonSOPClassNotSupported}, {dimsetest.SecondaryCaptureSOPClassUID, "2.25.404", dimse.StorageCommitmentFailureReasonNoSuchSOPInstance}, {"1.2.840.10008.5.1.4.1.1.2", uid, dimse.StorageCommitmentFailureReasonClassInstanceConflict}} {
		reason, err := c.verify(ctx, dimse.StorageCommitmentSOPReference{SOPClassUID: test.class, SOPInstanceUID: test.uid})
		if err != nil || reason != test.want {
			t.Fatalf("partition reason %04X want%04X %v", reason, test.want, err)
		}
	}
	if err := os.Remove(filepath.Join(opts.writable.root, uid+".dcm")); err != nil {
		t.Fatal(err)
	}
	reason, err := c.verify(ctx, dimse.StorageCommitmentSOPReference{SOPClassUID: dimsetest.SecondaryCaptureSOPClassUID, SOPInstanceUID: uid})
	if err != nil || reason != dimse.StorageCommitmentFailureReasonNoSuchSOPInstance {
		t.Fatalf("missing stored file %04X %v", reason, err)
	}
}

func TestWritableCommitmentSameAssociationDedicated(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	opts := writableTestOptions(t, t.TempDir(), "-storage-commitment", "-stgcmt-same-assoc")
	address, _ := startWritableQRSCP(t, opts)
	uid := seriesA + ".311"
	if status := networkStore(t, address, uid); status != dimse.StatusSuccess {
		t.Fatal(status)
	}
	store, err := dimse.NewMemoryStorageCommitmentStore(16)
	if err != nil {
		t.Fatal(err)
	}
	workflow, err := dimse.NewStorageCommitmentWorkflow(dimse.StorageCommitmentWorkflowOptions{Store: store, DefaultDeliveryMode: dimse.StorageCommitmentDeliverySameAssociation})
	if err != nil {
		t.Fatal(err)
	}
	transactionUID := "2.25.1357311"
	assoc := commitmentRequest(t, ctx, workflow, address, transactionUID, []dimse.StorageCommitmentSOPReference{{SOPClassUID: dimsetest.SecondaryCaptureSOPClassUID, SOPInstanceUID: uid}})
	if err := workflow.ServeAssociation(ctx, assoc, dimse.SCPControls{}); err != nil {
		t.Fatal(err)
	}
	transaction, err := store.Get(ctx, transactionUID)
	if err != nil || transaction.Result == nil || len(transaction.Result.ReferencedSOPs) != 1 {
		t.Fatalf("same association %+v %v", transaction, err)
	}
	mixed, err := ul.DialContext(ctx, address, ul.DialOptions{CalledAETitle: "QRSCP", CallingAETitle: "COMMITSCU", Contexts: []ul.PresentationContext{{AbstractSyntaxUID: dimse.StorageCommitmentPushModelSOPClassUID, TransferSyntaxUIDs: []string{ul.ImplicitVRLittleEndian}}, {AbstractSyntaxUID: dimse.VerificationSOPClassUID, TransferSyntaxUIDs: []string{ul.ImplicitVRLittleEndian}}}})
	if err != nil {
		t.Fatal(err)
	}
	defer mixed.Close()
	if _, err := workflow.Request(ctx, mixed, []dimse.StorageCommitmentSOPReference{{SOPClassUID: dimsetest.SecondaryCaptureSOPClassUID, SOPInstanceUID: uid}}, dimse.StorageCommitmentRequestOptions{TransactionUID: "2.25.1357312"}); err == nil {
		t.Fatal("mixed same-association profile admitted")
	}
	duplicate, err := ul.DialContext(ctx, address, ul.DialOptions{CalledAETitle: "QRSCP", CallingAETitle: "COMMITSCU", Contexts: []ul.PresentationContext{{AbstractSyntaxUID: dimse.StorageCommitmentPushModelSOPClassUID, TransferSyntaxUIDs: []string{ul.ImplicitVRLittleEndian}}, {AbstractSyntaxUID: dimse.StorageCommitmentPushModelSOPClassUID, TransferSyntaxUIDs: []string{ul.ExplicitVRLittleEndian}}}})
	if err != nil {
		t.Fatal(err)
	}
	defer duplicate.Close()
	if _, err := workflow.Request(ctx, duplicate, []dimse.StorageCommitmentSOPReference{{SOPClassUID: dimsetest.SecondaryCaptureSOPClassUID, SOPInstanceUID: uid}}, dimse.StorageCommitmentRequestOptions{TransactionUID: "2.25.1357313"}); err == nil {
		t.Fatal("multiple accepted same-association contexts admitted")
	}
}

func TestWritableCommitmentRestartAcceptedAndDigestFailure(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	peer := startCommitmentPeer(t, ctx)
	opts := writableTestOptions(t, t.TempDir(), "-storage-commitment", "-commitment-callback", "COMMITSCU="+peer.address)
	a := openArchiveTest(t, opts)
	uid := seriesA + ".321"
	if _, err := a.Store(ctx, storeRequest(writableData(t, uid)), "QRSCP", "TESTSCU"); err != nil {
		t.Fatal(err)
	}
	c, err := newArchiveCommitment(a, "QRSCP", opts.writable)
	if err != nil {
		t.Fatal(err)
	}
	listener, err := ul.Listen(ul.ListenOptions{Address: "127.0.0.1:0", Context: ctx})
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	done := make(chan error, 1)
	go func() {
		assoc, err := listener.AcceptAssociation(ul.AcceptOptions{AETitle: "QRSCP", Context: ctx, SupportedAbstractSyntaxes: []string{dimse.StorageCommitmentPushModelSOPClassUID}, SupportedTransferSyntaxes: []string{ul.ImplicitVRLittleEndian}})
		if err == nil {
			defer assoc.Close()
			_, err = c.workflow.ServeAction(ctx, assoc, assoc.AcceptedContexts[0].ID)
		}
		done <- err
	}()
	transactionUID := "2.25.1357321"
	assoc := commitmentRequest(t, ctx, peer.workflow, listener.Addr().String(), transactionUID, []dimse.StorageCommitmentSOPReference{{SOPClassUID: dimsetest.SecondaryCaptureSOPClassUID, SOPInstanceUID: uid}})
	assoc.Close()
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	c.Close()
	a.Close()
	a = openArchiveTest(t, opts)
	c, err = newArchiveCommitment(a, "QRSCP", opts.writable)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	if err := c.workflow.ProcessDue(ctx, 16); err != nil {
		t.Fatal(err)
	}
	if err := c.workflow.DeliverDue(ctx, 16); err != nil {
		t.Fatal(err)
	}
	transaction, err := peer.workflow.Wait(ctx, transactionUID, 10*time.Millisecond)
	if err != nil || transaction.Result == nil || len(transaction.Result.ReferencedSOPs) != 1 {
		t.Fatalf("restart %+v %v", transaction, err)
	}
	path := filepath.Join(opts.writable.root, uid+".dcm")
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	data[len(data)-1] ^= 1
	if err := os.WriteFile(path, data, 0600); err != nil {
		t.Fatal(err)
	}
	result, err := c.ProcessStorageCommitment(ctx, dimse.StorageCommitmentTransaction{TransactionUID: "2.25.1357322", ReferencedSOPs: []dimse.StorageCommitmentSOPReference{{SOPClassUID: dimsetest.SecondaryCaptureSOPClassUID, SOPInstanceUID: uid}}})
	if err != nil || len(result.ReferencedSOPs) != 0 || len(result.FailedSOPs) != 1 || result.FailedSOPs[0].FailureReason != dimse.StorageCommitmentFailureReasonProcessingFailure {
		t.Fatalf("changed bytes committed %+v %v", result, err)
	}
}
