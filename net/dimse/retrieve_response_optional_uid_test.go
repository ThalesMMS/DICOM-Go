package dimse

import (
	"testing"

	"github.com/ThalesMMS/dicom-go/core"
	"github.com/ThalesMMS/dicom-go/dictionary/std"
	"github.com/ThalesMMS/dicom-go/object"
)

// Affected SOP Class UID is optional in C-MOVE-RSP and C-GET-RSP. dcm4che
// omits it in a failed retrieve, and the status must still reach the caller.
func TestRetrieveResponsesAcceptMissingAffectedSOPClassUID(t *testing.T) {
	elements := func(field, status uint16) *object.Object {
		return object.FromElements([]core.Element{
			newUSCommandElement(CommandField, field),
			newUSCommandElement(MessageIDBeingRespondedTo, 1),
			newUSCommandElement(CommandDataSetType, NoDataSet),
			newUSCommandElement(Status, status),
		}, std.Dictionary)
	}
	move, err := ParseCMoveResponse(elements(CMoveRSP, StatusCMoveMoveDestinationUnknown))
	if err != nil || move.Status != StatusCMoveMoveDestinationUnknown || move.AffectedSOPClassUID != "" {
		t.Fatalf("C-MOVE-RSP = %+v, %v", move, err)
	}
	get, err := ParseCGetResponse(elements(CGetRSP, StatusCMoveUnableToProcess))
	if err != nil || get.Status != StatusCMoveUnableToProcess || get.AffectedSOPClassUID != "" {
		t.Fatalf("C-GET-RSP = %+v, %v", get, err)
	}
	// A present but empty UID is still malformed.
	malformed := elements(CMoveRSP, StatusSuccess)
	malformed.Put(core.Element{Header: core.ElementHeader{Tag: AffectedSOPClassUID, VR: core.VRUI}, Value: core.StringValue{""}})
	if _, err := ParseCMoveResponse(malformed); err == nil {
		t.Fatal("an empty Affected SOP Class UID was accepted")
	}
}
