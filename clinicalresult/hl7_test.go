package clinicalresult

import (
	"crypto/sha256"
	"errors"
	"reflect"
	"strings"
	"testing"

	"github.com/ThalesMMS/dicom-go/hl7"
)

const (
	syntheticHL7PID    = "PID|1||TEST-P1^^^HOSP&1.2.3&ISO^MR~ALT-1^^^OTHER^PI||Example^Synthetic||19800102|F"
	syntheticHL7Placer = "ORD-7^RIS^1.2.4^ISO"
	syntheticHL7Filler = "READ-7^PACS"
)

func TestHL7OrderRetainsQualifiedIdentifiers(t *testing.T) {
	wire := syntheticHL7Wire("ORM^O01^ORM_O01", "ORDER-MESSAGE", syntheticHL7PID,
		"ORC|NW|"+syntheticHL7Placer+"|"+syntheticHL7Filler,
		syntheticHL7OBR("", syntheticHL7Filler, ""))
	orderID := Identifier{System: "urn:test:local-order", Value: "test-013"}
	got, err := OrderFromHL7(wire, orderID, syntheticHL7Crosswalk)
	if err != nil {
		t.Fatal(err)
	}
	want := Order{
		ID:         orderID,
		Patient:    syntheticHL7Patient("1980-01-02", "female"),
		References: syntheticHL7References(),
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("projection = %#v, want %#v", got, want)
	}
}

func TestHL7ResultUsesExplicitRevisionAndOriginalBytes(t *testing.T) {
	for _, tc := range []struct {
		wireStatus string
		status     Status
		control    string
		sequence   uint64
	}{
		{"F", StatusFinal, "message-90", 4},
		{"C", StatusCorrected, "message-11", 12},
		{"X", StatusWithdrawn, "message-01", 13},
	} {
		t.Run(tc.wireStatus, func(t *testing.T) {
			wire := syntheticHL7Wire("ORU^R01^ORU_R01", tc.control, syntheticHL7PID,
				syntheticHL7OBR(syntheticHL7Placer, syntheticHL7Filler, tc.wireStatus),
				"OBX|1|TX|NOTE^Report^LN||Synthetic observations.||||||"+tc.wireStatus)
			logicalID := Identifier{System: "urn:test:report", Value: "test-r"}
			got, err := ResultFromHL7(wire, "test-approved-peer", logicalID, tc.sequence, syntheticHL7Crosswalk)
			if err != nil {
				t.Fatal(err)
			}
			want := Result{
				Source: "test-approved-peer", ID: logicalID, Patient: syntheticHL7Patient("1980-01-02", "female"), Sequence: tc.sequence,
				RevisionID: tc.control, Status: tc.status, Digest: sha256.Sum256(wire), OrderReferences: syntheticHL7References(),
			}
			if !reflect.DeepEqual(got, want) {
				t.Fatalf("projection = %#v, want %#v", got, want)
			}
		})
	}
}

func TestHL7ResultPreservesBirthDatePrecision(t *testing.T) {
	for _, tc := range []struct {
		wireDate string
		want     string
	}{
		{"", ""},
		{"1980", "1980"},
		{"198003", "1980-03"},
		{"20000229", "2000-02-29"},
	} {
		t.Run(tc.wireDate, func(t *testing.T) {
			pid := strings.Replace(syntheticHL7PID, "19800102|F", tc.wireDate+"|U", 1)
			wire := syntheticHL7Wire("ORU^R01", "REV", pid,
				"ORC|RE|"+syntheticHL7Placer,
				syntheticHL7OBR(syntheticHL7Placer, syntheticHL7Filler, "C"))
			id := Identifier{System: "urn:test:report", Value: "test-r"}
			got, err := ResultFromHL7(wire, "test-approved-peer", id, 1, syntheticHL7Crosswalk)
			if err != nil || got.Patient != syntheticHL7Patient(tc.want, "") {
				t.Fatalf("patient = %#v, err = %v", got.Patient, err)
			}
		})
	}
}

