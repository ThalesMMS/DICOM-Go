package ups_test

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/ThalesMMS/dicom-go/net/dimse"
	"github.com/ThalesMMS/dicom-go/net/ul"
	"github.com/ThalesMMS/dicom-go/transfer"
	"github.com/ThalesMMS/dicom-go/ups"
)

func TestUPSRestartStatusAcrossDIMSECallback(t *testing.T) {
	for _, lists := range []struct {
		name                     string
		keepSubscriptions, steps bool
	}{
		{"cold-both", false, false},
		{"warm-subscriptions", true, false},
		{"warm-steps", false, true},
		{"warm-both", true, true},
	} {
		t.Run(lists.name+"/accepted", func(t *testing.T) {
			checkUPSRestartCallback(t, lists.keepSubscriptions, lists.steps, false)
		})
		t.Run(lists.name+"/refused", func(t *testing.T) {
			checkUPSRestartCallback(t, lists.keepSubscriptions, lists.steps, true)
		})
	}
}

func checkUPSRestartCallback(t *testing.T, keepSubscriptions, keepSteps, refused bool) {
	t.Helper()
	const stepUID, transactionUID = "2.25.921801", "2.25.921802"
	const receivingAE, privateComment = "WATCHER", "SYNTHETIC PRIVATE CALLBACK TEXT"
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	store, err := ups.NewMemoryStore(ups.MemoryStoreOptions{})
	if err != nil {
		t.Fatal(err)
	}
	resolver := ups.CallbackResolverFunc(func(context.Context, ups.CallbackRequest) (ups.CallbackTarget, error) {
		return ups.CallbackTarget{Address: "callback.invalid:11112"}, nil
	})
	createTime := time.Date(2025, 6, 8, 7, 30, 0, 0, time.UTC)
	now := createTime
	before, err := ups.NewService(store, ups.ServiceOptions{Clock: func() time.Time { return now }, CallbackResolver: resolver})
	if err != nil {
		t.Fatal(err)
	}
	change := ups.SCPStatusChange{Status: ups.SCPStatusRestarted, SubscriptionListStatus: ups.ListStatusColdStart, UPSListStatus: ups.ListStatusColdStart}
	var claimed ups.Step
	if keepSteps {
		if _, err := before.Create(ctx, ups.CreateRequest{SOPInstanceUID: stepUID, Attributes: scheduledAttributes(t)}); err != nil {
			t.Fatal(err)
		}
		now = now.Add(time.Minute)
		claimed, err = before.ChangeState(ctx, ups.ChangeStateRequest{SOPInstanceUID: stepUID, State: ups.StateInProgress, TransactionUID: transactionUID})
		if err != nil {
			t.Fatal(err)
		}
		change.UPSListStatus = ups.ListStatusWarmStart
	}
	if keepSubscriptions {
		if _, err := before.Subscribe(ctx, ups.SubscribeRequest{SOPInstanceUID: ups.GlobalSubscriptionSOPInstanceUID, ReceivingAETitle: receivingAE}); err != nil {
			t.Fatal(err)
		}
		change.SubscriptionListStatus = ups.ListStatusWarmStart
	}

	peer, local := pipeUPSAssociations(t, []ul.AcceptedContext{{
		ID: 1, AbstractSyntaxUID: ups.EventSOPClassUID, TransferSyntaxUID: transfer.ExplicitVRLittleEndian.UID,
	}})
	type observedEvent struct {
		uid     string
		kind    ups.EventType
		change  ups.SCPStatusChange
		hasLock bool
	}
	observed := make(chan observedEvent, 1)
	receiverStore, err := ups.NewMemoryStore(ups.MemoryStoreOptions{})
	if err != nil {
		t.Fatal(err)
	}
	receiver, err := ups.NewService(receiverStore, ups.ServiceOptions{EventHandler: ups.EventHandlerFunc(func(_ context.Context, event ups.ReceivedEvent) error {
		status, _ := event.Information.GetString(ups.TagSCPStatus)
		subscriptions, _ := event.Information.GetString(ups.TagSubscriptionListStatus)
		steps, _ := event.Information.GetString(ups.TagUnifiedProcedureStepListStatus)
		_, hasLock := event.Information.Get(ups.TagTransactionUID)
		observed <- observedEvent{uid: event.SOPInstanceUID, kind: event.Type, hasLock: hasLock,
			change: ups.SCPStatusChange{Status: ups.SCPStatus(status), SubscriptionListStatus: ups.ListStatus(subscriptions), UPSListStatus: ups.ListStatus(steps)}}
		if refused {
			return errors.New(privateComment)
		}
		return nil
	})})
	if err != nil {
		t.Fatal(err)
	}
	receiverOptions := receiver.NormalizedOptions(local)
	serverDone := make(chan error, 1)
	go func() {
		serverDone <- dimse.ServeAssociation(ctx, local, dimse.AssociationSCPOptions{NormalizedSCP: &receiverOptions})
	}()

	// Recreate the service over the caller-owned repository. The caller
	// declares which lists it has retained across a real process restart.
	now = now.Add(time.Hour)
	dials := 0
	var fallbackAEs []string
	if !keepSubscriptions {
		fallbackAEs = []string{receivingAE}
	}
	restarted, err := ups.NewService(store, ups.ServiceOptions{
		Clock: func() time.Time { return now }, CallbackResolver: resolver, FallbackReceivingAETitles: fallbackAEs,
		AssociationDialer: ups.AssociationDialerFunc(func(_ context.Context, _ string, options ul.DialOptions) (*ul.Association, error) {
			dials++
			if options.CalledAETitle != receivingAE || dials != 1 {
				t.Errorf("unexpected callback attempt %d to %q", dials, options.CalledAETitle)
			}
			return peer, nil
		}),
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := restarted.ReportSCPStatusChange(ctx, change); err != nil {
		t.Fatal(err)
	}
	pending, err := restarted.Deliveries(ctx, ups.DeliveryQuery{})
	if err != nil || len(pending) != 1 || pending[0].State != ups.DeliveryPending || pending[0].Attempts != 0 || dials != 0 {
		t.Fatalf("before callback: deliveries %+v, dials %d, error %v", pending, dials, err)
	}
	err = restarted.DeliverDue(ctx, 1)
	wantState, wantFailure, wantStatus := ups.DeliveryDelivered, ups.DeliveryFailureNone, uint16(0)
	if refused {
		wantState, wantFailure, wantStatus = ups.DeliveryExhausted, ups.DeliveryFailureDIMSEStatus, dimse.StatusProcessingFailure
		var failure *ups.DeliveryError
		if !errors.As(err, &failure) || failure.Class != wantFailure || failure.Status != wantStatus || failure.Retryable || strings.Contains(err.Error(), privateComment) {
			t.Fatalf("callback refusal = %+v, error %v", failure, err)
		}
	} else if err != nil {
		t.Fatal(err)
	}
	select {
	case got := <-observed:
		if got.uid != ups.GlobalSubscriptionSOPInstanceUID || got.kind != ups.EventSCPStatusChange || got.change != change || got.hasLock {
			t.Fatalf("received restart event = %+v, want %+v", got, change)
		}
	default:
		t.Fatal("callback completed without notifying the receiver")
	}
	if err := <-serverDone; refused {
		var abort *ul.AbortError
		if !errors.As(err, &abort) {
			t.Fatalf("refusal cleanup = %v, want A-ABORT", err)
		}
	} else if err != nil {
		t.Fatalf("release after acknowledgement: %v", err)
	}
	final, err := restarted.Deliveries(ctx, ups.DeliveryQuery{})
	if err != nil || len(final) != 1 || final[0].ID != pending[0].ID || final[0].State != wantState || final[0].Attempts != 1 ||
		final[0].LastFailure != wantFailure || final[0].LastDIMSEStatus != wantStatus || final[0].LeaseToken != "" {
		t.Fatalf("after callback: deliveries %+v, error %v", final, err)
	}
	now = now.Add(24 * time.Hour)
	if err := restarted.DeliverDue(ctx, 1); err != nil || dials != 1 {
		t.Fatalf("completed delivery retried: dials %d, error %v", dials, err)
	}
	subscription, err := restarted.Subscription(ctx, ups.GlobalSubscriptionSOPInstanceUID, receivingAE)
	if keepSubscriptions {
		if err != nil || subscription.State != ups.SubscriptionWithoutLock {
			t.Fatalf("warm subscription list lost watcher: subscription %+v, error %v", subscription, err)
		}
	} else if !errors.Is(err, ups.ErrNotFound) {
		t.Fatalf("cold subscription list returned fallback watcher: subscription %+v, error %v", subscription, err)
	}
	if keepSteps {
		if _, err := restarted.Set(ctx, ups.SetRequest{SOPInstanceUID: stepUID, TransactionUID: "2.25.921803", Modifications: progressAttributes("90")}); !ups.IsStatus(err, ups.StatusIncorrectTransactionUID) {
			t.Fatalf("wrong transaction after recreating service = %v", err)
		}
		got, err := restarted.Get(ctx, stepUID)
		if err != nil || got.State != ups.StateInProgress || got.TransactionUID != transactionUID || got.Version != claimed.Version ||
			!got.CreatedAt.Equal(createTime) || !got.UpdatedAt.Equal(claimed.UpdatedAt) {
			t.Fatalf("reload of claimed step = %+v, error %v", got, err)
		}
	} else if _, err := restarted.Get(ctx, stepUID); !ups.IsStatus(err, ups.StatusUPSNotFound) {
		t.Fatalf("cold step list returned pre-restart UID: %v", err)
	}
}
