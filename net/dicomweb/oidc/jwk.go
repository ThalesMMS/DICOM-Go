package oidc

import (
	"context"
	"crypto"
	"crypto/ecdh"
	"crypto/ecdsa"
	"crypto/ed25519"
	"crypto/elliptic"
	"crypto/rsa"
	"encoding/base64"
	"encoding/json"
	"errors"
	"math/big"
	"net/http"
	"strings"
	"sync"
	"time"
)

const (
	// MinRSAKeyBits is the smallest RSA modulus accepted from a key set.
	MinRSAKeyBits = 2048
	// MaxJWKSKeys bounds the number of keys retained from one key set.
	MaxJWKSKeys = 100

	defaultKeySetMaxAge         = time.Hour
	defaultKeySetMinRefreshWait = 5 * time.Second
)

// JSONWebKey is one verified public signing key of a JWK Set (RFC 7517).
type JSONWebKey struct {
	KeyID     string
	Algorithm string
	Use       string
	Key       crypto.PublicKey
}

type rawJWK struct {
	KeyType   string   `json:"kty"`
	KeyID     string   `json:"kid"`
	Algorithm string   `json:"alg"`
	Use       string   `json:"use"`
	KeyOps    []string `json:"key_ops"`
	Curve     string   `json:"crv"`
	N         string   `json:"n"`
	E         string   `json:"e"`
	X         string   `json:"x"`
	Y         string   `json:"y"`
	D         string   `json:"d"`
}

// ParseJWKS decodes a JWK Set document and keeps the public signature keys it
// can use. Keys with "use" other than "sig", private parameters, weak RSA
// moduli, points off their curve or unknown types are skipped; a set without
// any usable key is an error.
func ParseJWKS(data []byte) ([]JSONWebKey, error) {
	var document struct {
		Keys []json.RawMessage `json:"keys"`
	}
	if err := decodeStrictJSON(data, &document); err != nil {
		return nil, ErrKeyNotFound
	}
	keys := make([]JSONWebKey, 0, len(document.Keys))
	for _, raw := range document.Keys {
		if len(keys) >= MaxJWKSKeys {
			break
		}
		var jwk rawJWK
		if json.Unmarshal(raw, &jwk) != nil {
			continue
		}
		key, err := parseJWK(jwk)
		if err != nil {
			continue
		}
		keys = append(keys, key)
	}
	if len(keys) == 0 {
		return nil, ErrKeyNotFound
	}
	return keys, nil
}

func parseJWK(jwk rawJWK) (JSONWebKey, error) {
	if jwk.D != "" {
		return JSONWebKey{}, errors.New("private key material")
	}
	if jwk.Use != "" && jwk.Use != "sig" {
		return JSONWebKey{}, errors.New("not a signature key")
	}
	if len(jwk.KeyOps) > 0 && !containsFold(jwk.KeyOps, "verify") {
		return JSONWebKey{}, errors.New("not a verification key")
	}
	out := JSONWebKey{KeyID: jwk.KeyID, Algorithm: jwk.Algorithm, Use: jwk.Use}
	switch jwk.KeyType {
	case "RSA":
		n, err := decodeBigInt(jwk.N)
		if err != nil {
			return JSONWebKey{}, err
		}
		e, err := decodeBigInt(jwk.E)
		if err != nil || !e.IsInt64() || e.Int64() < 3 || e.Int64() > 1<<31-1 || e.Bit(0) == 0 {
			return JSONWebKey{}, errors.New("invalid RSA exponent")
		}
		if n.BitLen() < MinRSAKeyBits {
			return JSONWebKey{}, errors.New("weak RSA key")
		}
		out.Key = &rsa.PublicKey{N: n, E: int(e.Int64())}
	case "EC":
		curve, ecdhCurve, size := curveFor(jwk.Curve)
		if curve == nil {
			return JSONWebKey{}, errors.New("unsupported curve")
		}
		x, errX := decodeFixed(jwk.X, size)
		y, errY := decodeFixed(jwk.Y, size)
		if errX != nil || errY != nil {
			return JSONWebKey{}, errors.New("invalid EC point")
		}
		point := append(append([]byte{4}, x...), y...)
		if _, err := ecdhCurve.NewPublicKey(point); err != nil {
			return JSONWebKey{}, errors.New("EC point not on curve")
		}
		out.Key = &ecdsa.PublicKey{Curve: curve, X: new(big.Int).SetBytes(x), Y: new(big.Int).SetBytes(y)}
	case "OKP":
		if jwk.Curve != "Ed25519" {
			return JSONWebKey{}, errors.New("unsupported curve")
		}
		x, err := decodeFixed(jwk.X, ed25519.PublicKeySize)
		if err != nil {
			return JSONWebKey{}, err
		}
		out.Key = ed25519.PublicKey(x)
	default:
		return JSONWebKey{}, errors.New("unsupported key type")
	}
	if out.Algorithm != "" && !keyMatchesAlgorithm(out.Key, out.Algorithm) {
		return JSONWebKey{}, errors.New("key algorithm mismatch")
	}
	return out, nil
}

func curveFor(name string) (elliptic.Curve, ecdh.Curve, int) {
	switch name {
	case "P-256":
		return elliptic.P256(), ecdh.P256(), 32
	case "P-384":
		return elliptic.P384(), ecdh.P384(), 48
	case "P-521":
		return elliptic.P521(), ecdh.P521(), 66
	}
	return nil, nil, 0
}

func decodeBigInt(value string) (*big.Int, error) {
	data, err := base64.RawURLEncoding.DecodeString(value)
	if err != nil || len(data) == 0 {
		return nil, errors.New("invalid integer")
	}
	return new(big.Int).SetBytes(data), nil
}

