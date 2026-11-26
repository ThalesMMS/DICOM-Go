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
	"testing"
)

const testMultipartDelimiter = "DicomMimeBoundary9"

var multipartDelimiterInstance = InstanceRef{
	StudyInstanceUID:  "2.25.820",
	SeriesInstanceUID: "2.25.821",
	SOPInstanceUID:    "2.25.822",
}

func TestWADOMultipartPreservesBoundaryLookalikes(t *testing.T) {
	for _, style := range []struct {
		name    string
		nl      string
		padding string
		last    string
	}{
		{"CRLF", "\r\n", " \t", "\r\nepilogue"},
		{"LF", "\n", "\t ", "\nepilogue"},
		{"closing_at_EOF", "\r\n", " \t", ""},
	} {
		t.Run(style.name, func(t *testing.T) {
			delimiter := "--" + testMultipartDelimiter
			want := []string{
				strings.Join([]string{
					delimiter + "--text",
					delimiter + " \tmore",
					delimiter + "-- \tmore",
					delimiter + "-",
					delimiter + "\rnot-a-line-end",
					delimiter + "\x00\xfe",
					strings.Repeat("payload", 45),
					"",
				}, style.nl),
				"\x01\x00\xff" + style.nl,
			}
			wire := "ignored MIME preamble" + style.nl
			for _, part := range want {
				wire += delimiter + style.padding + style.nl +
					"Content-Type: application/dicom" + style.nl +
					style.nl + part + style.nl
			}
			wire += delimiter + "--" + style.padding + style.last

			t.Run("buffered", func(t *testing.T) {
				client, body := multipartDelimiterTestClient(wire, 1)
				parts, err := client.RetrieveInstance(context.Background(), multipartDelimiterInstance)
				if err != nil {
					t.Fatal(err)
				}
				var got []string
				for _, part := range parts {
					got = append(got, string(part.Data))
				}
				if !slices.Equal(got, want) {
					t.Fatalf("buffered parts = %q, want %q", got, want)
				}
				if !body.closed {
					t.Fatal("buffered call left the HTTP response body open")
				}
			})

			for _, chunkSize := range []int{1, 2, 7, len(wire)} {
				t.Run(fmt.Sprintf("streamed_%d", chunkSize), func(t *testing.T) {
					client, body := multipartDelimiterTestClient(wire, chunkSize)
					var got []string
					err := client.RetrieveInstanceStreamWithOptions(context.Background(), multipartDelimiterInstance, RetrieveOptions{}, func(part ObjectPartStream) error {
						data, err := io.ReadAll(part.Reader)
						if err == nil {
							got = append(got, string(data))
						}
						return err
					})
					if err != nil || !slices.Equal(got, want) {
						t.Fatalf("streamed parts = %q, error = %v, want %q", got, err, want)
					}
					if !body.closed {
						t.Fatal("streamed call left the HTTP response body open")
					}
				})
			}
		})
	}
}

func TestWADOMultipartLookalikeCannotTerminateTruncatedResponse(t *testing.T) {
	for _, suffix := range []string{"--extra", "-- \textra", " \textra"} {
		t.Run(suffix, func(t *testing.T) {
			delimiter := "--" + testMultipartDelimiter
			payload := "prefix\r\n" + delimiter + suffix + "\x00\xff"
			wire := delimiter + "\r\nContent-Type: application/dicom\r\n\r\n" + payload

			buffered, body := multipartDelimiterTestClient(wire, 1)
			parts, err := buffered.RetrieveInstance(context.Background(), multipartDelimiterInstance)
			assertAdversarialResponseDecodeKind(t, err, ResponseDecodeMalformedMultipart)
			if len(parts) != 0 || !body.closed {
				t.Fatalf("buffered response yielded %d parts, body closed = %t", len(parts), body.closed)
			}

			streamed, body := multipartDelimiterTestClient(wire, 1)
			callbacks := 0
			var got []byte
			err = streamed.RetrieveInstanceStreamWithOptions(context.Background(), multipartDelimiterInstance, RetrieveOptions{}, func(part ObjectPartStream) error {
				callbacks++
				var readErr error
				got, readErr = io.ReadAll(part.Reader)
				assertAdversarialResponseDecodeKind(t, readErr, ResponseDecodeMalformedMultipart)
				return readErr
			})
			assertAdversarialResponseDecodeKind(t, err, ResponseDecodeMalformedMultipart)
			if callbacks != 1 || string(got) != payload || !body.closed {
				t.Fatalf("callbacks = %d, streamed data = %q, body closed = %t; want 1, %q, true", callbacks, got, body.closed, payload)
			}
		})
	}
}

