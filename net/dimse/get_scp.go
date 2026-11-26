package dimse

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/ThalesMMS/dicom-go/core"
	"github.com/ThalesMMS/dicom-go/net/ul"
	"github.com/ThalesMMS/dicom-go/object"
	"github.com/ThalesMMS/dicom-go/transfer"
)

var ErrCGetCanceled = errors.New("dicom dimse: C-GET canceled")

// CGetRequestContext is the decoded request passed to a C-GET SCP handler.
type CGetRequestContext struct {
	Request               CGetRequest
	Identifier            *object.Object
	QueryRetrieveLevel    string
	PresentationContextID byte
	IdentifierSyntax      transfer.Syntax
	// ExtendedNegotiation holds the C-MOVE/C-GET options accepted for the
	// request's SOP Class on this association (relational retrieval,
	// Enhanced Multi-Frame Image Conversion); the zero value when none was.
	ExtendedNegotiation CRetrieveExtendedNegotiation
}

// CGetSubOperation represents one C-STORE sub-operation sent on the C-GET
// association.
type CGetSubOperation struct {
	AffectedSOPClassUID    string
	AffectedSOPInstanceUID string
	// TransferSyntaxUIDs optionally constrains presentation context selection
	// for this C-STORE sub-operation. Empty preserves the StoreClient default.
	TransferSyntaxUIDs []string
	// MessageID optionally overrides the generated per-sub-operation Message ID.
	// Zero preserves the generated default.
	MessageID   uint16
	LoadDataSet func(context.Context) (*object.Object, error)
	// LoadFile optionally replaces LoadDataSet for a data set held in a known
	// transfer syntax, such as one fetched from an upstream archive; with it,
	// AffectedSOPClassUID may be empty and is then read from the data set. The
	// C-STORE is sent in that syntax, or, for a native one, in another
	// accepted native syntax (StoreTransferSyntaxUIDs); TransferSyntaxUIDs,
	// when set, further limits the candidates. Compressed pixel data is never
	// transcoded: without an accepted context for its syntax the
	// sub-operation fails. The file is closed after the C-STORE.
	LoadFile func(context.Context) (*object.File, error)
	// LoadStoreSource optionally replaces both dataset loaders. It must prepare a
	// writer for the first accepted TransferSyntaxUIDs candidate. Both affected
	// SOP UIDs are required. The core selects/checks storage role before loading,
	// validates the handle before sending, and calls Close exactly once (including
	// a partial handle returned with an error). No decoding is implied by core.
	LoadStoreSource func(context.Context) (OpenedStoreSource, error)
	// OnResponse observes only a correlated C-STORE response, once. Preflight,
	// role, load, write and transport errors without a response do not call it.
	// It must return promptly; observer panics do not alter protocol results.
	OnResponse func(context.Context, uint16)
}

// CGetSubOperationResult reports the final status of one same-association
// C-STORE sub-operation.
type CGetSubOperationResult struct {
	Status uint16
	Err    error
}

// CGetHandler resolves matching instances for a C-GET request.
type CGetHandler interface {
	Get(context.Context, CGetRequestContext) ([]CGetSubOperation, error)
}

// CGetHandlerFunc adapts a function to CGetHandler.
type CGetHandlerFunc func(context.Context, CGetRequestContext) ([]CGetSubOperation, error)

func (f CGetHandlerFunc) Get(ctx context.Context, req CGetRequestContext) ([]CGetSubOperation, error) {
	if f == nil {
		return nil, fmt.Errorf("dicom dimse: nil C-GET handler")
	}
	return f(ctx, req)
}

// CGetSCPError lets handlers choose a final C-GET status and Error Comment.
type CGetSCPError struct {
	Status              uint16
	ErrorComment        string
	FailedSuboperations uint16
	Err                 error
}

func NewCGetSCPError(status uint16, comment string, err error) *CGetSCPError {
	return &CGetSCPError{Status: status, ErrorComment: comment, Err: err}
}

func (e *CGetSCPError) Error() string {
	if e == nil {
		return "<nil>"
	}
	if e.Err != nil && e.ErrorComment != "" {
		return fmt.Sprintf("dicom dimse: C-GET SCP status 0x%04X: %s: %v", e.Status, e.ErrorComment, e.Err)
	}
	if e.Err != nil {
		return fmt.Sprintf("dicom dimse: C-GET SCP status 0x%04X: %v", e.Status, e.Err)
	}
	if e.ErrorComment != "" {
		return fmt.Sprintf("dicom dimse: C-GET SCP status 0x%04X: %s", e.Status, e.ErrorComment)
	}
	return fmt.Sprintf("dicom dimse: C-GET SCP status 0x%04X", e.Status)
}

