package clinicalresult

import (
	"bytes"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"io"
	"net/url"
	"strings"
	"unicode"
	"unicode/utf8"
)

const (
	maxFHIRProjectionBytes  = 1 << 20
	maxFHIRProjectionDepth  = 32
	maxFHIROrderIdentifiers = 32
	fhirStructureBase       = "http://hl7.org/fhir/StructureDefinition/"
)

// FHIRBinding ties a resource from an authenticated FHIR R4 source to the
// patient's identity already verified by the application. BaseURL includes
// the tenant path. Absolute references must use that exact base.
//
// OrderIdentifierKind optionally classifies business identifiers known to
// the application as RefPlacer, RefFiller, or RefAccession. Returning false
// omits an identifier on a ServiceRequest and refuses it in a DiagnosticReport
// basedOn reference. This callback should use the assigning system, rather
// than an identifier's bare value or an unverified type in the payload.
type FHIRBinding struct {
	BaseURL             string
	PatientLogicalID    string
	Patient             Patient
	OrderIdentifierKind func(Identifier) (ReferenceKind, bool)
}

type fhirAssociationReference struct {
	Reference         string                     `json:"reference"`
	Type              string                     `json:"type"`
	Identifier        *fhirAssociationIdentifier `json:"identifier"`
	ModifierExtension []json.RawMessage          `json:"modifierExtension"`
}

type fhirAssociationIdentifier struct {
	System string          `json:"system"`
	Value  string          `json:"value"`
	Use    string          `json:"use"`
	Period json.RawMessage `json:"period"`
}

func (id fhirAssociationIdentifier) currentIdentifier() (Identifier, bool) {
	identifier := Identifier{System: id.System, Value: id.Value}
	switch id.Use {
	case "", "usual", "official", "secondary":
		return identifier, len(id.Period) == 0 && identifierValid(identifier) &&
			!strings.ContainsRune(id.System, utf8.RuneError) && !strings.ContainsRune(id.Value, utf8.RuneError)
	default:
		return Identifier{}, false
	}
}

type fhirAssociationResource struct {
	ResourceType      string                      `json:"resourceType"`
	ID                string                      `json:"id"`
	Status            string                      `json:"status"`
	Intent            string                      `json:"intent"`
	DoNotPerform      json.RawMessage             `json:"doNotPerform"`
	NegationExtension json.RawMessage             `json:"_doNotPerform"`
	Subject           fhirAssociationReference    `json:"subject"`
	Identifier        []fhirAssociationIdentifier `json:"identifier"`
	BasedOn           []fhirAssociationReference  `json:"basedOn"`
	ModifierExtension []json.RawMessage           `json:"modifierExtension"`
	Meta              struct {
		VersionID string `json:"versionId"`
	} `json:"meta"`
}

// OrderFromFHIR projects one active or completed ServiceRequest order with a
// bound Patient subject. The resource's logical ID and recognized business
// identifiers can both match a report. The projection issues no network requests.
func OrderFromFHIR(wire []byte, binding FHIRBinding) (Order, error) {
	resource, base, err := readFHIRAssociation(wire, binding, "ServiceRequest")
	if err != nil {
		return Order{}, err
	}
	switch resource.Status {
	case "active", "completed":
	default:
		return Order{}, fhirProjectionError("ServiceRequest status")
	}
	switch resource.Intent {
	case "order", "original-order", "reflex-order", "filler-order", "instance-order":
	default:
		return Order{}, fhirProjectionError("ServiceRequest intent")
	}
	if len(resource.NegationExtension) != 0 ||
		(len(resource.DoNotPerform) != 0 && !bytes.Equal(bytes.TrimSpace(resource.DoNotPerform), []byte("false"))) ||
		len(resource.Identifier) > maxFHIROrderIdentifiers {
		return Order{}, fhirProjectionError("ServiceRequest identity or intent")
	}

	id := Identifier{System: base + "/ServiceRequest", Value: resource.ID}
	order := Order{ID: id, Patient: binding.Patient, References: []OrderReference{{Kind: RefServiceRequest, ID: id}}}
	for _, identifier := range resource.Identifier {
		if reference, ok := binding.classifiedFHIRIdentifier(identifier); ok {
			order.References = append(order.References, reference)
		}
	}
	if !orderValid(order) {
		return Order{}, fhirProjectionError("ServiceRequest identity")
	}
	return order, nil
}

