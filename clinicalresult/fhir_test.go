package clinicalresult

import (
	"crypto/sha256"
	"errors"
	"reflect"
	"strings"
	"testing"
)

const fhirFixtureBase = "https://records.example.test/fhir/site-a"

func fhirFixtureBinding() FHIRBinding {
	return FHIRBinding{
		BaseURL: fhirFixtureBase + "/", PatientLogicalID: "p1",
		Patient: Patient{ID: Identifier{System: "urn:site-a:mrn", Value: "MRN-1"}, BirthDate: "1980-01-02", Sex: "female"},
		OrderIdentifierKind: func(id Identifier) (ReferenceKind, bool) {
			switch id.System {
			case "urn:site-a:placer":
				return RefPlacer, true
			case "urn:site-a:filler":
				return RefFiller, true
			default:
				return "", false
			}
		},
	}
}

const fhirFixtureOrder = `{
	"resourceType":"ServiceRequest","id":"order1","status":"active","intent":"order",
	"subject":{"reference":"Patient/p1","type":"Patient","identifier":{"system":"urn:site-a:mrn","value":"MRN-1"}},
	"identifier":[{"system":"urn:site-a:placer","value":"O-1"},{"system":"urn:site-a:filler","value":"F-1","use":"official"},
	{"system":"urn:site-b:placer","value":"O-1"},{"system":"urn:site-a:placer","value":"previous","use":"old"}]
}`

const fhirFixtureReport = `{
	"resourceType":"DiagnosticReport","id":"report-1","meta":{"versionId":"z.2"},
	"status":"final","issued":"2025-02-17T18:00:00Z",
	"subject":{"reference":"Patient/p1"},
	"basedOn":[{"reference":"ServiceRequest/order1","type":"ServiceRequest","identifier":{"system":"urn:site-a:filler","value":"F-1"}}]
}`

func TestFHIRAssociationProjectionKeepsLogicalAndBusinessReferences(t *testing.T) {
	binding := fhirFixtureBinding()
	order, err := OrderFromFHIR([]byte(fhirFixtureOrder), binding)
	if err != nil {
		t.Fatal(err)
	}
	orderID := Identifier{System: fhirFixtureBase + "/ServiceRequest", Value: "order1"}
	logicalRef := OrderReference{Kind: RefServiceRequest, ID: orderID}
	placerRef := OrderReference{Kind: RefPlacer, ID: Identifier{System: "urn:site-a:placer", Value: "O-1"}}
	fillerRef := OrderReference{Kind: RefFiller, ID: Identifier{System: "urn:site-a:filler", Value: "F-1"}}
	wantOrder := Order{ID: orderID, Patient: binding.Patient, References: []OrderReference{logicalRef, placerRef, fillerRef}}
	if !reflect.DeepEqual(order, wantOrder) {
		t.Fatalf("order = %#v; want %#v", order, wantOrder)
	}

	for _, absolute := range []bool{false, true} {
		wire := []byte(fhirFixtureReport)
		if absolute {
			wire = []byte(strings.NewReplacer(
				"\"Patient/p1\"", "\""+fhirFixtureBase+"/Patient/p1\"",
				"\"ServiceRequest/order1\"", "\""+fhirFixtureBase+"/ServiceRequest/order1\"",
			).Replace(fhirFixtureReport))
		}
		report, err := ResultFromFHIR(wire, binding, 71)
		if err != nil {
			t.Fatal(err)
		}
		wantReport := Result{
			Source: fhirFixtureBase, ID: Identifier{System: fhirFixtureBase + "/DiagnosticReport", Value: "report-1"},
			Patient: binding.Patient, OrderReferences: []OrderReference{logicalRef, fillerRef}, Sequence: 71,
			RevisionID: "z.2", Status: StatusFinal, Digest: sha256.Sum256(wire),
		}
		if !reflect.DeepEqual(report, wantReport) {
			t.Errorf("result = %#v; want %#v", report, wantReport)
		}
	}

	wire := []byte(strings.Replace(fhirFixtureReport, `"reference":"ServiceRequest/order1",`, "", 1))
	report, err := ResultFromFHIR(wire, binding, 71)
	if err != nil || !reflect.DeepEqual(report.OrderReferences, []OrderReference{fillerRef}) {
		t.Fatalf("result from the explicit business identifier = %#v, %v", report, err)
	}
	untyped := []byte(strings.Replace(string(wire), `"type":"ServiceRequest",`, "", 1))
	if _, err := ResultFromFHIR(untyped, binding, 71); !errors.Is(err, ErrInvalidInput) {
		t.Fatalf("identifier-only basedOn requires explicit ServiceRequest type: %v", err)
	}
}

