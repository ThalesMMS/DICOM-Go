package dicomweb_test

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/ThalesMMS/dicom-go/core"
	"github.com/ThalesMMS/dicom-go/dicomjson"
	"github.com/ThalesMMS/dicom-go/dictionary/std"
	"github.com/ThalesMMS/dicom-go/mpps"
	"github.com/ThalesMMS/dicom-go/net/dicomweb"
	"github.com/ThalesMMS/dicom-go/object"
)

func webMPPSCreate(t *testing.T) *object.Object {
	t.Helper()
	attributes, err := mpps.BuildCreate(mpps.CreateAttributes{
		ScheduledSteps: []mpps.ScheduledStep{{StudyInstanceUID: "1.2.1828.1"}},
		PatientID:      "SYNTHETIC", PerformedProcedureStepID: "STEP-1",
		PerformedStationAETitle: "TWIN", StartDate: "20261116", StartTime: "140000",
		Modality: "CT",
	})
	if err != nil {
		t.Fatal(err)
	}
	return attributes
}

func TestMPPSWebTransactions(t *testing.T) {
	var gotMethods []string
	var gotState []string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/dicom/modality-performed-procedure-steps/1.2.1828" {
			t.Errorf("unexpected path %q", r.URL.Path)
		}
		if r.Header.Get("Accept") != "application/dicom+json" {
			t.Error("expected explicit DICOM JSON accept")
		}
		gotMethods = append(gotMethods, r.Method+"?"+r.URL.RawQuery)
		switch r.Method {
		case http.MethodPost:
			if r.Header.Get("Content-Type") != "application/dicom+json" {
				t.Error("expected DICOM JSON payload")
			}
			body, _ := io.ReadAll(r.Body)
			attributes, err := dicomjson.Unmarshal(body, std.Dictionary)
			if err != nil {
				t.Error(err)
				w.WriteHeader(http.StatusBadRequest)
				return
			}
			state, _ := attributes.LookupString(mpps.TagPerformedProcedureStepStatus)
			gotState = append(gotState, state)
			w.Header().Set("Location", "/dicom/modality-performed-procedure-steps/1.2.1828")
			if r.URL.RawQuery == "update" {
				w.WriteHeader(http.StatusOK)
			} else {
				w.WriteHeader(http.StatusCreated)
			}
		case http.MethodGet:
			w.Header().Set("Content-Type", "application/dicom+json")
			_, _ = io.WriteString(w, `{"00400252":{"vr":"CS","Value":["COMPLETED"]}}`)
		default:
			w.WriteHeader(http.StatusMethodNotAllowed)
		}
	}))
	defer server.Close()
	client := dicomweb.Client{Endpoint: dicomweb.Endpoint{BaseURL: server.URL + "/dicom"}}
	if err := client.CreateMPPS(context.Background(), "1.2.1828", webMPPSCreate(t)); err != nil {
		t.Fatal(err)
	}
	update, err := mpps.BuildSet(mpps.Update{Status: mpps.StatusCompleted, EndDate: "20261116", EndTime: "150000"})
	if err != nil {
		t.Fatal(err)
	}
	if err := client.UpdateMPPS(context.Background(), "1.2.1828", update); err != nil {
		t.Fatal(err)
	}
	retrieved, err := client.RetrieveMPPS(context.Background(), "1.2.1828")
	if err != nil {
		t.Fatal(err)
	}
	if status, _ := retrieved.LookupString(mpps.TagPerformedProcedureStepStatus); status != mpps.StatusCompleted {
		t.Errorf("retrieved state = %q", status)
	}
	if strings.Join(gotMethods, ",") != "POST?,POST?update,GET?" || strings.Join(gotState, ",") != "IN PROGRESS,COMPLETED" {
		t.Fatalf("unexpected requests %v / %v", gotMethods, gotState)
	}
}

