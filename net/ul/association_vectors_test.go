package ul

import (
	"bytes"
	"context"
	"errors"
	"net"
	"testing"
	"time"

	"github.com/ThalesMMS/dicom-go/internal/dicomtest"
)

// TestAssociationRQVectors: every malformed request from DCMTK's parser
// regressions fails with a typed error that maps to an A-P-ABORT reason,
// returns no PDU and allocates little; the well-formed one parses whole.
func TestAssociationRQVectors(t *testing.T) {
	for _, vector := range dicomtest.AssociationRQVectors() {
		t.Run(vector.Name, func(t *testing.T) {
			var pdu PDU
			var err error
			allocated := allocatedBytes(func() {
				pdu, err = ReadPDU(bytes.NewReader(vector.PDU), DefaultMaxPDU)
			})
			if !vector.Malformed {
				if err != nil {
					t.Fatal(err)
				}
				checkSecondaryFieldVector(t, pdu.(*AssociationRQ))
				return
			}
			if err == nil || pdu != nil {
				t.Fatalf("ReadPDU = %#v, %v; want a refusal without a PDU", pdu, err)
			}
			if _, typed := ProviderAbortReason(err); !typed {
				t.Fatalf("error %v has no A-P-ABORT reason", err)
			}
			if allocated > 64<<10 {
				t.Fatalf("refusing a %d-byte request allocated %d bytes", len(vector.PDU), allocated)
			}
		})
	}
}

func checkSecondaryFieldVector(t *testing.T, rq *AssociationRQ) {
	t.Helper()
	var identity *UserIdentityItem
	var maxLength uint32
	for _, item := range rq.UserInfo {
		switch item := item.(type) {
		case UserIdentityItem:
			identity = &item
		case MaxLengthItem:
			maxLength = item.Value
		}
	}
	if identity == nil || string(identity.PrimaryField) != "ABC" || string(identity.SecondaryField) != "abcde" {
		t.Fatalf("user identity %#v", identity)
	}
	if maxLength != 32768 {
		t.Fatalf("Maximum Length after the identity = %d, want 32768", maxLength)
	}
}

// TestAcceptorRefusesAssociationRQVectors sends each vector to an acceptor:
// malformed requests get an A-P-ABORT from the service-provider with the
// reason of their parse error; the well-formed one parses whole and gets a
// typed A-ASSOCIATE-RJ for its identity.
func TestAcceptorRefusesAssociationRQVectors(t *testing.T) {
	for _, vector := range dicomtest.AssociationRQVectors() {
		t.Run(vector.Name, func(t *testing.T) {
			addr, done := acceptOnce(t, AcceptOptions{
				AETitle: "DCMNETTESTCALLED", AcceptAnyAbstractSyntax: true,
				SupportedTransferSyntaxes: []string{ImplicitVRLittleEndian}, ARTIMTimeout: 200 * time.Millisecond,
				UserIdentityHandler: func(UserIdentityRequest) (UserIdentityResult, error) {
					return UserIdentityResult{Action: UserIdentityActionAccept}, nil
				},
			})
			conn, err := (&net.Dialer{}).DialContext(context.Background(), "tcp", addr)
			if err != nil {
				t.Fatal(err)
			}
			defer func() { _ = conn.Close() }()
			if _, err := conn.Write(vector.PDU); err != nil {
				t.Fatal(err)
			}
			response, err := ReadPDU(conn, DefaultMaxPDU)
			if err != nil {
				t.Fatal(err)
			}
			if !vector.Malformed {
				// Parsed whole, then refused by policy: a username identity
				// must not carry a secondary field (PS3.7 Table D.3-14).
				rj, ok := response.(*AssociationRJ)
				if !ok || rj.Result != AssociateRJResultPermanent {
					t.Fatalf("response %#v, want a permanent A-ASSOCIATE-RJ", response)
				}
				if err := <-done; err == nil {
					t.Fatal("Accept reported success for a refused identity")
				}
				return
			}
			_, parseErr := ReadPDU(bytes.NewReader(vector.PDU), DefaultMaxPDU)
			reason, _ := ProviderAbortReason(parseErr)
			abort, ok := response.(*AbortRQ)
			if !ok || abort.Source != AbortSourceServiceProvider || abort.Reason != reason {
				t.Fatalf("response %#v, want provider abort reason %d", response, reason)
			}
			if err := <-done; err == nil {
				t.Fatal("Accept reported success for a malformed request")
			}
		})
	}
}

