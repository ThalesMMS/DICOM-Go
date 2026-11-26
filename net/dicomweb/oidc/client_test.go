package oidc

import (
	"context"
	"crypto/elliptic"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/ThalesMMS/dicom-go/net/dicomweb"
)

func TestDiscoverValidatesMetadataAndCaches(t *testing.T) {
	provider := newFakeProvider(t)
	client := provider.client(t, FlowAuthorizationCode, nil)
	metadata, err := client.Discover(context.Background())
	if err != nil {
		t.Fatalf("Discover: %v", err)
	}
	if metadata.TokenEndpoint != provider.issuer+"/token" {
		t.Fatalf("metadata = %+v", metadata)
	}
	if _, err := client.Discover(context.Background()); err != nil {
		t.Fatal(err)
	}
	if got := provider.discoveries.Load(); got != 1 {
		t.Fatalf("discovery requests = %d, want 1", got)
	}

	untrusted, err := NewClient(Config{Issuer: provider.issuer, ClientID: testClientID})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := untrusted.Discover(context.Background()); !errors.Is(err, ErrDiscovery) {
		t.Fatalf("Discover with untrusted TLS = %v, want ErrDiscovery", err)
	}

	tests := []struct {
		name   string
		flow   Flow
		mutate func(*Metadata)
	}{
		{"issuer mismatch", FlowAuthorizationCode, func(m *Metadata) { m.Issuer = "https://other.example.test" }},
		{"issuer must match literally", FlowAuthorizationCode, func(m *Metadata) { m.Issuer += "/" }},
		{"HTTP token endpoint", FlowAuthorizationCode, func(m *Metadata) { m.TokenEndpoint = "http://provider.example.test/token" }},
		{"endpoint user information", FlowAuthorizationCode, func(m *Metadata) {
			m.TokenEndpoint = "https://user:secret@provider.example.test/token"
		}},
		{"endpoint fragment", FlowAuthorizationCode, func(m *Metadata) { m.TokenEndpoint += "#fragment" }},
		{"HTTP UserInfo endpoint", FlowAuthorizationCode, func(m *Metadata) { m.UserInfoEndpoint = "http://provider.example.test/userinfo" }},
		{"missing JWKS", FlowAuthorizationCode, func(m *Metadata) { m.JWKSURI = "" }},
		{"unsafe algorithms only", FlowAuthorizationCode, func(m *Metadata) { m.IDTokenSigningAlgs = []string{"none", "HS256"} }},
		{"missing S256", FlowAuthorizationCode, func(m *Metadata) { m.CodeChallengeMethods = []string{"plain"} }},
		{"missing device endpoint", FlowDeviceCode, func(m *Metadata) { m.DeviceAuthorizationEndpoint = "" }},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			provider := newFakeProvider(t)
			provider.metadata = test.mutate
			if _, err := provider.client(t, test.flow, nil).Discover(context.Background()); !errors.Is(err, ErrDiscovery) {
				t.Fatalf("Discover() = %v, want ErrDiscovery", err)
			}
		})
	}

	crossOrigin := provider.baseMetadata()
	crossOrigin.AuthorizationEndpoint = "https://login.example.test/authorize"
	crossOrigin.JWKSURI = "https://keys.example.test/jwks"
	if err := ValidateMetadata(provider.issuer, FlowAuthorizationCode, crossOrigin); err != nil {
		t.Fatalf("secure cross-origin endpoints rejected: %v", err)
	}
}

func TestNewClientRejectsUnsafeConfiguration(t *testing.T) {
	for name, config := range map[string]Config{
		"HTTP issuer":      {Issuer: "http://issuer.example.test", ClientID: "c"},
		"issuer query":     {Issuer: "https://issuer.example.test?x=1", ClientID: "c"},
		"missing client":   {Issuer: "https://issuer.example.test"},
		"scopes no openid": {Issuer: "https://issuer.example.test", ClientID: "c", Scopes: []string{"profile"}},
		"unknown flow":     {Issuer: "https://issuer.example.test", ClientID: "c", Flow: "implicit"},
	} {
		if _, err := NewClient(config); !errors.Is(err, ErrNotConfigured) {
			t.Errorf("%s: NewClient() = %v, want ErrNotConfigured", name, err)
		}
	}
}

