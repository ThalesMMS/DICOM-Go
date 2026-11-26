package ups_test

import (
	"context"
	"errors"
	"testing"

	"github.com/ThalesMMS/dicom-go/core"
	"github.com/ThalesMMS/dicom-go/net/dimse"
	"github.com/ThalesMMS/dicom-go/object"
	"github.com/ThalesMMS/dicom-go/ups"
)

func TestQueryIdentifierPreservesReturnAndMatchingKeys(t *testing.T) {
	t.Parallel()
	identifier, err := ups.BuildQueryIdentifier(ups.Query{Keys: map[core.Tag]ups.QueryKey{
		ups.TagPatientName:        ups.Match("DOE*"),
		ups.TagSOPInstanceUID:     ups.ReturnKey(),
		ups.TagProcedureStepState: ups.Match(string(ups.StateScheduled)),
	}})
	if err != nil {
		t.Fatal(err)
	}
	parsed, err := ups.ParseQueryIdentifier(identifier)
	if err != nil {
		t.Fatal(err)
	}
	if !parsed.HasMatchingKey() || len(parsed.Keys[ups.TagSOPInstanceUID].Values) != 0 || parsed.Keys[ups.TagPatientName].Values[0] != "DOE*" {
		t.Fatalf("parsed = %#v", parsed)
	}
	if _, err := ups.BuildQueryIdentifier(ups.Query{Keys: map[core.Tag]ups.QueryKey{ups.TagTransactionUID: ups.ReturnKey()}}); !errors.Is(err, ups.ErrQueryIdentifier) {
		t.Fatalf("Transaction UID query error = %v", err)
	}
}

