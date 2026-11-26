package hl7

import (
	"bytes"
	"errors"
	"reflect"
	"strings"
	"sync"
	"testing"

	"github.com/ThalesMMS/dicom-go/core"
	"github.com/ThalesMMS/dicom-go/object"
	"github.com/ThalesMMS/dicom-go/transfer"
)

func adtFixture(event string) *Message {
	structure := "ADT_A01"
	if event == EventAddPerson || event == EventUpdatePerson {
		structure = "ADT_A05"
	}
	if event == EventChangePatientIdentifier {
		structure = "ADT_A30"
	}
	m := NewMessage(MessageHeader{SendingApplication: HierarchicDesignator{NamespaceID: "SYNTHETIC"}, Type: MessageType{Code: "ADT", Trigger: event, Structure: structure}, ControlID: "FIXTURE", Version: "2.5.1", ProcessingID: "P", DateTime: "20261009120000", CharacterSet: "UNICODE UTF-8"})
	m.Add("EVN").SetValue(2, "20261009115900")
	pid := m.Add("PID")
	pid.SetField(3, Field{{{"MR-ONE"}, nil, nil, {"AUTH", "1.2.3", "ISO"}, {"MR"}}, {{"SECOND"}, nil, nil, {"OTHER"}, {"PI"}}})
	pid.SetField(5, Components("SYNTHETIC", "PATIENT", "Q", "JR", "DR"))
	pid.SetValue(7, "19700101123000-0300")
	pid.SetValue(8, "F")
	if event == EventChangePatientIdentifier {
		m.Add("MRG").SetField(1, Field{{{"OLD"}, nil, nil, {"AUTH", "1.2.3", "ISO"}, {"MR"}}, {{"OLD-ALT"}, nil, nil, {"OTHER"}, {"PI"}}})
		return m
	}
	pv1 := m.Add("PV1")
	if event == EventAddPerson || event == EventUpdatePerson {
		pv1.SetValue(2, "N")
		return m
	}
	pv1.SetValue(2, "I")
	pv1.SetField(3, Field{{{"WARD"}, {"ROOM"}, {"BED"}, {"FAC", "1.2.5", "ISO"}}})
	pv1.SetValue(4, "E")
	pv1.SetField(8, Components("DR-ID", "REFERRING", "DOCTOR", "Q", "JR", "DR"))
	pv1.SetValue(10, "RAD")
	pv1.SetField(19, Field{{{"VISIT-ONE"}, nil, nil, {"VISIT-AUTH", "1.2.4", "ISO"}, {"VN"}}})
	pv1.SetValue(44, "20261009113015.012-0300")
	pv1.SetValue(45, "202610101000")
	return m
}

func parseADTFixture(t *testing.T, m *Message) *ADTMessage {
	t.Helper()
	wire := m.String()
	parsed, err := Parse([]byte(wire))
	if err != nil {
		t.Fatal(err)
	}
	adt, err := ParseADT(parsed)
	if err != nil {
		t.Fatal(err)
	}
	return adt
}

