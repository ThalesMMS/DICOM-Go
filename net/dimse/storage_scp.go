package dimse

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"

	"github.com/ThalesMMS/dicom-go/core"
	"github.com/ThalesMMS/dicom-go/internal/netstore"
	"github.com/ThalesMMS/dicom-go/net/telemetry"
	"github.com/ThalesMMS/dicom-go/net/ul"
	"github.com/ThalesMMS/dicom-go/object"
	"github.com/ThalesMMS/dicom-go/parser"
	"github.com/ThalesMMS/dicom-go/transfer"
)

const (
	StatusCStoreOutOfResources      uint16 = 0xA700
	StatusCStoreDataSetDoesNotMatch uint16 = 0xA900
	StatusCStoreCannotUnderstand    uint16 = 0xC000
	StatusCStoreCoercionWarning     uint16 = 0xB000
)

// CStoreRequestContext describes one validated C-STORE request and decoded
// dataset delivered to a Storage SCP handler.
type CStoreRequestContext struct {
	Request               CStoreRequest
	PresentationContextID byte
	PresentationContext   ul.AcceptedContext
	DataSet               *object.Object
	DataSetSyntax         transfer.Syntax
}

type CStoreHandler interface {
	Store(context.Context, CStoreRequestContext) (uint16, error)
}

type CStoreHandlerFunc func(context.Context, CStoreRequestContext) (uint16, error)

func (f CStoreHandlerFunc) Store(ctx context.Context, req CStoreRequestContext) (uint16, error) {
	if f == nil {
		return StatusCStoreCannotUnderstand, errors.New("dicom dimse: nil C-STORE handler")
	}
	return f(ctx, req)
}

// CStoreSCPError lets handlers choose the C-STORE response status and Error
// Comment while retaining an underlying application error for errors.Is and
// errors.As. A zero Status leaves the handler's separately returned status in
// effect.
type CStoreSCPError struct {
	Status       uint16
	ErrorComment string
	Err          error
}

func NewCStoreSCPError(status uint16, comment string, err error) *CStoreSCPError {
	return &CStoreSCPError{Status: status, ErrorComment: comment, Err: err}
}

func (e *CStoreSCPError) Error() string {
	if e == nil {
		return "<nil>"
	}
	if e.Err != nil && e.ErrorComment != "" {
		return fmt.Sprintf("dicom dimse: C-STORE SCP status 0x%04X: %s: %v", e.Status, e.ErrorComment, e.Err)
	}
	if e.Err != nil {
		return fmt.Sprintf("dicom dimse: C-STORE SCP status 0x%04X: %v", e.Status, e.Err)
	}
	if e.ErrorComment != "" {
		return fmt.Sprintf("dicom dimse: C-STORE SCP status 0x%04X: %s", e.Status, e.ErrorComment)
	}
	return fmt.Sprintf("dicom dimse: C-STORE SCP status 0x%04X", e.Status)
}

func (e *CStoreSCPError) Unwrap() error {
	if e == nil {
		return nil
	}
	return e.Err
}

