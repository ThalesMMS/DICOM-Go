package dimse

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/ThalesMMS/dicom-go/core"
	"github.com/ThalesMMS/dicom-go/dictionary/std"
	"github.com/ThalesMMS/dicom-go/dictionary/tags"
	"github.com/ThalesMMS/dicom-go/net/ul"
	"github.com/ThalesMMS/dicom-go/object"
	"github.com/ThalesMMS/dicom-go/transfer"
)

const privateStorageSOPClassUID = "1.2.826.0.1.3680043.10.543.1158"

func TestRelatedGeneralStorageSOPClasses(t *testing.T) {
	item := RelatedGeneralStorageSOPClasses(privateStorageSOPClassUID, ctImageStorageSOPClassUID)
	if item.SopClassUID != privateStorageSOPClassUID || item.ServiceClassUID != StorageServiceClassUID || len(item.RelatedGeneralSOPClassUIDs) != 1 {
		t.Fatalf("item = %+v", item)
	}
	contexts := []ul.PresentationContext{{AbstractSyntaxUID: ctImageStorageSOPClassUID}}
	if got := commonExtendedNegotiationFor([]ul.SopClassCommonExtendedNegotiationItem{item}, contexts); got != nil {
		t.Fatalf("items for an association without the SOP Class = %+v", got)
	}
	contexts = append(contexts, ul.PresentationContext{AbstractSyntaxUID: privateStorageSOPClassUID})
	if got := commonExtendedNegotiationFor([]ul.SopClassCommonExtendedNegotiationItem{item}, contexts); len(got) != 1 {
		t.Fatalf("items = %+v", got)
	}
}

// TestStoreSessionSendsPrivateClassAsRelatedGeneralClass stores a private
// SOP Class instance and a CT instance on one association to an SCP that
// supports only CT Image Storage: with the related-class item the private
// context is accepted as CT, without it the private instance fails
// negotiation while the CT instance is stored.
func TestStoreSessionSendsPrivateClassAsRelatedGeneralClass(t *testing.T) {
	for _, withItem := range []bool{true, false} {
		name := "with related general class"
		if !withItem {
			name = "without related general class"
		}
		t.Run(name, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()
			listener, err := ul.Listen(ul.ListenOptions{Address: "127.0.0.1:0", Context: ctx})
			if err != nil {
				t.Fatal(err)
			}
			defer closeOrFail(t, "listener", listener)
			stored := make(chan ul.AcceptedContext, 2)
			serverDone := make(chan error, 1)
			go func() {
				assoc, err := listener.AcceptAssociation(ul.AcceptOptions{
					AETitle:                          "STORESCP",
					Context:                          ctx,
					SupportedAbstractSyntaxes:        []string{ctImageStorageSOPClassUID},
					SupportedTransferSyntaxes:        []string{transfer.ExplicitVRLittleEndian.UID, transfer.ImplicitVRLittleEndian.UID},
					CommonExtendedNegotiationHandler: ul.AcceptRelatedGeneralSOPClasses,
				})
				if err != nil {
					serverDone <- err
					return
				}
				defer func() { _ = assoc.Close() }()
				serverDone <- ServeAssociation(ctx, assoc, AssociationSCPOptions{StorageSCPOptions: StorageSCPOptions{
					StoreHandler: CStoreHandlerFunc(func(_ context.Context, req CStoreRequestContext) (uint16, error) {
						stored <- req.PresentationContext
						return StatusSuccess, nil
					}),
				}})
			}()
			dial := ul.DialOptions{CalledAETitle: "STORESCP", CallingAETitle: "STORESCU"}
			if withItem {
				dial.CommonExtendedNegotiation = []ul.SopClassCommonExtendedNegotiationItem{
					RelatedGeneralStorageSOPClasses(privateStorageSOPClassUID, ctImageStorageSOPClassUID),
					// Not in this batch: filtered out of the association.
					RelatedGeneralStorageSOPClasses("1.2.826.0.1.3680043.10.543.1158.9", ctImageStorageSOPClassUID),
				}
			}
			session, err := NewStoreSession(listener.Addr().String(), StoreSessionOptions{DialOptions: dial, ContinueOnError: true})
			if err != nil {
				t.Fatal(err)
			}
			defer func() { _ = session.Close(ctx) }()
			result, err := session.StoreBatch(ctx, []StoreSource{
				syntheticStoreSource(privateStorageSOPClassUID, "1.2.826.0.1.3680043.10.543.1158.1"),
				syntheticStoreSource(ctImageStorageSOPClassUID, "1.2.826.0.1.3680043.10.543.1158.2"),
			})
			if err != nil && withItem {
				t.Fatalf("StoreBatch: %v", err)
			}
			if err := <-serverDone; err != nil {
				t.Fatalf("SCP: %v", err)
			}
			close(stored)
			var contexts []ul.AcceptedContext
			for context := range stored {
				contexts = append(contexts, context)
			}
			if withItem {
				if result.Succeeded != 2 || len(contexts) != 2 {
					t.Fatalf("result %+v, stored %+v", result, contexts)
				}
				if contexts[0].AbstractSyntaxUID != privateStorageSOPClassUID || contexts[0].RelatedGeneralSOPClassUID != ctImageStorageSOPClassUID {
					t.Fatalf("private instance context %+v", contexts[0])
				}
				return
			}
			if result.Succeeded != 1 || result.Failed != 1 || !errors.Is(result.Items[0].Err, ErrStorePresentationContextRejected) {
				t.Fatalf("result %+v", result)
			}
		})
	}
}

func syntheticStoreSource(sopClassUID, sopInstanceUID string) StoreSource {
	dataset := object.FromElements([]core.Element{
		{Header: core.ElementHeader{Tag: tags.SOPClassUID, VR: core.VRUI}, Value: core.StringValue{sopClassUID}},
		{Header: core.ElementHeader{Tag: tags.SOPInstanceUID, VR: core.VRUI}, Value: core.StringValue{sopInstanceUID}},
		{Header: core.ElementHeader{Tag: tags.PatientID, VR: core.VRLO}, Value: core.StringValue{"SYNTHETIC-1158"}},
	}, std.Dictionary)
	return NewDataSetStoreSource(dataset, transfer.ExplicitVRLittleEndian, 512)
}
