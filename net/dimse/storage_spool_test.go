package dimse

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/binary"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/ThalesMMS/dicom-go/core"
	"github.com/ThalesMMS/dicom-go/internal/dicomtest"
	"github.com/ThalesMMS/dicom-go/net/ul"
	"github.com/ThalesMMS/dicom-go/object"
	"github.com/ThalesMMS/dicom-go/transfer"
)

// spoolTestDataSet is the head of an Explicit VR Little Endian data set
// whose last element is native Pixel Data of pixels zero bytes.
func spoolTestDataSet(instance string, pixels uint32) []byte {
	var head bytes.Buffer
	element := func(group, elem uint16, vr string, value []byte) {
		binary.Write(&head, binary.LittleEndian, [2]uint16{group, elem})
		head.WriteString(vr)
		binary.Write(&head, binary.LittleEndian, uint16(len(value)))
		head.Write(value)
	}
	uid := func(value string) []byte {
		if len(value)%2 == 1 {
			value += "\x00"
		}
		return []byte(value)
	}
	element(0x0008, 0x0016, "UI", uid(dicomtest.TestSOPClassUID))
	element(0x0008, 0x0018, "UI", uid(instance))
	binary.Write(&head, binary.LittleEndian, [2]uint16{0x7FE0, 0x0010})
	head.WriteString("OB\x00\x00")
	binary.Write(&head, binary.LittleEndian, pixels)
	return head.Bytes()
}

func liveHeap() uint64 {
	var stats runtime.MemStats
	runtime.GC()
	runtime.ReadMemStats(&stats)
	return stats.HeapAlloc
}

// spoolReception stores a data set with pixels bytes of Pixel Data through a
// Storage SCP configured with spoolDir and reports how far the live heap grew
// while the handler held the data set and wrote it back, and what the
// handler saw.
type spoolReception struct {
	status       uint16
	heapGrowth   int64
	deferred     bool
	writtenBytes int64
	writtenHash  [32]byte
}

func receiveForSpoolTest(t *testing.T, spoolDir string, pixels uint32, maxDataSetBytes int64) spoolReception {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	instance := "1.2.826.0.1.3680043.10.1989.1"
	var result spoolReception
	var base uint64
	addr, done := startStreamSCP(t, ctx, StorageSCPOptions{
		SpoolDir:        spoolDir,
		MaxDataSetBytes: maxDataSetBytes,
		StoreHandler: CStoreHandlerFunc(func(_ context.Context, req CStoreRequestContext) (uint16, error) {
			pixelData, ok := req.DataSet.Get(core.TagPixelData)
			result.deferred = ok && pixelData.Value == nil && pixelData.EncodedLength() == core.Length(pixels)
			hash := sha256.New()
			counted := &countingSpoolWriter{w: hash}
			if err := object.WriteDataSet(counted, req.DataSet, req.DataSetSyntax); err != nil {
				return StatusCStoreCannotUnderstand, err
			}
			result.writtenBytes = counted.n
			copy(result.writtenHash[:], hash.Sum(nil))
			result.heapGrowth = int64(liveHeap()) - int64(base)
			if spoolDir != "" {
				entries, _ := os.ReadDir(spoolDir)
				if len(entries) != 1 || !strings.HasPrefix(entries[0].Name(), DataSetSpoolPrefix) {
					t.Errorf("the spool directory holds %d entries during the handler, want the spool file", len(entries))
				}
			}
			return StatusSuccess, nil
		}),
	})
	assoc := dialStoreSCU(t, ctx, addr)
	base = liveHeap()
	head := spoolTestDataSet(instance, pixels)
	reception := storeRaw(t, ctx, assoc, io.MultiReader(bytes.NewReader(head), io.LimitReader(zeroReader{}, int64(pixels))), instance)
	result.status = reception.Response.Status
	_ = assoc.Release(ctx)
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	if spoolDir != "" {
		if entries, _ := os.ReadDir(spoolDir); len(entries) != 0 {
			t.Fatalf("the spool directory keeps %d entries after the response", len(entries))
		}
	}
	return result
}

type countingSpoolWriter struct {
	w io.Writer
	n int64
}

func (c *countingSpoolWriter) Write(p []byte) (int, error) {
	n, err := c.w.Write(p)
	c.n += int64(n)
	return n, err
}

