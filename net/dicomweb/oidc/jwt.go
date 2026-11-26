package oidc

import (
	"bytes"
	"context"
	"crypto"
	"crypto/ecdsa"
	"crypto/ed25519"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/sha512"
	"crypto/subtle"
	"encoding/base64"
	"encoding/json"
	"errors"
	"hash"
	"math/big"
	"strings"
	"time"
)

const (
	// MaxTokenBytes bounds the compact serialization accepted by Verify.
	MaxTokenBytes = 64 << 10
	// DefaultClockSkew tolerates small clock differences for exp, nbf and iat.
	DefaultClockSkew = time.Minute
)

var allowedAlgorithms = []string{
	"RS256", "RS384", "RS512",
	"PS256", "PS384", "PS512",
	"ES256", "ES384", "ES512",
	"EdDSA",
}

// SupportedAlgorithms filters advertised to the asymmetric JWS algorithms the
// package verifies, preserving order and dropping duplicates. "none" and
// HMAC algorithms are never returned.
func SupportedAlgorithms(advertised []string) []string {
	var out []string
	for _, algorithm := range advertised {
		algorithm = strings.TrimSpace(algorithm)
		if containsExact(allowedAlgorithms, algorithm) && !containsExact(out, algorithm) {
			out = append(out, algorithm)
		}
	}
	return out
}

func containsExact(values []string, want string) bool {
	for _, value := range values {
		if value == want {
			return true
		}
	}
	return false
}

// VerifierConfig selects the claims a Verifier enforces.
type VerifierConfig struct {
	// Issuer is compared literally with the iss claim. Required.
	Issuer string
	// Audience must appear in the aud claim; for ID tokens it is the client
	// ID. When azp is present it must equal Audience. Required unless
	// SkipAudienceCheck is set.
	Audience          string
	SkipAudienceCheck bool
	// Algorithms restricts the accepted JWS algorithms. Empty selects RS256
	// (the OpenID Connect default) unless the provider advertised others.
	Algorithms []string
	// ClockSkew tolerates clock differences. Zero selects DefaultClockSkew;
	// a negative value disables tolerance.
	ClockSkew time.Duration
	// Now overrides the clock for tests.
	Now func() time.Time
}

// Verifier validates signed JWTs from one issuer.
type Verifier struct {
	keys   KeySet
	config VerifierConfig
}

// NewVerifier returns a verifier for tokens signed by keys.
func NewVerifier(keys KeySet, config VerifierConfig) (*Verifier, error) {
	if keys == nil || strings.TrimSpace(config.Issuer) == "" ||
		(!config.SkipAudienceCheck && strings.TrimSpace(config.Audience) == "") {
		return nil, ErrNotConfigured
	}
	if len(config.Algorithms) == 0 {
		config.Algorithms = []string{"RS256"}
	} else {
		config.Algorithms = SupportedAlgorithms(config.Algorithms)
		if len(config.Algorithms) == 0 {
			return nil, ErrNotConfigured
		}
	}
	if config.ClockSkew == 0 {
		config.ClockSkew = DefaultClockSkew
	} else if config.ClockSkew < 0 {
		config.ClockSkew = 0
	}
	if config.Now == nil {
		config.Now = time.Now
	}
	return &Verifier{keys: keys, config: config}, nil
}

// IDToken holds the verified registered claims of a JWT. Claims decodes the
// complete payload, including private claims such as scope or roles.
type IDToken struct {
	Issuer          string
	Subject         string
	Audience        []string
	AuthorizedParty string
	Expiry          time.Time
	IssuedAt        time.Time
	NotBefore       time.Time
	Nonce           string
	AccessTokenHash string
	Algorithm       string
	KeyID           string

	payload []byte
}

// Claims decodes the full verified payload into v.
func (t *IDToken) Claims(v any) error {
	if t == nil {
		return ErrInvalidToken
	}
	return json.Unmarshal(t.payload, v)
}

// String omits the claims, which may identify a person.
func (t *IDToken) String() string {
	if t == nil {
		return "OIDC token (nil)"
	}
	return "OIDC token (alg=" + t.Algorithm + ", claims redacted)"
}

// GoString redacts claims from %#v output.
func (t *IDToken) GoString() string {
	return t.String()
}

