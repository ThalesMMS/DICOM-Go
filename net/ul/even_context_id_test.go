package ul

import (
	"bytes"
	"context"
	"errors"
	"net"
	"testing"
	"time"
)

// rawAssociateRQ encodes an A-ASSOCIATE-RQ whose presentation context IDs
// are written verbatim, so a test can propose the even IDs the encoder
// refuses to emit. abstractSyntaxes[i] is the abstract syntax of ids[i].
func rawAssociateRQ(t *testing.T, ids []byte, abstractSyntaxes []string) []byte {
	t.Helper()
	var body bytes.Buffer
	if err := writeAssociationFixed(&body, DefaultProtocolVersion, "ARCHIVE", "MODALITY"); err != nil {
		t.Fatal(err)
	}
	if err := writeApplicationContext(&body, ApplicationContextName); err != nil {
		t.Fatal(err)
	}
	for i, id := range ids {
		var item bytes.Buffer
		item.Write([]byte{id, 0, 0, 0})
		if err := writeItem(&item, ItemAbstractSyntax, []byte(abstractSyntaxes[i])); err != nil {
			t.Fatal(err)
		}
		if err := writeItem(&item, ItemTransferSyntax, []byte(ImplicitVRLittleEndian)); err != nil {
			t.Fatal(err)
		}
		if err := writeItem(&body, ItemPresentationContextRQ, item.Bytes()); err != nil {
			t.Fatal(err)
		}
	}
	if err := writeUserInformation(&body, implementationUserInfo(DefaultMaxPDU, "", "")); err != nil {
		t.Fatal(err)
	}
	return rawPDU(PDUAssociateRQ, body.Bytes())
}

// TestAcceptorEchoesEvenPresentationContextID covers a requestor that
// proposes even presentation context IDs, as dcm4che 5.35.2 stgcmtscu does
// with ID 2. The acceptor negotiates each context instead of aborting,
// echoes the peer's IDs in the A-ASSOCIATE-AC and carries P-DATA on the
// accepted even context in both directions.
func TestAcceptorEchoesEvenPresentationContextID(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	client, server := net.Pipe()
	defer func() { _ = client.Close() }()
	defer func() { _ = server.Close() }()

	type outcome struct {
		accepted []AcceptedContext
		request  *PDataTF
		err      error
	}
	done := make(chan outcome, 1)
	go func() {
		assoc, err := Accept(server, AcceptOptions{
			Context:                   ctx,
			AETitle:                   "ARCHIVE",
			SupportedAbstractSyntaxes: []string{verificationSOPClassUID},
			SupportedTransferSyntaxes: []string{ImplicitVRLittleEndian},
		})
		if err != nil {
			done <- outcome{err: err}
			return
		}
		defer assoc.Close()
		pdu, err := assoc.Receive(ctx)
		if err != nil {
			done <- outcome{accepted: assoc.AcceptedContexts, err: err}
			return
		}
		request, ok := pdu.(*PDataTF)
		if !ok {
			done <- outcome{accepted: assoc.AcceptedContexts, err: UnexpectedPDUError(pdu, "waiting for P-DATA-TF")}
			return
		}
		reply := &PDataTF{Values: []PDataValue{{PresentationContextID: request.Values[0].PresentationContextID, IsCommand: true, IsLast: true, Data: []byte{0xca, 0xfe}}}}
		done <- outcome{accepted: assoc.AcceptedContexts, request: request, err: assoc.Send(ctx, reply)}
	}()

	// Context 2 is supported; context 4 is not and must be refused alone.
	if _, err := client.Write(rawAssociateRQ(t, []byte{2, 4}, []string{verificationSOPClassUID, "1.2.3.4"})); err != nil {
		t.Fatalf("write A-ASSOCIATE-RQ: %v", err)
	}
	reply, err := ReadPDU(client, DefaultMaxPDU)
	if err != nil {
		t.Fatalf("read A-ASSOCIATE reply: %v", err)
	}
	ac, ok := reply.(*AssociationAC)
	if !ok {
		t.Fatalf("reply = %#v, want A-ASSOCIATE-AC", reply)
	}
	want := []PresentationContextResult{
		{ID: 2, Result: PresentationContextAcceptance, TransferSyntaxUID: ImplicitVRLittleEndian},
		{ID: 4, Result: PresentationContextAbstractSyntaxNotSupported, TransferSyntaxUID: ImplicitVRLittleEndian},
	}
	if len(ac.PresentationContexts) != len(want) {
		t.Fatalf("A-ASSOCIATE-AC results = %#v, want %#v", ac.PresentationContexts, want)
	}
	for i := range want {
		if got := ac.PresentationContexts[i]; got.ID != want[i].ID || got.Result != want[i].Result || (want[i].Result == PresentationContextAcceptance && got.TransferSyntaxUID != want[i].TransferSyntaxUID) {
			t.Fatalf("A-ASSOCIATE-AC result %d = %#v, want %#v", i, got, want[i])
		}
	}

	if err := WritePDU(client, &PDataTF{Values: []PDataValue{{PresentationContextID: 2, IsCommand: true, IsLast: true, Data: []byte{0xbe, 0xef}}}}); err != nil {
		t.Fatalf("write P-DATA-TF on context 2: %v", err)
	}
	echoed, err := ReadPDU(client, DefaultMaxPDU)
	if err != nil {
		t.Fatalf("read P-DATA-TF reply: %v", err)
	}
	data, ok := echoed.(*PDataTF)
	if !ok || len(data.Values) != 1 || data.Values[0].PresentationContextID != 2 || !bytes.Equal(data.Values[0].Data, []byte{0xca, 0xfe}) {
		t.Fatalf("P-DATA-TF reply = %#v, want one value on context 2", echoed)
	}

	result := <-done
	if result.err != nil {
		t.Fatalf("acceptor: %v", result.err)
	}
	if len(result.accepted) != 1 || result.accepted[0].ID != 2 || result.accepted[0].AbstractSyntaxUID != verificationSOPClassUID {
		t.Fatalf("accepted contexts = %#v, want only context 2", result.accepted)
	}
	if v := result.request.Values; len(v) != 1 || v[0].PresentationContextID != 2 || !bytes.Equal(v[0].Data, []byte{0xbe, 0xef}) {
		t.Fatalf("P-DATA-TF received by the acceptor = %#v", result.request)
	}
}

