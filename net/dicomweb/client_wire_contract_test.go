package dicomweb

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"reflect"
	"slices"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func TestWADOStudyMultipartAcrossReadBoundaries(t *testing.T) {
	const (
		boundary = "CaseSensitive7"
		studyUID = "2.25.810"
	)
	first := "\x00\xfe\x80\r\n--" + boundary + "extra\r\n" + "\x15\x00\xff"
	second := "end\r\n"
	locations := []string{
		"/dicom-web/studies/2.25.810/series/2.25.811/instances/2.25.812",
		"/dicom-web/studies/2.25.810/series/2.25.811/instances/2.25.813",
	}
	delimiter := "--" + boundary
	for _, style := range []struct {
		name    string
		newline string
		padding string
	}{
		{name: "CRLF", newline: "\r\n"},
		{name: "transport_padding", newline: "\r\n", padding: " \t"},
		{name: "LF_compatibility", newline: "\n"},
	} {
		t.Run(style.name, func(t *testing.T) {
			// The payload uses the same line-start lookalike under both MIME newline styles.
			expected := []string{strings.ReplaceAll(first, "\r\n", style.newline), second}
			part := func(index int) string {
				return strings.Join([]string{
					delimiter + style.padding,
					"Content-Type: application/dicom",
					"Content-Location: " + locations[index],
					"",
					expected[index],
				}, style.newline) + style.newline
			}
			wire := "preface" + style.newline + part(0) + part(1) +
				delimiter + "--" + style.padding + style.newline + "epilogue"
			for _, chunkSize := range []int{1, 2, 3, 5, 7, 11, 16, 29, 64, len(wire)} {
				t.Run(fmt.Sprintf("read_%d", chunkSize), func(t *testing.T) {
					body := &multipartTestBody{Reader: strings.NewReader(wire), chunkSize: chunkSize}
					client := Client{
						Endpoint: Endpoint{BaseURL: "https://pacs.example.invalid/dicom-web"},
						Options: Options{HTTPClient: &http.Client{
							Transport: roundTripFunc(func(request *http.Request) (*http.Response, error) {
								if request.Method != http.MethodGet || request.URL.Path != "/dicom-web/studies/"+studyUID {
									t.Errorf("unexpected request %s %s", request.Method, request.URL.Path)
								}
								return &http.Response{
									StatusCode: http.StatusOK,
									Header: http.Header{"Content-Type": {
										`Multipart/Related; Type="application/dicom"; Boundary="` + boundary + `"`,
									}},
									Body: body,
								}, nil
							}),
						}},
					}
					var received, receivedLocations []string
					err := client.RetrieveStudyStreamWithOptions(context.Background(), studyUID, RetrieveOptions{}, func(part LocatedObjectPartStream) error {
						data, err := io.ReadAll(part.Part.Reader)
						if err != nil {
							return err
						}
						received = append(received, string(data))
						receivedLocations = append(receivedLocations, part.ContentLocation)
						return nil
					})
					if err != nil || !slices.Equal(received, expected) || !slices.Equal(receivedLocations, locations) {
						t.Fatalf("retrieved bytes %q, locations %q, error %v; want %q, %q", received, receivedLocations, err, expected, locations)
					}
					if !body.closed {
						t.Error("retrieval left the HTTP body open")
					}
				})
			}
		})
	}
}

type multipartTestBody struct {
	*strings.Reader
	chunkSize int
	closed    bool
}

func (r *multipartTestBody) Read(p []byte) (int, error) {
	return r.Reader.Read(p[:min(len(p), r.chunkSize)])
}

func (r *multipartTestBody) Close() error {
	r.closed = true
	return nil
}