func TestQueryRouteStreamsOnlyMatchingRequestedAttributes(t *testing.T) {
	t.Parallel()
	store, err := ups.NewMemoryStore(ups.MemoryStoreOptions{})
	if err != nil {
		t.Fatal(err)
	}
	service, err := ups.NewService(store, ups.ServiceOptions{})
	if err != nil {
		t.Fatal(err)
	}
	createScheduledStep(t, service, "1.2.826.0.1.3680043.10.543.301", "DOE^ALPHA")
	createScheduledStep(t, service, "1.2.826.0.1.3680043.10.543.302", "OTHER^PATIENT")

	routes, err := service.QueryRoutes(ups.QuerySCPOptions{})
	if err != nil {
		t.Fatal(err)
	}
	identifier, err := ups.BuildQueryIdentifier(ups.Query{Keys: map[core.Tag]ups.QueryKey{
		ups.TagPatientName: ups.Match("DOE*"), ups.TagSOPInstanceUID: ups.ReturnKey(),
	}})
	if err != nil {
		t.Fatal(err)
	}
	var matches []*object.Object
	handler := routes[0].Handler
	err = handler.Find(context.Background(), dimse.StreamingCFindRequest{Identifier: identifier}, func(status uint16, match *object.Object) error {
		if status != dimse.StatusPending {
			t.Fatalf("status = 0x%04X", status)
		}
		matches = append(matches, match)
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(matches) != 1 {
		t.Fatalf("matches = %d", len(matches))
	}
	if _, ok := matches[0].Get(ups.TagPatientName); !ok {
		t.Fatal("matching Patient Name was not projected")
	}
	if _, ok := matches[0].Get(ups.TagProcedureStepState); ok {
		t.Fatal("unrequested state was projected")
	}
}

func TestQueryWithoutMatchingKeyReturnsZeroMatches(t *testing.T) {
	t.Parallel()
	store, _ := ups.NewMemoryStore(ups.MemoryStoreOptions{})
	service, _ := ups.NewService(store, ups.ServiceOptions{})
	createScheduledStep(t, service, "1.2.826.0.1.3680043.10.543.303", "DOE^ALPHA")
	routes, _ := service.QueryRoutes(ups.QuerySCPOptions{})
	identifier, _ := ups.BuildQueryIdentifier(ups.Query{Keys: map[core.Tag]ups.QueryKey{ups.TagSOPInstanceUID: ups.ReturnKey()}})
	count := 0
	if err := routes[0].Handler.Find(context.Background(), dimse.StreamingCFindRequest{Identifier: identifier}, func(uint16, *object.Object) error {
		count++
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if count != 0 {
		t.Fatalf("matches = %d, want 0", count)
	}
}

func TestParsedUPSQueryMatchesDateTimePrecisionAndOffset(t *testing.T) {
	for _, tc := range []struct {
		name, from, through, candidate string
		matches                        bool
	}{
		{"blank lower", "   ", "20260831123000+0000", "20260830123000+0000", true},
		{"blank upper", "20260829123000+0000", "\t", "20260830123000+0000", true},
		{"partial month in local time", "20260330", "202603+0300", "20260331200000+0000", true},
		{"outside partial month", "20260330", "202603+0300", "20260331210000+0000", false},
		{"negative offset range", "2026083012-0300", "2026083012-0300", "20260830154500+0000", true},
		{"negative offset lower bound", "2026083012-0300", "", "20260830144500+0000", false},
		{"negative offset upper bound", "", "2026083012-0300", "20260830154500+0000", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			key, err := ups.MatchDateTimeRange(tc.from, tc.through)
			if err != nil {
				t.Fatal(err)
			}
			wireIdentifier, err := ups.BuildQueryIdentifier(ups.Query{Keys: map[core.Tag]ups.QueryKey{
				ups.TagScheduledProcedureStepStartDateTime: key,
				ups.TagWorklistLabel:                       ups.ReturnKey(),
			}})
			if err != nil {
				t.Fatal(err)
			}
			query, err := ups.ParseQueryIdentifier(wireIdentifier)
			if err != nil {
				t.Fatal(err)
			}
			response := ups.NewDataSet(ups.StringElement(ups.TagScheduledProcedureStepStartDateTime, core.VRDT, tc.candidate))
			if got := query.MatchesIdentifier(response); got != tc.matches {
				t.Fatalf("MatchesIdentifier = %t, want %t", got, tc.matches)
			}
		})
	}
	if _, err := ups.MatchDateTimeRange("202608301400", "2026083012"); !errors.Is(err, ups.ErrQueryIdentifier) {
		t.Fatalf("inverted matching range error = %v", err)
	}
}

func createScheduledStep(t *testing.T, service *ups.Service, uid, patientName string) {
	t.Helper()
	attributes := scheduledAttributes(t)
	attributes.Put(ups.StringElement(ups.TagPatientName, core.VRPN, patientName))
	if _, err := service.Create(context.Background(), ups.CreateRequest{SOPInstanceUID: uid, Attributes: attributes}); err != nil {
		t.Fatal(err)
	}
}

func TestParsedUPSQueryAcceptsEmptyTimezoneAndCharacterSet(t *testing.T) {
	wireIdentifier, err := ups.BuildQueryIdentifier(ups.Query{Keys: map[core.Tag]ups.QueryKey{
		ups.TagProcedureStepState: ups.Match("SCHEDULED"),
		ups.TagWorklistLabel:      ups.ReturnKey(),
	}})
	if err != nil {
		t.Fatal(err)
	}
	query, err := ups.ParseQueryIdentifier(wireIdentifier)
	if err != nil {
		t.Fatal(err)
	}
	response := func(timezone ...string) *object.Object {
		return ups.NewDataSet(
			ups.StringElement(ups.TagSpecificCharacterSet, core.VRCS),
			ups.StringElement(ups.TagTimezoneOffsetFromUTC, core.VRSH, timezone...),
			ups.StringElement(ups.TagProcedureStepState, core.VRCS, "SCHEDULED"),
		)
	}
	// The pending responses of dcm4chee-arc 5.35.2 carry both attributes
	// with zero length.
	if !query.MatchesIdentifier(response()) || !query.MatchesIdentifier(response("")) {
		t.Fatal("a response with an empty Timezone Offset From UTC was refused")
	}
	if !query.MatchesIdentifier(response("+0100")) {
		t.Fatal("a response with a valid Timezone Offset From UTC was refused")
	}
	if query.MatchesIdentifier(response("UTC")) || query.MatchesIdentifier(response("+0100", "+0200")) {
		t.Fatal("a response with an invalid Timezone Offset From UTC was accepted")
	}
}
