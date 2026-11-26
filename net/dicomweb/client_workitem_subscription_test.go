package dicomweb

import (
	"context"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func TestSubscribeWorkitemSendsOnePS318FilterList(t *testing.T) {
	var mu sync.Mutex
	var rawQueries []string
	var filters [][]string
	host := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		rawQueries = append(rawQueries, r.URL.RawQuery)
		filters = append(filters, r.URL.Query()["filter"])
		mu.Unlock()
		w.WriteHeader(http.StatusCreated)
	}))
	defer host.Close()
	client := Client{Endpoint: Endpoint{BaseURL: host.URL}}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	_, err := client.SubscribeWorkitem(ctx, FilteredGlobalWorkitemSubscriptionUID, "VIEWER", WorkitemSubscribeOptions{
		DeletionLock: true,
		Filter: []SearchFilter{
			{Key: "00100020", Values: []string{"PAT 1"}},
			{Key: "00741202", Values: []string{"CARD*"}},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := client.SubscribeWorkitem(ctx, GlobalWorkitemSubscriptionUID, "VIEWER", WorkitemSubscribeOptions{}); err != nil {
		t.Fatal(err)
	}
	mu.Lock()
	defer mu.Unlock()
	if want := "deletionlock=true&filter=00100020%3DPAT%201,00741202%3DCARD%2A"; len(rawQueries) != 2 || rawQueries[0] != want {
		t.Fatalf("query = %q, want %q", rawQueries, want)
	}
	if want := []string{"00100020=PAT 1,00741202=CARD*"}; !reflect.DeepEqual(filters[0], want) {
		t.Fatalf("decoded filter parameters = %q, want %q", filters[0], want)
	}
	if rawQueries[1] != "" {
		t.Fatalf("unfiltered subscription query = %q", rawQueries[1])
	}
}

func TestSubscribeWorkitemRefusesAFilterTheListCannotCarry(t *testing.T) {
	var requests atomic.Int32
	host := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests.Add(1)
		w.WriteHeader(http.StatusCreated)
	}))
	defer host.Close()
	client := Client{Endpoint: Endpoint{BaseURL: host.URL}}
	for _, filter := range []SearchFilter{
		{Key: "00100010", Values: []string{"DOE,JOHN"}},
		{Key: "00100020", Values: []string{""}},
		{Key: "0010,0020", Values: []string{"X"}},
		{Key: "", Values: []string{"X"}},
	} {
		if _, err := client.SubscribeWorkitem(context.Background(), FilteredGlobalWorkitemSubscriptionUID, "VIEWER",
			WorkitemSubscribeOptions{Filter: []SearchFilter{filter}}); err == nil {
			t.Fatalf("filter %+v was accepted", filter)
		}
	}
	if requests.Load() != 0 {
		t.Fatalf("%d requests were sent", requests.Load())
	}
}

