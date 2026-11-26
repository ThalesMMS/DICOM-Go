package ul

import (
	"bytes"
	"context"
	"errors"
	"math"
	"testing"
	"time"
)

func TestUserIdentityResponseWireLengthBoundaries(t *testing.T) {
	// PS3.7 D.3.3.7.2 and PS3.8 9.3.3.3 use separate 16-bit lengths for
	// the response, its sub-item and the enclosing User Information item.
	const responseOverhead = 2
	const subItemHeader = 4
	const maxLengthSubItem = subItemHeader + 4
	const maxResponse = math.MaxUint16 - maxLengthSubItem - subItemHeader - responseOverhead
	for _, tc := range []struct {
		name string
		size int
		want error
	}{
		{name: "empty positive acknowledgment"},
		{name: "binary response", size: 4},
		{name: "exact user information boundary", size: maxResponse},
		{name: "user information overflow", size: maxResponse + 1, want: ErrLengthOverflow},
		{name: "identity sub-item overflow", size: math.MaxUint16 - responseOverhead + 1, want: ErrLengthOverflow},
		{name: "identity payload overflow", size: math.MaxUint16 + 1, want: ErrLengthOverflow},
	} {
		t.Run(tc.name, func(t *testing.T) {
			payload := bytes.Repeat([]byte{0x00, 0xfe, 0xff}, (tc.size+2)/3)[:tc.size]
			pdu := &AssociationAC{
				ProtocolVersion:        DefaultProtocolVersion,
				CalledAETitle:          "PACS",
				CallingAETitle:         "WORKSTATION",
				ApplicationContextName: ApplicationContextName,
				PresentationContexts:   []PresentationContextResult{{ID: 1, Result: PresentationContextAcceptance, TransferSyntaxUID: ImplicitVRLittleEndian}},
				UserInfo:               []UserVariableItem{MaxLengthItem{Value: DefaultMaxPDU}, UserIdentityResponseItem{ServerResponse: payload}},
			}
			var wire bytes.Buffer
			if err := WritePDU(&wire, pdu); !errors.Is(err, tc.want) {
				t.Fatalf("WritePDU() error = %v, want %v", err, tc.want)
			}
			if tc.want != nil {
				if wire.Len() != 0 {
					t.Fatal("encoder emitted a partial association PDU")
				}
				return
			}
			got, err := ReadPDU(&wire, DefaultMaxPDU)
			if err != nil {
				t.Fatal(err)
			}
			ac, ok := got.(*AssociationAC)
			if !ok || len(ac.UserInfo) != 2 || wire.Len() != 0 {
				t.Fatalf("unexpected PDU %T or incomplete consumption", got)
			}
			id, ok := ac.UserInfo[1].(UserIdentityResponseItem)
			if !ok || !bytes.Equal(id.ServerResponse, payload) {
				t.Fatalf("decoded identity response length = %d, want %d with exact payload", len(id.ServerResponse), len(payload))
			}
		})
	}
}

