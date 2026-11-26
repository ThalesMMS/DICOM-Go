package ian_test

import (
	"context"
	"errors"
	"reflect"
	"slices"
	"testing"
	"time"

	"github.com/ThalesMMS/dicom-go/core"
	"github.com/ThalesMMS/dicom-go/ian"
	"github.com/ThalesMMS/dicom-go/net/dimse"
	"github.com/ThalesMMS/dicom-go/net/ul"
	"github.com/ThalesMMS/dicom-go/object"
)

func TestNotificationNestedRefusalsOnWire(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	rec := &recorder{}
	service, err := ian.NewService(rec.handle, ian.ServiceOptions{})
	if err != nil {
		t.Fatal(err)
	}
	address, stop := serve(t, ctx, service)
	defer stop()
	assoc, err := ul.DialContext(ctx, address, ul.DialOptions{
		CallingAETitle: "MODALITY", CalledAETitle: "IANSCP", Contexts: []ul.PresentationContext{ian.PresentationContext()},
	})
	if err != nil {
		t.Fatal(err)
	}
	defer assoc.Close()
	raw := dimse.NewNormalizedClient(assoc)
	series := []core.Tag{ian.TagReferencedSeriesSequence}
	sop := []core.Tag{ian.TagReferencedSeriesSequence, ian.TagReferencedSOPSequence}
	pps := []core.Tag{ian.TagReferencedPerformedProcedureStepSequence}
	const notificationUID = "1.2.3.405"
	for _, tc := range []struct {
		name   string
		path   []core.Tag
		tag    core.Tag
		value  core.Value
		vr     core.VR
		status uint16
	}{
		{name: "missing series sequence", tag: ian.TagReferencedSeriesSequence, status: ian.DIMSEStatusMissingAttribute},
		{name: "missing series UID", path: series, tag: ian.TagSeriesInstanceUID, status: ian.DIMSEStatusMissingAttribute},
		{name: "missing SOP sequence", path: series, tag: ian.TagReferencedSOPSequence, status: ian.DIMSEStatusMissingAttribute},
		{name: "empty SOP sequence", path: series, tag: ian.TagReferencedSOPSequence, vr: core.VRSQ, value: core.SequenceValue{}, status: ian.DIMSEStatusMissingAttributeValue},
		{name: "missing SOP class UID", path: sop, tag: ian.TagReferencedSOPClassUID, status: ian.DIMSEStatusMissingAttribute},
		{name: "empty SOP class UID", path: sop, tag: ian.TagReferencedSOPClassUID, vr: core.VRUI, value: core.StringValue{}, status: ian.DIMSEStatusMissingAttributeValue},
		{name: "multiple SOP instance UIDs", path: sop, tag: ian.TagReferencedSOPInstanceUID, vr: core.VRUI, value: core.StringValue{"1.2.3.6", "1.2.3.7"}, status: ian.DIMSEStatusInvalidAttributeValue},
		{name: "empty availability", path: sop, tag: ian.TagInstanceAvailability, vr: core.VRCS, value: core.StringValue{}, status: ian.DIMSEStatusMissingAttributeValue},
		{name: "multiple optional storage media UIDs", path: sop, tag: ian.TagStorageMediaFileSetUID, vr: core.VRUI, value: core.StringValue{"1.2.8", "1.2.9"}, status: ian.DIMSEStatusInvalidAttributeValue},
		{name: "missing PPS instance UID", path: pps, tag: ian.TagReferencedSOPInstanceUID, status: ian.DIMSEStatusMissingAttribute},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ds := mustBuild(t, notification(1, 1))
			editNotificationItem(t, ds, tc.path, func(item *object.Object) {
				if tc.value == nil {
					item.Remove(tc.tag)
					return
				}
				item.Put(core.Element{Header: core.ElementHeader{Tag: tc.tag, VR: tc.vr}, Value: tc.value})
			})
			result, err := raw.CreateWithOptions(dimse.NormalizedOperationOptions{OperationOptions: dimse.OperationOptions{Context: ctx}}, dimse.NormalizedCreateRequest{
				AffectedSOPClassUID: ian.SOPClassUID, AffectedSOPInstanceUID: notificationUID, CommandDataSetType: dimse.DataSetPresent,
			}, ds)
			var statusErr *dimse.NormalizedStatusError
			if !errors.As(err, &statusErr) || result.Response == nil || result.Response.Status != tc.status {
				t.Fatalf("N-CREATE result = %+v, error = %v, want DIMSE status %04X", result.Response, err, tc.status)
			}
			fields := result.Response.StatusFields
			if fields == nil || !slices.Equal(fields.AttributeIdentifierList, []core.Tag{tc.tag}) {
				t.Fatalf("status fields = %+v, want only offending tag %v", fields, tc.tag)
			}
			if rec.count() != 0 {
				t.Fatal("invalid notification reached the consumer")
			}
		})
	}

	// A corrected request can reuse the UID after validation failures. Each
	// SOP instance still carries its own availability and retrieval addresses.
	n := notification(2, 2)
	n.PerformedProcedureSteps = nil
	n.Series[0].Instances[1].Availability = ian.AvailabilityNearline
	n.Series[0].Instances[1].RetrieveAETitle = "TIER_2"
	n.Series[0].Instances[1].RetrieveLocationUID = "1.2.3.8"
	n.Series[0].Instances[1].RetrieveURL = "https://example.invalid/instances/near"
	n.Series[1].Instances[0].Availability = ian.AvailabilityOffline
	n.Series[1].Instances[0].RetrieveAETitle = ""
	n.Series[1].Instances[0].RetrieveLocationUID = "1.2.3.9"
	n.Series[1].Instances[0].RetrieveURI = "https://example.invalid/instances/offline"
	n.Series[1].Instances[0].StorageMediaFileSetID = "MEDIA7"
	n.Series[1].Instances[0].StorageMediaFileSetUID = "1.2.3.10"
	n.Series[1].Instances[1].Availability = ian.AvailabilityUnavailable
	n.Series[1].Instances[1].RetrieveAETitle = ""
	result, err := ian.NewClient(assoc).NotifyNotification(ctx, notificationUID, n)
	if err != nil || result.Status != ian.DIMSEStatusSuccess || rec.count() != 1 {
		t.Fatalf("corrected N-CREATE result = %+v, error = %v, delivered = %d", result, err, rec.count())
	}
	if got := rec.received[0].Notification; !reflect.DeepEqual(got, n) {
		t.Fatalf("delivered notification = %+v, want %+v", got, n)
	}
	if err := assoc.Release(ctx); err != nil {
		t.Fatal(err)
	}
}

func editNotificationItem(t *testing.T, ds *object.Object, path []core.Tag, edit func(*object.Object)) {
	t.Helper()
	if len(path) == 0 {
		edit(ds)
		return
	}
	element, ok := ds.Get(path[0])
	if !ok {
		t.Fatalf("fixture is missing sequence %v", path[0])
	}
	sequence, ok := element.Value.(core.SequenceValue)
	if !ok || len(sequence.Items) != 1 {
		t.Fatalf("fixture sequence %v must have exactly one item", path[0])
	}
	item := object.FromDataSet(sequence.Items[0], nil)
	editNotificationItem(t, item, path[1:], edit)
	sequence.Items[0] = item.ToDataSet()
	element.Value = sequence
	ds.Put(element)
}
