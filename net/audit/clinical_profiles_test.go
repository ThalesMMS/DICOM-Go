package audit

import (
	"bytes"
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"github.com/ThalesMMS/dicom-go/core"
	"github.com/ThalesMMS/dicom-go/object"
	"github.com/ThalesMMS/dicom-go/transfer"
	"strings"
	"sync"
	"testing"
	"time"
)

func clinicalFixture() (ClinicalPolicy, ClinicalTransfer, ClinicalRecord) {
	p := ClinicalPolicy{IncludeIdentities: true, IncludeQuery: true}
	c := Common{Time: atnaTime, Source: Source{ID: "SYNTHETIC-TWIN"}}
	patient := PatientIdentity{ID: "SYNTHETIC-ID", Name: "SYNTHETIC^PATIENT", Issuer: "SYNTHETIC-ISSUER"}
	study := StudyIdentity{UID: "1.2.826.0.1.3680043.10.1282.1", Name: "Synthetic Study", SOPClasses: []SOPClass{{UID: "1.2.840.10008.5.1.4.1.1.2", NumberOfInstances: 100}}}
	t := ClinicalTransfer{Common: c, Source: Participant{UserID: "SYNTHETIC-SCU", Host: "192.0.2.1", Requestor: true}, Destination: Participant{UserID: "SYNTHETIC-SCP", Host: "scp.example.test"}, Patient: patient, Studies: []StudyIdentity{study}}
	r := ClinicalRecord{Common: c, Actors: []Participant{{UserID: "synthetic-operator", Requestor: true}}, Patient: patient, Studies: []StudyIdentity{study}}
	return p, t, r
}
func clinicalMessages(t *testing.T) map[string]Message {
	t.Helper()
	p, x, r := clinicalFixture()
	out := map[string]Message{}
	add := func(name string, m Message, err error) {
		t.Helper()
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		out[name] = m
	}
	m, e := BuildBeginTransferring(p, x)
	add("begin", m, e)
	m, e = BuildInstancesTransferred(p, x, ActionRead)
	add("transfer", m, e)
	m, e = BuildInstancesAccessed(p, r, ActionRead)
	add("access", m, e)
	m, e = BuildStudyDeleted(p, r)
	add("delete", m, e)
	record := r
	record.Studies = nil
	m, e = BuildOrderRecord(p, record, ActionCreate)
	add("order", m, e)
	m, e = BuildPatientRecord(p, record, ActionUpdate)
	add("patient", m, e)
	m, e = BuildProcedureRecord(p, r, ActionRead)
	add("procedure", m, e)
	media := ClinicalMedia{ID: "file:///synthetic/export", Type: Code{Value: "110035", CodeSystemName: "DCM", OriginalText: "CD"}}
	m, e = BuildImport(p, ClinicalImport{Common: r.Common, Destinations: r.Actors, Media: media, Patients: []PatientIdentity{r.Patient}, Studies: r.Studies})
	add("import", m, e)
	m, e = BuildExport(p, ClinicalExport{Common: r.Common, Sources: r.Actors, Media: media, Patients: []PatientIdentity{r.Patient}, Studies: r.Studies})
	add("export", m, e)
	m, e = BuildAuditLogUsed(p, ClinicalAuditLog{Common: r.Common, Actors: r.Actors, URI: "urn:synthetic:audit"})
	add("audit-log", m, e)
	ds := object.FromElements([]core.Element{{Header: core.ElementHeader{Tag: core.NewTag(0x0008, 0x0052), VR: core.VRCS}, Value: core.StringValue{"STUDY"}}}, nil)
	raw, e := EncodeClinicalQuery(p, ds, transfer.ExplicitVRLittleEndian)
	if e != nil {
		t.Fatal(e)
	}
	m, e = BuildQuery(p, QueryRequest{Common: r.Common, Source: x.Source, Destination: x.Destination, SOPClassUID: "1.2.840.10008.5.1.4.1.2.2.1", Query: raw, TransferSyntaxUID: transfer.ExplicitVRLittleEndian.UID})
	add("query", m, e)
	return out
}
func TestClinicalBuildersProfilesAndDetachedInputs(t *testing.T) {
	for name, m := range clinicalMessages(t) {
		t.Run(name, func(t *testing.T) {
			p, _, _ := clinicalFixture()
			b, err := m.MarshalClinical(p)
			if err != nil {
				t.Fatal(err)
			}
			if bytes.Contains(b, []byte(`<ParticipantObjectContainsStudy></`)) {
				t.Fatal("empty optional element")
			}
			if err := m.ValidateClinical(p); err != nil {
				t.Fatal(err)
			}
		})
	}
	p, x, _ := clinicalFixture()
	flag := true
	x.Studies[0].Encrypted = &flag
	m, err := BuildInstancesTransferred(p, x, ActionRead)
	if err != nil {
		t.Fatal(err)
	}
	x.Studies[0].SOPClasses[0].NumberOfInstances = 7
	flag = false
	if m.Objects[0].Description.SOPClasses[0].NumberOfInstances != 100 || !*m.Objects[0].Description.Encrypted {
		t.Fatal("caller mutated message")
	}
}
func TestClinicalRefusalsArePHIFree(t *testing.T) {
	p, x, r := clinicalFixture()
	x.Patient.Name = "SECRET^PATIENT"
	x.Patient.ID = "SECRET-ID"
	x.Studies[0].Name = "SECRET-STUDY"
	checks := []func() error{
		func() error { _, e := BuildInstancesTransferred(ClinicalPolicy{}, x, ActionRead); return e },
		func() error {
			v := x
			v.Patient.Name = ""
			_, e := BuildInstancesTransferred(p, v, ActionRead)
			return e
		},
		func() error {
			v := x
			v.Studies = []StudyIdentity{{UID: "1.2.3", Name: "SECRET-STUDY", Query: []byte("SECRET-QUERY")}}
			_, e := BuildInstancesTransferred(p, v, ActionRead)
			return e
		},
		func() error {
			_, e := BuildStudyDeleted(p, ClinicalRecord{Common: r.Common, Patient: r.Patient})
			return e
		},
		func() error {
			v := p
			v.Limits.MaxIdentityBytes = 1
			_, e := BuildInstancesTransferred(v, x, ActionRead)
			return e
		},
		func() error { v := p; v.Limits.MaxQueryBytes = -1; _, e := v.Normalize(); return e },
	}
	for i, check := range checks {
		e := check()
		var ce *ClinicalError
		if !errors.Is(e, ErrClinicalAudit) || !errors.As(e, &ce) {
			t.Fatalf("case %d: %v", i, e)
		}
		if strings.Contains(e.Error(), "SECRET") || ce.Code == "" {
			t.Fatalf("unsafe error %v", e)
		}
	}
	m := clinicalMessages(t)["transfer"]
	m.Participants[0].NetworkAccessPointTypeCode = "12"
	if m.ValidateClinical(p) == nil {
		t.Fatal("invalid multi-digit network type accepted")
	}
	m = clinicalMessages(t)["transfer"]
	m.Objects[0].Name = ""
	if m.ValidateClinical(p) == nil {
		t.Fatal("missing StudyName accepted")
	}
}
func TestClinicalQueryEncodingActualSyntaxAndCharset(t *testing.T) {
	p, _, _ := clinicalFixture()
	ds := object.FromElements([]core.Element{{Header: core.ElementHeader{Tag: core.NewTag(0x0008, 0x0005), VR: core.VRCS}, Value: core.StringValue{"ISO_IR 192"}}, {Header: core.ElementHeader{Tag: core.NewTag(0x0010, 0x0010), VR: core.VRPN}, Value: core.StringValue{"SYNTHÉTIQUE^PATIENT"}}}, nil)
	for _, ts := range []transfer.Syntax{transfer.ExplicitVRLittleEndian, transfer.ExplicitVRBigEndian, transfer.ImplicitVRLittleEndian, transfer.DeflatedExplicitVRLittleEndian} {
		t.Run(ts.UID, func(t *testing.T) {
			data, err := EncodeClinicalQuery(p, ds, ts)
			if err != nil {
				t.Fatal(err)
			}
			decoded, err := object.ReadDataSet(bytes.NewReader(data), ts)
			if err != nil {
				t.Fatal(err)
			}
			name, _ := decoded.GetString(core.NewTag(0x0010, 0x0010))
			if name != "SYNTHÉTIQUE^PATIENT" {
				t.Fatalf("charset lost %q", name)
			}
		})
	}
	if _, err := EncodeClinicalQuery(ClinicalPolicy{}, ds, transfer.ExplicitVRLittleEndian); err == nil {
		t.Fatal("query opt-in bypass")
	}
	v := p
	v.Limits.MaxQueryBytes = 3
	if _, err := EncodeClinicalQuery(v, ds, transfer.ExplicitVRLittleEndian); err == nil {
		t.Fatal("query limit bypass")
	}
	if err := validateQueryData(p, []byte("not DICOM"), transfer.ExplicitVRLittleEndian.UID); err == nil {
		t.Fatal("opaque query accepted")
	}
	// DICM inside a raw dataset value is not a Part 10 preamble marker.
	marker := object.FromElements([]core.Element{{Header: core.ElementHeader{Tag: core.NewTag(0x0008, 0x1030), VR: core.VRLO}, Value: core.StringValue{strings.Repeat("A", 120) + "DICM"}}}, nil)
	data, err := EncodeClinicalQuery(p, marker, transfer.ExplicitVRLittleEndian)
	if err != nil || len(data) < 132 || string(data[128:132]) != "DICM" {
		t.Fatalf("literal DICM raw value refused: %v", err)
	}
	var part10 bytes.Buffer
	image := object.FromElements([]core.Element{{Header: core.ElementHeader{Tag: core.NewTag(0x0008, 0x0016), VR: core.VRUI}, Value: core.StringValue{"1.2.840.10008.5.1.4.1.1.2"}}, {Header: core.ElementHeader{Tag: core.NewTag(0x0008, 0x0018), VR: core.VRUI}, Value: core.StringValue{"1.2.1282.999"}}}, nil)
	if err := object.WriteFile(&part10, &object.File{Dataset: image, TransferSyntax: transfer.ExplicitVRLittleEndian}); err != nil {
		t.Fatal(err)
	}
	if err := validateQueryData(p, part10.Bytes(), transfer.ExplicitVRLittleEndian.UID); err == nil {
		t.Fatal("Part 10 file accepted as raw query")
	}
}
func TestClinicalAssociationAggregates100AndSeparatesOutcomes(t *testing.T) {
	p, x, _ := clinicalFixture()
	var messages []Message
	a, err := NewAssociationAudit("synthetic-association", AssociationAuditOptions{Policy: p, Common: x.Common, Source: x.Source, Destination: x.Destination, Emit: func(_ context.Context, m Message) error { messages = append(messages, m); return nil }})
	if err != nil {
		t.Fatal(err)
	}
	for i := range 100 {
		r := InstanceTransfer{Time: atnaTime.Add(time.Duration(i) * time.Second), Patient: x.Patient, Study: x.Studies[0], SOPClassUID: x.Studies[0].SOPClasses[0].UID, SOPInstanceUID: fmt.Sprintf("1.2.826.0.1.3680043.10.1282.100.%d", i+1)}
		if err := a.Record(r); err != nil {
			t.Fatal(err)
		}
	}
	if err := a.Finish(context.Background()); err != nil {
		t.Fatal(err)
	}
	if len(messages) != 1 || messages[0].Objects[0].Description.SOPClasses[0].NumberOfInstances != 100 {
		t.Fatalf("not aggregate %d", len(messages))
	}
	if err := a.Finish(context.Background()); err != nil {
		t.Fatal(err)
	}
	if a.Stats().Emitted != 1 || !a.Stats().Finished {
		t.Fatalf("stats %+v", a.Stats())
	}
	if err := a.Record(InstanceTransfer{}); err == nil {
		t.Fatal("Record after Finish")
	}
}
func TestClinicalAssociationLimitsDedupRetryAndConcurrency(t *testing.T) {
	p, x, _ := clinicalFixture()
	calls := 0
	var accepted []Message
	a, err := NewAssociationAudit("retry", AssociationAuditOptions{Policy: p, Common: x.Common, Source: x.Source, Destination: x.Destination, Emit: func(_ context.Context, m Message) error {
		calls++
		if calls == 1 {
			m.Objects[0].Name = "MUTATED"
			return errors.New("SECRET-sink")
		}
		accepted = append(accepted, m)
		return nil
	}})
	if err != nil {
		t.Fatal(err)
	}
	r := InstanceTransfer{Time: atnaTime, Patient: x.Patient, Study: x.Studies[0], SOPClassUID: x.Studies[0].SOPClasses[0].UID, SOPInstanceUID: "1.2.826.0.1.3680043.10.1282.2"}
	var wg sync.WaitGroup
	for range 20 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if e := a.Record(r); e != nil {
				t.Error(e)
			}
		}()
	}
	wg.Wait()
	if a.Stats().Recorded != 1 || a.Stats().Duplicates != 19 {
		t.Fatalf("stats %+v", a.Stats())
	}
	if err := a.Finish(context.Background()); err == nil || strings.Contains(err.Error(), "SECRET") {
		t.Fatalf("sink error %v", err)
	}
	if err := a.Finish(context.Background()); err != nil {
		t.Fatal(err)
	}
	if len(accepted) != 1 || accepted[0].Objects[0].Name != x.Studies[0].Name {
		t.Fatal("mutating sink poisoned retry")
	}
	p.Limits.MaxInstances = 1
	a, _ = NewAssociationAudit("limited", AssociationAuditOptions{Policy: p, Common: x.Common, Source: x.Source, Destination: x.Destination, Emit: func(context.Context, Message) error { return nil }})
	if a.Record(r) != nil {
		t.Fatal("initial record failed")
	}
	r.SOPInstanceUID = "1.2.3.4"
	if a.Record(r) == nil {
		t.Fatal("instance bound ignored")
	}
}
func TestClinicalSyslogFullSpoolRefusesWithoutEviction(t *testing.T) {
	p, _, _ := clinicalFixture()
	m := clinicalMessages(t)["transfer"]
	l := &SyslogLogger{Network: SyslogTCP, Address: "audit.example.test:6514", MaxSpoolMessages: 1}
	if e := l.LogClinical(p, m); e != nil {
		t.Fatal(e)
	}
	if e := l.LogClinical(p, m); e == nil {
		t.Fatal("full spool accepted")
	}
	if s := l.Stats(); s.Spooled != 1 || s.Dropped != 0 {
		t.Fatalf("stats %+v", s)
	}
	q := clinicalMessages(t)["query"]
	b, e := base64.StdEncoding.DecodeString(q.Objects[0].Query)
	if e != nil || len(b) == 0 {
		t.Fatal("query missing")
	}
}

