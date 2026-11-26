package dicomwebcli

import (
	"bytes"
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"math/big"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

const dicomwebCLITestPassword = "synthetic-secret-value"

// dicomwebCLITestCertificate writes a certificate and its key as PEM files and
// returns their paths. A nil parent makes a self-signed CA.
func dicomwebCLITestCertificate(t *testing.T, directory, name string, parent *x509.Certificate, parentKey *ecdsa.PrivateKey) (string, string, *x509.Certificate, *ecdsa.PrivateKey) {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	template := &x509.Certificate{
		SerialNumber: big.NewInt(time.Now().UnixNano()),
		Subject:      pkix.Name{CommonName: name},
		NotBefore:    time.Now().Add(-time.Hour),
		NotAfter:     time.Now().Add(time.Hour),
		KeyUsage:     x509.KeyUsageDigitalSignature,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth, x509.ExtKeyUsageServerAuth},
	}
	signer, signerKey := template, key
	if parent == nil {
		template.IsCA, template.BasicConstraintsValid = true, true
		template.KeyUsage |= x509.KeyUsageCertSign
	} else {
		signer, signerKey = parent, parentKey
	}
	der, err := x509.CreateCertificate(rand.Reader, template, signer, &key.PublicKey, signerKey)
	if err != nil {
		t.Fatal(err)
	}
	certificate, err := x509.ParseCertificate(der)
	if err != nil {
		t.Fatal(err)
	}
	keyDER, err := x509.MarshalPKCS8PrivateKey(key)
	if err != nil {
		t.Fatal(err)
	}
	certPath, keyPath := filepath.Join(directory, name+".crt"), filepath.Join(directory, name+".key")
	if err := os.WriteFile(certPath, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(keyPath, pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: keyDER}), 0o600); err != nil {
		t.Fatal(err)
	}
	return certPath, keyPath, certificate, key
}

func dicomwebCLITestServerCA(t *testing.T, server *httptest.Server) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "server-ca.crt")
	block := &pem.Block{Type: "CERTIFICATE", Bytes: server.Certificate().Raw}
	if err := os.WriteFile(path, pem.EncodeToMemory(block), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

func dicomwebCLITestVerify(t *testing.T, args ...string) (int, string) {
	t.Helper()
	var stdout, stderr bytes.Buffer
	code := RunWithContext(context.Background(), append([]string{"verify", "-timeout", "5s"}, args...), &stdout, &stderr)
	for _, secret := range []string{dicomwebCLITestPassword, "PRIVATE KEY", ".crt", ".key"} {
		if strings.Contains(stderr.String(), secret) || strings.Contains(stdout.String(), secret) {
			t.Fatalf("the diagnostic exposes %q: %q", secret, stderr.String())
		}
	}
	return code, stderr.String()
}

// The private-CA server accepts a client only when -ca names its issuer, and
// each failure below HTTP names its own cause.
func TestTLSFlagsAndTransportDiagnostics(t *testing.T) {
	t.Setenv("SYNTHETIC_DICOMWEB_PASSWORD", dicomwebCLITestPassword)
	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		user, password, ok := r.BasicAuth()
		if !ok || user != "synthetic" || password != dicomwebCLITestPassword {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		w.Header().Set("Content-Type", "application/dicom+json")
		_, _ = w.Write([]byte("[]"))
	})
	server := httptest.NewTLSServer(handler)
	defer server.Close()
	ca := dicomwebCLITestServerCA(t, server)
	basic := []string{"-basic-user", "synthetic", "-basic-password-env", "SYNTHETIC_DICOMWEB_PASSWORD"}
	with := func(args ...string) []string {
		return append(append([]string{"-base-url", server.URL}, basic...), args...)
	}

	if code, diagnostic := dicomwebCLITestVerify(t, with("-ca", ca)...); code != ExitOK {
		t.Fatalf("verify with -ca: code=%d stderr=%q", code, diagnostic)
	}
	// The test certificate also names example.com.
	if code, diagnostic := dicomwebCLITestVerify(t, with("-ca", ca, "-tls-server-name", "example.com")...); code != ExitOK {
		t.Fatalf("verify with -tls-server-name: code=%d stderr=%q", code, diagnostic)
	}

	otherDirectory := t.TempDir()
	otherCA, _, _, _ := dicomwebCLITestCertificate(t, otherDirectory, "other-ca", nil, nil)
	closed, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	closedURL := "https://" + closed.Addr().String()
	_ = closed.Close()
	plain := httptest.NewServer(handler)
	defer plain.Close()

	for _, test := range []struct {
		name string
		args []string
		exit int
		want string
	}{
		{name: "unknown CA without -ca", args: with(), exit: ExitFailure, want: "cause=tls_unknown_authority"},
		{name: "unknown CA with another -ca", args: with("-ca", otherCA), exit: ExitFailure, want: "cause=tls_unknown_authority"},
		{name: "repeated -ca", args: with("-ca", otherCA, "-ca", ca), exit: ExitOK},
		{name: "wrong server name", args: with("-ca", ca, "-tls-server-name", "wrong.invalid"), exit: ExitFailure, want: "cause=tls_server_name_mismatch"},
		{name: "wrong password", args: []string{"-base-url", server.URL, "-ca", ca, "-basic-user", "other", "-basic-password-env", "SYNTHETIC_DICOMWEB_PASSWORD"}, exit: ExitAuth, want: "status=401: the server rejected"},
		{name: "no credentials", args: []string{"-base-url", server.URL, "-ca", ca}, exit: ExitAuth, want: "kind=auth_status"},
		{name: "connection refused", args: []string{"-base-url", closedURL, "-ca", ca}, exit: ExitFailure, want: "cause=connection_refused"},
		{name: "plain HTTP server", args: []string{"-base-url", strings.Replace(plain.URL, "http://", "https://", 1), "-ca", ca}, exit: ExitFailure, want: "cause=tls_not_offered_by_server"},
		{name: "unresolved host", args: []string{"-base-url", "https://no-such-host.invalid", "-ca", ca}, exit: ExitFailure, want: "cause=host_not_resolved"},
	} {
		t.Run(test.name, func(t *testing.T) {
			code, diagnostic := dicomwebCLITestVerify(t, test.args...)
			if code != test.exit || !strings.Contains(diagnostic, test.want) {
				t.Fatalf("code=%d stderr=%q, want exit %d with %q", code, diagnostic, test.exit, test.want)
			}
			if test.exit == ExitFailure && !strings.Contains(diagnostic, "kind=request_failure endpoint=https://") {
				t.Fatalf("the diagnostic lost its class or endpoint: %q", diagnostic)
			}
		})
	}
}

