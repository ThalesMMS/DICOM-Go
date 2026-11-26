package clinicalresult

import (
	"crypto/sha256"
	"fmt"
	"time"

	"github.com/ThalesMMS/dicom-go/hl7"
)

// HL7IdentifierRole identifies the domain of a v2 identifier in a crosswalk.
type HL7IdentifierRole string

const (
	HL7PatientIdentifier HL7IdentifierRole = "patient"
	HL7PlacerOrder       HL7IdentifierRole = "placer"
	HL7FillerOrder       HL7IdentifierRole = "filler"
)

// HL7AuthorityResolver maps a complete decoded assigning authority and
// identifier domain to a canonical identifier system. Configure the crosswalk
// for the verified sending connection and tenant. identifierType is CX.5 for
// patients and empty for order numbers. A declined patient namespace is not
// evidence of identity. A declined order namespace makes that link unusable.
type HL7AuthorityResolver func(authority hl7.HierarchicDesignator, role HL7IdentifierRole, identifierType string) (system string, ok bool)

const maxHL7WireBytes = 1 << 20

// OrderFromHL7 projects a received v2.5.1 ORM^O01 new order with one patient
// and one ORC/OBR group. orderID is the caller's stable order identifier.
// OBR-18 through OBR-21 are site-local fields outside this profile.
func OrderFromHL7(wire []byte, orderID Identifier, mapAuthority HL7AuthorityResolver) (Order, error) {
	if mapAuthority == nil || !identifierValid(orderID) {
		return Order{}, fmt.Errorf("%w: HL7 order identity or authority crosswalk", ErrInvalidInput)
	}
	group, err := parseHL7IdentityGroup(wire, hl7.MessageType{Code: "ORM", Trigger: "O01", Structure: "ORM_O01"})
	if err != nil {
		return Order{}, err
	}
	if group.orc == nil || !hl7SimpleField(group.orc.Field(1), "NW") {
		return Order{}, hl7IdentityError("ORC", 1, "expected a new order")
	}
	patient, references, err := group.identities(mapAuthority, false)
	if err != nil {
		return Order{}, err
	}
	order := Order{ID: orderID, Patient: patient, References: references}
	if !orderValid(order) {
		return Order{}, fmt.Errorf("%w: HL7 order projection", ErrInvalidInput)
	}
	return order, nil
}

// ResultFromHL7 projects the identity and report status of a v2.5.1 ORU^R01
// for one patient and one OBR. The caller binds the authenticated source, the
// logical report ID and its verified increasing revision sequence. MSH-10
// identifies the delivery and is never interpreted as a sequence. The SHA-256
// digest covers the complete original ER7. The caller must establish that
// the received record represents a complete report revision before replacing
// report content. This projection does not interpret individual observations.
func ResultFromHL7(wire []byte, source string, resultID Identifier, sequence uint64, mapAuthority HL7AuthorityResolver) (Result, error) {
	if mapAuthority == nil || !identifierValid(resultID) || source == "" || sequence == 0 {
		return Result{}, fmt.Errorf("%w: HL7 result identity, revision sequence or authority crosswalk", ErrInvalidInput)
	}
	group, err := parseHL7IdentityGroup(wire, hl7.MessageType{Code: "ORU", Trigger: "R01", Structure: "ORU_R01"})
	if err != nil {
		return Result{}, err
	}
	if group.orc != nil && !hl7SimpleField(group.orc.Field(1), "RE") {
		return Result{}, hl7IdentityError("ORC", 1, "unsupported result order control")
	}
	field := group.obr.Field(25)
	var status Status
	switch {
	case hl7SimpleField(field, "F"):
		status = StatusFinal
	case hl7SimpleField(field, "C"):
		status = StatusCorrected
	case hl7SimpleField(field, "X"):
		status = StatusWithdrawn
	default:
		return Result{}, hl7IdentityError("OBR", 25, "unsupported result status")
	}
	patient, references, err := group.identities(mapAuthority, true)
	if err != nil {
		return Result{}, err
	}
	result := Result{
		Source: source, ID: resultID, Patient: patient, OrderReferences: references,
		Sequence: sequence, RevisionID: group.controlID, Status: status, Digest: sha256.Sum256(wire),
	}
	if !resultValid(result) {
		return Result{}, fmt.Errorf("%w: HL7 result projection", ErrInvalidInput)
	}
	return result, nil
}

type hl7IdentityGroup struct {
	pid       *hl7.Segment
	orc       *hl7.Segment
	obr       *hl7.Segment
	controlID string
}