type StorageSCPOptions struct {
	StoreHandler CStoreHandler
	// AttributeCoercion runs on an independent bounded copy before StoreHandler.
	// It may change metadata, but cannot change SOP identity or any pixel/index/
	// layout attribute (groups 0028/7FE0/60xx, recursively). Its reported changes turn
	// handler success into B000. StreamSink and unlimited depth are incompatible.
	AttributeCoercion object.AttributeCoercion
	MaxDataSetBytes   int64
	MaxElementBytes   int64
	MaxElements       int
	// MaxSequenceDepth limits combined sequence and item nesting in each
	// received data set. Zero applies parser.DefaultMaxSequenceDepth (64
	// nested sequences); a larger value raises it and a negative value
	// (parser.UnlimitedSequenceDepth) removes it. Deeper data sets are
	// refused with parser.ErrMaxDepthExceeded.
	MaxSequenceDepth      int
	MaxPixelDataBytes     int64
	MaxPixelDataFragments int
	OnCStoreResponse      func(context.Context, CStoreRequestContext, uint16)
	// StreamSink, when set, receives each C-STORE data set as its raw
	// P-DATA bytes (bit-preserving, #1050) instead of StoreHandler; only
	// MaxDataSetBytes applies to it. The handler still receives the C-STOREs
	// the sink declines.
	StreamSink CStoreStreamSink
	// SpoolDir, when set, makes the reception of each C-STORE data set for
	// StoreHandler write it to a temporary file of this directory as it
	// arrives (#1989). The handler receives a data set whose Pixel Data and
	// other values longer than SpoolInlineBytes (zero selects
	// DefaultSpoolInlineBytes) stay in that file and are read on demand;
	// object.WriteFile streams them back. The data set is valid until the
	// handler and OnCStoreResponse return, then the file is removed. The
	// limits above still apply. A deflated data set is read into memory.
	SpoolDir         string
	SpoolInlineBytes int64
	// OnSpoolRemoveError receives a spool file that could not be removed
	// after a few retries, so the application can remove it later; the
	// C-STORE response is not affected.
	OnSpoolRemoveError func(path string, err error)
}

// ServeStorageAssociation handles C-ECHO and C-STORE commands on one accepted
// association until the peer releases it or a transport/protocol error occurs.
func ServeStorageAssociation(ctx context.Context, assoc *ul.Association, opts StorageSCPOptions) error {
	if ctx == nil {
		ctx = context.Background()
	}
	if assoc == nil {
		return fmt.Errorf("dicom dimse: nil association")
	}
	if opts.StoreHandler == nil && opts.StreamSink == nil {
		return fmt.Errorf("dicom dimse: missing C-STORE handler")
	}
	if opts.AttributeCoercion != nil {
		if opts.StreamSink != nil {
			return ErrStoreInvalidOptions
		}
		if _, err := storageCoercionOptions(opts); err != nil {
			return err
		}
	}

	for {
		pcID, command, released, err := receiveStorageCommandOrRelease(ctx, assoc)
		if err != nil || released {
			return err
		}
		field, err := CommandUint16(command, CommandField)
		if err != nil {
			return err
		}
		switch field {
		case CEchoRQ:
			if err := ServeCEchoCommand(ctx, assoc, pcID, command); err != nil {
				return err
			}
		case CStoreRQ:
			if err := serveStorageCStore(ctx, assoc, pcID, command, opts); err != nil {
				return err
			}
		default:
			return fmt.Errorf("dicom dimse: unsupported Storage SCP command field 0x%04X", field)
		}
	}
}

func ValidateCStoreDataSet(affectedSOPClassUID, affectedSOPInstanceUID string, pc ul.AcceptedContext, dataset *object.Object) error {
	if dataset == nil {
		return errors.New("missing dataset")
	}
	if affectedSOPClassUID == "" {
		return errors.New("missing affected SOP Class UID")
	}
	if affectedSOPInstanceUID == "" {
		return errors.New("missing affected SOP Instance UID")
	}
	if !netstore.CStoreContextMatches(affectedSOPClassUID, pc) {
		return fmt.Errorf("presentation context SOP Class UID %q does not support C-STORE for %q", pc.AbstractSyntaxUID, affectedSOPClassUID)
	}
	if sopClassUID, ok := dataset.GetUID(core.NewTag(0x0008, 0x0016)); !ok || sopClassUID != affectedSOPClassUID {
		return errors.New("dataset SOP Class UID does not match request")
	}
	if sopInstanceUID, ok := dataset.GetUID(core.NewTag(0x0008, 0x0018)); !ok || sopInstanceUID != affectedSOPInstanceUID {
		return errors.New("dataset SOP Instance UID does not match request")
	}
	return netstore.ValidateCStoreImagePayload(affectedSOPClassUID, pc.TransferSyntaxUID, dataset)
}

