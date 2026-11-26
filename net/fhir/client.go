package fhir

import (
	"bytes"
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"fmt"
	"io"
	"mime"
	"net"
	"net/http"
	"net/netip"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/ThalesMMS/dicom-go/internal/sourceport"
)

var (
	ErrEndpoint  = errors.New("fhir: invalid or out-of-scope endpoint")
	ErrOptions   = errors.New("fhir: invalid client options")
	ErrLimit     = errors.New("fhir: resource limit exceeded")
	ErrPageLimit = fmt.Errorf("%w: page count exceeded", ErrLimit)
	ErrPageLoop  = errors.New("fhir: repeated search page")
	ErrRequest   = errors.New("fhir: invalid search request")
	ErrAuth      = errors.New("fhir: authorization unavailable or out of scope")
	ErrResponse  = errors.New("fhir: invalid search response")
	ErrTransport = errors.New("fhir: HTTP exchange failed")
)

const (
	maxSearchURLBytes  = 32 << 10
	maxSearchFormBytes = 64 << 10
	defaultMaxPages    = 32
	defaultPageBytes   = 8 << 20
	defaultTotalBytes  = 32 << 20
	maxPageBytes       = 64 << 20
	maxTotalBytes      = 128 << 20
	maxPageCount       = 256
)

// HTTPStatusError identifies an unsuccessful search status and exposes no
// response body or endpoint. A redirect response is never followed.
type HTTPStatusError struct{ StatusCode int }

func (e *HTTPStatusError) Error() string {
	return fmt.Sprintf("fhir: search returned HTTP %d", e.StatusCode)
}

// SMARTGrant is authorization supplied by an application from a trusted
// token source. Audience must identify the configured FHIR base, including
// its tenant path. Scopes and Context must come from the granted token.
type SMARTGrant struct {
	AccessToken string
	Audience    string
	Scopes      string
	Context     SMARTContext
}

// ClientOptions configures a FHIR R4 JSON search client. All budgets have
// finite defaults. The application owns OAuth discovery, token validation,
// refresh, and verification of patient or other resource-level access.
type ClientOptions struct {
	Timeout       time.Duration
	MaxPages      int
	MaxPageBytes  int64
	MaxTotalBytes int64
	// RootCAs replaces system roots when non-nil. The client clones the pool.
	RootCAs *x509.CertPool
	// AllowLoopbackHTTP enables plaintext only at a literal numeric
	// loopback address, for local fixtures.
	AllowLoopbackHTTP bool
	// Authorize supplies a current SMART grant before each request. When
	// nil, search runs without an Authorization header.
	Authorize func(context.Context) (SMARTGrant, error)
}

// Client searches one trusted FHIR base and is safe to use concurrently
// when the optional Authorize callback is concurrency-safe.
type Client struct {
	base      *url.URL
	transport *http.Transport
	options   ClientOptions
}

