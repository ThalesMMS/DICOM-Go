package dsig

import (
	"bytes"
	"crypto"
	"crypto/ecdsa"
	"crypto/ed25519"
	"crypto/rsa"
	"crypto/sha1"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/asn1"
	"errors"
	"fmt"
	"math/big"
)

// This file is a minimal CMS SignedData reader (RFC 5652) for RFC 3161 time
// stamp tokens, written from the RFCs. It accepts DER only and verifies one
// signer with signed attributes: the content type, the message digest of the
// encapsulated content and the ESS signing certificate (RFC 2634, RFC 5035)
// that RFC 3161 requires.

var (
	oidSignedData                = asn1.ObjectIdentifier{1, 2, 840, 113549, 1, 7, 2}
	oidTSTInfo                   = asn1.ObjectIdentifier{1, 2, 840, 113549, 1, 9, 16, 1, 4}
	oidAttributeContentType      = asn1.ObjectIdentifier{1, 2, 840, 113549, 1, 9, 3}
	oidAttributeMessageDigest    = asn1.ObjectIdentifier{1, 2, 840, 113549, 1, 9, 4}
	oidAttributeSigningCert      = asn1.ObjectIdentifier{1, 2, 840, 113549, 1, 9, 16, 2, 12}
	oidAttributeSigningCertV2    = asn1.ObjectIdentifier{1, 2, 840, 113549, 1, 9, 16, 2, 47}
	oidRSAEncryption             = asn1.ObjectIdentifier{1, 2, 840, 113549, 1, 1, 1}
	oidSHA1WithRSA               = asn1.ObjectIdentifier{1, 2, 840, 113549, 1, 1, 5}
	oidSHA256WithRSA             = asn1.ObjectIdentifier{1, 2, 840, 113549, 1, 1, 11}
	oidSHA384WithRSA             = asn1.ObjectIdentifier{1, 2, 840, 113549, 1, 1, 12}
	oidSHA512WithRSA             = asn1.ObjectIdentifier{1, 2, 840, 113549, 1, 1, 13}
	oidECPublicKey               = asn1.ObjectIdentifier{1, 2, 840, 10045, 2, 1}
	oidECDSAWithSHA1             = asn1.ObjectIdentifier{1, 2, 840, 10045, 4, 1}
	oidECDSAWithSHA256           = asn1.ObjectIdentifier{1, 2, 840, 10045, 4, 3, 2}
	oidECDSAWithSHA384           = asn1.ObjectIdentifier{1, 2, 840, 10045, 4, 3, 3}
	oidECDSAWithSHA512           = asn1.ObjectIdentifier{1, 2, 840, 10045, 4, 3, 4}
	oidEd25519                   = asn1.ObjectIdentifier{1, 3, 101, 112}
	oidDigestSHA1                = asn1.ObjectIdentifier{1, 3, 14, 3, 2, 26}
	oidDigestSHA256              = asn1.ObjectIdentifier{2, 16, 840, 1, 101, 3, 4, 2, 1}
	oidDigestSHA384              = asn1.ObjectIdentifier{2, 16, 840, 1, 101, 3, 4, 2, 2}
	oidDigestSHA512              = asn1.ObjectIdentifier{2, 16, 840, 1, 101, 3, 4, 2, 3}
	oidExtensionExtendedKeyUsage = asn1.ObjectIdentifier{2, 5, 29, 37}
	oidExtKeyUsageTimeStamping   = asn1.ObjectIdentifier{1, 3, 6, 1, 5, 5, 7, 3, 8}
)

// digestAlgorithms maps the digest algorithm identifiers a token may use.
var digestAlgorithms = []struct {
	oid  asn1.ObjectIdentifier
	hash crypto.Hash
}{
	{oidDigestSHA1, crypto.SHA1},
	{oidDigestSHA256, crypto.SHA256},
	{oidDigestSHA384, crypto.SHA384},
	{oidDigestSHA512, crypto.SHA512},
}

func hashForOID(oid asn1.ObjectIdentifier) (crypto.Hash, bool) {
	for _, algorithm := range digestAlgorithms {
		if algorithm.oid.Equal(oid) {
			return algorithm.hash, true
		}
	}
	return 0, false
}

