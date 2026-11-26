package dimse

import (
	"context"
	"testing"
	"time"

	"github.com/ThalesMMS/dicom-go/core"
	"github.com/ThalesMMS/dicom-go/dictionary/std"
	"github.com/ThalesMMS/dicom-go/dictionary/tags"
	"github.com/ThalesMMS/dicom-go/net/ul"
	"github.com/ThalesMMS/dicom-go/object"
	"github.com/ThalesMMS/dicom-go/transfer"
)

// A C-MOVE whose Identifier has no Query/Retrieve Level used to end the
// whole association after its failure response: the requestor's next
// request, or its A-RELEASE-RQ, met a closed connection.
//
// Such an Identifier does not fit the Information Model. PS3.4 C.4.2.1.4
// reports that as A900 (Identifier does not match SOP Class) with an Error
// Comment naming the key; C000 (Unable to process) is kept for failures of
// the SCP itself, which a requestor may retry.
func TestServeAssociationKeepsOpenAfterIdentifierWithoutLevel(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()

	listener, err := ul.Listen(ul.ListenOptions{Address: "127.0.0.1:0", Context: ctx})
	if err != nil {
		t.Fatalf("Listen() error = %v", err)
	}
	defer closeOrFail(t, "listener", listener)

	serverDone := make(chan error, 1)
	handled := 0
	go func() {
		assoc, err := listener.AcceptAssociation(ul.AcceptOptions{
			AETitle:                   "MOVESCP",
			Context:                   ctx,
			SupportedAbstractSyntaxes: []string{StudyRootMoveSOPClassUID},
			SupportedTransferSyntaxes: []string{ul.ImplicitVRLittleEndian},
		})
		if err != nil {
			serverDone <- err
			return
		}
		defer closeOrFail(t, "server association", assoc)
		serverDone <- ServeAssociation(ctx, assoc, AssociationSCPOptions{
			CMoveHandler: CMoveHandlerFunc(func(context.Context, CMoveRequestContext) ([]CMoveSubOperation, error) {
				handled++
				return nil, NewCMoveSCPError(StatusCMoveMoveDestinationUnknown, "move destination unknown", nil)
			}),
		})
	}()

	assoc, err := ul.DialContext(ctx, listener.Addr().String(), ul.DialOptions{
		CalledAETitle:  "MOVESCP",
		CallingAETitle: "MOVESCU",
		Contexts:       []ul.PresentationContext{StudyRootMovePresentationContext()},
	})
	if err != nil {
		t.Fatalf("DialContext() error = %v", err)
	}
	defer closeOrFail(t, "client association", assoc)
	pc, err := AcceptedContextForSOPClass(assoc, StudyRootMoveSOPClassUID)
	if err != nil {
		t.Fatalf("AcceptedContextForSOPClass() error = %v", err)
	}
	withoutLevel := object.FromElements([]core.Element{
		{Header: core.ElementHeader{Tag: tags.StudyInstanceUID, VR: core.VRUI}, Value: core.StringValue{"1.2.3"}},
	}, std.Dictionary)
	rsp, err := SendCMove(ctx, assoc, pc.ID, CMoveRequest{
		AffectedSOPClassUID: StudyRootMoveSOPClassUID, MessageID: 1, Priority: PriorityMedium, MoveDestination: "ANYWHERE",
	}, withoutLevel, transfer.ImplicitVRLittleEndian)
	if err != nil {
		t.Fatalf("SendCMove() without a level: error = %v", err)
	}
	if rsp.Status != StatusCMoveIdentifierDoesNotMatchSOPClass {
		t.Fatalf("C-MOVE without a level: status = 0x%04X, want 0x%04X", rsp.Status, StatusCMoveIdentifierDoesNotMatchSOPClass)
	}
	if rsp.ErrorComment != "Missing Query/Retrieve Level (0008,0052)" {
		t.Fatalf("C-MOVE without a level: Error Comment = %q, want the Query/Retrieve Level key", rsp.ErrorComment)
	}
	// The same association serves a well-formed request and a release.
	identifierElems, err := BuildStudyRootStudyFindKeys(nil)
	if err != nil {
		t.Fatalf("BuildStudyRootStudyFindKeys() error = %v", err)
	}
	rsp, err = SendCMove(ctx, assoc, pc.ID, CMoveRequest{
		AffectedSOPClassUID: StudyRootMoveSOPClassUID, MessageID: 2, Priority: PriorityMedium, MoveDestination: "MISSING",
	}, object.FromElements(identifierElems, std.Dictionary), transfer.ImplicitVRLittleEndian)
	if err != nil {
		t.Fatalf("SendCMove() after the refused request: error = %v", err)
	}
	if rsp.Status != StatusCMoveMoveDestinationUnknown || handled != 1 {
		t.Fatalf("second C-MOVE: status = 0x%04X, handler calls = %d", rsp.Status, handled)
	}
	if err := assoc.Release(ctx); err != nil {
		t.Fatalf("Release() error = %v", err)
	}
	if err := <-serverDone; err != nil {
		t.Fatalf("ServeAssociation() error = %v", err)
	}
}
