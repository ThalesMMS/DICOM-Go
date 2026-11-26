package printmgmt_test

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/ThalesMMS/dicom-go/core"
	"github.com/ThalesMMS/dicom-go/net/dimse"
	"github.com/ThalesMMS/dicom-go/net/ul"
	"github.com/ThalesMMS/dicom-go/object"
	"github.com/ThalesMMS/dicom-go/printmgmt"
	"github.com/ThalesMMS/dicom-go/transfer"
)

func TestPrintFilmAcrossScriptedPeerResponses(t *testing.T) {
	const sessionUID = "2.25.78101"
	const filmUID = "2.25.78102"
	const peerComment = "SYNTHETIC PRIVATE COMMENT"
	const invalidBoxUID = "SYNTHETIC-PRIVATE-UID"
	boxUIDs := []string{"2.25.78131", "2.25.78112", "2.25.78123"}
	film := printmgmt.Film{Columns: 3, Rows: 1, Images: []*printmgmt.Image{nil, gray8(2, 2, 3), gray8(2, 2, 4)}}
	createSession := printExchange{field: dimse.NCreateRQ, class: printmgmt.FilmSessionSOPClassUID, assigned: sessionUID}
	createFilm := printExchange{field: dimse.NCreateRQ, class: printmgmt.FilmBoxSOPClassUID, assigned: filmUID, response: printImageBoxList(boxUIDs)}
	setSecond := printExchange{field: dimse.NSetRQ, class: printmgmt.GrayscaleImageBoxUID, uid: boxUIDs[1], position: 2}
	setThird := printExchange{field: dimse.NSetRQ, class: printmgmt.GrayscaleImageBoxUID, uid: boxUIDs[2], position: 3}
	action := printExchange{field: dimse.NActionRQ, class: printmgmt.FilmBoxSOPClassUID, uid: filmUID}
	cleanup := printExchange{field: dimse.NDeleteRQ, class: printmgmt.FilmSessionSOPClassUID, uid: sessionUID}

	shortFilm := createFilm
	shortFilm.response = printImageBoxList(boxUIDs[:2])
	longFilm := createFilm
	longFilm.response = printImageBoxList(append(slices.Clone(boxUIDs), "2.25.78144"))
	missingImageBoxes := createFilm
	missingImageBoxes.response = nil
	noFilmUID := createFilm
	noFilmUID.assigned = ""
	changeReference := func(index int, tag core.Tag, vr core.VR, value core.Value) printExchange {
		create := createFilm
		create.response = printImageBoxList(boxUIDs)
		items, _ := create.response.GetSequence(printmgmt.TagReferencedImageBoxSequence)
		items[index].Put(core.Element{Header: core.ElementHeader{Tag: tag, VR: vr}, Value: value})
		create.response.SetSequence(printmgmt.TagReferencedImageBoxSequence, items...)
		return create
	}
	wrongClass := changeReference(2, printmgmt.TagReferencedSOPClassUID, core.VRUI, core.StringValue{printmgmt.ColorImageBoxUID})
	wrongClassInBlankSlot := changeReference(0, printmgmt.TagReferencedSOPClassUID, core.VRUI, core.StringValue{printmgmt.ColorImageBoxUID})
	wrongClassWarning := wrongClass
	wrongClassWarning.status = 0xB605
	badInstanceUID := changeReference(2, printmgmt.TagReferencedSOPInstanceUID, core.VRUI, core.RawValue(invalidBoxUID))
	duplicateUID := changeReference(2, printmgmt.TagReferencedSOPInstanceUID, core.VRUI, core.StringValue{boxUIDs[0]})
	parentUID := changeReference(2, printmgmt.TagReferencedSOPInstanceUID, core.VRUI, core.StringValue{filmUID})
	multipleUIDs := changeReference(2, printmgmt.TagReferencedSOPInstanceUID, core.VRUI, core.StringValue{boxUIDs[2], "2.25.78144"})
	wrongVR := changeReference(2, printmgmt.TagReferencedSOPInstanceUID, core.VRLO, core.StringValue{boxUIDs[2]})
	warningSession := createSession
	warningSession.status = 0xB600
	warningFilm := createFilm
	warningFilm.status = 0xB605
	warningSet := setSecond
	warningSet.status = 0xB604
	warningAction := action
	warningAction.status = 0xB60A
	failingCleanup := cleanup
	failingCleanup.status = 0x0112
	failingCleanup.fields = &dimse.NormalizedStatusFields{ErrorComment: peerComment}
	lostAction := action
	lostAction.closeWithoutResponse = true
	refusedAction := action
	refusedAction.status = 0xC602

	for _, tc := range []struct {
		name           string
		exchanges      []printExchange
		wantPrinted    bool
		wantStep       string
		wantStatus     uint16
		wantUnknown    bool
		wantAbort      bool
		wantBadRefs    bool
		wantCleanupErr bool
		wantWarnings   []printmgmt.StepStatus
	}{
		{name: "sparse-positions-use-peer-uids", exchanges: []printExchange{createSession, createFilm, setSecond, setThird, action, cleanup}, wantPrinted: true},
		{name: "insufficient-image-boxes", exchanges: []printExchange{createSession, shortFilm, cleanup}, wantStep: printmgmt.StepCreateFilmBox},
		{name: "unexpected-extra-image-boxes", exchanges: []printExchange{createSession, longFilm, cleanup}, wantStep: printmgmt.StepCreateFilmBox},
		{name: "missing-image-boxes", exchanges: []printExchange{createSession, missingImageBoxes, cleanup}, wantStep: printmgmt.StepCreateFilmBox},
		{name: "missing-film-box-uid", exchanges: []printExchange{createSession, noFilmUID}, wantStep: printmgmt.StepCreateFilmBox, wantAbort: true},
		{name: "wrong-image-box-class", exchanges: []printExchange{createSession, wrongClass, cleanup}, wantStep: printmgmt.StepCreateFilmBox, wantBadRefs: true},
		{name: "wrong-class-in-blank-slot", exchanges: []printExchange{createSession, wrongClassInBlankSlot, cleanup}, wantStep: printmgmt.StepCreateFilmBox, wantBadRefs: true},
		{name: "wrong-class-warning-with-cleanup-refusal", exchanges: []printExchange{createSession, wrongClassWarning, failingCleanup}, wantStep: printmgmt.StepCreateFilmBox, wantStatus: 0xB605, wantBadRefs: true},
		{name: "malformed-instance-uid", exchanges: []printExchange{createSession, badInstanceUID, cleanup}, wantStep: printmgmt.StepCreateFilmBox, wantBadRefs: true},
		{name: "duplicate-instance-uid", exchanges: []printExchange{createSession, duplicateUID, cleanup}, wantStep: printmgmt.StepCreateFilmBox, wantBadRefs: true},
		{name: "instance-uid-reuses-film-box", exchanges: []printExchange{createSession, parentUID, cleanup}, wantStep: printmgmt.StepCreateFilmBox, wantBadRefs: true},
		{name: "multiple-instance-uids", exchanges: []printExchange{createSession, multipleUIDs, cleanup}, wantStep: printmgmt.StepCreateFilmBox, wantBadRefs: true},
		{name: "wrong-vr-for-instance-uid", exchanges: []printExchange{createSession, wrongVR, cleanup}, wantStep: printmgmt.StepCreateFilmBox, wantBadRefs: true},
		{name: "lost-action-answer", exchanges: []printExchange{createSession, createFilm, setSecond, setThird, lostAction}, wantStep: printmgmt.StepPrintFilmBox, wantUnknown: true},
		{name: "definite-action-refusal", exchanges: []printExchange{createSession, createFilm, setSecond, setThird, refusedAction, cleanup}, wantStep: printmgmt.StepPrintFilmBox, wantStatus: 0xC602},
		{name: "warnings-and-cleanup-refusal", exchanges: []printExchange{warningSession, warningFilm, warningSet, setThird, warningAction, failingCleanup},
			wantPrinted: true, wantCleanupErr: true, wantWarnings: []printmgmt.StepStatus{
				{Step: printmgmt.StepCreateFilmSession, Status: 0xB600}, {Step: printmgmt.StepCreateFilmBox, Status: 0xB605},
				{Step: printmgmt.StepSetImageBox, Position: 2, Status: 0xB604}, {Step: printmgmt.StepPrintFilmBox, Status: 0xB60A},
			}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			address, peerDone := startSyntheticEventPrinter(t, ctx, func(ctx context.Context, a *ul.Association) error {
				for index, exchange := range tc.exchanges {
					if err := exchange.serve(ctx, a); err != nil {
						return fmt.Errorf("print exchange %d: %w", index+1, err)
					}
				}
				if tc.wantUnknown {
					return nil // Close after fully receiving N-ACTION, without a response.
				}
				pdu, err := a.Receive(ctx)
				if tc.wantAbort {
					var abort *ul.AbortError
					if !errors.As(err, &abort) {
						return fmt.Errorf("after malformed response got %T, %v, want A-ABORT", pdu, err)
					}
					return nil
				}
				if err != nil {
					return err
				}
				if _, ok := pdu.(*ul.ReleaseRQ); !ok {
					return fmt.Errorf("after final exchange got %T, want A-RELEASE-RQ", pdu)
				}
				return a.Send(ctx, &ul.ReleaseRP{})
			})
			assoc, err := ul.DialContext(ctx, address, ul.DialOptions{CallingAETitle: "MODALITY", CalledAETitle: "PRINTER", Contexts: printmgmt.PresentationContexts(false)})
			if err != nil {
				t.Fatal(err)
			}
			defer assoc.Close()
			result, printErr := printmgmt.NewClient(assoc, false).PrintFilm(ctx, film)
			if !tc.wantUnknown && !tc.wantAbort {
				if err := assoc.Release(ctx); err != nil {
					t.Errorf("release: %v", err)
				}
			}
			if err := <-peerDone; err != nil {
				t.Fatal(err)
			}
			if tc.wantStep != "" {
				var step *printmgmt.PrintError
				if !errors.As(printErr, &step) || step.Step != tc.wantStep || step.OutcomeUnknown != tc.wantUnknown ||
					step.HasStatus != (tc.wantStatus != 0) || step.Status != tc.wantStatus {
					t.Fatalf("print error = %#v, want step %s, status %04X, uncertain %t", step, tc.wantStep, tc.wantStatus, tc.wantUnknown)
				}
			} else if printErr != nil {
				t.Fatal(printErr)
			}
			if tc.wantBadRefs && (!errors.Is(printErr, printmgmt.ErrInvalidAttributes) ||
				result.FilmBoxUID != "" || result.ImageBoxes != 0 || result.ImagesSet != 0) {
				t.Fatalf("bad reference marked as accepted: result %+v, error %v", result, printErr)
			}
			if printErr != nil && (strings.Contains(printErr.Error(), invalidBoxUID) || strings.Contains(printErr.Error(), peerComment)) {
				t.Fatal("print error exposed invalid peer data")
			}
			if result.Printed != tc.wantPrinted || result.FilmSessionUID != sessionUID || !slices.Equal(result.Warnings, tc.wantWarnings) {
				t.Fatalf("print result = %+v", result)
			}
			if (tc.wantPrinted || tc.wantStep == printmgmt.StepPrintFilmBox) && (result.FilmBoxUID != filmUID || result.ImageBoxes != 3 || result.ImagesSet != 2) {
				t.Fatalf("film/image counts = %+v", result)
			}
			if tc.wantCleanupErr {
				var failure *printmgmt.PrintError
				var wire *dimse.NormalizedStatusError
				if !errors.As(result.CleanupErr, &failure) || failure.Step != printmgmt.StepDeleteFilmSession || failure.Status != 0x0112 ||
					strings.Contains(failure.Error(), peerComment) || !errors.As(failure, &wire) || wire.Fields == nil || wire.Fields.ErrorComment != peerComment {
					t.Fatalf("cleanup error = %v", result.CleanupErr)
				}
			} else if result.CleanupErr != nil {
				t.Fatalf("unexpected cleanup error = %v", result.CleanupErr)
			}
		})
	}
}