// ResultFromFHIR projects a DiagnosticReport with one ServiceRequest basedOn
// and a bound Patient subject. Its exact JSON bytes supply Digest. The caller
// supplies a verified monotonic sequence from the publishing source.
// meta.versionId is opaque, and issued does not determine chronology.
func ResultFromFHIR(wire []byte, binding FHIRBinding, sequence uint64) (Result, error) {
	resource, base, err := readFHIRAssociation(wire, binding, "DiagnosticReport")
	if err != nil {
		return Result{}, err
	}
	if len(resource.BasedOn) != 1 || !validFHIRID(resource.Meta.VersionID) {
		return Result{}, fhirProjectionError("DiagnosticReport order or revision")
	}
	order, err := binding.fhirOrderReference(base, resource.BasedOn[0])
	if err != nil {
		return Result{}, err
	}
	var status Status
	switch resource.Status {
	case "final":
		status = StatusFinal
	case "amended", "corrected", "appended":
		status = StatusCorrected
	case "cancelled", "entered-in-error":
		status = StatusWithdrawn
	default:
		return Result{}, fhirProjectionError("DiagnosticReport status")
	}
	result := Result{
		Source: base, ID: Identifier{System: base + "/DiagnosticReport", Value: resource.ID},
		Patient: binding.Patient, OrderReferences: order, Sequence: sequence,
		RevisionID: resource.Meta.VersionID, Status: status, Digest: sha256.Sum256(wire),
	}
	if !resultValid(result) {
		return Result{}, fhirProjectionError("DiagnosticReport identity or revision")
	}
	return result, nil
}

func (binding FHIRBinding) classifiedFHIRIdentifier(raw fhirAssociationIdentifier) (OrderReference, bool) {
	id, valid := raw.currentIdentifier()
	if binding.OrderIdentifierKind == nil || !valid {
		return OrderReference{}, false
	}
	kind, ok := binding.OrderIdentifierKind(id)
	if !ok || (kind != RefPlacer && kind != RefFiller && kind != RefAccession) {
		return OrderReference{}, false
	}
	return OrderReference{Kind: kind, ID: id}, true
}

func (binding FHIRBinding) fhirOrderReference(base string, ref fhirAssociationReference) ([]OrderReference, error) {
	if !ref.validType("ServiceRequest") || (ref.Reference == "" && ref.Type == "") {
		return nil, fhirProjectionError("DiagnosticReport order type")
	}
	var references []OrderReference
	if ref.Reference != "" {
		id, ok := fhirLiteralID(base, "ServiceRequest", ref.Reference)
		if !ok {
			return nil, fhirProjectionError("DiagnosticReport order scope")
		}
		references = append(references, OrderReference{Kind: RefServiceRequest, ID: Identifier{System: base + "/ServiceRequest", Value: id}})
	}
	if ref.Identifier != nil {
		reference, ok := binding.classifiedFHIRIdentifier(*ref.Identifier)
		if !ok {
			return nil, fhirProjectionError("DiagnosticReport order identifier")
		}
		references = append(references, reference)
	}
	if len(references) == 0 {
		return nil, fhirProjectionError("DiagnosticReport order reference")
	}
	return references, nil
}

func (ref fhirAssociationReference) validType(kind string) bool {
	return len(ref.ModifierExtension) == 0 && (ref.Type == "" || ref.Type == kind || ref.Type == fhirStructureBase+kind)
}

func readFHIRAssociation(wire []byte, binding FHIRBinding, expectedType string) (fhirAssociationResource, string, error) {
	base, ok := fhirAssociationBase(binding.BaseURL)
	if !ok || !validFHIRID(binding.PatientLogicalID) || !patientValid(binding.Patient) {
		return fhirAssociationResource{}, "", fhirProjectionError("patient binding")
	}
	var resource fhirAssociationResource
	if len(wire) > maxFHIRProjectionBytes || !utf8.Valid(wire) ||
		!validFHIRAssociationJSON(wire) || json.Unmarshal(wire, &resource) != nil {
		return fhirAssociationResource{}, "", fhirProjectionError("resource encoding")
	}
	if resource.ResourceType != expectedType || !validFHIRID(resource.ID) || len(resource.ModifierExtension) != 0 {
		return fhirAssociationResource{}, "", fhirProjectionError("resource identity")
	}
	subject := resource.Subject
	id, ok := fhirLiteralID(base, "Patient", subject.Reference)
	if !ok || id != binding.PatientLogicalID || !subject.validType("Patient") {
		return fhirAssociationResource{}, "", fhirProjectionError("patient subject")
	}
	if subject.Identifier != nil {
		id, ok := subject.Identifier.currentIdentifier()
		if !ok || id != binding.Patient.ID {
			return fhirAssociationResource{}, "", fhirProjectionError("patient identifier")
		}
	}
	return resource, base, nil
}

