package ul

import (
	"errors"
	"fmt"
	"net"
	"syscall"
)

// ErrSocketBuffersUnsupported reports a platform or connection whose socket
// buffer sizes cannot be read or set.
var ErrSocketBuffersUnsupported = errors.New("dicom ul: socket buffer sizes are not supported on this connection")

// MaxSocketBufferBytes bounds a requested socket buffer size (64 MiB). The
// kernel may grant less (net.core.rmem_max/wmem_max on Linux,
// kern.ipc.maxsockbuf on macOS) or, on Linux, report twice the value.
const MaxSocketBufferBytes = 64 << 20

// SocketBuffers sets the kernel send and receive buffer sizes (SO_SNDBUF and
// SO_RCVBUF) of the TCP sockets of associations. A zero field leaves the
// operating system default, including its automatic buffer tuning, which an
// explicit size disables on Linux and macOS. Larger buffers raise the
// throughput ceiling of a single association on a path whose bandwidth-delay
// product exceeds the default window (about buffer / round-trip time); on a
// LAN or loopback they bring nothing. See docs/CONFORMANCE.md.
//
// The sizes are applied before connect and before listen, so the TCP window
// scale offered in the handshake can cover them, and again to each accepted
// socket.
type SocketBuffers struct {
	// Send is SO_SNDBUF in bytes, the unacknowledged data a sender keeps.
	Send int
	// Receive is SO_RCVBUF in bytes, the window a receiver advertises.
	Receive int
}

// IsZero reports whether both sizes are left to the operating system.
func (b SocketBuffers) IsZero() bool { return b.Send == 0 && b.Receive == 0 }

func (b SocketBuffers) validate() error {
	for _, size := range []struct {
		name  string
		value int
	}{{"send", b.Send}, {"receive", b.Receive}} {
		if size.value < 0 || size.value > MaxSocketBufferBytes {
			return fmt.Errorf("dicom ul: %s socket buffer %d outside 0..%d bytes", size.name, size.value, MaxSocketBufferBytes)
		}
	}
	return nil
}

// control sets the sizes on a socket before connect or listen, as the
// Control hook of net.Dialer and net.ListenConfig.
func (b SocketBuffers) control(_, _ string, raw syscall.RawConn) error {
	if b.IsZero() {
		return nil
	}
	var sockErr error
	if err := raw.Control(func(fd uintptr) { sockErr = setSocketBuffers(fd, b) }); err != nil {
		return err
	}
	return sockErr
}

// apply sets the sizes on an established TCP connection.
func (b SocketBuffers) apply(conn net.Conn) error {
	if b.IsZero() {
		return nil
	}
	tcp, ok := conn.(*net.TCPConn)
	if !ok {
		return ErrSocketBuffersUnsupported
	}
	if b.Send > 0 {
		if err := tcp.SetWriteBuffer(b.Send); err != nil {
			return fmt.Errorf("dicom ul: set send buffer: %w", err)
		}
	}
	if b.Receive > 0 {
		if err := tcp.SetReadBuffer(b.Receive); err != nil {
			return fmt.Errorf("dicom ul: set receive buffer: %w", err)
		}
	}
	return nil
}

// EffectiveSocketBuffers reports the send and receive buffer sizes the kernel
// granted to a TCP connection (on Linux twice the requested value, which
// includes bookkeeping overhead). It returns ErrSocketBuffersUnsupported for
// other connections, such as a TLS connection's wrapper, and on platforms
// without getsockopt.
func EffectiveSocketBuffers(conn net.Conn) (SocketBuffers, error) {
	tcp, ok := conn.(*net.TCPConn)
	if !ok {
		return SocketBuffers{}, ErrSocketBuffersUnsupported
	}
	raw, err := tcp.SyscallConn()
	if err != nil {
		return SocketBuffers{}, err
	}
	var sizes SocketBuffers
	var sockErr error
	if err := raw.Control(func(fd uintptr) { sizes, sockErr = getSocketBuffers(fd) }); err != nil {
		return SocketBuffers{}, err
	}
	return sizes, sockErr
}
