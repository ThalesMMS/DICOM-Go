package oidc

import (
	"context"
	"crypto/elliptic"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"
)

const testIssuer = "https://issuer.example.test"

func staticVerifier(t *testing.T, signers []testSigner, mutate func(*VerifierConfig)) *Verifier {
	t.Helper()
	var keys StaticKeySet
	for _, signer := range signers {
		raw, err := json.Marshal(map[string]any{"keys": []map[string]string{signer.jwk()}})
		if err != nil {
			t.Fatal(err)
		}
		parsed, err := ParseJWKS(raw)
		if err != nil {
			t.Fatalf("ParseJWKS(%s): %v", signer.algorithm, err)
		}
		keys = append(keys, parsed...)
	}
	config := VerifierConfig{
		Issuer:     testIssuer,
		Audience:   testClientID,
		Algorithms: allowedAlgorithms,
	}
	if mutate != nil {
		mutate(&config)
	}
	verifier, err := NewVerifier(keys, config)
	if err != nil {
		t.Fatalf("NewVerifier: %v", err)
	}
	return verifier
}

func TestVerifierAcceptsEverySupportedAlgorithm(t *testing.T) {
	signers := []testSigner{
		newRSASigner(t, "rs256", 0, "RS256"),
		newRSASigner(t, "rs384", 0, "RS384"),
		newRSASigner(t, "rs512", 0, "RS512"),
		newRSASigner(t, "ps256", 1, "PS256"),
		newRSASigner(t, "ps384", 1, "PS384"),
		newRSASigner(t, "ps512", 1, "PS512"),
		newECSigner(t, "es256", elliptic.P256(), "ES256"),
		newECSigner(t, "es384", elliptic.P384(), "ES384"),
		newECSigner(t, "es512", elliptic.P521(), "ES512"),
		newEdSigner(t, "eddsa"),
	}
	verifier := staticVerifier(t, signers, nil)
	for _, signer := range signers {
		t.Run(signer.algorithm, func(t *testing.T) {
			h := hashForAlgorithm(signer.algorithm)
			h.Write([]byte("access"))
			sum := h.Sum(nil)
			claims := testClaims(testIssuer, testClientID, "subject-1", time.Now().Add(time.Hour))
			claims["at_hash"] = base64.RawURLEncoding.EncodeToString(sum[:len(sum)/2])
			token, err := verifier.Verify(context.Background(), signer.sign(t, nil, claims))
			if err != nil {
				t.Fatalf("Verify: %v", err)
			}
			if token.Subject != "subject-1" || token.Algorithm != signer.algorithm || token.KeyID != signer.kid {
				t.Fatalf("token subject=%q alg=%q kid=%q", token.Subject, token.Algorithm, token.KeyID)
			}
			if err := token.VerifyAccessToken("access"); err != nil {
				t.Fatalf("VerifyAccessToken: %v", err)
			}
			if err := token.VerifyAccessToken("other"); !errors.Is(err, ErrAccessTokenHashMismatch) {
				t.Fatalf("VerifyAccessToken(other) = %v", err)
			}
		})
	}
}

func unsignedToken(header string, claims map[string]any, signature []byte) string {
	payload, _ := json.Marshal(claims)
	input := base64.RawURLEncoding.EncodeToString([]byte(header)) + "." +
		base64.RawURLEncoding.EncodeToString(payload)
	return input + "." + base64.RawURLEncoding.EncodeToString(signature)
}

