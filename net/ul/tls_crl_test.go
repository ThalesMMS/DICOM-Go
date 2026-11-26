package ul

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"errors"
	"fmt"
	"io"
	"math/big"
	"net"
	"sync"
	"testing"
	"time"
)

type tlsCRLFixture struct {
	root, intermediate       *x509.Certificate
	rootKey, intermediateKey *ecdsa.PrivateKey
	server, client           tls.Certificate
	pool                     *x509.CertPool
}

func newTLSCRLFixture(t *testing.T) tlsCRLFixture {
	t.Helper()
	issue := func(serial int64, parent *x509.Certificate, signer *ecdsa.PrivateKey, ca bool) (*x509.Certificate, *ecdsa.PrivateKey) {
		key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
		if err != nil {
			t.Fatal(err)
		}
		tmpl := &x509.Certificate{SerialNumber: big.NewInt(serial), Subject: pkix.Name{CommonName: fmt.Sprintf("synthetic TLS %d", serial)}, NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(24 * time.Hour), DNSNames: []string{"scp.test"}, IPAddresses: []net.IP{net.ParseIP("127.0.0.1")}, KeyUsage: x509.KeyUsageDigitalSignature, ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth, x509.ExtKeyUsageClientAuth}, IsCA: ca, BasicConstraintsValid: true, SubjectKeyId: []byte{byte(serial)}}
		if ca {
			tmpl.KeyUsage |= x509.KeyUsageCertSign | x509.KeyUsageCRLSign
		}
		if parent == nil {
			parent, signer = tmpl, key
		}
		der, err := x509.CreateCertificate(rand.Reader, tmpl, parent, &key.PublicKey, signer)
		if err != nil {
			t.Fatal(err)
		}
		cert, err := x509.ParseCertificate(der)
		if err != nil {
			t.Fatal(err)
		}
		return cert, key
	}
	root, rkey := issue(1, nil, nil, true)
	inter, ikey := issue(2, root, rkey, true)
	server, skey := issue(3, inter, ikey, false)
	client, ckey := issue(4, inter, ikey, false)
	pool := x509.NewCertPool()
	pool.AddCert(root)
	return tlsCRLFixture{root, inter, rkey, ikey, tls.Certificate{Certificate: [][]byte{server.Raw, inter.Raw}, PrivateKey: skey, Leaf: server}, tls.Certificate{Certificate: [][]byte{client.Raw, inter.Raw}, PrivateKey: ckey, Leaf: client}, pool}
}

func (f tlsCRLFixture) list(t *testing.T, root bool, revoked ...*x509.Certificate) *x509.RevocationList {
	t.Helper()
	issuer, key := f.intermediate, f.intermediateKey
	if root {
		issuer, key = f.root, f.rootKey
	}
	now := time.Now().UTC().Truncate(time.Second)
	template := &x509.RevocationList{Number: big.NewInt(1), ThisUpdate: now.Add(-time.Hour), NextUpdate: now.Add(time.Hour)}
	for _, cert := range revoked {
		template.RevokedCertificateEntries = append(template.RevokedCertificateEntries, x509.RevocationListEntry{SerialNumber: cert.SerialNumber, RevocationTime: now.Add(-time.Minute)})
	}
	der, err := x509.CreateRevocationList(rand.Reader, template, issuer, key)
	if err != nil {
		t.Fatal(err)
	}
	list, err := x509.ParseRevocationList(der)
	if err != nil {
		t.Fatal(err)
	}
	return list
}

func (f tlsCRLFixture) config(t *testing.T, server bool, mode TLSCRLMode, lists ...*x509.RevocationList) *tls.Config {
	t.Helper()
	opts := TLSProfileOptions{TrustedCAs: f.pool, ServerName: "scp.test"}
	if mode != TLSCRLNone {
		opts.Revocation = &TLSRevocationOptions{Mode: mode, CRLs: lists}
	}
	var config *tls.Config
	var err error
	if server {
		opts.Certificates = []tls.Certificate{f.server}
		config, err = ServerTLSConfig(opts)
	} else {
		opts.Certificates = []tls.Certificate{f.client}
		config, err = ClientTLSConfig(opts)
	}
	if err != nil {
		t.Fatal(err)
	}
	return config
}

