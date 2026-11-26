package oidc

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/json"
	"html"
	"io"
	"net"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"
)

// LoginPrompt is what the user must do to finish a login. For the
// authorization code flow, open AuthorizationURL in the system browser; for
// the device flow, show VerificationURI and UserCode (or
// VerificationURIComplete).
type LoginPrompt struct {
	Flow                    Flow
	AuthorizationURL        string
	VerificationURI         string
	VerificationURIComplete string
	UserCode                string
	ExpiresAt               time.Time
}

// String omits URLs and codes that carry one-time login state.
func (p LoginPrompt) String() string {
	flow := string(p.Flow)
	if flow == "" {
		flow = "unknown"
	}
	return "OIDC login prompt (flow=" + flow + ", interaction values redacted)"
}

// GoString redacts like String.
func (p LoginPrompt) GoString() string {
	return p.String()
}

// MarshalJSON emits only the flow and expiry.
func (p LoginPrompt) MarshalJSON() ([]byte, error) {
	return json.Marshal(struct {
		Flow      Flow      `json:"flow,omitempty"`
		ExpiresAt time.Time `json:"expiresAt,omitempty"`
	}{Flow: p.Flow, ExpiresAt: p.ExpiresAt})
}

// LoginSession is a login waiting for the browser or device step.
type LoginSession struct {
	Prompt LoginPrompt

	wait   func(context.Context) (Token, error)
	cancel func()
	once   sync.Once
	token  Token
	err    error
}

// String omits the pending state, codes and the resulting tokens.
func (s *LoginSession) String() string {
	if s == nil {
		return "OIDC login session (nil)"
	}
	return "OIDC login session (flow=" + string(s.Prompt.Flow) + ", secrets redacted)"
}

// GoString redacts like String.
func (s *LoginSession) GoString() string {
	return s.String()
}

// Wait blocks until the login completes, fails, ctx ends or Close is called
// (context.Canceled). The result is memoized; the session releases its
// loopback listener when Wait returns.
func (s *LoginSession) Wait(ctx context.Context) (Token, error) {
	if s == nil || s.wait == nil {
		return Token{}, newError(ErrorLogin, "wait", 0, "", nil)
	}
	s.once.Do(func() {
		s.token, s.err = s.wait(contextOrBackground(ctx))
		s.Close()
	})
	return s.token, s.err
}

// Close abandons the login and releases its listener or polling loop.
func (s *LoginSession) Close() {
	if s != nil && s.cancel != nil {
		s.cancel()
	}
}

// BeginLogin discovers the provider and starts the configured flow.
func (c *Client) BeginLogin(ctx context.Context) (*LoginSession, error) {
	metadata, err := c.Discover(ctx)
	if err != nil {
		return nil, err
	}
	if c.config.Flow == FlowDeviceCode {
		return c.beginDeviceLogin(ctx, metadata)
	}
	return c.beginAuthorizationCodeLogin(metadata)
}

// RandomURLToken returns size random bytes encoded as unpadded base64url.
func RandomURLToken(size int) (string, error) {
	value := make([]byte, size)
	if _, err := rand.Read(value); err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(value), nil
}

// PKCEChallenge returns the S256 code challenge of verifier (RFC 7636).
func PKCEChallenge(verifier string) string {
	sum := sha256.Sum256([]byte(verifier))
	return base64.RawURLEncoding.EncodeToString(sum[:])
}

type callbackResult struct {
	code string
	err  error
}

