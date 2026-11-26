package dicomweb

import (
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/ThalesMMS/dicom-go/dicomjson"
)

const (
	// DefaultTimeout bounds a single DICOMweb request when Options.Timeout is unset.
	DefaultTimeout = 20 * time.Second
	// DefaultMaxBodyBytes bounds response bodies when Options.MaxBodyBytes is unset.
	DefaultMaxBodyBytes = int64(512 << 20)
	// DefaultMaxResponseParts bounds one WADO-RS multipart response.
	DefaultMaxResponseParts = 100_000
	// DefaultMaxPartBytes is the effective part ceiling when both the part and
	// whole-response limits are unset. A zero part limit otherwise inherits the
	// configured whole-response ceiling.
	DefaultMaxPartBytes = DefaultMaxBodyBytes
	// DefaultMaxPartHeaderBytes bounds raw MIME headers for one response part.
	DefaultMaxPartHeaderBytes = 64 << 10
	// DefaultMaxMultipartDepth accepts the single multipart/related layer used by WADO-RS.
	DefaultMaxMultipartDepth = 1
)

// Endpoint describes a DICOMweb base URL and optional service-specific paths.
type Endpoint struct {
	BaseURL  string
	QIDOPath string
	WADOPath string
	STOWPath string
	// WADOURIPath enables single-instance URI Service requests on this path.
	WADOURIPath string
	// UPSPath is the optional Worklist Service (UPS-RS) path below BaseURL.
	UPSPath string
}

// Options configures HTTP transport, timeouts, response limits, and auth.
type Options struct {
	// HTTPClient sends the requests. Nil uses a shared client like
	// http.DefaultClient whose dial connects again, up to three more times,
	// when the source port the system chose cannot be used (#2011); a client
	// supplied here keeps its own dial (see ul.SourcePortRetryDialContext).
	HTTPClient   *http.Client `json:"-"`
	Timeout      time.Duration
	MaxBodyBytes int64
	// WorkitemRequester is the AE Title sent as the "requester" query
	// parameter of the UPS-RS Change Workitem State and Request
	// Cancellation transactions (PS3.18 11.7 and 11.8). Empty omits the
	// parameter; some origin servers refuse a request without it.
	WorkitemRequester string `json:",omitempty"`
	// RedirectPolicy can impose origin binding on ordinary HTTP requests.
	// Basic authentication and BulkData always use the original origin.
	RedirectPolicy RedirectPolicy `json:",omitempty"`
	// ResponseLimits bounds WADO-RS and WADO-URI responses. Zero fields select
	// finite defaults.
	ResponseLimits ResponseLimits
	// MetadataMediaTypes selects the ordered QIDO-RS and WADO-RS metadata
	// representations advertised by the client. Empty preserves the historical
	// DICOM JSON-only behavior. DICOM XML responses use multipart/related as
	// required by PS3.18.
	MetadataMediaTypes []MetadataMediaType
	// MaxMetadataParts bounds metadata datasets in WADO-RS responses,
	// multipart DICOM XML responses, and streaming QIDO-RS responses.
	// Zero uses a finite default.
	MaxMetadataParts int
	// BasicUsername and BasicPassword are sent only over certificate-verified
	// HTTPS. Basic authentication rejects plaintext endpoints, skip-verify TLS,
	// opaque RoundTrippers, custom TLS dialers, and redirects away from the
	// original HTTPS origin. Custom roots and client certificates remain
	// supported through a standard *http.Transport.
	BasicUsername string `json:"-"`
	BasicPassword string `json:"-"`
	BearerToken   string `json:"-"`
	// BearerTokenSource takes precedence over static bearer and basic
	// credentials. Failure to obtain a token fails closed without downgrade.
	BearerTokenSource BearerTokenSource `json:"-"`
	// Retry, when set, retries bodiless requests with exponential backoff and
	// jitter. Nil sends each request once.
	Retry *RetryPolicy `json:"-"`
	// Cache, when set, serves repeated buffered GET responses by URL and
	// Accept header. Nil disables caching.
	Cache *ResponseCache `json:"-"`
}

