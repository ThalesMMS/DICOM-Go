package dsig

import (
	"crypto"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/rsa"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"errors"
	"math/big"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/ThalesMMS/dicom-go/core"
	"github.com/ThalesMMS/dicom-go/internal/dicomtest"
	"github.com/ThalesMMS/dicom-go/internal/testutil"
	"github.com/ThalesMMS/dicom-go/transfer"
)

// trustCA is a synthetic certification authority generated at test time;
// File is its certificate in PEM for DCMTK.
type trustCA struct {
	Cert *x509.Certificate
	Key  crypto.Signer
	File string
	dir  string
}

// leafSpec describes a signer certificate.
type leafSpec struct {
	notBefore, notAfter time.Time
	keyUsage            x509.KeyUsage
	extKeyUsage         []x509.ExtKeyUsage
}

func randomSerial(t *testing.T) *big.Int {
	t.Helper()
	serial, err := rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 63))
	if err != nil {
		t.Fatal(err)
	}
	return serial.Add(serial, big.NewInt(1))
}

func writeTrustPEM(t *testing.T, path, blockType string, der []byte) {
	t.Helper()
	if err := os.WriteFile(path, pem.EncodeToMemory(&pem.Block{Type: blockType, Bytes: der}), 0o600); err != nil {
		t.Fatal(err)
	}
}

func newECKey(t *testing.T) crypto.Signer {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	return key
}

// newTrustCA creates a CA certificate signed by parent, or a self-signed
// root when parent is nil.
func newTrustCA(t *testing.T, dir, name string, parent *trustCA, notBefore, notAfter time.Time) trustCA {
	t.Helper()
	key := newECKey(t)
	template := &x509.Certificate{
		SerialNumber:          randomSerial(t),
		Subject:               pkix.Name{CommonName: name, Organization: []string{"dicom-go synthetic test PKI"}},
		NotBefore:             notBefore,
		NotAfter:              notAfter,
		IsCA:                  true,
		BasicConstraintsValid: true,
		KeyUsage:              x509.KeyUsageCertSign | x509.KeyUsageCRLSign,
	}
	issuer, issuerKey := template, key
	if parent != nil {
		issuer, issuerKey = parent.Cert, parent.Key
	}
	der, err := x509.CreateCertificate(rand.Reader, template, issuer, key.Public(), issuerKey)
	if err != nil {
		t.Fatal(err)
	}
	cert, err := x509.ParseCertificate(der)
	if err != nil {
		t.Fatal(err)
	}
	ca := trustCA{Cert: cert, Key: key, File: filepath.Join(dir, name+".pem"), dir: dir}
	writeTrustPEM(t, ca.File, "CERTIFICATE", der)
	return ca
}

// issue signs an RSA 2048 signer certificate, with its PEM certificate and
// key files for DCMTK.
func (ca trustCA) issue(t *testing.T, name string, spec leafSpec) signer {
	t.Helper()
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	template := &x509.Certificate{
		SerialNumber: randomSerial(t),
		Subject:      pkix.Name{CommonName: name, Organization: []string{"dicom-go synthetic test PKI"}},
		NotBefore:    spec.notBefore,
		NotAfter:     spec.notAfter,
		KeyUsage:     spec.keyUsage,
		ExtKeyUsage:  spec.extKeyUsage,
	}
	der, err := x509.CreateCertificate(rand.Reader, template, ca.Cert, key.Public(), ca.Key)
	if err != nil {
		t.Fatal(err)
	}
	leaf, err := x509.ParseCertificate(der)
	if err != nil {
		t.Fatal(err)
	}
	keyDER, err := x509.MarshalPKCS8PrivateKey(key)
	if err != nil {
		t.Fatal(err)
	}
	cred := testutil.TestCredential{
		Certificate: tls.Certificate{Certificate: [][]byte{der}, PrivateKey: key},
		CertFile:    filepath.Join(ca.dir, name+".pem"),
		KeyFile:     filepath.Join(ca.dir, name+".key"),
	}
	writeTrustPEM(t, cred.CertFile, "CERTIFICATE", der)
	writeTrustPEM(t, cred.KeyFile, "PRIVATE KEY", keyDER)
	return signer{cred: cred, leaf: leaf}
}

