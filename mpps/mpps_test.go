package mpps_test

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/ThalesMMS/dicom-go/core"
	"github.com/ThalesMMS/dicom-go/mpps"
	"github.com/ThalesMMS/dicom-go/net/dimse"
	"github.com/ThalesMMS/dicom-go/net/ul"
	"github.com/ThalesMMS/dicom-go/object"
)

const (
	testStudy = "1.2.826.0.1.3680043.10.947.1"
	testCT    = "1.2.840.10008.5.1.4.1.1.2"
)

// Synthetic attributes only.
func createAttributes() mpps.CreateAttributes {
	return mpps.CreateAttributes{
		ScheduledSteps: []mpps.ScheduledStep{{
			StudyInstanceUID: testStudy, AccessionNumber: "ACC947", RequestedProcedureID: "RP947",
			RequestedProcedureDescription: "SYNTHETIC CT", ScheduledProcedureStepID: "SPS947",
			ScheduledProcedureStepDescription: "SYNTHETIC STEP",
			ScheduledProtocolCodes:            []mpps.Code{{Value: "P1", Scheme: "99SYNTH", Meaning: "Synthetic protocol"}},
		}},
		PatientName: "SYNTHETIC^PATIENT", PatientID: "SYN947", PatientBirthDate: "19700101", PatientSex: "O",
		PerformedProcedureStepID: "PPS947", PerformedStationAETitle: "MODALITY", PerformedStationName: "CT1",
		PerformedLocation: "ROOM1", StartDate: "20260925", StartTime: "101500", Modality: "CT",
		Description: "SYNTHETIC", StudyID: "S947",
	}
}

func series(n int) []mpps.PerformedSeries {
	out := make([]mpps.PerformedSeries, n)
	for i := range out {
		out[i] = mpps.PerformedSeries{
			ProtocolName: "SYNTHETIC PROTOCOL", SeriesInstanceUID: fmt.Sprintf("%s.2.%d", testStudy, i+1), RetrieveAETitle: "ARCHIVE",
			Images: []mpps.Reference{{SOPClassUID: testCT, SOPInstanceUID: fmt.Sprintf("%s.3.%d", testStudy, i+1)}},
		}
	}
	return out
}

func mustCreate(t *testing.T) *object.Object {
	t.Helper()
	ds, err := mpps.BuildCreate(createAttributes())
	if err != nil {
		t.Fatal(err)
	}
	return ds
}

func newService(t *testing.T, options mpps.ServiceOptions) (*mpps.Service, *mpps.MemoryRepository) {
	t.Helper()
	repository, err := mpps.NewMemoryRepository(16)
	if err != nil {
		t.Fatal(err)
	}
	service, err := mpps.NewService(repository, options)
	if err != nil {
		t.Fatal(err)
	}
	return service, repository
}

func statusOf(t *testing.T, err error) (uint16, *mpps.StatusError) {
	t.Helper()
	var statusErr *mpps.StatusError
	if !errors.As(err, &statusErr) {
		t.Fatalf("error %v is not a status error", err)
	}
	return statusErr.Status, statusErr
}

