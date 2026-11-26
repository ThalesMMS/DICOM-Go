package ul

import (
	"crypto/ecdsa"
	"crypto/ed25519"
	"crypto/rsa"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"fmt"
	"strings"
	"time"
)

// TLSProfile names a PS3.15 Annex B TLS Secure Transport Connection Profile.
type TLSProfile string

const (
	// TLSProfileBCP195RFC8996 is the BCP 195 RFC 8996, 9325 TLS profile
	// (PS3.15 B.12): TLS 1.2 or 1.3 with the ECDHE AES-GCM suites that
	// RFC 9325 recommends. It is DCMTK's default profile.
	TLSProfileBCP195RFC8996 TLSProfile = "bcp195-rfc8996"
	// TLSProfileModifiedBCP195RFC8996 is the Modified BCP 195 RFC 8996, 9325
	// TLS profile (PS3.15 B.13). Go offers its AES-GCM and ChaCha20-Poly1305
	// suites but not the CCM, Camellia or DHE ones.
	TLSProfileModifiedBCP195RFC8996 TLSProfile = "bcp195-rfc8996-modified"
	// TLSProfileBCP195 is the retired BCP 195 TLS profile (PS3.15-2022d B.9),
	// offered without fallback below TLS 1.2.
	TLSProfileBCP195 TLSProfile = "bcp195"
	// TLSProfileNonDowngradingBCP195 is the retired Non-Downgrading BCP 195
	// TLS profile (PS3.15-2022d B.10).
	TLSProfileNonDowngradingBCP195 TLSProfile = "bcp195-nd"
	// TLSProfileExtendedBCP195 is the retired Extended BCP 195 TLS profile
	// (PS3.15-2022d B.11).
	TLSProfileExtendedBCP195 TLSProfile = "bcp195-extended"
)

// DefaultTLSProfile is the profile used when none is named.
const DefaultTLSProfile = TLSProfileBCP195RFC8996

// ErrUnknownTLSProfile reports a profile name that ParseTLSProfile does not
// recognize.
var ErrUnknownTLSProfile = errors.New("dicom ul: unknown TLS profile")

// ErrWeakTLSCertificate reports a certificate below the profile's key or
// signature requirements (RSA 2048 bits, ECC 256 bits, SHA-256 or stronger).
var ErrWeakTLSCertificate = errors.New("dicom ul: TLS certificate below profile requirements")

type tlsProfileSpec struct {
	section     string
	title       string
	suites      []uint16
	unsupported []string
}

var tlsProfileSpecs = map[TLSProfile]tlsProfileSpec{
	TLSProfileBCP195RFC8996: {
		section: "B.12",
		title:   "BCP 195 RFC 8996, 9325 TLS Secure Transport Connection Profile",
		suites: []uint16{
			tls.TLS_ECDHE_ECDSA_WITH_AES_128_GCM_SHA256,
			tls.TLS_ECDHE_RSA_WITH_AES_128_GCM_SHA256,
			tls.TLS_ECDHE_ECDSA_WITH_AES_256_GCM_SHA384,
			tls.TLS_ECDHE_RSA_WITH_AES_256_GCM_SHA384,
		},
	},
	TLSProfileModifiedBCP195RFC8996: {
		section: "B.13",
		title:   "Modified BCP 195 RFC 8996, 9325 TLS Secure Transport Connection Profile",
		suites: []uint16{
			tls.TLS_ECDHE_ECDSA_WITH_AES_256_GCM_SHA384,
			tls.TLS_ECDHE_RSA_WITH_AES_256_GCM_SHA384,
			tls.TLS_ECDHE_ECDSA_WITH_CHACHA20_POLY1305_SHA256,
			tls.TLS_ECDHE_RSA_WITH_CHACHA20_POLY1305_SHA256,
			tls.TLS_ECDHE_RSA_WITH_AES_128_GCM_SHA256,
			tls.TLS_ECDHE_ECDSA_WITH_AES_128_GCM_SHA256,
		},
		unsupported: []string{
			"TLS_AES_128_CCM_SHA256", "TLS_AES_128_CCM_8_SHA256",
			"TLS_ECDHE_ECDSA_WITH_AES_256_CCM", "TLS_ECDHE_ECDSA_WITH_AES_256_CCM_8",
			"TLS_ECDHE_ECDSA_WITH_AES_128_CCM", "TLS_ECDHE_ECDSA_WITH_AES_128_CCM_8",
		},
	},
	TLSProfileBCP195: {
		section: "B.9 (retired)",
		title:   "BCP 195 TLS Secure Transport Connection Profile",
		suites: []uint16{
			tls.TLS_ECDHE_RSA_WITH_AES_128_GCM_SHA256,
			tls.TLS_ECDHE_RSA_WITH_AES_256_GCM_SHA384,
		},
		unsupported: []string{"TLS_DHE_RSA_WITH_AES_128_GCM_SHA256", "TLS_DHE_RSA_WITH_AES_256_GCM_SHA384"},
	},
	TLSProfileNonDowngradingBCP195: {
		section: "B.10 (retired)",
		title:   "Non-Downgrading BCP 195 TLS Secure Transport Connection Profile",
		suites: []uint16{
			tls.TLS_ECDHE_RSA_WITH_AES_128_GCM_SHA256,
			tls.TLS_ECDHE_RSA_WITH_AES_256_GCM_SHA384,
		},
		unsupported: []string{"TLS_DHE_RSA_WITH_AES_128_GCM_SHA256", "TLS_DHE_RSA_WITH_AES_256_GCM_SHA384"},
	},
	TLSProfileExtendedBCP195: {
		section: "B.11 (retired)",
		title:   "Extended BCP 195 TLS Profile Secure Transport Connection Profile",
		suites: []uint16{
			tls.TLS_ECDHE_RSA_WITH_AES_128_GCM_SHA256,
			tls.TLS_ECDHE_RSA_WITH_AES_256_GCM_SHA384,
			tls.TLS_ECDHE_ECDSA_WITH_AES_256_GCM_SHA384,
			tls.TLS_ECDHE_ECDSA_WITH_AES_128_GCM_SHA256,
		},
		unsupported: []string{"TLS_DHE_RSA_WITH_AES_128_GCM_SHA256", "TLS_DHE_RSA_WITH_AES_256_GCM_SHA384"},
	},
}

