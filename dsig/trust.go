package dsig

import (
	"bytes"
	"crypto/x509"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/ThalesMMS/dicom-go/dcmtime"
	"github.com/ThalesMMS/dicom-go/object"
)

var (
	// ErrCertificateUntrusted reports a signer certificate that does not
	// chain to a trust anchor under the TrustOptions. Every trust failure
	// matches it; the errors below narrow the reason.
	ErrCertificateUntrusted = errors.New("dicom dsig: signer certificate not trusted")
	// ErrCertificateExpired reports a signer, intermediate or root
	// certificate outside its validity period at the verification time.
	ErrCertificateExpired = errors.New("dicom dsig: certificate not valid at the verification time")
	// ErrCertificateRevoked reports a certificate of the chain listed in a
	// certificate revocation list its issuer signed.
	ErrCertificateRevoked = errors.New("dicom dsig: certificate revoked")
	// ErrRevocationUnknown reports, with TrustOptions.RequireCRL, a chain
	// certificate whose issuer has no current CRL among TrustOptions.CRLs.
	ErrRevocationUnknown = errors.New("dicom dsig: no current certificate revocation list")
	// ErrCertificateUsage reports a signer certificate whose Key Usage or
	// Extended Key Usage does not allow signing under the KeyUsagePolicy.
	ErrCertificateUsage = errors.New("dicom dsig: certificate usage does not allow signing")
)

// KeyUsagePolicy states which signer certificates may sign.
type KeyUsagePolicy struct {
	// Allowed lists Key Usage bits of which a signer certificate carrying
	// the Key Usage extension must have at least one. Zero means
	// x509.KeyUsageDigitalSignature or x509.KeyUsageContentCommitment
	// (nonRepudiation).
	Allowed x509.KeyUsage
	// RequireExtension refuses a signer certificate without Key Usage.
	RequireExtension bool
	// ExtKeyUsages, when set, must be allowed by the Extended Key Usage of
	// every certificate of the chain, as crypto/x509 checks them. Nil
	// accepts any.
	ExtKeyUsages []x509.ExtKeyUsage
	// Ignore skips the Key Usage check (ExtKeyUsages still applies).
	Ignore bool
}

// TrustOptions configure the signer certificate checks of VerifyWithTrust
// and VerifyCertificate. Only crypto/x509 is used: no network access, no
// OCSP and no fetching of CRLs or intermediates.
type TrustOptions struct {
	// Roots are the trust anchors. Nil trusts nothing: the system roots
	// are not used unless the caller passes them.
	Roots *x509.CertPool
	// Intermediates are untrusted certificates that may complete a chain
	// from the signer certificate to a root.
	Intermediates *x509.CertPool
	// CRLs are certificate revocation lists. A CRL applies to a chain
	// certificate when its issuer signed it; a listed serial number
	// revokes the certificate whatever the revocation date, as OpenSSL
	// does. CRLs with another issuer or a bad signature are ignored.
	CRLs []*x509.RevocationList
	// RequireCRL fails a chain certificate (other than the root) whose
	// issuer has no CRL with a Next Update at or after the verification
	// time (or none), like DCMTK dcmsign +cl.
	RequireCRL bool
	// Time is the verification time; zero uses the current time.
	Time time.Time
	// AtSignatureTime verifies at the Digital Signature DateTime instead
	// of Time. That time is asserted by the signer, not certified: without
	// a verified timestamp it only tells whether the certificate was valid
	// when the signer says it signed.
	AtSignatureTime bool
	// KeyUsage is the policy for the signer certificate's key usages.
	KeyUsage KeyUsagePolicy
	// TimestampRoots are the trust anchors of time stamping authorities,
	// with untrusted TimestampIntermediates. When set, VerifyWithTrust
	// also requires the TSA of a Certified Timestamp to chain to them (at
	// Time) and reports it in Result.TimestampErr.
	TimestampRoots         *x509.CertPool
	TimestampIntermediates *x509.CertPool
	// AtTimestampTime verifies the signer certificate at the time its
	// Certified Timestamp certifies, so a signature made while the
	// certificate was valid stays trusted after it expires. The timestamp
	// must verify and its TSA chain to TimestampRoots (at Time);
	// otherwise the certificate is not trusted. It takes precedence over
	// AtSignatureTime.
	AtTimestampTime bool
}

// timestampOptions are the TSA checks the trust options ask for.
func (opts TrustOptions) timestampOptions() TimestampOptions {
	return TimestampOptions{Roots: opts.TimestampRoots, Intermediates: opts.TimestampIntermediates, Time: opts.Time}
}

// verificationTime returns the time the chain is verified at.
func (opts TrustOptions) verificationTime(sig Signature) (time.Time, error) {
	if opts.AtTimestampTime {
		if opts.TimestampRoots == nil {
			return time.Time{}, fmt.Errorf("%w: AtTimestampTime without TimestampRoots", ErrTimestampUntrusted)
		}
		info, err := VerifyTimestamp(sig, opts.timestampOptions())
		if err != nil {
			return time.Time{}, err
		}
		return info.Time, nil
	}
	if opts.AtSignatureTime {
		value := strings.TrimSpace(sig.DateTime)
		if value == "" {
			return time.Time{}, fmt.Errorf("%w: no Digital Signature DateTime", ErrMalformedSignature)
		}
		parsed, err := dcmtime.ParseDatetime(value)
		if err != nil {
			return time.Time{}, fmt.Errorf("%w: Digital Signature DateTime %q", ErrMalformedSignature, value)
		}
		return parsed.Time, nil
	}
	if opts.Time.IsZero() {
		return time.Now(), nil
	}
	return opts.Time, nil
}