func (c *Client) beginAuthorizationCodeLogin(metadata Metadata) (*LoginSession, error) {
	state, err1 := RandomURLToken(32)
	verifier, err2 := RandomURLToken(64)
	nonce, err3 := RandomURLToken(32)
	if err1 != nil || err2 != nil || err3 != nil {
		return nil, newError(ErrorLogin, "generate login secrets", 0, "", nil)
	}
	listener, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		return nil, newError(ErrorLogin, "listen for callback", 0, "", nil)
	}
	callbackHost := listener.Addr().String()
	redirectURI := "http://" + callbackHost + c.config.CallbackPath
	callback := make(chan callbackResult, 1)
	closed := make(chan struct{})
	application := html.EscapeString(c.config.ApplicationName)

	mux := http.NewServeMux()
	mux.HandleFunc(c.config.CallbackPath, func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet || r.Host != callbackHost {
			http.Error(w, "Invalid OAuth callback target.", http.StatusBadRequest)
			return
		}
		query := r.URL.Query()
		if subtle.ConstantTimeCompare([]byte(query.Get("state")), []byte(state)) != 1 {
			http.Error(w, "Invalid OAuth state. Return to the application and retry.", http.StatusBadRequest)
			return
		}
		result := callbackResult{code: strings.TrimSpace(query.Get("code"))}
		if providerCode := SafeProviderCode(query.Get("error")); providerCode != "" {
			result.err = newError(ErrorLogin, "authorization callback", 0, providerCode, nil)
		} else if query.Get("error") != "" || result.code == "" {
			result.err = newError(ErrorLogin, "authorization callback", 0, "", nil)
		}
		message := "Authentication completed. You may close this window and return to " + application + "."
		if result.err != nil {
			message = "Authentication failed. You may close this window and return to " + application + " to retry."
		}
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.Header().Set("Cache-Control", "no-store")
		w.Header().Set("Referrer-Policy", "no-referrer")
		_, _ = io.WriteString(w, "<!doctype html><title>Sign-in</title><p>"+message+"</p>")
		select {
		case callback <- result:
		default:
		}
	})
	server := &http.Server{
		Handler:           mux,
		ReadTimeout:       5 * time.Second,
		WriteTimeout:      5 * time.Second,
		ReadHeaderTimeout: 5 * time.Second,
		IdleTimeout:       5 * time.Second,
	}
	go func() { _ = server.Serve(listener) }()
	var cancelOnce sync.Once
	cancel := func() {
		cancelOnce.Do(func() {
			close(closed)
			shutdownCtx, stop := context.WithTimeout(context.Background(), time.Second)
			defer stop()
			_ = server.Shutdown(shutdownCtx)
			_ = listener.Close()
		})
	}

	authorizationURL, err := url.Parse(metadata.AuthorizationEndpoint)
	if err != nil {
		cancel()
		return nil, newError(ErrorLogin, "build authorization request", 0, "", nil)
	}
	query := authorizationURL.Query()
	query.Set("response_type", "code")
	query.Set("client_id", c.config.ClientID)
	query.Set("redirect_uri", redirectURI)
	query.Set("scope", c.scope)
	query.Set("state", state)
	query.Set("nonce", nonce)
	query.Set("code_challenge", PKCEChallenge(verifier))
	query.Set("code_challenge_method", "S256")
	if c.config.Audience != "" {
		query.Set("audience", c.config.Audience)
	}
	if containsExact(c.config.Scopes, "offline_access") {
		// OpenID Connect Core 11: offline_access requires prompt=consent.
		query.Set("prompt", "consent")
	}
	authorizationURL.RawQuery = query.Encode()

	session := &LoginSession{
		Prompt: LoginPrompt{Flow: FlowAuthorizationCode, AuthorizationURL: authorizationURL.String()},
		cancel: cancel,
	}
	session.wait = func(ctx context.Context) (Token, error) {
		select {
		case <-ctx.Done():
			return Token{}, ctx.Err()
		case <-closed:
			return Token{}, context.Canceled
		case result := <-callback:
			if result.err != nil {
				return Token{}, result.err
			}
			form := url.Values{
				"grant_type":    {"authorization_code"},
				"client_id":     {c.config.ClientID},
				"code":          {result.code},
				"redirect_uri":  {redirectURI},
				"code_verifier": {verifier},
			}
			return c.exchange(ctx, metadata, form, TokenExpectations{Kind: ErrorLogin, Nonce: nonce})
		}
	}
	return session, nil
}

type deviceAuthorization struct {
	DeviceCode              string `json:"device_code"`
	UserCode                string `json:"user_code"`
	VerificationURI         string `json:"verification_uri"`
	VerificationURIComplete string `json:"verification_uri_complete"`
	ExpiresIn               int64  `json:"expires_in"`
	Interval                int64  `json:"interval"`
}

const maxDevicePollInterval = 30 * time.Second

