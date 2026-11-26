package dicomweb

import (
	"context"
	"errors"
	"io"
	"mime"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/ThalesMMS/dicom-go/transfer"
)

func TestReadLeaseExactExpiry(t *testing.T) {
	at := time.Date(2026, 5, 12, 8, 0, 0, 0, time.UTC)
	lease := ReadLease{ExpiresAt: at}
	if !lease.validAt(at.Add(-time.Nanosecond)) || lease.validAt(at) || lease.validAt(at.Add(time.Nanosecond)) {
		t.Fatal("lease validity must end at the expiration instant")
	}
}

func TestReadLeaseExpiresWhileBackendWaits(t *testing.T) {
	var cause error
	backend := completeServerTestBackend()
	backend.metadata = func(ctx context.Context, _ MetadataRequest, _ func(Dataset) error) error {
		<-ctx.Done()
		cause = context.Cause(ctx)
		return ctx.Err()
	}
	server, _ := newOpenTestServer(t, backend, func(opts *ServerOptions) {
		opts.AuthorizeReadLease = func(context.Context, *http.Request, Operation) (ReadLease, error) {
			return ReadLease{ExpiresAt: time.Now().Add(500 * time.Millisecond)}, nil
		}
	})
	ctx, cancel := context.WithTimeout(context.Background(), 4*time.Second)
	defer cancel()
	rec := httptest.NewRecorder()
	req := leaseTestRequest("/studies/1.2.3/metadata", "application/dicom+json").WithContext(ctx)
	server.ServeHTTP(rec, req)
	if rec.Code != http.StatusForbidden || !errors.Is(cause, ErrReadLeaseDenied) {
		t.Fatalf("status=%d, backend context cause=%v", rec.Code, cause)
	}
}

func TestReadLeaseQIDOPagesRespectVersionAndScope(t *testing.T) {
	var version, searches, reads atomic.Int32
	version.Store(1)
	type principalKey struct{}
	const allowedUID = "1.2.3"
	const deniedUID = "1.2.4"
	backend := completeServerTestBackend()
	backend.search = func(ctx context.Context, req SearchRequest, yield func(Dataset) error) (SearchResult, error) {
		searches.Add(1)
		if req.MaximumResults != 1 {
			return SearchResult{}, ErrBackend
		}
		allowed, _ := ctx.Value(principalKey{}).(string)
		for _, uid := range []string{deniedUID, allowedUID} {
			if uid != allowed || req.Offset != 0 {
				continue
			}
			if err := CheckReadLease(ctx); err != nil {
				return SearchResult{}, err
			}
			reads.Add(1)
			if err := yield(Dataset{"0020000D": {VR: "UI", Value: []any{uid}}}); err != nil {
				return SearchResult{}, err
			}
		}
		remaining := 0
		return SearchResult{Remaining: &remaining}, nil
	}
	server, _ := newOpenTestServer(t, backend, func(opts *ServerOptions) {
		opts.Authorize = func(ctx context.Context, r *http.Request, _ Operation) error {
			*r = *r.WithContext(context.WithValue(ctx, principalKey{}, allowedUID))
			return nil
		}
		opts.AuthorizeReadLease = func(ctx context.Context, _ *http.Request, operation Operation) (ReadLease, error) {
			if operation != OperationSearchStudies || ctx.Value(principalKey{}) != allowedUID || version.Load() != 1 {
				return ReadLease{}, ErrForbidden
			}
			return ReadLease{ExpiresAt: time.Now().Add(time.Hour), Validate: func(context.Context) error {
				if version.Load() != 1 {
					return ErrForbidden
				}
				return nil
			}}, nil
		}
	})
	first := httptest.NewRecorder()
	server.ServeHTTP(first, leaseTestRequest("/studies?limit=1", "application/dicom+json"))
	if first.Code != http.StatusOK || !strings.Contains(first.Body.String(), allowedUID) ||
		strings.Contains(first.Body.String(), deniedUID) || first.Header().Get("Warning") != "" ||
		first.Header().Get("Cache-Control") != "private, no-store" {
		t.Fatalf("first page status=%d, headers=%v, body=%s", first.Code, first.Header(), first.Body.String())
	}
	version.Store(2)
	next := httptest.NewRecorder()
	server.ServeHTTP(next, leaseTestRequest("/studies?limit=1&offset=1", "application/dicom+json"))
	if next.Code != http.StatusForbidden || searches.Load() != 1 || reads.Load() != 1 ||
		strings.Contains(next.Body.String(), allowedUID) {
		t.Fatalf("refused page status=%d, searches=%d, reads=%d, body=%s",
			next.Code, searches.Load(), reads.Load(), next.Body.String())
	}
}

