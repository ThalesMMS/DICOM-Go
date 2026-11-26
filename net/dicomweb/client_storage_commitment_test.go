package dicomweb

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

const commitmentTestUID = "2.25.123"

var commitmentTestReferences = []StorageCommitmentReference{
	{SOPClassUID: "1.2.840.10008.5.1.4.1.1.2", SOPInstanceUID: "2.25.1"},
	{SOPClassUID: "1.2.840.10008.5.1.4.1.1.2", SOPInstanceUID: "2.25.2"},
}

func TestStorageCommitmentClientImmediateAndPolling(t *testing.T) {
	for _, async := range []bool{false, true} {
		t.Run(fmt.Sprintf("async=%t", async), func(t *testing.T) {
			var posts, polls atomic.Int32
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.String() != "/dicom-web/commitment-requests/"+commitmentTestUID {
					t.Errorf("unexpected resource %s", r.URL)
				}
				if got := r.Header.Get("Accept"); got != commitmentMediaType || r.Header.Get("Authorization") != "Bearer local-test" {
					t.Error("incorrect request media or auth")
				}
				switch r.Method {
				case http.MethodPost:
					posts.Add(1)
					var body Dataset
					if err := json.NewDecoder(r.Body).Decode(&body); err != nil || len(body) != 1 ||
						r.Header.Get("Content-Type") != commitmentMediaType {
						t.Errorf("invalid request representation: %v", err)
					} else {
						elem := body[commitmentReferencedSOPSequence]
						if elem.VR != "SQ" || len(elem.Value) != 2 {
							t.Errorf("unexpected request dataset %v", body)
						} else {
							for i, item := range elem.Value {
								dataset, _ := sequenceItemDataset(item)
								if commitmentUIDValue(dataset[commitmentSOPClassUID]) != commitmentTestReferences[i].SOPClassUID ||
									commitmentUIDValue(dataset[commitmentSOPInstanceUID]) != commitmentTestReferences[i].SOPInstanceUID {
									t.Error("unexpected SOP reference")
								}
							}
						}
					}
					if async {
						w.Header().Set("Location", r.URL.Path)
						w.WriteHeader(http.StatusAccepted)
						return
					}
				case http.MethodGet:
					if r.ContentLength != 0 || r.Header.Get("Content-Type") != "" {
						t.Error("poll must have no request payload")
					}
					if polls.Add(1) == 1 {
						w.WriteHeader(http.StatusAccepted)
						return
					}
				default:
					t.Errorf("unexpected method %s", r.Method)
					w.WriteHeader(http.StatusMethodNotAllowed)
					return
				}
				writeCommitmentResult(t, w, []int{0}, []int{1})
			}))
			defer srv.Close()

			c := Client{
				Endpoint: Endpoint{BaseURL: srv.URL + "/dicom-web"},
				Options:  Options{BearerToken: "local-test"},
			}
			result, err := c.CommitStorage(context.Background(), commitmentTestReferences, StorageCommitmentOptions{
				TransactionUID: commitmentTestUID, Timeout: 2 * time.Second, PollInterval: time.Millisecond,
			})
			wantPolls := int32(0)
			if async {
				wantPolls = 2
			}
			if err != nil || result.TransactionUID != commitmentTestUID || result.Pending ||
				len(result.Committed) != 1 || result.Committed[0] != commitmentTestReferences[0] ||
				len(result.Failed) != 1 || result.Failed[0].StorageCommitmentReference != commitmentTestReferences[1] || result.Failed[0].FailureReason != 274 ||
				posts.Load() != 1 || polls.Load() != wantPolls {
				t.Fatalf("result=%#v error=%v posts=%d polls=%d", result, err, posts.Load(), polls.Load())
			}
		})
	}
}

