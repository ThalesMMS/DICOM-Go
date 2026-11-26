package dimse

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/ThalesMMS/dicom-go/core"
	"github.com/ThalesMMS/dicom-go/net/telemetry"
	"github.com/ThalesMMS/dicom-go/net/ul"
	"github.com/ThalesMMS/dicom-go/object"
	"github.com/ThalesMMS/dicom-go/transfer"
)

var ErrCGetStorageRoleNotAccepted = errors.New("dicom dimse: C-GET storage SCP role not accepted")

type CGetRequest struct {
	AffectedSOPClassUID string
	MessageID           uint16
	Priority            uint16
}

func (r CGetRequest) CommandSet() []core.Element {
	return []core.Element{
		newUIElement(AffectedSOPClassUID, r.AffectedSOPClassUID),
		newUSCommandElement(CommandField, CGetRQ),
		newUSCommandElement(MessageID, r.MessageID),
		newUSCommandElement(Priority, r.Priority),
		newUSCommandElement(CommandDataSetType, DataSetPresent),
	}
}

type CGetResponse struct {
	AffectedSOPClassUID                 string
	MessageIDBeingRespondedTo           uint16
	Status                              uint16
	ErrorComment                        string
	Identifier                          *object.Object
	NumberOfRemainingSuboperationsOrNil *uint16
	NumberOfCompletedSuboperationsOrNil *uint16
	NumberOfFailedSuboperationsOrNil    *uint16
	NumberOfWarningSuboperationsOrNil   *uint16
	FailedSOPInstanceUIDListOrNil       []string
}

func (r CGetResponse) CommandSet() []core.Element {
	datasetType := NoDataSet
	if r.identifier() != nil && cGetResponseStatusAllowsIdentifier(r.Status) {
		datasetType = DataSetPresent
	}
	elements := []core.Element{
		newUIElement(AffectedSOPClassUID, r.AffectedSOPClassUID),
		newUSCommandElement(CommandField, CGetRSP),
		newUSCommandElement(MessageIDBeingRespondedTo, r.MessageIDBeingRespondedTo),
		newUSCommandElement(CommandDataSetType, datasetType),
		newUSCommandElement(Status, r.Status),
	}
	if r.NumberOfRemainingSuboperationsOrNil != nil {
		elements = append(elements, newUSCommandElement(NumberOfRemainingSuboperations, *r.NumberOfRemainingSuboperationsOrNil))
	}
	if r.NumberOfCompletedSuboperationsOrNil != nil {
		elements = append(elements, newUSCommandElement(NumberOfCompletedSuboperations, *r.NumberOfCompletedSuboperationsOrNil))
	}
	if r.NumberOfFailedSuboperationsOrNil != nil {
		elements = append(elements, newUSCommandElement(NumberOfFailedSuboperations, *r.NumberOfFailedSuboperationsOrNil))
	}
	if r.NumberOfWarningSuboperationsOrNil != nil {
		elements = append(elements, newUSCommandElement(NumberOfWarningSuboperations, *r.NumberOfWarningSuboperationsOrNil))
	}
	if r.ErrorComment != "" {
		elements = append(elements, core.Element{
			Header: core.ElementHeader{Tag: ErrorComment, VR: core.VRLO},
			Value:  core.StringValue{r.ErrorComment},
		})
	}
	return elements
}

// identifier returns the Identifier sent with the response, including the
// Failed SOP Instance UID List when one is set.
func (r CGetResponse) identifier() *object.Object {
	return retrieveResponseIdentifier(r.Identifier, r.Status, r.FailedSOPInstanceUIDListOrNil)
}

func ParseCGetRequest(obj *object.Object) (*CGetRequest, error) {
	field, err := CommandUint16(obj, CommandField)
	if err != nil {
		return nil, err
	}
	if field != CGetRQ {
		return nil, fmt.Errorf("dicom dimse: command field 0x%04X, want C-GET-RQ 0x%04X", field, CGetRQ)
	}
	sopClassUID, err := commandUID(obj, AffectedSOPClassUID)
	if err != nil {
		return nil, err
	}
	messageID, err := CommandUint16(obj, MessageID)
	if err != nil {
		return nil, err
	}
	priority, err := CommandUint16(obj, Priority)
	if err != nil {
		return nil, err
	}
	dataSetType, err := CommandUint16(obj, CommandDataSetType)
	if err != nil {
		return nil, err
	}
	// PS3.7 Table 9.3-6: every value except 0101H denotes a dataset.
	if dataSetType == NoDataSet {
		return nil, fmt.Errorf("dicom dimse: C-GET request requires a dataset")
	}
	return &CGetRequest{AffectedSOPClassUID: sopClassUID, MessageID: messageID, Priority: priority}, nil
}

