package fhir

import (
	"context"
	"crypto/x509"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

const (
	testPath = "/r4/tenant-a"
	testAuth = "Bearer synthetic-token"
)

func TestSearchImagingStudiesPOSTAndOpaquePagination(t *testing.T) {
	var calls, grants atomic.Int32
	const scope = "patient/ImagingStudy.rs"
	peer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != testAuth || r.Header.Get("Accept") != "application/fhir+json" {
			t.Errorf("request headers = %v", r.Header)
		}
		switch calls.Add(1) {
		case 1:
			body, _ := io.ReadAll(r.Body)
			if r.Method != http.MethodPost || r.URL.Path != testPath+"/ImagingStudy/_search" || r.URL.RawQuery != "" ||
				r.Header.Get("Content-Type") != "application/x-www-form-urlencoded" ||
				string(body) != "_count=1&patient=Patient%2Fsynthetic" {
				t.Errorf("initial request = %s %s %q", r.Method, r.URL, body)
			}
			writeBundle(t, w, testPath+"/ImagingStudy?opaque=some%2Btoken&sequence=2", []any{
				searchEntry("match", studyResource("first")),
				searchEntry("include", studyResource("ignored")),
				searchEntry("include", map[string]any{"resourceType": "Endpoint", "address": "https://never-fetch.example/secret"}),
				searchEntry("outcome", map[string]any{"resourceType": "OperationOutcome"}),
			})
		case 2:
			if r.Method != http.MethodGet || r.URL.Path != testPath+"/ImagingStudy" ||
				r.URL.RawQuery != "opaque=some%2Btoken&sequence=2" {
				t.Errorf("first continuation = %s %s", r.Method, r.URL)
			}
			writeBundle(t, w, "?_getpages=page3%2Btoken", []any{searchEntry("", studyResource("second"))})
		case 3:
			if r.Method != http.MethodGet || r.URL.Path != testPath+"/ImagingStudy" || r.URL.RawQuery != "_getpages=page3%2Btoken" {
				t.Errorf("second continuation = %s %s", r.Method, r.URL)
			}
			writeBundle(t, w, "", []any{searchEntry("match", studyResource("third"))})
		default:
			t.Error("unexpected request")
			http.Error(w, "unexpected", http.StatusBadRequest)
		}
	}))
	defer peer.Close()
	base := peer.URL + testPath
	client := loopbackClient(t, base+"/", ClientOptions{
		MaxPages: 3,
		Authorize: func(context.Context) (SMARTGrant, error) {
			grants.Add(1)
			return SMARTGrant{Audience: base, AccessToken: "synthetic-token", Scopes: scope, Context: SMARTPatient}, nil
		},
	})
	results, err := client.SearchImagingStudies(context.Background(), ImagingSearch{
		ByPOST: true, Parameters: url.Values{"_count": {"1"}, "patient": {"Patient/synthetic"}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(results) != 3 || results[0].ID != "first" || results[1].ID != "second" || results[2].ID != "third" ||
		results[0].Started != "2026-09-12T10:15:00+05:30" || results[0].Series[0].Started != "2026-09-12T11:15:30-03:00" ||
		calls.Load() != 3 || grants.Load() != 3 {
		t.Fatalf("studies = %+v, requests=%d, grants=%d", results, calls.Load(), grants.Load())
	}
}

func TestSearchImagingStudiesRejectsLinksBeforeDispatch(t *testing.T) {
	var foreignCalls atomic.Int32
	foreign := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		foreignCalls.Add(1)
		http.Error(w, "foreign", http.StatusBadRequest)
	}))
	defer foreign.Close()
	for _, link := range []string{
		foreign.URL + testPath + "/ImagingStudy?page=2",
		strings.TrimPrefix(foreign.URL, "http:") + testPath + "/ImagingStudy?page=2",
		"/r4/tenant-b/ImagingStudy?page=2",
		"/r4/tenant-a-other/ImagingStudy?page=2",
		testPath + "/%2e%2e/tenant-b/ImagingStudy?page=2",
		testPath + "/%252e%252e/tenant-b/ImagingStudy?page=2",
		testPath + "/../tenant-b/ImagingStudy?page=2",
		testPath + "//ImagingStudy?page=2",
		testPath + "/Patient?page=2",
		testPath + "?page=2",
		testPath + "/ImagingStudy/_search?page=2",
		testPath + "/ImagingStudy/secret?page=2",
		testPath + "/ImagingStudy?page=2#canary",
	} {
		t.Run(link, func(t *testing.T) {
			var calls atomic.Int32
			peer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				calls.Add(1)
				writeBundle(t, w, link, []any{searchEntry("match", studyResource("one"))})
			}))
			defer peer.Close()
			base := peer.URL + testPath
			client := loopbackClient(t, base, ClientOptions{Authorize: func(context.Context) (SMARTGrant, error) {
				return SMARTGrant{Audience: base, AccessToken: "synthetic-token", Context: SMARTPatient, Scopes: "patient/ImagingStudy.rs"}, nil
			}})
			got, err := client.SearchImagingStudies(context.Background(), ImagingSearch{})
			if !errors.Is(err, ErrEndpoint) || got != nil || calls.Load() != 1 {
				t.Fatalf("search = %+v, %v, calls=%d", got, err, calls.Load())
			}
		})
	}
	if foreignCalls.Load() != 0 {
		t.Fatalf("foreign requests=%d", foreignCalls.Load())
	}
}

