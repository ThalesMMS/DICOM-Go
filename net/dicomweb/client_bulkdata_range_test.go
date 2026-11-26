package dicomweb

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"math"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func TestRetrieveBulkDataRangeExactResponse(t *testing.T) {
	want := []byte{0x00, 0x7f, 0xff}
	for _, tc := range []struct {
		name     string
		total    string
		encoding string
	}{
		{name: "known length", total: "10"},
		{name: "unknown length", total: "*", encoding: "identity"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var calls atomic.Int32
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				calls.Add(1)
				if r.Method != http.MethodGet || r.URL.Path != "/dicomweb/bulk" ||
					len(r.Header.Values("Range")) != 1 || r.Header.Get("Range") != "bytes=2-4" ||
					r.Header.Get("Accept") != "application/octet-stream" ||
					r.Header.Get("Accept-Encoding") != "identity" {
					t.Errorf("invalid range request: %s %s, headers %v", r.Method, r.URL.Path, r.Header)
				}
				w.Header().Set("Content-Type", "application/octet-stream")
				w.Header().Set("Content-Range", "bytes 2-4/"+tc.total)
				if tc.encoding != "" {
					w.Header().Set("Content-Encoding", tc.encoding)
				}
				w.WriteHeader(http.StatusPartialContent)
				_, _ = w.Write(want)
			}))
			t.Cleanup(server.Close)

			client := Client{Endpoint: Endpoint{BaseURL: server.URL + "/dicomweb"}, Options: Options{MaxBodyBytes: 3}}
			part, err := client.RetrieveBulkDataRange(context.Background(), "bulk", 2, 3)
			if err != nil || !bytes.Equal(part.Data, want) || part.ContentType != "application/octet-stream" || calls.Load() != 1 {
				t.Fatalf("range: %v, %v (%d requests); want exact bytes and one request", part, err, calls.Load())
			}
		})
	}
}

func TestRetrieveBulkDataRangeFromRangeEnabledServer(t *testing.T) {
	backend := completeServerTestBackend()
	backend.bulk = func(context.Context, BulkDataRequest) (BulkDataPart, error) {
		reader := strings.NewReader("0123456789")
		return BulkDataPart{Reader: io.NopCloser(reader), ReaderAt: reader, Size: reader.Size()}, nil
	}
	_, server := newOpenTestServer(t, backend, func(options *ServerOptions) { options.EnableBulkDataRanges = true })
	client := Client{Endpoint: Endpoint{BaseURL: server.URL}}

	part, err := client.RetrieveBulkDataRange(context.Background(), "/bulkdata/controlled", 6, 4)
	if err != nil || string(part.Data) != "6789" {
		t.Fatalf("exact end-boundary read returned %v, %v", part, err)
	}
	// The Go server clamps the end to the available representation. That is
	// a valid HTTP response, but it cannot satisfy the exact-read contract.
	part, err = client.RetrieveBulkDataRange(context.Background(), "/bulkdata/controlled", 6, 5)
	requireClientRangeFailure(t, part, err, ErrorKindDecodeResponse, http.StatusPartialContent)
}

