package ups_test

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/ThalesMMS/dicom-go/core"
	"github.com/ThalesMMS/dicom-go/dicomjson"
	"github.com/ThalesMMS/dicom-go/net/dicomweb"
	"github.com/ThalesMMS/dicom-go/ups"
)

type upsRSFixture struct {
	service *ups.Service
	hub     *dicomweb.WorkitemEventHub
	server  *httptest.Server
	client  dicomweb.Client
}

func newUPSRSFixture(t *testing.T, mutate func(*ups.ServiceOptions)) *upsRSFixture {
	t.Helper()
	fixture := &upsRSFixture{}
	fixture.hub = dicomweb.NewWorkitemEventHub(dicomweb.WorkitemEventHubOptions{
		OnConnect: func(string) { fixture.service.WakeDeliveries() },
	})
	store, err := ups.NewMemoryStore(ups.MemoryStoreOptions{})
	if err != nil {
		t.Fatal(err)
	}
	options := ups.ServiceOptions{
		EventSink: ups.NewDICOMwebEventSink(fixture.hub),
		DeliveryLimits: ups.DeliveryLimits{
			InitialBackoff: 10 * time.Millisecond, MaxBackoff: 50 * time.Millisecond, MaxAttempts: 100,
		},
	}
	if mutate != nil {
		mutate(&options)
	}
	fixture.service, err = ups.NewService(store, options)
	if err != nil {
		t.Fatal(err)
	}
	backend, err := ups.NewDICOMwebBackend(fixture.service, ups.DICOMwebOptions{RequestingAETitle: "REST_CANCEL"})
	if err != nil {
		t.Fatal(err)
	}
	server, err := dicomweb.NewServer(dicomweb.ServerOptions{
		Workitems: backend, WorkitemEvents: fixture.hub, AllowUnauthenticated: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	fixture.server = httptest.NewServer(server)
	ctx, cancel := context.WithCancel(context.Background())
	var deliveries sync.WaitGroup
	deliveries.Add(1)
	go func() {
		defer deliveries.Done()
		_ = fixture.service.RunDeliveries(ctx, 20*time.Millisecond)
	}()
	t.Cleanup(func() {
		cancel()
		deliveries.Wait()
		_ = server.Shutdown(context.Background())
		fixture.server.Close()
	})
	fixture.client = dicomweb.Client{Endpoint: dicomweb.Endpoint{BaseURL: fixture.server.URL}}
	return fixture
}

// nextEvent waits for an event of eventType for uid that satisfies match,
// skipping others (for example progress reports emitted by an update).
func nextEvent(t *testing.T, channel *dicomweb.WorkitemEventChannel, uid string, eventType uint16, match func(dicomweb.Dataset) bool) dicomweb.WorkitemEvent {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	for {
		event, err := channel.Next(ctx)
		if err != nil {
			t.Fatalf("waiting for event type %d: %v", eventType, err)
		}
		if event.SOPInstanceUID == uid && event.EventTypeID == eventType && (match == nil || match(event.Information)) {
			return event
		}
	}
}

func stateIs(state string) func(dicomweb.Dataset) bool {
	return func(dataset dicomweb.Dataset) bool {
		return dicomjson.ElementString(dataset, "00741000") == state
	}
}

func TestUPSRSWorkitemLifecycleNotifiesWebSocketSubscriber(t *testing.T) {
	fixture := newUPSRSFixture(t, nil)
	ctx := context.Background()
	client := fixture.client
	const uid = "1.2.826.0.1.3680043.10.543.7001"
	const aeTitle = "VIEWER SCU"

	channel, err := client.OpenWorkitemEventChannel(ctx, aeTitle)
	if err != nil {
		t.Fatalf("OpenWorkitemEventChannel: %v", err)
	}
	defer channel.Close()

	created, err := client.CreateWorkitem(ctx, uid, scheduledAttributes(t))
	if err != nil {
		t.Fatalf("CreateWorkitem: %v", err)
	}
	if created.Response.StatusCode != http.StatusCreated || !strings.HasSuffix(created.Location, "/workitems/"+uid) {
		t.Fatalf("create = %d %q", created.Response.StatusCode, created.Location)
	}
	if _, err := client.CreateWorkitem(ctx, uid, scheduledAttributes(t)); !hasStatus(err, http.StatusConflict) {
		t.Fatalf("duplicate create = %v", err)
	}

	workitem, err := client.RetrieveWorkitem(ctx, uid)
	if err != nil {
		t.Fatalf("RetrieveWorkitem: %v", err)
	}
	if dicomjson.ElementString(workitem, "00741000") != "SCHEDULED" || dicomjson.ElementString(workitem, "00080018") != uid {
		t.Fatalf("retrieved workitem = %v", workitem)
	}
	if _, present := workitem["00081195"]; present {
		t.Fatal("retrieve returned the Transaction UID")
	}
	if _, err := client.RetrieveWorkitem(ctx, "1.2.826.0.1.3680043.10.543.7999"); !hasStatus(err, http.StatusNotFound) {
		t.Fatalf("retrieve unknown = %v", err)
	}

	if _, err := client.SubscribeWorkitem(ctx, uid, aeTitle, dicomweb.WorkitemSubscribeOptions{}); err != nil {
		t.Fatalf("SubscribeWorkitem: %v", err)
	}
	nextEvent(t, channel, uid, 1, stateIs("SCHEDULED"))

	matches, err := client.SearchWorkitems(ctx, url.Values{"ProcedureStepState": {"SCHEDULED"}, "ProcedureStepLabel": {"VER*"}})
	if err != nil || len(matches) != 1 || dicomjson.ElementString(matches[0], "00080018") != uid {
		t.Fatalf("search = %v, %v", matches, err)
	}
	if _, present := matches[0]["00081195"]; present {
		t.Fatal("search returned the Transaction UID")
	}
	if none, err := client.SearchWorkitems(ctx, url.Values{"ProcedureStepState": {"COMPLETED"}}); err != nil || len(none) != 0 {
		t.Fatalf("non-matching search = %v, %v", none, err)
	}

	const transactionUID = "1.2.826.0.1.3680043.10.543.7100"
	if _, err := client.ChangeWorkitemState(ctx, uid, "IN PROGRESS", transactionUID); err != nil {
		t.Fatalf("claim: %v", err)
	}
	nextEvent(t, channel, uid, 1, stateIs("IN PROGRESS"))
	if _, err := client.ChangeWorkitemState(ctx, uid, "IN PROGRESS", "1.2.826.0.1.3680043.10.543.7101"); !hasStatus(err, http.StatusConflict) {
		t.Fatalf("second claim = %v", err)
	}

	if _, err := client.UpdateWorkitem(ctx, uid, "", progressAttributes("40")); !hasStatus(err, http.StatusConflict) {
		t.Fatalf("update without transaction = %v", err)
	}
	if _, err := client.UpdateWorkitem(ctx, uid, transactionUID, progressAttributes("40")); err != nil {
		t.Fatalf("UpdateWorkitem: %v", err)
	}
	nextEvent(t, channel, uid, 3, nil)

	cancelInformation := ups.NewDataSet(ups.StringElement(ups.TagReasonForCancellation, core.VRLT, "Patient left"))
	if response, err := client.RequestWorkitemCancellation(ctx, uid, cancelInformation); err != nil || response.Response.StatusCode != http.StatusAccepted {
		t.Fatalf("RequestWorkitemCancellation: %v", err)
	}
	cancelEvent := nextEvent(t, channel, uid, 2, nil)
	if dicomjson.ElementString(cancelEvent.Information, "00741236") != "REST_CANCEL" ||
		dicomjson.ElementString(cancelEvent.Information, "00741238") != "Patient left" {
		t.Fatalf("cancel requested event = %v", cancelEvent.Information)
	}

	if _, err := client.UpdateWorkitem(ctx, uid, transactionUID, completedAttributes()); err != nil {
		t.Fatalf("final update: %v", err)
	}
	if _, err := client.ChangeWorkitemState(ctx, uid, "COMPLETED", transactionUID); err != nil {
		t.Fatalf("complete: %v", err)
	}
	nextEvent(t, channel, uid, 1, stateIs("COMPLETED"))

	repeat, err := client.ChangeWorkitemState(ctx, uid, "COMPLETED", transactionUID)
	if err != nil || len(repeat.Warnings) != 1 || !strings.Contains(repeat.Warnings[0], "COMPLETED") {
		t.Fatalf("repeated completion = %+v, %v", repeat.Warnings, err)
	}
	if _, err := client.UpdateWorkitem(ctx, uid, transactionUID, progressAttributes("90")); !hasStatus(err, http.StatusConflict) {
		t.Fatalf("update after completion = %v", err)
	}
	if _, err := client.UnsubscribeWorkitem(ctx, uid, aeTitle); err != nil {
		t.Fatalf("UnsubscribeWorkitem: %v", err)
	}
}

func TestUPSRSGlobalSubscriptionQueuesEventsUntilTheChannelOpens(t *testing.T) {
	fixture := newUPSRSFixture(t, func(options *ups.ServiceOptions) { options.RefuseDeletionLocks = true })
	ctx := context.Background()
	const aeTitle = "WATCHER"

	response, err := fixture.client.SubscribeWorkitem(ctx, dicomweb.GlobalWorkitemSubscriptionUID, aeTitle,
		dicomweb.WorkitemSubscribeOptions{DeletionLock: true})
	if err != nil {
		t.Fatalf("global subscribe: %v", err)
	}
	if len(response.Warnings) != 1 || !strings.Contains(response.Warnings[0], "Deletion lock") {
		t.Fatalf("refused deletion lock warnings = %q", response.Warnings)
	}
	const uid = "1.2.826.0.1.3680043.10.543.7201"
	if _, err := fixture.client.CreateWorkitem(ctx, uid, scheduledAttributes(t)); err != nil {
		t.Fatal(err)
	}
	// The event is committed while no channel is open; it must still arrive.
	time.Sleep(50 * time.Millisecond)
	channel, err := fixture.client.OpenWorkitemEventChannel(ctx, aeTitle)
	if err != nil {
		t.Fatal(err)
	}
	defer channel.Close()
	nextEvent(t, channel, uid, 1, stateIs("SCHEDULED"))

	if _, err := fixture.client.SuspendWorkitemSubscription(ctx, aeTitle); err != nil {
		t.Fatalf("suspend: %v", err)
	}
	if _, err := fixture.client.SubscribeWorkitem(ctx, dicomweb.FilteredGlobalWorkitemSubscriptionUID, aeTitle,
		dicomweb.WorkitemSubscribeOptions{Filter: []dicomweb.SearchFilter{{Key: "WorklistLabel", Values: []string{"RADIOLOGY"}}}}); err != nil {
		t.Fatalf("filtered subscribe: %v", err)
	}
	if _, err := fixture.client.SubscribeWorkitem(ctx, dicomweb.FilteredGlobalWorkitemSubscriptionUID, aeTitle,
		dicomweb.WorkitemSubscribeOptions{Filter: []dicomweb.SearchFilter{{Key: "PatientWeight", Values: []string{"70"}}}}); !hasStatus(err, http.StatusBadRequest) {
		t.Fatalf("filter on a non-query attribute = %v", err)
	}
}

func TestUPSRSEventSinkFallsBackWhenNoChannelIsOpen(t *testing.T) {
	hub := dicomweb.NewWorkitemEventHub(dicomweb.WorkitemEventHubOptions{})
	defer hub.Close()
	sink := ups.NewDICOMwebEventSink(hub)
	handled, err := sink.DeliverUPSEvent(context.Background(), ups.Delivery{ReceivingAETitle: "NOBODY", SOPInstanceUID: "1.2.3", EventType: ups.EventStateReport})
	if handled || err != nil {
		t.Fatalf("DeliverUPSEvent without a channel = %t, %v", handled, err)
	}
}

func hasStatus(err error, status int) bool {
	var webErr *dicomweb.Error
	return errors.As(err, &webErr) && webErr.StatusCode == status
}
