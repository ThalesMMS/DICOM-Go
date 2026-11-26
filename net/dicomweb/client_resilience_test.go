package dicomweb

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// pagingSearchBackend serves total synthetic studies honoring offset and
// limit, and reports the remaining count as PS3.18 Warning 299 requires.
func pagingSearchBackend(total int, calls *atomic.Int32, levels *[]SearchLevel, mu *sync.Mutex) *serverTestBackend {
	backend := completeServerTestBackend()
	backend.search = func(_ context.Context, request SearchRequest, yield func(Dataset) error) (SearchResult, error) {
		calls.Add(1)
		mu.Lock()
		*levels = append(*levels, request.Level)
		mu.Unlock()
		end := total
		if request.Offset > total {
			request.Offset = total
		}
		if request.MaximumResults > 0 && request.Offset+request.MaximumResults < end {
			end = request.Offset + request.MaximumResults
		}
		for index := request.Offset; index < end; index++ {
			if err := yield(Dataset{"0020000D": {VR: "UI", Value: []any{fmt.Sprintf("1.2.3.%d", index+1)}}}); err != nil {
				return SearchResult{}, err
			}
		}
		remaining := total - end
		return SearchResult{Remaining: &remaining}, nil
	}
	return backend
}

func studyUIDs(datasets []Dataset) string {
	values := make([]string, 0, len(datasets))
	for _, dataset := range datasets {
		values = append(values, dataset["0020000D"].Value[0].(string))
	}
	return strings.Join(values, ",")
}

func TestSearchPagerWalksEveryPageAgainstServer(t *testing.T) {
	tests := []struct {
		name          string
		total         int
		serverMax     int
		opts          SearchPagerOptions
		params        url.Values
		pager         func(Client, url.Values, SearchPagerOptions) *SearchPager
		wantRequests  int32
		wantUIDs      string
		wantWarnPages int
		level         SearchLevel
	}{
		{
			name: "three pages", total: 7, opts: SearchPagerOptions{PageSize: 3},
			pager: func(c Client, p url.Values, o SearchPagerOptions) *SearchPager { return c.StudyPager(p, o) },
			// 3 + 3 + 1: the short last page ends the search.
			wantRequests: 3, wantUIDs: "1.2.3.1,1.2.3.2,1.2.3.3,1.2.3.4,1.2.3.5,1.2.3.6,1.2.3.7",
			wantWarnPages: 2, level: SearchLevelStudy,
		},
		{
			name: "server caps the limit and warns", total: 7, serverMax: 2, opts: SearchPagerOptions{PageSize: 3},
			pager:        func(c Client, p url.Values, o SearchPagerOptions) *SearchPager { return c.StudyPager(p, o) },
			wantRequests: 4, wantUIDs: "1.2.3.1,1.2.3.2,1.2.3.3,1.2.3.4,1.2.3.5,1.2.3.6,1.2.3.7",
			wantWarnPages: 3, level: SearchLevelStudy,
		},
		{
			name: "starting offset and result cap", total: 9, opts: SearchPagerOptions{PageSize: 2, MaxResults: 3},
			params:       url.Values{"offset": {"4"}, "limit": {"50"}},
			pager:        func(c Client, p url.Values, o SearchPagerOptions) *SearchPager { return c.StudyPager(p, o) },
			wantRequests: 2, wantUIDs: "1.2.3.5,1.2.3.6,1.2.3.7",
			wantWarnPages: 2, level: SearchLevelStudy,
		},
		{
			name: "exact multiple needs one empty page", total: 6, opts: SearchPagerOptions{PageSize: 3},
			pager:        func(c Client, p url.Values, o SearchPagerOptions) *SearchPager { return c.StudyPager(p, o) },
			wantRequests: 3, wantUIDs: "1.2.3.1,1.2.3.2,1.2.3.3,1.2.3.4,1.2.3.5,1.2.3.6",
			wantWarnPages: 1, level: SearchLevelStudy,
		},
		{
			name: "series", total: 5, opts: SearchPagerOptions{PageSize: 2},
			pager: func(c Client, p url.Values, o SearchPagerOptions) *SearchPager {
				return c.SeriesPager("1.2.3", p, o)
			},
			wantRequests: 3, wantUIDs: "1.2.3.1,1.2.3.2,1.2.3.3,1.2.3.4,1.2.3.5",
			wantWarnPages: 2, level: SearchLevelSeries,
		},
		{
			name: "instances", total: 4, opts: SearchPagerOptions{PageSize: 3},
			pager: func(c Client, p url.Values, o SearchPagerOptions) *SearchPager {
				return c.InstancePager("1.2.3", "1.2.3.1", p, o)
			},
			wantRequests: 2, wantUIDs: "1.2.3.1,1.2.3.2,1.2.3.3,1.2.3.4",
			wantWarnPages: 1, level: SearchLevelInstance,
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			var (
				calls  atomic.Int32
				mu     sync.Mutex
				levels []SearchLevel
			)
			backend := pagingSearchBackend(test.total, &calls, &levels, &mu)
			_, httpServer := newOpenTestServer(t, backend, func(options *ServerOptions) {
				options.Limits.MaxResults = test.serverMax
			})
			client := Client{Endpoint: Endpoint{BaseURL: httpServer.URL}}
			pager := test.pager(client, test.params, test.opts)
			var all []Dataset
			warned := 0
			for {
				page, ok, err := pager.Next(context.Background())
				if err != nil {
					t.Fatalf("Next: %v", err)
				}
				if !ok {
					break
				}
				if page.MoreResults {
					warned++
				}
				all = append(all, page.Datasets...)
			}
			if !pager.Done() || calls.Load() != test.wantRequests || studyUIDs(all) != test.wantUIDs || warned != test.wantWarnPages {
				t.Fatalf("requests=%d uids=%s warned=%d done=%v", calls.Load(), studyUIDs(all), warned, pager.Done())
			}
			for _, level := range levels {
				if level != test.level {
					t.Fatalf("level = %s, want %s", level, test.level)
				}
			}
			if page, ok, err := pager.Next(context.Background()); ok || err != nil || page.Datasets != nil {
				t.Fatalf("Next after completion = %+v %v %v", page, ok, err)
			}
		})
	}
}