// checkKeyUsage applies the Key Usage part of the policy to the signer
// certificate.
func (p KeyUsagePolicy) checkKeyUsage(leaf *x509.Certificate) error {
	if p.Ignore {
		return nil
	}
	allowed := p.Allowed
	if allowed == 0 {
		allowed = x509.KeyUsageDigitalSignature | x509.KeyUsageContentCommitment
	}
	if leaf.KeyUsage == 0 {
		if p.RequireExtension {
			return fmt.Errorf("%w: no Key Usage extension", ErrCertificateUsage)
		}
		return nil
	}
	if leaf.KeyUsage&allowed == 0 {
		return fmt.Errorf("%w: Key Usage %#x has none of %#x", ErrCertificateUsage, int(leaf.KeyUsage), int(allowed))
	}
	return nil
}

// VerifyCertificate decides whether the signer certificate of sig is
// trusted: it chains to opts.Roots (through opts.Intermediates) with every
// certificate valid at the verification time, no chain certificate is
// revoked by opts.CRLs, and the key usages allow signing. It returns the
// accepted chains, signer first. It does not check the signature itself
// (see VerifySignature). Every error matches ErrCertificateUntrusted.
func VerifyCertificate(sig Signature, opts TrustOptions) ([][]*x509.Certificate, error) {
	// untrusted wraps the reason (when known) and the detail, such as a
	// crypto/x509 error, so both stay reachable with errors.Is and As.
	untrusted := func(reason, detail error) error {
		if reason == nil {
			return fmt.Errorf("%w: %w", ErrCertificateUntrusted, detail)
		}
		return fmt.Errorf("%w: %w: %w", ErrCertificateUntrusted, reason, detail)
	}
	leaf := sig.Certificate
	if leaf == nil {
		return nil, untrusted(ErrMalformedSignature, errors.New("unreadable Certificate of Signer"))
	}
	if opts.Roots == nil {
		return nil, untrusted(nil, errors.New("no trust anchors"))
	}
	at, err := opts.verificationTime(sig)
	if err != nil {
		return nil, fmt.Errorf("%w: %w", ErrCertificateUntrusted, err)
	}
	if err := opts.KeyUsage.checkKeyUsage(leaf); err != nil {
		return nil, fmt.Errorf("%w: %w", ErrCertificateUntrusted, err)
	}
	usages := opts.KeyUsage.ExtKeyUsages
	if len(usages) == 0 {
		usages = []x509.ExtKeyUsage{x509.ExtKeyUsageAny}
	}
	chains, err := leaf.Verify(x509.VerifyOptions{
		Roots:         opts.Roots,
		Intermediates: opts.Intermediates,
		CurrentTime:   at,
		KeyUsages:     usages,
	})
	if err != nil {
		var invalid x509.CertificateInvalidError
		if errors.As(err, &invalid) {
			switch invalid.Reason {
			case x509.Expired:
				return nil, untrusted(ErrCertificateExpired, err)
			case x509.IncompatibleUsage:
				return nil, untrusted(ErrCertificateUsage, err)
			}
		}
		return nil, untrusted(nil, err)
	}
	if len(opts.CRLs) == 0 && !opts.RequireCRL {
		return chains, nil
	}
	var accepted [][]*x509.Certificate
	var firstErr error
	for _, chain := range chains {
		if err := checkRevocation(chain, opts.CRLs, at, opts.RequireCRL); err != nil {
			if firstErr == nil {
				firstErr = err
			}
			continue
		}
		accepted = append(accepted, chain)
	}
	if len(accepted) == 0 {
		return nil, fmt.Errorf("%w: %w", ErrCertificateUntrusted, firstErr)
	}
	return accepted, nil
}

// checkRevocation looks up every certificate of chain but the root in the
// CRLs its issuer signed.
func checkRevocation(chain []*x509.Certificate, crls []*x509.RevocationList, at time.Time, requireCRL bool) error {
	for i := 0; i+1 < len(chain); i++ {
		certificate, issuer := chain[i], chain[i+1]
		current := false
		for _, crl := range crls {
			if crl == nil || !bytes.Equal(crl.RawIssuer, issuer.RawSubject) || crl.CheckSignatureFrom(issuer) != nil {
				continue
			}
			for _, entry := range crl.RevokedCertificateEntries {
				if entry.SerialNumber != nil && entry.SerialNumber.Cmp(certificate.SerialNumber) == 0 {
					return fmt.Errorf("%w: %q serial %s on %s", ErrCertificateRevoked, certificate.Subject.String(), certificate.SerialNumber, entry.RevocationTime.UTC().Format(time.RFC3339))
				}
			}
			if crl.NextUpdate.IsZero() || !at.After(crl.NextUpdate) {
				current = true
			}
		}
		if requireCRL && !current {
			return fmt.Errorf("%w: issuer %q of %q", ErrRevocationUnknown, issuer.Subject.String(), certificate.Subject.String())
		}
	}
	return nil
}

// VerifyWithTrust checks every digital signature of obj like Verify and
// also whether its signer certificate is trusted under opts. Result.Err
// reports the signature and Result.TrustErr the certificate, so a
// signature can be valid with an untrusted certificate. With
// opts.TimestampRoots a Certified Timestamp's TSA must be trusted too
// (Result.TimestampErr).
func VerifyWithTrust(obj *object.Object, opts TrustOptions) []Result {
	results := Verify(obj)
	for i := range results {
		results[i].TrustChecked = true
		results[i].Chains, results[i].TrustErr = VerifyCertificate(results[i].Signature, opts)
		if opts.TimestampRoots != nil {
			results[i].checkTimestamp(opts.timestampOptions())
		}
	}
	return results
}
