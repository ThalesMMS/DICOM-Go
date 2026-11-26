package dicomweb

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/url"
	"reflect"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/ThalesMMS/dicom-go/dicomjson"
)

type fakeWorkitemBackend struct {
	mu       sync.Mutex
	created  []WorkitemCreateRequest
	states   []WorkitemStateRequest
	subs     []WorkitemSubscriptionRequest
	stateErr error
}

func (b *fakeWorkitemBackend) CreateWorkitem(_ context.Context, request WorkitemCreateRequest) (WorkitemCreated, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.created = append(b.created, request)
	uid := request.SOPInstanceUID
	if uid == "" {
		uid = "1.2.3.999"
	}
	return WorkitemCreated{SOPInstanceUID: uid}, nil
}

func (b *fakeWorkitemBackend) RetrieveWorkitem(_ context.Context, uid string) (Dataset, error) {
	if uid != "1.2.3.4" {
		return nil, ErrNotFound
	}
	return Dataset{"00741000": dicomjson.Element{VR: "CS", Value: []any{"SCHEDULED"}}}, nil
}

func (b *fakeWorkitemBackend) UpdateWorkitem(context.Context, WorkitemUpdateRequest) (WorkitemResult, error) {
	return WorkitemResult{}, errors.New("backend detail with PHI")
}

func (b *fakeWorkitemBackend) ChangeWorkitemState(_ context.Context, request WorkitemStateRequest) (WorkitemResult, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.states = append(b.states, request)
	return WorkitemResult{Warnings: []string{"already\r\nCOMPLETED"}}, b.stateErr
}

func (b *fakeWorkitemBackend) RequestWorkitemCancellation(context.Context, WorkitemCancellationRequest) (WorkitemResult, error) {
	return WorkitemResult{}, ErrConflict
}

func (b *fakeWorkitemBackend) SearchWorkitems(context.Context, SearchRequest, func(Dataset) error) (SearchResult, error) {
	return SearchResult{}, nil
}

func (b *fakeWorkitemBackend) SubscribeWorkitem(_ context.Context, request WorkitemSubscriptionRequest) (WorkitemResult, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.subs = append(b.subs, request)
	return WorkitemResult{}, nil
}

func (b *fakeWorkitemBackend) UnsubscribeWorkitem(context.Context, WorkitemSubscriptionRequest) (WorkitemResult, error) {
	return WorkitemResult{}, ErrForbidden
}

func (b *fakeWorkitemBackend) SuspendWorkitemSubscription(context.Context, WorkitemSubscriptionRequest) (WorkitemResult, error) {
	return WorkitemResult{}, ErrResourceLimit
}

const minimalWorkitemJSON = `[{"00741000":{"vr":"CS","Value":["SCHEDULED"]}}]`