func TestSearchPagerStopsOnNonAdvancingServers(t *testing.T) {
	tests := []struct {
		name    string
		handler func(w http.ResponseWriter, r *http.Request)
		opts    SearchPagerOptions
		wantErr error
	}{
		{
			name: "offset ignored",
			handler: func(w http.ResponseWriter, _ *http.Request) {
				w.Header().Set("Content-Type", "application/dicom+json")
				_, _ = w.Write([]byte(`[{"0020000D":{"vr":"UI","Value":["1.2"]}},{"0020000D":{"vr":"UI","Value":["1.3"]}}]`))
			},
			opts: SearchPagerOptions{PageSize: 2}, wantErr: ErrSearchPagination,
		},
		{
			name: "more announced but nothing returned",
			handler: func(w http.ResponseWriter, _ *http.Request) {
				w.Header().Set("Warning", `299 example: There are 5 additional results that can be requested`)
				w.WriteHeader(http.StatusNoContent)
			},
			opts: SearchPagerOptions{PageSize: 2}, wantErr: ErrSearchPagination,
		},
		{
			name: "page limit",
			handler: func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "application/dicom+json")
				_, _ = fmt.Fprintf(w, `[{"0020000D":{"vr":"UI","Value":["1.%s"]}}]`, r.URL.Query().Get("offset"))
			},
			opts: SearchPagerOptions{PageSize: 1, MaxPages: 3}, wantErr: ErrSearchPageLimit,
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			httpServer := httptest.NewServer(http.HandlerFunc(test.handler))
			defer httpServer.Close()
			client := Client{Endpoint: Endpoint{BaseURL: httpServer.URL}}
			if _, err := client.StudyPager(nil, test.opts).All(context.Background()); !errors.Is(err, test.wantErr) {
				t.Fatalf("All error = %v, want %v", err, test.wantErr)
			}
		})
	}
	if _, _, err := (Client{Endpoint: Endpoint{BaseURL: "http://127.0.0.1:1"}}).StudyPager(url.Values{"offset": {"x"}}, SearchPagerOptions{}).Next(context.Background()); err == nil {
		t.Fatal("invalid offset was accepted")
	}
}

func TestStudySearchParamsOffset(t *testing.T) {
	params, err := StudySearchParams(StudySearchCriteria{Limit: 10, Offset: 20})
	if err != nil || params.Get("limit") != "10" || params.Get("offset") != "20" {
		t.Fatalf("params = %v, %v", params, err)
	}
}

type retryTestServer struct {
	attempts atomic.Int32
	statuses []int
	header   http.Header
	server   *httptest.Server
}

