package ul

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"testing"
	"time"
)

// BenchmarkSocketBuffersReceiverWindow measures how the acceptor's SO_RCVBUF
// (ListenOptions.SocketBuffers.Receive) bounds one association's throughput
// on a path with simulated latency. Over an established loopback
// association the requestor streams 8 MiB on the association socket while
// the acceptor drains its socket once per round-trip time, so each round
// trip delivers at most what the kernel receive buffer held: the receive
// window bound (buffer / RTT) of a real long path. The sender-side bound and
// congestion control are not emulated; RTT 0 is plain loopback. "default"
// leaves the operating system size and its automatic tuning.
func BenchmarkSocketBuffersReceiverWindow(b *testing.B) {
	const payloadBytes = 8 << 20
	payload := bytes.Repeat([]byte{0x5a}, payloadBytes)
	for _, rtt := range []time.Duration{0, 10 * time.Millisecond, 40 * time.Millisecond} {
		for _, receive := range []int{0, 64 << 10, 256 << 10, 1 << 20, 4 << 20} {
			name := "default"
			if receive > 0 {
				name = fmt.Sprintf("%dKiB", receive>>10)
			}
			b.Run(fmt.Sprintf("rtt=%s/receive=%s", rtt, name), func(b *testing.B) {
				benchmarkReceiverWindow(b, payload, rtt, SocketBuffers{Receive: receive})
			})
		}
	}
}

func benchmarkReceiverWindow(b *testing.B, payload []byte, rtt time.Duration, buffers SocketBuffers) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
	defer cancel()
	listener, err := Listen(ListenOptions{Address: "127.0.0.1:0", Context: ctx, SocketBuffers: buffers})
	if err != nil {
		b.Fatal(err)
	}
	defer func() { _ = listener.Close() }()
	type result struct {
		effective SocketBuffers
		err       error
	}
	serverDone := make(chan result, 1)
	go func() {
		assoc, err := listener.AcceptAssociation(AcceptOptions{AETitle: "SCP", Context: ctx, SupportedAbstractSyntaxes: []string{"1.2.840.10008.1.1"}, SupportedTransferSyntaxes: []string{ImplicitVRLittleEndian}})
		if err != nil {
			serverDone <- result{err: err}
			return
		}
		defer func() { _ = assoc.Close() }()
		var effective SocketBuffers
		buf := make([]byte, 64<<20)
		for {
			total := 0
			for total < len(payload) {
				n, err := assoc.Conn.Read(buf)
				if err != nil {
					serverDone <- result{err: err}
					return
				}
				total += n
				if rtt > 0 && total < len(payload) {
					time.Sleep(rtt)
				}
			}
			// After the transfer, so automatic tuning shows its outcome.
			effective, _ = EffectiveSocketBuffers(assoc.Conn)
			if _, err := assoc.Conn.Write([]byte{1}); err != nil {
				serverDone <- result{effective: effective, err: err}
				return
			}
			if total > len(payload) {
				serverDone <- result{err: fmt.Errorf("read %d bytes past the payload", total-len(payload))}
				return
			}
			var more [1]byte
			if _, err := io.ReadFull(assoc.Conn, more[:]); err != nil || more[0] == 0 {
				serverDone <- result{effective: effective}
				return
			}
		}
	}()
	assoc, err := DialContext(ctx, listener.Addr().String(), DialOptions{CallingAETitle: "SCU", CalledAETitle: "SCP", Contexts: testContexts()})
	if err != nil {
		b.Fatal(err)
	}
	defer func() { _ = assoc.Close() }()
	b.SetBytes(int64(len(payload)))
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if i > 0 {
			if _, err := assoc.Conn.Write([]byte{1}); err != nil {
				b.Fatal(err)
			}
		}
		if _, err := assoc.Conn.Write(payload); err != nil {
			b.Fatal(err)
		}
		var ack [1]byte
		if _, err := io.ReadFull(assoc.Conn, ack[:]); err != nil {
			b.Fatal(err)
		}
	}
	b.StopTimer()
	_, _ = assoc.Conn.Write([]byte{0})
	outcome := <-serverDone
	if outcome.err != nil {
		b.Fatal(outcome.err)
	}
	b.ReportMetric(float64(outcome.effective.Receive), "rcvbuf-bytes")
}
