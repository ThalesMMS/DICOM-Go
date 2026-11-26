package dicomweb

import (
	"context"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

// serverQueueTestServer serves searches that block until release closes,
// with one slot and the given queue.
func serverQueueTestServer(t *testing.T, queued int, wait time.Duration) (*Server, chan struct{}, chan struct{}) {
	t.Helper()
	backend := completeServerTestBackend()
	entered := make(chan struct{}, 8)
	release := make(chan struct{})
	backend.search = func(ctx context.Context, _ SearchRequest, _ func(Dataset) error) (SearchResult, error) {
		entered <- struct{}{}
		select {
		case <-release:
			return SearchResult{}, nil
		case <-ctx.Done():
			return SearchResult{}, ctx.Err()
		}
	}
	server, err := NewServer(ServerOptions{Backend: backend, AllowUnauthenticated: true,
		Limits: ServerLimits{MaxConcurrentRequests: 1, MaxQueuedRequests: queued, MaxQueueWait: wait}})
	if err != nil {
		t.Fatal(err)
	}
	return server, entered, release
}

func serveQueueTestSearch(server *Server, ctx context.Context) *httptest.ResponseRecorder {
	recorder := httptest.NewRecorder()
	request := httptest.NewRequest(http.MethodGet, "/studies", nil).WithContext(ctx)
	request.Header.Set("Accept", "application/dicom+json")
	server.ServeHTTP(recorder, request)
	return recorder
}

// TestServerQueuesExcessRequestsBeforeRefusing checks the bounded queue: a
// request above the concurrency limit waits for a slot and succeeds, one
// above the queue is refused at once and one that waits too long is refused
// when its wait ends, both with 503 and Retry-After.
func TestServerQueuesExcessRequestsBeforeRefusing(t *testing.T) {
	server, entered, release := serverQueueTestServer(t, 1, 5*time.Second)
	results := make(chan *httptest.ResponseRecorder, 2)
	go func() { results <- serveQueueTestSearch(server, context.Background()) }()
	<-entered
	go func() { results <- serveQueueTestSearch(server, context.Background()) }()
	// Wait until the second request holds the only queue place.
	deadline := time.Now().Add(5 * time.Second)
	for len(server.queue) != 1 {
		if time.Now().After(deadline) {
			t.Fatal("the second request never queued")
		}
		time.Sleep(time.Millisecond)
	}
	started := time.Now()
	full := serveQueueTestSearch(server, context.Background())
	if full.Code != http.StatusServiceUnavailable || full.Header().Get("Retry-After") == "" || time.Since(started) > time.Second {
		t.Fatalf("a request above the queue: HTTP %d, Retry-After %q after %v; want an immediate 503 with Retry-After", full.Code, full.Header().Get("Retry-After"), time.Since(started))
	}
	close(release)
	for range 2 {
		if result := <-results; result.Code != http.StatusOK {
			t.Fatalf("a running or queued request answered HTTP %d, want 200", result.Code)
		}
	}
	if len(server.sem) != 0 || len(server.queue) != 0 {
		t.Fatalf("slots %d and queue places %d remain taken", len(server.sem), len(server.queue))
	}
}

func TestServerQueueWaitEndsWithRetryAfter(t *testing.T) {
	server, entered, release := serverQueueTestServer(t, 4, 50*time.Millisecond)
	defer close(release)
	go serveQueueTestSearch(server, context.Background())
	<-entered
	started := time.Now()
	waited := serveQueueTestSearch(server, context.Background())
	elapsed := time.Since(started)
	if waited.Code != http.StatusServiceUnavailable || waited.Header().Get("Retry-After") != "1" || elapsed < 50*time.Millisecond {
		t.Fatalf("a request that waited too long: HTTP %d, Retry-After %q after %v", waited.Code, waited.Header().Get("Retry-After"), elapsed)
	}
	// A canceled client leaves the queue with the canceled status.
	ctx, cancel := context.WithCancel(context.Background())
	time.AfterFunc(10*time.Millisecond, cancel)
	if canceled := serveQueueTestSearch(server, ctx); canceled.Code != statusClientClosedRequest {
		t.Fatalf("a canceled queued request answered HTTP %d, want %d", canceled.Code, statusClientClosedRequest)
	}
	if len(server.queue) != 0 {
		t.Fatalf("%d queue places remain taken", len(server.queue))
	}
}

func TestServerQueueLimitsNormalize(t *testing.T) {
	for _, limits := range []ServerLimits{{MaxQueuedRequests: -1}, {MaxQueueWait: -time.Second}} {
		if _, err := normalizeServerLimits(limits); !errors.Is(err, ErrInvalidServerOptions) {
			t.Errorf("negative queue limits %+v were accepted", limits)
		}
	}
	for _, limits := range []ServerLimits{{MaxQueuedRequests: 4}, {MaxQueueWait: time.Second}} {
		normalized, err := normalizeServerLimits(limits)
		if err != nil || normalized.MaxQueuedRequests != 0 || normalized.MaxQueueWait != 0 {
			t.Errorf("half a queue %+v became %d places for %v (%v), want no queue", limits, normalized.MaxQueuedRequests, normalized.MaxQueueWait, err)
		}
	}
	normalized, err := normalizeServerLimits(ServerLimits{MaxQueuedRequests: 2, MaxQueueWait: time.Hour, MaxDuration: time.Minute})
	if err != nil || normalized.MaxQueueWait != time.Minute {
		t.Errorf("the queue wait %v exceeds the request duration (%v)", normalized.MaxQueueWait, err)
	}
}

// TestServerQueuedRequestKeepsItsConnectionDeadlines serves on the owned
// listener, whose read and write timeouts equal MaxDuration and start with
// each request. A request that waits for the only slot while another works
// 1.2 seconds, and then works as long, ends 2.4 seconds after it arrived:
// past the timeouts it started with, but within the MaxDuration of its own
// work. It must receive its answer, not a connection closed without status.
func TestServerQueuedRequestKeepsItsConnectionDeadlines(t *testing.T) {
	const work = 1200 * time.Millisecond
	backend := completeServerTestBackend()
	backend.search = func(ctx context.Context, _ SearchRequest, _ func(Dataset) error) (SearchResult, error) {
		select {
		case <-time.After(work):
			return SearchResult{}, nil
		case <-ctx.Done():
			return SearchResult{}, ctx.Err()
		}
	}
	server, err := NewServer(ServerOptions{Backend: backend, AllowUnauthenticated: true,
		Limits: ServerLimits{MaxConcurrentRequests: 1, MaxQueuedRequests: 1, MaxQueueWait: 2 * time.Second, MaxDuration: 2 * time.Second}})
	if err != nil {
		t.Fatal(err)
	}
	listener, err := new(net.ListenConfig).Listen(context.Background(), "tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() { done <- server.Serve(listener) }()
	defer func() {
		if err := server.Shutdown(context.Background()); err != nil {
			t.Errorf("Shutdown: %v", err)
		}
		if err := <-done; err != nil {
			t.Errorf("Serve: %v", err)
		}
	}()
	type answer struct {
		status  int
		err     error
		elapsed time.Duration
	}
	get := func(results chan<- answer) {
		started := time.Now()
		request, _ := http.NewRequest(http.MethodGet, "http://"+listener.Addr().String()+"/studies", nil)
		request.Header.Set("Accept", "application/dicom+json")
		response, err := (&http.Client{Transport: &http.Transport{}, Timeout: 10 * time.Second}).Do(request)
		if err != nil {
			results <- answer{err: err, elapsed: time.Since(started)}
			return
		}
		_, err = io.Copy(io.Discard, response.Body)
		response.Body.Close()
		results <- answer{status: response.StatusCode, err: err, elapsed: time.Since(started)}
	}
	results := make(chan answer, 2)
	go get(results)
	deadline := time.Now().Add(2 * time.Second)
	for len(server.sem) != 1 {
		if time.Now().After(deadline) {
			t.Fatal("the first request never started")
		}
		time.Sleep(time.Millisecond)
	}
	go get(results)
	for range 2 {
		if result := <-results; result.err != nil || result.status != http.StatusOK {
			t.Errorf("HTTP %d after %v (%v), want 200", result.status, result.elapsed, result.err)
		}
	}
}
