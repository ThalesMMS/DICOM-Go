package main

import (
	"bytes"
	"context"
	"testing"
	"time"

	"github.com/ThalesMMS/dicom-go/net/dimse"
	"github.com/ThalesMMS/dicom-go/net/ul"
	"github.com/ThalesMMS/dicom-go/transfer"
)

// refusedBefore1046 are objects the fixed list of ~40 Storage SOP Classes
// refused: mammography, intra-oral, US multi-frame, MR spectroscopy,
// Enhanced PET/XA, Legacy Converted, RT Image/Dose/Plan, encapsulated PDF,
// KOS, Basic/Enhanced SR, the VL family, whole slide microscopy, breast
// tomosynthesis, parametric maps, and a retired class.
var refusedBefore1046 = []string{
	"1.2.840.10008.5.1.4.1.1.1.2", "1.2.840.10008.5.1.4.1.1.1.3", "1.2.840.10008.5.1.4.1.1.3.1",
	"1.2.840.10008.5.1.4.1.1.4.2", "1.2.840.10008.5.1.4.1.1.130", "1.2.840.10008.5.1.4.1.1.12.1.1",
	"1.2.840.10008.5.1.4.1.1.2.2", "1.2.840.10008.5.1.4.1.1.4.4", "1.2.840.10008.5.1.4.1.1.128.1",
	"1.2.840.10008.5.1.4.1.1.481.1", "1.2.840.10008.5.1.4.1.1.481.2", "1.2.840.10008.5.1.4.1.1.481.5",
	"1.2.840.10008.5.1.4.1.1.104.1", "1.2.840.10008.5.1.4.1.1.88.59", "1.2.840.10008.5.1.4.1.1.88.11",
	"1.2.840.10008.5.1.4.1.1.88.22", "1.2.840.10008.5.1.4.1.1.77.1.1", "1.2.840.10008.5.1.4.1.1.77.1.2",
	"1.2.840.10008.5.1.4.1.1.77.1.4", "1.2.840.10008.5.1.4.1.1.77.1.6", "1.2.840.10008.5.1.4.1.1.13.1.3",
	"1.2.840.10008.5.1.4.1.1.30", "1.2.840.10008.5.1.4.1.1.6",
}

func TestStoreSCPAcceptsEveryStorageSOPClassOfTheCatalog(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	listener, err := ul.Listen(ul.ListenOptions{Address: "127.0.0.1:0", Context: ctx})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = listener.Close() }()
	serverDone := make(chan error, 1)
	go func() {
		assoc, err := listener.AcceptAssociation(ul.AcceptOptions{
			AETitle:                   "STORESCP",
			Context:                   ctx,
			SupportedAbstractSyntaxes: acceptedAbstractSyntaxes(),
			SupportedTransferSyntaxes: supportedTransferSyntaxUIDs(),
		})
		if err != nil {
			serverDone <- err
			return
		}
		var stdout bytes.Buffer
		serverDone <- handleAssociation(assoc, t.TempDir(), &stdout)
	}()
	contexts := make([]ul.PresentationContext, 0, len(refusedBefore1046))
	for _, sopClassUID := range refusedBefore1046 {
		contexts = append(contexts, ul.PresentationContext{AbstractSyntaxUID: sopClassUID, TransferSyntaxUIDs: []string{transfer.ExplicitVRLittleEndian.UID}})
	}
	assoc, err := ul.DialContext(ctx, listener.Addr().String(), ul.DialOptions{CalledAETitle: "STORESCP", CallingAETitle: "STORESCU", Contexts: contexts})
	if err != nil {
		t.Fatal(err)
	}
	for _, sopClassUID := range refusedBefore1046 {
		if _, err := dimse.AcceptedContextForSOPClass(assoc, sopClassUID); err != nil {
			t.Errorf("%s refused: %v", sopClassUID, err)
		}
	}
	if err := assoc.Release(ctx); err != nil {
		t.Fatal(err)
	}
	if err := <-serverDone; err != nil {
		t.Fatal(err)
	}
}