// TestSpoolDirKeepsPixelDataOutOfTheHeap is #1989: with SpoolDir the handler
// receives the 64 MiB data set with its Pixel Data left in the spool file,
// writes it back byte for byte, and the live heap stays far below the data
// set; the in-memory reception holds all of it. The spool file is gone after
// the response.
func TestSpoolDirKeepsPixelDataOutOfTheHeap(t *testing.T) {
	if testing.Short() {
		t.Skip("receives 64 MiB twice")
	}
	const pixels = 64 << 20
	want := sha256.New()
	head := spoolTestDataSet("1.2.826.0.1.3680043.10.1989.1", pixels)
	want.Write(head)
	if _, err := io.Copy(want, io.LimitReader(zeroReader{}, pixels)); err != nil {
		t.Fatal(err)
	}
	var wantHash [32]byte
	copy(wantHash[:], want.Sum(nil))

	spooled := receiveForSpoolTest(t, t.TempDir(), pixels, 0)
	inMemory := receiveForSpoolTest(t, "", pixels, 0)
	t.Logf("live heap growth while the handler holds a 64 MiB data set: spooled %d KiB, in memory %d KiB", spooled.heapGrowth>>10, inMemory.heapGrowth>>10)
	if spooled.status != StatusSuccess || !spooled.deferred {
		t.Fatalf("spooled reception: status 0x%04X, Pixel Data deferred %t", spooled.status, spooled.deferred)
	}
	if spooled.writtenBytes != int64(len(head))+pixels || spooled.writtenHash != wantHash {
		t.Fatalf("the spooled data set wrote back %d bytes that differ from the %d received", spooled.writtenBytes, int64(len(head))+pixels)
	}
	if spooled.heapGrowth > pixels/8 {
		t.Fatalf("the spooled reception grew the live heap by %d MiB for a 64 MiB data set", spooled.heapGrowth>>20)
	}
	if inMemory.status != StatusSuccess || inMemory.deferred || inMemory.writtenHash != wantHash || inMemory.heapGrowth < pixels*7/8 {
		t.Fatalf("in-memory reception: status 0x%04X, deferred %t, heap growth %d MiB", inMemory.status, inMemory.deferred, inMemory.heapGrowth>>20)
	}
}

// TestSpoolDirKeepsTheDataSetLimit: a data set over MaxDataSetBytes is
// answered with Out of Resources without reaching the handler, and its spool
// file is removed.
func TestSpoolDirKeepsTheDataSetLimit(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	dir := t.TempDir()
	called := false
	addr, done := startStreamSCP(t, ctx, StorageSCPOptions{
		SpoolDir: dir, MaxDataSetBytes: 1 << 20,
		StoreHandler: CStoreHandlerFunc(func(context.Context, CStoreRequestContext) (uint16, error) {
			called = true
			return StatusSuccess, nil
		}),
	})
	assoc := dialStoreSCU(t, ctx, addr)
	instance := "1.2.826.0.1.3680043.10.1989.2"
	head := spoolTestDataSet(instance, 2<<20)
	result := storeRaw(t, ctx, assoc, io.MultiReader(bytes.NewReader(head), io.LimitReader(zeroReader{}, 2<<20)), instance)
	if result.Response.Status != StatusCStoreOutOfResources || called {
		t.Fatalf("status 0x%04X, handler called %t; want Out of Resources without the handler", result.Response.Status, called)
	}
	// The association still serves the next C-STORE.
	small := spoolTestDataSet(instance, 16)
	if result := storeRaw(t, ctx, assoc, io.MultiReader(bytes.NewReader(small), io.LimitReader(zeroReader{}, 16)), instance); result.Response.Status != StatusSuccess || !called {
		t.Fatalf("next C-STORE status 0x%04X, handler called %t", result.Response.Status, called)
	}
	_ = assoc.Release(ctx)
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	if entries, _ := os.ReadDir(dir); len(entries) != 0 {
		t.Fatalf("the spool directory keeps %d entries", len(entries))
	}
}

