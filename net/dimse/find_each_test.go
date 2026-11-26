package dimse

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/ThalesMMS/dicom-go/core"
	"github.com/ThalesMMS/dicom-go/dictionary/std"
	"github.com/ThalesMMS/dicom-go/net/ul"
	"github.com/ThalesMMS/dicom-go/object"
	"github.com/ThalesMMS/dicom-go/transfer"
)

// findEachPeer is a scripted C-FIND SCP: it sends sendBeforeCancel pending
// responses, then, when cancelExpected, waits for the C-CANCEL-RQ, sends
// inFlight more pending responses and the final status. Without a cancel it
// sends the final status right after the pending responses.
type findEachPeer struct {
	sendBeforeCancel int
	cancelExpected   bool
	inFlight         int
	final            uint16
	silentAfter      bool
	// noIdentifier sends the pending responses without an identifier, and
	// messageID, when set, answers another message.
	noIdentifier bool
	messageID    uint16
}

type findEachPeerResult struct {
	canceledMessageID uint16
	err               error
}

func runFindEach(t *testing.T, peer findEachPeer, options FindEachOptions, each func(FindResult) bool) (FindEachOutcome, error, findEachPeerResult) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	listener, err := ul.Listen(ul.ListenOptions{Address: "127.0.0.1:0", Context: ctx})
	if err != nil {
		t.Fatalf("Listen() error = %v", err)
	}
	defer closeOrFail(t, "listener", listener)
	done := make(chan findEachPeerResult, 1)
	go func() {
		var result findEachPeerResult
		defer func() { done <- result }()
		assoc, err := listener.AcceptAssociation(ul.AcceptOptions{
			AETitle: "FINDSCP", Context: ctx,
			SupportedAbstractSyntaxes: []string{StudyRootFindSOPClassUID},
			SupportedTransferSyntaxes: []string{ul.ImplicitVRLittleEndian},
		})
		if err != nil {
			result.err = err
			return
		}
		defer assoc.Close()
		pc, err := AcceptedContextForSOPClass(assoc, StudyRootFindSOPClassUID)
		if err != nil {
			result.err = err
			return
		}
		command, err := ReceiveCommandSet(assoc, pc.ID)
		if err != nil {
			result.err = err
			return
		}
		request, err := ParseCFindRequest(command)
		if err != nil {
			result.err = err
			return
		}
		if _, err := receiveIdentifierObject(assoc, pc.ID, pc.TransferSyntaxUID); err != nil {
			result.err = err
			return
		}
		send := func(status uint16, index int) error {
			response := CFindResponse{AffectedSOPClassUID: StudyRootFindSOPClassUID, MessageIDBeingRespondedTo: request.MessageID, Status: status}
			if peer.messageID != 0 {
				response.MessageIDBeingRespondedTo = peer.messageID
			}
			var identifier *object.Object
			if status == StatusPending && !peer.noIdentifier {
				identifier = object.FromElements([]core.Element{{
					Header: core.ElementHeader{Tag: core.NewTag(0x0010, 0x0020), VR: core.VRLO},
					Value:  core.StringValue{fmt.Sprintf("PID-%d", index)},
				}}, std.Dictionary)
			}
			return SendCFindResponseWithContext(ctx, assoc, pc.ID, response, identifier, transfer.ImplicitVRLittleEndian)
		}
		for i := 0; i < peer.sendBeforeCancel; i++ {
			if result.err = send(StatusPending, i); result.err != nil {
				return
			}
		}
		if peer.cancelExpected {
			cancelCommand, err := ReceiveCommandSet(assoc, pc.ID)
			if err != nil {
				result.err = err
				return
			}
			canceled, err := ParseCCancelRequest(cancelCommand)
			if err != nil {
				result.err = err
				return
			}
			result.canceledMessageID = canceled.MessageIDBeingRespondedTo
			for i := 0; i < peer.inFlight; i++ {
				if result.err = send(StatusPending, peer.sendBeforeCancel+i); result.err != nil {
					return
				}
			}
		}
		if peer.silentAfter {
			<-ctx.Done()
			return
		}
		result.err = send(peer.final, 0)
	}()

	assoc, err := ul.DialContext(ctx, listener.Addr().String(), ul.DialOptions{
		CallingAETitle: "FINDSCU", CalledAETitle: "FINDSCP",
		Contexts: []ul.PresentationContext{StudyRootFindPresentationContext()},
	})
	if err != nil {
		t.Fatalf("DialContext() error = %v", err)
	}
	client, err := NewFindClientForSOPClass(assoc, StudyRootFindSOPClassUID)
	if err != nil {
		t.Fatalf("NewFindClientForSOPClass() error = %v", err)
	}
	keys, err := BuildStudyRootStudyFindKeys(nil)
	if err != nil {
		t.Fatal(err)
	}
	if options.Context == nil {
		options.Context = ctx
	}
	outcome, findErr := client.FindEach(options, CFindRequest{MessageID: 7}, object.FromElements(keys, std.Dictionary), each)
	_ = assoc.Close()
	cancel()
	return outcome, findErr, <-done
}

