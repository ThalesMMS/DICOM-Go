package dicomweb

import (
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"net/textproto"
	"os"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/ThalesMMS/dicom-go/core"
	"github.com/ThalesMMS/dicom-go/object"
	"github.com/ThalesMMS/dicom-go/transfer"
)

type bulkPatternReader struct{ offset int64 }

func (r *bulkPatternReader) Read(p []byte) (int, error) {
	for i := range p {
		p[i] = byte((r.offset + int64(i)) % 251)
	}
	r.offset += int64(len(p))
	return len(p), nil
}

type observedBulkStage struct {
	BulkDataStage
	owner *observedBulkStaging
}

func (s *observedBulkStage) Write(p []byte) (int, error) {
	for {
		old := s.owner.maximumWrite.Load()
		if int64(len(p)) <= old || s.owner.maximumWrite.CompareAndSwap(old, int64(len(p))) {
			break
		}
	}
	n, err := s.BulkDataStage.Write(p)
	if n > 0 && s.owner.wrote != nil {
		s.owner.once.Do(func() { close(s.owner.wrote) })
	}
	return n, err
}
func (s *observedBulkStage) Publish(ctx context.Context) (object.BulkDataSource, error) {
	s.owner.published.Add(1)
	return s.BulkDataStage.Publish(ctx)
}

type observedBulkStaging struct {
	file         FileBulkDataStaging
	maximumWrite atomic.Int64
	published    atomic.Int64
	once         sync.Once
	wrote        chan struct{}
	breakFile    bool
}

func (s *observedBulkStaging) BeginBulkData(ctx context.Context, limit int64) (BulkDataStage, error) {
	stage, err := s.file.BeginBulkData(ctx, limit)
	if err != nil {
		return stage, err
	}
	if s.breakFile {
		// Actual file I/O failure, not an invented disk error/status.
		if err := stage.(*fileBulkDataStage).reader.file.Close(); err != nil {
			_ = stage.Close()
			return nil, err
		}
	}
	return &observedBulkStage{BulkDataStage: stage, owner: s}, nil
}

func bulkStagingEmpty(t *testing.T, parent string) {
	t.Helper()
	entries, err := os.ReadDir(parent)
	if err != nil || len(entries) != 0 {
		t.Fatalf("staging remains: entries=%d err=%v", len(entries), err)
	}
}

func TestStagedBulkDataLargePayloadAndIdempotentClose(t *testing.T) {
	const size = int64(8<<20 + 17)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/octet-stream")
		w.Header().Set("Content-Length", strconv.FormatInt(size, 10))
		_, _ = io.CopyN(w, &bulkPatternReader{}, size)
	}))
	defer server.Close()
	parent := t.TempDir()
	staging := &observedBulkStaging{file: FileBulkDataStaging{Directory: parent}}
	resolver := StagedBulkDataResolver{Client: Client{Endpoint: Endpoint{BaseURL: server.URL}}, Staging: staging, ChunkBytes: 4096}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	source, err := resolver.ResolveBulkDataURI(ctx, "/bulk", size)
	if err != nil {
		t.Fatal(err)
	}
	defer source.Reader.Close()
	if source.Size != size || staging.maximumWrite.Load() > 4096 || staging.published.Load() != 1 {
		t.Fatalf("size/chunk/publish mismatch: %d/%d/%d", source.Size, staging.maximumWrite.Load(), staging.published.Load())
	}
	file := source.Reader.(*fileBulkDataReader)
	info, err := os.Stat(file.dir)
	if err != nil || (runtime.GOOS != "windows" && info.Mode().Perm() != 0o700) {
		t.Fatal("private stage directory mode", err)
	}
	info, err = file.file.Stat()
	if err != nil || info.Size() != size || (runtime.GOOS != "windows" && info.Mode().Perm() != 0o600) {
		t.Fatal("private file size/mode", err)
	}
	got, want := sha256.New(), sha256.New()
	if n, err := io.CopyBuffer(got, source.Reader, make([]byte, 4096)); err != nil || n != size {
		t.Fatalf("read=%d err=%v", n, err)
	}
	_, _ = io.CopyN(want, &bulkPatternReader{}, size)
	if string(got.Sum(nil)) != string(want.Sum(nil)) {
		t.Fatal("complete payload digest mismatch")
	}
	var wg sync.WaitGroup
	for range 16 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if err := source.Reader.Close(); err != nil {
				t.Error(err)
			}
		}()
	}
	wg.Wait()
	if _, err := source.Reader.Read(make([]byte, 1)); !errors.Is(err, os.ErrClosed) {
		t.Fatal("read after Close", err)
	}
	bulkStagingEmpty(t, parent)
}

