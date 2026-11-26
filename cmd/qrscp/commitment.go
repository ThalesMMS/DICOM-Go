package main

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"runtime"
	"strings"
	"time"

	"github.com/ThalesMMS/dicom-go/internal/nofollow"
	"github.com/ThalesMMS/dicom-go/net/dimse"
	"github.com/ThalesMMS/dicom-go/net/ul"
	"github.com/ThalesMMS/dicom-go/object"
)

type archiveCommitment struct {
	archive   *writableArchive
	store     *dimse.FileStorageCommitmentStore
	workflow  *dimse.StorageCommitmentWorkflow
	sameAssoc bool
	routes    map[string]string
}

func newArchiveCommitment(a *writableArchive, ae string, w writableOptions) (*archiveCommitment, error) {
	d, created, err := nofollow.OpenDirectoryAt(a.parent, ".qrscp-commitment", true)
	if err != nil {
		return nil, errArchive
	}
	d.Close()
	if created && runtime.GOOS != "windows" {
		if err := a.parent.Sync(); err != nil {
			return nil, errArchive
		}
	}
	store, err := dimse.NewFileStorageCommitmentStore(filepath.Join(a.root, ".qrscp-commitment"), dimse.FileStorageCommitmentStoreOptions{MaxTransactions: w.maxTransactions})
	if err != nil {
		return nil, err
	}
	c := &archiveCommitment{archive: a, store: store, sameAssoc: w.sameAssoc}
	mode := dimse.StorageCommitmentDeliveryCallback
	if w.sameAssoc {
		mode = dimse.StorageCommitmentDeliverySameAssociation
	}
	routes := make(map[string]string, len(w.callbacks))
	for ae, address := range w.callbacks {
		routes[ae] = address
	}
	c.routes = routes
	c.workflow, err = dimse.NewStorageCommitmentWorkflow(dimse.StorageCommitmentWorkflowOptions{
		Store: store, Processor: c, DefaultDeliveryMode: mode,
		Resolver: dimse.StorageCommitmentCallbackResolverFunc(func(_ context.Context, request dimse.StorageCommitmentCallbackRequest) (dimse.StorageCommitmentCallbackTarget, error) {
			address, ok := routes[request.RequestorAETitle]
			if !ok || request.CommitterAETitle != ae {
				return dimse.StorageCommitmentCallbackTarget{}, dimse.ErrStorageCommitmentCallbackUnknown
			}
			return dimse.StorageCommitmentCallbackTarget{Address: address, DialOptions: ul.DialOptions{CallingAETitle: ae, CalledAETitle: request.RequestorAETitle, NegotiationTimeout: 5 * time.Second, ReadProgressTimeout: 10 * time.Second, WriteProgressTimeout: 10 * time.Second, ReleaseTimeout: 2 * time.Second}}, nil
		}),
	})
	if err != nil {
		store.Close()
		return nil, err
	}
	return c, nil
}

func (c *archiveCommitment) Close() error { return c.store.Close() }

func (c *archiveCommitment) normalizedOptions(assoc *ul.Association) dimse.NormalizedSCPOptions {
	opts := c.workflow.NormalizedOptions(assoc)
	inner := opts.ActionHandler
	opts.ActionHandler = func(ctx context.Context, request dimse.NormalizedActionRequest, data *object.Object) (dimse.NormalizedActionSCPResult, error) {
		if _, ok := c.routes[strings.TrimSpace(assoc.CallingAETitle)]; !ok {
			return dimse.NormalizedActionSCPResult{Response: dimse.NormalizedActionResponse{Status: dimse.StatusStorageCommitmentProcessingFailure}}, errors.New("qrscp: Storage Commitment callback route unavailable")
		}
		return inner(ctx, request, data)
	}
	return opts
}

func (c *archiveCommitment) run(ctx context.Context) {
	ticker := time.NewTicker(250 * time.Millisecond)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			// Dedicated same-association handlers own processing and reads. Callback
			// transactions resume processing/delivery from the durable workflow state.
			if !c.sameAssoc {
				_ = c.workflow.ProcessDue(ctx, 16)
				_ = c.workflow.DeliverDue(ctx, 16)
			}
			_, _ = c.workflow.PurgeCompleted(ctx, time.Now().Add(-24*time.Hour), 64)
		}
	}
}

