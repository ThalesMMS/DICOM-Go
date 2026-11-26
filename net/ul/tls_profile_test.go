package ul

import (
	"crypto/tls"
	"crypto/x509"
	"errors"
	"net"
	"strings"
	"testing"

	"github.com/ThalesMMS/dicom-go/internal/testutil"
)

func TestClientTLSConfigRequiresExplicitPeerTrust(t *testing.T) {
	for _, test := range []struct {
		name      string
		options   TLSProfileOptions
		wantError bool
	}{
		{"system roots without name", TLSProfileOptions{}, true},
		{"blank name", TLSProfileOptions{ServerName: " \t "}, true},
		{"optional verification without name", TLSProfileOptions{PeerAuthentication: TLSVerifyPeerCertificateIfGiven}, true},
		{"system roots with name", TLSProfileOptions{ServerName: " scp.test "}, false},
		{"explicit roots without name", TLSProfileOptions{TrustedCAs: x509.NewCertPool()}, false},
		{"ignored peer without name", TLSProfileOptions{PeerAuthentication: TLSIgnorePeerCertificate}, false},
	} {
		t.Run(test.name, func(t *testing.T) {
			config, err := ClientTLSConfig(test.options)
			if (err != nil) != test.wantError {
				t.Fatalf("ClientTLSConfig() error = %v, want error %v", err, test.wantError)
			}
			if err == nil && (config.VerifyConnection == nil || config.ServerName != strings.TrimSpace(test.options.ServerName)) {
				t.Fatalf("verification or server name missing: %+v", config)
			}
		})
	}
}

func TestParseTLSProfile(t *testing.T) {
	for input, want := range map[string]TLSProfile{
		"":                        DefaultTLSProfile,
		"bcp195-rfc8996":          TLSProfileBCP195RFC8996,
		"8996":                    TLSProfileBCP195RFC8996,
		"8996-mod":                TLSProfileModifiedBCP195RFC8996,
		"BCP195-RFC8996-MODIFIED": TLSProfileModifiedBCP195RFC8996,
		"bcp195":                  TLSProfileBCP195,
		"bcp195-nd":               TLSProfileNonDowngradingBCP195,
		"bcp195-ex":               TLSProfileExtendedBCP195,
		"bcp195-extended":         TLSProfileExtendedBCP195,
	} {
		got, err := ParseTLSProfile(input)
		if err != nil || got != want {
			t.Errorf("ParseTLSProfile(%q) = %q, %v; want %q", input, got, err, want)
		}
	}
	if _, err := ParseTLSProfile("null"); !errors.Is(err, ErrUnknownTLSProfile) {
		t.Fatalf("null profile: %v", err)
	}
	for _, profile := range TLSProfiles() {
		if profile.Section() == "" || profile.Title() == "" || len(profile.CipherSuites()) == 0 {
			t.Errorf("profile %q lacks metadata", profile)
		}
		for _, id := range profile.CipherSuites() {
			suite := tls.CipherSuiteName(id)
			if !strings.HasPrefix(suite, "TLS_ECDHE_") || strings.HasSuffix(suite, "_SHA") ||
				strings.Contains(suite, "CBC") || strings.Contains(suite, "NULL") {
				t.Errorf("profile %q offers %s", profile, suite)
			}
		}
	}
}

type tlsFixture struct {
	pki    *testutil.TestPKI
	server testutil.TestCredential
	client testutil.TestCredential
}

func newTLSFixture(t *testing.T) tlsFixture {
	t.Helper()
	pki := testutil.NewTestPKI(t)
	return tlsFixture{pki: pki, server: pki.Issue(t, "scp.test", "rsa2048"), client: pki.Issue(t, "scu.test", "p256")}
}

// handshake runs one TLS handshake between the configurations and returns
// the client's connection state and both errors.
func handshake(t *testing.T, serverConfig, clientConfig *tls.Config) (tls.ConnectionState, error, error) {
	t.Helper()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = listener.Close() }()
	serverErr := make(chan error, 1)
	go func() {
		conn, err := listener.Accept()
		if err != nil {
			serverErr <- err
			return
		}
		defer func() { _ = conn.Close() }()
		serverErr <- tls.Server(conn, serverConfig).Handshake()
	}()
	conn, err := net.Dial("tcp", listener.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	client := tls.Client(conn, clientConfig)
	clientErr := client.Handshake()
	state := client.ConnectionState()
	_ = client.Close()
	return state, clientErr, <-serverErr
}

