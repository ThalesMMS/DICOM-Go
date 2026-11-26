package main

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/ThalesMMS/dicom-go/net/dimse"
	"github.com/ThalesMMS/dicom-go/net/ul"
)

// commitmentConfig enables Storage Commitment Push Model as the committing
// SCP. Without a durable transaction store and at least one callback route the
// archive neither negotiates the SOP Class nor promises anything.
type commitmentConfig struct {
	// dir holds the durable transaction records; it must not be the archive
	// root or overlap it.
	dir             string
	maxTransactions int
	// callbacks maps a requestor AE Title to the address that receives the
	// N-EVENT-REPORT on a separate association (PS3.4 J.3.3.1.2).
	callbacks map[string]string
	// dialer, interval and retention are test seams; zero values select the
	// defaults.
	dialer    dimse.StorageCommitmentAssociationDialer
	interval  time.Duration
	retention time.Duration
	now       func() time.Time
}

func (c commitmentConfig) enabled() bool { return c.dir != "" }

func (c commitmentConfig) valid() bool {
	return !c.enabled() || (len(c.callbacks) > 0 && c.maxTransactions > 0 && c.maxTransactions <= 100_000)
}

// archiveCommitment is the archive's commitment policy: a reference is
// committed only when the archive holds that SOP Instance of that SOP Class,
// and the file admitted to the catalog can be read again in full, is still the
// same file, and still has the SHA-256 recorded when it was admitted. The file
// was fsynced (with its directory on Unix) before admission by #913's
// publication. Retrieval reads the same file, so what is committed is what
// C-GET and C-MOVE return.
type archiveCommitment struct {
	archive  *archive
	store    *fileCommitmentStore
	workflow *dimse.StorageCommitmentWorkflow
	interval time.Duration
	retain   time.Duration
	now      func() time.Time
}

func newArchiveCommitment(a *archive, aeTitle string, config commitmentConfig) (*archiveCommitment, error) {
	if !config.enabled() || !config.valid() {
		return nil, errCommitmentStore
	}
	store, err := openFileCommitmentStore(config.dir, a.root, config.maxTransactions)
	if err != nil {
		return nil, err
	}
	callbacks := make(map[string]string, len(config.callbacks))
	for ae, address := range config.callbacks {
		callbacks[ae] = address
	}
	c := &archiveCommitment{archive: a, store: store, interval: config.interval, retain: config.retention, now: config.now}
	if c.interval <= 0 {
		c.interval = 250 * time.Millisecond
	}
	if c.retain <= 0 {
		c.retain = 24 * time.Hour
	}
	if c.now == nil {
		c.now = time.Now
	}
	workflow, err := dimse.NewStorageCommitmentWorkflow(dimse.StorageCommitmentWorkflowOptions{
		Store:               store,
		Processor:           c,
		DefaultDeliveryMode: dimse.StorageCommitmentDeliveryCallback,
		Dialer:              config.dialer,
		Now:                 config.now,
		Resolver: dimse.StorageCommitmentCallbackResolverFunc(func(_ context.Context, request dimse.StorageCommitmentCallbackRequest) (dimse.StorageCommitmentCallbackTarget, error) {
			address, ok := callbacks[request.RequestorAETitle]
			if !ok || request.CommitterAETitle != aeTitle {
				return dimse.StorageCommitmentCallbackTarget{}, dimse.ErrStorageCommitmentCallbackUnknown
			}
			return dimse.StorageCommitmentCallbackTarget{Address: address, DialOptions: ul.DialOptions{
				CallingAETitle: aeTitle, CalledAETitle: request.RequestorAETitle,
				NegotiationTimeout: 5 * time.Second, ReadProgressTimeout: 10 * time.Second,
				WriteProgressTimeout: 10 * time.Second, ReleaseTimeout: 2 * time.Second,
			}}, nil
		}),
	})
	if err != nil {
		store.Close()
		return nil, err
	}
	c.workflow = workflow
	return c, nil
}

func (c *archiveCommitment) Close() error { return c.store.Close() }

