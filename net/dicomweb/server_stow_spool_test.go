package dicomweb

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/ThalesMMS/dicom-go/transfer"
)

// errSpoolFault is what a faultySpool operation fails with, as a full disk
// would.
var errSpoolFault = errors.New("synthetic spool failure")

// faultySpool is a real spool file whose first call of one operation fails.
// A failed Close still closes the file, so that the handler can remove it.
type faultySpool struct {
	*os.File
	operation string
	failed    bool
}

func (f *faultySpool) fault(operation string) bool {
	if f.operation != operation || f.failed {
		return false
	}
	f.failed = true
	return true
}

func (f *faultySpool) Write(p []byte) (int, error) {
	if f.fault("write") {
		return 0, errSpoolFault
	}
	return f.File.Write(p)
}

func (f *faultySpool) Sync() error {
	if f.fault("sync") {
		return errSpoolFault
	}
	return f.File.Sync()
}

func (f *faultySpool) Seek(offset int64, whence int) (int64, error) {
	if f.fault("seek") {
		return 0, errSpoolFault
	}
	return f.File.Seek(offset, whence)
}

func (f *faultySpool) Stat() (os.FileInfo, error) {
	if f.fault("stat") {
		return nil, errSpoolFault
	}
	return f.File.Stat()
}

func (f *faultySpool) Close() error {
	err := f.File.Close()
	if f.fault("close") {
		return errSpoolFault
	}
	return err
}

// faultySpoolFiles creates the spool files whose pattern is failing with one
// failing operation, and the others as usual.
func faultySpoolFiles(failing, operation string) func(string, string) (spoolFile, error) {
	return func(directory, pattern string) (spoolFile, error) {
		file, err := os.CreateTemp(directory, pattern)
		if err != nil {
			return nil, err
		}
		if pattern != failing {
			return file, nil
		}
		return &faultySpool{File: file, operation: operation}, nil
	}
}

// A spool file the server cannot write, flush, seek, stat or close, such as
// on a full disk, is a temporary condition of the server: the request is
// answered 503 with Retry-After before the backend is called, like a spool
// file it cannot create, and the spool is left empty. A failed write was
// answered 400, blaming the request, when staging a request part, and 500
// when writing the instance; the other failures were answered 500.
func TestSTOWAnswersAFailingSpoolAsAServerSpoolFailure(t *testing.T) {
	obj, bulk, _ := compressedSTOWFixture(t, transfer.JPEGBaseline.UID, 1)
	const metadataPart, instance = ".dicomweb-stow-metadata-*", ".dicomweb-stow-*"
	type request struct {
		rootType string
		parts    []stowMetadataTestPart
	}
	metadata := request{"application/dicom+json", compressedSTOWParts(t, obj, bulk)}
	part10 := request{"application/dicom", []stowMetadataTestPart{{contentType: "application/dicom", body: stowTestFile()}}}
	for _, tc := range []struct {
		name, failing, operation string
		request                  request
	}{
		{"metadata request part/write", metadataPart, "write", metadata},
		{"metadata request part/stat", metadataPart, "stat", metadata},
		{"metadata request part/close", metadataPart, "close", metadata},
		{"metadata instance/write", instance, "write", metadata},
		{"metadata instance/sync", instance, "sync", metadata},
		{"metadata instance/seek", instance, "seek", metadata},
		{"metadata instance/stat", instance, "stat", metadata},
		{"metadata instance/close", instance, "close", metadata},
		{"PS3.10 part/write", instance, "write", part10},
		{"PS3.10 part/seek", instance, "seek", part10},
		{"PS3.10 part/stat", instance, "stat", part10},
		{"PS3.10 part/close", instance, "close", part10},
	} {
		t.Run(tc.name, func(t *testing.T) {
			parent := t.TempDir()
			backend := completeServerTestBackend()
			var calls atomic.Int32
			backend.store = func(context.Context, StoreRequest) StoreOutcome {
				calls.Add(1)
				return StoreOutcome{Status: StoreStatusStored}
			}
			server, err := NewServer(ServerOptions{Backend: backend, AllowUnauthenticated: true, SpoolDirectory: parent})
			if err != nil {
				t.Fatal(err)
			}
			server.createSpool = faultySpoolFiles(tc.failing, tc.operation)
			body, contentType := stowMetadataMultipart(t, tc.request.rootType, tc.request.parts...)
			httpRequest := httptest.NewRequest(http.MethodPost, "/studies", body)
			httpRequest.Header.Set("Content-Type", contentType)
			httpRequest.Header.Set("Accept", "application/dicom+json")
			recorder := httptest.NewRecorder()
			server.ServeHTTP(recorder, httpRequest)
			if recorder.Code != http.StatusServiceUnavailable || recorder.Header().Get("Retry-After") == "" {
				t.Fatalf("status=%d Retry-After=%q body=%q, want 503 with Retry-After", recorder.Code, recorder.Header().Get("Retry-After"), strings.TrimSpace(recorder.Body.String()))
			}
			if calls.Load() != 0 {
				t.Fatalf("the backend was called %d times", calls.Load())
			}
			compressedSTOWEmpty(t, parent)
		})
	}
}
