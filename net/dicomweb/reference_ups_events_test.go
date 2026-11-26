package dicomweb

import (
	"context"
	"encoding/json"
	"net/http/httptest"
	"reflect"
	"testing"
	"time"
)

func TestReferenceUPSEventsFanOutAndRemainScopedByAETitle(t *testing.T) {
	const primary = "READER A"
	const secondary = "READER B"
	hub := NewWorkitemEventHub(WorkitemEventHubOptions{
		MaxConnections: 3, MaxConnectionsPerAETitle: 2, PingInterval: -1,
	})
	t.Cleanup(func() { _ = hub.Close() })
	server, err := NewServer(ServerOptions{
		Workitems: &fakeWorkitemBackend{}, WorkitemEvents: hub, AllowUnauthenticated: true,
		SpoolDirectory: t.TempDir(),
	})
	if err != nil {
		t.Fatal(err)
	}
	host := httptest.NewServer(server)
	t.Cleanup(host.Close)
	client := Client{Endpoint: Endpoint{BaseURL: host.URL}, Options: Options{HTTPClient: host.Client()}}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	open := func(ae string) *WorkitemEventChannel {
		t.Helper()
		channel, err := client.OpenWorkitemEventChannel(ctx, ae)
		if err != nil {
			t.Fatalf("open channel %s: %v", ae, err)
		}
		t.Cleanup(func() { _ = channel.Close() })
		return channel
	}
	first, second, other := open(primary), open(primary), open(secondary)
	waitFor(t, func() bool { return hub.Connections(primary) == 2 && hub.Connections(secondary) == 1 })

	publish := func(t *testing.T, ae string, event WorkitemEvent, remaining int) {
		t.Helper()
		delivered, err := hub.Publish(ctx, ae, event)
		if err != nil || delivered != remaining {
			t.Fatalf("Publish to %s = %d, %v, want %d deliveries", ae, delivered, err, remaining)
		}
	}
	check := func(t *testing.T, channel *WorkitemEventChannel, want WorkitemEvent) {
		t.Helper()
		event, err := channel.Next(ctx)
		if err != nil || event.SOPInstanceUID != want.SOPInstanceUID || event.EventTypeID != want.EventTypeID {
			t.Fatalf("event = %v, %v, want UID %s type %d", event, err, want.SOPInstanceUID, want.EventTypeID)
		}
		if !reflect.DeepEqual(event.Information, want.Information) {
			t.Fatalf("event information = %v, want %v", event.Information, want.Information)
		}
	}
	newEvent := func(t *testing.T, uid string, eventType uint16, body string) WorkitemEvent {
		t.Helper()
		event := WorkitemEvent{SOPInstanceUID: uid, EventTypeID: eventType}
		if err := json.Unmarshal([]byte(body), &event.Information); err != nil {
			t.Fatalf("invalid event fixture: %v", err)
		}
		return event
	}
	for _, test := range []struct {
		name, uid, body string
		typeID          uint16
	}{
		{"state report", "2.25.7281", `{"00741000":{"vr":"CS","Value":["IN PROGRESS"]},"00404041":{"vr":"CS","Value":["READY"]}}`, 1},
		{"cancel requested", "2.25.7282", `{"00741236":{"vr":"AE","Value":["REQUESTOR"]}}`, 2},
		{"progress report", "2.25.7283", `{"00741002":{"vr":"SQ","Value":[{"00741004":{"vr":"DS","Value":[53.5]}}]}}`, 3},
		{"SCP status change", GlobalWorkitemSubscriptionUID, `{"00741242":{"vr":"CS","Value":["RESTARTED"]},
			"00741244":{"vr":"CS","Value":["WARM START"]},"00741246":{"vr":"CS","Value":["COLD START"]}}`, 4},
		{"assignment", "2.25.7285", `{"00404025":{"vr":"SQ","Value":[{"00080100":{"vr":"SH","Value":["ROOM2"]},
			"00080102":{"vr":"SH","Value":["99TEST"]},"00080104":{"vr":"LO","Value":["Room 2"]}}]}}`, 5},
	} {
		t.Run(test.name, func(t *testing.T) {
			event := newEvent(t, test.uid, test.typeID, test.body)
			publish(t, primary, event, 2)
			check(t, first, event)
			check(t, second, event)
		})
	}
	marker := newEvent(t, "2.25.7286", 1, `{"00741000":{"vr":"CS","Value":["COMPLETED"]},"00404041":{"vr":"CS","Value":["READY"]}}`)
	publish(t, secondary, marker, 1)
	check(t, other, marker)
	if err := first.Close(); err != nil {
		t.Fatal(err)
	}
	waitFor(t, func() bool { return hub.Connections(primary) == 1 })
	newlyConnected := open(primary)
	waitFor(t, func() bool { return hub.Connections(primary) == 2 })
	marker.SOPInstanceUID = "2.25.7287"
	publish(t, primary, marker, 2)
	check(t, second, marker)
	check(t, newlyConnected, marker)
}
