package main

import (
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"io"
	"net"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/ThalesMMS/dicom-go/internal/dicomtest"
	"github.com/ThalesMMS/dicom-go/net/dimse"
	"github.com/ThalesMMS/dicom-go/net/ul"
	"github.com/ThalesMMS/dicom-go/object"
	"github.com/ThalesMMS/dicom-go/parser"
	"github.com/ThalesMMS/dicom-go/transfer"
)

func TestParseArgsBitPreserving(t *testing.T) {
	opts, err := parseArgs([]string{"-bit-preserving"}, io.Discard)
	if err != nil || !opts.limits.bitPreserving {
		t.Fatalf("parseArgs(-bit-preserving) = %v, %v", opts.limits.bitPreserving, err)
	}
}

type bitPreservingReadGuard struct {
	net.Conn
	reads int
}

func (c *bitPreservingReadGuard) Read([]byte) (int, error) {
	c.reads++
	return 0, io.ErrUnexpectedEOF
}

func TestBitPreservingOversizedDataSetDoesNotDrain(t *testing.T) {
	for _, last := range []bool{false, true} {
		conn, peer := net.Pipe()
		t.Cleanup(func() { _ = conn.Close(); _ = peer.Close() })
		guard := &bitPreservingReadGuard{Conn: conn}
		assoc := &ul.Association{Conn: guard, Context: context.Background(), CalledAETitle: "STORESCP", CallingAETitle: "STORESCU"}
		limits := defaultStorescpLimits()
		limits.maxDataSetBytes = 32
		outDir := t.TempDir()
		status, err := storeBitPreserving(context.Background(), assoc,
			incomingCommand{pcID: 1, dataPrefix: make([]byte, 33), dataLast: last},
			dimse.CStoreRequest{AffectedSOPClassUID: dicomtest.TestSOPClassUID, AffectedSOPInstanceUID: dicomtest.TestSOPInstanceUID},
			ul.AcceptedContext{ID: 1, AbstractSyntaxUID: dicomtest.TestSOPClassUID}, transfer.ExplicitVRLittleEndian, outDir, limits)
		if status != statusOutOfResources || !errors.Is(err, parser.ErrMaxTotalBytesExceeded) {
			t.Errorf("last=%v: status=%04X error=%v, want byte-limit failure", last, status, err)
		}
		if guard.reads != 0 {
			t.Errorf("last=%v: read %d times after exceeding the byte limit", last, guard.reads)
		}
		entries, err := os.ReadDir(outDir)
		if err != nil || len(entries) != 0 {
			t.Fatalf("oversized stream left files: %v, %v", entries, err)
		}
	}
}