func receiveStorageCommandOrRelease(ctx context.Context, assoc *ul.Association) (byte, *object.Object, bool, error) {
	var (
		pcID     byte
		havePCID bool
		buf      []byte
	)
	for {
		values, released, err := receiveStoragePDataOrRelease(ctx, assoc)
		if err != nil || released {
			return 0, nil, released, err
		}
		for i, value := range values {
			if !value.IsCommand {
				return 0, nil, false, fmt.Errorf("dicom dimse: Storage SCP expected command P-DATA")
			}
			if !havePCID {
				pcID = value.PresentationContextID
				havePCID = true
			} else if value.PresentationContextID != pcID {
				return 0, nil, false, fmt.Errorf("%w: got %d, want %d", ErrPresentationContextMismatch, value.PresentationContextID, pcID)
			}
			buf, err = appendCommandSetFragment(buf, value.Data)
			if err != nil {
				return 0, nil, false, err
			}
			if value.IsLast {
				if err := storePDataCarryoverWithContext(ctx, assoc, values[i+1:]); err != nil {
					return 0, nil, false, err
				}
				command, err := DecodeCommandSet(buf)
				if err == nil {
					observeCommand(assoc, telemetry.Inbound, pcID, command, 0)
				}
				return pcID, command, false, err
			}
		}
		ctx = ul.WithActiveTransfer(ctx)
	}
}

func receiveStoragePDataOrRelease(ctx context.Context, assoc *ul.Association) ([]ul.PDataValue, bool, error) {
	values, err := takePDataCarryoverWithContext(ctx, assoc)
	if err != nil {
		return nil, false, err
	}
	if len(values) > 0 {
		return values, false, nil
	}
	pdu, err := assoc.Receive(ctx)
	if err != nil {
		return nil, false, err
	}
	switch pdu := pdu.(type) {
	case *ul.ReleaseRQ:
		return nil, true, assoc.WritePDU(&ul.ReleaseRP{})
	case *ul.PDataTF:
		return pdu.Values, false, nil
	default:
		return nil, false, ul.UnexpectedPDUError(pdu, "waiting for P-DATA-TF or A-RELEASE-RQ")
	}
}