func ParseCGetResponse(obj *object.Object) (*CGetResponse, error) {
	field, err := CommandUint16(obj, CommandField)
	if err != nil {
		return nil, err
	}
	if field != CGetRSP {
		return nil, protocolViolationf("dicom dimse: command field 0x%04X, want C-GET-RSP 0x%04X", field, CGetRSP)
	}
	// Affected SOP Class UID is optional in a response (PS3.7 Table 9.1-3
	// and 9.1-4); dcm4che omits it when a retrieve fails.
	sopClassUID, _, err := optionalCommandUID(obj, AffectedSOPClassUID)
	if err != nil {
		return nil, err
	}
	msgID, err := CommandUint16(obj, MessageIDBeingRespondedTo)
	if err != nil {
		return nil, err
	}
	dataSetType, err := CommandUint16(obj, CommandDataSetType)
	if err != nil {
		return nil, err
	}
	status, err := CommandUint16(obj, Status)
	if err != nil {
		return nil, err
	}
	var identifier *object.Object
	if dataSetType != NoDataSet {
		statusClass := ClassifyCMoveStatus(status)
		if statusClass != CMoveStatusSuccess && statusClass != CMoveStatusPending {
			identifier = object.New(nil)
		}
	}
	remaining, err := optionalCommandUint16(obj, NumberOfRemainingSuboperations)
	if err != nil {
		return nil, err
	}
	completed, err := optionalCommandUint16(obj, NumberOfCompletedSuboperations)
	if err != nil {
		return nil, err
	}
	failed, err := optionalCommandUint16(obj, NumberOfFailedSuboperations)
	if err != nil {
		return nil, err
	}
	warning, err := optionalCommandUint16(obj, NumberOfWarningSuboperations)
	if err != nil {
		return nil, err
	}
	errorComment := ""
	if s, ok := obj.GetString(ErrorComment); ok {
		errorComment = s
	}
	failedUIDs, ok, err := optionalCommandUIDs(obj, FailedSOPInstanceUIDList)
	if err != nil {
		return nil, err
	}
	if !ok {
		failedUIDs = nil
	}
	return &CGetResponse{
		AffectedSOPClassUID:                 sopClassUID,
		MessageIDBeingRespondedTo:           msgID,
		Status:                              status,
		ErrorComment:                        errorComment,
		Identifier:                          identifier,
		NumberOfRemainingSuboperationsOrNil: remaining,
		NumberOfCompletedSuboperationsOrNil: completed,
		NumberOfFailedSuboperationsOrNil:    failed,
		NumberOfWarningSuboperationsOrNil:   warning,
		FailedSOPInstanceUIDListOrNil:       append([]string(nil), failedUIDs...),
	}, nil
}

func SendCGetRequest(assoc *ul.Association, pcID byte, req CGetRequest) error {
	if req.MessageID == 0 {
		return fmt.Errorf("dicom dimse: C-GET request MessageID must be non-zero")
	}
	if req.AffectedSOPClassUID == "" {
		return fmt.Errorf("dicom dimse: C-GET request AffectedSOPClassUID is required")
	}
	return SendCommandSet(assoc, pcID, req.CommandSet())
}

func ReceiveCGetRequest(assoc *ul.Association, pcID byte, identifierSyntax transfer.Syntax) (*CGetRequest, *object.Object, error) {
	command, err := ReceiveCommandSet(assoc, pcID)
	if err != nil {
		return nil, nil, err
	}
	req, err := ParseCGetRequest(command)
	if err != nil {
		return nil, nil, err
	}
	identifier, err := receiveIdentifierDataSetWithContext(nil, assoc, pcID, identifierSyntax)
	if err != nil {
		return nil, nil, err
	}
	return req, identifier, nil
}

func SendCGetResponse(assoc *ul.Association, pcID byte, rsp CGetResponse) error {
	return SendCGetResponseWithContext(nil, assoc, pcID, rsp)
}

