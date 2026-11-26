package clinicalresult

import (
	"errors"
	"strings"
	"testing"

	"github.com/ThalesMMS/dicom-go/hl7"
)

func TestAssociateHL7DiagnosticRevisionWithFHIRServiceRequest(t *testing.T) {
	const (
		patientSystem = "urn:example:patient"
		placerSystem  = "urn:example:placer"
		fillerSystem  = "urn:example:filler"
	)
	p1 := Patient{ID: Identifier{patientSystem, "PAT-1"}, BirthDate: "1980-06-07", Sex: "female"}
	p2 := Patient{ID: Identifier{patientSystem, "PAT-2"}, BirthDate: "1981-06-07", Sex: "female"}
	fhirOrder := func(logicalID, localPatientID string, patient Patient) Order {
		t.Helper()
		order, err := OrderFromFHIR([]byte(`{
			"resourceType":"ServiceRequest",
			"id":"`+logicalID+`",
			"status":"active",
			"intent":"order",
			"subject":{"reference":"Patient/`+localPatientID+`"},
			"identifier":[
				{"system":"`+placerSystem+`","value":"order-42"},
				{"system":"`+fillerSystem+`","value":"result-43"}
			]
		}`), FHIRBinding{
			BaseURL:          "https://example.invalid/hospital-a/fhir",
			PatientLogicalID: localPatientID,
			Patient:          patient,
			OrderIdentifierKind: func(id Identifier) (ReferenceKind, bool) {
				switch id.System {
				case placerSystem:
					return RefPlacer, true
				case fillerSystem:
					return RefFiller, true
				default:
					return "", false
				}
			},
		})
		if err != nil {
			t.Fatal(err)
		}
		return order
	}
	p1Order, p2Order := fhirOrder("order-p1", "p1", p1), fhirOrder("order-p2", "p2", p2)
	candidates := []Order{p1Order, p2Order}
	hl7Report := func(deliveryID string, sequence uint64, patient Patient, status string, finding string) Result {
		t.Helper()
		orderNumbers := "order-42^RIS-A|result-43^LAB-A"
		obr := make([]string, 26)
		obr[0], obr[1], obr[2], obr[3] = "OBR", "1", "order-42^RIS-A", "result-43^LAB-A"
		obr[4], obr[25] = "18782-3^Radiology Study observation^LN", status
		wire := []byte(`MSH|^~\&|LAB|SITE|APP|SITE|20261001120000||ORU^R01^ORU_R01|` + deliveryID + "|P|2.5.1\r" +
			"PID|1||" + patient.ID.Value + "^^^MRN-A^MR||Synthetic^Test||" + strings.ReplaceAll(patient.BirthDate, "-", "") + "|F\r" +
			"ORC|RE|" + orderNumbers + "\r" + strings.Join(obr, "|") + "\r" +
			"OBX|1|TX|18782-3^Finding^LN||" + finding + "||||||" + status + "\r")
		result, err := ResultFromHL7(wire, "authenticated-lab", Identifier{"urn:example:report", "shared-battery"}, sequence,
			func(authority hl7.HierarchicDesignator, role HL7IdentifierRole, identifierType string) (string, bool) {
				switch {
				case authority.NamespaceID == "MRN-A" && role == HL7PatientIdentifier && identifierType == "MR":
					return patientSystem, true
				case authority.NamespaceID == "RIS-A" && role == HL7PlacerOrder:
					return placerSystem, true
				case authority.NamespaceID == "LAB-A" && role == HL7FillerOrder:
					return fillerSystem, true
				default:
					return "", false
				}
			})
		if err != nil {
			t.Fatal(err)
		}
		return result
	}
	ledger := testLedger(t, 4)
	p1Initial := hl7Report("p1-first", 10, p1, "F", "accepted")
	p2Initial := hl7Report("p2-first", 90, p2, "F", "different patient")
	for _, tt := range []struct {
		result Result
		order  Order
	}{{p1Initial, p1Order}, {p2Initial, p2Order}} {
		got, err := ledger.Apply(tt.result.Patient, tt.result, candidates)
		if err != nil || got.Current.OrderID != tt.order.ID || got.Disposition != Created {
			t.Fatalf("cross-protocol association = %+v, %v", got, err)
		}
	}
	p1Correction := hl7Report("p1-correction", 12, p1, "C", "revised")
	if got, err := ledger.Apply(p2, p1Correction, candidates); !errors.Is(err, ErrPatientScope) || got != (Decision{}) {
		t.Fatalf("foreign correction = %+v, %v", got, err)
	}
	if got, err := ledger.Apply(p1, p1Correction, candidates); err != nil || got.Disposition != Replaced ||
		got.Previous.ID != p1Initial.RevisionID || got.Current.OrderID != p1Order.ID {
		t.Fatalf("own correction = %+v, %v", got, err)
	}
	p1Stale := hl7Report("p1-delayed", 11, p1, "C", "superseded")
	if got, err := ledger.Apply(p1, p1Stale, candidates); err != nil || got.Disposition != Stale ||
		got.Current.Revision.Digest != p1Correction.Digest {
		t.Fatalf("late revision = %+v, %v", got, err)
	}
	if got, exists, err := ledger.Lookup(p2, p2Initial.Source, p2Initial.ID); err != nil || !exists ||
		got.OrderID != p2Order.ID || got.Revision.Digest != p2Initial.Digest {
		t.Fatalf("neighboring patient's unchanged revision = %+v, %t, %v", got, exists, err)
	}
}

