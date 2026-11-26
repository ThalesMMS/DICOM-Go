package ul

import "crypto/tls"

// TLSConnectionState returns the completed handshake state of an association
// over TLS. The returned certificates are shared with the TLS connection and
// must be treated as read-only. VerifiedChains reflects the trust settings
// of that connection.
func (a *Association) TLSConnectionState() (tls.ConnectionState, bool) {
	conn, ok := a.connection().(*tls.Conn)
	if !ok {
		return tls.ConnectionState{}, false
	}
	state := conn.ConnectionState()
	return state, state.HandshakeComplete
}