// NewClient configures one FHIR service base. Pagination never escapes its
// origin and canonical path. A base URL contains no credentials or query.
func NewClient(baseURL string, options ClientOptions) (*Client, error) {
	base, err := parseBase(baseURL)
	if err != nil {
		return nil, err
	}
	switch base.Scheme {
	case "https":
	case "http":
		ip, err := netip.ParseAddr(base.Hostname())
		if err != nil || !options.AllowLoopbackHTTP || !ip.IsLoopback() || ip.Zone() != "" {
			return nil, ErrEndpoint
		}
	default:
		return nil, ErrEndpoint
	}
	if options.Timeout < 0 || options.MaxPages < 0 || options.MaxPages > maxPageCount ||
		options.MaxPageBytes < 0 || options.MaxPageBytes > maxPageBytes || options.MaxTotalBytes < 0 ||
		options.MaxTotalBytes > maxTotalBytes {
		return nil, ErrOptions
	}
	if options.Timeout == 0 {
		options.Timeout = 30 * time.Second
	}
	if options.MaxPages == 0 {
		options.MaxPages = defaultMaxPages
	}
	if options.MaxPageBytes == 0 {
		options.MaxPageBytes = defaultPageBytes
	}
	if options.MaxTotalBytes == 0 {
		options.MaxTotalBytes = defaultTotalBytes
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
	return &Client{base: base, transport: transport, options: options}, nil
}

// ImagingSearch supplies encoded FHIR search values. ByPOST sends a standard
// form POST to ImagingStudy/_search. The server's next links drive subsequent
// GET requests. The caller must keep Parameters unchanged until return.
type ImagingSearch struct {
	Parameters url.Values
	ByPOST     bool
}

// SearchImagingStudies returns the matched ImagingStudy resources in wire
// order. A failure returns no partial result. Included resources and outcomes
// are never adopted as matches, and no referenced resource is fetched.
func (c *Client) SearchImagingStudies(ctx context.Context, opts ImagingSearch) ([]ImagingStudy, error) {
	if c == nil || c.transport == nil {
		return nil, ErrEndpoint
	}
	if ctx == nil {
		ctx = context.Background()
	}
	ctx, cancel := context.WithTimeout(ctx, c.options.Timeout)
	defer cancel()

	target := *c.base
	target.Path += "/" + imagingStudyType
	query, err := encodeSearch(opts.Parameters)
	if err != nil {
		return nil, err
	}
	var body []byte
	if opts.ByPOST {
		target.Path += "/_search"
		body = []byte(query)
	} else {
		target.RawQuery = query
	}

	var results []ImagingStudy
	used := make(map[string]struct{})
	totalBytes := int64(0)
	for page := 0; ; page++ {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		if page >= c.options.MaxPages {
			return nil, ErrPageLimit
		}
		if totalBytes >= c.options.MaxTotalBytes {
			return nil, ErrLimit
		}
		byPOST := page == 0 && opts.ByPOST
		// Only a GET URL is replayable as a continuation. A next link
		// to the initial POST URL may be a different GET interaction.
		if !byPOST {
			key := target.String()
			if _, repeated := used[key]; repeated {
				return nil, ErrPageLoop
			}
			used[key] = struct{}{}
		}
		limit := min(c.options.MaxPageBytes, c.options.MaxTotalBytes-totalBytes)
		data, err := c.searchPage(ctx, &target, byPOST, body, limit)
		if err != nil {
			return nil, err
		}
		totalBytes += int64(len(data))
		studies, next, err := decodePage(ctx, data)
		if err != nil {
			return nil, err
		}
		results = append(results, studies...)
		if next == "" {
			return results, nil
		}
		target, err = c.resolveNext(&target, next)
		if err != nil {
			return nil, err
		}
		body = nil
	}
}

func encodeSearch(values url.Values) (string, error) {
	if len(values) > 256 {
		return "", ErrLimit
	}
	total := 0
	for key, items := range values {
		if key == "" || len(key) > maxSearchFormBytes {
			return "", ErrRequest
		}
		for _, value := range items {
			if len(value) > maxSearchFormBytes {
				return "", ErrLimit
			}
			total += len(key) + len(value) + 2
			if total > maxSearchFormBytes {
				return "", ErrLimit
			}
		}
	}
	query := values.Encode()
	if len(query) > maxSearchFormBytes {
		return "", ErrLimit
	}
	return query, nil
}

func (c *Client) searchPage(ctx context.Context, target *url.URL, byPOST bool, body []byte, limit int64) ([]byte, error) {
	if len(target.String()) > maxSearchURLBytes {
		return nil, ErrLimit
	}
	method := http.MethodGet
	route := strings.TrimPrefix(target.Path, c.base.Path)
	var payload io.Reader
	if byPOST {
		method, payload = http.MethodPost, bytes.NewReader(body)
	}
	request, err := http.NewRequestWithContext(ctx, method, target.String(), payload)
	if err != nil {
		return nil, ErrRequest
	}
	request.Header.Set("Accept", "application/fhir+json")
	if byPOST {
		request.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	}
	if c.options.Authorize != nil {
		grant, err := c.options.Authorize(ctx)
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		audience, audienceErr := parseBase(grant.Audience)
		if err != nil || audienceErr != nil || !sameOrigin(c.base, audience) || c.base.Path != audience.Path ||
			!validBearer(grant.AccessToken) {
			return nil, ErrAuth
		}
		if err := CheckSMARTScopes(grant.Scopes, grant.Context, method, route); err != nil {
			return nil, err
		}
		request.Header.Set("Authorization", "Bearer "+grant.AccessToken)
	}
	response, err := c.transport.RoundTrip(request)
	if err != nil {
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		return nil, ErrTransport
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		return nil, &HTTPStatusError{StatusCode: response.StatusCode}
	}
	media, _, err := mime.ParseMediaType(response.Header.Get("Content-Type"))
	if err != nil || (media != "application/fhir+json" && media != "application/json") {
		return nil, ErrResponse
	}
	if response.ContentLength > limit {
		return nil, ErrLimit
	}
	data, err := io.ReadAll(io.LimitReader(response.Body, limit+1))
	if err != nil {
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		return nil, ErrTransport
	}
	if int64(len(data)) > limit {
		return nil, ErrLimit
	}
	return data, nil
}

func validBearer(token string) bool {
	if len(token) == 0 || len(token) > 16<<10 {
		return false
	}
	padding := false
	for i := 0; i < len(token); i++ {
		b := token[i]
		if b == '=' && i > 0 {
			padding = true
			continue
		}
		if padding || !((b >= 'a' && b <= 'z') || (b >= 'A' && b <= 'Z') ||
			(b >= '0' && b <= '9') || strings.ContainsRune("-._~+/", rune(b))) {
			return false
		}
	}
	return true
}

func parseBase(raw string) (*url.URL, error) {
	u, err := parseSafeURL(raw)
	if err != nil || u.Hostname() == "" || (u.Scheme != "http" && u.Scheme != "https") ||
		u.RawQuery != "" || u.ForceQuery {
		return nil, ErrEndpoint
	}
	u.Path = strings.TrimSuffix(u.Path, "/")
	return u, nil
}

func parseSafeURL(raw string) (*url.URL, error) {
	if len(raw) == 0 || len(raw) > maxSearchURLBytes || strings.ContainsAny(raw, "#\\") {
		return nil, ErrEndpoint
	}
	u, err := url.Parse(raw)
	if err != nil || u.Opaque != "" || u.User != nil || strings.HasSuffix(u.Host, ":") ||
		!canonicalURLPath(u.EscapedPath()) {
		return nil, ErrEndpoint
	}
	// The transport can translate Unicode hostnames with IDNA. Compare
	// the same ASCII origin used for dialing and TLS, including A-labels.
	host := u.Hostname()
	if !isASCIIString(host) || strings.Contains(host, "%") {
		return nil, ErrEndpoint
	}
	if p := u.Port(); p != "" {
		value, err := strconv.ParseUint(p, 10, 16)
		if err != nil || value == 0 || strconv.FormatUint(value, 10) != p {
			return nil, ErrEndpoint
		}
	}
	for i := 0; i < len(raw); i++ {
		if raw[i] <= ' ' || raw[i] == 127 {
			return nil, ErrEndpoint
		}
	}
	return u, nil
}

func isASCIIString(value string) bool {
	for i := 0; i < len(value); i++ {
		if value[i] >= 0x80 {
			return false
		}
	}
	return true
}

func canonicalURLPath(path string) bool {
	if path == "" || path == "/" {
		return true
	}
	for _, part := range strings.Split(strings.TrimSuffix(strings.TrimPrefix(path, "/"), "/"), "/") {
		if part == "" || part == "." || part == ".." {
			return false
		}
		for i := 0; i < len(part); i++ {
			b := part[i]
			if !((b >= 'a' && b <= 'z') || (b >= 'A' && b <= 'Z') ||
				(b >= '0' && b <= '9') || strings.ContainsRune("-._~", rune(b))) {
				return false
			}
		}
	}
	return true
}

func sameOrigin(left, right *url.URL) bool {
	return left.Scheme == right.Scheme && strings.EqualFold(left.Hostname(), right.Hostname()) &&
		effectivePort(left) == effectivePort(right)
}

func effectivePort(u *url.URL) string {
	if port := u.Port(); port != "" {
		return port
	}
	if u.Scheme == "https" {
		return "443"
	}
	return "80"
}

func (c *Client) resolveNext(previous *url.URL, raw string) (url.URL, error) {
	reference, err := parseSafeURL(raw)
	if err != nil {
		return url.URL{}, err
	}
	target := previous.ResolveReference(reference)
	if !sameOrigin(c.base, target) || target.RawQuery == "" || target.Path != c.base.Path+"/"+imagingStudyType {
		return url.URL{}, ErrEndpoint
	}
	return *target, nil
}
