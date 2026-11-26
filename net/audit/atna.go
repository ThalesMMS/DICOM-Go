package audit

import (
	"bytes"
	"encoding/base64"
	"encoding/xml"
	"errors"
	"fmt"
	"strings"
	"time"
)

// ATNA audit messages (PS3.15 A.5, IHE ITI ATNA). Message is the
// AuditMessage of the DICOM audit schema (A.5.1); the builders produce the
// historical envelopes. Marshal performs basic structural checks, not A.5.3
// specialization qualification. Use the explicit ClinicalPolicy builders and
// ValidateClinical for complete, opt-in clinical messages.

// ErrAuditMessage reports an audit message that is missing a required part.
var ErrAuditMessage = errors.New("dicom: invalid audit message")

// Code is a coded value (CodedValueType): csd-code with its coding scheme
// and meaning.
type Code struct {
	Value          string `xml:"csd-code,attr"`
	CodeSystemName string `xml:"codeSystemName,attr,omitempty"`
	OriginalText   string `xml:"originalText,attr,omitempty"`
}

func dcm(value, meaning string) Code {
	return Code{Value: value, CodeSystemName: "DCM", OriginalText: meaning}
}

// Event IDs, event type codes and role IDs of PS3.16 CID 400-404 used by
// the PS3.15 A.5.3 messages.
var (
	EventApplicationActivity  = dcm("110100", "Application Activity")
	EventAuditLogUsed         = dcm("110101", "Audit Log Used")
	EventBeginTransferring    = dcm("110102", "Begin Transferring DICOM Instances")
	EventInstancesAccessed    = dcm("110103", "DICOM Instances Accessed")
	EventInstancesTransferred = dcm("110104", "DICOM Instances Transferred")
	EventStudyDeleted         = dcm("110105", "DICOM Study Deleted")
	EventExport               = dcm("110106", "Export")
	EventImport               = dcm("110107", "Import")
	EventOrderRecord          = dcm("110109", "Order Record")
	EventPatientRecord        = dcm("110110", "Patient Record")
	EventProcedureRecord      = dcm("110111", "Procedure Record")
	EventQuery                = dcm("110112", "Query")
	EventSecurityAlert        = dcm("110113", "Security Alert")
	EventUserAuthentication   = dcm("110114", "User Authentication")

	TypeApplicationStart       = dcm("110120", "Application Start")
	TypeApplicationStop        = dcm("110121", "Application Stop")
	TypeLogin                  = dcm("110122", "Login")
	TypeLogout                 = dcm("110123", "Logout")
	TypeNodeAuthentication     = dcm("110126", "Node Authentication")
	TypeNetworkConfiguration   = dcm("110128", "Network Configuration")
	TypeSecurityConfiguration  = dcm("110129", "Security Configuration")
	TypeSoftwareConfiguration  = dcm("110131", "Software Configuration")
	TypeUseOfRestrictedFeature = dcm("110132", "Use of Restricted Function")
	TypeAuditRecordingStopped  = dcm("110133", "Audit Recording Stopped")
	TypeAuditRecordingStarted  = dcm("110134", "Audit Recording Started")

	RoleApplication         = dcm("110150", "Application")
	RoleApplicationLauncher = dcm("110151", "Application Launcher")
	RoleDestination         = dcm("110152", "Destination Role ID")
	RoleSource              = dcm("110153", "Source Role ID")
	RoleDestinationMedia    = dcm("110154", "Destination Media")
	RoleSourceMedia         = dcm("110155", "Source Media")

	idStudyInstanceUID = dcm("110180", "Study Instance UID")
	idSOPClassUID      = dcm("110181", "SOP Class UID")
	idNodeID           = dcm("110182", "Node ID")
	idPatientNumber    = Code{Value: "2", CodeSystemName: "RFC-3881", OriginalText: "Patient Number"}
)

// Event action codes.
const (
	ActionCreate  = "C"
	ActionRead    = "R"
	ActionUpdate  = "U"
	ActionDelete  = "D"
	ActionExecute = "E"
)