// TestSubscribedEventChannelURLsTryTheStandardChannelFirst checks that the
// standard channel always comes first and that a Subscribe Location, which
// PS3.18 does not define, is only added as a fallback when it is clearly an
// event channel on the endpoint origin.
func TestSubscribedEventChannelURLsTryTheStandardChannelFirst(t *testing.T) {
	endpoint := Endpoint{BaseURL: "http://archive.test:8080/arc/aets/WORKLIST/rs"}
	request := "http://archive.test:8080/arc/aets/WORKLIST/rs/workitems/1.2.840.10008.5.1.4.34.5/subscribers/VIEWER%20ONE"
	standard := "http://archive.test:8080/arc/aets/WORKLIST/rs/subscribers/VIEWER%20ONE"
	for _, test := range []struct {
		name, location, requestURL, fallback string
	}{
		{"no Location", "", request, ""},
		{"dcm4chee-arc ws channel", "ws://archive.test:8080/arc/aets/WORKLIST/ws/subscribers/VIEWER%20ONE", request, "http://archive.test:8080/arc/aets/WORKLIST/ws/subscribers/VIEWER%20ONE"},
		{"ws channel with fragment", "ws://ARCHIVE.test:8080/ws/VIEWER#x", request, "http://ARCHIVE.test:8080/ws/VIEWER"},
		{"http channel path", "/arc/aets/WORKLIST/ws/subscribers/VIEWER%20ONE", request, "http://archive.test:8080/arc/aets/WORKLIST/ws/subscribers/VIEWER%20ONE"},
		{"the standard channel itself", "/arc/aets/WORKLIST/rs/subscribers/VIEWER%20ONE", request, ""},
		{"the standard channel relative", "../../../subscribers/VIEWER%20ONE", request, ""},
		{"the created subscription", "/arc/aets/WORKLIST/rs/workitems/1.2.840.10008.5.1.4.34.5/subscribers/VIEWER%20ONE", request, ""},
		{"the created subscription absolute", request, request, ""},
		{"the created subscription relative", "VIEWER%20ONE", request, ""},
		{"http path that is not a channel", "http://archive.test:8080/ws/VIEWER", request, ""},
		{"channel of another AE", "/arc/aets/WORKLIST/ws/subscribers/OTHER", request, ""},
		{"relative channel without request URL", "../ws/subscribers/VIEWER%20ONE", "", "http://archive.test:8080/arc/aets/WORKLIST/rs/ws/subscribers/VIEWER%20ONE"},
		{"ws on another host", "ws://elsewhere.test:8080/ws/subscribers/VIEWER%20ONE", request, ""},
		{"ws on another port", "ws://archive.test:9090/ws/subscribers/VIEWER%20ONE", request, ""},
		{"wss on an http endpoint", "wss://archive.test:8080/ws/subscribers/VIEWER%20ONE", request, ""},
		{"ftp", "ftp://archive.test:8080/ws/subscribers/VIEWER%20ONE", request, ""},
		{"credentials", "ws://user:secret@archive.test:8080/ws/subscribers/VIEWER%20ONE", request, ""},
		{"opaque", "mailto:viewer@archive.test", request, ""},
		{"malformed", "://bad", request, ""},
	} {
		t.Run(test.name, func(t *testing.T) {
			got, err := endpoint.SubscribedEventChannelURLs("VIEWER ONE", WorkitemResponse{Location: test.location, Response: Response{URL: test.requestURL}})
			if err != nil {
				t.Fatal(err)
			}
			want := []string{standard}
			if test.fallback != "" {
				want = append(want, test.fallback)
			}
			var gotURLs []string
			for _, u := range got {
				gotURLs = append(gotURLs, u.String())
			}
			if !reflect.DeepEqual(gotURLs, want) {
				t.Fatalf("channels = %q, want %q", gotURLs, want)
			}
		})
	}
	if _, err := endpoint.SubscribedEventChannelURLs("SEVENTEEN_CHARS_X", WorkitemResponse{}); err == nil {
		t.Fatal("invalid AE Title accepted")
	}
}

// channelHost serves the library server under /rs and the same event channel
// under /ws, and lets each test decide what the standard channel, the
// Subscribe Location and the /ws channel do.
type channelHost struct {
	*httptest.Server
	hub             *WorkitemEventHub
	location        func(host string) string
	standardOpen    bool
	announcedStatus int
	standard        atomic.Int32
	announced       atomic.Int32
}

