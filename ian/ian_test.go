package ian_test

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/ThalesMMS/dicom-go/core"
	"github.com/ThalesMMS/dicom-go/ian"
	"github.com/ThalesMMS/dicom-go/net/dimse"
	"github.com/ThalesMMS/dicom-go/net/ul"
	"github.com/ThalesMMS/dicom-go/object"
)

const (
	testStudy = "1.2.826.0.1.3680043.10.1168.1"
	testCT    = "1.2.840.10008.5.1.4.1.1.2"
	testPPS   = "1.2.826.0.1.3680043.10.1168.9"
)

// Synthetic references only.
func notification(series, perSeries int) ian.Notification {
	n := ian.Notification{
		StudyInstanceUID:        testStudy,
		PerformedProcedureSteps: []ian.ProcedureStepReference{{SOPClassUID: ian.ModalityPerformedProcedureStepSOPClassUID, SOPInstanceUID: testPPS}},
	}
	for s := 0; s < series; s++ {
		item := ian.Series{SeriesInstanceUID: fmt.Sprintf("%s.2.%d", testStudy, s+1)}
		for i := 0; i < perSeries; i++ {
			item.Instances = append(item.Instances, ian.Instance{
				SOPClassUID: testCT, SOPInstanceUID: fmt.Sprintf("%s.3.%d.%d", testStudy, s+1, i+1),
				Availability: ian.AvailabilityOnline, RetrieveAETitle: "ARCHIVE",
			})
		}
		n.Series = append(n.Series, item)
	}
	return n
}

func mustBuild(t *testing.T, n ian.Notification) *object.Object {
	t.Helper()
	ds, err := ian.Build(n)
	if err != nil {
		t.Fatal(err)
	}
	return ds
}

func statusOf(t *testing.T, err error) (uint16, []core.Tag) {
	t.Helper()
	var statusErr *ian.StatusError
	if !errors.As(err, &statusErr) {
		t.Fatalf("error %v is not a status error", err)
	}
	return statusErr.Status, statusErr.Tags
}

func textElement(tag core.Tag, vr core.VR, value string) core.Element {
	if value == "" {
		return core.Element{Header: core.ElementHeader{Tag: tag, VR: vr}, Value: core.StringValue(nil)}
	}
	return core.Element{Header: core.ElementHeader{Tag: tag, VR: vr}, Value: core.StringValue{value}}
}

// editFirstInstance applies edit to the first Referenced SOP Sequence item
// and stores the result back in ds.
func editFirstInstance(t *testing.T, ds *object.Object, edit func(*object.Object)) {
	t.Helper()
	element, _ := ds.Get(ian.TagReferencedSeriesSequence)
	series := element.Value.(core.SequenceValue).Items
	seriesItem := object.FromDataSet(series[0], nil)
	sopElement, _ := seriesItem.Get(ian.TagReferencedSOPSequence)
	instances := sopElement.Value.(core.SequenceValue).Items
	instance := object.FromDataSet(instances[0], nil)
	edit(instance)
	instances[0] = instance.ToDataSet()
	sopElement.Value = core.SequenceValue{Items: instances}
	seriesItem.Put(sopElement)
	series[0] = seriesItem.ToDataSet()
	element.Value = core.SequenceValue{Items: series}
	ds.Put(element)
}

func firstInstance(t *testing.T, ds *object.Object) *object.Object {
	t.Helper()
	series, _ := ds.GetSequence(ian.TagReferencedSeriesSequence)
	instances, _ := series[0].GetSequence(ian.TagReferencedSOPSequence)
	return instances[0]
}