func TestAssociateFHIRDiagnosticReportWithHL7Order(t *testing.T) {
	const (
		patientSystem = "urn:example:local:patient"
		fillerSystem  = "urn:example:local:filler"
	)
	hl7Order := []byte(`MSH|^~\&|RIS|S|APP|S|20261001120000||ORM^O01^ORM_O01|order-1|P|2.5.1` + "\r" +
		"PID|1||P-1^^^HOSP^MR||Synthetic^Test||19850404|M\rORC|NW||order-42^RIS\r" +
		"OBR|1||order-42^RIS|18782-3^Radiology Study observation^LN\r")
	order, err := OrderFromHL7(hl7Order, Identifier{"urn:example:ris", "local-order"},
		func(authority hl7.HierarchicDesignator, role HL7IdentifierRole, idType string) (string, bool) {
			switch {
			case authority.NamespaceID == "HOSP" && role == HL7PatientIdentifier && idType == "MR":
				return patientSystem, true
			case authority.NamespaceID == "RIS" && role == HL7FillerOrder:
				return fillerSystem, true
			default:
				return "", false
			}
		})
	if err != nil {
		t.Fatal(err)
	}
	binding := FHIRBinding{
		BaseURL: "https://example.invalid/tenant-a/fhir", PatientLogicalID: "rel-p1", Patient: order.Patient,
		OrderIdentifierKind: func(id Identifier) (ReferenceKind, bool) {
			return RefFiller, id.System == fillerSystem
		},
	}
	readReport := func(version, status string, seq uint64) Result {
		t.Helper()
		report := []byte(`{"resourceType":"DiagnosticReport","id":"report-1","status":"` + status +
			`","meta":{"versionId":"` + version +
			`"},"subject":{"reference":"Patient/rel-p1"},"basedOn":[{"type":"ServiceRequest","identifier":` +
			`{"system":"` + fillerSystem + `","value":"order-42"}}],"code":{"text":"Synthetic control"}}`)
		result, err := ResultFromFHIR(report, binding, seq)
		if err != nil {
			t.Fatal(err)
		}
		return result
	}
	ledger := testLedger(t, 1)
	original := readReport("z9", "final", 1)
	if got, err := ledger.Apply(binding.Patient, original, []Order{order}); err != nil ||
		got.Disposition != Created || got.Current.OrderID != order.ID {
		t.Fatalf("FHIR result association = %+v, %v", got, err)
	}
	correction := readReport("a1", "corrected", 2)
	if got, err := ledger.Apply(binding.Patient, correction, []Order{order}); err != nil ||
		got.Disposition != Replaced || got.Previous.ID != "z9" || got.Current.Revision.ID != "a1" {
		t.Fatalf("FHIR correction order = %+v, %v", got, err)
	}
}
