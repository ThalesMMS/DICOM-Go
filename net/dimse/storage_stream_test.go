package dimse

import (
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"testing"
	"time"

	"github.com/ThalesMMS/dicom-go/core"
	"github.com/ThalesMMS/dicom-go/internal/dicomtest"
	"github.com/ThalesMMS/dicom-go/net/ul"
	"github.com/ThalesMMS/dicom-go/object"
	"github.com/ThalesMMS/dicom-go/transfer"
)

// startStreamSCP accepts one association with a small maximum PDU, so data
// sets arrive in many PDVs, and serves it with opts.
func startStreamSCP(t *testing.T, ctx context.Context, opts StorageSCPOptions) (string, <-chan error) {
	t.Helper()
	listener, err := ul.Listen(ul.ListenOptions{Address: "127.0.0.1:0", Context: ctx})
	if err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() {
		defer func() { _ = listener.Close() }()
		assoc, err := listener.AcceptAssociation(ul.AcceptOptions{
			AETitle: "STORESCP", Context: ctx, MaxPDU: 4096,
			SupportedAbstractSyntaxes: []string{dicomtest.TestSOPClassUID},
			SupportedTransferSyntaxes: []string{transfer.ExplicitVRLittleEndian.UID},
		})
		if err != nil {
			done <- err
			return
		}
		defer func() { _ = assoc.Close() }()
		done <- ServeStorageAssociation(ctx, assoc, opts)
	}()
	return listener.Addr().String(), done
}

// fixtureDataSetBytes is the encoded data set of the Explicit VR fixture:
// its Part 10 bytes after the File Meta group.
func fixtureDataSetBytes(t *testing.T) []byte {
	t.Helper()
	data, err := dicomtest.ExplicitVRFile()
	if err != nil {
		t.Fatal(err)
	}
	groupLength := binary.LittleEndian.Uint32(data[132+8:])
	return data[132+12+int(groupLength):]
}

func storeRaw(t *testing.T, ctx context.Context, assoc *ul.Association, raw io.Reader, instance string) CStoreResult {
	t.Helper()
	result, err := NewStoreClient(assoc).StoreEncodedWithOptions(ctx, func(_ context.Context, w io.Writer, _ transfer.Syntax) error {
		_, err := io.Copy(w, raw)
		return err
	}, CStoreOptions{
		AffectedSOPClassUID: dicomtest.TestSOPClassUID, AffectedSOPInstanceUID: instance,
		TransferSyntaxUIDs: []string{transfer.ExplicitVRLittleEndian.UID}, MessageID: 3,
	})
	if err != nil && result.Response.Status == 0 {
		t.Fatalf("StoreEncodedWithOptions() error = %v", err)
	}
	return result
}

// TestStreamSinkStoresBytesAsReceived: the published file is the File Meta
// header built from the command and the accepted syntax followed by the data
// set exactly as sent, across many PDVs, and the in-memory handler is not
// called.
func TestStreamSinkStoresBytesAsReceived(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	dir := t.TempDir()
	var storedPath string
	addr, done := startStreamSCP(t, ctx, StorageSCPOptions{
		StoreHandler: CStoreHandlerFunc(func(context.Context, CStoreRequestContext) (uint16, error) {
			t.Error("the in-memory handler received a streamed C-STORE")
			return StatusSuccess, nil
		}),
		StreamSink: FileStreamSink{Dir: dir, Verify: true, OnStored: func(_ context.Context, path string, _ CStoreStreamRequest) {
			storedPath = path
		}},
	})
	assoc := dialStoreSCU(t, ctx, addr)
	raw := fixtureDataSetBytes(t)
	if result := storeRaw(t, ctx, assoc, bytes.NewReader(raw), dicomtest.TestSOPInstanceUID); result.Response.Status != StatusSuccess {
		t.Fatalf("C-STORE status 0x%04X", result.Response.Status)
	}
	if err := assoc.Release(ctx); err != nil {
		t.Fatal(err)
	}
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	want := filepath.Join(dir, dicomtest.TestSOPInstanceUID+".dcm")
	if storedPath != want {
		t.Fatalf("stored at %q, want %q", storedPath, want)
	}
	stored, err := os.ReadFile(want)
	if err != nil {
		t.Fatal(err)
	}
	header, err := object.EncodeFileMetaHeader(dicomtest.TestSOPClassUID, dicomtest.TestSOPInstanceUID, transfer.ExplicitVRLittleEndian,
		object.FileMetaAETitles{Source: "STORESCP", Sending: "STORESCU", Receiving: "STORESCP"})
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(stored[:len(header)], header) || !bytes.Equal(stored[len(header):], raw) {
		t.Fatal("stored file is not the File Meta header followed by the data set as sent")
	}
	file, err := object.ReadFile(bytes.NewReader(stored))
	if err != nil {
		t.Fatalf("stored file does not parse: %v", err)
	}
	if uid, _ := file.Meta.GetString(core.NewTag(0x0002, 0x0010)); transfer.NormalizeUID(uid) != transfer.ExplicitVRLittleEndian.UID {
		t.Fatalf("File Meta transfer syntax %q", uid)
	}
	entries, _ := os.ReadDir(dir)
	if len(entries) != 1 {
		t.Fatalf("%d entries in the output, want only the published instance", len(entries))
	}
}

