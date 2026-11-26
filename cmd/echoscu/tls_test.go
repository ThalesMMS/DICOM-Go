package main

import (
	"bytes"
	"crypto/tls"
	"net"
	"testing"

	"github.com/ThalesMMS/dicom-go/internal/testutil"
	"github.com/ThalesMMS/dicom-go/net/dimse"
	"github.com/ThalesMMS/dicom-go/net/ul"
)

// TestRunEchoOverTLSProfile: echoscu -tls-profile completes a C-ECHO with
// mutual authentication, and fails without its client certificate.
func TestRunEchoOverTLSProfile(t *testing.T) {
	pki := testutil.NewTestPKI(t)
	server := pki.Issue(t, "scp.test", "rsa2048")
	client := pki.Issue(t, "scu.test", "p256")
	serverConfig, err := ul.ServerTLSConfig(ul.TLSProfileOptions{Certificates: []tls.Certificate{server.Certificate}, TrustedCAs: pki.CAPool})
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name string
		args []string
		code int
	}{
		{"mutual", []string{"-tls-profile", "bcp195-rfc8996", "-ca", pki.CAFile, "-cert", client.CertFile, "-key", client.KeyFile}, 0},
		{"modified profile", []string{"-tls-profile", "8996-mod", "-ca", pki.CAFile, "-cert", client.CertFile, "-key", client.KeyFile, "-tls-server-name", "scp.test"}, 0},
		{"no client certificate", []string{"-tls-profile", "bcp195-rfc8996", "-ca", pki.CAFile}, 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			listener, err := ul.Listen(ul.ListenOptions{Address: "127.0.0.1:0"})
			if err != nil {
				t.Fatal(err)
			}
			defer func() { _ = listener.Close() }()
			go func() {
				assoc, err := listener.AcceptAssociation(ul.AcceptOptions{
					AETitle: "ANY-SCP", TLSConfig: serverConfig,
					SupportedAbstractSyntaxes: []string{dimse.VerificationSOPClassUID},
					SupportedTransferSyntaxes: []string{ul.ImplicitVRLittleEndian},
				})
				if err != nil {
					return
				}
				defer func() { _ = assoc.Close() }()
				if dimse.HandleCEcho(assoc) == nil {
					if pdu, err := assoc.ReadPDU(); err == nil {
						if _, ok := pdu.(*ul.ReleaseRQ); ok {
							_ = assoc.WritePDU(&ul.ReleaseRP{})
						}
					}
				}
			}()
			_, port, _ := net.SplitHostPort(listener.Addr().String())
			args := append([]string{"-host", "127.0.0.1", "-port", port}, tc.args...)
			var stdout, stderr bytes.Buffer
			if code := run(args, &stdout, &stderr); code != tc.code {
				t.Fatalf("exit %d, want %d; stderr=%s", code, tc.code, stderr.String())
			}
		})
	}
}

func TestParseArgsTLSFlags(t *testing.T) {
	for _, tc := range []struct {
		args []string
		ok   bool
	}{
		{[]string{"-cert", "c.pem", "-key", "k.pem"}, false},
		{[]string{"-tls-profile", "null"}, false},
		{[]string{"-tls-profile", "bcp195-nd", "-cert", "c.pem"}, false},
		{[]string{"-tls-profile", "bcp195-nd", "-require-peer-cert=false"}, true},
	} {
		_, err := parseArgs(tc.args, &bytes.Buffer{})
		if (err == nil) != tc.ok {
			t.Errorf("parseArgs(%q) error %v, want ok %v", tc.args, err, tc.ok)
		}
	}
}
