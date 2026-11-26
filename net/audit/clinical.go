package audit

import (
	"bytes"
	"encoding/base64"
	"encoding/xml"
	"errors"
	"strings"
	"time"
)

// ClinicalPolicy is separate from telemetry and legacy Event. The zero value
// does not authorize capture of patient names, study identities, or query data.
type ClinicalPolicy struct {
	IncludeIdentities bool
	IncludeQuery      bool
	Limits            ClinicalLimits
}

// ClinicalLimits bound each message and the retained state of one association.
// Zero selects finite defaults; negative limits are invalid.
type ClinicalLimits struct {
	MaxGroups, MaxInstances, MaxClasses, MaxObjects  int
	MaxIdentityBytes, MaxQueryBytes, MaxMessageBytes int
}

var ErrClinicalAudit = errors.New("dicom: clinical audit refused")

// ClinicalError contains only a stable code and field path, never a rejected
// value, patient identity, encoded query, underlying error, or remote URL.
type ClinicalError struct{ Code, Field string }

func (e *ClinicalError) Error() string       { return "dicom: clinical audit " + e.Code + " at " + e.Field }
func (e *ClinicalError) Unwrap() error       { return ErrClinicalAudit }
func clinicalError(code, field string) error { return &ClinicalError{Code: code, Field: field} }

func (p ClinicalPolicy) Normalize() (ClinicalPolicy, error) {
	fields := []*int{&p.Limits.MaxGroups, &p.Limits.MaxInstances, &p.Limits.MaxClasses, &p.Limits.MaxObjects, &p.Limits.MaxIdentityBytes, &p.Limits.MaxQueryBytes, &p.Limits.MaxMessageBytes}
	defaults := []int{64, 10000, 64, 1024, 1 << 20, 1 << 20, 4 << 20}
	for i, v := range fields {
		if *v < 0 {
			return p, clinicalError("limit", "Limits")
		}
		if *v == 0 {
			*v = defaults[i]
		}
	}
	return p, nil
}

type PatientIdentity struct{ ID, Name, Issuer string }
type StudyIdentity struct {
	UID, Name string
	// Query is optional and requires IncludeQuery. Name and Query are exclusive.
	Query                 []byte
	Accessions            []string
	SOPClasses            []SOPClass
	Encrypted, Anonymized *bool
}
type ClinicalTransfer struct {
	Common
	Source, Destination Participant
	Requestor           *Participant
	Patient             PatientIdentity
	Studies             []StudyIdentity
}
type ClinicalRecord struct {
	Common
	Actors  []Participant
	Patient PatientIdentity
	Studies []StudyIdentity
}
type ClinicalMedia struct {
	// ID is the explicit URI/volume identity in UserID (PS3.15 A.5.2.1).
	ID   string
	Type Code
	Host string
}
type ClinicalImport struct {
	Common
	Destinations []Participant
	Media        ClinicalMedia
	Sources      []Participant
	Patients     []PatientIdentity
	Studies      []StudyIdentity
}
type ClinicalExport struct {
	Common
	Sources  []Participant
	Media    ClinicalMedia
	Patients []PatientIdentity
	Studies  []StudyIdentity
}
type ClinicalAuditLog struct {
	Common
	Actors []Participant
	URI    string
}

func clinicalCommon(c Common) Common {
	if c.Time.IsZero() {
		c.Time = time.Now()
	}
	c.Time = c.Time.UTC()
	return c
}
func patientObject(p PatientIdentity) ParticipantObject {
	o := Patient{ID: p.ID, Name: p.Name}.object()
	if p.Issuer != "" {
		o.Details = []ParticipantObjectDetail{{Type: "IssuerOfPatientID", Value: base64.StdEncoding.EncodeToString([]byte(p.Issuer))}}
	}
	return o
}
func studyObject(s StudyIdentity) ParticipantObject {
	s = cloneStudy(s)
	o := Study{UID: s.UID, Accessions: s.Accessions, SOPClasses: s.SOPClasses}.object("")
	o.Name = s.Name
	if len(s.Query) > 0 {
		o.Query = base64.StdEncoding.EncodeToString(s.Query)
	}
	o.Description.Encrypted = s.Encrypted
	o.Description.Anonymized = s.Anonymized
	return o
}
func checkClinicalInputs(p ClinicalPolicy, studies []StudyIdentity, patients []PatientIdentity) error {
	p, err := p.Normalize()
	if err != nil {
		return err
	}
	if len(studies)+len(patients) > p.Limits.MaxObjects {
		return clinicalError("limit", "Objects")
	}
	n := 0
	add := func(v int) bool {
		if v > p.Limits.MaxIdentityBytes-n {
			return false
		}
		n += v
		return true
	}
	for _, v := range patients {
		if v.Issuer != "" && !clinicalText(v.Issuer) {
			return clinicalError("text", "Patient.Issuer")
		}
		if !add(len(v.ID)) || !add(len(v.Name)) || !add(len(v.Issuer)) {
			return clinicalError("limit", "Identity")
		}
	}
	for _, v := range studies {
		if !add(len(v.UID)) || !add(len(v.Name)) {
			return clinicalError("limit", "Identity")
		}
		if len(v.Query) > p.Limits.MaxQueryBytes {
			return clinicalError("limit", "Study.Query")
		}
		if len(v.SOPClasses) > p.Limits.MaxClasses {
			return clinicalError("limit", "Study.SOPClasses")
		}
		if len(v.Accessions) > p.Limits.MaxObjects {
			return clinicalError("limit", "Study.Accessions")
		}
		for _, a := range v.Accessions {
			if !add(len(a)) {
				return clinicalError("limit", "Identity")
			}
		}
		for _, c := range v.SOPClasses {
			if len(c.Instances) > p.Limits.MaxInstances || c.NumberOfInstances > p.Limits.MaxInstances {
				return clinicalError("limit", "Study.Instances")
			}
			if !add(len(c.UID)) {
				return clinicalError("limit", "Identity")
			}
			for _, i := range c.Instances {
				if !add(len(i.UID)) {
					return clinicalError("limit", "Identity")
				}
			}
		}
	}
	return nil
}