func newChannelHost(t *testing.T, configure func(*channelHost)) *channelHost {
	t.Helper()
	h := &channelHost{hub: NewWorkitemEventHub(WorkitemEventHubOptions{PingInterval: -1})}
	t.Cleanup(func() { _ = h.hub.Close() })
	configure(h)
	backend := &fakeWorkitemBackend{}
	service, err := NewServer(ServerOptions{Workitems: backend, WorkitemEvents: h.hub, AllowUnauthenticated: true, ServiceRoot: "/rs"})
	if err != nil {
		t.Fatal(err)
	}
	channels, err := NewServer(ServerOptions{Workitems: backend, WorkitemEvents: h.hub, AllowUnauthenticated: true, ServiceRoot: "/ws"})
	if err != nil {
		t.Fatal(err)
	}
	h.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasPrefix(r.URL.Path, "/rs/subscribers/"):
			h.standard.Add(1)
			if !h.standardOpen {
				http.NotFound(w, r)
				return
			}
			http.StripPrefix("/rs", service).ServeHTTP(w, r)
		case strings.HasPrefix(r.URL.Path, "/ws/"):
			h.announced.Add(1)
			if h.announcedStatus != 0 {
				http.Error(w, "refused", h.announcedStatus)
				return
			}
			http.StripPrefix("/ws", channels).ServeHTTP(w, r)
		case r.Method == http.MethodPost && strings.Contains(r.URL.Path, "/subscribers/"):
			recorder := httptest.NewRecorder()
			http.StripPrefix("/rs", service).ServeHTTP(recorder, r)
			for key, values := range recorder.Header() {
				w.Header()[key] = values
			}
			if recorder.Code == http.StatusCreated && h.location != nil {
				w.Header().Set("Location", h.location(r.Host))
			}
			w.WriteHeader(recorder.Code)
			_, _ = w.Write(recorder.Body.Bytes())
		default:
			http.StripPrefix("/rs", service).ServeHTTP(w, r)
		}
	}))
	t.Cleanup(h.Server.Close)
	return h
}

func (h *channelHost) subscribe(t *testing.T, ctx context.Context) (Client, WorkitemResponse) {
	t.Helper()
	client := Client{Endpoint: Endpoint{BaseURL: h.URL + "/rs"}}
	response, err := client.SubscribeWorkitem(ctx, GlobalWorkitemSubscriptionUID, "VIEWER ONE", WorkitemSubscribeOptions{})
	if err != nil {
		t.Fatal(err)
	}
	return client, response
}

func (h *channelHost) expectEvent(t *testing.T, ctx context.Context, channel *WorkitemEventChannel) {
	t.Helper()
	waitFor(t, func() bool { return h.hub.Connections("VIEWER ONE") == 1 })
	event := WorkitemEvent{SOPInstanceUID: "2.25.7290", EventTypeID: 1, Information: Dataset{}}
	if delivered, err := h.hub.Publish(ctx, "VIEWER ONE", event); err != nil || delivered != 1 {
		t.Fatalf("publish = %d, %v", delivered, err)
	}
	received, err := channel.Next(ctx)
	if err != nil || received.SOPInstanceUID != event.SOPInstanceUID || received.EventTypeID != 1 {
		t.Fatalf("event = %+v, %v", received, err)
	}
}

// TestOpenSubscribedWorkitemEventChannelFallsBackToTheAnnouncedChannel
// reproduces dcm4chee-arc: no channel at {service}/subscribers, and a ws URL
// of the real channel in the Location of the Subscribe response.
func TestOpenSubscribedWorkitemEventChannelFallsBackToTheAnnouncedChannel(t *testing.T) {
	h := newChannelHost(t, func(h *channelHost) {
		h.location = func(host string) string { return "ws://" + host + "/ws/subscribers/VIEWER%20ONE" }
	})
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	client, response := h.subscribe(t, ctx)
	channel, err := client.OpenSubscribedWorkitemEventChannel(ctx, "VIEWER ONE", response)
	if err != nil {
		t.Fatal(err)
	}
	defer channel.Close()
	if h.standard.Load() != 1 || h.announced.Load() != 1 {
		t.Fatalf("standard attempts %d, announced attempts %d", h.standard.Load(), h.announced.Load())
	}
	h.expectEvent(t, ctx, channel)
}

