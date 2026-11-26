package dsig

import (
	"crypto"
	"crypto/ecdsa"
	"crypto/md5"
	"crypto/rsa"
	"crypto/sha1"
	"crypto/sha256"
	"crypto/sha512"
	"crypto/x509"
	"encoding/asn1"
	"fmt"
	"hash"

	"github.com/ThalesMMS/dicom-go/object"
)

// macHash returns the hash of a MAC Algorithm and the DigestInfo object
// identifier PKCS #1 v1.5 RSA signatures carry for it.
func macHash(algorithm string) (hash.Hash, asn1.ObjectIdentifier, error) {
	switch algorithm {
	case MACRIPEMD160:
		return newRIPEMD160(), asn1.ObjectIdentifier{1, 3, 36, 3, 2, 1}, nil
	case MACMD5:
		return md5.New(), asn1.ObjectIdentifier{1, 2, 840, 113549, 2, 5}, nil
	case MACSHA1:
		return sha1.New(), asn1.ObjectIdentifier{1, 3, 14, 3, 2, 26}, nil
	case MACSHA256:
		return sha256.New(), asn1.ObjectIdentifier{2, 16, 840, 1, 101, 3, 4, 2, 1}, nil
	case MACSHA384:
		return sha512.New384(), asn1.ObjectIdentifier{2, 16, 840, 1, 101, 3, 4, 2, 2}, nil
	case MACSHA512:
		return sha512.New(), asn1.ObjectIdentifier{2, 16, 840, 1, 101, 3, 4, 2, 3}, nil
	default:
		return nil, nil, fmt.Errorf("%w: MAC Algorithm %q", ErrUnsupported, algorithm)
	}
}

// macCryptoHash returns the crypto.Hash identifier of a MAC Algorithm, which
// crypto.Signer implementations such as ECDSA require since Go 1.27 to sign a
// precomputed digest. RIPEMD-160 is identified without being registered.
func macCryptoHash(algorithm string) (crypto.Hash, error) {
	switch algorithm {
	case MACRIPEMD160:
		return crypto.RIPEMD160, nil
	case MACMD5:
		return crypto.MD5, nil
	case MACSHA1:
		return crypto.SHA1, nil
	case MACSHA256:
		return crypto.SHA256, nil
	case MACSHA384:
		return crypto.SHA384, nil
	case MACSHA512:
		return crypto.SHA512, nil
	default:
		return 0, fmt.Errorf("%w: MAC Algorithm %q", ErrUnsupported, algorithm)
	}
}

type algorithmIdentifier struct {
	Algorithm  asn1.ObjectIdentifier
	Parameters asn1.RawValue `asn1:"optional"`
}

type digestInfo struct {
	Algorithm algorithmIdentifier
	Digest    []byte
}

// digestInfoDER encodes the PKCS #1 DigestInfo of a MAC, with NULL
// parameters (as OpenSSL writes them) or without.
func digestInfoDER(oid asn1.ObjectIdentifier, mac []byte, nullParameters bool) ([]byte, error) {
	info := digestInfo{Algorithm: algorithmIdentifier{Algorithm: oid}, Digest: mac}
	if nullParameters {
		info.Algorithm.Parameters = asn1.NullRawValue
	}
	return asn1.Marshal(info)
}

// computeMAC hashes the signed byte stream of sig.
func computeMAC(sig Signature) ([]byte, asn1.ObjectIdentifier, error) {
	h, oid, err := macHash(sig.MACAlgorithm)
	if err != nil {
		return nil, nil, err
	}
	syntax, err := macTransferSyntax(sig.MACTransferSyntaxUID)
	if err != nil {
		return nil, nil, err
	}
	if err := writeSignedStream(h, sig.dataSet, sig.DataElementsSigned, sig.item, syntax); err != nil {
		return nil, nil, err
	}
	return h.Sum(nil), oid, nil
}

