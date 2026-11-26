package dicomweb

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/ThalesMMS/dicom-go/dicomjson"
	"github.com/ThalesMMS/dicom-go/dictionary/std"
)

func TestMetadataStreamingReturnsBeforeWholeBodyAndDoesNotUseCache(t *testing.T) {
	first := `{"00080018":{"vr":"UI","Value":["1.2.3"]}}`
	second := `{"00080018":{"vr":"UI","Value":["1.2.4"]}}`
	release := make(chan struct{})
	defer close(release)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.Contains(r.Header.Get("Accept"), "application/dicom+json") {
			t.Errorf("Accept %q", r.Header.Get("Accept"))
		}
		w.Header().Set("Content-Type", "application/dicom+json")
		w.Header().Set("Warning", "299 test: remaining")
		_, _ = io.WriteString(w, "["+first+",")
		w.(http.Flusher).Flush()
		select {
		case <-release:
		case <-r.Context().Done():
			return
		}
		_, _ = io.WriteString(w, second+"]")
	}))
	defer server.Close()
	client := Client{Endpoint: Endpoint{BaseURL: server.URL}, Options: Options{Cache: NewResponseCache(1<<20, time.Minute)}}
	sentinel := errors.New("callback stop")
	consumed := make(chan struct{})
	done := make(chan error, 1)
	go func() {
		response, err := client.SearchStudiesStream(context.Background(), nil, func(ds Dataset) error {
			if dicomjson.ElementString(ds, "00080018") == "1.2.3" {
				close(consumed)
			}
			return sentinel
		})
		if len(response.Body) != 0 || response.Header.Get("Warning") == "" {
			t.Errorf("response ownership/header %v", response)
		}
		done <- err
	}()
	select {
	case <-consumed:
	case <-time.After(2 * time.Second):
		t.Fatal("callback waited for entire body")
	}
	if err := <-done; !errors.Is(err, sentinel) {
		t.Fatalf("callback error %v", err)
	}
	if stats := client.Options.Cache.Stats(); stats.Entries != 0 || stats.Hits != 0 {
		t.Fatalf("stream entered resident cache %v", stats)
	}
}