func TestBuildParseRoundTrip(t *testing.T) {
	n := notification(2, 3)
	n.SpecificCharacterSet = "ISO_IR 192"
	n.Series[1].Instances[0].Availability = ian.AvailabilityNearline
	n.Series[1].Instances[1] = ian.Instance{SOPClassUID: testCT, SOPInstanceUID: n.Series[1].Instances[1].SOPInstanceUID, Availability: ian.AvailabilityOffline,
		RetrieveLocationUID: "1.2.826.0.1.3680043.10.1168.7", StorageMediaFileSetID: "DISC1", StorageMediaFileSetUID: "1.2.826.0.1.3680043.10.1168.8"}
	n.Series[1].Instances[2] = ian.Instance{SOPClassUID: testCT, SOPInstanceUID: n.Series[1].Instances[2].SOPInstanceUID, Availability: ian.AvailabilityUnavailable}
	ds := mustBuild(t, n)
	got, err := ian.Parse(ds)
	if err != nil {
		t.Fatal(err)
	}
	// An UNAVAILABLE instance without a location is sent with an empty
	// Retrieve AE Title; the PPS reference carries an empty code sequence.
	if fmt.Sprint(got.Series) != fmt.Sprint(n.Series) || got.StudyInstanceUID != testStudy || got.SpecificCharacterSet != "ISO_IR 192" || got.Count() != 6 {
		t.Fatalf("round trip\n got %+v\nwant %+v", got, n)
	}
	if len(got.PerformedProcedureSteps) != 1 || got.PerformedProcedureSteps[0].SOPInstanceUID != testPPS {
		t.Fatalf("steps %+v", got.PerformedProcedureSteps)
	}
	step, _ := ds.GetSequence(ian.TagReferencedPerformedProcedureStepSequence)
	if _, ok := step[0].Get(ian.TagPerformedWorkitemCodeSequence); !ok {
		t.Fatal("Performed Workitem Code Sequence not sent")
	}
	located := firstInstance(t, ds)
	if _, ok := located.Get(ian.TagRetrieveLocationUID); ok {
		t.Fatal("empty Retrieve Location UID sent")
	}
	// Without PPS references the Type 2 sequence is present and empty.
	n.PerformedProcedureSteps = nil
	empty := mustBuild(t, n)
	if items, ok := empty.GetSequence(ian.TagReferencedPerformedProcedureStepSequence); !ok || len(items) != 0 {
		t.Fatalf("empty PPS sequence %v %v", items, ok)
	}
}

func TestValidateFollowsTableR31(t *testing.T) {
	for name, tc := range map[string]struct {
		mutate func(*testing.T, *object.Object)
		status uint16
		tag    core.Tag
	}{
		"missing study": {func(_ *testing.T, ds *object.Object) { ds.Remove(ian.TagStudyInstanceUID) }, ian.DIMSEStatusMissingAttribute, ian.TagStudyInstanceUID},
		"missing PPS sequence": {func(_ *testing.T, ds *object.Object) {
			ds.Remove(ian.TagReferencedPerformedProcedureStepSequence)
		}, ian.DIMSEStatusMissingAttribute, ian.TagReferencedPerformedProcedureStepSequence},
		"empty series sequence": {func(_ *testing.T, ds *object.Object) {
			ds.Put(core.Element{Header: core.ElementHeader{Tag: ian.TagReferencedSeriesSequence, VR: core.VRSQ}, Value: core.SequenceValue{}})
		}, ian.DIMSEStatusMissingAttributeValue, ian.TagReferencedSeriesSequence},
		"invalid study UID": {func(_ *testing.T, ds *object.Object) {
			ds.Put(textElement(ian.TagStudyInstanceUID, core.VRUI, "1..2"))
		}, ian.DIMSEStatusInvalidAttributeValue, ian.TagStudyInstanceUID},
		"unknown availability": {func(t *testing.T, ds *object.Object) {
			editFirstInstance(t, ds, func(item *object.Object) { item.Put(textElement(ian.TagInstanceAvailability, core.VRCS, "SOMEWHERE")) })
		}, ian.DIMSEStatusInvalidAttributeValue, ian.TagInstanceAvailability},
		"missing availability": {func(t *testing.T, ds *object.Object) {
			editFirstInstance(t, ds, func(item *object.Object) { item.Remove(ian.TagInstanceAvailability) })
		}, ian.DIMSEStatusMissingAttribute, ian.TagInstanceAvailability},
		"missing retrieve AE": {func(t *testing.T, ds *object.Object) {
			editFirstInstance(t, ds, func(item *object.Object) { item.Remove(ian.TagRetrieveAETitle) })
		}, ian.DIMSEStatusMissingAttribute, ian.TagRetrieveAETitle},
		"empty retrieve AE": {func(t *testing.T, ds *object.Object) {
			editFirstInstance(t, ds, func(item *object.Object) { item.Put(textElement(ian.TagRetrieveAETitle, core.VRAE, "")) })
		}, ian.DIMSEStatusMissingAttributeValue, ian.TagRetrieveAETitle},
		"retrieve AE too long": {func(t *testing.T, ds *object.Object) {
			editFirstInstance(t, ds, func(item *object.Object) {
				item.Put(textElement(ian.TagRetrieveAETitle, core.VRAE, "SEVENTEEN_CHARS_X"))
			})
		}, ian.DIMSEStatusInvalidAttributeValue, ian.TagRetrieveAETitle},
		"missing SOP instance": {func(t *testing.T, ds *object.Object) {
			editFirstInstance(t, ds, func(item *object.Object) { item.Remove(ian.TagReferencedSOPInstanceUID) })
		}, ian.DIMSEStatusMissingAttribute, ian.TagReferencedSOPInstanceUID},
		"PPS item without class": {func(_ *testing.T, ds *object.Object) {
			element, _ := ds.Get(ian.TagReferencedPerformedProcedureStepSequence)
			items := element.Value.(core.SequenceValue).Items
			step := object.FromDataSet(items[0], nil)
			step.Remove(ian.TagReferencedSOPClassUID)
			items[0] = step.ToDataSet()
			ds.Put(element)
		}, ian.DIMSEStatusMissingAttribute, ian.TagReferencedSOPClassUID},
		"series as text": {func(_ *testing.T, ds *object.Object) {
			ds.Put(textElement(ian.TagReferencedSeriesSequence, core.VRUI, testStudy))
		}, ian.DIMSEStatusInvalidAttributeValue, ian.TagReferencedSeriesSequence},
	} {
		ds := mustBuild(t, notification(1, 2))
		tc.mutate(t, ds)
		status, tags := statusOf(t, ian.Validate(ds))
		if status != tc.status || len(tags) == 0 || tags[0] != tc.tag {
			t.Errorf("%s: status %04X tags %v", name, status, tags)
		}
		if err := ian.Validate(ds); strings.Contains(err.Error(), "ARCHIVE") || strings.Contains(err.Error(), testStudy) {
			t.Errorf("%s: error exposes a value: %v", name, err)
		}
	}
	// A Retrieve URL replaces the Retrieve AE Title.
	n := notification(1, 1)
	n.Series[0].Instances[0].RetrieveAETitle = ""
	n.Series[0].Instances[0].RetrieveURL = "https://archive.invalid/wado"
	if _, err := ian.Build(n); err != nil {
		t.Fatalf("retrieve URL only: %v", err)
	}
	if status, _ := statusOf(t, ian.Validate(nil)); status != ian.DIMSEStatusMissingAttribute {
		t.Fatalf("nil data set: %04X", status)
	}
	if ian.StatusOf(errors.New("x")) != ian.DIMSEStatusProcessingFailure || ian.StatusOf(nil) != 0 {
		t.Fatal("StatusOf defaults")
	}
}

