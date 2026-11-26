package dimsecli

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"net"
	"strconv"
	"strings"
	"time"

	"github.com/ThalesMMS/dicom-go/net/ul"
)

// ConnectionDefaults preserves a tool's existing defaults. Listener interprets
// Host/Port as bind address, CalledAE as local title, CallingAE as an optional
// exact peer filter, and Timeout as negotiation only. A default zero listener
// timeout retains a legacy unbounded negotiation; explicit -timeout is positive.
type ConnectionDefaults struct {
	Host                string
	Port                int
	CalledAE, CallingAE string
	Timeout             time.Duration
	Listener            bool
}

type connectionValue struct {
	read func() string
	set  func(string) error
}

func (v connectionValue) String() string {
	if v.read == nil {
		return ""
	}
	return v.read()
}
func (v connectionValue) Set(s string) error { return v.set(s) }

// RegisterWithDefaults registers canonical endpoint flags and historical AE
// aliases while retaining defaults and the TLS/UserIdentity role of the tool.
func (c *ConnectionFlags) RegisterWithDefaults(fs *flag.FlagSet, d ConnectionDefaults) {
	c.Host, c.Port, c.CalledAE, c.CallingAE, c.Timeout = d.Host, d.Port, d.CalledAE, d.CallingAE, d.Timeout
	c.listener = d.Listener
	c.explicit = make(map[string]string)
	stringFlag := func(name string, target *string, usage string, normalize func(string) (string, error)) {
		fs.Var(connectionValue{func() string { return *target }, func(s string) error {
			v, err := normalize(s)
			if err != nil {
				return err
			}
			if err = c.record(map[string]string{name: v}); err != nil {
				return err
			}
			*target = v
			return nil
		}}, name, usage)
	}
	host := func(s string) (string, error) {
		if err := validateConnectionHost(s, c.listener); err != nil {
			return "", err
		}
		return s, nil
	}
	ae := normalizeConnectionAE
	hostUse, calledUse, callingUse, timeUse := "SCP host name or unbracketed IP address", "called peer AE title", "calling local AE title", "tool association/operation timeout (existing tool semantics)"
	if c.listener {
		hostUse = "bind host name or unbracketed IP address; empty binds all interfaces"
		calledUse = "local SCP AE title"
		callingUse = "optional exact Calling AE filter; empty default permits any peer"
		timeUse = "association negotiation timeout; does not limit server lifetime"
	}
	stringFlag("host", &c.Host, hostUse, host)
	stringFlag("called", &c.CalledAE, calledUse, ae)
	stringFlag("calling", &c.CallingAE, callingUse, ae)
	fs.Var(connectionValue{func() string { return strconv.Itoa(c.Port) }, func(s string) error {
		v, err := parseConnectionPort(s, c.listener)
		if err != nil {
			return err
		}
		if err = c.record(map[string]string{"port": strconv.Itoa(v)}); err != nil {
			return err
		}
		c.Port = v
		return nil
	}}, "port", "TCP port (listener permits 0 for an ephemeral port)")
	fs.Var(connectionValue{func() string { return c.Timeout.String() }, func(s string) error {
		v, err := time.ParseDuration(s)
		if err != nil || v <= 0 {
			return errors.New("-timeout must be a positive duration")
		}
		if err = c.record(map[string]string{"timeout": v.String()}); err != nil {
			return err
		}
		c.Timeout = v
		return nil
	}}, "timeout", timeUse)
	fs.Var(connectionValue{func() string { return "" }, func(s string) error {
		i := strings.LastIndexByte(s, '@')
		if i < 0 || len(s) > 1024 {
			return errors.New("-c requires AET@host:port (IPv6 in brackets), at most 1024 bytes")
		}
		title, err := normalizeConnectionAE(s[:i])
		if err != nil {
			return err
		}
		h, p, err := c.parseAddress(s[i+1:])
		if err != nil {
			return err
		}
		if err = c.record(map[string]string{"called": title, "host": h, "port": strconv.Itoa(p)}); err != nil {
			return err
		}
		c.CalledAE, c.Host, c.Port = title, h, p
		return nil
	}}, "c", "AET@host:port shorthand; IPv6 requires AET@[address]:port; same values may repeat, conflicts refused")
	for _, pair := range [][2]string{{"called-ae", "called"}, {"called-aet", "called"}, {"calling-ae", "calling"}, {"calling-aet", "calling"}} {
		c.Alias(fs, pair[0], pair[1])
	}
	if !c.listener {
		c.RegisterAddressAlias(fs, "remote")
	}
	c.TLS.Register(fs, c.listener)
	if !c.listener {
		c.Identity.Register(fs)
	}
}

// Alias registers a deprecated spelling sharing the canonical parser and
// conflict accounting; callers use it for their historical local AE/time flags.
func (c *ConnectionFlags) Alias(fs *flag.FlagSet, alias, canonical string) {
	fs.Var(fs.Lookup(canonical).Value, alias, "deprecated alias for -"+canonical)
}

// RegisterAddressAlias retains a tool's old host:port spelling.
func (c *ConnectionFlags) RegisterAddressAlias(fs *flag.FlagSet, name string) {
	fs.Var(connectionValue{func() string { return c.Address() }, c.SetAddress}, name, "deprecated host:port alias for -host/-port; IPv6 requires brackets")
}

