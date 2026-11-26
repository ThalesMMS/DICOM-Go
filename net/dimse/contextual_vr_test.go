package dimse

import (
	"context"
	"errors"
	"io"
	"testing"
	"time"

	"github.com/ThalesMMS/dicom-go/core"
	"github.com/ThalesMMS/dicom-go/net/ul"
	"github.com/ThalesMMS/dicom-go/object"
	"github.com/ThalesMMS/dicom-go/transfer"
)

// Over Implicit VR Little Endian the receiver resolves "US or SS" from the
// sender's Pixel Representation, and a sender never transmits a value that its
// VR cannot represent.
func TestDIMSEImplicitContextualVRAndUnrepresentableValues(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	listener, err := ul.Listen(ul.ListenOptions{Address: "127.0.0.1:0", Context: ctx})
	if err != nil {
		t.Fatal(err)
	}
	defer closeOrFail(t, "listener", listener)
	const sopClass = "1.2.840.10008.5.1.4.1.1.7"
	received := make(chan *object.Object, 1)
	serverDone := make(chan error, 1)
	go func() {
		serverDone <- func() error {
			assoc, err := listener.AcceptAssociation(ul.AcceptOptions{
				AETitle: "STORESCP", Context: ctx,
				SupportedAbstractSyntaxes: []string{sopClass},
				SupportedTransferSyntaxes: []string{transfer.ImplicitVRLittleEndian.UID},
			})
			if err != nil {
				return err
			}
			defer func() { _ = assoc.Close() }()
			pcID, req, err := ReceiveCStoreRequestAny(assoc)
			if err != nil {
				return err
			}
			dataset, err := ReceiveDataSet(assoc, pcID, transfer.ImplicitVRLittleEndian)
			if err != nil {
				return err
			}
			received <- dataset
			if err := SendCStoreResponse(assoc, pcID, CStoreResponse{
				AffectedSOPClassUID: req.AffectedSOPClassUID, MessageIDBeingRespondedTo: req.MessageID,
				AffectedSOPInstanceUID: req.AffectedSOPInstanceUID, Status: StatusSuccess,
			}); err != nil {
				return err
			}
			if pdu, err := assoc.ReadPDU(); err != nil {
				return err
			} else if _, ok := pdu.(*ul.ReleaseRQ); !ok {
				return errors.New("expected A-RELEASE-RQ")
			}
			return assoc.WritePDU(&ul.ReleaseRP{})
		}()
	}()
	assoc, err := ul.DialContext(ctx, listener.Addr().String(), ul.DialOptions{
		CalledAETitle: "STORESCP", CallingAETitle: "STORESCU",
		Contexts: []ul.PresentationContext{{AbstractSyntaxUID: sopClass, TransferSyntaxUIDs: []string{transfer.ImplicitVRLittleEndian.UID}}},
	})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = assoc.Close() }()
	pcID := assoc.AcceptedContexts[0].ID
	u16 := func(group, element, v uint16) core.Element {
		return core.Element{Header: core.ElementHeader{Tag: core.NewTag(group, element), VR: core.VRUS}, Value: core.Uint16Value{v}}
	}
	uid := func(element uint16, v string) core.Element {
		return core.Element{Header: core.ElementHeader{Tag: core.NewTag(0x0008, element), VR: core.VRUI}, Value: core.StringValue{v}}
	}
	base := []core.Element{
		uid(0x0016, sopClass), uid(0x0018, "1.2.826.0.1.3680043.10.543.938.40"),
		u16(0x0028, 0x0103, 1),
		{Header: core.ElementHeader{Tag: core.NewTag(0x0028, 0x0106), VR: core.VRSS}, Value: core.Int16Value{-2000}},
	}

	// A value US cannot represent fails before the dataset is completed.
	invalid := object.FromElements(append(append([]core.Element(nil), base...), u16(0x0028, 0x0010, 0)), nil)
	invalid.Put(core.Element{Header: core.ElementHeader{Tag: core.NewTag(0x0028, 0x0010), VR: core.VRUS}, Value: core.Uint32Value{70000}})
	if err := SendCStoreRequest(assoc, pcID, CStoreRequest{AffectedSOPClassUID: sopClass, MessageID: 1, AffectedSOPInstanceUID: "1.2.826.0.1.3680043.10.543.938.40"}); err != nil {
		t.Fatal(err)
	}
	if err := SendDataSet(assoc, pcID, invalid, transfer.ImplicitVRLittleEndian); !errors.Is(err, core.ErrValueNotRepresentable) {
		t.Fatalf("invalid send error = %v", err)
	}
	_ = assoc.Close()
	if err := <-serverDone; !errors.Is(err, io.ErrUnexpectedEOF) {
		t.Fatalf("receiver error = %v, want truncated-message error", err)
	}
	select {
	case <-received:
		t.Fatal("partial dataset delivered")
	default:
	}

	// A fresh association carries the valid dataset.
	serverDone = make(chan error, 1)
	go func() {
		serverDone <- func() error {
			assoc, err := listener.AcceptAssociation(ul.AcceptOptions{
				AETitle: "STORESCP", Context: ctx,
				SupportedAbstractSyntaxes: []string{sopClass},
				SupportedTransferSyntaxes: []string{transfer.ImplicitVRLittleEndian.UID},
			})
			if err != nil {
				return err
			}
			defer func() { _ = assoc.Close() }()
			pcID, _, err := ReceiveCStoreRequestAny(assoc)
			if err != nil {
				return err
			}
			dataset, err := ReceiveDataSet(assoc, pcID, transfer.ImplicitVRLittleEndian)
			if err != nil {
				return err
			}
			received <- dataset
			return nil
		}()
	}()
	assoc, err = ul.DialContext(ctx, listener.Addr().String(), ul.DialOptions{
		CalledAETitle: "STORESCP", CallingAETitle: "STORESCU",
		Contexts: []ul.PresentationContext{{AbstractSyntaxUID: sopClass, TransferSyntaxUIDs: []string{transfer.ImplicitVRLittleEndian.UID}}},
	})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = assoc.Close() }()
	pcID = assoc.AcceptedContexts[0].ID
	if err := SendCStoreRequest(assoc, pcID, CStoreRequest{AffectedSOPClassUID: sopClass, MessageID: 1, AffectedSOPInstanceUID: "1.2.826.0.1.3680043.10.543.938.40"}); err != nil {
		t.Fatal(err)
	}
	if err := SendDataSet(assoc, pcID, object.FromElements(base, nil), transfer.ImplicitVRLittleEndian); err != nil {
		t.Fatal(err)
	}
	if err := <-serverDone; err != nil {
		t.Fatal(err)
	}
	dataset := <-received
	element, ok := dataset.Get(core.NewTag(0x0028, 0x0106))
	raw, _ := dataset.GetRaw(core.NewTag(0x0028, 0x0106))
	if !ok || element.VR() != core.VRSS || len(raw) != 2 || int16(uint16(raw[0])|uint16(raw[1])<<8) != -2000 {
		t.Fatalf("received Smallest Image Pixel Value %s % x", element.VR(), raw)
	}
}