func TestVerifierRejectsInvalidTokens(t *testing.T) {
	signer := newRSASigner(t, "key-1", 0, "RS256")
	impostor := newRSASigner(t, "key-1", 1, "RS256")
	verifier := staticVerifier(t, []testSigner{signer}, nil)
	claims := func(mutate func(map[string]any)) map[string]any {
		out := testClaims(testIssuer, testClientID, "subject-1", time.Now().Add(time.Hour))
		if mutate != nil {
			mutate(out)
		}
		return out
	}
	valid := signer.sign(t, nil, claims(nil))

	// Algorithm confusion: an HS256 token keyed with the public RSA modulus.
	hsInput := base64.RawURLEncoding.EncodeToString([]byte(`{"alg":"HS256","kid":"key-1"}`)) + "." +
		base64.RawURLEncoding.EncodeToString(mustJSON(t, claims(nil)))
	mac := hmac.New(sha256.New, []byte(signer.jwk()["n"]))
	mac.Write([]byte(hsInput))
	hsToken := hsInput + "." + base64.RawURLEncoding.EncodeToString(mac.Sum(nil))

	tests := []struct {
		name  string
		token string
		want  error
	}{
		{"empty", "", ErrMalformedToken},
		{"two segments", "a.b", ErrMalformedToken},
		{"tampered payload", tamperPayload(t, valid), ErrInvalidSignature},
		{"truncated signature", valid[:len(valid)-4], ErrInvalidSignature},
		{"wrong key same kid", impostor.sign(t, nil, claims(nil)), ErrInvalidSignature},
		{"alg none", unsignedToken(`{"alg":"none"}`, claims(nil), []byte("x")), ErrUnsupportedAlgorithm},
		{"HMAC confusion", hsToken, ErrUnsupportedAlgorithm},
		{"critical header", signer.sign(t, map[string]any{"crit": []string{"exp"}}, claims(nil)), ErrUnsupportedAlgorithm},
		{"unknown kid", signer.sign(t, map[string]any{"kid": "missing"}, claims(nil)), ErrKeyNotFound},
		{"expired", signer.sign(t, nil, claims(func(c map[string]any) {
			c["exp"] = time.Now().Add(-2 * time.Minute).Unix()
		})), ErrTokenExpired},
		{"missing exp", signer.sign(t, nil, claims(func(c map[string]any) { delete(c, "exp") })), ErrMalformedToken},
		{"not before", signer.sign(t, nil, claims(func(c map[string]any) {
			c["nbf"] = time.Now().Add(10 * time.Minute).Unix()
		})), ErrTokenNotYetValid},
		{"issued in the future", signer.sign(t, nil, claims(func(c map[string]any) {
			c["iat"] = time.Now().Add(10 * time.Minute).Unix()
		})), ErrTokenNotYetValid},
		{"issuer", signer.sign(t, nil, claims(func(c map[string]any) { c["iss"] = testIssuer + "/" })), ErrIssuerMismatch},
		{"audience", signer.sign(t, nil, claims(func(c map[string]any) { c["aud"] = "other-client" })), ErrAudienceMismatch},
		{"authorized party", signer.sign(t, nil, claims(func(c map[string]any) {
			c["aud"] = []string{testClientID, "other-client"}
			c["azp"] = "other-client"
		})), ErrAudienceMismatch},
		{"empty subject", signer.sign(t, nil, claims(func(c map[string]any) { c["sub"] = "" })), ErrMalformedToken},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			_, err := verifier.Verify(context.Background(), test.token)
			if !errors.Is(err, test.want) || !errors.Is(err, ErrInvalidToken) {
				t.Fatalf("Verify() error = %v, want %v", err, test.want)
			}
		})
	}

	multi := signer.sign(t, nil, claims(func(c map[string]any) {
		c["aud"] = []string{"other-client", testClientID}
		c["azp"] = testClientID
	}))
	if _, err := verifier.Verify(context.Background(), multi); err != nil {
		t.Fatalf("multi-audience token with matching azp: %v", err)
	}
}

func TestVerifierClockSkewAndAlgorithmAllowList(t *testing.T) {
	signer := newRSASigner(t, "key-1", 0, "RS256")
	now := time.Now()
	token := signer.sign(t, nil, testClaims(testIssuer, testClientID, "s", now.Add(-30*time.Second)))
	lenient := staticVerifier(t, []testSigner{signer}, func(c *VerifierConfig) { c.Now = func() time.Time { return now } })
	if _, err := lenient.Verify(context.Background(), token); err != nil {
		t.Fatalf("token expired within the default skew: %v", err)
	}
	strict := staticVerifier(t, []testSigner{signer}, func(c *VerifierConfig) {
		c.Now = func() time.Time { return now }
		c.ClockSkew = -1
	})
	if _, err := strict.Verify(context.Background(), token); !errors.Is(err, ErrTokenExpired) {
		t.Fatalf("strict verifier error = %v, want ErrTokenExpired", err)
	}

	es := newECSigner(t, "es", elliptic.P256(), "ES256")
	rsOnly := staticVerifier(t, []testSigner{signer, es}, func(c *VerifierConfig) { c.Algorithms = []string{"RS256"} })
	esToken := es.sign(t, nil, testClaims(testIssuer, testClientID, "s", now.Add(time.Hour)))
	if _, err := rsOnly.Verify(context.Background(), esToken); !errors.Is(err, ErrUnsupportedAlgorithm) {
		t.Fatalf("ES256 against RS256-only verifier = %v", err)
	}
	if _, err := NewVerifier(StaticKeySet{}, VerifierConfig{Issuer: testIssuer, Audience: "a", Algorithms: []string{"none", "HS256"}}); !errors.Is(err, ErrNotConfigured) {
		t.Fatalf("NewVerifier with only unsafe algorithms = %v", err)
	}
}

