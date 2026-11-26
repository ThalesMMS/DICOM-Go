package dicomweb

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"syscall"
	"testing"
)

// resetAfterReader returns data and then a connection reset, as a response
// body does when the peer resets the connection.
type resetAfterReader struct {
	data *bytes.Reader
}

var errTestConnectionReset = &net.OpError{Op: "read", Net: "tcp", Err: syscall.ECONNRESET}

func (r *resetAfterReader) Read(p []byte) (int, error) {
	if r.data.Len() == 0 {
		return 0, errTestConnectionReset
	}
	return r.data.Read(p)
}

func (r *resetAfterReader) Close() error { return nil }

type resetRoundTripper struct {
	contentType string
	body        []byte
}

func (rt resetRoundTripper) RoundTrip(request *http.Request) (*http.Response, error) {
	return &http.Response{
		StatusCode: http.StatusOK,
		Header:     http.Header{"Content-Type": {rt.contentType}},
		Body:       &resetAfterReader{data: bytes.NewReader(rt.body)},
		Request:    request,
	}, nil
}

// TestRetrieveStreamReportsAConnectionResetAsARequestFailure cuts a WADO-RS
// response at a part boundary and after a handler that read only part of
// its part. Both are request failures that keep the transport cause, not
// malformed responses.
func TestRetrieveStreamReportsAConnectionResetAsARequestFailure(t *testing.T) {
	const boundary = "cut-boundary"
	contentType := `multipart/related; type="application/dicom"; boundary=` + boundary
	part := "--" + boundary + "\r\nContent-Type: application/dicom\r\n\r\n" + string(bytes.Repeat([]byte("x"), 256))
	tests := []struct {
		name   string
		body   string
		handle func(LocatedObjectPartStream) error
	}{
		{
			name:   "in the headers of the next part",
			body:   part + "\r\n--" + boundary + "\r\nContent-Ty",
			handle: func(p LocatedObjectPartStream) error { _, err := io.Copy(io.Discard, p.Part.Reader); return err },
		},
		{
			name:   "while draining a part",
			body:   part,
			handle: func(LocatedObjectPartStream) error { return nil },
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			client := Client{
				Endpoint: Endpoint{BaseURL: "http://pacs.example.test/dicom-web"},
				Options:  Options{HTTPClient: &http.Client{Transport: resetRoundTripper{contentType: contentType, body: []byte(test.body)}}},
			}
			err := client.RetrieveStudyStreamWithOptions(context.Background(), "1.2.3", RetrieveOptions{}, test.handle)
			var clientErr *Error
			if !errors.As(err, &clientErr) || clientErr.Kind != ErrorKindRequestFailure {
				t.Fatalf("error = %v, want a request failure", err)
			}
			if !errors.Is(err, syscall.ECONNRESET) {
				t.Fatalf("error = %v, want the connection reset in its chain", err)
			}
			var decodeErr *ResponseDecodeError
			if errors.As(err, &decodeErr) {
				t.Fatalf("error = %v, a reset is not a malformed response", err)
			}
		})
	}
}

// TestRetrieveStreamReportsABodyClosedEarlyAsTruncated closes the connection
// after half of a response with a declared Content-Length. The part reader
// and the retrieve report a truncated body, which a multipart body that
// arrives whole but lacks its closing delimiter is not.
func TestRetrieveStreamReportsABodyClosedEarlyAsTruncated(t *testing.T) {
	const boundary = "close-boundary"
	contentType := `multipart/related; type="application/dicom"; boundary=` + boundary
	body := "--" + boundary + "\r\nContent-Type: application/dicom\r\n\r\n" + string(bytes.Repeat([]byte("y"), 4096)) + "\r\n--" + boundary + "--\r\n"
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		connection, buffer, err := w.(http.Hijacker).Hijack()
		if err != nil {
			t.Error(err)
			return
		}
		defer connection.Close()
		fmt.Fprintf(buffer, "HTTP/1.1 200 OK\r\nContent-Type: %s\r\nContent-Length: %d\r\n\r\n%s", contentType, len(body), body[:len(body)/2])
		_ = buffer.Flush()
	}))
	defer server.Close()
	client := Client{Endpoint: Endpoint{BaseURL: server.URL}, Options: Options{HTTPClient: server.Client()}}
	var readErr error
	err := client.RetrieveStudyStreamWithOptions(context.Background(), "1.2.3", RetrieveOptions{}, func(part LocatedObjectPartStream) error {
		_, readErr = io.Copy(io.Discard, part.Part.Reader)
		return readErr
	})
	assertClientResponseDecodeKind(t, err, ResponseDecodeTruncatedBody)
	var decodeErr *ResponseDecodeError
	if !errors.As(readErr, &decodeErr) || decodeErr.Kind != ResponseDecodeTruncatedBody {
		t.Fatalf("part read error = %v, want a truncated body", readErr)
	}
}
