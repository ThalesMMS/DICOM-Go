package dicomweb

import (
	"bytes"
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/ThalesMMS/dicom-go/dicomjson"
	"github.com/ThalesMMS/dicom-go/internal/sourceport"
	"github.com/ThalesMMS/dicom-go/internal/websocket"
	"github.com/ThalesMMS/dicom-go/object"
)

// ErrEventChannelTransport reports an HTTP client whose transport cannot
// carry a WebSocket (a RoundTripper other than *http.Transport).
var ErrEventChannelTransport = errors.New("dicomweb: event channel requires an *http.Transport")

// WorkitemResponse is the outcome of a UPS-RS transaction.
type WorkitemResponse struct {
	Response Response
	// Warnings holds the text of Warning response headers.
	Warnings []string
	// Location is the workitem URL returned by Create Workitem, or the
	// Location header of any other response. PS3.18 defines none for
	// Subscribe; some origin servers announce their event channel there (see
	// Endpoint.SubscribedEventChannelURLs).
	Location string
}

// WorkitemSearchURL builds the UPS-RS search URL (GET /workitems).
func (e Endpoint) WorkitemSearchURL(params url.Values) (*url.URL, error) {
	return e.serviceURL(e.UPSPath, []string{"workitems"}, params)
}

// WorkitemURL builds /workitems/{uid} with optional extra path segments.
func (e Endpoint) WorkitemURL(sopInstanceUID string, params url.Values, segments ...string) (*url.URL, error) {
	sopInstanceUID = strings.TrimSpace(sopInstanceUID)
	if sopInstanceUID == "" {
		return nil, &Error{Kind: ErrorKindInvalidEndpoint, Err: fmt.Errorf("workitem UID is required")}
	}
	return e.serviceURL(e.UPSPath, append([]string{"workitems", sopInstanceUID}, segments...), params)
}

// EventChannelURL builds the event channel URL (/subscribers/{aetitle}).
func (e Endpoint) EventChannelURL(aeTitle string) (*url.URL, error) {
	if !ValidAETitle(aeTitle) {
		return nil, &Error{Kind: ErrorKindInvalidEndpoint, Err: fmt.Errorf("invalid AE Title")}
	}
	u, err := e.serviceURL(e.UPSPath, []string{"subscribers"}, nil)
	if err != nil {
		return nil, err
	}
	appendPathSegment(u, aeTitle)
	return u, nil
}

// SubscribedEventChannelURLs returns the event channels of aeTitle to try, in
// order, after a successful Subscribe: the standard channel
// EventChannelURL(aeTitle) first, then the channel the response announces in
// its Location header, if any.
//
// PS3.18 defines no Location for Subscribe, and a 201 Location normally names
// the created subscription. Some origin servers nonetheless serve the channel
// outside {service}/subscribers and announce it there: dcm4chee-arc answers
// with ws://{host}/dcm4chee-arc/aets/{AE}/ws/subscribers/{aetitle}. The
// announced URL is therefore only a fallback, and only when it is clearly an
// event channel: a ws or wss URL, or an http(s) URL whose path ends in
// subscribers/{aetitle} outside any workitems resource. It is resolved
// against the request URL, returned with the http or https scheme the
// handshake uses, and dropped when it is malformed, carries credentials or
// leaves the endpoint origin, because the channel receives the configured
// credentials. Only an invalid AE Title or endpoint is an error.
func (e Endpoint) SubscribedEventChannelURLs(aeTitle string, response WorkitemResponse) ([]*url.URL, error) {
	standard, err := e.EventChannelURL(aeTitle)
	if err != nil {
		return nil, err
	}
	channels := []*url.URL{standard}
	if announced := e.announcedEventChannel(aeTitle, standard, response); announced != nil && announced.String() != standard.String() {
		channels = append(channels, announced)
	}
	return channels, nil
}

