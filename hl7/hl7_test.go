package hl7

import (
	"errors"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/ThalesMMS/dicom-go/object"
)

// Synthetic messages: no real patients.
func message(lines ...string) []byte { return []byte(strings.Join(lines, "\r") + "\r") }

var adtA08 = message(
	`MSH|^~\&|ADT1|HOSP|TWIN|RAD|20261007120000||ADT^A08^ADT_A01|MSG00001|P|2.5.1`,
	`EVN|A08|20261007115900`,
	`PID|1||MRN123^^^HOSP&1.2.3.4&ISO^MR~ALT9^^^OTHER^PI||SYNTHETIC^PATIENT^Q^^DR||19700101|F`,
	`PV1|1|O`,
	`ZXY|custom\F\value|a\S\b\T\c\R\d\E\e|\X41\\.br\x\H\y\N\|""`,
)

func TestParseADTA08(t *testing.T) {
	m, err := Parse(adtA08)
	if err != nil {
		t.Fatal(err)
	}
	if m.Delimiters != DefaultDelimiters {
		t.Fatalf("delimiters %+v", m.Delimiters)
	}
	h := m.Header()
	if h.SendingApplication.NamespaceID != "ADT1" || h.ReceivingFacility.NamespaceID != "RAD" || h.Type != (MessageType{"ADT", "A08", "ADT_A01"}) ||
		h.ControlID != "MSG00001" || h.ProcessingID != "P" || h.Version != "2.5.1" || h.DateTime != "20261007120000" {
		t.Fatalf("header %+v", h)
	}
	msh, _ := m.Segment("MSH")
	if msh.Field(1).String() != "|" || msh.Field(2).String() != `^~\&` {
		t.Fatalf("MSH-1/2 = %q %q", msh.Field(1).String(), msh.Field(2).String())
	}
	z, ok := m.Segment("ZXY")
	if !ok {
		t.Fatal("Z segment lost")
	}
	if got := z.Field(1).String(); got != "custom|value" {
		t.Fatalf("ZXY-1 = %q", got)
	}
	if got := z.Field(2).String(); got != `a^b&c~d\e` {
		t.Fatalf("ZXY-2 = %q", got)
	}
	if got := z.Field(3).String(); got != "A\nxy" {
		t.Fatalf("ZXY-3 = %q", got)
	}
	if !z.Field(4).IsNull() || z.Field(5) != nil {
		t.Fatalf("ZXY-4 null %v, ZXY-5 %v", z.Field(4).IsNull(), z.Field(5))
	}

	adt, err := ParseADT(m)
	if err != nil {
		t.Fatal(err)
	}
	want := Patient{
		Identifiers: []PatientIdentifier{
			{IDWithIssuer: object.IDWithIssuer{ID: "MRN123", Issuer: object.Issuer{LocalNamespaceEntityID: "HOSP", UniversalEntityID: "1.2.3.4", UniversalEntityIDType: "ISO"}}, IdentifierTypeCode: "MR"},
			{IDWithIssuer: object.IDWithIssuer{ID: "ALT9", Issuer: object.Issuer{LocalNamespaceEntityID: "OTHER"}}, IdentifierTypeCode: "PI"},
		},
		Name: "SYNTHETIC^PATIENT^Q^DR", BirthDate: "19700101", Sex: "F",
	}
	if adt.Event != EventUpdatePatient || adt.RecordedDateTime != "20261007115900" || !reflect.DeepEqual(adt.Patient, want) {
		t.Fatalf("ADT = %+v\nwant patient %+v", adt, want)
	}
	if adt.Patient.DICOMSex() != "F" || len(adt.Patient.DICOMPatientIDs()) != 2 {
		t.Fatal("DICOM mapping")
	}
	// The DICOM matching rule of the issuer applies to HL7 identifiers.
	dicom := object.IDWithIssuer{ID: "MRN123", Issuer: object.Issuer{UniversalEntityID: "1.2.3.4", UniversalEntityIDType: "ISO"}}
	if !adt.Patient.Identifiers[0].Matches(dicom) || adt.Patient.Identifiers[1].Matches(dicom) {
		t.Fatal("issuer matching")
	}
}