func TestValidateCreateFollowsTableF721(t *testing.T) {
	good := mustCreate(t)
	if status, err := good.LookupString(mpps.TagPerformedProcedureStepStatus); err != nil || status != mpps.StatusInProgress {
		t.Fatalf("status %q %v", status, err)
	}
	for name, tc := range map[string]struct {
		mutate func(*object.Object)
		status uint16
		tag    core.Tag
	}{
		"missing Type 1": {func(ds *object.Object) { ds.Remove(mpps.TagPerformedProcedureStepID) }, mpps.DIMSEStatusMissingAttribute, mpps.TagPerformedProcedureStepID},
		"missing Type 2": {func(ds *object.Object) { ds.Remove(mpps.TagPatientName) }, mpps.DIMSEStatusMissingAttribute, mpps.TagPatientName},
		"empty Type 1":   {func(ds *object.Object) { ds.Put(textElement(mpps.TagModality, core.VRCS, "")) }, mpps.DIMSEStatusMissingAttributeValue, mpps.TagModality},
		"created COMPLETED": {func(ds *object.Object) {
			ds.Put(textElement(mpps.TagPerformedProcedureStepStatus, core.VRCS, "COMPLETED"))
		}, mpps.DIMSEStatusInvalidAttributeValue, mpps.TagPerformedProcedureStepStatus},
		"invalid start date": {func(ds *object.Object) {
			ds.Put(textElement(mpps.TagPerformedProcedureStepStartDate, core.VRDA, "20261399"))
		}, mpps.DIMSEStatusInvalidAttributeValue, mpps.TagPerformedProcedureStepStartDate},
		"no scheduled step item": {func(ds *object.Object) {
			ds.Put(core.Element{Header: core.ElementHeader{Tag: mpps.TagScheduledStepAttributesSequence, VR: core.VRSQ}, Value: core.SequenceValue{}})
		}, mpps.DIMSEStatusMissingAttributeValue, mpps.TagScheduledStepAttributesSequence},
		"station AE too long": {func(ds *object.Object) {
			ds.Put(textElement(mpps.TagPerformedStationAETitle, core.VRAE, "SEVENTEEN_CHARS_X"))
		}, mpps.DIMSEStatusInvalidAttributeValue, mpps.TagPerformedStationAETitle},
	} {
		ds := mustCreate(t)
		tc.mutate(ds)
		status, statusErr := statusOf(t, mpps.ValidateCreate(ds))
		if status != tc.status || len(statusErr.Tags) == 0 || statusErr.Tags[0] != tc.tag {
			t.Errorf("%s: status %04X tags %v", name, status, statusErr.Tags)
		}
		if strings.Contains(statusErr.Error(), "SYNTHETIC") {
			t.Errorf("%s: error exposes a value", name)
		}
	}
	// A study reference with an invalid UID inside the scheduled step.
	attributes := createAttributes()
	attributes.ScheduledSteps[0].StudyInstanceUID = "1..2"
	if _, err := mpps.BuildCreate(attributes); mpps.StatusOf(err) != mpps.DIMSEStatusInvalidAttributeValue {
		t.Fatalf("invalid study UID: %v", err)
	}
}

// Coding Scheme Designator is Type 1C (PS3.3 Table 8.8-1a): required with
// Code Value or Long Code Value, optional with URN Code Value.
func TestValidateCreateCodeItemIdentifiers(t *testing.T) {
	meaning := textElement(mpps.TagCodeMeaning, core.VRLO, "Synthetic procedure")
	scheme := textElement(mpps.TagCodingSchemeDesignator, core.VRSH, "99SYNTH")
	code := textElement(mpps.TagCodeValue, core.VRSH, "P1")
	long := textElement(mpps.TagLongCodeValue, core.VRUC, "SYNTHETIC-LONG-CODE-VALUE-0001")
	urn := textElement(mpps.TagURNCodeValue, core.VRUR, "urn:oid:1.2.826.0.1.3680043.10.947.9")
	put := func(elements ...core.Element) *object.Object {
		ds := mustCreate(t)
		ds.Put(core.Element{Header: core.ElementHeader{Tag: mpps.TagProcedureCodeSequence, VR: core.VRSQ}, Value: core.SequenceValue{Items: []core.DataSet{object.FromElements(elements, nil).ToDataSet()}}})
		return ds
	}
	for name, elements := range map[string][]core.Element{
		"URN Code Value":              {urn, meaning},
		"URN Code Value with scheme":  {scheme, urn, meaning},
		"Long Code Value with scheme": {scheme, meaning, long},
		"Code Value with scheme":      {code, scheme, meaning},
	} {
		if err := mpps.ValidateCreate(put(elements...)); err != nil {
			t.Errorf("%s: %v", name, err)
		}
	}
	for name, tc := range map[string]struct {
		elements []core.Element
		status   uint16
		tag      core.Tag
	}{
		"Code Value without scheme":      {[]core.Element{code, meaning}, mpps.DIMSEStatusMissingAttribute, mpps.TagCodingSchemeDesignator},
		"Long Code Value without scheme": {[]core.Element{meaning, long}, mpps.DIMSEStatusMissingAttribute, mpps.TagCodingSchemeDesignator},
		"no identifier":                  {[]core.Element{scheme, meaning}, mpps.DIMSEStatusMissingAttribute, mpps.TagCodeValue},
		"empty URN Code Value":           {[]core.Element{meaning, textElement(mpps.TagURNCodeValue, core.VRUR, "")}, mpps.DIMSEStatusMissingAttributeValue, mpps.TagURNCodeValue},
		"two identifiers":                {[]core.Element{code, scheme, meaning, urn}, mpps.DIMSEStatusInvalidAttributeValue, mpps.TagCodeValue},
	} {
		status, statusErr := statusOf(t, mpps.ValidateCreate(put(tc.elements...)))
		if status != tc.status || len(statusErr.Tags) == 0 || statusErr.Tags[0] != tc.tag {
			t.Errorf("%s: status %04X tags %v", name, status, statusErr.Tags)
		}
	}
}