func decodeFixed(value string, size int) ([]byte, error) {
	data, err := base64.RawURLEncoding.DecodeString(value)
	if err != nil || len(data) != size {
		return nil, errors.New("invalid coordinate")
	}
	return data, nil
}

// KeySet resolves the candidate verification keys of a token.
type KeySet interface {
	// Keys returns the keys whose key ID equals keyID, or every key when
	// keyID is empty. A remote set may refresh itself when nothing matches.
	Keys(ctx context.Context, keyID string) ([]JSONWebKey, error)
}

// StaticKeySet is a fixed in-memory key set.
type StaticKeySet []JSONWebKey

// Keys implements KeySet.
func (s StaticKeySet) Keys(_ context.Context, keyID string) ([]JSONWebKey, error) {
	matches := matchingKeys(s, keyID)
	if len(matches) == 0 {
		return nil, ErrKeyNotFound
	}
	return matches, nil
}

func matchingKeys(keys []JSONWebKey, keyID string) []JSONWebKey {
	if keyID == "" {
		return append([]JSONWebKey(nil), keys...)
	}
	var out []JSONWebKey
	for _, key := range keys {
		if key.KeyID == keyID {
			out = append(out, key)
		}
	}
	return out
}

// RemoteKeySetOptions tunes RemoteKeySet caching.
type RemoteKeySetOptions struct {
	// MaxAge forces a refetch of a cached set older than this. Zero selects
	// one hour.
	MaxAge time.Duration
	// MinRefreshInterval limits refetches triggered by unknown key IDs, so
	// tokens with random key IDs cannot turn into a request flood. Zero
	// selects five seconds; a negative value disables the limit.
	MinRefreshInterval time.Duration
	// Now overrides the clock for tests.
	Now func() time.Time
}

// RemoteKeySet fetches a provider JWK Set over HTTPS, caches it, and refetches
// it when a token names an unknown key ID (key rotation). Concurrent refetches
// are coalesced.
type RemoteKeySet struct {
	uri     string
	client  *http.Client
	options RemoteKeySetOptions

	mu        sync.Mutex
	keys      []JSONWebKey
	fetchedAt time.Time
	inFlight  *keySetFetch
}

type keySetFetch struct {
	done chan struct{}
	keys []JSONWebKey
	err  error
}

// NewRemoteKeySet returns a key set for an HTTPS jwks_uri. The client is
// wrapped by SecureHTTPClient.
func NewRemoteKeySet(client *http.Client, jwksURI string, options RemoteKeySetOptions) (*RemoteKeySet, error) {
	if ValidateSecureEndpoint(jwksURI) != nil {
		return nil, ErrNotConfigured
	}
	if options.MaxAge <= 0 {
		options.MaxAge = defaultKeySetMaxAge
	}
	if options.MinRefreshInterval == 0 {
		options.MinRefreshInterval = defaultKeySetMinRefreshWait
	}
	if options.Now == nil {
		options.Now = time.Now
	}
	return &RemoteKeySet{uri: strings.TrimSpace(jwksURI), client: SecureHTTPClient(client), options: options}, nil
}

// Keys implements KeySet.
func (s *RemoteKeySet) Keys(ctx context.Context, keyID string) ([]JSONWebKey, error) {
	s.mu.Lock()
	now := s.options.Now()
	fresh := !s.fetchedAt.IsZero() && now.Sub(s.fetchedAt) < s.options.MaxAge
	if fresh {
		if matches := matchingKeys(s.keys, keyID); len(matches) > 0 {
			s.mu.Unlock()
			return matches, nil
		}
		// Unknown key ID: refetch unless the last fetch is too recent.
		if s.options.MinRefreshInterval > 0 && now.Sub(s.fetchedAt) < s.options.MinRefreshInterval {
			s.mu.Unlock()
			return nil, ErrKeyNotFound
		}
	}
	call := s.inFlight
	if call == nil {
		call = &keySetFetch{done: make(chan struct{})}
		s.inFlight = call
		go s.fetch(call)
	}
	s.mu.Unlock()

	select {
	case <-contextOrBackground(ctx).Done():
		return nil, ctx.Err()
	case <-call.done:
	}
	if call.err != nil {
		return nil, call.err
	}
	matches := matchingKeys(call.keys, keyID)
	if len(matches) == 0 {
		return nil, ErrKeyNotFound
	}
	return matches, nil
}

func (s *RemoteKeySet) fetch(call *keySetFetch) {
	ctx, cancel := context.WithTimeout(context.Background(), DefaultHTTPTimeout)
	defer cancel()
	keys, err := s.download(ctx)

	s.mu.Lock()
	if err == nil {
		s.keys = keys
		s.fetchedAt = s.options.Now()
	}
	call.keys, call.err = keys, err
	s.inFlight = nil
	s.mu.Unlock()
	close(call.done)
}

func (s *RemoteKeySet) download(ctx context.Context) ([]JSONWebKey, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, s.uri, nil)
	if err != nil {
		return nil, ErrKeyNotFound
	}
	req.Header.Set("Accept", "application/jwk-set+json, application/json")
	response, err := s.client.Do(req)
	if err != nil {
		return nil, ErrKeyNotFound
	}
	defer response.Body.Close()
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		discardBounded(response.Body)
		return nil, ErrKeyNotFound
	}
	data, err := readBounded(response.Body)
	if err != nil {
		return nil, ErrKeyNotFound
	}
	return ParseJWKS(data)
}