func parseHL7IdentityGroup(wire []byte, expected hl7.MessageType) (hl7IdentityGroup, error) {
	message, err := hl7.ParseWithOptions(wire, hl7.ParseOptions{
		MaxBytes: maxHL7WireBytes, MaxSegments: 4096, MaxFields: 256, MaxRepetitions: 128,
	})
	if err != nil {
		return hl7IdentityGroup{}, fmt.Errorf("%w: invalid or oversized HL7 message", ErrInvalidInput)
	}
	msh := &message.Segments[0]
	kind := msh.Field(9)
	if len(kind) != 1 || !hl7ComponentsBounded(kind[0], 3) ||
		kind[0].String(1) != expected.Code || kind[0].String(2) != expected.Trigger ||
		(kind[0].String(3) != "" && kind[0].String(3) != expected.Structure) {
		return hl7IdentityGroup{}, hl7IdentityError("MSH", 9, "unsupported message type or structure")
	}
	version := msh.Field(12)
	if len(version) != 1 || len(version[0]) > 3 || len(version[0].Component(1)) != 1 ||
		version[0].String(1) != "2.5.1" {
		return hl7IdentityGroup{}, hl7IdentityError("MSH", 12, "expected v2.5.1")
	}
	controlID := msh.Field(10)
	if !hl7Scalar(controlID) || controlID.String() == "" || controlID.IsNull() {
		return hl7IdentityGroup{}, hl7IdentityError("MSH", 10, "expected one control identifier")
	}
	if !msh.Field(14).IsEmpty() {
		return hl7IdentityGroup{}, hl7IdentityError("MSH", 14, "fragmented message")
	}
	group := hl7IdentityGroup{controlID: controlID.String()}
	for i := 1; i < len(message.Segments); i++ {
		segment := &message.Segments[i]
		switch segment.Name {
		case "SFT":
			if group.pid == nil {
				continue
			}
		case "PID":
			if group.pid == nil {
				group.pid = segment
				continue
			}
		case "PD1", "NK1", "PV1", "PV2":
			if group.pid != nil && group.orc == nil && group.obr == nil {
				continue
			}
		case "ORC":
			if group.pid != nil && group.orc == nil && group.obr == nil {
				group.orc = segment
				continue
			}
		case "OBR":
			if group.pid != nil && group.obr == nil && (expected.Code != "ORM" || group.orc != nil) {
				group.obr = segment
				continue
			}
		case "OBX", "TQ1", "TQ2", "CTD", "CTI":
			if group.obr != nil {
				continue
			}
		case "NTE":
			continue
		}
		return hl7IdentityGroup{}, fmt.Errorf("%w: unsupported, repeated or misplaced HL7 segment at position %d", ErrInvalidInput, i+1)
	}
	if group.pid == nil || group.obr == nil {
		return hl7IdentityGroup{}, fmt.Errorf("%w: expected one HL7 patient and observation request group", ErrInvalidInput)
	}
	return group, nil
}

func (g hl7IdentityGroup) identities(mapAuthority HL7AuthorityResolver, result bool) (Patient, []OrderReference, error) {
	patient, err := hl7Patient(g.pid, mapAuthority)
	if err != nil {
		return Patient{}, nil, err
	}
	selectors := []struct {
		field int
		kind  ReferenceKind
		role  HL7IdentifierRole
	}{
		{2, RefPlacer, HL7PlacerOrder},
		{3, RefFiller, HL7FillerOrder},
	}
	references := make([]OrderReference, 0, len(selectors))
	for _, selector := range selectors {
		var commonField hl7.Field
		if g.orc != nil {
			commonField = g.orc.Field(selector.field)
		}
		common, err := readHL7OrderID(commonField, "ORC", selector.field)
		if err != nil {
			return Patient{}, nil, err
		}
		detail, err := readHL7OrderID(g.obr.Field(selector.field), "OBR", selector.field)
		if err != nil {
			return Patient{}, nil, err
		}
		if common.value != "" && detail.value != "" && common != detail {
			return Patient{}, nil, hl7IdentityError("OBR", selector.field, "order identifier disagrees with ORC")
		}
		if detail.value == "" {
			if result && common.value != "" {
				return Patient{}, nil, hl7IdentityError("OBR", selector.field, "result is missing its order identifier")
			}
			detail = common
		}
		if detail.value == "" {
			continue
		}
		system, ok := mapAuthority(detail.authority, selector.role, "")
		id := Identifier{System: system, Value: detail.value}
		if !ok || !identifierValid(id) {
			return Patient{}, nil, hl7IdentityError("OBR", selector.field, "order authority is not in the crosswalk")
		}
		references = append(references, OrderReference{Kind: selector.kind, ID: id})
	}
	if len(references) == 0 {
		return Patient{}, nil, hl7IdentityError("OBR", 2, "no assigned order identifier")
	}
	return patient, references, nil
}

