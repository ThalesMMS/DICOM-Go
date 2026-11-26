package ul

import (
	"crypto/tls"
	"errors"
	"fmt"
	"io"
	"net"
	"sync"
	"sync/atomic"
	"syscall"
	"time"
)

// TLSClientCertificateRefusedError reports an association request over TLS
// 1.3 that the server ended right after the handshake: the server asked for
// a client certificate, the handshake completed on the requestor's side, and
// the connection reached its end of stream before the server sent anything
// else, not even an alert or a session ticket, and before any answer to the
// A-ASSOCIATE-RQ.
//
// In TLS 1.3 the server judges the client certificate, or its absence, only
// after the client has sent its last handshake message, so the client's
// handshake succeeds even when the server refuses it. The refusal is an alert
// followed by a close, often a TCP reset because the A-ASSOCIATE-RQ is
// already on its way. From the moment the server asks for a certificate the
// requestor keeps a read waiting on the connection, so it receives the alert
// even when the reset follows at once, and the error is then the alert. This
// error names a refusal whose server ended the stream without an alert.
//
// A server that accepts the client sends a session ticket right away. The
// requestor accepts tickets for that purpose even when its configuration has
// no ClientSessionCache, and keeps none of them. An end of stream delivers
// everything sent before it, so an accepted client whose association the
// server then closes is not reported as refused, unless the server sends no
// tickets or the requestor has SessionTicketsDisabled: an accepted handshake
// followed by an end of stream then reads as a refusal.
//
// A connection reset is never reported with this error. A reset can discard
// what the server sent, at the requestor, where Windows discards what was not
// read yet, or at the server, whose abortive close discards what it had not
// sent yet. A reset with nothing received after the handshake therefore does
// not tell a refusal from an accepted client, and it remains the plain
// closed-connection error even when it was a refusal whose alert was lost.
//
// Err is the end-of-stream error that was seen; errors.Is and errors.As still
// find it.
type TLSClientCertificateRefusedError struct {
	// CertificateSent reports whether the requestor presented a client
	// certificate.
	CertificateSent bool
	Err             error
}

func (e *TLSClientCertificateRefusedError) Error() string {
	presented := "none was sent"
	if e.CertificateSent {
		presented = "one was sent"
	}
	return "dicom ul: the TLS 1.3 peer asked for a client certificate, " + presented +
		", and it closed the connection before answering: " + errorText(e.Err)
}

func (e *TLSClientCertificateRefusedError) Unwrap() error { return e.Err }

func errorText(err error) string {
	if err == nil {
		return "connection closed"
	}
	return err.Error()
}

// tlsClientAuthWatch observes the TLS connection of an association requestor:
// whether the server asked for a client certificate, whether one was sent,
// and how many bytes the server sent after the handshake.
type tlsClientAuthWatch struct {
	requested atomic.Bool
	sent      atomic.Bool
	raw       *watchedConn
	conn      *tls.Conn
	// handshakeBytes is what raw had handed to TLS when the handshake
	// returned.
	handshakeBytes int64
}

// settle records the end of a successful handshake.
func (w *tlsClientAuthWatch) settle(conn *tls.Conn) {
	w.conn, w.handshakeBytes = conn, w.raw.delivered.Load()
	w.raw.settled.Store(true)
}

// watchClientAuth changes config so that its client certificate selection
// records the server's request and the answer, and arms the waiting reads of
// raw before the requestor sends its certificate. It selects the same
// certificate crypto/tls would: the config's own GetClientCertificate, or
// else the first of Certificates that the request supports.
func (w *tlsClientAuthWatch) watchClientAuth(config *tls.Config) {
	selectCertificate := config.GetClientCertificate
	certificates := config.Certificates
	config.GetClientCertificate = func(request *tls.CertificateRequestInfo) (*tls.Certificate, error) {
		w.requested.Store(true)
		w.raw.arm()
		var chosen *tls.Certificate
		if selectCertificate != nil {
			certificate, err := selectCertificate(request)
			if err != nil {
				return nil, err
			}
			chosen = certificate
		} else {
			for i := range certificates {
				if request.SupportsCertificate(&certificates[i]) == nil {
					chosen = &certificates[i]
					break
				}
			}
			if chosen == nil {
				chosen = new(tls.Certificate)
			}
		}
		w.sent.Store(chosen != nil && len(chosen.Certificate) > 0)
		return chosen, nil
	}
}

