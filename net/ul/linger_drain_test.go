package ul

import (
	"net"
	"os"
	"sync/atomic"
	"testing"
	"time"
)

// streamingConn is a peer that never stops sending and never closes.
type streamingConn struct {
	net.Conn
	read     atomic.Int64
	deadline atomic.Int64
	closed   atomic.Bool
}

func (c *streamingConn) Read(buffer []byte) (int, error) {
	if deadline := c.deadline.Load(); deadline != 0 && time.Now().UnixNano() >= deadline {
		return 0, os.ErrDeadlineExceeded
	}
	c.read.Add(int64(len(buffer)))
	return len(buffer), nil
}

func (c *streamingConn) SetReadDeadline(deadline time.Time) error {
	c.deadline.Store(deadline.UnixNano())
	return nil
}

func (c *streamingConn) Close() error {
	c.closed.Store(true)
	return nil
}

// After an A-ABORT this side keeps reading what a streaming peer already
// sent, so the peer reaches the abort before the connection is reset. The
// drain is still bounded, in bytes and by the ARTIM deadline.
func TestLingerCloseDrainsAStreamingPeerWithinItsBounds(t *testing.T) {
	conn := &streamingConn{}
	started := time.Now()
	if err := lingerClose(conn, 2*time.Second); err != nil {
		t.Fatal(err)
	}
	if !conn.closed.Load() {
		t.Fatal("the connection was not closed")
	}
	if read := conn.read.Load(); read < 16<<20 || read > maxDrainBytes+(1<<20) {
		t.Fatalf("drained %d bytes, want between 16 MiB and the %d byte bound", read, maxDrainBytes)
	}
	if elapsed := time.Since(started); elapsed > 3*time.Second {
		t.Fatalf("the drain took %s, want it inside the ARTIM deadline", elapsed)
	}

	slow := &slowStreamingConn{}
	started = time.Now()
	if err := lingerClose(slow, 200*time.Millisecond); err != nil {
		t.Fatal(err)
	}
	if elapsed := time.Since(started); elapsed < 150*time.Millisecond || elapsed > 2*time.Second {
		t.Fatalf("the drain of a slow sender took %s, want the 200ms ARTIM deadline", elapsed)
	}
}

// slowStreamingConn sends a byte every 10 ms and never closes.
type slowStreamingConn struct {
	streamingConn
}

func (c *slowStreamingConn) Read(buffer []byte) (int, error) {
	time.Sleep(10 * time.Millisecond)
	if deadline := c.deadline.Load(); deadline != 0 && time.Now().UnixNano() >= deadline {
		return 0, os.ErrDeadlineExceeded
	}
	return 1, nil
}