func TestTLSProfilesMutualAuthentication(t *testing.T) {
	fixture := newTLSFixture(t)
	for _, profile := range TLSProfiles() {
		for _, maxVersion := range []uint16{tls.VersionTLS13, tls.VersionTLS12} {
			server, err := ServerTLSConfig(TLSProfileOptions{Profile: profile, Certificates: []tls.Certificate{fixture.server.Certificate}, TrustedCAs: fixture.pki.CAPool})
			if err != nil {
				t.Fatal(err)
			}
			client, err := ClientTLSConfig(TLSProfileOptions{Profile: profile, Certificates: []tls.Certificate{fixture.client.Certificate}, TrustedCAs: fixture.pki.CAPool})
			if err != nil {
				t.Fatal(err)
			}
			client.MaxVersion = maxVersion
			state, clientErr, serverErr := handshake(t, server, client)
			if clientErr != nil || serverErr != nil {
				t.Fatalf("%s max %x: client %v, server %v", profile, maxVersion, clientErr, serverErr)
			}
			if state.Version != maxVersion {
				t.Fatalf("%s: negotiated %x, want %x", profile, state.Version, maxVersion)
			}
			if maxVersion == tls.VersionTLS12 {
				allowed := false
				for _, id := range profile.CipherSuites() {
					allowed = allowed || id == state.CipherSuite
				}
				if !allowed {
					t.Fatalf("%s: negotiated %s outside the profile", profile, tls.CipherSuiteName(state.CipherSuite))
				}
			}
		}
	}
}

func TestTLSProfileRejectsForbiddenSuitesAndVersions(t *testing.T) {
	fixture := newTLSFixture(t)
	server, err := ServerTLSConfig(TLSProfileOptions{Certificates: []tls.Certificate{fixture.server.Certificate}, PeerAuthentication: TLSIgnorePeerCertificate})
	if err != nil {
		t.Fatal(err)
	}
	for name, legacy := range map[string]*tls.Config{
		"ECDHE-CBC-SHA1":   {InsecureSkipVerify: true, MaxVersion: tls.VersionTLS12, CipherSuites: []uint16{tls.TLS_ECDHE_RSA_WITH_AES_128_CBC_SHA}},
		"RSA key exchange": {InsecureSkipVerify: true, MaxVersion: tls.VersionTLS12, CipherSuites: []uint16{tls.TLS_RSA_WITH_AES_128_GCM_SHA256}},
		"TLS 1.1":          {InsecureSkipVerify: true, MinVersion: tls.VersionTLS10, MaxVersion: tls.VersionTLS11},
	} {
		if _, clientErr, serverErr := handshake(t, server, legacy); clientErr == nil || serverErr == nil {
			t.Errorf("%s: the profile server accepted a forbidden handshake", name)
		}
	}
	// A client refuses a server that only offers a forbidden suite.
	legacyServer := &tls.Config{Certificates: []tls.Certificate{fixture.server.Certificate}, MaxVersion: tls.VersionTLS12, CipherSuites: []uint16{tls.TLS_ECDHE_RSA_WITH_AES_128_CBC_SHA}}
	client, err := ClientTLSConfig(TLSProfileOptions{TrustedCAs: fixture.pki.CAPool})
	if err != nil {
		t.Fatal(err)
	}
	if _, clientErr, _ := handshake(t, legacyServer, client); clientErr == nil {
		t.Fatal("the profile client accepted a CBC-only server")
	}
}

