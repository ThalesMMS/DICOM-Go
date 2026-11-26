package dicomweb

import (
	"bytes"
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
)

type bulkRangeSpy struct {
	data               []byte
	reads, ats, closes atomic.Int32
	offset             atomic.Int64
}

func (s *bulkRangeSpy) Read(p []byte) (int, error) { s.reads.Add(1); return 0, io.ErrNoProgress }
func (s *bulkRangeSpy) ReadAt(p []byte, off int64) (int, error) {
	s.ats.Add(1)
	s.offset.Store(off)
	return bytes.NewReader(s.data).ReadAt(p, off)
}
func (s *bulkRangeSpy) Close() error { s.closes.Add(1); return nil }

func TestBulkRangeRealHTTPFormsAndNoMaterialization(t *testing.T) {
	for _, tc := range []struct {
		rangeHeader, ifRange string
		status               int
		body, contentRange   string
		offset               int64
	}{
		{"bytes=2-5", "", 206, "2345", "bytes 2-5/10", 2},
		{"bytes=7-", "", 206, "789", "bytes 7-9/10", 7},
		{"bytes=-3", "", 206, "789", "bytes 7-9/10", 7},
		{"bytes=3-999999999999999999999999999", "", 206, "3456789", "bytes 3-9/10", 3},
		{"bytes=-999999999999999999999999999", "", 206, "0123456789", "bytes 0-9/10", 0},
		{"bytes=10-", "", 416, "", "bytes */10", 0},
		{"bytes=999999999999999999999999999-", "", 416, "", "bytes */10", 0},
		{"bytes=-0", "", 416, "", "bytes */10", 0},
		{"bytes=5-2", "", 200, "0123456789", "", 0},
		{"bytes=+1-2", "", 200, "0123456789", "", 0},
		{"bytes=0-1,4-5", "", 200, "0123456789", "", 0},
		{"items=1-2", "", 200, "0123456789", "", 0},
		{"bytes=2-5", "\"no-validator\"", 200, "0123456789", "", 0},
	} {
		t.Run(tc.rangeHeader+tc.ifRange, func(t *testing.T) {
			spy := &bulkRangeSpy{data: []byte("0123456789")}
			b := completeServerTestBackend()
			b.bulk = func(context.Context, BulkDataRequest) (BulkDataPart, error) {
				return BulkDataPart{Reader: spy, ReaderAt: spy, Size: 10}, nil
			}
			_, hs := newOpenTestServer(t, b, func(o *ServerOptions) { o.EnableBulkDataRanges = true })
			r, _ := http.NewRequest("GET", hs.URL+"/bulkdata/controlled", nil)
			r.Header.Set("Accept", "application/octet-stream")
			r.Header.Set("Range", tc.rangeHeader)
			if tc.ifRange != "" {
				r.Header.Set("If-Range", tc.ifRange)
			}
			response, err := hs.Client().Do(r)
			if err != nil {
				t.Fatal(err)
			}
			data, err := io.ReadAll(response.Body)
			response.Body.Close()
			if err != nil || response.StatusCode != tc.status || string(data) != tc.body || response.Header.Get("Content-Range") != tc.contentRange || response.ContentLength != int64(len(data)) {
				t.Fatalf("Range status/body/framing=%d/%q/%q err=%v", response.StatusCode, data, response.Header.Get("Content-Range"), err)
			}
			if spy.reads.Load() != 0 || spy.closes.Load() != 1 {
				t.Fatal("range materialized stream or lost ownership")
			}
			if tc.status != 416 && spy.offset.Load() != tc.offset {
				t.Fatal("range was not value-relative")
			}
		})
	}
}
func TestBulkRangeFallbackEmptyMultipartAndLimits(t *testing.T) {
	for _, tc := range []struct {
		name              string
		size              int64
		random, multipart bool
		limit             int64
		status            int
		want              string
	}{
		{"unknown", -1, false, false, 0, 200, "complete"}, {"no random access", 8, false, false, 0, 200, "complete"}, {"multipart", 8, true, true, 0, 200, "complete"}, {"empty", 0, true, false, 0, 200, ""}, {"total cap", 8, true, false, 4, 413, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var closes atomic.Int32
			owner := &bulkOwnedReader{Reader: strings.NewReader(tc.want), closes: &closes}
			b := completeServerTestBackend()
			b.bulk = func(context.Context, BulkDataRequest) (BulkDataPart, error) {
				p := BulkDataPart{Reader: owner, Size: tc.size}
				if tc.random {
					p.ReaderAt = strings.NewReader(tc.want)
				}
				return p, nil
			}
			s, _ := newOpenTestServer(t, b, func(o *ServerOptions) { o.EnableBulkDataRanges = true; o.Limits.MaxPartBytes = tc.limit })
			r := httptest.NewRequest("GET", "/bulkdata/controlled", nil)
			accept := "application/octet-stream"
			if tc.multipart {
				accept = `multipart/related; type="application/octet-stream"`
			}
			r.Header.Set("Accept", accept)
			r.Header.Set("Range", "bytes=1-2")
			w := httptest.NewRecorder()
			s.ServeHTTP(w, r)
			if w.Code != tc.status || closes.Load() != 1 {
				t.Fatal("fallback status/ownership")
			}
			if tc.status == 200 {
				if tc.multipart {
					if !strings.Contains(w.Body.String(), tc.want) || w.Header().Get("Content-Range") != "" {
						t.Fatal("Range applied to multipart")
					}
				} else if w.Body.String() != tc.want {
					t.Fatal("compatible full body changed")
				}
			}
		})
	}
}

