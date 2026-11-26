package main

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/ThalesMMS/dicom-go/core"
	"github.com/ThalesMMS/dicom-go/net/dimse"
	"github.com/ThalesMMS/dicom-go/net/ul"
)

const (
	commitArchiveAE   = "REFERENCEARCHIVE"
	commitRequestorAE = "COMMITSCU"
)

// commitRequestor is a Go Storage Commitment SCU with its own callback
// listener, standing for a modality that asks the archive to commit.
type commitRequestor struct {
	workflow *dimse.StorageCommitmentWorkflow
	store    *dimse.MemoryStorageCommitmentStore
	address  string
}

func startCommitRequestor(t *testing.T, ctx context.Context) commitRequestor {
	t.Helper()
	store, err := dimse.NewMemoryStorageCommitmentStore(100)
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
		_ = workflow.ServeListener(serveCtx, listener, dimse.StorageCommitmentListenerOptions{Accept: ul.AcceptOptions{
			AETitle: commitRequestorAE, RequireMatchingCalledAE: true,
			SupportedAbstractSyntaxes: []string{dimse.StorageCommitmentPushModelSOPClassUID},
			SupportedTransferSyntaxes: []string{ul.ExplicitVRLittleEndian, ul.ImplicitVRLittleEndian},
			RoleSelections:            []ul.RoleSelectionItem{{SopClassUID: dimse.StorageCommitmentPushModelSOPClassUID, SCPRole: true}},
		}})
	}()
	t.Cleanup(func() { stop(); _ = listener.Close(); <-done })
	return commitRequestor{workflow: workflow, store: store, address: listener.Addr().String()}
}

// request sends one N-ACTION to the archive on its own association.
func (r commitRequestor) request(t *testing.T, ctx context.Context, archiveAddress, uid string, refs []dimse.StorageCommitmentSOPReference) error {
	t.Helper()
	assoc, err := ul.DialContext(ctx, archiveAddress, ul.DialOptions{
		CallingAETitle: commitRequestorAE, CalledAETitle: commitArchiveAE,
		Contexts: []ul.PresentationContext{{AbstractSyntaxUID: dimse.StorageCommitmentPushModelSOPClassUID, TransferSyntaxUIDs: []string{ul.ImplicitVRLittleEndian}}},
	})
	if err != nil {
		t.Fatal(err)
	}
	defer assoc.Close()
	_, err = r.workflow.Request(ctx, assoc, refs, dimse.StorageCommitmentRequestOptions{TransactionUID: uid})
	_ = assoc.Release(ctx)
	return err
}

func (r commitRequestor) result(t *testing.T, ctx context.Context, uid string) dimse.StorageCommitmentResult {
	t.Helper()
	waitCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	transaction, err := r.workflow.Wait(waitCtx, uid, 20*time.Millisecond)
	if err != nil || transaction.Result == nil {
		t.Fatalf("result of %s: %+v %v", uid, transaction, err)
	}
	return *transaction.Result
}

type commitArchive struct {
	archive    *archive
	commitment *archiveCommitment
	address    string
}

// startCommitArchive serves the archive with Storage Commitment; the worker
// runs only when worker is true, so restart tests can stop between the
// accepted request and its processing.
func startCommitArchive(t *testing.T, ctx context.Context, root, commitDir string, callbacks map[string]string, worker bool) (commitArchive, func()) {
	t.Helper()
	a, err := openArchive(ctx, root, defaultArchiveLimits())
	if err != nil {
		t.Fatal(err)
	}
	c, err := newArchiveCommitment(a, commitArchiveAE, commitmentConfig{dir: commitDir, maxTransactions: 16, callbacks: callbacks, interval: 20 * time.Millisecond})
	if err != nil {
		t.Fatal(err)
	}
	listener, err := ul.Listen(ul.ListenOptions{Address: "127.0.0.1:0", Context: ctx})
	if err != nil {
		t.Fatal(err)
	}
	server, err := newArchiveServerWithCommitment(listener, a, commitArchiveAE, nil, c)
	if err != nil {
		t.Fatal(err)
	}
	serveCtx, stop := context.WithCancel(ctx)
	serverDone, workerDone := make(chan error, 1), make(chan struct{})
	go func() { serverDone <- server.Serve(serveCtx) }()
	go func() {
		defer close(workerDone)
		if worker {
			c.run(serveCtx, nil)
		}
	}()
	stopped := false
	shutdown := func() {
		if stopped {
			return
		}
		stopped = true
		stop()
		<-serverDone
		<-workerDone
		_ = listener.Close()
		_ = c.Close()
		_ = a.Close()
	}
	t.Cleanup(shutdown)
	return commitArchive{archive: a, commitment: c, address: listener.Addr().String()}, shutdown
}

