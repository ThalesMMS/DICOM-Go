package dicomweb

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/ThalesMMS/dicom-go/transfer"
)

var wadoFallbackRef = InstanceRef{
	StudyInstanceUID:  "2.25.71",
	SeriesInstanceUID: "2.25.72",
	SOPInstanceUID:    "2.25.73",
}

const (
	wadoFallbackStudyPath    = "/dicom-web/studies/2.25.71"
	wadoFallbackSeriesPath   = wadoFallbackStudyPath + "/series/2.25.72"
	wadoFallbackInstancePath = wadoFallbackSeriesPath + "/instances/2.25.73"
	wadoFallbackPayload      = "DICOM payload"
)

type wadoAcceptExchange struct {
	accept      string
	status      int
	contentType string
	body        string
}

func wadoAcceptPeer(t *testing.T, path string, script ...wadoAcceptExchange) Client {
	t.Helper()
	var requests atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		index := int(requests.Add(1)) - 1
		if index >= len(script) {
			t.Errorf("unexpected request %d with Accept %q", index+1, r.Header.Get("Accept"))
			w.WriteHeader(http.StatusInternalServerError)
			return
		}
		exchange := script[index]
		if r.Method != http.MethodGet || r.URL.Path != path || r.Header.Get("Accept") != exchange.accept {
			t.Errorf("request %d = %s %s with Accept %q, want GET %s with Accept %q",
				index+1, r.Method, r.URL.Path, r.Header.Get("Accept"), path, exchange.accept)
		}
		if exchange.contentType != "" {
			w.Header().Set("Content-Type", exchange.contentType)
		}
		w.WriteHeader(exchange.status)
		_, _ = io.WriteString(w, exchange.body)
	}))
	t.Cleanup(func() {
		server.Close()
		if got := int(requests.Load()); got != len(script) {
			t.Errorf("received %d HTTP requests, want %d", got, len(script))
		}
	})
	return Client{
		Endpoint: Endpoint{BaseURL: server.URL + "/dicom-web"},
		Options:  Options{HTTPClient: server.Client()},
	}
}

func wadoObjectAcceptValue(uid string) string {
	return `multipart/related; type="application/dicom"; transfer-syntax=` + uid + ", application/dicom; transfer-syntax=" + uid
}