func TestHL7ProjectionRefusesAmbiguousOrUnboundInput(t *testing.T) {
	obr := syntheticHL7OBR(syntheticHL7Placer, syntheticHL7Filler, "F")
	orc := "ORC|RE|" + syntheticHL7Placer + "|" + syntheticHL7Filler
	for _, tc := range []struct {
		name string
		wire []byte
	}{
		{"multiple PID", syntheticHL7Wire("ORU^R01", "SECRET-MSG", syntheticHL7PID, syntheticHL7PID, obr)},
		{"multiple OBR", syntheticHL7Wire("ORU^R01", "SECRET-MSG", syntheticHL7PID, obr, obr)},
		{"multiple ORC", syntheticHL7Wire("ORU^R01", "SECRET-MSG", syntheticHL7PID, orc, orc, obr)},
		{"before PID", syntheticHL7Wire("ORU^R01", "SECRET-MSG", obr, syntheticHL7PID)},
		{"ORC after OBR", syntheticHL7Wire("ORU^R01", "SECRET-MSG", syntheticHL7PID, obr, orc)},
		{"OBX before OBR", syntheticHL7Wire("ORU^R01", "SECRET-MSG", syntheticHL7PID, "OBX|1|TX|NOTE^Pending^LN", obr)},
		{"partial report", syntheticHL7Wire("ORU^R01", "SECRET-MSG", syntheticHL7PID, obr, "DSC|NEXT-PAGE")},
		{"continuation header", []byte(strings.Replace(string(syntheticHL7Wire("ORU^R01", "SECRET-MSG", syntheticHL7PID, obr)), "2.5.1\r", "2.5.1||BAD-PHI\r", 1))},
		{"conflicting ORC value", syntheticHL7Wire("ORU^R01", "SECRET-MSG", syntheticHL7PID, "ORC|RE|BAD-PHI^RIS^1.2.4^ISO", obr)},
		{"conflicting ORC issuer", syntheticHL7Wire("ORU^R01", "SECRET-MSG", syntheticHL7PID, "ORC|RE|ORD-7^RIS^1.2.9^ISO", obr)},
		{"ORC only reference", syntheticHL7Wire("ORU^R01", "SECRET-MSG", syntheticHL7PID, orc, syntheticHL7OBR("", syntheticHL7Filler, "F"))},
		{"unknown order authority", syntheticHL7Wire("ORU^R01", "SECRET-MSG", syntheticHL7PID, syntheticHL7OBR("ORD-7^OTHER", syntheticHL7Filler, "F"))},
		{"missing order authority", syntheticHL7Wire("ORU^R01", "SECRET-MSG", syntheticHL7PID, syntheticHL7OBR("BAD-PHI", syntheticHL7Filler, "F"))},
		{"truncated authority pair", syntheticHL7Wire("ORU^R01", "SECRET-MSG", syntheticHL7PID, syntheticHL7OBR("ORD-7^RIS^1.2.4", syntheticHL7Filler, "F"))},
		{"repeated order reference", syntheticHL7Wire("ORU^R01", "SECRET-MSG", syntheticHL7PID, syntheticHL7OBR(syntheticHL7Placer+"~BAD-PHI^RIS", syntheticHL7Filler, "F"))},
		{"missing patient authority", syntheticHL7Wire("ORU^R01", "SECRET-MSG", "PID|1||BAD-PHI", obr)},
		{"unknown patient authority", syntheticHL7Wire("ORU^R01", "SECRET-MSG", "PID|1||BAD-PHI^^^OTHER^PI", obr)},
		{"two mapped patient identifiers", syntheticHL7Wire("ORU^R01", "SECRET-MSG", "PID|1||BAD-PHI^^^HOSP&1.2.3&ISO^MR~TEST-P1^^^HOSP&1.2.3&ISO^MR", obr)},
		{"qualifiers outside crosswalk", syntheticHL7Wire("ORU^R01", "SECRET-MSG", "PID|1||BAD-PHI^^^HOSP&1.2.3&ISO^MR^^^19800101", obr)},
		{"invalid date", syntheticHL7Wire("ORU^R01", "SECRET-MSG", strings.Replace(syntheticHL7PID, "19800102", "19800230", 1), obr)},
		{"wrong structure", syntheticHL7Wire("ORU^R01^ORU_R99", "SECRET-MSG", syntheticHL7PID, obr)},
		{"repeated message type", syntheticHL7Wire("ORU^R01~ORM^O01", "SECRET-MSG", syntheticHL7PID, obr)},
		{"repeated control ID", syntheticHL7Wire("ORU^R01", "SECRET-MSG~BAD-PHI", syntheticHL7PID, obr)},
		{"missing control ID", syntheticHL7Wire("ORU^R01", "", syntheticHL7PID, obr)},
		{"unpublishable result status", syntheticHL7Wire("ORU^R01", "SECRET-MSG", syntheticHL7PID, syntheticHL7OBR(syntheticHL7Placer, syntheticHL7Filler, "P"))},
		{"no order ID despite local fields", syntheticHL7Wire("ORU^R01", "SECRET-MSG", syntheticHL7PID, syntheticHL7OBR("", "", "F"))},
		{"oversized", make([]byte, maxHL7WireBytes+1)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			id := Identifier{System: "urn:test:report", Value: "test-r"}
			got, err := ResultFromHL7(tc.wire, "test-approved-peer", id, 2, syntheticHL7Crosswalk)
			if !errors.Is(err, ErrInvalidInput) || !reflect.DeepEqual(got, Result{}) {
				t.Fatalf("projection = %#v, err = %v", got, err)
			}
			for _, private := range []string{"BAD-PHI", "SECRET-MSG", "TEST-P1", "19800230"} {
				if strings.Contains(err.Error(), private) {
					t.Fatalf("diagnostic exposed an input value: %v", err)
				}
			}
		})
	}
}