func (e *CGetSCPError) Unwrap() error {
	if e == nil {
		return nil
	}
	return e.Err
}

func ServeStudyRootCGet(ctx context.Context, assoc *ul.Association, pcID byte, handler CGetHandler) error {
	if ctx == nil {
		ctx = context.Background()
	}
	if handler == nil {
		return fmt.Errorf("dicom dimse: nil C-GET handler")
	}
	releaseOperation, err := beginAssociationOperation(assoc)
	if err != nil {
		return err
	}
	defer releaseOperation()
	command, err := ReceiveCommandSet(assoc, pcID)
	if err != nil {
		return err
	}
	return serveStudyRootCGetCommand(ctx, assoc, pcID, command, handler)
}

func ServePatientRootCGet(ctx context.Context, assoc *ul.Association, pcID byte, handler CGetHandler) error {
	if ctx == nil {
		ctx = context.Background()
	}
	if handler == nil {
		return fmt.Errorf("dicom dimse: nil C-GET handler")
	}
	releaseOperation, err := beginAssociationOperation(assoc)
	if err != nil {
		return err
	}
	defer releaseOperation()
	command, err := ReceiveCommandSet(assoc, pcID)
	if err != nil {
		return err
	}
	return servePatientRootCGetCommand(ctx, assoc, pcID, command, handler)
}

func serveStudyRootCGetCommand(ctx context.Context, assoc *ul.Association, pcID byte, command *object.Object, handler CGetHandler) error {
	return serveCGetCommand(ctx, assoc, pcID, command, handler, cGetSCPModel{
		Name:        "Study Root GET",
		SOPClassUID: StudyRootGetSOPClassUID,
		Model:       QueryRetrieveModelStudyRoot,
		Level:       studyRootCGetLevel,
	})
}

func servePatientRootCGetCommand(ctx context.Context, assoc *ul.Association, pcID byte, command *object.Object, handler CGetHandler) error {
	return serveCGetCommand(ctx, assoc, pcID, command, handler, cGetSCPModel{
		Name:        "Patient Root GET",
		SOPClassUID: PatientRootGetSOPClassUID,
		Model:       QueryRetrieveModelPatientRoot,
		Level:       patientRootCGetLevel,
	})
}

type cGetSCPModel struct {
	Name        string
	SOPClassUID string
	Model       QueryRetrieveModel
	Level       func(*object.Object) (string, error)
}