func TestStagedBulkDataFailureCleanupAndNoPartialPublication(t *testing.T) {
	for _, name := range []string{"caller-limit", "part-limit", "body-limit", "staging-limit", "truncated", "two-parts", "missing-terminal", "truncated-after-terminal", "disk-write"} {
		t.Run(name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if strings.Contains(name, "parts") || strings.Contains(name, "terminal") {
					w.Header().Set("Content-Type", `multipart/related; type="application/octet-stream"; boundary=stage`)
					body := "--stage\r\nContent-Type: application/octet-stream\r\n\r\nabcdefgh\r\n"
					if name == "two-parts" {
						body += "--stage\r\nContent-Type: application/octet-stream\r\n\r\nsecond\r\n"
					}
					if name != "missing-terminal" {
						body += "--stage--\r\n"
					}
					if name == "truncated-after-terminal" {
						w.Header().Set("Content-Length", strconv.Itoa(len(body)+100))
					}
					_, _ = io.WriteString(w, body)
					return
				}
				w.Header().Set("Content-Type", "application/octet-stream")
				if name == "truncated" {
					w.Header().Set("Content-Length", "100")
				}
				_, _ = io.WriteString(w, "abcdefgh")
			}))
			defer server.Close()
			parent := t.TempDir()
			staging := &observedBulkStaging{file: FileBulkDataStaging{Directory: parent}, breakFile: name == "disk-write"}
			client := Client{Endpoint: Endpoint{BaseURL: server.URL}}
			maximum := int64(64)
			switch name {
			case "caller-limit":
				maximum = 4
			case "part-limit":
				client.Options.ResponseLimits.MaxPartBytes = 4
			case "body-limit":
				client.Options.MaxBodyBytes = 4
			case "staging-limit":
				staging.file.MaxBytes = 4
			}
			ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
			defer cancel()
			source, err := (StagedBulkDataResolver{Client: client, Staging: staging, ChunkBytes: 3}).ResolveBulkDataURI(ctx, "/bulk", maximum)
			if err == nil || source.Reader != nil || staging.published.Load() != 0 {
				t.Fatalf("partial source/publish: %+v %v published=%d", source, err, staging.published.Load())
			}
			if (name == "caller-limit" || name == "part-limit" || name == "staging-limit") && !errors.Is(err, object.ErrBulkDataTooLarge) {
				t.Fatal("size error mapping", err)
			}
			if name == "body-limit" {
				var web *Error
				if !errors.As(err, &web) || web.Kind != ErrorKindRequestFailure {
					t.Fatal("whole body mapping changed", err)
				}
			}
			if name == "disk-write" && !errors.Is(err, os.ErrClosed) {
				t.Fatal("actual disk failure not preserved", err)
			}
			bulkStagingEmpty(t, parent)
		})
	}
}

