package dimsecli

import (
	"context"
	"flag"
	"net"
	"strconv"
	"time"

	"github.com/ThalesMMS/dicom-go/net/ul"
)

// DefaultReleaseTimeout bounds A-RELEASE for the requestor tools.
const DefaultReleaseTimeout = 5 * time.Second

// ConnectionFlags are the peer, AE title, timeout, TLS and User Identity
// options shared by DIMSE requestor and listener tools. Listener role uses
// CalledAE as local title and CallingAE as an optional exact peer filter.
type ConnectionFlags struct {
	Host      string
	Port      int
	CalledAE  string
	CallingAE string
	Timeout   time.Duration
	TLS       TLSFlags
	Identity  UserIdentityFlags
	listener  bool
	explicit  map[string]string
}

// Register adds -host, -port, -called, -calling and -timeout plus the TLS
// and User Identity flags to fs. callingAE is the default calling AE title.
func (c *ConnectionFlags) Register(fs *flag.FlagSet, callingAE string) {
	c.RegisterWithDefaults(fs, ConnectionDefaults{Host: "localhost", Port: 11112, CalledAE: "ANY-SCP", CallingAE: callingAE, Timeout: 30 * time.Second})
}

// Validate checks the connection flags without reading files or secrets.
func (c *ConnectionFlags) Validate() error {
	return c.validateConnectionFlags()
}

// Address is the host:port endpoint (peer or listener bind address).
func (c *ConnectionFlags) Address() string {
	return net.JoinHostPort(c.Host, strconv.Itoa(c.Port))
}

// Dial opens one association proposing contexts and roles. The TLS
// configuration and User Identity are built here, so a missing secret or
// certificate fails before any connection is made.
func (c *ConnectionFlags) Dial(ctx context.Context, contexts []ul.PresentationContext, roles []ul.RoleSelectionItem) (*ul.Association, error) {
	tlsConfig, err := c.TLS.Config()
	if err != nil {
		return nil, err
	}
	identity, err := c.Identity.Request()
	if err != nil {
		return nil, err
	}
	return ul.DialContext(ctx, c.Address(), ul.DialOptions{
		CalledAETitle:  c.CalledAE,
		CallingAETitle: c.CallingAE,
		Contexts:       contexts,
		RoleSelections: roles,
		TLSConfig:      tlsConfig,
		UserIdentity:   identity,
	})
}

// Release releases assoc within DefaultReleaseTimeout and closes it when the
// release fails.
func Release(assoc *ul.Association) error {
	if assoc == nil {
		return nil
	}
	ctx, cancel := context.WithTimeout(context.Background(), DefaultReleaseTimeout)
	defer cancel()
	if err := assoc.Release(ctx); err != nil {
		_ = assoc.Close()
		return err
	}
	return nil
}
