package dimse

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"sync/atomic"
	"testing"
	"time"

	"github.com/ThalesMMS/dicom-go/core"
	"github.com/ThalesMMS/dicom-go/net/ul"
	"github.com/ThalesMMS/dicom-go/object"
	"github.com/ThalesMMS/dicom-go/transfer"
)

const eventTestClass = "1.2.4"
const operationTestClass = "1.2.3"
const eventTestInstance = "1.2.4.5"

func eventTestContexts() []ul.AcceptedContext {
	return []ul.AcceptedContext{
		{ID: 1, AbstractSyntaxUID: operationTestClass, TransferSyntaxUID: transfer.ImplicitVRLittleEndian.UID},
		{ID: 3, AbstractSyntaxUID: eventTestClass, TransferSyntaxUID: transfer.ExplicitVRBigEndian.UID},
	}
}

func eventTestOptions(handler NormalizedEventReportHandler) *NormalizedEventReportOptions {
	return &NormalizedEventReportOptions{Handler: handler, Subscriptions: []NormalizedEventReportSubscription{{SOPClassUID: eventTestClass, EventTypeIDs: []uint16{1, 2, 3}}}}
}

func eventTestRequest(id, eventType uint16) NormalizedEventReportRequest {
	return NormalizedEventReportRequest{AffectedSOPClassUID: eventTestClass, AffectedSOPInstanceUID: eventTestInstance,
		MessageID: id, EventTypeID: eventType, CommandDataSetType: DataSetPresent}
}

func eventTestInformation() *object.Object {
	return object.FromElements([]core.Element{{Header: core.ElementHeader{Tag: core.NewTag(0x2110, 0x0020), VR: core.VRCS}, Value: core.StringValue{"SYNTHETIC"}}}, nil)
}

func receiveTestOriginal(ctx context.Context, peer *ul.Association) (*object.Object, error) {
	command, err := receiveCommandSetWithContext(ctx, peer, 1)
	if err != nil {
		return nil, err
	}
	dataSetType, err := CommandUint16(command, CommandDataSetType)
	if err == nil && normalizedHasDataSet(dataSetType) {
		_, err = receiveDataSetWithContext(ctx, peer, 1, transfer.ImplicitVRLittleEndian)
	}
	return command, err
}

func respondTestOriginal(ctx context.Context, peer *ul.Association, command *object.Object, pcID byte, wrongCorrelation bool) error {
	id, err := CommandUint16(command, MessageID)
	if err != nil {
		return err
	}
	if wrongCorrelation {
		id++
	}
	field, err := CommandUint16(command, CommandField)
	if err != nil {
		return err
	}
	switch field {
	case NCreateRQ:
		return SendCommandSetWithContext(ctx, peer, pcID, (NormalizedCreateResponse{MessageIDBeingRespondedTo: id, CommandDataSetType: NoDataSet, AffectedSOPInstanceUID: "1.2.3.4"}).CommandSet())
	case NSetRQ:
		return SendCommandSetWithContext(ctx, peer, pcID, (NormalizedSetResponse{MessageIDBeingRespondedTo: id, CommandDataSetType: NoDataSet}).CommandSet())
	case NActionRQ:
		return SendCommandSetWithContext(ctx, peer, pcID, (NormalizedActionResponse{MessageIDBeingRespondedTo: id, CommandDataSetType: NoDataSet}).CommandSet())
	case NDeleteRQ:
		return SendCommandSetWithContext(ctx, peer, pcID, (NormalizedDeleteResponse{MessageIDBeingRespondedTo: id}).CommandSet())
	default:
		return fmt.Errorf("test: unexpected original field")
	}
}

func sendTestEvent(ctx context.Context, peer *ul.Association, pcID byte, request NormalizedEventReportRequest, information *object.Object, wantStatus uint16, wantReply bool) error {
	syntax := transfer.ExplicitVRBigEndian
	if pcID == 1 {
		syntax = transfer.ImplicitVRLittleEndian
	}
	if err := sendMessageDataSet(ctx, peer, pcID, request.CommandSet(), information, syntax); err != nil {
		return err
	}
	command, err := receiveCommandSetWithContext(ctx, peer, pcID)
	if err != nil {
		return err
	}
	response, err := ParseNormalizedEventReportResponse(command)
	if err != nil {
		return err
	}
	if response.MessageIDBeingRespondedTo != request.MessageID || response.Status != wantStatus ||
		response.AffectedSOPClassUID != request.AffectedSOPClassUID || response.AffectedSOPInstanceUID != request.AffectedSOPInstanceUID ||
		response.EventTypeIDOrNil == nil || *response.EventTypeIDOrNil != request.EventTypeID {
		return fmt.Errorf("test: event response correlation/status mismatch: %#v", response)
	}
	if normalizedHasDataSet(response.CommandDataSetType) != wantReply {
		return fmt.Errorf("test: event reply dataset flag mismatch")
	}
	if wantReply {
		reply, err := receiveDataSetWithContext(ctx, peer, pcID, syntax)
		if err != nil {
			return err
		}
		value, _ := reply.GetString(core.NewTag(0x2110, 0x0020))
		if value != "SYNTHETIC" {
			return fmt.Errorf("test: event reply value changed")
		}
	}
	return nil
}