func TestFindEachCancelsWhenTheCallbackStops(t *testing.T) {
	var seen []string
	outcome, err, peer := runFindEach(t, findEachPeer{sendBeforeCancel: 3, cancelExpected: true, inFlight: 2, final: CFindStatusCancel}, FindEachOptions{}, func(result FindResult) bool {
		id, _ := result.Identifier.GetString(core.NewTag(0x0010, 0x0020))
		seen = append(seen, id)
		return len(seen) < 3
	})
	if err != nil {
		t.Fatalf("FindEach() error = %v", err)
	}
	if peer.err != nil {
		t.Fatalf("peer error = %v", peer.err)
	}
	if peer.canceledMessageID != 7 {
		t.Fatalf("C-CANCEL-RQ Message ID Being Responded To = %d, want 7", peer.canceledMessageID)
	}
	if len(seen) != 3 || seen[2] != "PID-2" {
		t.Fatalf("callback saw %q, want the first three matches", seen)
	}
	if !outcome.CancelSent || outcome.Pending != 3 || outcome.Discarded != 2 || outcome.Final == nil || outcome.Final.Status != CFindStatusCancel {
		t.Fatalf("outcome = %+v, want cancel sent, 3 pending, 2 discarded and a Cancel final", outcome)
	}
}

func TestFindEachReadsToTheFinalResponseWithoutCancel(t *testing.T) {
	count := 0
	outcome, err, peer := runFindEach(t, findEachPeer{sendBeforeCancel: 4, final: StatusSuccess}, FindEachOptions{}, func(FindResult) bool {
		count++
		return true
	})
	if err != nil || peer.err != nil {
		t.Fatalf("FindEach() error = %v, peer error = %v", err, peer.err)
	}
	if count != 4 || outcome.CancelSent || outcome.Pending != 4 || outcome.Final == nil || outcome.Final.Status != StatusSuccess {
		t.Fatalf("outcome = %+v after %d callbacks, want 4 matches and Success", outcome, count)
	}
}

func TestFindEachReportsAFinalFailure(t *testing.T) {
	outcome, err, peer := runFindEach(t, findEachPeer{sendBeforeCancel: 1, final: CFindStatusUnableToProcess}, FindEachOptions{}, func(FindResult) bool { return true })
	if peer.err != nil {
		t.Fatalf("peer error = %v", peer.err)
	}
	if err == nil || !strings.Contains(err.Error(), "0xC000") || outcome.Final == nil || outcome.Final.Status != CFindStatusUnableToProcess {
		t.Fatalf("FindEach() = %+v, %v; want the C000 failure", outcome, err)
	}
}

func TestFindEachBoundsTheDrainOfAPeerThatIgnoresTheCancel(t *testing.T) {
	started := time.Now()
	outcome, err, _ := runFindEach(t, findEachPeer{sendBeforeCancel: 1, cancelExpected: true, silentAfter: true}, FindEachOptions{CancelDrainTimeout: 300 * time.Millisecond}, func(FindResult) bool { return false })
	if !errors.Is(err, ErrFindCancelNotAnswered) || !outcome.CancelSent {
		t.Fatalf("FindEach() = %+v, %v; want ErrFindCancelNotAnswered after the C-CANCEL-RQ", outcome, err)
	}
	if elapsed := time.Since(started); elapsed > 5*time.Second {
		t.Fatalf("the drain took %s, want about the 300 ms drain timeout", elapsed)
	}
}

// A pending response carries one match (PS3.4 C.4.1.3), and a response
// answers the request's message: a peer that breaks either rule ends the
// operation with a protocol violation instead of an empty match.
func TestFindEachRefusesResponsesThatBreakTheProtocol(t *testing.T) {
	for _, tc := range []struct {
		name string
		peer findEachPeer
	}{
		{"pending without an identifier", findEachPeer{sendBeforeCancel: 1, final: StatusSuccess, noIdentifier: true}},
		{"response to another message", findEachPeer{sendBeforeCancel: 1, final: StatusSuccess, messageID: 8}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			calls := 0
			_, err, _ := runFindEach(t, tc.peer, FindEachOptions{}, func(FindResult) bool {
				calls++
				return true
			})
			if !errors.Is(err, ErrProtocolViolation) {
				t.Fatalf("FindEach() error = %v, want a protocol violation", err)
			}
			if calls != 0 {
				t.Fatalf("the callback saw %d responses, want none", calls)
			}
		})
	}
}