// A server that requires a client certificate accepts -cert and -key issued
// by its CA and refuses the handshake without them.
func TestClientCertificateFlags(t *testing.T) {
	directory := t.TempDir()
	clientCA, _, caCertificate, caKey := dicomwebCLITestCertificate(t, directory, "client-ca", nil, nil)
	cert, key, _, _ := dicomwebCLITestCertificate(t, directory, "client", caCertificate, caKey)
	_, otherKey, _, _ := dicomwebCLITestCertificate(t, directory, "unrelated", nil, nil)
	_ = clientCA
	pool := x509.NewCertPool()
	pool.AddCert(caCertificate)
	server := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if len(r.TLS.PeerCertificates) != 1 {
			t.Errorf("request arrived without a client certificate")
		}
		w.Header().Set("Content-Type", "application/dicom+json")
		_, _ = w.Write([]byte("[]"))
	}))
	server.TLS = &tls.Config{ClientAuth: tls.RequireAndVerifyClientCert, ClientCAs: pool}
	server.StartTLS()
	defer server.Close()
	ca := dicomwebCLITestServerCA(t, server)

	if code, diagnostic := dicomwebCLITestVerify(t, "-base-url", server.URL, "-ca", ca, "-cert", cert, "-key", key); code != ExitOK {
		t.Fatalf("verify with a client certificate: code=%d stderr=%q", code, diagnostic)
	}
	code, diagnostic := dicomwebCLITestVerify(t, "-base-url", server.URL, "-ca", ca)
	if code != ExitFailure || !strings.Contains(diagnostic, "cause=tls_rejected_by_server") {
		t.Fatalf("verify without a client certificate: code=%d stderr=%q", code, diagnostic)
	}
	for _, test := range []struct {
		name string
		args []string
		want string
	}{
		{name: "certificate without key", args: []string{"-cert", cert}, want: "-cert and -key must be given together"},
		{name: "key of another certificate", args: []string{"-cert", cert, "-key", otherKey}, want: "cannot be loaded as a matching"},
		{name: "missing CA file", args: []string{"-ca", filepath.Join(directory, "absent.crt")}, want: "a -ca file cannot be read"},
		{name: "CA file without a certificate", args: []string{"-ca", key}, want: "a -ca file holds no PEM certificate"},
	} {
		t.Run(test.name, func(t *testing.T) {
			code, diagnostic := dicomwebCLITestVerify(t, append([]string{"-base-url", server.URL}, test.args...)...)
			if code != ExitInput || !strings.Contains(diagnostic, test.want) {
				t.Fatalf("code=%d stderr=%q, want an input error with %q", code, diagnostic, test.want)
			}
		})
	}
	code, diagnostic = dicomwebCLITestVerify(t, "-base-url", "http://127.0.0.1:1", "-ca", ca)
	if code != ExitInput || !strings.Contains(diagnostic, "require an HTTPS -base-url") {
		t.Fatalf("TLS flags on an HTTP URL: code=%d stderr=%q", code, diagnostic)
	}
}