func invokeEventTestOperation(ctx context.Context, client *NormalizedClient, service string, options NormalizedOperationOptions) (NormalizedExchangeInfo, error) {
	options.Context = ctx
	switch service {
	case "N-CREATE":
		r, err := client.CreateWithOptions(options, NormalizedCreateRequest{AffectedSOPClassUID: operationTestClass, CommandDataSetType: DataSetPresent}, eventTestInformation())
		if err == nil && (r.Response == nil || r.Response.AffectedSOPInstanceUID != "1.2.3.4") {
			return r.NormalizedExchangeInfo, fmt.Errorf("test: lost create assigned instance")
		}
		return r.NormalizedExchangeInfo, err
	case "N-SET":
		r, err := client.SetWithOptions(options, NormalizedSetRequest{RequestedSOPClassUID: operationTestClass, RequestedSOPInstanceUID: "1.2.3.4"}, eventTestInformation())
		if err == nil && r.Response == nil {
			return r.NormalizedExchangeInfo, fmt.Errorf("test: lost set response")
		}
		return r.NormalizedExchangeInfo, err
	default:
		r, err := client.ActionWithOptions(options, NormalizedActionRequest{RequestedSOPClassUID: operationTestClass, RequestedSOPInstanceUID: "1.2.3.4", ActionTypeID: 1, CommandDataSetType: NoDataSet}, nil)
		if err == nil && r.Response == nil {
			return r.NormalizedExchangeInfo, fmt.Errorf("test: lost action response")
		}
		return r.NormalizedExchangeInfo, err
	}
}

func TestNormalizedClientInterleavedEventsPreserveOperations(t *testing.T) {
	for _, service := range []string{"N-CREATE", "N-SET", "N-ACTION"} {
		t.Run(service, func(t *testing.T) {
			peer, local := testPipeAssociations(t, eventTestContexts())
			peer.PeerMaxPDU, local.PeerMaxPDU = 64, 64
			ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
			defer cancel()
			done := make(chan error, 1)
			go func() {
				command, err := receiveTestOriginal(ctx, peer)
				if err == nil {
					for id := uint16(10); id < 13 && err == nil; id++ {
						err = sendTestEvent(ctx, peer, 3, eventTestRequest(id, id-9), eventTestInformation(), StatusSuccess, true)
					}
				}
				if err == nil {
					err = respondTestOriginal(ctx, peer, command, 1, false)
				}
				if err == nil {
					command, err = receiveTestOriginal(ctx, peer)
					if err == nil {
						err = respondTestOriginal(ctx, peer, command, 1, false)
					}
				}
				done <- err
			}()
			calls := 0
			client := NewNormalizedClient(local)
			info, err := invokeEventTestOperation(ctx, client, service, NormalizedOperationOptions{EventReports: eventTestOptions(func(handlerCtx context.Context, request NormalizedEventReportRequest, dataSet *object.Object) (NormalizedEventReportSCPResult, error) {
				calls++
				requestInfo, ok := NormalizedRequestInfoFromContext(handlerCtx)
				if !ok || requestInfo.PresentationContext.ID != 3 || requestInfo.TransferSyntax.UID != transfer.ExplicitVRBigEndian.UID || dataSet == nil {
					return NormalizedEventReportSCPResult{}, fmt.Errorf("test: wrong event context/dataset")
				}
				// Reentry is refused without creating another association reader.
				_, reentryErr := client.Delete(handlerCtx, NormalizedDeleteRequest{RequestedSOPClassUID: operationTestClass, RequestedSOPInstanceUID: "1.2.3.4"})
				if !errors.Is(reentryErr, ErrOperationInProgress) {
					return NormalizedEventReportSCPResult{}, fmt.Errorf("test: reentry was not refused")
				}
				return NormalizedEventReportSCPResult{DataSet: dataSet}, nil
			})})
			if err != nil || calls != 3 || len(info.EventReports) != 3 || info.PresentationContext.ID != 1 || info.TransferSyntax.UID != transfer.ImplicitVRLittleEndian.UID {
				t.Fatalf("original exchange: calls=%d info=%#v err=%v", calls, info, err)
			}
			if _, err := client.Delete(ctx, NormalizedDeleteRequest{RequestedSOPClassUID: operationTestClass, RequestedSOPInstanceUID: "1.2.3.4"}); err != nil {
				t.Fatalf("association reuse: %v", err)
			}
			if err := <-done; err != nil {
				t.Fatal(err)
			}
		})
	}
}