func serveStorageCStore(ctx context.Context, assoc *ul.Association, pcID byte, command *object.Object, opts StorageSCPOptions) error {
	req, err := ParseCStoreRequest(command)
	if err != nil {
		return err
	}
	pc, err := AcceptedContextByID(assoc, pcID)
	if err != nil {
		return err
	}
	reqCtx := CStoreRequestContext{
		Request:               *req,
		PresentationContextID: pcID,
		PresentationContext:   pc,
	}
	syntax, err := TransferSyntaxForAcceptedContext(pc)
	if err != nil {
		if opts.OnCStoreResponse != nil {
			opts.OnCStoreResponse(ctx, reqCtx, StatusCStoreCannotUnderstand)
		}
		return sendStorageCStoreResponse(ctx, assoc, pcID, req, StatusCStoreCannotUnderstand, "")
	}
	reqCtx.DataSetSyntax = syntax
	if !netstore.CStoreContextMatches(req.AffectedSOPClassUID, pc) {
		if err := drainPDataReader(newTypedPDataReaderWithContext(dataSetReadContext(ctx, assoc), assoc, pcID, false)); err != nil {
			return err
		}
		const status = StatusCStoreDataSetDoesNotMatch
		if opts.OnCStoreResponse != nil {
			opts.OnCStoreResponse(ctx, reqCtx, status)
		}
		return sendStorageCStoreResponse(ctx, assoc, pcID, req, status, "")
	}

	if opts.StreamSink != nil {
		handled, status, errorComment, err := serveStorageCStoreStream(ctx, assoc, pcID, req, reqCtx, opts)
		if err != nil {
			return err
		}
		if handled {
			if opts.OnCStoreResponse != nil {
				opts.OnCStoreResponse(ctx, reqCtx, status)
			}
			return sendStorageCStoreResponse(ctx, assoc, pcID, req, status, errorComment)
		}
		if opts.StoreHandler == nil {
			if err := drainPDataReader(newTypedPDataReaderWithContext(dataSetReadContext(ctx, assoc), assoc, pcID, false)); err != nil {
				return err
			}
			return sendStorageCStoreResponse(ctx, assoc, pcID, req, StatusCStoreOutOfResources, "")
		}
	}

	var status uint16
	errorComment := ""
	dataset, release, err := receiveStorageDataSet(ctx, assoc, pcID, syntax, opts)
	defer release()
	if err != nil {
		if !shouldDrainDataSetPDataOnError(err) {
			return err
		}
		status = storageDataSetErrorStatus(err)
	} else if err := ValidateCStoreDataSet(req.AffectedSOPClassUID, req.AffectedSOPInstanceUID, pc, dataset); err != nil {
		reqCtx.DataSet = dataset
		status = StatusCStoreDataSetDoesNotMatch
	} else if err := dataset.CheckPixelDataEncoding(syntax); err != nil {
		// Pixel Data that contradicts the negotiated syntax is never handed to
		// the handler, so it cannot be persisted under a false syntax.
		reqCtx.DataSet = dataset
		status = StatusCStoreCannotUnderstand
	} else {
		reqCtx.DataSet = dataset
		changed := false
		if opts.AttributeCoercion != nil {
			coercionOptions, _ := storageCoercionOptions(opts)
			var file *object.File
			file, changed, err = prepareStoreCoercion(ctx, func(_ context.Context, dst io.Writer, target transfer.Syntax) error {
				return object.WriteDataSet(dst, dataset, target)
			}, syntax, coercionOptions)
			if err == nil {
				err = ValidateCStoreDataSet(req.AffectedSOPClassUID, req.AffectedSOPInstanceUID, pc, file.Dataset)
				if err != nil {
					err = ErrStoreCoercionIdentity
				} else {
					reqCtx.DataSet = file.Dataset
				}
			}
		}
		if err != nil {
			status, errorComment = storageCoercionErrorStatus(err), "attribute coercion failed"
		} else {
			status, err = opts.StoreHandler.Store(ctx, reqCtx)
			status, errorComment = normalizeStoreStatus(status, err)
			if changed && err == nil && status == StatusSuccess {
				status = StatusCStoreCoercionWarning
			}
		}
	}
	if opts.OnCStoreResponse != nil {
		opts.OnCStoreResponse(ctx, reqCtx, status)
	}

	return sendStorageCStoreResponse(ctx, assoc, pcID, req, status, errorComment)
}

func storageCoercionOptions(opts StorageSCPOptions) (StoreCoercionOptions, error) {
	return normalizeStoreCoercionOptions(StoreCoercionOptions{Coercion: opts.AttributeCoercion, CloneOptions: object.CloneFileOptions{
		MaxTotalBytes: opts.MaxDataSetBytes, MaxElementBytes: opts.MaxElementBytes,
		MaxElements: opts.MaxElements, MaxSequenceDepth: opts.MaxSequenceDepth,
		MaxPixelDataBytes: opts.MaxPixelDataBytes, MaxFragments: opts.MaxPixelDataFragments,
	}})
}