func TestMetadataStreamingScopesLateErrorsLimitsAndCancellation(t *testing.T) {
	first := `{"0020000D":{"vr":"UI","Value":["1.2"]},"0020000E":{"vr":"UI","Value":["1.2.3"]},"00080018":{"vr":"UI","Value":["1.2.3.4"]}}`
	second := `{"0020000D":{"vr":"UI","Value":["1.2"]},"0020000E":{"vr":"UI","Value":["1.2.3"]},"00080018":{"vr":"UI","Value":["1.2.3.5"]}}`
	body := "[" + first + "," + second + "]"
	var requests atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests.Add(1)
		w.Header().Set("Content-Type", "application/dicom+json")
		if r.URL.Path == "/studies/1.2/series/1.2.3/instances/1.2.3.4/metadata" {
			_, _ = io.WriteString(w, "["+first+"]")
			return
		}
		_, _ = io.WriteString(w, body)
	}))
	defer server.Close()
	client := Client{Endpoint: Endpoint{BaseURL: server.URL}}
	for _, test := range []struct {
		want int
		call func(func(Dataset) error) (Response, error)
	}{
		{2, func(h func(Dataset) error) (Response, error) {
			return client.SearchStudiesStream(context.Background(), url.Values{"limit": {"2"}}, h)
		}},
		{2, func(h func(Dataset) error) (Response, error) {
			return client.SearchSeriesStream(context.Background(), "1.2", nil, h)
		}},
		{2, func(h func(Dataset) error) (Response, error) {
			return client.SearchInstancesStream(context.Background(), "1.2", "1.2.3", nil, h)
		}},
		{2, func(h func(Dataset) error) (Response, error) {
			return client.StudyMetadataStream(context.Background(), "1.2", h)
		}},
		{2, func(h func(Dataset) error) (Response, error) {
			return client.SeriesMetadataStream(context.Background(), "1.2", "1.2.3", h)
		}},
		{1, func(h func(Dataset) error) (Response, error) {
			return client.InstanceMetadataStream(context.Background(), InstanceRef{"1.2", "1.2.3", "1.2.3.4"}, h)
		}},
	} {
		count := 0
		response, err := test.call(func(Dataset) error { count++; return nil })
		if err != nil || count != test.want || response.Body != nil {
			t.Fatalf("scope count %d, want %d, response %v, err %v", count, test.want, response, err)
		}
	}
	client.Options.MaxMetadataParts = 1
	count := 0
	_, err := client.SearchStudiesStream(context.Background(), nil, func(Dataset) error { count++; return nil })
	var limit *ResponseDecodeError
	if !errors.As(err, &limit) || limit.Kind != ResponseDecodePartLimit || count != 1 {
		t.Fatalf("count limit %d/%v", count, err)
	}
	client.Options.MaxMetadataParts = 0
	client.Options.MaxBodyBytes = int64(len(body) - 1)
	_, err = client.SearchStudiesStream(context.Background(), nil, func(Dataset) error { return nil })
	if err == nil {
		t.Fatal("whole body limit ignored")
	}
	client.Options.MaxBodyBytes = 0
	ctx, cancel := context.WithCancel(context.Background())
	count = 0
	_, err = client.SearchStudiesStream(ctx, nil, func(Dataset) error { count++; cancel(); return nil })
	if !errors.Is(err, context.Canceled) || count != 1 {
		t.Fatalf("cancel %d/%v", count, err)
	}
	before := requests.Load()
	client.Options.MetadataMediaTypes = []MetadataMediaType{MetadataMediaTypeDICOMXML}
	_, err = client.SearchStudiesStream(context.Background(), nil, func(Dataset) error { return nil })
	if err == nil || requests.Load() != before {
		t.Fatal("unsupported configured representation caused request")
	}
}
func TestMetadataStreamingClosesOnCallbackErrorAndDoesNotRetryConsumedBody(t *testing.T) {
	closed := atomic.Bool{}
	attempts := atomic.Int32{}
	sentinel := errors.New("handler stop")
	client := Client{Endpoint: Endpoint{BaseURL: "http://example.test"}, Options: Options{Retry: &RetryPolicy{MaxAttempts: 3}, HTTPClient: &http.Client{Transport: metadataStreamRoundTripper(func(r *http.Request) (*http.Response, error) {
		attempts.Add(1)
		return &http.Response{StatusCode: 200, Header: http.Header{"Content-Type": {"application/dicom+json"}}, Request: r, Body: &metadataStreamCloser{Reader: strings.NewReader(`[{"00080018":{"vr":"UI","Value":["1.2.3"]}},broken]`), closed: &closed}}, nil
	})}}}
	count := 0
	_, err := client.SearchStudiesStream(context.Background(), nil, func(Dataset) error { count++; return sentinel })
	if !errors.Is(err, sentinel) || count != 1 || attempts.Load() != 1 || !closed.Load() {
		t.Fatalf("cleanup/replay count%d attempts%d closed%t error%v", count, attempts.Load(), closed.Load(), err)
	}
}

type metadataStreamRoundTripper func(*http.Request) (*http.Response, error)

func (f metadataStreamRoundTripper) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

type metadataStreamCloser struct {
	io.Reader
	closed *atomic.Bool
}

func (c *metadataStreamCloser) Close() error { c.closed.Store(true); return nil }

