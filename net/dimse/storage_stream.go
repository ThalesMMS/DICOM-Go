package dimse

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"

	"github.com/ThalesMMS/dicom-go/core"
	"github.com/ThalesMMS/dicom-go/internal/netstore"
	"github.com/ThalesMMS/dicom-go/net/ul"
	"github.com/ThalesMMS/dicom-go/object"
	"github.com/ThalesMMS/dicom-go/transfer"
)

// Bit-preserving C-STORE reception (#1050), after DCMTK's
// DIMSE_createFilestream and storescp --bit-preserving: the data set's
// P-DATA bytes go to a stream as received, without building an
// *object.Object or re-encoding, so memory stays independent of the data
// set size. Only the command is checked on the way (the UIDs, the accepted
// context and the size limit); parsing the stored data set is left to the
// sink (FileStreamSink.Verify).

// CStoreStreamRequest describes one C-STORE whose data set is streamed.
type CStoreStreamRequest struct {
	Request               CStoreRequest
	PresentationContextID byte
	PresentationContext   ul.AcceptedContext
	DataSetSyntax         transfer.Syntax
	// CallingAETitle and CalledAETitle identify the association.
	CallingAETitle, CalledAETitle string
}

// FileMetaHeader is the Part 10 preamble, "DICM" and File Meta Information
// for the request (the command's SOP UIDs and the accepted syntax), to write
// before the data set bytes.
func (r CStoreStreamRequest) FileMetaHeader(aeTitles object.FileMetaAETitles) ([]byte, error) {
	return object.EncodeFileMetaHeader(r.Request.AffectedSOPClassUID, r.Request.AffectedSOPInstanceUID, r.DataSetSyntax, aeTitles)
}

// CStoreStream receives the data set bytes of one C-STORE.
type CStoreStream interface {
	io.Writer
	// Commit completes the instance after its last byte and returns the
	// C-STORE status (0 is success); a *CStoreSCPError sets the status and
	// Error Comment, as for CStoreHandler.
	Commit(ctx context.Context) (uint16, error)
	// Abort discards an incomplete instance.
	Abort()
}

// CStoreStreamSink opens the stream of each C-STORE. A nil stream with a
// nil error hands that C-STORE to the in-memory StoreHandler instead; an
// error refuses it (a *CStoreSCPError chooses the status, otherwise Out of
// Resources) and the data set is discarded.
type CStoreStreamSink interface {
	OpenCStoreStream(ctx context.Context, req CStoreStreamRequest) (CStoreStream, error)
}

// serveStorageCStoreStream receives req's data set into sink. handled is
// false when the sink declined, before any data set byte was read.
func serveStorageCStoreStream(ctx context.Context, assoc *ul.Association, pcID byte, req *CStoreRequest, reqCtx CStoreRequestContext, opts StorageSCPOptions) (handled bool, status uint16, errorComment string, err error) {
	sreq := CStoreStreamRequest{
		Request: *req, PresentationContextID: pcID, PresentationContext: reqCtx.PresentationContext,
		DataSetSyntax: reqCtx.DataSetSyntax,
	}
	if assoc != nil {
		sreq.CallingAETitle, sreq.CalledAETitle = assoc.CallingAETitle, assoc.CalledAETitle
	}
	ctx = dataSetReadContext(ctx, assoc)
	reader := newTypedPDataReaderWithContext(ctx, assoc, pcID, false)
	refuse := func(status uint16, comment string) (bool, uint16, string, error) {
		if err := drainPDataReader(reader); err != nil {
			return true, 0, "", err
		}
		return true, status, comment, nil
	}
	// The command's identity is all a stream can check before storing.
	if !core.IsValidUID(req.AffectedSOPClassUID) || !core.IsValidUID(req.AffectedSOPInstanceUID) {
		return refuse(StatusCStoreDataSetDoesNotMatch, "")
	}
	stream, err := opts.StreamSink.OpenCStoreStream(ctx, sreq)
	if err != nil {
		status, comment := streamErrorStatus(err, StatusCStoreOutOfResources)
		return refuse(status, comment)
	}
	if stream == nil {
		return false, 0, "", nil
	}
	source := io.Reader(reader)
	if opts.MaxDataSetBytes > 0 {
		source = io.LimitReader(reader, opts.MaxDataSetBytes+1)
	}
	written, copyErr := io.Copy(stream, source)
	if copyErr != nil {
		stream.Abort()
		var writeFailure *streamWriteError
		if !errors.As(copyErr, &writeFailure) && !shouldDrainDataSetPDataOnError(copyErr) {
			return true, 0, "", copyErr
		}
		if errors.As(copyErr, &writeFailure) {
			copyErr = writeFailure.err
		}
		status, comment := streamErrorStatus(copyErr, StatusCStoreOutOfResources)
		return refuse(status, comment)
	}
	if opts.MaxDataSetBytes > 0 && written > opts.MaxDataSetBytes {
		stream.Abort()
		return refuse(StatusCStoreOutOfResources, "")
	}
	status, err = stream.Commit(ctx)
	status, comment := normalizeStoreStatus(status, err)
	return true, status, comment, nil
}

// streamWriteError marks a failure of the stream, not of the association.
type streamWriteError struct{ err error }