func TestHTTPClientRefusesCrossOriginRedirects(t *testing.T) {
	elsewhere := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		t.Error("redirect target must not be contacted")
	}))
	defer elsewhere.Close()
	var issuer string
	sameOriginHops := atomic.Int32{}
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/cross" + WellKnownPath:
			http.Redirect(w, r, elsewhere.URL+WellKnownPath, http.StatusFound)
		case "/same" + WellKnownPath:
			sameOriginHops.Add(1)
			http.Redirect(w, r, "/metadata", http.StatusFound)
		case "/metadata":
			metadata := Metadata{
				Issuer: issuer + "/same", TokenEndpoint: issuer + "/token", JWKSURI: issuer + "/jwks",
			}
			_ = json.NewEncoder(w).Encode(metadata)
		case "/loop" + WellKnownPath:
			http.Redirect(w, r, r.URL.Path, http.StatusFound)
		}
	}))
	defer server.Close()
	issuer = server.URL
	transport := server.Client().Transport.(*http.Transport).Clone()
	transport.TLSClientConfig.RootCAs.AddCert(elsewhere.Certificate())
	httpClient := &http.Client{Transport: transport}

	if _, err := Discover(context.Background(), httpClient, issuer+"/cross", ""); !errors.Is(err, ErrDiscovery) {
		t.Fatalf("cross-origin redirect = %v, want ErrDiscovery", err)
	}
	if _, err := Discover(context.Background(), httpClient, issuer+"/same", ""); err != nil {
		t.Fatalf("same-origin redirect: %v", err)
	}
	if _, err := Discover(context.Background(), httpClient, issuer+"/loop", ""); !errors.Is(err, ErrDiscovery) {
		t.Fatalf("redirect loop = %v, want ErrDiscovery", err)
	}
}

func TestHTTPClientRefusesRetargetingAfterRedirectCallback(t *testing.T) {
	var destinationRequests atomic.Int32
	external := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		destinationRequests.Add(1)
		w.WriteHeader(http.StatusNoContent)
	}))
	defer external.Close()
	origin := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, "/local-token", http.StatusTemporaryRedirect)
	}))
	defer origin.Close()

	client := *origin.Client()
	remote, err := url.Parse(external.URL + "/external-token")
	if err != nil {
		t.Fatal(err)
	}
	client.CheckRedirect = func(req *http.Request, via []*http.Request) error {
		req.URL = remote
		via[0].URL = remote
		return nil
	}
	req, err := http.NewRequestWithContext(context.Background(), http.MethodPost, origin.URL+"/token", strings.NewReader("synthetic-token-payload"))
	if err != nil {
		t.Fatal(err)
	}
	_, err = SecureHTTPClient(&client).Do(req)
	if !errors.Is(err, errCrossOrigin) || destinationRequests.Load() != 0 {
		t.Fatalf("retargeted token request count=%d error=%v", destinationRequests.Load(), err)
	}
}