// TestSpoolDirThatCannotBeWrittenAnswersOutOfResources: a spool directory
// that does not exist refuses the C-STORE with Out of Resources and leaves the
// association usable.
func TestSpoolDirThatCannotBeWrittenAnswersOutOfResources(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	missing := t.TempDir() + string(os.PathSeparator) + "missing"
	addr, done := startStreamSCP(t, ctx, StorageSCPOptions{
		SpoolDir: missing,
		StoreHandler: CStoreHandlerFunc(func(context.Context, CStoreRequestContext) (uint16, error) {
			t.Error("the handler received a data set that was not spooled")
			return StatusSuccess, nil
		}),
	})
	assoc := dialStoreSCU(t, ctx, addr)
	instance := "1.2.826.0.1.3680043.10.1989.3"
	for range 2 {
		head := spoolTestDataSet(instance, 4096)
		if result := storeRaw(t, ctx, assoc, io.MultiReader(bytes.NewReader(head), io.LimitReader(zeroReader{}, 4096)), instance); result.Response.Status != StatusCStoreOutOfResources {
			t.Fatalf("status 0x%04X, want Out of Resources", result.Response.Status)
		}
	}
	_ = assoc.Release(ctx)
	if err := <-done; err != nil {
		t.Fatal(err)
	}
}

// TestCGetSpoolDirSpoolsSubOperations: a C-GET requestor with
// WithCGetSpoolDir receives each sub-operation through a spool file, which is
// removed once the sub-operation is answered.
func TestCGetSpoolDirSpoolsSubOperations(t *testing.T) {
	dir := t.TempDir()
	spooled := false
	runCGetFailedStore(t, func(ctx context.Context) context.Context { return WithCGetSpoolDir(ctx, dir) }, StatusCStoreOutOfResources,
		CGetStoreHandlerFunc(func(_ context.Context, req CGetStoreRequestContext) (uint16, error) {
			entries, _ := os.ReadDir(dir)
			spooled = len(entries) == 1 && strings.HasPrefix(entries[0].Name(), DataSetSpoolPrefix) && req.DataSet != nil
			return StatusCStoreOutOfResources, nil
		}))
	if !spooled {
		t.Fatal("the C-GET sub-operation was not received through a spool file")
	}
	if entries, _ := os.ReadDir(dir); len(entries) != 0 {
		t.Fatalf("the spool directory keeps %d entries", len(entries))
	}
}

// TestSpoolDirDefersEncapsulatedPixelData: encapsulated Pixel Data larger
// than the inline threshold also stays in the spool file, and writing the
// data set back reproduces the received fragments.
func TestSpoolDirDefersEncapsulatedPixelData(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	syntax := transfer.RLELossless
	instance := "1.2.826.0.1.3680043.10.1989.4"
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
	binary.Write(&head, binary.LittleEndian, uint32(0xFFFFFFFF))
	item := func(value []byte) {
		binary.Write(&head, binary.LittleEndian, [2]uint16{0xFFFE, 0xE000})
		binary.Write(&head, binary.LittleEndian, uint32(len(value)))
		head.Write(value)
	}
	item(nil)
	fragment := bytes.Repeat([]byte{1, 2, 3, 4}, 64<<10)
	item(fragment)
	binary.Write(&head, binary.LittleEndian, [2]uint16{0xFFFE, 0xE0DD})
	binary.Write(&head, binary.LittleEndian, uint32(0))
	sent := head.Bytes()

	listener, err := ul.Listen(ul.ListenOptions{Address: "127.0.0.1:0", Context: ctx})
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	var deferred bool
	var written []byte
	done := make(chan error, 1)
	go func() {
		defer func() { _ = listener.Close() }()
		assoc, err := listener.AcceptAssociation(ul.AcceptOptions{
			AETitle: "STORESCP", Context: ctx, MaxPDU: 4096,
			SupportedAbstractSyntaxes: []string{dicomtest.TestSOPClassUID},
			SupportedTransferSyntaxes: []string{syntax.UID},
		})
		if err != nil {
			done <- err
			return
		}
		defer func() { _ = assoc.Close() }()
		done <- ServeStorageAssociation(ctx, assoc, StorageSCPOptions{SpoolDir: dir, StoreHandler: CStoreHandlerFunc(func(_ context.Context, req CStoreRequestContext) (uint16, error) {
			pixelData, _ := req.DataSet.Get(core.TagPixelData)
			deferred = pixelData.Value == nil
			var out bytes.Buffer
			if err := object.WriteDataSet(&out, req.DataSet, req.DataSetSyntax); err != nil {
				return StatusCStoreCannotUnderstand, err
			}
			written = out.Bytes()
			return StatusSuccess, nil
		})})
	}()
	assoc, err := ul.DialContext(ctx, listener.Addr().String(), ul.DialOptions{
		CalledAETitle: "STORESCP", CallingAETitle: "STORESCU",
		Contexts: []ul.PresentationContext{{AbstractSyntaxUID: dicomtest.TestSOPClassUID, TransferSyntaxUIDs: []string{syntax.UID}}},
	})
	if err != nil {
		t.Fatal(err)
	}
	result, err := NewStoreClient(assoc).StoreEncodedWithOptions(ctx, func(_ context.Context, w io.Writer, _ transfer.Syntax) error {
		_, err := w.Write(sent)
		return err
	}, CStoreOptions{AffectedSOPClassUID: dicomtest.TestSOPClassUID, AffectedSOPInstanceUID: instance, TransferSyntaxUIDs: []string{syntax.UID}, MessageID: 5})
	if err != nil {
		t.Fatalf("C-STORE: %v", err)
	}
	if result.Response.Status != StatusSuccess {
		t.Fatalf("C-STORE status 0x%04X", result.Response.Status)
	}
	_ = assoc.Release(ctx)
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	if !deferred || !bytes.Equal(written, sent) {
		t.Fatalf("encapsulated Pixel Data deferred %t; written back %d bytes, equal to the %d received: %t", deferred, len(written), len(sent), bytes.Equal(written, sent))
	}
}