func TestNormalizedClientInterleavedEventRefusalsKeepOriginal(t *testing.T) {
	for _, tc := range []struct {
		name      string
		configure func(*NormalizedEventReportOptions)
		mutate    func(*NormalizedEventReportRequest)
		want      uint16
	}{
		{"no-handler-explicit", func(o *NormalizedEventReportOptions) { o.Handler = nil }, nil, StatusUnrecognizedOperation},
		{"unsubscribed-class", nil, func(r *NormalizedEventReportRequest) { r.AffectedSOPClassUID = "1.2.8" }, StatusNoSuchSOPClass},
		{"context-policy-panic", func(o *NormalizedEventReportOptions) {
			o.PresentationContextPolicy = func(ul.AcceptedContext, string) error { panic("synthetic policy") }
		}, nil, StatusNoSuchSOPClass},
		{"invalid-event-type", nil, func(r *NormalizedEventReportRequest) { r.EventTypeID = 99 }, StatusNoSuchEventType},
		{"handler-failure", func(o *NormalizedEventReportOptions) {
			o.Handler = func(context.Context, NormalizedEventReportRequest, *object.Object) (NormalizedEventReportSCPResult, error) {
				return NormalizedEventReportSCPResult{}, errors.New("synthetic handler error")
			}
		}, nil, StatusProcessingFailure},
		{"handler-explicit-failure", func(o *NormalizedEventReportOptions) {
			o.Handler = func(context.Context, NormalizedEventReportRequest, *object.Object) (NormalizedEventReportSCPResult, error) {
				return NormalizedEventReportSCPResult{Response: NormalizedEventReportResponse{Status: StatusNoSuchSOPInstance}}, errors.New("synthetic handler error")
			}
		}, nil, StatusNoSuchSOPInstance},
		{"handler-panic", func(o *NormalizedEventReportOptions) {
			o.Handler = func(context.Context, NormalizedEventReportRequest, *object.Object) (NormalizedEventReportSCPResult, error) {
				panic("synthetic")
			}
		}, nil, StatusProcessingFailure},
		{"handler-correlation", func(o *NormalizedEventReportOptions) {
			o.Handler = func(context.Context, NormalizedEventReportRequest, *object.Object) (NormalizedEventReportSCPResult, error) {
				return NormalizedEventReportSCPResult{Response: NormalizedEventReportResponse{MessageIDBeingRespondedTo: 999}}, nil
			}
		}, nil, StatusProcessingFailure},
		{"handler-type-correlation", func(o *NormalizedEventReportOptions) {
			o.Handler = func(context.Context, NormalizedEventReportRequest, *object.Object) (NormalizedEventReportSCPResult, error) {
				wrong := uint16(99)
				return NormalizedEventReportSCPResult{Response: NormalizedEventReportResponse{EventTypeIDOrNil: &wrong}}, nil
			}
		}, nil, StatusProcessingFailure},
		{"reply-exceeds-limit", func(o *NormalizedEventReportOptions) {
			o.MaxDataSetBytes = 64
			o.Handler = func(context.Context, NormalizedEventReportRequest, *object.Object) (NormalizedEventReportSCPResult, error) {
				return NormalizedEventReportSCPResult{DataSet: normalizedTestDataSet(string(bytes.Repeat([]byte{'X'}, 100)))}, nil
			}
		}, nil, StatusProcessingFailure},
	} {
		t.Run(tc.name, func(t *testing.T) {
			peer, local := testPipeAssociations(t, eventTestContexts())
			ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
			defer cancel()
			options := eventTestOptions(func(context.Context, NormalizedEventReportRequest, *object.Object) (NormalizedEventReportSCPResult, error) {
				t.Error("refused event called handler")
				return NormalizedEventReportSCPResult{}, nil
			})
			if tc.configure != nil {
				tc.configure(options)
			}
			request := eventTestRequest(10, 1)
			if tc.mutate != nil {
				tc.mutate(&request)
			}
			done := make(chan error, 1)
			go func() {
				command, err := receiveTestOriginal(ctx, peer)
				if err == nil {
					err = sendTestEvent(ctx, peer, 3, request, eventTestInformation(), tc.want, false)
				}
				if err == nil {
					err = respondTestOriginal(ctx, peer, command, 1, false)
				}
				done <- err
			}()
			info, err := invokeEventTestOperation(ctx, NewNormalizedClient(local), "N-ACTION", NormalizedOperationOptions{EventReports: options})
			if err != nil || len(info.EventReports) != 1 || info.EventReports[0].Status != tc.want {
				t.Fatalf("info=%#v err=%v", info, err)
			}
			if err := <-done; err != nil {
				t.Fatal(err)
			}
		})
	}
}