func TestRetrieveBulkDataRangeRejectsUnboundResponse(t *testing.T) {
	const secret = "patient-query-marker"
	for _, tc := range []struct {
		name            string
		status          int
		ranges          []string
		body            string
		mediaType       string
		contentEncoding string
		contentLength   string
		chunked         bool
		allowedMedia    []string
		wantKind        ErrorKind
	}{
		{name: "whole response", status: http.StatusOK, body: "0123456789"},
		{name: "no content", status: http.StatusNoContent},
		{name: "unsatisfiable", status: http.StatusRequestedRangeNotSatisfiable, ranges: []string{"bytes */2"}, wantKind: ErrorKindHTTPStatus},
		{name: "forbidden", status: http.StatusForbidden, wantKind: ErrorKindAuthStatus},
		{name: "missing range"},
		{name: "malformed range", ranges: []string{"bytes 2-4/" + secret}},
		{name: "wrong unit", ranges: []string{"items 2-4/10"}},
		{name: "multiple range fields", ranges: []string{"bytes 2-4/10", "bytes 2-4/10"}},
		{name: "different complete range", ranges: []string{"bytes 3-5/10"}},
		{name: "partial requested length", ranges: []string{"bytes 2-3/4"}, body: "23"},
		{name: "last byte outside representation", ranges: []string{"bytes 2-4/4"}},
		{name: "invalid decimal", ranges: []string{"bytes +2-4/10"}},
		{name: "overflowing total", ranges: []string{"bytes 2-4/9223372036854775808"}},
		{name: "multipart response", ranges: []string{"bytes 2-4/10"}, mediaType: "multipart/byteranges; boundary=range"},
		{name: "compressed media", ranges: []string{"bytes 2-4/10"}, mediaType: "image/jpeg"},
		{name: "media policy refusal", ranges: []string{"bytes 2-4/10"}, allowedMedia: []string{"image/jpeg"}},
		{name: "content coding", ranges: []string{"bytes 2-4/10"}, contentEncoding: "gzip"},
		{name: "invalid declared length", ranges: []string{"bytes 2-4/10"}, body: "234", contentLength: "5"},
		{name: "short streamed payload", ranges: []string{"bytes 2-4/10"}, body: "23", chunked: true},
		{name: "long streamed payload", ranges: []string{"bytes 2-4/10"}, body: "2345", chunked: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			status := tc.status
			if status == 0 {
				status = http.StatusPartialContent
			}
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				media := tc.mediaType
				if media == "" {
					media = "application/octet-stream"
				}
				w.Header().Set("Content-Type", media)
				w.Header()["Content-Range"] = tc.ranges
				if tc.contentEncoding != "" {
					w.Header().Set("Content-Encoding", tc.contentEncoding)
				}
				if tc.contentLength != "" {
					w.Header().Set("Content-Length", tc.contentLength)
				}
				w.WriteHeader(status)
				if tc.chunked {
					w.(http.Flusher).Flush()
				}
				if status != http.StatusNoContent {
					body := tc.body
					if body == "" {
						body = "234"
					}
					_, _ = io.WriteString(w, body)
				}
			}))
			t.Cleanup(server.Close)

			client := Client{Endpoint: Endpoint{BaseURL: server.URL}, Options: Options{
				ResponseLimits: ResponseLimits{AllowedMediaTypes: tc.allowedMedia},
			}}
			part, err := client.RetrieveBulkDataRange(context.Background(), "/bulk?query="+secret, 2, 3)
			wantKind := tc.wantKind
			if wantKind == "" {
				wantKind = ErrorKindDecodeResponse
			}
			requireClientRangeFailure(t, part, err, wantKind, status)
			if strings.Contains(fmt.Sprintf("%v %+v %#v %v", err, err, err, errors.Unwrap(err)), secret) {
				t.Fatalf("range error exposed the response or query: %+v", err)
			}
		})
	}
}

func TestRetrieveBulkDataRangeCancellationDiscardsPrefix(t *testing.T) {
	started := make(chan struct{})
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/octet-stream")
		w.Header().Set("Content-Range", "bytes 2-4/10")
		w.WriteHeader(http.StatusPartialContent)
		_, _ = io.WriteString(w, "2")
		w.(http.Flusher).Flush()
		close(started)
		<-r.Context().Done()
	}))
	t.Cleanup(server.Close)

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	client := Client{Endpoint: Endpoint{BaseURL: server.URL}}
	type result struct {
		part ObjectPart
		err  error
	}
	done := make(chan result, 1)
	go func() {
		part, err := client.RetrieveBulkDataRange(ctx, "/bulk", 2, 3)
		done <- result{part: part, err: err}
	}()
	select {
	case <-started:
		cancel()
	case <-ctx.Done():
		t.Fatal("server did not start the range response")
	}
	select {
	case got := <-done:
		if got.part.Data != nil || !errors.Is(got.err, context.Canceled) {
			t.Fatalf("canceled range returned %v, %v", got.part, got.err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("range read did not return after cancellation")
	}
}

func TestRetrieveBulkDataRangePreflight(t *testing.T) {
	var calls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { calls.Add(1) }))
	t.Cleanup(server.Close)

	for _, tc := range []struct {
		name           string
		offset, length int64
		options        Options
	}{
		{name: "negative offset", offset: -1, length: 1},
		{name: "zero length", length: 0},
		{name: "negative length", length: -1},
		{name: "end overflows", offset: math.MaxInt64, length: 2},
		{name: "body budget", length: 3, options: Options{MaxBodyBytes: 2}},
		{name: "part budget", length: 3, options: Options{ResponseLimits: ResponseLimits{MaxPartBytes: 2}}},
		{name: "invalid limits", length: 3, options: Options{ResponseLimits: ResponseLimits{MaxPartBytes: -1}}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			client := Client{Endpoint: Endpoint{BaseURL: server.URL}, Options: tc.options}
			part, err := client.RetrieveBulkDataRange(context.Background(), "/bulk", tc.offset, tc.length)
			requireClientRangeFailure(t, part, err, ErrorKindRequestFailure, 0)
		})
	}
	if calls.Load() != 0 {
		t.Fatalf("made %d requests before range admission", calls.Load())
	}
}