// crl issues a CRL revoking certificates, written to a PEM file.
func (ca trustCA) crl(t *testing.T, name string, thisUpdate, nextUpdate time.Time, revoked ...*x509.Certificate) (*x509.RevocationList, string) {
	t.Helper()
	template := &x509.RevocationList{Number: randomSerial(t), ThisUpdate: thisUpdate, NextUpdate: nextUpdate}
	for _, cert := range revoked {
		template.RevokedCertificateEntries = append(template.RevokedCertificateEntries, x509.RevocationListEntry{
			SerialNumber:   cert.SerialNumber,
			RevocationTime: thisUpdate,
		})
	}
	der, err := x509.CreateRevocationList(rand.Reader, template, ca.Cert, ca.Key)
	if err != nil {
		t.Fatal(err)
	}
	list, err := x509.ParseRevocationList(der)
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(ca.dir, name+".crl.pem")
	writeTrustPEM(t, path, "X509 CRL", der)
	return list, path
}

func certPool(certs ...*x509.Certificate) *x509.CertPool {
	pool := x509.NewCertPool()
	for _, cert := range certs {
		pool.AddCert(cert)
	}
	return pool
}

func signingUsage() x509.KeyUsage {
	return x509.KeyUsageDigitalSignature | x509.KeyUsageContentCommitment
}