func TestTLSProfilePeerAuthentication(t *testing.T) {
	fixture := newTLSFixture(t)
	other := testutil.NewTestPKI(t)
	serverFor := func(mode TLSPeerAuthentication) *tls.Config {
		config, err := ServerTLSConfig(TLSProfileOptions{Certificates: []tls.Certificate{fixture.server.Certificate}, TrustedCAs: fixture.pki.CAPool, PeerAuthentication: mode})
		if err != nil {
			t.Fatal(err)
		}
		return config
	}
	clientFor := func(opts TLSProfileOptions) *tls.Config {
		config, err := ClientTLSConfig(opts)
		if err != nil {
			t.Fatal(err)
		}
		return config
	}
	withCert := []tls.Certificate{fixture.client.Certificate}
	for _, tc := range []struct {
		name   string
		server *tls.Config
		client *tls.Config
		ok     bool
	}{
		{"mutual", serverFor(TLSRequirePeerCertificate), clientFor(TLSProfileOptions{Certificates: withCert, TrustedCAs: fixture.pki.CAPool}), true},
		{"client without certificate", serverFor(TLSRequirePeerCertificate), clientFor(TLSProfileOptions{TrustedCAs: fixture.pki.CAPool}), false},
		{"optional client certificate", serverFor(TLSVerifyPeerCertificateIfGiven), clientFor(TLSProfileOptions{TrustedCAs: fixture.pki.CAPool}), true},
		{"server ignores client", serverFor(TLSIgnorePeerCertificate), clientFor(TLSProfileOptions{TrustedCAs: fixture.pki.CAPool}), true},
		{"untrusted server", serverFor(TLSIgnorePeerCertificate), clientFor(TLSProfileOptions{TrustedCAs: other.CAPool}), false},
		{"client ignores server", serverFor(TLSIgnorePeerCertificate), clientFor(TLSProfileOptions{TrustedCAs: other.CAPool, PeerAuthentication: TLSIgnorePeerCertificate}), true},
		{"matching server name", serverFor(TLSIgnorePeerCertificate), clientFor(TLSProfileOptions{TrustedCAs: fixture.pki.CAPool, ServerName: "scp.test"}), true},
		{"wrong server name", serverFor(TLSIgnorePeerCertificate), clientFor(TLSProfileOptions{TrustedCAs: fixture.pki.CAPool, ServerName: "other.test"}), false},
	} {
		_, clientErr, serverErr := handshake(t, tc.server, tc.client)
		if ok := clientErr == nil && serverErr == nil; ok != tc.ok {
			t.Errorf("%s: client %v, server %v; want success %v", tc.name, clientErr, serverErr, tc.ok)
		}
	}
	if _, err := ServerTLSConfig(TLSProfileOptions{Certificates: []tls.Certificate{fixture.server.Certificate}}); err == nil {
		t.Fatal("mutual authentication without trusted CAs was accepted")
	}
	if _, err := ServerTLSConfig(TLSProfileOptions{TrustedCAs: fixture.pki.CAPool}); err == nil {
		t.Fatal("a server without a certificate was accepted")
	}
}

func TestTLSProfileRejectsWeakKeys(t *testing.T) {
	fixture := newTLSFixture(t)
	weak := fixture.pki.Issue(t, "weak.test", "rsa1024")
	if _, err := ServerTLSConfig(TLSProfileOptions{Certificates: []tls.Certificate{weak.Certificate}, PeerAuthentication: TLSIgnorePeerCertificate}); !errors.Is(err, ErrWeakTLSCertificate) {
		t.Fatalf("1024-bit server certificate: %v", err)
	}
	weakServer := &tls.Config{Certificates: []tls.Certificate{weak.Certificate}}
	client, err := ClientTLSConfig(TLSProfileOptions{TrustedCAs: fixture.pki.CAPool, PeerAuthentication: TLSIgnorePeerCertificate})
	if err != nil {
		t.Fatal(err)
	}
	if _, clientErr, _ := handshake(t, weakServer, client); !errors.Is(clientErr, ErrWeakTLSCertificate) {
		t.Fatalf("client accepted a 1024-bit server key: %v", clientErr)
	}
}

// TestTLSProfileAssociation runs a full association over the profile
// configurations with mutual authentication.
func TestTLSProfileAssociation(t *testing.T) {
	fixture := newTLSFixture(t)
	server, err := ServerTLSConfig(TLSProfileOptions{Certificates: []tls.Certificate{fixture.server.Certificate}, TrustedCAs: fixture.pki.CAPool})
	if err != nil {
		t.Fatal(err)
	}
	client, err := ClientTLSConfig(TLSProfileOptions{Certificates: []tls.Certificate{fixture.client.Certificate}, TrustedCAs: fixture.pki.CAPool})
	if err != nil {
		t.Fatal(err)
	}
	addr, done := acceptOnce(t, AcceptOptions{AETitle: "ARCHIVE", AcceptAnyAbstractSyntax: true, SupportedTransferSyntaxes: []string{ImplicitVRLittleEndian}, TLSConfig: server})
	assoc, err := Dial(addr, DialOptions{
		CalledAETitle: "ARCHIVE", CallingAETitle: "SCU", TLSConfig: client,
		Contexts: []PresentationContext{{AbstractSyntaxUID: verificationSOPClassUID, TransferSyntaxUIDs: []string{ImplicitVRLittleEndian}}},
	})
	if err != nil {
		t.Fatal(err)
	}
	_ = assoc.Close()
	if err := <-done; err != nil {
		t.Fatal(err)
	}
}