func (c *ConnectionFlags) record(values map[string]string) error {
	if c.explicit == nil {
		c.explicit = make(map[string]string)
	}
	for _, key := range []string{"called", "calling", "host", "port", "timeout"} {
		value, present := values[key]
		if !present {
			continue
		}
		if previous, ok := c.explicit[key]; ok && previous != value {
			return fmt.Errorf("conflicting explicit values for -%s", key)
		}
	}
	for key, value := range values {
		c.explicit[key] = value
	}
	return nil
}
func normalizeConnectionAE(s string) (string, error) {
	if len(s) > 1024 {
		return "", errors.New("AE flag exceeds 1024 bytes before trimming padding")
	}
	if err := ul.ValidateAETitle(s); err != nil {
		return "", err
	}
	return ul.NormalizeAETitle(s), nil
}

func parseConnectionPort(s string, listener bool) (int, error) {
	if s == "" || len(s) > 5 {
		return 0, errors.New("TCP port must be decimal in range")
	}
	for _, b := range []byte(s) {
		if b < '0' || b > '9' {
			return 0, errors.New("TCP port must be decimal in range")
		}
	}
	v, err := strconv.Atoi(s)
	if err != nil || v > 65535 || v == 0 && !listener {
		return 0, errors.New("TCP port is outside the tool role's range")
	}
	return v, nil
}
func validateConnectionHost(s string, listener bool) error {
	if s == "" && listener {
		return nil
	}
	if s == "" || len(s) > 1024 {
		return errors.New("host must be nonempty and at most 1024 bytes")
	}
	for _, b := range []byte(s) {
		if b <= 0x20 || b >= 0x7f || strings.ContainsRune("[]/@\\", rune(b)) {
			return errors.New("host contains invalid address characters")
		}
	}
	if strings.Contains(s, ":") {
		ip, zone, _ := strings.Cut(s, "%")
		if net.ParseIP(ip) == nil || strings.Contains(zone, "%") || strings.Contains(s, "%") && zone == "" {
			return errors.New("host must be a valid unbracketed IPv6 address")
		}
	}
	return nil
}
func (c *ConnectionFlags) parseAddress(s string) (string, int, error) {
	if len(s) > 1024 {
		return "", 0, errors.New("host:port exceeds 1024 bytes")
	}
	h, p, err := net.SplitHostPort(s)
	if err != nil {
		return "", 0, errors.New("address must be host:port; IPv6 requires brackets")
	}
	if err = validateConnectionHost(h, c.listener); err != nil {
		return "", 0, err
	}
	n, err := parseConnectionPort(p, c.listener)
	return h, n, err
}

// IsAddress reports whether address is a valid endpoint for this tool role,
// without I/O, mutation or comparing it with previously selected flag values.
// It lets a positional CLI distinguish endpoints from paths containing colons.
func (c *ConnectionFlags) IsAddress(address string) bool {
	_, _, err := c.parseAddress(address)
	return err == nil
}

// SetAddress consumes a positional or legacy endpoint with the same conflict
// policy as canonical flags. A failed parse/conflict leaves every field intact.
func (c *ConnectionFlags) SetAddress(address string) error {
	h, p, err := c.parseAddress(address)
	if err != nil {
		return err
	}
	if err = c.record(map[string]string{"host": h, "port": strconv.Itoa(p)}); err != nil {
		return err
	}
	c.Host, c.Port = h, p
	return nil
}

// EndpointExplicit reports whether the user selected host/port via any spelling.
func (c *ConnectionFlags) EndpointExplicit() bool {
	_, h := c.explicit["host"]
	_, p := c.explicit["port"]
	return h || p
}

// Listener reports the role selected when flags were registered.
func (c *ConnectionFlags) Listener() bool { return c.listener }
func (c *ConnectionFlags) validateConnectionFlags() error {
	var errs []error
	errs = append(errs, validateConnectionHost(c.Host, c.listener))
	if c.Port < 0 || c.Port > 65535 || c.Port == 0 && !c.listener {
		errs = append(errs, errors.New("-port is outside the tool role's range"))
	}
	if err := ul.ValidateAETitle(c.CalledAE); err != nil {
		errs = append(errs, fmt.Errorf("-called: %w", err))
	}
	if c.CallingAE != "" || !c.listener {
		if err := ul.ValidateAETitle(c.CallingAE); err != nil {
			errs = append(errs, fmt.Errorf("-calling: %w", err))
		}
	}
	if c.Timeout < 0 || c.Timeout == 0 && (!c.listener || c.explicit["timeout"] != "") {
		errs = append(errs, errors.New("-timeout must be positive"))
	}
	errs = append(errs, c.TLS.Validate())
	if !c.listener {
		errs = append(errs, c.Identity.Validate())
	}
	return errors.Join(errs...)
}

// ApplyListenerOptions adds the optional exact Calling AE filter without
// replacing any existing admission policy. It preserves existing timeouts when
// this ConnectionFlags was not registered as a listener.
func (c *ConnectionFlags) ApplyListenerOptions(opts ul.AcceptOptions) ul.AcceptOptions {
	if !c.listener {
		return opts
	}
	opts.NegotiationTimeout = c.Timeout
	if c.CallingAE != "" {
		title := c.CallingAE
		next := opts.Admission
		opts.Admission = func(ctx context.Context, request ul.AdmissionRequest) *ul.AssociationRJ {
			if !ul.EqualAETitles(request.CallingAETitle, title) {
				return ul.RejectCallingAETitle()
			}
			if next != nil {
				return next(ctx, request)
			}
			return nil
		}
	}
	return opts
}
