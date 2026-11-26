package ul

import (
	"bytes"
	"context"
	"errors"
	"net"
	"testing"
	"time"
)

func TestAdmissionNormalizesInvalidRejectReasons(t *testing.T) {
	for _, tc := range []struct {
		name        string
		input, want AssociationRJ
	}{
		{"user reason", AssociationRJ{Result: 1, Source: 1, Reason: 0}, *RejectPermanent()},
		{"ACSE reason", AssociationRJ{Result: 2, Source: 2, Reason: 3}, *RejectTransient()},
		{"presentation reason", AssociationRJ{Result: 2, Source: 3, Reason: 8}, *RejectTransient()},
		{"invalid result", AssociationRJ{Result: 9, Source: 1, Reason: 3}, *RejectCallingAETitle()},
		{"valid rejection", *RejectLocalLimitExceeded(), *RejectLocalLimitExceeded()},
	} {
		t.Run(tc.name, func(t *testing.T) {
			rj, err := admit(context.Background(), func(context.Context, AdmissionRequest) *AssociationRJ {
				return &tc.input
			}, AdmissionRequest{})
			if err != nil || rj == nil || *rj != tc.want {
				t.Fatalf("admit = %+v, %v; want %+v", rj, err, tc.want)
			}
			var encoded bytes.Buffer
			if err := WritePDU(&encoded, rj); err != nil {
				t.Fatalf("normalized rejection cannot be encoded: %v", err)
			}
		})
	}
}

// admissionExchange sends an A-ASSOCIATE-RQ to Accept with the given
// options and returns the first response PDU and Accept's error.
func admissionExchange(t *testing.T, opts AcceptOptions, dial DialOptions) (PDU, error) {
	t.Helper()
	client, server := net.Pipe()
	defer func() { _ = client.Close() }()
	defer func() { _ = server.Close() }()
	acceptDone := make(chan error, 1)
	go func() {
		assoc, err := Accept(server, opts)
		if assoc != nil {
			_ = assoc.Conn.Close()
		}
		acceptDone <- err
	}()
	if dial.Contexts == nil {
		dial.Contexts = []PresentationContext{{AbstractSyntaxUID: verificationSOPClassUID, TransferSyntaxUIDs: []string{ImplicitVRLittleEndian}}}
	}
	rq := buildAssociateRQ(dial)
	if err := WritePDU(client, &rq); err != nil {
		t.Fatalf("write A-ASSOCIATE-RQ: %v", err)
	}
	pdu, err := ReadPDU(client, DefaultMaxPDU)
	if err != nil {
		t.Fatalf("read response: %v", err)
	}
	_ = client.Close()
	return pdu, <-acceptDone
}

func TestAdmissionRejectsWithTypedRJ(t *testing.T) {
	var seen AdmissionRequest
	opts := AcceptOptions{
		AETitle:                   "ARCHIVE",
		SupportedAbstractSyntaxes: []string{verificationSOPClassUID},
		SupportedTransferSyntaxes: []string{ImplicitVRLittleEndian},
		Admission: func(_ context.Context, req AdmissionRequest) *AssociationRJ {
			seen = req
			if req.CallingAETitle != "ALLOWED" {
				return RejectCallingAETitle()
			}
			return nil
		},
	}
	dial := DialOptions{
		CallingAETitle: "INTRUDER", CalledAETitle: "ARCHIVE",
		ImplementationClassUID: "1.2.826.0.1.3680043.10.543.989", ImplementationVersionName: "PROBE_989",
		UserIdentity: &UserIdentityRequest{Type: 1, PrimaryField: []byte("operator")},
	}
	pdu, err := admissionExchange(t, opts, dial)
	rj, ok := pdu.(*AssociationRJ)
	if !ok || *rj != *RejectCallingAETitle() {
		t.Fatalf("response %#v, want calling-AE rejection", pdu)
	}
	var rejection *RejectionError
	if !errors.As(err, &rejection) || rejection.Reason != AssociateRJReasonCallingAETitleNotRecognized || !errors.Is(err, ErrAssociationRejected) {
		t.Fatalf("Accept error %v", err)
	}
	if seen.CallingAETitle != "INTRUDER" || seen.CalledAETitle != "ARCHIVE" || seen.ImplementationClassUID != "1.2.826.0.1.3680043.10.543.989" ||
		seen.ImplementationVersionName != "PROBE_989" || seen.RemoteAddr == nil || seen.UserIdentity == nil || string(seen.UserIdentity.PrimaryField) != "operator" {
		t.Fatalf("admission request %+v", seen)
	}

	dial.CallingAETitle = "ALLOWED"
	dial.UserIdentity = nil // no identity handler is configured
	pdu, err = admissionExchange(t, opts, dial)
	if _, ok := pdu.(*AssociationAC); !ok || err != nil {
		t.Fatalf("admitted peer: %T, %v", pdu, err)
	}
}

