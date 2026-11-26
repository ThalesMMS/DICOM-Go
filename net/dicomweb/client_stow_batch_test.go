package dicomweb

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"mime"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"sync"
	"testing"
)

// stowBatchServer answers STOW-RS requests, records the parts of each and
// refuses with 413 a request with more than maxParts parts (0: no limit).
type stowBatchServer struct {
	mu       sync.Mutex
	requests [][]string
	maxParts int
}

func (s *stowBatchServer) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	_, params, err := mime.ParseMediaType(r.Header.Get("Content-Type"))
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	reader := multipart.NewReader(r.Body, params["boundary"])
	var parts []string
	for {
		part, err := reader.NextPart()
		if err == io.EOF {
			break
		}
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		data, _ := io.ReadAll(part)
		parts = append(parts, string(data))
	}
	s.mu.Lock()
	s.requests = append(s.requests, parts)
	s.mu.Unlock()
	if s.maxParts > 0 && len(parts) > s.maxParts {
		http.Error(w, "too large", http.StatusRequestEntityTooLarge)
		return
	}
	w.Header().Set("Content-Type", "application/dicom+json")
	_, _ = w.Write([]byte("{}"))
}

func (s *stowBatchServer) sizes() []int {
	s.mu.Lock()
	defer s.mu.Unlock()
	sizes := make([]int, len(s.requests))
	for i, request := range s.requests {
		sizes[i] = len(request)
	}
	return sizes
}

func stowBatchInstances(count int, size int64) []StoreInstance {
	instances := make([]StoreInstance, count)
	for i := range instances {
		payload := []byte(fmt.Sprintf("object-%02d", i))
		instances[i] = StoreInstance{
			SOPInstanceUID: fmt.Sprintf("1.2.3.%d", i),
			Size:           size,
			Open:           func() (io.ReadCloser, error) { return io.NopCloser(bytes.NewReader(payload)), nil },
		}
	}
	return instances
}

func stowBatchClient(t *testing.T, handler http.Handler) Client {
	t.Helper()
	server := httptest.NewServer(handler)
	t.Cleanup(server.Close)
	return Client{Endpoint: Endpoint{BaseURL: server.URL + "/dicom-web"}, Options: Options{HTTPClient: server.Client()}}
}

type stowBatchRecord struct {
	start, end int
	status     int
	reduced    bool
}

func stowBatchRecorder(records *[]stowBatchRecord) func(StoreBatch) error {
	return func(batch StoreBatch) error {
		*records = append(*records, stowBatchRecord{batch.Start, batch.End, batch.Result.StatusCode, batch.Reduced})
		return nil
	}
}

func TestStoreInstancesBatchedSplitsByCountAndBytes(t *testing.T) {
	for _, tc := range []struct {
		name      string
		instances []StoreInstance
		limits    StoreBatchLimits
		want      []int
	}{
		{"count", stowBatchInstances(7, 10), StoreBatchLimits{MaxInstances: 3}, []int{3, 3, 1}},
		{"bytes", stowBatchInstances(7, 10), StoreBatchLimits{MaxBytes: 25}, []int{2, 2, 2, 1}},
		{"default", stowBatchInstances(250, 1), StoreBatchLimits{}, []int{100, 100, 50}},
		{"object-above-the-limit-alone", append(stowBatchInstances(2, 10), append(stowBatchInstances(1, 100), stowBatchInstances(2, 10)...)...), StoreBatchLimits{MaxBytes: 30}, []int{2, 1, 2}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			server := &stowBatchServer{}
			var records []stowBatchRecord
			if err := stowBatchClient(t, server).StoreInstancesBatched(context.Background(), tc.instances, tc.limits, stowBatchRecorder(&records)); err != nil {
				t.Fatal(err)
			}
			if got := server.sizes(); !reflect.DeepEqual(got, tc.want) {
				t.Fatalf("request sizes = %v, want %v", got, tc.want)
			}
			end := 0
			for _, record := range records {
				if record.start != end || record.status != http.StatusOK || record.reduced {
					t.Fatalf("batches = %+v", records)
				}
				end = record.end
			}
			if end != len(tc.instances) {
				t.Fatalf("batches covered %d of %d instances", end, len(tc.instances))
			}
			planned := PlanStoreBatches(tc.instances, tc.limits)
			if len(planned) != len(tc.want) || planned[len(planned)-1] != len(tc.instances) {
				t.Fatalf("plan = %v for requests %v", planned, tc.want)
			}
		})
	}
}