// Event outcome indicators.
const (
	OutcomeSuccess      = "0"
	OutcomeMinorFailure = "4"
	OutcomeSerious      = "8"
	OutcomeMajorFailure = "12"
)

// Message is an AuditMessage.
type Message struct {
	XMLName      xml.Name            `xml:"AuditMessage"`
	Event        EventIdentification `xml:"EventIdentification"`
	Participants []ActiveParticipant `xml:"ActiveParticipant"`
	Sources      []AuditSource       `xml:"AuditSourceIdentification"`
	Objects      []ParticipantObject `xml:"ParticipantObjectIdentification"`
}

// EventIdentification identifies the audited event.
type EventIdentification struct {
	ID                 Code   `xml:"EventID"`
	TypeCodes          []Code `xml:"EventTypeCode"`
	OutcomeDescription string `xml:"EventOutcomeDescription,omitempty"`
	ActionCode         string `xml:"EventActionCode,attr,omitempty"`
	DateTime           string `xml:"EventDateTime,attr"`
	OutcomeIndicator   string `xml:"EventOutcomeIndicator,attr"`
}

// ActiveParticipant is a user, process or node taking part in the event.
type ActiveParticipant struct {
	RoleIDCodes                []Code           `xml:"RoleIDCode"`
	MediaIdentifier            *MediaIdentifier `xml:"MediaIdentifier,omitempty"`
	UserID                     string           `xml:"UserID,attr"`
	AlternativeUserID          string           `xml:"AlternativeUserID,attr,omitempty"`
	UserName                   string           `xml:"UserName,attr,omitempty"`
	UserIsRequestor            bool             `xml:"UserIsRequestor,attr"`
	NetworkAccessPointID       string           `xml:"NetworkAccessPointID,attr,omitempty"`
	NetworkAccessPointTypeCode string           `xml:"NetworkAccessPointTypeCode,attr,omitempty"`
}

// MediaIdentifier follows the normative RELAX NG wrapper. The media URI or
// volume identifier is supplied as the participant UserID (A.5.2.1); the
// wrapper carries its media/application type, not an invented XML attribute.
type MediaIdentifier struct {
	Type Code `xml:"MediaType"`
}

// AuditSource identifies the system reporting the event.
type AuditSource struct {
	EnterpriseSiteID string `xml:"AuditEnterpriseSiteID,attr,omitempty"`
	ID               string `xml:"AuditSourceID,attr"`
	TypeCodes        []Code `xml:"AuditSourceTypeCode"`
}

// ParticipantObject is a patient, study, query or other object involved.
type ParticipantObject struct {
	IDTypeCode  Code                         `xml:"ParticipantObjectIDTypeCode"`
	Name        string                       `xml:"ParticipantObjectName,omitempty"`
	Query       string                       `xml:"ParticipantObjectQuery,omitempty"`
	Details     []ParticipantObjectDetail    `xml:"ParticipantObjectDetail"`
	Description ParticipantObjectDescription `xml:"ParticipantObjectDescription"`
	ID          string                       `xml:"ParticipantObjectID,attr"`
	TypeCode    string                       `xml:"ParticipantObjectTypeCode,attr,omitempty"`
	Role        string                       `xml:"ParticipantObjectTypeCodeRole,attr,omitempty"`
	LifeCycle   string                       `xml:"ParticipantObjectDataLifeCycle,attr,omitempty"`
}

// ParticipantObjectDetail is a type/value pair; Value is base64 encoded.
type ParticipantObjectDetail struct {
	Type  string `xml:"type,attr"`
	Value string `xml:"value,attr"`
}

// ParticipantObjectDescription holds the DICOM object description.
type ParticipantObjectDescription struct {
	Accessions    []Accession   `xml:"Accession"`
	SOPClasses    []SOPClass    `xml:"SOPClass"`
	ContainsStudy ContainsStudy `xml:"ParticipantObjectContainsStudy"`
	Encrypted     *bool         `xml:"Encrypted,omitempty"`
	Anonymized    *bool         `xml:"Anonymized,omitempty"`
}

