package testutil

import (
	"context"
	"crypto"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha1"
	"crypto/sha256"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/asn1"
	"errors"
	"fmt"
	"math/big"
	"path/filepath"
	"sync"
	"testing"
	"time"
)

// TestTSA is an in-process RFC 3161 time stamping authority for tests: a
// synthetic root CA and a TSA certificate (critical Extended Key Usage of
// timeStamping only) that answer DER TimeStampReqs with CMS SignedData
// tokens. No network is involved. It holds no real identities.
type TestTSA struct {
	Root        *x509.Certificate
	RootPool    *x509.CertPool
	RootFile    string
	Certificate *x509.Certificate
	CertFile    string
	KeyFile     string
	Key         crypto.Signer

	// Policy is the TSA policy of tokens when the request names none.
	Policy asn1.ObjectIdentifier
	// Now gives genTime; nil uses the current time.
	Now func() time.Time
	// OmitCertificate leaves the TSA certificate out of every token.
	OmitCertificate bool
	// SigningCertificateV1 uses the ESS signing-certificate attribute
	// (SHA-1) instead of signing-certificate-v2.
	SigningCertificateV1 bool
	// Reject answers every request with the rejection status.
	Reject bool

	mu     sync.Mutex
	serial int64
}

// TestTSAPolicy is the default policy of a TestTSA (an example arc).
var TestTSAPolicy = asn1.ObjectIdentifier{1, 3, 6, 1, 4, 1, 32473, 1, 1174}

// TestTSAOptions configure NewTestTSA.
type TestTSAOptions struct {
	// KeyKind is "p256" (default) or "rsa2048".
	KeyKind string
	// NotBefore and NotAfter bound the root and TSA certificates; zero
	// values give an hour ago and ten years from now.
	NotBefore, NotAfter time.Time
	// NonCriticalUsage marks the time stamping Extended Key Usage non
	// critical, which RFC 3161 forbids.
	NonCriticalUsage bool
}

// NewTestTSA creates a TSA with its own synthetic root, writing PEM files
// to a temporary directory.
func NewTestTSA(t testing.TB, opts TestTSAOptions) *TestTSA {
	t.Helper()
	if opts.NotBefore.IsZero() {
		opts.NotBefore = time.Now().Add(-time.Hour)
	}
	if opts.NotAfter.IsZero() {
		opts.NotAfter = time.Now().Add(10 * 365 * 24 * time.Hour)
	}
	dir := t.TempDir()
	rootKey, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	rootTemplate := &x509.Certificate{
		SerialNumber:          big.NewInt(1),
		Subject:               pkix.Name{CommonName: "dicom-go test TSA root", Organization: []string{"dicom-go synthetic test PKI"}},
		NotBefore:             opts.NotBefore,
		NotAfter:              opts.NotAfter,
		IsCA:                  true,
		BasicConstraintsValid: true,
		KeyUsage:              x509.KeyUsageCertSign | x509.KeyUsageCRLSign,
	}
	rootDER, err := x509.CreateCertificate(rand.Reader, rootTemplate, rootTemplate, rootKey.Public(), rootKey)
	if err != nil {
		t.Fatal(err)
	}
	root, err := x509.ParseCertificate(rootDER)
	if err != nil {
		t.Fatal(err)
	}
	var key crypto.Signer
	switch opts.KeyKind {
	case "", "p256":
		key, err = ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	case "rsa2048":
		key, err = rsa.GenerateKey(rand.Reader, 2048)
	default:
		t.Fatalf("unknown key kind %q", opts.KeyKind)
	}
	if err != nil {
		t.Fatal(err)
	}
	usage, err := asn1.Marshal([]asn1.ObjectIdentifier{{1, 3, 6, 1, 5, 5, 7, 3, 8}})
	if err != nil {
		t.Fatal(err)
	}
	template := &x509.Certificate{
		SerialNumber: big.NewInt(2),
		Subject:      pkix.Name{CommonName: "dicom-go test TSA", Organization: []string{"dicom-go synthetic test PKI"}},
		NotBefore:    opts.NotBefore,
		NotAfter:     opts.NotAfter,
		KeyUsage:     x509.KeyUsageDigitalSignature | x509.KeyUsageContentCommitment,
		ExtraExtensions: []pkix.Extension{{
			Id: asn1.ObjectIdentifier{2, 5, 29, 37}, Critical: !opts.NonCriticalUsage, Value: usage,
		}},
	}
	der, err := x509.CreateCertificate(rand.Reader, template, root, key.Public(), rootKey)
	if err != nil {
		t.Fatal(err)
	}
	cert, err := x509.ParseCertificate(der)
	if err != nil {
		t.Fatal(err)
	}
	keyDER, err := x509.MarshalPKCS8PrivateKey(key)
	if err != nil {
		t.Fatal(err)
	}
	tsa := &TestTSA{
		Root: root, RootPool: x509.NewCertPool(), RootFile: filepath.Join(dir, "tsa-root.pem"),
		Certificate: cert, CertFile: filepath.Join(dir, "tsa.pem"), KeyFile: filepath.Join(dir, "tsa.key"),
		Key: key, Policy: TestTSAPolicy,
	}
	tsa.RootPool.AddCert(root)
	writePEM(t, tsa.RootFile, "CERTIFICATE", rootDER)
	writePEM(t, tsa.CertFile, "CERTIFICATE", der)
	writePEM(t, tsa.KeyFile, "PRIVATE KEY", keyDER)
	return tsa
}