func TestHL7OrderRefusesNonCreationControl(t *testing.T) {
	wire := syntheticHL7Wire("ORM^O01^ORM_O01", "ORDER-MESSAGE", syntheticHL7PID,
		"ORC|CA|"+syntheticHL7Placer+"|"+syntheticHL7Filler,
		syntheticHL7OBR(syntheticHL7Placer, syntheticHL7Filler, ""))
	id := Identifier{System: "urn:test:local-order", Value: "test-013"}
	got, err := OrderFromHL7(wire, id, syntheticHL7Crosswalk)
	if !errors.Is(err, ErrInvalidInput) || !reflect.DeepEqual(got, Order{}) {
		t.Fatalf("projection = %#v, err = %v", got, err)
	}
}

func syntheticHL7Wire(messageType, controlID string, segments ...string) []byte {
	header := "MSH|^~\\&|SENDER|HOSP|RECEIVER|DEPT|20201101010101||" + messageType + "|" + controlID + "|P|2.5.1"
	return []byte(strings.Join(append([]string{header}, segments...), "\r") + "\r")
}

func syntheticHL7OBR(placer, filler, status string) string {
	fields := make([]string, 26)
	fields[0], fields[1], fields[2], fields[3] = "OBR", "1", placer, filler
	fields[4] = "XR^Synthetic order^L"
	fields[18] = "NOT-AN-ACCESSION"
	fields[20] = "NOT-AN-ISSUER"
	fields[25] = status
	return strings.Join(fields, "|")
}

func syntheticHL7Crosswalk(authority hl7.HierarchicDesignator, role HL7IdentifierRole, kind string) (string, bool) {
	switch {
	case role == HL7PatientIdentifier && kind == "MR" && authority == (hl7.HierarchicDesignator{NamespaceID: "HOSP", UniversalID: "1.2.3", UniversalIDType: "ISO"}):
		return "urn:test:patient", true
	case role == HL7PlacerOrder && kind == "" && authority == (hl7.HierarchicDesignator{NamespaceID: "RIS", UniversalID: "1.2.4", UniversalIDType: "ISO"}):
		return "urn:test:placer", true
	case role == HL7FillerOrder && kind == "" && authority == (hl7.HierarchicDesignator{NamespaceID: "PACS"}):
		return "urn:test:filler", true
	default:
		return "", false
	}
}

func syntheticHL7Patient(date, sex string) Patient {
	return Patient{ID: Identifier{System: "urn:test:patient", Value: "TEST-P1"}, BirthDate: date, Sex: sex}
}

func syntheticHL7References() []OrderReference {
	return []OrderReference{
		{Kind: RefPlacer, ID: Identifier{System: "urn:test:placer", Value: "ORD-7"}},
		{Kind: RefFiller, ID: Identifier{System: "urn:test:filler", Value: "READ-7"}},
	}
}