func TestWADOMultipartLookalikesCountTowardPartLimit(t *testing.T) {
	const (
		delimiter = "--" + testMultipartDelimiter
		payload   = "\x99\r\n" + delimiter + "-- \tX\r\nlast\xff"
		wire      = delimiter + "\r\nContent-Type: application/dicom\r\n\r\n" + payload +
			"\r\n" + delimiter + "--\r\n"
	)
	for _, limit := range []int64{int64(len(payload)), int64(len(payload) - 1)} {
		t.Run(fmt.Sprintf("limit_%d", limit), func(t *testing.T) {
			ctx := context.Background()
			buffered, _ := multipartDelimiterTestClient(wire, 1)
			buffered.Options.ResponseLimits.MaxPartBytes = limit
			parts, err := buffered.RetrieveInstance(ctx, multipartDelimiterInstance)
			if limit < int64(len(payload)) {
				assertAdversarialResponseDecodeKind(t, err, ResponseDecodePartBytes)
				if len(parts) != 0 {
					t.Fatalf("buffered call returned %d parts after a byte limit error", len(parts))
				}
			} else if err != nil || len(parts) != 1 || string(parts[0].Data) != payload {
				t.Fatalf("buffered parts = %v, error = %v", parts, err)
			}

			streamed, _ := multipartDelimiterTestClient(wire, 1)
			streamed.Options.ResponseLimits.MaxPartBytes = limit
			var got []byte
			err = streamed.RetrieveInstanceStreamWithOptions(ctx, multipartDelimiterInstance, RetrieveOptions{}, func(part ObjectPartStream) error {
				var readErr error
				got, readErr = io.ReadAll(part.Reader)
				return readErr
			})
			if limit < int64(len(payload)) {
				assertAdversarialResponseDecodeKind(t, err, ResponseDecodePartBytes)
			} else if err != nil {
				t.Fatal(err)
			}
			if string(got) != payload[:limit] {
				t.Fatalf("streamed data = %q, want %q", got, payload[:limit])
			}
		})
	}
}

func TestWADOMultipartLookalikeTruncationAtExactPartLimit(t *testing.T) {
	const (
		delimiter = "--" + testMultipartDelimiter
		payload   = "pixel bytes\r\n" + delimiter + "--looks-final"
		wire      = delimiter + "\r\nContent-Type: application/dicom\r\n\r\n" + payload
	)
	client, body := multipartDelimiterTestClient(wire, 1)
	client.Options.ResponseLimits.MaxPartBytes = int64(len(payload))
	err := client.RetrieveInstanceStreamWithOptions(context.Background(), multipartDelimiterInstance, RetrieveOptions{}, func(part ObjectPartStream) error {
		got, err := io.ReadAll(part.Reader)
		assertAdversarialResponseDecodeKind(t, err, ResponseDecodeMalformedMultipart)
		if string(got) != payload {
			t.Fatalf("streamed data = %q, want %q", got, payload)
		}
		return err
	})
	var clientErr *Error
	if !errors.As(err, &clientErr) || clientErr.Kind != ErrorKindDecodeResponse || !body.closed {
		t.Fatalf("streamed error = %v, body closed = %t; want typed decode error and closed body", err, body.closed)
	}
	assertAdversarialResponseDecodeKind(t, err, ResponseDecodeMalformedMultipart)
}