func TestExpandedADTEvents(t *testing.T) {
	for _, event := range []string{EventAdmitPatient, EventRegisterPatient, EventUpdatePatient, EventAddPerson, EventUpdatePerson, EventChangePatientIdentifier} {
		t.Run(event, func(t *testing.T) {
			adt := parseADTFixture(t, adtFixture(event))
			if adt.Event != event || adt.RecordedDateTime != "20261009115900" || len(adt.Merges) != 0 {
				t.Fatalf("wrong event shape: %+v", adt)
			}
			if event == EventChangePatientIdentifier {
				if adt.Change == nil || len(adt.Change.Current) != 2 || len(adt.Change.Prior) != 2 || adt.Change.Prior[0].ID != "OLD" || !reflect.DeepEqual(adt.Patient, Patient{}) || adt.Visit != nil {
					t.Fatalf("not a distinct correction: %+v", adt)
				}
				mapped, err := adt.Change.DICOMCurrentAttributes(0)
				if err != nil {
					t.Fatal(err)
				}
				if ids := object.AllPatientIDsWithIssuer(mapped); len(ids) != 2 || ids[0].ID != "MR-ONE" || ids[1].ID != "SECOND" {
					t.Fatalf("correction lost identifiers: %+v", ids)
				}
				return
			}
			if adt.Change != nil || adt.Patient.Name != "SYNTHETIC^PATIENT^Q^DR^JR" || adt.Patient.BirthDate != "19700101" || len(adt.Patient.Identifiers) != 2 {
				t.Fatalf("patient: %+v", adt)
			}
			if event == EventAddPerson || event == EventUpdatePerson {
				if adt.Visit != nil {
					t.Fatal("pseudo PV1 created an encounter")
				}
				return
			}
			v := adt.Visit
			if v == nil || v.PatientClass != "I" || v.Location != (PatientLocation{"WARD", "ROOM", "BED", HierarchicDesignator{"FAC", "1.2.5", "ISO"}}) || v.Number.ID != "VISIT-ONE" || v.ReferringPhysician != "REFERRING^DOCTOR^Q^DR^JR" || v.HospitalService != "RAD" || v.AdmittedDateTime != "20261009113015.012-0300" || v.DischargedDateTime != "202610101000" {
				t.Fatalf("visit: %+v", v)
			}
		})
	}
}

func TestADTAttributesPatientAndVisitRoundTrip(t *testing.T) {
	m := adtFixture(EventAdmitPatient)
	pid, _ := m.Segment("PID")
	pid.SetField(5, Components("MÉDICI", "ANA"))
	pid.SetField(3, Field{{{"MR-ONE"}, nil, nil, {"AUTH", "1.2.3", "ISO"}, {"MR"}}, {{"SECOND"}, nil, nil, {"HÔPITAL"}, {"PI"}}})
	adt := parseADTFixture(t, m)
	before := m.String()
	patient, err := adt.Patient.DICOMAttributes(1)
	if err != nil {
		t.Fatal(err)
	}
	if got := object.AllPatientIDsWithIssuer(patient); len(got) != 2 || got[0].ID != "SECOND" || got[0].Issuer.LocalNamespaceEntityID != "HÔPITAL" || got[1].ID != "MR-ONE" || got[1].Issuer.UniversalEntityID != "1.2.3" {
		t.Fatalf("IDs: %+v", got)
	}
	assertADTText(t, patient, core.NewTag(0x0010, 0x0010), "MÉDICI^ANA")
	assertADTText(t, patient, core.NewTag(0x0010, 0x0030), "19700101")
	assertADTText(t, patient, core.NewTag(0x0010, 0x0040), "F")
	visit, err := adt.Visit.DICOMAttributes()
	if err != nil {
		t.Fatal(err)
	}
	for tag, value := range map[core.Tag]string{
		core.NewTag(0x0038, 0x0010): "VISIT-ONE", core.NewTag(0x0038, 0x0300): "WARD/ROOM/BED", core.NewTag(0x0038, 0x0016): "E",
		core.NewTag(0x0008, 0x0090): "REFERRING^DOCTOR^Q^DR^JR", core.NewTag(0x0038, 0x0020): "20261009", core.NewTag(0x0038, 0x0021): "113015.012", core.NewTag(0x0008, 0x0201): "-0300",
	} {
		assertADTText(t, visit, tag, value)
	}
	issuer, ok := visit.GetSequence(core.NewTag(0x0038, 0x0014))
	if !ok || len(issuer) != 1 {
		t.Fatal("admission issuer")
	}
	assertADTText(t, issuer[0], core.NewTag(0x0040, 0x0031), "VISIT-AUTH")
	assertADTText(t, issuer[0], core.NewTag(0x0040, 0x0032), "1.2.4")
	for _, syntax := range []transfer.Syntax{transfer.ExplicitVRLittleEndian, transfer.ExplicitVRBigEndian, transfer.ImplicitVRLittleEndian} {
		t.Run(syntax.Name, func(t *testing.T) {
			for _, attrs := range []*object.Object{patient, visit} {
				var encoded bytes.Buffer
				if err := object.WriteDataSet(&encoded, attrs, syntax); err != nil {
					t.Fatal(err)
				}
				read, err := object.ReadDataSet(bytes.NewReader(encoded.Bytes()), syntax)
				if err != nil {
					t.Fatal(err)
				}
				if attrs == patient {
					assertADTText(t, read, core.NewTag(0x0010, 0x0010), "MÉDICI^ANA")
					if ids := object.AllPatientIDsWithIssuer(read); len(ids) != 2 || ids[0].Issuer.LocalNamespaceEntityID != "HÔPITAL" {
						t.Fatalf("UTF8 IDs: %+v", ids)
					}
				} else {
					assertADTText(t, read, core.NewTag(0x0038, 0x0021), "113015.012")
				}
			}
		})
	}
	patient.Put(core.Element{Header: core.ElementHeader{Tag: adtPatientID, VR: core.VRLO}, Value: core.StringValue{"EDITED"}})
	if adt.Patient.Identifiers[1].ID != "SECOND" || m.String() != before {
		t.Fatal("mapper mutated the source")
	}
}

