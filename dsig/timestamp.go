package dsig

import (
	"bytes"
	"context"
	"crypto"
	"crypto/ecdsa"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/asn1"
	"errors"
	"fmt"
	"io"
	"math/big"
	"net/http"
	"strings"
	"time"

	"github.com/ThalesMMS/dicom-go/internal/sourceport"
)

// CertifiedTimestampCMSTSP is the Certified Timestamp Type (0400,0305) of an
// RFC 3161 time stamp token (PS3.3 C.12.1.1.3): the Certified Timestamp
// (0400,0310) holds the DER TimeStampToken, a CMS SignedData ContentInfo.
const CertifiedTimestampCMSTSP = "CMS_TSP"

var (
	// ErrNoTimestamp reports a signature without a Certified Timestamp.
	ErrNoTimestamp = errors.New("dicom dsig: no certified timestamp")
	// ErrTimestampInvalid reports a time stamp token that cannot be read,
	// whose CMS signature does not verify, that does not certify this
	// signature's value, or whose TSA certificate is not a time stamping
	// certificate.
	ErrTimestampInvalid = errors.New("dicom dsig: certified timestamp invalid")
	// ErrTimestampUntrusted reports a time stamp token whose TSA
	// certificate does not chain to TimestampOptions.Roots.
	ErrTimestampUntrusted = errors.New("dicom dsig: time stamping authority not trusted")
	// ErrTimestampResponse reports a time stamp response that refuses the
	// request or does not answer it (status, nonce, imprint, policy).
	ErrTimestampResponse = errors.New("dicom dsig: time stamp response rejected")
)

// TimestampOptions configure VerifyTimestamp.
type TimestampOptions struct {
	// Roots are the trust anchors of time stamping authorities. Nil checks
	// the token and its signature but not whether the TSA is trusted:
	// TimestampInfo.Trusted stays false.
	Roots *x509.CertPool
	// Intermediates may complete a chain from the TSA certificate to a
	// root.
	Intermediates *x509.CertPool
	// Certificates are candidate TSA certificates for tokens issued
	// without one (certReq false).
	Certificates []*x509.Certificate
	// Time is when the TSA chain is verified; zero uses the current time.
	Time time.Time
}

// TimestampInfo describes a verified time stamp token.
type TimestampInfo struct {
	// Time is the genTime the TSA certified.
	Time time.Time
	// Accuracy is the TSA's stated accuracy around Time (zero when absent).
	Accuracy     time.Duration
	Policy       asn1.ObjectIdentifier
	SerialNumber *big.Int
	// Nonce echoes the request nonce (nil when the request had none).
	Nonce *big.Int
	// HashAlgorithm hashed the Digital Signature value in the imprint.
	HashAlgorithm crypto.Hash
	// Certificate is the TSA certificate that signed the token, and
	// CertificateIncluded reports that the token carried it.
	Certificate         *x509.Certificate
	CertificateIncluded bool
	// Trusted reports that the TSA certificate chains to
	// TimestampOptions.Roots; Chains are the accepted chains, TSA first.
	Trusted bool
	Chains  [][]*x509.Certificate
}

type messageImprint struct {
	HashAlgorithm pkix.AlgorithmIdentifier
	HashedMessage []byte
}

type timeStampReq struct {
	Version        int
	MessageImprint messageImprint
	ReqPolicy      asn1.ObjectIdentifier `asn1:"optional"`
	Nonce          *big.Int              `asn1:"optional"`
	CertReq        bool                  `asn1:"optional,default:false"`
	Extensions     []pkix.Extension      `asn1:"optional,tag:0"`
}

type pkiStatusInfo struct {
	Status       int
	StatusString []asn1.RawValue `asn1:"optional"`
	FailInfo     asn1.BitString  `asn1:"optional"`
}

type timeStampResp struct {
	Status         pkiStatusInfo
	TimeStampToken asn1.RawValue `asn1:"optional"`
}

type tsAccuracy struct {
	Seconds int `asn1:"optional"`
	Millis  int `asn1:"optional,tag:0"`
	Micros  int `asn1:"optional,tag:1"`
}

