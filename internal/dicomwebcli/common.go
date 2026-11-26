package dicomwebcli

import (
	"crypto/tls"
	"crypto/x509"
	"errors"
	"flag"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"strings"
	"time"

	"github.com/ThalesMMS/dicom-go/core"
	"github.com/ThalesMMS/dicom-go/internal/sourceport"
	"github.com/ThalesMMS/dicom-go/net/dicomweb"
)

type commonOptions struct {
	baseURL          string
	qidoPath         string
	wadoPath         string
	stowPath         string
	timeout          time.Duration
	maxBodyBytes     int64
	output           string
	metadata         string
	basicUser        string
	basicPasswordEnv string
	bearerTokenEnv   string
	allowHTTPAuth    bool
	caFiles          stringList
	tlsServerName    string
	certFile         string
	keyFile          string
}

func defaultCommonOptions() commonOptions {
	return commonOptions{
		timeout:      dicomweb.DefaultTimeout,
		maxBodyBytes: dicomweb.DefaultMaxBodyBytes,
		metadata:     "json",
	}
}

func addCommonFlags(fs *flag.FlagSet, opts *commonOptions) {
	fs.StringVar(&opts.baseURL, "base-url", "", "DICOMweb base URL (required)")
	fs.StringVar(&opts.qidoPath, "qido-path", "", "QIDO-RS service path")
	fs.StringVar(&opts.wadoPath, "wado-path", "", "WADO-RS service path")
	fs.StringVar(&opts.stowPath, "stow-path", "", "STOW-RS service path")
	fs.DurationVar(&opts.timeout, "timeout", opts.timeout, "request timeout")
	fs.Int64Var(&opts.maxBodyBytes, "max-body-bytes", opts.maxBodyBytes, "maximum response body bytes")
	fs.StringVar(&opts.output, "output", "", "output file or directory")
	fs.StringVar(&opts.metadata, "metadata", opts.metadata, "metadata negotiation: json, xml, or both")
	fs.StringVar(&opts.basicUser, "basic-user", "", "HTTP Basic username")
	fs.StringVar(&opts.basicPasswordEnv, "basic-password-env", "", "environment variable containing the Basic password")
	fs.StringVar(&opts.bearerTokenEnv, "bearer-token-env", "", "environment variable containing a bearer token")
	fs.BoolVar(&opts.allowHTTPAuth, "allow-insecure-auth", false, "allow bearer tokens over plain HTTP; Basic always requires HTTPS")
	fs.Var(&opts.caFiles, "ca", "PEM file of CA certificates trusted to verify the HTTPS server, instead of the system roots (repeatable)")
	fs.StringVar(&opts.tlsServerName, "tls-server-name", "", "require the HTTPS server certificate to match this name, sent as SNI")
	fs.StringVar(&opts.certFile, "cert", "", "PEM client certificate presented to the HTTPS server")
	fs.StringVar(&opts.keyFile, "key", "", "PEM private key matching -cert")
}

func (opts commonOptions) validate() error {
	baseURL := strings.TrimSpace(opts.baseURL)
	if baseURL == "" {
		return invalidInput("-base-url is required")
	}
	u, err := url.Parse(baseURL)
	if err != nil || u.Host == "" || (u.Scheme != "http" && u.Scheme != "https") {
		return invalidInput("-base-url must be an absolute HTTP or HTTPS URL")
	}
	if u.User != nil {
		return invalidInput("userinfo is not allowed in -base-url")
	}
	if u.RawQuery != "" || u.ForceQuery || u.Fragment != "" {
		return invalidInput("query and fragment are not allowed in -base-url")
	}
	if opts.timeout <= 0 {
		return invalidInput("-timeout must be positive")
	}
	if opts.maxBodyBytes <= 0 {
		return invalidInput("-max-body-bytes must be positive")
	}
	basicUser := strings.TrimSpace(opts.basicUser)
	basicPasswordEnv := strings.TrimSpace(opts.basicPasswordEnv)
	bearerTokenEnv := strings.TrimSpace(opts.bearerTokenEnv)
	if (basicUser == "") != (basicPasswordEnv == "") {
		return invalidInput("-basic-user and -basic-password-env must be used together")
	}
	if opts.basicUser != "" && basicUser == "" || opts.basicPasswordEnv != "" && basicPasswordEnv == "" || opts.bearerTokenEnv != "" && bearerTokenEnv == "" {
		return invalidInput("authentication names must not be blank")
	}
	if bearerTokenEnv != "" && (basicUser != "" || basicPasswordEnv != "") {
		return invalidInput("Basic and bearer authentication are mutually exclusive")
	}
	if u.Scheme == "http" && basicUser != "" {
		return invalidInput("Basic authentication requires HTTPS with certificate verification")
	}
	if u.Scheme == "http" && !opts.allowHTTPAuth && bearerTokenEnv != "" {
		return invalidInput("bearer authentication over HTTP requires -allow-insecure-auth")
	}
	switch opts.metadata {
	case "json", "xml", "both":
	default:
		return invalidInput("-metadata must be json, xml, or both")
	}
	if (strings.TrimSpace(opts.certFile) == "") != (strings.TrimSpace(opts.keyFile) == "") {
		return invalidInput("-cert and -key must be given together")
	}
	if u.Scheme != "https" && opts.usesTLSFlags() {
		return invalidInput("-ca, -tls-server-name, -cert and -key require an HTTPS -base-url")
	}
	return nil
}

