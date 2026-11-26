package dimse

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/ThalesMMS/dicom-go/dictionary/std"
	"github.com/ThalesMMS/dicom-go/net/ul"
	"github.com/ThalesMMS/dicom-go/object"
	"github.com/ThalesMMS/dicom-go/transfer"
)

func TestCMoveCancelCountsPreserveAvailableTerminalResult(t *testing.T) {
	for _, mode := range []string{"legacy", "batch"} {
		for _, outcome := range []string{"before-result", "success", "warning", "failure", "canceled-result"} {
			t.Run(mode+"/"+outcome, func(t *testing.T) {
				ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
				defer cancel()
				peer, local := testPipeAssociations(t, []ul.AcceptedContext{{ID: 1, AbstractSyntaxUID: StudyRootMoveSOPClassUID, TransferSyntaxUID: ul.ImplicitVRLittleEndian}})
				ready := make(chan struct{})
				result := CMoveSubOperationResult{Status: StatusSuccess}
				var completed, failed, warning uint16
				switch outcome {
				case "success":
					completed = 1
				case "warning":
					result.Status, warning = 0xB007, 1
				case "failure":
					result.Status, failed = StatusCStoreOutOfResources, 1
				case "canceled-result":
					result.Err = context.Canceled
				}
				stores := 0
				store := func(operationCtx context.Context) CMoveSubOperationResult {
					stores++
					// A terminal result is already available before cancellation;
					// only its publication is delayed by this test barrier.
					close(ready)
					<-operationCtx.Done()
					if outcome == "before-result" {
						return CMoveSubOperationResult{Err: operationCtx.Err()}
					}
					return result
				}
				legacy := CMoveHandlerFunc(func(context.Context, CMoveRequestContext) ([]CMoveSubOperation, error) {
					return []CMoveSubOperation{
						{AffectedSOPClassUID: "1.2.840.10008.5.1.4.1.1.2", AffectedSOPInstanceUID: "1.2.3.1", Store: store},
						{AffectedSOPClassUID: "1.2.840.10008.5.1.4.1.1.2", AffectedSOPInstanceUID: "1.2.3.2", Store: func(context.Context) CMoveSubOperationResult {
							stores++
							return CMoveSubOperationResult{Status: StatusSuccess}
						}},
					}, nil
				})
				var handler CMoveHandler = legacy
				if mode == "batch" {
					handler = cMoveBatchTestHandler{prepare: func(context.Context, CMoveRequestContext) (CMoveBatch, error) {
						return CMoveBatch{
							SubOperations: []CMoveSubOperationDescriptor{
								{AffectedSOPInstanceUID: "1.2.3.1"}, {AffectedSOPInstanceUID: "1.2.3.2"},
							},
							Run: func(operationCtx context.Context, yield func(CMoveSubOperationResult) error) error {
								terminal := store(operationCtx)
								if outcome == "before-result" {
									return terminal.Err
								}
								stop := yield(terminal)
								if !errors.Is(stop, ErrCMoveCanceled) && !errors.Is(stop, context.Canceled) {
									return fmt.Errorf("yield stop = %v, want cancellation", stop)
								}
								// A misbehaving duplicate publication must not count twice.
								if second := yield(terminal); second != stop {
									return fmt.Errorf("second yield = %v, want same stop %v", second, stop)
								}
								return stop
							},
						}, nil
					}}
				}
				done := make(chan error, 1)
				go func() { done <- ServeStudyRootCMove(ctx, local, 1, handler) }()
				identifier, err := BuildStudyRootStudyFindKeys(nil)
				if err != nil {
					t.Fatal(err)
				}
				request := CMoveRequest{AffectedSOPClassUID: StudyRootMoveSOPClassUID, MessageID: 81, MoveDestination: "STOREAE"}
				if err := SendCMoveRequest(peer, 1, request); err != nil {
					t.Fatal(err)
				}
				if err := SendDataSet(peer, 1, object.FromElements(identifier, std.Dictionary), transfer.ImplicitVRLittleEndian); err != nil {
					t.Fatal(err)
				}
				select {
				case <-ready:
				case <-ctx.Done():
					t.Fatal("handler barrier not reached")
				}
				if err := SendCCancelRequest(peer, 1, CCancelRequest{MessageIDBeingRespondedTo: request.MessageID}); err != nil {
					t.Fatal(err)
				}
				command, err := ReceiveCommandSet(peer, 1)
				if err != nil {
					t.Fatal(err)
				}
				response, err := ParseCMoveResponse(command)
				if err != nil {
					t.Fatal(err)
				}
				if command.Has(NumberOfRemainingSuboperations) || response.NumberOfRemainingSuboperationsOrNil != nil {
					t.Fatal("Cancel has Remaining; opaque handler does not expose uninitiated count")
				}
				for _, field := range []struct {
					tag     string
					present bool
				}{
					{"completed", command.Has(NumberOfCompletedSuboperations)},
					{"failed", command.Has(NumberOfFailedSuboperations)},
					{"warning", command.Has(NumberOfWarningSuboperations)},
				} {
					if !field.present {
						t.Fatalf("Cancel lacks required %s count", field.tag)
					}
				}
				assertCMoveCounts(t, "cancel", response, StatusCMoveCancel, 0, completed, failed, warning)
				dataSetType, err := CommandUint16(command, CommandDataSetType)
				if err != nil {
					t.Fatal(err)
				}
				if dataSetType != NoDataSet {
					identifier, err := ReceiveDataSet(peer, 1, transfer.ImplicitVRLittleEndian)
					if err != nil {
						t.Fatal(err)
					}
					if uid, _ := identifier.GetString(FailedSOPInstanceUIDList); failed != 1 || uid != "1.2.3.1" {
						t.Fatalf("failed list=%q with failed=%d", uid, failed)
					}
				}
				if err := <-done; !errors.Is(err, ErrCMoveCanceled) && !errors.Is(err, context.Canceled) {
					t.Fatalf("server error=%v, want cancel", err)
				}
				if stores != 1 {
					t.Fatalf("Store invocations=%d, want1 and no next invocation", stores)
				}
			})
		}
	}
}