func tlsCRLKind(t *testing.T, err error, kind TLSRevocationKind) {
	t.Helper()
	var e *TLSRevocationError
	if !errors.As(err, &e) || e.Kind != kind {
		t.Fatalf("error=%v want CRL kind %s", err, kind)
	}
	if kind == TLSRevocationRevoked && !errors.Is(err, ErrTLSCertificateRevoked) {
		t.Fatal("revoked sentinel lost")
	}
}

func TestTLSCRLPolicyDefaultAndPeerVerification(t *testing.T) {
	f := newTLSCRLFixture(t)
	// Nil preserves existing mutual-authentication behavior.
	_, ce, se := handshake(t, f.config(t, true, TLSCRLNone), f.config(t, false, TLSCRLNone))
	if ce != nil || se != nil {
		t.Fatalf("default: client=%v server=%v", ce, se)
	}
	for _, mode := range []TLSCRLMode{TLSCRLLeaf, TLSCRLChain} {
		t.Run(map[TLSCRLMode]string{TLSCRLLeaf: "leaf", TLSCRLChain: "chain"}[mode], func(t *testing.T) {
			lists := []*x509.RevocationList{f.list(t, false)}
			if mode == TLSCRLChain {
				lists = append(lists, f.list(t, true))
			}
			_, ce, se := handshake(t, f.config(t, true, mode, lists...), f.config(t, false, mode, lists...))
			if ce != nil || se != nil {
				t.Fatalf("valid: client=%v server=%v", ce, se)
			}
		})
	}
	_, ce, _ = handshake(t, f.config(t, true, TLSCRLNone), f.config(t, false, TLSCRLLeaf, f.list(t, false, f.server.Leaf)))
	tlsCRLKind(t, ce, TLSRevocationRevoked)
	_, _, se = handshake(t, f.config(t, true, TLSCRLLeaf, f.list(t, false, f.client.Leaf)), f.config(t, false, TLSCRLNone))
	tlsCRLKind(t, se, TLSRevocationRevoked)
	chainRevoked := []*x509.RevocationList{f.list(t, false), f.list(t, true, f.intermediate)}
	_, ce, _ = handshake(t, f.config(t, true, TLSCRLNone), f.config(t, false, TLSCRLChain, chainRevoked...))
	tlsCRLKind(t, ce, TLSRevocationRevoked)
	_, ce, se = handshake(t, f.config(t, true, TLSCRLNone), f.config(t, false, TLSCRLLeaf, chainRevoked...))
	if ce != nil || se != nil {
		t.Fatalf("leaf checked anchor/intermediate: %v %v", ce, se)
	}
	other := newTLSCRLFixture(t)
	_, ce, _ = handshake(t, f.config(t, true, TLSCRLNone), f.config(t, false, TLSCRLLeaf, other.list(t, true)))
	tlsCRLKind(t, ce, TLSRevocationIssuerMismatch)
	// Ordinary trust remains authoritative; a current clean CRL cannot bypass it.
	config := f.config(t, false, TLSCRLLeaf, f.list(t, false))
	state := tls.ConnectionState{PeerCertificates: []*x509.Certificate{other.server.Leaf, other.intermediate}}
	err := config.VerifyConnection(state)
	var unknown x509.UnknownAuthorityError
	if !errors.As(err, &unknown) {
		t.Fatalf("normal trust error lost: %v", err)
	}
	for _, opts := range []TLSProfileOptions{
		{TrustedCAs: f.pool, Revocation: &TLSRevocationOptions{Mode: TLSCRLLeaf}},
		{TrustedCAs: f.pool, PeerAuthentication: TLSIgnorePeerCertificate, Revocation: &TLSRevocationOptions{Mode: TLSCRLLeaf, CRLs: []*x509.RevocationList{f.list(t, false)}}},
		{TrustedCAs: f.pool, Revocation: &TLSRevocationOptions{Mode: TLSCRLNone, CRLs: []*x509.RevocationList{f.list(t, false)}}},
	} {
		if _, err := ClientTLSConfig(opts); err == nil {
			t.Fatal("invalid policy accepted")
		}
	}
}