func doWorkitemRequest(t *testing.T, method, target, contentType, body string, header http.Header) *http.Response {
	t.Helper()
	request, err := http.NewRequest(method, target, strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	if contentType != "" {
		request.Header.Set("Content-Type", contentType)
	}
	for key, values := range header {
		request.Header[key] = values
	}
	response, err := http.DefaultClient.Do(request)
	if err != nil {
		t.Fatal(err)
	}
	response.Body.Close()
	return response
}

func TestWorkitemRoutesStatusMapping(t *testing.T) {
	backend := &fakeWorkitemBackend{}
	var operations []Operation
	var opsMu sync.Mutex
	server, err := NewServer(ServerOptions{
		Workitems: backend,
		Authorize: func(_ context.Context, _ *http.Request, operation Operation) error {
			opsMu.Lock()
			operations = append(operations, operation)
			opsMu.Unlock()
			return nil
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	httpServer := httptest.NewServer(server)
	defer httpServer.Close()
	base := httpServer.URL
	const dicomJSON = "application/dicom+json"

	tests := []struct {
		name        string
		method      string
		path        string
		contentType string
		body        string
		status      int
	}{
		{"create with query UID", http.MethodPost, "/workitems?workitem=1.2.3.4", dicomJSON, minimalWorkitemJSON, http.StatusCreated},
		{"create with bare UID", http.MethodPost, "/workitems?1.2.3.5", dicomJSON, minimalWorkitemJSON, http.StatusCreated},
		{"create assigned UID", http.MethodPost, "/workitems", dicomJSON + "; charset=utf-8", `{"00741000":{"vr":"CS","Value":["SCHEDULED"]}}`, http.StatusCreated},
		{"create UID mismatch", http.MethodPost, "/workitems?workitem=1.2.3.4", dicomJSON, `[{"00080018":{"vr":"UI","Value":["1.2.3.6"]}}]`, http.StatusConflict},
		{"create wrong media type", http.MethodPost, "/workitems", "application/json", minimalWorkitemJSON, http.StatusUnsupportedMediaType},
		{"create two datasets", http.MethodPost, "/workitems", dicomJSON, `[{},{}]`, http.StatusBadRequest},
		{"create bulk data reference", http.MethodPost, "/workitems", dicomJSON, `[{"00420011":{"vr":"OB","BulkDataURI":"http://elsewhere/x"}}]`, http.StatusBadRequest},
		{"create empty body", http.MethodPost, "/workitems", dicomJSON, ``, http.StatusBadRequest},
		{"create unknown parameter", http.MethodPost, "/workitems?debug=1", dicomJSON, minimalWorkitemJSON, http.StatusBadRequest},
		{"retrieve", http.MethodGet, "/workitems/1.2.3.4", "", "", http.StatusOK},
		{"retrieve unknown", http.MethodGet, "/workitems/1.2.3.7", "", "", http.StatusNotFound},
		{"retrieve invalid UID", http.MethodGet, "/workitems/not-a-uid", "", "", http.StatusBadRequest},
		{"update redacts backend errors", http.MethodPost, "/workitems/1.2.3.4?transaction=1.2.3.8", dicomJSON, minimalWorkitemJSON, http.StatusInternalServerError},
		{"state", http.MethodPut, "/workitems/1.2.3.4/state", dicomJSON, `[{"00741000":{"vr":"CS","Value":["IN PROGRESS"]},"00081195":{"vr":"UI","Value":["1.2.3.9"]}}]`, http.StatusOK},
		{"state without transaction", http.MethodPut, "/workitems/1.2.3.4/state", dicomJSON, minimalWorkitemJSON, http.StatusBadRequest},
		{"state with requester", http.MethodPut, "/workitems/1.2.3.4/state?requester=VIEWER%20ONE", dicomJSON, `[{"00741000":{"vr":"CS","Value":["IN PROGRESS"]},"00081195":{"vr":"UI","Value":["1.2.3.9"]}}]`, http.StatusOK},
		{"state with invalid requester", http.MethodPut, "/workitems/1.2.3.4/state?requester=SEVENTEEN_CHARS_X", dicomJSON, `[{"00741000":{"vr":"CS","Value":["IN PROGRESS"]},"00081195":{"vr":"UI","Value":["1.2.3.9"]}}]`, http.StatusBadRequest},
		{"state with unknown parameter", http.MethodPut, "/workitems/1.2.3.4/state?debug=1", dicomJSON, `[{"00741000":{"vr":"CS","Value":["IN PROGRESS"]},"00081195":{"vr":"UI","Value":["1.2.3.9"]}}]`, http.StatusBadRequest},
		{"cancel conflict", http.MethodPost, "/workitems/1.2.3.4/cancelrequest", "", "", http.StatusConflict},
		{"cancel with requester", http.MethodPost, "/workitems/1.2.3.4/cancelrequest?requester=VIEWER", "", "", http.StatusConflict},
		{"cancel with invalid requester", http.MethodPost, "/workitems/1.2.3.4/cancelrequest?requester=", "", "", http.StatusBadRequest},
		{"search", http.MethodGet, "/workitems?PatientID=X", "", "", http.StatusOK},
		{"subscribe", http.MethodPost, "/workitems/1.2.3.4/subscribers/VIEWER%20ONE?deletionlock=true", "", "", http.StatusCreated},
		{"subscribe invalid AE", http.MethodPost, "/workitems/1.2.3.4/subscribers/SEVENTEEN_CHARS_X", "", "", http.StatusBadRequest},
		{"filter needs filtered global UID", http.MethodPost, "/workitems/1.2.3.4/subscribers/AE?filter=PatientID%3DX", "", "", http.StatusBadRequest},
		{"filtered subscribe", http.MethodPost, "/workitems/1.2.840.10008.5.1.4.34.5.1/subscribers/AE?filter=PatientID%3DX", "", "", http.StatusCreated},
		{"filtered subscribe with a PS3.18 list", http.MethodPost, "/workitems/1.2.840.10008.5.1.4.34.5.1/subscribers/AE?filter=00100020%3DX,00741202%3DCARD%2A", "", "", http.StatusCreated},
		{"filtered subscribe with repeated filters", http.MethodPost, "/workitems/1.2.840.10008.5.1.4.34.5.1/subscribers/AE?filter=PatientID%3DX&filter=WorklistLabel%3DY", "", "", http.StatusCreated},
		// An encoded comma belongs to the value; only literal commas separate pairs.
		{"filtered subscribe with an encoded comma in a value", http.MethodPost, "/workitems/1.2.840.10008.5.1.4.34.5.1/subscribers/AE?filter=00741202%3DCT%2C%20MR&filter=00741204%3DHead%2Cneck,00100020%3DX", "", "", http.StatusCreated},
		{"filter list with an empty pair", http.MethodPost, "/workitems/1.2.840.10008.5.1.4.34.5.1/subscribers/AE?filter=PatientID%3DX,", "", "", http.StatusBadRequest},
		{"filter list with a bare value", http.MethodPost, "/workitems/1.2.840.10008.5.1.4.34.5.1/subscribers/AE?filter=PatientID%3DX,Y", "", "", http.StatusBadRequest},
		{"unsubscribe forbidden", http.MethodDelete, "/workitems/1.2.3.4/subscribers/AE", "", "", http.StatusForbidden},
		{"suspend needs global UID", http.MethodPost, "/workitems/1.2.3.4/subscribers/AE/suspend", "", "", http.StatusBadRequest},
		{"suspend resource limit", http.MethodPost, "/workitems/1.2.840.10008.5.1.4.34.5/subscribers/AE/suspend", "", "", http.StatusServiceUnavailable},
		{"method not allowed", http.MethodPatch, "/workitems/1.2.3.4", "", "", http.StatusMethodNotAllowed},
		{"event channel without hub", http.MethodGet, "/subscribers/AE", "", "", http.StatusNotImplemented},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			response := doWorkitemRequest(t, test.method, base+test.path, test.contentType, test.body, nil)
			if response.StatusCode != test.status {
				t.Fatalf("status = %d, want %d", response.StatusCode, test.status)
			}
			if test.status == http.StatusCreated && strings.HasPrefix(test.path, "/workitems?") || test.path == "/workitems" && test.status == http.StatusCreated {
				if !strings.HasPrefix(response.Header.Get("Location"), "/workitems/1.2.3.") {
					t.Fatalf("Location = %q", response.Header.Get("Location"))
				}
			}
			if test.name == "state" {
				warning := response.Header.Get("Warning")
				if !strings.HasPrefix(warning, "299 ") || strings.ContainsAny(warning, "\r\n") || !strings.Contains(warning, "alreadyCOMPLETED") {
					t.Fatalf("Warning = %q", warning)
				}
			}
			if test.method == http.MethodPatch && response.Header.Get("Allow") != "GET, POST" {
				t.Fatalf("Allow = %q", response.Header.Get("Allow"))
			}
		})
	}

	backend.mu.Lock()
	defer backend.mu.Unlock()
	if len(backend.created) != 3 || backend.created[0].SOPInstanceUID != "1.2.3.4" || backend.created[1].SOPInstanceUID != "1.2.3.5" || backend.created[2].SOPInstanceUID != "" {
		t.Fatalf("created = %+v", backend.created)
	}
	if len(backend.states) != 2 || backend.states[0].State != "IN PROGRESS" || backend.states[0].TransactionUID != "1.2.3.9" {
		t.Fatalf("state requests = %+v", backend.states)
	}
	if len(backend.subs) != 5 || backend.subs[0].AETitle != "VIEWER ONE" || !backend.subs[0].DeletionLock ||
		len(backend.subs[1].Filter) != 1 || backend.subs[1].Filter[0].Key != "PatientID" {
		t.Fatalf("subscriptions = %+v", backend.subs)
	}
	for index, want := range [][]SearchFilter{
		{{Key: "00100020", Values: []string{"X"}}, {Key: "00741202", Values: []string{"CARD*"}}},
		{{Key: "PatientID", Values: []string{"X"}}, {Key: "WorklistLabel", Values: []string{"Y"}}},
		{{Key: "00741202", Values: []string{"CT, MR"}}, {Key: "00741204", Values: []string{"Head,neck"}}, {Key: "00100020", Values: []string{"X"}}},
	} {
		if got := backend.subs[index+2].Filter; !reflect.DeepEqual(got, want) {
			t.Fatalf("filter of subscription %d = %+v, want %+v", index+2, got, want)
		}
	}
	opsMu.Lock()
	defer opsMu.Unlock()
	for _, want := range []Operation{OperationCreateWorkitem, OperationRetrieveWorkitem, OperationChangeWorkitemState,
		OperationRequestWorkitemCancellation, OperationSearchWorkitems, OperationSubscribeWorkitem,
		OperationUnsubscribeWorkitem, OperationSuspendWorkitemSubscription, OperationOpenEventChannel} {
		found := false
		for _, operation := range operations {
			found = found || operation == want
		}
		if !found {
			t.Errorf("Authorize never saw %s", want)
		}
	}
}

func TestEventChannelHandshakeLimitsAndShutdown(t *testing.T) {
	hub := NewWorkitemEventHub(WorkitemEventHubOptions{MaxConnectionsPerAETitle: 1, PingInterval: 20 * time.Millisecond})
	var denied atomic.Bool
	server, err := NewServer(ServerOptions{
		Workitems:      &fakeWorkitemBackend{},
		WorkitemEvents: hub,
		Authorize: func(_ context.Context, r *http.Request, operation Operation) error {
			if operation == OperationOpenEventChannel && r.Header.Get("Authorization") == "" {
				denied.Store(true)
				return ErrUnauthorized
			}
			return nil
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	httpServer := httptest.NewServer(server)
	defer httpServer.Close()
	client := Client{Endpoint: Endpoint{BaseURL: httpServer.URL}, Options: Options{BearerToken: "token"}}

	if _, err := (Client{Endpoint: client.Endpoint}).OpenWorkitemEventChannel(context.Background(), "VIEWER ONE"); !hasHTTPStatus(err, http.StatusUnauthorized) || !denied.Load() {
		t.Fatalf("unauthenticated channel = %v", err)
	}
	if response := doWorkitemRequest(t, http.MethodGet, httpServer.URL+"/subscribers/AE", "", "", http.Header{"Authorization": {"Bearer token"}}); response.StatusCode != http.StatusUpgradeRequired {
		t.Fatalf("plain GET status = %d", response.StatusCode)
	}
	crossSite := http.Header{
		"Authorization": {"Bearer token"}, "Connection": {"Upgrade"}, "Upgrade": {"websocket"},
		"Sec-Websocket-Version": {"13"}, "Sec-Websocket-Key": {"dGhlIHNhbXBsZSBub25jZQ=="}, "Origin": {"https://evil.example"},
	}
	if response := doWorkitemRequest(t, http.MethodGet, httpServer.URL+"/subscribers/AE", "", "", crossSite); response.StatusCode != http.StatusForbidden {
		t.Fatalf("cross-site channel status = %d", response.StatusCode)
	}

	channel, err := client.OpenWorkitemEventChannel(context.Background(), "VIEWER ONE")
	if err != nil {
		t.Fatalf("OpenWorkitemEventChannel: %v", err)
	}
	if _, err := client.OpenWorkitemEventChannel(context.Background(), "VIEWER ONE"); !hasHTTPStatus(err, http.StatusServiceUnavailable) {
		t.Fatalf("second channel for the AE = %v", err)
	}
	waitFor(t, func() bool { return hub.Connections("VIEWER ONE") == 1 })
	information := Dataset{"00741000": dicomjson.Element{VR: "CS", Value: []any{"IN PROGRESS"}}}
	sent, err := hub.Publish(context.Background(), "VIEWER ONE", WorkitemEvent{SOPInstanceUID: "1.2.3.4", EventTypeID: 1, Information: information})
	if err != nil || sent != 1 {
		t.Fatalf("Publish = %d, %v", sent, err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	event, err := channel.Next(ctx)
	if err != nil || event.SOPInstanceUID != "1.2.3.4" || event.EventTypeID != 1 ||
		dicomjson.ElementString(event.Information, "00741000") != "IN PROGRESS" {
		t.Fatalf("event = %+v, %v", event, err)
	}
	if _, present := event.Information["00001000"]; present {
		t.Fatal("command elements leaked into the event information")
	}
	if sent, err := hub.Publish(context.Background(), "OTHER", WorkitemEvent{SOPInstanceUID: "1.2.3.4", EventTypeID: 1}); sent != 0 || err != nil {
		t.Fatalf("Publish without channel = %d, %v", sent, err)
	}
	time.Sleep(60 * time.Millisecond) // pings must not disturb the channel

	if err := server.Shutdown(context.Background()); err != nil {
		t.Fatal(err)
	}
	if _, err := channel.Next(ctx); !errors.Is(err, ErrEventChannelClosed) {
		t.Fatalf("Next after shutdown = %v", err)
	}
	if _, err := hub.Publish(context.Background(), "VIEWER ONE", WorkitemEvent{SOPInstanceUID: "1.2.3.4", EventTypeID: 1}); !errors.Is(err, ErrEventChannelClosed) {
		t.Fatalf("Publish after close = %v", err)
	}
}

func TestWorkitemEventEncoding(t *testing.T) {
	message, err := EncodeWorkitemEvent(WorkitemEvent{SOPInstanceUID: "1.2.3", EventTypeID: 5, Information: Dataset{}}, 7)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{`"00000002":{"vr":"UI","Value":["1.2.840.10008.5.1.4.34.6.4"]}`, `"00000110":{"vr":"US","Value":[7]}`, `"00001002":{"vr":"US","Value":[5]}`} {
		if !strings.Contains(string(message), want) {
			t.Fatalf("message %s lacks %s", message, want)
		}
	}
	for _, input := range []string{"[" + string(message) + "]", string(message)} {
		event, err := DecodeWorkitemEvent([]byte(input))
		if err != nil || event.SOPInstanceUID != "1.2.3" || event.EventTypeID != 5 {
			t.Fatalf("DecodeWorkitemEvent(%s) = %+v, %v", input, event, err)
		}
	}
	for _, bad := range []string{`{}`, `[{},{}]`, `{"00001000":{"vr":"UI","Value":["1.2"]},"00001002":{"vr":"US","Value":[70000]}}`} {
		if _, err := DecodeWorkitemEvent([]byte(bad)); err == nil {
			t.Errorf("DecodeWorkitemEvent(%s) accepted", bad)
		}
	}
	if _, err := EncodeWorkitemEvent(WorkitemEvent{EventTypeID: 1}, 1); err == nil {
		t.Fatal("event without UID encoded")
	}
}

func TestWorkitemClientURLs(t *testing.T) {
	endpoint := Endpoint{BaseURL: "https://pacs.example/dicomweb", UPSPath: "ups"}
	channel, err := endpoint.EventChannelURL("VIEWER ONE")
	if err != nil || channel.String() != "https://pacs.example/dicomweb/ups/subscribers/VIEWER%20ONE" {
		t.Fatalf("EventChannelURL = %v, %v", channel, err)
	}
	state, err := endpoint.WorkitemURL("1.2.3", url.Values{"transaction": {"4.5"}}, "state")
	if err != nil || state.String() != "https://pacs.example/dicomweb/ups/workitems/1.2.3/state?transaction=4.5" {
		t.Fatalf("WorkitemURL = %v, %v", state, err)
	}
	if _, err := endpoint.EventChannelURL(`BAD\AE`); err == nil {
		t.Fatal("invalid AE accepted")
	}
	if !ValidAETitle("A") || ValidAETitle("") || ValidAETitle("    ") || ValidAETitle(strings.Repeat("A", 17)) {
		t.Fatal("ValidAETitle")
	}
}

func hasHTTPStatus(err error, status int) bool {
	var webErr *Error
	return errors.As(err, &webErr) && webErr.StatusCode == status
}

func waitFor(t *testing.T, condition func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for !condition() {
		if time.Now().After(deadline) {
			t.Fatal("condition not reached")
		}
		time.Sleep(5 * time.Millisecond)
	}
}
