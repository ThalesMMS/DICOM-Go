package ups_test

import (
	"context"
	"errors"
	"testing"

	"github.com/ThalesMMS/dicom-go/object"
	"github.com/ThalesMMS/dicom-go/ups"
)

const subscriptionMatrixAE = "MATRIXWATCH"

func TestUPSSubscriptionTransitionTable(t *testing.T) {
	ctx := context.Background()
	const stepUID = "2.25.48501"
	const laterUID = "2.25.48502"
	const globalUID = ups.GlobalSubscriptionSOPInstanceUID
	const none = ups.SubscriptionNone
	const locked = ups.SubscriptionWithDeletionLock
	const unlocked = ups.SubscriptionWithoutLock
	initialStates := []struct {
		name  string
		state ups.SubscriptionState
	}{{"absent", none}, {"locked", locked}, {"unlocked", unlocked}}

	// DICOM PS3.4 CC.2.3-2. Expectations are for the original instance, the
	// AE's global registration, and the original instance's new deliveries.
	operations := []struct {
		name           string
		seedGlobal     bool
		run            func(*testing.T, *ups.Service)
		instanceStates [3]ups.SubscriptionState
		globalState    ups.SubscriptionState
		initialEvents  [3]int
	}{
		{"global-lock", false, func(t *testing.T, s *ups.Service) { subscribeMatrix(t, s, globalUID, true) },
			[3]ups.SubscriptionState{locked, locked, unlocked}, locked, [3]int{1, 0, 0}},
		{"global-unlocked", false, func(t *testing.T, s *ups.Service) { subscribeMatrix(t, s, globalUID, false) },
			[3]ups.SubscriptionState{unlocked, locked, unlocked}, unlocked, [3]int{0, 0, 0}},
		{"specific-lock", false, func(t *testing.T, s *ups.Service) { subscribeMatrix(t, s, stepUID, true) },
			[3]ups.SubscriptionState{locked, locked, locked}, none, [3]int{1, 1, 1}},
		{"specific-unlocked", false, func(t *testing.T, s *ups.Service) { subscribeMatrix(t, s, stepUID, false) },
			[3]ups.SubscriptionState{unlocked, unlocked, unlocked}, none, [3]int{1, 1, 1}},
		{"specific-unsubscribe", false, func(t *testing.T, s *ups.Service) { unsubscribeMatrix(t, s, stepUID) },
			[3]ups.SubscriptionState{none, none, none}, none, [3]int{0, 0, 0}},
		{"global-unsubscribe", true, func(t *testing.T, s *ups.Service) { unsubscribeMatrix(t, s, globalUID) },
			[3]ups.SubscriptionState{none, none, none}, none, [3]int{0, 0, 0}},
		{"global-suspend", true, func(t *testing.T, s *ups.Service) {
			t.Helper()
			if err := s.SuspendGlobal(ctx, subscriptionMatrixAE); err != nil {
				t.Fatal(err)
			}
		}, [3]ups.SubscriptionState{none, locked, unlocked}, none, [3]int{0, 0, 0}},
	}

	for _, op := range operations {
		t.Run(op.name, func(t *testing.T) {
			for column, initial := range initialStates {
				t.Run(initial.name, func(t *testing.T) {
					service := newSubscriptionMatrixService(t)
					if op.seedGlobal {
						subscribeMatrix(t, service, globalUID, false)
					}
					if _, err := service.Create(ctx, ups.CreateRequest{SOPInstanceUID: stepUID, Attributes: scheduledAttributes(t)}); err != nil {
						t.Fatal(err)
					}
					if initial.state != none {
						subscribeMatrix(t, service, stepUID, initial.state == locked)
					} else if op.seedGlobal {
						unsubscribeMatrix(t, service, stepUID)
					}
					assertUPSSubscription(t, service, stepUID, initial.state)
					before := matrixDeliveries(t, service, stepUID)
					op.run(t, service)
					assertUPSSubscription(t, service, stepUID, op.instanceStates[column])
					assertUPSSubscription(t, service, globalUID, op.globalState)
					assertMatrixInitialEvents(t, matrixDeliveries(t, service, stepUID), len(before), op.initialEvents[column])

					if _, err := service.Create(ctx, ups.CreateRequest{SOPInstanceUID: laterUID, Attributes: scheduledAttributes(t)}); err != nil {
						t.Fatal(err)
					}
					assertUPSSubscription(t, service, laterUID, op.globalState)
					want := 0
					if op.globalState != none {
						want = 1
					}
					assertMatrixInitialEvents(t, matrixDeliveries(t, service, laterUID), 0, want)
				})
			}
		})
	}
}

func newSubscriptionMatrixService(t *testing.T) *ups.Service {
	t.Helper()
	store, err := ups.NewMemoryStore(ups.MemoryStoreOptions{})
	if err != nil {
		t.Fatal(err)
	}
	service, err := ups.NewService(store, ups.ServiceOptions{CallbackResolver: ups.CallbackResolverFunc(func(context.Context, ups.CallbackRequest) (ups.CallbackTarget, error) {
		return ups.CallbackTarget{Address: "127.0.0.1:11112"}, nil
	})})
	if err != nil {
		t.Fatal(err)
	}
	return service
}

func subscribeMatrix(t *testing.T, service *ups.Service, uid string, lock bool) {
	t.Helper()
	result, err := service.Subscribe(context.Background(), ups.SubscribeRequest{SOPInstanceUID: uid, ReceivingAETitle: subscriptionMatrixAE, DeletionLock: lock})
	if err != nil || result.Status != 0 {
		t.Fatalf("subscribe = %04X, %v", result.Status, err)
	}
}

func unsubscribeMatrix(t *testing.T, service *ups.Service, uid string) {
	t.Helper()
	if err := service.Unsubscribe(context.Background(), ups.UnsubscribeRequest{SOPInstanceUID: uid, ReceivingAETitle: subscriptionMatrixAE}); err != nil {
		t.Fatal(err)
	}
}

func assertUPSSubscription(t *testing.T, service *ups.Service, uid string, want ups.SubscriptionState) {
	t.Helper()
	subscription, err := service.Subscription(context.Background(), uid, subscriptionMatrixAE)
	if want == ups.SubscriptionNone {
		if !errors.Is(err, ups.ErrNotFound) {
			t.Fatalf("subscription = %+v, %v, want absent", subscription, err)
		}
		return
	}
	if err != nil || subscription.State != want {
		t.Fatalf("subscription state = %q, %v, want %q", subscription.State, err, want)
	}
}

func matrixDeliveries(t *testing.T, service *ups.Service, uid string) []ups.Delivery {
	t.Helper()
	deliveries, err := service.Deliveries(context.Background(), ups.DeliveryQuery{SOPInstanceUID: uid, ReceivingAETitle: subscriptionMatrixAE})
	if err != nil {
		t.Fatal(err)
	}
	return deliveries
}

func assertMatrixInitialEvents(t *testing.T, deliveries []ups.Delivery, previous, want int) {
	t.Helper()
	if len(deliveries) != previous+want {
		t.Fatalf("delivery count = %d, want %d (%d new)", len(deliveries), previous+want, want)
	}
	for _, delivery := range deliveries[previous:] {
		state, _ := object.FromDataSet(delivery.Event.Information, nil).GetString(ups.TagProcedureStepState)
		if delivery.State != ups.DeliveryPending || delivery.EventType != ups.EventStateReport || state != string(ups.StateScheduled) {
			t.Fatalf("initial delivery = %s/%d/%s, want pending scheduled state report", delivery.State, delivery.EventType, state)
		}
	}
}
