package ups_test

import (
	"context"
	"io"
	"testing"
	"time"

	"github.com/ThalesMMS/dicom-go/core"
	"github.com/ThalesMMS/dicom-go/net/dimse"
	"github.com/ThalesMMS/dicom-go/net/ul"
	"github.com/ThalesMMS/dicom-go/object"
	"github.com/ThalesMMS/dicom-go/transfer"
	"github.com/ThalesMMS/dicom-go/ups"
)

func TestUPSFilteredWatchIdentifierWritesUnicode(t *testing.T) {
	identifier, err := ups.BuildSubscriptionInformation("WATCHER", false, map[string][]string{
		ups.TagPatientName.String(): {"GARCÍA^*"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := object.WriteDataSet(io.Discard, identifier, transfer.ExplicitVRLittleEndian); err != nil {
		t.Fatalf("write Unicode Watch matching key: %v", err)
	}
}

func TestUPSFilteredWatchMatchesEncodedCandidatesAcrossRestart(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	peer, local := pipeUPSAssociations(t, []ul.AcceptedContext{{
		ID: 1, AbstractSyntaxUID: ups.WatchSOPClassUID, TransferSyntaxUID: transfer.ExplicitVRLittleEndian.UID,
	}})
	store, service := filteredService(t, ups.MemoryStoreOptions{})
	create := func(service *ups.Service, uid string, name string) {
		t.Helper()
		attributes := scheduledAttributes(t)
		attributes.Put(ups.StringElement(ups.TagSpecificCharacterSet, core.VRCS, "ISO_IR 100"))
		attributes.Put(core.NewRawElement(ups.TagPatientName, core.VRPN, []byte(name)))
		if _, err := service.Create(ctx, ups.CreateRequest{SOPInstanceUID: uid, Attributes: attributes}); err != nil {
			t.Fatal(err)
		}
	}
	const existing = "2.25.1789.1"
	const outside = "2.25.1789.2"
	const future = "2.25.1789.3"
	create(service, existing, "GARC\xcdA^PATIENT ")
	create(service, outside, "OTHER^PATIENT ")

	options := service.NormalizedOptions(local)
	serverDone := make(chan error, 1)
	go func() {
		command, err := dimse.ReceiveCommandSet(local, 1)
		if err == nil {
			err = dimse.ServeNormalizedCommand(ctx, local, 1, command, options)
		}
		serverDone <- err
	}()
	result, err := ups.NewClient(peer).SubscribeFiltered(ctx, "WATCHER", true, map[string][]string{
		ups.TagPatientName.String(): {"GARCÍA^*"},
	})
	if err != nil || result.Status != ups.StatusSuccess {
		t.Fatalf("SubscribeFiltered = %#v, %v", result, err)
	}
	if err := <-serverDone; err != nil {
		t.Fatal(err)
	}

	restarted := newFilteredService(t, store)
	global, err := restarted.Subscription(ctx, ups.FilteredGlobalSubscriptionSOPInstanceUID, "WATCHER")
	if err != nil || global.Filter == nil {
		t.Fatalf("durable filtered subscription = %#v, %v", global, err)
	}
	foundUnicode := false
	for _, key := range global.Filter.Keys {
		if key.Tag == ups.TagPatientName && len(key.Values) == 1 && key.Values[0] == "GARCÍA^*" {
			foundUnicode = true
		}
	}
	if !foundUnicode {
		t.Fatal("filter lost decoded patient name")
	}
	assertSubscriptionState(t, restarted, existing, "WATCHER", ups.SubscriptionWithDeletionLock)
	assertNoSubscription(t, restarted, outside, "WATCHER")
	create(restarted, future, "GARC\xcdA^FUTURE")
	assertSubscriptionState(t, restarted, future, "WATCHER", ups.SubscriptionWithDeletionLock)
}
