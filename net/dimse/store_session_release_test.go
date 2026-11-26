package dimse

import (
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"net"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/ThalesMMS/dicom-go/core"
	"github.com/ThalesMMS/dicom-go/dictionary/std"
	"github.com/ThalesMMS/dicom-go/net/ul"
	"github.com/ThalesMMS/dicom-go/object"
	"github.com/ThalesMMS/dicom-go/transfer"
)

// releaseIgnoringConn hides every A-RELEASE-RQ the requestor sends from the
// acceptor reading it: the peer never sees, and so never answers, the release.
type releaseIgnoringConn struct {
	net.Conn
	pending []byte
	ignored *atomic.Int32
}

func (c *releaseIgnoringConn) Read(p []byte) (int, error) {
	for len(c.pending) == 0 {
		header := make([]byte, 6)
		if _, err := io.ReadFull(c.Conn, header); err != nil {
			return 0, err
		}
		body := make([]byte, binary.BigEndian.Uint32(header[2:]))
		if _, err := io.ReadFull(c.Conn, body); err != nil {
			return 0, err
		}
		if header[0] == byte(ul.PDUReleaseRQ) {
			c.ignored.Add(1)
			continue
		}
		c.pending = append(header, body...)
	}
	n := copy(p, c.pending)
	c.pending = c.pending[n:]
	return n, nil
}

// releaseIgnoringPeer is a Storage SCP that never answers the release of its
// first association and answers every later one.
type releaseIgnoringPeer struct {
	address      string
	ignored      atomic.Int32
	mu           sync.Mutex
	associations int
	stored       map[string]bool
}

func startReleaseIgnoringPeer(t *testing.T, ctx context.Context, window *ul.AsynchronousOperationsWindow) *releaseIgnoringPeer {
	t.Helper()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = listener.Close() })
	peer := &releaseIgnoringPeer{address: listener.Addr().String(), stored: map[string]bool{}}
	go func() {
		for first := true; ; first = false {
			conn, err := listener.Accept()
			if err != nil {
				return
			}
			if first {
				conn = &releaseIgnoringConn{Conn: conn, ignored: &peer.ignored}
			}
			go func(conn net.Conn) {
				defer conn.Close()
				assoc, err := ul.Accept(conn, ul.AcceptOptions{
					AETitle:                      "RELEASESCP",
					Context:                      ctx,
					AcceptAnyAbstractSyntax:      true,
					SupportedTransferSyntaxes:    []string{transfer.ExplicitVRLittleEndian.UID},
					AsynchronousOperationsWindow: window,
				})
				if err != nil {
					return
				}
				defer assoc.Close()
				peer.mu.Lock()
				peer.associations++
				peer.mu.Unlock()
				_ = ServeAssociation(ctx, assoc, AssociationSCPOptions{StorageSCPOptions: StorageSCPOptions{StoreHandler: CStoreHandlerFunc(func(_ context.Context, req CStoreRequestContext) (uint16, error) {
					peer.mu.Lock()
					peer.stored[req.Request.AffectedSOPInstanceUID] = true
					peer.mu.Unlock()
					return StatusSuccess, nil
				})}})
			}(conn)
		}
	}()
	return peer
}

// releaseSplitBatch has one more distinct SOP Class than one association can
// propose, so the plan needs a second association.
func releaseSplitBatch(t *testing.T) ([]StoreSource, []string) {
	t.Helper()
	var sources []StoreSource
	var uids []string
	for class := 1; class <= ul.MaxPresentationContexts+1; class++ {
		uid := fmt.Sprintf("%s.2000.%d", batchUIDRoot, class)
		dataset := object.FromElements([]core.Element{
			batchText(core.NewTag(0x0008, 0x0016), core.VRUI, fmt.Sprintf("%s.9.%d", batchUIDRoot, class)),
			batchText(core.NewTag(0x0008, 0x0018), core.VRUI, uid),
			batchText(core.NewTag(0x0010, 0x0010), core.VRPN, "SYNTHETIC^RELEASE"),
		}, std.Dictionary)
		sources = append(sources, NewDataSetStoreSource(dataset, transfer.ExplicitVRLittleEndian, 256))
		uids = append(uids, uid)
	}
	return sources, uids
}

// TestStoreSessionContinuesPastALostRelease covers #2000: a batch of more
// than 128 SOP Class and transfer syntax pairs spans two associations, and
// the peer never answers the release of the first. With ContinueOnError the
// lost release is recorded and every instance of the second association is
// still sent; without it the batch stops there as before.
func TestStoreSessionContinuesPastALostRelease(t *testing.T) {
	for _, tc := range []struct {
		name    string
		invoked int
		window  *ul.AsynchronousOperationsWindow
	}{
		{"serial", 0, nil},
		{"pipelined", 4, &ul.AsynchronousOperationsWindow{MaximumInvoked: 4, MaximumPerformed: 4}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			for _, continueOnError := range []bool{true, false} {
				t.Run(fmt.Sprintf("ContinueOnError=%t", continueOnError), func(t *testing.T) {
					ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
					defer cancel()
					peer := startReleaseIgnoringPeer(t, ctx, tc.window)
					sources, uids := releaseSplitBatch(t)
					session, err := NewStoreSession(peer.address, StoreSessionOptions{
						DialOptions:          ul.DialOptions{CallingAETitle: "STORESCU", CalledAETitle: "RELEASESCP"},
						ContinueOnError:      continueOnError,
						MaxInvokedOperations: tc.invoked,
						ReleaseTimeout:       200 * time.Millisecond,
					})
					if err != nil {
						t.Fatal(err)
					}
					defer func() { _ = session.Abort(context.Background()) }()
					result, err := session.StoreBatch(ctx, sources)
					var storeErr *StoreError
					if !errors.As(err, &storeErr) || storeErr.Stage != "release" || !errors.Is(err, ErrStoreAssociation) {
						t.Fatalf("StoreBatch error = %v, want the release StoreError", err)
					}
					if result.ReleaseFailures != 1 || peer.ignored.Load() != 1 {
						t.Fatalf("release failures = %d, peer ignored %d releases; want 1 and 1", result.ReleaseFailures, peer.ignored.Load())
					}
					if !continueOnError {
						if result.Associations != 1 || result.Succeeded != ul.MaxPresentationContexts {
							t.Fatalf("stored %d over %d associations, want the batch to stop after the first", result.Succeeded, result.Associations)
						}
						return
					}
					if result.Associations != 2 || result.Succeeded != len(sources) || result.Failed != 0 || result.Unknown != 0 || !result.Complete {
						t.Fatalf("stored %d/%d over %d associations (failed %d, unknown %d, complete %t)", result.Succeeded, len(sources), result.Associations, result.Failed, result.Unknown, result.Complete)
					}
					for _, item := range result.Items {
						if item.Outcome != StoreOutcomeSuccess || item.Err != nil {
							t.Fatalf("source %d: outcome %s, err %v", item.SourceIndex, item.Outcome, item.Err)
						}
					}
					peer.mu.Lock()
					defer peer.mu.Unlock()
					if peer.associations != 2 {
						t.Fatalf("peer saw %d associations, want 2", peer.associations)
					}
					for _, uid := range uids {
						if !peer.stored[uid] {
							t.Fatalf("%s missing at peer", uid)
						}
					}
				})
			}
		})
	}
}