func serveCGetCommand(ctx context.Context, assoc *ul.Association, pcID byte, command *object.Object, handler CGetHandler, model cGetSCPModel) error {
	if ctx == nil {
		ctx = context.Background()
	}
	if handler == nil {
		return fmt.Errorf("dicom dimse: nil C-GET handler")
	}
	syntax, err := acceptedSCPTransferSyntax(ctx, assoc, pcID, model.SOPClassUID)
	if err != nil {
		return err
	}
	req, err := ParseCGetRequest(command)
	if err != nil {
		return err
	}
	identifier, err := receiveIdentifierDataSetWithContext(ctx, assoc, pcID, syntax)
	if err != nil {
		return err
	}
	if req.AffectedSOPClassUID != model.SOPClassUID {
		err := fmt.Errorf("dicom dimse: C-GET SCP SOP Class UID %q, want %s %q", req.AffectedSOPClassUID, model.Name, model.SOPClassUID)
		_ = sendCGetResponse(ctx, assoc, pcID, *req, model.SOPClassUID, StatusCGetUnableToProcess, err.Error(), cMoveSubOperationCounts{})
		return err
	}
	negotiated := AcceptedCRetrieveExtendedNegotiation(assoc, model.SOPClassUID)
	var level string
	if model.Model != "" && cGetIdentifierCheckEnabled(ctx) {
		keys, err := ParseCGetIdentifierKeys(model.Model, identifier, negotiated)
		if err != nil {
			status := StatusCGetIdentifierDoesNotMatchSOPClass
			comment := CGetIdentifierErrorComment(err)
			if sendErr := sendCGetResponse(ctx, assoc, pcID, *req, model.SOPClassUID, status, comment, cMoveSubOperationCounts{}); sendErr != nil {
				return sendErr
			}
			return NewCGetSCPError(status, comment, err)
		}
		level = keys.Level
	} else {
		level, err = model.Level(identifier)
		if err != nil {
			// The request is at fault, not the association. A missing or
			// unknown Query/Retrieve Level does not fit the Information
			// Model, which PS3.4 C.4.3.1.4 reports as A900 (Identifier does
			// not match SOP Class); C000 would suggest an internal failure
			// worth retrying.
			status := StatusCGetIdentifierDoesNotMatchSOPClass
			comment := queryRetrieveLevelErrorComment(identifier, "C-GET")
			if sendErr := sendCGetResponse(ctx, assoc, pcID, *req, model.SOPClassUID, status, comment, cMoveSubOperationCounts{}); sendErr != nil {
				return sendErr
			}
			return NewCGetSCPError(status, comment, err)
		}
	}

	monitor := startSCPCancelMonitor(ctx, assoc, pcID, req.MessageID, ErrCGetCanceled, true)
	defer monitor.Stop()
	operationCtx := monitor.Context()
	finalComment := ""
	counts, err := runSCPHandler(operationCtx, assoc, scpControlsFromContext(ctx).CancelGrace, func(handlerCtx context.Context) (cMoveSubOperationCounts, error) {
		ops, handlerErr := handler.Get(handlerCtx, CGetRequestContext{
			Request:               *req,
			Identifier:            identifier,
			QueryRetrieveLevel:    level,
			PresentationContextID: pcID,
			IdentifierSyntax:      syntax,
			ExtendedNegotiation:   negotiated,
		})
		if handlerErr != nil {
			counts := cMoveSubOperationCounts{}
			var statusErr *CGetSCPError
			if errors.As(handlerErr, &statusErr) && statusErr != nil {
				counts.failed = statusErr.FailedSuboperations
			}
			return counts, handlerErr
		}
		if validationErr := validateCGetSubOperations(assoc, ops); validationErr != nil {
			return cMoveSubOperationCounts{}, validationErr
		}

		counts := cMoveSubOperationCounts{remaining: uint16(len(ops))}
		client := NewStoreClient(assoc)
		for i, op := range ops {
			if operationErr := monitor.OperationError(); operationErr != nil {
				return counts, operationErr
			}
			// Keep the response receive on the association/server context so an
			// already-issued C-STORE can be drained after C-CANCEL. The outer
			// runSCPHandler still enforces cancel grace and aborts a silent peer.
			result := runCGetSubOperation(handlerCtx, ctx, client, monitor, op, uint16(i+1), req.Priority)
			if errors.Is(result.Err, ErrStoreAccessDenied) {
				return counts, ErrStoreAccessDenied
			}
			if errors.Is(result.Err, ErrCGetCanceled) || errors.Is(result.Err, context.Canceled) {
				return counts, result.Err
			}
			// A C-STORE issued before cancellation may have been drained solely
			// to keep the association synchronized. Check the monitor before committing
			// counters, preserving the remaining count captured when cancellation won.
			if operationErr := monitor.OperationError(); operationErr != nil {
				return counts, operationErr
			}
			if result.Err != nil {
				finalComment = result.Err.Error()
			}
			counts.remaining--
			countCGetSubOperationResult(&counts, result, op.AffectedSOPInstanceUID)
			if operationErr := monitor.OperationError(); operationErr != nil {
				return counts, operationErr
			}
			if i < len(ops)-1 {
				if sendErr := sendCGetResponse(ctx, assoc, pcID, *req, model.SOPClassUID, StatusPending, "", counts); sendErr != nil {
					return counts, sendErr
				}
			}
		}
		return counts, monitor.OperationError()
	})
	if monitorErr := monitor.Stop(); monitorErr != nil {
		err = errors.Join(err, monitorErr)
	}
	if err != nil {
		return sendCGetOperationError(operationCtx, assoc, pcID, *req, model.SOPClassUID, counts, err)
	}

	status := StatusSuccess
	if counts.failed > 0 || counts.warning > 0 {
		status = StatusCGetSubOperationsCompleteOneOrMoreFailures
	}
	return sendCGetResponse(ctx, assoc, pcID, *req, model.SOPClassUID, status, finalComment, counts)
}

