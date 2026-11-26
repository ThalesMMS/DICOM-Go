package hl7v3_test

import (
	"bytes"
	"context"
	"crypto/x509"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/ThalesMMS/dicom-go/net/hl7v3"
)

const syntheticCDA = `<ClinicalDocument xmlns="urn:hl7-org:v3"><id root="1.2.3" extension="SYNTHETIC"/></ClinicalDocument>`

func TestSubmitPostsExactCDAAndReturnsXMLReceipt(t *testing.T) {
	payload := append([]byte{0xef, 0xbb, 0xbf}, []byte("<?xml version=\"1.0\"?>\r\n"+syntheticCDA+"\r\n")...)
	receipt := []byte("<?xml version=\"1.0\" encoding=\"UTF-8\"?>\n<receipt status=\"queued\"/>")
	var requests atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests.Add(1)
		if r.Method != http.MethodPost || r.URL.RequestURI() != "/receive/cda?site=synthetic" ||
			r.ProtoMajor != 1 || r.ContentLength != int64(len(payload)) ||
			r.Header.Get("Content-Type") != "application/xml; charset=utf-8" ||
			r.Header.Get("Accept-Encoding") != "identity" {
			t.Errorf("unexpected HTTP request: %s %s, %s, length=%d, headers=%v",
				r.Method, r.URL.RequestURI(), r.Proto, r.ContentLength, r.Header)
		}
		got, err := io.ReadAll(r.Body)
		if err != nil || !bytes.Equal(got, payload) {
			t.Errorf("body = %q, err = %v", got, err)
		}
		w.Header().Set("Content-Type", "application/hl7-v3+xml; charset=UTF-8")
		w.WriteHeader(http.StatusAccepted)
		_, _ = w.Write(receipt)
	}))
	t.Cleanup(server.Close)
	client := newClient(t, server.URL+"/receive/cda?site=synthetic", hl7v3.Options{
		AllowLoopbackHTTP: true,
		MaxRequestBytes:   int64(len(payload)),
		MaxResponseBytes:  int64(len(receipt)),
	})
	result, err := client.Submit(context.Background(), payload)
	if err != nil || result.StatusCode != http.StatusAccepted || !bytes.Equal(result.XML, receipt) || requests.Load() != 1 {
		t.Fatalf("receipt = %+v, err = %v, requests = %d", result, err, requests.Load())
	}
}

func TestSubmitLostResponseAfterFullPostIsUncertain(t *testing.T) {
	for _, tc := range []struct {
		name   string
		status int
	}{
		{name: "lost before headers"},
		{name: "truncated body", status: http.StatusOK},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var received atomic.Int32
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				data, err := io.ReadAll(r.Body)
				if err != nil || string(data) != syntheticCDA {
					t.Errorf("incomplete POST: %v", err)
					return
				}
				received.Add(1)
				if tc.status == 0 {
					conn, _, err := w.(http.Hijacker).Hijack()
					if err != nil {
						t.Error(err)
						return
					}
					_ = conn.Close()
					return
				}
				w.Header().Set("Content-Type", "application/xml")
				w.Header().Set("Content-Length", "64")
				_, _ = w.Write([]byte("<receipt>"))
			}))
			t.Cleanup(server.Close)
			client := newClient(t, server.URL, hl7v3.Options{AllowLoopbackHTTP: true})
			assertFailure(t, client, context.Background(), []byte(syntheticCDA), hl7v3.Uncertain, tc.status, hl7v3.ErrTransport)
			if received.Load() != 1 {
				t.Fatalf("fully received POST count = %d", received.Load())
			}
		})
	}
}

func TestSubmitDeadlineAfterBodyIsUncertain(t *testing.T) {
	var received atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		data, err := io.ReadAll(r.Body)
		if err != nil || string(data) != syntheticCDA {
			t.Errorf("incomplete POST: %v", err)
			return
		}
		received.Add(1)
		<-r.Context().Done()
	}))
	t.Cleanup(server.Close)
	client := newClient(t, server.URL, hl7v3.Options{AllowLoopbackHTTP: true, Timeout: time.Second})
	assertFailure(t, client, context.Background(), []byte(syntheticCDA), hl7v3.Uncertain, 0, context.DeadlineExceeded)
	if received.Load() != 1 {
		t.Fatalf("fully received POST count = %d", received.Load())
	}
}

