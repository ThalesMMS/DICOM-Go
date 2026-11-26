package ul

import (
	"context"
	"net"

	"github.com/ThalesMMS/dicom-go/internal/sourceport"
)

// LocalEndpointUnavailable reports whether a TCP dial failed with an error
// that names the local side of the connection: address in use, address not
// available, or no buffer space left. With a source port left to the system
// such errors usually come from ephemeral port pressure, and another attempt
// from a fresh port can succeed. On Windows an address in use is also the
// connect failure behind Tcpip event 4227, a port reused while its TIME_WAIT
// connection to the same remote endpoint lasts. An address not available can
// instead come from a source IP that is not on this host or, on Windows, from
// an invalid remote address, which another attempt does not change; the
// built-in dialer does not retry those.
func LocalEndpointUnavailable(err error) bool {
	return sourceport.Unavailable(err)
}

// SourcePortRetryDialContext returns a dial function, for an http.Transport
// or any other TCP client, with the source port retry of the built-in UL dial:
// a "tcp", "tcp4" or "tcp6" connect that fails before reaching the peer
// because the source endpoint the system chose could not be used is made
// again, up to three more times within the dial context, from a port the
// system chooses afresh. A configured source port (d.LocalAddr), an
// address-not-available error with a configured source IP or an invalid
// remote address, any other network and every other error are not retried. A
// nil d is the dialer of http.DefaultTransport (30 second connect timeout and
// keep-alive), so
//
//	transport.DialContext = ul.SourcePortRetryDialContext(nil)
//
// gives a clone of http.DefaultTransport the retry.
func SourcePortRetryDialContext(d *net.Dialer) func(ctx context.Context, network, address string) (net.Conn, error) {
	return sourceport.DialContext(d)
}

// retryUnavailableSourcePort runs dial again, within ctx and up to three more
// times, while it fails with an error a fresh ephemeral source port can avoid.
// Any other error, such as a refusal by the peer, is returned at once.
var retryUnavailableSourcePort = sourceport.Retry
