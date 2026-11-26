package oidc

import (
	"context"
	"crypto/subtle"
	"encoding/json"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/ThalesMMS/dicom-go/net/dicomweb"
)

// Config configures a public (native-app) OpenID Connect client.
type Config struct {
	// Issuer is the HTTPS issuer identifier. Required.
	Issuer string
	// ClientID is the registered public client identifier. Required.
	ClientID string
	// Scopes must include "openid". Empty selects "openid".
	Scopes []string
	// Audience, when set, is sent as the "audience" request parameter used
	// by several providers to select the DICOMweb API.
	Audience string
	// Flow selects the login grant. Empty selects FlowAuthorizationCode.
	Flow Flow
	// HTTPClient carries TLS trust and timeouts. It is wrapped by
	// SecureHTTPClient; nil uses a client with DefaultHTTPTimeout.
	HTTPClient *http.Client
	// FetchUserInfo requests the UserInfo endpoint, when advertised, after
	// each token exchange. A transport or HTTP failure leaves Token.UserInfo
	// nil, but a response for another subject fails the exchange.
	FetchUserInfo bool
	// ApplicationName is shown on the loopback callback page.
	ApplicationName string
	// CallbackPath is the loopback redirect path. Empty selects
	// "/oauth2/callback".
	CallbackPath string
	// KeySetOptions tunes the JWK Set cache.
	KeySetOptions RemoteKeySetOptions
	// ClockSkew and Now are forwarded to the ID token verifier.
	ClockSkew time.Duration
	Now       func() time.Time
}

// Client runs discovery, login, refresh and revocation for one provider.
// It is safe for concurrent use; discovery and the key set are cached.
type Client struct {
	config Config
	scope  string
	http   *http.Client

	mu       sync.Mutex
	metadata *Metadata
	verifier *Verifier
}

// NewClient validates config and returns a client. No request is sent.
func NewClient(config Config) (*Client, error) {
	issuer, err := NormalizeIssuer(config.Issuer)
	if err != nil {
		return nil, newError(ErrorConfiguration, "validate issuer", 0, "", nil)
	}
	config.Issuer = issuer
	config.ClientID = strings.TrimSpace(config.ClientID)
	if config.ClientID == "" {
		return nil, newError(ErrorConfiguration, "validate client", 0, "", nil)
	}
	scopes := make([]string, 0, len(config.Scopes)+1)
	for _, scope := range config.Scopes {
		for _, field := range strings.Fields(scope) {
			if !containsExact(scopes, field) {
				scopes = append(scopes, field)
			}
		}
	}
	if len(scopes) == 0 {
		scopes = []string{"openid"}
	}
	if !containsExact(scopes, "openid") {
		return nil, newError(ErrorConfiguration, "validate scopes", 0, "", nil)
	}
	config.Scopes = scopes
	switch config.Flow {
	case "":
		config.Flow = FlowAuthorizationCode
	case FlowAuthorizationCode, FlowDeviceCode:
	default:
		return nil, newError(ErrorConfiguration, "validate flow", 0, "", nil)
	}
	config.Audience = strings.TrimSpace(config.Audience)
	if strings.TrimSpace(config.ApplicationName) == "" {
		config.ApplicationName = "the application"
	}
	if config.CallbackPath == "" {
		config.CallbackPath = "/oauth2/callback"
	}
	if !strings.HasPrefix(config.CallbackPath, "/") {
		return nil, newError(ErrorConfiguration, "validate callback path", 0, "", nil)
	}
	return &Client{
		config: config,
		scope:  strings.Join(scopes, " "),
		http:   SecureHTTPClient(config.HTTPClient),
	}, nil
}

// Config returns the normalized configuration.
func (c *Client) Config() Config {
	return c.config
}

// Discover returns the cached provider metadata, fetching it once.
func (c *Client) Discover(ctx context.Context) (Metadata, error) {
	c.mu.Lock()
	if c.metadata != nil {
		out := *c.metadata
		c.mu.Unlock()
		return out, nil
	}
	c.mu.Unlock()
	metadata, err := discover(ctx, c.http, c.config.Issuer, c.config.Flow)
	if err != nil {
		return Metadata{}, err
	}
	c.mu.Lock()
	if c.metadata == nil {
		c.metadata = &metadata
	}
	out := *c.metadata
	c.mu.Unlock()
	return out, nil
}