func TestStagedBulkDataCancellationJoinsAndDoesNotPublish(t *testing.T) {
	finished := make(chan struct{})
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		defer close(finished)
		w.Header().Set("Content-Type", "application/octet-stream")
		_, _ = io.WriteString(w, "first chunk")
		w.(http.Flusher).Flush()
		<-r.Context().Done()
	}))
	defer server.Close()
	parent := t.TempDir()
	staging := &observedBulkStaging{file: FileBulkDataStaging{Directory: parent}, wrote: make(chan struct{})}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	result := make(chan error, 1)
	go func() {
		source, err := (StagedBulkDataResolver{Client: Client{Endpoint: Endpoint{BaseURL: server.URL}}, Staging: staging}).ResolveBulkDataURI(ctx, "/bulk", 100)
		if source.Reader != nil {
			_ = source.Reader.Close()
			result <- errors.New("partial source published")
			return
		}
		result <- err
	}()
	select {
	case <-staging.wrote:
	case <-ctx.Done():
		t.Fatal("no actual staged write", ctx.Err())
	}
	if staging.published.Load() != 0 {
		t.Fatal("published before response completion")
	}
	cancel()
	select {
	case err := <-result:
		if !errors.Is(err, context.Canceled) {
			t.Fatal(err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("resolver failed to join cancellation")
	}
	select {
	case <-finished:
	case <-time.After(3 * time.Second):
		t.Fatal("HTTP handler failed to join")
	}
	bulkStagingEmpty(t, parent)
}

func TestStagedBulkDataWaitsForTerminalAndPreservesWriterIntegration(t *testing.T) {
	allow := make(chan struct{})
	defer close(allow)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", `multipart/related; type="application/octet-stream"; boundary=stage`)
		_, _ = io.WriteString(w, "--stage\r\nContent-Type: application/octet-stream\r\n\r\nabcdefgh")
		w.(http.Flusher).Flush()
		select {
		case <-allow:
		case <-r.Context().Done():
			return
		}
		_, _ = io.WriteString(w, "\r\n--stage--\r\n")
	}))
	// Cleanup ordering: permit the handler before server.Close on test failure.
	t.Cleanup(server.Close)
	parent := t.TempDir()
	staging := &observedBulkStaging{file: FileBulkDataStaging{Directory: parent}, wrote: make(chan struct{})}
	resolver := StagedBulkDataResolver{Client: Client{Endpoint: Endpoint{BaseURL: server.URL}}, Staging: staging, ChunkBytes: 3}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	result := make(chan object.BulkDataSource, 1)
	failure := make(chan error, 1)
	go func() {
		source, err := resolver.ResolveBulkDataURI(ctx, "/bulk", 100)
		result <- source
		failure <- err
	}()
	select {
	case <-staging.wrote:
	case <-ctx.Done():
		t.Fatal(ctx.Err())
	}
	if staging.published.Load() != 0 {
		t.Fatal("published without terminal boundary")
	}
	allow <- struct{}{}
	var source object.BulkDataSource
	select {
	case source = <-result:
	case <-ctx.Done():
		t.Fatal(ctx.Err())
	}
	if err := <-failure; err != nil {
		t.Fatal(err)
	}
	defer source.Reader.Close()
	obj := object.New(nil)
	defer obj.Close()
	obj.Put(core.Element{Header: core.ElementHeader{Tag: core.NewTag(0x0042, 0x0011), VR: core.VROB}, Value: core.BulkDataValue{URI: "/bulk"}})
	// Writer owns the returned stage reader and closes it after streaming.
	writer := io.Discard
	err := object.WriteDataSetWithOptions(writer, obj, transfer.ExplicitVRLittleEndian, object.WriteDataSetOptions{BulkDataResolver: func(core.BulkDataValue) (object.BulkDataSource, error) { return source, nil }})
	if err != nil {
		t.Fatal(err)
	}
	bulkStagingEmpty(t, parent)
}

func TestStagedBulkDataOriginRedirectAuthAndConfiguration(t *testing.T) {
	var remoteCalls atomic.Int32
	remote := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { remoteCalls.Add(1) }))
	defer remote.Close()
	var authorized atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") == "Bearer staging-test-token" {
			authorized.Add(1)
		}
		if r.URL.Path == "/redirect" {
			http.Redirect(w, r, remote.URL+"/bulk", http.StatusFound)
			return
		}
		w.Header().Set("Content-Type", "application/octet-stream")
		_, _ = io.WriteString(w, "data")
	}))
	defer server.Close()
	parent := t.TempDir()
	httpClient := &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { return errors.New("configured redirect refusal") }}
	client := Client{Endpoint: Endpoint{BaseURL: server.URL}, Options: Options{BearerToken: "staging-test-token", HTTPClient: httpClient}}
	resolver := StagedBulkDataResolver{Client: client, Staging: FileBulkDataStaging{Directory: parent}}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	for _, uri := range []string{remote.URL + "/bulk", "https://user:pass@example.invalid/bulk"} {
		if _, err := resolver.ResolveBulkDataURI(ctx, uri, 10); err == nil {
			t.Fatal("unconfined URI accepted")
		}
	}
	// The explicitly configured existing redirect policy must remain authoritative.
	if _, err := resolver.ResolveBulkDataURI(ctx, "/redirect", 10); err == nil {
		t.Fatal("cross-origin redirect accepted")
	}
	if remoteCalls.Load() != 0 {
		t.Fatal("cross-origin request sent", remoteCalls.Load())
	}
	source, err := resolver.ResolveBulkDataURI(ctx, "/bulk", 10)
	if err != nil {
		t.Fatal(err)
	}
	if err := source.Reader.Close(); err != nil {
		t.Fatal(err)
	}
	if authorized.Load() == 0 {
		t.Fatal("authentication omitted")
	}
	for _, bad := range []StagedBulkDataResolver{{Client: client}, {Client: client, Staging: resolver.Staging, ChunkBytes: -1}, {Client: client, Staging: resolver.Staging, ChunkBytes: 1<<20 + 1}} {
		if _, err := bad.ResolveBulkDataURI(ctx, "/bulk", 10); err == nil {
			t.Fatal("invalid staging configuration accepted")
		}
	}
	bulkStagingEmpty(t, parent)
}