// VerifySignature checks one signature against its data set and the public
// key of its certificate. It does not decide whether the certificate is
// trusted (see VerifyCertificate).
func VerifySignature(sig Signature) error {
	if !sig.macParametersItemPresent {
		return fmt.Errorf("%w: no MAC Parameters item with MAC ID Number %d", ErrMalformedSignature, sig.MACIDNumber)
	}
	if sig.CertificateType != CertificateTypeX509 {
		return fmt.Errorf("%w: Certificate Type %q", ErrUnsupported, sig.CertificateType)
	}
	if sig.Certificate == nil {
		return fmt.Errorf("%w: unreadable Certificate of Signer", ErrMalformedSignature)
	}
	if len(sig.Signature) == 0 {
		return fmt.Errorf("%w: empty Signature", ErrMalformedSignature)
	}
	mac, oid, err := computeMAC(sig)
	if err != nil {
		return err
	}
	switch key := sig.Certificate.PublicKey.(type) {
	case *rsa.PublicKey:
		for _, nullParameters := range []bool{true, false} {
			info, err := digestInfoDER(oid, mac, nullParameters)
			if err != nil {
				return err
			}
			if rsa.VerifyPKCS1v15(key, crypto.Hash(0), info, sig.Signature) == nil {
				return nil
			}
		}
		return ErrSignatureInvalid
	case *ecdsa.PublicKey:
		// An odd-length DER signature carries the OB padding byte.
		var outer asn1.RawValue
		if _, err := asn1.Unmarshal(sig.Signature, &outer); err != nil {
			return ErrSignatureInvalid
		}
		if ecdsa.VerifyASN1(key, mac, outer.FullBytes) {
			return nil
		}
		return ErrSignatureInvalid
	default:
		return fmt.Errorf("%w: %T signer key", ErrUnsupported, sig.Certificate.PublicKey)
	}
}

// Result is the outcome of verifying one signature; Err is nil when the
// signature matches its data and certificate.
type Result struct {
	Signature Signature
	Err       error
	// TrustChecked reports that VerifyWithTrust checked the signer
	// certificate; Verify leaves it false.
	TrustChecked bool
	// TrustErr is nil when the checked signer certificate is trusted; it
	// matches ErrCertificateUntrusted otherwise.
	TrustErr error
	// Chains are the accepted certificate chains, signer first.
	Chains [][]*x509.Certificate
	// Timestamp describes the Certified Timestamp when the signature has
	// one whose token could be read. TimestampErr is nil when the token
	// verified and certifies this signature, and, for VerifyWithTrust with
	// TrustOptions.TimestampRoots, its TSA is trusted.
	Timestamp    *TimestampInfo
	TimestampErr error
}

// HasTimestamp reports a signature that carries a Certified Timestamp.
func (r Result) HasTimestamp() bool { return len(r.Signature.CertifiedTimestamp) > 0 }

// TimestampValid reports a Certified Timestamp that verified (see
// TimestampErr); Timestamp.Trusted tells whether its TSA was trusted.
func (r Result) TimestampValid() bool {
	return r.HasTimestamp() && r.Timestamp != nil && r.TimestampErr == nil
}

// checkTimestamp fills the timestamp outcome of r.
func (r *Result) checkTimestamp(opts TimestampOptions) {
	r.Timestamp, r.TimestampErr = nil, nil
	if !r.HasTimestamp() {
		return
	}
	info, err := VerifyTimestamp(r.Signature, opts)
	if info.Certificate != nil {
		r.Timestamp = &info
	}
	r.TimestampErr = err
}

// Valid reports whether the signature verified: the data matches the
// signature and the key of its certificate, trusted or not.
func (r Result) Valid() bool { return r.Err == nil }

// Trusted reports whether the signature is valid and its signer
// certificate was checked and trusted.
func (r Result) Trusted() bool { return r.Valid() && r.TrustChecked && r.TrustErr == nil }

// Verify checks every digital signature of obj, including those in sequence
// items. An object without signatures yields no results. A Certified
// Timestamp is checked too (token signature and imprint, not the TSA's
// trust). It does not check the signer certificates; VerifyWithTrust does.
func Verify(obj *object.Object) []Result {
	if obj == nil {
		return nil
	}
	var results []Result
	for _, sig := range Signatures(obj.Elements()) {
		result := Result{Signature: sig, Err: VerifySignature(sig)}
		result.checkTimestamp(TimestampOptions{})
		results = append(results, result)
	}
	return results
}