// announcedEventChannel returns the event channel a Subscribe response
// announces in Location, or nil when Location is not clearly one.
func (e Endpoint) announcedEventChannel(aeTitle string, standard *url.URL, response WorkitemResponse) *url.URL {
	location := strings.TrimSpace(response.Location)
	if location == "" {
		return nil
	}
	reference, err := url.Parse(location)
	if err != nil || reference.User != nil || reference.Opaque != "" {
		return nil
	}
	webSocket := false
	switch strings.ToLower(reference.Scheme) {
	case "ws", "wss":
		webSocket = true
	case "", "http", "https":
	default:
		return nil
	}
	base := standard
	if requestURL, err := url.Parse(response.Response.URL); err == nil && requestURL.IsAbs() && sameOrigin(standard, requestURL) {
		base = requestURL
	}
	target, err := e.eventChannelTarget(base.ResolveReference(reference))
	if err != nil || !webSocket && !eventChannelPath(target, aeTitle) {
		return nil
	}
	return target
}

// eventChannelPath reports whether u ends in subscribers/{aeTitle} outside a
// workitems resource. A subscription resource
// (.../workitems/{uid}/subscribers/{aetitle}) is not an event channel.
func eventChannelPath(u *url.URL, aeTitle string) bool {
	segments := strings.Split(u.EscapedPath(), "/")
	if len(segments) < 2 || segments[len(segments)-2] != "subscribers" {
		return false
	}
	last, err := url.PathUnescape(segments[len(segments)-1])
	if err != nil || strings.TrimSpace(last) != strings.TrimSpace(aeTitle) {
		return false
	}
	for _, segment := range segments[:len(segments)-2] {
		if segment == "workitems" {
			return false
		}
	}
	return true
}

// eventChannelTarget maps a ws or wss URL to the scheme of its handshake
// and requires the endpoint origin.
func (e Endpoint) eventChannelTarget(target *url.URL) (*url.URL, error) {
	if target == nil {
		return nil, &Error{Kind: ErrorKindInvalidEndpoint, Err: fmt.Errorf("event channel URL is required")}
	}
	resolved := *target
	switch strings.ToLower(resolved.Scheme) {
	case "ws", "http":
		resolved.Scheme = "http"
	case "wss", "https":
		resolved.Scheme = "https"
	default:
		return nil, &Error{Kind: ErrorKindInvalidEndpoint, Err: fmt.Errorf("event channel URL must use ws, wss, http or https")}
	}
	resolved.Fragment, resolved.RawFragment = "", ""
	origin, err := e.serviceURL("", nil, nil)
	if err != nil {
		return nil, err
	}
	if resolved.Host == "" || !sameOrigin(origin, &resolved) {
		return nil, &Error{Kind: ErrorKindInvalidEndpoint, Err: fmt.Errorf("event channel URL must use the endpoint origin")}
	}
	return &resolved, nil
}

// appendPathSegment adds one literal segment. An AE Title may contain spaces
// and "/" (PS3.5), so the segment is escaped as a whole: "/" becomes %2F and
// does not split the segment.
func appendPathSegment(u *url.URL, segment string) {
	escaped := strings.TrimSuffix(u.EscapedPath(), "/") + "/" + url.PathEscape(segment)
	u.Path = strings.TrimSuffix(u.Path, "/") + "/" + segment
	u.RawPath = escaped
}

// CreateWorkitem creates a workitem (POST /workitems?workitem={uid}).
// sopInstanceUID may be empty when the server assigns it; the stored
// workitem URL is returned in Location. The dataset must satisfy PS3.4
// CC.2.5.1 (Procedure Step State SCHEDULED and an empty Transaction UID).
func (c Client) CreateWorkitem(ctx context.Context, sopInstanceUID string, workitem *object.Object) (WorkitemResponse, error) {
	params := url.Values{}
	if uid := strings.TrimSpace(sopInstanceUID); uid != "" {
		params.Set("workitem", uid)
	}
	u, err := c.Endpoint.WorkitemSearchURL(params)
	if err != nil {
		return WorkitemResponse{}, err
	}
	return c.workitemTransaction(ctx, http.MethodPost, u, workitem, http.StatusCreated)
}