// TestLegacyRoleSelectionReply: AcceptOptions.RoleSelections replies with
// the proposed roles it supports and omits the item when none remain, so the
// requestor keeps its default roles (an SCP-only entry that enables C-GET
// must not deny a C-STORE sender its SCU role).
func TestLegacyRoleSelectionReply(t *testing.T) {
	type roles struct{ scu, scp bool }
	scu, scp, both := roles{true, false}, roles{false, true}, roles{true, true}
	for _, tc := range []struct {
		name      string
		requested roles
		supported *roles
		want      *roles
	}{
		{"SCU vs SCP", scu, &scp, nil},
		{"SCU vs SCU/SCP", scu, &both, &scu},
		{"SCP vs SCP", scp, &scp, &scp},
		{"SCP vs SCU", scp, &scu, nil},
		{"SCU/SCP vs SCP", both, &scp, &scp},
		{"SCU/SCP vs SCU/SCP", both, &both, &both},
		{"SCU/SCP vs none", both, nil, nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			accept := AcceptOptions{AETitle: "ACCEPTOR", AcceptAnyAbstractSyntax: true, SupportedTransferSyntaxes: []string{ImplicitVRLittleEndian}}
			if tc.supported != nil {
				accept.RoleSelections = []RoleSelectionItem{{SopClassUID: verificationSOPClassUID, SCURole: tc.supported.scu, SCPRole: tc.supported.scp}}
			}
			got, err := dialWithRole(t, accept, &RoleSelectionItem{SopClassUID: verificationSOPClassUID, SCURole: tc.requested.scu, SCPRole: tc.requested.scp})
			if err != nil {
				t.Fatal(err)
			}
			if tc.want == nil {
				if len(got) != 0 {
					t.Fatalf("reply %+v, want none (default roles)", got)
				}
				return
			}
			if len(got) != 1 || got[0].SCURole != tc.want.scu || got[0].SCPRole != tc.want.scp {
				t.Fatalf("reply %+v, want SCU=%v SCP=%v", got, tc.want.scu, tc.want.scp)
			}
		})
	}
}

// dialWithRole negotiates Verification with an optional role proposal and
// returns the accepted role selections.
func dialWithRole(t *testing.T, accept AcceptOptions, proposed *RoleSelectionItem) ([]RoleSelectionItem, error) {
	t.Helper()
	addr, done := acceptOnce(t, accept)
	dial := DialOptions{
		CalledAETitle: "ACCEPTOR", CallingAETitle: "REQUESTOR",
		Contexts: []PresentationContext{{AbstractSyntaxUID: verificationSOPClassUID, TransferSyntaxUIDs: []string{ImplicitVRLittleEndian}}},
	}
	if proposed != nil {
		dial.RoleSelections = []RoleSelectionItem{*proposed}
	}
	assoc, err := Dial(addr, dial)
	if err != nil {
		<-done
		return nil, err
	}
	got := assoc.AcceptedRoleSelections
	_ = assoc.Close()
	<-done
	return got, nil
}

// TestAcceptorRejectsDisallowedHost follows DCMTK's tscuscp
// fail_on_disallowed_host: an admission policy that refuses the calling host
// answers with a typed A-ASSOCIATE-RJ, and Accept reports the rejection.
func TestAcceptorRejectsDisallowedHost(t *testing.T) {
	for _, tc := range []struct {
		name    string
		allowed string
	}{{"disallowed", "192.0.2.1"}, {"allowed", "127.0.0.1"}} {
		t.Run(tc.name, func(t *testing.T) {
			addr, done := acceptOnce(t, AcceptOptions{
				AETitle: "REJECT_HOST", AcceptAnyAbstractSyntax: true, SupportedTransferSyntaxes: []string{ImplicitVRLittleEndian},
				Admission: func(_ context.Context, request AdmissionRequest) *AssociationRJ {
					if host, _, err := net.SplitHostPort(request.RemoteAddr.String()); err != nil || host != tc.allowed {
						return RejectPermanent()
					}
					return nil
				},
			})
			assoc, err := Dial(addr, DialOptions{
				CalledAETitle: "REJECT_HOST", CallingAETitle: "SCU",
				Contexts: []PresentationContext{{AbstractSyntaxUID: verificationSOPClassUID, TransferSyntaxUIDs: []string{ImplicitVRLittleEndian}}},
			})
			if tc.allowed == "127.0.0.1" {
				if err != nil {
					t.Fatal(err)
				}
				_ = assoc.Close()
				<-done
				return
			}
			var rejection *RejectionError
			if !errors.As(err, &rejection) || *rejection != (RejectionError{Result: AssociateRJResultPermanent, Source: AssociateRJSourceServiceUser, Reason: AssociateRJReasonNoReasonGiven}) {
				t.Fatalf("Dial error %v, want a permanent no-reason A-ASSOCIATE-RJ", err)
			}
			if err := <-done; !errors.Is(err, ErrAssociationRejected) {
				t.Fatalf("Accept error %v", err)
			}
		})
	}
}
