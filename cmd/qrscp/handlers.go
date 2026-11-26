package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"time"

	"github.com/ThalesMMS/dicom-go/core"
	"github.com/ThalesMMS/dicom-go/net/dimse"
	"github.com/ThalesMMS/dicom-go/net/ul"
	"github.com/ThalesMMS/dicom-go/object"
	"github.com/ThalesMMS/dicom-go/transfer"
)

// Status values of PS3.4 C.4.1.1.4, C.4.2.1.5 and C.4.3.1.4.
const (
	statusOutOfResources         uint16 = 0xA700
	statusMoveDestinationUnknown uint16 = 0xA801
	statusIdentifierMismatch     uint16 = 0xA900
	statusUnableToProcess        uint16 = 0xC000
)

// associationServices serves one association; C-MOVE sub-operations name
// its calling AE title as Move Originator.
type associationServices struct {
	*server
	callingAE string
}

func statusFor(err error) (uint16, string) {
	switch {
	case errors.Is(err, errIdentifier):
		return statusIdentifierMismatch, "Identifier does not match SOP Class"
	case errors.Is(err, errTooManyMatches):
		return statusOutOfResources, "Too many matches"
	default:
		return statusUnableToProcess, "Unable to process"
	}
}

func (s *associationServices) Find(ctx context.Context, request dimse.CFindRequestContext) ([]*object.Object, error) {
	if s.findStatus != 0 {
		return nil, dimse.NewCFindSCPError(s.findStatus, "Injected C-FIND failure", errors.New("qrscp: injected C-FIND failure"))
	}
	view, err := s.server.view(ctx)
	if err != nil {
		return nil, dimse.NewCFindSCPError(statusUnableToProcess, "Archive unavailable", err)
	}
	s = &associationServices{server: view, callingAE: s.callingAE}
	model := modelOf(request.Request.AffectedSOPClassUID)
	entities, err := s.selectEntities(ctx, model, request.QueryRetrieveLevel, request.Identifier, false)
	if err != nil {
		if ctx.Err() != nil {
			return nil, err
		}
		status, comment := statusFor(err)
		return nil, dimse.NewCFindSCPError(status, comment, err)
	}
	responses := make([]*object.Object, 0, len(entities))
	for _, e := range entities {
		responses = append(responses, s.response(e, model, request.QueryRetrieveLevel, request.Identifier))
	}
	return responses, nil
}

func (s *associationServices) retrieve(ctx context.Context, sopClassUID, level string, identifier *object.Object) ([]*instance, error) {
	view, err := s.server.view(ctx)
	if err != nil {
		return nil, err
	}
	entities, err := view.selectEntities(ctx, modelOf(sopClassUID), level, identifier, true)
	if err != nil {
		return nil, err
	}
	return instancesOf(entities), nil
}

func (s *associationServices) Get(ctx context.Context, request dimse.CGetRequestContext) ([]dimse.CGetSubOperation, error) {
	items, err := s.retrieve(ctx, request.Request.AffectedSOPClassUID, request.QueryRetrieveLevel, request.Identifier)
	if err != nil {
		if ctx.Err() != nil {
			return nil, err
		}
		status, comment := statusFor(err)
		return nil, dimse.NewCGetSCPError(status, comment, err)
	}
	operations := make([]dimse.CGetSubOperation, 0, len(items))
	for _, item := range items {
		item := item
		operations = append(operations, dimse.CGetSubOperation{
			AffectedSOPClassUID:    item.record.Instance.SOPClassUID,
			AffectedSOPInstanceUID: item.record.Instance.SOPInstanceUID,
			TransferSyntaxUIDs:     transfer.ProposedStoreTransferSyntaxUIDs(item.record.FileMeta.TransferSyntaxUID, transfer.NativeStoreSourceFirst),
			LoadDataSet:            func(ctx context.Context) (*object.Object, error) { return s.load(ctx, item) },
		})
	}
	return operations, nil
}