func TestClinicalAssociationKnownThirdPartyRequestorIsDetached(t *testing.T) {
	p, x, _ := clinicalFixture()
	x.Source.Requestor = false
	x.Destination.Requestor = false
	requestor := Participant{UserID: "SYNTHETIC-MOVESCU", Host: "192.0.2.2", Requestor: true}
	var got Message
	opts := AssociationAuditOptions{Policy: p, Common: x.Common, Source: x.Source, Destination: x.Destination, Requestor: &requestor, Emit: func(_ context.Context, m Message) error { got = m; return nil }}
	a, err := NewAssociationAudit("synthetic-move-store-association", opts)
	if err != nil {
		t.Fatal(err)
	}
	requestor.UserID = "MUTATED"
	opts.Source.UserID = "MUTATED"
	opts.Common.Source.ID = "MUTATED"
	if err := a.Record(InstanceTransfer{Time: atnaTime, Patient: x.Patient, Study: x.Studies[0], SOPClassUID: x.Studies[0].SOPClasses[0].UID, SOPInstanceUID: "1.2.1282.1.1"}); err != nil {
		t.Fatal(err)
	}
	if err := a.Finish(context.Background()); err != nil {
		t.Fatal(err)
	}
	if len(got.Participants) != 3 || got.Participants[0].UserIsRequestor || got.Participants[1].UserIsRequestor || !got.Participants[2].UserIsRequestor || got.Participants[2].UserID != "SYNTHETIC-MOVESCU" || got.Sources[0].ID != x.Common.Source.ID {
		t.Fatalf("requestor attribution %+v", got.Participants)
	}
}
func TestClinicalAssociationSeparatesPatientStudyClassAndOutcome(t *testing.T) {
	p, x, _ := clinicalFixture()
	var emitted []Message
	a, err := NewAssociationAudit("synthetic-multiple", AssociationAuditOptions{Policy: p, Common: x.Common, Source: x.Source, Destination: x.Destination, Emit: func(_ context.Context, m Message) error { emitted = append(emitted, m); return nil }})
	if err != nil {
		t.Fatal(err)
	}
	r := InstanceTransfer{Time: atnaTime, Patient: x.Patient, Study: x.Studies[0], SOPClassUID: x.Studies[0].SOPClasses[0].UID, SOPInstanceUID: "1.2.1282.1.1"}
	if err := a.Record(r); err != nil {
		t.Fatal(err)
	}
	r.SOPInstanceUID = "1.2.1282.1.2"
	r.SOPClassUID = "1.2.840.10008.5.1.4.1.1.4"
	if err := a.Record(r); err != nil {
		t.Fatal(err)
	}
	r.SOPInstanceUID = "1.2.1282.1.3"
	r.Outcome = OutcomeSerious
	if err := a.Record(r); err != nil {
		t.Fatal(err)
	}
	r.Study.UID = "1.2.1282.2"
	r.Patient.ID = "SYNTHETIC-SECOND"
	r.Patient.Name = "SYNTHETIC^SECOND"
	r.SOPInstanceUID = "1.2.1282.2.1"
	if err := a.Record(r); err != nil {
		t.Fatal(err)
	}
	// One study cannot be attributed to two patient identities silently.
	bad := r
	bad.Patient.Name = "CONFLICT^NAME"
	bad.SOPInstanceUID = "1.2.1282.2.2"
	if err := a.Record(bad); err == nil {
		t.Fatal("patient conflict accepted")
	}
	if err := a.Finish(context.Background()); err != nil {
		t.Fatal(err)
	}
	if len(emitted) != 3 {
		t.Fatalf("messages %d", len(emitted))
	}
	classes := 0
	for _, m := range emitted {
		if len(m.Objects) != 2 {
			t.Fatal("patients mixed")
		}
		classes += len(m.Objects[0].Description.SOPClasses)
	}
	if classes != 4 {
		t.Fatalf("classes %d", classes)
	}
}
func TestClinicalBoundedXMLAndEncodedInputs(t *testing.T) {
	p, x, _ := clinicalFixture()
	p.Limits.MaxMessageBytes = 20
	if _, err := BuildInstancesTransferred(p, x, ActionRead); !errors.Is(err, ErrClinicalAudit) {
		t.Fatalf("message cap %v", err)
	}
	p, _, _ = clinicalFixture()
	m := clinicalMessages(t)["query"]
	p.Limits.MaxQueryBytes = 2
	if err := m.ValidateClinical(p); !errors.Is(err, ErrClinicalAudit) {
		t.Fatalf("encoded query cap %v", err)
	}
	p, _, _ = clinicalFixture()
	m = clinicalMessages(t)["transfer"]
	m.Objects[0].LifeCycle = "16"
	if m.ValidateClinical(p) == nil {
		t.Fatal("lifecycle enum ignored")
	}
}