func sendCGetOperationError(ctx context.Context, assoc *ul.Association, pcID byte, req CGetRequest, sopClassUID string, counts cMoveSubOperationCounts, err error) error {
	if errors.Is(err, ErrStoreAccessDenied) {
		if sendErr := sendCGetResponse(ctx, assoc, pcID, req, sopClassUID, StatusCGetUnableToProcess, "access denied", counts); sendErr != nil {
			return sendErr
		}
		return NewCGetSCPError(StatusCGetUnableToProcess, "access denied", ErrStoreAccessDenied)
	}
	status, comment := cGetSCPStatusForError(err)
	_ = sendCGetResponse(ctx, assoc, pcID, req, sopClassUID, status, comment, counts)
	return err
}

func validateCGetSubOperations(assoc *ul.Association, ops []CGetSubOperation) error {
	if len(ops) > maxCMoveSubOperations {
		return fmt.Errorf("dicom dimse: too many C-GET sub-operations: %d exceeds %d", len(ops), maxCMoveSubOperations)
	}
	for i, op := range ops {
		if strings.TrimSpace(op.AffectedSOPClassUID) == "" && (op.LoadFile == nil || op.LoadStoreSource != nil) {
			return fmt.Errorf("dicom dimse: C-GET sub-operation %d missing Affected SOP Class UID", i)
		}
		if strings.TrimSpace(op.AffectedSOPInstanceUID) == "" {
			return fmt.Errorf("dicom dimse: C-GET sub-operation %d missing Affected SOP Instance UID", i)
		}
		if op.LoadDataSet == nil && op.LoadFile == nil && op.LoadStoreSource == nil {
			return fmt.Errorf("dicom dimse: C-GET sub-operation %d missing LoadDataSet callback", i)
		}
	}
	return nil
}

func runCGetSubOperation(loadCtx, responseCtx context.Context, client *StoreClient, monitor *scpCancelMonitor, op CGetSubOperation, messageID, priority uint16) CGetSubOperationResult {
	if client == nil || client.Assoc == nil {
		return CGetSubOperationResult{Status: StatusCGetUnableToProcess, Err: fmt.Errorf("dicom dimse: C-GET store client association is nil")}
	}
	sopClassUID := strings.TrimSpace(op.AffectedSOPClassUID)
	if sopClassUID != "" {
		if result, ok := checkCGetStorageContext(client.Assoc, sopClassUID); !ok {
			return result
		}
	}
	if op.LoadStoreSource != nil {
		return runCGetStoreSource(loadCtx, responseCtx, client, monitor, op, messageID, priority)
	}
	var dataset *object.Object
	syntaxes := op.TransferSyntaxUIDs
	if op.LoadFile != nil {
		file, err := op.LoadFile(loadCtx)
		if file != nil {
			defer file.Close()
		}
		if err != nil {
			return CGetSubOperationResult{Status: StatusCGetUnableToProcess, Err: err}
		}
		if file == nil {
			return CGetSubOperationResult{Status: StatusCGetUnableToProcess, Err: fmt.Errorf("dicom dimse: C-GET sub-operation file is nil")}
		}
		dataset = file.Dataset
		if sopClassUID == "" && dataset != nil {
			// A class learned from the instance itself is checked now.
			sopClassUID, _ = dataset.GetUID(core.NewTag(0x0008, 0x0016))
			if !core.IsValidUID(sopClassUID) {
				return CGetSubOperationResult{Status: StatusCGetUnableToProcess, Err: fmt.Errorf("dicom dimse: C-GET sub-operation data set lacks SOP Class UID")}
			}
			if result, ok := checkCGetStorageContext(client.Assoc, sopClassUID); !ok {
				return result
			}
		}
		syntaxes = StoreTransferSyntaxUIDs(file.TransferSyntax.UID, op.TransferSyntaxUIDs)
		if len(syntaxes) == 0 {
			return CGetSubOperationResult{Status: StatusCGetUnableToProcess, Err: ErrStoreTransferSyntax}
		}
	} else {
		var err error
		dataset, err = op.LoadDataSet(loadCtx)
		if err != nil {
			return CGetSubOperationResult{Status: StatusCGetUnableToProcess, Err: err}
		}
	}
	if err := context.Cause(loadCtx); err != nil {
		return CGetSubOperationResult{Status: StatusCGetUnableToProcess, Err: err}
	}
	if dataset == nil {
		return CGetSubOperationResult{Status: StatusCGetUnableToProcess, Err: fmt.Errorf("dicom dimse: C-GET sub-operation dataset is nil")}
	}
	if op.MessageID != 0 {
		messageID = op.MessageID
	}
	result, err := client.storeWithinAssociationOperation(responseCtx, dataset, CStoreOptions{
		AffectedSOPClassUID:    sopClassUID,
		AffectedSOPInstanceUID: op.AffectedSOPInstanceUID,
		TransferSyntaxUIDs:     syntaxes,
		MessageID:              messageID,
		Priority:               priority,
	}, monitor.receiveCStoreResponse)
	status := StatusSuccess
	if result.Response != nil {
		status = result.Response.Status
		observeCGetStoreResponse(op.OnResponse, responseCtx, status)
	}
	return CGetSubOperationResult{Status: status, Err: err}
}

