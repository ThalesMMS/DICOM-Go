// Package hl7v3 submits CDA documents to an explicitly configured XML HTTP
// endpoint and distinguishes requests that may have reached the peer.
package hl7v3

import (
	"bytes"
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptrace"
	"net/netip"
	"net/url"
	"strconv"
	"strings"
	"sync/atomic"
	"time"
	"unicode"

	"github.com/ThalesMMS/dicom-go/encapdoc"
	"github.com/ThalesMMS/dicom-go/internal/sourceport"
)

var (
	ErrEndpoint         = errors.New("hl7v3: invalid CDA endpoint")
	ErrInsecureEndpoint = errors.New("hl7v3: CDA endpoint requires verified HTTPS")
	ErrOptions          = errors.New("hl7v3: invalid CDA transport limits")
	ErrRequest          = errors.New("hl7v3: invalid CDA document")
	ErrRequestTooLarge  = errors.New("hl7v3: CDA document exceeds a size or structure limit")
	ErrTransport        = errors.New("hl7v3: HTTP exchange failed")
	ErrRedirect         = errors.New("hl7v3: HTTP redirection response refused")
	ErrStatus           = errors.New("hl7v3: peer returned an unsuccessful HTTP status")
	ErrResponse         = errors.New("hl7v3: invalid XML response")
	ErrResponseTooLarge = errors.New("hl7v3: response exceeds a size or structure limit")
)

// DeliveryState describes a failed submission at the HTTP boundary.
type DeliveryState string

const (
	// NotSent means local validation failed or the transport never obtained
	// an HTTP connection on which to send the request.
	NotSent DeliveryState = "not-sent"
	// Uncertain means the peer may have received the document. Reconcile
	// the outcome with the peer before sending the document again.
	Uncertain DeliveryState = "uncertain"
	// Rejected means the peer returned a non-2xx HTTP status.
	Rejected DeliveryState = "rejected"
)

// DeliveryError identifies the state and any known HTTP status. Errors from
// Submit omit request and response data, credentials and endpoint strings.
type DeliveryError struct {
	State      DeliveryState
	StatusCode int
	Cause      error
}

func (e *DeliveryError) Error() string {
	if e.StatusCode != 0 {
		return fmt.Sprintf("%v (%s, HTTP %d)", e.Cause, e.State, e.StatusCode)
	}
	return fmt.Sprintf("%v (%s)", e.Cause, e.State)
}

func (e *DeliveryError) Unwrap() error { return e.Cause }

// Options configures one trusted endpoint. All limits have finite defaults.
type Options struct {
	// Timeout bounds validation, network I/O and response parsing. Zero
	// selects 30 seconds. The caller's context may set an earlier deadline.
	Timeout time.Duration
	// MaxRequestBytes and MaxResponseBytes default to 4 MiB each and may
	// be set to at most 64 MiB each.
	MaxRequestBytes  int64
	MaxResponseBytes int64
	// RootCAs replaces the system root pool when non-nil. NewClient clones
	// the pool. HTTPS still verifies the hostname against the endpoint.
	RootCAs *x509.CertPool
	// AllowLoopbackHTTP permits plaintext only at a numeric loopback IP.
	AllowLoopbackHTTP bool
}

// Response is a completed successful HTTP exchange. XML contains owned
// bytes if there was a body. The application interprets any receipt XML.
type Response struct {
	StatusCode int
	XML        []byte
}

// Client submits documents to one endpoint. A client returned by NewClient
// can be used concurrently.
type Client struct {
	endpoint  string
	transport *http.Transport
	options   Options
}

const (
	defaultMaxBytes = 4 << 20
	maxPayloadBytes = 64 << 20
)

