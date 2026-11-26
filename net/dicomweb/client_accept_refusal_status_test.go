package dicomweb

import (
	"context"
	"errors"
	"net/http"
	"testing"

	"github.com/ThalesMMS/dicom-go/transfer"
)

// Orthanc's DICOMweb plugin answers 500 when it cannot transcode to the
// preferred transfer syntax, and 400 for a media type it does not serve. The
// client then asks for the next acceptable representation.
func TestWADOAcceptFallbackAfterServerRefusalStatuses(t *testing.T) {
	uid := transfer.ExplicitVRLittleEndian.UID
	wildcard := wadoObjectAcceptValue("*")
	for _, status := range []int{http.StatusInternalServerError, http.StatusBadRequest, http.StatusUnsupportedMediaType, http.StatusNotImplemented} {
		client := wadoAcceptPeer(t, wadoFallbackInstancePath,
			wadoAcceptExchange{accept: wadoObjectAcceptValue(uid) + ", " + wildcard, status: status, contentType: "text/plain", body: "cannot transcode"},
			wadoAcceptExchange{accept: wildcard, status: http.StatusOK, contentType: "application/dicom", body: wadoFallbackPayload},
		)
		parts, err := client.RetrieveInstanceWithOptions(context.Background(), wadoFallbackRef,
			RetrieveOptions{TransferSyntaxUIDs: []string{uid, "*"}})
		if err != nil || len(parts) != 1 || string(parts[0].Data) != wadoFallbackPayload {
			t.Fatalf("after HTTP %d: parts = %d, err = %v, want the wildcard representation", status, len(parts), err)
		}
	}
}

// A status that does not concern the representation ends the negotiation,
// and the last refusal keeps its own status.
func TestWADOAcceptFallbackKeepsOtherStatusesFinal(t *testing.T) {
	uid := transfer.ExplicitVRLittleEndian.UID
	wildcard := wadoObjectAcceptValue("*")
	first := wadoObjectAcceptValue(uid) + ", " + wildcard
	for _, test := range []struct {
		name   string
		script []wadoAcceptExchange
		want   int
		kind   ErrorKind
	}{
		{"not found", []wadoAcceptExchange{{accept: first, status: http.StatusNotFound}}, http.StatusNotFound, ErrorKindHTTPStatus},
		{"unauthorized", []wadoAcceptExchange{{accept: first, status: http.StatusUnauthorized}}, http.StatusUnauthorized, ErrorKindAuthStatus},
		{"forbidden", []wadoAcceptExchange{{accept: first, status: http.StatusForbidden}}, http.StatusForbidden, ErrorKindAuthStatus},
		{"server error for every representation", []wadoAcceptExchange{
			{accept: first, status: http.StatusInternalServerError},
			{accept: wildcard, status: http.StatusInternalServerError},
		}, http.StatusInternalServerError, ErrorKindHTTPStatus},
	} {
		t.Run(test.name, func(t *testing.T) {
			client := wadoAcceptPeer(t, wadoFallbackInstancePath, test.script...)
			parts, err := client.RetrieveInstanceWithOptions(context.Background(), wadoFallbackRef,
				RetrieveOptions{TransferSyntaxUIDs: []string{uid, "*"}})
			var webErr *Error
			if !errors.As(err, &webErr) || webErr.Kind != test.kind || webErr.StatusCode != test.want || len(parts) != 0 {
				t.Fatalf("parts = %d, err = %v, want HTTP %d", len(parts), err, test.want)
			}
		})
	}
}