func TestStoreInstancesBatchedReducesARefusedBatchOnce(t *testing.T) {
	t.Run("halves-succeed-and-later-batches-stay-reduced", func(t *testing.T) {
		server := &stowBatchServer{maxParts: 4}
		var records []stowBatchRecord
		if err := stowBatchClient(t, server).StoreInstancesBatched(context.Background(), stowBatchInstances(12, 10), StoreBatchLimits{MaxInstances: 8}, stowBatchRecorder(&records)); err != nil {
			t.Fatal(err)
		}
		if got := server.sizes(); !reflect.DeepEqual(got, []int{8, 4, 4, 4}) {
			t.Fatalf("request sizes = %v", got)
		}
		want := []stowBatchRecord{{0, 4, 200, true}, {4, 8, 200, true}, {8, 12, 200, false}}
		if !reflect.DeepEqual(records, want) {
			t.Fatalf("batches = %+v, want %+v", records, want)
		}
	})
	t.Run("two-halves-whatever-the-sizes", func(t *testing.T) {
		server := &stowBatchServer{maxParts: 3}
		instances := append(stowBatchInstances(4, 10), stowBatchInstances(1, 1000)...)
		var records []stowBatchRecord
		if err := stowBatchClient(t, server).StoreInstancesBatched(context.Background(), instances, StoreBatchLimits{}, stowBatchRecorder(&records)); err != nil {
			t.Fatal(err)
		}
		if got := server.sizes(); !reflect.DeepEqual(got, []int{5, 3, 2}) {
			t.Fatalf("request sizes = %v", got)
		}
		want := []stowBatchRecord{{0, 3, 200, true}, {3, 5, 200, true}}
		if !reflect.DeepEqual(records, want) {
			t.Fatalf("batches = %+v, want %+v", records, want)
		}
	})
	t.Run("a-refused-half-is-not-split-again", func(t *testing.T) {
		server := &stowBatchServer{maxParts: 1}
		var records []stowBatchRecord
		var errs []error
		err := stowBatchClient(t, server).StoreInstancesBatched(context.Background(), stowBatchInstances(4, 10), StoreBatchLimits{}, func(batch StoreBatch) error {
			records = append(records, stowBatchRecord{batch.Start, batch.End, batch.Result.StatusCode, batch.Reduced})
			errs = append(errs, batch.Err)
			return nil
		})
		if err != nil {
			t.Fatal(err)
		}
		if got := server.sizes(); !reflect.DeepEqual(got, []int{4, 2, 2}) {
			t.Fatalf("request sizes = %v", got)
		}
		want := []stowBatchRecord{{0, 2, 413, true}, {2, 4, 413, true}}
		if !reflect.DeepEqual(records, want) {
			t.Fatalf("batches = %+v, want %+v", records, want)
		}
		for _, err := range errs {
			var webErr *Error
			if !errors.As(err, &webErr) || webErr.StatusCode != http.StatusRequestEntityTooLarge {
				t.Fatalf("a refused half reported %v", err)
			}
		}
	})
	t.Run("single-instance", func(t *testing.T) {
		server := &stowBatchServer{maxParts: 1}
		var records []stowBatchRecord
		instances := stowBatchInstances(3, 10)
		if err := stowBatchClient(t, server).StoreInstancesBatched(context.Background(), instances, StoreBatchLimits{MaxInstances: 1}, stowBatchRecorder(&records)); err != nil {
			t.Fatal(err)
		}
		if got := server.sizes(); !reflect.DeepEqual(got, []int{1, 1, 1}) {
			t.Fatalf("request sizes = %v", got)
		}
	})
	t.Run("payload-read-once-is-not-repeated", func(t *testing.T) {
		server := &stowBatchServer{maxParts: 1}
		instances := []StoreInstance{{Reader: bytes.NewReader([]byte("a"))}, {Reader: bytes.NewReader([]byte("b"))}}
		var records []stowBatchRecord
		if err := stowBatchClient(t, server).StoreInstancesBatched(context.Background(), instances, StoreBatchLimits{}, stowBatchRecorder(&records)); err != nil {
			t.Fatal(err)
		}
		if got := server.sizes(); !reflect.DeepEqual(got, []int{2}) || len(records) != 1 || records[0].status != 413 || records[0].reduced {
			t.Fatalf("request sizes = %v, batches = %+v", got, records)
		}
	})
}

// failingReader returns a few bytes and then an error, like a source that
// breaks while it is re-encoded.
type failingReader struct {
	sent bool
	err  error
}