func TestReadAssociationRejectsMalformedPeerNegotiation(t *testing.T) {
	for _, tc := range []struct {
		name             string
		pduType          PDUType
		contextIDs       []byte
		identityResponse []byte
		duplicateReply   bool
		want             error
	}{
		{name: "request with odd endpoints", pduType: PDUAssociateRQ, contextIDs: []byte{1, 255}},
		{name: "request with zero context ID", pduType: PDUAssociateRQ, contextIDs: []byte{0}, want: ErrInvalidPCID},
		// PS3.8 9.3.2.2 asks requestors for odd IDs, but a peer's even ID
		// (dcm4che stgcmtscu proposes 2) is tolerated rather than aborting.
		{name: "request with even context ID", pduType: PDUAssociateRQ, contextIDs: []byte{2}},
		{name: "request with odd and even context IDs", pduType: PDUAssociateRQ, contextIDs: []byte{1, 2, 254}},
		{name: "request with repeated ID", pduType: PDUAssociateRQ, contextIDs: []byte{1, 255, 1}, want: ErrInvalidPCID},
		{name: "request with repeated even ID", pduType: PDUAssociateRQ, contextIDs: []byte{2, 2}, want: ErrInvalidPCID},
		{name: "accept echoing an even ID", pduType: PDUAssociateAC, contextIDs: []byte{2}},
		{name: "accept with zero context ID", pduType: PDUAssociateAC, contextIDs: []byte{0}, want: ErrInvalidPCID},
		{name: "accept with repeated ID", pduType: PDUAssociateAC, contextIDs: []byte{255, 1, 255}, want: ErrInvalidPCID},
		{name: "accept with empty identity reply", pduType: PDUAssociateAC, identityResponse: []byte{0, 0}},
		{name: "accept with truncated response length", pduType: PDUAssociateAC, identityResponse: []byte{0}, want: ErrInvalidUserItem},
		{name: "accept with declared response larger than item", pduType: PDUAssociateAC, identityResponse: []byte{0, 2, 0xfe}, want: ErrInvalidUserItem},
		{name: "accept with trailing response bytes", pduType: PDUAssociateAC, identityResponse: []byte{0, 0, 0xfe}, want: ErrInvalidUserItem},
		{name: "accept with repeated identity reply", pduType: PDUAssociateAC, identityResponse: []byte{0, 0}, duplicateReply: true, want: ErrInvalidUserItem},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var body bytes.Buffer
			if err := writeAssociationFixed(&body, DefaultProtocolVersion, "PACS", "WORKSTATION"); err != nil {
				t.Fatal(err)
			}
			if err := writeApplicationContext(&body, ApplicationContextName); err != nil {
				t.Fatal(err)
			}
			ids := tc.contextIDs
			if ids == nil {
				ids = []byte{1}
			}
			for _, id := range ids {
				// Write raw item headers so the malformed IDs reach the decoder.
				var item bytes.Buffer
				item.Write([]byte{id, 0, PresentationContextAcceptance, 0})
				itemType := ItemPresentationContextAC
				if tc.pduType == PDUAssociateRQ {
					itemType = ItemPresentationContextRQ
					if err := writeItem(&item, ItemAbstractSyntax, []byte(verificationSOPClassUID)); err != nil {
						t.Fatal(err)
					}
				}
				if err := writeItem(&item, ItemTransferSyntax, []byte(ImplicitVRLittleEndian)); err != nil {
					t.Fatal(err)
				}
				if err := writeItem(&body, itemType, item.Bytes()); err != nil {
					t.Fatal(err)
				}
			}
			var user bytes.Buffer
			if err := writeMaxLengthItem(&user, &MaxLengthItem{Value: DefaultMaxPDU}); err != nil {
				t.Fatal(err)
			}
			if tc.identityResponse != nil {
				if err := writeItem(&user, SubItemUserIdentityResponse, tc.identityResponse); err != nil {
					t.Fatal(err)
				}
				if tc.duplicateReply {
					if err := writeItem(&user, SubItemUserIdentityResponse, tc.identityResponse); err != nil {
						t.Fatal(err)
					}
				}
			}
			if err := writeItem(&body, ItemUserInformation, user.Bytes()); err != nil {
				t.Fatal(err)
			}
			got, err := ReadPDU(bytes.NewReader(rawPDU(tc.pduType, body.Bytes())), DefaultMaxPDU)
			if !errors.Is(err, tc.want) || (got == nil) != (tc.want != nil) {
				t.Fatalf("ReadPDU() = (%T, %v), want error %v", got, err, tc.want)
			}
		})
	}
}

func TestReleasePreservesPeerAbortAfterPendingPData(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	conn := &deadlineTrackingConn{readData: pduStream(t,
		&PDataTF{Values: []PDataValue{{PresentationContextID: 1, IsLast: true, Data: []byte{0, 1}}}},
		&AbortRQ{Source: AbortSourceServiceProvider, Reason: AbortReasonUnexpectedPDU},
	)}
	assoc := &Association{Conn: conn, MaxPDU: DefaultMaxPDU, Context: ctx}

	err := assoc.Release(ctx)
	var aborted *AbortError
	if !errors.As(err, &aborted) || aborted.Source != AbortSourceServiceProvider || aborted.Reason != AbortReasonUnexpectedPDU {
		t.Fatalf("Release() error = %v, want typed peer abort with source and reason", err)
	}
	if assoc.Conn != nil {
		t.Fatal("Release() left the connection open after peer abort")
	}
	pdus := writtenPDUs(t, conn)
	if len(pdus) != 1 {
		t.Fatalf("sent PDUs = %#v, want only A-RELEASE-RQ", pdus)
	}
	if _, ok := pdus[0].(*ReleaseRQ); !ok {
		t.Fatalf("sent PDU = %T, want A-RELEASE-RQ", pdus[0])
	}
}