// VerifyAccessToken checks the at_hash claim against accessToken (OpenID
// Connect Core 3.1.3.8). A token without at_hash passes.
func (t *IDToken) VerifyAccessToken(accessToken string) error {
	if t == nil {
		return ErrInvalidToken
	}
	if t.AccessTokenHash == "" {
		return nil
	}
	h := hashForAlgorithm(t.Algorithm)
	if h == nil {
		return ErrAccessTokenHashMismatch
	}
	h.Write([]byte(accessToken))
	sum := h.Sum(nil)
	want := base64.RawURLEncoding.EncodeToString(sum[:len(sum)/2])
	if subtle.ConstantTimeCompare([]byte(want), []byte(t.AccessTokenHash)) != 1 {
		return ErrAccessTokenHashMismatch
	}
	return nil
}

func hashForAlgorithm(algorithm string) hash.Hash {
	switch algorithm {
	case "RS256", "PS256", "ES256":
		return sha256.New()
	case "RS384", "PS384", "ES384":
		return sha512.New384()
	case "RS512", "PS512", "ES512", "EdDSA":
		return sha512.New()
	}
	return nil
}

type jwsHeader struct {
	Algorithm string          `json:"alg"`
	KeyID     string          `json:"kid"`
	Type      string          `json:"typ"`
	Critical  json.RawMessage `json:"crit"`
}

type audience []string

func (a *audience) UnmarshalJSON(data []byte) error {
	var single string
	if err := json.Unmarshal(data, &single); err == nil {
		*a = audience{single}
		return nil
	}
	var many []string
	if err := json.Unmarshal(data, &many); err != nil {
		return err
	}
	*a = many
	return nil
}

type numericDate struct {
	set   bool
	value time.Time
}

func (d *numericDate) UnmarshalJSON(data []byte) error {
	var number json.Number
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.UseNumber()
	if err := decoder.Decode(&number); err != nil {
		return err
	}
	seconds, err := number.Float64()
	if err != nil {
		return err
	}
	if seconds < 0 || seconds > 1<<40 {
		return errors.New("numeric date out of range")
	}
	d.set = true
	d.value = time.Unix(int64(seconds), 0).UTC()
	return nil
}

type registeredClaims struct {
	Issuer          string      `json:"iss"`
	Subject         string      `json:"sub"`
	Audience        audience    `json:"aud"`
	AuthorizedParty string      `json:"azp"`
	Expiry          numericDate `json:"exp"`
	IssuedAt        numericDate `json:"iat"`
	NotBefore       numericDate `json:"nbf"`
	Nonce           string      `json:"nonce"`
	AccessTokenHash string      `json:"at_hash"`
}

