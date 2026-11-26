package dimse

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/ThalesMMS/dicom-go/core"
	"github.com/ThalesMMS/dicom-go/internal/dicomtest"
	"github.com/ThalesMMS/dicom-go/net/ul"
	"github.com/ThalesMMS/dicom-go/object"
	"github.com/ThalesMMS/dicom-go/transfer"
)

func TestAsyncCFindOccupancyRefusesStoreAndMoveWithoutLosingAssociation(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	syntax := transfer.ExplicitVRLittleEndian
	client, server := newAsyncSessionPairOptions(t, 2, 0, []ul.AcceptedContext{
		{ID: 1, AbstractSyntaxUID: StudyRootFindSOPClassUID, TransferSyntaxUID: syntax.UID},
		{ID: 3, AbstractSyntaxUID: dicomtest.TestSOPClassUID, TransferSyntaxUID: syntax.UID},
		{ID: 5, AbstractSyntaxUID: StudyRootMoveSOPClassUID, TransferSyntaxUID: syntax.UID},
	}, AsyncSessionOptions{}, AsyncSessionOptions{MaxPerformedOperations: 1, MaxPendingRequests: 1})
	findDone := make(chan error, 1)
	server.Handle(CFindRQ, func(handlerCtx context.Context, session *AsyncSession, request AsyncMessage) error {
		err := session.Respond(handlerCtx, request, CFindResponse{
			AffectedSOPClassUID: StudyRootFindSOPClassUID, CommandDataSetType: DataSetPresent, Status: StatusPending,
		}.CommandSet(), object.New(nil))
		if err != nil {
			return err
		}
		<-handlerCtx.Done()
		err = session.Respond(ctx, request, CFindResponse{
			AffectedSOPClassUID: StudyRootFindSOPClassUID, CommandDataSetType: NoDataSet, Status: 0xFE00,
		}.CommandSet(), nil)
		findDone <- err
		return err
	})
	textTag := core.NewTag(0x0040, 0xa160)
	const allowedText = "allowed after C-FIND cancellation"
	stored := make(chan uint16, 2)
	server.Handle(CStoreRQ, func(handlerCtx context.Context, session *AsyncSession, message AsyncMessage) error {
		if value, ok := message.DataSet.GetString(textTag); !ok || value != allowedText {
			return fmt.Errorf("unexpected text value in C-STORE handler (length %d)", len(value))
		}
		request, err := ParseCStoreRequest(message.Command)
		if err != nil {
			return err
		}
		stored <- request.MessageID
		return session.Respond(handlerCtx, message, CStoreResponse{
			AffectedSOPClassUID: request.AffectedSOPClassUID, AffectedSOPInstanceUID: request.AffectedSOPInstanceUID,
			Status: StatusSuccess,
		}.CommandSet(), nil)
	})
	server.Handle(CMoveRQ, func(context.Context, *AsyncSession, AsyncMessage) error {
		t.Error("resource-limited request entered the C-MOVE handler")
		return fmt.Errorf("unexpected C-MOVE")
	})
	identifier := object.New(nil)
	if err := identifier.SetStrings(core.NewTag(0x0008, 0x0052), core.VRCS, "STUDY"); err != nil {
		t.Fatal(err)
	}
	find, err := client.StartCFind(ctx, 1, CFindRequest{AffectedSOPClassUID: StudyRootFindSOPClassUID}, identifier)
	if err != nil {
		t.Fatal(err)
	}
	pending, err := find.Next(ctx)
	if err != nil {
		t.Fatal(err)
	}
	assertAsyncCapacityResponse(t, pending, find.MessageID(), 1, CFindRSP, StatusPending)

	storeRequest := CStoreRequest{AffectedSOPClassUID: dicomtest.TestSOPClassUID, AffectedSOPInstanceUID: dicomtest.TestSOPInstanceUID}
	startStore := func(t *testing.T, value string) *AsyncOperation {
		t.Helper()
		dataset := object.New(nil)
		for tag, uid := range map[core.Tag]string{
			core.NewTag(0x0008, 0x0016): storeRequest.AffectedSOPClassUID,
			core.NewTag(0x0008, 0x0018): storeRequest.AffectedSOPInstanceUID,
		} {
			if err := dataset.SetStrings(tag, core.VRUI, uid); err != nil {
				t.Fatal(err)
			}
		}
		if err := dataset.SetStrings(textTag, core.VRUT, value); err != nil {
			t.Fatal(err)
		}
		operation, err := client.StartCStore(ctx, 3, storeRequest, dataset)
		if err != nil {
			t.Fatal(err)
		}
		return operation
	}
	t.Run("store-refuses-and-drains-fragmented-dataset", func(t *testing.T) {
		refused := startStore(t, strings.Repeat("STORAGE", 4000))
		response, err := refused.Wait(ctx)
		if err != nil {
			t.Fatal(err)
		}
		assertAsyncCapacityResponse(t, response, refused.MessageID(), 3, CStoreRSP, 0xA700)
		command, err := ParseCStoreResponse(response.Command)
		if err != nil || command.AffectedSOPInstanceUID != storeRequest.AffectedSOPInstanceUID || response.DataSet != nil {
			t.Fatalf("refused C-STORE: command = %+v, dataset present = %t, error = %v", command, response.DataSet != nil, err)
		}
	})
	t.Run("move-uses-query-retrieve-resource-status", func(t *testing.T) {
		move, err := client.StartCMove(ctx, 5, CMoveRequest{AffectedSOPClassUID: StudyRootMoveSOPClassUID, MoveDestination: "DEST"}, identifier)
		if err != nil {
			t.Fatal(err)
		}
		response, err := move.Wait(ctx)
		if err != nil {
			t.Fatal(err)
		}
		assertAsyncCapacityResponse(t, response, move.MessageID(), 5, CMoveRSP, 0xA701)
		if response.DataSet != nil {
			t.Fatal("refused C-MOVE returned an Identifier")
		}
	})
	t.Run("cancel-reclaims-capacity-for-next-store", func(t *testing.T) {
		if err := find.Cancel(ctx); err != nil {
			t.Fatal(err)
		}
		response, err := find.Wait(ctx)
		if err != nil {
			t.Fatal(err)
		}
		assertAsyncCapacityResponse(t, response, find.MessageID(), 1, CFindRSP, 0xFE00)
		select {
		case err := <-findDone:
			if err != nil {
				t.Fatal(err)
			}
		case <-ctx.Done():
			t.Fatal(ctx.Err())
		}
		accepted := startStore(t, allowedText)
		response, err = accepted.Wait(ctx)
		if err != nil {
			t.Fatal(err)
		}
		assertAsyncCapacityResponse(t, response, accepted.MessageID(), 3, CStoreRSP, StatusSuccess)
		select {
		case id := <-stored:
			if id != accepted.MessageID() {
				t.Fatalf("C-STORE handler received message %d, want %d", id, accepted.MessageID())
			}
		default:
			t.Fatal("accepted C-STORE did not enter the handler")
		}
	})
	if err := client.Release(ctx, AsyncReleaseWait); err != nil {
		t.Fatal(err)
	}
	metrics := server.Snapshot()
	if metrics.WindowViolations != 0 || metrics.PeakPerformed != 1 || metrics.ActivePerformed != 0 || metrics.QueuedMessageBytes != 0 {
		t.Fatalf("server resource metrics: %+v", metrics)
	}
}

func assertAsyncCapacityResponse(t *testing.T, response AsyncMessage, id uint16, pcID byte, field, wantStatus uint16) {
	t.Helper()
	status, err := CommandUint16(response.Command, Status)
	if err != nil || status != wantStatus || response.CommandField != field || response.MessageID != id || response.PresentationContextID != pcID {
		t.Fatalf("received status 0x%04X, command 0x%04X, message %d, context %d (error %v), want 0x%04X/0x%04X/%d/%d",
			status, response.CommandField, response.MessageID, response.PresentationContextID, err, wantStatus, field, id, pcID)
	}
}