func TestFHIRAssociationProjectionRequiresBoundPatientAndOrder(t *testing.T) {
	for _, tc := range []struct {
		name, old, replacement string
	}{
		{"other patient", "Patient/p1", "Patient/secret-other"},
		{"foreign origin", "Patient/p1", "https://other.example.test/fhir/site-a/Patient/p1"},
		{"other tenant", "Patient/p1", "https://records.example.test/fhir/site-b/Patient/p1"},
		{"tenant prefix", "Patient/p1", "https://records.example.test/fhir/site-a2/Patient/p1"},
		{"absolute path", "Patient/p1", "/fhir/site-a/Patient/p1"},
		{"encoded path", "Patient/p1", "https://records.example.test/fhir/%73ite-a/Patient/p1"},
		{"contained", "Patient/p1", "#secret-p1"},
		{"history", "Patient/p1", "Patient/p1/_history/7"},
		{"fragment", "Patient/p1", "Patient/p1#x"},
		{"wrong type", `"reference":"Patient/p1"`, `"reference":"Patient/p1","type":"Group"`},
		{"conflicting subject identifier", `"reference":"Patient/p1"`, `"reference":"Patient/p1","identifier":{"system":"urn:site-a:mrn","value":"secret-other"}`},
		{"missing patient literal", `"reference":"Patient/p1"`, `"identifier":{"system":"urn:site-a:mrn","value":"MRN-1"}`},
		{"expired alias", `"value":"F-1"`, `"value":"F-1","period":{"end":"2024-01-01"}`},
		{"retired alias", `"value":"F-1"`, `"value":"F-1","use":"old"`},
		{"untrusted order system", "urn:site-a:filler", "urn:site-b:filler"},
		{"ambiguous basedOn", `"basedOn":[{`, `"basedOn":[{"reference":"ServiceRequest/secret-other"},{`},
		{"other order tenant", "ServiceRequest/order1", "https://records.example.test/fhir/site-b/ServiceRequest/order1"},
		{"other order type", `"type":"ServiceRequest"`, `"type":"CarePlan"`},
		{"non-order reference", "ServiceRequest/order1", "CarePlan/order1"},
		{"modifier", `"status":"final"`, `"status":"final","modifierExtension":[{"url":"urn:secret:modifier","valueBoolean":true}]`},
		{"reference modifier", `"reference":"Patient/p1"`, `"reference":"Patient/p1","modifierExtension":[{"url":"urn:secret:modifier","valueBoolean":true}]`},
		{"duplicate patient key", `"reference":"Patient/p1"`, `"reference":"Patient/secret-other","reference":"Patient/p1"`},
		{"case variant patient key", `"reference":"Patient/p1"`, `"Reference":"Patient/p1"`},
		{"case duplicate patient key", `"reference":"Patient/p1"`, `"Reference":"Patient/secret-other","reference":"Patient/p1"`},
		{"unicode-fold duplicate patient key", `"subject":{`, `"ſubject":{"reference":"Patient/secret-other"},"subject":{`},
		{"missing revision", "z.2", ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			wire := []byte(strings.Replace(fhirFixtureReport, tc.old, tc.replacement, 1))
			result, err := ResultFromFHIR(wire, fhirFixtureBinding(), 1)
			if !errors.Is(err, ErrInvalidInput) || strings.Contains(err.Error(), "secret") || !reflect.DeepEqual(result, Result{}) {
				t.Fatalf("result = %#v, error = %v", result, err)
			}
		})
	}
}