func TestGroupSplitsStudiesAndSeries(t *testing.T) {
	ref := func(study, series, sop string) ian.StudyInstance {
		return ian.StudyInstance{StudyInstanceUID: study, SeriesInstanceUID: series, Instance: ian.Instance{SOPClassUID: testCT, SOPInstanceUID: sop, Availability: ian.AvailabilityOnline, RetrieveAETitle: "AE"}}
	}
	steps := []ian.ProcedureStepReference{{SOPClassUID: ian.ModalityPerformedProcedureStepSOPClassUID, SOPInstanceUID: testPPS}}
	got := ian.Group([]ian.StudyInstance{
		ref("1.1", "1.1.1", "1.1.1.1"), ref("1.2", "1.2.1", "1.2.1.1"), ref("1.1", "1.1.2", "1.1.2.1"),
		ref("1.1", "1.1.1", "1.1.1.2"), ref("1.1", "1.1.1", "1.1.1.1"),
	}, steps)
	if len(got) != 2 || got[0].StudyInstanceUID != "1.1" || got[1].StudyInstanceUID != "1.2" {
		t.Fatalf("studies %+v", got)
	}
	if len(got[0].Series) != 2 || got[0].Count() != 3 || got[0].Series[0].Instances[1].SOPInstanceUID != "1.1.1.2" || len(got[1].PerformedProcedureSteps) != 1 {
		t.Fatalf("first study %+v", got[0])
	}
	for _, n := range got {
		if _, err := ian.Build(n); err != nil {
			t.Fatal(err)
		}
	}
}

type recorder struct {
	mu       sync.Mutex
	received []ian.Received
	fail     error
}

func (r *recorder) handle(_ context.Context, received ian.Received) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.fail != nil {
		return r.fail
	}
	r.received = append(r.received, received)
	return nil
}

func (r *recorder) count() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return len(r.received)
}