type tstInfo struct {
	Version        int
	Policy         asn1.ObjectIdentifier
	MessageImprint messageImprint
	SerialNumber   *big.Int
	GenTime        asn1.RawValue
	Accuracy       tsAccuracy    `asn1:"optional"`
	Ordering       bool          `asn1:"optional,default:false"`
	Nonce          *big.Int      `asn1:"optional"`
	TSA            asn1.RawValue `asn1:"optional,explicit,tag:0"`
	Extensions     asn1.RawValue `asn1:"optional,tag:1"`
}

// timestampedValue returns the bytes a certified timestamp covers: the
// Digital Signature (0400,0120) value without the OB padding byte an odd
// length DER ECDSA signature gets (PS3.3 C.12.1.1.3; DCMTK does the same).
func timestampedValue(sig Signature) []byte {
	value := sig.Signature
	if sig.Certificate == nil {
		return value
	}
	if _, ok := sig.Certificate.PublicKey.(*ecdsa.PublicKey); ok {
		var outer asn1.RawValue
		if _, err := asn1.Unmarshal(value, &outer); err == nil {
			return outer.FullBytes
		}
	}
	return value
}

// TimestampRequestOptions configure NewTimestampRequest.
type TimestampRequestOptions struct {
	// Hash of the imprint; zero uses SHA-256. SHA-1, SHA-256, SHA-384 and
	// SHA-512 are accepted.
	Hash crypto.Hash
	// Policy, when set, asks the TSA for that policy.
	Policy asn1.ObjectIdentifier
	// NoNonce omits the random nonce that ties the response to the request.
	NoNonce bool
	// NoCertificate does not ask the TSA to include its certificate; the
	// verifier then needs it in TimestampOptions.Certificates.
	NoCertificate bool
	// Rand defaults to crypto/rand.
	Rand io.Reader
}

// TimestampRequest is an RFC 3161 TimeStampReq for one signature.
type TimestampRequest struct {
	// DER is the encoded request (a .tsq file, or the body of an HTTP
	// request with Content-Type application/timestamp-query).
	DER     []byte
	Hash    crypto.Hash
	Imprint []byte
	Nonce   *big.Int
	Policy  asn1.ObjectIdentifier
	CertReq bool
}

// NewTimestampRequest builds the time stamp request for sig: the imprint is
// the hash of its Digital Signature value.
func NewTimestampRequest(sig Signature, opts TimestampRequestOptions) (*TimestampRequest, error) {
	if len(sig.Signature) == 0 {
		return nil, fmt.Errorf("%w: empty Signature", ErrMalformedSignature)
	}
	if opts.Hash == 0 {
		opts.Hash = crypto.SHA256
	}
	oid, ok := oidForHash(opts.Hash)
	if !ok || !opts.Hash.Available() {
		return nil, fmt.Errorf("%w: timestamp hash %v", ErrUnsupported, opts.Hash)
	}
	if opts.Rand == nil {
		opts.Rand = rand.Reader
	}
	req := &TimestampRequest{
		Hash:    opts.Hash,
		Imprint: digest(opts.Hash, timestampedValue(sig)),
		Policy:  opts.Policy,
		CertReq: !opts.NoCertificate,
	}
	if !opts.NoNonce {
		nonce, err := rand.Int(opts.Rand, new(big.Int).Lsh(big.NewInt(1), 64))
		if err != nil {
			return nil, fmt.Errorf("dicom dsig: timestamp nonce: %w", err)
		}
		req.Nonce = nonce
	}
	der, err := asn1.Marshal(timeStampReq{
		Version: 1,
		MessageImprint: messageImprint{
			HashAlgorithm: pkix.AlgorithmIdentifier{Algorithm: oid, Parameters: asn1.NullRawValue},
			HashedMessage: req.Imprint,
		},
		ReqPolicy: opts.Policy,
		Nonce:     req.Nonce,
		CertReq:   req.CertReq,
	})
	if err != nil {
		return nil, fmt.Errorf("dicom dsig: encode timestamp request: %w", err)
	}
	req.DER = der
	return req, nil
}