// IDTokenVerifier returns the cached verifier for ID tokens of metadata.
func (c *Client) IDTokenVerifier(metadata Metadata) (*Verifier, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.verifier != nil {
		return c.verifier, nil
	}
	keys, err := NewRemoteKeySet(c.http, metadata.JWKSURI, c.config.KeySetOptions)
	if err != nil {
		return nil, newError(ErrorDiscovery, "configure key set", 0, "", nil)
	}
	verifier, err := NewVerifier(keys, VerifierConfig{
		Issuer:     metadata.Issuer,
		Audience:   c.config.ClientID,
		Algorithms: SupportedAlgorithms(metadata.IDTokenSigningAlgs),
		ClockSkew:  c.config.ClockSkew,
		Now:        c.config.Now,
	})
	if err != nil {
		return nil, newError(ErrorDiscovery, "configure verifier", 0, "", nil)
	}
	c.verifier = verifier
	return verifier, nil
}

// UserInfo holds the identity claims of a UserInfo response.
type UserInfo struct {
	Subject           string `json:"sub"`
	Email             string `json:"email,omitempty"`
	PreferredUsername string `json:"preferred_username,omitempty"`
	Name              string `json:"name,omitempty"`
}

// Account returns the first non-empty of email, preferred_username, name
// and sub.
func (u UserInfo) Account() string {
	for _, value := range []string{u.Email, u.PreferredUsername, u.Name, u.Subject} {
		if value = strings.TrimSpace(value); value != "" {
			return value
		}
	}
	return ""
}

// Token is the verified result of a token exchange.
type Token struct {
	AccessToken dicomweb.AccessToken
	// IDToken is nil when a refresh response carried no new ID token.
	IDToken *IDToken
	// Subject is the verified sub of the ID token (or the expected subject
	// of a refresh without a new ID token).
	Subject string
	// UserInfo is set when FetchUserInfo is enabled and the provider answered.
	UserInfo *UserInfo

	refreshToken string
}

// NewToken assembles a token, for example from persisted state in tests.
func NewToken(accessToken dicomweb.AccessToken, refreshToken, subject string) Token {
	return Token{AccessToken: accessToken, refreshToken: strings.TrimSpace(refreshToken), Subject: subject}
}

// RefreshToken returns the refresh token to persist, possibly empty.
func (t Token) RefreshToken() string {
	return t.refreshToken
}

// String redacts every secret.
func (t Token) String() string {
	if t.refreshToken == "" {
		return "OIDC token (access token redacted, no refresh token)"
	}
	return "OIDC token (access and refresh tokens redacted)"
}

// GoString redacts every secret from %#v output.
func (t Token) GoString() string {
	return t.String()
}

// MarshalJSON emits only the access-token expiry.
func (t Token) MarshalJSON() ([]byte, error) {
	return json.Marshal(struct {
		Expiry time.Time `json:"expiry,omitempty"`
	}{Expiry: t.AccessToken.Expiry})
}

// TokenExpectations bind a token response to the request that produced it.
type TokenExpectations struct {
	// Kind is ErrorLogin or ErrorRefresh and selects the error sentinel and
	// whether an ID token is mandatory.
	Kind ErrorKind
	// Nonce, when set, must equal the ID token nonce.
	Nonce string
	// Subject, when set, must equal the ID token and UserInfo sub. A refresh
	// response may omit the ID token only when Subject is known.
	Subject string
	// PreviousRefreshToken is kept when the response does not rotate it.
	PreviousRefreshToken string
}