func fhirAssociationBase(raw string) (string, bool) {
	if strings.ContainsAny(raw, "?#\\") {
		return "", false
	}
	base, err := url.Parse(raw)
	if err != nil || (base.Scheme != "https" && base.Scheme != "http") ||
		base.User != nil || base.Hostname() == "" || base.Opaque != "" ||
		base.RawPath != "" || base.EscapedPath() != base.Path {
		return "", false
	}
	path := strings.TrimSuffix(base.Path, "/")
	if strings.Contains(path, "//") || strings.HasSuffix(path, "/") {
		return "", false
	}
	for _, segment := range strings.Split(path, "/") {
		if segment == "." || segment == ".." {
			return "", false
		}
	}
	result := strings.TrimSuffix(raw, "/")
	return result, identifierValid(Identifier{System: result + "/DiagnosticReport", Value: "id"})
}

func fhirLiteralID(base, kind, reference string) (string, bool) {
	ref := strings.TrimPrefix(reference, base+"/")
	id, ok := strings.CutPrefix(ref, kind+"/")
	return id, ok && validFHIRID(id)
}

func validFHIRID(id string) bool {
	if len(id) == 0 || len(id) > 64 || id == "." || id == ".." {
		return false
	}
	for _, c := range id {
		if !((c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z') || (c >= '0' && c <= '9') || c == '-' || c == '.') {
			return false
		}
	}
	return true
}

func fhirProjectionError(field string) error {
	return fmt.Errorf("%w: FHIR %s", ErrInvalidInput, field)
}

// validFHIRAssociationJSON refuses duplicate members and excessive nesting
// before unmarshalling. Folded keys also cover encoding/json's case-insensitive
// matching of struct tags.
func validFHIRAssociationJSON(wire []byte) bool {
	reader := json.NewDecoder(bytes.NewReader(wire))
	reader.UseNumber()
	start, err := reader.Token()
	if err != nil || start != json.Delim('{') || !fhirContainer(reader, '{', 1) {
		return false
	}
	_, err = reader.Token()
	return err == io.EOF
}

var fhirProjectionProperties = map[string]string{
	"resourcetype":      "resourceType",
	"id":                "id",
	"status":            "status",
	"intent":            "intent",
	"donotperform":      "doNotPerform",
	"_donotperform":     "_doNotPerform",
	"subject":           "subject",
	"basedon":           "basedOn",
	"meta":              "meta",
	"versionid":         "versionId",
	"reference":         "reference",
	"type":              "type",
	"identifier":        "identifier",
	"system":            "system",
	"value":             "value",
	"use":               "use",
	"period":            "period",
	"modifierextension": "modifierExtension",
}

func fhirContainer(reader *json.Decoder, start json.Delim, depth int) bool {
	if depth > maxFHIRProjectionDepth {
		return false
	}
	var keys map[string]struct{}
	if start == '{' {
		keys = make(map[string]struct{})
	}
	for reader.More() {
		if keys != nil {
			token, err := reader.Token()
			key, ok := token.(string)
			if err != nil || !ok {
				return false
			}
			if strings.IndexFunc(key, func(r rune) bool { return r > unicode.MaxASCII }) >= 0 {
				return false
			}
			folded := strings.ToLower(key)
			if canonical, known := fhirProjectionProperties[folded]; known && canonical != key {
				return false
			}
			if _, seen := keys[folded]; seen {
				return false
			}
			keys[folded] = struct{}{}
		}
		token, err := reader.Token()
		if err != nil {
			return false
		}
		if delim, ok := token.(json.Delim); ok && !fhirContainer(reader, delim, depth+1) {
			return false
		}
	}
	_, err := reader.Token()
	return err == nil
}
