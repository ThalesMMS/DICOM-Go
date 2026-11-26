// Package ldapconfig reads explicitly configured LDAP directories into netconf
// models and verifies user identities and direct group memberships. It never
// writes the directory, follows referrals, discovers servers, stores
// credentials, or approves clinical use of discovered endpoints.
package ldapconfig

import (
	"crypto/tls"
	"errors"
	"fmt"
	"net"
	"net/url"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/ThalesMMS/dicom-go/net/netconf"
	ldap "github.com/go-ldap/ldap/v3"
)

const NormativeEdition = "DICOM PS3.15 2025e Annex H"

var (
	ErrInvalid        = errors.New("LDAP directory configuration or schema is invalid")
	ErrLimit          = errors.New("LDAP directory resource limit exceeded")
	ErrAuthentication = errors.New("LDAP directory authentication failed")
	ErrTransport      = errors.New("LDAP directory connection or TLS failed")
	ErrProtocol       = errors.New("LDAP directory response is invalid")
	ErrReferral       = errors.New("LDAP directory referral is not allowed")
)

// Config is borrowed only during Read or Authenticate. Bind credentials are
// ephemeral and must be supplied by the application. Diagnostics never
// include their values.
type Config struct {
	URL, BaseDN, BindDN string
	Password            string      `json:"-"`
	TLS                 *tls.Config `json:"-"`
	// LDAP uses verified StartTLS by default; LDAPS uses verified TLS immediately.
	// Cleartext is an explicit anonymous-only option, never an auth downgrade.
	AllowAnonymousCleartext bool
}

func (c Config) Format(s fmt.State, _ rune) {
	_, _ = fmt.Fprint(s, "LDAP configuration (credentials redacted)")
}

type Options struct {
	Timeout                                                    time.Duration
	PageSize, MaxPages, MaxDevices, MaxEntries, MaxDepth       int
	MaxPacketBytes, MaxWireBytes, MaxEntryBytes, MaxModelBytes int
	MaxBERDepth, MaxBERNodes                                   int
	// DeviceName is an exact LDAP equality filter value, escaped by the client.
	// AETitle filters the returned model after connections/capabilities resolve.
	DeviceName, AETitle string
	Model               netconf.Options
}

type Diagnostic struct{ Code, Object string }
type Device struct {
	// ID hashes the parsed DN with canonical attribute names and preserved
	// case-sensitive values. DN spelling changes do not change source identity.
	ID          string
	DN          string
	Model       netconf.Device
	Diagnostics []Diagnostic
}
type Snapshot struct {
	Devices []Device
	Edition string
	// Skipped lists the devices a whole-directory read left out: Code is
	// "device-invalid" for a device whose declarations are incomplete or
	// contradictory and "application-entities-absent" for a device that
	// declares no DICOM Application Entity; Object is the device name.
	Skipped []Diagnostic `json:",omitempty"`
}

func normalized(o Options) (Options, error) {
	if o.Timeout == 0 {
		o.Timeout = 20 * time.Second
	}
	if o.Timeout < time.Millisecond || o.Timeout > 5*time.Minute {
		return o, ErrLimit
	}
	for _, field := range []struct {
		value    *int
		def, max int
	}{
		{&o.PageSize, 64, 256}, {&o.MaxPages, 128, 512}, {&o.MaxDevices, 32, 128},
		{&o.MaxEntries, 4096, 16384}, {&o.MaxDepth, 8, 32},
		{&o.MaxPacketBytes, 1 << 20, 16 << 20}, {&o.MaxWireBytes, 16 << 20, 64 << 20},
		{&o.MaxEntryBytes, 64 << 10, 1 << 20}, {&o.MaxModelBytes, 8 << 20, 32 << 20},
		{&o.MaxBERDepth, 16, 32}, {&o.MaxBERNodes, 8192, 65536},
	} {
		if *field.value == 0 {
			*field.value = field.def
		}
		if *field.value < 1 || *field.value > field.max {
			return o, ErrLimit
		}
	}
	if len(o.DeviceName) > 128 || len(o.AETitle) > 16 || !utf8.ValidString(o.DeviceName) || !utf8.ValidString(o.AETitle) {
		return o, ErrLimit
	}
	return o, nil
}

func validated(c Config) (*url.URL, *ldap.DN, error) {
	if len(c.URL) > 2048 || len(c.BaseDN) > 4096 || len(c.BindDN) > 4096 || len(c.Password) > 65535 {
		return nil, nil, ErrLimit
	}
	u, err := url.Parse(c.URL)
	if err != nil || u.User != nil || u.Opaque != "" || u.RawQuery != "" || u.Fragment != "" || (u.Path != "" && u.Path != "/") || (u.Scheme != "ldap" && u.Scheme != "ldaps") || u.Hostname() == "" {
		return nil, nil, ErrInvalid
	}
	if strings.ContainsAny(u.Hostname(), " /\\\r\n\x00") || len(u.Hostname()) > 253 {
		return nil, nil, ErrInvalid
	}
	if u.Port() != "" {
		if _, err := net.LookupPort("tcp", u.Port()); err != nil {
			return nil, nil, ErrInvalid
		}
	}
	dn, err := ldap.ParseDN(c.BaseDN)
	if err != nil || len(dn.RDNs) == 0 || len(dn.RDNs) > 32 {
		return nil, nil, ErrInvalid
	}
	if c.BindDN != "" {
		if _, err := ldap.ParseDN(c.BindDN); err != nil {
			return nil, nil, ErrInvalid
		}
	}
	if (c.Password != "" && c.BindDN == "") || (c.BindDN != "" && c.Password == "") {
		return nil, nil, ErrInvalid
	}
	if c.AllowAnonymousCleartext && (u.Scheme != "ldap" || c.BindDN != "" || c.Password != "") {
		return nil, nil, ErrInvalid
	}
	if c.TLS != nil && (c.TLS.InsecureSkipVerify || c.TLS.VerifyPeerCertificate != nil || c.TLS.VerifyConnection != nil) {
		return nil, nil, ErrInvalid
	}
	return u, dn, nil
}

// ValidateConfig does no I/O and returns fixed, value-free diagnostics.
func ValidateConfig(c Config, o Options) error {
	if _, err := normalized(o); err != nil {
		return err
	}
	_, _, err := validated(c)
	return err
}