func TestQIDOResponsePreservesWireValues(t *testing.T) {
	for _, test := range []struct {
		name string
		tag  string
		wire string
		want Element
	}{
		{"ideographic_only", "00100010", `{"vr":"PN","Value":[{"Ideographic":"柳^明"}]}`,
			Element{VR: "PN", Value: []any{map[string]any{"Ideographic": "柳^明"}}}},
		{"multiple_name_groups", "00100010", `{"vr":"PN","Value":[{"Alphabetic":"Aoki^Haru","Ideographic":"青木^春","Phonetic":"あおき^はる"}]}`,
			Element{VR: "PN", Value: []any{map[string]any{"Alphabetic": "Aoki^Haru", "Ideographic": "青木^春", "Phonetic": "あおき^はる"}}}},
		{"null_between_names", "00080090", `{"vr":"PN","Value":[{"Phonetic":"あき"},null,{"Alphabetic":"Example^Two"}]}`,
			Element{VR: "PN", Value: []any{map[string]any{"Phonetic": "あき"}, nil, map[string]any{"Alphabetic": "Example^Two"}}}},
		{"null_between_codes", "00080061", `{"vr":"CS","Value":["US",null,"NM"]}`,
			Element{VR: "CS", Value: []any{"US", nil, "NM"}}},
		{"decimal_string_and_number", "00280030", `{"vr":"DS","Value":["0.125",0.750]}`,
			Element{VR: "DS", Value: []any{"0.125", json.Number("0.750")}}},
		{"integer_string_and_number", "00200013", `{"vr":"IS","Value":["0007",19]}`,
			Element{VR: "IS", Value: []any{"0007", json.Number("19")}}},
		{"unsigned_integer_beyond_float64_precision", "00720083", `{"vr":"UV","Value":[18446744073709551614]}`,
			Element{VR: "UV", Value: []any{json.Number("18446744073709551614")}}},
		{"present_without_value", "00100020", `{"vr":"LO"}`, Element{VR: "LO"}},
		{"relative_bulk_data", "7FE00010", `{"vr":"OW","BulkDataURI":"../../bulk/edge"}`,
			Element{VR: "OW", BulkDataURI: "../../bulk/edge"}},
		{"foreign_bulk_data", "7FE00010", `{"vr":"OW","BulkDataURI":"https://files.example.invalid/bulk/edge"}`,
			Element{VR: "OW", BulkDataURI: "https://files.example.invalid/bulk/edge"}},
		{"inline_binary", "7FE00010", `{"vr":"OB","InlineBinary":"AP+BfA=="}`,
			Element{VR: "OB", InlineBinary: "AP+BfA=="}},
	} {
		t.Run(test.name, func(t *testing.T) {
			body := `[{"` + test.tag + `":` + test.wire + `}]`
			var requests atomic.Int32
			client := verifyClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				requests.Add(1)
				if r.URL.Path != "/dicom-web/studies" {
					t.Errorf("unexpected metadata side request at %s", r.URL.Path)
					w.WriteHeader(http.StatusNotFound)
					return
				}
				w.Header().Set("Content-Type", "application/dicom+json")
				_, _ = io.WriteString(w, body)
			}), Endpoint{QIDOPath: "dicom-web"}, Options{})

			want := []Dataset{{test.tag: test.want}}
			got, err := client.SearchStudies(context.Background(), nil)
			if err != nil || !reflect.DeepEqual(got, want) {
				t.Fatalf("buffered datasets = %#v, %v; want %#v", got, err, want)
			}
			got = nil
			response, err := client.SearchStudiesStream(context.Background(), nil, func(dataset Dataset) error {
				got = append(got, dataset)
				return nil
			})
			if err != nil || !reflect.DeepEqual(got, want) || len(response.Body) != 0 {
				t.Fatalf("streamed datasets = %#v, response body bytes %d, %v; want %#v", got, len(response.Body), err, want)
			}
			if count := requests.Load(); count != 2 {
				t.Fatalf("requests = %d, want one per search", count)
			}
		})
	}
}