func TestReadLeaseDeniesBeforeBackendOrSpoolPublication(t *testing.T) {
	for _, failure := range []string{"expired", "audit error", "authorization panic", "revoked before publication"} {
		t.Run(failure, func(t *testing.T) {
			var calls, backendCalls, auditCalls int
			var audit AuditEvent
			backend := completeServerTestBackend()
			backend.metadata = func(_ context.Context, _ MetadataRequest, yield func(Dataset) error) error {
				backendCalls++
				return yield(Dataset{"00080018": {VR: "UI", Value: []any{"1.2.3.4"}}})
			}
			server, _ := newOpenTestServer(t, backend, func(opts *ServerOptions) {
				opts.SpoolDirectory = t.TempDir()
				opts.Audit = func(_ context.Context, event AuditEvent) { auditCalls++; audit = event }
				opts.AuthorizeReadLease = func(context.Context, *http.Request, Operation) (ReadLease, error) {
					lease := ReadLease{ExpiresAt: time.Now().Add(time.Hour)}
					if failure == "expired" {
						lease.ExpiresAt = time.Now()
					}
					lease.Validate = func(context.Context) error {
						calls++
						switch failure {
						case "audit error":
							return errors.New("PRIVATE AUDIT FAILURE")
						case "authorization panic":
							panic("PRIVATE SESSION")
						case "revoked before publication":
							if calls > 3 { // admission, backend entry, and the sole staged result
								return ErrForbidden
							}
						}
						return nil
					}
					return lease, nil
				}
			})
			rec := httptest.NewRecorder()
			server.ServeHTTP(rec, leaseTestRequest("/studies/1.2.3/metadata", "application/dicom+json"))
			expectedCalls := 0
			if failure == "revoked before publication" {
				expectedCalls = 1
			}
			if rec.Code != http.StatusForbidden || backendCalls != expectedCalls || auditCalls != 1 ||
				audit.StatusCode != http.StatusForbidden || audit.ErrorCode != "read_lease_denied" || audit.ItemCount != 0 ||
				strings.Contains(rec.Body.String(), "PRIVATE") || strings.Contains(rec.Body.String(), "1.2.3.4") {
				t.Fatalf("status=%d, backend=%d, audit=%+v, body=%s", rec.Code, backendCalls, audit, rec.Body.String())
			}
		})
	}
}