func oidForHash(hash crypto.Hash) (asn1.ObjectIdentifier, bool) {
	for _, algorithm := range digestAlgorithms {
		if algorithm.hash == hash {
			return algorithm.oid, true
		}
	}
	return nil, false
}

func digest(hash crypto.Hash, data []byte) []byte {
	h := hash.New()
	h.Write(data)
	return h.Sum(nil)
}

type contentInfo struct {
	ContentType asn1.ObjectIdentifier
	Content     asn1.RawValue `asn1:"explicit,optional,tag:0"`
}

type encapsulatedContentInfo struct {
	EContentType asn1.ObjectIdentifier
	EContent     asn1.RawValue `asn1:"explicit,optional,tag:0"`
}

type signedData struct {
	Version          int
	DigestAlgorithms []pkix.AlgorithmIdentifier `asn1:"set"`
	EncapContentInfo encapsulatedContentInfo
	Certificates     asn1.RawValue `asn1:"optional,tag:0"`
	CRLs             asn1.RawValue `asn1:"optional,tag:1"`
	SignerInfos      []signerInfo  `asn1:"set"`
}

type signerInfo struct {
	Version            int
	SID                asn1.RawValue
	DigestAlgorithm    pkix.AlgorithmIdentifier
	SignedAttrs        asn1.RawValue `asn1:"optional,tag:0"`
	SignatureAlgorithm pkix.AlgorithmIdentifier
	Signature          []byte
	UnsignedAttrs      asn1.RawValue `asn1:"optional,tag:1"`
}

type issuerAndSerialNumber struct {
	Issuer       asn1.RawValue
	SerialNumber *big.Int
}

type cmsAttribute struct {
	Type   asn1.ObjectIdentifier
	Values asn1.RawValue `asn1:"set"`
}

type essCertID struct {
	CertHash     []byte
	IssuerSerial asn1.RawValue `asn1:"optional"`
}

type essCertIDv2 struct {
	HashAlgorithm pkix.AlgorithmIdentifier `asn1:"optional"`
	CertHash      []byte
	IssuerSerial  asn1.RawValue `asn1:"optional"`
}

type signingCertificate struct {
	Certs    []essCertID
	Policies asn1.RawValue `asn1:"optional"`
}

type signingCertificateV2 struct {
	Certs    []essCertIDv2
	Policies asn1.RawValue `asn1:"optional"`
}

// signedToken is a parsed and cryptographically verified SignedData.
type signedToken struct {
	content []byte
	signer  *x509.Certificate
	// embedded reports that the token carried the signer certificate.
	embedded bool
}

var errTrailingData = errors.New("trailing data")

// unmarshalDER decodes one DER value that must fill data.
func unmarshalDER(data []byte, value any) error {
	rest, err := asn1.Unmarshal(data, value)
	if err != nil {
		return err
	}
	if len(rest) != 0 {
		return errTrailingData
	}
	return nil
}