func TestQIDORejectsWireErrorsBeforeYieldingResults(t *testing.T) {
	for _, test := range []struct {
		name        string
		status      int
		contentType string
		body        string
		wantKind    ErrorKind
	}{
		{"truncated_array", http.StatusOK, "application/dicom+json", "[", ErrorKindDecodeResponse},
		{"non_array_object", http.StatusOK, "application/dicom+json", "{}", ErrorKindDecodeResponse},
		{"null_array", http.StatusOK, "application/dicom+json", "null", ErrorKindDecodeResponse},
		{"null_record", http.StatusOK, "application/dicom+json", "[null]", ErrorKindDecodeResponse},
		{"non_object_record", http.StatusOK, "application/dicom+json", "[23]", ErrorKindDecodeResponse},
		{"duplicate_tag", http.StatusOK, "application/dicom+json", `[{"0020000D":{"vr":"UI","Value":["2.25.1"]},"0020000D":{"vr":"UI","Value":["2.25.2"]}}]`, ErrorKindDecodeResponse},
		{"truncated_attribute", http.StatusOK, "application/dicom+json", `[{"00080060":`, ErrorKindDecodeResponse},
		{"HTML_with_valid_JSON_bytes", http.StatusOK, "text/html", "[]", ErrorKindDecodeResponse},
		{"unsupported_charset", http.StatusOK, "application/dicom+json; charset=ISO-8859-1", "[]", ErrorKindDecodeResponse},
		{"conflict_with_empty_results", http.StatusConflict, "application/dicom+json", "[]", ErrorKindHTTPStatus},
		{"busy_with_empty_results", http.StatusServiceUnavailable, "application/dicom+json", "[]", ErrorKindHTTPStatus},
	} {
		t.Run(test.name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.Header().Set("Content-Type", test.contentType)
				w.WriteHeader(test.status)
				_, _ = io.WriteString(w, test.body)
			}))
			t.Cleanup(server.Close)
			client := Client{Endpoint: Endpoint{BaseURL: server.URL}}

			datasets, err := client.SearchStudies(context.Background(), nil)
			assertQIDOWireError(t, err, test.wantKind, test.status)
			if len(datasets) != 0 {
				t.Fatalf("buffered search returned %d datasets on error", len(datasets))
			}
			callbacks := 0
			response, err := client.SearchStudiesStream(context.Background(), nil, func(Dataset) error {
				callbacks++
				return nil
			})
			assertQIDOWireError(t, err, test.wantKind, test.status)
			if callbacks != 0 || response.StatusCode != test.status {
				t.Fatalf("callbacks = %d, status = %d; want 0, %d", callbacks, response.StatusCode, test.status)
			}
		})
	}
}

func TestQIDOAcceptsEmptyDatasetObject(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/dicom+json")
		_, _ = io.WriteString(w, "[{}]")
	}))
	t.Cleanup(server.Close)
	client := Client{Endpoint: Endpoint{BaseURL: server.URL}}
	datasets, err := client.SearchStudies(context.Background(), nil)
	if err != nil || len(datasets) != 1 || datasets[0] == nil || len(datasets[0]) != 0 {
		t.Fatalf("buffered search = %#v, %v; want one empty non-nil dataset", datasets, err)
	}
	datasets = nil
	_, err = client.SearchStudiesStream(context.Background(), nil, func(dataset Dataset) error {
		datasets = append(datasets, dataset)
		return nil
	})
	if err != nil || len(datasets) != 1 || datasets[0] == nil || len(datasets[0]) != 0 {
		t.Fatalf("streaming search = %#v, %v; want one empty non-nil dataset", datasets, err)
	}
}

func TestQIDOInvalidDatasetAfterValidPrefix(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/dicom+json")
		_, _ = io.WriteString(w, `[{"0020000D":{"vr":"UI","Value":["2.25.10"]}},null]`)
	}))
	t.Cleanup(server.Close)
	client := Client{Endpoint: Endpoint{BaseURL: server.URL}}
	datasets, err := client.SearchStudies(context.Background(), nil)
	assertQIDOWireError(t, err, ErrorKindDecodeResponse, http.StatusOK)
	if len(datasets) != 0 {
		t.Fatalf("buffered search published %d datasets before failure", len(datasets))
	}
	var consumed []string
	_, err = client.SearchStudiesStream(context.Background(), nil, func(dataset Dataset) error {
		consumed = append(consumed, dataset["0020000D"].Value[0].(string))
		return nil
	})
	assertQIDOWireError(t, err, ErrorKindDecodeResponse, http.StatusOK)
	if len(consumed) != 1 || consumed[0] != "2.25.10" {
		t.Fatalf("streamed prefix = %v; want exactly the first dataset", consumed)
	}
}

