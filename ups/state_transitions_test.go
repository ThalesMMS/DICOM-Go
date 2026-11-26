package ups_test

import (
	"context"
	"reflect"
	"slices"
	"testing"

	"github.com/ThalesMMS/dicom-go/core"
	"github.com/ThalesMMS/dicom-go/object"
	"github.com/ThalesMMS/dicom-go/ups"
)

func TestUPSStateTransitionTable(t *testing.T) {
	const uid = "2.25.48301"
	const lock = "2.25.48302"
	ctx := context.Background()
	states := []struct {
		name  string
		state ups.State
	}{
		{"missing", ""},
		{"scheduled", ups.StateScheduled},
		{"in-progress", ups.StateInProgress},
		{"completed", ups.StateCompleted},
		{"canceled", ups.StateCanceled},
	}
	change := func(target ups.State, transactionUID string) func(*testing.T, *ups.Service) error {
		return func(t *testing.T, service *ups.Service) error {
			t.Helper()
			_, err := service.ChangeState(ctx, ups.ChangeStateRequest{SOPInstanceUID: uid, State: target, TransactionUID: transactionUID})
			return err
		}
	}
	create := func(t *testing.T, service *ups.Service) error {
		t.Helper()
		_, err := service.Create(ctx, ups.CreateRequest{SOPInstanceUID: uid, Attributes: scheduledAttributes(t)})
		return err
	}
	requestCancel := func(t *testing.T, service *ups.Service) error {
		t.Helper()
		_, err := service.RequestCancel(ctx, ups.CancelRequest{SOPInstanceUID: uid, RequestingAETitle: "REQUESTOR"})
		return err
	}
	// DICOM PS3.4 CC.1.1-2. The columns follow the initial states above.
	// Use the wire codes directly so a changed status constant cannot alter the oracle.
	operations := []struct {
		name   string
		run    func(*testing.T, *ups.Service) error
		status [5]uint16
		state  ups.State
	}{
		{"create", create, [5]uint16{0, 0x0111, 0x0111, 0x0111, 0x0111}, ups.StateScheduled},
		{"claim", change(ups.StateInProgress, lock), [5]uint16{0xC307, 0, 0xC302, 0xC300, 0xC300}, ups.StateInProgress},
		{"claim-without-lock", change(ups.StateInProgress, ""), [5]uint16{0xC307, 0xC301, 0xC301, 0xC301, 0xC301}, ""},
		{"reschedule", change(ups.StateScheduled, lock), [5]uint16{0xC307, 0xC303, 0xC303, 0xC303, 0xC303}, ""},
		{"complete", change(ups.StateCompleted, lock), [5]uint16{0xC307, 0xC310, 0, 0xB306, 0xC300}, ups.StateCompleted},
		{"complete-without-lock", change(ups.StateCompleted, ""), [5]uint16{0xC307, 0xC301, 0xC301, 0xC301, 0xC301}, ""},
		{"request-cancel", requestCancel, [5]uint16{0xC307, 0, 0, 0xC311, 0xB304}, ups.StateCanceled},
		{"cancel", change(ups.StateCanceled, lock), [5]uint16{0xC307, 0xC310, 0, 0xC300, 0xB304}, ups.StateCanceled},
		{"cancel-without-lock", change(ups.StateCanceled, ""), [5]uint16{0xC307, 0xC301, 0xC301, 0xC301, 0xC301}, ""},
	}
	for column, initial := range states {
		t.Run(initial.name, func(t *testing.T) {
			for _, op := range operations {
				t.Run(op.name, func(t *testing.T) {
					service := newTestService(t)
					seedUPSState(t, service, uid, lock, initial.state)
					before, beforeErr := service.Get(ctx, uid)
					eventsBefore, err := service.Events(ctx, ups.EventQuery{SOPInstanceUID: uid})
					if err != nil {
						t.Fatal(err)
					}

					status := op.status[column]
					err = op.run(t, service)
					if (status == 0 && err != nil) || (status != 0 && !ups.IsStatus(err, status)) {
						t.Fatalf("operation status = %v, want %04X", err, status)
					}
					after, afterErr := service.Get(ctx, uid)
					eventsAfter, err := service.Events(ctx, ups.EventQuery{SOPInstanceUID: uid})
					if err != nil {
						t.Fatal(err)
					}
					if status != 0 {
						if (beforeErr == nil && afterErr != nil) || (beforeErr != nil && !ups.IsStatus(afterErr, 0xC307)) ||
							!reflect.DeepEqual(before, after) || !reflect.DeepEqual(eventsBefore, eventsAfter) {
							t.Fatal("refused or repeated request modified the step or event history")
						}
						return
					}

					if len(eventsAfter) < len(eventsBefore) || !slices.EqualFunc(eventsBefore, eventsAfter[:len(eventsBefore)], func(a, b ups.Event) bool {
						return reflect.DeepEqual(a, b)
					}) {
						t.Fatal("operation rewrote existing events")
					}
					wantState := op.state
					wantReports := []ups.State{wantState}
					if op.name == "request-cancel" {
						wantReports = []ups.State{ups.StateInProgress, ups.StateCanceled}
						if initial.state == ups.StateInProgress {
							wantState = ups.StateInProgress
							wantReports = nil
						}
					}
					if afterErr != nil || after.State != wantState {
						t.Fatalf("stored state = %q, %v, want %q", after.State, afterErr, wantState)
					}
					var reports []ups.State
					cancelRequested := false
					for _, event := range eventsAfter[len(eventsBefore):] {
						info := object.FromDataSet(event.Information, nil)
						if _, present := info.Get(ups.TagTransactionUID); present {
							t.Fatal("event exposed the Transaction UID")
						}
						switch event.Type {
						case ups.EventStateReport:
							state, _ := info.GetString(ups.TagProcedureStepState)
							reports = append(reports, ups.State(state))
						case ups.EventCancelRequested:
							ae, _ := info.GetString(ups.TagRequestingAE)
							cancelRequested = ae == "REQUESTOR"
						}
					}
					if !slices.Equal(reports, wantReports) {
						t.Fatalf("state reports = %v, want %v", reports, wantReports)
					}
					if op.name == "request-cancel" && initial.state == ups.StateInProgress && !cancelRequested {
						t.Fatal("in-progress cancellation request did not notify the performer")
					}
				})
			}
		})
	}
}