func TestWADOAcceptFallbackTriesRemainingPreferences(t *testing.T) {
	ctx := context.Background()
	j2k := transfer.JPEG2000LosslessOnly.UID
	jls := transfer.JPEGLSLossless.UID
	opts := RetrieveOptions{TransferSyntaxUIDs: []string{" " + j2k + " ", "", jls, j2k, "*"}}
	instanceType := "application/dicom; transfer-syntax=" + transfer.ExplicitVRLittleEndian.UID
	multipartType := `multipart/related; type="application/dicom"; boundary=objects`
	multipartBody := multipartWire("objects", []string{
		"Content-Type: " + instanceType + "\r\nContent-Location: " + wadoFallbackInstancePath + "\r\n\r\n" + wadoFallbackPayload,
	}, true)
	frameType := `multipart/related; type="application/octet-stream"; boundary=frames`
	frameBody := multipartWire("frames", []string{
		"Content-Type: application/octet-stream\r\n\r\nframe two",
		"Content-Type: application/octet-stream\r\n\r\nframe four",
	}, true)
	objectAccepts := []string{wadoObjectAcceptValue(j2k), wadoObjectAcceptValue(jls), wadoObjectAcceptValue("*")}
	frameAccepts := []string{
		`multipart/related; type="image/jp2"; transfer-syntax=` + j2k,
		`multipart/related; type="image/jls"; transfer-syntax=` + jls,
		`multipart/related; type="application/octet-stream"; transfer-syntax=*`,
	}
	for _, test := range []struct {
		name, path, contentType, body string
		accepts                       []string
		want                          []string
	}{
		{"buffered instance", wadoFallbackInstancePath, instanceType, wadoFallbackPayload, objectAccepts, []string{wadoFallbackPayload}},
		{"streamed instance", wadoFallbackInstancePath, instanceType, wadoFallbackPayload, objectAccepts, []string{wadoFallbackPayload}},
		{"streamed study", wadoFallbackStudyPath, multipartType, multipartBody, objectAccepts, []string{wadoFallbackPayload}},
		{"streamed series", wadoFallbackSeriesPath, multipartType, multipartBody, objectAccepts, []string{wadoFallbackPayload}},
		{"streamed frames", wadoFallbackInstancePath + "/frames/2,4", frameType, frameBody, frameAccepts, []string{"2:frame two", "4:frame four"}},
	} {
		t.Run(test.name, func(t *testing.T) {
			// A rejected representation can carry a body exceeding the
			// accepted content budget. It must not enter the part reader.
			rejected := strings.Repeat("rejected ", 128)
			client := wadoAcceptPeer(t, test.path,
				wadoAcceptExchange{strings.Join(test.accepts, ", "), http.StatusNotAcceptable, test.contentType, rejected},
				wadoAcceptExchange{strings.Join(test.accepts[1:], ", "), http.StatusNotAcceptable, test.contentType, rejected},
				wadoAcceptExchange{test.accepts[2], http.StatusOK, test.contentType, test.body},
			)
			client.Options.MaxBodyBytes = 512
			var got []string
			handleObject := func(part ObjectPartStream) error {
				body, err := io.ReadAll(part.Reader)
				if part.TransferSyntaxUID != transfer.ExplicitVRLittleEndian.UID {
					t.Errorf("selected transfer syntax = %q", part.TransferSyntaxUID)
				}
				got = append(got, string(body))
				return err
			}
			handleLocated := func(part LocatedObjectPartStream) error {
				if part.ContentLocation != wadoFallbackInstancePath {
					t.Errorf("Content-Location = %q", part.ContentLocation)
				}
				return handleObject(part.Part)
			}
			var err error
			switch test.name {
			case "buffered instance":
				var parts []ObjectPart
				parts, err = client.RetrieveInstanceWithOptions(ctx, wadoFallbackRef, opts)
				for _, part := range parts {
					if part.TransferSyntaxUID != transfer.ExplicitVRLittleEndian.UID {
						t.Errorf("selected transfer syntax = %q", part.TransferSyntaxUID)
					}
					got = append(got, string(part.Data))
				}
			case "streamed instance":
				err = client.RetrieveInstanceStreamWithOptions(ctx, wadoFallbackRef, opts, handleObject)
			case "streamed study":
				err = client.RetrieveStudyStreamWithOptions(ctx, wadoFallbackRef.StudyInstanceUID, opts, handleLocated)
			case "streamed series":
				err = client.RetrieveSeriesStreamWithOptions(ctx, wadoFallbackRef.StudyInstanceUID, wadoFallbackRef.SeriesInstanceUID, opts, handleLocated)
			case "streamed frames":
				err = client.RetrieveFramesStreamWithOptions(ctx, wadoFallbackRef, []int{2, 4}, opts, func(part FramePartStream) error {
					body, err := io.ReadAll(part.Reader)
					got = append(got, fmt.Sprintf("%d:%s", part.FrameNumber, body))
					return err
				})
			}
			if err != nil || !slices.Equal(got, test.want) {
				t.Fatalf("parts = %q, error = %v, want %q", got, err, test.want)
			}
		})
	}
}

