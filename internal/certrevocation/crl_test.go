package certrevocation

import (
	"bytes"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/asn1"
	"errors"
	"math/big"
	"testing"
	"time"
)

type crlAuthority struct {
	cert *x509.Certificate
	key  *ecdsa.PrivateKey
}

func crlCA(t *testing.T, parent *crlAuthority, serial int64) crlAuthority {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC().Truncate(time.Second)
	template := &x509.Certificate{SerialNumber: big.NewInt(serial), Subject: pkix.Name{CommonName: "synthetic CA"}, NotBefore: now.Add(-time.Hour), NotAfter: now.Add(24 * time.Hour), IsCA: true, BasicConstraintsValid: true, KeyUsage: x509.KeyUsageCertSign | x509.KeyUsageCRLSign, SubjectKeyId: []byte{byte(serial)}}
	issuer, signer := template, key
	if parent != nil {
		issuer, signer = parent.cert, parent.key
	}
	der, err := x509.CreateCertificate(rand.Reader, template, issuer, &key.PublicKey, signer)
	if err != nil {
		t.Fatal(err)
	}
	cert, err := x509.ParseCertificate(der)
	if err != nil {
		t.Fatal(err)
	}
	return crlAuthority{cert, key}
}

func crlLeaf(t *testing.T, a crlAuthority) *x509.Certificate {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	template := &x509.Certificate{SerialNumber: big.NewInt(100), Subject: pkix.Name{CommonName: "synthetic leaf"}, NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(time.Hour), KeyUsage: x509.KeyUsageDigitalSignature}
	der, err := x509.CreateCertificate(rand.Reader, template, a.cert, &key.PublicKey, a.key)
	if err != nil {
		t.Fatal(err)
	}
	cert, err := x509.ParseCertificate(der)
	if err != nil {
		t.Fatal(err)
	}
	return cert
}

func crlList(t *testing.T, a crlAuthority, mutate func(*x509.RevocationList)) *x509.RevocationList {
	t.Helper()
	now := time.Now().UTC().Truncate(time.Second)
	template := &x509.RevocationList{Number: big.NewInt(1), ThisUpdate: now.Add(-time.Hour), NextUpdate: now.Add(time.Hour)}
	if mutate != nil {
		mutate(template)
	}
	der, err := x509.CreateRevocationList(rand.Reader, template, a.cert, a.key)
	if err != nil {
		t.Fatal(err)
	}
	list, err := x509.ParseRevocationList(der)
	if err != nil {
		t.Fatal(err)
	}
	return list
}

func requireCRLKind(t *testing.T, err error, kind Kind) {
	t.Helper()
	var e *Error
	if !errors.As(err, &e) || e.Kind != kind {
		t.Fatalf("error=%v; want kind %s", err, kind)
	}
	if e.Error() != "certificate revocation: "+string(kind) {
		t.Fatalf("unsafe/nonstable error: %s", e)
	}
}