// Verify checks the signature of a compact JWS and its registered claims:
// iss, aud/azp, exp (required), nbf, iat and a non-empty sub. Embedded key
// headers (jwk, jku, x5u, x5c) are ignored; keys come only from the KeySet.
func (v *Verifier) Verify(ctx context.Context, raw string) (*IDToken, error) {
	if v == nil {
		return nil, ErrNotConfigured
	}
	raw = strings.TrimSpace(raw)
	if raw == "" || len(raw) > MaxTokenBytes {
		return nil, ErrMalformedToken
	}
	parts := strings.Split(raw, ".")
	if len(parts) != 3 {
		return nil, ErrMalformedToken
	}
	headerBytes, err1 := base64.RawURLEncoding.DecodeString(parts[0])
	payload, err2 := base64.RawURLEncoding.DecodeString(parts[1])
	signature, err3 := base64.RawURLEncoding.DecodeString(parts[2])
	if err1 != nil || err2 != nil || err3 != nil || len(signature) == 0 {
		return nil, ErrMalformedToken
	}
	var header jwsHeader
	if decodeStrictJSON(headerBytes, &header) != nil {
		return nil, ErrMalformedToken
	}
	if len(header.Critical) > 0 {
		// No JWS extensions are understood, so any critical one is fatal.
		return nil, ErrUnsupportedAlgorithm
	}
	if !containsExact(v.config.Algorithms, header.Algorithm) {
		return nil, ErrUnsupportedAlgorithm
	}
	keys, err := v.keys.Keys(ctx, header.KeyID)
	if err != nil {
		if ctxErr := contextOrBackground(ctx).Err(); ctxErr != nil {
			return nil, ctxErr
		}
		if errors.Is(err, ErrInvalidToken) {
			return nil, err
		}
		return nil, ErrKeyNotFound
	}
	signingInput := []byte(parts[0] + "." + parts[1])
	verified := false
	compatible := false
	for _, key := range keys {
		if key.Algorithm != "" && key.Algorithm != header.Algorithm {
			continue
		}
		if !keyMatchesAlgorithm(key.Key, header.Algorithm) {
			continue
		}
		compatible = true
		if verifySignature(key.Key, header.Algorithm, signingInput, signature) {
			verified = true
			break
		}
	}
	if !compatible {
		return nil, ErrKeyNotFound
	}
	if !verified {
		return nil, ErrInvalidSignature
	}

	var claims registeredClaims
	if json.Unmarshal(payload, &claims) != nil {
		return nil, ErrMalformedToken
	}
	if claims.Issuer != v.config.Issuer {
		return nil, ErrIssuerMismatch
	}
	if !v.config.SkipAudienceCheck {
		if !containsExact(claims.Audience, v.config.Audience) {
			return nil, ErrAudienceMismatch
		}
		if claims.AuthorizedParty != "" && claims.AuthorizedParty != v.config.Audience {
			return nil, ErrAudienceMismatch
		}
	}
	now := v.config.Now()
	skew := v.config.ClockSkew
	if !claims.Expiry.set {
		return nil, ErrMalformedToken
	}
	if !now.Before(claims.Expiry.value.Add(skew)) {
		return nil, ErrTokenExpired
	}
	if claims.NotBefore.set && now.Add(skew).Before(claims.NotBefore.value) {
		return nil, ErrTokenNotYetValid
	}
	if claims.IssuedAt.set && now.Add(skew).Before(claims.IssuedAt.value) {
		return nil, ErrTokenNotYetValid
	}
	if strings.TrimSpace(claims.Subject) == "" {
		return nil, ErrMalformedToken
	}
	return &IDToken{
		Issuer:          claims.Issuer,
		Subject:         claims.Subject,
		Audience:        append([]string(nil), claims.Audience...),
		AuthorizedParty: claims.AuthorizedParty,
		Expiry:          claims.Expiry.value,
		IssuedAt:        claims.IssuedAt.value,
		NotBefore:       claims.NotBefore.value,
		Nonce:           claims.Nonce,
		AccessTokenHash: claims.AccessTokenHash,
		Algorithm:       header.Algorithm,
		KeyID:           header.KeyID,
		payload:         payload,
	}, nil
}

func keyMatchesAlgorithm(key crypto.PublicKey, algorithm string) bool {
	switch k := key.(type) {
	case *rsa.PublicKey:
		return strings.HasPrefix(algorithm, "RS") || strings.HasPrefix(algorithm, "PS")
	case *ecdsa.PublicKey:
		switch algorithm {
		case "ES256":
			return k.Curve.Params().BitSize == 256
		case "ES384":
			return k.Curve.Params().BitSize == 384
		case "ES512":
			return k.Curve.Params().BitSize == 521
		}
	case ed25519.PublicKey:
		return algorithm == "EdDSA"
	}
	return false
}

func verifySignature(key crypto.PublicKey, algorithm string, input, signature []byte) bool {
	if algorithm == "EdDSA" {
		pub, ok := key.(ed25519.PublicKey)
		return ok && ed25519.Verify(pub, input, signature)
	}
	h := hashForAlgorithm(algorithm)
	if h == nil {
		return false
	}
	h.Write(input)
	digest := h.Sum(nil)
	hashID := map[int]crypto.Hash{32: crypto.SHA256, 48: crypto.SHA384, 64: crypto.SHA512}[len(digest)]
	switch k := key.(type) {
	case *rsa.PublicKey:
		if strings.HasPrefix(algorithm, "PS") {
			return rsa.VerifyPSS(k, hashID, digest, signature, &rsa.PSSOptions{SaltLength: rsa.PSSSaltLengthEqualsHash}) == nil
		}
		return rsa.VerifyPKCS1v15(k, hashID, digest, signature) == nil
	case *ecdsa.PublicKey:
		size := (k.Curve.Params().BitSize + 7) / 8
		if len(signature) != 2*size {
			return false
		}
		r := new(big.Int).SetBytes(signature[:size])
		s := new(big.Int).SetBytes(signature[size:])
		return ecdsa.Verify(k, digest, r, s)
	}
	return false
}