type clinicalXMLBuffer struct {
	bytes.Buffer
	limit int
}

func (w *clinicalXMLBuffer) Write(data []byte) (int, error) {
	if len(data) > w.limit-w.Len() {
		return 0, clinicalError("limit", "Message")
	}
	return w.Buffer.Write(data)
}
func encodeClinical(p ClinicalPolicy, m Message) ([]byte, error) {
	p, err := p.Normalize()
	if err != nil {
		return nil, err
	}
	w := &clinicalXMLBuffer{limit: p.Limits.MaxMessageBytes}
	if _, err := w.Write([]byte(xml.Header)); err != nil {
		return nil, err
	}
	if err := xml.NewEncoder(w).Encode(m); err != nil {
		if errors.Is(err, ErrClinicalAudit) {
			return nil, err
		}
		return nil, clinicalError("encoding", "Message")
	}
	return w.Bytes(), nil
}
func qualified(p ClinicalPolicy, m Message) (Message, error) {
	if err := m.ValidateClinical(p); err != nil {
		return Message{}, err
	}
	if _, err := encodeClinical(p, m); err != nil {
		return Message{}, err
	}
	return m, nil
}

// MarshalClinical validates a supported event profile and writes with a finite
// byte budget. Legacy Marshal does not establish specialized conformance.
func (m Message) MarshalClinical(p ClinicalPolicy) ([]byte, error) {
	if err := m.ValidateClinical(p); err != nil {
		return nil, err
	}
	return encodeClinical(p, m)
}
func transferClinical(p ClinicalPolicy, t ClinicalTransfer, id Code, action string) (Message, error) {
	if err := checkClinicalInputs(p, t.Studies, []PatientIdentity{t.Patient}); err != nil {
		return Message{}, err
	}
	t.Common = clinicalCommon(t.Common)
	m := Message{Event: t.event(id, action), Sources: []AuditSource{t.Common.Source.element()}, Participants: []ActiveParticipant{t.Source.element(RoleSource), t.Destination.element(RoleDestination)}}
	if t.Requestor != nil {
		v := *t.Requestor
		v.Requestor = true
		m.Participants = append(m.Participants, v.element())
	}
	for _, s := range t.Studies {
		m.Objects = append(m.Objects, studyObject(s))
	}
	m.Objects = append(m.Objects, patientObject(t.Patient))
	return qualified(p, m)
}
func BuildInstancesTransferred(p ClinicalPolicy, t ClinicalTransfer, action string) (Message, error) {
	return transferClinical(p, t, EventInstancesTransferred, action)
}
func BuildBeginTransferring(p ClinicalPolicy, t ClinicalTransfer) (Message, error) {
	return transferClinical(p, t, EventBeginTransferring, ActionExecute)
}
func recordClinical(p ClinicalPolicy, r ClinicalRecord, id Code, action string) (Message, error) {
	if err := checkClinicalInputs(p, r.Studies, []PatientIdentity{r.Patient}); err != nil {
		return Message{}, err
	}
	r.Common = clinicalCommon(r.Common)
	m := Message{Event: r.event(id, action), Sources: []AuditSource{r.Common.Source.element()}}
	for _, a := range r.Actors {
		m.Participants = append(m.Participants, a.element())
	}
	for _, s := range r.Studies {
		m.Objects = append(m.Objects, studyObject(s))
	}
	m.Objects = append(m.Objects, patientObject(r.Patient))
	return qualified(p, m)
}
func BuildInstancesAccessed(p ClinicalPolicy, r ClinicalRecord, action string) (Message, error) {
	return recordClinical(p, r, EventInstancesAccessed, action)
}
func BuildStudyDeleted(p ClinicalPolicy, r ClinicalRecord) (Message, error) {
	return recordClinical(p, r, EventStudyDeleted, ActionDelete)
}
func BuildOrderRecord(p ClinicalPolicy, r ClinicalRecord, action string) (Message, error) {
	return recordClinical(p, r, EventOrderRecord, action)
}
func BuildPatientRecord(p ClinicalPolicy, r ClinicalRecord, action string) (Message, error) {
	return recordClinical(p, r, EventPatientRecord, action)
}