func TestSearchImagingStudiesCapPreventsExtraPage(t *testing.T) {
	var calls, grants atomic.Int32
	peer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		calls.Add(1)
		writeBundle(t, w, "ImagingStudy?page=2", []any{searchEntry("match", studyResource("one"))})
	}))
	defer peer.Close()
	base := peer.URL + testPath
	client := loopbackClient(t, base, ClientOptions{MaxPages: 1, Authorize: func(context.Context) (SMARTGrant, error) {
		grants.Add(1)
		return SMARTGrant{Audience: base, AccessToken: "synthetic-token", Context: SMARTUser, Scopes: "user/ImagingStudy.read"}, nil
	}})
	got, err := client.SearchImagingStudies(context.Background(), ImagingSearch{})
	if !errors.Is(err, ErrPageLimit) || got != nil || calls.Load() != 1 || grants.Load() != 1 {
		t.Fatalf("search = %+v, %v, requests=%d, grants=%d", got, err, calls.Load(), grants.Load())
	}
}

func TestSearchImagingStudiesRejectsOtherTenantGrantAndWriteScope(t *testing.T) {
	var calls atomic.Int32
	peer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		calls.Add(1)
		writeBundle(t, w, "", nil)
	}))
	defer peer.Close()
	for _, tc := range []struct {
		name     string
		audience string
		scopes   string
		token    string
		want     error
	}{
		{"tenant", "/r4/tenant-b", "patient/ImagingStudy.rs", "token", ErrAuth},
		{"different context scope", testPath, "user/ImagingStudy.rs", "token", ErrSMARTScopeDenied},
		{"write only", testPath, "patient/ImagingStudy.write", "token", ErrSMARTScopeDenied},
		{"header injection", testPath, "patient/ImagingStudy.rs", "token\r\nsecret", ErrAuth},
	} {
		t.Run(tc.name, func(t *testing.T) {
			client := loopbackClient(t, peer.URL+testPath, ClientOptions{Authorize: func(context.Context) (SMARTGrant, error) {
				return SMARTGrant{
					Context: SMARTPatient, Audience: peer.URL + tc.audience, Scopes: tc.scopes, AccessToken: tc.token,
				}, nil
			}})
			_, err := client.SearchImagingStudies(context.Background(), ImagingSearch{ByPOST: true})
			if !errors.Is(err, tc.want) || strings.Contains(err.Error(), "secret") {
				t.Fatalf("search error=%v, want=%v", err, tc.want)
			}
		})
	}
	if calls.Load() != 0 {
		t.Fatalf("unexpected authorized requests: %d", calls.Load())
	}
}