// RetrieveWorkitem returns the workitem (GET /workitems/{uid}). Servers do
// not return its Transaction UID.
func (c Client) RetrieveWorkitem(ctx context.Context, sopInstanceUID string) (Dataset, error) {
	u, err := c.Endpoint.WorkitemURL(sopInstanceUID, nil)
	if err != nil {
		return nil, err
	}
	resp, err := c.doGETUncached(ctx, u, acceptDICOMJSON)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode != http.StatusOK {
		return nil, statusError(resp)
	}
	datasets, err := datasetsFromDICOMJSON(resp.Body)
	if err != nil || len(datasets) != 1 {
		return nil, newDICOMwebError(ErrorKindDecodeResponse, resp.URL, resp.StatusCode, err)
	}
	return datasets[0], nil
}

// UpdateWorkitem changes workitem attributes (POST /workitems/{uid}). The
// transaction UID is required once the workitem is IN PROGRESS.
func (c Client) UpdateWorkitem(ctx context.Context, sopInstanceUID, transactionUID string, modifications *object.Object) (WorkitemResponse, error) {
	params := url.Values{}
	if transactionUID = strings.TrimSpace(transactionUID); transactionUID != "" {
		params.Set("transaction", transactionUID)
	}
	u, err := c.Endpoint.WorkitemURL(sopInstanceUID, params)
	if err != nil {
		return WorkitemResponse{}, err
	}
	return c.workitemTransaction(ctx, http.MethodPost, u, modifications, http.StatusOK)
}

// ChangeWorkitemState moves a workitem to IN PROGRESS, COMPLETED or CANCELED
// (PUT /workitems/{uid}/state). Claiming a SCHEDULED workitem chooses its
// transaction UID; the later changes must present the same one.
func (c Client) ChangeWorkitemState(ctx context.Context, sopInstanceUID, state, transactionUID string) (WorkitemResponse, error) {
	u, err := c.Endpoint.WorkitemURL(sopInstanceUID, c.requesterParams(), "state")
	if err != nil {
		return WorkitemResponse{}, err
	}
	body := Dataset{
		"00741000": dicomjson.Element{VR: "CS", Value: []any{state}},
		"00081195": dicomjson.Element{VR: "UI", Value: []any{transactionUID}},
	}
	return c.workitemDatasetTransaction(ctx, http.MethodPut, u, body, http.StatusOK)
}

// RequestWorkitemCancellation asks the performer to cancel a workitem
// (POST /workitems/{uid}/cancelrequest). information may be nil or carry
// Reason For Cancellation, Procedure Step Discontinuation Reason Code
// Sequence, Contact URI and Contact Display Name.
func (c Client) RequestWorkitemCancellation(ctx context.Context, sopInstanceUID string, information *object.Object) (WorkitemResponse, error) {
	u, err := c.Endpoint.WorkitemURL(sopInstanceUID, c.requesterParams(), "cancelrequest")
	if err != nil {
		return WorkitemResponse{}, err
	}
	return c.workitemTransaction(ctx, http.MethodPost, u, information, http.StatusAccepted)
}

// requesterParams returns the optional requester query parameter.
func (c Client) requesterParams() url.Values {
	requester := strings.TrimSpace(c.Options.WorkitemRequester)
	if !ValidAETitle(requester) {
		return nil
	}
	return url.Values{"requester": {requester}}
}

// SearchWorkitems runs a UPS-RS search (GET /workitems) with QIDO-style
// parameters: attribute=value matching keys, includefield, limit and offset.
// A comma in a value separates the values of one matching key (PS3.18
// 8.3.4.1) and is sent as a literal comma: an origin server such as
// dcm4chee-arc does not read "%2C" as a separator.
func (c Client) SearchWorkitems(ctx context.Context, params url.Values) ([]Dataset, error) {
	u, err := c.Endpoint.WorkitemSearchURL(params)
	if err != nil {
		return nil, err
	}
	u.RawQuery = strings.ReplaceAll(u.RawQuery, "%2C", ",")
	resp, err := c.doGETUncached(ctx, u, acceptDICOMJSON)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode == http.StatusNoContent {
		return nil, nil
	}
	if resp.StatusCode != http.StatusOK {
		return nil, statusError(resp)
	}
	datasets, err := datasetsFromDICOMJSON(resp.Body)
	if err != nil {
		return nil, newDICOMwebError(ErrorKindDecodeResponse, resp.URL, resp.StatusCode, err)
	}
	return datasets, nil
}

