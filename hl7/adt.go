package hl7

import (
	"errors"
	"strings"

	"github.com/ThalesMMS/dicom-go/object"
)

// PatientIdentifier is one CX value, PID-3 or MRG-1: the identifier with its
// assigning authority (CX-4) as DICOM holds it, and the identifier type code
// (CX-5, HL7 table 0203, such as MR or PI).
type PatientIdentifier struct {
	object.IDWithIssuer
	IdentifierTypeCode string
}

func patientIdentifierFrom(r Repetition) PatientIdentifier {
	authority := hdFromComponent(r.Component(4))
	return PatientIdentifier{
		IDWithIssuer: object.IDWithIssuer{
			ID: strings.TrimSpace(r.String(1)),
			Issuer: object.Issuer{
				LocalNamespaceEntityID: strings.TrimSpace(authority.NamespaceID),
				UniversalEntityID:      strings.TrimSpace(authority.UniversalID),
				UniversalEntityIDType:  strings.TrimSpace(authority.UniversalIDType),
			},
		},
		IdentifierTypeCode: strings.TrimSpace(r.String(5)),
	}
}

// repetition encodes the identifier as CX: ID^^^authority^type.
func (p PatientIdentifier) repetition() Repetition {
	authority := HierarchicDesignator{NamespaceID: p.Issuer.LocalNamespaceEntityID, UniversalID: p.Issuer.UniversalEntityID, UniversalIDType: p.Issuer.UniversalEntityIDType}
	return Repetition{{p.ID}, nil, nil, authority.component(), {p.IdentifierTypeCode}}
}

func identifiersFrom(field Field) []PatientIdentifier {
	var out []PatientIdentifier
	for _, repetition := range field {
		if id := patientIdentifierFrom(repetition); id.ID != "" && id.ID != Null {
			out = append(out, id)
		}
	}
	return out
}

// Patient is the demographic core of a PID segment.
type Patient struct {
	// Identifiers is PID-3, every repetition with an ID.
	Identifiers []PatientIdentifier
	// Name is PID-5's first repetition in DICOM PN order:
	// Family^Given^Middle^Prefix^Suffix.
	Name string
	// NameHasReservedCharacters reports a decoded HL7 name component that
	// cannot be represented in DICOM PN without changing its meaning.
	NameHasReservedCharacters bool
	// BirthDate is PID-7's date, YYYYMMDD or shorter.
	BirthDate string
	// Sex is PID-8 (HL7 table 0001).
	Sex string
	// NameIsNull, BirthDateIsNull and SexIsNull report the HL7 null "",
	// a request to delete the value.
	NameIsNull, BirthDateIsNull, SexIsNull bool
}

func patientFrom(pid *Segment) Patient {
	name := pid.Field(5).First()
	pn, reserved := dicomPersonName(name, false)
	birth := pid.Field(7).String()
	if len(birth) > 8 {
		birth = birth[:8]
	}
	return Patient{
		Identifiers:               identifiersFrom(pid.Field(3)),
		Name:                      strings.Join(pn, "^"),
		NameHasReservedCharacters: reserved,
		BirthDate:                 birth,
		Sex:                       pid.Field(8).String(),
		NameIsNull:                pid.Field(5).IsNull(),
		BirthDateIsNull:           pid.Field(7).IsNull(),
		SexIsNull:                 pid.Field(8).IsNull(),
	}
}

// DICOMPatientIDs returns the identifiers as DICOM Patient IDs with issuer.
func (p Patient) DICOMPatientIDs() []object.IDWithIssuer {
	out := make([]object.IDWithIssuer, len(p.Identifiers))
	for i, id := range p.Identifiers {
		out[i] = id.IDWithIssuer
	}
	return out
}

// DICOMSex maps Sex to Patient's Sex (0010,0040): M, F, O, or "" when
// unknown.
func (p Patient) DICOMSex() string {
	switch strings.ToUpper(strings.TrimSpace(p.Sex)) {
	case "M":
		return "M"
	case "F":
		return "F"
	case "O", "A", "N", "X":
		return "O"
	default:
		return ""
	}
}