// load reads an instance again and checks that it is still the indexed one.
func (s *server) load(ctx context.Context, item *instance) (*object.Object, error) {
	if s.archive != nil {
		parsed, err := s.archive.verifiedFile(ctx, item)
		if err != nil {
			return nil, err
		}
		defer parsed.Close()
		return parsed.Dataset, nil
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	file, err := os.Open(item.path)
	if err != nil {
		return nil, errors.New("qrscp: indexed file is no longer readable")
	}
	defer file.Close()
	parsed, err := object.ReadFileWithOptions(contextReader{ctx: ctx, r: io.LimitReader(file, s.maxInstanceSize+1)}, object.ReadFileOptions{MaxTotalBytes: s.maxInstanceSize})
	if err != nil {
		return nil, fmt.Errorf("qrscp: indexed file cannot be read: %w", err)
	}
	defer parsed.Close()
	class, _ := parsed.Dataset.GetUID(core.NewTag(0x0008, 0x0016))
	uid, _ := parsed.Dataset.GetUID(core.NewTag(0x0008, 0x0018))
	if class != item.record.Instance.SOPClassUID || uid != item.record.Instance.SOPInstanceUID {
		return nil, errors.New("qrscp: indexed file changed since startup")
	}
	return parsed.Dataset, nil
}

type contextReader struct {
	ctx context.Context
	r   io.Reader
}

func (r contextReader) Read(p []byte) (int, error) {
	if err := r.ctx.Err(); err != nil {
		return 0, err
	}
	return r.r.Read(p)
}

func (s *associationServices) moveSelection(ctx context.Context, request dimse.CMoveRequestContext) (string, []*instance, error) {
	address, ok := s.destinations[request.Request.MoveDestination]
	if !ok {
		return "", nil, dimse.NewCMoveSCPError(statusMoveDestinationUnknown, "Move Destination unknown", errors.New("qrscp: move destination is not configured"))
	}
	items, err := s.retrieve(ctx, request.Request.AffectedSOPClassUID, request.QueryRetrieveLevel, request.Identifier)
	if err != nil {
		if ctx.Err() != nil {
			return "", nil, err
		}
		status, comment := statusFor(err)
		return "", nil, dimse.NewCMoveSCPError(status, comment, err)
	}
	return address, items, nil
}

// send stores items at the destination with one StoreSession and reports
// one result per item in order.
func (s *associationServices) send(ctx context.Context, request dimse.CMoveRequestContext, address string, items []*instance, yield func(dimse.CMoveSubOperationResult) error) error {
	messageID := request.Request.MessageID
	session, err := dimse.NewStoreSession(address, dimse.StoreSessionOptions{
		DialOptions: ul.DialOptions{
			CallingAETitle: s.aeTitle, CalledAETitle: request.Request.MoveDestination,
			NegotiationTimeout: 10 * time.Second, ReadProgressTimeout: 30 * time.Second, WriteProgressTimeout: 30 * time.Second,
		},
		PlanOptions: dimse.StorePlanOptions{Limits: dimse.StoreLimits{
			MaxItems: len(items) + 1, MaxItemBytes: s.maxInstanceSize, MaxTotalBytes: 1 << 62, MaxAssociations: 8, MaxTransferSyntaxes: 4,
		}},
		MaxInFlightBytes:       s.maxInstanceSize,
		MaxAssociationAttempts: 1, MaxStoreAttempts: 1, ContinueOnError: true,
		MoveOriginatorAETitle: s.callingAE, MoveOriginatorMessageIDOrNil: &messageID,
		ReleaseTimeout: 5 * time.Second,
	})
	if err != nil {
		return err
	}
	defer func() {
		cleanup, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = session.Close(cleanup)
	}()
	sources := make([]dimse.StoreSource, 0, len(items))
	for _, item := range items {
		if s.archive != nil {
			item := item
			sources = append(sources, dimse.NewLazyStoreSource(dimse.StoreDescriptor{SOPClassUID: item.record.Instance.SOPClassUID, SOPInstanceUID: item.record.Instance.SOPInstanceUID, TransferSyntaxUID: item.record.FileMeta.TransferSyntaxUID, Size: item.record.Source.Size}, func(ctx context.Context) (*object.File, error) { return s.archive.verifiedFile(ctx, item) }))
			continue
		}
		sources = append(sources, dimse.NewPathStoreSource(item.path, dimse.StorePathSourceOptions{
			IndexOptions: indexOptions(),
			ReadOptions:  object.ReadFileOptions{MaxTotalBytes: s.maxInstanceSize},
		}))
	}
	result, batchErr := session.StoreBatch(ctx, sources)
	for _, item := range result.Items {
		status := item.Status
		if !item.StatusSet {
			status = dimse.StatusCStoreOutOfResources
		}
		if err := yield(dimse.CMoveSubOperationResult{Status: status, Err: item.Err}); err != nil {
			return err
		}
	}
	return batchErr
}

func (s *associationServices) Store(ctx context.Context, request dimse.CStoreRequestContext) (uint16, error) {
	if s.archive == nil {
		return dimse.StatusCStoreCannotUnderstand, errors.New("qrscp: read-only archive")
	}
	return s.archive.Store(ctx, request, s.aeTitle, s.callingAE)
}

func (s *associationServices) PrepareCMoveBatch(ctx context.Context, request dimse.CMoveRequestContext) (dimse.CMoveBatch, error) {
	address, items, err := s.moveSelection(ctx, request)
	if err != nil {
		return dimse.CMoveBatch{}, err
	}
	descriptors := make([]dimse.CMoveSubOperationDescriptor, 0, len(items))
	for _, item := range items {
		descriptors = append(descriptors, dimse.CMoveSubOperationDescriptor{
			AffectedSOPClassUID: item.record.Instance.SOPClassUID, AffectedSOPInstanceUID: item.record.Instance.SOPInstanceUID,
		})
	}
	return dimse.CMoveBatch{SubOperations: descriptors, Run: func(ctx context.Context, yield func(dimse.CMoveSubOperationResult) error) error {
		if len(items) == 0 {
			return nil
		}
		return s.send(ctx, request, address, items, yield)
	}}, nil
}

// Move is the per-instance form used when the batch interface is not;
// each sub-operation opens its own association.
func (s *associationServices) Move(ctx context.Context, request dimse.CMoveRequestContext) ([]dimse.CMoveSubOperation, error) {
	address, items, err := s.moveSelection(ctx, request)
	if err != nil {
		return nil, err
	}
	operations := make([]dimse.CMoveSubOperation, 0, len(items))
	for _, item := range items {
		item := item
		operations = append(operations, dimse.CMoveSubOperation{
			AffectedSOPClassUID: item.record.Instance.SOPClassUID, AffectedSOPInstanceUID: item.record.Instance.SOPInstanceUID,
			Store: func(ctx context.Context) dimse.CMoveSubOperationResult {
				result := dimse.CMoveSubOperationResult{Status: dimse.StatusCStoreOutOfResources}
				err := s.send(ctx, request, address, []*instance{item}, func(r dimse.CMoveSubOperationResult) error { result = r; return nil })
				result.Err = errors.Join(result.Err, err)
				return result
			},
		})
	}
	return operations, nil
}