func TestReadLeaseStopsWADOAfterFirstObject(t *testing.T) {
	firstPayload := strings.Repeat("ALLOWED\n", 2_048)
	const secondPayload = "REVOKED OBJECT"
	var revoked atomic.Bool
	first := &countedLeasePart{Reader: strings.NewReader(firstPayload)}
	second := &countedLeasePart{Reader: strings.NewReader(secondPayload)}
	audits := make(chan AuditEvent, 1)
	backend := completeServerTestBackend()
	backend.retrieve = func(ctx context.Context, _ RetrieveRequest, yield func(RetrievePart) error) error {
		if err := yield(leaseTestPart("1.2.3.1", first)); err != nil {
			return err
		}
		revoked.Store(true)
		_ = yield(leaseTestPart("1.2.3.2", second)) // A provider cannot erase a denial.
		if err := CheckReadLease(ctx); !errors.Is(err, ErrReadLeaseDenied) || !errors.Is(context.Cause(ctx), ErrReadLeaseDenied) {
			t.Errorf("subsequent backend check = %v, cause = %v", err, context.Cause(ctx))
		}
		return nil
	}
	_, peer := newOpenTestServer(t, backend, func(opts *ServerOptions) {
		opts.Audit = func(_ context.Context, event AuditEvent) { audits <- event }
		opts.AuthorizeReadLease = func(context.Context, *http.Request, Operation) (ReadLease, error) {
			return ReadLease{ExpiresAt: time.Now().Add(time.Hour), Validate: func(context.Context) error {
				if revoked.Load() {
					return ErrForbidden
				}
				return nil
			}}, nil
		}
	})
	req, err := http.NewRequest(http.MethodGet, peer.URL+"/studies/1.2.3", nil)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Accept", `multipart/related; type="application/dicom"; transfer-syntax=*`)
	response, err := peer.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	body, readErr := io.ReadAll(response.Body)
	if response.StatusCode != http.StatusOK || readErr == nil || strings.Contains(string(body), secondPayload) {
		t.Fatalf("status=%d, len=%d, read error=%v", response.StatusCode, len(body), readErr)
	}
	_, params, err := mime.ParseMediaType(response.Header.Get("Content-Type"))
	if err != nil || !strings.Contains(string(body), firstPayload[:4096]) ||
		strings.Contains(string(body), "--"+params["boundary"]+"--") {
		t.Fatalf("response did not contain the first object prefix followed by a transport abort: %v", err)
	}
	audit := <-audits
	if first.reads.Load() == 0 || first.closes.Load() != 1 || second.reads.Load() != 0 || second.closes.Load() != 1 ||
		audit.StatusCode != http.StatusOK || audit.ItemCount != 1 || audit.ErrorCode != "read_lease_denied" {
		t.Fatalf("first=%d/%d second=%d/%d audit=%+v", first.reads.Load(), first.closes.Load(), second.reads.Load(), second.closes.Load(), audit)
	}
}

func TestReadLeaseRevalidatesBeforeReturningResourceStatus(t *testing.T) {
	for _, rangeValue := range []string{"", "bytes=0-1", "bytes=100-200"} {
		t.Run(rangeValue, func(t *testing.T) {
			var revoked bool
			var checks int
			backend := completeServerTestBackend()
			backend.retrieve = func(context.Context, RetrieveRequest, func(RetrievePart) error) error {
				revoked = true
				return ErrNotFound
			}
			backend.bulk = func(context.Context, BulkDataRequest) (BulkDataPart, error) {
				r := strings.NewReader("private size")
				return BulkDataPart{Reader: io.NopCloser(r), ReaderAt: r, Size: r.Size(), ContentType: "application/octet-stream"}, nil
			}
			server, _ := newOpenTestServer(t, backend, func(opts *ServerOptions) {
				opts.EnableBulkDataRanges = true
				opts.AuthorizeReadLease = func(context.Context, *http.Request, Operation) (ReadLease, error) {
					return ReadLease{ExpiresAt: time.Now().Add(time.Hour), Validate: func(context.Context) error {
						checks++
						if revoked || (rangeValue != "" && checks > 4) {
							return ErrReadLeaseDenied
						}
						return nil
					}}, nil
				}
			})
			req := leaseTestRequest("/studies/1.2.3", `multipart/related; type="application/dicom"; transfer-syntax=*`)
			if rangeValue != "" {
				req = leaseTestRequest("/bulkdata/opaque", "application/octet-stream")
				req.Header.Set("Range", rangeValue)
			}
			rec := httptest.NewRecorder()
			server.ServeHTTP(rec, req)
			if rec.Code != http.StatusForbidden || rec.Header().Get("Content-Range") != "" ||
				rec.Header().Get("Content-Length") != "" || strings.Contains(rec.Body.String(), "private size") {
				t.Fatalf("status=%d, checks=%d, headers=%v, body=%s", rec.Code, checks, rec.Header(), rec.Body.String())
			}
		})
	}
}