func TestSubmitNeverRedirectsOrRetriesHTTPRejections(t *testing.T) {
	var redirected atomic.Int32
	destination := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		redirected.Add(1)
		w.WriteHeader(http.StatusNoContent)
	}))
	t.Cleanup(destination.Close)
	for _, tc := range []struct {
		status   int
		location string
		want     error
	}{
		{http.StatusFound, "http://secret.example/%", hl7v3.ErrRedirect},
		{http.StatusSeeOther, destination.URL, hl7v3.ErrRedirect},
		{http.StatusTemporaryRedirect, destination.URL, hl7v3.ErrRedirect},
		{http.StatusPermanentRedirect, "/private-redirect", hl7v3.ErrRedirect},
		{http.StatusServiceUnavailable, "", hl7v3.ErrStatus},
	} {
		t.Run(fmt.Sprint(tc.status), func(t *testing.T) {
			var requests atomic.Int32
			source := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				requests.Add(1)
				_, _ = io.Copy(io.Discard, r.Body)
				if tc.location != "" {
					w.Header().Set("Location", tc.location)
				}
				w.Header().Set("Retry-After", "0")
				w.WriteHeader(tc.status)
				_, _ = io.WriteString(w, "PRIVATE_REMOTE_BODY")
			}))
			t.Cleanup(source.Close)
			client := newClient(t, source.URL+"/private-endpoint?key=SECRET", hl7v3.Options{
				AllowLoopbackHTTP: true,
				MaxResponseBytes:  1,
			})
			err := assertFailure(t, client, context.Background(), []byte(syntheticCDA), hl7v3.Rejected, tc.status, tc.want)
			for _, secret := range []string{source.URL, destination.URL, "private-endpoint", "secret.example",
				"private-redirect", "PRIVATE_REMOTE_BODY", "SECRET", "SYNTHETIC"} {
				if strings.Contains(err.Error(), secret) {
					t.Fatalf("failure includes private data %q: %v", secret, err)
				}
			}
			if requests.Load() != 1 || redirected.Load() != 0 {
				t.Fatalf("initial requests = %d, redirected = %d", requests.Load(), redirected.Load())
			}
		})
	}
}

func TestSubmitVerifiedTLSAndPreSendFailure(t *testing.T) {
	var requests atomic.Int32
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests.Add(1)
		if r.TLS == nil || r.ProtoMajor != 1 {
			t.Error("expected verified HTTPS over HTTP/1.1")
		}
		_, _ = io.Copy(io.Discard, r.Body)
		w.WriteHeader(http.StatusNoContent)
	}))
	t.Cleanup(server.Close)
	pool := x509.NewCertPool()
	untrusted := newClient(t, server.URL, hl7v3.Options{RootCAs: pool})
	pool.AddCert(server.Certificate())
	trusted := newClient(t, server.URL, hl7v3.Options{RootCAs: pool})
	assertFailure(t, untrusted, context.Background(), []byte(syntheticCDA), hl7v3.NotSent, 0, hl7v3.ErrTransport)
	result, err := trusted.Submit(context.Background(), []byte(syntheticCDA))
	if err != nil || result.StatusCode != http.StatusNoContent || len(result.XML) != 0 || requests.Load() != 1 {
		t.Fatalf("receipt = %+v, err = %v, requests = %d", result, err, requests.Load())
	}
	server.Close()
	assertFailure(t, trusted, context.Background(), []byte(syntheticCDA), hl7v3.NotSent, 0, hl7v3.ErrTransport)
}