func TestWADOMultipartStopsBeforeUnreadPayloadAfterCallbackError(t *testing.T) {
	const delimiter = "--" + testMultipartDelimiter
	payload := "first data\r\n" + delimiter + "--not-a-boundary\r\n" + strings.Repeat("x", 16<<10)
	wire := delimiter + "\r\nContent-Type: application/dicom\r\n\r\n" + payload +
		"\r\n" + delimiter + "--\r\n"
	client, body := multipartDelimiterTestClient(wire, 1)
	stop := errors.New("stop reading")
	callbacks := 0
	err := client.RetrieveInstanceStreamWithOptions(context.Background(), multipartDelimiterInstance, RetrieveOptions{}, func(part ObjectPartStream) error {
		callbacks++
		var prefix [5]byte
		if _, err := io.ReadFull(part.Reader, prefix[:]); err != nil || string(prefix[:]) != "first" {
			t.Fatalf("payload prefix = %q, error = %v", prefix, err)
		}
		return stop
	})
	if !errors.Is(err, stop) || callbacks != 1 || !body.closed || body.Len() < len(payload)/2 {
		t.Fatalf("returned %v, callbacks = %d, body closed = %t, remaining bytes = %d", err, callbacks, body.closed, body.Len())
	}
}

func TestWADOMultipartPreservesLookalikesAcrossBufferEdges(t *testing.T) {
	const (
		window    = 4096
		delimiter = "--" + testMultipartDelimiter
		headers   = delimiter + "\r\nContent-Type: application/dicom\r\n\r\n"
	)
	// Start the body on a buffer edge, then put a delimiter token on the
	// next edge without a preceding line break.
	preamble := strings.Repeat("p", window-len(headers)-2) + "\r\n"
	var payload strings.Builder
	payload.WriteString(strings.Repeat("a", window))
	payload.WriteString(delimiter + "--\r\n")
	payload.WriteString(delimiter + strings.Repeat(" \t", window/2+1) + "payload\r\n")
	// The first MIME boundary selected CRLF, so an isolated LF cannot
	// start a boundary even when the following line otherwise matches.
	payload.WriteString("binary\n" + delimiter + "--\r\n")
	// Put the CR of the final separator on the last byte of a buffer.
	payload.WriteString(strings.Repeat("z", window-1-payload.Len()%window))
	want := payload.String()
	wire := preamble + headers + want + "\r\n" + delimiter + "--\r\n"

	t.Run("buffered", func(t *testing.T) {
		client, _ := multipartDelimiterTestClient(wire, 1)
		parts, err := client.RetrieveInstance(context.Background(), multipartDelimiterInstance)
		if err != nil || len(parts) != 1 || string(parts[0].Data) != want {
			t.Fatalf("buffered parts = %d, error = %v, want exactly one %d-byte payload", len(parts), err, len(want))
		}
	})
	t.Run("streamed", func(t *testing.T) {
		client, _ := multipartDelimiterTestClient(wire, 1)
		var parts []string
		err := client.RetrieveInstanceStreamWithOptions(context.Background(), multipartDelimiterInstance, RetrieveOptions{}, func(part ObjectPartStream) error {
			data, err := io.ReadAll(part.Reader)
			parts = append(parts, string(data))
			return err
		})
		if err != nil || len(parts) != 1 || parts[0] != want {
			t.Fatalf("streamed parts = %d, error = %v, want exactly one %d-byte payload", len(parts), err, len(want))
		}
	})
}

func TestWADOMultipartStreamsDisprovedPrefixBeforeMoreWireArrives(t *testing.T) {
	const (
		delimiter = "--" + testMultipartDelimiter
		headers   = delimiter + "\r\nContent-Type: application/dicom\r\n\r\n"
		suffix    = "more body\r\n"
		closing   = "\r\n" + delimiter + "--\r\n"
	)
	for _, test := range []struct {
		name   string
		prefix string
	}{
		{"at_start_of_part", "-X"},
		{"after_body_linebreak", "body\r\n--DicomMimX"},
	} {
		t.Run(test.name, func(t *testing.T) {
			body := &multipartGatedBody{
				prefix: strings.NewReader(headers + test.prefix),
				rest:   strings.NewReader(suffix + closing),
			}
			client := multipartDelimiterClientForBody(body)
			callbacks := 0
			err := client.RetrieveInstanceStreamWithOptions(context.Background(), multipartDelimiterInstance, RetrieveOptions{}, func(part ObjectPartStream) error {
				callbacks++
				first := make([]byte, len(test.prefix))
				if _, err := io.ReadFull(part.Reader, first); err != nil || string(first) != test.prefix {
					t.Fatalf("before the remaining body arrived, read %q with error %v; want %q", first, err, test.prefix)
				}
				body.release = true
				rest, err := io.ReadAll(part.Reader)
				if err != nil || string(rest) != suffix {
					t.Fatalf("remaining body = %q, error = %v; want %q", rest, err, suffix)
				}
				return nil
			})
			if err != nil || callbacks != 1 || !body.closed {
				t.Fatalf("response error = %v, callbacks = %d, body closed = %t", err, callbacks, body.closed)
			}
		})
	}
}