func TestTLSCRLCallerMutationCannotChangeSnapshot(t *testing.T) {
	f := newTLSCRLFixture(t)
	good := f.list(t, false)
	config := f.config(t, false, TLSCRLLeaf, good)
	state := tls.ConnectionState{PeerCertificates: []*x509.Certificate{f.server.Leaf, f.intermediate}}
	other := newTLSCRLFixture(t)
	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := 0; j < 20; j++ {
				if err := config.VerifyConnection(state); err != nil {
					t.Errorf("snapshot: %v", err)
				}
			}
		}()
	}
	f.pool.AddCert(other.root)
	for i := range good.Raw {
		good.Raw[i] = 0
	}
	good.NextUpdate = time.Time{}
	wg.Wait()
	var unknown x509.UnknownAuthorityError
	if err := config.VerifyConnection(tls.ConnectionState{PeerCertificates: []*x509.Certificate{other.server.Leaf, other.intermediate}}); !errors.As(err, &unknown) {
		t.Fatalf("trust pool aliased: %v", err)
	}
}

func TestTLSCRLPreflightAndAuthenticatedEvidenceReasons(t *testing.T) {
	f := newTLSCRLFixture(t)
	now := time.Now().UTC().Truncate(time.Second)
	state := tls.ConnectionState{PeerCertificates: []*x509.Certificate{f.server.Leaf, f.intermediate}}
	for _, tc := range []struct {
		name       string
		start, end time.Time
		kind       TLSRevocationKind
	}{
		{"expired", now.Add(-2 * time.Hour), now.Add(-time.Hour), TLSRevocationExpired},
		{"future", now.Add(time.Hour), now.Add(2 * time.Hour), TLSRevocationNotYetValid},
	} {
		t.Run(tc.name, func(t *testing.T) {
			der, err := x509.CreateRevocationList(rand.Reader, &x509.RevocationList{Number: big.NewInt(1), ThisUpdate: tc.start, NextUpdate: tc.end}, f.intermediate, f.intermediateKey)
			if err != nil {
				t.Fatal(err)
			}
			list, err := x509.ParseRevocationList(der)
			if err != nil {
				t.Fatal(err)
			}
			opts := TLSProfileOptions{TrustedCAs: f.pool, ServerName: "scp.test", Revocation: &TLSRevocationOptions{Mode: TLSCRLLeaf, CRLs: []*x509.RevocationList{list}}}
			_, err = ClientTLSConfig(opts)
			tlsCRLKind(t, err, tc.kind)
			// An unrelated current root CRL satisfies only bundle preflight. It
			// cannot replace current evidence for the leaf's actual issuer.
			opts.Revocation.CRLs = append(opts.Revocation.CRLs, f.list(t, true))
			config, err := ClientTLSConfig(opts)
			if err != nil {
				t.Fatal(err)
			}
			tlsCRLKind(t, config.VerifyConnection(state), tc.kind)
		})
	}
	bad := f.list(t, false)
	bad.Raw = append([]byte(nil), bad.Raw...)
	bad.Raw[len(bad.Raw)-1] ^= 1
	config := f.config(t, false, TLSCRLLeaf, bad)
	tlsCRLKind(t, config.VerifyConnection(state), TLSRevocationBadSignature)
	_, err := ClientTLSConfig(TLSProfileOptions{TrustedCAs: f.pool, Revocation: &TLSRevocationOptions{Mode: TLSCRLLeaf}})
	tlsCRLKind(t, err, TLSRevocationMissing)
	config = f.config(t, false, TLSCRLChain, f.list(t, false))
	err = config.VerifyConnection(state)
	var diagnostic *TLSRevocationError
	if !errors.As(err, &diagnostic) || diagnostic.CertificateIndex != 1 || diagnostic.ChainIndex != 0 || diagnostic.Kind != TLSRevocationIssuerMismatch {
		t.Fatalf("bounded chain diagnostic lost: %v", err)
	}
}