func (opts commonOptions) usesTLSFlags() bool {
	return len(opts.caFiles) > 0 || strings.TrimSpace(opts.tlsServerName) != "" ||
		strings.TrimSpace(opts.certFile) != "" || strings.TrimSpace(opts.keyFile) != ""
}

// httpClient returns nil, which selects the default client, unless a TLS flag
// asks for private roots, a pinned server name or a client certificate.
// Its errors name the flag and never a path or file content.
func (opts commonOptions) httpClient() (*http.Client, error) {
	if !opts.usesTLSFlags() {
		return nil, nil
	}
	config := &tls.Config{MinVersion: tls.VersionTLS12, ServerName: strings.TrimSpace(opts.tlsServerName)}
	if len(opts.caFiles) > 0 {
		pool := x509.NewCertPool()
		for _, path := range opts.caFiles {
			data, err := os.ReadFile(path)
			if err != nil {
				return nil, invalidInput("a -ca file cannot be read")
			}
			if !pool.AppendCertsFromPEM(data) {
				return nil, invalidInput("a -ca file holds no PEM certificate")
			}
		}
		config.RootCAs = pool
	}
	if certFile := strings.TrimSpace(opts.certFile); certFile != "" {
		certificate, err := tls.LoadX509KeyPair(certFile, strings.TrimSpace(opts.keyFile))
		if err != nil {
			return nil, invalidInput("-cert and -key cannot be loaded as a matching PEM certificate and unencrypted private key")
		}
		config.Certificates = []tls.Certificate{certificate}
	}
	transport := http.DefaultTransport.(*http.Transport).Clone()
	transport.DialContext = sourceport.DialContext(nil)
	transport.TLSClientConfig = config
	return &http.Client{Transport: transport}, nil
}

func (opts commonOptions) client() (dicomweb.Client, error) {
	if err := opts.validate(); err != nil {
		return dicomweb.Client{}, err
	}
	httpClient, err := opts.httpClient()
	if err != nil {
		return dicomweb.Client{}, err
	}
	clientOptions := dicomweb.Options{HTTPClient: httpClient, Timeout: opts.timeout, MaxBodyBytes: opts.maxBodyBytes}
	switch opts.metadata {
	case "xml":
		clientOptions.MetadataMediaTypes = []dicomweb.MetadataMediaType{dicomweb.MetadataMediaTypeDICOMXML}
	case "both":
		clientOptions.MetadataMediaTypes = []dicomweb.MetadataMediaType{dicomweb.MetadataMediaTypeDICOMJSON, dicomweb.MetadataMediaTypeDICOMXML}
	default:
		clientOptions.MetadataMediaTypes = []dicomweb.MetadataMediaType{dicomweb.MetadataMediaTypeDICOMJSON}
	}
	basicUser := strings.TrimSpace(opts.basicUser)
	basicPasswordEnv := strings.TrimSpace(opts.basicPasswordEnv)
	bearerTokenEnv := strings.TrimSpace(opts.bearerTokenEnv)
	if basicPasswordEnv != "" {
		password, ok := os.LookupEnv(basicPasswordEnv)
		if !ok || password == "" {
			return dicomweb.Client{}, invalidInput("Basic password environment variable is missing or empty")
		}
		clientOptions.BasicUsername = basicUser
		clientOptions.BasicPassword = password
	}
	if bearerTokenEnv != "" {
		token, ok := os.LookupEnv(bearerTokenEnv)
		if !ok || strings.TrimSpace(token) == "" {
			return dicomweb.Client{}, invalidInput("bearer token environment variable is missing or empty")
		}
		clientOptions.BearerToken = strings.TrimSpace(token)
	}
	return dicomweb.Client{
		Endpoint: dicomweb.Endpoint{BaseURL: strings.TrimSpace(opts.baseURL), QIDOPath: opts.qidoPath, WADOPath: opts.wadoPath, STOWPath: opts.stowPath},
		Options:  clientOptions,
	}, nil
}

func configureUsage(fs *flag.FlagSet, stderr io.Writer, synopsis string) {
	fs.SetOutput(stderr)
	fs.Usage = func() {
		_, _ = fmt.Fprintf(stderr, "Usage: %s\n\nFlags:\n", synopsis)
		fs.PrintDefaults()
	}
}

func parseFlags(fs *flag.FlagSet, args []string) error {
	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return flag.ErrHelp
		}
		return invalidInput("invalid command flags")
	}
	return nil
}

type stringList []string

func (values *stringList) String() string { return strings.Join(*values, ",") }

func (values *stringList) Set(value string) error {
	*values = append(*values, value)
	return nil
}

func validateTransferSyntaxes(values []string) error {
	for _, value := range values {
		uid := strings.TrimSpace(value)
		if uid != "*" && !core.IsValidUID(uid) {
			return invalidInput("each -transfer-syntax must be a valid UID or *")
		}
	}
	return nil
}
