package main

import (
	"context"
	"errors"
	"io"
	"net"
	"os"
	"sync"
	"testing"
	"time"

	"github.com/ThalesMMS/dicom-go/net/dimse"
	"github.com/ThalesMMS/dicom-go/net/ul"
)

type lockedWriter struct {
	mu sync.Mutex
	w  io.Writer
}

func (w *lockedWriter) Write(p []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.w.Write(p)
}

func TestDefaultListenerOptionsBoundNegotiation(t *testing.T) {
	opts, err := parseArgs(nil, io.Discard)
	if err != nil {
		t.Fatal(err)
	}
	accept := opts.conn.ApplyListenerOptions(ul.AcceptOptions{})
	if accept.NegotiationTimeout != ul.DefaultServerNegotiationTimeout {
		t.Fatalf("default NegotiationTimeout = %v, want %v", accept.NegotiationTimeout, ul.DefaultServerNegotiationTimeout)
	}
}

// TestServeTimesOutSilentPeerAndServesNextEcho connects a peer that never
// sends an A-ASSOCIATE-RQ and then a C-ECHO requestor. The negotiation
// timeout closes the silent connection and the requestor is served.
func TestServeTimesOutSilentPeerAndServesNextEcho(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	opts, err := parseArgs(nil, io.Discard)
	if err != nil {
		t.Fatal(err)
	}
	opts.conn.Timeout = 200 * time.Millisecond
	listener, err := ul.Listen(ul.ListenOptions{Address: "127.0.0.1:0", Context: ctx})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = listener.Close() }()
	stdout := &lockedWriter{w: io.Discard}
	stderr := &lockedWriter{w: io.Discard}
	serveDone := make(chan error, 1)
	go func() { serveDone <- serve(listener, opts, nil, nil, stdout, stderr) }()

	silent, err := (&net.Dialer{}).DialContext(ctx, "tcp", listener.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = silent.Close() }()

	assoc, err := ul.DialContext(ctx, listener.Addr().String(), ul.DialOptions{
		CalledAETitle:  "ECHOSCP",
		CallingAETitle: "ECHOSCU",
		Contexts: []ul.PresentationContext{{
			AbstractSyntaxUID:  dimse.VerificationSOPClassUID,
			TransferSyntaxUIDs: []string{ul.ImplicitVRLittleEndian},
		}},
	})
	if err != nil {
		t.Fatalf("DialContext() behind a silent peer error = %v", err)
	}
	pc, ok := dimse.AcceptedVerificationContext(assoc)
	if !ok {
		t.Fatal("AcceptedVerificationContext() = false")
	}
	response, err := dimse.SendCEcho(assoc, pc.ID, 1)
	if err != nil || response.Status != dimse.StatusSuccess {
		t.Fatalf("SendCEcho() = %#v, %v", response, err)
	}
	if err := assoc.Release(ctx); err != nil {
		t.Fatalf("Release() error = %v", err)
	}

	// The server closed the silent connection when its negotiation timed
	// out: the read ends with EOF or a reset, not with this test's deadline.
	deadline, _ := ctx.Deadline()
	_ = silent.SetReadDeadline(deadline)
	if _, err := io.Copy(io.Discard, silent); errors.Is(err, os.ErrDeadlineExceeded) {
		t.Fatal("silent connection was not closed by the negotiation timeout")
	}

	if err := listener.Close(); err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-serveDone:
		if err != nil {
			t.Fatalf("serve() error = %v", err)
		}
	case <-ctx.Done():
		t.Fatal("serve() did not return after the listener closed")
	}
}