func TestCRLCompileRestrictedMaterial(t *testing.T) {
	a := crlCA(t, nil, 1)
	for _, tc := range []struct {
		name   string
		mutate func(*x509.RevocationList)
		kind   Kind
	}{
		{"delta", func(r *x509.RevocationList) {
			r.ExtraExtensions = []pkix.Extension{{Id: deltaOID, Value: []byte{2, 1, 1}}}
		}, Unsupported},
		{"scoped", func(r *x509.RevocationList) { r.ExtraExtensions = []pkix.Extension{{Id: idpOID, Value: []byte{48, 0}}} }, Unsupported},
		{"critical", func(r *x509.RevocationList) {
			r.ExtraExtensions = []pkix.Extension{{Id: asn1.ObjectIdentifier{1, 2, 3, 4}, Critical: true, Value: []byte{5, 0}}}
		}, Unsupported},
		{"remove", func(r *x509.RevocationList) {
			r.RevokedCertificateEntries = []x509.RevocationListEntry{{SerialNumber: big.NewInt(100), RevocationTime: r.ThisUpdate, ReasonCode: 8}}
		}, Unsupported},
		{"indirect-entry", func(r *x509.RevocationList) {
			r.RevokedCertificateEntries = []x509.RevocationListEntry{{SerialNumber: big.NewInt(100), RevocationTime: r.ThisUpdate, ExtraExtensions: []pkix.Extension{{Id: issuerOID, Value: []byte{48, 0}}}}}
		}, Unsupported},
		{"critical-entry", func(r *x509.RevocationList) {
			r.RevokedCertificateEntries = []x509.RevocationListEntry{{SerialNumber: big.NewInt(100), RevocationTime: r.ThisUpdate, ExtraExtensions: []pkix.Extension{{Id: asn1.ObjectIdentifier{1, 2, 3}, Critical: true, Value: []byte{5, 0}}}}}
		}, Unsupported},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, err := New([]*x509.RevocationList{crlList(t, a, tc.mutate)})
			requireCRLKind(t, err, tc.kind)
		})
	}
	_, err := New([]*x509.RevocationList{nil})
	requireCRLKind(t, err, InvalidMaterial)
	// CreateRevocationList requires NextUpdate. Remove the optional ASN.1
	// field to exercise material rejection before any signature is trusted.
	valid := crlList(t, a, nil)
	var outer struct {
		TBS       asn1.RawValue
		Algorithm pkix.AlgorithmIdentifier
		Signature asn1.BitString
	}
	if _, err := asn1.Unmarshal(valid.Raw, &outer); err != nil {
		t.Fatal(err)
	}
	var fields []asn1.RawValue
	if _, err := asn1.Unmarshal(outer.TBS.FullBytes, &fields); err != nil {
		t.Fatal(err)
	}
	fields = append(fields[:4], fields[5:]...)
	inner, err := asn1.Marshal(fields)
	if err != nil {
		t.Fatal(err)
	}
	outer.TBS = asn1.RawValue{FullBytes: inner}
	withoutNext, err := asn1.Marshal(outer)
	if err != nil {
		t.Fatal(err)
	}
	parsed, err := x509.ParseRevocationList(withoutNext)
	if err != nil {
		t.Fatal(err)
	}
	_, err = New([]*x509.RevocationList{parsed})
	requireCRLKind(t, err, InvalidMaterial)
	_, err = New([]*x509.RevocationList{{Raw: []byte{1, 2, 3}}})
	requireCRLKind(t, err, InvalidMaterial)
	_, err = New(make([]*x509.RevocationList, MaxLists+1))
	requireCRLKind(t, err, Limit)
	_, err = New([]*x509.RevocationList{{Raw: make([]byte, MaxBytes+1)}})
	requireCRLKind(t, err, Limit)
	entries := make([]x509.RevocationListEntry, MaxEntries+1)
	for i := range entries {
		entries[i] = x509.RevocationListEntry{SerialNumber: big.NewInt(int64(i + 1)), RevocationTime: time.Now().Add(-time.Minute)}
	}
	_, err = New([]*x509.RevocationList{crlList(t, a, func(r *x509.RevocationList) { r.RevokedCertificateEntries = entries })})
	requireCRLKind(t, err, Limit)
}

func TestCRLPreflightTimeBoundaries(t *testing.T) {
	a := crlCA(t, nil, 1)
	raw := crlList(t, a, nil)
	s, err := New([]*x509.RevocationList{raw})
	if err != nil {
		t.Fatal(err)
	}
	if err = s.Preflight(raw.ThisUpdate); err != nil {
		t.Fatal(err)
	}
	requireCRLKind(t, s.Preflight(raw.ThisUpdate.Add(-time.Nanosecond)), NotYetValid)
	requireCRLKind(t, s.Preflight(raw.NextUpdate), Expired)
	empty, _ := New(nil)
	requireCRLKind(t, empty.Preflight(time.Now()), Missing)
}