func (r *failingReader) Read(p []byte) (int, error) {
	if !r.sent {
		r.sent = true
		return copy(p, "partial"), nil
	}
	return 0, r.err
}

func (r *failingReader) Close() error { return nil }

// An instance whose payload cannot be produced fails alone; the others of its
// request, and the later requests, are still sent.
func TestStoreInstancesBatchedFailsAnUnreadablePayloadAlone(t *testing.T) {
	openErr := errors.New("open refused")
	readErr := errors.New("re-encoding broke")
	instances := stowBatchInstances(7, 10)
	instances[1].Path = "missing.dcm"
	instances[1].Open = func() (io.ReadCloser, error) { return nil, openErr }
	instances[4].Path = "broken.dcm"
	instances[4].Open = func() (io.ReadCloser, error) { return &failingReader{err: readErr}, nil }
	server := &stowBatchServer{}
	var records []stowBatchRecord
	var errs []error
	err := stowBatchClient(t, server).StoreInstancesBatched(context.Background(), instances, StoreBatchLimits{MaxInstances: 4}, func(batch StoreBatch) error {
		records = append(records, stowBatchRecord{batch.Start, batch.End, batch.Result.StatusCode, batch.Reduced})
		errs = append(errs, batch.Err)
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	// [0 1 2 3] is cut at 1: 0 is sent alone, 1 fails, [2 3] is sent.
	// [4 5 6] is cut at 4: 4 fails, [5 6] is sent.
	want := []stowBatchRecord{{0, 1, 200, false}, {1, 2, 0, false}, {2, 4, 200, false}, {4, 5, 0, false}, {5, 7, 200, false}}
	if !reflect.DeepEqual(records, want) {
		t.Fatalf("batches = %+v, want %+v", records, want)
	}
	if got := server.sizes(); !reflect.DeepEqual(got, []int{1, 2, 2}) {
		t.Fatalf("answered requests = %v", got)
	}
	// Index is the position in the request the payload cut: 1 of [0 1 2 3],
	// 0 of [4 5 6].
	for _, check := range []struct {
		batch, index int
		cause        error
	}{{1, 1, openErr}, {3, 0, readErr}} {
		err := errs[check.batch]
		var payloadErr *StorePayloadError
		if !errors.As(err, &payloadErr) || !errors.Is(err, check.cause) || payloadErr.Index != check.index {
			t.Fatalf("batch %d error = %v (%+v)", check.batch, err, payloadErr)
		}
		wantPath := instances[records[check.batch].start].Path
		if payloadErr.Path != wantPath || !strings.Contains(payloadErr.Error(), wantPath) {
			t.Fatalf("batch %d names %q: %v", check.batch, payloadErr.Path, payloadErr)
		}
	}
	for _, i := range []int{0, 2, 4} {
		if errs[i] != nil {
			t.Fatalf("batch %d error = %v", i, errs[i])
		}
	}

	// Payloads that cannot be read again leave the whole request failed.
	once := []StoreInstance{{Reader: bytes.NewReader([]byte("a"))}, {Path: "missing.dcm", Open: func() (io.ReadCloser, error) { return nil, openErr }}}
	records = nil
	if err := stowBatchClient(t, &stowBatchServer{}).StoreInstancesBatched(context.Background(), once, StoreBatchLimits{}, stowBatchRecorder(&records)); err != nil {
		t.Fatal(err)
	}
	if want := []stowBatchRecord{{0, 2, 0, false}}; !reflect.DeepEqual(records, want) {
		t.Fatalf("batches of a payload read once = %+v", records)
	}
}

func TestStoreInstancesBatchedStopsWhenTheCallbackFails(t *testing.T) {
	server := &stowBatchServer{}
	stop := errors.New("stop")
	calls := 0
	err := stowBatchClient(t, server).StoreInstancesBatched(context.Background(), stowBatchInstances(5, 1), StoreBatchLimits{MaxInstances: 2}, func(StoreBatch) error {
		calls++
		return stop
	})
	if !errors.Is(err, stop) || calls != 1 || len(server.sizes()) != 1 {
		t.Fatalf("err = %v after %d callbacks and %d requests", err, calls, len(server.sizes()))
	}
	canceled, cancel := context.WithCancel(context.Background())
	cancel()
	if err := stowBatchClient(t, server).StoreInstancesBatched(canceled, stowBatchInstances(1, 1), StoreBatchLimits{}, nil); !errors.Is(err, context.Canceled) {
		t.Fatalf("a canceled upload = %v", err)
	}
}