func TestNormalizedClientInterleavedEventFatalCasesAbort(t *testing.T) {
	for _, name := range []string{"default-no-opt-in", "event-count", "dataset-size", "unknown-context", "invalid-uid", "wrong-original-id", "wrong-original-context", "event-dataset-context"} {
		t.Run(name, func(t *testing.T) {
			peer, local := testPipeAssociations(t, eventTestContexts())
			ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
			defer cancel()
			options := eventTestOptions(func(context.Context, NormalizedEventReportRequest, *object.Object) (NormalizedEventReportSCPResult, error) {
				return NormalizedEventReportSCPResult{}, nil
			})
			if name == "default-no-opt-in" {
				options = nil
			}
			if name == "event-count" {
				options.MaxEvents = 1
			}
			if name == "dataset-size" {
				options.MaxDataSetBytes = 8
			}
			done := make(chan error, 1)
			go func() {
				command, err := receiveTestOriginal(ctx, peer)
				if err == nil && (name == "wrong-original-id" || name == "wrong-original-context") {
					err = sendTestEvent(ctx, peer, 3, eventTestRequest(10, 1), eventTestInformation(), StatusSuccess, false)
					pcID := byte(1)
					if name == "wrong-original-context" {
						pcID = 3
					}
					if err == nil {
						err = respondTestOriginal(ctx, peer, command, pcID, name == "wrong-original-id")
					}
				} else if err == nil {
					if name == "event-count" {
						err = sendTestEvent(ctx, peer, 3, eventTestRequest(10, 1), eventTestInformation(), StatusSuccess, false)
					}
					if err == nil {
						request := eventTestRequest(11, 1)
						if name == "invalid-uid" {
							request.AffectedSOPInstanceUID = "bad"
						}
						pcID := byte(3)
						if name == "unknown-context" {
							pcID = 5
						}
						if name == "event-dataset-context" {
							err = SendCommandSetWithContext(ctx, peer, 3, request.CommandSet())
							if err == nil {
								err = SendDataSetWithContext(ctx, peer, 1, eventTestInformation(), transfer.ImplicitVRLittleEndian)
							}
						} else {
							err = sendMessageDataSet(ctx, peer, pcID, request.CommandSet(), eventTestInformation(), transfer.ExplicitVRBigEndian)
						}
					}
				}
				if err == nil {
					_, err = peer.Receive(ctx)
				}
				// Abort/closed writes are expected for a malformed peer exchange.
				if err == nil {
					err = fmt.Errorf("test: expected peer abort")
				} else {
					err = nil
				}
				done <- err
			}()
			_, err := invokeEventTestOperation(ctx, NewNormalizedClient(local), "N-ACTION", NormalizedOperationOptions{EventReports: options})
			if err == nil || !errors.Is(err, ErrAssociationStateUncertain) || errors.Is(err, ErrOperationTimeout) || errors.Is(err, ErrOperationCanceled) {
				t.Fatalf("err=%v, want protocol/limit uncertain failure", err)
			}
			if err := <-done; err != nil {
				t.Fatal(err)
			}
			release, err := beginAssociationOperation(local)
			if err != nil {
				t.Fatal(err)
			}
			release()
		})
	}
}