func TestServiceRefusesDuplicatesAndHonorsLimits(t *testing.T) {
	ctx := context.Background()
	rec := &recorder{}
	service, err := ian.NewService(rec.handle, ian.ServiceOptions{Limits: ian.Limits{MaxInstances: 4}, RememberedNotifications: 2})
	if err != nil {
		t.Fatal(err)
	}
	ds := mustBuild(t, notification(1, 2))
	if _, err := service.Receive(ctx, "1.2.3.1", ds); err != nil {
		t.Fatal(err)
	}
	if _, err := service.Receive(ctx, "1.2.3.1", ds); ian.StatusOf(err) != ian.DIMSEStatusDuplicateSOPInstance || !errors.Is(err, ian.ErrDuplicate) {
		t.Fatalf("duplicate: %v", err)
	}
	if _, err := service.Receive(ctx, "1..1", ds); ian.StatusOf(err) != ian.DIMSEStatusInvalidObjectInstance {
		t.Fatalf("invalid UID: %v", err)
	}
	if _, err := service.Receive(ctx, "", mustBuild(t, notification(1, 5))); ian.StatusOf(err) != ian.DIMSEStatusResourceLimitation {
		t.Fatalf("limit: %v", err)
	}
	// The oldest UID is forgotten once the window is full.
	for _, uid := range []string{"1.2.3.2", "1.2.3.3"} {
		if _, err := service.Receive(ctx, uid, ds); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := service.Receive(ctx, "1.2.3.1", ds); err != nil {
		t.Fatalf("forgotten UID: %v", err)
	}
	// A failing handler does not consume the UID, so the SCU may retry.
	rec.fail = &ian.StatusError{Status: 0xC001, Err: errors.New("synthetic")}
	if _, err := service.Receive(ctx, "1.2.3.9", ds); ian.StatusOf(err) != 0xC001 {
		t.Fatalf("handler status: %v", err)
	}
	rec.fail = nil
	if _, err := service.Receive(ctx, "1.2.3.9", ds); err != nil {
		t.Fatalf("retry after failure: %v", err)
	}
	if rec.count() != 5 {
		t.Fatalf("handler calls %d", rec.count())
	}
	if _, err := ian.NewService(nil, ian.ServiceOptions{}); err == nil {
		t.Fatal("nil handler accepted")
	}
}

func TestServiceIdenticalReplayIsOptIn(t *testing.T) {
	ctx := context.Background()
	rec := &recorder{}
	service, err := ian.NewService(rec.handle, ian.ServiceOptions{AcceptIdenticalReplay: true})
	if err != nil {
		t.Fatal(err)
	}
	ds := mustBuild(t, notification(1, 1))
	for i := 0; i < 2; i++ {
		if _, err := service.Receive(ctx, "1.2.3.4", ds); err != nil {
			t.Fatalf("attempt %d: %v", i, err)
		}
	}
	if rec.count() != 1 {
		t.Fatalf("handler ran %d times", rec.count())
	}
	if _, err := service.Receive(ctx, "1.2.3.4", mustBuild(t, notification(1, 2))); ian.StatusOf(err) != ian.DIMSEStatusDuplicateSOPInstance {
		t.Fatalf("changed replay: %v", err)
	}
}

func serve(t *testing.T, ctx context.Context, service *ian.Service) (string, func()) {
	t.Helper()
	listener, err := ul.Listen(ul.ListenOptions{Address: "127.0.0.1:0", Context: ctx})
	if err != nil {
		t.Fatal(err)
	}
	server, err := ul.NewAssociationServer(listener, ul.AssociationServerOptions{
		MaxConcurrentAssociations: 4, SaturationPolicy: ul.SaturationReject,
		Accept: ul.AcceptOptions{AETitle: "IANSCP", RequireMatchingCalledAE: true, SupportedAbstractSyntaxes: []string{ian.SOPClassUID}, SupportedTransferSyntaxes: ian.PresentationContext().TransferSyntaxUIDs},
		Handler: func(ctx context.Context, assoc *ul.Association) error {
			defer assoc.Close()
			options := service.NormalizedOptions(assoc)
			return dimse.ServeAssociation(ctx, assoc, dimse.AssociationSCPOptions{NormalizedSCP: &options})
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	serveCtx, stop := context.WithCancel(ctx)
	done := make(chan error, 1)
	go func() { done <- server.Serve(serveCtx) }()
	return listener.Addr().String(), func() { stop(); <-done; _ = listener.Close() }
}

// SCU to SCP over a real association: a valid notification is delivered with
// the association's AE titles, an invalid one is refused with the status and
// Attribute Identifier List of PS3.7, and the association stays usable.
func TestClientAndServiceOverAssociation(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	rec := &recorder{}
	service, err := ian.NewService(rec.handle, ian.ServiceOptions{})
	if err != nil {
		t.Fatal(err)
	}
	address, stop := serve(t, ctx, service)
	defer stop()
	assoc, err := ul.DialContext(ctx, address, ul.DialOptions{CallingAETitle: "MODALITY", CalledAETitle: "IANSCP", Contexts: []ul.PresentationContext{ian.PresentationContext()}})
	if err != nil {
		t.Fatal(err)
	}
	defer assoc.Close()
	client := ian.NewClient(assoc)
	result, err := client.NotifyNotification(ctx, "", notification(2, 2))
	if err != nil || result.Status != 0 || !core.IsValidUID(result.SOPInstanceUID) {
		t.Fatalf("notify %+v %v", result, err)
	}
	if rec.count() != 1 {
		t.Fatalf("delivered %d", rec.count())
	}
	got := rec.received[0]
	if got.SOPInstanceUID != result.SOPInstanceUID || got.CallingAETitle != "MODALITY" || got.CalledAETitle != "IANSCP" || got.Notification.Count() != 4 || got.Notification.StudyInstanceUID != testStudy {
		t.Fatalf("received %+v", got)
	}
	// The client validates before sending; bypass it with a raw N-CREATE.
	invalid := mustBuild(t, notification(1, 1))
	editFirstInstance(t, invalid, func(item *object.Object) { item.Put(textElement(ian.TagInstanceAvailability, core.VRCS, "LOST")) })
	if _, err := client.Notify(ctx, "1.2.3.5", invalid); ian.StatusOf(err) != ian.DIMSEStatusInvalidAttributeValue {
		t.Fatalf("client validation: %v", err)
	}
	raw, err := dimse.NewNormalizedClient(assoc).CreateWithOptions(dimse.NormalizedOperationOptions{OperationOptions: dimse.OperationOptions{Context: ctx}}, dimse.NormalizedCreateRequest{
		AffectedSOPClassUID: ian.SOPClassUID, AffectedSOPInstanceUID: "1.2.3.5", CommandDataSetType: dimse.DataSetPresent,
	}, invalid)
	var statusErr *dimse.NormalizedStatusError
	if !errors.As(err, &statusErr) || raw.Response == nil || raw.Response.Status != ian.DIMSEStatusInvalidAttributeValue ||
		raw.Response.StatusFields == nil || len(raw.Response.StatusFields.AttributeIdentifierList) != 1 || raw.Response.StatusFields.AttributeIdentifierList[0] != ian.TagInstanceAvailability {
		t.Fatalf("invalid notification: %+v %v", raw.Response, err)
	}
	missing := mustBuild(t, notification(1, 1))
	missing.Remove(ian.TagStudyInstanceUID)
	raw, _ = dimse.NewNormalizedClient(assoc).CreateWithOptions(dimse.NormalizedOperationOptions{OperationOptions: dimse.OperationOptions{Context: ctx}}, dimse.NormalizedCreateRequest{
		AffectedSOPClassUID: ian.SOPClassUID, AffectedSOPInstanceUID: "1.2.3.6", CommandDataSetType: dimse.DataSetPresent,
	}, missing)
	if raw.Response == nil || raw.Response.Status != ian.DIMSEStatusMissingAttribute {
		t.Fatalf("missing attribute: %+v", raw.Response)
	}
	// The same UID again is a duplicate; the association is still usable.
	if result, err := client.NotifyNotification(ctx, got.SOPInstanceUID, notification(2, 2)); result.Status != ian.DIMSEStatusDuplicateSOPInstance || err == nil {
		t.Fatalf("duplicate %+v %v", result, err)
	}
	if result, err := client.NotifyNotification(ctx, "1.2.3.7", notification(1, 1)); err != nil || result.SOPInstanceUID != "1.2.3.7" {
		t.Fatalf("after refusals %+v %v", result, err)
	}
	if rec.count() != 2 {
		t.Fatalf("delivered %d", rec.count())
	}
	if err := assoc.Release(ctx); err != nil {
		t.Fatal(err)
	}
}