// WorkitemSubscribeOptions configures Subscribe.
type WorkitemSubscribeOptions struct {
	DeletionLock bool
	// Filter holds the matching keys of a filtered global subscription
	// (sopInstanceUID FilteredGlobalWorkitemSubscriptionUID).
	Filter []SearchFilter
}

// SubscribeWorkitem subscribes aeTitle to one workitem, or to all
// (GlobalWorkitemSubscriptionUID) or matching (filtered) workitems
// (POST /workitems/{uid}/subscribers/{aetitle}). Events arrive on the
// event channel: OpenWorkitemEventChannel opens the standard one, and
// OpenSubscribedWorkitemEventChannel also falls back to a channel the
// response announces.
//
// The matching keys travel in one "filter" query parameter holding a
// comma-separated list of attribute=value pairs (PS3.18 11.10). Each pair is
// percent-encoded and the separating commas are literal, as the URI template
// expansion of a list gives. A key or value containing a comma has no
// representation in that list and is refused.
func (c Client) SubscribeWorkitem(ctx context.Context, sopInstanceUID, aeTitle string, options WorkitemSubscribeOptions) (WorkitemResponse, error) {
	params := url.Values{}
	if options.DeletionLock {
		params.Set("deletionlock", "true")
	}
	filter, err := workitemSubscriptionFilter(options.Filter)
	if err != nil {
		return WorkitemResponse{}, err
	}
	u, err := c.subscriberURL(sopInstanceUID, aeTitle, params)
	if err != nil {
		return WorkitemResponse{}, err
	}
	if filter != "" {
		if u.RawQuery != "" {
			u.RawQuery += "&"
		}
		u.RawQuery += "filter=" + filter
	}
	return c.workitemTransaction(ctx, http.MethodPost, u, nil, http.StatusCreated)
}

// workitemSubscriptionFilter encodes the matching keys of a filtered global
// subscription as the value of the PS3.18 11.10 "filter" parameter.
func workitemSubscriptionFilter(filters []SearchFilter) (string, error) {
	var pairs []string
	for _, filter := range filters {
		key := strings.TrimSpace(filter.Key)
		if key == "" || strings.ContainsAny(key, ",=") {
			return "", &Error{Kind: ErrorKindRequestFailure, Err: fmt.Errorf("invalid subscription filter attribute")}
		}
		for _, value := range filter.Values {
			if value == "" || strings.ContainsRune(value, ',') {
				return "", &Error{Kind: ErrorKindRequestFailure, Err: fmt.Errorf("subscription filter value cannot be empty or contain a comma")}
			}
			// "%20", not "+": not every origin server reads "+" as a space.
			pairs = append(pairs, strings.ReplaceAll(url.QueryEscape(key+"="+value), "+", "%20"))
		}
	}
	return strings.Join(pairs, ","), nil
}

// UnsubscribeWorkitem removes a subscription (DELETE
// /workitems/{uid}/subscribers/{aetitle}).
func (c Client) UnsubscribeWorkitem(ctx context.Context, sopInstanceUID, aeTitle string) (WorkitemResponse, error) {
	u, err := c.subscriberURL(sopInstanceUID, aeTitle, nil)
	if err != nil {
		return WorkitemResponse{}, err
	}
	return c.workitemTransaction(ctx, http.MethodDelete, u, nil, http.StatusOK)
}

// SuspendWorkitemSubscription stops future global subscription inheritance
// for aeTitle while keeping existing per-workitem subscriptions
// (POST /workitems/{global}/subscribers/{aetitle}/suspend).
func (c Client) SuspendWorkitemSubscription(ctx context.Context, aeTitle string) (WorkitemResponse, error) {
	u, err := c.subscriberURL(GlobalWorkitemSubscriptionUID, aeTitle, nil)
	if err != nil {
		return WorkitemResponse{}, err
	}
	appendPathSegment(u, "suspend")
	return c.workitemTransaction(ctx, http.MethodPost, u, nil, http.StatusOK)
}