// ParseTimestampRequest reads a DER TimeStampReq, such as a .tsq file
// written earlier, so that the response to it can be checked.
func ParseTimestampRequest(der []byte) (*TimestampRequest, error) {
	var decoded timeStampReq
	if err := unmarshalDER(der, &decoded); err != nil {
		return nil, fmt.Errorf("dicom dsig: unreadable TimeStampReq: %w", err)
	}
	if decoded.Version != 1 {
		return nil, fmt.Errorf("dicom dsig: TimeStampReq version %d", decoded.Version)
	}
	hash, ok := hashForOID(decoded.MessageImprint.HashAlgorithm.Algorithm)
	if !ok {
		return nil, fmt.Errorf("%w: imprint hash %s", ErrUnsupported, decoded.MessageImprint.HashAlgorithm.Algorithm)
	}
	return &TimestampRequest{
		DER:     append([]byte(nil), der...),
		Hash:    hash,
		Imprint: decoded.MessageImprint.HashedMessage,
		Nonce:   decoded.Nonce,
		Policy:  decoded.ReqPolicy,
		CertReq: decoded.CertReq,
	}, nil
}

// ForSignature reports whether the request's imprint is the hash of sig's
// Digital Signature value.
func (req *TimestampRequest) ForSignature(sig Signature) bool {
	return req.Hash.Available() && bytes.Equal(req.Imprint, digest(req.Hash, timestampedValue(sig)))
}

// pkiStatusText names the PKIStatus values of RFC 3161.
func pkiStatusText(status int) string {
	switch status {
	case 0:
		return "granted"
	case 1:
		return "grantedWithMods"
	case 2:
		return "rejection"
	case 3:
		return "waiting"
	case 4:
		return "revocationWarning"
	case 5:
		return "revocationNotification"
	default:
		return fmt.Sprintf("status %d", status)
	}
}

// TokenFromResponse returns the time stamp token of a DER TimeStampResp (a
// .tsr file or an application/timestamp-reply body). A status other than
// granted or grantedWithMods fails with ErrTimestampResponse.
func TokenFromResponse(response []byte) ([]byte, error) {
	var resp timeStampResp
	if err := unmarshalDER(response, &resp); err != nil {
		return nil, fmt.Errorf("%w: unreadable TimeStampResp: %w", ErrTimestampResponse, err)
	}
	if status := resp.Status.Status; status != 0 && status != 1 {
		var texts []string
		for _, value := range resp.Status.StatusString {
			texts = append(texts, strings.ToValidUTF8(string(value.Bytes), "?"))
		}
		text := strings.Join(texts, "; ")
		if text != "" {
			text = ": " + text
		}
		return nil, fmt.Errorf("%w: %s%s", ErrTimestampResponse, pkiStatusText(status), text)
	}
	if len(resp.TimeStampToken.FullBytes) == 0 {
		return nil, fmt.Errorf("%w: granted without a token", ErrTimestampResponse)
	}
	return append([]byte(nil), resp.TimeStampToken.FullBytes...), nil
}

// Check verifies that token answers req: its signature verifies (with the
// embedded TSA certificate or one of certificates) and it certifies the
// request's imprint, nonce and policy, and holds the TSA certificate when the
// request asked for it. It does not check whether the TSA is trusted.
func (req *TimestampRequest) Check(token []byte, certificates ...*x509.Certificate) (TimestampInfo, error) {
	parsed, info, err := parseTimestampToken(token, certificates)
	if err != nil {
		return TimestampInfo{}, fmt.Errorf("%w: %w", ErrTimestampInvalid, err)
	}
	switch {
	case info.HashAlgorithm != req.Hash || !bytes.Equal(parsed.MessageImprint.HashedMessage, req.Imprint):
		return info, fmt.Errorf("%w: the token certifies another imprint", ErrTimestampResponse)
	case (req.Nonce == nil) != (info.Nonce == nil), req.Nonce != nil && req.Nonce.Cmp(info.Nonce) != 0:
		return info, fmt.Errorf("%w: nonce does not match the request", ErrTimestampResponse)
	case len(req.Policy) > 0 && !req.Policy.Equal(info.Policy):
		return info, fmt.Errorf("%w: policy %s, requested %s", ErrTimestampResponse, info.Policy, req.Policy)
	}
	if req.CertReq && !info.CertificateIncluded {
		return info, fmt.Errorf("%w: the TSA certificate was requested but not included", ErrTimestampResponse)
	}
	return info, nil
}