// checkCGetStorageContext reports whether the requester accepted a context
// and the SCP role for the SOP Class, and the failed result when not.
func checkCGetStorageContext(assoc *ul.Association, sopClassUID string) (CGetSubOperationResult, bool) {
	if _, err := AcceptedContextForSOPClass(assoc, sopClassUID); err != nil {
		return CGetSubOperationResult{Status: StatusCGetUnableToProcess, Err: err}, false
	}
	if !AcceptedStorageSCPRole(assoc, sopClassUID) {
		return CGetSubOperationResult{
			Status: StatusCGetUnableToProcess,
			Err:    fmt.Errorf("dicom dimse: C-GET storage SCP role not accepted for SOP Class UID %q", sopClassUID),
		}, false
	}
	return CGetSubOperationResult{}, true
}

func countCGetSubOperationResult(counts *cMoveSubOperationCounts, result CGetSubOperationResult, sopInstanceUID string) {
	if result.Err != nil {
		counts.failed++
		counts.failedSOPInstanceUIDs = append(counts.failedSOPInstanceUIDs, sopInstanceUID)
		return
	}
	switch {
	case result.Status == StatusSuccess:
		counts.completed++
	case IsCStoreWarningStatus(result.Status):
		counts.warning++
	default:
		counts.failed++
		counts.failedSOPInstanceUIDs = append(counts.failedSOPInstanceUIDs, sopInstanceUID)
	}
}

func studyRootCGetLevel(identifier *object.Object) (string, error) {
	if identifier == nil {
		return "", fmt.Errorf("dicom dimse: missing C-GET Identifier")
	}
	level, ok := identifier.GetString(core.NewTag(0x0008, 0x0052))
	if !ok || strings.TrimSpace(level) == "" {
		return "", fmt.Errorf("dicom dimse: missing QueryRetrieveLevel")
	}
	level = strings.ToUpper(strings.TrimSpace(level))
	switch level {
	case QueryRetrieveLevelStudy, QueryRetrieveLevelSeries, QueryRetrieveLevelImage:
		return level, nil
	default:
		return "", fmt.Errorf("dicom dimse: unsupported QueryRetrieveLevel %q", level)
	}
}

func patientRootCGetLevel(identifier *object.Object) (string, error) {
	if identifier == nil {
		return "", fmt.Errorf("dicom dimse: missing C-GET Identifier")
	}
	level, ok := identifier.GetString(core.NewTag(0x0008, 0x0052))
	if !ok || strings.TrimSpace(level) == "" {
		return "", fmt.Errorf("dicom dimse: missing QueryRetrieveLevel")
	}
	level = strings.ToUpper(strings.TrimSpace(level))
	switch level {
	case QueryRetrieveLevelPatient, QueryRetrieveLevelStudy, QueryRetrieveLevelSeries, QueryRetrieveLevelImage:
		return level, nil
	default:
		return "", fmt.Errorf("dicom dimse: unsupported QueryRetrieveLevel %q", level)
	}
}

func cGetSCPStatusForError(err error) (uint16, string) {
	var statusErr *CGetSCPError
	if errors.As(err, &statusErr) {
		comment := statusErr.ErrorComment
		if comment == "" && statusErr.Err != nil {
			comment = statusErr.Err.Error()
		}
		return statusErr.Status, comment
	}
	if errors.Is(err, context.DeadlineExceeded) {
		return StatusCGetUnableToProcess, err.Error()
	}
	if errors.Is(err, ErrCGetCanceled) || errors.Is(err, context.Canceled) {
		return StatusCGetCancel, "C-GET canceled"
	}
	return StatusCGetUnableToProcess, err.Error()
}

