package certrevocation

import (
	"bytes"
	"crypto/x509"
	"time"
)

// Check checks an already authenticated certificate/issuer pair. Every
// authenticated current full list contributes; any listed serial revokes it,
// even when the entry claims a future revocation date. Old lists do not provide
// current evidence. This conservative policy does not merge delta/hold state.
func (s *Set) Check(certificate, issuer *x509.Certificate, at time.Time) error {
	if certificate == nil || issuer == nil || certificate.SerialNumber == nil {
		return failure(IssuerMismatch, -1)
	}
	if !bytes.Equal(certificate.RawIssuer, issuer.RawSubject) {
		return failure(IssuerMismatch, -1)
	}
	if s == nil || len(s.lists) == 0 {
		return failure(Missing, -1)
	}
	current := false
	reason := failure(IssuerMismatch, -1)
	for i, l := range s.lists {
		crl := l.crl
		if !bytes.Equal(crl.RawIssuer, issuer.RawSubject) {
			continue
		}
		if len(crl.AuthorityKeyId) != 0 && len(issuer.SubjectKeyId) != 0 && !bytes.Equal(crl.AuthorityKeyId, issuer.SubjectKeyId) {
			continue
		}
		if err := crl.CheckSignatureFrom(issuer); err != nil {
			reason = failure(BadSignature, i)
			continue
		}
		if at.Before(crl.ThisUpdate) {
			reason = failure(NotYetValid, i)
			continue
		}
		if !at.Before(crl.NextUpdate) {
			reason = failure(Expired, i)
			continue
		}
		current = true
		if _, revoked := l.serials[string(certificate.SerialNumber.Bytes())]; revoked {
			return failure(Revoked, i)
		}
	}
	if current {
		return nil
	}
	return reason
}

// CheckChains accepts only a complete usable verified path. The trust anchor
// is excluded, except that the peer leaf is never excluded when itself trusted.
func (s *Set) CheckChains(chains [][]*x509.Certificate, all bool, at time.Time) error {
	var first error
	for ci, chain := range chains {
		end := 1
		if all && len(chain)-1 > end {
			end = len(chain) - 1
		}
		if len(chain) == 0 {
			if first == nil {
				first = failure(Missing, -1)
			}
			continue
		}
		var failed error
		for i := 0; i < end; i++ {
			issuer := chain[i]
			if i+1 < len(chain) {
				issuer = chain[i+1]
			}
			if err := s.Check(chain[i], issuer, at); err != nil {
				e := *err.(*Error)
				e.ChainIndex, e.CertificateIndex = ci, i
				failed = &e
				break
			}
		}
		if failed == nil {
			return nil
		}
		if first == nil {
			first = failed
		}
	}
	if first == nil {
		first = failure(Missing, -1)
	}
	return first
}
