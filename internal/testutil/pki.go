package testutil

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
	"math/big"
	"net"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// TestPKI is a synthetic certification authority generated at test time,
// with PEM files for tools such as DCMTK. It holds no real identities.
type TestPKI struct {
	Dir    string
	CA     *x509.Certificate
	CAPool *x509.CertPool
	CAFile string
	caKey  crypto.Signer
	serial int64
}

// TestCredential is a certificate and key issued by a TestPKI.
type TestCredential struct {
	Certificate tls.Certificate
	CertFile    string
	KeyFile     string
}

// NewTestPKI creates a P-256 CA in a temporary directory.
func NewTestPKI(t testing.TB) *TestPKI {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	template := &x509.Certificate{
		SerialNumber:          big.NewInt(1),
		Subject:               pkix.Name{CommonName: "dicom-go test CA"},
		NotBefore:             time.Now().Add(-time.Hour),
		NotAfter:              time.Now().Add(24 * time.Hour),
		IsCA:                  true,
		BasicConstraintsValid: true,
		KeyUsage:              x509.KeyUsageCertSign | x509.KeyUsageDigitalSignature,
	}
	der, err := x509.CreateCertificate(rand.Reader, template, template, key.Public(), key)
	if err != nil {
		t.Fatal(err)
	}
	ca, err := x509.ParseCertificate(der)
	if err != nil {
		t.Fatal(err)
	}
	pki := &TestPKI{Dir: t.TempDir(), CA: ca, CAPool: x509.NewCertPool(), caKey: key, serial: 1}
	pki.CAPool.AddCert(ca)
	pki.CAFile = filepath.Join(pki.Dir, "ca.pem")
	writePEM(t, pki.CAFile, "CERTIFICATE", der)
	return pki
}

// Issue signs a leaf certificate for name (also a DNS SAN, with 127.0.0.1 as
// an IP SAN) with a key of the given kind: "rsa2048", "rsa1024" or "p256".
func (p *TestPKI) Issue(t testing.TB, name, keyKind string) TestCredential {
	t.Helper()
	var key crypto.Signer
	var err error
	switch keyKind {
	case "rsa2048":
		key, err = rsa.GenerateKey(rand.Reader, 2048)
	case "rsa1024":
		key, err = rsa.GenerateKey(rand.Reader, 1024)
	case "p256":
		key, err = ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	default:
		t.Fatalf("unknown key kind %q", keyKind)
	}
	if err != nil {
		t.Fatal(err)
	}
	p.serial++
	template := &x509.Certificate{
		SerialNumber: big.NewInt(p.serial),
		Subject:      pkix.Name{CommonName: name},
		DNSNames:     []string{name},
		IPAddresses:  []net.IP{net.ParseIP("127.0.0.1")},
		NotBefore:    time.Now().Add(-time.Hour),
		NotAfter:     time.Now().Add(24 * time.Hour),
		KeyUsage:     x509.KeyUsageDigitalSignature | x509.KeyUsageKeyEncipherment,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth, x509.ExtKeyUsageClientAuth},
	}
	der, err := x509.CreateCertificate(rand.Reader, template, p.CA, key.Public(), p.caKey)
	if err != nil {
		t.Fatal(err)
	}
	keyDER, err := x509.MarshalPKCS8PrivateKey(key)
	if err != nil {
		t.Fatal(err)
	}
	credential := TestCredential{
		CertFile: filepath.Join(p.Dir, name+"-"+keyKind+".pem"),
		KeyFile:  filepath.Join(p.Dir, name+"-"+keyKind+".key"),
	}
	writePEM(t, credential.CertFile, "CERTIFICATE", der)
	writePEM(t, credential.KeyFile, "PRIVATE KEY", keyDER)
	credential.Certificate, err = tls.LoadX509KeyPair(credential.CertFile, credential.KeyFile)
	if err != nil {
		t.Fatal(err)
	}
	return credential
}

func writePEM(t testing.TB, path, blockType string, der []byte) {
	t.Helper()
	data := pem.EncodeToMemory(&pem.Block{Type: blockType, Bytes: der})
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatal(err)
	}
}
