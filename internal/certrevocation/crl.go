// Package certrevocation checks a deliberately restricted offline CRL profile.
// It does not establish trust, fetch data, process delta or indirect CRLs, or
// replace crypto/x509 certification path validation.
package certrevocation

import (
	"bytes"
	"crypto/x509"
	"encoding/asn1"
	"errors"
	"time"
)

const (
	MaxLists   = 64
	MaxBytes   = 16 << 20
	MaxEntries = 100000
)

type Kind string

const (
	Revoked         Kind = "revoked"
	Missing         Kind = "missing"
	Expired         Kind = "expired"
	NotYetValid     Kind = "not-yet-valid"
	IssuerMismatch  Kind = "issuer-mismatch"
	BadSignature    Kind = "bad-signature"
	Unsupported     Kind = "unsupported"
	InvalidMaterial Kind = "invalid-material"
	Limit           Kind = "limit"
)

var (
	ErrRevoked = errors.New("certificate revoked")
	ErrUnknown = errors.New("certificate revocation unknown")
	ErrInvalid = errors.New("invalid CRL material")
)

// Error exposes bounded positions and a stable reason, never certificate
// names, serials, CRL contents or file paths. Positions are -1 when unavailable.
type Error struct {
	Kind                                    Kind
	ChainIndex, CertificateIndex, ListIndex int
}

func (e *Error) Error() string { return "certificate revocation: " + string(e.Kind) }
func (e *Error) Is(target error) bool {
	switch e.Kind {
	case Revoked:
		return target == ErrRevoked
	case Unsupported, InvalidMaterial, Limit:
		return target == ErrInvalid
	default:
		return target == ErrUnknown
	}
}

func failure(kind Kind, list int) *Error {
	return &Error{Kind: kind, ChainIndex: -1, CertificateIndex: -1, ListIndex: list}
}

type list struct {
	crl     *x509.RevocationList
	serials map[string]struct{}
}

// Set owns reparsed DER and indexes. No caller-owned CRL or serial is retained.
type Set struct{ lists []list }

var (
	akiOID    = asn1.ObjectIdentifier{2, 5, 29, 35}
	numberOID = asn1.ObjectIdentifier{2, 5, 29, 20}
	deltaOID  = asn1.ObjectIdentifier{2, 5, 29, 27}
	idpOID    = asn1.ObjectIdentifier{2, 5, 29, 28}
	issuerOID = asn1.ObjectIdentifier{2, 5, 29, 29}
	reasonOID = asn1.ObjectIdentifier{2, 5, 29, 21}
)

// New compiles complete direct v2 lists. The signed Raw is authoritative;
// modifying parsed fields while leaving Raw unchanged cannot change policy.
func New(crls []*x509.RevocationList) (*Set, error) {
	return newWithParser(crls, x509.ParseRevocationList)
}

