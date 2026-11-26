package ul

import (
	"bytes"
	"context"
	"errors"
	"testing"
	"time"
)

const (
	testStorageServiceClassUID = "1.2.840.10008.4.2"
	testCTImageStorageUID      = "1.2.840.10008.5.1.4.1.1.2"
	testPrivateStorageUID      = "1.2.826.0.1.3680043.10.543.1158"
)

// TestSopClassCommonExtendedNegotiationLayout checks the PS3.7 D.3.3.6
// layout byte by byte: item type 57H, Sub-Item-version 0, item length, then
// each UID with a two-byte length, the Related General SOP Class
// identification as length/UID pairs behind its own two-byte length, and a
// zero-length Reserved field.
func TestSopClassCommonExtendedNegotiationLayout(t *testing.T) {
	item := SopClassCommonExtendedNegotiationItem{
		SopClassUID:                "1.2.3",
		ServiceClassUID:            testStorageServiceClassUID,
		RelatedGeneralSOPClassUIDs: []string{testCTImageStorageUID, "1.2.4"},
	}
	var got bytes.Buffer
	if err := writeUserVariableItem(&got, item); err != nil {
		t.Fatal(err)
	}
	var want []byte
	u16 := func(v int) { want = append(want, byte(v>>8), byte(v)) }
	field := func(s string) { u16(len(s)); want = append(want, s...) }
	body := len("1.2.3") + len(testStorageServiceClassUID) + 2*3 + 2 + len(testCTImageStorageUID) + 2 + len("1.2.4")
	want = append(want, 0x57, 0x00)
	u16(body)
	field("1.2.3")
	field(testStorageServiceClassUID)
	u16(2 + len(testCTImageStorageUID) + 2 + len("1.2.4"))
	field(testCTImageStorageUID)
	field("1.2.4")
	if !bytes.Equal(got.Bytes(), want) {
		t.Fatalf("encoded\n% X\nwant\n% X", got.Bytes(), want)
	}
	items, err := readUserInformation(got.Bytes())
	if err != nil {
		t.Fatal(err)
	}
	decoded, ok := items[0].(SopClassCommonExtendedNegotiationItem)
	if !ok || decoded.SopClassUID != item.SopClassUID || decoded.ServiceClassUID != item.ServiceClassUID || len(decoded.RelatedGeneralSOPClassUIDs) != 2 || decoded.RelatedGeneralSOPClassUIDs[1] != "1.2.4" {
		t.Fatalf("decoded %#v", items[0])
	}

	// No related classes: the identification length is zero.
	got.Reset()
	if err := writeUserVariableItem(&got, SopClassCommonExtendedNegotiationItem{SopClassUID: "1.2.3", ServiceClassUID: "1.2.5"}); err != nil {
		t.Fatal(err)
	}
	if want := []byte{0x57, 0, 0, 16, 0, 5, '1', '.', '2', '.', '3', 0, 5, '1', '.', '2', '.', '5', 0, 0}; !bytes.Equal(got.Bytes(), want) {
		t.Fatalf("no related classes: % X", got.Bytes())
	}
}