// DecodeTokenResponse validates a token endpoint response body: Bearer token
// type, a non-empty access token, the signed ID token (signature, issuer,
// audience, expiry, nonce, at_hash and subject binding) and, when enabled,
// the UserInfo subject.
func (c *Client) DecodeTokenResponse(
	ctx context.Context,
	metadata Metadata,
	body []byte,
	expect TokenExpectations,
) (Token, error) {
	kind := expect.Kind
	if kind != ErrorRefresh {
		kind = ErrorLogin
	}
	var response struct {
		AccessToken  string `json:"access_token"`
		TokenType    string `json:"token_type"`
		ExpiresIn    int64  `json:"expires_in"`
		RefreshToken string `json:"refresh_token"`
		IDToken      string `json:"id_token"`
	}
	if err := json.Unmarshal(body, &response); err != nil ||
		strings.TrimSpace(response.AccessToken) == "" ||
		!strings.EqualFold(strings.TrimSpace(response.TokenType), "Bearer") {
		return Token{}, newError(kind, "decode token", 0, "", nil)
	}
	var expiry time.Time
	if response.ExpiresIn > 0 {
		expiry = c.now().Add(time.Duration(response.ExpiresIn) * time.Second)
	}
	accessToken, err := dicomweb.NewAccessToken(response.AccessToken, expiry)
	if err != nil {
		return Token{}, newError(kind, "decode token", 0, "", nil)
	}
	subject := strings.TrimSpace(expect.Subject)
	token := Token{AccessToken: accessToken}
	rawIDToken := strings.TrimSpace(response.IDToken)
	if rawIDToken == "" {
		if kind == ErrorLogin || subject == "" {
			return Token{}, newError(kind, "validate ID token", 0, "", ErrMalformedToken)
		}
	} else {
		verifier, err := c.IDTokenVerifier(metadata)
		if err != nil {
			return Token{}, err
		}
		idToken, err := verifier.Verify(ctx, rawIDToken)
		if err != nil {
			if ctxErr := contextOrBackground(ctx).Err(); ctxErr != nil {
				return Token{}, ctxErr
			}
			return Token{}, newError(kind, "validate ID token", 0, "", err)
		}
		if expect.Nonce != "" && subtle.ConstantTimeCompare([]byte(expect.Nonce), []byte(idToken.Nonce)) != 1 {
			return Token{}, newError(kind, "validate ID token", 0, "", ErrNonceMismatch)
		}
		if err := idToken.VerifyAccessToken(response.AccessToken); err != nil {
			return Token{}, newError(kind, "validate ID token", 0, "", err)
		}
		if subject != "" && subtle.ConstantTimeCompare([]byte(subject), []byte(idToken.Subject)) != 1 {
			return Token{}, newError(kind, "validate ID token subject", 0, "", ErrSubjectMismatch)
		}
		subject = idToken.Subject
		token.IDToken = idToken
	}
	token.Subject = subject
	token.refreshToken = strings.TrimSpace(response.RefreshToken)
	if token.refreshToken == "" {
		token.refreshToken = strings.TrimSpace(expect.PreviousRefreshToken)
	}
	if c.config.FetchUserInfo && metadata.UserInfoEndpoint != "" {
		info, err := c.userInfo(ctx, metadata, accessToken)
		switch {
		case err == nil:
			if subtle.ConstantTimeCompare([]byte(subject), []byte(strings.TrimSpace(info.Subject))) != 1 {
				return Token{}, newError(kind, "validate UserInfo subject", 0, "", ErrSubjectMismatch)
			}
			token.UserInfo = &info
		case err == errUserInfoInvalid:
			return Token{}, newError(kind, "validate UserInfo subject", 0, "", ErrSubjectMismatch)
		default:
			if ctxErr := contextOrBackground(ctx).Err(); ctxErr != nil {
				return Token{}, ctxErr
			}
		}
	}
	return token, nil
}

var errUserInfoInvalid = &Error{Kind: ErrorLogin, Operation: "decode UserInfo", Err: ErrLogin}

// UserInfo requests the UserInfo endpoint with accessToken.
func (c *Client) UserInfo(ctx context.Context, accessToken dicomweb.AccessToken) (UserInfo, error) {
	metadata, err := c.Discover(ctx)
	if err != nil {
		return UserInfo{}, err
	}
	if metadata.UserInfoEndpoint == "" {
		return UserInfo{}, newError(ErrorDiscovery, "UserInfo", 0, "", nil)
	}
	info, err := c.userInfo(ctx, metadata, accessToken)
	if err != nil && err != errUserInfoInvalid {
		return UserInfo{}, err
	}
	if err != nil {
		return UserInfo{}, newError(ErrorLogin, "decode UserInfo", 0, "", nil)
	}
	return info, nil
}

func (c *Client) userInfo(ctx context.Context, metadata Metadata, accessToken dicomweb.AccessToken) (UserInfo, error) {
	var info UserInfo
	status, err := getJSON(ctx, c.http, metadata.UserInfoEndpoint, accessToken.Value(), &info)
	if err != nil {
		if status >= 200 && status < 300 {
			return UserInfo{}, errUserInfoInvalid
		}
		return UserInfo{}, contextError(ErrorLogin, "UserInfo", err)
	}
	if status < 200 || status >= 300 {
		return UserInfo{}, newError(ErrorLogin, "UserInfo", status, "", nil)
	}
	if strings.TrimSpace(info.Subject) == "" {
		return UserInfo{}, errUserInfoInvalid
	}
	return info, nil
}

func (c *Client) now() time.Time {
	if c.config.Now != nil {
		return c.config.Now()
	}
	return time.Now()
}

func (c *Client) exchange(
	ctx context.Context,
	metadata Metadata,
	form url.Values,
	expect TokenExpectations,
) (Token, error) {
	body, status, providerCode, err := postForm(ctx, c.http, metadata.TokenEndpoint, form)
	if err != nil {
		return Token{}, contextError(expect.Kind, "exchange token", err)
	}
	if status < 200 || status >= 300 {
		return Token{}, newError(expect.Kind, "exchange token", status, providerCode, nil)
	}
	return c.DecodeTokenResponse(ctx, metadata, body, expect)
}