func newWithParser(crls []*x509.RevocationList, parse func([]byte) (*x509.RevocationList, error)) (*Set, error) {
	if len(crls) > MaxLists {
		return nil, failure(Limit, -1)
	}
	bytesTotal := 0
	der := make([][]byte, 0, len(crls))
	for i, supplied := range crls {
		if supplied == nil || len(supplied.Raw) == 0 {
			return nil, failure(InvalidMaterial, i)
		}
		if len(supplied.Raw) > MaxBytes-bytesTotal {
			return nil, failure(Limit, i)
		}
		bytesTotal += len(supplied.Raw)
		der = append(der, append([]byte(nil), supplied.Raw...))
	}
	// Admit the complete aggregate before the first full parser invocation.
	// crypto/x509 materializes both current and legacy entry slices.
	if err := PreflightDER(der); err != nil {
		return nil, err
	}
	s := &Set{}
	for i, raw := range der {
		crl, err := parse(raw)
		if err != nil || !bytes.Equal(crl.Raw, raw) {
			return nil, failure(InvalidMaterial, i)
		}
		if crl.ThisUpdate.IsZero() || crl.NextUpdate.IsZero() || !crl.ThisUpdate.Before(crl.NextUpdate) {
			return nil, failure(InvalidMaterial, i)
		}
		// Go 1.22 reports the complete AKI extension DER in AuthorityKeyId;
		// newer releases report just its keyIdentifier. Derive the signed
		// identifier ourselves instead of comparing version-dependent fields.
		crl.AuthorityKeyId = nil
		seen := make(map[string]bool)
		for _, ext := range crl.Extensions {
			key := ext.Id.String()
			if seen[key] {
				return nil, failure(InvalidMaterial, i)
			}
			seen[key] = true
			if ext.Id.Equal(akiOID) {
				identifier, kind := parseAuthorityKeyID(ext.Value)
				if kind != "" {
					return nil, failure(kind, i)
				}
				crl.AuthorityKeyId = identifier
			}
			if ext.Id.Equal(deltaOID) || ext.Id.Equal(idpOID) {
				return nil, failure(Unsupported, i)
			}
			if ext.Critical && !ext.Id.Equal(akiOID) && !ext.Id.Equal(numberOID) {
				return nil, failure(Unsupported, i)
			}
		}
		compiled := list{crl: crl, serials: make(map[string]struct{}, len(crl.RevokedCertificateEntries))}
		for _, entry := range crl.RevokedCertificateEntries {
			if entry.SerialNumber == nil || entry.SerialNumber.Sign() < 0 {
				return nil, failure(InvalidMaterial, i)
			}
			if entry.ReasonCode == 8 {
				return nil, failure(Unsupported, i)
			}
			if entry.ReasonCode < 0 || entry.ReasonCode == 7 || entry.ReasonCode > 10 {
				return nil, failure(InvalidMaterial, i)
			}
			seen := make(map[string]bool)
			for _, ext := range entry.Extensions {
				key := ext.Id.String()
				if seen[key] {
					return nil, failure(InvalidMaterial, i)
				}
				seen[key] = true
				if ext.Id.Equal(issuerOID) || (ext.Critical && !ext.Id.Equal(reasonOID)) {
					return nil, failure(Unsupported, i)
				}
			}
			compiled.serials[string(entry.SerialNumber.Bytes())] = struct{}{}
		}
		s.lists = append(s.lists, compiled)
	}
	return s, nil
}

// PreflightDER checks only bounded ASN.1 structure and aggregate entry count.
// It is an allocation admission gate, not certification/signature/CRL validity.
// RawValue traversal aliases the bounded input and never builds entry slices.
func PreflightDER(lists [][]byte) error {
	if len(lists) > MaxLists {
		return failure(Limit, -1)
	}
	bytesTotal, entriesTotal := 0, 0
	for i, raw := range lists {
		if len(raw) == 0 {
			return failure(InvalidMaterial, i)
		}
		if len(raw) > MaxBytes-bytesTotal {
			return failure(Limit, i)
		}
		bytesTotal += len(raw)
	}
	for i, raw := range lists {
		count, kind := countDEREntries(raw, MaxEntries-entriesTotal)
		if kind != "" {
			return failure(kind, i)
		}
		entriesTotal += count
	}
	return nil
}

func rawDERField(data []byte) (asn1.RawValue, []byte, bool) {
	var value asn1.RawValue
	rest, err := asn1.Unmarshal(data, &value)
	return value, rest, err == nil && len(rest) < len(data)
}

func universalDER(value asn1.RawValue, tag int, compound bool) bool {
	return value.Class == 0 && value.Tag == tag && value.IsCompound == compound
}

func timeDER(value asn1.RawValue) bool {
	return value.Class == 0 && !value.IsCompound && (value.Tag == asn1.TagUTCTime || value.Tag == asn1.TagGeneralizedTime)
}

