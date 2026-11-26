package xdsi

import (
	"crypto/sha1"
	"encoding/hex"
	"strconv"
	"strings"
	"time"

	"github.com/ThalesMMS/dicom-go/core"
	"github.com/ThalesMMS/dicom-go/sr"
)

// Code supplies an affinity-domain code and its display name.
type Code struct{ Value, Scheme, Meaning string }

func (c Code) valid() bool {
	return cleanText(c.Value, 256) && cleanText(c.Scheme, 256) && cleanText(c.Meaning, 256)
}

func cleanText(s string, limit int) bool {
	return len(s) > 0 && len(s) <= limit && strings.TrimSpace(s) == s && isPrintableASCII(s)
}

// PublicationMetadata supplies document-sharing identity and clinical metadata.
// Patient identifies the affinity domain. SourcePatient must match the local
// patient in the manifest. Both identifiers must be explicitly mapped by the
// caller. Clinical timestamps and codes refer to the images being published.
type PublicationMetadata struct {
	Patient, SourcePatient                                                                PatientID
	SubmissionUID, SourceUID                                                              string
	Title, LanguageCode                                                                   string
	CreatedAt, SubmittedAt                                                                time.Time
	ClassCode, TypeCode, ConfidentialityCode, FacilityCode, PracticeCode, ContentTypeCode Code
	EventCodes                                                                            []Code
	AuthorPerson, AuthorInstitution, AuthorRole, AuthorSpecialty                          string
	ServiceStart, ServiceStop                                                             time.Time
	// ReplacesEntryUUID names the prior registered manifest entry when this
	// publication replaces references that have become unavailable.
	ReplacesEntryUUID string
}

func (m PublicationMetadata) validate(manifest *Manifest) error {
	if manifest == nil || !m.Patient.valid() || !m.SourcePatient.valid() || m.SourcePatient != manifest.patient {
		return ErrMapping
	}
	if !core.IsValidUID(m.SubmissionUID) || !core.IsValidUID(m.SourceUID) ||
		m.SubmissionUID == manifest.uid || !cleanText(m.Title, 256) ||
		!cleanText(m.LanguageCode, 35) || m.CreatedAt.IsZero() || m.SubmittedAt.IsZero() ||
		len(m.EventCodes) > 64 || !validEntryUUID(m.ReplacesEntryUUID) {
		return ErrManifest
	}
	for _, code := range []Code{m.ClassCode, m.TypeCode, m.ConfidentialityCode, m.FacilityCode, m.PracticeCode, m.ContentTypeCode} {
		if !code.valid() {
			return ErrManifest
		}
	}
	for _, code := range m.EventCodes {
		if !code.valid() {
			return ErrManifest
		}
	}
	for _, author := range []string{m.AuthorPerson, m.AuthorInstitution, m.AuthorRole, m.AuthorSpecialty} {
		if author != "" && !cleanText(author, 256) {
			return ErrManifest
		}
	}
	if !m.ServiceStart.IsZero() && !m.ServiceStop.IsZero() && m.ServiceStop.Before(m.ServiceStart) {
		return ErrManifest
	}
	return nil
}

func validEntryUUID(value string) bool {
	if value == "" {
		return true
	}
	if len(value) != 45 || !strings.HasPrefix(value, "urn:uuid:") {
		return false
	}
	value = strings.TrimPrefix(value, "urn:uuid:")
	for i, c := range value {
		if i == 8 || i == 13 || i == 18 || i == 23 {
			if c != '-' {
				return false
			}
		} else if !(c >= '0' && c <= '9') && !(c >= 'a' && c <= 'f') && !(c >= 'A' && c <= 'F') {
			return false
		}
	}
	return true
}

func slot(name string, values ...string) *wireElement {
	n := element(nsRIM, "Slot", "", "name", name)
	list := element(nsRIM, "ValueList", "")
	for _, value := range values {
		list.Children = append(list.Children, element(nsRIM, "Value", value))
	}
	n.Children = []*wireElement{list}
	return n
}

func name(value string) *wireElement {
	n := element(nsRIM, "Name", "")
	n.Children = []*wireElement{element(nsRIM, "LocalizedString", "", "value", value)}
	return n
}

func classification(target, id, scheme string, code Code) *wireElement {
	n := element(nsRIM, "Classification", "", "id", id, "classifiedObject", target, "classificationScheme", scheme,
		"nodeRepresentation", code.Value)
	n.Children = []*wireElement{slot("codingScheme", code.Scheme), name(code.Meaning)}
	return n
}

func externalID(target, id, scheme, label, value string) *wireElement {
	n := element(nsRIM, "ExternalIdentifier", "", "id", id, "registryObject", target, "identificationScheme", scheme, "value", value)
	n.Children = []*wireElement{name(label)}
	return n
}

func authorMetadata(target, id, scheme string, m PublicationMetadata) *wireElement {
	n := element(nsRIM, "Classification", "", "id", id, "classifiedObject", target, "classificationScheme", scheme, "nodeRepresentation", "")
	for _, a := range []struct{ key, value string }{
		{"authorPerson", m.AuthorPerson}, {"authorInstitution", m.AuthorInstitution},
		{"authorRole", m.AuthorRole}, {"authorSpecialty", m.AuthorSpecialty},
	} {
		if a.value != "" {
			n.Children = append(n.Children, slot(a.key, a.value))
		}
	}
	if len(n.Children) == 0 {
		return nil
	}
	return n
}