var tlsProfileOrder = []TLSProfile{
	TLSProfileBCP195RFC8996,
	TLSProfileModifiedBCP195RFC8996,
	TLSProfileNonDowngradingBCP195,
	TLSProfileBCP195,
	TLSProfileExtendedBCP195,
}

// tlsProfileAliases accepts the short names of DCMTK's profile options.
var tlsProfileAliases = map[string]TLSProfile{
	"8996":        TLSProfileBCP195RFC8996,
	"8996-mod":    TLSProfileModifiedBCP195RFC8996,
	"bcp195-ex":   TLSProfileExtendedBCP195,
	"bcp195-nd":   TLSProfileNonDowngradingBCP195,
	"bcp195-8996": TLSProfileBCP195RFC8996,
}

// TLSProfiles lists the supported profiles, current ones first.
func TLSProfiles() []TLSProfile {
	return append([]TLSProfile(nil), tlsProfileOrder...)
}

// ParseTLSProfile resolves a profile name or a DCMTK option suffix such as
// "8996" or "bcp195-ex"; the empty name is DefaultTLSProfile.
func ParseTLSProfile(name string) (TLSProfile, error) {
	name = strings.ToLower(strings.TrimSpace(name))
	if name == "" {
		return DefaultTLSProfile, nil
	}
	if _, ok := tlsProfileSpecs[TLSProfile(name)]; ok {
		return TLSProfile(name), nil
	}
	if profile, ok := tlsProfileAliases[name]; ok {
		return profile, nil
	}
	return "", fmt.Errorf("%w %q", ErrUnknownTLSProfile, name)
}

func (p TLSProfile) spec() (tlsProfileSpec, error) {
	if p == "" {
		p = DefaultTLSProfile
	}
	spec, ok := tlsProfileSpecs[p]
	if !ok {
		return tlsProfileSpec{}, fmt.Errorf("%w %q", ErrUnknownTLSProfile, string(p))
	}
	return spec, nil
}

// Section is the PS3.15 section that defines the profile.
func (p TLSProfile) Section() string {
	spec, _ := p.spec()
	return spec.section
}

// Title is the profile's PS3.15 title.
func (p TLSProfile) Title() string {
	spec, _ := p.spec()
	return spec.title
}

// CipherSuites lists the TLS 1.2 suites offered, in preference order. TLS 1.3
// uses Go's AES-GCM and ChaCha20-Poly1305 suites, which every profile allows.
func (p TLSProfile) CipherSuites() []uint16 {
	spec, _ := p.spec()
	return append([]uint16(nil), spec.suites...)
}