func TestCMoveCallerCancelResponsePreservesExplicitRemaining(t *testing.T) {
	remaining := uint16(7)
	command := object.FromElements(CMoveResponse{Status: StatusCMoveCancel, NumberOfRemainingSuboperationsOrNil: &remaining}.CommandSet(), nil)
	if got, err := CommandUint16(command, NumberOfRemainingSuboperations); err != nil || got != remaining {
		t.Fatalf("caller-owned Cancel Remaining=%d,%v, want7", got, err)
	}
}

func TestCMoveBatchDeadlineResultIsNotRemoteFailure(t *testing.T) {
	operationCtx, cancel := context.WithCancelCause(context.Background())
	cancel(context.DeadlineExceeded)
	monitor := &scpCancelMonitor{operationCtx: operationCtx, cancelErr: ErrCMoveCanceled}
	counts, err := runCMoveBatch(operationCtx, context.Background(), nil, 1, CMoveRequest{}, StudyRootMoveSOPClassUID, monitor, CMoveBatch{
		SubOperations: []CMoveSubOperationDescriptor{{AffectedSOPInstanceUID: "1.2.3.1"}},
		Run: func(_ context.Context, yield func(CMoveSubOperationResult) error) error {
			return yield(CMoveSubOperationResult{Err: context.DeadlineExceeded})
		},
	})
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("error=%v want deadline", err)
	}
	if counts.remaining != 1 || counts.completed != 0 || counts.failed != 0 || counts.warning != 0 || len(counts.failedSOPInstanceUIDs) != 0 {
		t.Fatalf("interrupted exchange fabricated a remote result: %+v", counts)
	}
}