func storeCommitInstance(t *testing.T, ctx context.Context, a *archive, uid string) {
	t.Helper()
	if status, err := a.Store(ctx, storeRequest(syntheticInstance("2.25.9461", "2.25.9462", uid))); err != nil || status != dimse.StatusSuccess {
		t.Fatalf("store %s: %#x %v", uid, status, err)
	}
}

func ref(class, uid string) dimse.StorageCommitmentSOPReference {
	return dimse.StorageCommitmentSOPReference{SOPClassUID: class, SOPInstanceUID: uid}
}

const ctClass = "1.2.840.10008.5.1.4.1.1.2"

// Store, request commitment, receive the event on a separate association and
// retrieve: every Failure Reason of PS3.3 C.14.1.1 the archive can produce,
// Event Type 2 with the exact partition, then Event Type 1.
func TestArchiveStorageCommitmentVerifiesEveryReference(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	requestor := startCommitRequestor(t, ctx)
	running, _ := startCommitArchive(t, ctx, t.TempDir(), t.TempDir(), map[string]string{commitRequestorAE: requestor.address}, true)
	a := running.archive
	for _, uid := range []string{"2.25.9463.1", "2.25.9463.2", "2.25.9463.3", "2.25.9463.4", "2.25.9463.5"} {
		storeCommitInstance(t, ctx, a, uid)
	}
	// .4 is rewritten in place with the same size and time: only the content
	// digest can tell. .5 disappears.
	tampered := filepath.Join(a.root, a.items["2.25.9463.4"].name)
	info, err := os.Stat(tampered)
	if err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(tampered)
	if err != nil {
		t.Fatal(err)
	}
	data[len(data)-1] ^= 0xFF
	if err := os.WriteFile(tampered, data, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(tampered, info.ModTime(), info.ModTime()); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(filepath.Join(a.root, a.items["2.25.9463.5"].name)); err != nil {
		t.Fatal(err)
	}
	refs := []dimse.StorageCommitmentSOPReference{
		ref(archiveTestClass, "2.25.9463.1"),
		ref(archiveTestClass, "2.25.9463.2"),
		ref(ctClass, "2.25.9463.3"),                         // stored as another class
		ref(archiveTestClass, "2.25.9463.4"),                // content changed
		ref(archiveTestClass, "2.25.9463.5"),                // file removed
		ref(archiveTestClass, "2.25.9463.6"),                // never stored
		ref("1.2.840.10008.5.1.4.1.1.481.5", "2.25.9463.7"), // class the archive does not store
	}
	if err := requestor.request(t, ctx, running.address, "2.25.9464", refs); err != nil {
		t.Fatal(err)
	}
	result := requestor.result(t, ctx, "2.25.9464")
	wantFailed := map[string]uint16{
		"2.25.9463.3": dimse.StorageCommitmentFailureReasonClassInstanceConflict,
		"2.25.9463.4": dimse.StorageCommitmentFailureReasonProcessingFailure,
		"2.25.9463.5": dimse.StorageCommitmentFailureReasonNoSuchSOPInstance,
		"2.25.9463.6": dimse.StorageCommitmentFailureReasonNoSuchSOPInstance,
		"2.25.9463.7": dimse.StorageCommitmentFailureReasonSOPClassNotSupported,
	}
	if len(result.ReferencedSOPs) != 2 || len(result.FailedSOPs) != len(wantFailed) {
		t.Fatalf("partition %+v", result)
	}
	for _, failed := range result.FailedSOPs {
		if wantFailed[failed.SOPInstanceUID] != failed.FailureReason {
			t.Fatalf("%s failed with %#04x", failed.SOPInstanceUID, failed.FailureReason)
		}
	}
	// Event Type 1: every reference committed.
	if err := requestor.request(t, ctx, running.address, "2.25.9465", refs[:2]); err != nil {
		t.Fatal(err)
	}
	if result := requestor.result(t, ctx, "2.25.9465"); len(result.FailedSOPs) != 0 || len(result.ReferencedSOPs) != 2 {
		t.Fatalf("event type 1 %+v", result)
	}
	// What was committed is what retrieval returns.
	loaded, err := a.load(ctx, a.items["2.25.9463.1"])
	if err != nil {
		t.Fatal(err)
	}
	if raw, _ := loaded.GetRaw(core.TagPixelData); !bytes.Equal(raw, []byte{0, 1, 2, 127, 128, 255}) {
		t.Fatalf("retrieved pixels %v", raw)
	}
}

// The transaction survives a restart between N-ACTION-RSP and processing:
// the reopened archive processes and delivers it once.
func TestArchiveStorageCommitmentResumesAfterRestart(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	requestor := startCommitRequestor(t, ctx)
	root, commitDir := t.TempDir(), t.TempDir()
	callbacks := map[string]string{commitRequestorAE: requestor.address}
	first, shutdown := startCommitArchive(t, ctx, root, commitDir, callbacks, false)
	storeCommitInstance(t, ctx, first.archive, "2.25.9466.1")
	if err := requestor.request(t, ctx, first.address, "2.25.9467", []dimse.StorageCommitmentSOPReference{ref(archiveTestClass, "2.25.9466.1")}); err != nil {
		t.Fatal(err)
	}
	pending, err := first.commitment.store.Get(ctx, "2.25.9467")
	if err != nil || pending.State != dimse.StorageCommitmentStateAccepted {
		t.Fatalf("before restart %+v %v", pending, err)
	}
	shutdown()
	second, _ := startCommitArchive(t, ctx, root, commitDir, callbacks, true)
	if result := requestor.result(t, ctx, "2.25.9467"); len(result.ReferencedSOPs) != 1 || len(result.FailedSOPs) != 0 {
		t.Fatalf("after restart %+v", result)
	}
	waitCtx, stop := context.WithTimeout(ctx, 5*time.Second)
	defer stop()
	for {
		delivered, err := second.commitment.store.Get(ctx, "2.25.9467")
		if err == nil && delivered.State == dimse.StorageCommitmentStateDelivered && delivered.DeliveryAttempts == 1 {
			break
		}
		select {
		case <-waitCtx.Done():
			t.Fatalf("committer record %+v %v", delivered, err)
		case <-time.After(20 * time.Millisecond):
		}
	}
}

// A reused Transaction UID with different references is refused; an identical
// retry is accepted once. Store failures refuse the request (0110H) without
// leaving a record, and a full store answers Resource limitation (0213H).
func TestArchiveStorageCommitmentDuplicatesAndStoreFailures(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	requestor := startCommitRequestor(t, ctx)
	commitDir := t.TempDir()
	running, _ := startCommitArchive(t, ctx, t.TempDir(), commitDir, map[string]string{commitRequestorAE: requestor.address}, false)
	one := []dimse.StorageCommitmentSOPReference{ref(archiveTestClass, "2.25.9468.1")}
	if err := requestor.request(t, ctx, running.address, "2.25.9469", one); err != nil {
		t.Fatal(err)
	}
	other, err := dimse.NewStorageCommitmentWorkflow(dimse.StorageCommitmentWorkflowOptions{Store: mustMemoryStore(t)})
	if err != nil {
		t.Fatal(err)
	}
	conflicting := commitRequestor{workflow: other, store: nil, address: requestor.address}
	if err := conflicting.request(t, ctx, running.address, "2.25.9469", []dimse.StorageCommitmentSOPReference{ref(archiveTestClass, "2.25.9468.2")}); err == nil {
		t.Fatal("conflicting reuse of a Transaction UID accepted")
	}
	stored, err := running.commitment.store.Get(ctx, "2.25.9469")
	if err != nil || len(stored.ReferencedSOPs) != 1 || stored.ReferencedSOPs[0].SOPInstanceUID != "2.25.9468.1" {
		t.Fatalf("original transaction %+v %v", stored, err)
	}

	running.commitment.store.syncFile = func(*os.File) error { return errors.New("injected sync failure") }
	if err := requestor.request(t, ctx, running.address, "2.25.9470", one); err == nil {
		t.Fatal("request accepted without a durable record")
	}
	if record, _ := requestor.store.Get(ctx, "2.25.9470"); record.LastDIMSEStatus != dimse.StatusStorageCommitmentProcessingFailure {
		t.Fatalf("status %#04x", record.LastDIMSEStatus)
	}
	if _, err := running.commitment.store.Get(ctx, "2.25.9470"); !errors.Is(err, dimse.ErrStorageCommitmentTransactionNotFound) {
		t.Fatalf("record after failed sync: %v", err)
	}
	entries, _ := os.ReadDir(commitDir)
	for _, entry := range entries {
		if filepath.Ext(entry.Name()) != ".json" && entry.Name() != commitmentLockName {
			t.Fatalf("leftover %s", entry.Name())
		}
	}
	running.commitment.store.syncFile = (*os.File).Sync
	running.commitment.store.max = 1
	if err := requestor.request(t, ctx, running.address, "2.25.9471", one); err == nil {
		t.Fatal("full store accepted a request")
	}
	if record, _ := requestor.store.Get(ctx, "2.25.9471"); record.LastDIMSEStatus != dimse.StatusStorageCommitmentResourceLimitation {
		t.Fatalf("full store status %#04x", record.LastDIMSEStatus)
	}
}

func mustMemoryStore(t *testing.T) *dimse.MemoryStorageCommitmentStore {
	t.Helper()
	store, err := dimse.NewMemoryStorageCommitmentStore(10)
	if err != nil {
		t.Fatal(err)
	}
	return store
}

// Without a commitment configuration the SOP Class is not negotiated, and a
// commitment directory must be configured with callbacks outside the root.
func TestArchiveStorageCommitmentOffByDefaultAndConfigurationChecks(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	running := startEncapsulatedArchive(t, ctx, nil)
	assoc, err := ul.DialContext(ctx, running.address, ul.DialOptions{
		CallingAETitle: commitRequestorAE, CalledAETitle: commitArchiveAE,
		Contexts: []ul.PresentationContext{
			{AbstractSyntaxUID: dimse.VerificationSOPClassUID, TransferSyntaxUIDs: []string{ul.ImplicitVRLittleEndian}},
			{AbstractSyntaxUID: dimse.StorageCommitmentPushModelSOPClassUID, TransferSyntaxUIDs: []string{ul.ImplicitVRLittleEndian}},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	for _, accepted := range assoc.AcceptedContexts {
		if accepted.AbstractSyntaxUID == dimse.StorageCommitmentPushModelSOPClassUID {
			t.Fatal("Storage Commitment negotiated without a commitment policy")
		}
	}
	_ = assoc.Release(ctx)
	_ = assoc.Close()

	root := t.TempDir()
	a, err := openArchive(ctx, root, defaultArchiveLimits())
	if err != nil {
		t.Fatal(err)
	}
	defer a.Close()
	for name, config := range map[string]commitmentConfig{
		"no callbacks":      {dir: t.TempDir(), maxTransactions: 4},
		"inside the root":   {dir: filepath.Join(root, "commit"), maxTransactions: 4, callbacks: map[string]string{"A": "127.0.0.1:1"}},
		"the root itself":   {dir: root, maxTransactions: 4, callbacks: map[string]string{"A": "127.0.0.1:1"}},
		"zero transactions": {dir: t.TempDir(), callbacks: map[string]string{"A": "127.0.0.1:1"}},
	} {
		if c, err := newArchiveCommitment(a, commitArchiveAE, config); err == nil {
			c.Close()
			t.Fatalf("%s accepted", name)
		}
	}
	// A foreign file in the commitment directory stops the restart.
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "notes.txt"), []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := openFileCommitmentStore(dir, root, 4); err == nil {
		t.Fatal("foreign entry accepted")
	}
}

// Cancellation during verification reports no result.
func TestArchiveStorageCommitmentCanceledVerificationReportsNothing(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	running, _ := startCommitArchive(t, ctx, t.TempDir(), t.TempDir(), map[string]string{commitRequestorAE: "127.0.0.1:1"}, false)
	storeCommitInstance(t, ctx, running.archive, "2.25.9472.1")
	canceled, stop := context.WithCancel(ctx)
	stop()
	result, err := running.commitment.ProcessStorageCommitment(canceled, dimse.StorageCommitmentTransaction{
		TransactionUID: "2.25.9473", ReferencedSOPs: []dimse.StorageCommitmentSOPReference{ref(archiveTestClass, "2.25.9472.1")},
	})
	if !errors.Is(err, context.Canceled) || len(result.ReferencedSOPs)+len(result.FailedSOPs) != 0 {
		t.Fatalf("canceled verification %+v %v", result, err)
	}
}