func TestTLSCRLTrustAnchorOptionalAndNormalConstraints(t *testing.T) {
	f := newTLSCRLFixture(t)
	// The anchor normally stays outside chain mode. A root CRL listing its
	// own serial must not invalidate a different peer's verified path.
	config := f.config(t, false, TLSCRLChain, f.list(t, false), f.list(t, true, f.root))
	state := tls.ConnectionState{PeerCertificates: []*x509.Certificate{f.server.Leaf, f.intermediate}}
	if err := config.VerifyConnection(state); err != nil {
		t.Fatalf("anchor included: %v", err)
	}
	// A root used as the actual leaf is still checked.
	opts := TLSProfileOptions{TrustedCAs: f.pool, ServerName: "scp.test", Revocation: &TLSRevocationOptions{Mode: TLSCRLChain, CRLs: []*x509.RevocationList{f.list(t, true, f.root)}}}
	direct, err := ClientTLSConfig(opts)
	if err != nil {
		t.Fatal(err)
	}
	tlsCRLKind(t, direct.VerifyConnection(tls.ConnectionState{PeerCertificates: []*x509.Certificate{f.root}}), TLSRevocationRevoked)
	// A directly trusted non-self-signed leaf cannot silently waive issuer
	// evidence just because its verified path contains one certificate.
	leafPool := x509.NewCertPool()
	leafPool.AddCert(f.server.Leaf)
	opts.TrustedCAs = leafPool
	opts.Revocation.CRLs = []*x509.RevocationList{f.list(t, false)}
	direct, err = ClientTLSConfig(opts)
	if err != nil {
		t.Fatal(err)
	}
	tlsCRLKind(t, direct.VerifyConnection(tls.ConnectionState{PeerCertificates: []*x509.Certificate{f.server.Leaf}}), TLSRevocationIssuerMismatch)
	optional, err := ServerTLSConfig(TLSProfileOptions{Certificates: []tls.Certificate{f.server}, TrustedCAs: f.pool, PeerAuthentication: TLSVerifyPeerCertificateIfGiven, Revocation: &TLSRevocationOptions{Mode: TLSCRLLeaf, CRLs: []*x509.RevocationList{f.list(t, false)}}})
	if err != nil {
		t.Fatal(err)
	}
	client := f.config(t, false, TLSCRLLeaf, f.list(t, false))
	client.Certificates = nil
	_, ce, se := handshake(t, optional, client)
	if ce != nil || se != nil {
		t.Fatalf("optional absence changed: %v %v", ce, se)
	}
	// Current clean CRLs do not override the normal hostname check.
	opts = TLSProfileOptions{TrustedCAs: f.pool, ServerName: "wrong.test", Revocation: &TLSRevocationOptions{Mode: TLSCRLLeaf, CRLs: []*x509.RevocationList{f.list(t, false)}}}
	badName, err := ClientTLSConfig(opts)
	if err != nil {
		t.Fatal(err)
	}
	var hostname x509.HostnameError
	if err := badName.VerifyConnection(state); !errors.As(err, &hostname) {
		t.Fatalf("normal hostname lost: %v", err)
	}
}

