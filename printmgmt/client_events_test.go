package printmgmt_test

import (
	"context"
	"errors"
	"fmt"
	"sync/atomic"
	"testing"
	"time"

	"github.com/ThalesMMS/dicom-go/core"
	"github.com/ThalesMMS/dicom-go/net/dimse"
	"github.com/ThalesMMS/dicom-go/net/ul"
	"github.com/ThalesMMS/dicom-go/object"
	"github.com/ThalesMMS/dicom-go/printmgmt"
	"github.com/ThalesMMS/dicom-go/transfer"
)

func startSyntheticEventPrinter(t *testing.T, ctx context.Context, serve func(context.Context, *ul.Association) error) (string, <-chan error) {
	t.Helper()
	return startSyntheticPrinter(t, ctx, []string{printmgmt.GrayscaleMetaSOPClassUID}, serve)
}

func startSyntheticPrinter(t *testing.T, ctx context.Context, abstracts []string, serve func(context.Context, *ul.Association) error) (string, <-chan error) {
	t.Helper()
	listener, err := ul.Listen(ul.ListenOptions{Address: "127.0.0.1:0", Context: ctx})
	if err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	server, err := ul.NewAssociationServer(listener, ul.AssociationServerOptions{MaxConcurrentAssociations: 1, Accept: ul.AcceptOptions{AETitle: "PRINTER", SupportedAbstractSyntaxes: abstracts, SupportedTransferSyntaxes: []string{transfer.ExplicitVRLittleEndian.UID}}, Handler: func(c context.Context, a *ul.Association) error {
		defer a.Close()
		err := serve(c, a)
		done <- err
		return nil
	}})
	if err != nil {
		t.Fatal(err)
	}
	serverCtx, stop := context.WithCancel(ctx)
	serverDone := make(chan error, 1)
	go func() { serverDone <- server.Serve(serverCtx) }()
	t.Cleanup(func() { stop(); _ = listener.Close(); <-serverDone })
	return listener.Addr().String(), done
}

func syntheticPrinterEvent(ctx context.Context, assoc *ul.Association, pcID byte, eventType uint16, instance string, wantStatus uint16) error {
	req := dimse.NormalizedEventReportRequest{AffectedSOPClassUID: printmgmt.PrinterSOPClassUID, AffectedSOPInstanceUID: instance, MessageID: 20 + eventType, EventTypeID: eventType, CommandDataSetType: dimse.NoDataSet}
	if eventType != 1 {
		req.CommandDataSetType = dimse.DataSetPresent
	}
	if err := dimse.SendCommandSetWithContext(ctx, assoc, pcID, req.CommandSet()); err != nil {
		return err
	}
	if eventType != 1 {
		info := object.FromElements([]core.Element{{Header: core.ElementHeader{Tag: printmgmt.TagPrinterStatusInfo, VR: core.VRCS}, Value: core.StringValue{"SYNTHETIC"}}}, nil)
		if err := dimse.SendDataSetWithContext(ctx, assoc, pcID, info, transfer.ExplicitVRLittleEndian); err != nil {
			return err
		}
	}
	responsePC, command, err := dimse.ReceiveCommandSetAnyWithContext(ctx, assoc)
	if err != nil {
		return err
	}
	rsp, err := dimse.ParseNormalizedEventReportResponse(command)
	if err != nil {
		return err
	}
	if responsePC != pcID || rsp.Status != wantStatus || rsp.MessageIDBeingRespondedTo != req.MessageID || rsp.EventTypeIDOrNil == nil || *rsp.EventTypeIDOrNil != eventType {
		return fmt.Errorf("synthetic event ACK correlation/status mismatch: %#v", rsp)
	}
	return nil
}

