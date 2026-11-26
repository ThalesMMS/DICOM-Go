package dicomweb

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/ThalesMMS/dicom-go/transfer"
)

var wadoURIClientTestRef = InstanceRef{StudyInstanceUID: "1.2", SeriesInstanceUID: "1.3", SOPInstanceUID: "1.4"}

func TestNormalizeWADOURIPath(t *testing.T) {
	for path, want := range map[string]string{"": "", " /wado/ ": "wado", "nested/wado": "nested/wado"} {
		if got, err := NormalizeWADOURIPath(path); err != nil || got != want {
			t.Errorf("NormalizeWADOURIPath(%q) = %q, %v; want %q", path, got, err, want)
		}
	}
	for _, path := range []string{"/", "//server", "https://server/wado", "wado?query", "wado#fragment",
		"inside/../wado", "./wado", "inside//wado", "inside\\wado", "%2e%2e/wado", "wado\n"} {
		if _, err := NormalizeWADOURIPath(path); err == nil {
			t.Errorf("NormalizeWADOURIPath(%q) accepted", path)
		}
	}
}

func TestWADOURIClientStreamsAnInstanceFromTheURIServer(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	source := transfer.ImplicitVRLittleEndian
	input, pixels := preparedTestFixture(t, source)
	ref := wadoURIClientTestRef
	wantQuery := url.Values{
		"requestType": {"WADO"},
		"studyUID":    {ref.StudyInstanceUID},
		"seriesUID":   {ref.SeriesInstanceUID},
		"objectUID":   {ref.SOPInstanceUID},
		"contentType": {"application/dicom"},
	}
	server, err := NewServer(ServerOptions{
		Backend:           preparedTestBackend(input, source.UID),
		EnableWADOURI:     true,
		ServiceRoot:       "/dicomweb",
		RetrieveTranscode: preparedTestPolicy(t),
		Authorize: func(_ context.Context, r *http.Request, op Operation) error {
			if op != OperationRetrieveWADOURI || r.Method != http.MethodGet ||
				r.Header.Get("Accept") != "application/dicom" || !reflect.DeepEqual(r.URL.Query(), wantQuery) {
				t.Error("client did not request exactly one DICOM representation through the URI Service")
				return ErrForbidden
			}
			return nil
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	peer := httptest.NewServer(http.StripPrefix("/dicomweb", server))
	defer peer.Close()
	client := Client{Endpoint: Endpoint{BaseURL: peer.URL + "/dicomweb", WADOURIPath: "/wado/"}}
	calls := 0
	var data []byte
	err = client.RetrieveWADOURIInstanceStream(ctx, ref, func(part ObjectPartStream) error {
		calls++
		if part.ContentType != "application/dicom" || part.TransferSyntaxUID != "" {
			t.Error("unexpected URI stream part metadata")
		}
		var readErr error
		data, readErr = io.ReadAll(part.Reader)
		return readErr
	})
	if err != nil || calls != 1 {
		t.Fatalf("URI stream error=%v calls=%d, want success and one part", err, calls)
	}
	assertPreparedPixels(t, data, pixels, transfer.ExplicitVRLittleEndian.UID)
}

func TestWADOURIClientRequiresAnExplicitEndpointAndIdentifiers(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	ref := wadoURIClientTestRef
	for _, test := range []struct {
		path string
		ref  InstanceRef
	}{
		{path: "", ref: ref},
		{path: "wado", ref: InstanceRef{SeriesInstanceUID: ref.SeriesInstanceUID, SOPInstanceUID: ref.SOPInstanceUID}},
		{path: "wado", ref: InstanceRef{StudyInstanceUID: ref.StudyInstanceUID, SeriesInstanceUID: "1.03", SOPInstanceUID: ref.SOPInstanceUID}},
		{path: "wado", ref: InstanceRef{StudyInstanceUID: ref.StudyInstanceUID, SeriesInstanceUID: ref.SeriesInstanceUID, SOPInstanceUID: "1.4&transferSyntax=1.5"}},
	} {
		client := Client{Endpoint: Endpoint{BaseURL: "https://configured.test", WADOURIPath: test.path}, Options: Options{
			HTTPClient: &http.Client{Transport: metadataStreamRoundTripper(func(*http.Request) (*http.Response, error) {
				t.Fatal("client sent an invalid URI request")
				return nil, nil
			})},
		}}
		err := client.RetrieveWADOURIInstanceStream(ctx, test.ref, func(ObjectPartStream) error {
			t.Fatal("invalid URI request delivered a response")
			return nil
		})
		var clientErr *Error
		if !errors.As(err, &clientErr) || clientErr.Kind != ErrorKindInvalidEndpoint {
			t.Errorf("invalid URI request error=%v, want ErrorKindInvalidEndpoint", err)
		}
	}
}

func TestWADOURIClientRejectsUnexpectedRepresentations(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	for _, test := range []struct {
		name        string
		contentType string
		want        ResponseDecodeErrorKind
	}{
		{"rendered", "image/jpeg", ResponseDecodeUnexpectedMedia},
		{"multipart", "multipart/related; type=\"application/dicom\"; boundary=uri", ResponseDecodeUnexpectedMedia},
		{"transfer syntax parameter", "application/dicom; transfer-syntax=1.2.840.10008.1.2.1", ResponseDecodeUnexpectedMedia},
	} {
		t.Run(test.name, func(t *testing.T) {
			client := responseLimitClient(t, test.contentType, "body", Options{})
			client.Endpoint.WADOURIPath = "wado"
			err := client.RetrieveWADOURIInstanceStream(ctx, wadoURIClientTestRef, func(ObjectPartStream) error {
				t.Fatal("incompatible representation reached handler")
				return nil
			})
			assertClientResponseDecodeKind(t, err, test.want)
		})
	}
}

func TestWADOURIClientRefusesHTTPPartialContent(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	for _, code := range []int{http.StatusOK, http.StatusPartialContent} {
		t.Run(http.StatusText(code), func(t *testing.T) {
			peer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.Header().Set("Content-Type", "application/dicom")
				w.Header().Set("Content-Range", "bytes 0-3/8")
				w.WriteHeader(code)
				_, _ = io.WriteString(w, "part")
			}))
			defer peer.Close()
			client := Client{Endpoint: Endpoint{BaseURL: peer.URL, WADOURIPath: "wado"}}
			err := client.RetrieveWADOURIInstanceStream(ctx, wadoURIClientTestRef, func(ObjectPartStream) error {
				t.Fatal("partial object reached handler")
				return nil
			})
			var clientErr *Error
			if !errors.As(err, &clientErr) || clientErr.StatusCode != code {
				t.Fatalf("partial object error=%v, want typed DICOMweb error", err)
			}
		})
	}
}