// TestValidateCreateCodeMeaningAndSchemeVersion checks the conditions the
// MPPS code sequences put on Code Meaning and Coding Scheme Version (#2042).
func TestValidateCreateCodeMeaningAndSchemeVersion(t *testing.T) {
	meaning := textElement(mpps.TagCodeMeaning, core.VRLO, "Synthetic protocol")
	scheme := textElement(mpps.TagCodingSchemeDesignator, core.VRSH, "99SYNTH")
	version := textElement(mpps.TagCodingSchemeVersion, core.VRSH, "1.0")
	code := textElement(mpps.TagCodeValue, core.VRSH, "P1")
	urn := textElement(mpps.TagURNCodeValue, core.VRUR, "urn:oid:1.2.826.0.1.3680043.10.947.9")
	sequence := func(tag core.Tag, elements ...core.Element) core.Element {
		return core.Element{Header: core.ElementHeader{Tag: tag, VR: core.VRSQ}, Value: core.SequenceValue{Items: []core.DataSet{object.FromElements(elements, nil).ToDataSet()}}}
	}
	procedure := func(elements ...core.Element) *object.Object {
		ds := mustCreate(t)
		ds.Put(sequence(mpps.TagProcedureCodeSequence, elements...))
		return ds
	}
	scheduled := func(elements ...core.Element) *object.Object {
		ds := mustCreate(t)
		steps, ok := ds.GetSequence(mpps.TagScheduledStepAttributesSequence)
		if !ok || len(steps) != 1 {
			t.Fatalf("scheduled step attributes = %d items", len(steps))
		}
		stepElements := append(steps[0].Elements(), sequence(mpps.TagScheduledProtocolCodeSequence, elements...))
		ds.Put(sequence(mpps.TagScheduledStepAttributesSequence, stepElements...))
		return ds
	}
	for name, ds := range map[string]*object.Object{
		"procedure code without meaning":      procedure(code, scheme),
		"procedure code with version":         procedure(code, scheme, version, meaning),
		"scheduled protocol with meaning":     scheduled(code, scheme, meaning),
		"scheduled protocol URN with meaning": scheduled(urn, meaning),
	} {
		if err := mpps.ValidateCreate(ds); err != nil {
			t.Errorf("%s: %v", name, err)
		}
	}
	for name, tc := range map[string]struct {
		ds     *object.Object
		status uint16
		tag    core.Tag
	}{
		"procedure URN with version, no designator": {procedure(urn, version, meaning), mpps.DIMSEStatusInvalidAttributeValue, mpps.TagCodingSchemeVersion},
		"scheduled protocol without meaning":        {scheduled(code, scheme), mpps.DIMSEStatusMissingAttribute, mpps.TagCodeMeaning},
	} {
		status, statusErr := statusOf(t, mpps.ValidateCreate(tc.ds))
		if status != tc.status || len(statusErr.Tags) == 0 || statusErr.Tags[0] != tc.tag {
			t.Errorf("%s: status %04X tags %v", name, status, statusErr.Tags)
		}
	}
}

func textElement(tag core.Tag, vr core.VR, value string) core.Element {
	if value == "" {
		return core.Element{Header: core.ElementHeader{Tag: tag, VR: vr}, Value: core.StringValue(nil)}
	}
	return core.Element{Header: core.ElementHeader{Tag: tag, VR: vr}, Value: core.StringValue{value}}
}