func TestRetrieveBulkDataRangeRetainsHeadersOnRetryAndRefresh(t *testing.T) {
	var calls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		call := calls.Add(1)
		if r.Header.Get("Range") != "bytes=2-4" || r.Header.Get("Accept-Encoding") != "identity" {
			t.Errorf("attempt %d dropped the range headers: %v", call, r.Header)
		}
		authorization := "Bearer expired-test-token"
		if call >= 3 {
			authorization = "Bearer refreshed-test-token"
		}
		if r.Header.Get("Authorization") != authorization {
			t.Errorf("attempt %d did not use the expected token", call)
		}
		switch call {
		case 1:
			w.WriteHeader(http.StatusServiceUnavailable)
		case 2:
			w.WriteHeader(http.StatusUnauthorized)
		default:
			w.Header().Set("Content-Type", "application/octet-stream")
			w.Header().Set("Content-Range", "bytes 2-4/8")
			w.WriteHeader(http.StatusPartialContent)
			_, _ = io.WriteString(w, "234")
		}
	}))
	t.Cleanup(server.Close)

	source := &rangeTestTokenSource{}
	client := Client{Endpoint: Endpoint{BaseURL: server.URL}, Options: Options{
		BearerTokenSource: source,
		Retry:             &RetryPolicy{MaxAttempts: 2, InitialBackoff: time.Millisecond},
	}}
	part, err := client.RetrieveBulkDataRange(context.Background(), "/bulk", 2, 3)
	if err != nil || string(part.Data) != "234" || calls.Load() != 3 || source.invalidations.Load() != 1 {
		t.Fatalf("retried range: %v, %v; requests %d, invalidations %d", part, err, calls.Load(), source.invalidations.Load())
	}
}

type rangeTestTokenSource struct {
	invalidations atomic.Int32
}

func (s *rangeTestTokenSource) Token(context.Context) (AccessToken, error) {
	if s.invalidations.Load() != 0 {
		return NewAccessToken("refreshed-test-token", time.Time{})
	}
	return NewAccessToken("expired-test-token", time.Time{})
}

func (s *rangeTestTokenSource) Invalidate(AccessToken) {
	s.invalidations.Add(1)
}

func TestRetrieveBulkDataRangeConfinesOrigin(t *testing.T) {
	var foreignCalls atomic.Int32
	var localBulkCalls atomic.Int32
	foreign := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { foreignCalls.Add(1) }))
	t.Cleanup(foreign.Close)
	local := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/foreign":
			http.Redirect(w, r, foreign.URL+"/bulk", http.StatusFound)
		case "/local":
			http.Redirect(w, r, "/bulk", http.StatusFound)
		default:
			localBulkCalls.Add(1)
			if r.Header.Get("Range") != "bytes=2-4" || r.Header.Get("Authorization") != "Bearer range-test-token" {
				t.Error("same-origin redirect lost range or authorization")
			}
			w.Header().Set("Content-Type", "application/octet-stream")
			w.Header().Set("Content-Range", "bytes 2-4/8")
			w.WriteHeader(http.StatusPartialContent)
			_, _ = io.WriteString(w, "234")
		}
	}))
	t.Cleanup(local.Close)

	ctx := context.Background()
	client := Client{Endpoint: Endpoint{BaseURL: local.URL}, Options: Options{BearerToken: "range-test-token"}}
	part, err := client.RetrieveBulkDataRange(ctx, foreign.URL+"/bulk", 2, 3)
	requireClientRangeFailure(t, part, err, ErrorKindInvalidEndpoint, 0)
	part, err = client.RetrieveBulkDataRange(ctx, "/foreign", 2, 3)
	requireClientRangeFailure(t, part, err, ErrorKindRequestFailure, 0)
	part, err = client.RetrieveBulkDataRange(ctx, "/local", 2, 3)
	if err != nil || string(part.Data) != "234" {
		t.Fatalf("same-origin redirect: %v, %v", part, err)
	}

	destination, err := url.Parse(foreign.URL + "/bulk")
	if err != nil {
		t.Fatal(err)
	}
	client.Options.HTTPClient = &http.Client{CheckRedirect: func(r *http.Request, _ []*http.Request) error {
		r.URL = destination
		return nil
	}}
	part, err = client.RetrieveBulkDataRange(ctx, "/local", 2, 3)
	requireClientRangeFailure(t, part, err, ErrorKindRequestFailure, 0)
	for _, host := range []string{"other.example.invalid", destination.Host} {
		client.Options.HTTPClient.CheckRedirect = func(r *http.Request, _ []*http.Request) error {
			r.Host = host
			return nil
		}
		part, err = client.RetrieveBulkDataRange(ctx, "/local", 2, 3)
		requireClientRangeFailure(t, part, err, ErrorKindRequestFailure, 0)
	}
	if foreignCalls.Load() != 0 {
		t.Fatalf("%d range requests escaped the endpoint origin", foreignCalls.Load())
	}
	if calls := localBulkCalls.Load(); calls != 1 {
		t.Fatalf("bulk handler received %d requests; only the same-origin redirect may reach it", calls)
	}
}

func requireClientRangeFailure(t *testing.T, part ObjectPart, err error, kind ErrorKind, status int) {
	t.Helper()
	var webErr *Error
	if part.Data != nil || part.ContentType != "" || part.TransferSyntaxUID != "" ||
		!errors.As(err, &webErr) || webErr.Kind != kind || webErr.StatusCode != status {
		t.Fatalf("range returned part=%v, error=%#v; want only kind %q, status %d", part, err, kind, status)
	}
}
