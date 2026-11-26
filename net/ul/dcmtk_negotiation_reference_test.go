package ul

import (
	"bytes"
	"context"
	"crypto/tls"
	"encoding/binary"
	"errors"
	"fmt"
	"net"
	"testing"
	"time"
)

// Adapt the malformed nested-item cases from DCMTK dcmnet/tests/tparseassoc.cc
// and the worker reuse check from tpool.cc. Complete outer PDU lengths let the
// nested item reach the decoder, and a single slot exposes failure to reclaim it.
func TestDCMTKReferenceMalformedNegotiationReleasesPoolSlot(t *testing.T) {
	listener, err := Listen(ListenOptions{Address: "127.0.0.1:0"})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = listener.Close() })
	server, err := NewAssociationServer(listener, AssociationServerOptions{
		Accept: AcceptOptions{
			AETitle:                   "ARCHIVE",
			SupportedAbstractSyntaxes: []string{verificationSOPClassUID},
			SupportedTransferSyntaxes: []string{ImplicitVRLittleEndian},
		},
		MaxConcurrentAssociations: 1,
		Handler:                   releaseDCMTKReferenceAssociation,
	})
	if err != nil {
		t.Fatal(err)
	}
	stopDCMTKReferenceServer(t, server)

	identity := dcmtkReferenceItem(SubItemUserIdentity, []byte{UserIdentityUsername, 0, 0, 3, 's', 'c', 'u', 0, 0})
	validExtended := dcmtkReferenceItem(SubItemSopClassExtendedNegotiation, []byte{0, 5, '1', '.', '2', '.', '3', 1})
	validSyntax := dcmtkReferenceItem(ItemTransferSyntax, []byte(ImplicitVRLittleEndian))
	for _, tc := range []struct {
		name      string
		userItems []byte
		transfers []byte
		want      error
		abort     byte
	}{
		{"late truncated extended item", append(bytes.Clone(validExtended), SubItemSopClassExtendedNegotiation, 0, 0, 2, 0), validSyntax, ErrInvalidPDUItem, AbortReasonUnrecognizedPDUParameter},
		{"short extended item body", append(bytes.Clone(validExtended), SubItemSopClassExtendedNegotiation, 0, 0, 1, 0), validSyntax, ErrInvalidUserItem, AbortReasonUnrecognizedPDUParameter},
		{"extended UID crosses item boundary", append(bytes.Clone(validExtended), SubItemSopClassExtendedNegotiation, 0, 0, 3, 0, 2, '1'), validSyntax, ErrInvalidUserItem, AbortReasonUnrecognizedPDUParameter},
		{"duplicate username", bytes.Repeat(identity, 2), validSyntax, ErrInvalidUserItem, AbortReasonUnrecognizedPDUParameter},
		{"late truncated transfer syntax", nil, append(bytes.Clone(validSyntax), ItemTransferSyntax, 0, 0, 2, '1'), ErrInvalidPDUItem, AbortReasonUnrecognizedPDUParameter},
		{"missing transfer syntax", nil, nil, ErrMissingPDUField, AbortReasonInvalidPDUParameter},
	} {
		t.Run(tc.name, func(t *testing.T) {
			wire := dcmtkReferenceAssociate(t, tc.transfers, tc.userItems)
			if pdu, err := ReadPDU(bytes.NewReader(wire), DefaultMaxPDU); pdu != nil || !errors.Is(err, tc.want) {
				t.Fatalf("malformed RQ decoded as (%T, %v), want nil and %v", pdu, err, tc.want)
			}

			ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
			defer cancel()
			address := listener.Addr().String()
			peer, err := (&net.Dialer{}).DialContext(ctx, "tcp", address)
			if err != nil {
				t.Fatal(err)
			}
			defer func() { _ = peer.Close() }()
			deadline, _ := ctx.Deadline()
			if err := peer.SetDeadline(deadline); err != nil {
				t.Fatal(err)
			}
			if _, err := peer.Write(wire); err != nil {
				t.Fatalf("write malformed RQ: %v", err)
			}
			pdu, err := ReadPDU(peer, DefaultMaxPDU)
			if err != nil {
				t.Fatalf("read provider abort: %v", err)
			}
			abort, ok := pdu.(*AbortRQ)
			if !ok || abort.Source != AbortSourceServiceProvider || abort.Reason != tc.abort {
				t.Fatalf("reply = %#v, want provider abort reason %d", pdu, tc.abort)
			}
			// A real peer closes after the abort. The server may then release its
			// ARTIM wait and serve the next requester without a timer or sleep.
			_ = peer.Close()
			before := server.Snapshot().Accepted
			assoc, err := DialContext(ctx, address, dcmtkReferenceDialOptions("HEALTHY"))
			if err != nil {
				t.Fatalf("dial healthy peer after malformed RQ: %v", err)
			}
			defer func() { _ = assoc.Close() }()
			if err := assoc.Release(ctx); err != nil {
				t.Fatalf("release healthy association: %v", err)
			}
			if got := server.Snapshot().Accepted; got != before+1 {
				t.Fatalf("accepted associations = %d, want %d", got, before+1)
			}
		})
	}
}

