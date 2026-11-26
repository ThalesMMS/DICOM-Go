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
	"slices"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func TestReferenceSTOWPreservesRefusalAndNeverReplaysStream(t *testing.T) {
	const credential = "synthetic-stow-credential"
	const privateBody = "synthetic-patient-payload"
	const privateWarning = `299 fixture "synthetic-private-warning"`
	const diagnostic = "Refused instance for " + privateBody + "\r\nAuthorization: Bearer " + credential
	const failureReport = `{"00081198":{"vr":"SQ","Value":[` +
		`{"00081155":{"vr":"UI","Value":["2.25.7232"]},"00081197":{"vr":"US","Value":[43264]}},` +
		`{"00081155":{"vr":"UI","Value":["2.25.7231"]},"00081197":{"vr":"US","Value":[49601]}}]}}`
	const partialReport = `{"00081199":{"vr":"SQ","Value":[` +
		`{"00081155":{"vr":"UI","Value":["2.25.7231"]},"00081196":{"vr":"US","Value":[45062]}}]},` +
		`"00081198":{"vr":"SQ","Value":[{"00081155":{"vr":"UI","Value":["2.25.7232"]},` +
		`"00081197":{"vr":"US","Value":[43264]}}]}}`
	first := InstanceRef{"2.25.7230", "2.25.7233", "2.25.7231"}
	second := InstanceRef{first.StudyInstanceUID, first.SeriesInstanceUID, "2.25.7232"}
	firstBytes, secondBytes := stowTestFileWithRef(first), stowTestFileWithRef(second)
	failedFirst := StoreItem{SOPInstanceUID: first.SOPInstanceUID, FailureReason: 49601}
	failedSecond := StoreItem{SOPInstanceUID: second.SOPInstanceUID, FailureReason: 43264}
	storedFirst := StoreItem{SOPInstanceUID: first.SOPInstanceUID, WarningReason: 45062}
	tests := []struct {
		name, contentType, body string
		status                  int
		kind                    ErrorKind
		stored, failed          []StoreItem
	}{
		{"partial acceptance", "application/dicom+json", partialReport, 202, "", []StoreItem{storedFirst}, []StoreItem{failedSecond}},
		{"malformed success", "application/dicom+json", `{"unfinished":`, 200, ErrorKindDecodeResponse, nil, nil},
		{"bad request with report", "application/dicom+json", failureReport, 400, ErrorKindHTTPStatus, nil, []StoreItem{failedSecond, failedFirst}},
		{"bad request without report", "text/plain", diagnostic, 400, ErrorKindHTTPStatus, nil, nil},
		{"authentication challenge", "text/plain", diagnostic, 401, ErrorKindAuthStatus, nil, nil},
		{"permission denied", "text/plain", diagnostic, 403, ErrorKindAuthStatus, nil, nil},
		{"body too large", "text/plain", diagnostic, 413, ErrorKindHTTPStatus, nil, nil},
		{"unsupported media", "text/plain", diagnostic, 415, ErrorKindHTTPStatus, nil, nil},
		{"rate limited", "text/plain", diagnostic, 429, ErrorKindHTTPStatus, nil, nil},
		{"bad gateway", "text/plain", diagnostic, 502, ErrorKindHTTPStatus, nil, nil},
		{"service unavailable", "text/plain", diagnostic, 503, ErrorKindHTTPStatus, nil, nil},
		{"gateway timeout", "text/plain", diagnostic, 504, ErrorKindHTTPStatus, nil, nil},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			var requests, opens, closes, refreshes, retries atomic.Int32
			token := mustAccessToken(t, credential, time.Time{})
			tokenSource := NewTokenManager(token, func(context.Context) (AccessToken, error) {
				refreshes.Add(1)
				return token, nil
			})
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				requests.Add(1)
				if r.Method != http.MethodPost || r.URL.Path != "/upload/studies/2.25.7230" || r.Header.Get("Authorization") != "Bearer "+credential {
					t.Errorf("unexpected STOW method, route, or credentials")
				}
				media, params, err := mime.ParseMediaType(r.Header.Get("Content-Type"))
				if err != nil || media != "multipart/related" || params["type"] != "application/dicom" || params["boundary"] == "" {
					t.Errorf("STOW media type = %q, %v", r.Header.Get("Content-Type"), err)
					w.WriteHeader(http.StatusBadRequest)
					return
				}
				multipartBody := multipart.NewReader(r.Body, params["boundary"])
				for index, want := range [][]byte{firstBytes, secondBytes} {
					part, err := multipartBody.NextPart()
					if err != nil {
						t.Errorf("part %d: %v", index, err)
						w.WriteHeader(http.StatusBadRequest)
						return
					}
					got, err := io.ReadAll(part)
					if err != nil || part.Header.Get("Content-Type") != "application/dicom" || !bytes.Equal(got, want) {
						t.Errorf("part %d: media=%q bytes=%d error=%v", index, part.Header.Get("Content-Type"), len(got), err)
					}
				}
				if _, err := multipartBody.NextPart(); !errors.Is(err, io.EOF) {
					t.Errorf("expected terminal boundary, got %v", err)
				}
				w.Header().Set("Content-Type", test.contentType)
				w.Header().Set("Warning", privateWarning)
				w.Header().Set("Retry-After", "0")
				w.WriteHeader(test.status)
				_, _ = io.WriteString(w, test.body)
			}))
			t.Cleanup(server.Close)
			client := Client{Endpoint: Endpoint{BaseURL: server.URL, STOWPath: "upload"}, Options: Options{
				HTTPClient: server.Client(), BearerTokenSource: tokenSource,
				Retry: &RetryPolicy{MaxAttempts: 3, InitialBackoff: time.Millisecond, OnRetry: func(int, int, time.Duration) {
					retries.Add(1)
				}},
			}}
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			result, err := client.StoreInstancesToStudy(ctx, first.StudyInstanceUID, []StoreInstance{
				{SOPInstanceUID: first.SOPInstanceUID, Open: func() (io.ReadCloser, error) {
					opens.Add(1)
					return &trackingReadCloser{Reader: bytes.NewReader(firstBytes), closed: &closes}, nil
				}},
				{SOPInstanceUID: second.SOPInstanceUID, Data: secondBytes},
			})
			if test.kind == "" {
				if err != nil {
					t.Fatal(err)
				}
			} else {
				var webErr *Error
				if !errors.As(err, &webErr) || webErr.Kind != test.kind || webErr.StatusCode != test.status {
					t.Fatalf("error = %#v, want %s with status %d", err, test.kind, test.status)
				}
				for _, format := range []string{"%v", "%+v", "%#v"} {
					for _, private := range []string{credential, privateBody, "synthetic-private-warning", first.StudyInstanceUID, "/upload/studies"} {
						if strings.Contains(fmt.Sprintf(format, err), private) {
							t.Errorf("error format %s exposed remote or request data", format)
						}
					}
				}
			}
			if result.StatusCode != test.status || string(result.Body) != test.body || result.Header.Get("Warning") != privateWarning {
				t.Fatal("explicit result did not retain status, body and warning")
			}
			if !slices.Equal(result.Stored, test.stored) || !slices.Equal(result.Failed, test.failed) {
				t.Fatalf("stored/failed = %#v/%#v, want %#v/%#v", result.Stored, result.Failed, test.stored, test.failed)
			}
			if requests.Load() != 1 || opens.Load() != 1 || closes.Load() != 1 || retries.Load() != 0 || refreshes.Load() != 0 {
				t.Fatalf("requests/opens/closes/retries/refreshes = %d/%d/%d/%d/%d; want 1/1/1/0/0",
					requests.Load(), opens.Load(), closes.Load(), retries.Load(), refreshes.Load())
			}
		})
	}
}