func TestPrintClientInterleavedPrinterEvents(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	address, done := startSyntheticEventPrinter(t, ctx, func(c context.Context, assoc *ul.Association) error {
		for i := 0; i < 3; i++ {
			pc, command, err := dimse.ReceiveCommandSetAnyWithContext(c, assoc)
			if err != nil {
				return err
			}
			id, err := dimse.CommandUint16(command, dimse.MessageID)
			if err != nil {
				return err
			}
			dsType, err := dimse.CommandUint16(command, dimse.CommandDataSetType)
			if err != nil {
				return err
			}
			if dsType != dimse.NoDataSet {
				if _, err := dimse.ReceiveDataSet(assoc, pc, transfer.ExplicitVRLittleEndian); err != nil {
					return err
				}
			}
			if err := syntheticPrinterEvent(c, assoc, pc, uint16(i+1), printmgmt.PrinterSOPInstanceUID, dimse.StatusSuccess); err != nil {
				return err
			}
			var response []core.Element
			switch i {
			case 0:
				response = (dimse.NormalizedCreateResponse{MessageIDBeingRespondedTo: id, AffectedSOPInstanceUID: "1.2.3.4", CommandDataSetType: dimse.NoDataSet}).CommandSet()
			case 1:
				response = (dimse.NormalizedSetResponse{MessageIDBeingRespondedTo: id, CommandDataSetType: dimse.NoDataSet}).CommandSet()
			case 2:
				response = (dimse.NormalizedActionResponse{MessageIDBeingRespondedTo: id, CommandDataSetType: dimse.NoDataSet}).CommandSet()
			}
			if err := dimse.SendCommandSetWithContext(c, assoc, pc, response); err != nil {
				return err
			}
		}
		return nil
	})
	assoc, err := ul.DialContext(ctx, address, ul.DialOptions{CallingAETitle: "MODALITY", CalledAETitle: "PRINTER", Contexts: printmgmt.PresentationContexts(false)})
	if err != nil {
		t.Fatal(err)
	}
	defer assoc.Close()
	var calls atomic.Int32
	configured := &printmgmt.PrinterEventReportOptions{Handler: func(c context.Context, req dimse.NormalizedEventReportRequest, info *object.Object) (dimse.NormalizedEventReportSCPResult, error) {
		calls.Add(1)
		pc, ok := dimse.NormalizedRequestInfoFromContext(c)
		if !ok || pc.PresentationContext.AbstractSyntaxUID != printmgmt.GrayscaleMetaSOPClassUID || req.AffectedSOPClassUID != printmgmt.PrinterSOPClassUID || (req.EventTypeID == 1) != (info == nil) {
			return dimse.NormalizedEventReportSCPResult{}, fmt.Errorf("invalid captured printer event")
		}
		return dimse.NormalizedEventReportSCPResult{}, nil
	}}
	client := printmgmt.NewClientWithOptions(assoc, false, printmgmt.ClientOptions{PrinterEvents: configured})
	configured.Handler = nil // construction must have captured the callback.
	uid, _, err := client.CreateFilmSession(ctx, "", printmgmt.FilmSessionPresentation{})
	if err != nil || uid != "1.2.3.4" {
		t.Fatalf("CreateFilmSession uid=%s err=%v", uid, err)
	}
	if _, err := client.SetFilmBox(ctx, "1.2.3.5", object.FromElements([]core.Element{{Header: core.ElementHeader{Tag: printmgmt.TagFilmOrientation, VR: core.VRCS}, Value: core.StringValue{"PORTRAIT"}}}, nil)); err != nil {
		t.Fatal(err)
	}
	if _, err := client.PrintFilmBox(ctx, "1.2.3.5"); err != nil {
		t.Fatal(err)
	}
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	if calls.Load() != 3 {
		t.Fatalf("handler calls=%d", calls.Load())
	}
}

func TestPrintClientEventOptInRefusesUnsupportedAndPreservesDefault(t *testing.T) {
	for _, name := range []string{"legacy-default", "explicit-refusal", "unsupported-type", "wrong-instance"} {
		t.Run(name, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
			defer cancel()
			want := dimse.StatusUnrecognizedOperation
			eventType := uint16(1)
			instance := printmgmt.PrinterSOPInstanceUID
			if name == "unsupported-type" {
				want = dimse.StatusNoSuchEventType
				eventType = 99
			}
			if name == "wrong-instance" {
				want = dimse.StatusNoSuchSOPInstance
				instance = "1.2.3.99"
			}
			address, done := startSyntheticEventPrinter(t, ctx, func(c context.Context, a *ul.Association) error {
				pc, command, err := dimse.ReceiveCommandSetAnyWithContext(c, a)
				if err != nil {
					return err
				}
				err = syntheticPrinterEvent(c, a, pc, eventType, instance, want)
				if name == "legacy-default" {
					var abort *ul.AbortError
					if !errors.As(err, &abort) {
						return fmt.Errorf("expected legacy abort: %v", err)
					}
					return nil
				}
				if err != nil {
					return err
				}
				id, _ := dimse.CommandUint16(command, dimse.MessageID)
				return dimse.SendCommandSetWithContext(c, a, pc, (dimse.NormalizedActionResponse{MessageIDBeingRespondedTo: id, CommandDataSetType: dimse.NoDataSet}).CommandSet())
			})
			assoc, err := ul.DialContext(ctx, address, ul.DialOptions{CallingAETitle: "MODALITY", CalledAETitle: "PRINTER", Contexts: printmgmt.PresentationContexts(false)})
			if err != nil {
				t.Fatal(err)
			}
			defer assoc.Close()
			client := printmgmt.NewClient(assoc, false)
			if name != "legacy-default" {
				options := &printmgmt.PrinterEventReportOptions{}
				if name != "explicit-refusal" {
					options.Handler = func(context.Context, dimse.NormalizedEventReportRequest, *object.Object) (dimse.NormalizedEventReportSCPResult, error) {
						t.Error("unsupported printer event called handler")
						return dimse.NormalizedEventReportSCPResult{}, nil
					}
				}
				client = printmgmt.NewClientWithOptions(assoc, false, printmgmt.ClientOptions{PrinterEvents: options})
			}
			_, err = client.PrintFilmBox(ctx, "1.2.3.5")
			if name == "legacy-default" {
				if !errors.Is(err, dimse.ErrAssociationStateUncertain) {
					t.Fatalf("legacy err=%v", err)
				}
			} else if err != nil {
				t.Fatal(err)
			}
			if err := <-done; err != nil {
				t.Fatal(err)
			}
		})
	}
}