// MetadataMediaType identifies a supported QIDO-RS or WADO-RS metadata
// representation. DICOMXML is carried in multipart/related responses while
// DICOMJSON is carried as one JSON array.
type MetadataMediaType string

const (
	MetadataMediaTypeDICOMJSON MetadataMediaType = "application/dicom+json"
	MetadataMediaTypeDICOMXML  MetadataMediaType = "application/dicom+xml"
)

// String reports configuration shape without exposing credentials.
func (o Options) String() string {
	return fmt.Sprintf(
		"DICOMweb options (custom_http_client=%t, timeout=%s, max_body_bytes=%d, max_response_parts=%d, max_part_bytes=%d, max_part_header_bytes=%d, max_multipart_depth=%d, allowed_wado_media_types=%d, metadata_media_types=%d, max_metadata_parts=%d, basic_auth=%t, static_bearer=%t, dynamic_bearer=%t, retry=%t, cache=%t)",
		o.HTTPClient != nil,
		o.Timeout,
		o.MaxBodyBytes,
		o.ResponseLimits.MaxParts,
		o.ResponseLimits.MaxPartBytes,
		o.ResponseLimits.MaxPartHeaderBytes,
		o.ResponseLimits.MaxMultipartDepth,
		len(o.ResponseLimits.AllowedMediaTypes),
		len(o.MetadataMediaTypes),
		o.MaxMetadataParts,
		strings.TrimSpace(o.BasicUsername) != "",
		strings.TrimSpace(o.BearerToken) != "",
		o.BearerTokenSource != nil,
		o.Retry != nil,
		o.Cache != nil,
	)
}

// ResponseLimits bounds client-side WADO response parsing. AllowedMediaTypes
// optionally narrows the standard operation-specific part media types; it
// cannot enable a representation incompatible with the requested resource.
type ResponseLimits struct {
	MaxParts           int
	MaxPartBytes       int64
	MaxPartHeaderBytes int
	MaxMultipartDepth  int
	AllowedMediaTypes  []string
}

// GoString redacts credentials from %#v formatting and crash diagnostics.
func (o Options) GoString() string {
	return o.String()
}

// Client is a neutral DICOMweb client for one endpoint.
type Client struct {
	Endpoint Endpoint
	Options  Options
}

// Dataset is a raw DICOM JSON dataset keyed by uppercase tag hex.
type Dataset = dicomjson.Dataset

// Element is a raw DICOM JSON element.
type Element = dicomjson.Element

// Response captures common HTTP response details.
type Response struct {
	URL        string
	StatusCode int
	Status     string
	Header     http.Header
	Body       []byte
}

// VerifyResult describes a DICOMweb verify request.
type VerifyResult struct {
	Response
	Duration  time.Duration
	StartedAt time.Time
}

// InstanceRef identifies a DICOM instance inside a study and series.
type InstanceRef struct {
	StudyInstanceUID  string
	SeriesInstanceUID string
	SOPInstanceUID    string
}

// RetrieveOptions configures WADO-RS object retrieval.
type RetrieveOptions struct {
	// TransferSyntaxUIDs lists DICOM transfer syntax preferences for the WADO-RS
	// Accept header. After HTTP 406, retrieval retries with the first preference
	// removed until the server responds with another status or the preferences
	// are exhausted. Use "*" explicitly to request the server's
	// default/preserved transfer syntax as a fallback.
	TransferSyntaxUIDs []string
}

// ObjectPart is one WADO-RS DICOM object payload.
type ObjectPart struct {
	ContentType       string
	TransferSyntaxUID string
	Data              []byte
}

// ObjectPartStream is one DICOM object payload streamed from the HTTP response.
type ObjectPartStream struct {
	ContentType       string
	TransferSyntaxUID string
	Reader            io.Reader
}

// LocatedObjectPartStream is one streamed object plus its same-origin
// Content-Location. Study and series retrieval use this type so callers can
// correlate each representation without reparsing its Part 10 payload.
type LocatedObjectPartStream struct {
	Part            ObjectPartStream
	ContentLocation string
}