func (c Client) subscriberURL(sopInstanceUID, aeTitle string, params url.Values) (*url.URL, error) {
	if !ValidAETitle(aeTitle) {
		return nil, &Error{Kind: ErrorKindInvalidEndpoint, Err: fmt.Errorf("invalid AE Title")}
	}
	u, err := c.Endpoint.WorkitemURL(sopInstanceUID, params, "subscribers")
	if err != nil {
		return nil, err
	}
	appendPathSegment(u, aeTitle)
	return u, nil
}

func (c Client) workitemTransaction(ctx context.Context, method string, u *url.URL, body *object.Object, expected int) (WorkitemResponse, error) {
	if body == nil {
		return c.workitemRequest(ctx, method, u, nil, expected)
	}
	data, err := dicomjson.MarshalContext(contextOrBackground(ctx), body, dicomjson.DefaultOptions())
	if err != nil {
		return WorkitemResponse{}, newDICOMwebError(ErrorKindRequestFailure, u.String(), 0, err)
	}
	return c.workitemRequest(ctx, method, u, append(append([]byte{'['}, data...), ']'), expected)
}

func (c Client) workitemDatasetTransaction(ctx context.Context, method string, u *url.URL, body Dataset, expected int) (WorkitemResponse, error) {
	data, err := jsonDatasets([]Dataset{body})
	if err != nil {
		return WorkitemResponse{}, newDICOMwebError(ErrorKindRequestFailure, u.String(), 0, err)
	}
	return c.workitemRequest(ctx, method, u, data, expected)
}

func (c Client) workitemRequest(ctx context.Context, method string, u *url.URL, body []byte, expected int) (WorkitemResponse, error) {
	var reader *bytes.Reader
	contentType := ""
	if body != nil {
		reader = bytes.NewReader(body)
		contentType = string(MetadataMediaTypeDICOMJSON)
	}
	var resp Response
	var err error
	if reader != nil {
		resp, err = c.doRequest(ctx, method, u, reader, contentType, acceptDICOMJSON)
	} else {
		resp, err = c.doRequest(ctx, method, u, nil, "", acceptDICOMJSON)
	}
	result := WorkitemResponse{Response: resp}
	if err != nil {
		return result, err
	}
	result.Warnings = warningTexts(resp.Header)
	result.Location = resp.Header.Get("Location")
	if resp.StatusCode != expected && (resp.StatusCode < 200 || resp.StatusCode >= 300) {
		return result, statusError(resp)
	}
	return result, nil
}

// warningTexts extracts the text of "Warning: 299 <agent>: <text>" headers.
func warningTexts(header http.Header) []string {
	var out []string
	for _, value := range header.Values("Warning") {
		value = strings.TrimSpace(value)
		if code, rest, ok := strings.Cut(value, " "); ok {
			if _, err := strconv.Atoi(code); err == nil {
				value = rest
				if agent, text, ok := strings.Cut(value, ": "); ok && !strings.ContainsAny(agent, " \"") {
					value = text
				}
			}
		}
		out = append(out, strings.Trim(value, `"`))
	}
	return out
}

func contextOrBackground(ctx context.Context) context.Context {
	if ctx == nil {
		return context.Background()
	}
	return ctx
}

// WorkitemEventChannel is an open UPS-RS event channel. Next returns events
// in arrival order; Close ends the channel.
type WorkitemEventChannel struct {
	conn      *websocket.Conn
	events    chan WorkitemEvent
	done      chan struct{}
	closeOnce sync.Once
	mu        sync.Mutex
	err       error
	response  Response
}

// OpenWorkitemEventChannel opens the WebSocket event channel of aeTitle
// (GET {service}/subscribers/{aetitle}, RFC 6455). It uses the configured
// credentials (retrying once after a 401 with a dynamic bearer source) and
// the TLS configuration and dialer of Options.HTTPClient's *http.Transport;
// HTTP proxies are not used. Open the channel before subscribing so the
// initial events are not left waiting for delivery retries; for an origin
// server that serves the channel elsewhere, use
// OpenSubscribedWorkitemEventChannel after subscribing.
func (c Client) OpenWorkitemEventChannel(ctx context.Context, aeTitle string) (*WorkitemEventChannel, error) {
	u, err := c.Endpoint.EventChannelURL(aeTitle)
	if err != nil {
		return nil, err
	}
	return c.OpenWorkitemEventChannelURL(ctx, u)
}

