package dicomweb

import (
	"context"
	"net/http"
	"net/http/httptest"
	"net/url"
	"sync"
	"testing"
)

// The request targets follow PS3.18 chapter 11: a comma separates the values
// of one matching key, and the requester is an optional query parameter of
// the state and cancellation transactions.
func TestWorkitemClientRequestTargets(t *testing.T) {
	var mu sync.Mutex
	var targets []string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		targets = append(targets, r.Method+" "+r.URL.RequestURI())
		mu.Unlock()
		switch {
		case r.Method == http.MethodGet:
			w.WriteHeader(http.StatusNoContent)
		case r.Method == http.MethodPost:
			w.WriteHeader(http.StatusAccepted)
		}
	}))
	defer server.Close()
	ctx := context.Background()
	const uid = "1.2.826.0.1.3680043.10.543.7200"
	anonymous := Client{Endpoint: Endpoint{BaseURL: server.URL}}
	named := anonymous
	named.Options.WorkitemRequester = "VIEWER ONE"
	if _, err := anonymous.SearchWorkitems(ctx, url.Values{"00741000": {"SCHEDULED,IN PROGRESS"}}); err != nil {
		t.Fatal(err)
	}
	for _, client := range []Client{anonymous, named} {
		if _, err := client.ChangeWorkitemState(ctx, uid, "IN PROGRESS", "1.2.826.0.1.3680043.10.543.7201"); err != nil {
			t.Fatal(err)
		}
		if _, err := client.RequestWorkitemCancellation(ctx, uid, nil); err != nil {
			t.Fatal(err)
		}
	}
	invalid := anonymous
	invalid.Options.WorkitemRequester = "SEVENTEEN_CHARS_X"
	if _, err := invalid.RequestWorkitemCancellation(ctx, uid, nil); err != nil {
		t.Fatal(err)
	}
	want := []string{
		"GET /workitems?00741000=SCHEDULED,IN+PROGRESS",
		"PUT /workitems/" + uid + "/state",
		"POST /workitems/" + uid + "/cancelrequest",
		"PUT /workitems/" + uid + "/state?requester=VIEWER+ONE",
		"POST /workitems/" + uid + "/cancelrequest?requester=VIEWER+ONE",
		// An invalid AE Title is never sent.
		"POST /workitems/" + uid + "/cancelrequest",
	}
	mu.Lock()
	defer mu.Unlock()
	if len(targets) != len(want) {
		t.Fatalf("requests = %q", targets)
	}
	for index := range want {
		if targets[index] != want[index] {
			t.Errorf("request %d = %q, want %q", index, targets[index], want[index])
		}
	}
}
