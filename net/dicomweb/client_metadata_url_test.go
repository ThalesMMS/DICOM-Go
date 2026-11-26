package dicomweb

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
)

func TestStudyMetadataDatasetsWithURLRetainsFinalRetrievalContext(t *testing.T) {
	const (
		initial    = "/studies/" + metadataIdentityStudy + "/metadata"
		final      = "/mounted%2Fslides/metadata?route=private-ticket"
		badStudy   = "2.25.999"
		badInitial = "/studies/" + badStudy + "/metadata"
		badFinal   = "/invalid-private-path/metadata?route=private-ticket"
	)
	var metadataRequests atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.RequestURI() {
		case initial:
			http.Redirect(w, r, final+"#ignored-fragment", http.StatusFound)
		case final:
			metadataRequests.Add(1)
			w.Header().Set("Content-Type", "application/dicom+json")
			_ = json.NewEncoder(w).Encode([]Dataset{metadataIdentityDataset()})
		case badInitial:
			http.Redirect(w, r, badFinal, http.StatusFound)
		case badFinal:
			w.Header().Set("Content-Type", "application/dicom+json")
			_, _ = fmt.Fprint(w, "[malformed")
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()
	client := Client{Endpoint: Endpoint{BaseURL: server.URL}, Options: Options{
		Cache:          NewResponseCache(4096, 0),
		RedirectPolicy: RedirectPolicySameOrigin,
	}}
	ctx := context.Background()
	for range 2 {
		datasets, requestURL, err := client.StudyMetadataDatasetsWithURL(ctx, metadataIdentityStudy)
		if err != nil || len(datasets) != 1 || requestURL != server.URL+final {
			t.Fatalf("metadata datasets=%d request URL=%q error=%v", len(datasets), requestURL, err)
		}
	}
	if stats := client.Options.Cache.Stats(); metadataRequests.Load() != 1 || stats.Hits != 1 {
		t.Fatalf("metadata requests=%d cache=%+v", metadataRequests.Load(), stats)
	}

	datasets, requestURL, err := client.StudyMetadataDatasetsWithURL(ctx, badStudy)
	if err == nil || len(datasets) != 0 || requestURL != "" {
		t.Fatalf("failed read disclosed unpublished metadata or URL: datasets=%d request URL=%q error=%v", len(datasets), requestURL, err)
	}
	if diagnostic := fmt.Sprintf("%+v", err); strings.Contains(diagnostic, "private-ticket") || strings.Contains(diagnostic, "invalid-private-path") {
		t.Fatalf("diagnostic disclosed the redirected URL: %s", diagnostic)
	}
}