func TestAuthorizationCodeLoginWithPKCE(t *testing.T) {
	provider := newFakeProvider(t)
	var nonce atomic.Value
	provider.token = func(form url.Values) (int, any) {
		if form.Get("grant_type") != "authorization_code" || form.Get("code") != "the-code" {
			return http.StatusBadRequest, map[string]string{"error": "invalid_grant"}
		}
		return http.StatusOK, map[string]any{
			"access_token": "access-secret", "refresh_token": "refresh-secret",
			"token_type": "Bearer", "expires_in": 3600,
			"id_token": provider.idToken("subject-1", nonce.Load().(string), "access-secret", time.Now().Add(time.Hour)),
		}
	}
	provider.userInfo = func(r *http.Request) (int, any) {
		if r.Header.Get("Authorization") != "Bearer access-secret" {
			return http.StatusUnauthorized, map[string]string{}
		}
		return http.StatusOK, map[string]string{"sub": "subject-1", "preferred_username": "reader"}
	}
	client := provider.client(t, FlowAuthorizationCode, func(c *Config) {
		c.FetchUserInfo = true
		c.ApplicationName = "Viewer <test>"
	})
	session, err := client.BeginLogin(context.Background())
	if err != nil {
		t.Fatalf("BeginLogin: %v", err)
	}
	defer session.Close()
	authorization, err := url.Parse(session.Prompt.AuthorizationURL)
	if err != nil {
		t.Fatal(err)
	}
	query := authorization.Query()
	nonce.Store(query.Get("nonce"))
	for field, want := range map[string]string{
		"response_type": "code", "client_id": testClientID, "scope": "openid profile offline_access",
		"audience": "dicomweb-api", "code_challenge_method": "S256", "prompt": "consent",
	} {
		if got := query.Get(field); got != want {
			t.Errorf("authorization %s = %q, want %q", field, got, want)
		}
	}
	redirectURI := query.Get("redirect_uri")
	if !strings.HasPrefix(redirectURI, "http://127.0.0.1:") {
		t.Fatalf("redirect_uri = %q, want loopback", redirectURI)
	}
	state := query.Get("state")
	for _, text := range []string{session.Prompt.String(), fmt.Sprintf("%#v", session), string(mustJSON(t, session.Prompt))} {
		if containsAny(text, state, query.Get("nonce"), redirectURI) {
			t.Fatalf("login formatting leaks state: %s", text)
		}
	}

	expectStatus := func(request *http.Request, want int) string {
		t.Helper()
		response, err := http.DefaultClient.Do(request)
		if err != nil {
			t.Fatal(err)
		}
		defer response.Body.Close()
		body, _ := io.ReadAll(response.Body)
		if response.StatusCode != want {
			t.Fatalf("callback status = %d, want %d", response.StatusCode, want)
		}
		return string(body)
	}
	wrongState, _ := http.NewRequest(http.MethodGet, redirectURI+"?code=the-code&state=forged", nil)
	expectStatus(wrongState, http.StatusBadRequest)
	wrongHost, _ := http.NewRequest(http.MethodGet, redirectURI+"?code=the-code&state="+url.QueryEscape(state), nil)
	wrongHost.Host = "attacker.example.test"
	expectStatus(wrongHost, http.StatusBadRequest)
	valid, _ := http.NewRequest(http.MethodGet, redirectURI+"?code=the-code&state="+url.QueryEscape(state), nil)
	page := expectStatus(valid, http.StatusOK)
	if !strings.Contains(page, "Authentication completed") || !strings.Contains(page, "Viewer &lt;test&gt;") {
		t.Fatalf("callback page = %q", page)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	token, err := session.Wait(ctx)
	if err != nil {
		t.Fatalf("Wait: %v", err)
	}
	if token.AccessToken.Value() != "access-secret" || token.RefreshToken() != "refresh-secret" ||
		token.Subject != "subject-1" || token.UserInfo == nil || token.UserInfo.Account() != "reader" {
		t.Fatalf("token = %v subject=%q userinfo=%+v", token, token.Subject, token.UserInfo)
	}
	for _, text := range []string{token.String(), fmt.Sprintf("%#v", token), string(mustJSON(t, token))} {
		if containsAny(text, "access-secret", "refresh-secret") {
			t.Fatalf("token formatting leaks a secret: %s", text)
		}
	}
	provider.mu.Lock()
	form := provider.tokenForms[len(provider.tokenForms)-1]
	provider.mu.Unlock()
	if form.Get("redirect_uri") != redirectURI || PKCEChallenge(form.Get("code_verifier")) != query.Get("code_challenge") {
		t.Fatalf("token request does not prove PKCE: %v", form)
	}
	if _, err := http.Get(redirectURI); err == nil {
		t.Fatal("loopback listener still accepts connections after Wait")
	}
}

func TestAuthorizationCodeLoginFailures(t *testing.T) {
	t.Run("provider error", func(t *testing.T) {
		provider := newFakeProvider(t)
		session, err := provider.client(t, FlowAuthorizationCode, nil).BeginLogin(context.Background())
		if err != nil {
			t.Fatal(err)
		}
		defer session.Close()
		authorization, _ := url.Parse(session.Prompt.AuthorizationURL)
		response, err := http.Get(authorization.Query().Get("redirect_uri") +
			"?error=access_denied&error_description=secret+text&state=" + url.QueryEscape(authorization.Query().Get("state")))
		if err != nil {
			t.Fatal(err)
		}
		body, _ := io.ReadAll(response.Body)
		response.Body.Close()
		if !strings.Contains(string(body), "Authentication failed") {
			t.Fatalf("failure page = %q", body)
		}
		_, err = session.Wait(context.Background())
		var oidcErr *Error
		if !errors.As(err, &oidcErr) || oidcErr.ProviderCode != "access_denied" || !errors.Is(err, ErrLogin) ||
			strings.Contains(err.Error(), "secret") {
			t.Fatalf("Wait() = %v", err)
		}
	})
	t.Run("nonce mismatch", func(t *testing.T) {
		provider := newFakeProvider(t)
		provider.token = func(url.Values) (int, any) {
			return http.StatusOK, map[string]any{
				"access_token": "a", "token_type": "Bearer",
				"id_token": provider.idToken("s", "replayed-nonce", "", time.Now().Add(time.Hour)),
			}
		}
		session, err := provider.client(t, FlowAuthorizationCode, nil).BeginLogin(context.Background())
		if err != nil {
			t.Fatal(err)
		}
		authorization, _ := url.Parse(session.Prompt.AuthorizationURL)
		response, err := http.Get(authorization.Query().Get("redirect_uri") +
			"?code=c&state=" + url.QueryEscape(authorization.Query().Get("state")))
		if err != nil {
			t.Fatal(err)
		}
		response.Body.Close()
		if _, err := session.Wait(context.Background()); !errors.Is(err, ErrNonceMismatch) || !errors.Is(err, ErrLogin) {
			t.Fatalf("Wait() = %v, want nonce mismatch", err)
		}
	})
	t.Run("close unblocks wait", func(t *testing.T) {
		provider := newFakeProvider(t)
		session, err := provider.client(t, FlowAuthorizationCode, nil).BeginLogin(context.Background())
		if err != nil {
			t.Fatal(err)
		}
		done := make(chan error, 1)
		go func() {
			_, err := session.Wait(context.Background())
			done <- err
		}()
		session.Close()
		select {
		case err := <-done:
			if !errors.Is(err, context.Canceled) {
				t.Fatalf("Wait after Close = %v", err)
			}
		case <-time.After(3 * time.Second):
			t.Fatal("Close did not unblock Wait")
		}
	})
}

func TestDeviceLoginPollsUntilAuthorized(t *testing.T) {
	provider := newFakeProvider(t)
	var polls atomic.Int32
	provider.device = func(form url.Values) (int, any) {
		if form.Get("client_id") != testClientID || form.Get("audience") != "dicomweb-api" {
			return http.StatusBadRequest, map[string]string{"error": "invalid_client"}
		}
		return http.StatusOK, map[string]any{
			"device_code": "device-secret", "user_code": "ABCD-EFGH",
			"verification_uri": provider.issuer + "/activate", "expires_in": 10, "interval": 1,
		}
	}
	provider.token = func(form url.Values) (int, any) {
		if form.Get("grant_type") != "urn:ietf:params:oauth:grant-type:device_code" || form.Get("device_code") != "device-secret" {
			return http.StatusBadRequest, map[string]string{"error": "invalid_grant"}
		}
		if polls.Add(1) == 1 {
			return http.StatusBadRequest, map[string]string{"error": "authorization_pending"}
		}
		return http.StatusOK, map[string]any{
			"access_token": "device-access", "refresh_token": "device-refresh", "token_type": "bearer", "expires_in": 60,
			"id_token": provider.idToken("device-subject", "", "", time.Now().Add(time.Hour)),
		}
	}
	session, err := provider.client(t, FlowDeviceCode, nil).BeginLogin(context.Background())
	if err != nil {
		t.Fatalf("BeginLogin: %v", err)
	}
	defer session.Close()
	if session.Prompt.Flow != FlowDeviceCode || session.Prompt.UserCode != "ABCD-EFGH" {
		t.Fatalf("prompt = %+v", session.Prompt)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	token, err := session.Wait(ctx)
	if err != nil {
		t.Fatalf("Wait: %v", err)
	}
	if polls.Load() != 2 || token.Subject != "device-subject" || token.RefreshToken() != "device-refresh" {
		t.Fatalf("polls=%d subject=%q", polls.Load(), token.Subject)
	}
}

func TestDeviceLoginStopsAtProviderExpiry(t *testing.T) {
	provider := newFakeProvider(t)
	provider.device = func(url.Values) (int, any) {
		return http.StatusOK, map[string]any{
			"device_code": "d", "user_code": "U", "verification_uri": provider.issuer + "/activate",
			"expires_in": 1, "interval": 30,
		}
	}
	session, err := provider.client(t, FlowDeviceCode, nil).BeginLogin(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	started := time.Now()
	_, err = session.Wait(context.Background())
	var oidcErr *Error
	if !errors.As(err, &oidcErr) || oidcErr.ProviderCode != "expired_token" {
		t.Fatalf("Wait() = %v, want expired_token", err)
	}
	if elapsed := time.Since(started); elapsed > 3*time.Second {
		t.Fatalf("expiry observed after %v", elapsed)
	}
	provider.mu.Lock()
	defer provider.mu.Unlock()
	if len(provider.tokenForms) != 0 {
		t.Fatalf("expired device code was polled %d times", len(provider.tokenForms))
	}
}

func TestRefreshVerifiesSubjectAndFeedsTokenManager(t *testing.T) {
	provider := newFakeProvider(t)
	var refreshes atomic.Int32
	var refreshSubject atomic.Value
	refreshSubject.Store("subject-1")
	provider.token = func(form url.Values) (int, any) {
		if form.Get("grant_type") != "refresh_token" {
			return http.StatusBadRequest, map[string]string{"error": "unsupported_grant_type"}
		}
		switch form.Get("refresh_token") {
		case "refresh-1":
			n := refreshes.Add(1)
			return http.StatusOK, map[string]any{
				"access_token": fmt.Sprintf("access-%d", n), "refresh_token": "refresh-2",
				"token_type": "Bearer", "expires_in": 3600,
				"id_token": provider.idToken(refreshSubject.Load().(string), "", "", time.Now().Add(time.Hour)),
			}
		case "refresh-2":
			// No rotated refresh token and no new ID token.
			return http.StatusOK, map[string]any{"access_token": "access-without-id", "token_type": "Bearer", "expires_in": 3600}
		}
		return http.StatusBadRequest, map[string]string{"error": "invalid_grant", "error_description": "token secret leaked?"}
	}
	client := provider.client(t, FlowAuthorizationCode, nil)

	store := NewMemoryRefreshTokenStore(NewToken(dicomweb.AccessToken{}, "refresh-1", "subject-1"))
	manager := dicomweb.NewTokenManager(dicomweb.AccessToken{}, client.TokenRefreshFunc(store))
	access, err := manager.Token(context.Background())
	if err != nil {
		t.Fatalf("TokenManager.Token: %v", err)
	}
	if access.Value() != "access-1" {
		t.Fatalf("access token = %q", access.Value())
	}
	if refresh, subject, _ := store.LoadRefreshToken(context.Background()); refresh != "refresh-2" || subject != "subject-1" {
		t.Fatal("rotated refresh token was not persisted")
	}
	manager.Invalidate(access)
	access, err = manager.Token(context.Background())
	if err != nil || access.Value() != "access-without-id" {
		t.Fatalf("refresh without ID token: %v %q", err, access.Value())
	}
	if refresh, _, _ := store.LoadRefreshToken(context.Background()); refresh != "refresh-2" {
		t.Fatal("refresh token without rotation must be kept")
	}

	refreshSubject.Store("someone-else")
	if _, err := client.Refresh(context.Background(), "refresh-1", "subject-1"); !errors.Is(err, ErrSubjectMismatch) || !errors.Is(err, ErrRefresh) {
		t.Fatalf("subject switch on refresh = %v", err)
	}
	if _, err := client.Refresh(context.Background(), "refresh-2", ""); !errors.Is(err, ErrRefresh) {
		t.Fatalf("refresh without ID token or known subject = %v", err)
	}
	_, err = client.Refresh(context.Background(), "unknown", "subject-1")
	var oidcErr *Error
	if !errors.As(err, &oidcErr) || oidcErr.ProviderCode != "invalid_grant" || oidcErr.StatusCode != http.StatusBadRequest ||
		strings.Contains(err.Error(), "secret") {
		t.Fatalf("invalid grant = %v", err)
	}
	if _, err := client.Refresh(context.Background(), "", ""); !errors.Is(err, ErrNoRefreshToken) {
		t.Fatalf("empty refresh token = %v", err)
	}
}

func TestKeyRotationAndRetirement(t *testing.T) {
	provider := newFakeProvider(t)
	now := time.Now()
	clock := atomic.Int64{}
	clock.Store(now.UnixNano())
	client := provider.client(t, FlowAuthorizationCode, func(c *Config) {
		c.KeySetOptions = RemoteKeySetOptions{
			MinRefreshInterval: 10 * time.Second,
			Now:                func() time.Time { return time.Unix(0, clock.Load()) },
		}
	})
	metadata, err := client.Discover(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	verifier, err := client.IDTokenVerifier(metadata)
	if err != nil {
		t.Fatal(err)
	}
	verify := func(raw string) error {
		_, err := verifier.Verify(context.Background(), raw)
		return err
	}
	old := provider.idToken("s", "", "", time.Now().Add(time.Hour))
	if err := verify(old); err != nil {
		t.Fatalf("initial key: %v", err)
	}
	provider.rotate(newECSigner(t, "key-2", elliptic.P256(), "ES256"), false)
	rotated := provider.idToken("s", "", "", time.Now().Add(time.Hour))
	if err := verify(rotated); !errors.Is(err, ErrKeyNotFound) {
		t.Fatalf("unknown kid inside the refresh interval = %v, want ErrKeyNotFound", err)
	}
	clock.Add(int64(11 * time.Second))
	if err := verify(rotated); err != nil {
		t.Fatalf("rotated key after refetch: %v", err)
	}
	if err := verify(old); err != nil {
		t.Fatalf("old key still published: %v", err)
	}
	if got := provider.jwksFetches.Load(); got != 2 {
		t.Fatalf("JWKS fetches = %d, want 2", got)
	}

	provider.rotate(newEdSigner(t, "key-3"), true)
	clock.Add(int64(2 * time.Hour))
	if err := verify(old); !errors.Is(err, ErrKeyNotFound) {
		t.Fatalf("retired key after cache expiry = %v, want ErrKeyNotFound", err)
	}
	if err := verify(provider.idToken("s", "", "", time.Now().Add(time.Hour))); err != nil {
		t.Fatalf("EdDSA key after retirement: %v", err)
	}
}

func TestRevokeSendsRefreshToken(t *testing.T) {
	provider := newFakeProvider(t)
	client := provider.client(t, FlowAuthorizationCode, nil)
	if err := client.Revoke(context.Background(), "refresh-x"); err != nil {
		t.Fatal(err)
	}
	if err := client.Revoke(context.Background(), ""); err != nil {
		t.Fatal(err)
	}
	provider.mu.Lock()
	defer provider.mu.Unlock()
	if len(provider.revoked) != 1 || provider.revoked[0] != "refresh-x" {
		t.Fatalf("revoked = %d tokens", len(provider.revoked))
	}
}

func TestUserInfoSubjectMustMatch(t *testing.T) {
	provider := newFakeProvider(t)
	provider.token = func(url.Values) (int, any) {
		return http.StatusOK, map[string]any{
			"access_token": "a", "token_type": "Bearer",
			"id_token": provider.idToken("id-subject", "", "", time.Now().Add(time.Hour)),
		}
	}
	provider.userInfo = func(*http.Request) (int, any) {
		return http.StatusOK, map[string]string{"sub": "other-subject", "email": "x@example.test"}
	}
	client := provider.client(t, FlowAuthorizationCode, func(c *Config) { c.FetchUserInfo = true })
	metadata, err := client.Discover(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	body := mustJSON(t, map[string]any{
		"access_token": "a", "token_type": "Bearer",
		"id_token": provider.idToken("id-subject", "", "", time.Now().Add(time.Hour)),
	})
	if _, err := client.DecodeTokenResponse(context.Background(), metadata, body, TokenExpectations{Kind: ErrorLogin}); !errors.Is(err, ErrSubjectMismatch) {
		t.Fatalf("UserInfo for another subject = %v", err)
	}
	provider.userInfo = func(*http.Request) (int, any) { return http.StatusServiceUnavailable, map[string]string{} }
	token, err := client.DecodeTokenResponse(context.Background(), metadata, body, TokenExpectations{Kind: ErrorLogin})
	if err != nil || token.UserInfo != nil {
		t.Fatalf("unavailable UserInfo must be optional: %v", err)
	}
	if _, err := client.DecodeTokenResponse(context.Background(), metadata,
		mustJSON(t, map[string]any{"access_token": "a", "token_type": "mac"}), TokenExpectations{}); !errors.Is(err, ErrLogin) {
		t.Fatalf("non-Bearer token type = %v", err)
	}
	if _, err := client.DecodeTokenResponse(context.Background(), metadata,
		mustJSON(t, map[string]any{"access_token": "a", "token_type": "Bearer"}), TokenExpectations{}); !errors.Is(err, ErrLogin) {
		t.Fatalf("login without ID token = %v", err)
	}
}