func (e *streamWriteError) Error() string { return e.err.Error() }
func (e *streamWriteError) Unwrap() error { return e.err }

func streamErrorStatus(err error, fallback uint16) (uint16, string) {
	var scpErr *CStoreSCPError
	if errors.As(err, &scpErr) && scpErr != nil && scpErr.Status != 0 {
		return scpErr.Status, scpErr.ErrorComment
	}
	return fallback, ""
}

// normalizeStoreStatus applies the CStoreHandler status rules.
func normalizeStoreStatus(status uint16, err error) (uint16, string) {
	comment := ""
	var scpErr *CStoreSCPError
	if errors.As(err, &scpErr) && scpErr != nil {
		if scpErr.Status != 0 {
			status = scpErr.Status
		}
		comment = scpErr.ErrorComment
	}
	if err != nil && (status == 0 || status == StatusSuccess) {
		status = StatusCStoreCannotUnderstand
	} else if status == 0 {
		status = StatusSuccess
	}
	return status, comment
}

// FileStreamSink stores every C-STORE bit-preserving in Dir as
// <SOP Instance UID>.dcm (numbered on collision), with the guarantees of
// the netstore persistence (a private temporary synced and published
// atomically, never replacing a file): the File Meta Information built
// from the command and the accepted syntax, then the data set bytes exactly
// as received.
type FileStreamSink struct {
	Dir string
	// AETitles are recorded in the File Meta Information; empty fields take
	// the association's titles (Source and Receiving the called AE, Sending
	// the calling AE).
	AETitles object.FileMetaAETitles
	// Verify parses the written data set before publishing it. Parse errors
	// or Pixel Data encoding mismatches return 0xC000. A SOP identity mismatch
	// or empty CT Pixel Data returns 0xA900.
	Verify bool
	// OnStored, when set, receives the published path.
	OnStored func(ctx context.Context, path string, req CStoreStreamRequest)
}

// OpenCStoreStream implements CStoreStreamSink.
func (s FileStreamSink) OpenCStoreStream(ctx context.Context, req CStoreStreamRequest) (CStoreStream, error) {
	titles := s.AETitles
	if titles == (object.FileMetaAETitles{}) {
		titles = netstore.ReceivedAETitles(req.CalledAETitle, req.CallingAETitle)
	}
	header, err := req.FileMetaHeader(titles)
	if err != nil {
		return nil, NewCStoreSCPError(StatusCStoreCannotUnderstand, "", err)
	}
	file, err := netstore.BeginStream(ctx, s.Dir, transfer.NormalizeUID(req.Request.AffectedSOPInstanceUID))
	if err != nil {
		return nil, NewCStoreSCPError(StatusCStoreOutOfResources, "", err)
	}
	if _, err := file.Write(header); err != nil {
		return nil, NewCStoreSCPError(StatusCStoreOutOfResources, "", errors.Join(err, file.Abort()))
	}
	return &fileStream{sink: s, file: file, buffer: bufio.NewWriterSize(file, fileStreamBufferSize), req: req}, nil
}

// fileStreamBufferSize batches the PDV-sized writes into fewer system calls.
const fileStreamBufferSize = 256 << 10

type fileStream struct {
	sink   FileStreamSink
	file   *netstore.StreamFile
	buffer *bufio.Writer
	req    CStoreStreamRequest
}

func (f *fileStream) Write(p []byte) (int, error) {
	n, err := f.buffer.Write(p)
	if err != nil {
		return n, &streamWriteError{err: err}
	}
	return n, nil
}

func (f *fileStream) Abort() { _ = f.file.Abort() }

func (f *fileStream) Commit(ctx context.Context) (uint16, error) {
	if err := f.buffer.Flush(); err != nil {
		f.Abort()
		return StatusCStoreOutOfResources, NewCStoreSCPError(StatusCStoreOutOfResources, "", err)
	}
	if f.sink.Verify {
		if status, err := f.verify(); err != nil {
			f.Abort()
			return status, NewCStoreSCPError(status, "", err)
		}
	}
	path, err := f.file.Commit()
	if err != nil {
		return StatusCStoreOutOfResources, NewCStoreSCPError(StatusCStoreOutOfResources, "", err)
	}
	if f.sink.OnStored != nil {
		f.sink.OnStored(ctx, path, f.req)
	}
	return StatusSuccess, nil
}

// verify checks identity, required CT pixels and encoding before publication.
func (f *fileStream) verify() (uint16, error) {
	reader, err := f.file.ReadBack()
	if err != nil {
		return StatusCStoreOutOfResources, err
	}
	file, err := object.ReadFile(reader)
	if err != nil {
		return StatusCStoreCannotUnderstand, fmt.Errorf("dicom dimse: stored data set: %w", err)
	}
	if err := ValidateCStoreDataSet(f.req.Request.AffectedSOPClassUID, f.req.Request.AffectedSOPInstanceUID, f.req.PresentationContext, file.Dataset); err != nil {
		return StatusCStoreDataSetDoesNotMatch, err
	}
	if err := file.Dataset.CheckPixelDataEncoding(f.req.DataSetSyntax); err != nil {
		return StatusCStoreCannotUnderstand, err
	}
	return StatusSuccess, nil
}