func hl7Patient(pid *hl7.Segment, mapAuthority HL7AuthorityResolver) (Patient, error) {
	var patient Patient
	for _, repetition := range pid.Field(3) {
		if len(repetition) > 10 || len(repetition.Component(1)) != 1 ||
			len(repetition.Component(4)) > 3 || len(repetition.Component(5)) > 1 {
			return Patient{}, hl7IdentityError("PID", 3, "invalid patient identifier shape")
		}
		hd := repetition.Component(4)
		authority := hl7.HierarchicDesignator{NamespaceID: hd.Sub(1), UniversalID: hd.Sub(2), UniversalIDType: hd.Sub(3)}
		value := repetition.String(1)
		if !hl7AuthorityPresent(authority) || value == "" || value == hl7.Null {
			return Patient{}, hl7IdentityError("PID", 3, "missing patient identity or assigning authority")
		}
		system, ok := mapAuthority(authority, HL7PatientIdentifier, repetition.String(5))
		if !ok {
			continue
		}
		// Values outside CX.1, CX.4 and CX.5 can qualify an identifier's
		// validity or scope. This crosswalk cannot interpret them.
		qualifiers := hl7.Field{{repetition.Component(2), repetition.Component(3)}}
		if len(repetition) > 5 {
			qualifiers = append(qualifiers, repetition[5:])
		}
		if !qualifiers.IsEmpty() {
			return Patient{}, hl7IdentityError("PID", 3, "unsupported patient identifier qualifiers")
		}
		id := Identifier{System: system, Value: value}
		if !identifierValid(id) || (patient.ID != (Identifier{}) && patient.ID != id) {
			return Patient{}, hl7IdentityError("PID", 3, "ambiguous or invalid mapped patient identity")
		}
		patient.ID = id
	}
	if patient.ID == (Identifier{}) {
		return Patient{}, hl7IdentityError("PID", 3, "no accepted patient authority")
	}
	birthDate, err := hl7PatientBirthDate(pid.Field(7))
	if err != nil {
		return Patient{}, err
	}
	patient.BirthDate = birthDate
	field := pid.Field(8)
	switch {
	case field.IsEmpty() || hl7SimpleField(field, "A") || hl7SimpleField(field, "N") || hl7SimpleField(field, "U"):
	case hl7SimpleField(field, "M"):
		patient.Sex = "male"
	case hl7SimpleField(field, "F"):
		patient.Sex = "female"
	case hl7SimpleField(field, "O"):
		patient.Sex = "other"
	default:
		return Patient{}, hl7IdentityError("PID", 8, "unsupported administrative sex")
	}
	return patient, nil
}

func hl7PatientBirthDate(field hl7.Field) (string, error) {
	if field.IsEmpty() {
		return "", nil
	}
	if !hl7Scalar(field) {
		return "", hl7IdentityError("PID", 7, "expected one date")
	}
	date := field.String()
	var format, canonical string
	switch len(date) {
	case 4:
		format, canonical = "2006", "2006"
	case 6:
		format, canonical = "200601", "2006-01"
	case 8:
		format, canonical = "20060102", "2006-01-02"
	default:
		return "", hl7IdentityError("PID", 7, "unsupported birth date precision")
	}
	value, err := time.Parse(format, date)
	if err != nil || value.Year() == 0 {
		return "", hl7IdentityError("PID", 7, "invalid birth date")
	}
	return value.Format(canonical), nil
}

type hl7OrderID struct {
	value     string
	authority hl7.HierarchicDesignator
}

func readHL7OrderID(field hl7.Field, segment string, number int) (hl7OrderID, error) {
	if field.IsEmpty() {
		return hl7OrderID{}, nil
	}
	if len(field) != 1 || !hl7ComponentsBounded(field[0], 4) {
		return hl7OrderID{}, hl7IdentityError(segment, number, "invalid order identifier shape")
	}
	id := field[0]
	authority := hl7.HierarchicDesignator{NamespaceID: id.String(2), UniversalID: id.String(3), UniversalIDType: id.String(4)}
	if id.String(1) == "" || id.String(1) == hl7.Null || !hl7AuthorityPresent(authority) {
		return hl7OrderID{}, hl7IdentityError(segment, number, "missing order identifier or assigning authority")
	}
	return hl7OrderID{value: id.String(1), authority: authority}, nil
}

func hl7AuthorityPresent(authority hl7.HierarchicDesignator) bool {
	return (authority.NamespaceID != "" || authority.UniversalID != "") &&
		(authority.UniversalID == "") == (authority.UniversalIDType == "") &&
		authority.NamespaceID != hl7.Null && authority.UniversalID != hl7.Null && authority.UniversalIDType != hl7.Null
}

func hl7Scalar(field hl7.Field) bool {
	return len(field) == 1 && len(field[0]) == 1 && len(field[0][0]) == 1
}

func hl7SimpleField(field hl7.Field, want string) bool {
	return hl7Scalar(field) && field.String() == want
}

func hl7ComponentsBounded(repetition hl7.Repetition, limit int) bool {
	if len(repetition) > limit {
		return false
	}
	for _, component := range repetition {
		if len(component) > 1 {
			return false
		}
	}
	return true
}

func hl7IdentityError(segment string, field int, reason string) error {
	return fmt.Errorf("%w: HL7 %s-%d: %s", ErrInvalidInput, segment, field, reason)
}
