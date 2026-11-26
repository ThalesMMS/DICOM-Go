package ul

import (
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"net"
	"testing"
	"time"
)

// rawPeer dials the listener and sends an A-ASSOCIATE-RQ.
func rawPeer(t *testing.T, addr string, dial DialOptions) net.Conn {
	t.Helper()
	conn, err := (&net.Dialer{}).DialContext(context.Background(), "tcp", addr)
	if err != nil {
		t.Fatal(err)
	}
	if dial.Contexts == nil {
		dial.Contexts = []PresentationContext{{AbstractSyntaxUID: verificationSOPClassUID, TransferSyntaxUIDs: []string{ImplicitVRLittleEndian}}}
	}
	rq := buildAssociateRQ(dial)
	if err := WritePDU(conn, &rq); err != nil {
		t.Fatal(err)
	}
	return conn
}

func acceptOnce(t *testing.T, opts AcceptOptions) (string, chan error) {
	t.Helper()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	// A requestor that never connects, such as one whose dial failed, ends
	// the wait with an error instead of leaving it blocked forever.
	_ = listener.(*net.TCPListener).SetDeadline(time.Now().Add(10 * time.Second))
	done := make(chan error, 1)
	go func() {
		defer func() { _ = listener.Close() }()
		conn, err := listener.Accept()
		if err != nil {
			done <- err
			return
		}
		assoc, err := Accept(conn, opts)
		if assoc != nil {
			_ = assoc.Close()
		}
		done <- err
	}()
	return listener.Addr().String(), done
}

// TestRejectionLingersWhilePeerKeepsSending: a peer that keeps writing still
// reads the A-ASSOCIATE-RJ (no reset), and the acceptor gives up after the
// ARTIM deadline.
func TestRejectionLingersWhilePeerKeepsSending(t *testing.T) {
	const artim = 300 * time.Millisecond
	addr, done := acceptOnce(t, AcceptOptions{AETitle: "ARCHIVE", RequireMatchingCalledAE: true, ARTIMTimeout: artim})
	conn := rawPeer(t, addr, DialOptions{CalledAETitle: "OTHER"})
	defer func() { _ = conn.Close() }()
	stop := make(chan struct{})
	defer close(stop)
	go func() {
		chunk := bytes.Repeat([]byte{0x04}, 1024)
		for {
			select {
			case <-stop:
				return
			default:
			}
			if _, err := conn.Write(chunk); err != nil {
				return
			}
			time.Sleep(time.Millisecond)
		}
	}()
	started := time.Now()
	pdu, err := ReadPDU(conn, DefaultMaxPDU)
	if err != nil {
		t.Fatalf("peer could not read the rejection: %v", err)
	}
	if rj, ok := pdu.(*AssociationRJ); !ok || rj.Reason != AssociateRJReasonCalledAETitleNotRecognized {
		t.Fatalf("response %#v", pdu)
	}
	select {
	case err := <-done:
		if !errors.Is(err, ErrAssociationRejected) {
			t.Fatalf("Accept error %v", err)
		}
		if elapsed := time.Since(started); elapsed < artim/2 || elapsed > artim+time.Second {
			t.Fatalf("acceptor lingered %v, want about %v", elapsed, artim)
		}
	case <-time.After(artim + 2*time.Second):
		t.Fatal("acceptor did not honor the ARTIM deadline")
	}
}