func assertADTText(t *testing.T, o *object.Object, tag core.Tag, want string) {
	t.Helper()
	elem, ok := o.Get(tag)
	if !ok {
		t.Fatalf("missing %s", tag)
	}
	got, err := o.LookupString(tag)
	if err != nil || got != want {
		t.Fatalf("%s = %q, %v; want %q (VR %s)", tag, got, err, want, elem.VR())
	}
}

func TestADTNullVersusEmptyMapping(t *testing.T) {
	for _, null := range []bool{false, true} {
		t.Run(map[bool]string{false: "empty", true: "null"}[null], func(t *testing.T) {
			m := adtFixture(EventAdmitPatient)
			pid, _ := m.Segment("PID")
			pv1, _ := m.Segment("PV1")
			value := ""
			if null {
				value = Null
			}
			for _, field := range []int{5, 7, 8} {
				pid.SetValue(field, value)
			}
			for _, field := range []int{3, 4, 8, 19, 44} {
				pv1.SetValue(field, value)
			}
			adt := parseADTFixture(t, m)
			patient, err := adt.Patient.DICOMAttributes(0)
			if err != nil {
				t.Fatal(err)
			}
			visit, err := adt.Visit.DICOMAttributes()
			if err != nil {
				t.Fatal(err)
			}
			for _, entry := range []struct {
				o   *object.Object
				tag core.Tag
			}{
				{patient, core.NewTag(0x0010, 0x0010)}, {patient, core.NewTag(0x0010, 0x0030)}, {patient, core.NewTag(0x0010, 0x0040)},
				{visit, core.NewTag(0x0038, 0x0300)}, {visit, core.NewTag(0x0038, 0x0016)}, {visit, core.NewTag(0x0008, 0x0090)},
				{visit, core.NewTag(0x0038, 0x0010)}, {visit, core.NewTag(0x0038, 0x0020)}, {visit, core.NewTag(0x0038, 0x0021)},
			} {
				elem, exists := entry.o.Get(entry.tag)
				if exists != null {
					t.Fatalf("%s presence %v, null %v", entry.tag, exists, null)
				}
				if exists && elem.StringValue() != "" {
					t.Fatalf("%s not cleared", entry.tag)
				}
			}
			seq, exists := visit.GetSequence(core.NewTag(0x0038, 0x0014))
			if exists != null || len(seq) != 0 {
				t.Fatal("null admission issuer")
			}
		})
	}
	for _, sex := range []string{"M", "F", "O", "A", "N", "X", "U"} {
		t.Run("sex-"+sex, func(t *testing.T) {
			p := Patient{Identifiers: []PatientIdentifier{{IDWithIssuer: object.IDWithIssuer{ID: "ID"}}}, Sex: sex}
			attrs, err := p.DICOMAttributes(0)
			if err != nil {
				t.Fatal(err)
			}
			assertADTText(t, attrs, core.NewTag(0x0010, 0x0040), p.DICOMSex())
		})
	}
}