func SendCGetResponseWithContext(ctx context.Context, assoc *ul.Association, pcID byte, rsp CGetResponse) error {
	if rsp.MessageIDBeingRespondedTo == 0 {
		return fmt.Errorf("dicom dimse: C-GET response MessageIDBeingRespondedTo must be non-zero")
	}
	if rsp.AffectedSOPClassUID == "" {
		return fmt.Errorf("dicom dimse: C-GET response AffectedSOPClassUID is required")
	}
	if rsp.Identifier != nil && !cGetResponseStatusAllowsIdentifier(rsp.Status) {
		return fmt.Errorf("dicom dimse: C-GET response identifier dataset not allowed for status 0x%04X", rsp.Status)
	}
	if err := SendCommandSetWithContext(ctx, assoc, pcID, rsp.CommandSet()); err != nil {
		return err
	}
	identifier := rsp.identifier()
	if identifier == nil || !cGetResponseStatusAllowsIdentifier(rsp.Status) {
		return nil
	}
	syntax, err := acceptedTransferSyntax(assoc, pcID)
	if err != nil {
		return err
	}
	return SendDataSetWithContext(ctx, assoc, pcID, identifier, syntax)
}

type CGetProgress struct {
	Response    *CGetResponse
	Identifier  *object.Object
	Final       bool
	StatusClass CMoveStatus
	Err         error
}

type CGetStoreRequestContext struct {
	Request               CStoreRequest
	DataSet               *object.Object
	PresentationContextID byte
	DataSetSyntax         transfer.Syntax
}

// CGetStoreHandler stores the C-STORE sub-operations of a C-GET. Its context
// carries the values of the C-GET's context but not its cancellation: a
// sub-operation whose data set has arrived completes, and its status is sent,
// before a cancellation ends the C-GET.
type CGetStoreHandler interface {
	Store(context.Context, CGetStoreRequestContext) (uint16, error)
}

type CGetStoreHandlerFunc func(context.Context, CGetStoreRequestContext) (uint16, error)

func (f CGetStoreHandlerFunc) Store(ctx context.Context, req CGetStoreRequestContext) (uint16, error) {
	if f == nil {
		return StatusCGetUnableToProcess, fmt.Errorf("dicom dimse: nil C-GET store handler")
	}
	return f(ctx, req)
}

func SendCGet(ctx context.Context, assoc *ul.Association, pcID byte, req CGetRequest, identifier *object.Object, identifierSyntax transfer.Syntax, storeHandler CGetStoreHandler) (*CGetResponse, error) {
	progress, err := SendCGetWithProgress(ctx, assoc, pcID, req, identifier, identifierSyntax, storeHandler)
	if err != nil {
		return nil, err
	}
	var last *CGetResponse
	for event := range progress {
		if event.Err != nil {
			return nil, event.Err
		}
		if event.Response != nil {
			last = event.Response
		}
	}
	if last == nil {
		return nil, fmt.Errorf("dicom dimse: C-GET: no response received")
	}
	return last, nil
}