func TestServiceLifecycleCompleted(t *testing.T) {
	ctx := context.Background()
	service, _ := newService(t, mpps.ServiceOptions{})
	step, err := service.Create(ctx, "", mustCreate(t))
	if err != nil || !core.IsValidUID(step.SOPInstanceUID) || step.Status != mpps.StatusInProgress {
		t.Fatalf("create %+v %v", step, err)
	}
	progress, err := mpps.BuildSet(mpps.Update{PerformedSeries: series(1)})
	if err != nil {
		t.Fatal(err)
	}
	if updated, err := service.Set(ctx, step.SOPInstanceUID, progress); err != nil || updated.Status != mpps.StatusInProgress || updated.Version != 2 {
		t.Fatalf("progress %+v %v", updated, err)
	}
	// COMPLETED without End Date/Time is refused and changes nothing.
	incomplete, err := mpps.BuildSet(mpps.Update{Status: mpps.StatusCompleted})
	if err != nil {
		t.Fatal(err)
	}
	if status, statusErr := statusOf(t, func() error { _, err := service.Set(ctx, step.SOPInstanceUID, incomplete); return err }()); status != mpps.DIMSEStatusProcessingFailure || !errors.Is(statusErr, mpps.ErrFinalState) {
		t.Fatalf("incomplete final: %04X", status)
	}
	if unchanged, _ := service.Get(ctx, step.SOPInstanceUID); unchanged.Version != 2 || unchanged.Status != mpps.StatusInProgress {
		t.Fatalf("partial state published: %+v", unchanged)
	}
	final, err := mpps.Complete("20260925", "103000", series(2))
	if err != nil {
		t.Fatal(err)
	}
	done, err := service.Set(ctx, step.SOPInstanceUID, final)
	if err != nil || done.Status != mpps.StatusCompleted {
		t.Fatalf("complete %+v %v", done, err)
	}
	ds := done.DataSet()
	if items, _ := ds.GetSequence(mpps.TagPerformedSeriesSequence); len(items) != 2 {
		t.Fatalf("series replaced wholesale: %d items", len(items))
	}
	// After a terminal state every N-SET is refused with A710.
	again, _ := mpps.BuildSet(mpps.Update{Status: mpps.StatusInProgress})
	_, err = service.Set(ctx, step.SOPInstanceUID, again)
	status, statusErr := statusOf(t, err)
	if status != mpps.DIMSEStatusProcessingFailure || statusErr.ErrorID != mpps.ErrorIDNoLongerUpdatable || statusErr.ErrorComment != mpps.ErrorCommentNoLongerUpdatable {
		t.Fatalf("set after COMPLETED: %+v", statusErr)
	}
}