// TestOpenSubscribedWorkitemEventChannelKeepsTheStandardChannel covers
// Locations that are not the event channel: a standard server naming the
// created subscription, and a path-rewriting proxy that passes on an internal
// channel path the client cannot reach. The standard channel opens and the
// announced URL is never tried.
func TestOpenSubscribedWorkitemEventChannelKeepsTheStandardChannel(t *testing.T) {
	for _, test := range []struct {
		name     string
		location func(host string) string
	}{
		{"created subscription", func(host string) string {
			return "http://" + host + "/rs/workitems/" + GlobalWorkitemSubscriptionUID + "/subscribers/VIEWER%20ONE"
		}},
		{"relative created subscription", func(string) string {
			return "/rs/workitems/" + GlobalWorkitemSubscriptionUID + "/subscribers/VIEWER%20ONE"
		}},
		{"internal channel behind a proxy", func(host string) string { return "ws://" + host + "/ws/subscribers/VIEWER%20ONE" }},
		{"channel on the backend host", func(string) string { return "ws://10.0.0.7:8080/rs/subscribers/VIEWER%20ONE" }},
	} {
		t.Run(test.name, func(t *testing.T) {
			h := newChannelHost(t, func(h *channelHost) {
				h.standardOpen = true
				h.announcedStatus = http.StatusNotFound
				h.location = test.location
			})
			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()
			client, response := h.subscribe(t, ctx)
			channel, err := client.OpenSubscribedWorkitemEventChannel(ctx, "VIEWER ONE", response)
			if err != nil {
				t.Fatal(err)
			}
			defer channel.Close()
			if h.standard.Load() != 1 || h.announced.Load() != 0 {
				t.Fatalf("standard attempts %d, announced attempts %d", h.standard.Load(), h.announced.Load())
			}
			h.expectEvent(t, ctx, channel)
		})
	}
}

func TestOpenSubscribedWorkitemEventChannelReportsTheAnnouncedFailure(t *testing.T) {
	h := newChannelHost(t, func(h *channelHost) {
		h.announcedStatus = http.StatusForbidden
		h.location = func(host string) string { return "ws://" + host + "/ws/subscribers/VIEWER%20ONE" }
	})
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	client, response := h.subscribe(t, ctx)
	channel, err := client.OpenSubscribedWorkitemEventChannel(ctx, "VIEWER ONE", response)
	if err == nil {
		_ = channel.Close()
		t.Fatal("no channel should open")
	}
	if !hasHTTPStatus(err, http.StatusForbidden) || h.standard.Load() != 1 || h.announced.Load() != 1 {
		t.Fatalf("err = %v, standard attempts %d, announced attempts %d", err, h.standard.Load(), h.announced.Load())
	}

	// Without an announced channel the standard failure is reported.
	plain := newChannelHost(t, func(*channelHost) {})
	client, response = plain.subscribe(t, ctx)
	if _, err := client.OpenSubscribedWorkitemEventChannel(ctx, "VIEWER ONE", response); !hasHTTPStatus(err, http.StatusNotFound) {
		t.Fatalf("err = %v", err)
	}
}

// TestLibraryServerSubscribeSendsNoLocation keeps the library server on
// PS3.18, which defines no Location for Subscribe: its channel is the
// standard one.
func TestLibraryServerSubscribeSendsNoLocation(t *testing.T) {
	hub := NewWorkitemEventHub(WorkitemEventHubOptions{PingInterval: -1})
	t.Cleanup(func() { _ = hub.Close() })
	server, err := NewServer(ServerOptions{Workitems: &fakeWorkitemBackend{}, WorkitemEvents: hub, AllowUnauthenticated: true, ServiceRoot: "/dicomweb"})
	if err != nil {
		t.Fatal(err)
	}
	host := httptest.NewServer(http.StripPrefix("/dicomweb", server))
	defer host.Close()
	client := Client{Endpoint: Endpoint{BaseURL: host.URL + "/dicomweb"}}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	response, err := client.SubscribeWorkitem(ctx, FilteredGlobalWorkitemSubscriptionUID, "VIEWER ONE",
		WorkitemSubscribeOptions{Filter: []SearchFilter{{Key: "00100020", Values: []string{"X"}}}})
	if err != nil {
		t.Fatal(err)
	}
	if response.Response.StatusCode != http.StatusCreated || response.Location != "" {
		t.Fatalf("Subscribe answered %d with Location %q", response.Response.StatusCode, response.Location)
	}
	channel, err := client.OpenSubscribedWorkitemEventChannel(ctx, "VIEWER ONE", response)
	if err != nil {
		t.Fatal(err)
	}
	_ = channel.Close()
}