// TestRequestorProposesOnlyOddPresentationContextIDs keeps the requestor on
// PS3.8 9.3.2.2 while the acceptor tolerates even IDs from peers.
func TestRequestorProposesOnlyOddPresentationContextIDs(t *testing.T) {
	contexts := make([]PresentationContext, MaxPresentationContexts)
	for i := range contexts {
		contexts[i] = PresentationContext{AbstractSyntaxUID: verificationSOPClassUID, TransferSyntaxUIDs: []string{ImplicitVRLittleEndian}}
	}
	rq := buildAssociateRQ(DialOptions{CalledAETitle: "ARCHIVE", CallingAETitle: "MODALITY", Contexts: contexts})
	if len(rq.PresentationContexts) != MaxPresentationContexts {
		t.Fatalf("proposed %d contexts, want %d", len(rq.PresentationContexts), MaxPresentationContexts)
	}
	for i, pc := range rq.PresentationContexts {
		if pc.ID%2 != 1 || pc.ID != byte(2*i+1) {
			t.Fatalf("proposed context %d has ID %d, want %d", i, pc.ID, 2*i+1)
		}
	}
	// The encoder refuses an even proposal built by hand.
	rq.PresentationContexts = rq.PresentationContexts[:1]
	rq.PresentationContexts[0].ID = 2
	if err := WritePDU(&bytes.Buffer{}, &rq); !errors.Is(err, ErrInvalidPCID) {
		t.Fatalf("WritePDU(even proposal) error = %v, want ErrInvalidPCID", err)
	}
}

// TestRequestorRejectsUnproposedEvenResultID covers an acceptor that answers
// with an even ID the requestor never proposed: the decoder tolerates the
// ID, and negotiation refuses the reply as a result for an unknown context.
func TestRequestorRejectsUnproposedEvenResultID(t *testing.T) {
	opts := normalizeDialOptions(DialOptions{
		CalledAETitle: "ARCHIVE", CallingAETitle: "MODALITY",
		Contexts: []PresentationContext{{AbstractSyntaxUID: verificationSOPClassUID, TransferSyntaxUIDs: []string{ImplicitVRLittleEndian}}},
	})
	proposed, err := proposedContexts(opts.Contexts)
	if err != nil {
		t.Fatal(err)
	}
	ac := &AssociationAC{
		ProtocolVersion:        opts.ProtocolVersion,
		CalledAETitle:          "ARCHIVE",
		CallingAETitle:         "MODALITY",
		ApplicationContextName: opts.ApplicationContextName,
		PresentationContexts:   []PresentationContextResult{{ID: 2, Result: PresentationContextAcceptance, TransferSyntaxUID: ImplicitVRLittleEndian}},
	}
	if _, _, err := processAssociationAC(ac, proposed, opts); !errors.Is(err, ErrInvalidPDUField) {
		t.Fatalf("processAssociationAC(unproposed even ID) error = %v, want ErrInvalidPDUField", err)
	}
}