// acceptSessionTickets lets a TLS 1.3 server that accepted the client send
// its session ticket, which is what tells an accepted client apart from a
// refused one when the connection then closes before any answer. A server
// sends tickets only to a client that offers psk_key_exchange_modes (RFC 8446,
// section 4.2.9), and crypto/tls offers them only with a ClientSessionCache.
// When the caller set none, the tickets go to a cache that keeps nothing, so
// no session is ever resumed. A caller's own cache, or its
// SessionTicketsDisabled, is left as it is.
func acceptSessionTickets(config *tls.Config) {
	if config.ClientSessionCache == nil && !config.SessionTicketsDisabled {
		config.ClientSessionCache = discardSessionCache{}
	}
}

// discardSessionCache accepts session tickets and keeps none.
type discardSessionCache struct{}

func (discardSessionCache) Get(string) (*tls.ClientSessionState, bool) { return nil, false }
func (discardSessionCache) Put(string, *tls.ClientSessionState)        {}

// refusal returns what ended the negotiation of a TLS 1.3 association whose
// server asked for a client certificate, when err is the connection closing:
//
//   - the server's alert, when the server sent one that TLS had not read
//     because the close ended a write first;
//   - a TLSClientCertificateRefusedError, when the connection ended with an
//     end of stream and the server sent nothing after the handshake;
//   - err otherwise. A reset is never a refusal by itself: it can discard
//     what the server sent, at either end, so it cannot show that the server
//     sent nothing.
func (w *tlsClientAuthWatch) refusal(err error) error {
	if w == nil || err == nil || !w.requested.Load() || !closedByPeer(err) {
		return err
	}
	if w.conn.ConnectionState().Version != tls.VersionTLS13 {
		return err
	}
	// A write that failed on the close can return while the waiting read
	// already holds what the server sent before it, but has not counted it
	// yet; let that read finish first.
	w.raw.finishReads(100 * time.Millisecond)
	if w.raw.received.Load() != w.handshakeBytes {
		return w.unreadAlert(err)
	}
	if !errors.Is(err, io.EOF) && !errors.Is(err, io.ErrUnexpectedEOF) {
		return err
	}
	return &TLSClientCertificateRefusedError{CertificateSent: w.sent.Load(), Err: err}
}

// unreadAlert reads, through TLS, what the server sent after the handshake
// and TLS has not read yet, and returns the server's alert, wrapping err,
// when that is what it was; otherwise it returns err.
func (w *tlsClientAuthWatch) unreadAlert(err error) error {
	if w.raw.received.Load() == w.raw.delivered.Load() {
		return err
	}
	// The connection is closed, so the read ends at once; the deadline only
	// bounds a peer that half-closed it.
	_ = w.conn.SetReadDeadline(time.Now().Add(100 * time.Millisecond))
	_, readErr := w.conn.Read(make([]byte, 1))
	var alert *net.OpError
	if errors.As(readErr, &alert) && alert.Op == "remote error" {
		return fmt.Errorf("%w; %w", readErr, err)
	}
	return err
}

// Winsock reports a reset or aborted connection with these numbers, which
// Go does not map to the portable errno constants.
const (
	wsaeConnectionAborted = syscall.Errno(10053)
	wsaeConnectionReset   = syscall.Errno(10054)
)