func TestReadLeaseRevalidatesAfterPreparedRetrieveQueue(t *testing.T) {
	syntax := transfer.ExplicitVRLittleEndian.UID
	var checks atomic.Int32
	var revoked atomic.Bool
	entered := make(chan struct{})
	release := make(chan struct{})
	part := &countedLeasePart{Reader: strings.NewReader("NEVER READ")}
	backend := completeServerTestBackend()
	backend.retrieve = func(_ context.Context, _ RetrieveRequest, yield func(RetrievePart) error) error {
		return yield(leaseTestPart("1.2.3.1", part))
	}
	server, _ := newOpenTestServer(t, backend, func(opts *ServerOptions) {
		opts.RetrieveTranscode = preparedTestPolicy(t, syntax)
		opts.RetrieveTranscode.MaxConcurrentJobs = 1
		opts.AuthorizeReadLease = func(context.Context, *http.Request, Operation) (ReadLease, error) {
			return ReadLease{ExpiresAt: time.Now().Add(time.Hour), Validate: func(ctx context.Context) error {
				if checks.Add(1) == 3 {
					close(entered)
					select {
					case <-release:
						return nil // Force revocation after this point-in-time check.
					case <-ctx.Done():
						return ctx.Err()
					}
				}
				if revoked.Load() {
					return ErrForbidden
				}
				return nil
			}}, nil
		}
	})
	server.retrieveSem <- struct{}{}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	rec := httptest.NewRecorder()
	done := make(chan struct{})
	go func() {
		defer close(done)
		request := leaseTestRequest("/studies/1.2.3", `multipart/related; type="application/dicom"; transfer-syntax=*`)
		server.ServeHTTP(rec, request.WithContext(ctx))
	}()
	select {
	case <-entered:
	case <-ctx.Done():
		t.Fatal("retrieve never reached its preparation boundary")
	}
	revoked.Store(true)
	close(release)
	<-server.retrieveSem
	select {
	case <-done:
	case <-ctx.Done():
		t.Fatal("retrieve never finished")
	}
	if rec.Code != http.StatusForbidden || checks.Load() != 4 || part.closes.Load() != 1 || part.reads.Load() != 0 {
		t.Fatalf("status=%d, checks=%d, reads=%d, closes=%d, body=%s",
			rec.Code, checks.Load(), part.reads.Load(), part.closes.Load(), rec.Body.String())
	}
}

// TestReadLeasePreparedRetrieveValidatesBoundedTimes converts a small
// Implicit VR object for a client and counts lease validations: parsing the
// staged copies must not validate the lease for every few bytes it reads,
// since an application's validation can read a policy file.
func TestReadLeasePreparedRetrieveValidatesBoundedTimes(t *testing.T) {
	input, pixels := preparedTestFixture(t, transfer.ImplicitVRLittleEndian)
	var validations atomic.Int32
	_, hs := newOpenTestServer(t, preparedTestBackend(input, transfer.ImplicitVRLittleEndian.UID), func(opts *ServerOptions) {
		opts.RetrieveTranscode = &RetrieveTranscodePolicy{}
		opts.AuthorizeReadLease = func(context.Context, *http.Request, Operation) (ReadLease, error) {
			return ReadLease{ExpiresAt: time.Now().Add(time.Hour), Validate: func(context.Context) error {
				validations.Add(1)
				return nil
			}}, nil
		}
	})
	request, _ := http.NewRequest(http.MethodGet, hs.URL+preparedInstancePath, nil)
	request.Header.Set("Accept", "application/dicom")
	response, err := hs.Client().Do(request)
	if err != nil {
		t.Fatal(err)
	}
	data, err := io.ReadAll(response.Body)
	response.Body.Close()
	if err != nil || response.StatusCode != http.StatusOK {
		t.Fatalf("HTTP %d (%v)", response.StatusCode, err)
	}
	assertPreparedPixels(t, data, pixels, transfer.ExplicitVRLittleEndian.UID)
	// The fixed boundaries of one prepared object are about twenty; the
	// staged reads used to add one validation per read, hundreds here.
	if got := validations.Load(); got > 40 {
		t.Fatalf("one %d-byte object validated the lease %d times", len(input), got)
	}
	t.Logf("one %d-byte object validated the lease %d times", len(input), validations.Load())
}

// preparedLeaseFile opens a staged file of size bytes behind a lease whose
// validation counts its calls and fails once revoked is set.
func preparedLeaseFile(t *testing.T, size int) (*retrieveContextFile, *os.File, *requestReadLease, *atomic.Bool, *atomic.Int32) {
	t.Helper()
	file, err := os.CreateTemp(t.TempDir(), "prepared-*.dcm")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { file.Close() })
	if _, err := file.Write([]byte(strings.Repeat("A", size))); err != nil {
		t.Fatal(err)
	}
	if _, err := file.Seek(0, io.SeekStart); err != nil {
		t.Fatal(err)
	}
	revoked, validations := &atomic.Bool{}, &atomic.Int32{}
	ctx, cancel := context.WithCancelCause(context.Background())
	t.Cleanup(func() { cancel(nil) })
	lease := &requestReadLease{ReadLease: ReadLease{
		ExpiresAt: time.Now().Add(time.Hour),
		Validate: func(context.Context) error {
			validations.Add(1)
			if revoked.Load() {
				return ErrForbidden
			}
			return nil
		},
	}, cancel: cancel}
	return &retrieveContextFile{File: file, ctx: context.WithValue(ctx, readLeaseKey{}, lease)}, file, lease, revoked, validations
}

