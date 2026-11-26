package dimse

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/ThalesMMS/dicom-go/dictionary/std"
	"github.com/ThalesMMS/dicom-go/net/ul"
	"github.com/ThalesMMS/dicom-go/object"
)

func TestParseCommandsNormalizeNULPaddedAETitles(t *testing.T) {
	originatorID := uint16(7)
	move := CMoveRequest{
		AffectedSOPClassUID:          StudyRootMoveSOPClassUID,
		MessageID:                    1,
		MoveDestination:              "STOREAE\x00\x00\x00",
		MoveOriginatorAETitle:        " ORIGIN\x00",
		MoveOriginatorMessageIDOrNil: &originatorID,
	}
	parsedMove, err := ParseCMoveRequest(object.FromElements(move.CommandSet(), std.Dictionary))
	if err != nil {
		t.Fatalf("ParseCMoveRequest() error = %v", err)
	}
	if parsedMove.MoveDestination != "STOREAE" || parsedMove.MoveOriginatorAETitle != "ORIGIN" {
		t.Fatalf("C-MOVE titles = %q/%q, want STOREAE/ORIGIN", parsedMove.MoveDestination, parsedMove.MoveOriginatorAETitle)
	}

	store := CStoreRequest{
		AffectedSOPClassUID:          "1.2.840.10008.5.1.4.1.1.7",
		MessageID:                    2,
		AffectedSOPInstanceUID:       "1.2.3.1026",
		MoveOriginatorAETitle:        "ORIGIN\x00\x00",
		MoveOriginatorMessageIDOrNil: &originatorID,
	}
	parsedStore, err := ParseCStoreRequest(object.FromElements(store.CommandSet(), std.Dictionary))
	if err != nil {
		t.Fatalf("ParseCStoreRequest() error = %v", err)
	}
	if parsedStore.MoveOriginatorAETitle != "ORIGIN" {
		t.Fatalf("C-STORE originator = %q, want ORIGIN", parsedStore.MoveOriginatorAETitle)
	}
}

func TestServeStudyRootCMoveRejectsInvalidMoveDestination(t *testing.T) {
	for _, destination := range []string{"STORE\nFORGED", "STORE\x00EVIL", "\x1b[2JSTORE"} {
		t.Run(destination, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
			defer cancel()
			called := false
			status, serverErr := runStudyRootCMoveStatusTestTo(t, ctx, CMoveHandlerFunc(func(context.Context, CMoveRequestContext) ([]CMoveSubOperation, error) {
				called = true
				return nil, nil
			}), destination)
			if status != StatusCMoveMoveDestinationUnknown {
				t.Fatalf("final status = 0x%04X, want destination unknown 0x%04X", status, StatusCMoveMoveDestinationUnknown)
			}
			if !errors.Is(serverErr, ul.ErrInvalidAEtitle) {
				t.Fatalf("server error = %v, want ErrInvalidAEtitle", serverErr)
			}
			if called {
				t.Fatal("handler received an invalid Move Destination")
			}
		})
	}
}

func TestServeStudyRootCMoveAcceptsNULPaddedMoveDestination(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	var seen string
	status, _ := runStudyRootCMoveStatusTestTo(t, ctx, CMoveHandlerFunc(func(_ context.Context, req CMoveRequestContext) ([]CMoveSubOperation, error) {
		seen = req.Request.MoveDestination
		return nil, NewCMoveSCPError(StatusCMoveMoveDestinationUnknown, "move destination unknown", nil)
	}), "STOREAE\x00\x00")
	if status != StatusCMoveMoveDestinationUnknown {
		t.Fatalf("final status = 0x%04X", status)
	}
	if seen != "STOREAE" {
		t.Fatalf("handler Move Destination = %q, want STOREAE", seen)
	}
}