// parseSignedToken reads a ContentInfo holding a SignedData whose
// encapsulated content type is contentType, finds its signer certificate
// (among the embedded ones, then extra) and verifies the signature. It does
// not decide whether the signer is trusted.
func parseSignedToken(der []byte, contentType asn1.ObjectIdentifier, extra []*x509.Certificate) (signedToken, error) {
	var info contentInfo
	if err := unmarshalDER(der, &info); err != nil {
		return signedToken{}, fmt.Errorf("ContentInfo: %w", err)
	}
	if !info.ContentType.Equal(oidSignedData) {
		return signedToken{}, fmt.Errorf("content type %s is not SignedData", info.ContentType)
	}
	var sd signedData
	if err := unmarshalDER(info.Content.Bytes, &sd); err != nil {
		return signedToken{}, fmt.Errorf("SignedData: %w", err)
	}
	if !sd.EncapContentInfo.EContentType.Equal(contentType) {
		return signedToken{}, fmt.Errorf("encapsulated content type %s, want %s", sd.EncapContentInfo.EContentType, contentType)
	}
	var content []byte
	if err := unmarshalDER(sd.EncapContentInfo.EContent.Bytes, &content); err != nil {
		return signedToken{}, fmt.Errorf("encapsulated content: %w", err)
	}
	if len(sd.SignerInfos) != 1 {
		return signedToken{}, fmt.Errorf("%d signer infos, want 1", len(sd.SignerInfos))
	}
	var certificates []*x509.Certificate
	if len(sd.Certificates.Bytes) > 0 {
		// Only X.509 certificates are taken; other CertificateChoices make
		// the set unreadable.
		parsed, err := x509.ParseCertificates(sd.Certificates.Bytes)
		if err != nil {
			return signedToken{}, fmt.Errorf("certificates: %w", err)
		}
		certificates = parsed
	}
	si := sd.SignerInfos[0]
	signer, err := findSigner(si.SID, certificates)
	embedded := err == nil
	if !embedded {
		signer, err = findSigner(si.SID, extra)
	}
	if err != nil {
		return signedToken{}, err
	}
	hash, ok := hashForOID(si.DigestAlgorithm.Algorithm)
	if !ok {
		return signedToken{}, fmt.Errorf("%w: digest algorithm %s", ErrUnsupported, si.DigestAlgorithm.Algorithm)
	}
	if len(si.SignedAttrs.FullBytes) == 0 {
		return signedToken{}, errors.New("no signed attributes")
	}
	attributes, err := parseAttributes(si.SignedAttrs.Bytes)
	if err != nil {
		return signedToken{}, fmt.Errorf("signed attributes: %w", err)
	}
	var signedType asn1.ObjectIdentifier
	if value, ok := attributes[oidAttributeContentType.String()]; !ok || unmarshalDER(value, &signedType) != nil || !signedType.Equal(contentType) {
		return signedToken{}, errors.New("content-type attribute missing or different from the content")
	}
	var messageDigest []byte
	if value, ok := attributes[oidAttributeMessageDigest.String()]; !ok || unmarshalDER(value, &messageDigest) != nil {
		return signedToken{}, errors.New("message-digest attribute missing")
	}
	if !bytes.Equal(messageDigest, digest(hash, content)) {
		return signedToken{}, errors.New("message digest does not match the content")
	}
	if err := checkSigningCertificate(attributes, signer); err != nil {
		return signedToken{}, err
	}
	// The signature covers the DER of the signed attributes with the SET
	// OF tag in place of the [0] IMPLICIT tag (RFC 5652 5.4).
	signedBytes := append([]byte(nil), si.SignedAttrs.FullBytes...)
	signedBytes[0] = 0x31
	if err := checkCMSSignature(signer, si.SignatureAlgorithm.Algorithm, hash, signedBytes, si.Signature); err != nil {
		return signedToken{}, err
	}
	return signedToken{content: content, signer: signer, embedded: embedded}, nil
}

// findSigner returns the certificate a SignerIdentifier names: an
// IssuerAndSerialNumber or a [0] SubjectKeyIdentifier.
func findSigner(sid asn1.RawValue, certificates []*x509.Certificate) (*x509.Certificate, error) {
	switch {
	case sid.Class == asn1.ClassUniversal && sid.Tag == asn1.TagSequence:
		var id issuerAndSerialNumber
		if err := unmarshalDER(sid.FullBytes, &id); err != nil || id.SerialNumber == nil {
			return nil, errors.New("unreadable signer identifier")
		}
		for _, cert := range certificates {
			if bytes.Equal(cert.RawIssuer, id.Issuer.FullBytes) && cert.SerialNumber.Cmp(id.SerialNumber) == 0 {
				return cert, nil
			}
		}
	case sid.Class == asn1.ClassContextSpecific && sid.Tag == 0:
		for _, cert := range certificates {
			if len(cert.SubjectKeyId) > 0 && bytes.Equal(cert.SubjectKeyId, sid.Bytes) {
				return cert, nil
			}
		}
	default:
		return nil, errors.New("unreadable signer identifier")
	}
	return nil, errors.New("signer certificate not in the token nor among the given certificates")
}

// parseAttributes maps each attribute type to the DER of its single value.
func parseAttributes(data []byte) (map[string][]byte, error) {
	out := map[string][]byte{}
	for len(data) > 0 {
		var attribute cmsAttribute
		rest, err := asn1.Unmarshal(data, &attribute)
		if err != nil {
			return nil, err
		}
		data = rest
		var value asn1.RawValue
		if rest, err := asn1.Unmarshal(attribute.Values.Bytes, &value); err != nil || len(rest) != 0 {
			return nil, fmt.Errorf("attribute %s does not have exactly one value", attribute.Type)
		}
		key := attribute.Type.String()
		if _, dup := out[key]; dup {
			return nil, fmt.Errorf("attribute %s repeated", attribute.Type)
		}
		out[key] = value.FullBytes
	}
	return out, nil
}