func TestCRLAuthorityKeyIdentifierFromSignedDER(t *testing.T) {
	a := crlCA(t, nil, 1)
	leaf := crlLeaf(t, a)
	good := crlList(t, a, nil)
	// The caller field deliberately resembles Go 1.22's extension value.
	for _, ext := range good.Extensions {
		if ext.Id.Equal(akiOID) {
			good.AuthorityKeyId = append([]byte(nil), ext.Value...)
		}
	}
	s, err := New([]*x509.RevocationList{good})
	if err != nil {
		t.Fatal(err)
	}
	if err = s.Check(leaf, a.cert, time.Now()); err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		value []byte
		kind  Kind
	}{
		{[]byte{48, 0}, InvalidMaterial},
		{[]byte{48, 6, 128, 1, 1, 128, 1, 2}, InvalidMaterial},
		{[]byte{48, 3, 130, 1, 1}, Unsupported},
		{[]byte{48, 3, 128, 1, 1, 0}, InvalidMaterial},
	} {
		// Replace the extension in DER: CreateRevocationList always appends
		// its own AKI, so ExtraExtensions would exercise duplicate rejection.
		// These shape controls are refused before their signature is trusted.
		var envelope pkix.CertificateList
		if _, err := asn1.Unmarshal(good.Raw, &envelope); err != nil {
			t.Fatal(err)
		}
		envelope.TBSCertList.Raw = nil
		for i := range envelope.TBSCertList.Extensions {
			if envelope.TBSCertList.Extensions[i].Id.Equal(akiOID) {
				envelope.TBSCertList.Extensions[i].Value = tc.value
			}
		}
		der, err := asn1.Marshal(envelope)
		if err != nil {
			t.Fatal(err)
		}
		_, err = New([]*x509.RevocationList{{Raw: der}})
		requireCRLKind(t, err, tc.kind)
	}
}

// derEntryCount constructs structural controls without building or parsing an
// x509 entry slice. The retained signature is intentionally not authenticated.
func derEntryCount(t *testing.T, base []byte, count int) []byte {
	t.Helper()
	var outer struct {
		TBS       asn1.RawValue
		Algorithm pkix.AlgorithmIdentifier
		Signature asn1.BitString
	}
	if _, err := asn1.Unmarshal(base, &outer); err != nil {
		t.Fatal(err)
	}
	var fields []asn1.RawValue
	if _, err := asn1.Unmarshal(outer.TBS.FullBytes, &fields); err != nil {
		t.Fatal(err)
	}
	entry, err := asn1.Marshal(struct {
		Serial *big.Int
		Date   time.Time
	}{big.NewInt(1), time.Now().UTC().Truncate(time.Second)})
	if err != nil {
		t.Fatal(err)
	}
	items := asn1.RawValue{Class: 0, Tag: asn1.TagSequence, IsCompound: true, Bytes: bytes.Repeat(entry, count)}
	fields = append(fields[:5], append([]asn1.RawValue{items}, fields[5:]...)...)
	inner, err := asn1.Marshal(fields)
	if err != nil {
		t.Fatal(err)
	}
	outer.TBS = asn1.RawValue{FullBytes: inner}
	der, err := asn1.Marshal(outer)
	if err != nil {
		t.Fatal(err)
	}
	return der
}

func TestCRLAdmissionCountsAggregateBeforeAnyFullParse(t *testing.T) {
	a := crlCA(t, nil, 1)
	base := crlList(t, a, nil)
	for _, counts := range [][]int{{MaxEntries + 1}, {50000, 50001}} {
		var input []*x509.RevocationList
		for _, count := range counts {
			input = append(input, &x509.RevocationList{Raw: derEntryCount(t, base.Raw, count)})
		}
		calls := 0
		_, err := newWithParser(input, func(raw []byte) (*x509.RevocationList, error) { calls++; return x509.ParseRevocationList(raw) })
		requireCRLKind(t, err, Limit)
		if calls != 0 {
			t.Fatalf("oversize aggregate invoked full parser %d times", calls)
		}
	}
	boundary := derEntryCount(t, base.Raw, MaxEntries)
	calls := 0
	_, err := newWithParser([]*x509.RevocationList{{Raw: boundary}}, func(raw []byte) (*x509.RevocationList, error) { calls++; return x509.ParseRevocationList(raw) })
	if err != nil || calls != 1 {
		t.Fatalf("exact entry ceiling rejected: calls=%d err=%v", calls, err)
	}
}
