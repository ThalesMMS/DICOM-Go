package main

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"io"

	"github.com/ThalesMMS/dicom-go/internal/clireadopts"
	"github.com/ThalesMMS/dicom-go/internal/netstore"
	"github.com/ThalesMMS/dicom-go/net/dimse"
	"github.com/ThalesMMS/dicom-go/net/ul"
	"github.com/ThalesMMS/dicom-go/object"
	"github.com/ThalesMMS/dicom-go/parser"
	"github.com/ThalesMMS/dicom-go/transfer"
)

// storeBitPreserving is -bit-preserving (#1050), after DCMTK's storescp
// option of the same name: the File Meta header built from the command and
// the accepted syntax, then the data set bytes exactly as received, go to a
// private temporary without building the data set in memory. The stored
// file is parsed within the dataset limits and its identity checked against
// the command before publication, as the re-encoding path does. The status
// answers the C-STORE; a returned error ends the association.
func storeBitPreserving(ctx context.Context, assoc *ul.Association, incoming incomingCommand, req dimse.CStoreRequest, pc ul.AcceptedContext, syntax transfer.Syntax, outDir string, limits storescpLimits) (uint16, error) {
	if incoming.dataErr != nil {
		return dataSetErrorStatus(incoming.dataErr), incoming.dataErr
	}
	refuse := func(status uint16) (uint16, error) {
		return status, discardDataSetWithLimits(assoc, incoming, limits)
	}
	if !netstore.CStoreContextMatches(req.AffectedSOPClassUID, pc) {
		return refuse(statusDataSetDoesNotMatch)
	}
	// This SCP (the called AE) received the instance from the calling AE.
	titles := netstore.ReceivedAETitles(assoc.CalledAETitle, assoc.CallingAETitle)
	header, err := object.EncodeFileMetaHeader(req.AffectedSOPClassUID, req.AffectedSOPInstanceUID, syntax, titles)
	if err != nil {
		return refuse(statusDataSetDoesNotMatch)
	}
	stream, err := netstore.BeginStream(ctx, outDir, transfer.NormalizeUID(req.AffectedSOPInstanceUID))
	if err != nil {
		return refuse(statusOutOfResources)
	}
	source := &readTracker{reader: bytes.NewReader(incoming.dataPrefix)}
	if !incoming.dataLast {
		source.reader = io.MultiReader(source.reader, dimse.NewPDataReader(assoc, incoming.pcID))
	}
	buffered := bufio.NewWriterSize(stream, 256<<10)
	_, _ = buffered.Write(header) // a write failure resurfaces at Flush
	written, copyErr := io.Copy(buffered, io.LimitReader(source, limits.maxDataSetBytes+1))
	if copyErr == nil {
		copyErr = buffered.Flush()
	}
	switch {
	case written > limits.maxDataSetBytes:
		_ = stream.Abort()
		return statusOutOfResources, parser.ErrMaxTotalBytesExceeded
	case source.err != nil:
		// The association failed; nothing more can be answered on it.
		_ = stream.Abort()
		return statusOutOfResources, source.err
	case copyErr != nil:
		_ = stream.Abort()
		return statusOutOfResources, drainPData(incoming, source)
	}
	var prepared *storagePrepared
	prepare := func(dataset *object.Object) error {
		if limits.storageRuntime == nil {
			return nil
		}
		var err error
		prepared, err = limits.storageRuntime.prepare(ctx, dataset, assoc, req)
		if err != nil {
			return err
		}
		return prepared.format(ctx, dataset)
	}
	if status := verifyStoredStreamPrepared(ctx, stream, req, pc, limits, int64(len(header)), prepare); status != dimse.StatusSuccess {
		prepared.abort()
		_ = stream.Abort()
		return status, nil
	}
	var path string
	if prepared == nil {
		path, err = stream.Commit()
	} else {
		path, err = stream.CommitRelative(prepared.relative)
	}
	if err != nil {
		prepared.abort()
		return statusOutOfResources, nil
	}
	prepared.stored(path)
	return dimse.StatusSuccess, nil
}

// verifyStoredStream parses the written file within the limits and compares
// its identity with the command and the accepted context.
func verifyStoredStream(ctx context.Context, stream *netstore.StreamFile, req dimse.CStoreRequest, pc ul.AcceptedContext, limits storescpLimits, headerBytes int64) uint16 {
	return verifyStoredStreamPrepared(ctx, stream, req, pc, limits, headerBytes, nil)
}
func verifyStoredStreamPrepared(ctx context.Context, stream *netstore.StreamFile, req dimse.CStoreRequest, pc ul.AcceptedContext, limits storescpLimits, headerBytes int64, prepare func(*object.Object) error) uint16 {
	reader, err := stream.ReadBack()
	if err != nil {
		return statusOutOfResources
	}
	read := limits.readPolicy.Apply(object.ReadFileOptions{
		MaxElementBytes:   limits.maxElementBytes,
		MaxTotalBytes:     limits.maxDataSetBytes + headerBytes,
		MaxElements:       limits.maxElements,
		MaxSequenceDepth:  limits.maxSequenceDepth,
		MaxPixelDataBytes: limits.maxPixelDataBytes,
		MaxFragments:      limits.maxPixelDataFragments,
	})
	file, err := object.ReadFileWithOptions(reader, read)
	if err != nil {
		return dataSetErrorStatus(err)
	}
	defer file.Close()
	// Both reads belong to this same private, uncommitted StreamFile. No writes
	// occur between verification and Commit; arbitrary readers are not replayed.
	report, err := clireadopts.CollectFileReplay(ctx, file, limits.readPolicy, read, clireadopts.MetaReplay{
		Open: stream.ReadBack, Check: func() error { return nil },
	})
	if err != nil {
		return dataSetErrorStatus(err)
	}
	if err := limits.readSink.Emit("storescp", report); err != nil {
		return statusOutOfResources
	}
	if netstore.ValidateCStoreDataSet(req.AffectedSOPClassUID, req.AffectedSOPInstanceUID, pc, file.Dataset) != nil {
		return statusDataSetDoesNotMatch
	}
	if err := file.Dataset.CheckPixelDataEncoding(file.TransferSyntax); err != nil {
		return statusCannotUnderstand
	}
	if prepare != nil {
		if err := prepare(file.Dataset); err != nil {
			return storageFeatureErrorStatus(err)
		}
	}
	return dimse.StatusSuccess
}

// drainPData reads the rest of a refused data set so the association stays
// usable.
func drainPData(incoming incomingCommand, source *readTracker) error {
	if incoming.dataLast {
		return nil
	}
	if _, err := io.Copy(io.Discard, source); err != nil && !errors.Is(err, io.EOF) {
		return err
	}
	return nil
}

// readTracker remembers a read failure (other than EOF), to tell a failed
// association from a failed write.
type readTracker struct {
	reader io.Reader
	err    error
}

func (r *readTracker) Read(p []byte) (int, error) {
	n, err := r.reader.Read(p)
	if err != nil && !errors.Is(err, io.EOF) && r.err == nil {
		r.err = err
	}
	return n, err
}