// closedByPeer reports whether err is the connection ending under a read or
// a write: an end of stream, a reset or an aborted connection.
func closedByPeer(err error) bool {
	if errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF) {
		return true
	}
	var errno syscall.Errno
	if !errors.As(err, &errno) {
		return false
	}
	switch errno {
	case syscall.ECONNRESET, syscall.ECONNABORTED, syscall.EPIPE, wsaeConnectionReset, wsaeConnectionAborted:
		return true
	}
	return false
}

// readAheadSize holds a whole TLS record.
const readAheadSize = 1<<14 + 2048

// watchedConn is the connection under a requestor's TLS. It counts the bytes
// it received and those it handed to TLS and, once armed, keeps a read
// waiting on the connection in a goroutine of its own until a read completes
// after the handshake has returned. Everything the server sends after it
// judged the client certificate, its alert or its session ticket, then lands
// in a read that is already waiting: a TCP reset that follows at once
// discards data the requestor had not read yet, as Windows does, but not
// data a read already received. The reads queue what they receive for TLS.
type watchedConn struct {
	net.Conn
	received  atomic.Int64
	delivered atomic.Int64
	// settled is set once the handshake has returned.
	settled atomic.Bool

	mu    sync.Mutex
	ready *sync.Cond
	// started is set by arm; armed while reads go through the queue.
	started, armed bool
	// queued holds what the reads received and TLS has not read yet, and
	// failure the error the last read ended with; done reports that the
	// goroutine ended, after which TLS reads the connection itself again.
	queued  [][]byte
	failure error
	done    bool
}

func newWatchedConn(conn net.Conn) *watchedConn {
	c := &watchedConn{Conn: conn}
	c.ready = sync.NewCond(&c.mu)
	return c
}

// arm starts the waiting reads, once, and returns when the first is about to
// wait on the connection.
func (c *watchedConn) arm() {
	c.mu.Lock()
	if c.started {
		c.mu.Unlock()
		return
	}
	c.started, c.armed = true, true
	c.mu.Unlock()
	waiting := make(chan struct{})
	go c.readAhead(waiting)
	<-waiting
}

// finishReads returns once the waiting reads have ended and queued what they
// received. On a connection that failed, the read in progress ends at once;
// a read that is still waiting after wait ends on a passed deadline.
func (c *watchedConn) finishReads(wait time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if !c.started || c.done {
		return
	}
	_ = c.Conn.SetReadDeadline(time.Now().Add(wait))
	for !c.done {
		c.ready.Wait()
	}
}

func (c *watchedConn) readAhead(waiting chan struct{}) {
	for {
		buffer := make([]byte, readAheadSize)
		if waiting != nil {
			close(waiting)
			waiting = nil
		}
		n, err := c.Conn.Read(buffer)
		c.received.Add(int64(n))
		end := err != nil || c.settled.Load()
		c.mu.Lock()
		if n > 0 {
			c.queued = append(c.queued, buffer[:n])
		}
		// A deadline that passed is not queued: TLS reads the connection
		// itself next, with the deadline in force then.
		var timeout net.Error
		if err != nil && !(errors.As(err, &timeout) && timeout.Timeout()) {
			c.failure = err
		}
		c.done = end
		c.ready.Broadcast()
		c.mu.Unlock()
		if end {
			return
		}
	}
}

func (c *watchedConn) Read(p []byte) (int, error) {
	c.mu.Lock()
	for c.armed {
		switch {
		case len(c.queued) > 0:
			n := copy(p, c.queued[0])
			if c.queued[0] = c.queued[0][n:]; len(c.queued[0]) == 0 {
				c.queued = c.queued[1:]
			}
			c.mu.Unlock()
			c.delivered.Add(int64(n))
			return n, nil
		case c.failure != nil:
			err := c.failure
			c.failure, c.armed = nil, false
			c.mu.Unlock()
			return 0, err
		case c.done:
			c.armed = false
		default:
			c.ready.Wait()
		}
	}
	c.mu.Unlock()
	n, err := c.Conn.Read(p)
	c.received.Add(int64(n))
	c.delivered.Add(int64(n))
	return n, err
}