func TestReadSopClassCommonExtendedNegotiation(t *testing.T) {
	valid := []byte{0, 3, '1', '.', '2', 0, 3, '1', '.', '5', 0, 5, 0, 3, '1', '.', '9'}
	tests := []struct {
		name    string
		data    []byte
		want    SopClassCommonExtendedNegotiationItem
		wantErr error
	}{
		{"valid", valid, SopClassCommonExtendedNegotiationItem{SopClassUID: "1.2", ServiceClassUID: "1.5", RelatedGeneralSOPClassUIDs: []string{"1.9"}}, nil},
		{"later version reserved field ignored", append(append([]byte(nil), valid...), 0xAB, 0xCD), SopClassCommonExtendedNegotiationItem{SopClassUID: "1.2", ServiceClassUID: "1.5", RelatedGeneralSOPClassUIDs: []string{"1.9"}}, nil},
		{"truncated SOP class", []byte{0, 9, '1'}, SopClassCommonExtendedNegotiationItem{}, ErrInvalidUserItem},
		{"missing service class", []byte{0, 3, '1', '.', '2'}, SopClassCommonExtendedNegotiationItem{}, ErrInvalidUserItem},
		{"empty service class", []byte{0, 3, '1', '.', '2', 0, 0, 0, 0}, SopClassCommonExtendedNegotiationItem{}, ErrMissingPDUField},
		{"related length overruns", []byte{0, 3, '1', '.', '2', 0, 3, '1', '.', '5', 0, 9, 0, 3}, SopClassCommonExtendedNegotiationItem{}, ErrInvalidUserItem},
		{"related sub-field overruns", []byte{0, 3, '1', '.', '2', 0, 3, '1', '.', '5', 0, 3, 0, 4, '1'}, SopClassCommonExtendedNegotiationItem{}, ErrInvalidUserItem},
		{"empty related UID", []byte{0, 3, '1', '.', '2', 0, 3, '1', '.', '5', 0, 2, 0, 0}, SopClassCommonExtendedNegotiationItem{}, ErrInvalidUserItem},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, err := readSopClassCommonExtendedNegotiation(tc.data)
			if tc.wantErr != nil {
				if !errors.Is(err, tc.wantErr) {
					t.Fatalf("err = %v, want %v", err, tc.wantErr)
				}
				return
			}
			if err != nil || got.SopClassUID != tc.want.SopClassUID || got.ServiceClassUID != tc.want.ServiceClassUID || len(got.RelatedGeneralSOPClassUIDs) != len(tc.want.RelatedGeneralSOPClassUIDs) {
				t.Fatalf("got %#v, %v", got, err)
			}
		})
	}
}

func TestDialOptionsValidateCommonExtendedNegotiation(t *testing.T) {
	base := DialOptions{
		CalledAETitle: "SCP", CallingAETitle: "SCU",
		Contexts: []PresentationContext{{AbstractSyntaxUID: testPrivateStorageUID, TransferSyntaxUIDs: []string{ImplicitVRLittleEndian}}},
	}
	item := SopClassCommonExtendedNegotiationItem{SopClassUID: testPrivateStorageUID, ServiceClassUID: testStorageServiceClassUID, RelatedGeneralSOPClassUIDs: []string{testCTImageStorageUID}}
	tests := []struct {
		name    string
		items   []SopClassCommonExtendedNegotiationItem
		wantErr bool
	}{
		{"valid", []SopClassCommonExtendedNegotiationItem{item}, false},
		{"not proposed", []SopClassCommonExtendedNegotiationItem{{SopClassUID: testCTImageStorageUID, ServiceClassUID: testStorageServiceClassUID}}, true},
		{"duplicate", []SopClassCommonExtendedNegotiationItem{item, item}, true},
		{"missing service class", []SopClassCommonExtendedNegotiationItem{{SopClassUID: testPrivateStorageUID}}, true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			opts := base
			opts.CommonExtendedNegotiation = tc.items
			if err := validateDialOptions(opts); (err != nil) != tc.wantErr {
				t.Fatalf("validateDialOptions = %v, want error %v", err, tc.wantErr)
			}
		})
	}
}