func TestNormalizedClientInterleavedEventDeadlineAndCancel(t *testing.T) {
	for _, name := range []string{"response-deadline", "cancel-in-handler"} {
		t.Run(name, func(t *testing.T) {
			peer, local := testPipeAssociations(t, eventTestContexts())
			ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
			defer cancel()
			done := make(chan error, 1)
			go func() {
				_, err := receiveTestOriginal(ctx, peer)
				if err == nil {
					err = sendMessageDataSet(ctx, peer, 3, eventTestRequest(10, 1).CommandSet(), eventTestInformation(), transfer.ExplicitVRBigEndian)
				}
				if err == nil {
					_, err = peer.Receive(ctx)
				}
				if err == nil {
					err = fmt.Errorf("test: expected abort")
				} else {
					err = nil
				}
				done <- err
			}()
			var handlerExited atomic.Bool
			var originalDeadline time.Time
			options := NormalizedOperationOptions{OperationOptions: OperationOptions{ResponseTimeout: 40 * time.Millisecond}, EventReports: eventTestOptions(func(handlerCtx context.Context, _ NormalizedEventReportRequest, _ *object.Object) (NormalizedEventReportSCPResult, error) {
				originalDeadline, _ = handlerCtx.Deadline()
				if name == "cancel-in-handler" {
					cancel()
				}
				<-handlerCtx.Done()
				handlerExited.Store(true)
				return NormalizedEventReportSCPResult{}, handlerCtx.Err()
			})}
			_, err := invokeEventTestOperation(ctx, NewNormalizedClient(local), "N-ACTION", options)
			want := ErrOperationTimeout
			if name == "cancel-in-handler" {
				want = ErrOperationCanceled
			}
			if !errors.Is(err, want) || !errors.Is(err, ErrAssociationStateUncertain) || !handlerExited.Load() || originalDeadline.IsZero() {
				t.Fatalf("handlerExited=%v deadline=%v err=%v", handlerExited.Load(), originalDeadline, err)
			}
			if err := <-done; err != nil {
				t.Fatal(err)
			}
		})
	}
}

func TestNormalizedEventConfigurationIsDetachedAndValidatedBeforeSend(t *testing.T) {
	options := eventTestOptions(func(context.Context, NormalizedEventReportRequest, *object.Object) (NormalizedEventReportSCPResult, error) {
		return NormalizedEventReportSCPResult{}, nil
	})
	state, err := newNormalizedEventReportState(options)
	if err != nil {
		t.Fatal(err)
	}
	options.Subscriptions[0].EventTypeIDs[0] = 99
	options.Subscriptions[0].SOPClassUID = "1.2.99"
	options.MaxEvents = 99
	if _, ok := state.allowed[eventTestClass][1]; !ok || state.options.MaxEvents != DefaultMaxInterleavedEventReports || state.options.Subscriptions != nil {
		t.Fatal("configuration aliases caller")
	}
	for _, options := range []*NormalizedEventReportOptions{{MaxEvents: -1}, {MaxDataSetBytes: -1}, {Handler: state.options.Handler}, {Subscriptions: []NormalizedEventReportSubscription{{SOPClassUID: "invalid"}}}, {Subscriptions: []NormalizedEventReportSubscription{{SOPClassUID: eventTestClass, EventTypeIDs: []uint16{1, 1}}}}} {
		_, local := testPipeAssociations(t, eventTestContexts())
		_, err := invokeEventTestOperation(context.Background(), NewNormalizedClient(local), "N-ACTION", NormalizedOperationOptions{EventReports: options})
		if err == nil || errors.Is(err, ErrAssociationStateUncertain) {
			t.Fatalf("pre-send configuration: %v", err)
		}
	}
}

func TestInterleavedEventDeflatedReplyLogicalLimit(t *testing.T) {
	large := normalizedTestDataSet(string(bytes.Repeat([]byte{'X'}, 4096)))
	if _, err := encodeInterleavedEventReply(context.Background(), large, transfer.DeflatedExplicitVRLittleEndian, 128); err == nil {
		t.Fatal("compressed size bypassed logical bound")
	}
	raw, err := encodeInterleavedEventReply(context.Background(), eventTestInformation(), transfer.DeflatedExplicitVRLittleEndian, 1024)
	if err != nil {
		t.Fatal(err)
	}
	obj, err := object.ReadDataSetWithOptions(bytes.NewReader(raw), transfer.DeflatedExplicitVRLittleEndian, object.ReadFileOptions{MaxTotalBytes: 1024})
	if err != nil || obj.Len() != 1 {
		t.Fatalf("deflated reply: obj=%v err=%v", obj, err)
	}
}