func TestSearchImagingStudiesRejectsInvalidBundleAndPartialResults(t *testing.T) {
	for _, body := range []string{
		`{"resourceType":"Bundle","type":"searchset","link":[{"relation":"next","url":"?page=3"},{"relation":"next","url":"?page=4"}]}`,
		`{"resourceType":"Bundle","type":"searchset","link":[],"LINK":[{"relation":"next","url":"?page=3"}]}`,
		`{"resourceType":"Bundle","type":"searchset","link":[],"lin\u212a":[{"relation":"next","url":"?page=3"}]}`,
		`{"resourceType":"Bundle","type":"searchset","entry":[{"resource":{"resourceType":"Patient","id":"canary"}}]}`,
		`{"resourceType":"Bundle","type":"searchset","entry":[{"resource":{"resourceType":"ImagingStudy","started":"2026-09-12T10:15:00"}}]}`,
		`{"resourceType":"Bundle","type":"collection"}`,
		`{"resourceType":"Bundle","type":"searchset"} false`,
	} {
		t.Run(body, func(t *testing.T) {
			var calls atomic.Int32
			peer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				if calls.Add(1) == 1 {
					writeBundle(t, w, "?page=2", []any{searchEntry("match", studyResource("first"))})
					return
				}
				w.Header().Set("Content-Type", "application/fhir+json")
				_, _ = io.WriteString(w, body)
			}))
			defer peer.Close()
			client := loopbackClient(t, peer.URL+testPath, ClientOptions{})
			got, err := client.SearchImagingStudies(context.Background(), ImagingSearch{})
			if got != nil || !errors.Is(err, ErrResponse) || calls.Load() != 2 {
				t.Fatalf("search = %+v, %v, requests=%d", got, err, calls.Load())
			}
		})
	}
}

func TestFHIRClientRequiresASCIIOrigins(t *testing.T) {
	for _, host := range []string{"σ.example.test", "ς.example.test", "[fe80::1%25local]"} {
		t.Run(host, func(t *testing.T) {
			if _, err := NewClient("https://"+host+testPath, ClientOptions{}); !errors.Is(err, ErrEndpoint) {
				t.Fatalf("configured endpoint error = %v, want ErrEndpoint", err)
			}
		})
	}

	client, err := NewClient("https://xn--4xa.example.test"+testPath, ClientOptions{})
	if err != nil {
		t.Fatal(err)
	}
	typedPath := testPath + "/ImagingStudy?page=2"
	if _, err := client.resolveNext(client.base, "https://XN--4XA.EXAMPLE.TEST:443"+typedPath); err != nil {
		t.Fatalf("same A-label continuation refused: %v", err)
	}
	if _, err := client.resolveNext(client.base, "https://xn--3xa.example.test"+typedPath); !errors.Is(err, ErrEndpoint) {
		t.Fatalf("different A-label continuation error = %v, want ErrEndpoint", err)
	}
}

func TestSearchImagingStudiesTransportAndByteBudgets(t *testing.T) {
	t.Run("redirect", func(t *testing.T) {
		var calls atomic.Int32
		peer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			calls.Add(1)
			http.Redirect(w, r, testPath+"/ImagingStudy?page=2", http.StatusTemporaryRedirect)
		}))
		defer peer.Close()
		client := loopbackClient(t, peer.URL+testPath, ClientOptions{})
		got, err := client.SearchImagingStudies(context.Background(), ImagingSearch{})
		var status *HTTPStatusError
		if got != nil || !errors.As(err, &status) || status.StatusCode != http.StatusTemporaryRedirect || calls.Load() != 1 {
			t.Fatalf("search = %+v, %v, requests=%d", got, err, calls.Load())
		}
	})
	t.Run("page-loop", func(t *testing.T) {
		var calls atomic.Int32
		peer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			calls.Add(1)
			writeBundle(t, w, "?page=2", nil)
		}))
		defer peer.Close()
		client := loopbackClient(t, peer.URL+testPath, ClientOptions{})
		_, err := client.SearchImagingStudies(context.Background(), ImagingSearch{Parameters: url.Values{"page": {"2"}}})
		if !errors.Is(err, ErrPageLoop) || calls.Load() != 1 {
			t.Fatalf("search error=%v, requests=%d", err, calls.Load())
		}
	})
	t.Run("total-bytes", func(t *testing.T) {
		var calls atomic.Int32
		body := `{"resourceType":"Bundle","type":"searchset","link":[{"relation":"next","url":"?page=2"}]}`
		peer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			calls.Add(1)
			w.Header().Set("Content-Type", "application/fhir+json")
			// The body crosses the limit on a stream with no length header.
			if flusher, ok := w.(http.Flusher); ok {
				flusher.Flush()
			}
			_, _ = io.WriteString(w, body)
		}))
		defer peer.Close()
		client := loopbackClient(t, peer.URL+testPath, ClientOptions{MaxTotalBytes: int64(len(body))})
		_, err := client.SearchImagingStudies(context.Background(), ImagingSearch{})
		if !errors.Is(err, ErrLimit) || calls.Load() != 1 {
			t.Fatalf("search error=%v, requests=%d", err, calls.Load())
		}
		client = loopbackClient(t, peer.URL+testPath, ClientOptions{MaxPageBytes: int64(len(body) - 1)})
		_, err = client.SearchImagingStudies(context.Background(), ImagingSearch{})
		if !errors.Is(err, ErrLimit) || calls.Load() != 2 {
			t.Fatalf("streamed bytes error=%v, requests=%d", err, calls.Load())
		}
	})
	t.Run("deadline", func(t *testing.T) {
		peer := httptest.NewServer(http.HandlerFunc(func(_ http.ResponseWriter, request *http.Request) {
			<-request.Context().Done()
		}))
		defer peer.Close()
		client := loopbackClient(t, peer.URL+testPath, ClientOptions{Timeout: 75 * time.Millisecond})
		_, err := client.SearchImagingStudies(context.Background(), ImagingSearch{})
		if !errors.Is(err, context.DeadlineExceeded) {
			t.Fatalf("search timeout = %v", err)
		}
	})
}

