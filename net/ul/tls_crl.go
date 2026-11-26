package ul

import (
	"crypto/x509"
	"time"

	"github.com/ThalesMMS/dicom-go/internal/certrevocation"
)

// TLSCRLMode selects strict offline peer revocation checking.
type TLSCRLMode uint8

const (
	TLSCRLNone TLSCRLMode = iota
	TLSCRLLeaf
	// TLSCRLChain checks leaf and intermediates, excluding the trust anchor.
	// A directly trusted leaf is still checked, never vacuously accepted.
	TLSCRLChain
)

// TLSRevocationOptions supplies complete direct v2 CRLs. No network fetching
// occurs. Unknown status fails closed. Inputs are snapshotted from signed DER.
// Delta/indirect/scoped CRLs and removeFromCRL are unsupported and refused.
type TLSRevocationOptions struct {
	Mode TLSCRLMode
	CRLs []*x509.RevocationList
}

type TLSRevocationKind = certrevocation.Kind
type TLSRevocationError = certrevocation.Error

const (
	TLSRevocationRevoked         = certrevocation.Revoked
	TLSRevocationMissing         = certrevocation.Missing
	TLSRevocationExpired         = certrevocation.Expired
	TLSRevocationNotYetValid     = certrevocation.NotYetValid
	TLSRevocationIssuerMismatch  = certrevocation.IssuerMismatch
	TLSRevocationBadSignature    = certrevocation.BadSignature
	TLSRevocationUnsupported     = certrevocation.Unsupported
	TLSRevocationInvalidMaterial = certrevocation.InvalidMaterial
	TLSRevocationLimit           = certrevocation.Limit
	MaxTLSCRLLists               = certrevocation.MaxLists
	MaxTLSCRLBytes               = certrevocation.MaxBytes
	MaxTLSCRLEntries             = certrevocation.MaxEntries
)

var (
	ErrTLSCertificateRevoked = certrevocation.ErrRevoked
	ErrTLSRevocationUnknown  = certrevocation.ErrUnknown
	ErrTLSCRLInvalid         = certrevocation.ErrInvalid
)

// PreflightTLSCRLDER applies aggregate DER/list/entry bounds before full x509
// parsing. It checks only admission structure; success does not establish CRL
// validity, issuer/signature authority, temporal availability or peer trust.
func PreflightTLSCRLDER(lists [][]byte) error {
	return certrevocation.PreflightDER(lists)
}

// ValidateTLSCRLs checks bounded supported material and temporal availability.
// It cannot authenticate its issuer without an already verified peer chain.
func ValidateTLSCRLs(crls []*x509.RevocationList, at time.Time) error {
	s, err := certrevocation.New(crls)
	if err != nil {
		return err
	}
	return s.Preflight(at)
}

func compileTLSRevocation(opts TLSProfileOptions) (*certrevocation.Set, TLSCRLMode, error) {
	if opts.Revocation == nil {
		return nil, TLSCRLNone, nil
	}
	r := *opts.Revocation
	if r.Mode == TLSCRLNone && len(r.CRLs) == 0 {
		return nil, TLSCRLNone, nil
	}
	if (r.Mode != TLSCRLLeaf && r.Mode != TLSCRLChain) || opts.PeerAuthentication == TLSIgnorePeerCertificate {
		return nil, TLSCRLNone, &TLSRevocationError{Kind: TLSRevocationInvalidMaterial, ChainIndex: -1, CertificateIndex: -1, ListIndex: -1}
	}
	s, err := certrevocation.New(r.CRLs)
	if err == nil {
		err = s.Preflight(time.Now())
	}
	return s, r.Mode, err
}