func TestNormalizedClientInterleavedDeflatedWireAndExpansionBounds(t *testing.T) {
	for _, name := range []string{"exact-wire-cap-with-next-command", "trailing-past-wire-cap", "expanded-past-logical-cap"} {
		t.Run(name, func(t *testing.T) {
			contexts := eventTestContexts()
			contexts[1].TransferSyntaxUID = transfer.DeflatedExplicitVRLittleEndian.UID
			peer, local := testPipeAssociations(t, contexts)
			ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
			defer cancel()
			information := eventTestInformation()
			if name == "expanded-past-logical-cap" {
				information = normalizedTestDataSet(string(bytes.Repeat([]byte{'X'}, 4096)))
			}
			var encoded bytes.Buffer
			if err := object.WriteDataSet(&encoded, information, transfer.DeflatedExplicitVRLittleEndian); err != nil {
				t.Fatal(err)
			}
			wire := append(append([]byte(nil), encoded.Bytes()...), make([]byte, 24)...)
			limit := int64(len(wire))
			if name == "trailing-past-wire-cap" {
				wire = append(wire, 0, 0)
			}
			if name == "expanded-past-logical-cap" {
				limit = 128
			}
			done := make(chan error, 1)
			go func() {
				command, err := receiveTestOriginal(ctx, peer)
				if err == nil {
					err = SendCommandSetWithContext(ctx, peer, 3, eventTestRequest(10, 1).CommandSet())
				}
				if err == nil {
					id, _ := CommandUint16(command, MessageID)
					original, encodeErr := EncodeCommandSet((NormalizedActionResponse{MessageIDBeingRespondedTo: id, CommandDataSetType: NoDataSet}).CommandSet())
					err = encodeErr
					if err == nil {
						// The next command shares this PDU with the event's final data
						// PDV. The bounded drain must leave it for the command reader.
						err = peer.Send(ctx, &ul.PDataTF{Values: []ul.PDataValue{
							{PresentationContextID: 3, IsLast: true, Data: wire},
							{PresentationContextID: 1, IsCommand: true, IsLast: true, Data: original},
						}})
					}
				}
				if err == nil {
					var reply *object.Object
					reply, err = receiveCommandSetWithContext(ctx, peer, 3)
					if name == "exact-wire-cap-with-next-command" && err == nil {
						rsp, parseErr := ParseNormalizedEventReportResponse(reply)
						err = parseErr
						if err == nil && (rsp.Status != StatusSuccess || rsp.MessageIDBeingRespondedTo != 10) {
							err = fmt.Errorf("test: invalid successful ACK")
						}
					}
				}
				if name != "exact-wire-cap-with-next-command" {
					if err == nil {
						err = fmt.Errorf("test: exceeded dataset received ACK")
					} else if errors.Is(err, context.DeadlineExceeded) {
						err = fmt.Errorf("test: peer timed out instead of being aborted: %w", err)
					} else {
						err = nil
					}
				}
				done <- err
			}()
			called := false
			options := eventTestOptions(func(context.Context, NormalizedEventReportRequest, *object.Object) (NormalizedEventReportSCPResult, error) {
				called = true
				return NormalizedEventReportSCPResult{}, nil
			})
			options.MaxDataSetBytes = limit
			info, err := invokeEventTestOperation(ctx, NewNormalizedClient(local), "N-ACTION", NormalizedOperationOptions{EventReports: options})
			if name == "exact-wire-cap-with-next-command" {
				if err != nil || !called || len(info.EventReports) != 1 {
					t.Fatalf("exact boundary: info=%#v called=%v err=%v", info, called, err)
				}
			} else if err == nil || !errors.Is(err, ErrAssociationStateUncertain) || called {
				t.Fatalf("overflow: called=%v err=%v", called, err)
			}
			if err := <-done; err != nil {
				t.Fatal(err)
			}
		})
	}
}

