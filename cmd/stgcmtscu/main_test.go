package main

import (
	"bytes"
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/ThalesMMS/dicom-go/internal/dimsetest"
	"github.com/ThalesMMS/dicom-go/net/dimse"
	"github.com/ThalesMMS/dicom-go/net/ul"
)

const studyUID = "1.2.826.0.1.3680043.10.1191.2"

func writeInstances(t *testing.T, uids ...string) string {
	t.Helper()
	dir := t.TempDir()
	for _, uid := range uids {
		dimsetest.WriteInstance(t, dir, dimsetest.Instance{StudyUID: studyUID, SeriesUID: studyUID + ".1", SOPInstanceUID: uid})
	}
	return dir
}

func newWorkflow(t *testing.T, options dimse.StorageCommitmentWorkflowOptions) *dimse.StorageCommitmentWorkflow {
	t.Helper()
	store, err := dimse.NewMemoryStorageCommitmentStore(0)
	if err != nil {
		t.Fatal(err)
	}
	options.Store = store
	workflow, err := dimse.NewStorageCommitmentWorkflow(options)
	if err != nil {
		t.Fatal(err)
	}
	return workflow
}

// commitExcept commits every instance except failUID.
func commitExcept(transaction dimse.StorageCommitmentTransaction, failUID string) dimse.StorageCommitmentResult {
	result := dimse.StorageCommitmentResult{TransactionUID: transaction.TransactionUID}
	for _, ref := range transaction.ReferencedSOPs {
		if ref.SOPInstanceUID == failUID {
			ref.FailureReason = dimse.StorageCommitmentFailureReasonNoSuchSOPInstance
			result.FailedSOPs = append(result.FailedSOPs, ref)
			continue
		}
		result.ReferencedSOPs = append(result.ReferencedSOPs, ref)
	}
	return result
}

var committerAccept = ul.AcceptOptions{
	AETitle: "COMMITTER", SupportedAbstractSyntaxes: []string{dimse.StorageCommitmentPushModelSOPClassUID},
	SupportedTransferSyntaxes: []string{ul.ExplicitVRLittleEndian, ul.ImplicitVRLittleEndian},
}

// startSameAssociationCommitter answers on the requesting association and
// leaves the release to the requestor.
func startSameAssociationCommitter(t *testing.T, failUID string) string {
	t.Helper()
	committer := newWorkflow(t, dimse.StorageCommitmentWorkflowOptions{DefaultDeliveryMode: dimse.StorageCommitmentDeliverySameAssociation})
	return dimsetest.StartSCP(t, committerAccept, func(ctx context.Context, assoc *ul.Association) error {
		pc, err := dimse.AcceptedContextForSOPClass(assoc, dimse.StorageCommitmentPushModelSOPClassUID)
		if err != nil {
			return err
		}
		transaction, err := committer.ServeAction(ctx, assoc, pc.ID)
		if err != nil {
			return err
		}
		if _, err := committer.SetResult(ctx, commitExcept(transaction, failUID)); err != nil {
			return err
		}
		if _, err := committer.DeliverOnAssociation(ctx, transaction.TransactionUID, assoc); err != nil {
			return err
		}
		return dimse.ServeAssociation(ctx, assoc, dimse.AssociationSCPOptions{})
	})
}

func TestCommitOnSameAssociation(t *testing.T) {
	port := startSameAssociationCommitter(t, "")
	dir := writeInstances(t, studyUID+".1.1", studyUID+".1.2")
	var stdout, stderr bytes.Buffer
	const transaction = "1.2.826.0.1.3680043.10.1191.2.99"
	code := run([]string{"-port", port, "-called", "COMMITTER", "-transaction-uid", transaction, "-wait", "5s", dir}, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("exit %d\n%s%s", code, stdout.String(), stderr.String())
	}
	for _, want := range []string{
		"Storage Commitment N-ACTION status=0x0000 transaction=" + transaction + " instances=2",
		"Storage Commitment result transaction=" + transaction + " committed=2 failed=0",
	} {
		if !strings.Contains(stdout.String(), want) {
			t.Fatalf("stdout lacks %q:\n%s", want, stdout.String())
		}
	}
}

func TestFailedInstancesExitWithFailure(t *testing.T) {
	failUID := studyUID + ".1.2"
	port := startSameAssociationCommitter(t, failUID)
	dir := writeInstances(t, studyUID+".1.1", failUID)
	var stdout, stderr bytes.Buffer
	code := run([]string{"-port", port, "-wait", "5s", dir}, &stdout, &stderr)
	if code != 1 {
		t.Fatalf("exit %d, want 1\n%s%s", code, stdout.String(), stderr.String())
	}
	if !strings.Contains(stdout.String(), "committed=1 failed=1") || !strings.Contains(stdout.String(), "instance="+failUID+" reason=0x0112") {
		t.Fatalf("stdout:\n%s", stdout.String())
	}
}

