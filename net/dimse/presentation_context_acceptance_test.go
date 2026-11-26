package dimse

import (
	"bytes"
	"context"
	"errors"
	"sync/atomic"
	"testing"
	"time"

	"github.com/ThalesMMS/dicom-go/core"
	"github.com/ThalesMMS/dicom-go/dictionary/std"
	"github.com/ThalesMMS/dicom-go/dictionary/tags"
	"github.com/ThalesMMS/dicom-go/net/ul"
	"github.com/ThalesMMS/dicom-go/object"
	"github.com/ThalesMMS/dicom-go/transfer"
)

// DCMTK dcmqrdb/tests/tdispatch.cc checks that the SCP rejects commands
// submitted through an incompatible accepted presentation context.
func TestReferenceAcceptanceRejectsDIMSECommandOnWrongPresentationContext(t *testing.T) {
	tests := []struct {
		name            string
		serviceSOP      string
		wrongContextSOP string
		responseField   uint16
		command         func(uint16) []core.Element
	}{
		{"echo-on-storage", VerificationSOPClassUID, secondaryCaptureSOPClassUID, CEchoRSP, func(id uint16) []core.Element {
			return (CEchoRequest{MessageID: id}).CommandSet()
		}},
		{"find-on-verification", StudyRootFindSOPClassUID, VerificationSOPClassUID, CFindRSP, func(id uint16) []core.Element {
			return (CFindRequest{MessageID: id, AffectedSOPClassUID: StudyRootFindSOPClassUID}).CommandSet()
		}},
		{"get-on-verification", StudyRootGetSOPClassUID, VerificationSOPClassUID, CGetRSP, func(id uint16) []core.Element {
			return (CGetRequest{MessageID: id, AffectedSOPClassUID: StudyRootGetSOPClassUID}).CommandSet()
		}},
		{"move-on-find", StudyRootMoveSOPClassUID, StudyRootFindSOPClassUID, CMoveRSP, func(id uint16) []core.Element {
			return (CMoveRequest{MessageID: id, AffectedSOPClassUID: StudyRootMoveSOPClassUID, MoveDestination: "DEST"}).CommandSet()
		}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			var handlerCalls atomic.Int32
			options := AssociationSCPOptions{
				CFindHandler: CFindHandlerFunc(func(context.Context, CFindRequestContext) ([]*object.Object, error) {
					handlerCalls.Add(1)
					return nil, nil
				}),
				CGetHandler: CGetHandlerFunc(func(context.Context, CGetRequestContext) ([]CGetSubOperation, error) {
					handlerCalls.Add(1)
					return nil, nil
				}),
				CMoveHandler: CMoveHandlerFunc(func(context.Context, CMoveRequestContext) ([]CMoveSubOperation, error) {
					handlerCalls.Add(1)
					return nil, nil
				}),
			}
			assoc, done := startDCMTKReferenceSCP(t, ctx, []string{tc.serviceSOP, tc.wrongContextSOP}, options)
			servicePC, err := AcceptedContextForSOPClass(assoc, tc.serviceSOP)
			if err != nil {
				t.Fatal(err)
			}
			wrongPC, err := AcceptedContextForSOPClass(assoc, tc.wrongContextSOP)
			if err != nil {
				t.Fatal(err)
			}
			var identifier *object.Object
			var callsPerValidCommand int32
			if tc.responseField != CEchoRSP {
				identifier = object.FromElements([]core.Element{
					{Header: core.ElementHeader{Tag: tags.QueryRetrieveLevel, VR: core.VRCS}, Value: core.StringValue{"STUDY"}},
					{Header: core.ElementHeader{Tag: tags.StudyInstanceUID, VR: core.VRUI}, Value: core.StringValue{"1.2.3.4"}},
				}, std.Dictionary)
				callsPerValidCommand = 1
			}
			exchange := func(pc byte, id uint16) (uint16, error) {
				return exchangeReferenceContextCommand(t, ctx, assoc, pc, id, tc.command(id), identifier, tc.responseField)
			}
			status, err := exchange(servicePC.ID, 1)
			if calls := handlerCalls.Load(); err != nil || status != StatusSuccess || calls != callsPerValidCommand {
				t.Fatalf("control on %s returned status %04X, error %v, handler calls %d; want success and %d calls",
					tc.serviceSOP, status, err, calls, callsPerValidCommand)
			}

			status, err = exchange(wrongPC.ID, 2)
			if calls := handlerCalls.Load() - callsPerValidCommand; calls != 0 {
				t.Errorf("handler calls for command on %s = %d; want zero", tc.wrongContextSOP, calls)
			}
			if !errors.Is(err, ul.ErrAssociationAborted) {
				t.Fatalf("command on %s returned status %04X, error %v; want A-ABORT", tc.wrongContextSOP, status, err)
			}
			if err := assoc.Close(); err != nil {
				t.Fatal(err)
			}
			select {
			case err := <-done:
				if !errors.Is(err, ErrPresentationContextMismatch) {
					t.Fatalf("SCP returned %v, want ErrPresentationContextMismatch", err)
				}
			case <-ctx.Done():
				t.Fatalf("waiting for SCP to finish: %v", ctx.Err())
			}
		})
	}
}

func exchangeReferenceContextCommand(t *testing.T, ctx context.Context, assoc *ul.Association, pc byte, id uint16, command []core.Element, identifier *object.Object, responseField uint16) (uint16, error) {
	t.Helper()
	rawCommand, err := EncodeCommandSet(command)
	if err != nil {
		t.Fatal(err)
	}
	values := []ul.PDataValue{{PresentationContextID: pc, IsCommand: true, IsLast: true, Data: rawCommand}}
	if identifier != nil {
		var rawIdentifier bytes.Buffer
		if err := object.WriteDataSet(&rawIdentifier, identifier, transfer.ExplicitVRLittleEndian); err != nil {
			t.Fatal(err)
		}
		values = append(values, ul.PDataValue{PresentationContextID: pc, IsLast: true, Data: rawIdentifier.Bytes()})
	}
	// One PDU places the entire request on the wire before a peer can abort.
	if err := assoc.Send(ctx, &ul.PDataTF{Values: values}); err != nil {
		t.Fatalf("sending command on presentation context %d: %v", pc, err)
	}
	resp, err := receiveCommandSetWithContext(ctx, assoc, pc)
	if err != nil {
		return 0, err
	}
	for _, field := range []struct {
		tag  core.Tag
		want uint16
	}{{CommandField, responseField}, {MessageIDBeingRespondedTo, id}} {
		got, err := CommandUint16(resp, field.tag)
		if err != nil || got != field.want {
			t.Fatalf("response %s = %04X, error %v; want %04X", field.tag, got, err, field.want)
		}
	}
	dataSetType, err := CommandUint16(resp, CommandDataSetType)
	if err != nil {
		t.Fatal(err)
	}
	if dataSetType != NoDataSet {
		if _, err := receiveIdentifierDataSetWithContext(ctx, assoc, pc, transfer.ExplicitVRLittleEndian); err != nil {
			t.Fatalf("reading response dataset: %v", err)
		}
	}
	status, err := CommandUint16(resp, Status)
	if err != nil {
		t.Fatal(err)
	}
	return status, nil
}