func TestADTExpandedErrors(t *testing.T) {
	tests := []struct {
		name, event string
		mutate      func(*Message)
		segment     string
		field       int
		code        string
	}{
		{"no PID", EventAddPerson, func(m *Message) { m.Segments = append(m.Segments[:2], m.Segments[3:]...) }, "PID", 0, ErrorCodeSegmentSequence},
		{"empty PID3", EventUpdatePerson, func(m *Message) { p, _ := m.Segment("PID"); p.SetValue(3, "") }, "PID", 3, ErrorCodeRequiredFieldMissing},
		{"null PID3", EventRegisterPatient, func(m *Message) { p, _ := m.Segment("PID"); p.SetValue(3, Null) }, "PID", 3, ErrorCodeRequiredFieldMissing},
		{"no PV1", EventAdmitPatient, func(m *Message) { m.Segments = m.Segments[:3] }, "PV1", 0, ErrorCodeSegmentSequence},
		{"empty class", EventRegisterPatient, func(m *Message) { p, _ := m.Segment("PV1"); p.SetValue(2, "") }, "PV1", 2, ErrorCodeRequiredFieldMissing},
		{"null class", EventAdmitPatient, func(m *Message) { p, _ := m.Segment("PV1"); p.SetValue(2, Null) }, "PV1", 2, ErrorCodeRequiredFieldMissing},
		{"PV1 before PID", EventAdmitPatient, func(m *Message) { m.Segments[2], m.Segments[3] = m.Segments[3], m.Segments[2] }, "PV1", 0, ErrorCodeSegmentSequence},
		{"space class", EventRegisterPatient, func(m *Message) { p, _ := m.Segment("PV1"); p.SetValue(2, " ") }, "PV1", 2, ErrorCodeRequiredFieldMissing},
		{"duplicate PID", EventAddPerson, func(m *Message) { m.Add("PID").SetValue(3, "ID") }, "PID", 0, ErrorCodeSegmentSequence},
		{"duplicate PV1", EventRegisterPatient, func(m *Message) { m.Add("PV1").SetValue(2, "O") }, "PV1", 0, ErrorCodeSegmentSequence},
		{"A47 no MRG", EventChangePatientIdentifier, func(m *Message) { m.Segments = m.Segments[:3] }, "MRG", 0, ErrorCodeSegmentSequence},
		{"A47 empty MRG1", EventChangePatientIdentifier, func(m *Message) { p, _ := m.Segment("MRG"); p.SetValue(1, "") }, "MRG", 1, ErrorCodeRequiredFieldMissing},
		{"A47 empty PID3", EventChangePatientIdentifier, func(m *Message) { p, _ := m.Segment("PID"); p.SetValue(3, "") }, "PID", 3, ErrorCodeRequiredFieldMissing},
		{"A47 MRG before PID", EventChangePatientIdentifier, func(m *Message) { m.Segments[2], m.Segments[3] = m.Segments[3], m.Segments[2] }, "MRG", 0, ErrorCodeSegmentSequence},
		{"A47 second MRG", EventChangePatientIdentifier, func(m *Message) { m.Add("MRG").SetValue(1, "ID") }, "MRG", 0, ErrorCodeSegmentSequence},
		{"A47 second PID", EventChangePatientIdentifier, func(m *Message) { m.Add("PID").SetValue(3, "ID") }, "PID", 0, ErrorCodeSegmentSequence},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			m := adtFixture(tc.event)
			tc.mutate(m)
			adt, err := ParseADT(m)
			var hl7Err *Error
			if adt != nil || !errors.As(err, &hl7Err) || hl7Err.Code != tc.code || hl7Err.Location.Segment != tc.segment || hl7Err.Location.Field != tc.field {
				t.Fatalf("wrong refusal: %+v, %v", adt, err)
			}
			sequence := 1
			if strings.Contains(tc.name, "duplicate") || strings.Contains(tc.name, "second") {
				sequence = 2
			}
			if hl7Err.Location.Sequence != sequence {
				t.Fatalf("ERR occurrence %d, want %d", hl7Err.Location.Sequence, sequence)
			}
			ack, err := ParseAcknowledgment(ACKFor(m, err, AckOptions{}))
			if err != nil || ack.Code != AckError || ack.ErrorCode != tc.code {
				t.Fatalf("ACK: %+v, %v", ack, err)
			}
		})
	}
	var typed *Error
	if _, err := ParseADT(nil); !errors.As(err, &typed) || !typed.Reject || typed.Code != ErrorCodeUnsupportedMessageType {
		t.Fatalf("nil message: %v", err)
	}
}