// OpenSubscribedWorkitemEventChannel opens the event channel of aeTitle after
// the successful Subscribe that returned response: it tries the channels of
// Endpoint.SubscribedEventChannelURLs in order, the standard one first, and
// returns the first that completes the handshake. When every channel fails it
// returns the error of the last one tried, which is the announced channel
// when the response announced one.
func (c Client) OpenSubscribedWorkitemEventChannel(ctx context.Context, aeTitle string, response WorkitemResponse) (*WorkitemEventChannel, error) {
	ctx = contextOrBackground(ctx)
	channels, err := c.Endpoint.SubscribedEventChannelURLs(aeTitle, response)
	if err != nil {
		return nil, err
	}
	for _, target := range channels {
		var channel *WorkitemEventChannel
		channel, err = c.OpenWorkitemEventChannelURL(ctx, target)
		if err == nil || ctx.Err() != nil {
			return channel, err
		}
	}
	return nil, err
}

// OpenWorkitemEventChannelURL opens the WebSocket event channel at channel,
// for example one of the URLs SubscribedEventChannelURLs returns, like
// OpenWorkitemEventChannel. channel may use the ws, wss, http or https
// scheme and must use the endpoint origin.
func (c Client) OpenWorkitemEventChannelURL(ctx context.Context, channel *url.URL) (*WorkitemEventChannel, error) {
	ctx = contextOrBackground(ctx)
	u, err := c.Endpoint.eventChannelTarget(channel)
	if err != nil {
		return nil, err
	}
	httpClient, err := c.httpClientForRequest(u, false)
	if err != nil {
		return nil, &Error{Kind: ErrorKindInvalidEndpoint, Err: err}
	}
	transport := http.DefaultTransport.(*http.Transport)
	if httpClient.Transport != nil {
		configured, ok := httpClient.Transport.(*http.Transport)
		if !ok {
			return nil, &Error{Kind: ErrorKindInvalidEndpoint, Err: ErrEventChannelTransport}
		}
		transport = configured
	}
	timeout := c.Options.Timeout
	if timeout <= 0 {
		timeout = DefaultTimeout
	}
	for attempt := 0; attempt < 2; attempt++ {
		handshakeCtx, cancel := context.WithTimeout(ctx, timeout)
		channel, retry, err := c.openEventChannel(handshakeCtx, transport, u)
		cancel()
		if err == nil {
			return channel, nil
		}
		if !retry {
			return nil, err
		}
	}
	return nil, newDICOMwebError(ErrorKindAuthStatus, u.String(), http.StatusUnauthorized, nil)
}

func (c Client) openEventChannel(ctx context.Context, transport *http.Transport, u *url.URL) (*WorkitemEventChannel, bool, error) {
	conn, err := dialEventChannel(ctx, transport, u)
	if err != nil {
		return nil, false, requestError(u.String(), err)
	}
	request, token, sourceAuth, err := c.newHTTPRequest(ctx, http.MethodGet, u, nil, "", "")
	if err != nil {
		_ = conn.Close()
		return nil, false, err
	}
	request.Header.Del("Accept")
	if deadline, ok := ctx.Deadline(); ok {
		_ = conn.SetDeadline(deadline)
	}
	maxMessage := c.Options.MaxBodyBytes
	if maxMessage <= 0 || maxMessage > 64<<20 {
		maxMessage = 64 << 20
	}
	ws, httpResponse, err := websocket.ClientHandshake(conn, request, websocket.Options{MaxMessageBytes: maxMessage})
	if err != nil {
		_ = conn.Close()
		if httpResponse == nil {
			return nil, false, requestError(u.String(), err)
		}
		resp := responseFromHTTP(u.String(), httpResponse)
		if httpResponse.StatusCode == http.StatusUnauthorized && sourceAuth {
			if invalidator, ok := c.Options.BearerTokenSource.(bearerTokenInvalidator); ok {
				invalidator.Invalidate(token)
				return nil, true, statusError(resp)
			}
		}
		if httpResponse.StatusCode == http.StatusSwitchingProtocols {
			return nil, false, newDICOMwebError(ErrorKindDecodeResponse, resp.URL, resp.StatusCode, err)
		}
		return nil, false, statusError(resp)
	}
	_ = conn.SetDeadline(time.Time{})
	channel := &WorkitemEventChannel{
		conn:     ws,
		events:   make(chan WorkitemEvent, 64),
		done:     make(chan struct{}),
		response: responseFromHTTP(u.String(), httpResponse),
	}
	go channel.read()
	return channel, false, nil
}