// parseTimestampToken verifies the CMS signature of token and reads its
// TSTInfo; extra are candidate signer certificates.
func parseTimestampToken(token []byte, extra []*x509.Certificate) (tstInfo, TimestampInfo, error) {
	signed, err := parseSignedToken(token, oidTSTInfo, extra)
	if err != nil {
		return tstInfo{}, TimestampInfo{}, err
	}
	var tst tstInfo
	if err := unmarshalDER(signed.content, &tst); err != nil {
		return tstInfo{}, TimestampInfo{}, fmt.Errorf("TSTInfo: %w", err)
	}
	if tst.Version != 1 {
		return tstInfo{}, TimestampInfo{}, fmt.Errorf("TSTInfo version %d", tst.Version)
	}
	hash, ok := hashForOID(tst.MessageImprint.HashAlgorithm.Algorithm)
	if !ok {
		return tstInfo{}, TimestampInfo{}, fmt.Errorf("%w: imprint hash %s", ErrUnsupported, tst.MessageImprint.HashAlgorithm.Algorithm)
	}
	genTime, err := parseGeneralizedTime(tst.GenTime)
	if err != nil {
		return tstInfo{}, TimestampInfo{}, err
	}
	if !hasCriticalTimeStampingUsage(signed.signer) {
		return tstInfo{}, TimestampInfo{}, errors.New("TSA certificate lacks a critical Extended Key Usage of timeStamping only (RFC 3161 2.3)")
	}
	info := TimestampInfo{
		Time:                genTime,
		Accuracy:            time.Duration(tst.Accuracy.Seconds)*time.Second + time.Duration(tst.Accuracy.Millis)*time.Millisecond + time.Duration(tst.Accuracy.Micros)*time.Microsecond,
		Policy:              tst.Policy,
		SerialNumber:        tst.SerialNumber,
		Nonce:               tst.Nonce,
		HashAlgorithm:       hash,
		Certificate:         signed.signer,
		CertificateIncluded: signed.embedded,
	}
	return tst, info, nil
}

// parseGeneralizedTime reads a GeneralizedTime in UTC ("Z"), with optional
// fractional seconds, as RFC 3161 2.4.2 requires.
func parseGeneralizedTime(value asn1.RawValue) (time.Time, error) {
	if value.Class != asn1.ClassUniversal || value.Tag != asn1.TagGeneralizedTime {
		return time.Time{}, errors.New("genTime is not a GeneralizedTime")
	}
	text := string(value.Bytes)
	parsed, err := time.Parse("20060102150405.999999999Z", text)
	if err != nil {
		return time.Time{}, fmt.Errorf("genTime %q: %w", text, err)
	}
	return parsed, nil
}