// TestAcceptRelatedGeneralSOPClass negotiates a private Storage SOP Class
// whose Related General SOP Class is CT Image Storage with an acceptor that
// supports only CT Image Storage.
func TestAcceptRelatedGeneralSOPClass(t *testing.T) {
	refuseOtherServices := func(item SopClassCommonExtendedNegotiationItem, _ string) bool {
		return item.ServiceClassUID == testStorageServiceClassUID
	}
	tests := []struct {
		name          string
		accept        AcceptOptions
		serviceClass  string
		wantAccepted  bool
		wantTransfer  string
		wantRoleReply bool
	}{
		{name: "no handler rejects", accept: AcceptOptions{SupportedAbstractSyntaxes: []string{testCTImageStorageUID}}, serviceClass: testStorageServiceClassUID},
		{name: "related class accepted", accept: AcceptOptions{SupportedAbstractSyntaxes: []string{testCTImageStorageUID}, CommonExtendedNegotiationHandler: AcceptRelatedGeneralSOPClasses}, serviceClass: testStorageServiceClassUID, wantAccepted: true, wantTransfer: ImplicitVRLittleEndian},
		{name: "handler refuses service class", accept: AcceptOptions{SupportedAbstractSyntaxes: []string{testCTImageStorageUID}, CommonExtendedNegotiationHandler: refuseOtherServices}, serviceClass: "1.2.840.10008.4.99"},
		{name: "related class unsupported", accept: AcceptOptions{SupportedAbstractSyntaxes: []string{"1.2.840.10008.5.1.4.1.1.6.1"}, CommonExtendedNegotiationHandler: AcceptRelatedGeneralSOPClasses}, serviceClass: testStorageServiceClassUID},
		{name: "related class rule gives transfer syntax and role", accept: AcceptOptions{
			PresentationContextRules:         []PresentationContextRule{{AbstractSyntaxUID: testCTImageStorageUID, TransferSyntaxes: []string{ExplicitVRLittleEndian}, Roles: &RequestorRoles{SCU: true}}},
			CommonExtendedNegotiationHandler: AcceptRelatedGeneralSOPClasses,
		}, serviceClass: testStorageServiceClassUID, wantAccepted: true, wantTransfer: ExplicitVRLittleEndian, wantRoleReply: true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			listener, err := Listen(ListenOptions{Address: "127.0.0.1:0", Context: ctx})
			if err != nil {
				t.Fatal(err)
			}
			defer listener.Close()
			type accepted struct {
				assoc *Association
				err   error
			}
			done := make(chan accepted, 1)
			go func() {
				opts := tc.accept
				opts.AETitle, opts.Context = "SCP", ctx
				if opts.SupportedTransferSyntaxes == nil {
					opts.SupportedTransferSyntaxes = []string{ImplicitVRLittleEndian}
				}
				assoc, err := listener.AcceptAssociation(opts)
				done <- accepted{assoc, err}
			}()
			common := SopClassCommonExtendedNegotiationItem{
				SopClassUID:                testPrivateStorageUID,
				ServiceClassUID:            tc.serviceClass,
				RelatedGeneralSOPClassUIDs: []string{"1.2.840.10008.5.1.4.1.1.4", testCTImageStorageUID},
			}
			client, dialErr := Dial(listener.Addr().String(), DialOptions{
				CalledAETitle: "SCP", CallingAETitle: "SCU", Context: ctx,
				Contexts:                  []PresentationContext{{AbstractSyntaxUID: testPrivateStorageUID, TransferSyntaxUIDs: []string{ExplicitVRLittleEndian, ImplicitVRLittleEndian}}},
				RoleSelections:            []RoleSelectionItem{{SopClassUID: testPrivateStorageUID, SCURole: true}},
				CommonExtendedNegotiation: []SopClassCommonExtendedNegotiationItem{common},
			})
			server := <-done
			if !tc.wantAccepted {
				if !errors.Is(dialErr, ErrAssociationRejected) || !errors.Is(server.err, ErrNoAcceptedPresentationContexts) {
					t.Fatalf("dial %v, accept %v; want the context rejected", dialErr, server.err)
				}
				return
			}
			if dialErr != nil || server.err != nil {
				t.Fatalf("dial %v, accept %v", dialErr, server.err)
			}
			defer client.Close()
			defer server.assoc.Close()
			if len(server.assoc.RequestedCommonExtendedNegotiation) != 1 || server.assoc.RequestedCommonExtendedNegotiation[0].ServiceClassUID != tc.serviceClass {
				t.Fatalf("acceptor saw %+v", server.assoc.RequestedCommonExtendedNegotiation)
			}
			if len(client.RequestedCommonExtendedNegotiation) != 1 {
				t.Fatalf("requestor kept %+v", client.RequestedCommonExtendedNegotiation)
			}
			got := server.assoc.AcceptedContexts[0]
			if got.AbstractSyntaxUID != testPrivateStorageUID || got.RelatedGeneralSOPClassUID != testCTImageStorageUID || got.TransferSyntaxUID != tc.wantTransfer {
				t.Fatalf("acceptor context %+v", got)
			}
			if client.AcceptedContexts[0].TransferSyntaxUID != tc.wantTransfer || client.AcceptedContexts[0].RelatedGeneralSOPClassUID != "" {
				t.Fatalf("requestor context %+v", client.AcceptedContexts[0])
			}
			if gotRole := len(client.AcceptedRoleSelections) == 1 && client.AcceptedRoleSelections[0].SCURole; gotRole != tc.wantRoleReply {
				t.Fatalf("role reply %+v, want %v", client.AcceptedRoleSelections, tc.wantRoleReply)
			}
		})
	}
}