func TestNewClientRefusesUntrustedOrAmbiguousEndpoint(t *testing.T) {
	for _, tc := range []struct {
		name, endpoint string
		allowLoopback  bool
		want           error
	}{
		{name: "no default plaintext", endpoint: "http://127.0.0.1/submit", want: hl7v3.ErrInsecureEndpoint},
		{name: "hostname plaintext", endpoint: "http://localhost/submit", allowLoopback: true, want: hl7v3.ErrInsecureEndpoint},
		{name: "remote plaintext", endpoint: "http://192.0.2.4/submit", allowLoopback: true, want: hl7v3.ErrInsecureEndpoint},
		{name: "IPv6 zone", endpoint: "http://[::1%25example]/submit", allowLoopback: true, want: hl7v3.ErrInsecureEndpoint},
		{name: "opaque", endpoint: "https:example.test", want: hl7v3.ErrEndpoint},
		{name: "relative", endpoint: "/receive", want: hl7v3.ErrEndpoint},
		{name: "unsupported scheme", endpoint: "ftp://example.test/submit", want: hl7v3.ErrEndpoint},
		{name: "userinfo", endpoint: "https://SECRET:secret@example.test/submit", want: hl7v3.ErrEndpoint},
		{name: "fragment", endpoint: "https://example.test/submit#SECRET", want: hl7v3.ErrEndpoint},
		{name: "empty fragment", endpoint: "https://example.test/submit#", want: hl7v3.ErrEndpoint},
		{name: "empty port", endpoint: "https://example.test:/submit", want: hl7v3.ErrEndpoint},
		{name: "invalid port", endpoint: "https://example.test:65536/submit", want: hl7v3.ErrEndpoint},
	} {
		t.Run(tc.name, func(t *testing.T) {
			client, err := hl7v3.NewClient(tc.endpoint, hl7v3.Options{AllowLoopbackHTTP: tc.allowLoopback})
			if client != nil || !errors.Is(err, tc.want) || strings.Contains(err.Error(), "SECRET") {
				t.Fatalf("NewClient error = %v, want %v", err, tc.want)
			}
		})
	}
	for _, opts := range []hl7v3.Options{{Timeout: -1}, {MaxRequestBytes: -1}, {MaxResponseBytes: 65 << 20}} {
		if client, err := hl7v3.NewClient("https://example.test/receive", opts); client != nil || !errors.Is(err, hl7v3.ErrOptions) {
			t.Fatalf("NewClient invalid limits error = %v", err)
		}
	}
	_ = newClient(t, "http://[::1]:8080/receive", hl7v3.Options{AllowLoopbackHTTP: true})
}

func TestSubmitRefusesMalformedOrOversizedCDABeforeNetwork(t *testing.T) {
	var requests atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests.Add(1)
		w.WriteHeader(http.StatusNoContent)
	}))
	t.Cleanup(server.Close)
	client := newClient(t, server.URL, hl7v3.Options{AllowLoopbackHTTP: true})
	nested := strings.Replace(syntheticCDA, "</ClinicalDocument>", strings.Repeat("<x>", 64)+strings.Repeat("</x>", 64)+"</ClinicalDocument>", 1)
	for _, tc := range []struct {
		document string
		want     error
	}{
		{"", hl7v3.ErrRequest},
		{"<other/>", hl7v3.ErrRequest},
		{`<ClinicalDocument xmlns="urn:hl7-org:v3"/>`, hl7v3.ErrRequest},
		{`<!DOCTYPE ClinicalDocument SYSTEM "https://secret.example/">` + syntheticCDA, hl7v3.ErrRequest},
		{syntheticCDA + "<extra/>", hl7v3.ErrRequest},
		{nested, hl7v3.ErrRequestTooLarge},
	} {
		err := assertFailure(t, client, context.Background(), []byte(tc.document), hl7v3.NotSent, 0, tc.want)
		if strings.Contains(err.Error(), "secret.example") {
			t.Fatalf("invalid CDA leaked content: %v", err)
		}
	}
	small := newClient(t, server.URL, hl7v3.Options{AllowLoopbackHTTP: true, MaxRequestBytes: int64(len(syntheticCDA) - 1)})
	assertFailure(t, small, context.Background(), []byte(syntheticCDA), hl7v3.NotSent, 0, hl7v3.ErrRequestTooLarge)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	assertFailure(t, client, ctx, []byte(syntheticCDA), hl7v3.NotSent, 0, context.Canceled)
	if requests.Load() != 0 {
		t.Fatalf("requests from locally refused input = %d", requests.Load())
	}
}

