package ul

import (
	"context"
	"errors"
	"net"
	"testing"
	"time"
)

func TestOnAssociationRejectionReportsAdmissionRefusal(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	listener, err := Listen(ListenOptions{Address: "127.0.0.1:0", Context: ctx})
	if err != nil {
		t.Fatalf("Listen() error = %v", err)
	}
	cleanupClose(t, "listener", listener)

	reported := make(chan RejectedAssociation, 1)
	serverErr := make(chan error, 1)
	go func() {
		_, err := listener.AcceptAssociation(AcceptOptions{
			Context:                   ctx,
			AETitle:                   "ACCEPTOR",
			SupportedAbstractSyntaxes: []string{verificationSOPClassUID},
			SupportedTransferSyntaxes: []string{ImplicitVRLittleEndian},
			Admission:                 func(context.Context, AdmissionRequest) *AssociationRJ { return RejectCallingAETitle() },
			OnAssociationRejection:    func(rejected RejectedAssociation) { reported <- rejected },
		})
		serverErr <- err
	}()
	_, err = DialContext(ctx, listener.Addr().String(), DialOptions{
		CallingAETitle: "intruder",
		CalledAETitle:  "ACCEPTOR",
		Contexts:       []PresentationContext{{AbstractSyntaxUID: verificationSOPClassUID, TransferSyntaxUIDs: []string{ImplicitVRLittleEndian}}},
	})
	var reject *RejectionError
	if !errors.As(err, &reject) {
		t.Fatalf("DialContext() error = %v, want RejectionError", err)
	}
	// The callback runs before the rejection is written, so it has already
	// reported when the requestor reads the A-ASSOCIATE-RJ.
	select {
	case got := <-reported:
		if got.CallingAETitle != "intruder" || got.CalledAETitle != "ACCEPTOR" || got.Rejection != *RejectCallingAETitle() {
			t.Fatalf("reported rejection = %+v", got)
		}
		if host, _, err := net.SplitHostPort(got.RemoteAddr.String()); err != nil || host != "127.0.0.1" {
			t.Fatalf("reported remote address = %v", got.RemoteAddr)
		}
	default:
		t.Fatal("OnAssociationRejection had not run when the peer observed the rejection")
	}
	<-serverErr
}

func TestOnAssociationRejectionOmitsInvalidAETitles(t *testing.T) {
	var reported []RejectedAssociation
	opts := AcceptOptions{OnAssociationRejection: func(rejected RejectedAssociation) { reported = append(reported, rejected) }}
	opts.rejectedRequest = &AssociationRQ{CallingAETitle: "BAD\x01TITLE", CalledAETitle: "  ACCEPTOR  "}
	notifyAssociationRejection(opts, nil, RejectCallingAETitle())
	if len(reported) != 1 || reported[0].CallingAETitle != "" || reported[0].CalledAETitle != "ACCEPTOR" || reported[0].RemoteAddr != nil {
		t.Fatalf("reported = %+v", reported)
	}
	// A panicking observer must not disturb the rejection path.
	opts.OnAssociationRejection = func(RejectedAssociation) { panic("observer") }
	notifyAssociationRejection(opts, nil, RejectCallingAETitle())
}

func TestAssociationServerReportsSaturationRejection(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	listener, err := Listen(ListenOptions{Address: "127.0.0.1:0", Context: ctx})
	if err != nil {
		t.Fatalf("Listen() error = %v", err)
	}
	reported := make(chan RejectedAssociation, 1)
	release := make(chan struct{})
	server, err := NewAssociationServer(listener, AssociationServerOptions{
		Accept: AcceptOptions{
			AETitle:                   "ACCEPTOR",
			SupportedAbstractSyntaxes: []string{verificationSOPClassUID},
			SupportedTransferSyntaxes: []string{ImplicitVRLittleEndian},
			OnAssociationRejection:    func(rejected RejectedAssociation) { reported <- rejected },
		},
		MaxConcurrentAssociations: 1,
		SaturationPolicy:          SaturationReject,
		Handler: func(ctx context.Context, assoc *Association) error {
			select {
			case <-release:
			case <-ctx.Done():
			}
			return nil
		},
	})
	if err != nil {
		t.Fatalf("NewAssociationServer() error = %v", err)
	}
	done := make(chan error, 1)
	go func() { done <- server.Serve(ctx) }()
	dial := func() (*Association, error) {
		return DialContext(ctx, listener.Addr().String(), DialOptions{
			CallingAETitle: "REQUESTOR",
			CalledAETitle:  "ACCEPTOR",
			Contexts:       []PresentationContext{{AbstractSyntaxUID: verificationSOPClassUID, TransferSyntaxUIDs: []string{ImplicitVRLittleEndian}}},
		})
	}
	first, err := dial()
	if err != nil {
		t.Fatalf("first association: %v", err)
	}
	_, err = dial()
	var reject *RejectionError
	if !errors.As(err, &reject) || reject.Reason != AssociateRJReasonLocalLimitExceeded {
		t.Fatalf("second association error = %v, want local-limit-exceeded", err)
	}
	select {
	case got := <-reported:
		if got.CallingAETitle != "REQUESTOR" || got.Rejection.Reason != AssociateRJReasonLocalLimitExceeded || got.RemoteAddr == nil {
			t.Fatalf("reported rejection = %+v", got)
		}
	default:
		t.Fatal("the saturation rejection was not reported before the peer observed it")
	}
	close(release)
	_ = first.Close()
	cancel()
	<-done
}

func TestOnTLSHandshakeFailureReportsAPlainTextPeer(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	serverTLS, _ := testTLSConfigs(t)
	listener, err := Listen(ListenOptions{Address: "127.0.0.1:0", Context: ctx})
	if err != nil {
		t.Fatalf("Listen() error = %v", err)
	}
	cleanupClose(t, "listener", listener)
	reported := make(chan TLSHandshakeFailure, 1)
	serverErr := make(chan error, 1)
	go func() {
		_, err := listener.AcceptAssociation(AcceptOptions{
			Context:                   ctx,
			TLSConfig:                 serverTLS,
			SupportedAbstractSyntaxes: []string{verificationSOPClassUID},
			SupportedTransferSyntaxes: []string{ImplicitVRLittleEndian},
			OnTLSHandshakeFailure:     func(failure TLSHandshakeFailure) { reported <- failure },
		})
		serverErr <- err
	}()
	// A requestor without TLS sends its A-ASSOCIATE-RQ in plain text.
	if _, err := DialContext(ctx, listener.Addr().String(), DialOptions{
		Contexts: []PresentationContext{{AbstractSyntaxUID: verificationSOPClassUID, TransferSyntaxUIDs: []string{ImplicitVRLittleEndian}}},
	}); err == nil {
		t.Fatal("a plain-text association succeeded against a TLS acceptor")
	}
	if err := <-serverErr; err == nil {
		t.Fatal("AcceptAssociation() succeeded for a plain-text peer")
	}
	select {
	case failure := <-reported:
		if !failure.NotTLS || failure.RemoteAddr == nil || failure.Err == nil {
			t.Fatalf("reported failure = %+v", failure)
		}
	default:
		t.Fatal("the failed handshake was not reported")
	}
}