// FramePart is one WADO-RS RetrieveFrames payload. FrameNumber is populated in
// request order because DICOMweb returns one MIME part per requested frame.
type FramePart struct {
	FrameNumber       int
	ContentType       string
	TransferSyntaxUID string
	Data              []byte
}

// StoreInstance is one DICOM object to include in a STOW-RS upload.
type StoreInstance struct {
	SOPClassUID    string
	SOPInstanceUID string
	Path           string
	// Data is the in-memory DICOM Part 10 payload. Prefer Reader or Open for
	// large objects so STOW-RS can stream the multipart body.
	Data []byte
	// Reader streams the DICOM Part 10 payload for this instance. It is used
	// when Open is nil and Data is empty.
	Reader io.Reader
	// Open returns a fresh DICOM Part 10 payload reader. It is preferred over
	// Reader so callers can open files lazily during multipart streaming.
	Open func() (io.ReadCloser, error)
	// Size is the length in bytes of the payload sent, which for Open is
	// what its reader returns, not the length of a file it re-encodes. It
	// only plans the requests of StoreInstancesBatched; zero is unknown
	// (len(Data) for a Data payload).
	Size int64
}

// StoreItem describes one SOP item reported in a STOW-RS response.
type StoreItem struct {
	SOPClassUID    string
	SOPInstanceUID string
	RetrieveURL    string
	WarningReason  uint16
	FailureReason  uint16
}

// StoreResult describes a STOW-RS response.
type StoreResult struct {
	Response
	Stored []StoreItem
	Failed []StoreItem
}

// ErrorKind classifies DICOMweb failures.
type ErrorKind string

const (
	ErrorKindInvalidEndpoint ErrorKind = "invalid_endpoint"
	ErrorKindRequestFailure  ErrorKind = "request_failure"
	ErrorKindTimeout         ErrorKind = "timeout"
	ErrorKindHTTPStatus      ErrorKind = "http_status"
	ErrorKindAuthStatus      ErrorKind = "auth_status"
	ErrorKindAuthToken       ErrorKind = "auth_token"
	ErrorKindDecodeResponse  ErrorKind = "decode_response"
)

// Error is a typed DICOMweb error.
type Error struct {
	Kind       ErrorKind
	URL        string
	StatusCode int
	Status     string
	Err        error
}

func (e *Error) Error() string {
	if e == nil {
		return "DICOMweb error"
	}
	target := SafeURL(e.URL)
	status := safeHTTPStatus(e.StatusCode)
	switch e.Kind {
	case ErrorKindHTTPStatus, ErrorKindAuthStatus:
		if status != "" {
			return fmt.Sprintf("DICOMweb %s returned %s", target, status)
		}
		return fmt.Sprintf("DICOMweb %s returned an HTTP error", target)
	case ErrorKindAuthToken:
		if errors.Is(e.Err, ErrBearerSignInRequired) {
			return fmt.Sprintf("DICOMweb authentication for %s requires a new sign-in", target)
		}
		return fmt.Sprintf("DICOMweb authentication for %s could not obtain a bearer token", target)
	case ErrorKindTimeout:
		return fmt.Sprintf("DICOMweb %s timed out", target)
	case ErrorKindInvalidEndpoint:
		return "invalid DICOMweb endpoint"
	case ErrorKindDecodeResponse:
		return fmt.Sprintf("decode DICOMweb response from %s", target)
	default:
		if status != "" {
			return fmt.Sprintf("DICOMweb request to %s failed after %s", target, status)
		}
		return fmt.Sprintf("DICOMweb request to %s failed", target)
	}
}

// GoString prevents diagnostic formatting from bypassing Error's redaction.
func (e *Error) GoString() string {
	if e == nil {
		return "DICOMweb error"
	}
	return fmt.Sprintf("DICOMweb error (kind=%q, url=%q, status_code=%d, status=%q)",
		e.Kind, SafeURL(e.URL), e.StatusCode, safeHTTPStatus(e.StatusCode))
}

// Unwrap returns the underlying error.
func (e *Error) Unwrap() error {
	if e == nil {
		return nil
	}
	return e.Err
}