func (c *Client) beginDeviceLogin(ctx context.Context, metadata Metadata) (*LoginSession, error) {
	form := url.Values{"client_id": {c.config.ClientID}, "scope": {c.scope}}
	if c.config.Audience != "" {
		form.Set("audience", c.config.Audience)
	}
	body, status, providerCode, err := postForm(ctx, c.http, metadata.DeviceAuthorizationEndpoint, form)
	if err != nil {
		return nil, contextError(ErrorLogin, "start device login", err)
	}
	if status < 200 || status >= 300 {
		return nil, newError(ErrorLogin, "start device login", status, providerCode, nil)
	}
	var device deviceAuthorization
	if err := json.Unmarshal(body, &device); err != nil ||
		strings.TrimSpace(device.DeviceCode) == "" ||
		strings.TrimSpace(device.UserCode) == "" ||
		device.ExpiresIn <= 0 {
		return nil, newError(ErrorLogin, "decode device login", status, "", nil)
	}
	if ValidateSecureEndpoint(device.VerificationURI) != nil ||
		(device.VerificationURIComplete != "" && ValidateSecureEndpoint(device.VerificationURIComplete) != nil) {
		return nil, newError(ErrorLogin, "validate device login", status, "", nil)
	}
	interval := time.Duration(device.Interval) * time.Second
	if interval <= 0 {
		interval = 5 * time.Second
	}
	expiresAt := time.Now().Add(time.Duration(device.ExpiresIn) * time.Second)
	sessionCtx, cancel := context.WithCancel(context.Background())
	session := &LoginSession{
		Prompt: LoginPrompt{
			Flow:                    FlowDeviceCode,
			VerificationURI:         device.VerificationURI,
			VerificationURIComplete: device.VerificationURIComplete,
			UserCode:                device.UserCode,
			ExpiresAt:               expiresAt,
		},
		cancel: cancel,
	}
	session.wait = func(ctx context.Context) (Token, error) {
		return c.pollDeviceToken(ctx, sessionCtx, metadata, device.DeviceCode, expiresAt, interval)
	}
	return session, nil
}

func (c *Client) pollDeviceToken(
	ctx context.Context,
	sessionCtx context.Context,
	metadata Metadata,
	deviceCode string,
	expiresAt time.Time,
	interval time.Duration,
) (Token, error) {
	expired := func() error {
		return newError(ErrorLogin, "poll device login", 0, "expired_token", nil)
	}
	delay := minDuration(interval, maxDuration(time.Until(expiresAt), 0))
	timer := time.NewTimer(delay)
	defer timer.Stop()
	for {
		select {
		case <-ctx.Done():
			return Token{}, ctx.Err()
		case <-sessionCtx.Done():
			return Token{}, context.Canceled
		case <-timer.C:
		}
		if !time.Now().Before(expiresAt) {
			return Token{}, expired()
		}
		form := url.Values{
			"grant_type":  {"urn:ietf:params:oauth:grant-type:device_code"},
			"device_code": {deviceCode},
			"client_id":   {c.config.ClientID},
		}
		body, status, providerCode, err := postForm(ctx, c.http, metadata.TokenEndpoint, form)
		switch {
		case err != nil:
			if ctx.Err() != nil {
				return Token{}, ctx.Err()
			}
			// Transient transport failure: back off (RFC 8628 3.5).
			interval = minDuration(interval*2, maxDevicePollInterval)
		case status >= 200 && status < 300:
			return c.DecodeTokenResponse(ctx, metadata, body, TokenExpectations{Kind: ErrorLogin})
		case providerCode == "authorization_pending":
		case providerCode == "slow_down":
			interval += 5 * time.Second
		default:
			return Token{}, newError(ErrorLogin, "poll device login", status, providerCode, nil)
		}
		delay = minDuration(interval, maxDuration(time.Until(expiresAt), 0))
		if delay <= 0 {
			return Token{}, expired()
		}
		timer.Reset(delay)
	}
}

func minDuration(a, b time.Duration) time.Duration {
	if a < b {
		return a
	}
	return b
}

func maxDuration(a, b time.Duration) time.Duration {
	if a > b {
		return a
	}
	return b
}