type bulkOwnedReader struct {
	io.Reader
	closes *atomic.Int32
}

func (r *bulkOwnedReader) Close() error { r.closes.Add(1); return nil }
func TestBulkRangeShortSourceCancellationAndAuth(t *testing.T) {
	spy := &bulkRangeSpy{data: []byte("short")}
	b := completeServerTestBackend()
	var calls atomic.Int32
	b.bulk = func(context.Context, BulkDataRequest) (BulkDataPart, error) {
		calls.Add(1)
		return BulkDataPart{Reader: spy, ReaderAt: spy, Size: 10}, nil
	}
	_, hs := newOpenTestServer(t, b, func(o *ServerOptions) { o.EnableBulkDataRanges = true })
	r, _ := http.NewRequest("GET", hs.URL+"/bulkdata/controlled", nil)
	r.Header.Set("Accept", "application/octet-stream")
	r.Header.Set("Range", "bytes=0-9")
	response, err := hs.Client().Do(r)
	if err != nil {
		t.Fatal(err)
	}
	_, readErr := io.ReadAll(response.Body)
	response.Body.Close()
	if readErr == nil || spy.closes.Load() != 1 {
		t.Fatal("short random access body was treated as complete")
	}
	s, err := NewServer(ServerOptions{Backend: b, EnableBulkDataRanges: true})
	if err != nil {
		t.Fatal(err)
	}
	q := httptest.NewRequest("GET", "/bulkdata/controlled", nil)
	q.Header.Set("Accept", "application/octet-stream")
	q.Header.Set("Range", "bytes=0-1")
	w := httptest.NewRecorder()
	s.ServeHTTP(w, q)
	if w.Code != 401 || calls.Load() != 1 {
		t.Fatal("Range bypassed auth")
	}
	open, _ := newOpenTestServer(t, b, func(o *ServerOptions) { o.EnableBulkDataRanges = true })
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	q = q.WithContext(ctx)
	w = httptest.NewRecorder()
	open.ServeHTTP(w, q)
	if w.Code != 499 || calls.Load() != 1 {
		t.Fatal("canceled range called backend")
	}
}

func TestBulkRangeRepeatedHeaderIgnoredAndValueReadCancellation(t *testing.T) {
	spy := &bulkRangeSpy{data: []byte("0123456789")}
	b := completeServerTestBackend()
	b.bulk = func(context.Context, BulkDataRequest) (BulkDataPart, error) {
		return BulkDataPart{Reader: spy, ReaderAt: spy, Size: 10}, nil
	}
	s, _ := newOpenTestServer(t, b, func(o *ServerOptions) { o.EnableBulkDataRanges = true })
	r := httptest.NewRequest("GET", "/bulkdata/controlled", nil)
	r.Header.Set("Accept", "application/octet-stream")
	r.Header.Add("Range", "bytes=0-1")
	r.Header.Add("Range", "bytes=4-5")
	w := httptest.NewRecorder()
	s.ServeHTTP(w, r)
	if w.Code != 200 || w.Body.String() != "0123456789" || w.Header().Get("Content-Range") != "" || spy.closes.Load() != 1 {
		t.Fatal("repeated Range fields were applied partially")
	}
	spy = &bulkRangeSpy{data: []byte("0123456789")}
	r = httptest.NewRequest("GET", "/bulkdata/controlled", nil)
	r.Header.Set("Accept", "application/octet-stream")
	r.Header.Set("Range", "bytes=2-5")
	r.Header.Add("If-Range", "")
	r.Header.Add("If-Range", `"etag"`)
	w = httptest.NewRecorder()
	s.ServeHTTP(w, r)
	if w.Code != 200 || w.Body.String() != "0123456789" || w.Header().Get("Content-Range") != "" || spy.closes.Load() != 1 {
		t.Fatal("repeated If-Range bypassed full-response policy")
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	reader := bulkContextReader{ctx: ctx, reader: io.NewSectionReader(bulkCancelAt{cancel: cancel}, 2, 4)}
	_, err := reader.Read(make([]byte, 4))
	if !errors.Is(err, context.Canceled) {
		t.Fatal("random-access cancellation was swallowed")
	}
}

type bulkCancelAt struct{ cancel context.CancelFunc }

func (r bulkCancelAt) ReadAt(p []byte, _ int64) (int, error) { r.cancel(); return copy(p, "data"), nil }