type tsaAlgorithm struct {
	Algorithm  asn1.ObjectIdentifier
	Parameters asn1.RawValue `asn1:"optional"`
}

type tsaImprint struct {
	HashAlgorithm tsaAlgorithm
	HashedMessage []byte
}

type tsaRequest struct {
	Version        int
	MessageImprint tsaImprint
	ReqPolicy      asn1.ObjectIdentifier `asn1:"optional"`
	Nonce          *big.Int              `asn1:"optional"`
	CertReq        bool                  `asn1:"optional,default:false"`
	Extensions     asn1.RawValue         `asn1:"optional,tag:0"`
}

type tsaAccuracy struct {
	Millis int `asn1:"optional,tag:0"`
}

type tsaTSTInfo struct {
	Version        int
	Policy         asn1.ObjectIdentifier
	MessageImprint tsaImprint
	SerialNumber   *big.Int
	GenTime        time.Time `asn1:"generalized"`
	Accuracy       tsaAccuracy
	Nonce          *big.Int `asn1:"optional"`
}

type tsaAttribute struct {
	Type   asn1.ObjectIdentifier
	Values []asn1.RawValue `asn1:"set"`
}

type tsaIssuerSerial struct {
	Issuer asn1.RawValue
	Serial *big.Int
}

type tsaSignerInfo struct {
	Version            int
	SID                tsaIssuerSerial
	DigestAlgorithm    tsaAlgorithm
	SignedAttrs        asn1.RawValue
	SignatureAlgorithm tsaAlgorithm
	Signature          []byte
}

type tsaEncapsulated struct {
	EContentType asn1.ObjectIdentifier
	EContent     asn1.RawValue
}

type tsaSignedData struct {
	Version          int
	DigestAlgorithms []tsaAlgorithm `asn1:"set"`
	EncapContentInfo tsaEncapsulated
	Certificates     asn1.RawValue   `asn1:"optional"`
	SignerInfos      []tsaSignerInfo `asn1:"set"`
}

type tsaContentInfo struct {
	ContentType asn1.ObjectIdentifier
	Content     asn1.RawValue
}

type tsaStatus struct {
	Status       int
	StatusString []asn1.RawValue `asn1:"optional"`
}

type tsaResponse struct {
	Status tsaStatus
	Token  asn1.RawValue `asn1:"optional"`
}

var (
	oidTSASignedData = asn1.ObjectIdentifier{1, 2, 840, 113549, 1, 7, 2}
	oidTSATSTInfo    = asn1.ObjectIdentifier{1, 2, 840, 113549, 1, 9, 16, 1, 4}
	oidTSASHA256     = asn1.ObjectIdentifier{2, 16, 840, 1, 101, 3, 4, 2, 1}
)

// Timestamp answers a DER TimeStampReq; it satisfies dsig.Timestamper.
func (a *TestTSA) Timestamp(_ context.Context, request []byte) ([]byte, error) {
	return a.Respond(request)
}

// Respond answers a DER TimeStampReq with a DER TimeStampResp.
func (a *TestTSA) Respond(request []byte) ([]byte, error) {
	if a.Reject {
		return asn1.Marshal(tsaResponse{Status: tsaStatus{Status: 2, StatusString: []asn1.RawValue{{Tag: asn1.TagUTF8String, Bytes: []byte("test TSA rejects every request")}}}})
	}
	var req tsaRequest
	if rest, err := asn1.Unmarshal(request, &req); err != nil || len(rest) != 0 || req.Version != 1 {
		return nil, errors.New("test TSA: bad request")
	}
	token, err := a.Token(req.MessageImprint.HashAlgorithm.Algorithm, req.MessageImprint.HashedMessage, req.Nonce, req.ReqPolicy, req.CertReq)
	if err != nil {
		return nil, err
	}
	return asn1.Marshal(tsaResponse{Status: tsaStatus{Status: 0}, Token: asn1.RawValue{FullBytes: token}})
}