// DCMTK dcmnet/tests/tparseassoc.cc exercises the length of a User Identity
// item's secondary field. Check the following real item and PDU to detect an
// incorrect cursor in either enclosing frame. Go's wire reader keeps the
// original fields for semantic validation during association admission.
func TestDCMTKReferenceUserIdentitySecondaryFieldFraming(t *testing.T) {
	for _, tc := range []struct {
		name         string
		identityType byte
		declared     uint16
		secondary    []byte
		want         error
	}{
		{"username empty secondary", UserIdentityUsername, 0, nil, nil},
		{"username with secondary bytes", UserIdentityUsername, 4, []byte{0x55, 0, 0, 9}, nil},
		{"password with binary secondary", UserIdentityUsernamePassword, 3, []byte{0xff, 0, 7}, nil},
		{"secondary declaration shorter than actual", UserIdentityUsernamePassword, 2, []byte{1, 2, 3}, ErrInvalidUserItem},
		{"secondary declaration longer than actual", UserIdentityUsernamePassword, 4, []byte{1, 2, 3}, ErrInvalidUserItem},
		{"secondary declaration crosses into the following item", UserIdentityUsername, 9, nil, ErrInvalidUserItem},
	} {
		t.Run(tc.name, func(t *testing.T) {
			identity := []byte{tc.identityType, 1, 0, 3, 's', 'c', 'u'}
			identity = append(identity, byte(tc.declared>>8), byte(tc.declared))
			identity = append(identity, tc.secondary...)
			userItems := dcmtkReferenceItem(SubItemUserIdentity, identity)
			userItems = append(userItems, dcmtkReferenceItem(SubItemImplementationVersionName, []byte("NEXT_ITEM"))...)
			wire := dcmtkReferenceAssociate(t, dcmtkReferenceItem(ItemTransferSyntax, []byte(ImplicitVRLittleEndian)), userItems)
			wire = append(wire, rawPDU(PDUReleaseRQ, []byte{0, 0, 0, 0})...)
			reader := bytes.NewReader(wire)
			pdu, err := ReadPDU(reader, DefaultMaxPDU)
			if !errors.Is(err, tc.want) {
				t.Fatalf("ReadPDU() = (%T, %v), want error %v", pdu, err, tc.want)
			}
			if tc.want == nil {
				rq, ok := pdu.(*AssociationRQ)
				if !ok || len(rq.UserInfo) != 3 {
					t.Fatalf("expected max length, identity, and version, got %#v", pdu)
				}
				got, ok := rq.UserInfo[1].(UserIdentityItem)
				if !ok || got.Type != tc.identityType || !got.PositiveResponseRequested || string(got.PrimaryField) != "scu" || !bytes.Equal(got.SecondaryField, tc.secondary) {
					t.Fatalf("identity = %#v, want complete primary and secondary fields", rq.UserInfo[1])
				}
				var wantValidation error
				if tc.identityType == UserIdentityUsername && len(tc.secondary) > 0 {
					wantValidation = ErrInvalidUserIdentity
				}
				if err := validateUserIdentityRequest(UserIdentityRequest(got)); !errors.Is(err, wantValidation) {
					t.Fatalf("identity validation = %v, want %v", err, wantValidation)
				}
				if got, ok := rq.UserInfo[2].(ImplementationVersionNameItem); !ok || got.Name != "NEXT_ITEM" {
					t.Fatalf("following item = %#v, want implementation version NEXT_ITEM", rq.UserInfo[2])
				}
			} else if pdu != nil {
				t.Fatalf("malformed identity negotiation exposed a partial %T", pdu)
			}
			next, err := ReadPDU(reader, DefaultMaxPDU)
			if _, ok := next.(*ReleaseRQ); err != nil || !ok || reader.Len() != 0 {
				t.Fatalf("following PDU = (%T, %v) with %d bytes left, want release and EOF", next, err, reader.Len())
			}
		})
	}
}