func dialEventChannel(ctx context.Context, transport *http.Transport, u *url.URL) (net.Conn, error) {
	host := u.Hostname()
	port := u.Port()
	if port == "" {
		port = "80"
		if u.Scheme == "https" {
			port = "443"
		}
	}
	address := net.JoinHostPort(host, port)
	if u.Scheme == "https" && transport.DialTLSContext != nil {
		return transport.DialTLSContext(ctx, "tcp", address)
	}
	dial := transport.DialContext
	if dial == nil {
		dial = sourceport.DialContext(&net.Dialer{Timeout: 30 * time.Second, KeepAlive: 30 * time.Second})
	}
	conn, err := dial(ctx, "tcp", address)
	if err != nil || u.Scheme != "https" {
		return conn, err
	}
	config := &tls.Config{MinVersion: tls.VersionTLS12}
	if transport.TLSClientConfig != nil {
		config = transport.TLSClientConfig.Clone()
	}
	if config.ServerName == "" {
		config.ServerName = host
	}
	// WebSocket over TLS speaks HTTP/1.1 (RFC 6455); never negotiate h2.
	config.NextProtos = []string{"http/1.1"}
	tlsConn := tls.Client(conn, config)
	if err := tlsConn.HandshakeContext(ctx); err != nil {
		_ = conn.Close()
		return nil, err
	}
	return tlsConn, nil
}

func (ch *WorkitemEventChannel) read() {
	defer close(ch.events)
	for {
		opcode, message, err := ch.conn.ReadMessage()
		if err != nil {
			// A terminated transport closes the channel even when the peer's
			// close frame was overtaken by an OS connection-aborted error.
			// Preserve the cause for diagnostics and protocol-error callers.
			ch.fail(errors.Join(ErrEventChannelClosed, err))
			return
		}
		if opcode != websocket.OpText {
			continue
		}
		event, err := DecodeWorkitemEvent(message)
		if err != nil {
			continue
		}
		select {
		case ch.events <- event:
		case <-ch.done:
			return
		}
	}
}

func (ch *WorkitemEventChannel) fail(err error) {
	if errors.Is(err, websocket.ErrClosed) {
		err = ErrEventChannelClosed
	}
	ch.mu.Lock()
	if ch.err == nil {
		ch.err = err
	}
	ch.mu.Unlock()
}

func (ch *WorkitemEventChannel) closeError() error {
	ch.mu.Lock()
	defer ch.mu.Unlock()
	if ch.err == nil {
		return ErrEventChannelClosed
	}
	return ch.err
}

// Response returns the handshake response metadata.
func (ch *WorkitemEventChannel) Response() Response {
	return ch.response
}

// Next waits for the next event. After the channel closes it returns
// ErrEventChannelClosed or the transport error that ended it.
func (ch *WorkitemEventChannel) Next(ctx context.Context) (WorkitemEvent, error) {
	ctx = contextOrBackground(ctx)
	select {
	case <-ctx.Done():
		return WorkitemEvent{}, ctx.Err()
	case event, ok := <-ch.events:
		if !ok {
			return WorkitemEvent{}, ch.closeError()
		}
		return event, nil
	}
}

// Close ends the channel with a normal closure.
func (ch *WorkitemEventChannel) Close() error {
	if ch == nil {
		return nil
	}
	ch.closeOnce.Do(func() { close(ch.done) })
	ch.fail(ErrEventChannelClosed)
	return ch.conn.Close(websocket.CloseNormal, "")
}