func TestEncodeRoundTrip(t *testing.T) {
	m, err := Parse(adtA08)
	if err != nil {
		t.Fatal(err)
	}
	again, err := Parse([]byte(m.String()))
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(m.Segments, again.Segments) {
		t.Fatalf("round trip changed the message:\n%q\n%q", adtA08, m.String())
	}
	if !strings.Contains(m.String(), `ZXY|custom\F\value|a\S\b\T\c\R\d\E\e|A\.br\xy|""`) {
		t.Fatalf("escaped output %q", m.String())
	}
}

func TestCustomDelimitersAndLineEndings(t *testing.T) {
	data := []byte("MSH#$*@%#APP#FAC#####ADT$A08#C1#P#2.5\r\nPID#1##ID1$$$AUTH#\nPID#2##ID2*ID3")
	m, err := Parse(data)
	if err != nil {
		t.Fatal(err)
	}
	if m.Delimiters != (Delimiters{Field: '#', Component: '$', Repetition: '*', Escape: '@', Subcomponent: '%'}) {
		t.Fatalf("delimiters %+v", m.Delimiters)
	}
	pids := m.All("PID")
	if len(pids) != 2 || pids[0].Value(3, 4, 1) != "AUTH" || len(pids[1].Field(3)) != 2 || pids[1].Field(3)[1].String(1) != "ID3" {
		t.Fatalf("PIDs %+v", pids)
	}
	if !strings.HasPrefix(m.String(), "MSH#$*@%#APP#FAC") {
		t.Fatalf("encoded %q", m.String())
	}
}

func TestParseER7EscapesProtectDelimiters(t *testing.T) {
	const standardHeader = "MSH|^~\\&|\\Zfield|separator\\|B|C|D|20261007||ADT^A08|C1|P|2.5||||||UNICODE UTF-8"
	const customHeader = "MSH*%!?@*A*B*C*D*20261007**ADT%A08*C1*P*2.5"
	for _, tc := range []struct {
		name    string
		header  string
		wire    string
		want    Field
		charset string
	}{
		{"default", standardHeader, "ZXY|left&\\Zopaque|^~&séquence\\^right~second|LAST",
			Field{{{"left", "\\Zopaque|^~&séquence\\"}, {"right"}}, {{"second"}}}, "UNICODE UTF-8"},
		{"custom", customHeader, "ZXY*?Zopaque*%!@sequence?*LAST", Text("?Zopaque*%!@sequence?"), ""},
		{"unterminated", customHeader, "ZXY*?Zopaque*LAST", Text("?Zopaque"), ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			m, err := ParseWithOptions(message(tc.header, tc.wire), ParseOptions{MaxFields: 18, MaxRepetitions: 2})
			if err != nil {
				t.Fatal(err)
			}
			if m.Header().CharacterSet != tc.charset {
				t.Errorf("MSH-18 = %q, want %q", m.Header().CharacterSet, tc.charset)
			}
			segment, _ := m.Segment("ZXY")
			want := []Field{tc.want, Text("LAST")}
			if !reflect.DeepEqual(segment.Fields, want) {
				t.Errorf("ZXY fields = %#v; want %#v", segment.Fields, want)
			}
		})
	}
}

func TestCharacterSets(t *testing.T) {
	latin1 := message(`MSH|^~\&|A|B|C|D|20261007||ADT^A08|C1|P|2.5|||||| 8859/1`, "PID|1||X1||M\xc9DICI^ANA")
	if _, err := Parse(latin1); err != nil {
		t.Fatal(err)
	}
	latin1 = message(`MSH|^~\&|A|B|C|D|20261007||ADT^A08|C1|P|2.5||||||8859/1`, "PID|1||X1||M\xc9DICI^ANA|\\XE9\\")
	m, err := Parse(latin1)
	if err != nil {
		t.Fatal(err)
	}
	pid, _ := m.Segment("PID")
	if got := pid.Value(5, 1, 1); got != "MÉDICI" {
		t.Fatalf("Latin-1 name %q", got)
	}
	if got := pid.Field(6).String(); got != "é" {
		t.Fatalf("hex escape in Latin-1 %q", got)
	}
	encoded, err := m.Marshal()
	if err != nil || !strings.Contains(string(encoded), "M\xc9DICI") {
		t.Fatalf("Marshal back to Latin-1: %v %q", err, encoded)
	}

	// Big5 許 is B3 5C: its second byte is the escape character, harmless
	// because the message is decoded before it is split.
	big5 := message("MSH|^~\\&|A\xb3\x5c|B\xb3\x5c|C|D|20261007||ADT^A08|C1|P|2.5||||||BIG-5", "PID|1||X1||\xb3\x5c^A")
	if m, err = Parse(big5); err != nil {
		t.Fatal(err)
	}
	if got := m.Header().SendingApplication.NamespaceID; got != "A許" {
		t.Fatalf("Big5 MSH sending application %q", got)
	}
	pid, _ = m.Segment("PID")
	if got := pid.Value(5, 1, 1); got != "許" {
		t.Fatalf("Big5 name %q", got)
	}

	ascii := message(`MSH|^~\&|A|B|C|D|20261007||ADT^A08|C1|P|2.5`, "PID|1||X1||M\xc3\x89DICI")
	var parseErr *ParseError
	if _, err := Parse(ascii); !errors.Is(err, ErrCharset) || !errors.As(err, &parseErr) || parseErr.Field != 18 {
		t.Fatalf("non-ASCII without MSH-18: %v", err)
	}
	if m, err = ParseWithOptions(ascii, ParseOptions{DefaultCharacterSet: "UNICODE UTF-8"}); err != nil {
		t.Fatal(err)
	}
	if _, err := Parse(message(`MSH|^~\&|A|B|C|D|20261007||ADT^A08|C1|P|2.5||||||UNICODE UTF-16`)); !errors.Is(err, ErrCharset) {
		t.Fatalf("UTF-16: %v", err)
	}
}