func TestFileBulkDataStageBoundCloseAndEmptyRepresentation(t *testing.T) {
	parent := t.TempDir()
	// A different Publish context cannot revive a canceled Begin operation.
	beginCtx, cancelBegin := context.WithCancel(context.Background())
	canceledStage, err := (FileBulkDataStaging{Directory: parent}).BeginBulkData(beginCtx, 100)
	if err != nil {
		t.Fatal(err)
	}
	if n, err := canceledStage.Write([]byte("retained")); err != nil || n != 8 {
		t.Fatalf("actual write=%d %v", n, err)
	}
	cancelBegin()
	unpublished, publishErr := canceledStage.Publish(context.Background())
	if !errors.Is(publishErr, context.Canceled) || unpublished.Reader != nil {
		t.Fatalf("new context revived canceled Begin: %+v %v", unpublished, publishErr)
	}
	if _, err := canceledStage.Write([]byte("more")); !errors.Is(err, context.Canceled) {
		t.Fatal("write after Begin cancel", err)
	}
	if err := canceledStage.Close(); err != nil {
		t.Fatal(err)
	}
	if err := canceledStage.Close(); err != nil {
		t.Fatal(err)
	}
	bulkStagingEmpty(t, parent)

	stage, err := (FileBulkDataStaging{Directory: parent, MaxBytes: 2}).BeginBulkData(context.Background(), 100)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := stage.Write([]byte("abc")); !errors.Is(err, object.ErrBulkDataTooLarge) {
		t.Fatal("backend quota", err)
	}
	if err := stage.Close(); err != nil {
		t.Fatal(err)
	}
	if err := stage.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := stage.Write([]byte("a")); !errors.Is(err, os.ErrClosed) {
		t.Fatal("write after Close", err)
	}
	bulkStagingEmpty(t, parent)
	// A single zero-length octet-stream is an exact representation, not no part.
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/octet-stream")
	}))
	defer server.Close()
	source, err := (StagedBulkDataResolver{Client: Client{Endpoint: Endpoint{BaseURL: server.URL}}, Staging: FileBulkDataStaging{Directory: parent}}).ResolveBulkDataURI(context.Background(), "/empty", 1)
	if err != nil || source.Reader == nil || source.Size != 0 {
		t.Fatalf("empty: %+v %v", source, err)
	}
	if err := source.Reader.Close(); err != nil {
		t.Fatal(err)
	}
	bulkStagingEmpty(t, parent)
}

// Streaming multipart helper also used by the allocation benchmark. It does
// not allocate the response payload or store a retained []byte fixture.
func serveBulkPattern(w http.ResponseWriter, size int64) {
	writer := multipart.NewWriter(w)
	w.Header().Set("Content-Type", fmt.Sprintf(`multipart/related; type="application/octet-stream"; boundary=%s`, writer.Boundary()))
	part, err := writer.CreatePart(textproto.MIMEHeader{"Content-Type": {"application/octet-stream"}})
	if err == nil {
		_, _ = io.CopyN(part, &bulkPatternReader{}, size)
	}
	_ = writer.Close()
}

type bulkStagingRoundTripFunc func(*http.Request) (*http.Response, error)