func TestSubmitRejectsUnsafeOrOversizedResponses(t *testing.T) {
	for _, tc := range []struct {
		name         string
		body         string
		mediaTypes   []string
		contentCodes []string
		limit        int64
		chunked      bool
		want         error
	}{
		{name: "declared bytes", body: "<receipt/>", limit: 9, want: hl7v3.ErrResponseTooLarge},
		{name: "chunked bytes", body: "<receipt/>", limit: 9, chunked: true, want: hl7v3.ErrResponseTooLarge},
		{name: "truncated XML", body: "<receipt>", want: hl7v3.ErrResponse},
		{name: "multiple XML roots", body: "<a/><b/>", want: hl7v3.ErrResponse},
		{name: "no XML root", body: "<!-- done -->", want: hl7v3.ErrResponse},
		{name: "outside text", body: "<a/>trailing", want: hl7v3.ErrResponse},
		{name: "duplicate attribute", body: `<a id="1" id="2"/>`, want: hl7v3.ErrResponse},
		{name: "DTD", body: `<!DOCTYPE a SYSTEM "https://synthetic.example/a"><a/>`, want: hl7v3.ErrResponse},
		{name: "processing instruction", body: `<?xml-stylesheet href="https://synthetic.example"?><a/>`, want: hl7v3.ErrResponse},
		{name: "media type", body: "<a/>", mediaTypes: []string{"text/html"}, want: hl7v3.ErrResponse},
		{name: "duplicate media types", body: "<a/>", mediaTypes: []string{"application/xml", "text/xml"}, want: hl7v3.ErrResponse},
		{name: "charset", body: "<a/>", mediaTypes: []string{"application/xml; charset=utf-16"}, want: hl7v3.ErrResponse},
		{name: "content coding", body: "<a/>", contentCodes: []string{"gzip"}, want: hl7v3.ErrResponse},
		{name: "duplicate content coding", body: "<a/>", contentCodes: []string{"identity", "identity"}, want: hl7v3.ErrResponse},
		{name: "nesting", body: strings.Repeat("<a>", 65) + strings.Repeat("</a>", 65), want: hl7v3.ErrResponseTooLarge},
		{name: "tokens", body: "<a>" + strings.Repeat("<x/>", 50_000) + "</a>", want: hl7v3.ErrResponseTooLarge},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var requests atomic.Int32
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				requests.Add(1)
				_, _ = io.Copy(io.Discard, r.Body)
				if len(tc.mediaTypes) == 0 {
					w.Header().Set("Content-Type", "application/xml")
				}
				for _, mediaType := range tc.mediaTypes {
					w.Header().Add("Content-Type", mediaType)
				}
				for _, coding := range tc.contentCodes {
					w.Header().Add("Content-Encoding", coding)
				}
				if tc.chunked {
					w.(http.Flusher).Flush()
				}
				_, _ = io.WriteString(w, tc.body)
			}))
			t.Cleanup(server.Close)
			client := newClient(t, server.URL, hl7v3.Options{AllowLoopbackHTTP: true, MaxResponseBytes: tc.limit})
			assertFailure(t, client, context.Background(), []byte(syntheticCDA), hl7v3.Uncertain, http.StatusOK, tc.want)
			if requests.Load() != 1 {
				t.Fatalf("received POST count = %d", requests.Load())
			}
		})
	}
}

func newClient(t *testing.T, endpoint string, options hl7v3.Options) *hl7v3.Client {
	t.Helper()
	client, err := hl7v3.NewClient(endpoint, options)
	if err != nil {
		t.Fatal(err)
	}
	return client
}

func assertFailure(t *testing.T, client *hl7v3.Client, ctx context.Context, document []byte, state hl7v3.DeliveryState, status int, cause error) error {
	t.Helper()
	result, err := client.Submit(ctx, document)
	var failure *hl7v3.DeliveryError
	if result.StatusCode != 0 || result.XML != nil || !errors.As(err, &failure) ||
		failure.State != state || failure.StatusCode != status || !errors.Is(err, cause) {
		t.Fatalf("receipt = %+v, err = %v, want %s, HTTP %d, cause %v", result, err, state, status, cause)
	}
	return err
}