// TestCommitOnCallbackAssociation: the committer releases the requesting
// association and reports on a new association to the -listen address.
func TestCommitOnCallbackAssociation(t *testing.T) {
	var mu sync.Mutex
	callback := ""
	committer := newWorkflow(t, dimse.StorageCommitmentWorkflowOptions{
		Resolver: dimse.StorageCommitmentCallbackResolverFunc(func(context.Context, dimse.StorageCommitmentCallbackRequest) (dimse.StorageCommitmentCallbackTarget, error) {
			mu.Lock()
			defer mu.Unlock()
			return dimse.StorageCommitmentCallbackTarget{Address: callback}, nil
		}),
	})
	delivered := make(chan error, 1)
	port := dimsetest.StartSCP(t, committerAccept, func(ctx context.Context, assoc *ul.Association) error {
		pc, err := dimse.AcceptedContextForSOPClass(assoc, dimse.StorageCommitmentPushModelSOPClassUID)
		if err != nil {
			return err
		}
		transaction, err := committer.ServeAction(ctx, assoc, pc.ID)
		if err != nil {
			return err
		}
		if err := assoc.Release(ctx); err != nil {
			return err
		}
		go func() {
			deliverCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			_, err := committer.SetResult(deliverCtx, commitExcept(transaction, ""))
			if err == nil {
				_, err = committer.DeliverCallback(deliverCtx, transaction.TransactionUID)
			}
			delivered <- err
		}()
		return nil
	})
	opts, err := parseArgs([]string{"-port", port, "-called", "COMMITTER", "-calling", "MODALITY", "-listen", "127.0.0.1:0", "-wait", "5s", writeInstances(t, studyUID+".1.1")}, &bytes.Buffer{})
	if err != nil {
		t.Fatal(err)
	}
	var stdout bytes.Buffer
	err = execute(context.Background(), opts, &stdout, func(address string) {
		mu.Lock()
		callback = address
		mu.Unlock()
	})
	if err != nil {
		t.Fatalf("execute: %v\n%s", err, stdout.String())
	}
	if !strings.Contains(stdout.String(), "committed=1 failed=0") {
		t.Fatalf("stdout:\n%s", stdout.String())
	}
	if err := <-delivered; err != nil {
		t.Fatalf("delivery: %v", err)
	}
}

func TestNoResultBeforeWaitExpires(t *testing.T) {
	committer := newWorkflow(t, dimse.StorageCommitmentWorkflowOptions{DefaultDeliveryMode: dimse.StorageCommitmentDeliverySameAssociation})
	port := dimsetest.StartSCP(t, committerAccept, func(ctx context.Context, assoc *ul.Association) error {
		pc, err := dimse.AcceptedContextForSOPClass(assoc, dimse.StorageCommitmentPushModelSOPClassUID)
		if err != nil {
			return err
		}
		if _, err := committer.ServeAction(ctx, assoc, pc.ID); err != nil {
			return err
		}
		return dimse.ServeAssociation(ctx, assoc, dimse.AssociationSCPOptions{})
	})
	var stdout, stderr bytes.Buffer
	code := run([]string{"-port", port, "-wait", "300ms", writeInstances(t, studyUID+".1.1")}, &stdout, &stderr)
	if code != 1 || !strings.Contains(stderr.String(), "no Storage Commitment result within 300ms") {
		t.Fatalf("exit %d\n%s%s", code, stdout.String(), stderr.String())
	}
	stdout.Reset()
	stderr.Reset()
	if code := run([]string{"-port", port, "-wait", "0", writeInstances(t, studyUID+".1.1")}, &stdout, &stderr); code != 0 {
		t.Fatalf("-wait 0: exit %d\n%s%s", code, stdout.String(), stderr.String())
	}
}

func TestParseArgsUsage(t *testing.T) {
	for _, tc := range [][]string{
		nil,
		{"-transaction-uid", "x", "dir"},
		{"-wait", "-1s", "dir"},
		{"-listen", "nonsense", "dir"},
		{"-listen", "127.0.0.1:0", "-wait", "0", "dir"},
		{"-listen", "127.0.0.1:0", "-tls-profile", "bcp195-nd", "-require-peer-cert=false", "dir"},
		{"-port", "0", "dir"},
	} {
		var stderr bytes.Buffer
		if _, err := parseArgs(tc, &stderr); !errors.Is(err, errUsage) {
			t.Errorf("parseArgs(%q) = %v, want usage error", tc, err)
		}
	}
}