func (c *archiveCommitment) ProcessStorageCommitment(ctx context.Context, t dimse.StorageCommitmentTransaction) (dimse.StorageCommitmentResult, error) {
	result := dimse.StorageCommitmentResult{TransactionUID: t.TransactionUID}
	for _, ref := range t.ReferencedSOPs {
		if err := ctx.Err(); err != nil {
			return dimse.StorageCommitmentResult{}, err
		}
		reason, err := c.verify(ctx, ref)
		if err != nil {
			return dimse.StorageCommitmentResult{}, err
		}
		copy := dimse.StorageCommitmentSOPReference{SOPClassUID: ref.SOPClassUID, SOPInstanceUID: ref.SOPInstanceUID, FailureReason: reason}
		if reason == 0 {
			result.ReferencedSOPs = append(result.ReferencedSOPs, copy)
		} else {
			result.FailedSOPs = append(result.FailedSOPs, copy)
		}
	}
	return result, nil
}

func (c *archiveCommitment) verify(ctx context.Context, ref dimse.StorageCommitmentSOPReference) (uint16, error) {
	a := c.archive
	if err := a.enter(ctx); err != nil {
		if ctx.Err() != nil {
			return 0, ctx.Err()
		}
		return dimse.StorageCommitmentFailureReasonProcessingFailure, nil
	}
	row, ok := a.items[ref.SOPInstanceUID]
	supported := false
	for _, uid := range dimse.DefaultStorageSOPClassUIDs() {
		if uid == ref.SOPClassUID {
			supported = true
			break
		}
	}
	if ok && row.row.Record.Instance.SOPClassUID == ref.SOPClassUID {
		supported = true
	}
	a.leave()
	if !supported {
		return dimse.StorageCommitmentFailureReasonSOPClassNotSupported, nil
	}
	if !ok {
		return dimse.StorageCommitmentFailureReasonNoSuchSOPInstance, nil
	}
	if row.row.Record.Instance.SOPClassUID != ref.SOPClassUID {
		return dimse.StorageCommitmentFailureReasonClassInstanceConflict, nil
	}
	f, openErr := nofollow.OpenAt(a.parent, ref.SOPInstanceUID+".dcm")
	if errors.Is(openErr, os.ErrNotExist) {
		return dimse.StorageCommitmentFailureReasonNoSuchSOPInstance, nil
	}
	if openErr != nil {
		return dimse.StorageCommitmentFailureReasonProcessingFailure, nil
	}
	f.Close()
	current, err := a.inspect(ctx, ref.SOPInstanceUID)
	if ctx.Err() != nil {
		return 0, ctx.Err()
	}
	if err != nil || !reflect.DeepEqual(row.row, current.row) {
		return dimse.StorageCommitmentFailureReasonProcessingFailure, nil
	}
	return 0, nil
}

// serveDedicated uses the workflow's public single-reader sequence. Mixing
// STGCMT with other accepted classes would route arbitrary commands to N-ACTION;
// refuse that profile explicitly rather than race ServeAssociation's read loop.
func (c *archiveCommitment) serveDedicated(ctx context.Context, assoc *ul.Association) error {
	if len(assoc.AcceptedContexts) != 1 {
		return errors.New("qrscp: same-association Storage Commitment requires exactly one accepted STGCMT presentation context")
	}
	var id byte
	for _, pc := range assoc.AcceptedContexts {
		if pc.AbstractSyntaxUID != dimse.StorageCommitmentPushModelSOPClassUID {
			return errors.New("qrscp: same-association Storage Commitment requires a dedicated STGCMT-only association")
		}
		if id == 0 {
			id = pc.ID
		}
	}
	if id == 0 {
		return errors.New("qrscp: Storage Commitment context missing")
	}
	t, err := c.workflow.ServeAction(ctx, assoc, id)
	if err != nil {
		return err
	}
	if t.Result == nil {
		t, err = c.workflow.Process(ctx, t.TransactionUID)
		if err != nil {
			return err
		}
	}
	if _, err := c.workflow.DeliverOnAssociation(ctx, t.TransactionUID, assoc); err != nil {
		return err
	}
	return assoc.Release(ctx)
}