// NewClient fixes the endpoint and the trust policy for every submission.
// The endpoint must come from the calling application's trusted configuration.
func NewClient(endpoint string, options Options) (*Client, error) {
	parsed, err := url.Parse(endpoint)
	if err != nil || parsed.Opaque != "" || parsed.Hostname() == "" || parsed.User != nil ||
		strings.Contains(endpoint, "#") || strings.IndexFunc(endpoint, unicode.IsSpace) >= 0 ||
		strings.HasSuffix(parsed.Host, ":") {
		return nil, ErrEndpoint
	}
	if port := parsed.Port(); port != "" {
		value, err := strconv.ParseUint(port, 10, 16)
		if err != nil || value == 0 {
			return nil, ErrEndpoint
		}
	}
	switch parsed.Scheme {
	case "https":
	case "http":
		address, err := netip.ParseAddr(parsed.Hostname())
		if !options.AllowLoopbackHTTP || err != nil || !address.IsLoopback() || address.Zone() != "" {
			return nil, ErrInsecureEndpoint
		}
	default:
		return nil, ErrEndpoint
	}
	if options.Timeout < 0 || options.MaxRequestBytes < 0 || options.MaxRequestBytes > maxPayloadBytes ||
		options.MaxResponseBytes < 0 || options.MaxResponseBytes > maxPayloadBytes {
		return nil, ErrOptions
	}
	if options.Timeout == 0 {
		options.Timeout = 30 * time.Second
	}
	if options.MaxRequestBytes == 0 {
		options.MaxRequestBytes = defaultMaxBytes
	}
	if options.MaxResponseBytes == 0 {
		options.MaxResponseBytes = defaultMaxBytes
	}

	tlsConfig := &tls.Config{MinVersion: tls.VersionTLS12}
	if options.RootCAs != nil {
		tlsConfig.RootCAs = options.RootCAs.Clone()
	}
	transport := &http.Transport{
		DialContext:            sourceport.DialContext(&net.Dialer{}),
		TLSClientConfig:        tlsConfig,
		TLSNextProto:           map[string]func(string, *tls.Conn) http.RoundTripper{},
		DisableKeepAlives:      true,
		DisableCompression:     true,
		MaxResponseHeaderBytes: 64 << 10,
	}
	return &Client{endpoint: parsed.String(), transport: transport, options: options}, nil
}

// Submit inspects a UTF-8 CDA document and POSTs its exact bytes once.
// The caller must keep document unchanged until Submit returns. Submit
// returns a DeliveryError on failure. An unsuccessful or incomplete HTTP
// exchange never triggers a replay.
func (c *Client) Submit(ctx context.Context, document []byte) (Response, error) {
	if c == nil || c.transport == nil {
		return Response{}, &DeliveryError{State: NotSent, Cause: ErrEndpoint}
	}
	if ctx == nil {
		ctx = context.Background()
	}
	ctx, cancel := context.WithTimeout(ctx, c.options.Timeout)
	defer cancel()

	if err := ctx.Err(); err != nil {
		return Response{}, &DeliveryError{State: NotSent, Cause: err}
	}
	if int64(len(document)) > c.options.MaxRequestBytes {
		return Response{}, &DeliveryError{State: NotSent, Cause: ErrRequestTooLarge}
	}
	payload := bytes.Clone(document)
	if _, err := encapdoc.InspectPayload(ctx, "text/xml", withoutXMLBOM(payload), encapdoc.PayloadLimits{}); err != nil {
		cause := ErrRequest
		if errors.Is(err, encapdoc.ErrEncapsulatedDocumentTooLarge) {
			cause = ErrRequestTooLarge
		}
		return Response{}, &DeliveryError{State: NotSent, Cause: contextOr(ctx, cause)}
	}

	var gotConn atomic.Bool
	ctx = httptrace.WithClientTrace(ctx, &httptrace.ClientTrace{
		GotConn: func(httptrace.GotConnInfo) { gotConn.Store(true) },
	})
	// An io.ReadCloser prevents NewRequest from installing GetBody. The
	// transport also disables connection reuse, and RoundTrip is single-hop.
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.endpoint, io.NopCloser(bytes.NewReader(payload)))
	if err != nil {
		return Response{}, &DeliveryError{State: NotSent, Cause: ErrEndpoint}
	}
	req.ContentLength = int64(len(payload))
	req.Header.Set("Content-Type", "application/xml; charset=utf-8")
	req.Header.Set("Accept", "application/xml, text/xml")
	req.Header.Set("Accept-Encoding", "identity")
	resp, err := c.transport.RoundTrip(req)
	if err != nil {
		state := NotSent
		if gotConn.Load() {
			state = Uncertain
		}
		return Response{}, &DeliveryError{State: state, Cause: contextOr(ctx, ErrTransport)}
	}
	defer resp.Body.Close()
	status := resp.StatusCode
	if status < 200 || status >= 300 {
		cause := ErrStatus
		if status >= 300 && status < 400 {
			cause = ErrRedirect
		}
		return Response{}, &DeliveryError{State: Rejected, StatusCode: status, Cause: cause}
	}
	body, err := readXMLResponse(ctx, resp, c.options.MaxResponseBytes)
	if err != nil {
		return Response{}, &DeliveryError{State: Uncertain, StatusCode: status, Cause: err}
	}
	return Response{StatusCode: status, XML: body}, nil
}

func contextOr(ctx context.Context, fallback error) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	return fallback
}