// UnsupportedCipherSuites lists suites the profile requires that Go's TLS
// stack cannot offer; a conformance statement must declare them.
func (p TLSProfile) UnsupportedCipherSuites() []string {
	spec, _ := p.spec()
	return append([]string(nil), spec.unsupported...)
}

// TLSPeerAuthentication selects how the peer's certificate is checked.
type TLSPeerAuthentication int

const (
	// TLSRequirePeerCertificate verifies the peer's certificate chain and
	// fails when it is absent (mutual authentication on a server).
	TLSRequirePeerCertificate TLSPeerAuthentication = iota
	// TLSVerifyPeerCertificateIfGiven verifies a client certificate when one
	// is sent; on a client it is TLSRequirePeerCertificate.
	TLSVerifyPeerCertificateIfGiven
	// TLSIgnorePeerCertificate does not verify the peer's chain, as DCMTK
	// --ignore-peer-cert; key strength is still checked.
	TLSIgnorePeerCertificate
)

// TLSProfileOptions configures ClientTLSConfig and ServerTLSConfig.
type TLSProfileOptions struct {
	// Profile is the PS3.15 profile; empty is DefaultTLSProfile.
	Profile TLSProfile
	// Certificates are this side's certificates: required on a server,
	// optional on a client (sent for mutual authentication).
	Certificates []tls.Certificate
	// TrustedCAs verify the peer: server certificates on a client (nil uses
	// the system roots) and client certificates on a server (required unless
	// the peer certificate is ignored).
	TrustedCAs *x509.CertPool
	// PeerAuthentication selects how the peer certificate is checked.
	PeerAuthentication TLSPeerAuthentication
	// ServerName, on a client, is sent as SNI and must match the server
	// certificate; empty verifies the chain only when TrustedCAs is provided.
	ServerName string
	// Revocation enables an offline CRL policy after ordinary certificate
	// verification. Nil preserves the existing policy without revocation checks.
	// CRL DER and trust pools are captured when this configuration is built.
	Revocation *TLSRevocationOptions
}

// ClientTLSConfig builds the tls.Config of an association requestor for a
// PS3.15 profile.
func ClientTLSConfig(opts TLSProfileOptions) (*tls.Config, error) {
	config, err := profileTLSConfig(opts)
	if err != nil {
		return nil, err
	}
	revocation, mode, err := compileTLSRevocation(opts)
	if err != nil {
		return nil, err
	}
	config.ServerName = strings.TrimSpace(opts.ServerName)
	// Verification runs in VerifyConnection so a chain can be checked without
	// a name, which DICOM peers addressed by IP rarely carry.
	config.InsecureSkipVerify = true
	verifyChain := opts.PeerAuthentication != TLSIgnorePeerCertificate
	if verifyChain && opts.TrustedCAs == nil && config.ServerName == "" {
		return nil, errors.New("dicom ul: TLS client verification requires trusted CAs or a server name")
	}
	roots := opts.TrustedCAs
	if revocation != nil && roots != nil {
		roots = roots.Clone()
	}
	serverName := config.ServerName
	config.VerifyConnection = func(state tls.ConnectionState) error {
		if len(state.PeerCertificates) == 0 {
			return errors.New("dicom ul: TLS server sent no certificate")
		}
		if err := checkTLSCertificateStrength(state.PeerCertificates[0]); err != nil {
			return err
		}
		if !verifyChain {
			return nil
		}
		intermediates := x509.NewCertPool()
		for _, cert := range state.PeerCertificates[1:] {
			intermediates.AddCert(cert)
		}
		at := time.Now()
		chains, err := state.PeerCertificates[0].Verify(x509.VerifyOptions{
			Roots:         roots,
			Intermediates: intermediates,
			DNSName:       serverName,
			KeyUsages:     []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
			CurrentTime:   at,
		})
		if err != nil {
			return fmt.Errorf("dicom ul: verify TLS server certificate: %w", err)
		}
		if revocation != nil {
			return revocation.CheckChains(chains, mode == TLSCRLChain, at)
		}
		return nil
	}
	return config, nil
}