func publicationBody(manifest *Manifest, m PublicationMetadata, entryID, setID string) *wireElement {
	const docType = "urn:uuid:7edca82f-054d-47f2-a032-9b2a5b5186c1"
	digest := sha1.Sum(manifest.data)
	doc := element(nsRIM, "ExtrinsicObject", "", "id", entryID, "mimeType", dicomMediaType, "objectType", docType)
	doc.Children = []*wireElement{
		slot("creationTime", m.CreatedAt.UTC().Format("20060102150405")),
		slot("languageCode", m.LanguageCode), slot("sourcePatientId", m.SourcePatient.cx()),
		slot("hash", hex.EncodeToString(digest[:])), slot("size", strconv.Itoa(len(manifest.data))),
		slot("urn:ihe:iti:xds:2013:referenceIdList", manifest.studyUID+"^^^^urn:ihe:iti:xds:2016:studyInstanceUID"),
		name(m.Title),
		externalID(entryID, "document-patient", "urn:uuid:58a6f841-87b3-4a3e-92fd-a8ffeff98427", "XDSDocumentEntry.patientId", m.Patient.cx()),
		externalID(entryID, "document-unique", "urn:uuid:2e82c1f6-a085-4c72-9da3-8640a32e42ab", "XDSDocumentEntry.uniqueId", manifest.uid),
	}
	if !m.ServiceStart.IsZero() {
		doc.Children = append(doc.Children, slot("serviceStartTime", m.ServiceStart.UTC().Format("20060102150405")))
	}
	if !m.ServiceStop.IsZero() {
		doc.Children = append(doc.Children, slot("serviceStopTime", m.ServiceStop.UTC().Format("20060102150405")))
	}
	for _, field := range []struct {
		id, scheme string
		code       Code
	}{
		{"class", "urn:uuid:41a5887f-8865-4c09-adf7-e362475b143a", m.ClassCode},
		{"type", "urn:uuid:f0306f51-975f-434e-a61c-c59651d33983", m.TypeCode},
		{"confidentiality", "urn:uuid:f4f85eac-e6cb-4883-b524-f2705394840f", m.ConfidentialityCode},
		{"facility", "urn:uuid:f33fb8ac-18af-42cc-ae0e-ed0b0bdb91e1", m.FacilityCode},
		{"practice", "urn:uuid:cccf5598-8b07-4b77-a05e-ae952c785ead", m.PracticeCode},
		{"format", "urn:uuid:a09d5840-386c-46f2-b5ad-9c3699a4309d", Code{sr.KeyObjectSelectionDocumentStorage, "1.2.840.10008.2.6.1", "DICOM Manifest"}},
	} {
		doc.Children = append(doc.Children, classification(entryID, "document-"+field.id, field.scheme, field.code))
	}
	for i, code := range m.EventCodes {
		doc.Children = append(doc.Children, classification(entryID, "event-"+strconv.Itoa(i), "urn:uuid:2c6b8cb7-8b2a-4051-b291-b1ae6a575ef4", code))
	}
	if author := authorMetadata(entryID, "document-author", "urn:uuid:93606bcf-9494-43ec-9b4e-a7748d1a838d", m); author != nil {
		doc.Children = append(doc.Children, author)
	}
	set := element(nsRIM, "RegistryPackage", "", "id", setID)
	set.Children = []*wireElement{
		slot("submissionTime", m.SubmittedAt.UTC().Format("20060102150405")), name(m.Title),
		externalID(setID, "submission-unique", "urn:uuid:96fdda7c-d067-4183-912e-bf5ee74998a8", "XDSSubmissionSet.uniqueId", m.SubmissionUID),
		externalID(setID, "submission-source", "urn:uuid:554ac39e-e3fe-47fe-b233-965d2a147832", "XDSSubmissionSet.sourceId", m.SourceUID),
		externalID(setID, "submission-patient", "urn:uuid:6b5aea1a-874d-4603-a4bc-96a0a7b38446", "XDSSubmissionSet.patientId", m.Patient.cx()),
		classification(setID, "submission-content", "urn:uuid:aa543740-bdda-424e-8c96-df4873be8500", m.ContentTypeCode),
	}
	if author := authorMetadata(setID, "submission-author", "urn:uuid:a7058bb9-b4e4-4307-ba5b-e3f0ab85e12d", m); author != nil {
		set.Children = append(set.Children, author)
	}
	setType := element(nsRIM, "Classification", "", "id", "submission-set-type", "classifiedObject", setID,
		"classificationNode", "urn:uuid:a54d6aa5-d40d-43f9-88c5-b4633d873bdd")
	association := element(nsRIM, "Association", "", "id", "submission-document", "associationType", "urn:oasis:names:tc:ebxml-regrep:AssociationType:HasMember",
		"sourceObject", setID, "targetObject", entryID)
	association.Children = []*wireElement{slot("SubmissionSetStatus", "Original")}
	list := element(nsRIM, "RegistryObjectList", "")
	list.Children = []*wireElement{doc, set, setType, association}
	if m.ReplacesEntryUUID != "" {
		list.Children = append(list.Children, element(nsRIM, "Association", "", "id", "replacement",
			"associationType", "urn:ihe:iti:2007:AssociationType:RPLC", "sourceObject", entryID, "targetObject", m.ReplacesEntryUUID))
	}
	submit := element(nsLCM, "SubmitObjectsRequest", "")
	submit.Children = []*wireElement{list}
	attachment := element(nsXDS, "Document", "", "id", entryID)
	attachment.Children = []*wireElement{element(nsXOP, "Include", "", "href", "cid:manifest@xdsi")}
	body := element(nsXDS, "ProvideAndRegisterDocumentSetRequest", "")
	body.Children = []*wireElement{submit, attachment}
	return body
}