func sendStudyRootCGetResponse(assoc *ul.Association, pcID byte, req CGetRequest, status uint16, comment string, counts cMoveSubOperationCounts) error {
	return sendCGetResponse(nil, assoc, pcID, req, StudyRootGetSOPClassUID, status, comment, counts)
}

func sendCGetResponse(ctx context.Context, assoc *ul.Association, pcID byte, req CGetRequest, sopClassUID string, status uint16, comment string, counts cMoveSubOperationCounts) error {
	return sendWithSCPResponseContext(ctx, assoc, func(responseCtx context.Context) error {
		return SendCGetResponseWithContext(responseCtx, assoc, pcID, CGetResponse{
			AffectedSOPClassUID:                 sopClassUID,
			MessageIDBeingRespondedTo:           req.MessageID,
			Status:                              status,
			ErrorComment:                        comment,
			NumberOfRemainingSuboperationsOrNil: remainingSuboperationsPtr(status, counts.remaining),
			NumberOfCompletedSuboperationsOrNil: uint16Ptr(counts.completed),
			NumberOfFailedSuboperationsOrNil:    uint16Ptr(counts.failed),
			NumberOfWarningSuboperationsOrNil:   uint16Ptr(counts.warning),
			FailedSOPInstanceUIDListOrNil:       counts.failedSOPInstanceUIDs,
		})
	})
}

func runCGetStoreSource(loadCtx, responseCtx context.Context, client *StoreClient, monitor *scpCancelMonitor, op CGetSubOperation, messageID, priority uint16) (result CGetSubOperationResult) {
	result.Status = StatusCGetUnableToProcess
	pc, err := AcceptedContextForSOPClassTransferSyntaxes(client.Assoc, op.AffectedSOPClassUID, op.TransferSyntaxUIDs)
	if err != nil {
		result.Err = err
		return
	}
	opened, err := loadCGetStoreSource(loadCtx, op.LoadStoreSource)
	if opened.Close != nil {
		defer func() {
			if closeErr := closeStoreSource(opened.Close); closeErr != nil {
				result.Err = errors.Join(result.Err, ErrStoreInvalidSource)
			}
		}()
	}
	if err != nil {
		result.Err = err
		return
	}
	if err := context.Cause(loadCtx); err != nil {
		result.Err = err
		return
	}
	descriptor, err := normalizeStoreDescriptor(opened.Descriptor)
	if err != nil || opened.Close == nil || opened.WriteDataSet == nil {
		result.Err = ErrStoreInvalidSource
		return
	}
	if descriptor.SOPClassUID != op.AffectedSOPClassUID || descriptor.SOPInstanceUID != op.AffectedSOPInstanceUID {
		result.Err = ErrStoreSourceChanged
		return
	}
	if !storeSyntaxUIDPresent(descriptor.WritableTransferSyntaxUIDs, pc.TransferSyntaxUID) {
		result.Err = ErrStoreTransferSyntax
		return
	}
	if op.MessageID != 0 {
		messageID = op.MessageID
	}
	if err := checkStoreAccess(loadCtx, opened.BeforeStore); err != nil {
		result.Err = err
		return
	}
	stored, err := client.storeEncodedWithOptionsOperation(responseCtx, opened.WriteDataSet, CStoreOptions{
		AffectedSOPClassUID: op.AffectedSOPClassUID, AffectedSOPInstanceUID: op.AffectedSOPInstanceUID,
		TransferSyntaxUIDs: []string{pc.TransferSyntaxUID}, MessageID: messageID, Priority: priority,
	}, monitor.receiveCStoreResponse, false)
	result.Err = err
	result.Status = StatusSuccess
	if stored.Response != nil {
		result.Status = stored.Response.Status
		observeCGetStoreResponse(op.OnResponse, responseCtx, result.Status)
	}
	return
}

func loadCGetStoreSource(ctx context.Context, load func(context.Context) (OpenedStoreSource, error)) (opened OpenedStoreSource, err error) {
	defer func() {
		if recover() != nil {
			err = ErrStoreInvalidSource
		}
	}()
	return load(ctx)
}

func observeCGetStoreResponse(observer func(context.Context, uint16), ctx context.Context, status uint16) {
	if observer == nil {
		return
	}
	defer func() { _ = recover() }()
	observer(ctx, status)
}