func receiveStorageDataSet(ctx context.Context, assoc *ul.Association, pcID byte, syntax transfer.Syntax, opts StorageSCPOptions) (*object.Object, func(), error) {
	ctx = dataSetReadContext(ctx, assoc)
	reader := newTypedPDataReaderWithContext(ctx, assoc, pcID, false)
	maxElementBytes := opts.MaxElementBytes
	if maxElementBytes == 0 {
		maxElementBytes = opts.MaxDataSetBytes
	}
	maxPixelDataBytes := opts.MaxPixelDataBytes
	if maxPixelDataBytes == 0 {
		maxPixelDataBytes = opts.MaxDataSetBytes
	}
	readOptions := object.ReadFileOptions{
		MaxElementBytes:   maxElementBytes,
		MaxTotalBytes:     opts.MaxDataSetBytes,
		MaxElements:       opts.MaxElements,
		MaxSequenceDepth:  opts.MaxSequenceDepth,
		MaxPixelDataBytes: maxPixelDataBytes,
		MaxFragments:      opts.MaxPixelDataFragments,
	}
	var dataset *object.Object
	release := func() {}
	var err error
	if opts.SpoolDir != "" {
		dataset, release, err = spoolDataSet(reader, syntax, spoolTarget{dir: opts.SpoolDir, inline: opts.SpoolInlineBytes, onRemoveError: opts.OnSpoolRemoveError}, readOptions)
	} else {
		dataset, err = readPDataDataSet(reader, syntax, readOptions)
	}
	if err != nil {
		if shouldDrainDataSetPDataOnError(err) {
			if drainErr := drainPDataReader(reader); drainErr != nil {
				return nil, release, drainErr
			}
		}
		return nil, release, err
	}
	return dataset, release, nil
}

// ErrRejectedDataSetDrainLimit reports a data set that was rejected and then
// kept arriving past the drain budget. The association cannot be brought back
// to a message boundary, so it has to end.
var ErrRejectedDataSetDrainLimit = errors.New("dicom dimse: rejected data set exceeds the drain limit")

// minRejectedDataSetDrainBytes is the least a rejected data set may still
// send before the association is given up.
const minRejectedDataSetDrainBytes = 1 << 30

// drainPDataReader discards the rest of a data set that will not be used, so
// the next command can be read. A peer could keep such a data set going
// forever; the drain stops after the larger of what the data set had already
// delivered when it was rejected and minRejectedDataSetDrainBytes.
func drainPDataReader(reader *PDataReader) error {
	return drainPDataReaderWithBudget(reader, max(int64(reader.receivedBytes), minRejectedDataSetDrainBytes))
}

func drainPDataReaderWithBudget(reader *PDataReader, budget int64) error {
	_, err := io.CopyN(io.Discard, reader, budget+1)
	if err == io.EOF {
		// The data set ended inside the budget.
		return nil
	}
	if err != nil {
		return err
	}
	return ErrRejectedDataSetDrainLimit
}

func shouldDrainDataSetPDataOnError(err error) bool {
	if err == nil {
		return false
	}
	return !errors.Is(err, net.ErrClosed) &&
		!errors.Is(err, ul.ErrAssociationAborted) &&
		!errors.Is(err, ul.ErrAssociationTimeout) &&
		!errors.Is(err, context.Canceled) &&
		!errors.Is(err, context.DeadlineExceeded)
}

func storageDataSetErrorStatus(err error) uint16 {
	if errors.Is(err, parser.ErrMaxElementBytesExceeded) ||
		errors.Is(err, parser.ErrMaxTotalBytesExceeded) ||
		errors.Is(err, parser.ErrMaxElementsExceeded) ||
		errors.Is(err, parser.ErrMaxDepthExceeded) ||
		errors.Is(err, parser.ErrMaxPixelDataBytesExceeded) ||
		errors.Is(err, parser.ErrMaxFragmentsExceeded) ||
		errors.Is(err, ErrDataSetSpool) {
		return StatusCStoreOutOfResources
	}
	return StatusCStoreCannotUnderstand
}

func sendStorageCStoreResponse(ctx context.Context, assoc *ul.Association, pcID byte, req *CStoreRequest, status uint16, errorComment string) error {
	return sendWithSCPResponseContext(ctx, assoc, func(responseCtx context.Context) error {
		return SendCommandSetWithContext(responseCtx, assoc, pcID, CStoreResponse{
			AffectedSOPClassUID:       req.AffectedSOPClassUID,
			MessageIDBeingRespondedTo: req.MessageID,
			AffectedSOPInstanceUID:    req.AffectedSOPInstanceUID,
			Status:                    status,
			ErrorComment:              errorComment,
		}.CommandSet())
	})
}