func TestNormalizedClientInterleavedEventNodeAndDepthBounds(t *testing.T) {
	for _, name := range []string{"nodes", "empty-items", "depth"} {
		t.Run(name, func(t *testing.T) {
			information := eventTestInformation()
			if name == "nodes" {
				elements := make([]core.Element, MaxNormalizedDataSetElements+1)
				for i := range elements {
					elements[i] = core.Element{Header: core.ElementHeader{Tag: core.NewTag(0x0042, uint16(i+1)), VR: core.VRLO}, Value: core.StringValue{"X"}}
				}
				information = object.FromElements(elements, nil)
			} else if name == "empty-items" {
				information = object.FromElements([]core.Element{{Header: core.ElementHeader{Tag: core.NewTag(0x0040, 0xA730), VR: core.VRSQ}, Value: core.SequenceValue{Items: make([]core.DataSet, MaxNormalizedDataSetElements+1)}}}, nil)
			} else {
				ds := core.DataSet{Elements: information.Elements()}
				for i := 0; i < MaxNormalizedDataSetDepth+1; i++ {
					ds = core.DataSet{Elements: []core.Element{{Header: core.ElementHeader{Tag: core.NewTag(0x0040, 0xA730), VR: core.VRSQ}, Value: core.SequenceValue{Items: []core.DataSet{ds}}}}}
				}
				information = object.FromDataSet(ds, nil)
			}
			peer, local := testPipeAssociations(t, eventTestContexts())
			ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
			defer cancel()
			done := make(chan error, 1)
			go func() {
				_, err := receiveTestOriginal(ctx, peer)
				if err == nil {
					err = sendMessageDataSet(ctx, peer, 3, eventTestRequest(10, 1).CommandSet(), information, transfer.ExplicitVRBigEndian)
				}
				if err == nil {
					_, err = receiveCommandSetWithContext(ctx, peer, 3)
				}
				if err == nil {
					err = fmt.Errorf("test: excessive graph acknowledged")
				} else if errors.Is(err, context.DeadlineExceeded) {
					err = fmt.Errorf("test: deadline instead of graph bound: %w", err)
				} else {
					err = nil
				}
				done <- err
			}()
			called := false
			_, err := invokeEventTestOperation(ctx, NewNormalizedClient(local), "N-ACTION", NormalizedOperationOptions{EventReports: eventTestOptions(func(context.Context, NormalizedEventReportRequest, *object.Object) (NormalizedEventReportSCPResult, error) {
				called = true
				return NormalizedEventReportSCPResult{}, nil
			})})
			if err == nil || !errors.Is(err, ErrAssociationStateUncertain) || called {
				t.Fatalf("graph bound: called=%v err=%v", called, err)
			}
			if err := <-done; err != nil {
				t.Fatal(err)
			}
		})
	}
}

func TestNormalizedClientInterleavedEventsKeepOneResponseDeadline(t *testing.T) {
	peer, local := testPipeAssociations(t, eventTestContexts())
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	done := make(chan error, 1)
	go func() {
		_, err := receiveTestOriginal(ctx, peer)
		if err == nil {
			err = sendTestEvent(ctx, peer, 3, eventTestRequest(10, 1), eventTestInformation(), StatusSuccess, false)
		}
		if err == nil {
			err = sendTestEvent(ctx, peer, 3, eventTestRequest(11, 2), eventTestInformation(), StatusSuccess, false)
		}
		if err == nil {
			_, err = peer.Receive(ctx)
		}
		var abortErr *ul.AbortError
		if !errors.As(err, &abortErr) {
			err = fmt.Errorf("test: expected deadline abort, got %v", err)
		} else {
			err = nil
		}
		done <- err
	}()
	var deadlines []time.Time
	_, err := invokeEventTestOperation(ctx, NewNormalizedClient(local), "N-ACTION", NormalizedOperationOptions{OperationOptions: OperationOptions{ResponseTimeout: 100 * time.Millisecond}, EventReports: eventTestOptions(func(c context.Context, _ NormalizedEventReportRequest, _ *object.Object) (NormalizedEventReportSCPResult, error) {
		deadline, _ := c.Deadline()
		deadlines = append(deadlines, deadline)
		return NormalizedEventReportSCPResult{}, nil
	})})
	if !errors.Is(err, ErrOperationTimeout) || len(deadlines) != 2 || !deadlines[0].Equal(deadlines[1]) {
		t.Fatalf("response deadline reset: deadlines=%v err=%v", deadlines, err)
	}
	if err := <-done; err != nil {
		t.Fatal(err)
	}
}

