package oidc

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/ThalesMMS/dicom-go/internal/sourceport"
	"github.com/ThalesMMS/dicom-go/net/dicomweb/internal/originpolicy"
)

const (
	// MaxResponseBytes bounds every provider response body.
	MaxResponseBytes = int64(1 << 20)
	// DefaultHTTPTimeout applies when the caller does not provide a client.
	DefaultHTTPTimeout = 30 * time.Second
	maxRedirects       = 5
)

var (
	errResponseTooLarge = errors.New("oidc: provider response too large")
	errRedirectLimit    = errors.New("oidc: redirect limit exceeded")
	errCrossOrigin      = errors.New("oidc: cross-origin redirect denied")
)

// SecureHTTPClient returns a copy of client (or a default client) whose
// redirect policy refuses another origin and more than five redirects. Any
// caller CheckRedirect still runs for permitted redirects. The default client
// has DefaultHTTPTimeout and a transport like http.DefaultTransport whose dial
// connects again, up to three more times, when the source port the system
// chose cannot be used.
func SecureHTTPClient(client *http.Client) *http.Client {
	if client == nil {
		client = &http.Client{Timeout: DefaultHTTPTimeout, Transport: sourceport.HTTPTransport()}
	}
	return originpolicy.RestrictRedirects(client, maxRedirects, errCrossOrigin, errRedirectLimit)
}

// SameOrigin compares scheme, host and effective port.
func SameOrigin(left, right *url.URL) bool {
	return originpolicy.SameOrigin(left, right)
}

// ValidateSecureEndpoint accepts only absolute HTTPS URLs without user
// information or fragments.
func ValidateSecureEndpoint(raw string) error {
	u, err := url.Parse(strings.TrimSpace(raw))
	if err != nil || u.Scheme != "https" || u.Host == "" || u.User != nil || u.Fragment != "" {
		return ErrNotConfigured
	}
	return nil
}

func readBounded(reader io.Reader) ([]byte, error) {
	data, err := io.ReadAll(io.LimitReader(reader, MaxResponseBytes+1))
	if err != nil {
		return nil, err
	}
	if int64(len(data)) > MaxResponseBytes {
		return nil, errResponseTooLarge
	}
	return data, nil
}

func decodeStrictJSON(data []byte, out any) error {
	decoder := json.NewDecoder(bytes.NewReader(data))
	if err := decoder.Decode(out); err != nil {
		return err
	}
	if decoder.Decode(&struct{}{}) != io.EOF {
		return errors.New("oidc: trailing JSON data")
	}
	return nil
}

func discardBounded(reader io.Reader) {
	_, _ = io.Copy(io.Discard, io.LimitReader(reader, 4096))
}

func contextOrBackground(ctx context.Context) context.Context {
	if ctx == nil {
		return context.Background()
	}
	return ctx
}

// getJSON performs a GET and decodes a bounded JSON body. Non-2xx responses
// return the status with a nil error.
func getJSON(ctx context.Context, client *http.Client, endpoint, bearer string, out any) (int, error) {
	req, err := http.NewRequestWithContext(contextOrBackground(ctx), http.MethodGet, endpoint, nil)
	if err != nil {
		return 0, err
	}
	req.Header.Set("Accept", "application/json")
	if bearer != "" {
		req.Header.Set("Authorization", "Bearer "+bearer)
	}
	response, err := client.Do(req)
	if err != nil {
		return 0, err
	}
	defer response.Body.Close()
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		discardBounded(response.Body)
		return response.StatusCode, nil
	}
	data, err := readBounded(response.Body)
	if err != nil {
		return response.StatusCode, err
	}
	return response.StatusCode, decodeStrictJSON(data, out)
}

// postForm posts an application/x-www-form-urlencoded body. It returns the
// bounded body, the status and, for failures, the safe OAuth error code.
func postForm(ctx context.Context, client *http.Client, endpoint string, form url.Values) ([]byte, int, string, error) {
	req, err := http.NewRequestWithContext(
		contextOrBackground(ctx), http.MethodPost, endpoint, strings.NewReader(form.Encode()),
	)
	if err != nil {
		return nil, 0, "", err
	}
	req.Header.Set("Accept", "application/json")
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	response, err := client.Do(req)
	if err != nil {
		return nil, 0, "", err
	}
	defer response.Body.Close()
	body, err := readBounded(response.Body)
	if err != nil {
		return nil, response.StatusCode, "", err
	}
	providerCode := ""
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		var providerError struct {
			Error string `json:"error"`
		}
		_ = json.Unmarshal(body, &providerError)
		providerCode = SafeProviderCode(providerError.Error)
	}
	return body, response.StatusCode, providerCode, nil
}