func SendCGetWithProgress(ctx context.Context, assoc *ul.Association, pcID byte, req CGetRequest, identifier *object.Object, identifierSyntax transfer.Syntax, storeHandler CGetStoreHandler) (<-chan CGetProgress, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	if identifier == nil {
		return nil, fmt.Errorf("dicom dimse: C-GET: identifier dataset is required")
	}
	if storeHandler == nil {
		return nil, fmt.Errorf("dicom dimse: nil C-GET store handler")
	}
	if !hasAcceptedStorageSCPRole(assoc) {
		return nil, ErrCGetStorageRoleNotAccepted
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	releaseOperation, err := beginAssociationOperation(assoc)
	if err != nil {
		return nil, err
	}
	if err := SendCGetRequest(assoc, pcID, req); err != nil {
		releaseOperation()
		return nil, err
	}
	if err := SendDataSet(assoc, pcID, identifier, identifierSyntax); err != nil {
		releaseOperation()
		return nil, err
	}

	out := make(chan CGetProgress, 1)
	go func() {
		defer close(out)
		defer releaseOperation()
		// Every terminal event reaches the reader through
		// deliverRetrieveTerminal, in place of a pending response the reader
		// has not read once the context has ended (#2004).
		for {
			select {
			case <-ctx.Done():
				abortCGetAssociation(assoc)
				deliverRetrieveTerminal(ctx, out, CGetProgress{Final: true, Err: ctx.Err()})
				return
			default:
			}
			commandPCID, command, err := receiveCommandSetAnyWithContext(ctx, assoc)
			if err != nil {
				if ctx.Err() != nil {
					abortCGetAssociation(assoc)
				}
				deliverRetrieveTerminal(ctx, out, CGetProgress{Final: true, Err: err})
				return
			}
			field, err := CommandUint16(command, CommandField)
			if err != nil {
				deliverRetrieveTerminal(ctx, out, CGetProgress{Final: true, Err: err})
				return
			}
			switch field {
			case CStoreRQ:
				if err := handleCGetStoreRequest(ctx, assoc, commandPCID, command, storeHandler); err != nil {
					// A cancellation inside a sub-operation ends like one
					// between messages: the provider is told with an A-ABORT.
					if ctx.Err() != nil {
						abortCGetAssociation(assoc)
					}
					deliverRetrieveTerminal(ctx, out, CGetProgress{Final: true, Err: err})
					return
				}
			case CGetRSP:
				rsp, err := parseCGetResponseWithOptionalIdentifier(ctx, assoc, commandPCID, command)
				if err != nil {
					if ctx.Err() != nil {
						abortCGetAssociation(assoc)
					}
					deliverRetrieveTerminal(ctx, out, CGetProgress{Final: true, Err: err})
					return
				}
				cls := ClassifyCMoveStatus(rsp.Status)
				event := CGetProgress{Response: rsp, Identifier: rsp.Identifier, Final: cls != CMoveStatusPending, StatusClass: cls}
				if event.Final {
					// The operation is over: its final response is handed
					// over even when the context ends meanwhile, with no abort
					// in place of a release.
					deliverRetrieveTerminal(ctx, out, event)
					return
				}
				if !emitCGetProgress(ctx, out, event) {
					// Canceled while the reader had not taken the previous
					// event: abort as for a cancellation between messages.
					abortCGetAssociation(assoc)
					deliverRetrieveTerminal(ctx, out, CGetProgress{Final: true, Err: ctx.Err()})
					return
				}
			default:
				deliverRetrieveTerminal(ctx, out, CGetProgress{Final: true, Err: protocolViolationf("dicom dimse: C-GET received command field 0x%04X", field)})
				return
			}
		}
	}()
	return out, nil
}

func abortCGetAssociation(assoc *ul.Association) {
	abortCtx, abortCancel := context.WithTimeout(context.Background(), time.Second)
	defer abortCancel()
	_ = assoc.AbortWithContext(abortCtx, ul.AbortReasonNotSpecified)
}

func emitCGetProgress(ctx context.Context, out chan<- CGetProgress, progress CGetProgress) bool {
	select {
	case out <- progress:
		return true
	default:
	}
	retrieveHandOverBlocked()
	select {
	case <-ctx.Done():
		return false
	case out <- progress:
		return true
	}
}

type cGetMaxDataSetBytesKey struct{}

// WithCGetMaxDataSetBytes bounds the data set of each C-STORE sub-operation a
// C-GET requestor reads into memory. A larger data set is drained and its
// sub-operation answered with Out of Resources, without calling the store
// handler; a data set that keeps arriving past the drain budget ends the
// retrieve with ErrRejectedDataSetDrainLimit. Zero or less sets no bound.
func WithCGetMaxDataSetBytes(ctx context.Context, maxBytes int64) context.Context {
	if ctx == nil {
		ctx = context.Background()
	}
	if maxBytes <= 0 {
		return ctx
	}
	return context.WithValue(ctx, cGetMaxDataSetBytesKey{}, maxBytes)
}

func cGetMaxDataSetBytes(ctx context.Context) int64 {
	if ctx == nil {
		return 0
	}
	maxBytes, _ := ctx.Value(cGetMaxDataSetBytesKey{}).(int64)
	return maxBytes
}

// receiveSpooledCGetDataSet is receiveDataSetWithContextAndLimit for a C-GET
// requestor with WithCGetSpoolDir: the data set goes to a spool file as it
// arrives and its large values stay there. A data set over maxBytes, or one
// the spool cannot hold, is drained.
func receiveSpooledCGetDataSet(ctx context.Context, assoc *ul.Association, pcID byte, syntax transfer.Syntax, dir string, maxBytes int64) (*object.Object, func(), error) {
	ctx = dataSetReadContext(ctx, assoc)
	reader := newTypedPDataReaderWithContext(ctx, assoc, pcID, false)
	dataset, release, err := spoolDataSet(reader, syntax, spoolTarget{dir: dir, onRemoveError: cGetSpoolRemoveError(ctx)}, object.ReadFileOptions{
		MaxElementBytes: maxBytes,
		MaxTotalBytes:   maxBytes,
	})
	if err != nil {
		if shouldDrainDataSetPDataOnError(err) {
			if drainErr := drainPDataReader(reader); drainErr != nil {
				return nil, release, fmt.Errorf("dicom dimse: drain rejected dataset: %w", drainErr)
			}
		}
		return nil, release, fmt.Errorf("dicom dimse: receive dataset: %w", err)
	}
	return dataset, release, nil
}

func handleCGetStoreRequest(ctx context.Context, assoc *ul.Association, pcID byte, command *object.Object, storeHandler CGetStoreHandler) error {
	req, err := ParseCStoreRequest(command)
	if err != nil {
		return err
	}
	pc, err := AcceptedContextByID(assoc, pcID)
	if err != nil {
		return err
	}
	syntax, err := TransferSyntaxForAcceptedContext(pc)
	if err != nil {
		return err
	}
	var dataset *object.Object
	if spoolDir := cGetSpoolDir(ctx); spoolDir != "" {
		var release func()
		dataset, release, err = receiveSpooledCGetDataSet(ctx, assoc, pcID, syntax, spoolDir, cGetMaxDataSetBytes(ctx))
		defer release()
	} else {
		dataset, err = receiveDataSetWithContextAndLimit(ctx, assoc, pcID, syntax, cGetMaxDataSetBytes(ctx))
	}
	if err != nil && storageDataSetErrorStatus(err) != StatusCStoreOutOfResources {
		return err
	}
	var status uint16
	var handlerErr error
	if err != nil {
		// The data set passed a resource limit and was drained to its end:
		// this sub-operation fails and the retrieve goes on.
		status = StatusCStoreOutOfResources
	} else if identityErr := ValidateCStoreDataSet(req.AffectedSOPClassUID, req.AffectedSOPInstanceUID, pc, dataset); identityErr != nil {
		status = StatusCStoreDataSetDoesNotMatch
	} else if encodingErr := dataset.CheckPixelDataEncoding(syntax); encodingErr != nil {
		status, handlerErr = StatusCStoreCannotUnderstand, encodingErr
	} else {
		// The data set is in: this sub-operation is performed even when the
		// C-GET is canceled meanwhile, and answered with its real storage
		// status (#1994). A C-GET cancellation (PS3.4 C.4.3) stops the
		// C-STORE sub-operations still to come; the C-STORE statuses (PS3.4
		// B.2.3) have no Cancel, and a Failure such as C000 (Cannot
		// understand) would claim a defect of an intact object and make the
		// provider count it failed. The handler's context keeps the values
		// of ctx but not its cancellation; the cancellation takes effect
		// once the response is sent.
		status, handlerErr = storeHandler.Store(context.WithoutCancel(ctx), CGetStoreRequestContext{
			Request:               *req,
			DataSet:               dataset,
			PresentationContextID: pcID,
			DataSetSyntax:         syntax,
		})
	}
	if handlerErr != nil && status == StatusSuccess {
		status = StatusCGetUnableToProcess
	}
	// The response is written without the cancellation and the deadline of
	// ctx and of the association's context, which may be the same: they
	// would keep the real status from the provider, or cut the response
	// inside its PDU, with the A-ABORT that follows inside it (#2004). The
	// write still ends cGetStoreResponseGrace after either of them ends.
	respondCtx, stopRespond := cGetStoreResponseContext(ctx, assoc)
	defer stopRespond()
	if err := SendCStoreResponseWithContext(respondCtx, assoc, pcID, CStoreResponse{
		AffectedSOPClassUID:       req.AffectedSOPClassUID,
		MessageIDBeingRespondedTo: req.MessageID,
		AffectedSOPInstanceUID:    req.AffectedSOPInstanceUID,
		Status:                    status,
	}); err != nil {
		return err
	}
	return nil
}

// cGetStoreResponseGrace bounds the write of the response of a performed
// C-GET sub-operation once the operation's or the association's context has
// ended.
const cGetStoreResponseGrace = time.Second

// cGetStoreResponseContext returns a context with the values of ctx, and its
// release. It ends cGetStoreResponseGrace after the first of ctx and the
// association's context ends, counted from the call when one has already
// ended.
func cGetStoreResponseContext(ctx context.Context, assoc *ul.Association) (context.Context, context.CancelFunc) {
	respond, cancel := context.WithCancel(context.WithoutCancel(ctx))
	var mu sync.Mutex
	var grace *time.Timer
	released := false
	startGrace := func() {
		mu.Lock()
		defer mu.Unlock()
		if !released && grace == nil {
			grace = time.AfterFunc(cGetStoreResponseGrace, cancel)
		}
	}
	stops := []func() bool{context.AfterFunc(ctx, startGrace)}
	if assoc != nil && assoc.Context != nil && assoc.Context != ctx {
		stops = append(stops, context.AfterFunc(assoc.Context, startGrace))
	}
	return respond, func() {
		for _, stop := range stops {
			stop()
		}
		mu.Lock()
		released = true
		if grace != nil {
			grace.Stop()
		}
		mu.Unlock()
		cancel()
	}
}

func parseCGetResponseWithOptionalIdentifier(ctx context.Context, assoc *ul.Association, pcID byte, command *object.Object) (*CGetResponse, error) {
	rsp, err := ParseCGetResponse(command)
	if err != nil {
		return nil, err
	}
	if rsp.Identifier == nil {
		return rsp, nil
	}
	syntax, err := acceptedTransferSyntax(assoc, pcID)
	if err != nil {
		return nil, err
	}
	rsp.Identifier, err = receiveIdentifierDataSetWithContext(ctx, assoc, pcID, syntax)
	if err != nil {
		return nil, err
	}
	if failed, ok := failedSOPInstanceUIDsFromIdentifier(rsp.Identifier); ok {
		rsp.FailedSOPInstanceUIDListOrNil = failed
	}
	return rsp, nil
}

func receiveCommandSetAnyWithContext(ctx context.Context, assoc *ul.Association) (byte, *object.Object, error) {
	var (
		pcID     byte
		havePCID bool
		buf      []byte
	)
	for {
		values, err := receivePDataValues(ctx, assoc)
		if err != nil {
			return 0, nil, err
		}
		for i, value := range values {
			if !value.IsCommand {
				return 0, nil, fmt.Errorf("dicom dimse: C-GET expected command P-DATA")
			}
			if !havePCID {
				pcID = value.PresentationContextID
				havePCID = true
			} else if value.PresentationContextID != pcID {
				return 0, nil, fmt.Errorf("%w: got %d, want %d", ErrPresentationContextMismatch, value.PresentationContextID, pcID)
			}
			buf, err = appendCommandSetFragment(buf, value.Data)
			if err != nil {
				return 0, nil, err
			}
			if value.IsLast {
				if err := storePDataCarryoverWithContext(ctx, assoc, values[i+1:]); err != nil {
					return 0, nil, err
				}
				command, err := DecodeCommandSet(buf)
				if err != nil {
					return 0, nil, err
				}
				observeCommand(assoc, telemetry.Inbound, pcID, command, 0)
				return pcID, command, nil
			}
		}
		ctx = ul.WithActiveTransfer(ctx)
	}
}

func hasAcceptedStorageSCPRole(assoc *ul.Association) bool {
	if assoc == nil {
		return false
	}
	acceptedBySOP := make(map[string]struct{}, len(assoc.AcceptedContexts))
	for _, context := range assoc.AcceptedContexts {
		if context.AbstractSyntaxUID != StudyRootGetSOPClassUID {
			acceptedBySOP[context.AbstractSyntaxUID] = struct{}{}
		}
	}
	for _, role := range assoc.AcceptedRoleSelections {
		if !role.SCPRole {
			continue
		}
		if _, ok := acceptedBySOP[role.SopClassUID]; ok {
			return true
		}
	}
	return false
}

func cGetResponseStatusAllowsIdentifier(status uint16) bool {
	switch ClassifyCMoveStatus(status) {
	case CMoveStatusCancel, CMoveStatusWarning, CMoveStatusFailure:
		return true
	default:
		return false
	}
}