// ServerTLSConfig builds the tls.Config of an association acceptor for a
// PS3.15 profile. The profiles require servers to support mutual
// authentication; TLSRequirePeerCertificate (the zero value) enforces it.
func ServerTLSConfig(opts TLSProfileOptions) (*tls.Config, error) {
	if len(opts.Certificates) == 0 {
		return nil, errors.New("dicom ul: TLS server requires a certificate")
	}
	config, err := profileTLSConfig(opts)
	if err != nil {
		return nil, err
	}
	revocation, mode, err := compileTLSRevocation(opts)
	if err != nil {
		return nil, err
	}
	roots := opts.TrustedCAs
	if revocation != nil && roots != nil {
		roots = roots.Clone()
	}
	switch opts.PeerAuthentication {
	case TLSRequirePeerCertificate, TLSVerifyPeerCertificateIfGiven:
		if opts.TrustedCAs == nil {
			return nil, errors.New("dicom ul: verifying TLS client certificates requires trusted CAs")
		}
		config.ClientCAs = roots
		if revocation != nil {
			config.ClientCAs = roots.Clone()
		}
		config.ClientAuth = tls.RequireAndVerifyClientCert
		if opts.PeerAuthentication == TLSVerifyPeerCertificateIfGiven {
			config.ClientAuth = tls.VerifyClientCertIfGiven
		}
	case TLSIgnorePeerCertificate:
		config.ClientAuth = tls.NoClientCert
	default:
		return nil, fmt.Errorf("dicom ul: unknown TLS peer authentication %d", opts.PeerAuthentication)
	}
	config.VerifyConnection = func(state tls.ConnectionState) error {
		if len(state.PeerCertificates) == 0 {
			return nil
		}
		if err := checkTLSCertificateStrength(state.PeerCertificates[0]); err != nil {
			return err
		}
		if revocation == nil {
			return nil
		}
		// Go's ordinary client authentication remains enabled. Reverification
		// here also checks current trust and validity on resumed connections.
		at := time.Now()
		intermediates := x509.NewCertPool()
		for _, cert := range state.PeerCertificates[1:] {
			intermediates.AddCert(cert)
		}
		chains, err := state.PeerCertificates[0].Verify(x509.VerifyOptions{
			Roots: roots, Intermediates: intermediates, CurrentTime: at,
			KeyUsages: []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth},
		})
		if err != nil {
			return fmt.Errorf("dicom ul: verify TLS client certificate: %w", err)
		}
		return revocation.CheckChains(chains, mode == TLSCRLChain, at)
	}
	return config, nil
}

func profileTLSConfig(opts TLSProfileOptions) (*tls.Config, error) {
	spec, err := opts.Profile.spec()
	if err != nil {
		return nil, err
	}
	for _, certificate := range opts.Certificates {
		leaf := certificate.Leaf
		if leaf == nil && len(certificate.Certificate) > 0 {
			if leaf, err = x509.ParseCertificate(certificate.Certificate[0]); err != nil {
				return nil, fmt.Errorf("dicom ul: parse TLS certificate: %w", err)
			}
		}
		if leaf != nil {
			if err := checkTLSCertificateStrength(leaf); err != nil {
				return nil, err
			}
		}
	}
	return &tls.Config{
		MinVersion:   tls.VersionTLS12,
		MaxVersion:   tls.VersionTLS13,
		CipherSuites: append([]uint16(nil), spec.suites...),
		Certificates: append([]tls.Certificate(nil), opts.Certificates...),
	}, nil
}

// checkTLSCertificateStrength applies the profiles' minimums to a leaf: RSA
// keys of 2048 bits, ECC keys of 256 bits and a SHA-256 or stronger
// signature.
func checkTLSCertificateStrength(cert *x509.Certificate) error {
	switch key := cert.PublicKey.(type) {
	case *rsa.PublicKey:
		if key.N.BitLen() < 2048 {
			return fmt.Errorf("%w: %d-bit RSA key", ErrWeakTLSCertificate, key.N.BitLen())
		}
	case *ecdsa.PublicKey:
		if bits := key.Curve.Params().BitSize; bits < 256 {
			return fmt.Errorf("%w: %d-bit ECC key", ErrWeakTLSCertificate, bits)
		}
	case ed25519.PublicKey:
	default:
		return fmt.Errorf("%w: unsupported %T key", ErrWeakTLSCertificate, cert.PublicKey)
	}
	switch cert.SignatureAlgorithm {
	case x509.MD2WithRSA, x509.MD5WithRSA, x509.SHA1WithRSA, x509.DSAWithSHA1, x509.DSAWithSHA256, x509.ECDSAWithSHA1:
		return fmt.Errorf("%w: %s signature", ErrWeakTLSCertificate, cert.SignatureAlgorithm)
	}
	return nil
}