// TestBitPreservingStoresTheDataSetAsSent: with -bit-preserving the stored
// file is the File Meta header followed by the data set bytes exactly as
// sent over many PDVs; a data set whose identity differs from the command is
// refused without a file, and the association remains usable.
func TestBitPreservingStoresTheDataSetAsSent(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	data, err := dicomtest.ExplicitVRFile()
	if err != nil {
		t.Fatal(err)
	}
	raw := data[132+12+int(binary.LittleEndian.Uint32(data[132+8:])):]

	listener, err := ul.Listen(ul.ListenOptions{Address: "127.0.0.1:0", Context: ctx})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = listener.Close() }()
	outDir := t.TempDir()
	limits := defaultStorescpLimits()
	limits.bitPreserving = true
	serverDone := make(chan error, 1)
	go func() {
		assoc, err := listener.AcceptAssociation(ul.AcceptOptions{
			AETitle: "STORESCP", Context: ctx, MaxPDU: 4096, AcceptAnyAbstractSyntax: true,
			SupportedTransferSyntaxes: []string{transfer.ExplicitVRLittleEndian.UID},
		})
		if err != nil {
			serverDone <- err
			return
		}
		serverDone <- handleAssociationWithLimits(assoc, outDir, io.Discard, limits)
	}()
	assoc, err := ul.DialContext(ctx, listener.Addr().String(), ul.DialOptions{
		CalledAETitle: "STORESCP", CallingAETitle: "STORESCU",
		Contexts: []ul.PresentationContext{{AbstractSyntaxUID: dicomtest.TestSOPClassUID, TransferSyntaxUIDs: []string{transfer.ExplicitVRLittleEndian.UID}}},
	})
	if err != nil {
		t.Fatal(err)
	}
	store := func(instance string) uint16 {
		t.Helper()
		result, err := dimse.NewStoreClient(assoc).StoreEncodedWithOptions(ctx, func(_ context.Context, w io.Writer, _ transfer.Syntax) error {
			_, err := w.Write(raw)
			return err
		}, dimse.CStoreOptions{
			AffectedSOPClassUID: dicomtest.TestSOPClassUID, AffectedSOPInstanceUID: instance,
			TransferSyntaxUIDs: []string{transfer.ExplicitVRLittleEndian.UID},
		})
		if err != nil && result.Response.Status == 0 {
			t.Fatalf("store %s: %v", instance, err)
		}
		return result.Response.Status
	}
	if status := store("1.2.826.0.1.3680043.10.1050.9"); status != statusDataSetDoesNotMatch {
		t.Fatalf("mismatched identity status 0x%04X", status)
	}
	if status := store(dicomtest.TestSOPInstanceUID); status != dimse.StatusSuccess {
		t.Fatalf("status 0x%04X", status)
	}
	if err := assoc.Release(ctx); err != nil {
		t.Fatal(err)
	}
	if err := <-serverDone; err != nil {
		t.Fatal(err)
	}
	entries, _ := os.ReadDir(outDir)
	if len(entries) != 1 {
		t.Fatalf("%d entries, want the one stored instance", len(entries))
	}
	stored, err := os.ReadFile(filepath.Join(outDir, dicomtest.TestSOPInstanceUID+".dcm"))
	if err != nil {
		t.Fatal(err)
	}
	header, err := object.EncodeFileMetaHeader(dicomtest.TestSOPClassUID, dicomtest.TestSOPInstanceUID, transfer.ExplicitVRLittleEndian,
		object.FileMetaAETitles{Source: "STORESCP", Sending: "STORESCU", Receiving: "STORESCP"})
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(stored, append(header, raw...)) {
		t.Fatal("stored file is not the File Meta header followed by the data set as sent")
	}
}

func TestBitPreservingRejectsPixelDataEncodingMismatch(t *testing.T) {
	ctx := context.Background()
	dataset := object.FromElements(dicomtest.DatasetWithPixelData(), nil)
	var encoded bytes.Buffer
	if err := object.WriteDataSet(&encoded, dataset, transfer.ExplicitVRLittleEndian); err != nil {
		t.Fatal(err)
	}
	pc := ul.AcceptedContext{
		ID: 1, AbstractSyntaxUID: dicomtest.TestSOPClassUID, TransferSyntaxUID: transfer.JPEGBaseline.UID,
	}
	assoc := &ul.Association{CalledAETitle: "STORESCP", CallingAETitle: "STORESCU"}
	dir := t.TempDir()
	status, err := storeBitPreserving(ctx, assoc,
		incomingCommand{pcID: pc.ID, dataPrefix: encoded.Bytes(), dataLast: true},
		dimse.CStoreRequest{AffectedSOPClassUID: dicomtest.TestSOPClassUID, AffectedSOPInstanceUID: dicomtest.TestSOPInstanceUID},
		pc, transfer.JPEGBaseline, dir, defaultStorescpLimits())
	if err != nil || status != dimse.StatusCStoreCannotUnderstand {
		t.Errorf("bit-preserving C-STORE status = 0x%04X, error %v; want 0xC000", status, err)
	}
	if entries, err := os.ReadDir(dir); err != nil || len(entries) != 0 {
		t.Errorf("pixel mismatch left entries %v, error %v", entries, err)
	}
}