func TestAdmissionHandlerFailuresReject(t *testing.T) {
	base := AcceptOptions{AETitle: "ARCHIVE", AcceptAnyAbstractSyntax: true}
	panicking := base
	panicking.Admission = func(context.Context, AdmissionRequest) *AssociationRJ { panic("policy store unavailable") }
	pdu, err := admissionExchange(t, panicking, DialOptions{CalledAETitle: "ARCHIVE"})
	if rj, ok := pdu.(*AssociationRJ); !ok || *rj != *RejectPermanent() || !errors.Is(err, ErrAdmissionHandler) {
		t.Fatalf("panicking handler: %#v, %v", pdu, err)
	}

	slow := base
	slow.NegotiationTimeout = 50 * time.Millisecond
	release := make(chan struct{})
	defer close(release)
	slow.Admission = func(context.Context, AdmissionRequest) *AssociationRJ { <-release; return nil }
	pdu, err = admissionExchange(t, slow, DialOptions{CalledAETitle: "ARCHIVE"})
	if rj, ok := pdu.(*AssociationRJ); !ok || rj.Result != AssociateRJResultTransient || !errors.Is(err, ErrAdmissionHandler) {
		t.Fatalf("slow handler: %#v, %v", pdu, err)
	}

	invalid := base
	invalid.Admission = func(context.Context, AdmissionRequest) *AssociationRJ {
		return &AssociationRJ{Result: 9, Source: 9, Reason: 9}
	}
	pdu, _ = admissionExchange(t, invalid, DialOptions{CalledAETitle: "ARCHIVE"})
	if rj, ok := pdu.(*AssociationRJ); !ok || *rj != *RejectPermanent() {
		t.Fatalf("out-of-range RJ not normalized: %#v", pdu)
	}
}

// A handler that returns its rejection after the negotiation deadline, but
// before the context reports it, still gets the A-ASSOCIATE-RJ sent.
func TestAdmissionRejectionAfterTheDeadlineIsSentBeforeTheContextReportsIt(t *testing.T) {
	ctx := newLateDeadlineContext(100 * time.Millisecond)
	defer ctx.expire()
	opts := AcceptOptions{AETitle: "ARCHIVE", AcceptAnyAbstractSyntax: true, Context: ctx,
		Admission: func(context.Context, AdmissionRequest) *AssociationRJ {
			time.Sleep(time.Until(ctx.deadline) + 20*time.Millisecond)
			return RejectCallingAETitle()
		}}
	pdu, err := admissionExchange(t, opts, DialOptions{CalledAETitle: "ARCHIVE"})
	if rj, ok := pdu.(*AssociationRJ); !ok || *rj != *RejectCallingAETitle() || !errors.Is(err, ErrAssociationRejected) {
		t.Fatalf("late rejection: %#v, %v", pdu, err)
	}
}