type multipartGatedBody struct {
	prefix, rest *strings.Reader
	release      bool
	closed       bool
}

func (b *multipartGatedBody) Read(p []byte) (int, error) {
	if b.prefix.Len() != 0 {
		return b.prefix.Read(p[:min(len(p), 1)])
	}
	if !b.release {
		return 0, errors.New("reader requested more wire bytes before delivering the disproved prefix")
	}
	return b.rest.Read(p)
}

func (b *multipartGatedBody) Close() error {
	b.closed = true
	return nil
}

func TestStagedBulkDataRequiresRealMultipartClosingDelimiter(t *testing.T) {
	const (
		boundary = "staged-close"
		prefix   = "pixels\x00\r\n--" + boundary + "--"
		suffix   = "still-pixels\xff"
	)
	for name, terminal := range map[string]string{
		"complete":  "\r\n--" + boundary + "--\r\n",
		"truncated": "",
	} {
		t.Run(name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.Header().Set("Content-Type", `multipart/related; type="application/octet-stream"; boundary=`+boundary)
				for _, chunk := range []string{
					"--" + boundary + "\r\nContent-Type: application/octet-stream\r\n\r\n",
					prefix, suffix, terminal,
				} {
					_, _ = io.WriteString(w, chunk)
					w.(http.Flusher).Flush()
				}
			}))
			t.Cleanup(server.Close)
			parent := t.TempDir()
			resolver := StagedBulkDataResolver{
				Client:  Client{Endpoint: Endpoint{BaseURL: server.URL}},
				Staging: FileBulkDataStaging{Directory: parent},
			}
			source, err := resolver.ResolveBulkDataURI(context.Background(), "/bulk", int64(len(prefix+suffix)))
			if terminal == "" {
				if source.Reader != nil {
					_ = source.Reader.Close()
					t.Fatal("truncated body was published")
				}
				assertAdversarialResponseDecodeKind(t, err, ResponseDecodeMalformedMultipart)
			} else {
				if err != nil || source.Reader == nil {
					t.Fatalf("complete body was not published: %v", err)
				}
				defer source.Reader.Close()
				got, err := io.ReadAll(source.Reader)
				if err != nil || string(got) != prefix+suffix || source.Size != int64(len(prefix+suffix)) {
					t.Fatalf("bulk data = %q, size=%d, err=%v", got, source.Size, err)
				}
				if err := source.Reader.Close(); err != nil {
					t.Fatal(err)
				}
			}
			bulkStagingEmpty(t, parent)
		})
	}
}

func multipartDelimiterTestClient(wire string, chunkSize int) (Client, *multipartTestBody) {
	body := &multipartTestBody{Reader: strings.NewReader(wire), chunkSize: chunkSize}
	return multipartDelimiterClientForBody(body), body
}

func multipartDelimiterClientForBody(body io.ReadCloser) Client {
	return Client{
		Endpoint: Endpoint{BaseURL: "https://pacs.example.invalid"},
		Options: Options{
			ResponseLimits: ResponseLimits{MaxPartHeaderBytes: 128},
			HTTPClient: &http.Client{Transport: roundTripFunc(func(*http.Request) (*http.Response, error) {
				return &http.Response{
					StatusCode: http.StatusOK,
					Header: http.Header{"Content-Type": {
						`multipart/related; type="application/dicom"; boundary=` + testMultipartDelimiter,
					}},
					Body: body,
				}, nil
			})},
		},
	}
}