func TestParseErrorsNameNoValues(t *testing.T) {
	secret := "SECRETNAME"
	cases := []struct {
		name    string
		data    []byte
		options ParseOptions
		want    error
	}{
		{"no MSH", message("PID|1||" + secret), ParseOptions{}, ErrSyntax},
		{"repeated delimiter", message(`MSH|^^\&|` + secret), ParseOptions{}, ErrSyntax},
		{"bad segment ID", message(`MSH|^~\&|A`, "pid|"+secret), ParseOptions{}, ErrSyntax},
		{"second MSH", message(`MSH|^~\&|A`, `MSH|^~\&|`+secret), ParseOptions{}, ErrSyntax},
		{"bytes", message(`MSH|^~\&|A`, "PID|"+secret), ParseOptions{MaxBytes: 10}, ErrLimit},
		{"segments", message(`MSH|^~\&|A`, "PID|"+secret, "PV1|"), ParseOptions{MaxSegments: 2}, ErrLimit},
		{"fields", message(`MSH|^~\&|A`, "PID|1|2|"+secret), ParseOptions{MaxFields: 2}, ErrLimit},
		{"repetitions", message(`MSH|^~\&|A`, "PID|1||a~b~"+secret), ParseOptions{MaxRepetitions: 2}, ErrLimit},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := ParseWithOptions(tc.data, tc.options)
			if !errors.Is(err, tc.want) {
				t.Fatalf("error %v, want %v", err, tc.want)
			}
			if strings.Contains(err.Error(), secret) {
				t.Fatalf("error %q quotes the message", err)
			}
		})
	}
}

func TestACK(t *testing.T) {
	m, err := Parse(adtA08)
	if err != nil {
		t.Fatal(err)
	}
	at := time.Date(2026, 10, 7, 12, 0, 1, 0, time.UTC)
	ack := NewACK(m, AckAccept, AckOptions{ControlID: "ACK1", Time: at})
	want := "MSH|^~\\&|TWIN|RAD|ADT1|HOSP|20261007120001||ACK^A08^ACK|ACK1|P|2.5.1\rMSA|AA|MSG00001\r"
	if ack.String() != want {
		t.Fatalf("AA\n%q\nwant\n%q", ack.String(), want)
	}

	m.Segments[2].Fields[2] = nil // PID-3
	_, adtErr := ParseADT(m)
	nak := ACKFor(m, adtErr, AckOptions{ControlID: "ACK2", Time: at})
	want = "MSH|^~\\&|TWIN|RAD|ADT1|HOSP|20261007120001||ACK^A08^ACK|ACK2|P|2.5.1\rMSA|AE|MSG00001|Required field missing\rERR||PID^1^3|101^Required field missing^HL70357|E\r"
	if nak.String() != want {
		t.Fatalf("AE\n%q\nwant\n%q", nak.String(), want)
	}
	parsed, err := ParseAcknowledgment(nak)
	if err != nil || parsed != (Acknowledgment{Code: AckError, ControlID: "MSG00001", Text: "Required field missing", ErrorCode: "101"}) {
		t.Fatalf("ParseAcknowledgment = %+v, %v", parsed, err)
	}

	m.Segments[0].Fields[8] = Components("ADT", "A99")
	_, adtErr = ParseADT(m)
	reject := ACKFor(m, adtErr, AckOptions{})
	if got, _ := ParseAcknowledgment(reject); got.Code != AckReject || got.ErrorCode != ErrorCodeUnsupportedEventCode {
		t.Fatalf("unsupported event: %+v", got)
	}

	v23 := message(`MSH|^~\&|A|B|C|D|20261007||ACK|X|P|2.3`, "MSA|AE|C1", "ERR|PID^1^3^101&Required field missing")
	m, err = Parse(v23)
	if err != nil {
		t.Fatal(err)
	}
	if got, err := ParseAcknowledgment(m); err != nil || got.ErrorCode != "101" {
		t.Fatalf("v2.3 ERR-1: %+v %v", got, err)
	}
}