// TestRejectionClosesWhenPeerCloses: the linger ends as soon as the peer
// closes.
func TestRejectionClosesWhenPeerCloses(t *testing.T) {
	addr, done := acceptOnce(t, AcceptOptions{AETitle: "ARCHIVE", RequireMatchingCalledAE: true, ARTIMTimeout: 10 * time.Second})
	conn := rawPeer(t, addr, DialOptions{CalledAETitle: "OTHER"})
	if _, err := ReadPDU(conn, DefaultMaxPDU); err != nil {
		t.Fatal(err)
	}
	started := time.Now()
	_ = conn.Close()
	select {
	case <-done:
		if elapsed := time.Since(started); elapsed > 2*time.Second {
			t.Fatalf("acceptor waited %v after the peer closed", elapsed)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("acceptor did not close after the peer did")
	}
}

func TestMalformedRequestGetsProviderAbort(t *testing.T) {
	for _, tc := range []struct {
		name   string
		pdu    []byte
		reason byte
	}{
		// PDU type 0x01 declaring a length above the acceptor's limit.
		{"oversized", []byte{0x01, 0x00, 0x7F, 0xFF, 0xFF, 0xFF}, AbortReasonInvalidPDUParameter},
		// A-ASSOCIATE-RQ whose body is too short for its fixed fields.
		{"truncated fields", append([]byte{0x01, 0x00, 0x00, 0x00, 0x00, 0x04}, 0x00, 0x01, 0x00, 0x00), AbortReasonInvalidPDUParameter},
		// Unknown PDU type instead of an A-ASSOCIATE-RQ.
		{"unknown type", []byte{0x7F, 0x00, 0x00, 0x00, 0x00, 0x02, 0x00, 0x00}, AbortReasonUnrecognizedPDU},
		// A-RELEASE-RP instead of an A-ASSOCIATE-RQ.
		{"unexpected type", []byte{0x06, 0x00, 0x00, 0x00, 0x00, 0x04, 0x00, 0x00, 0x00, 0x00}, AbortReasonUnexpectedPDU},
	} {
		t.Run(tc.name, func(t *testing.T) {
			addr, done := acceptOnce(t, AcceptOptions{AETitle: "ARCHIVE", ARTIMTimeout: 200 * time.Millisecond, MaxPDU: 1 << 16})
			conn, err := (&net.Dialer{}).DialContext(context.Background(), "tcp", addr)
			if err != nil {
				t.Fatal(err)
			}
			defer func() { _ = conn.Close() }()
			if _, err := conn.Write(tc.pdu); err != nil {
				t.Fatal(err)
			}
			pdu, err := ReadPDU(conn, DefaultMaxPDU)
			if err != nil {
				t.Fatalf("read abort: %v", err)
			}
			abort, ok := pdu.(*AbortRQ)
			if !ok || abort.Source != AbortSourceServiceProvider || abort.Reason != tc.reason {
				t.Fatalf("response %#v, want provider abort reason %d", pdu, tc.reason)
			}
			<-done
		})
	}
}

// TestServerAbortsUnknownPDUDuringDataTransfer: a handler that stops on an
// unknown PDU makes the server send an A-P-ABORT with reason 1.
func TestServerAbortsUnknownPDUDuringDataTransfer(t *testing.T) {
	listener, err := Listen(ListenOptions{Address: "127.0.0.1:0"})
	if err != nil {
		t.Fatal(err)
	}
	server, err := NewAssociationServer(listener, AssociationServerOptions{
		Accept: AcceptOptions{AETitle: "ARCHIVE", AcceptAnyAbstractSyntax: true, SupportedTransferSyntaxes: []string{ImplicitVRLittleEndian}, ARTIMTimeout: 200 * time.Millisecond},
		Handler: func(ctx context.Context, assoc *Association) error {
			pdu, err := assoc.Receive(ctx)
			if err != nil {
				return err
			}
			return UnexpectedPDUError(pdu, "waiting for P-DATA-TF")
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	go func() { _ = server.Serve(context.Background()) }()
	defer func() {
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		defer cancel()
		_ = server.Shutdown(ctx)
	}()
	conn := rawPeer(t, listener.Addr().String(), DialOptions{CalledAETitle: "ARCHIVE"})
	defer func() { _ = conn.Close() }()
	if pdu, err := ReadPDU(conn, DefaultMaxPDU); err != nil {
		t.Fatal(err)
	} else if _, ok := pdu.(*AssociationAC); !ok {
		t.Fatalf("response %T", pdu)
	}
	unknown := make([]byte, 10)
	unknown[0] = 0x7E
	binary.BigEndian.PutUint32(unknown[2:], 4)
	if _, err := conn.Write(unknown); err != nil {
		t.Fatal(err)
	}
	pdu, err := ReadPDU(conn, DefaultMaxPDU)
	if err != nil {
		t.Fatal(err)
	}
	if abort, ok := pdu.(*AbortRQ); !ok || abort.Source != AbortSourceServiceProvider || abort.Reason != AbortReasonUnrecognizedPDU {
		t.Fatalf("response %#v, want provider abort reason 1", pdu)
	}
}

func TestProviderAbortReason(t *testing.T) {
	for _, tc := range []struct {
		err    error
		reason byte
		ok     bool
	}{
		{UnexpectedPDUError(&UnknownPDU{Type: 0x7F}, "x"), AbortReasonUnrecognizedPDU, true},
		{UnexpectedPDUError(&ReleaseRP{}, "x"), AbortReasonUnexpectedPDU, true},
		{fmt.Errorf("wrap: %w", ErrInvalidPDUItem), AbortReasonUnrecognizedPDUParameter, true},
		{fmt.Errorf("wrap: %w", ErrPDUTooLarge), AbortReasonInvalidPDUParameter, true},
		{ErrMissingPDUField, AbortReasonInvalidPDUParameter, true},
		{context.DeadlineExceeded, 0, false},
		{net.ErrClosed, 0, false},
		{errors.New("handler failed"), 0, false},
		{nil, 0, false},
	} {
		reason, ok := ProviderAbortReason(tc.err)
		if reason != tc.reason || ok != tc.ok {
			t.Errorf("%v: %d %v, want %d %v", tc.err, reason, ok, tc.reason, tc.ok)
		}
	}
	if !errors.Is(UnexpectedPDUError(&UnknownPDU{}, "x"), ErrUnexpectedPDU) {
		t.Fatal("an unrecognized PDU must remain ErrUnexpectedPDU")
	}
}