// storeThroughSpool stores one small data set through a Storage SCP with a
// spool directory and returns the directory and the C-STORE status.
func storeThroughSpool(t *testing.T, opts StorageSCPOptions) (string, uint16) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	dir := t.TempDir()
	opts.SpoolDir = dir
	opts.StoreHandler = CStoreHandlerFunc(func(context.Context, CStoreRequestContext) (uint16, error) { return StatusSuccess, nil })
	addr, done := startStreamSCP(t, ctx, opts)
	assoc := dialStoreSCU(t, ctx, addr)
	instance := "1.2.826.0.1.3680043.10.1989.5"
	head := spoolTestDataSet(instance, 4096)
	result := storeRaw(t, ctx, assoc, io.MultiReader(bytes.NewReader(head), io.LimitReader(zeroReader{}, 4096)), instance)
	_ = assoc.Release(ctx)
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	return dir, result.Response.Status
}

// TestSpoolFileHeldBrieflyIsStillRemoved: a spool file whose removal fails a
// few times, as when a scanner holds it on Windows, is removed by a retry
// instead of being left behind (#1989).
func TestSpoolFileHeldBrieflyIsStillRemoved(t *testing.T) {
	failures := 2
	attempts := 0
	restore := spoolRemove
	spoolRemove = func(path string) error {
		attempts++
		if failures > 0 {
			failures--
			return &os.PathError{Op: "remove", Path: path, Err: os.ErrPermission}
		}
		return os.Remove(path)
	}
	t.Cleanup(func() { spoolRemove = restore })
	var reported []string
	dir, status := storeThroughSpool(t, StorageSCPOptions{OnSpoolRemoveError: func(path string, _ error) { reported = append(reported, path) }})
	if status != StatusSuccess {
		t.Fatalf("status 0x%04X", status)
	}
	if entries, _ := os.ReadDir(dir); len(entries) != 0 || attempts != 3 || len(reported) != 0 {
		t.Fatalf("after %d removal attempts the spool directory keeps %d entries; reported %v", attempts, len(entries), reported)
	}
}

// TestSpoolFileThatStaysIsReported: a spool file that cannot be removed after
// the retries reaches OnSpoolRemoveError, so the application can remove it
// later, and the C-STORE still succeeds (#1989).
func TestSpoolFileThatStaysIsReported(t *testing.T) {
	restore := spoolRemove
	spoolRemove = func(path string) error {
		return &os.PathError{Op: "remove", Path: path, Err: os.ErrPermission}
	}
	t.Cleanup(func() { spoolRemove = restore })
	var reported []string
	dir, status := storeThroughSpool(t, StorageSCPOptions{OnSpoolRemoveError: func(path string, err error) {
		if err == nil {
			t.Error("the removal failure carries no error")
		}
		reported = append(reported, path)
	}})
	if status != StatusSuccess {
		t.Fatalf("status 0x%04X", status)
	}
	if len(reported) != 1 || filepath.Dir(reported[0]) != dir || !strings.HasPrefix(filepath.Base(reported[0]), DataSetSpoolPrefix) {
		t.Fatalf("reported %v, want the spool file in %s", reported, dir)
	}
}
