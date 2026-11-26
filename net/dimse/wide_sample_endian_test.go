package dimse

import (
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/ThalesMMS/dicom-go/core"
	"github.com/ThalesMMS/dicom-go/net/ul"
	"github.com/ThalesMMS/dicom-go/object"
	"github.com/ThalesMMS/dicom-go/pixeldata"
	"github.com/ThalesMMS/dicom-go/transfer"
)

const wideStoreRTDoseSOPClassUID = "1.2.840.10008.5.1.4.1.1.481.2"

var wideStoreValues = []uint32{1249000, 0x01020304, 0xA1B2C3D4, 0xFFFFFFFF, 0x80000000, 7}

func wideStoreSamples(order binary.ByteOrder) []byte {
	out := make([]byte, 4*len(wideStoreValues))
	for i, v := range wideStoreValues {
		order.PutUint32(out[i*4:], v)
	}
	return out
}

func wideStoreObject(instance string) *object.Object {
	u16 := func(group, element, value uint16) core.Element {
		return core.Element{Header: core.ElementHeader{Tag: core.NewTag(group, element), VR: core.VRUS}, Value: core.Uint16Value{value}}
	}
	str := func(group, element uint16, vr core.VR, value string) core.Element {
		return core.Element{Header: core.ElementHeader{Tag: core.NewTag(group, element), VR: vr}, Value: core.StringValue{value}}
	}
	icon := []core.Element{
		u16(0x0028, 0x0002, 1), str(0x0028, 0x0004, core.VRCS, "MONOCHROME2"),
		u16(0x0028, 0x0010, 1), u16(0x0028, 0x0011, 2),
		u16(0x0028, 0x0100, 16), u16(0x0028, 0x0101, 16), u16(0x0028, 0x0102, 15), u16(0x0028, 0x0103, 0),
		core.NewRawElement(core.TagPixelData, core.VROW, []byte{0x02, 0x01, 0xB0, 0xA0}),
	}
	return object.FromElements([]core.Element{
		str(0x0008, 0x0016, core.VRUI, wideStoreRTDoseSOPClassUID),
		str(0x0008, 0x0018, core.VRUI, instance),
		str(0x0010, 0x0010, core.VRPN, "SYNTHETIC^ENDIAN"),
		u16(0x0028, 0x0002, 1), str(0x0028, 0x0004, core.VRCS, "MONOCHROME2"),
		str(0x0028, 0x0008, core.VRIS, "1"),
		u16(0x0028, 0x0010, 2), u16(0x0028, 0x0011, 3),
		u16(0x0028, 0x0100, 32), u16(0x0028, 0x0101, 32), u16(0x0028, 0x0102, 31), u16(0x0028, 0x0103, 0),
		{Header: core.ElementHeader{Tag: core.NewTag(0x0088, 0x0200), VR: core.VRSQ}, Value: core.SequenceValue{Items: []core.DataSet{{Elements: icon}}}},
		core.NewRawElement(core.TagPixelData, core.VROW, wideStoreSamples(binary.LittleEndian)),
	}, nil)
}

type wideStoreReceived struct {
	syntax  string
	dataset *object.Object
}