// exchangeTLSCRL processes TLS 1.3 NewSessionTicket messages by reading
// application data, then joins the server. It uses a bounded real TCP pair.
func exchangeTLSCRL(t *testing.T, listener net.Listener, serverConfig, clientConfig *tls.Config) (tls.ConnectionState, error, error) {
	t.Helper()
	serverDone := make(chan error, 1)
	go func() {
		raw, err := listener.Accept()
		if err != nil {
			serverDone <- err
			return
		}
		defer raw.Close()
		_ = raw.SetDeadline(time.Now().Add(5 * time.Second))
		conn := tls.Server(raw, serverConfig)
		if err = conn.Handshake(); err == nil {
			_, err = conn.Write([]byte{1})
			if err == nil {
				_, err = io.ReadFull(conn, make([]byte, 1))
			}
		}
		serverDone <- err
	}()
	raw, err := net.DialTimeout("tcp", listener.Addr().String(), 5*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	_ = raw.SetDeadline(time.Now().Add(5 * time.Second))
	conn := tls.Client(raw, clientConfig)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	clientErr := conn.HandshakeContext(ctx)
	if clientErr == nil {
		_, clientErr = io.ReadFull(conn, make([]byte, 1))
		if clientErr == nil {
			_, clientErr = conn.Write([]byte{2})
		}
	}
	state := conn.ConnectionState()
	_ = raw.Close()
	return state, clientErr, <-serverDone
}

func TestTLSCRLResumptionRechecksBothPeers(t *testing.T) {
	for _, version := range []uint16{tls.VersionTLS12, tls.VersionTLS13} {
		for _, rejectServer := range []bool{true, false} {
			for _, failure := range []string{"revoked", "unknown", "trust"} {
				// Changing the server's advertised client CAs can invalidate a
				// ticket or prevent client certificate selection before our
				// callback. Keep factual resumed CRL proof separate from that.
				if failure == "trust" && !rejectServer {
					continue
				}
				name := tls.VersionName(version) + map[bool]string{true: "/client-checks-server", false: "/server-checks-client"}[rejectServer] + "/" + failure
				t.Run(name, func(t *testing.T) {
					f := newTLSCRLFixture(t)
					clean := f.list(t, false)
					server := f.config(t, true, TLSCRLLeaf, clean)
					client := f.config(t, false, TLSCRLLeaf, clean)
					var ticket [32]byte
					if _, err := rand.Read(ticket[:]); err != nil {
						t.Fatal(err)
					}
					server.SetSessionTicketKeys([][32]byte{ticket})
					client.ClientSessionCache = tls.NewLRUClientSessionCache(4)
					server.MinVersion, server.MaxVersion = version, version
					client.MinVersion, client.MaxVersion = version, version
					listener, err := net.Listen("tcp", "127.0.0.1:0")
					if err != nil {
						t.Fatal(err)
					}
					defer listener.Close()
					state, ce, se := exchangeTLSCRL(t, listener, server, client)
					if ce != nil || se != nil || state.DidResume {
						t.Fatalf("initial: resumed=%v client=%v server=%v", state.DidResume, ce, se)
					}
					state, ce, se = exchangeTLSCRL(t, listener, server, client)
					if ce != nil || se != nil || !state.DidResume {
						t.Fatalf("positive resume not established: resumed=%v client=%v server=%v", state.DidResume, ce, se)
					}
					list := f.list(t, false, f.server.Leaf)
					if !rejectServer {
						list = f.list(t, false, f.client.Leaf)
					}
					kind := TLSRevocationRevoked
					if failure == "unknown" {
						other := newTLSCRLFixture(t)
						list = other.list(t, true)
						kind = TLSRevocationIssuerMismatch
					}
					if failure == "trust" {
						other := newTLSCRLFixture(t)
						f.pool = other.pool
						list = f.list(t, false)
					}
					observedResume := false
					if rejectServer {
						replacement := f.config(t, false, TLSCRLLeaf, list)
						replacement.MinVersion, replacement.MaxVersion = version, version
						replacement.ClientSessionCache = client.ClientSessionCache
						verify := replacement.VerifyConnection
						replacement.VerifyConnection = func(s tls.ConnectionState) error { observedResume = s.DidResume; return verify(s) }
						client = replacement
					} else {
						replacement := f.config(t, true, TLSCRLLeaf, list)
						replacement.MinVersion, replacement.MaxVersion = version, version
						replacement.SetSessionTicketKeys([][32]byte{ticket})
						verify := replacement.VerifyConnection
						replacement.VerifyConnection = func(s tls.ConnectionState) error { observedResume = s.DidResume; return verify(s) }
						server = replacement
					}
					_, ce, se = exchangeTLSCRL(t, listener, server, client)
					if !observedResume {
						t.Fatal("rejection was not observed inside resumed VerifyConnection")
					}
					if failure == "trust" {
						err := se
						if rejectServer {
							err = ce
						}
						var trust x509.UnknownAuthorityError
						if !errors.As(err, &trust) {
							t.Fatalf("resumed current trust was not verified: %v", err)
						}
						return
					}
					if rejectServer {
						tlsCRLKind(t, ce, kind)
					} else {
						tlsCRLKind(t, se, kind)
					}
				})
			}
		}
	}
}