func TestParseADTA40(t *testing.T) {
	data := message(
		`MSH|^~\&|ADT1|HOSP|TWIN|RAD|20261007||ADT^A40^ADT_A39|MSG2|P|2.5`,
		`EVN|A40|20261007`,
		`PID|1||KEEP1^^^HOSP^MR`,
		`MRG|OLD1^^^HOSP^MR~OLD2^^^HOSP^MR`,
		`PID|2||KEEP2^^^&2.16.840.1.1&ISO`,
		`MRG|OLD3^^^&2.16.840.1.1&ISO`,
	)
	m, err := Parse(data)
	if err != nil {
		t.Fatal(err)
	}
	adt, err := ParseADT(m)
	if err != nil {
		t.Fatal(err)
	}
	if len(adt.Merges) != 2 || len(adt.Merges[0].Prior) != 2 || adt.Merges[0].Prior[1].ID != "OLD2" ||
		adt.Merges[1].Patient.Identifiers[0].Issuer.UniversalEntityID != "2.16.840.1.1" || adt.Merges[1].Prior[0].ID != "OLD3" {
		t.Fatalf("merges %+v", adt.Merges)
	}
	for _, tc := range []struct {
		name string
		data []byte
		code string
	}{
		{"MRG without PID", message(`MSH|^~\&|A|B|C|D|1||ADT^A40|C|P|2.5`, `MRG|OLD1`), ErrorCodeSegmentSequence},
		{"PID without MRG", message(`MSH|^~\&|A|B|C|D|1||ADT^A40|C|P|2.5`, `PID|1||K1`), ErrorCodeSegmentSequence},
		{"empty MRG-1", message(`MSH|^~\&|A|B|C|D|1||ADT^A40|C|P|2.5`, `PID|1||K1`, `MRG|`), ErrorCodeRequiredFieldMissing},
		{"not ADT", message(`MSH|^~\&|A|B|C|D|1||ORM^O01|C|P|2.5`), ErrorCodeUnsupportedMessageType},
	} {
		m, err := Parse(tc.data)
		if err != nil {
			t.Fatal(err)
		}
		var hl7Err *Error
		if _, err := ParseADT(m); !errors.As(err, &hl7Err) || hl7Err.Code != tc.code {
			t.Fatalf("%s: %v, want code %s", tc.name, err, tc.code)
		}
	}
}