func newRetryTestServer(t *testing.T, statuses []int, header http.Header) *retryTestServer {
	t.Helper()
	server := &retryTestServer{statuses: statuses, header: header}
	server.server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		attempt := int(server.attempts.Add(1))
		status := server.statuses[len(server.statuses)-1]
		if attempt <= len(server.statuses) {
			status = server.statuses[attempt-1]
		}
		if status == 0 {
			hijacker, ok := w.(http.Hijacker)
			if !ok {
				t.Error("response writer cannot hijack")
				return
			}
			conn, _, err := hijacker.Hijack()
			if err == nil {
				_ = conn.Close()
			}
			return
		}
		for name, values := range server.header {
			w.Header()[name] = values
		}
		if status == http.StatusOK {
			w.Header().Set("Content-Type", "application/dicom+json")
			w.WriteHeader(status)
			_, _ = w.Write([]byte(`[{"0020000D":{"vr":"UI","Value":["1.2.3"]}}]`))
			return
		}
		if r.Method == http.MethodPost {
			_, _ = r.Body.Read(make([]byte, 1))
		}
		w.WriteHeader(status)
	}))
	t.Cleanup(server.server.Close)
	return server
}

func TestRetryPolicyRetriesSimulatedFailures(t *testing.T) {
	fast := func(attempts int) *RetryPolicy {
		return &RetryPolicy{MaxAttempts: attempts, InitialBackoff: time.Millisecond, MaxBackoff: 4 * time.Millisecond, Jitter: 0.5}
	}
	tests := []struct {
		name         string
		statuses     []int
		header       http.Header
		policy       *RetryPolicy
		wantAttempts int32
		wantStatus   int
		wantRetries  []int
	}{
		{"503 then success", []int{503, 503, 200}, nil, fast(3), 3, 200, []int{503, 503}},
		{"503 exhausts attempts", []int{503}, nil, fast(3), 3, 503, []int{503, 503}},
		{"429 and 504 default statuses", []int{429, 504, 200}, nil, fast(4), 3, 200, []int{429, 504}},
		{"transport failure", []int{0, 200}, nil, fast(2), 2, 200, []int{0}},
		{"404 is not retried", []int{404}, nil, fast(3), 1, 404, nil},
		{"custom status list", []int{500, 200}, nil, &RetryPolicy{MaxAttempts: 2, InitialBackoff: time.Millisecond, RetryStatusCodes: []int{500}}, 2, 200, []int{500}},
		{"no policy", []int{503, 200}, nil, nil, 1, 503, nil},
		{"one attempt", []int{503, 200}, nil, fast(1), 1, 503, nil},
		{"short Retry-After honored", []int{503, 200}, http.Header{"Retry-After": {"0"}}, fast(2), 2, 200, []int{503}},
		{"long Retry-After returns the response", []int{503, 200}, http.Header{"Retry-After": {"120"}}, fast(3), 1, 503, nil},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			server := newRetryTestServer(t, test.statuses, test.header)
			var retried []int
			if test.policy != nil {
				test.policy.OnRetry = func(attempt, status int, delay time.Duration) {
					if delay < 0 || delay > test.policy.MaxBackoff && test.policy.MaxBackoff > 0 {
						t.Errorf("attempt %d delay %s out of range", attempt, delay)
					}
					retried = append(retried, status)
				}
			}
			client := Client{Endpoint: Endpoint{BaseURL: server.server.URL}, Options: Options{Retry: test.policy}}
			datasets, err := client.SearchStudies(context.Background(), nil)
			if got := server.attempts.Load(); got != test.wantAttempts {
				t.Fatalf("attempts = %d, want %d", got, test.wantAttempts)
			}
			if fmt.Sprint(retried) != fmt.Sprint(test.wantRetries) {
				t.Fatalf("retried statuses = %v, want %v", retried, test.wantRetries)
			}
			if test.wantStatus == http.StatusOK {
				if err != nil || len(datasets) != 1 {
					t.Fatalf("SearchStudies = %v, %v", datasets, err)
				}
				return
			}
			var webErr *Error
			if !errors.As(err, &webErr) || webErr.StatusCode != test.wantStatus {
				t.Fatalf("error = %v, want status %d", err, test.wantStatus)
			}
		})
	}
}