// TestVerifyWithTrust separates a valid signature from a trusted signer
// certificate: chains, missing anchors and intermediates, validity at the
// current, a fixed or the signature time, CRLs of the signer and of the
// intermediate, and the key usage policy.
func TestVerifyWithTrust(t *testing.T) {
	dir := t.TempDir()
	now := time.Now().Truncate(time.Second)
	year := 365 * 24 * time.Hour
	root := newTrustCA(t, dir, "trust-root", nil, now.Add(-year), now.Add(10*year))
	inter := newTrustCA(t, dir, "trust-intermediate", &root, now.Add(-year), now.Add(5*year))
	other := newTrustCA(t, dir, "other-root", nil, now.Add(-year), now.Add(10*year))
	// Same subject as the intermediate, another key: its CRLs must not count.
	impostor := newTrustCA(t, t.TempDir(), "trust-intermediate", nil, now.Add(-year), now.Add(10*year))

	valid := leafSpec{notBefore: now.Add(-time.Hour), notAfter: now.Add(year), keyUsage: signingUsage()}
	good := inter.issue(t, "signer-good", valid)
	revoked := inter.issue(t, "signer-revoked", valid)
	foreign := other.issue(t, "signer-foreign", valid)
	expired := inter.issue(t, "signer-expired", leafSpec{notBefore: now.Add(-48 * time.Hour), notAfter: now.Add(-24 * time.Hour), keyUsage: signingUsage()})
	encipherOnly := inter.issue(t, "signer-encipher", leafSpec{notBefore: valid.notBefore, notAfter: valid.notAfter, keyUsage: x509.KeyUsageKeyEncipherment})
	noKeyUsage := inter.issue(t, "signer-no-ku", leafSpec{notBefore: valid.notBefore, notAfter: valid.notAfter})
	clientAuth := inter.issue(t, "signer-client", leafSpec{notBefore: valid.notBefore, notAfter: valid.notAfter, keyUsage: signingUsage(), extKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth}})

	interCRL, _ := inter.crl(t, "inter", now.Add(-time.Hour), now.Add(24*time.Hour), revoked.leaf)
	rootCRL, _ := root.crl(t, "root", now.Add(-time.Hour), now.Add(24*time.Hour))
	rootRevokesInter, _ := root.crl(t, "root-revokes", now.Add(-time.Hour), now.Add(24*time.Hour), inter.Cert)
	staleCRL, _ := inter.crl(t, "stale", now.Add(-48*time.Hour), now.Add(-24*time.Hour))
	forgedCRL, _ := impostor.crl(t, "forged", now.Add(-time.Hour), now.Add(24*time.Hour), good.leaf)

	sign := func(s signer, at time.Time) Signature {
		opts := s.options(MACSHA256)
		opts.Time = at
		results := requireValid(t, signedObject(t, s, opts, false), 1)
		return results[0].Signature
	}
	goodSig := sign(good, now)
	base := TrustOptions{Roots: certPool(root.Cert), Intermediates: certPool(inter.Cert)}
	with := func(change func(*TrustOptions)) TrustOptions {
		opts := base
		change(&opts)
		return opts
	}
	badDateTime := goodSig
	badDateTime.DateTime = "not a date"

	for _, tc := range []struct {
		name   string
		sig    Signature
		opts   TrustOptions
		want   []error // nil: trusted
		chains int     // length of the first chain when trusted
	}{
		{name: "chain through intermediate", sig: goodSig, opts: base, chains: 3},
		{name: "intermediate as anchor", sig: goodSig, opts: TrustOptions{Roots: certPool(inter.Cert)}, chains: 2},
		{name: "no trust anchors", sig: goodSig, opts: TrustOptions{Intermediates: base.Intermediates}, want: []error{ErrCertificateUntrusted}},
		{name: "missing intermediate", sig: goodSig, opts: TrustOptions{Roots: base.Roots}, want: []error{ErrCertificateUntrusted}},
		{name: "other authority", sig: sign(foreign, now), opts: base, want: []error{ErrCertificateUntrusted}},
		{name: "expired now", sig: sign(expired, now.Add(-36*time.Hour)), opts: base, want: []error{ErrCertificateUntrusted, ErrCertificateExpired}},
		{name: "expired at signature time is valid", sig: sign(expired, now.Add(-36*time.Hour)), opts: with(func(o *TrustOptions) { o.AtSignatureTime = true }), chains: 3},
		{name: "signature time after expiry", sig: sign(expired, now), opts: with(func(o *TrustOptions) { o.AtSignatureTime = true }), want: []error{ErrCertificateExpired}},
		{name: "fixed time before validity", sig: goodSig, opts: with(func(o *TrustOptions) { o.Time = now.Add(-2 * year) }), want: []error{ErrCertificateExpired}},
		{name: "unreadable signature time", sig: badDateTime, opts: with(func(o *TrustOptions) { o.AtSignatureTime = true }), want: []error{ErrCertificateUntrusted, ErrMalformedSignature}},
		{name: "revoked signer", sig: sign(revoked, now), opts: with(func(o *TrustOptions) { o.CRLs = []*x509.RevocationList{interCRL} }), want: []error{ErrCertificateUntrusted, ErrCertificateRevoked}},
		{name: "revoked signer at signature time", sig: sign(revoked, now.Add(-2*time.Hour)), opts: with(func(o *TrustOptions) { o.CRLs = []*x509.RevocationList{interCRL} }), want: []error{ErrCertificateRevoked}},
		{name: "not revoked", sig: goodSig, opts: with(func(o *TrustOptions) { o.CRLs = []*x509.RevocationList{interCRL} }), chains: 3},
		{name: "revoked intermediate", sig: goodSig, opts: with(func(o *TrustOptions) { o.CRLs = []*x509.RevocationList{rootRevokesInter} }), want: []error{ErrCertificateRevoked}},
		{name: "CRL of another key ignored", sig: goodSig, opts: with(func(o *TrustOptions) { o.CRLs = []*x509.RevocationList{forgedCRL} }), chains: 3},
		{name: "required CRLs present", sig: goodSig, opts: with(func(o *TrustOptions) {
			o.CRLs, o.RequireCRL = []*x509.RevocationList{interCRL, rootCRL}, true
		}), chains: 3},
		{name: "required CRL missing", sig: goodSig, opts: with(func(o *TrustOptions) { o.CRLs, o.RequireCRL = []*x509.RevocationList{interCRL}, true }), want: []error{ErrCertificateUntrusted, ErrRevocationUnknown}},
		{name: "required CRL stale", sig: goodSig, opts: with(func(o *TrustOptions) {
			o.CRLs, o.RequireCRL = []*x509.RevocationList{staleCRL, rootCRL}, true
		}), want: []error{ErrRevocationUnknown}},
		{name: "key encipherment only", sig: sign(encipherOnly, now), opts: base, want: []error{ErrCertificateUntrusted, ErrCertificateUsage}},
		{name: "key usage ignored", sig: sign(encipherOnly, now), opts: with(func(o *TrustOptions) { o.KeyUsage.Ignore = true }), chains: 3},
		{name: "key encipherment allowed", sig: sign(encipherOnly, now), opts: with(func(o *TrustOptions) { o.KeyUsage.Allowed = x509.KeyUsageKeyEncipherment }), chains: 3},
		{name: "no key usage extension", sig: sign(noKeyUsage, now), opts: base, chains: 3},
		{name: "key usage extension required", sig: sign(noKeyUsage, now), opts: with(func(o *TrustOptions) { o.KeyUsage.RequireExtension = true }), want: []error{ErrCertificateUsage}},
		{name: "extended key usage refused", sig: sign(clientAuth, now), opts: with(func(o *TrustOptions) {
			o.KeyUsage.ExtKeyUsages = []x509.ExtKeyUsage{x509.ExtKeyUsageEmailProtection}
		}), want: []error{ErrCertificateUntrusted, ErrCertificateUsage}},
		{name: "extended key usage accepted", sig: sign(clientAuth, now), opts: with(func(o *TrustOptions) {
			o.KeyUsage.ExtKeyUsages = []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth}
		}), chains: 3},
	} {
		t.Run(tc.name, func(t *testing.T) {
			chains, err := VerifyCertificate(tc.sig, tc.opts)
			if tc.want == nil {
				if err != nil {
					t.Fatalf("not trusted: %v", err)
				}
				if len(chains) == 0 || len(chains[0]) != tc.chains || chains[0][0] != tc.sig.Certificate {
					t.Fatalf("chains %v, want a chain of %d from the signer", chains, tc.chains)
				}
				return
			}
			if err == nil || chains != nil {
				t.Fatalf("trusted (%d chains), want %v", len(chains), tc.want)
			}
			for _, want := range tc.want {
				if !errors.Is(err, want) {
					t.Fatalf("error %v does not match %v", err, want)
				}
			}
			if !errors.Is(err, ErrCertificateUntrusted) {
				t.Fatalf("error %v does not match ErrCertificateUntrusted", err)
			}
		})
	}

	t.Run("results separate signature and trust", func(t *testing.T) {
		obj := signedObject(t, good, good.options(MACSHA256), false)
		if results := Verify(obj); len(results) != 1 || !results[0].Valid() || results[0].TrustChecked || results[0].Trusted() {
			t.Fatalf("Verify: %+v", results)
		}
		results := VerifyWithTrust(obj, base)
		if len(results) != 1 || !results[0].Trusted() || len(results[0].Chains) != 1 {
			t.Fatalf("trusted signature: %+v", results)
		}
		untrusted := VerifyWithTrust(obj, TrustOptions{Roots: certPool(other.Cert)})
		if len(untrusted) != 1 || !untrusted[0].Valid() || untrusted[0].Trusted() || !errors.Is(untrusted[0].TrustErr, ErrCertificateUntrusted) {
			t.Fatalf("valid signature, untrusted certificate: %+v", untrusted)
		}
		if unknown := (x509.UnknownAuthorityError{}); !errors.As(untrusted[0].TrustErr, &unknown) {
			t.Fatalf("crypto/x509 error not wrapped: %v", untrusted[0].TrustErr)
		}
		obj.Put(dicomtest.StringElement(tagPatientName, core.VRPN, "TEST^SIGM"))
		tampered := VerifyWithTrust(obj, base)
		if len(tampered) != 1 || tampered[0].Valid() || tampered[0].TrustErr != nil || tampered[0].Trusted() {
			t.Fatalf("invalid signature, trusted certificate: %+v", tampered)
		}
		if obj := newObject(testElements(false), transfer.ExplicitVRLittleEndian); len(VerifyWithTrust(obj, base)) != 0 {
			t.Fatal("results for an unsigned object")
		}
	})

	t.Run("unreadable certificate", func(t *testing.T) {
		if _, err := VerifyCertificate(Signature{}, base); !errors.Is(err, ErrCertificateUntrusted) || !errors.Is(err, ErrMalformedSignature) {
			t.Fatalf("no certificate: %v", err)
		}
	})
}