func TestADTMappingRefusals(t *testing.T) {
	for _, tc := range []struct {
		name   string
		mutate func(*Message)
		field  int
	}{
		{"partial birth", func(m *Message) { p, _ := m.Segment("PID"); p.SetValue(7, "197001") }, 7},
		{"invalid birth", func(m *Message) { p, _ := m.Segment("PID"); p.SetValue(7, "19700231") }, 7},
		{"escaped component", func(m *Message) { p, _ := m.Segment("PID"); p.SetField(5, Components("FAMILY^PART", "GIVEN")) }, 5},
		{"escaped value separator", func(m *Message) { p, _ := m.Segment("PID"); p.SetField(3, Components("SECRET\\VALUE")) }, 3},
		{"long identifier", func(m *Message) { p, _ := m.Segment("PID"); p.SetValue(3, strings.Repeat("S", 65)) }, 3},
		{"authority without type", func(m *Message) {
			p, _ := m.Segment("PID")
			p.SetField(3, Field{{{"ID"}, nil, nil, {"AUTH", "1.2.3"}}})
		}, 3},
	} {
		t.Run(tc.name, func(t *testing.T) {
			m := adtFixture(EventAdmitPatient)
			tc.mutate(m)
			adt := parseADTFixture(t, m)
			out, err := adt.Patient.DICOMAttributes(0)
			var typed *Error
			if out != nil || !errors.As(err, &typed) || typed.Location.Field != tc.field || typed.Code != ErrorCodeDataType || strings.Contains(err.Error(), "SECRET") || strings.Contains(err.Error(), strings.Repeat("S", 65)) {
				t.Fatalf("refusal: %v", err)
			}
		})
	}
	p := Patient{Identifiers: []PatientIdentifier{{IDWithIssuer: object.IDWithIssuer{ID: "ID"}}}}
	for _, i := range []int{-1, 1} {
		if attrs, err := p.DICOMAttributes(i); attrs != nil || err == nil {
			t.Fatal("invalid primary index accepted")
		}
	}
	for _, tc := range []struct {
		name   string
		mutate func(*Visit)
		field  int
	}{
		{"partial admission", func(v *Visit) { v.AdmittedDateTime = "202610" }, 44},
		{"invalid admission", func(v *Visit) { v.AdmittedDateTime = "2026023010" }, 44},
		{"invalid offset", func(v *Visit) { v.AdmittedDateTime = "202610091000+1560" }, 44},
		{"unrepresentable physician", func(v *Visit) { v.ReferringNameHasReservedCharacters = true }, 8},
		{"authority without type", func(v *Visit) { v.Number.Issuer.UniversalEntityIDType = "" }, 19},
	} {
		t.Run(tc.name, func(t *testing.T) {
			v := *parseADTFixture(t, adtFixture(EventAdmitPatient)).Visit
			tc.mutate(&v)
			attrs, err := v.DICOMAttributes()
			var typed *Error
			if attrs != nil || !errors.As(err, &typed) || typed.Location.Field != tc.field {
				t.Fatalf("wrong refusal: %v", err)
			}
		})
	}
}