func TestRetryPolicyRespectsContextAndRequestBodies(t *testing.T) {
	server := newRetryTestServer(t, []int{503}, nil)
	client := Client{Endpoint: Endpoint{BaseURL: server.server.URL}, Options: Options{Retry: &RetryPolicy{MaxAttempts: 5, InitialBackoff: time.Hour, MaxBackoff: time.Hour}}}
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	started := time.Now()
	_, err := client.SearchStudies(ctx, nil)
	if !errors.Is(err, context.DeadlineExceeded) || time.Since(started) > 5*time.Second {
		t.Fatalf("error = %v after %s, want deadline during backoff", err, time.Since(started))
	}
	if server.attempts.Load() != 1 {
		t.Fatalf("attempts = %d, want 1", server.attempts.Load())
	}

	canceled, cancelNow := context.WithCancel(context.Background())
	cancelNow()
	server.attempts.Store(0)
	if _, err := client.SearchStudies(canceled, nil); err == nil || server.attempts.Load() > 1 {
		t.Fatalf("canceled search = %v after %d attempts", err, server.attempts.Load())
	}

	// STOW-RS streams its body once and is never replayed.
	server.attempts.Store(0)
	client.Options.Retry = &RetryPolicy{MaxAttempts: 3, InitialBackoff: time.Millisecond}
	_, err = client.StoreInstances(context.Background(), []StoreInstance{{Data: []byte("DICM")}})
	if err == nil || server.attempts.Load() != 1 {
		t.Fatalf("STOW = %v after %d attempts, want one attempt", err, server.attempts.Load())
	}
}

func TestRetryPolicyBackoffAndValidation(t *testing.T) {
	settings, err := (&RetryPolicy{InitialBackoff: 10 * time.Millisecond, MaxBackoff: 50 * time.Millisecond, Multiplier: 2, Jitter: 0.5}).settings()
	if err != nil {
		t.Fatal(err)
	}
	bases := []time.Duration{10, 20, 40, 50, 50}
	for index, base := range bases {
		base *= time.Millisecond
		for range 50 {
			delay := settings.backoff(index + 1)
			if delay < base/2 || delay > base {
				t.Fatalf("attempt %d delay %s outside [%s, %s]", index+1, delay, base/2, base)
			}
		}
	}
	defaults, err := DefaultRetryPolicy().settings()
	if err != nil || defaults.attempts != 3 || !defaults.statuses[503] || defaults.statuses[500] {
		t.Fatalf("defaults = %+v, %v", defaults, err)
	}
	for _, invalid := range []*RetryPolicy{
		{MaxAttempts: -1}, {Multiplier: 0.5}, {Jitter: 1.5}, {InitialBackoff: -1}, {RetryStatusCodes: []int{42}},
	} {
		client := Client{Endpoint: Endpoint{BaseURL: "http://127.0.0.1:1"}, Options: Options{Retry: invalid}}
		if _, err := client.SearchStudies(context.Background(), nil); err == nil {
			t.Fatalf("invalid policy %+v accepted", invalid)
		}
	}
	if delay, ok := retryAfter(http.Header{"Retry-After": {time.Unix(100, 0).UTC().Format(http.TimeFormat)}}, time.Unix(90, 0)); !ok || delay != 10*time.Second {
		t.Fatalf("Retry-After date = %s, %v", delay, ok)
	}
}