// Accession is an Accession Number.
type Accession struct {
	Number string `xml:"Number,attr"`
}

// SOPClass counts the instances of one SOP Class.
type SOPClass struct {
	Instances         []StudyID `xml:"Instance"`
	UID               string    `xml:"UID,attr"`
	NumberOfInstances int       `xml:"NumberOfInstances,attr"`
}

// ContainsStudy lists studies the object belongs to.
type ContainsStudy struct {
	Studies []StudyID `xml:"StudyIDs"`
}

// StudyID is a Study Instance UID.
type StudyID struct {
	UID string `xml:"UID,attr"`
}

// Marshal validates the message and encodes it as UTF-8 XML with a
// declaration.
func (m Message) Marshal() ([]byte, error) {
	if err := m.validate(); err != nil {
		return nil, err
	}
	var buf bytes.Buffer
	buf.WriteString(xml.Header)
	if err := xml.NewEncoder(&buf).Encode(m); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

func (m Message) validate() error {
	switch {
	case m.Event.ID.Value == "":
		return fmt.Errorf("%w: no EventID", ErrAuditMessage)
	case m.Event.DateTime == "":
		return fmt.Errorf("%w: no EventDateTime", ErrAuditMessage)
	case m.Event.OutcomeIndicator == "":
		return fmt.Errorf("%w: no EventOutcomeIndicator", ErrAuditMessage)
	case len(m.Participants) == 0:
		return fmt.Errorf("%w: no ActiveParticipant", ErrAuditMessage)
	case len(m.Sources) == 0:
		return fmt.Errorf("%w: no AuditSourceIdentification", ErrAuditMessage)
	}
	for _, participant := range m.Participants {
		if participant.UserID == "" {
			return fmt.Errorf("%w: ActiveParticipant without UserID", ErrAuditMessage)
		}
	}
	for _, source := range m.Sources {
		if source.ID == "" {
			return fmt.Errorf("%w: AuditSourceIdentification without AuditSourceID", ErrAuditMessage)
		}
	}
	for _, object := range m.Objects {
		if object.ID == "" || object.IDTypeCode.Value == "" {
			return fmt.Errorf("%w: ParticipantObjectIdentification without ID or ID type", ErrAuditMessage)
		}
		if object.Name != "" && object.Query != "" {
			return fmt.Errorf("%w: ParticipantObjectName and ParticipantObjectQuery are exclusive", ErrAuditMessage)
		}
	}
	return nil
}

// Source describes the reporting system: AuditSourceID (for example the
// host or AE title), an optional enterprise site, and the
// AuditSourceTypeCode ("4", application server, when empty).
type Source struct {
	ID         string
	SiteID     string
	SourceType string
}

func (s Source) element() AuditSource {
	kind := s.SourceType
	if kind == "" {
		kind = "4"
	}
	return AuditSource{EnterpriseSiteID: s.SiteID, ID: s.ID, TypeCodes: []Code{{Value: kind}}}
}

// Participant describes an active participant: a process (AE title as
// alternative user ID), a person or a node.
type Participant struct {
	UserID            string
	AlternativeUserID string
	UserName          string
	Requestor         bool
	// Host is the network access point: an IP address or a host name.
	Host string
}

func (p Participant) element(roles ...Code) ActiveParticipant {
	out := ActiveParticipant{
		RoleIDCodes: roles, UserID: p.UserID, AlternativeUserID: p.AlternativeUserID,
		UserName: p.UserName, UserIsRequestor: p.Requestor, NetworkAccessPointID: p.Host,
	}
	if p.Host != "" {
		out.NetworkAccessPointTypeCode = "1"
		if isIPAddress(p.Host) {
			out.NetworkAccessPointTypeCode = "2"
		}
	}
	return out
}

func isIPAddress(host string) bool {
	host = strings.Trim(host, "[]")
	if strings.Contains(host, ":") {
		return true
	}
	parts := strings.Split(host, ".")
	if len(parts) != 4 {
		return false
	}
	for _, part := range parts {
		if part == "" || strings.Trim(part, "0123456789") != "" {
			return false
		}
	}
	return true
}

// Study describes a study participant object.
type Study struct {
	UID        string
	Accessions []string
	// SOPClasses maps SOP Class UIDs to instance counts.
	SOPClasses []SOPClass
}

func (s Study) object(lifeCycle string) ParticipantObject {
	description := ParticipantObjectDescription{SOPClasses: s.SOPClasses}
	for _, number := range s.Accessions {
		if number = strings.TrimSpace(number); number != "" {
			description.Accessions = append(description.Accessions, Accession{Number: number})
		}
	}
	return ParticipantObject{
		IDTypeCode: idStudyInstanceUID, ID: s.UID, TypeCode: "2", Role: "3", LifeCycle: lifeCycle,
		Description: description,
	}
}

// Patient describes the patient participant object.
type Patient struct {
	ID   string
	Name string
}

func (p Patient) object() ParticipantObject {
	return ParticipantObject{IDTypeCode: idPatientNumber, ID: p.ID, Name: p.Name, TypeCode: "1", Role: "1"}
}

// Common are the parts every builder takes.
type Common struct {
	Time    time.Time
	Outcome string
	// OutcomeDescription is free text; keep it free of patient data.
	OutcomeDescription string
	Source             Source
}

func (c Common) event(id Code, action string, types ...Code) EventIdentification {
	at := c.Time
	if at.IsZero() {
		at = time.Now()
	}
	outcome := c.Outcome
	if outcome == "" {
		outcome = OutcomeSuccess
	}
	return EventIdentification{
		ID: id, TypeCodes: types, OutcomeDescription: c.OutcomeDescription, ActionCode: action,
		DateTime: at.Format(time.RFC3339Nano), OutcomeIndicator: outcome,
	}
}

// ApplicationActivity is the Application Activity message (A.5.3.1): the
// application started (TypeApplicationStart) or stopped
// (TypeApplicationStop), optionally with the user who launched it.
func ApplicationActivity(c Common, kind Code, application Participant, launcher *Participant) Message {
	application.Requestor = false
	m := Message{
		Event:        c.event(EventApplicationActivity, ActionExecute, kind),
		Participants: []ActiveParticipant{application.element(RoleApplication)},
		Sources:      []AuditSource{c.Source.element()},
	}
	if launcher != nil {
		l := *launcher
		l.Requestor = true
		m.Participants = append(m.Participants, l.element(RoleApplicationLauncher))
	}
	return m
}

// Transfer describes the parties and objects of a transfer.
type Transfer struct {
	Common
	Source      Participant
	Destination Participant
	// Requestor is set when a third party asked for the transfer.
	Requestor *Participant
	Studies   []Study
	Patient   Patient
}

func (t Transfer) message(id Code, action, lifeCycle string) Message {
	m := Message{
		Event: t.event(id, action),
		Participants: []ActiveParticipant{
			t.Source.element(RoleSource),
			t.Destination.element(RoleDestination),
		},
		Sources: []AuditSource{t.Common.Source.element()},
	}
	if t.Requestor != nil {
		r := *t.Requestor
		r.Requestor = true
		m.Participants = append(m.Participants, r.element())
	}
	for _, study := range t.Studies {
		m.Objects = append(m.Objects, study.object(lifeCycle))
	}
	if t.Patient.ID != "" {
		m.Objects = append(m.Objects, t.Patient.object())
	}
	return m
}

// BeginTransferring is the Begin Transferring DICOM Instances message
// (A.5.3.3).
func BeginTransferring(t Transfer) Message {
	return t.message(EventBeginTransferring, ActionExecute, "")
}

// InstancesTransferred is the DICOM Instances Transferred message
// (A.5.3.7); action is ActionCreate for new instances, ActionRead for a
// send, ActionUpdate for instances that existed.
func InstancesTransferred(t Transfer, action string) Message {
	return t.message(EventInstancesTransferred, action, "")
}

// Access describes instances accessed locally.
type Access struct {
	Common
	User    Participant
	Studies []Study
	Patient Patient
}

// InstancesAccessed is the DICOM Instances Accessed message (A.5.3.6);
// action is one of C, R, U or D.
func InstancesAccessed(a Access, action string) Message {
	user := a.User
	user.Requestor = true
	m := Message{
		Event:        a.event(EventInstancesAccessed, action),
		Participants: []ActiveParticipant{user.element()},
		Sources:      []AuditSource{a.Common.Source.element()},
	}
	for _, study := range a.Studies {
		m.Objects = append(m.Objects, study.object("6"))
	}
	if a.Patient.ID != "" {
		m.Objects = append(m.Objects, a.Patient.object())
	}
	return m
}

// QueryRequest describes a query.
type QueryRequest struct {
	Common
	Source      Participant
	Destination Participant
	// SOPClassUID is the queried Information Model.
	SOPClassUID string
	// Query is the encoded query data set (base64 in the message).
	Query []byte
	// TransferSyntaxUID is the encoding of Query.
	TransferSyntaxUID string
}

// Query is the Query message (A.5.3.10).
func Query(q QueryRequest) Message {
	source := q.Source
	source.Requestor = true
	object := ParticipantObject{
		IDTypeCode: idSOPClassUID, ID: q.SOPClassUID, TypeCode: "2", Role: "3",
		Query: base64.StdEncoding.EncodeToString(q.Query),
	}
	if q.TransferSyntaxUID != "" {
		object.Details = []ParticipantObjectDetail{{Type: "TransferSyntax", Value: base64.StdEncoding.EncodeToString([]byte(q.TransferSyntaxUID))}}
	}
	return Message{
		Event:        q.event(EventQuery, ActionExecute),
		Participants: []ActiveParticipant{source.element(RoleSource), q.Destination.element(RoleDestination)},
		Sources:      []AuditSource{q.Common.Source.element()},
		Objects:      []ParticipantObject{object},
	}
}

// ExportRequest describes an export to media or a file.
type ExportRequest struct {
	Common
	User Participant
	// Media names the destination (a path, a URI or a media label).
	Media   string
	Studies []Study
	Patient Patient
}

// Export is the Export message (A.5.3.4).
func Export(e ExportRequest) Message {
	user := e.User
	user.Requestor = true
	m := Message{
		Event: e.event(EventExport, ActionRead),
		Participants: []ActiveParticipant{
			user.element(RoleSource),
			{RoleIDCodes: []Code{RoleDestinationMedia}, UserID: e.Media, UserIsRequestor: false},
		},
		Sources: []AuditSource{e.Common.Source.element()},
	}
	for _, study := range e.Studies {
		m.Objects = append(m.Objects, study.object("10"))
	}
	if e.Patient.ID != "" {
		m.Objects = append(m.Objects, e.Patient.object())
	}
	return m
}

// UserAuthentication is the User Authentication message (A.5.3.12): kind
// is TypeLogin or TypeLogout; node is the node the user authenticated to.
func UserAuthentication(c Common, kind Code, user Participant, node *Participant) Message {
	user.Requestor = true
	m := Message{
		Event:        c.event(EventUserAuthentication, ActionExecute, kind),
		Participants: []ActiveParticipant{user.element()},
		Sources:      []AuditSource{c.Source.element()},
	}
	if node != nil {
		n := *node
		n.Requestor = false
		m.Participants = append(m.Participants, n.element())
	}
	return m
}

// SecurityAlert is the Security Alert message (A.5.3.11): kind is the
// alert type (for example TypeNodeAuthentication); reporter is the process
// reporting it; subject, when set, names the node or object alerted on.
func SecurityAlert(c Common, kind Code, reporter Participant, subject string) Message {
	reporter.Requestor = false
	m := Message{
		Event:        c.event(EventSecurityAlert, ActionExecute, kind),
		Participants: []ActiveParticipant{reporter.element()},
		Sources:      []AuditSource{c.Source.element()},
	}
	if subject != "" {
		m.Objects = []ParticipantObject{{IDTypeCode: idNodeID, ID: subject, TypeCode: "2", Role: "13"}}
	}
	return m
}