func TestStorageCommitmentPendingTimeoutAndCancellation(t *testing.T) {
	for _, cancelRequested := range []bool{false, true} {
		t.Run(fmt.Sprintf("cancel=%t", cancelRequested), func(t *testing.T) {
			seen := make(chan struct{}, 1)
			var gets atomic.Int32
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Method == http.MethodGet {
					gets.Add(1)
				}
				w.Header().Set("Retry-After", "60")
				w.WriteHeader(http.StatusAccepted)
				_, _ = w.Write(nil)
				w.(http.Flusher).Flush()
				select {
				case seen <- struct{}{}:
				default:
				}
			}))
			defer srv.Close()
			c := Client{Endpoint: Endpoint{BaseURL: srv.URL}}

			// The public Check operation returns the peer's state and retry hint.
			pending, err := c.CheckStorageCommitment(context.Background(), commitmentTestUID, commitmentTestReferences)
			if err != nil || !pending.Pending || pending.RetryAfter != time.Minute {
				t.Fatalf("check result %#v, error %v", pending, err)
			}
			<-seen
			gets.Store(0)
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			wait := 250 * time.Millisecond
			wantErr := context.DeadlineExceeded
			if cancelRequested {
				// Cancel after the initial request finished and polling began.
				// Use a short first hint here to observe that through the wire.
				wantErr = context.Canceled
				wait = 2 * time.Second
				go func() {
					<-seen
					timer := time.NewTimer(80 * time.Millisecond)
					defer timer.Stop()
					select {
					case <-timer.C:
						cancel()
					case <-ctx.Done():
					}
				}()
			}
			got, err := c.CommitStorage(ctx, commitmentTestReferences, StorageCommitmentOptions{
				TransactionUID: commitmentTestUID, Timeout: wait, PollInterval: time.Millisecond,
			})
			if !errors.Is(err, wantErr) || !got.Pending || got.TransactionUID != commitmentTestUID ||
				len(got.Committed) != 0 || len(got.Failed) != 0 || gets.Load() != 0 {
				t.Fatalf("result %#v, error %v, unexpected polls %d", got, err, gets.Load())
			}
		})
	}
}

func TestStorageCommitmentRejectsMismatchedResult(t *testing.T) {
	for _, tc := range []struct {
		name   string
		mutate func(Dataset)
	}{
		{name: "foreign SOP", mutate: func(d Dataset) {
			commitmentTestItem(d, commitmentReferencedSOPSequence)[commitmentSOPInstanceUID] = Element{VR: "UI", Value: []any{"2.25.333"}}
		}},
		{name: "different class", mutate: func(d Dataset) {
			commitmentTestItem(d, commitmentReferencedSOPSequence)[commitmentSOPClassUID] = Element{VR: "UI", Value: []any{"1.3.4"}}
		}},
		{name: "repeated SOP across outcomes", mutate: func(d Dataset) {
			commitmentTestItem(d, commitmentFailedSOPSequence)[commitmentSOPInstanceUID] = Element{VR: "UI", Value: []any{"2.25.1"}}
		}},
		{name: "omitted SOP", mutate: func(d Dataset) { delete(d, commitmentFailedSOPSequence) }},
		{name: "missing failure reason", mutate: func(d Dataset) {
			delete(commitmentTestItem(d, commitmentFailedSOPSequence), commitmentFailureReason)
		}},
		{name: "foreign transaction", mutate: func(d Dataset) {
			d[commitmentTransactionUID] = Element{VR: "UI", Value: []any{"2.25.456"}}
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				d := commitmentTestDataset([]int{0}, []int{1})
				tc.mutate(d)
				w.Header().Set("Content-Type", commitmentMediaType)
				if err := json.NewEncoder(w).Encode(d); err != nil {
					t.Error(err)
				}
			}))
			defer srv.Close()
			c := Client{Endpoint: Endpoint{BaseURL: srv.URL}}
			result, err := c.CheckStorageCommitment(context.Background(), commitmentTestUID, commitmentTestReferences)
			var protocolErr *Error
			if len(result.Committed) != 0 || len(result.Failed) != 0 || !errors.As(err, &protocolErr) || protocolErr.Kind != ErrorKindDecodeResponse {
				t.Fatalf("invalid result %#v, error %v", result, err)
			}
		})
	}
}

func TestStorageCommitmentDoesNotFollowOtherResources(t *testing.T) {
	var requests atomic.Int32
	other := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		requests.Add(1)
	}))
	defer other.Close()
	for _, tc := range []struct {
		status   int
		location string
	}{
		{http.StatusAccepted, other.URL + "/commitment-requests/" + commitmentTestUID},
		{http.StatusAccepted, "/commitment-requests/2.25.456"},
		{http.StatusTemporaryRedirect, "/foreign"},
		{http.StatusTemporaryRedirect, other.URL},
	} {
		t.Run(fmt.Sprintf("%d/%s", tc.status, tc.location), func(t *testing.T) {
			var initiations atomic.Int32
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				initiations.Add(1)
				w.Header().Set("Location", tc.location)
				w.WriteHeader(tc.status)
			}))
			defer srv.Close()
			c := Client{Endpoint: Endpoint{BaseURL: srv.URL}, Options: Options{BearerToken: "secret-for-first-origin"}}
			got, err := c.CommitStorage(context.Background(), commitmentTestReferences, StorageCommitmentOptions{
				TransactionUID: commitmentTestUID, Timeout: time.Second, PollInterval: time.Millisecond,
			})
			if err == nil || strings.Contains(err.Error(), "secret-for-first-origin") ||
				len(got.Committed) != 0 || initiations.Load() != 1 || requests.Load() != 0 {
				t.Fatalf("result %#v, error %v, requests %d/%d", got, err, initiations.Load(), requests.Load())
			}
		})
	}
}