func TestResponseCacheServesRepeatedGETs(t *testing.T) {
	var (
		calls  atomic.Int32
		mu     sync.Mutex
		levels []SearchLevel
	)
	backend := pagingSearchBackend(3, &calls, &levels, &mu)
	backend.metadata = func(_ context.Context, _ MetadataRequest, yield func(Dataset) error) error {
		calls.Add(1)
		return yield(Dataset{"00080018": {VR: "UI", Value: []any{"1.2.3.4"}}})
	}
	_, httpServer := newOpenTestServer(t, backend, nil)
	cache := NewResponseCache(1<<20, 0)
	client := Client{Endpoint: Endpoint{BaseURL: httpServer.URL}, Options: Options{Cache: cache}}
	ctx := context.Background()
	params := url.Values{"PatientID": {"SYNTHETIC"}}

	for range 3 {
		if datasets, err := client.SearchStudies(ctx, params); err != nil || len(datasets) != 3 {
			t.Fatalf("SearchStudies = %d, %v", len(datasets), err)
		}
	}
	if calls.Load() != 1 {
		t.Fatalf("cached search reached the server %d times", calls.Load())
	}
	// A different query and a different Accept header are distinct entries.
	if _, err := client.SearchStudies(ctx, url.Values{"PatientID": {"OTHER"}}); err != nil {
		t.Fatal(err)
	}
	xml := client
	xml.Options.MetadataMediaTypes = []MetadataMediaType{MetadataMediaTypeDICOMXML}
	if _, err := xml.SearchStudies(ctx, params); err != nil {
		t.Fatal(err)
	}
	if calls.Load() != 3 {
		t.Fatalf("distinct keys made %d server calls, want 3", calls.Load())
	}
	ref := InstanceRef{StudyInstanceUID: "1.2.3", SeriesInstanceUID: "1.2.3.1", SOPInstanceUID: "1.2.3.4"}
	for range 2 {
		if _, err := client.InstanceMetadata(ctx, ref); err != nil {
			t.Fatal(err)
		}
	}
	if calls.Load() != 4 {
		t.Fatalf("cached metadata made %d server calls, want 4", calls.Load())
	}
	// Verify always reaches the server.
	before := calls.Load()
	if _, err := client.Verify(ctx); err != nil {
		t.Fatal(err)
	}
	if _, err := client.Verify(ctx); err != nil {
		t.Fatal(err)
	}
	if calls.Load() != before+2 {
		t.Fatal("Verify was served from the cache")
	}
	stats := cache.Stats()
	if stats.Hits != 3 || stats.Entries != 4 || stats.Bytes <= 0 {
		t.Fatalf("stats = %+v", stats)
	}
	cache.Purge()
	if stats := cache.Stats(); stats.Entries != 0 || stats.Bytes != 0 {
		t.Fatalf("purged stats = %+v", stats)
	}
}

func TestResponseCacheBoundsBytesAndHonorsPolicy(t *testing.T) {
	key := func(index int) responseCacheKey { return responseCacheKey{url: fmt.Sprintf("u%d", index), accept: "a"} }
	response := func(body string, header http.Header) bufferedGET {
		if header == nil {
			header = http.Header{}
		}
		return bufferedGET{Response: Response{StatusCode: http.StatusOK, Header: header, Body: []byte(body)}}
	}
	cache := NewResponseCache(64, 0)
	for index := range 5 {
		cache.put(key(index), response(strings.Repeat("x", 20), nil))
	}
	if stats := cache.Stats(); stats.Bytes > 64 || stats.Entries != 2 {
		t.Fatalf("stats after eviction = %+v", stats)
	}
	if _, ok := cache.get(key(0)); ok {
		t.Fatal("least recently used entry survived")
	}
	if _, ok := cache.get(key(4)); !ok {
		t.Fatal("newest entry was evicted")
	}
	cache.put(key(9), response(strings.Repeat("y", 100), nil))
	if _, ok := cache.get(key(9)); ok {
		t.Fatal("entry larger than the cache was stored")
	}
	cache.put(key(10), response("z", http.Header{"Cache-Control": {"no-store"}}))
	if _, ok := cache.get(key(10)); ok {
		t.Fatal("no-store response was cached")
	}
	failed := response("err", nil)
	failed.StatusCode = http.StatusServiceUnavailable
	cache.put(key(11), failed)
	if _, ok := cache.get(key(11)); ok {
		t.Fatal("error response was cached")
	}
	// Returned responses are copies.
	cache.put(key(12), response("abc", nil))
	first, _ := cache.get(key(12))
	first.Body[0] = 'X'
	second, _ := cache.get(key(12))
	if string(second.Body) != "abc" {
		t.Fatalf("cached body mutated to %q", second.Body)
	}

	now := time.Unix(1000, 0)
	expiring := NewResponseCache(1<<10, time.Minute)
	expiring.now = func() time.Time { return now }
	expiring.put(key(1), response("abc", nil))
	if _, ok := expiring.get(key(1)); !ok {
		t.Fatal("fresh entry missing")
	}
	now = now.Add(time.Minute)
	if _, ok := expiring.get(key(1)); ok {
		t.Fatal("expired entry served")
	}
	if NewResponseCache(0, 0) != nil {
		t.Fatal("zero-byte cache should be disabled")
	}
	var disabled *ResponseCache
	disabled.put(key(1), response("abc", nil))
	if _, ok := disabled.get(key(1)); ok || disabled.Stats() != (ResponseCacheStats{}) {
		t.Fatal("nil cache stored a response")
	}
}