func TestWADOAcceptFallbackTerminalStatus(t *testing.T) {
	uid := transfer.ExplicitVRLittleEndian.UID
	accept := wadoObjectAcceptValue(uid)
	multipleAccept := accept + ", " + wadoObjectAcceptValue("*")
	for _, test := range []struct {
		name   string
		uids   []string
		script []wadoAcceptExchange
		want   int
	}{
		{"exhaustion", []string{uid, "*"}, []wadoAcceptExchange{
			{accept: multipleAccept, status: http.StatusNotAcceptable},
			{accept: wadoObjectAcceptValue("*"), status: http.StatusNotAcceptable},
		}, http.StatusNotAcceptable},
		{"single preference", []string{uid}, []wadoAcceptExchange{
			{accept: accept, status: http.StatusNotAcceptable},
		}, http.StatusNotAcceptable},
		{"no preferences", nil, []wadoAcceptExchange{
			{accept: acceptDICOM, status: http.StatusNotAcceptable},
		}, http.StatusNotAcceptable},
		{"missing object", []string{uid, "*"}, []wadoAcceptExchange{
			{accept: multipleAccept, status: http.StatusNotFound},
		}, http.StatusNotFound},
		// A server may answer 500 for a representation it cannot produce, as
		// Orthanc does, so the remaining representation is requested once.
		{"peer error", []string{uid, "*"}, []wadoAcceptExchange{
			{accept: multipleAccept, status: http.StatusInternalServerError},
			{accept: wadoObjectAcceptValue("*"), status: http.StatusInternalServerError},
		}, http.StatusInternalServerError},
		{"peer unavailable", []string{uid, "*"}, []wadoAcceptExchange{
			{accept: multipleAccept, status: http.StatusServiceUnavailable},
		}, http.StatusServiceUnavailable},
		{"authentication", []string{uid, "*"}, []wadoAcceptExchange{
			{accept: multipleAccept, status: http.StatusForbidden},
		}, http.StatusForbidden},
	} {
		t.Run(test.name, func(t *testing.T) {
			client := wadoAcceptPeer(t, wadoFallbackInstancePath, test.script...)
			callbacks := 0
			err := client.RetrieveInstanceStreamWithOptions(context.Background(), wadoFallbackRef,
				RetrieveOptions{TransferSyntaxUIDs: test.uids}, func(ObjectPartStream) error {
					callbacks++
					return nil
				})
			var webErr *Error
			if !errors.As(err, &webErr) || webErr.StatusCode != test.want || callbacks != 0 {
				t.Fatalf("callbacks = %d, error = %v, want zero callbacks and HTTP %d", callbacks, err, test.want)
			}
		})
	}
}

func TestWADOAcceptFallbackReportsFinalRefusalWithoutReadingItsBody(t *testing.T) {
	uid := transfer.ExplicitVRLittleEndian.UID
	wildcard := wadoObjectAcceptValue("*")
	rejected := strings.Repeat("rejected ", 128)
	client := wadoAcceptPeer(t, wadoFallbackInstancePath,
		wadoAcceptExchange{accept: wadoObjectAcceptValue(uid) + ", " + wildcard, status: http.StatusNotAcceptable, body: rejected},
		wadoAcceptExchange{accept: wildcard, status: http.StatusNotAcceptable, body: rejected},
	)
	client.Options.MaxBodyBytes = 16
	parts, err := client.RetrieveInstanceWithOptions(context.Background(), wadoFallbackRef,
		RetrieveOptions{TransferSyntaxUIDs: []string{uid, "*"}})
	var webErr *Error
	if !errors.As(err, &webErr) || webErr.Kind != ErrorKindHTTPStatus || webErr.StatusCode != http.StatusNotAcceptable || len(parts) != 0 {
		t.Fatalf("parts = %v, err = %v, want final HTTP 406", parts, err)
	}
}

func TestWADOAcceptFallbackNeverReplaysConsumedParts(t *testing.T) {
	uid := transfer.ExplicitVRLittleEndian.UID
	opts := RetrieveOptions{TransferSyntaxUIDs: []string{uid, "*"}}
	accept := wadoObjectAcceptValue(uid) + ", " + wadoObjectAcceptValue("*")
	const multipartType = `multipart/related; type="application/dicom"; boundary=part`
	const first = "--part\r\nContent-Type: application/dicom\r\n\r\nfirst object"
	stop := errors.New("consumer stopped")
	for _, test := range []struct {
		name, body  string
		callbackErr error
	}{
		{"truncated body after first valid part", first + "\r\n--part\r\nContent-T", nil},
		{"callback failed", first + "\r\n--part--\r\n", stop},
	} {
		t.Run(test.name, func(t *testing.T) {
			client := wadoAcceptPeer(t, wadoFallbackStudyPath,
				wadoAcceptExchange{accept, http.StatusOK, multipartType, test.body},
			)
			client.Options.Retry = &RetryPolicy{MaxAttempts: 2, InitialBackoff: time.Millisecond}
			var got []string
			err := client.RetrieveStudyStreamWithOptions(context.Background(), wadoFallbackRef.StudyInstanceUID,
				opts, func(part LocatedObjectPartStream) error {
					body, err := io.ReadAll(part.Part.Reader)
					if err != nil {
						return err
					}
					got = append(got, string(body))
					return test.callbackErr
				})
			if !slices.Equal(got, []string{"first object"}) {
				t.Fatalf("callbacks returned %q, want one complete object", got)
			}
			if test.callbackErr != nil {
				if !errors.Is(err, test.callbackErr) {
					t.Fatalf("error = %v, want %v", err, test.callbackErr)
				}
				return
			}
			assertAdversarialResponseDecodeKind(t, err, ResponseDecodeMalformedMultipart)
		})
	}
}