func TestResponseStreamingParityGenericShapesAndLateHTTPFailure(t *testing.T) {
	dataset := Dataset{
		"00081111": {VR: "SQ", Value: []any{map[string]any{"00100020": map[string]any{"vr": "LO", "Value": []string{"<é>"}}, "00280010": map[string]any{"vr": "US", "Value": []uint16{42}}}}},
		"00100010": {VR: "PN", Value: []any{dicomjson.PersonNameComponents{Alphabetic: "A^B"}}},
	}
	server := &Server{limits: DefaultServerLimits()}
	old, err := legacyResponseEncodeForTest(server, dataset)
	if err != nil {
		t.Fatal(err)
	}
	got, err := server.encodeResponseDataset(context.Background(), dataset)
	if err != nil || !bytes.Equal(got, old) {
		t.Fatalf("byte parity got%s old%s err%v", got, old, err)
	}
	spool := t.TempDir()
	backend := completeServerTestBackend()
	backend.search = func(_ context.Context, _ SearchRequest, yield func(Dataset) error) (SearchResult, error) {
		if err := yield(dataset); err != nil {
			return SearchResult{}, err
		}
		return SearchResult{}, yield(Dataset{"00280010": {VR: "US", Value: []any{-1}}})
	}
	srv, err := NewServer(ServerOptions{Backend: backend, AllowUnauthenticated: true, SpoolDirectory: spool})
	if err != nil {
		t.Fatal(err)
	}
	response := httptest.NewRecorder()
	request := httptest.NewRequest(http.MethodGet, "/studies", nil)
	request.Header.Set("Accept", "application/dicom+json")
	srv.ServeHTTP(response, request)
	if response.Code != 500 || bytes.Contains(response.Body.Bytes(), old) {
		t.Fatalf("staged invalid dataset published: %d/%s", response.Code, response.Body.Bytes())
	}
	entries, err := os.ReadDir(spool)
	if err != nil || len(entries) != 0 {
		t.Fatalf("spool not cleaned %v %v", entries, err)
	}
}
func legacyResponseEncodeForTest(server *Server, dataset Dataset) ([]byte, error) {
	if err := server.validateResponseDataset(context.Background(), dataset); err != nil {
		return nil, err
	}
	encoded, err := json.Marshal(dataset)
	if err != nil {
		return nil, err
	}
	if int64(len(encoded)) > server.limits.MaxResponseBytes {
		return nil, ErrResourceLimit
	}
	if _, err := dicomjson.Unmarshal(encoded, std.Dictionary); err != nil {
		return nil, err
	}
	return encoded, nil
}

func TestResponseStreamingPoisonedSpoolNeverPublishesIgnoredYieldFailure(t *testing.T) {
	spool := t.TempDir()
	backend := completeServerTestBackend()
	backend.search = func(_ context.Context, _ SearchRequest, yield func(Dataset) error) (SearchResult, error) {
		// More than a buffer is staged before the last sorted tag fails its VR.
		invalid := Dataset{"00100020": {VR: "LO", Value: []any{strings.Repeat("x", 40<<10)}}, "00280010": {VR: "US", Value: []any{-1}}}
		_ = yield(invalid)
		_ = yield(Dataset{"00080018": {VR: "UI", Value: []any{"1.2.3"}}})
		return SearchResult{}, nil // A broken backend must not hide poisoned output.
	}
	server, err := NewServer(ServerOptions{Backend: backend, AllowUnauthenticated: true, SpoolDirectory: spool})
	if err != nil {
		t.Fatal(err)
	}
	response := httptest.NewRecorder()
	request := httptest.NewRequest(http.MethodGet, "/studies", nil)
	request.Header.Set("Accept", "application/dicom+json")
	server.ServeHTTP(response, request)
	if response.Code != 500 || bytes.Contains(response.Body.Bytes(), []byte(strings.Repeat("x", 128))) {
		t.Fatalf("poisoned spool published %d/%d", response.Code, response.Body.Len())
	}
	entries, err := os.ReadDir(spool)
	if err != nil || len(entries) != 0 {
		t.Fatalf("poisoned spool cleanup %v/%v", entries, err)
	}
}