func TestServiceLifecycleDiscontinuedAndRefusals(t *testing.T) {
	ctx := context.Background()
	service, repository := newService(t, mpps.ServiceOptions{Limits: mpps.Limits{MaxPerformedSeries: 2}})
	uid := testStudy + ".9.1"
	if _, err := service.Create(ctx, uid, mustCreate(t)); err != nil {
		t.Fatal(err)
	}
	// Duplicate UID.
	if _, err := service.Create(ctx, uid, mustCreate(t)); mpps.StatusOf(err) != mpps.DIMSEStatusDuplicateSOPInstance {
		t.Fatalf("duplicate: %v", err)
	}
	// Invalid UID in the command.
	if _, err := service.Create(ctx, "1..bad", mustCreate(t)); mpps.StatusOf(err) != mpps.DIMSEStatusInvalidObjectInstance {
		t.Fatalf("invalid UID: %v", err)
	}
	// Unknown step.
	progress, _ := mpps.BuildSet(mpps.Update{PerformedSeries: series(1)})
	if _, err := service.Set(ctx, testStudy+".9.404", progress); mpps.StatusOf(err) != mpps.DIMSEStatusNoSuchSOPInstance {
		t.Fatalf("unknown: %v", err)
	}
	// Attributes that N-SET may not change (identity) and a charset change.
	for name, element := range map[string]core.Element{
		"patient name":  textElement(mpps.TagPatientName, core.VRPN, "OTHER^NAME"),
		"modality":      textElement(mpps.TagModality, core.VRCS, "MR"),
		"character set": textElement(mpps.TagSpecificCharacterSet, core.VRCS, "ISO_IR 100"),
	} {
		modifications := object.FromElements([]core.Element{element}, nil)
		if _, err := service.Set(ctx, uid, modifications); mpps.StatusOf(err) != mpps.DIMSEStatusInvalidAttributeValue {
			t.Fatalf("%s: %v", name, err)
		}
	}
	// Limits.
	tooMany, _ := mpps.BuildSet(mpps.Update{PerformedSeries: series(3)})
	if _, err := service.Set(ctx, uid, tooMany); mpps.StatusOf(err) != mpps.DIMSEStatusResourceLimitation {
		t.Fatalf("series limit: %v", err)
	}
	stored, _ := repository.Get(ctx, uid)
	if stored.Version != 1 {
		t.Fatalf("refused updates changed the step: version %d", stored.Version)
	}
	reason := &mpps.Code{Value: "110514", Scheme: "DCM", Meaning: "Incorrect worklist entry selected"}
	discontinue, err := mpps.Discontinue("20260925", "102000", series(1), reason)
	if err != nil {
		t.Fatal(err)
	}
	done, err := service.Set(ctx, uid, discontinue)
	if err != nil || done.Status != mpps.StatusDiscontinued {
		t.Fatalf("discontinue %+v %v", done, err)
	}
	if _, err := service.Set(ctx, uid, progress); !errors.Is(err, mpps.ErrNotUpdatable) {
		t.Fatalf("set after DISCONTINUED: %v", err)
	}
	// An unsupported status value never reaches the service.
	if _, err := mpps.BuildSet(mpps.Update{Status: "PAUSED"}); mpps.StatusOf(err) != mpps.DIMSEStatusInvalidAttributeValue {
		t.Fatalf("status value: %v", err)
	}
}

func TestServiceIdenticalCreateReplayIsOptIn(t *testing.T) {
	ctx := context.Background()
	service, _ := newService(t, mpps.ServiceOptions{AcceptIdenticalCreateReplay: true})
	uid := testStudy + ".9.2"
	if _, err := service.Create(ctx, uid, mustCreate(t)); err != nil {
		t.Fatal(err)
	}
	if replay, err := service.Create(ctx, uid, mustCreate(t)); err != nil || replay.Version != 1 {
		t.Fatalf("identical replay: %+v %v", replay, err)
	}
	different := mustCreate(t)
	different.Put(textElement(mpps.TagPerformedLocation, core.VRSH, "ROOM2"))
	if _, err := service.Create(ctx, uid, different); mpps.StatusOf(err) != mpps.DIMSEStatusDuplicateSOPInstance {
		t.Fatalf("different replay: %v", err)
	}
	progress, _ := mpps.BuildSet(mpps.Update{PerformedSeries: series(1)})
	if _, err := service.Set(ctx, uid, progress); err != nil {
		t.Fatal(err)
	}
	if _, err := service.Create(ctx, uid, mustCreate(t)); mpps.StatusOf(err) != mpps.DIMSEStatusDuplicateSOPInstance {
		t.Fatalf("replay after update: %v", err)
	}
}