func TestMPPSWebRejectsUnexpectedResponses(t *testing.T) {
	cases := []struct {
		name, body, location, contentType string
		status                            int
		retrieve                          bool
	}{
		{name: "create accepted is pending", status: 202},
		{name: "create OK is wrong", status: 200},
		{name: "create body", status: 201, body: "{}"},
		{name: "create cross origin", status: 201, location: "https://elsewhere.example/step"},
		{name: "create other resource", status: 201, location: "/modality-performed-procedure-steps/1.2.99"},
		{name: "retrieve wrong media", retrieve: true, status: 200, contentType: "application/json", body: "{}"},
		{name: "retrieve wrong instance", retrieve: true, status: 200, contentType: "application/dicom+json",
			body: `{"00080018":{"vr":"UI","Value":["1.2.99"]},"00400252":{"vr":"CS","Value":["IN PROGRESS"]}}`},
		{name: "retrieve duplicate", retrieve: true, status: 200, contentType: "application/dicom+json",
			body: `{"00400252":{"vr":"CS","Value":["IN PROGRESS"]},"00400252":{"vr":"CS","Value":["COMPLETED"]}}`},
		{name: "retrieve multiple", retrieve: true, status: 200, contentType: "application/dicom+json",
			body: `[{"00400252":{"vr":"CS","Value":["IN PROGRESS"]}},{"00400252":{"vr":"CS","Value":["COMPLETED"]}}]`},
		{name: "retrieve invalid state", retrieve: true, status: 200, contentType: "application/dicom+json",
			body: `{"00400252":{"vr":"CS","Value":["PENDING"]}}`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var requests atomic.Int32
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				requests.Add(1)
				if tc.contentType != "" {
					w.Header().Set("Content-Type", tc.contentType)
				}
				if tc.location != "" {
					w.Header().Set("Location", tc.location)
				}
				w.WriteHeader(tc.status)
				_, _ = io.WriteString(w, tc.body)
			}))
			defer server.Close()
			client := dicomweb.Client{Endpoint: dicomweb.Endpoint{BaseURL: server.URL}}
			var err error
			if tc.retrieve {
				_, err = client.RetrieveMPPS(context.Background(), "1.2.1828")
			} else {
				err = client.CreateMPPS(context.Background(), "1.2.1828", webMPPSCreate(t))
			}
			if err == nil || requests.Load() != 1 {
				t.Fatalf("error=%v requests=%d", err, requests.Load())
			}
		})
	}
}

func TestMPPSWebDoesNotRedirectOrSendInvalidIdentity(t *testing.T) {
	var redirected atomic.Int32
	other := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { redirected.Add(1) }))
	defer other.Close()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Location", other.URL+r.URL.Path)
		w.WriteHeader(http.StatusTemporaryRedirect)
	}))
	defer server.Close()
	client := dicomweb.Client{Endpoint: dicomweb.Endpoint{BaseURL: server.URL}}
	if err := client.CreateMPPS(context.Background(), "1.2.1828", webMPPSCreate(t)); err == nil || redirected.Load() != 0 {
		t.Fatalf("redirect error %v, calls=%d", err, redirected.Load())
	}
	for _, invalid := range []string{"", "../1", "1.2/a", "1.2?secret"} {
		if _, err := client.Endpoint.MPPSURL(invalid); err == nil {
			t.Fatalf("accepted invalid UID %q", invalid)
		}
	}
	attributes := webMPPSCreate(t)
	if err := attributes.SetStrings(core.NewTag(0x0008, 0x0018), core.VRUI, "1.2.99"); err != nil {
		t.Fatal(err)
	}
	if err := client.CreateMPPS(context.Background(), "1.2.1828", attributes); err == nil {
		t.Fatal("accepted mismatched payload identity")
	}
}

func TestMPPSWebCancelsReadsAndBoundsBody(t *testing.T) {
	cancelServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/dicom+json")
		_, _ = io.WriteString(w, "{")
		w.(http.Flusher).Flush()
		<-r.Context().Done()
	}))
	defer cancelServer.Close()
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	client := dicomweb.Client{Endpoint: dicomweb.Endpoint{BaseURL: cancelServer.URL}}
	if _, err := client.RetrieveMPPS(ctx, "1.2.1828"); !errors.Is(err, context.Canceled) {
		t.Fatalf("canceled request: %v", err)
	}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/dicom+json")
		_ = json.NewEncoder(w).Encode(map[string]any{"00400252": map[string]any{"vr": "CS", "Value": []string{"IN PROGRESS"}}})
	}))
	defer server.Close()
	client = dicomweb.Client{Endpoint: dicomweb.Endpoint{BaseURL: server.URL}, Options: dicomweb.Options{MaxBodyBytes: 8}}
	if _, err := client.RetrieveMPPS(context.Background(), "1.2.1828"); err == nil {
		t.Fatal("accepted response beyond configured limit")
	}
}
