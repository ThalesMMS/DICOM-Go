package dicomwebcli

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"syscall"

	"github.com/ThalesMMS/dicom-go/net/dicomweb"
)

type inputError struct {
	message string
}

func (e *inputError) Error() string { return e.message }

func invalidInput(message string) error { return &inputError{message: message} }

func classifyExit(ctx context.Context, err error) int {
	switch {
	case errors.Is(err, errPartialStore):
		return ExitPartialStore
	case errors.Is(err, context.Canceled), errors.Is(ctx.Err(), context.Canceled):
		return ExitCanceled
	}
	var input *inputError
	if errors.As(err, &input) {
		return ExitInput
	}
	var protocolError *dicomweb.Error
	if !errors.As(err, &protocolError) {
		return ExitFailure
	}
	switch protocolError.Kind {
	case dicomweb.ErrorKindInvalidEndpoint:
		return ExitInput
	case dicomweb.ErrorKindAuthStatus, dicomweb.ErrorKindAuthToken:
		return ExitAuth
	case dicomweb.ErrorKindHTTPStatus:
		return ExitHTTP
	case dicomweb.ErrorKindDecodeResponse:
		return ExitDecode
	default:
		return ExitFailure
	}
}

func writeDiagnostic(w io.Writer, operation string, err error, exit int) {
	if exit == ExitInput {
		var input *inputError
		if errors.As(err, &input) && input.message != "" {
			_, _ = fmt.Fprintf(w, "%s: input error: %s\n", operation, input.message)
			return
		}
	}
	if exit == ExitPartialStore {
		_, _ = fmt.Fprintln(w, "store: partial result")
		return
	}
	if exit == ExitCanceled {
		_, _ = fmt.Fprintf(w, "%s: canceled\n", operation)
		return
	}
	var protocolError *dicomweb.Error
	if errors.As(err, &protocolError) {
		target := dicomweb.SafeURL(protocolError.URL)
		switch cause := transportCause(protocolError); {
		case protocolError.StatusCode != 0:
			_, _ = fmt.Fprintf(w, "%s: kind=%s endpoint=%s status=%d%s\n", operation, protocolError.Kind, target, protocolError.StatusCode, statusHint(protocolError.StatusCode))
		case cause != "":
			_, _ = fmt.Fprintf(w, "%s: kind=%s endpoint=%s cause=%s: %s\n", operation, protocolError.Kind, target, cause, transportCauseHints[cause])
		default:
			_, _ = fmt.Fprintf(w, "%s: kind=%s endpoint=%s\n", operation, protocolError.Kind, target)
		}
		return
	}
	_, _ = fmt.Fprintf(w, "%s: kind=io_or_request\n", operation)
}

// Value-free causes of a request that never received an HTTP status.
const (
	causeTLSUnknownAuthority   = "tls_unknown_authority"
	causeTLSServerName         = "tls_server_name_mismatch"
	causeTLSCertificateInvalid = "tls_certificate_invalid"
	causeTLSNotOffered         = "tls_not_offered_by_server"
	causeTLSRejectedByServer   = "tls_rejected_by_server"
	causeHostNotResolved       = "host_not_resolved"
	causeConnectionRefused     = "connection_refused"
	causeConnectionClosed      = "connection_closed"
	causeHostUnreachable       = "host_unreachable"
)

var transportCauseHints = map[string]string{
	causeTLSUnknownAuthority:   "the server certificate is not signed by a trusted CA; pass the issuing CA with -ca",
	causeTLSServerName:         "the server certificate does not match the requested name; check the host in -base-url or -tls-server-name",
	causeTLSCertificateInvalid: "the server certificate is expired, not yet valid or not usable for a server",
	causeTLSNotOffered:         "the server did not answer with TLS; check whether the endpoint is plain HTTP",
	causeTLSRejectedByServer:   "the server refused the TLS handshake; check whether it requires a client certificate (-cert and -key) that it trusts",
	causeHostNotResolved:       "the host name in -base-url does not resolve",
	causeConnectionRefused:     "nothing accepts connections at that host and port",
	causeConnectionClosed:      "the server closed the connection before answering",
	causeHostUnreachable:       "the host or its network is unreachable",
}

// Winsock reports these numbers without mapping them to the portable errno
// constants.
const (
	wsaeConnectionAborted  = syscall.Errno(10053)
	wsaeConnectionReset    = syscall.Errno(10054)
	wsaeConnectionRefused  = syscall.Errno(10061)
	wsaeHostUnreachable    = syscall.Errno(10065)
	wsaeNetworkUnreachable = syscall.Errno(10051)
)

// transportCause names why a request failed below HTTP, or returns "" when
// the failure has no recognized transport cause. The result is one of a fixed
// set of words, so it cannot carry a credential, a path or an identifier.
func transportCause(err error) string {
	var unknownAuthority x509.UnknownAuthorityError
	var hostname x509.HostnameError
	var invalid x509.CertificateInvalidError
	var verification *tls.CertificateVerificationError
	var recordHeader tls.RecordHeaderError
	var alert tls.AlertError
	var dns *net.DNSError
	var operation *net.OpError
	var errno syscall.Errno
	switch {
	case errors.As(err, &unknownAuthority):
		return causeTLSUnknownAuthority
	case errors.As(err, &hostname):
		return causeTLSServerName
	case errors.As(err, &invalid), errors.As(err, &verification):
		return causeTLSCertificateInvalid
	case errors.As(err, &recordHeader), errors.Is(err, http.ErrSchemeMismatch):
		return causeTLSNotOffered
	case errors.As(err, &alert):
		return causeTLSRejectedByServer
	case errors.As(err, &dns):
		return causeHostNotResolved
	}
	// crypto/tls reports an alert sent by the server as a "remote error".
	if errors.As(err, &operation) && operation.Op == "remote error" {
		return causeTLSRejectedByServer
	}
	if errors.As(err, &errno) {
		switch errno {
		case syscall.ECONNREFUSED, wsaeConnectionRefused:
			return causeConnectionRefused
		case syscall.ECONNRESET, syscall.ECONNABORTED, syscall.EPIPE, wsaeConnectionReset, wsaeConnectionAborted:
			return causeConnectionClosed
		case syscall.EHOSTUNREACH, syscall.ENETUNREACH, wsaeHostUnreachable, wsaeNetworkUnreachable:
			return causeHostUnreachable
		}
	}
	if errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF) {
		return causeConnectionClosed
	}
	return ""
}

// statusHint explains the two statuses an operator fixes with credentials.
func statusHint(status int) string {
	switch status {
	case http.StatusUnauthorized:
		return ": the server rejected or did not receive credentials; check the authentication flags"
	case http.StatusForbidden:
		return ": the credentials are valid but not authorized for this resource"
	}
	return ""
}