// PatientMerge is one ADT^A40 merge: the surviving patient (PID) and the
// identifiers it absorbs (MRG-1).
type PatientMerge struct {
	Patient Patient
	Prior   []PatientIdentifier
}

// ADT events with typed support.
const (
	EventAdmitPatient            = "A01" // admit/visit notification
	EventRegisterPatient         = "A04" // register an outpatient
	EventAddPerson               = "A28" // add person information
	EventUpdatePerson            = "A31" // update person information
	EventChangePatientIdentifier = "A47" // correct patient identifier list
	EventUpdatePatient           = "A08" // update patient information
	EventMergePatient            = "A40" // merge patient - patient identifier list
)

// ADTMessage is a typed patient admission, registration, demographic update,
// identifier correction, or merge. It describes an operation; it does not
// apply one to an application catalog.
type ADTMessage struct {
	Header MessageHeader
	// Event is one of the supported ADT trigger event constants.
	Event string
	// RecordedDateTime is EVN-2.
	RecordedDateTime string
	// Patient is the PID for A01, A04, A08, A28 and A31.
	Patient Patient
	// Merges are the A40 PID and MRG groups, in order.
	Merges []PatientMerge
	// Visit is the PV1 for encounter events A01, A04 and A08, when present.
	// Person events A28/A31 have no visit, including a pseudo PV1 class N.
	Visit *Visit
	// Change is an A47 identifier correction. It is not a patient merge or
	// demographic update, so Patient, Visit and Merges are not populated.
	Change *PatientIdentifierChange
}

