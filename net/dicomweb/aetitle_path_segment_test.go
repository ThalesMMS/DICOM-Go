package dicomweb

import (
	"context"
	"net/http"
	"net/http/httptest"
	"net/url"
	"sync"
	"testing"
)

// An AE Title may contain "/" and spaces (PS3.5). The client escapes it as
// one path segment and the server routes it back to the same title.
func TestWorkitemSubscriberAETitleStaysOnePathSegment(t *testing.T) {
	const ae = "CT/1 SCP"
	endpoint := Endpoint{BaseURL: "https://example.test/dicomweb"}
	channel, err := endpoint.EventChannelURL(ae)
	if err != nil {
		t.Fatal(err)
	}
	if got, want := channel.String(), "https://example.test/dicomweb/subscribers/CT%2F1%20SCP"; got != want {
		t.Fatalf("event channel URL = %q, want %q", got, want)
	}

	var mu sync.Mutex
	var targets []string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		targets = append(targets, r.URL.EscapedPath())
		mu.Unlock()
		parts := requestResourceParts(r.URL)
		if len(parts) != 4 || parts[2] != "subscribers" || parts[3] != ae {
			t.Errorf("server parts = %q, want the AE Title as the fourth segment", parts)
		}
		if operation := workitemRouteOperation(r.Method, parts); operation == OperationUnknown {
			t.Errorf("%s %s did not route to a workitem operation", r.Method, r.URL.EscapedPath())
		}
		w.WriteHeader(http.StatusOK)
	}))
	defer server.Close()
	client := Client{Endpoint: Endpoint{BaseURL: server.URL}}
	if _, err := client.UnsubscribeWorkitem(context.Background(), "1.2.3", ae); err != nil {
		t.Fatal(err)
	}
	mu.Lock()
	defer mu.Unlock()
	if len(targets) != 1 || targets[0] != "/workitems/1.2.3/subscribers/"+url.PathEscape(ae) {
		t.Fatalf("request paths = %q", targets)
	}
}