// Concurrent N-SETs never lose an update, and once one completes the step
// every later update is refused.
func TestServiceConcurrentUpdates(t *testing.T) {
	ctx := context.Background()
	service, _ := newService(t, mpps.ServiceOptions{})
	uid := testStudy + ".9.3"
	if _, err := service.Create(ctx, uid, mustCreate(t)); err != nil {
		t.Fatal(err)
	}
	// Every accepted update is counted exactly once; an update that loses
	// the bounded compare-and-swap race is refused explicitly, never lost.
	var wg sync.WaitGroup
	var mu sync.Mutex
	accepted := 0
	for i := 0; i < 16; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			description := fmt.Sprintf("UPDATE %d", i)
			modifications, _ := mpps.BuildSet(mpps.Update{Description: &description})
			_, err := service.Set(ctx, uid, modifications)
			mu.Lock()
			defer mu.Unlock()
			switch {
			case err == nil:
				accepted++
			case errors.Is(err, mpps.ErrConcurrentUpdate) && mpps.StatusOf(err) == mpps.DIMSEStatusProcessingFailure:
			default:
				t.Errorf("update %d: %v", i, err)
			}
		}(i)
	}
	wg.Wait()
	if step, _ := service.Get(ctx, uid); step.Version != uint64(1+accepted) || accepted == 0 {
		t.Fatalf("version %d after %d accepted updates", step.Version, accepted)
	}
	final, _ := mpps.Complete("20260925", "110000", series(1))
	var completed, refused int
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, err := service.Set(ctx, uid, final)
			mu.Lock()
			defer mu.Unlock()
			switch {
			case err == nil:
				completed++
			case errors.Is(err, mpps.ErrNotUpdatable):
				refused++
			default:
				t.Errorf("race: %v", err)
			}
		}()
	}
	wg.Wait()
	if completed != 1 || refused != 7 {
		t.Fatalf("completed %d refused %d", completed, refused)
	}
	canceled, cancel := context.WithCancel(ctx)
	cancel()
	if _, err := service.Create(canceled, testStudy+".9.4", mustCreate(t)); !errors.Is(err, context.Canceled) {
		t.Fatalf("canceled create: %v", err)
	}
	if _, err := service.Get(ctx, testStudy+".9.4"); !errors.Is(err, mpps.ErrNoSuchInstance) {
		t.Fatalf("canceled create stored: %v", err)
	}
}

// The SCU and SCP exchange N-CREATE and N-SET over a real association, and
// failures carry status, Error Comment and Error ID.
func TestClientAndServiceOverAssociation(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	service, _ := newService(t, mpps.ServiceOptions{})
	listener, err := ul.Listen(ul.ListenOptions{Address: "127.0.0.1:0", Context: ctx})
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	serverDone := make(chan error, 1)
	go func() {
		assoc, err := listener.AcceptAssociation(ul.AcceptOptions{
			AETitle: "RIS", Context: ctx, RequireMatchingCalledAE: true,
			SupportedAbstractSyntaxes: []string{mpps.SOPClassUID},
			SupportedTransferSyntaxes: mpps.PresentationContext().TransferSyntaxUIDs,
		})
		if err != nil {
			serverDone <- err
			return
		}
		defer assoc.Close()
		options := service.NormalizedOptions(assoc)
		serverDone <- dimse.ServeAssociation(ctx, assoc, dimse.AssociationSCPOptions{NormalizedSCP: &options})
	}()
	assoc, err := ul.DialContext(ctx, listener.Addr().String(), ul.DialOptions{
		CallingAETitle: "MODALITY", CalledAETitle: "RIS", Contexts: []ul.PresentationContext{mpps.PresentationContext()},
	})
	if err != nil {
		t.Fatal(err)
	}
	defer assoc.Close()
	client := mpps.NewClient(assoc)
	created, err := client.Create(ctx, "", mustCreate(t))
	if err != nil || created.Status != 0 || !core.IsValidUID(created.SOPInstanceUID) {
		t.Fatalf("create %+v %v", created, err)
	}
	final, _ := mpps.Complete("20260925", "103000", series(1))
	if result, err := client.Set(ctx, created.SOPInstanceUID, final); err != nil || result.Status != 0 {
		t.Fatalf("complete %+v %v", result, err)
	}
	again, _ := mpps.BuildSet(mpps.Update{PerformedSeries: series(1)})
	result, err := client.Set(ctx, created.SOPInstanceUID, again)
	var statusErr *dimse.NormalizedStatusError
	if !errors.As(err, &statusErr) || result.Status != mpps.DIMSEStatusProcessingFailure || result.StatusFields == nil ||
		result.StatusFields.ErrorIDOrNil == nil || *result.StatusFields.ErrorIDOrNil != mpps.ErrorIDNoLongerUpdatable {
		t.Fatalf("set after COMPLETED: %+v %v", result, err)
	}
	stored, err := service.Get(ctx, created.SOPInstanceUID)
	if err != nil || stored.Status != mpps.StatusCompleted {
		t.Fatalf("stored %+v %v", stored, err)
	}
	if name, err := stored.DataSet().LookupString(mpps.TagPatientName); err != nil || name != "SYNTHETIC^PATIENT" {
		t.Fatalf("stored attributes %q %v", name, err)
	}
	_ = assoc.Release(ctx)
	<-serverDone
}
