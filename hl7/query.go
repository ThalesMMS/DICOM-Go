package hl7

import (
	"errors"
	"strconv"
	"strings"
	"time"
)

// IHE query names (QPD-1).
const (
	PDQQueryName = "IHE PDQ Query"
	PIXQueryName = "IHE PIX Query"
)

// QueryParameter is one PDQ demographic field (QIP): an HL7 field path such
// as PID.5.1.1 (family name), PID.3.1 or PID.7, and its value.
type QueryParameter struct {
	Field string
	Value string
}

// QueryOptions complete a PDQ or PIX query.
type QueryOptions struct {
	// Header supplies applications, facilities and the processing ID; the
	// message type, version (2.5 by default) and control ID are set here
	// when empty.
	Header MessageHeader
	// Tag is QPD-2, echoed in QAK-1; default the control ID.
	Tag string
	// Limit is RCP-2 (records per response), zero for none.
	Limit int
	// Continuation is DSC-1 from a previous response, to fetch the next
	// increment (ITI-21 continuation).
	Continuation string
}

func newQuery(trigger, name string, options QueryOptions) (*Message, *Segment, QueryOptions) {
	header := options.Header
	header.Type = MessageType{Code: "QBP", Trigger: trigger, Structure: "QBP_Q21"}
	if header.ControlID == "" {
		header.ControlID = newControlID()
	}
	if header.Version == "" {
		header.Version = "2.5"
	}
	if header.ProcessingID == "" {
		header.ProcessingID = "P"
	}
	if header.DateTime == "" {
		header.DateTime = time.Now().Format("20060102150405")
	}
	if options.Tag == "" {
		options.Tag = header.ControlID
	}
	message := NewMessage(header)
	qpd := message.Add("QPD")
	qpd.SetValue(1, name)
	qpd.SetValue(2, options.Tag)
	return message, qpd, options
}

func addResponseControl(message *Message, options QueryOptions) {
	rcp := message.Add("RCP")
	rcp.SetValue(1, "I")
	if options.Limit > 0 {
		rcp.SetField(2, Components(strconv.Itoa(options.Limit), "RD"))
	}
	if options.Continuation != "" {
		dsc := message.Add("DSC")
		dsc.SetValue(1, options.Continuation)
		dsc.SetValue(2, "I")
	}
}

// NewPDQQuery builds an IHE ITI-21 Patient Demographics Query, QBP^Q22,
// whose QPD-3 lists the parameters as @field^value.
func NewPDQQuery(parameters []QueryParameter, options QueryOptions) *Message {
	message, qpd, options := newQuery("Q22", PDQQueryName, options)
	field := make(Field, 0, len(parameters))
	for _, p := range parameters {
		name := strings.TrimSpace(p.Field)
		if !strings.HasPrefix(name, "@") {
			name = "@" + name
		}
		field = append(field, Repetition{{name}, {p.Value}})
	}
	qpd.SetField(3, field)
	addResponseControl(message, options)
	return message
}

// NewPIXQuery builds an IHE ITI-9 PIX Query, QBP^Q23: the identifier to
// cross-reference (QPD-3) and, optionally, the assigning authorities whose
// identifiers to return (QPD-4); none returns every known domain.
func NewPIXQuery(identifier PatientIdentifier, domains []HierarchicDesignator, options QueryOptions) *Message {
	message, qpd, options := newQuery("Q23", PIXQueryName, options)
	qpd.SetField(3, Field{identifier.repetition()})
	if len(domains) > 0 {
		field := make(Field, 0, len(domains))
		for _, domain := range domains {
			field = append(field, Repetition{nil, nil, nil, domain.component()})
		}
		qpd.SetField(4, field)
	}
	addResponseControl(message, options)
	return message
}

// QueryResponse is a parsed RSP^K22 (PDQ) or RSP^K23 (PIX).
type QueryResponse struct {
	Acknowledgment Acknowledgment
	// Tag is QAK-1, the query's QPD-2.
	Tag string
	// Status is QAK-2: OK (data found), NF (no data), AE or AR.
	Status string
	// Patients are the response PIDs: the matching patients for PDQ, the
	// one cross-referenced patient (its PID-3 identifiers) for PIX.
	Patients []Patient
	// Continuation is DSC-1 when more PDQ results remain.
	Continuation string
}

// ParseQueryResponse reads a PDQ or PIX response.
func ParseQueryResponse(m *Message) (*QueryResponse, error) {
	header := m.Header()
	if header.Type.Code != "RSP" {
		return nil, &Error{Code: ErrorCodeUnsupportedMessageType, Reject: true, Location: ErrorLocation{Segment: "MSH", Sequence: 1, Field: 9}, Err: errors.New("not a query response")}
	}
	ack, err := ParseAcknowledgment(m)
	if err != nil {
		return nil, err
	}
	response := &QueryResponse{Acknowledgment: ack}
	if qak, ok := m.Segment("QAK"); ok {
		response.Tag = qak.Field(1).String()
		response.Status = qak.Field(2).String()
	}
	for _, pid := range m.All("PID") {
		response.Patients = append(response.Patients, patientFrom(pid))
	}
	if dsc, ok := m.Segment("DSC"); ok {
		response.Continuation = dsc.Field(1).String()
	}
	return response, nil
}