func assertQIDOWireError(t *testing.T, err error, kind ErrorKind, status int) {
	t.Helper()
	var webError *Error
	if !errors.As(err, &webError) || webError.Kind != kind || webError.StatusCode != status {
		t.Fatalf("error = %v; want kind %q, HTTP status %d", err, kind, status)
	}
}

func TestWADOBulkDataPreservesMultipartOrder(t *testing.T) {
	const boundary = "bulk-parts-order"
	payloads := []string{"\x10\x00\xfb", "", "\xcc\x7f"}
	wireParts := make([]string, 0, len(payloads))
	for _, payload := range payloads {
		wireParts = append(wireParts, "Content-Type: application/octet-stream\r\n\r\n"+payload)
	}
	body := multipartWire(boundary, wireParts, true)
	client := responseLimitClient(t, `multipart/related; type="application/octet-stream"; boundary="`+boundary+`"`, body, Options{})

	parts, err := client.RetrieveBulkData(context.Background(), "/bulk/order")
	if err != nil || len(parts) != len(payloads) {
		t.Fatalf("RetrieveBulkData returned %d parts, %v; want %d parts", len(parts), err, len(payloads))
	}
	for index, part := range parts {
		if part.ContentType != "application/octet-stream" || string(part.Data) != payloads[index] {
			t.Errorf("part %d = %q, %x; want application/octet-stream, %x", index, part.ContentType, part.Data, payloads[index])
		}
	}

	source, err := client.ResolveBulkDataURI(context.Background(), "/bulk/order", 32)
	assertClientResponseDecodeKind(t, err, ResponseDecodePartLimit)
	if source.Reader != nil {
		_ = source.Reader.Close()
		t.Fatal("single-value resolution exposed a reader for a multipart value")
	}
}

func TestSTOWReportKeepsInstanceAndNonInstanceOutcomes(t *testing.T) {
	const report = `{"00081199":{"vr":"SQ","Value":[
		{"00081155":{"vr":"UI","Value":["2.25.3201"]}},
		{"00081155":{"vr":"UI","Value":["2.25.3202"]},"00081196":{"vr":"US","Value":[45063]},
		 "00081190":{"vr":"UR","Value":["https://pacs.example.invalid/instances/2.25.3202"]}}
	]},"00081198":{"vr":"SQ","Value":[
		{"00081155":{"vr":"UI","Value":["2.25.3203"]},"00081197":{"vr":"US","Value":[42761]}}
	]},"0008119A":{"vr":"SQ","Value":[
		{"00081197":{"vr":"US","Value":[49161]}}
	]}}`
	wantStored := []StoreItem{
		{SOPInstanceUID: "2.25.3201"},
		{SOPInstanceUID: "2.25.3202", WarningReason: 45063, RetrieveURL: "https://pacs.example.invalid/instances/2.25.3202"},
	}
	wantFailed := []StoreItem{
		{SOPInstanceUID: "2.25.3203", FailureReason: 42761},
		{FailureReason: 49161},
	}
	for _, status := range []int{http.StatusOK, http.StatusAccepted, http.StatusConflict} {
		t.Run(fmt.Sprint(status), func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Method != http.MethodPost || r.URL.Path != "/dicom-web/studies/2.25.3200" {
					t.Errorf("unexpected STOW request %s %s", r.Method, r.URL.Path)
				}
				_, _ = io.Copy(io.Discard, r.Body)
				w.Header().Set("Content-Type", "application/dicom+json")
				w.WriteHeader(status)
				_, _ = io.WriteString(w, report)
			}))
			t.Cleanup(server.Close)
			client := Client{Endpoint: Endpoint{BaseURL: server.URL, STOWPath: "dicom-web"}}
			result, err := client.StoreInstancesToStudy(context.Background(), "2.25.3200", []StoreInstance{{Data: []byte("opaque upload")}})
			if status == http.StatusConflict {
				var httpErr *Error
				if !errors.As(err, &httpErr) || httpErr.Kind != ErrorKindHTTPStatus || httpErr.StatusCode != status {
					t.Fatalf("error = %v, want typed HTTP status %d", err, status)
				}
			} else if err != nil {
				t.Fatal(err)
			}
			if result.Response.StatusCode != status || !bytes.Equal(result.Response.Body, []byte(report)) ||
				!slices.Equal(result.Stored, wantStored) || !slices.Equal(result.Failed, wantFailed) {
				t.Fatalf("StoreInstancesToStudy = %#v; want HTTP %d, stored %#v and failed %#v", result, status, wantStored, wantFailed)
			}
		})
	}
}