// ProcedureRecord uses the same explicit Name field as the generic schema.
// It never invents a PatientName even though A.5.3.15 permits its omission.
func BuildProcedureRecord(p ClinicalPolicy, r ClinicalRecord, action string) (Message, error) {
	return recordClinical(p, r, EventProcedureRecord, action)
}
func mediaParticipant(m ClinicalMedia, role Code) ActiveParticipant {
	o := Participant{UserID: m.ID, Host: m.Host}.element(role)
	o.MediaIdentifier = &MediaIdentifier{Type: m.Type}
	return o
}
func BuildImport(p ClinicalPolicy, r ClinicalImport) (Message, error) {
	if err := checkClinicalInputs(p, r.Studies, r.Patients); err != nil {
		return Message{}, err
	}
	r.Common = clinicalCommon(r.Common)
	m := Message{Event: r.event(EventImport, ActionCreate), Sources: []AuditSource{r.Common.Source.element()}}
	for _, a := range r.Destinations {
		m.Participants = append(m.Participants, a.element(RoleDestination))
	}
	m.Participants = append(m.Participants, mediaParticipant(r.Media, RoleSourceMedia))
	for _, a := range r.Sources {
		m.Participants = append(m.Participants, a.element(RoleSource))
	}
	for _, s := range r.Studies {
		m.Objects = append(m.Objects, studyObject(s))
	}
	for _, v := range r.Patients {
		m.Objects = append(m.Objects, patientObject(v))
	}
	return qualified(p, m)
}
func BuildExport(p ClinicalPolicy, r ClinicalExport) (Message, error) {
	if err := checkClinicalInputs(p, r.Studies, r.Patients); err != nil {
		return Message{}, err
	}
	r.Common = clinicalCommon(r.Common)
	m := Message{Event: r.event(EventExport, ActionRead), Sources: []AuditSource{r.Common.Source.element()}}
	for _, a := range r.Sources {
		m.Participants = append(m.Participants, a.element(RoleSource))
	}
	m.Participants = append(m.Participants, mediaParticipant(r.Media, RoleDestinationMedia))
	for _, s := range r.Studies {
		m.Objects = append(m.Objects, studyObject(s))
	}
	for _, v := range r.Patients {
		m.Objects = append(m.Objects, patientObject(v))
	}
	return qualified(p, m)
}
func BuildAuditLogUsed(p ClinicalPolicy, r ClinicalAuditLog) (Message, error) {
	r.Common = clinicalCommon(r.Common)
	m := Message{Event: r.event(EventAuditLogUsed, ActionRead), Sources: []AuditSource{r.Common.Source.element()}, Objects: []ParticipantObject{{ID: r.URI, IDTypeCode: Code{Value: "12", CodeSystemName: "RFC-3881", OriginalText: "URI"}, Name: "Security Audit Log", TypeCode: "2", Role: "13"}}}
	for _, a := range r.Actors {
		m.Participants = append(m.Participants, a.element())
	}
	return qualified(p, m)
}
func BuildQuery(p ClinicalPolicy, q QueryRequest) (Message, error) {
	if !p.IncludeQuery {
		return Message{}, clinicalError("opt_in", "Query")
	}
	q.Common = clinicalCommon(q.Common)
	if err := validateQueryData(p, q.Query, q.TransferSyntaxUID); err != nil {
		return Message{}, err
	}
	return qualified(p, Query(q))
}

func clinicalUID(v string) bool {
	if len(v) == 0 || len(v) > 64 || strings.TrimSpace(v) != v {
		return false
	}
	parts := strings.Split(v, ".")
	if len(parts) < 2 {
		return false
	}
	for _, v := range parts {
		if v == "" || len(v) > 1 && v[0] == '0' {
			return false
		}
		for _, r := range v {
			if r < '0' || r > '9' {
				return false
			}
		}
	}
	return true
}