// VerifyTimestamp checks the Certified Timestamp of sig: an RFC 3161 token
// (CMS_TSP) whose CMS signature verifies with a TSA certificate carrying the
// time stamping usage, that certifies the hash of the Digital Signature
// value, at a time within the validity of the Certificate of Signer. With
// opts.Roots it also verifies the TSA chain (ErrTimestampUntrusted when it
// fails; the info is returned all the same). It does not verify the DICOM
// signature itself (VerifySignature does).
func VerifyTimestamp(sig Signature, opts TimestampOptions) (TimestampInfo, error) {
	if len(sig.CertifiedTimestamp) == 0 {
		return TimestampInfo{}, ErrNoTimestamp
	}
	if sig.CertifiedTimestampType != CertifiedTimestampCMSTSP {
		return TimestampInfo{}, fmt.Errorf("%w: Certified Timestamp Type %q", ErrUnsupported, sig.CertifiedTimestampType)
	}
	// The OB value may carry a padding byte after the DER token.
	token := sig.CertifiedTimestamp
	var outer asn1.RawValue
	if _, err := asn1.Unmarshal(token, &outer); err == nil {
		token = outer.FullBytes
	}
	tst, info, err := parseTimestampToken(token, opts.Certificates)
	if err != nil {
		return TimestampInfo{}, fmt.Errorf("%w: %w", ErrTimestampInvalid, err)
	}
	if !bytes.Equal(tst.MessageImprint.HashedMessage, digest(info.HashAlgorithm, timestampedValue(sig))) {
		return info, fmt.Errorf("%w: the token certifies another Signature value", ErrTimestampInvalid)
	}
	if cert := sig.Certificate; cert != nil && (info.Time.Before(cert.NotBefore) || info.Time.After(cert.NotAfter)) {
		return info, fmt.Errorf("%w: certified time %s outside the signer certificate validity", ErrTimestampInvalid, info.Time.UTC().Format(time.RFC3339))
	}
	if opts.Roots == nil {
		return info, nil
	}
	at := opts.Time
	if at.IsZero() {
		at = time.Now()
	}
	chains, err := info.Certificate.Verify(x509.VerifyOptions{
		Roots:         opts.Roots,
		Intermediates: opts.Intermediates,
		CurrentTime:   at,
		KeyUsages:     []x509.ExtKeyUsage{x509.ExtKeyUsageTimeStamping},
	})
	if err != nil {
		return info, fmt.Errorf("%w: %w", ErrTimestampUntrusted, err)
	}
	info.Trusted, info.Chains = true, chains
	return info, nil
}

// Timestamper obtains a time stamp response for a DER TimeStampReq.
type Timestamper interface {
	Timestamp(ctx context.Context, request []byte) (response []byte, err error)
}

// TimestamperFunc adapts a function, such as an in-process TSA, to
// Timestamper.
type TimestamperFunc func(ctx context.Context, request []byte) ([]byte, error)

// Timestamp calls f.
func (f TimestamperFunc) Timestamp(ctx context.Context, request []byte) ([]byte, error) {
	return f(ctx, request)
}

// MaxTimestampResponseSize bounds the time stamp response HTTPTimestamper
// reads.
const MaxTimestampResponseSize = 1 << 20

// HTTPTimestamper posts requests to an RFC 3161 TSA over HTTP (RFC 3161
// 3.4). The token is verified whatever the transport, so plain HTTP keeps
// integrity; use HTTPS when the request itself must stay private.
type HTTPTimestamper struct {
	URL string
	// Client defaults to a shared client like http.DefaultClient whose dial
	// connects again when the source port the system chose cannot be used;
	// set its Timeout.
	Client *http.Client
}

// Timestamp posts the request and returns the response body.
func (h HTTPTimestamper) Timestamp(ctx context.Context, request []byte) ([]byte, error) {
	client := h.Client
	if client == nil {
		client = sourceport.HTTPClient()
	}
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, h.URL, bytes.NewReader(request))
	if err != nil {
		return nil, fmt.Errorf("dicom dsig: timestamp request: %w", err)
	}
	httpReq.Header.Set("Content-Type", "application/timestamp-query")
	httpReq.Header.Set("Accept", "application/timestamp-reply")
	resp, err := client.Do(httpReq)
	if err != nil {
		return nil, fmt.Errorf("dicom dsig: timestamp request: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("%w: HTTP %s", ErrTimestampResponse, resp.Status)
	}
	if mediaType := strings.TrimSpace(strings.Split(resp.Header.Get("Content-Type"), ";")[0]); !strings.EqualFold(mediaType, "application/timestamp-reply") {
		return nil, fmt.Errorf("%w: Content-Type %q", ErrTimestampResponse, mediaType)
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, MaxTimestampResponseSize+1))
	if err != nil {
		return nil, fmt.Errorf("dicom dsig: timestamp response: %w", err)
	}
	if len(body) > MaxTimestampResponseSize {
		return nil, fmt.Errorf("%w: response larger than %d bytes", ErrTimestampResponse, MaxTimestampResponseSize)
	}
	return body, nil
}