func TestSearchImagingStudiesTLS(t *testing.T) {
	var calls atomic.Int32
	peer := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		calls.Add(1)
		writeBundle(t, w, "", nil)
	}))
	defer peer.Close()
	withoutTrust, err := NewClient(peer.URL+testPath, ClientOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := withoutTrust.SearchImagingStudies(context.Background(), ImagingSearch{}); !errors.Is(err, ErrTransport) {
		t.Fatalf("untrusted HTTPS error = %v", err)
	}
	pool := x509.NewCertPool()
	pool.AddCert(peer.Certificate())
	withTrust, err := NewClient(peer.URL+testPath, ClientOptions{RootCAs: pool})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := withTrust.SearchImagingStudies(context.Background(), ImagingSearch{}); err != nil || calls.Load() != 1 {
		t.Fatalf("trusted HTTPS error = %v, calls=%d", err, calls.Load())
	}
	for _, endpoint := range []string{"http://localhost/r4", "http://example.test/r4", "http://127.0.0.1/r4?tenant=b"} {
		if _, err := NewClient(endpoint, ClientOptions{AllowLoopbackHTTP: true}); !errors.Is(err, ErrEndpoint) {
			t.Errorf("unsafe endpoint %s error = %v", endpoint, err)
		}
	}
}

func loopbackClient(t *testing.T, endpoint string, options ClientOptions) *Client {
	t.Helper()
	options.AllowLoopbackHTTP = true
	client, err := NewClient(endpoint, options)
	if err != nil {
		t.Fatal(err)
	}
	return client
}

func writeBundle(t *testing.T, w http.ResponseWriter, next string, entries []any) {
	t.Helper()
	w.Header().Set("Content-Type", "application/fhir+json")
	bundle := map[string]any{"resourceType": "Bundle", "type": "searchset"}
	if len(entries) != 0 {
		bundle["entry"] = entries
	}
	if next != "" {
		bundle["link"] = []map[string]string{{"relation": "next", "url": next}}
	}
	if err := json.NewEncoder(w).Encode(bundle); err != nil {
		t.Errorf("write test response: %v", err)
	}
}

func searchEntry(mode string, resource any) any {
	entry := map[string]any{"resource": resource}
	if mode != "" {
		entry["search"] = map[string]string{"mode": mode}
	}
	return entry
}

func studyResource(id string) any {
	return map[string]any{
		"resourceType": "ImagingStudy",
		"id":           id,
		"status":       "available",
		"subject":      map[string]string{"reference": "Patient/synthetic"},
		"started":      "2026-09-12T10:15:00+05:30",
		"series": []map[string]any{{
			"uid":      "1.2.840.1.31",
			"started":  "2026-09-12T11:15:30-03:00",
			"modality": map[string]string{"code": "CT"},
		}},
		"identifier": []Identifier{{System: dicomUIDSystem, Value: oidPrefix + "1.2.840.1.1"}},
	}
}