// TestCStoreWideSamplesKeepNegotiatedByteOrder sends 32-bit samples over a Big
// Endian and a Little Endian context. DIMSE carries the dataset in the
// negotiated syntax without re-swapping; the sender converts explicitly.
func TestCStoreWideSamplesKeepNegotiatedByteOrder(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	listener, err := ul.Listen(ul.ListenOptions{Address: "127.0.0.1:0", Context: ctx})
	if err != nil {
		t.Fatal(err)
	}
	defer closeOrFail(t, "listener", listener)

	received := make(chan wideStoreReceived, 2)
	serverDone := make(chan error, 1)
	go func() {
		serverDone <- func() error {
			assoc, err := listener.AcceptAssociation(ul.AcceptOptions{
				AETitle:                   "STORESCP",
				Context:                   ctx,
				SupportedAbstractSyntaxes: []string{wideStoreRTDoseSOPClassUID},
				SupportedTransferSyntaxes: []string{transfer.ExplicitVRBigEndian.UID, transfer.ExplicitVRLittleEndian.UID},
			})
			if err != nil {
				return err
			}
			defer func() { _ = assoc.Close() }()
			for i := 0; i < 2; i++ {
				pcID, req, err := ReceiveCStoreRequestAny(assoc)
				if err != nil {
					return err
				}
				var syntaxUID string
				for _, accepted := range assoc.AcceptedContexts {
					if accepted.ID == pcID {
						syntaxUID = accepted.TransferSyntaxUID
					}
				}
				syntax, ok := transfer.DefaultRegistry.Get(syntaxUID)
				if !ok {
					return fmt.Errorf("unknown accepted syntax %q", syntaxUID)
				}
				dataset, err := ReceiveDataSet(assoc, pcID, syntax)
				if err != nil {
					return err
				}
				received <- wideStoreReceived{syntax: syntaxUID, dataset: dataset}
				if err := SendCStoreResponse(assoc, pcID, CStoreResponse{
					AffectedSOPClassUID: req.AffectedSOPClassUID, MessageIDBeingRespondedTo: req.MessageID,
					AffectedSOPInstanceUID: req.AffectedSOPInstanceUID, Status: StatusSuccess,
				}); err != nil {
					return err
				}
			}
			pdu, err := assoc.ReadPDU()
			if err != nil {
				return err
			}
			if _, ok := pdu.(*ul.ReleaseRQ); !ok {
				return errors.New("expected A-RELEASE-RQ")
			}
			return assoc.WritePDU(&ul.ReleaseRP{})
		}()
	}()

	assoc, err := ul.DialContext(ctx, listener.Addr().String(), ul.DialOptions{
		CalledAETitle: "STORESCP", CallingAETitle: "STORESCU",
		Contexts: []ul.PresentationContext{
			{AbstractSyntaxUID: wideStoreRTDoseSOPClassUID, TransferSyntaxUIDs: []string{transfer.ExplicitVRBigEndian.UID}},
			{AbstractSyntaxUID: wideStoreRTDoseSOPClassUID, TransferSyntaxUIDs: []string{transfer.ExplicitVRLittleEndian.UID}},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = assoc.Close() }()

	bigEndian, _, err := pixeldata.TranscodeDataSet(ctx, wideStoreObject("1.2.826.0.1.3680043.10.543.937.21"), transfer.ExplicitVRLittleEndian, transfer.ExplicitVRBigEndian, pixeldata.TranscodeOptions{})
	if err != nil {
		t.Fatal(err)
	}
	// The Little Endian copy is converted from the Big Endian object, never
	// from the original, so a symmetric swap error cannot cancel itself out.
	littleEndian, _, err := pixeldata.TranscodeDataSet(ctx, bigEndian, transfer.ExplicitVRBigEndian, transfer.ExplicitVRLittleEndian, pixeldata.TranscodeOptions{})
	if err != nil {
		t.Fatal(err)
	}
	littleEndian.Put(core.Element{Header: core.ElementHeader{Tag: core.NewTag(0x0008, 0x0018), VR: core.VRUI}, Value: core.StringValue{"1.2.826.0.1.3680043.10.543.937.22"}})
	sends := []struct {
		syntax  transfer.Syntax
		dataset *object.Object
		uid     string
	}{
		{transfer.ExplicitVRBigEndian, bigEndian, "1.2.826.0.1.3680043.10.543.937.21"},
		{transfer.ExplicitVRLittleEndian, littleEndian, "1.2.826.0.1.3680043.10.543.937.22"},
	}
	for i, send := range sends {
		var pcID byte
		for _, accepted := range assoc.AcceptedContexts {
			if accepted.TransferSyntaxUID == send.syntax.UID {
				pcID = accepted.ID
			}
		}
		if pcID == 0 {
			t.Fatalf("%s context not accepted", send.syntax.Name)
		}
		if err := SendCStoreRequest(assoc, pcID, CStoreRequest{AffectedSOPClassUID: wideStoreRTDoseSOPClassUID, MessageID: uint16(i + 1), AffectedSOPInstanceUID: send.uid}); err != nil {
			t.Fatal(err)
		}
		if err := SendDataSet(assoc, pcID, send.dataset, send.syntax); err != nil {
			t.Fatal(err)
		}
		response, err := ReceiveCStoreResponse(assoc, pcID)
		if err != nil || response.Status != StatusSuccess {
			t.Fatalf("C-STORE %s: status=%v err=%v", send.syntax.Name, response, err)
		}
	}
	if err := assoc.Release(ctx); err != nil {
		t.Fatal(err)
	}
	if err := <-serverDone; err != nil {
		t.Fatal(err)
	}
	close(received)
	count := 0
	for got := range received {
		count++
		order := binary.ByteOrder(binary.LittleEndian)
		iconWant := []byte{0x02, 0x01, 0xB0, 0xA0}
		if got.syntax == transfer.ExplicitVRBigEndian.UID {
			order = binary.BigEndian
			iconWant = []byte{0x01, 0x02, 0xA0, 0xB0}
		}
		if got.dataset.ValueByteOrder() != order {
			t.Fatalf("%s: received value order %v", got.syntax, got.dataset.ValueByteOrder())
		}
		if raw, _ := got.dataset.GetRaw(core.TagPixelData); !bytes.Equal(raw, wideStoreSamples(order)) {
			t.Fatalf("%s: received Pixel Data % x, want % x", got.syntax, raw, wideStoreSamples(order))
		}
		items, ok := got.dataset.GetSequence(core.NewTag(0x0088, 0x0200))
		if !ok || len(items) != 1 {
			t.Fatalf("%s: icon sequence missing", got.syntax)
		}
		if raw, _ := items[0].GetRaw(core.TagPixelData); !bytes.Equal(raw, iconWant) {
			t.Fatalf("%s: icon Pixel Data % x, want % x", got.syntax, raw, iconWant)
		}
	}
	if count != 2 {
		t.Fatalf("received %d datasets, want 2", count)
	}
}