func TestUPSFinalTransitionRefusesIncompletePerformedAttributes(t *testing.T) {
	const uid = "2.25.48601"
	const lock = "2.25.48602"
	ctx := context.Background()
	for _, tc := range []struct {
		name    string
		target  ups.State
		missing core.Tag
	}{
		{"performed-procedure", ups.StateCompleted, ups.TagUnifiedProcedureStepPerformedProcedureSequence},
		{"performed-station", ups.StateCompleted, ups.TagPerformedStationNameCodeSequence},
		{"performed-workitem", ups.StateCompleted, ups.TagPerformedWorkitemCodeSequence},
		{"start-date-time", ups.StateCompleted, ups.TagPerformedProcedureStepStartDateTime},
		{"end-date-time", ups.StateCompleted, ups.TagPerformedProcedureStepEndDateTime},
		{"discontinuation-reason", ups.StateCanceled, ups.TagProcedureStepDiscontinuationReasonCodeSequence},
	} {
		t.Run(tc.name, func(t *testing.T) {
			service := newTestService(t)
			seedUPSState(t, service, uid, lock, ups.StateInProgress)
			full := completedAttributes()
			sequenceTag := ups.TagUnifiedProcedureStepPerformedProcedureSequence
			if tc.target == ups.StateCanceled {
				var err error
				full, err = ups.BuildDiscontinuationProgress(ups.Code{Value: "STOP", Scheme: "99SYNTH", Meaning: "Synthetic stop"}, "20260102121212+0000")
				if err != nil {
					t.Fatal(err)
				}
				sequenceTag = ups.TagProcedureStepProgressInformationSequence
			}
			partial := ups.EmptySequence(sequenceTag)
			if tc.missing != sequenceTag {
				items, _ := full.GetSequence(sequenceTag)
				items[0].Remove(tc.missing)
				partial.Value = core.SequenceValue{Items: []core.DataSet{items[0].ToDataSet()}}
			}
			before, err := service.Set(ctx, ups.SetRequest{SOPInstanceUID: uid, TransactionUID: lock, Modifications: ups.NewDataSet(partial)})
			if err != nil {
				t.Fatal(err)
			}
			eventsBefore, err := service.Events(ctx, ups.EventQuery{SOPInstanceUID: uid})
			if err != nil {
				t.Fatal(err)
			}
			request := ups.ChangeStateRequest{SOPInstanceUID: uid, TransactionUID: lock, State: tc.target}
			if _, err := service.ChangeState(ctx, request); !ups.IsStatus(err, 0xC304) {
				t.Fatalf("incomplete final state = %v, want C304", err)
			}
			after, err := service.Get(ctx, uid)
			if err != nil || !reflect.DeepEqual(after, before) {
				t.Fatalf("refusal changed the stored step, read error %v", err)
			}
			eventsAfter, err := service.Events(ctx, ups.EventQuery{SOPInstanceUID: uid})
			if err != nil || !reflect.DeepEqual(eventsAfter, eventsBefore) {
				t.Fatalf("refusal changed event history, read error %v", err)
			}
			if _, err := service.Set(ctx, ups.SetRequest{SOPInstanceUID: uid, TransactionUID: lock, Modifications: full}); err != nil {
				t.Fatal(err)
			}
			if step, err := service.ChangeState(ctx, request); err != nil || step.State != tc.target {
				t.Fatalf("corrected transition = %q, %v, want %q", step.State, err, tc.target)
			}
		})
	}
}

func seedUPSState(t *testing.T, service *ups.Service, uid, lock string, state ups.State) {
	t.Helper()
	ctx := context.Background()
	if state == "" {
		return
	}
	if _, err := service.Create(ctx, ups.CreateRequest{SOPInstanceUID: uid, Attributes: scheduledAttributes(t)}); err != nil {
		t.Fatal(err)
	}
	if state == ups.StateScheduled {
		return
	}
	if _, err := service.ChangeState(ctx, ups.ChangeStateRequest{SOPInstanceUID: uid, TransactionUID: lock, State: ups.StateInProgress}); err != nil {
		t.Fatal(err)
	}
	if state == ups.StateInProgress || state == ups.StateCompleted {
		// The matrix's successful final transitions require valid final attributes.
		if _, err := service.Set(ctx, ups.SetRequest{SOPInstanceUID: uid, TransactionUID: lock, Modifications: completedAttributes()}); err != nil {
			t.Fatal(err)
		}
	}
	if state == ups.StateInProgress || state == ups.StateCanceled {
		reason, err := ups.BuildDiscontinuationProgress(ups.Code{Value: "STOP", Scheme: "99SYNTH", Meaning: "Synthetic stop"}, "20260102121212+0000")
		if err != nil {
			t.Fatal(err)
		}
		if _, err := service.Set(ctx, ups.SetRequest{SOPInstanceUID: uid, TransactionUID: lock, Modifications: reason}); err != nil {
			t.Fatal(err)
		}
	}
	if state != ups.StateInProgress {
		if _, err := service.ChangeState(ctx, ups.ChangeStateRequest{SOPInstanceUID: uid, TransactionUID: lock, State: state}); err != nil {
			t.Fatal(err)
		}
	}
}