func TestPDQAndPIXQueries(t *testing.T) {
	header := MessageHeader{
		SendingApplication: HierarchicDesignator{NamespaceID: "TWIN"}, SendingFacility: HierarchicDesignator{NamespaceID: "RAD"},
		ReceivingApplication: HierarchicDesignator{NamespaceID: "MPI"}, ReceivingFacility: HierarchicDesignator{NamespaceID: "HOSP"},
		DateTime: "20261007120000", ControlID: "Q1",
	}
	pdq := NewPDQQuery([]QueryParameter{{Field: "PID.5.1.1", Value: "SYNTH"}, {Field: "@PID.8", Value: "F"}}, QueryOptions{Header: header, Limit: 10})
	want := "MSH|^~\\&|TWIN|RAD|MPI|HOSP|20261007120000||QBP^Q22^QBP_Q21|Q1|P|2.5\rQPD|IHE PDQ Query|Q1|@PID.5.1.1^SYNTH~@PID.8^F\rRCP|I|10^RD\r"
	if pdq.String() != want {
		t.Fatalf("PDQ\n%q\nwant\n%q", pdq.String(), want)
	}
	header.ControlID = "Q2"
	id := PatientIdentifier{IDWithIssuer: object.IDWithIssuer{ID: "L101", Issuer: object.Issuer{LocalNamespaceEntityID: "LOCAL", UniversalEntityID: "1.2.3", UniversalEntityIDType: "ISO"}}, IdentifierTypeCode: "PI"}
	pix := NewPIXQuery(id, []HierarchicDesignator{{NamespaceID: "OTHER", UniversalID: "1.2.4", UniversalIDType: "ISO"}}, QueryOptions{Header: header, Tag: "T2"})
	want = "MSH|^~\\&|TWIN|RAD|MPI|HOSP|20261007120000||QBP^Q23^QBP_Q21|Q2|P|2.5\rQPD|IHE PIX Query|T2|L101^^^LOCAL&1.2.3&ISO^PI|^^^OTHER&1.2.4&ISO\rRCP|I\r"
	if pix.String() != want {
		t.Fatalf("PIX\n%q\nwant\n%q", pix.String(), want)
	}

	k22 := message(
		`MSH|^~\&|MPI|HOSP|TWIN|RAD|20261007||RSP^K22^RSP_K21|R1|P|2.5`,
		`MSA|AA|Q1`,
		`QAK|Q1|OK`,
		`QPD|IHE PDQ Query|Q1|@PID.5.1.1^SYNTH`,
		`PID|1||A1^^^HOSP^MR||SYNTH^ONE||19800101|F`,
		`PID|2||A2^^^HOSP^MR||SYNTH^TWO||19900101|M`,
		`DSC|CONT1|I`,
	)
	m, err := Parse(k22)
	if err != nil {
		t.Fatal(err)
	}
	response, err := ParseQueryResponse(m)
	if err != nil {
		t.Fatal(err)
	}
	if response.Acknowledgment.Code != AckAccept || response.Tag != "Q1" || response.Status != "OK" || len(response.Patients) != 2 ||
		response.Patients[1].Name != "SYNTH^TWO" || response.Continuation != "CONT1" {
		t.Fatalf("PDQ response %+v", response)
	}
	k23 := message(
		`MSH|^~\&|MPI|HOSP|TWIN|RAD|20261007||RSP^K23^RSP_K23|R2|P|2.5`,
		`MSA|AA|Q2`,
		`QAK|T2|OK`,
		`QPD|IHE PIX Query|T2|L101^^^LOCAL&1.2.3&ISO^PI`,
		`PID|||X9^^^OTHER&1.2.4&ISO^PI~Y7^^^THIRD&1.2.5&ISO^PI`,
	)
	if m, err = Parse(k23); err != nil {
		t.Fatal(err)
	}
	if response, err = ParseQueryResponse(m); err != nil {
		t.Fatal(err)
	}
	if len(response.Patients) != 1 || len(response.Patients[0].Identifiers) != 2 || response.Patients[0].Identifiers[1].Issuer.UniversalEntityID != "1.2.5" {
		t.Fatalf("PIX response %+v", response)
	}
}

// FuzzParse checks that arbitrary input never panics and that what parses
// encodes to text that parses to the same segments.
func FuzzParse(f *testing.F) {
	f.Add(adtA08)
	f.Add([]byte("MSH#$*@%#APP\nPID#1##ID1$$$AUTH"))
	f.Add([]byte("MSH|^~\\&|A|B|C|D|1||ADT^A40|C|P|2.5||||||BIG-5\rPID|1||\xb3\x5c\rMRG|X\\X41\\"))
	f.Fuzz(func(t *testing.T, data []byte) {
		m, err := ParseWithOptions(data, ParseOptions{MaxBytes: 1 << 16})
		if err != nil {
			return
		}
		_, _ = ParseADT(m)
		_, _ = ParseQueryResponse(m)
		_ = NewACK(m, AckError, AckOptions{})
		encoded, err := m.Marshal()
		if err != nil {
			return // not representable in the declared set
		}
		again, err := ParseWithOptions(encoded, ParseOptions{DefaultCharacterSet: "UNICODE UTF-8"})
		if err != nil {
			t.Fatalf("re-parse of the encoding failed: %v", err)
		}
		if len(again.Segments) != len(m.Segments) {
			t.Fatalf("re-parse has %d segments, want %d", len(again.Segments), len(m.Segments))
		}
	})
}