func commitmentTestItem(d Dataset, key string) Dataset {
	return d[key].Value[0].(Dataset)
}

func commitmentTestDataset(committed, failed []int) Dataset {
	d := Dataset{}
	for tag, indexes := range map[string][]int{commitmentReferencedSOPSequence: committed, commitmentFailedSOPSequence: failed} {
		if len(indexes) == 0 {
			continue
		}
		items := make([]any, 0, len(indexes))
		for _, index := range indexes {
			ref := commitmentTestReferences[index]
			item := Dataset{
				commitmentSOPClassUID:    {VR: "UI", Value: []any{ref.SOPClassUID}},
				commitmentSOPInstanceUID: {VR: "UI", Value: []any{ref.SOPInstanceUID}},
			}
			if tag == commitmentFailedSOPSequence {
				item[commitmentFailureReason] = Element{VR: "US", Value: []any{274}}
			}
			items = append(items, item)
		}
		d[tag] = Element{VR: "SQ", Value: items}
	}
	return d
}

func writeCommitmentResult(t *testing.T, w http.ResponseWriter, committed, failed []int) {
	t.Helper()
	w.Header().Set("Content-Type", commitmentMediaType)
	if err := json.NewEncoder(w).Encode([]Dataset{commitmentTestDataset(committed, failed)}); err != nil {
		t.Error(err)
	}
}

func TestStorageCommitmentRejectsInvalidRequestsBeforeTraffic(t *testing.T) {
	var requests atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { requests.Add(1) }))
	defer srv.Close()
	c := Client{Endpoint: Endpoint{BaseURL: srv.URL}}
	for _, refs := range [][]StorageCommitmentReference{
		nil,
		{{SOPClassUID: "invalid", SOPInstanceUID: "2.25.1"}},
		{{SOPClassUID: "1.2.3", SOPInstanceUID: "1.2.3/path"}},
		{commitmentTestReferences[0], commitmentTestReferences[0]},
	} {
		if _, err := c.CommitStorage(context.Background(), refs, StorageCommitmentOptions{TransactionUID: commitmentTestUID}); err == nil {
			t.Fatal("invalid request accepted", refs)
		}
	}
	if _, err := c.CommitStorage(context.Background(), commitmentTestReferences, StorageCommitmentOptions{TransactionUID: "../x"}); err == nil {
		t.Fatal("invalid transaction accepted")
	}
	if _, err := c.CommitStorage(context.Background(), commitmentTestReferences, StorageCommitmentOptions{Timeout: -time.Second}); err == nil {
		t.Fatal("negative timeout accepted")
	}
	if requests.Load() != 0 {
		t.Fatal("invalid input emitted traffic")
	}
}

func TestStorageCommitmentRejectsStatusAndRepresentationFailures(t *testing.T) {
	for _, tc := range []struct {
		name        string
		status      int
		body        string
		contentType string
	}{
		{"HTTP conflict", http.StatusConflict, "", commitmentMediaType},
		{"HTTP failure", http.StatusServiceUnavailable, "", commitmentMediaType},
		{"empty 200", http.StatusOK, "", commitmentMediaType},
		{"payload on 202", http.StatusAccepted, "[]", commitmentMediaType},
		{"non DICOM JSON MIME", http.StatusOK, "[]", "application/json"},
		{"multiple datasets", http.StatusOK, "[{},{}]", commitmentMediaType},
		{"trailing JSON", http.StatusOK, "[] []", commitmentMediaType},
	} {
		t.Run(tc.name, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.Header().Set("Content-Type", tc.contentType)
				w.WriteHeader(tc.status)
				_, _ = io.WriteString(w, tc.body)
			}))
			defer srv.Close()
			c := Client{Endpoint: Endpoint{BaseURL: srv.URL}}
			result, err := c.CheckStorageCommitment(context.Background(), commitmentTestUID, commitmentTestReferences)
			if err == nil || len(result.Committed) != 0 || len(result.Failed) != 0 {
				t.Fatalf("result %#v, error %v", result, err)
			}
		})
	}
}