func countDEREntries(raw []byte, remaining int) (int, Kind) {
	envelope, tail, ok := rawDERField(raw)
	if !ok || len(tail) != 0 || !universalDER(envelope, asn1.TagSequence, true) {
		return 0, InvalidMaterial
	}
	tbs, outer, ok := rawDERField(envelope.Bytes)
	if !ok || !universalDER(tbs, asn1.TagSequence, true) {
		return 0, InvalidMaterial
	}
	algorithm, outer, ok := rawDERField(outer)
	if !ok || !universalDER(algorithm, asn1.TagSequence, true) {
		return 0, InvalidMaterial
	}
	signature, outer, ok := rawDERField(outer)
	if !ok || len(outer) != 0 || !universalDER(signature, asn1.TagBitString, false) {
		return 0, InvalidMaterial
	}
	data := tbs.Bytes
	field, rest, ok := rawDERField(data)
	if !ok {
		return 0, InvalidMaterial
	}
	if universalDER(field, asn1.TagInteger, false) {
		data = rest
	}
	// signature AlgorithmIdentifier, issuer Name
	for i := 0; i < 2; i++ {
		field, rest, ok = rawDERField(data)
		if !ok || !universalDER(field, asn1.TagSequence, true) {
			return 0, InvalidMaterial
		}
		data = rest
	}
	field, rest, ok = rawDERField(data)
	if !ok || !timeDER(field) {
		return 0, InvalidMaterial
	}
	data = rest
	if len(data) != 0 {
		field, rest, ok = rawDERField(data)
		if !ok {
			return 0, InvalidMaterial
		}
		if timeDER(field) {
			data = rest
		}
	}
	count := 0
	if len(data) != 0 {
		field, rest, ok = rawDERField(data)
		if !ok {
			return 0, InvalidMaterial
		}
		if universalDER(field, asn1.TagSequence, true) {
			for entries := field.Bytes; len(entries) != 0; {
				entry, next, valid := rawDERField(entries)
				if !valid || !universalDER(entry, asn1.TagSequence, true) {
					return 0, InvalidMaterial
				}
				count++
				if count > remaining {
					return 0, Limit
				}
				entries = next
			}
			data = rest
		}
	}
	if len(data) != 0 {
		field, rest, ok = rawDERField(data)
		if !ok || field.Class != 2 || field.Tag != 0 || !field.IsCompound || len(rest) != 0 {
			return 0, InvalidMaterial
		}
	}
	return count, ""
}

// This profile supports the usual keyIdentifier AKI. Issuer/serial-qualified
// AKIs require additional GeneralName matching and are refused explicitly.
func parseAuthorityKeyID(der []byte) ([]byte, Kind) {
	var sequence asn1.RawValue
	rest, err := asn1.Unmarshal(der, &sequence)
	if err != nil || len(rest) != 0 || sequence.Class != 0 || sequence.Tag != asn1.TagSequence || !sequence.IsCompound {
		return nil, InvalidMaterial
	}
	var identifier []byte
	for data := sequence.Bytes; len(data) != 0; {
		var field asn1.RawValue
		remaining, err := asn1.Unmarshal(data, &field)
		if err != nil || field.Class != 2 {
			return nil, InvalidMaterial
		}
		if field.Tag == 1 || field.Tag == 2 {
			return nil, Unsupported
		}
		if field.Tag != 0 || field.IsCompound || len(field.Bytes) == 0 || identifier != nil {
			return nil, InvalidMaterial
		}
		identifier = append([]byte(nil), field.Bytes...)
		data = remaining
	}
	if identifier == nil {
		return nil, InvalidMaterial
	}
	return identifier, ""
}

// Preflight refuses empty material and bundles with no temporally current
// list. It cannot establish issuer applicability before a peer chain exists.
func (s *Set) Preflight(at time.Time) error {
	if s == nil || len(s.lists) == 0 {
		return failure(Missing, -1)
	}
	for _, l := range s.lists {
		if !at.Before(l.crl.ThisUpdate) && at.Before(l.crl.NextUpdate) {
			return nil
		}
	}
	if at.Before(s.lists[0].crl.ThisUpdate) {
		return failure(NotYetValid, 0)
	}
	return failure(Expired, 0)
}