func TestRetryAfterHTTPWireValues(t *testing.T) {
	now := time.Date(2025, time.April, 8, 14, 3, 0, 0, time.UTC)
	for _, test := range []struct {
		name  string
		value string
		want  time.Duration
		valid bool
	}{
		{"delta", "37", 37 * time.Second, true},
		{"zero_with_whitespace", " 0\t", 0, true},
		{"preferred_HTTP_date", "Tue, 08 Apr 2025 14:03:16 GMT", 16 * time.Second, true},
		{"obsolete_RFC_850_date", "Tuesday, 08-Apr-25 14:03:16 GMT", 16 * time.Second, true},
		{"obsolete_asctime_date", "Tue Apr  8 14:03:16 2025", 16 * time.Second, true},
		{"past_date", "Tue, 08 Apr 2025 14:02:53 GMT", 0, true},
		{"at_current_time", "Tue, 08 Apr 2025 14:03:00 GMT", 0, true},
		{"negative", "-1", 0, false},
		{"fractional", "0.5", 0, false},
		{"malformed", "later", 0, false},
		{"missing", "", 0, false},
	} {
		t.Run(test.name, func(t *testing.T) {
			got, ok := retryAfter(http.Header{"Retry-After": {test.value}}, now)
			if got != test.want || ok != test.valid {
				t.Errorf("retryAfter(%q) = %s, %v; want %s, %v", test.value, got, ok, test.want, test.valid)
			}
		})
	}
}

func TestWADORetryAfterControlsDelayAndCancellation(t *testing.T) {
	for _, test := range []struct {
		name string
		wait string
		min  time.Duration
		max  time.Duration
	}{
		{name: "delta_seconds", wait: "24", min: 24 * time.Second, max: 24 * time.Second},
		{name: "HTTP_date", min: 50 * time.Second, max: time.Minute},
	} {
		t.Run(test.name, func(t *testing.T) {
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			var requests atomic.Int32
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				requests.Add(1)
				wait := test.wait
				if wait == "" {
					wait = time.Now().UTC().Add(time.Minute).Format(http.TimeFormat)
				}
				w.Header().Set("Retry-After", wait)
				w.WriteHeader(http.StatusTooManyRequests)
			}))
			t.Cleanup(server.Close)

			retries := 0
			client := Client{Endpoint: Endpoint{BaseURL: server.URL}, Options: Options{
				Retry: &RetryPolicy{
					MaxAttempts:    2,
					InitialBackoff: time.Millisecond,
					MaxBackoff:     time.Minute,
					OnRetry: func(attempt int, status int, delay time.Duration) {
						retries++
						cancel()
						if attempt != 1 || status != http.StatusTooManyRequests || delay < test.min || delay > test.max {
							t.Errorf("retry after attempt %d, status %d, delay %s; want attempt 1, HTTP 429, delay [%s, %s]", attempt, status, delay, test.min, test.max)
						}
					},
				},
			}}
			parts, err := client.RetrieveInstance(ctx, responseLimitTestRef)
			if !errors.Is(err, context.Canceled) || len(parts) != 0 || retries != 1 || requests.Load() != 1 {
				t.Fatalf("parts = %d, retries = %d, requests = %d, error = %v; want no parts, one cancelled retry and only one request",
					len(parts), retries, requests.Load(), err)
			}
		})
	}
}
