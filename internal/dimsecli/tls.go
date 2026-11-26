package dimsecli

import (
	"crypto/tls"
	"crypto/x509"
	"errors"
	"flag"
	"fmt"
	"os"
	"strings"

	"github.com/ThalesMMS/dicom-go/net/ul"
)

// TLSFlags are the TLS options shared by the DIMSE command-line tools. TLS
// is enabled by -tls-profile; the other flags require it.
type TLSFlags struct {
	Profile         string
	CertFile        string
	KeyFile         string
	CAFiles         []string
	RequirePeerCert bool
	ServerName      string
	server          bool
}

type stringListFlag struct{ values *[]string }

func (f stringListFlag) String() string {
	if f.values == nil {
		return ""
	}
	return strings.Join(*f.values, ",")
}

func (f stringListFlag) Set(value string) error {
	*f.values = append(*f.values, value)
	return nil
}

// Register adds -tls-profile, -cert, -key, -ca and -require-peer-cert, plus
// -tls-server-name for a requestor, to fs.
func (f *TLSFlags) Register(fs *flag.FlagSet, server bool) {
	f.server = server
	f.RequirePeerCert = true
	names := make([]string, 0, len(ul.TLSProfiles()))
	for _, profile := range ul.TLSProfiles() {
		names = append(names, string(profile))
	}
	fs.StringVar(&f.Profile, "tls-profile", "", "enable TLS with a PS3.15 profile ("+strings.Join(names, ", ")+"); empty disables TLS")
	fs.StringVar(&f.CertFile, "cert", "", "PEM certificate of this side (required for an SCP, optional for an SCU)")
	fs.StringVar(&f.KeyFile, "key", "", "PEM private key matching -cert")
	fs.Var(stringListFlag{&f.CAFiles}, "ca", "PEM file of CA certificates trusted to verify the peer (repeatable)")
	if server {
		fs.BoolVar(&f.RequirePeerCert, "require-peer-cert", true, "require and verify a client certificate against -ca (mutual authentication)")
	} else {
		fs.BoolVar(&f.RequirePeerCert, "require-peer-cert", true, "verify the server certificate against -ca (system roots without -ca)")
		fs.StringVar(&f.ServerName, "tls-server-name", "", "also require the server certificate to match this name, sent as SNI")
	}
}

// Enabled reports whether -tls-profile was given.
func (f *TLSFlags) Enabled() bool {
	return f != nil && strings.TrimSpace(f.Profile) != ""
}

// Validate checks that the TLS flags are used together consistently.
func (f *TLSFlags) Validate() error {
	if !f.Enabled() {
		if f.CertFile != "" || f.KeyFile != "" || len(f.CAFiles) > 0 || f.ServerName != "" {
			return errors.New("-cert, -key, -ca and -tls-server-name require -tls-profile")
		}
		return nil
	}
	if _, err := ul.ParseTLSProfile(f.Profile); err != nil {
		return err
	}
	if (f.CertFile == "") != (f.KeyFile == "") {
		return errors.New("-cert and -key must be given together")
	}
	if f.server && f.CertFile == "" {
		return errors.New("a TLS SCP requires -cert and -key")
	}
	if f.server && f.RequirePeerCert && len(f.CAFiles) == 0 {
		return errors.New("-require-peer-cert needs -ca to verify client certificates (or -require-peer-cert=false)")
	}
	return nil
}

// Config builds the TLS configuration, or nil when TLS is disabled.
func (f *TLSFlags) Config() (*tls.Config, error) {
	if !f.Enabled() {
		return nil, nil
	}
	if err := f.Validate(); err != nil {
		return nil, err
	}
	profile, _ := ul.ParseTLSProfile(f.Profile)
	opts := ul.TLSProfileOptions{Profile: profile, ServerName: f.ServerName}
	if f.CertFile != "" {
		certificate, err := tls.LoadX509KeyPair(f.CertFile, f.KeyFile)
		if err != nil {
			return nil, fmt.Errorf("load TLS certificate: %w", err)
		}
		opts.Certificates = []tls.Certificate{certificate}
	}
	if len(f.CAFiles) > 0 {
		pool := x509.NewCertPool()
		for _, path := range f.CAFiles {
			data, err := os.ReadFile(path)
			if err != nil {
				return nil, fmt.Errorf("read TLS CA file: %w", err)
			}
			if !pool.AppendCertsFromPEM(data) {
				return nil, fmt.Errorf("TLS CA file %q holds no PEM certificate", path)
			}
		}
		opts.TrustedCAs = pool
	}
	if !f.RequirePeerCert {
		opts.PeerAuthentication = ul.TLSIgnorePeerCertificate
	}
	if f.server {
		return ul.ServerTLSConfig(opts)
	}
	return ul.ClientTLSConfig(opts)
}