type wadoRefusalBody struct {
	onClose func()
	reads   int
	closes  int
}

func (b *wadoRefusalBody) Read([]byte) (int, error) {
	b.reads++
	return 0, io.EOF
}

func (b *wadoRefusalBody) Close() error {
	b.closes++
	b.onClose()
	return nil
}

func TestWADOAcceptFallbackStopsOnCancellationAfterClosingRefusal(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	body := &wadoRefusalBody{onClose: cancel}
	requests := 0
	client := Client{
		Endpoint: Endpoint{BaseURL: "http://localhost"},
		Options: Options{HTTPClient: &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
			requests++
			return &http.Response{StatusCode: http.StatusNotAcceptable, Body: body, Request: r}, nil
		})}},
	}
	parts, err := client.RetrieveInstanceWithOptions(ctx, wadoFallbackRef,
		RetrieveOptions{TransferSyntaxUIDs: []string{transfer.ExplicitVRLittleEndian.UID, "*"}})
	if !errors.Is(err, context.Canceled) || len(parts) != 0 || requests != 1 || body.reads != 0 || body.closes != 1 {
		t.Fatalf("parts = %d, error = %v, requests = %d, refusal reads = %d, closes = %d", len(parts), err, requests, body.reads, body.closes)
	}
}

func TestWADOAcceptFallbackRespectsRetryAndBufferedCache(t *testing.T) {
	uid := transfer.JPEG2000LosslessOnly.UID
	wildcard := wadoObjectAcceptValue("*")
	accept := wadoObjectAcceptValue(uid) + ", " + wildcard
	client := wadoAcceptPeer(t, wadoFallbackInstancePath,
		wadoAcceptExchange{accept: accept, status: http.StatusServiceUnavailable},
		wadoAcceptExchange{accept: accept, status: http.StatusNotAcceptable},
		wadoAcceptExchange{wildcard, http.StatusOK, "application/dicom", wadoFallbackPayload},
		wadoAcceptExchange{accept: wadoObjectAcceptValue(uid), status: http.StatusNotAcceptable},
	)
	client.Options.Retry = &RetryPolicy{MaxAttempts: 2, InitialBackoff: time.Millisecond}
	client.Options.Cache = NewResponseCache(16<<10, 0)
	ctx := context.Background()
	for i := 0; i < 2; i++ {
		parts, err := client.RetrieveInstanceWithOptions(ctx, wadoFallbackRef, RetrieveOptions{TransferSyntaxUIDs: []string{uid, "*"}})
		if err != nil || len(parts) != 1 || string(parts[0].Data) != wadoFallbackPayload {
			t.Fatalf("matching Accept preferences (request %d) returned %v, %v", i+1, parts, err)
		}
	}
	parts, err := client.RetrieveInstanceWithOptions(ctx, wadoFallbackRef, RetrieveOptions{TransferSyntaxUIDs: []string{uid}})
	var webErr *Error
	if len(parts) != 0 || !errors.As(err, &webErr) || webErr.StatusCode != http.StatusNotAcceptable {
		t.Fatalf("restricted Accept returned %v, %v, want remote refusal", parts, err)
	}
}