// DCMTK dcmtls/tests/tscuscptls.cc runs several TLS requesters on one SCP pool.
// Exercise Go's bounded pool with distinct authenticated identities and verify
// that a saturated requester cannot disturb either established association.
func TestDCMTKReferenceMutualTLSAssociationPool(t *testing.T) {
	fixture := newTLSFixture(t)
	second := fixture.pki.Issue(t, "second.test", "p256")
	serverTLS, err := ServerTLSConfig(TLSProfileOptions{
		Certificates: []tls.Certificate{fixture.server.Certificate},
		TrustedCAs:   fixture.pki.CAPool,
	})
	if err != nil {
		t.Fatal(err)
	}
	listener, err := Listen(ListenOptions{Address: "127.0.0.1:0"})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = listener.Close() })
	type authenticatedPeer struct{ ae, certificate string }
	observed := make(chan authenticatedPeer, 4)
	server, err := NewAssociationServer(listener, AssociationServerOptions{
		Accept: AcceptOptions{
			AETitle:                   "ARCHIVE",
			TLSConfig:                 serverTLS,
			SupportedAbstractSyntaxes: []string{verificationSOPClassUID},
			SupportedTransferSyntaxes: []string{ImplicitVRLittleEndian},
		},
		MaxConcurrentAssociations: 2,
		SaturationPolicy:          SaturationReject,
		Handler: func(ctx context.Context, assoc *Association) error {
			state, ok := assoc.TLSConnectionState()
			if !ok || len(state.PeerCertificates) != 1 {
				return fmt.Errorf("%s lacks its authenticated client certificate", assoc.CallingAETitle)
			}
			select {
			case observed <- authenticatedPeer{assoc.CallingAETitle, state.PeerCertificates[0].Subject.CommonName}:
			case <-ctx.Done():
				return ctx.Err()
			}
			return releaseDCMTKReferenceAssociation(ctx, assoc)
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	stopDCMTKReferenceServer(t, server)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	dial := func(ae string, certificate tls.Certificate) (*Association, error) {
		t.Helper()
		clientTLS, err := ClientTLSConfig(TLSProfileOptions{
			Certificates: []tls.Certificate{certificate},
			TrustedCAs:   fixture.pki.CAPool,
			ServerName:   "scp.test",
		})
		if err != nil {
			return nil, err
		}
		options := dcmtkReferenceDialOptions(ae)
		options.TLSConfig = clientTLS
		return DialContext(ctx, listener.Addr().String(), options)
	}
	first, err := dial("FIRST", fixture.client.Certificate)
	if err != nil {
		t.Fatalf("first authenticated association: %v", err)
	}
	defer func() { _ = first.Close() }()
	// Every successful RQ handler reports the peer identity before receiving its
	// release. Await both reports while holding both associations open.
	other, err := dial("SECOND", second.Certificate)
	if err != nil {
		t.Fatalf("second authenticated association: %v", err)
	}
	defer func() { _ = other.Close() }()
	want := map[string]string{"FIRST": "scu.test", "SECOND": "second.test"}
	for range 2 {
		select {
		case peer := <-observed:
			if cn, ok := want[peer.ae]; !ok || peer.certificate != cn {
				t.Fatalf("pool observed %+v, want one of %v", peer, want)
			}
			delete(want, peer.ae)
		case <-ctx.Done():
			t.Fatalf("authenticated pool handlers missing: %v", want)
		}
	}
	if snap := server.Snapshot(); snap.Active != 2 || snap.Accepted != 2 {
		t.Fatalf("active pool = %+v, want two authenticated associations", snap)
	}
	saturated, err := dial("THIRD", second.Certificate)
	if saturated != nil {
		_ = saturated.Close()
	}
	var rejection *RejectionError
	if !errors.As(err, &rejection) || rejection.PDU() != (AssociationRJ{
		Result: AssociateRJResultTransient,
		Source: AssociateRJSourceServiceProviderPresentation,
		Reason: AssociateRJReasonLocalLimitExceeded,
	}) {
		t.Fatalf("third authenticated dial = %v, want local-limit rejection", err)
	}
	for _, assoc := range []*Association{first, other} {
		if state, ok := assoc.TLSConnectionState(); !ok || len(state.PeerCertificates) != 1 || state.PeerCertificates[0].Subject.CommonName != "scp.test" {
			t.Fatalf("%s did not authenticate the SCP certificate", assoc.CallingAETitle)
		}
		if err := assoc.Release(ctx); err != nil {
			t.Fatalf("release %s after saturation: %v", assoc.CallingAETitle, err)
		}
	}
}

func dcmtkReferenceItem(itemType byte, body []byte) []byte {
	item := []byte{itemType, 0, 0, 0}
	binary.BigEndian.PutUint16(item[2:], uint16(len(body)))
	return append(item, body...)
}

func dcmtkReferenceAssociate(t *testing.T, transfers, userItems []byte) []byte {
	t.Helper()
	var fixed bytes.Buffer
	if err := writeAssociationFixed(&fixed, DefaultProtocolVersion, "ARCHIVE", "RAW_PEER"); err != nil {
		t.Fatal(err)
	}
	body := append(fixed.Bytes(), dcmtkReferenceItem(ItemApplicationContext, []byte(ApplicationContextName))...)
	pc := append([]byte{1, 0, 0, 0}, dcmtkReferenceItem(ItemAbstractSyntax, []byte(verificationSOPClassUID))...)
	pc = append(pc, transfers...)
	body = append(body, dcmtkReferenceItem(ItemPresentationContextRQ, pc)...)
	maxLength := binary.BigEndian.AppendUint32(nil, DefaultMaxPDU)
	user := append(dcmtkReferenceItem(SubItemMaxLength, maxLength), userItems...)
	body = append(body, dcmtkReferenceItem(ItemUserInformation, user)...)
	return rawPDU(PDUAssociateRQ, body)
}

func dcmtkReferenceDialOptions(callingAE string) DialOptions {
	return DialOptions{
		CalledAETitle:  "ARCHIVE",
		CallingAETitle: callingAE,
		Contexts: []PresentationContext{{
			AbstractSyntaxUID:  verificationSOPClassUID,
			TransferSyntaxUIDs: []string{ImplicitVRLittleEndian},
		}},
	}
}

func releaseDCMTKReferenceAssociation(ctx context.Context, assoc *Association) error {
	pdu, err := assoc.Receive(ctx)
	if err != nil {
		return err
	}
	if _, ok := pdu.(*ReleaseRQ); !ok {
		return fmt.Errorf("expected A-RELEASE-RQ, got %T", pdu)
	}
	return assoc.Send(ctx, &ReleaseRP{})
}

func stopDCMTKReferenceServer(t *testing.T, server *AssociationServer) {
	t.Helper()
	done := make(chan error, 1)
	go func() { done <- server.Serve(context.Background()) }()
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer cancel()
		if err := server.Shutdown(ctx); err != nil {
			t.Errorf("server shutdown: %v", err)
		}
		select {
		case err := <-done:
			if err != nil {
				t.Errorf("server serve: %v", err)
			}
		case <-ctx.Done():
			t.Error("association server did not stop")
		}
	})
}
