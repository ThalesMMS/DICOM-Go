package ul

import (
	"context"
	"errors"
	"net"
	"testing"
	"time"
)

func TestSocketBuffersValidation(t *testing.T) {
	for _, buffers := range []SocketBuffers{{Send: -1}, {Receive: -1}, {Send: MaxSocketBufferBytes + 1}} {
		if _, err := Listen(ListenOptions{Address: "127.0.0.1:0", SocketBuffers: buffers}); err == nil {
			t.Errorf("Listen accepted %+v", buffers)
		}
		_, err := DialContext(context.Background(), "127.0.0.1:1", DialOptions{CallingAETitle: "SCU", CalledAETitle: "SCP", SocketBuffers: buffers, Contexts: testContexts()})
		if err == nil || errors.Is(err, ErrAssociationRejected) {
			t.Errorf("Dial accepted %+v: %v", buffers, err)
		}
	}
	if !(SocketBuffers{}).IsZero() || (SocketBuffers{Send: 1}).IsZero() {
		t.Fatal("IsZero")
	}
	if err := (SocketBuffers{Send: 4096}).apply(&net.UnixConn{}); !errors.Is(err, ErrSocketBuffersUnsupported) {
		t.Fatalf("non-TCP apply: %v", err)
	}
	if _, err := EffectiveSocketBuffers(&net.UnixConn{}); !errors.Is(err, ErrSocketBuffersUnsupported) {
		t.Fatalf("non-TCP effective: %v", err)
	}
}

func testContexts() []PresentationContext {
	return []PresentationContext{{AbstractSyntaxUID: "1.2.840.10008.1.1", TransferSyntaxUIDs: []string{ImplicitVRLittleEndian}}}
}

// The sizes reach both sockets of an association: the requestor's from
// DialOptions, the acceptor's from ListenOptions. The kernel may round the
// value up (Linux doubles it), never below the request on the platforms
// tested here.
func TestSocketBuffersReachBothSides(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	listenBuffers := SocketBuffers{Send: 192 << 10, Receive: 320 << 10}
	dialBuffers := SocketBuffers{Send: 448 << 10, Receive: 256 << 10}
	listener, err := Listen(ListenOptions{Address: "127.0.0.1:0", Context: ctx, SocketBuffers: listenBuffers})
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	accepted := make(chan *Association, 1)
	go func() {
		assoc, err := listener.AcceptAssociation(AcceptOptions{AETitle: "SCP", Context: ctx, SupportedAbstractSyntaxes: []string{"1.2.840.10008.1.1"}, SupportedTransferSyntaxes: []string{ImplicitVRLittleEndian}})
		if err != nil {
			t.Error(err)
		}
		accepted <- assoc
	}()
	assoc, err := DialContext(ctx, listener.Addr().String(), DialOptions{CallingAETitle: "SCU", CalledAETitle: "SCP", Contexts: testContexts(), SocketBuffers: dialBuffers})
	if err != nil {
		t.Fatal(err)
	}
	defer assoc.Close()
	server := <-accepted
	if server == nil {
		t.FailNow()
	}
	defer server.Close()
	for name, side := range map[string]struct {
		conn net.Conn
		want SocketBuffers
	}{"requestor": {assoc.Conn, dialBuffers}, "acceptor": {server.Conn, listenBuffers}} {
		got, err := EffectiveSocketBuffers(side.conn)
		if errors.Is(err, ErrSocketBuffersUnsupported) {
			t.Skipf("effective sizes not readable here: %v", err)
		}
		if err != nil {
			t.Fatal(err)
		}
		if got.Send < side.want.Send || got.Receive < side.want.Receive {
			t.Errorf("%s: effective %+v, requested %+v", name, got, side.want)
		}
		t.Logf("%s: requested %+v effective %+v", name, side.want, got)
	}
	// Plain Listener.Accept applies the sizes too. A dial that fails closes
	// the listener, so that the Accept fails instead of waiting forever.
	dialFailed := make(chan error, 1)
	go func() {
		conn, err := net.Dial("tcp", listener.Addr().String())
		if err != nil {
			dialFailed <- err
			_ = listener.Close()
			return
		}
		defer conn.Close()
		time.Sleep(100 * time.Millisecond)
	}()
	conn, err := listener.Accept()
	if err != nil {
		select {
		case dialErr := <-dialFailed:
			t.Fatalf("dial: %v (Accept: %v)", dialErr, err)
		default:
			t.Fatal(err)
		}
	}
	defer conn.Close()
	if got, err := EffectiveSocketBuffers(conn); err != nil || got.Receive < listenBuffers.Receive {
		t.Fatalf("Listener.Accept: %+v %v", got, err)
	}
}