func (f bulkStagingRoundTripFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

type bulkStagingFailCloseBody struct {
	io.Reader
	closed atomic.Int32
	err    error
}

func (b *bulkStagingFailCloseBody) Close() error { b.closed.Add(1); return b.err }

func TestStagedBulkDataTransportAndBodyCloseFailuresRemainUnpublished(t *testing.T) {
	for _, name := range []string{"transport", "body-close"} {
		t.Run(name, func(t *testing.T) {
			failure := errors.New("synthetic transport failure")
			body := &bulkStagingFailCloseBody{Reader: strings.NewReader("abcdefgh"), err: failure}
			transport := bulkStagingRoundTripFunc(func(r *http.Request) (*http.Response, error) {
				if name == "transport" {
					return nil, failure
				}
				return &http.Response{StatusCode: 200, Header: http.Header{"Content-Type": {"application/octet-stream"}}, Body: body, Request: r}, nil
			})
			parent := t.TempDir()
			staging := &observedBulkStaging{file: FileBulkDataStaging{Directory: parent}}
			client := Client{Endpoint: Endpoint{BaseURL: "http://example.invalid"}, Options: Options{HTTPClient: &http.Client{Transport: transport}}}
			source, err := (StagedBulkDataResolver{Client: client, Staging: staging}).ResolveBulkDataURI(context.Background(), "/bulk", 100)
			if err == nil || source.Reader != nil || staging.published.Load() != 0 {
				t.Fatalf("partial source: %+v %v", source, err)
			}
			if name == "body-close" && (!errors.Is(err, failure) || body.closed.Load() != 1) {
				t.Fatalf("HTTP close cause/count: %v/%d", err, body.closed.Load())
			}
			if name == "transport" {
				var web *Error
				if !errors.As(err, &web) || web.Kind != ErrorKindRequestFailure {
					t.Fatal("transport error mapping changed", err)
				}
			}
			bulkStagingEmpty(t, parent)
		})
	}
}

// A valid reader may return useful bytes and a non-EOF error together, then EOF.
// The complete MIME boundary fits within those bytes; parsing it must not hide
// the transport error from atomic staged publication.
type bulkStagingTerminalErrorBody struct {
	data   []byte
	err    error
	sent   bool
	closed atomic.Int32
}

func (b *bulkStagingTerminalErrorBody) Read(p []byte) (int, error) {
	if b.sent {
		return 0, io.EOF
	}
	n := copy(p, b.data)
	b.data = b.data[n:]
	if len(b.data) > 0 {
		return n, nil
	}
	b.sent = true
	return n, b.err
}
func (b *bulkStagingTerminalErrorBody) Close() error { b.closed.Add(1); return nil }

func TestStagedBulkDataRetainsBufferedTerminalReadError(t *testing.T) {
	for _, failure := range []error{io.ErrUnexpectedEOF, errors.New("synthetic final transport error")} {
		t.Run(failure.Error(), func(t *testing.T) {
			body := &bulkStagingTerminalErrorBody{data: []byte("--stage\r\nContent-Type: application/octet-stream\r\n\r\nabcdefgh\r\n--stage--\r\n"), err: failure}
			transport := bulkStagingRoundTripFunc(func(r *http.Request) (*http.Response, error) {
				return &http.Response{StatusCode: 200, Header: http.Header{"Content-Type": {`multipart/related; type="application/octet-stream"; boundary=stage`}}, Body: body, Request: r}, nil
			})
			parent := t.TempDir()
			staging := &observedBulkStaging{file: FileBulkDataStaging{Directory: parent}}
			client := Client{Endpoint: Endpoint{BaseURL: "http://example.invalid"}, Options: Options{HTTPClient: &http.Client{Transport: transport}}}
			source, err := (StagedBulkDataResolver{Client: client, Staging: staging, ChunkBytes: 3}).ResolveBulkDataURI(context.Background(), "/bulk", 100)
			if !errors.Is(err, failure) || source.Reader != nil || staging.published.Load() != 0 || body.closed.Load() != 1 {
				t.Fatalf("masked transport error: source=%+v err=%v publish=%d close=%d", source, err, staging.published.Load(), body.closed.Load())
			}
			if staging.maximumWrite.Load() == 0 {
				t.Fatal("test did not stage actual bytes before terminal rejection")
			}
			bulkStagingEmpty(t, parent)
		})
	}
}