// ParseADT types ADT A01, A04, A08, A28, A31, A40 and A47. Other messages and events
// are an *Error with Reject (codes 200 and 201); a missing PID or MRG, or
// one without an identifier, an *Error with codes 100 or 101 and the
// location, to answer with ACKFor.
func ParseADT(m *Message) (*ADTMessage, error) {
	header := m.Header()
	if header.Type.Code != "ADT" {
		return nil, &Error{Code: ErrorCodeUnsupportedMessageType, Reject: true, Location: ErrorLocation{Segment: "MSH", Sequence: 1, Field: 9}, Err: errors.New("not an ADT message")}
	}
	event := header.Type.Trigger
	if evn, ok := m.Segment("EVN"); ok && event == "" {
		event = evn.Field(1).String()
	}
	adt := &ADTMessage{Header: header, Event: event}
	if evn, ok := m.Segment("EVN"); ok {
		adt.RecordedDateTime = evn.Field(2).String()
	}
	switch event {
	case EventAdmitPatient, EventRegisterPatient, EventUpdatePatient, EventAddPerson, EventUpdatePerson:
		pid, ok := m.Segment("PID")
		if !ok {
			return nil, &Error{Code: ErrorCodeSegmentSequence, Location: ErrorLocation{Segment: "PID", Sequence: 1}, Err: errors.New("missing PID segment")}
		}
		if event != EventUpdatePatient {
			counts := map[string]int{}
			for _, segment := range m.Segments {
				if segment.Name == "PID" || segment.Name == "PV1" {
					counts[segment.Name]++
					if segment.Name == "PV1" && counts["PID"] == 0 {
						return nil, adtError("PV1", 0, ErrorCodeSegmentSequence, "PV1 before its PID")
					}
					if counts[segment.Name] > 1 {
						return nil, &Error{Code: ErrorCodeSegmentSequence, Location: ErrorLocation{Segment: segment.Name, Sequence: counts[segment.Name]}, Err: errors.New("duplicate patient or visit segment")}
					}
				}
			}
		}
		adt.Patient = patientFrom(pid)
		if len(adt.Patient.Identifiers) == 0 {
			return nil, &Error{Code: ErrorCodeRequiredFieldMissing, Location: ErrorLocation{Segment: "PID", Sequence: 1, Field: 3}, Err: errors.New("missing patient identifier")}
		}
		// A01/A04 convey an encounter; A08 may update one. A28/A31 convey
		// person information, and their pseudo PV1 must not create a visit.
		if event == EventAdmitPatient || event == EventRegisterPatient || event == EventUpdatePatient {
			pv1, ok := m.Segment("PV1")
			if !ok && event != EventUpdatePatient {
				return nil, adtError("PV1", 0, ErrorCodeSegmentSequence, "missing PV1 segment")
			}
			if ok {
				if (strings.TrimSpace(pv1.Field(2).String()) == "" || pv1.Field(2).IsNull()) && event != EventUpdatePatient {
					return nil, adtError("PV1", 2, ErrorCodeRequiredFieldMissing, "missing patient class")
				}
				if !pv1.Field(2).IsEmpty() && !pv1.Field(2).IsNull() && strings.TrimSpace(pv1.Field(2).String()) != "N" {
					adt.Visit = visitFrom(pv1)
				}
			}
		}
	case EventChangePatientIdentifier:
		var pid, mrg *Segment
		pidCount, mrgCount := 0, 0
		for i := range m.Segments {
			segment := &m.Segments[i]
			switch segment.Name {
			case "PID":
				pidCount++
				if pid != nil || mrg != nil {
					err := adtError("PID", 0, ErrorCodeSegmentSequence, "duplicate or misplaced PID segment")
					err.Location.Sequence = pidCount
					return nil, err
				}
				pid = segment
			case "MRG":
				mrgCount++
				if pid == nil || mrg != nil {
					err := adtError("MRG", 0, ErrorCodeSegmentSequence, "MRG without its PID")
					err.Location.Sequence = mrgCount
					return nil, err
				}
				mrg = segment
			}
		}
		if pid == nil {
			return nil, adtError("PID", 0, ErrorCodeSegmentSequence, "missing PID segment")
		}
		if mrg == nil {
			return nil, adtError("MRG", 0, ErrorCodeSegmentSequence, "missing MRG segment")
		}
		change := &PatientIdentifierChange{Current: identifiersFrom(pid.Field(3)), Prior: identifiersFrom(mrg.Field(1))}
		if len(change.Current) == 0 {
			return nil, adtError("PID", 3, ErrorCodeRequiredFieldMissing, "missing patient identifier")
		}
		if len(change.Prior) == 0 {
			return nil, adtError("MRG", 1, ErrorCodeRequiredFieldMissing, "missing prior patient identifier")
		}
		adt.Change = change
	case EventMergePatient:
		pidCount, mrgCount := 0, 0
		var current *PatientMerge
		for i := range m.Segments {
			segment := &m.Segments[i]
			switch segment.Name {
			case "PID":
				pidCount++
				patient := patientFrom(segment)
				if len(patient.Identifiers) == 0 {
					return nil, &Error{Code: ErrorCodeRequiredFieldMissing, Location: ErrorLocation{Segment: "PID", Sequence: pidCount, Field: 3}, Err: errors.New("missing patient identifier")}
				}
				adt.Merges = append(adt.Merges, PatientMerge{Patient: patient})
				current = &adt.Merges[len(adt.Merges)-1]
			case "MRG":
				mrgCount++
				if current == nil || current.Prior != nil {
					return nil, &Error{Code: ErrorCodeSegmentSequence, Location: ErrorLocation{Segment: "MRG", Sequence: mrgCount}, Err: errors.New("MRG without its PID")}
				}
				current.Prior = identifiersFrom(segment.Field(1))
				if len(current.Prior) == 0 {
					return nil, &Error{Code: ErrorCodeRequiredFieldMissing, Location: ErrorLocation{Segment: "MRG", Sequence: mrgCount, Field: 1}, Err: errors.New("missing prior patient identifier")}
				}
			}
		}
		if len(adt.Merges) == 0 {
			return nil, &Error{Code: ErrorCodeSegmentSequence, Location: ErrorLocation{Segment: "PID", Sequence: 1}, Err: errors.New("missing PID segment")}
		}
		for i, merge := range adt.Merges {
			if merge.Prior == nil {
				return nil, &Error{Code: ErrorCodeSegmentSequence, Location: ErrorLocation{Segment: "MRG", Sequence: i + 1}, Err: errors.New("missing MRG segment")}
			}
		}
	default:
		return nil, &Error{Code: ErrorCodeUnsupportedEventCode, Reject: true, Location: ErrorLocation{Segment: "MSH", Sequence: 1, Field: 9, Component: 2}, Err: errors.New("unsupported ADT event")}
	}
	return adt, nil
}
