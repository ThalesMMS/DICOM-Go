package dicomweb

import (
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strings"
)

const safeEndpointLabel = "endpoint"

// SafeURL returns the non-sensitive part of an absolute HTTP(S) URL for use in
// errors and diagnostics. Credentials, query parameters, and fragments are
// always removed. Invalid, relative, and unsupported URLs fail closed to the
// generic word "endpoint".
func SafeURL(value string) string {
	parsed, err := url.Parse(strings.TrimSpace(value))
	if err != nil {
		return safeEndpointLabel
	}
	return safeParsedURL(parsed)
}

func safeParsedURL(value *url.URL) string {
	if value == nil || value.Host == "" ||
		(!strings.EqualFold(value.Scheme, "http") && !strings.EqualFold(value.Scheme, "https")) {
		return safeEndpointLabel
	}
	clean := *value
	clean.Scheme = strings.ToLower(clean.Scheme)
	clean.User = nil
	clean.RawQuery = ""
	clean.ForceQuery = false
	clean.Fragment = ""
	clean.RawFragment = ""
	clean.Opaque = ""
	return clean.String()
}

func safeDiagnosticURL(value string) string {
	parsed, err := url.Parse(strings.TrimSpace(value))
	if err != nil {
		return safeEndpointLabel
	}
	parsed.Path = ""
	parsed.RawPath = ""
	return safeParsedURL(parsed)
}

func safeHTTPStatus(code int) string {
	if code <= 0 {
		return ""
	}
	if text := http.StatusText(code); text != "" {
		return fmt.Sprintf("%d %s", code, text)
	}
	return fmt.Sprintf("HTTP %d", code)
}

func safeOperationalError(err error) error {
	if err == nil {
		return nil
	}
	urlErr, ok := err.(*url.Error)
	if !ok {
		return err
	}
	clone := *urlErr
	clone.URL = safeDiagnosticURL(urlErr.URL)
	switch cause := urlErr.Err.(type) {
	case nil:
	case *url.Error:
		clone.Err = safeOperationalError(cause)
	default:
		// net/http can embed a malformed Location in its error text before
		// CheckRedirect runs. RoundTrippers and callbacks can also include
		// private request details. Preserve classification without that text.
		clone.Err = redactedHTTPClientCause{cause: cause}
	}
	return &clone
}

type redactedHTTPClientCause struct {
	cause error
}

func (e redactedHTTPClientCause) Error() string {
	return "HTTP client operation failed"
}

func (e redactedHTTPClientCause) GoString() string {
	return e.Error()
}

func (e redactedHTTPClientCause) Is(target error) bool {
	if target == ErrHTTPConnectionBroken {
		return e.cause != nil && strings.Contains(e.cause.Error(), httpConnectionBrokenText)
	}
	return errors.Is(e.cause, target)
}

// ErrHTTPConnectionBroken matches a request whose response ended inside its
// status line or headers. net/http reports the case in text only, and that
// text is redacted from the errors of this package; errors.Is keeps the
// classification.
var ErrHTTPConnectionBroken = errors.New("dicom dicomweb: the HTTP connection broke inside the response")

// httpConnectionBrokenText is how net/http words the case.
const httpConnectionBrokenText = "transport connection broken"

func (e redactedHTTPClientCause) As(target any) bool {
	return errors.As(e.cause, target)
}

// net/url.Error asks its immediate cause for these flags.
func (e redactedHTTPClientCause) Timeout() bool {
	cause, ok := e.cause.(interface{ Timeout() bool })
	return ok && cause.Timeout()
}

func (e redactedHTTPClientCause) Temporary() bool {
	cause, ok := e.cause.(interface{ Temporary() bool })
	return ok && cause.Temporary()
}

func newDICOMwebError(kind ErrorKind, rawURL string, statusCode int, err error) *Error {
	return &Error{
		Kind:       kind,
		URL:        safeDiagnosticURL(rawURL),
		StatusCode: statusCode,
		Status:     safeHTTPStatus(statusCode),
		Err:        safeOperationalError(err),
	}
}