// TestReadLeaseRevalidatesPreparedFileByVolume validates the lease before
// the first read of a staged file and again after each
// retrieveLeaseCheckBytes, not for every few bytes a parser reads: a
// revocation stops reading within that volume, and a denial already
// recorded stops the next read before it moves the file.
func TestReadLeaseRevalidatesPreparedFileByVolume(t *testing.T) {
	size := retrieveLeaseCheckBytes + 64
	source, file, _, revoked, validations := preparedLeaseFile(t, size)
	var b [2]byte
	read := 0
	for read < retrieveLeaseCheckBytes-2 {
		n, err := source.Read(b[:])
		if err != nil || n == 0 {
			t.Fatalf("allowed read at %d = %d, %v", read, n, err)
		}
		read += n
	}
	if got := validations.Load(); got != 1 {
		t.Fatalf("%d bytes in reads of 2 validated the lease %d times, want 1", read, got)
	}
	revoked.Store(true)
	var err error
	for err == nil && read < size {
		var n int
		n, err = source.Read(b[:])
		read += n
	}
	if !errors.Is(err, ErrReadLeaseDenied) || read > retrieveLeaseCheckBytes+len(b) {
		t.Fatalf("revoked lease: %v after %d bytes, want a denial within %d", err, read, retrieveLeaseCheckBytes+len(b))
	}
	offset, _ := file.Seek(0, io.SeekCurrent)
	if n, err := source.Read(b[:]); n != 0 || !errors.Is(err, ErrReadLeaseDenied) {
		t.Fatalf("read after denial = %d, %v", n, err)
	}
	if after, _ := file.Seek(0, io.SeekCurrent); after != offset {
		t.Fatalf("physical file cursor moved after denial: %d to %d", offset, after)
	}
}

// TestReadLeasePreparedFileSeesExpiryAtOnce ends a staged read at the
// lease's expiry, between two validations, before the file moves.
func TestReadLeasePreparedFileSeesExpiryAtOnce(t *testing.T) {
	source, file, lease, _, validations := preparedLeaseFile(t, 16)
	var b [1]byte
	if n, err := source.Read(b[:]); n != 1 || err != nil {
		t.Fatalf("allowed read = %d, %v", n, err)
	}
	lease.ExpiresAt = time.Now().Add(-time.Second)
	if n, err := source.Read(b[:]); n != 0 || !errors.Is(err, ErrReadLeaseDenied) {
		t.Fatalf("expired read = %d, %v", n, err)
	}
	if offset, err := file.Seek(0, io.SeekCurrent); err != nil || offset != 1 || validations.Load() != 1 {
		t.Fatalf("cursor %d (%v), %d validations after expiry", offset, err, validations.Load())
	}
}

func leaseTestRequest(path, accept string) *http.Request {
	req := httptest.NewRequest(http.MethodGet, path, nil)
	req.Header.Set("Accept", accept)
	return req
}

type countedLeasePart struct {
	io.Reader
	reads, closes atomic.Int32
}

func (p *countedLeasePart) Read(data []byte) (int, error) {
	p.reads.Add(1)
	return p.Reader.Read(data)
}

func (p *countedLeasePart) Close() error {
	p.closes.Add(1)
	return nil
}

func leaseTestPart(sop string, source io.ReadCloser) RetrievePart {
	return RetrievePart{
		Ref:               InstanceRef{StudyInstanceUID: "1.2.3", SeriesInstanceUID: "1.2.3.9", SOPInstanceUID: sop},
		TransferSyntaxUID: transfer.ExplicitVRLittleEndian.UID, Reader: source, Size: -1,
	}
}