type printExchange struct {
	field                uint16
	class, uid, assigned string
	contextUID           string
	position             uint16
	response             *object.Object
	status               uint16
	fields               *dimse.NormalizedStatusFields
	closeWithoutResponse bool
}

func (exchange printExchange) serve(ctx context.Context, assoc *ul.Association) error {
	pc, command, err := dimse.ReceiveCommandSetAnyWithContext(ctx, assoc)
	if err != nil {
		return err
	}
	if exchange.contextUID != "" && !slices.ContainsFunc(assoc.AcceptedContexts, func(c ul.AcceptedContext) bool {
		return c.ID == pc && c.AbstractSyntaxUID == exchange.contextUID
	}) {
		return fmt.Errorf("presentation context %d does not carry %s", pc, exchange.contextUID)
	}
	field, err := dimse.CommandUint16(command, dimse.CommandField)
	if err != nil || field != exchange.field {
		return fmt.Errorf("command field = %04X, %v, want %04X", field, err, exchange.field)
	}
	id, err := dimse.CommandUint16(command, dimse.MessageID)
	if err != nil {
		return err
	}
	classTag, uidTag := dimse.RequestedSOPClassUID, dimse.RequestedSOPInstanceUID
	if field == dimse.NCreateRQ {
		classTag, uidTag = dimse.AffectedSOPClassUID, dimse.AffectedSOPInstanceUID
	}
	class, _ := command.GetString(classTag)
	uid, _ := command.GetString(uidTag)
	if class != exchange.class || uid != exchange.uid {
		return fmt.Errorf("class/instance = %s/%s, want %s/%s", class, uid, exchange.class, exchange.uid)
	}
	syntax := transfer.ExplicitVRLittleEndian
	if field == dimse.NCreateRQ || field == dimse.NSetRQ {
		dataset, err := dimse.ReceiveDataSet(assoc, pc, syntax)
		if err != nil {
			return err
		}
		if field == dimse.NSetRQ {
			positionTag := printmgmt.TagImageBoxPosition
			if exchange.class == printmgmt.AnnotationBoxSOPClassUID {
				positionTag = printmgmt.TagAnnotationPosition
			}
			position, err := dimse.CommandUint16(dataset, positionTag)
			if err != nil || position != exchange.position {
				return fmt.Errorf("box position = %d, %v, want %d", position, err, exchange.position)
			}
		}
	}
	if exchange.closeWithoutResponse {
		return nil
	}
	var response []core.Element
	switch field {
	case dimse.NCreateRQ:
		dataSetType := uint16(dimse.NoDataSet)
		if exchange.response != nil {
			dataSetType = dimse.DataSetPresent
		}
		response = (dimse.NormalizedCreateResponse{MessageIDBeingRespondedTo: id, Status: exchange.status, AffectedSOPClassUID: class, AffectedSOPInstanceUID: exchange.assigned, CommandDataSetType: dataSetType}).CommandSet()
	case dimse.NSetRQ:
		response = (dimse.NormalizedSetResponse{MessageIDBeingRespondedTo: id, Status: exchange.status, CommandDataSetType: dimse.NoDataSet}).CommandSet()
	case dimse.NActionRQ:
		response = (dimse.NormalizedActionResponse{MessageIDBeingRespondedTo: id, Status: exchange.status, CommandDataSetType: dimse.NoDataSet}).CommandSet()
	case dimse.NDeleteRQ:
		response = (dimse.NormalizedDeleteResponse{MessageIDBeingRespondedTo: id, Status: exchange.status, StatusFields: exchange.fields}).CommandSet()
	}
	if err := dimse.SendCommandSetWithContext(ctx, assoc, pc, response); err != nil {
		return err
	}
	if exchange.response != nil {
		return dimse.SendDataSetWithContext(ctx, assoc, pc, exchange.response, syntax)
	}
	return nil
}

func printImageBoxList(instances []string) *object.Object {
	references := make([]core.DataSet, 0, len(instances))
	for _, uid := range instances {
		references = append(references, core.DataSet{Elements: []core.Element{
			{Header: core.ElementHeader{Tag: printmgmt.TagReferencedSOPClassUID, VR: core.VRUI}, Value: core.StringValue{printmgmt.GrayscaleImageBoxUID}},
			{Header: core.ElementHeader{Tag: printmgmt.TagReferencedSOPInstanceUID, VR: core.VRUI}, Value: core.StringValue{uid}},
		}})
	}
	return object.FromElements([]core.Element{{Header: core.ElementHeader{Tag: printmgmt.TagReferencedImageBoxSequence, VR: core.VRSQ}, Value: core.SequenceValue{Items: references}}}, nil)
}
