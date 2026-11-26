package oidc

import (
	"crypto"
	"crypto/ecdsa"
	"crypto/ed25519"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"math/big"
	"net/http"
	"net/http/httptest"
	"net/url"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// testSigner is a private signing key generated per test run; no key material
// is stored in the repository.
type testSigner struct {
	kid       string
	algorithm string
	key       crypto.Signer
}

var (
	rsaKeyOnce sync.Once
	rsaKeys    [2]*rsa.PrivateKey
	rsaKeyErr  error
)

func testRSAKey(t *testing.T, index int) *rsa.PrivateKey {
	t.Helper()
	rsaKeyOnce.Do(func() {
		for i := range rsaKeys {
			rsaKeys[i], rsaKeyErr = rsa.GenerateKey(rand.Reader, 2048)
			if rsaKeyErr != nil {
				return
			}
		}
	})
	if rsaKeyErr != nil {
		t.Fatalf("generate RSA key: %v", rsaKeyErr)
	}
	return rsaKeys[index]
}

func newRSASigner(t *testing.T, kid string, index int, algorithm string) testSigner {
	return testSigner{kid: kid, algorithm: algorithm, key: testRSAKey(t, index)}
}

func newECSigner(t *testing.T, kid string, curve elliptic.Curve, algorithm string) testSigner {
	t.Helper()
	key, err := ecdsa.GenerateKey(curve, rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	return testSigner{kid: kid, algorithm: algorithm, key: key}
}

func newEdSigner(t *testing.T, kid string) testSigner {
	t.Helper()
	_, key, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	return testSigner{kid: kid, algorithm: "EdDSA", key: key}
}

func (s testSigner) jwk() map[string]string {
	out := map[string]string{"kid": s.kid, "use": "sig", "alg": s.algorithm}
	switch key := s.key.Public().(type) {
	case *rsa.PublicKey:
		out["kty"] = "RSA"
		out["n"] = base64.RawURLEncoding.EncodeToString(key.N.Bytes())
		out["e"] = base64.RawURLEncoding.EncodeToString(big.NewInt(int64(key.E)).Bytes())
	case *ecdsa.PublicKey:
		size := (key.Curve.Params().BitSize + 7) / 8
		out["kty"] = "EC"
		out["crv"] = key.Curve.Params().Name
		out["x"] = base64.RawURLEncoding.EncodeToString(key.X.FillBytes(make([]byte, size)))
		out["y"] = base64.RawURLEncoding.EncodeToString(key.Y.FillBytes(make([]byte, size)))
	case ed25519.PublicKey:
		out["kty"] = "OKP"
		out["crv"] = "Ed25519"
		out["x"] = base64.RawURLEncoding.EncodeToString(key)
	}
	return out
}

func (s testSigner) sign(t *testing.T, header map[string]any, claims map[string]any) string {
	t.Helper()
	if header == nil {
		header = map[string]any{}
	}
	if _, ok := header["alg"]; !ok {
		header["alg"] = s.algorithm
	}
	if _, ok := header["kid"]; !ok && s.kid != "" {
		header["kid"] = s.kid
	}
	headerJSON, err := json.Marshal(header)
	if err != nil {
		t.Fatal(err)
	}
	payload, err := json.Marshal(claims)
	if err != nil {
		t.Fatal(err)
	}
	input := base64.RawURLEncoding.EncodeToString(headerJSON) + "." + base64.RawURLEncoding.EncodeToString(payload)
	var signature []byte
	switch key := s.key.(type) {
	case ed25519.PrivateKey:
		signature = ed25519.Sign(key, []byte(input))
	default:
		h := hashForAlgorithm(s.algorithm)
		h.Write([]byte(input))
		digest := h.Sum(nil)
		hashID := map[int]crypto.Hash{32: crypto.SHA256, 48: crypto.SHA384, 64: crypto.SHA512}[len(digest)]
		switch key := s.key.(type) {
		case *rsa.PrivateKey:
			if s.algorithm[:2] == "PS" {
				signature, err = rsa.SignPSS(rand.Reader, key, hashID, digest, &rsa.PSSOptions{SaltLength: rsa.PSSSaltLengthEqualsHash})
			} else {
				signature, err = rsa.SignPKCS1v15(rand.Reader, key, hashID, digest)
			}
		case *ecdsa.PrivateKey:
			var r, sv *big.Int
			r, sv, err = ecdsa.Sign(rand.Reader, key, digest)
			size := (key.Curve.Params().BitSize + 7) / 8
			signature = append(r.FillBytes(make([]byte, size)), sv.FillBytes(make([]byte, size))...)
		}
		if err != nil {
			t.Fatal(err)
		}
	}
	return input + "." + base64.RawURLEncoding.EncodeToString(signature)
}

func testClaims(issuer, audience, subject string, expiry time.Time) map[string]any {
	return map[string]any{
		"iss": issuer,
		"aud": audience,
		"sub": subject,
		"iat": time.Now().Add(-time.Minute).Unix(),
		"exp": expiry.Unix(),
	}
}

func accessTokenHash(accessToken string) string {
	sum := sha256.Sum256([]byte(accessToken))
	return base64.RawURLEncoding.EncodeToString(sum[:16])
}

// fakeProvider is an in-process OpenID Provider served over TLS.
type fakeProvider struct {
	t      *testing.T
	server *httptest.Server
	issuer string

	mu         sync.Mutex
	signers    []testSigner
	active     testSigner
	metadata   func(*Metadata)
	token      func(form url.Values) (int, any)
	device     func(form url.Values) (int, any)
	userInfo   func(r *http.Request) (int, any)
	revoked    []string
	tokenForms []url.Values

	jwksFetches atomic.Int32
	discoveries atomic.Int32
}

const testClientID = "dicomweb-viewer"

func newFakeProvider(t *testing.T) *fakeProvider {
	t.Helper()
	p := &fakeProvider{t: t}
	signer := newRSASigner(t, "key-1", 0, "RS256")
	p.signers = []testSigner{signer}
	p.active = signer
	p.server = httptest.NewTLSServer(http.HandlerFunc(p.serve))
	t.Cleanup(p.server.Close)
	p.issuer = p.server.URL
	return p
}

func (p *fakeProvider) baseMetadata() Metadata {
	return Metadata{
		Issuer:                      p.issuer,
		AuthorizationEndpoint:       p.issuer + "/authorize",
		TokenEndpoint:               p.issuer + "/token",
		DeviceAuthorizationEndpoint: p.issuer + "/device",
		RevocationEndpoint:          p.issuer + "/revoke",
		UserInfoEndpoint:            p.issuer + "/userinfo",
		JWKSURI:                     p.issuer + "/jwks",
		CodeChallengeMethods:        []string{"S256"},
		IDTokenSigningAlgs:          []string{"RS256", "ES256", "EdDSA"},
	}
}

// rotate publishes a new signing key, optionally retiring the old ones.
func (p *fakeProvider) rotate(signer testSigner, retireOld bool) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if retireOld {
		p.signers = nil
	}
	p.signers = append(p.signers, signer)
	p.active = signer
}

func (p *fakeProvider) idToken(subject, nonce, accessToken string, expiry time.Time) string {
	p.mu.Lock()
	signer := p.active
	p.mu.Unlock()
	claims := testClaims(p.issuer, testClientID, subject, expiry)
	if nonce != "" {
		claims["nonce"] = nonce
	}
	if accessToken != "" {
		claims["at_hash"] = accessTokenHash(accessToken)
	}
	return signer.sign(p.t, nil, claims)
}

func (p *fakeProvider) writeJSON(w http.ResponseWriter, status int, value any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(value)
}

func (p *fakeProvider) serve(w http.ResponseWriter, r *http.Request) {
	switch r.URL.Path {
	case WellKnownPath:
		p.discoveries.Add(1)
		metadata := p.baseMetadata()
		p.mu.Lock()
		mutate := p.metadata
		p.mu.Unlock()
		if mutate != nil {
			mutate(&metadata)
		}
		p.writeJSON(w, http.StatusOK, metadata)
	case "/jwks":
		p.jwksFetches.Add(1)
		p.mu.Lock()
		keys := make([]map[string]string, 0, len(p.signers))
		for _, signer := range p.signers {
			keys = append(keys, signer.jwk())
		}
		p.mu.Unlock()
		p.writeJSON(w, http.StatusOK, map[string]any{"keys": keys})
	case "/token":
		if err := r.ParseForm(); err != nil {
			p.writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid_request"})
			return
		}
		p.mu.Lock()
		p.tokenForms = append(p.tokenForms, r.PostForm)
		handler := p.token
		p.mu.Unlock()
		if handler == nil {
			p.writeJSON(w, http.StatusBadRequest, map[string]string{"error": "unsupported_grant_type"})
			return
		}
		status, body := handler(r.PostForm)
		p.writeJSON(w, status, body)
	case "/device":
		if err := r.ParseForm(); err != nil {
			p.writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid_request"})
			return
		}
		p.mu.Lock()
		handler := p.device
		p.mu.Unlock()
		status, body := handler(r.PostForm)
		p.writeJSON(w, status, body)
	case "/userinfo":
		p.mu.Lock()
		handler := p.userInfo
		p.mu.Unlock()
		if handler == nil {
			http.NotFound(w, r)
			return
		}
		status, body := handler(r)
		p.writeJSON(w, status, body)
	case "/revoke":
		_ = r.ParseForm()
		p.mu.Lock()
		p.revoked = append(p.revoked, r.PostForm.Get("token"))
		p.mu.Unlock()
		w.WriteHeader(http.StatusOK)
	default:
		http.NotFound(w, r)
	}
}

func (p *fakeProvider) client(t *testing.T, flow Flow, mutate func(*Config)) *Client {
	t.Helper()
	config := Config{
		Issuer:     p.issuer,
		ClientID:   testClientID,
		Scopes:     []string{"openid profile offline_access"},
		Audience:   "dicomweb-api",
		Flow:       flow,
		HTTPClient: p.server.Client(),
	}
	if mutate != nil {
		mutate(&config)
	}
	client, err := NewClient(config)
	if err != nil {
		t.Fatalf("NewClient: %v", err)
	}
	return client
}