// Token builds a DER TimeStampToken for an imprint.
func (a *TestTSA) Token(hashOID asn1.ObjectIdentifier, imprint []byte, nonce *big.Int, policy asn1.ObjectIdentifier, includeCertificate bool) ([]byte, error) {
	a.mu.Lock()
	a.serial++
	serial := a.serial
	a.mu.Unlock()
	now := time.Now()
	if a.Now != nil {
		now = a.Now()
	}
	if len(policy) == 0 {
		policy = a.Policy
	}
	content, err := asn1.Marshal(tsaTSTInfo{
		Version:        1,
		Policy:         policy,
		MessageImprint: tsaImprint{HashAlgorithm: tsaAlgorithm{Algorithm: hashOID, Parameters: asn1.NullRawValue}, HashedMessage: imprint},
		SerialNumber:   big.NewInt(serial),
		GenTime:        now.UTC().Truncate(time.Millisecond),
		Accuracy:       tsaAccuracy{Millis: 500},
		Nonce:          nonce,
	})
	if err != nil {
		return nil, err
	}
	contentDigest := sha256.Sum256(content)
	contentType, _ := asn1.Marshal(oidTSATSTInfo)
	messageDigest, _ := asn1.Marshal(contentDigest[:])
	certHash := sha256.Sum256(a.Certificate.Raw)
	signingCertType := asn1.ObjectIdentifier{1, 2, 840, 113549, 1, 9, 16, 2, 47}
	signingCert, _ := asn1.Marshal(struct{ Certs []struct{ CertHash []byte } }{Certs: []struct{ CertHash []byte }{{CertHash: certHash[:]}}})
	if a.SigningCertificateV1 {
		signingCertType = asn1.ObjectIdentifier{1, 2, 840, 113549, 1, 9, 16, 2, 12}
		sum := sha1.Sum(a.Certificate.Raw)
		signingCert, _ = asn1.Marshal(struct{ Certs []struct{ CertHash []byte } }{Certs: []struct{ CertHash []byte }{{CertHash: sum[:]}}})
	}
	attributes := []tsaAttribute{
		{Type: asn1.ObjectIdentifier{1, 2, 840, 113549, 1, 9, 3}, Values: []asn1.RawValue{{FullBytes: contentType}}},
		{Type: asn1.ObjectIdentifier{1, 2, 840, 113549, 1, 9, 4}, Values: []asn1.RawValue{{FullBytes: messageDigest}}},
		{Type: signingCertType, Values: []asn1.RawValue{{FullBytes: signingCert}}},
	}
	set, err := asn1.MarshalWithParams(attributes, "set")
	if err != nil {
		return nil, err
	}
	attributesDigest := sha256.Sum256(set)
	var signatureAlgorithm asn1.ObjectIdentifier
	switch a.Key.Public().(type) {
	case *ecdsa.PublicKey:
		signatureAlgorithm = asn1.ObjectIdentifier{1, 2, 840, 10045, 4, 3, 2}
	case *rsa.PublicKey:
		signatureAlgorithm = asn1.ObjectIdentifier{1, 2, 840, 113549, 1, 1, 11}
	default:
		return nil, fmt.Errorf("test TSA: %T key", a.Key.Public())
	}
	signature, err := a.Key.Sign(rand.Reader, attributesDigest[:], crypto.SHA256)
	if err != nil {
		return nil, err
	}
	octetString, err := asn1.Marshal(content)
	if err != nil {
		return nil, err
	}
	signedAttrs := append([]byte(nil), set...)
	signedAttrs[0] = 0xA0 // [0] IMPLICIT in the SignerInfo
	sd := tsaSignedData{
		Version:          3,
		DigestAlgorithms: []tsaAlgorithm{{Algorithm: oidTSASHA256, Parameters: asn1.NullRawValue}},
		EncapContentInfo: tsaEncapsulated{EContentType: oidTSATSTInfo, EContent: explicitZero(octetString)},
		SignerInfos: []tsaSignerInfo{{
			Version:            1,
			SID:                tsaIssuerSerial{Issuer: asn1.RawValue{FullBytes: a.Certificate.RawIssuer}, Serial: a.Certificate.SerialNumber},
			DigestAlgorithm:    tsaAlgorithm{Algorithm: oidTSASHA256, Parameters: asn1.NullRawValue},
			SignedAttrs:        asn1.RawValue{FullBytes: signedAttrs},
			SignatureAlgorithm: tsaAlgorithm{Algorithm: signatureAlgorithm},
			Signature:          signature,
		}},
	}
	if includeCertificate && !a.OmitCertificate {
		sd.Certificates = asn1.RawValue{Class: asn1.ClassContextSpecific, Tag: 0, IsCompound: true, Bytes: a.Certificate.Raw}
	}
	sdDER, err := asn1.Marshal(sd)
	if err != nil {
		return nil, err
	}
	return asn1.Marshal(tsaContentInfo{ContentType: oidTSASignedData, Content: explicitZero(sdDER)})
}

// explicitZero wraps DER in a [0] EXPLICIT tag (encoding/asn1 ignores tag
// parameters on RawValue fields).
func explicitZero(der []byte) asn1.RawValue {
	return asn1.RawValue{Class: asn1.ClassContextSpecific, Tag: 0, IsCompound: true, Bytes: der}
}