// Refresh exchanges refreshToken. subject, when known, must match the
// refreshed ID token; a response that omits the ID token is accepted only
// with a known subject. A response without a new refresh token keeps the old
// one.
func (c *Client) Refresh(ctx context.Context, refreshToken, subject string) (Token, error) {
	refreshToken = strings.TrimSpace(refreshToken)
	if refreshToken == "" {
		return Token{}, newError(ErrorRefresh, "refresh", 0, "", ErrNoRefreshToken)
	}
	metadata, err := c.Discover(ctx)
	if err != nil {
		return Token{}, err
	}
	form := url.Values{
		"grant_type":    {"refresh_token"},
		"refresh_token": {refreshToken},
		"client_id":     {c.config.ClientID},
	}
	return c.exchange(ctx, metadata, form, TokenExpectations{
		Kind: ErrorRefresh, Subject: subject, PreviousRefreshToken: refreshToken,
	})
}

// Revoke revokes refreshToken (RFC 7009) when the provider advertises a
// revocation endpoint; without one it returns nil.
func (c *Client) Revoke(ctx context.Context, refreshToken string) error {
	refreshToken = strings.TrimSpace(refreshToken)
	if refreshToken == "" {
		return nil
	}
	metadata, err := c.Discover(ctx)
	if err != nil {
		return err
	}
	if metadata.RevocationEndpoint == "" {
		return nil
	}
	form := url.Values{
		"token":           {refreshToken},
		"token_type_hint": {"refresh_token"},
		"client_id":       {c.config.ClientID},
	}
	_, status, providerCode, err := postForm(ctx, c.http, metadata.RevocationEndpoint, form)
	if err != nil {
		return contextError(ErrorRevocation, "revoke token", err)
	}
	if status < 200 || status >= 300 {
		return newError(ErrorRevocation, "revoke token", status, providerCode, nil)
	}
	return nil
}

// RefreshTokenStore persists the refresh token and subject between runs,
// typically in an OS-protected keyring.
type RefreshTokenStore interface {
	LoadRefreshToken(ctx context.Context) (refreshToken, subject string, err error)
	SaveRefreshToken(ctx context.Context, refreshToken, subject string) error
}

// MemoryRefreshTokenStore is an in-process RefreshTokenStore.
type MemoryRefreshTokenStore struct {
	mu           sync.Mutex
	refreshToken string
	subject      string
}

// NewMemoryRefreshTokenStore seeds a store, for example from a completed login.
func NewMemoryRefreshTokenStore(token Token) *MemoryRefreshTokenStore {
	return &MemoryRefreshTokenStore{refreshToken: token.refreshToken, subject: token.Subject}
}

// LoadRefreshToken implements RefreshTokenStore.
func (s *MemoryRefreshTokenStore) LoadRefreshToken(context.Context) (string, string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.refreshToken == "" {
		return "", "", ErrNoRefreshToken
	}
	return s.refreshToken, s.subject, nil
}

// SaveRefreshToken implements RefreshTokenStore.
func (s *MemoryRefreshTokenStore) SaveRefreshToken(_ context.Context, refreshToken, subject string) error {
	s.mu.Lock()
	s.refreshToken, s.subject = refreshToken, subject
	s.mu.Unlock()
	return nil
}

// String redacts the stored token.
func (s *MemoryRefreshTokenStore) String() string {
	return "OIDC memory refresh-token store (redacted)"
}

// TokenRefreshFunc returns a dicomweb.TokenRefreshFunc that loads the refresh
// token from store, exchanges it, persists a rotated refresh token and
// returns the new access token:
//
//	manager := dicomweb.NewTokenManager(login.AccessToken, client.TokenRefreshFunc(store))
func (c *Client) TokenRefreshFunc(store RefreshTokenStore) dicomweb.TokenRefreshFunc {
	return func(ctx context.Context) (dicomweb.AccessToken, error) {
		if store == nil {
			return dicomweb.AccessToken{}, ErrNoRefreshToken
		}
		refreshToken, subject, err := store.LoadRefreshToken(ctx)
		if err != nil {
			return dicomweb.AccessToken{}, err
		}
		token, err := c.Refresh(ctx, refreshToken, subject)
		if err != nil {
			return dicomweb.AccessToken{}, err
		}
		if token.refreshToken != refreshToken || token.Subject != subject {
			if err := store.SaveRefreshToken(ctx, token.refreshToken, token.Subject); err != nil {
				return dicomweb.AccessToken{}, err
			}
		}
		return token.AccessToken, nil
	}
}