func TestWADOURIClientDetectsTruncatedBodyEvenWhenHandlerDoesNotRead(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	peer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/dicom")
		w.Header().Set("Content-Length", "40")
		_, _ = io.WriteString(w, "truncated")
	}))
	defer peer.Close()
	client := Client{Endpoint: Endpoint{BaseURL: peer.URL, WADOURIPath: "wado"}}
	err := client.RetrieveWADOURIInstanceStream(ctx, wadoURIClientTestRef, func(ObjectPartStream) error { return nil })
	assertClientResponseDecodeKind(t, err, ResponseDecodeTruncatedBody)
}

func TestWADOURIClientEnforcesBodyAndPartByteBoundaries(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	for _, test := range []struct {
		name    string
		options Options
		kind    ErrorKind
	}{
		{name: "body", options: Options{MaxBodyBytes: 4}, kind: ErrorKindRequestFailure},
		{name: "part", options: Options{ResponseLimits: ResponseLimits{MaxPartBytes: 4}}, kind: ErrorKindDecodeResponse},
	} {
		t.Run(test.name, func(t *testing.T) {
			for _, body := range []string{"1234", "12345"} {
				client := responseLimitClient(t, "application/dicom", body, test.options)
				client.Endpoint.WADOURIPath = "wado"
				count := int64(0)
				err := client.RetrieveWADOURIInstanceStream(ctx, wadoURIClientTestRef, func(part ObjectPartStream) error {
					var readErr error
					count, readErr = io.Copy(io.Discard, part.Reader)
					return readErr
				})
				if count != 4 {
					t.Fatalf("%d bytes reached callback, want 4", count)
				}
				if len(body) == 4 {
					if err != nil {
						t.Fatalf("body at the configured ceiling failed: %v", err)
					}
					continue
				}
				var clientErr *Error
				if !errors.As(err, &clientErr) || clientErr.Kind != test.kind {
					t.Fatalf("over-limit body error=%v, want %v", err, test.kind)
				}
			}
		})
	}
}

func TestWADOURIClientRejectsCrossOriginRedirectWithoutCredentials(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	foreign := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		t.Error("client followed a WADO-URI redirect to a different origin")
		w.WriteHeader(http.StatusNoContent)
	}))
	defer foreign.Close()
	peer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, foreign.URL+"?"+r.URL.RawQuery, http.StatusTemporaryRedirect)
	}))
	defer peer.Close()
	client := Client{Endpoint: Endpoint{BaseURL: peer.URL, WADOURIPath: "wado"}}
	err := client.RetrieveWADOURIInstanceStream(ctx, wadoURIClientTestRef, func(ObjectPartStream) error {
		t.Fatal("redirect response reached handler")
		return nil
	})
	if !errors.Is(err, ErrRedirectOrigin) || strings.Contains(err.Error(), "objectUID") {
		t.Fatalf("redirect error=%v, want redacted ErrRedirectOrigin", err)
	}
}