func TestParseJWKSSkipsUnusableKeys(t *testing.T) {
	good := newRSASigner(t, "good", 0, "RS256").jwk()
	private := newRSASigner(t, "private", 0, "RS256").jwk()
	private["d"] = "AQAB"
	encryption := newRSASigner(t, "enc", 0, "RS256").jwk()
	encryption["use"] = "enc"
	weak := map[string]string{"kty": "RSA", "kid": "weak", "n": base64.RawURLEncoding.EncodeToString(make([]byte, 64)), "e": "AQAB"}
	ec := newECSigner(t, "off-curve", elliptic.P256(), "ES256").jwk()
	ec["y"] = ec["x"]
	mismatch := newRSASigner(t, "mismatch", 0, "RS256").jwk()
	mismatch["alg"] = "ES256"
	raw := mustJSON(t, map[string]any{"keys": []map[string]string{private, encryption, weak, ec, mismatch, good}})
	keys, err := ParseJWKS(raw)
	if err != nil {
		t.Fatalf("ParseJWKS: %v", err)
	}
	if len(keys) != 1 || keys[0].KeyID != "good" {
		t.Fatalf("ParseJWKS kept %d keys: %+v", len(keys), keys)
	}
	if _, err := ParseJWKS(mustJSON(t, map[string]any{"keys": []map[string]string{private}})); !errors.Is(err, ErrKeyNotFound) {
		t.Fatalf("ParseJWKS(no usable keys) = %v", err)
	}
}

func TestIDTokenRedactsClaims(t *testing.T) {
	signer := newRSASigner(t, "key-1", 0, "RS256")
	verifier := staticVerifier(t, []testSigner{signer}, nil)
	claims := testClaims(testIssuer, testClientID, "person-identifier", time.Now().Add(time.Hour))
	claims["roles"] = []string{"reader"}
	token, err := verifier.Verify(context.Background(), signer.sign(t, nil, claims))
	if err != nil {
		t.Fatal(err)
	}
	for _, text := range []string{token.String(), token.GoString()} {
		if containsAny(text, "person-identifier", "reader") {
			t.Fatalf("token formatting leaks claims: %s", text)
		}
	}
	var custom struct {
		Roles []string `json:"roles"`
	}
	if err := token.Claims(&custom); err != nil || len(custom.Roles) != 1 {
		t.Fatalf("Claims() = %+v, %v", custom, err)
	}
}

func tamperPayload(t *testing.T, token string) string {
	t.Helper()
	parts := splitToken(t, token)
	payload, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		t.Fatal(err)
	}
	var claims map[string]any
	if err := json.Unmarshal(payload, &claims); err != nil {
		t.Fatal(err)
	}
	claims["sub"] = "attacker"
	return parts[0] + "." + base64.RawURLEncoding.EncodeToString(mustJSON(t, claims)) + "." + parts[2]
}

func splitToken(t *testing.T, token string) []string {
	t.Helper()
	parts := strings.Split(token, ".")
	if len(parts) != 3 {
		t.Fatalf("token has %d segments", len(parts))
	}
	return parts
}

func mustJSON(t *testing.T, value any) []byte {
	t.Helper()
	data, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	return data
}

func containsAny(text string, needles ...string) bool {
	for _, needle := range needles {
		if needle != "" && strings.Contains(text, needle) {
			return true
		}
	}
	return false
}