func TestFHIRAssociationProjectionUsesExplicitRevisionAndStatus(t *testing.T) {
	for _, tc := range []struct {
		input string
		want  Status
	}{
		{"final", StatusFinal},
		{"corrected", StatusCorrected},
		{"amended", StatusCorrected},
		{"appended", StatusCorrected},
		{"cancelled", StatusWithdrawn},
		{"entered-in-error", StatusWithdrawn},
		{"registered", ""},
		{"partial", ""},
		{"preliminary", ""},
		{"unknown", ""},
	} {
		t.Run(tc.input, func(t *testing.T) {
			wire := []byte(strings.NewReplacer(
				"\"final\"", "\""+tc.input+"\"",
				"\"z.2\"", "\"a-opaque\"",
			).Replace(fhirFixtureReport))
			result, err := ResultFromFHIR(wire, fhirFixtureBinding(), 1234)
			if tc.want == "" {
				if !errors.Is(err, ErrInvalidInput) {
					t.Fatalf("result = %#v, error = %v", result, err)
				}
				return
			}
			if err != nil || result.Status != tc.want || result.Sequence != 1234 || result.RevisionID != "a-opaque" {
				t.Fatalf("result = %#v, error = %v", result, err)
			}
		})
	}
	if _, err := ResultFromFHIR([]byte(fhirFixtureReport), fhirFixtureBinding(), 0); !errors.Is(err, ErrInvalidInput) {
		t.Fatalf("missing explicit sequence: %v", err)
	}
}

func TestFHIRAssociationProjectionRejectsInapplicableOrderAndExcessInput(t *testing.T) {
	for _, tc := range []struct{ old, replacement string }{
		{`"status":"active"`, `"status":"revoked"`},
		{`"status":"active"`, `"status":"on-hold"`},
		{`"intent":"order"`, `"intent":"proposal"`},
		{`"intent":"order"`, `"intent":"order","doNotPerform":true`},
		{`"intent":"order"`, `"intent":"order","doNotPerform":null`},
		{`"intent":"order"`, `"intent":"order","_doNotPerform":{"extension":[{"url":"http://hl7.org/fhir/StructureDefinition/data-absent-reason","valueCode":"unknown"}]}`},
		{`"reference":"Patient/p1"`, `"reference":"Patient/p2"`},
		{`"system":"urn:site-a:mrn"`, `"system":"urn:site-b:mrn"`},
	} {
		wire := []byte(strings.Replace(fhirFixtureOrder, tc.old, tc.replacement, 1))
		if _, err := OrderFromFHIR(wire, fhirFixtureBinding()); !errors.Is(err, ErrInvalidInput) {
			t.Errorf("replaced %s: %v", tc.old, err)
		}
	}
	falseModifier := strings.Replace(fhirFixtureOrder, `"intent":"order"`, `"intent":"order","doNotPerform":false`, 1)
	if _, err := OrderFromFHIR([]byte(falseModifier), fhirFixtureBinding()); err != nil {
		t.Fatalf("known order without negation: %v", err)
	}
	wire := fhirFixtureOrder + strings.Repeat(" ", maxFHIRProjectionBytes-len(fhirFixtureOrder))
	if _, err := OrderFromFHIR([]byte(wire), fhirFixtureBinding()); err != nil {
		t.Fatalf("at the document limit: %v", err)
	}
	if _, err := OrderFromFHIR([]byte(wire+" "), fhirFixtureBinding()); !errors.Is(err, ErrInvalidInput) {
		t.Fatalf("above the document limit: %v", err)
	}
	deep := strings.TrimSuffix(fhirFixtureOrder, "}") + `,"ignored":` +
		strings.Repeat("[", maxFHIRProjectionDepth) + "0" + strings.Repeat("]", maxFHIRProjectionDepth) + "}"
	for _, malformed := range []string{deep, fhirFixtureOrder + " {}", `[]`, `{"id":"` + string([]byte{0xff}) + `"}`} {
		if _, err := OrderFromFHIR([]byte(malformed), fhirFixtureBinding()); !errors.Is(err, ErrInvalidInput) {
			t.Errorf("invalid JSON admitted: %v", err)
		}
	}
}

func TestFHIRAssociationProjectionRejectsAmbiguousSourceBinding(t *testing.T) {
	for _, source := range []string{
		"/fhir/site-a", "javascript://records.example.test/fhir/site-a",
		"https://user@records.example.test/fhir/site-a",
		"https://records.example.test/fhir/site-a?",
		"https://records.example.test/fhir/site-a#",
		"https://records.example.test/fhir/site-a/../site-a",
		"https://records.example.test/fhir//site-a",
		"https://records.example.test/fhir/%73ite-a",
	} {
		binding := fhirFixtureBinding()
		binding.BaseURL = source
		if _, err := ResultFromFHIR([]byte(fhirFixtureReport), binding, 99); !errors.Is(err, ErrInvalidInput) {
			t.Errorf("base %q: %v", source, err)
		}
	}
}