func TestADTAdmissionPrecisionAndLegacyBehavior(t *testing.T) {
	for _, value := range []string{"20261009", "2026100912", "202610091234", "20261009123456", "20261009123456.123456+1400", "20260630235960-1200"} {
		t.Run(value, func(t *testing.T) {
			v := Visit{AdmittedDateTime: value}
			attrs, err := v.DICOMAttributes()
			if err != nil {
				t.Fatal(err)
			}
			assertADTText(t, attrs, core.NewTag(0x0038, 0x0020), value[:8])
			timePart := value[8:]
			if i := strings.IndexAny(timePart, "+-"); i >= 0 {
				assertADTText(t, attrs, core.NewTag(0x0008, 0x0201), timePart[i:])
				timePart = timePart[:i]
			}
			if timePart == "" {
				if _, ok := attrs.Get(core.NewTag(0x0038, 0x0021)); ok {
					t.Fatal("invented missing time")
				}
			} else {
				assertADTText(t, attrs, core.NewTag(0x0038, 0x0021), timePart)
			}
		})
	}
	for _, pv1 := range []string{"", "PV1|", "PV1|1|N"} {
		m, err := Parse(message(`MSH|^~\&|A|B|C|D|20261009||ADT^A08|ID|P|2.5.1`, "PID|1||ID", pv1))
		if err != nil {
			t.Fatal(err)
		}
		adt, err := ParseADT(m)
		if err != nil || adt.Visit != nil {
			t.Fatalf("legacy A08 without real visit: %+v, %v", adt, err)
		}
	}
	m := adtFixture(EventAddPerson)
	m.Segments[0].SetField(9, Components("ADT"))
	m.Segments[1].SetValue(1, EventAddPerson)
	if adt := parseADTFixture(t, m); adt.Event != EventAddPerson {
		t.Fatal("EVN fallback changed")
	}
}

func TestADTCharsetEscapingAndConcurrentMapping(t *testing.T) {
	m, err := Parse(message(`MSH|^~\&|A|B|C|D|20261009||ADT^A31^ADT_A05|ID|P|2.5.1||||||8859/1`, "PID|1||ID||M\xc9DICI^ANA\\F\\B", "PV1|1|N"))
	if err != nil {
		t.Fatal(err)
	}
	adt, err := ParseADT(m)
	if err != nil {
		t.Fatal(err)
	}
	if adt.Patient.Name != "MÉDICI^ANA|B" {
		t.Fatalf("HL7 decode: %q", adt.Patient.Name)
	}
	var wg sync.WaitGroup
	for i := 0; i < 16; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			attrs, err := adt.Patient.DICOMAttributes(0)
			if err != nil {
				t.Error(err)
				return
			}
			got, err := attrs.LookupString(core.NewTag(0x0010, 0x0010))
			if err != nil || got != "MÉDICI^ANA|B" {
				t.Errorf("mapping %q, %v", got, err)
			}
		}()
	}
	wg.Wait()
	m, err = Parse([]byte("MSH#$*@%#A#B#C#D#20261009##ADT$A04#ID#P#2.5.1\rPID#1##ID$$$AUTH##SYNTHETIC$PATIENT\rPV1#1#O"))
	if err != nil {
		t.Fatal(err)
	}
	adt, err = ParseADT(m)
	if err != nil || adt.Visit == nil || adt.Patient.Name != "SYNTHETIC^PATIENT" {
		t.Fatalf("custom delimiters: %+v, %v", adt, err)
	}
}

func TestADTA08VisitAndFirstSegmentSelection(t *testing.T) {
	m := adtFixture(EventUpdatePatient)
	m.Add("PID").SetValue(3, "LATER-PATIENT")
	later := m.Add("PV1")
	later.SetValue(2, "O")
	later.SetValue(19, "LATER-VISIT")
	adt := parseADTFixture(t, m)
	if adt.Patient.Identifiers[0].ID != "MR-ONE" || adt.Visit == nil || adt.Visit.PatientClass != "I" || adt.Visit.Number.ID != "VISIT-ONE" {
		t.Fatalf("A08 must retain first PID/PV1 selection: %+v", adt)
	}
}