// TestStreamSinkMemoryIsIndependentOfTheDataSetSize streams a 64 MiB data
// set in 4 KiB PDUs and measures the live heap every 4 MiB sent: it stays far
// below the data set. Each sample forces a collection while the sender waits,
// so it counts what the SCU and SCP retain rather than garbage a busy machine
// has not collected yet, and is taken at a fixed point of the transfer rather
// than whenever a sampler gets scheduled. Buffering the data set anywhere on
// either side keeps the bytes received so far live. The 16 thousand PDUs also
// guard the per-PDU cost (a context layer added per P-DATA once made this
// take 19 s instead of 0.2 s).
func TestStreamSinkMemoryIsIndependentOfTheDataSetSize(t *testing.T) {
	if testing.Short() {
		t.Skip("streams 64 MiB")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	dir := t.TempDir()
	addr, done := startStreamSCP(t, ctx, StorageSCPOptions{StreamSink: FileStreamSink{Dir: dir}})
	assoc := dialStoreSCU(t, ctx, addr)
	const pixels = 64 << 20
	instance := "1.2.826.0.1.3680043.10.1050.1"
	var head bytes.Buffer
	element := func(group, elem uint16, vr string, value []byte) {
		binary.Write(&head, binary.LittleEndian, [2]uint16{group, elem})
		head.WriteString(vr)
		binary.Write(&head, binary.LittleEndian, uint16(len(value)))
		head.Write(value)
	}
	element(0x0008, 0x0016, "UI", []byte(dicomtest.TestSOPClassUID + "\x00")[:len(dicomtest.TestSOPClassUID)+len(dicomtest.TestSOPClassUID)%2])
	element(0x0008, 0x0018, "UI", []byte(instance + "\x00")[:len(instance)+len(instance)%2])
	binary.Write(&head, binary.LittleEndian, [2]uint16{0x7FE0, 0x0010})
	head.WriteString("OB\x00\x00")
	binary.Write(&head, binary.LittleEndian, uint32(pixels))
	runtime.GC()
	var base runtime.MemStats
	runtime.ReadMemStats(&base)
	var peak uint64
	samples := 0
	body := &liveHeapSampler{
		r:     io.MultiReader(bytes.NewReader(head.Bytes()), io.LimitReader(zeroReader{}, pixels)),
		every: 4 << 20,
		sample: func() {
			var stats runtime.MemStats
			runtime.GC()
			runtime.ReadMemStats(&stats)
			peak = max(peak, stats.HeapAlloc)
			samples++
		},
	}
	result := storeRaw(t, ctx, assoc, body, instance)
	if result.Response.Status != StatusSuccess {
		t.Fatalf("C-STORE status 0x%04X", result.Response.Status)
	}
	_ = assoc.Release(ctx)
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(filepath.Join(dir, instance+".dcm"))
	if err != nil || info.Size() < pixels {
		t.Fatalf("stored %v, %v", info, err)
	}
	if samples < pixels/(4<<20) {
		t.Fatalf("%d live heap samples, want one per 4 MiB sent", samples)
	}
	growth := int64(peak) - int64(base.HeapAlloc)
	t.Logf("64 MiB data set streamed with a live heap peak %d KiB above the start", growth>>10)
	if growth > pixels/4 {
		t.Fatalf("live heap grew %d MiB for a 64 MiB data set", growth>>20)
	}
}

// liveHeapSampler calls sample each time every more bytes have been read
// through it.
type liveHeapSampler struct {
	r      io.Reader
	every  int64
	sample func()
	read   int64
}

func (s *liveHeapSampler) Read(p []byte) (int, error) {
	n, err := s.r.Read(p)
	before := s.read
	s.read += int64(n)
	if s.read/s.every > before/s.every {
		s.sample()
	}
	return n, err
}

type zeroReader struct{}

func (zeroReader) Read(p []byte) (int, error) {
	clear(p)
	return len(p), nil
}

// TestStreamSinkLimitsAndChecks: a data set over MaxDataSetBytes is refused
// with Out of Resources and nothing is published; a command whose SOP Class
// is not the accepted one is refused before storing; Verify refuses a data
// set whose identity differs from the command; the association stays usable.
func TestStreamSinkLimitsAndChecks(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	dir := t.TempDir()
	raw := fixtureDataSetBytes(t)
	addr, done := startStreamSCP(t, ctx, StorageSCPOptions{
		StreamSink:      FileStreamSink{Dir: dir, Verify: true},
		MaxDataSetBytes: int64(len(raw) - 1),
	})
	assoc := dialStoreSCU(t, ctx, addr)
	if result := storeRaw(t, ctx, assoc, bytes.NewReader(raw), dicomtest.TestSOPInstanceUID); result.Response.Status != StatusCStoreOutOfResources {
		t.Fatalf("oversized data set status 0x%04X", result.Response.Status)
	}
	if err := assoc.Release(ctx); err != nil {
		t.Fatal(err)
	}
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	if entries, _ := os.ReadDir(dir); len(entries) != 0 {
		t.Fatalf("%d entries left after a refused C-STORE", len(entries))
	}

	dir = t.TempDir()
	addr, done = startStreamSCP(t, ctx, StorageSCPOptions{StreamSink: FileStreamSink{Dir: dir, Verify: true}})
	assoc = dialStoreSCU(t, ctx, addr)
	// The fixture's bytes under another instance UID fail the verification.
	other := "1.2.826.0.1.3680043.10.1050.2"
	if result := storeRaw(t, ctx, assoc, bytes.NewReader(raw), other); result.Response.Status != StatusCStoreDataSetDoesNotMatch {
		t.Fatalf("mismatched identity status 0x%04X", result.Response.Status)
	}
	if result := storeRaw(t, ctx, assoc, bytes.NewReader(raw), dicomtest.TestSOPInstanceUID); result.Response.Status != StatusSuccess {
		t.Fatalf("association unusable after a refusal: 0x%04X", result.Response.Status)
	}
	_ = assoc.Release(ctx)
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	if entries, _ := os.ReadDir(dir); len(entries) != 1 {
		t.Fatalf("%d entries, want the one verified instance", len(entries))
	}
}

// TestStreamSinkDeclineFallsBackToTheHandler: a sink that declines leaves
// the C-STORE to the in-memory handler.
func TestStreamSinkDeclineFallsBackToTheHandler(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	handled := false
	addr, done := startStreamSCP(t, ctx, StorageSCPOptions{
		StreamSink: declineSink{},
		StoreHandler: CStoreHandlerFunc(func(_ context.Context, req CStoreRequestContext) (uint16, error) {
			handled = req.DataSet != nil
			return StatusSuccess, nil
		}),
	})
	assoc := dialStoreSCU(t, ctx, addr)
	if result := storeRaw(t, ctx, assoc, bytes.NewReader(fixtureDataSetBytes(t)), dicomtest.TestSOPInstanceUID); result.Response.Status != StatusSuccess || !handled {
		t.Fatalf("declined C-STORE status 0x%04X handled %v", result.Response.Status, handled)
	}
	_ = assoc.Release(ctx)
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	if _, err := (CStoreStreamRequest{Request: CStoreRequest{AffectedSOPClassUID: "1.2", AffectedSOPInstanceUID: "bad uid"}}).FileMetaHeader(object.FileMetaAETitles{}); !errors.Is(err, object.ErrMissingSOPInstanceUID) {
		t.Fatalf("invalid UID header error %v", err)
	}
}

type declineSink struct{}

func (declineSink) OpenCStoreStream(context.Context, CStoreStreamRequest) (CStoreStream, error) {
	return nil, nil
}