func TestNormalizedClientInterleavedEventFailureDoesNotReplaceOriginalFailure(t *testing.T) {
	peer, local := testPipeAssociations(t, eventTestContexts())
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	done := make(chan error, 1)
	go func() {
		command, err := receiveTestOriginal(ctx, peer)
		if err == nil {
			err = sendTestEvent(ctx, peer, 3, eventTestRequest(10, 1), eventTestInformation(), StatusProcessingFailure, false)
		}
		if err == nil {
			id, _ := CommandUint16(command, MessageID)
			actionID := uint16(1)
			err = sendMessageDataSet(ctx, peer, 1, (NormalizedActionResponse{MessageIDBeingRespondedTo: id, CommandDataSetType: DataSetPresent, Status: 0x0115, ActionTypeIDOrNil: &actionID, StatusFields: &NormalizedStatusFields{ErrorComment: "synthetic original failure"}}).CommandSet(), eventTestInformation(), transfer.ImplicitVRLittleEndian)
		}
		done <- err
	}()
	result, err := NewNormalizedClient(local).ActionWithOptions(NormalizedOperationOptions{OperationOptions: OperationOptions{Context: ctx}, EventReports: eventTestOptions(func(context.Context, NormalizedEventReportRequest, *object.Object) (NormalizedEventReportSCPResult, error) {
		return NormalizedEventReportSCPResult{}, errors.New("synthetic event failure")
	})}, NormalizedActionRequest{RequestedSOPClassUID: operationTestClass, RequestedSOPInstanceUID: "1.2.3.4", ActionTypeID: 1, CommandDataSetType: NoDataSet}, nil)
	var statusErr *NormalizedStatusError
	if !errors.As(err, &statusErr) || statusErr.Status != 0x0115 || statusErr.Fields.ErrorComment != "synthetic original failure" || errors.Is(err, ErrAssociationStateUncertain) || result.Response == nil || result.DataSet == nil || len(result.EventReports) != 1 || result.EventReports[0].Err == nil || result.EventReports[0].Status != StatusProcessingFailure {
		t.Fatalf("original failure overwritten: result=%#v err=%v", result, err)
	}
	if err := <-done; err != nil {
		t.Fatal(err)
	}
}

func TestInterleavedEventReplyRejectsCyclicAndEmptyItemGraphs(t *testing.T) {
	cycle := core.DataSet{Elements: make([]core.Element, 1)}
	root := core.Element{Header: core.ElementHeader{Tag: core.NewTag(0x0040, 0xA730), VR: core.VRSQ}, Value: core.SequenceValue{Items: []core.DataSet{cycle}}}
	// Construct while acyclic, then simulate a handler mutating its borrowed
	// nested value. Object construction itself traverses deferred values.
	reply := object.FromElements([]core.Element{root}, nil)
	cycle.Elements[0] = root
	if _, err := encodeInterleavedEventReply(context.Background(), reply, transfer.ExplicitVRLittleEndian, 1024); err == nil {
		t.Fatal("cyclic reply was not rejected before serialization")
	}
	emptyItems := object.FromElements([]core.Element{{Header: core.ElementHeader{Tag: core.NewTag(0x0040, 0xA730), VR: core.VRSQ}, Value: core.SequenceValue{Items: make([]core.DataSet, MaxNormalizedDataSetElements+1)}}}, nil)
	if err := validateInterleavedEventGraph(context.Background(), emptyItems); err == nil {
		t.Fatal("empty items escaped node budget")
	}
}

func TestNormalizedClientInterleavedDefaultLeaveOpenPolicy(t *testing.T) {
	peer, local := testPipeAssociations(t, eventTestContexts())
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	done := make(chan error, 1)
	go func() {
		_, err := receiveTestOriginal(ctx, peer)
		if err == nil {
			request := eventTestRequest(10, 1)
			request.CommandDataSetType = NoDataSet
			err = SendCommandSetWithContext(ctx, peer, 3, request.CommandSet())
		}
		if err == nil {
			_, err = peer.Receive(ctx)
		}
		var abort *ul.AbortError
		if !errors.As(err, &abort) {
			err = fmt.Errorf("test: expected caller abort, got %v", err)
		} else {
			err = nil
		}
		done <- err
	}()
	_, err := invokeEventTestOperation(ctx, NewNormalizedClient(local), "N-ACTION", NormalizedOperationOptions{OperationOptions: OperationOptions{ErrorPolicy: OperationErrorPolicyLeaveOpen}})
	if err == nil || !errors.Is(err, ErrAssociationStateUncertain) {
		t.Fatalf("unexpected event err=%v", err)
	}
	// The explicit legacy LeaveOpen policy leaves cleanup to the caller.
	if err := local.Send(ctx, &ul.AbortRQ{Source: 0, Reason: 0}); err != nil {
		t.Fatalf("leave-open association was closed: %v", err)
	}
	if err := <-done; err != nil {
		t.Fatal(err)
	}
}