// checkSigningCertificate requires an ESS signing-certificate attribute (v1
// with SHA-1 or v2) whose first certificate hash is that of signer.
func checkSigningCertificate(attributes map[string][]byte, signer *x509.Certificate) error {
	if value, ok := attributes[oidAttributeSigningCertV2.String()]; ok {
		var sc signingCertificateV2
		if err := unmarshalDER(value, &sc); err != nil || len(sc.Certs) == 0 {
			return errors.New("unreadable signing-certificate-v2 attribute")
		}
		id := sc.Certs[0]
		hash := crypto.SHA256
		if len(id.HashAlgorithm.Algorithm) > 0 {
			var ok bool
			if hash, ok = hashForOID(id.HashAlgorithm.Algorithm); !ok {
				return fmt.Errorf("%w: signing certificate hash %s", ErrUnsupported, id.HashAlgorithm.Algorithm)
			}
		}
		if !bytes.Equal(id.CertHash, digest(hash, signer.Raw)) {
			return errors.New("signing-certificate-v2 does not name the signer certificate")
		}
		return nil
	}
	if value, ok := attributes[oidAttributeSigningCert.String()]; ok {
		var sc signingCertificate
		if err := unmarshalDER(value, &sc); err != nil || len(sc.Certs) == 0 {
			return errors.New("unreadable signing-certificate attribute")
		}
		sum := sha1.Sum(signer.Raw)
		if !bytes.Equal(sc.Certs[0].CertHash, sum[:]) {
			return errors.New("signing-certificate does not name the signer certificate")
		}
		return nil
	}
	return errors.New("no ESS signing-certificate attribute")
}

// checkCMSSignature verifies a SignerInfo signature over message with the
// signer certificate's key.
func checkCMSSignature(signer *x509.Certificate, algorithm asn1.ObjectIdentifier, hash crypto.Hash, message, signature []byte) error {
	invalid := errors.New("token signature does not verify with the signer certificate")
	switch key := signer.PublicKey.(type) {
	case *rsa.PublicKey:
		switch {
		case algorithm.Equal(oidRSAEncryption), algorithm.Equal(oidSHA1WithRSA), algorithm.Equal(oidSHA256WithRSA),
			algorithm.Equal(oidSHA384WithRSA), algorithm.Equal(oidSHA512WithRSA):
		default:
			return fmt.Errorf("%w: token signature algorithm %s", ErrUnsupported, algorithm)
		}
		if rsa.VerifyPKCS1v15(key, hash, digest(hash, message), signature) != nil {
			return invalid
		}
	case *ecdsa.PublicKey:
		switch {
		case algorithm.Equal(oidECPublicKey), algorithm.Equal(oidECDSAWithSHA1), algorithm.Equal(oidECDSAWithSHA256),
			algorithm.Equal(oidECDSAWithSHA384), algorithm.Equal(oidECDSAWithSHA512):
		default:
			return fmt.Errorf("%w: token signature algorithm %s", ErrUnsupported, algorithm)
		}
		if !ecdsa.VerifyASN1(key, digest(hash, message), signature) {
			return invalid
		}
	case ed25519.PublicKey:
		if !algorithm.Equal(oidEd25519) {
			return fmt.Errorf("%w: token signature algorithm %s", ErrUnsupported, algorithm)
		}
		if !ed25519.Verify(key, message, signature) {
			return invalid
		}
	default:
		return fmt.Errorf("%w: %T token signer key", ErrUnsupported, signer.PublicKey)
	}
	return nil
}

// hasCriticalTimeStampingUsage reports a certificate whose Extended Key
// Usage extension is critical and lists only id-kp-timeStamping, as RFC 3161
// 2.3 requires of a TSA certificate.
func hasCriticalTimeStampingUsage(cert *x509.Certificate) bool {
	for _, ext := range cert.Extensions {
		if !ext.Id.Equal(oidExtensionExtendedKeyUsage) {
			continue
		}
		var usages []asn1.ObjectIdentifier
		if !ext.Critical || unmarshalDER(ext.Value, &usages) != nil || len(usages) != 1 {
			return false
		}
		return usages[0].Equal(oidExtKeyUsageTimeStamping)
	}
	return false
}