// run processes accepted requests and delivers results until ctx ends. Work
// is at-least-once and resumes after a restart from the durable store.
func (c *archiveCommitment) run(ctx context.Context, observe func(error)) {
	ticker := time.NewTicker(c.interval)
	defer ticker.Stop()
	for {
		if err := c.step(ctx); err != nil && observe != nil && ctx.Err() == nil {
			observe(err)
		}
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
	}
}

func (c *archiveCommitment) step(ctx context.Context) error {
	processErr := c.workflow.ProcessDue(ctx, 16)
	deliverErr := c.workflow.DeliverDue(ctx, 16)
	_, purgeErr := c.workflow.PurgeCompleted(ctx, c.now().Add(-c.retain), 64)
	return errors.Join(processErr, deliverErr, purgeErr)
}

// ProcessStorageCommitment partitions the requested references exactly. A
// canceled or failing verification returns an error instead of a result, so
// no reference is reported before it was checked.
func (c *archiveCommitment) ProcessStorageCommitment(ctx context.Context, transaction dimse.StorageCommitmentTransaction) (dimse.StorageCommitmentResult, error) {
	result := dimse.StorageCommitmentResult{TransactionUID: transaction.TransactionUID}
	for _, ref := range transaction.ReferencedSOPs {
		reason, err := c.archive.verifyCommitment(ctx, ref)
		if err != nil {
			return dimse.StorageCommitmentResult{}, err
		}
		if reason == 0 {
			result.ReferencedSOPs = append(result.ReferencedSOPs, dimse.StorageCommitmentSOPReference{SOPClassUID: ref.SOPClassUID, SOPInstanceUID: ref.SOPInstanceUID})
			continue
		}
		result.FailedSOPs = append(result.FailedSOPs, dimse.StorageCommitmentSOPReference{SOPClassUID: ref.SOPClassUID, SOPInstanceUID: ref.SOPInstanceUID, FailureReason: reason})
	}
	return result, nil
}

// verifyCommitment returns 0 when the archive commits to ref, or the PS3.3
// C.14.1.1 Failure Reason. Errors are returned only for cancellation.
func (a *archive) verifyCommitment(ctx context.Context, ref dimse.StorageCommitmentSOPReference) (uint16, error) {
	if !supportedStorageClass(ref.SOPClassUID) {
		return dimse.StorageCommitmentFailureReasonSOPClassNotSupported, nil
	}
	if err := a.enter(ctx); err != nil {
		if ctxErr := ctx.Err(); ctxErr != nil {
			return 0, ctxErr
		}
		return dimse.StorageCommitmentFailureReasonProcessingFailure, nil
	}
	item, ok := a.items[ref.SOPInstanceUID]
	a.leave()
	if !ok {
		return dimse.StorageCommitmentFailureReasonNoSuchSOPInstance, nil
	}
	if item.record.Instance.SOPClassUID != ref.SOPClassUID {
		return dimse.StorageCommitmentFailureReasonClassInstanceConflict, nil
	}
	current, err := a.inspect(ctx, item.name)
	if ctxErr := ctx.Err(); ctxErr != nil {
		return 0, ctxErr
	}
	if err != nil {
		if _, statErr := os.Lstat(filepath.Join(a.root, item.name)); errors.Is(statErr, os.ErrNotExist) {
			return dimse.StorageCommitmentFailureReasonNoSuchSOPInstance, nil
		}
		return dimse.StorageCommitmentFailureReasonProcessingFailure, nil
	}
	if !sameStoredFile(current.info, item.info) || current.digest != item.digest ||
		current.record.Instance.SOPInstanceUID != ref.SOPInstanceUID || current.record.Instance.SOPClassUID != ref.SOPClassUID {
		return dimse.StorageCommitmentFailureReasonProcessingFailure, nil
	}
	return 0, nil
}

func supportedStorageClass(uid string) bool {
	for _, class := range storageClasses {
		if strings.TrimSpace(uid) == class {
			return true
		}
	}
	return false
}