func TestAdmissionRunsAfterProtocolChecks(t *testing.T) {
	called := false
	opts := AcceptOptions{AETitle: "ARCHIVE", RequireMatchingCalledAE: true, AcceptAnyAbstractSyntax: true,
		Admission: func(context.Context, AdmissionRequest) *AssociationRJ { called = true; return nil }}
	pdu, _ := admissionExchange(t, opts, DialOptions{CalledAETitle: "OTHER"})
	if rj, ok := pdu.(*AssociationRJ); !ok || rj.Reason != AssociateRJReasonCalledAETitleNotRecognized || called {
		t.Fatalf("called-AE check: %#v, handler called %v", pdu, called)
	}
}

func TestRejectionErrorDescription(t *testing.T) {
	for _, tc := range []struct {
		rj   *AssociationRJ
		want string
	}{
		{RejectCallingAETitle(), "rejected-permanent, service-user, calling-AE-title-not-recognized"},
		{RejectCalledAETitle(), "rejected-permanent, service-user, called-AE-title-not-recognized"},
		{RejectTransient(), "rejected-transient, service-user, no-reason-given"},
		{RejectLocalLimitExceeded(), "rejected-transient, service-provider (presentation), local-limit-exceeded"},
		{&AssociationRJ{Result: 1, Source: 2, Reason: 2}, "rejected-permanent, service-provider (ACSE), protocol-version-not-supported"},
		{&AssociationRJ{Result: 1, Source: 1, Reason: 9}, "rejected-permanent, service-user, reason 9"},
	} {
		err := &RejectionError{Result: tc.rj.Result, Source: tc.rj.Source, Reason: tc.rj.Reason}
		if got := err.Description(); got != tc.want {
			t.Errorf("%+v: %q, want %q", *tc.rj, got, tc.want)
		}
	}
}

// TestAdmissionDeciderAnswersTheUserIdentityOfItsAssociation: the identity
// handler the decider returns answers the association it admitted, in place
// of the listener-wide handler, which would reject it.
func TestAdmissionDeciderAnswersTheUserIdentityOfItsAssociation(t *testing.T) {
	var decided int
	opts := AcceptOptions{
		AETitle:                   "ARCHIVE",
		SupportedAbstractSyntaxes: []string{verificationSOPClassUID},
		SupportedTransferSyntaxes: []string{ImplicitVRLittleEndian},
		UserIdentityHandler: func(UserIdentityRequest) (UserIdentityResult, error) {
			return UserIdentityResult{Action: UserIdentityActionReject}, nil
		},
		AdmissionDecider: func(_ context.Context, req AdmissionRequest) AdmissionDecision {
			decided++
			if req.CallingAETitle != "ALLOWED" {
				return AdmissionDecision{Reject: RejectCallingAETitle()}
			}
			return AdmissionDecision{UserIdentity: func(UserIdentityRequest) (UserIdentityResult, error) {
				return UserIdentityResult{Action: UserIdentityActionAccept}, nil
			}}
		},
	}
	dial := DialOptions{CallingAETitle: "ALLOWED", CalledAETitle: "ARCHIVE", UserIdentity: &UserIdentityRequest{Type: 1, PrimaryField: []byte("operator")}}
	if pdu, err := admissionExchange(t, opts, dial); err != nil {
		t.Fatalf("admitted peer: %T, %v", pdu, err)
	} else if _, ok := pdu.(*AssociationAC); !ok {
		t.Fatalf("admitted peer received %T, want A-ASSOCIATE-AC", pdu)
	}
	dial.CallingAETitle = "INTRUDER"
	if pdu, _ := admissionExchange(t, opts, dial); func() bool { rj, ok := pdu.(*AssociationRJ); return !ok || *rj != *RejectCallingAETitle() }() {
		t.Fatalf("refused peer received %#v", pdu)
	}
	if decided != 2 {
		t.Fatalf("the decider ran %d times for two associations", decided)
	}
}
