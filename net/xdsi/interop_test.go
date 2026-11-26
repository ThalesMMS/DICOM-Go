package xdsi

import (
	"bytes"
	"context"
	"crypto/sha1"
	"crypto/sha256"
	"encoding/hex"
	"encoding/xml"
	"errors"
	"fmt"
	"io"
	"mime"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"net/textproto"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/ThalesMMS/dicom-go/core"
	"github.com/ThalesMMS/dicom-go/internal/derivedio"
	"github.com/ThalesMMS/dicom-go/object"
	"github.com/ThalesMMS/dicom-go/transfer"
)

// The peer uses its own XML model and MIME serialization. It never calls the
// package's SOAP, metadata or MTOM builders/readers.
type peerNode struct {
	XMLName  xml.Name
	Attrs    []xml.Attr `xml:",any,attr"`
	Text     string     `xml:",chardata"`
	Children []peerNode `xml:",any"`
}

func (p peerNode) attr(key string) string {
	for _, a := range p.Attrs {
		if a.Name.Space == "" && a.Name.Local == key {
			return a.Value
		}
	}
	return ""
}

func (p peerNode) find(ns, local string) []peerNode {
	var out []peerNode
	for _, child := range p.Children {
		if child.XMLName.Space == ns && child.XMLName.Local == local {
			out = append(out, child)
		}
	}
	return out
}

func peerOne(p peerNode, ns, local string) (peerNode, error) {
	parts := p.find(ns, local)
	if len(parts) != 1 {
		return peerNode{}, fmt.Errorf("expected one {%s}%s, got %d", ns, local, len(parts))
	}
	return parts[0], nil
}

func peerText(p peerNode, ns, local string) (string, error) {
	c, err := peerOne(p, ns, local)
	if err != nil {
		return "", err
	}
	if len(c.Children) != 0 {
		return "", fmt.Errorf("nested value %s", local)
	}
	return strings.TrimSpace(c.Text), nil
}

func peerRead(r *http.Request) (peerNode, map[string][]byte, error) {
	var root peerNode
	mt, params, err := mime.ParseMediaType(r.Header.Get("Content-Type"))
	if err != nil || mt != "multipart/related" || params["type"] != "application/xop+xml" || params["start"] != "<root@xdsi>" {
		return root, nil, fmt.Errorf("invalid peer content type")
	}
	reader := multipart.NewReader(r.Body, params["boundary"])
	parts := map[string][]byte{}
	for {
		part, err := reader.NextPart()
		if err == io.EOF {
			break
		}
		if err != nil {
			return root, nil, err
		}
		id := strings.Trim(part.Header.Get("Content-ID"), "<>")
		if _, exists := parts[id]; exists {
			return root, nil, fmt.Errorf("duplicate MIME id")
		}
		data, err := io.ReadAll(io.LimitReader(part, 8<<20))
		if err != nil {
			return root, nil, err
		}
		parts[id] = data
	}
	soap, exists := parts["root@xdsi"]
	if !exists {
		return root, nil, fmt.Errorf("no root MIME part")
	}
	if err := xml.Unmarshal(soap, &root); err != nil {
		return root, nil, err
	}
	delete(parts, "root@xdsi")
	return root, parts, nil
}

func peerEnvelope(action, message, body string) string {
	return `<s:Envelope xmlns:s="http://www.w3.org/2003/05/soap-envelope" xmlns:wsa="http://www.w3.org/2005/08/addressing" xmlns:xds="urn:ihe:iti:xds-b:2007" xmlns:rs="urn:oasis:names:tc:ebxml-regrep:xsd:rs:3.0" xmlns:xop="http://www.w3.org/2004/08/xop/include"><s:Header><wsa:Action s:mustUnderstand="1">` + action + `</wsa:Action><wsa:RelatesTo>` + message + `</wsa:RelatesTo></s:Header><s:Body>` + body + `</s:Body></s:Envelope>`
}

func peerWrite(w http.ResponseWriter, soap string, parts map[string][]byte) error {
	var out bytes.Buffer
	multi := multipart.NewWriter(&out)
	root := textproto.MIMEHeader{}
	root.Set("Content-ID", "<reply-root>")
	root.Set("Content-Type", `application/xop+xml; charset=UTF-8; type="application/soap+xml"`)
	root.Set("Content-Transfer-Encoding", "binary")
	part, err := multi.CreatePart(root)
	if err != nil {
		return err
	}
	if _, err = part.Write([]byte(soap)); err != nil {
		return err
	}
	for id, data := range parts {
		header := textproto.MIMEHeader{}
		header.Set("Content-ID", "<"+id+">")
		header.Set("Content-Type", "application/dicom")
		header.Set("Content-Transfer-Encoding", "binary")
		part, err := multi.CreatePart(header)
		if err != nil {
			return err
		}
		if _, err = part.Write(data); err != nil {
			return err
		}
	}
	if err = multi.Close(); err != nil {
		return err
	}
	w.Header().Set("Content-Type", mime.FormatMediaType("multipart/related", map[string]string{
		"boundary": multi.Boundary(), "type": "application/xop+xml", "start": "<reply-root>", "start-info": "application/soap+xml",
	}))
	_, err = w.Write(out.Bytes())
	return err
}

func peerSlot(node peerNode, key string) (string, error) {
	for _, slot := range node.find("urn:oasis:names:tc:ebxml-regrep:xsd:rim:3.0", "Slot") {
		if slot.attr("name") != key {
			continue
		}
		list, err := peerOne(slot, "urn:oasis:names:tc:ebxml-regrep:xsd:rim:3.0", "ValueList")
		if err != nil {
			return "", err
		}
		return peerText(list, "urn:oasis:names:tc:ebxml-regrep:xsd:rim:3.0", "Value")
	}
	return "", fmt.Errorf("slot absent %s", key)
}

func peerIdentifier(node peerNode, scheme string) string {
	for _, child := range node.find("urn:oasis:names:tc:ebxml-regrep:xsd:rim:3.0", "ExternalIdentifier") {
		if child.attr("identificationScheme") == scheme {
			return child.attr("value")
		}
	}
	return ""
}

type syntheticPeer struct {
	mu                               sync.Mutex
	manifests                        map[string][]byte
	images                           map[string][]byte
	published, retrieved             int
	reject, mismatch, badCorrelation bool
	missing                          map[string]bool
	fault                            error
	expectedAuth                     string
}

func (p *syntheticPeer) serve(w http.ResponseWriter, r *http.Request) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if r.Method != "POST" || r.Header.Get("Authorization") != p.expectedAuth {
		p.fault = errors.New("missing caller transport authentication")
		http.Error(w, "denied", http.StatusForbidden)
		return
	}
	root, parts, err := peerRead(r)
	if err != nil {
		p.fail(w, err)
		return
	}
	if root.XMLName != (xml.Name{Space: "http://www.w3.org/2003/05/soap-envelope", Local: "Envelope"}) {
		p.fail(w, errors.New("SOAP 1.2 required"))
		return
	}
	header, err := peerOne(root, "http://www.w3.org/2003/05/soap-envelope", "Header")
	if err != nil {
		p.fail(w, err)
		return
	}
	action, err := peerText(header, "http://www.w3.org/2005/08/addressing", "Action")
	if err != nil {
		p.fail(w, err)
		return
	}
	message, err := peerText(header, "http://www.w3.org/2005/08/addressing", "MessageID")
	if err != nil {
		p.fail(w, err)
		return
	}
	body, err := peerOne(root, "http://www.w3.org/2003/05/soap-envelope", "Body")
	if err != nil {
		p.fail(w, err)
		return
	}
	if p.badCorrelation {
		message = "urn:uuid:invalid"
	}
	switch action {
	case "urn:ihe:iti:2007:ProvideAndRegisterDocumentSet-b":
		if err := p.publish(body, parts); err != nil {
			p.fail(w, err)
			return
		}
		status := `<rs:RegistryResponse status="urn:oasis:names:tc:ebxml-regrep:ResponseStatusType:Success"/>`
		if p.reject {
			status = `<rs:RegistryResponse status="urn:oasis:names:tc:ebxml-regrep:ResponseStatusType:Failure"><rs:RegistryErrorList><rs:RegistryError errorCode="XDSRepositoryMetadataError" severity="urn:oasis:names:tc:ebxml-regrep:ErrorSeverityType:Error" codeContext="synthetic rejection"/></rs:RegistryErrorList></rs:RegistryResponse>`
		}
		err = peerWrite(w, peerEnvelope("urn:ihe:iti:2007:ProvideAndRegisterDocumentSet-bResponse", message, status), nil)
	case "urn:ihe:rad:2009:RetrieveImagingDocumentSet":
		response, attachments, err2 := p.retrieve(body)
		if err2 != nil {
			p.fail(w, err2)
			return
		}
		err = peerWrite(w, peerEnvelope("urn:ihe:iti:2007:RetrieveDocumentSetResponse", message, response), attachments)
	default:
		err = fmt.Errorf("unknown SOAP action %s", action)
	}
	if err != nil {
		p.fault = err
	}
}

func (p *syntheticPeer) fail(w http.ResponseWriter, err error) {
	p.fault = err
	http.Error(w, "peer assertion failed", http.StatusBadRequest)
}

func (p *syntheticPeer) publish(body peerNode, parts map[string][]byte) error {
	const xds = "urn:ihe:iti:xds-b:2007"
	const rim = "urn:oasis:names:tc:ebxml-regrep:xsd:rim:3.0"
	request, err := peerOne(body, xds, "ProvideAndRegisterDocumentSetRequest")
	if err != nil {
		return err
	}
	submit, err := peerOne(request, "urn:oasis:names:tc:ebxml-regrep:xsd:lcm:3.0", "SubmitObjectsRequest")
	if err != nil {
		return err
	}
	list, err := peerOne(submit, rim, "RegistryObjectList")
	if err != nil {
		return err
	}
	entry, err := peerOne(list, rim, "ExtrinsicObject")
	if err != nil {
		return err
	}
	set, err := peerOne(list, rim, "RegistryPackage")
	if err != nil {
		return err
	}
	if entry.attr("mimeType") != "application/dicom" || entry.attr("objectType") != "urn:uuid:7edca82f-054d-47f2-a032-9b2a5b5186c1" {
		return errors.New("bad stable document entry")
	}
	doc, err := peerOne(request, xds, "Document")
	if err != nil || doc.attr("id") != entry.attr("id") {
		return errors.New("attachment is not document entry")
	}
	include, err := peerOne(doc, "http://www.w3.org/2004/08/xop/include", "Include")
	if err != nil {
		return err
	}
	data, exists := parts[strings.TrimPrefix(include.attr("href"), "cid:")]
	if !exists || len(parts) != 1 {
		return errors.New("missing or surplus manifest attachment")
	}
	hash, err := peerSlot(entry, "hash")
	if err != nil {
		return err
	}
	expectedHash := sha1.Sum(data)
	size, err := peerSlot(entry, "size")
	if err != nil || hash != hex.EncodeToString(expectedHash[:]) || size != strconv.Itoa(len(data)) {
		return errors.New("manifest hash/size failed")
	}
	patient := peerIdentifier(entry, "urn:uuid:58a6f841-87b3-4a3e-92fd-a8ffeff98427")
	if patient != "mapped-patient^^^&1.2.826.0.1.3680043.10.52&ISO" || patient != peerIdentifier(set, "urn:uuid:6b5aea1a-874d-4603-a4bc-96a0a7b38446") {
		return errors.New("affinity patient mismatch")
	}
	source, err := peerSlot(entry, "sourcePatientId")
	if err != nil || source != "patient-local^^^&1.2.826.0.1.3680043.10.51&ISO" {
		return errors.New("local patient mismatch")
	}
	format := ""
	for _, code := range entry.find(rim, "Classification") {
		if code.attr("classificationScheme") == "urn:uuid:a09d5840-386c-46f2-b5ad-9c3699a4309d" {
			format = code.attr("nodeRepresentation")
		}
	}
	if format != "1.2.840.10008.5.1.4.1.1.88.59" {
		return errors.New("format classification mismatch")
	}
	file, err := object.ReadFile(bytes.NewReader(data))
	if err != nil {
		return err
	}
	defer file.Close()
	studies, ok := file.Dataset.GetSequence(core.NewTag(0x0040, 0xA375))
	if !ok || len(studies) != 1 {
		return errors.New("missing manifest evidence")
	}
	series, _ := studies[0].GetSequence(core.NewTag(0x0008, 0x1115))
	if len(series) != 1 {
		return errors.New("evidence series")
	}
	location, _ := series[0].GetString(core.NewTag(0x0040, 0xE011))
	ae, _ := series[0].GetString(core.NewTag(0x0008, 0x0054))
	if location != testSource || ae != "XDSISOURCE" {
		return errors.New("manifest retrieval route")
	}
	refs, _ := series[0].GetSequence(core.NewTag(0x0008, 0x1199))
	if len(refs) != 2 {
		return errors.New("manifest SOP count")
	}
	for _, ref := range refs {
		id, _ := ref.GetString(core.NewTag(0x0008, 0x1155))
		class, _ := ref.GetString(core.NewTag(0x0008, 0x1150))
		if p.images[id] == nil || class != testCT {
			return errors.New("unavailable manifest reference")
		}
	}
	if peerIdentifier(entry, "urn:uuid:2e82c1f6-a085-4c72-9da3-8640a32e42ab") != testManifestUID {
		return errors.New("manifest unique ID")
	}
	if !p.reject {
		p.manifests[testManifestUID] = append([]byte(nil), data...)
	}
	p.published++
	return nil
}

func (p *syntheticPeer) retrieve(body peerNode) (string, map[string][]byte, error) {
	const rad = "urn:ihe:rad:xdsi-b:2009"
	const xds = "urn:ihe:iti:xds-b:2007"
	request, err := peerOne(body, rad, "RetrieveImagingDocumentSetRequest")
	if err != nil {
		return "", nil, err
	}
	study, err := peerOne(request, rad, "StudyRequest")
	if err != nil || study.attr("studyInstanceUID") != testStudy {
		return "", nil, errors.New("bad study")
	}
	series, err := peerOne(study, rad, "SeriesRequest")
	if err != nil || series.attr("seriesInstanceUID") != testSeries {
		return "", nil, errors.New("bad series")
	}
	syntaxes, err := peerOne(request, rad, "TransferSyntaxUIDList")
	if err != nil {
		return "", nil, err
	}
	syntax, err := peerText(syntaxes, rad, "TransferSyntaxUID")
	if err != nil || syntax != "1.2.840.10008.1.2.1" {
		return "", nil, errors.New("syntax preference")
	}
	var docs, reported strings.Builder
	parts := map[string][]byte{}
	for _, doc := range series.find(xds, "DocumentRequest") {
		repository, err := peerText(doc, xds, "RepositoryUniqueId")
		if err != nil || repository != testSource {
			return "", nil, errors.New("repository mismatch")
		}
		id, err := peerText(doc, xds, "DocumentUniqueId")
		if err != nil {
			return "", nil, err
		}
		data, found := p.images[id]
		if !found {
			return "", nil, errors.New("unknown request reference")
		}
		if p.missing[id] {
			reported.WriteString(`<rs:RegistryError errorCode="XDSDocumentUniqueIdError" severity="urn:oasis:names:tc:ebxml-regrep:ErrorSeverityType:Error" location="` + id + `" codeContext="synthetic unavailable"/>`)
			continue
		}
		if p.mismatch {
			data = p.images[testImage2]
		}
		cid := "document-" + strconv.Itoa(len(parts))
		parts[cid] = data
		docs.WriteString(`<xds:DocumentResponse><xds:RepositoryUniqueId>` + testSource + `</xds:RepositoryUniqueId><xds:DocumentUniqueId>` + id + `</xds:DocumentUniqueId><xds:mimeType>application/dicom</xds:mimeType><xds:Document><xop:Include href="cid:` + cid + `"/></xds:Document></xds:DocumentResponse>`)
	}
	status := "urn:oasis:names:tc:ebxml-regrep:ResponseStatusType:Success"
	if reported.Len() != 0 {
		status = "urn:ihe:iti:2007:ResponseStatusType:PartialSuccess"
		if len(parts) == 0 {
			status = "urn:oasis:names:tc:ebxml-regrep:ResponseStatusType:Failure"
		}
	}
	registry := `<rs:RegistryResponse status="` + status + `">`
	if reported.Len() != 0 {
		registry += `<rs:RegistryErrorList>` + reported.String() + `</rs:RegistryErrorList>`
	}
	registry += `</rs:RegistryResponse>`
	p.retrieved++
	return `<xds:RetrieveDocumentSetResponse>` + registry + docs.String() + `</xds:RetrieveDocumentSetResponse>`, parts, nil
}

type bearerTransport struct{ base http.RoundTripper }

func (tr bearerTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	clone := req.Clone(req.Context())
	clone.Header = req.Header.Clone()
	clone.Header.Set("Authorization", "Bearer synthetic-local")
	return tr.base.RoundTrip(clone)
}

func testMetadata() PublicationMetadata {
	return PublicationMetadata{
		Patient:       PatientID{"mapped-patient", "1.2.826.0.1.3680043.10.52"},
		SourcePatient: PatientID{"patient-local", "1.2.826.0.1.3680043.10.51"},
		SubmissionUID: "1.2.826.0.1.3680043.10.50.8", SourceUID: testSource,
		Title: "Synthetic CT", LanguageCode: "en-US",
		CreatedAt: time.Date(2026, 11, 16, 9, 0, 0, 0, time.UTC), SubmittedAt: time.Date(2026, 11, 16, 10, 0, 0, 0, time.UTC),
		ClassCode:           Code{"IMAGES", "1.2.3.4", "Imaging study"},
		TypeCode:            Code{"CT", "1.2.3.4", "Computed tomography"},
		ConfidentialityCode: Code{"N", "2.16.840.1.113883.5.25", "Normal"},
		FacilityCode:        Code{"HOSP", "1.2.3.4", "Hospital"},
		PracticeCode:        Code{"309964003", "2.16.840.1.113883.6.96", "Radiology"},
		ContentTypeCode:     Code{"IMAGES", "1.2.3.4", "Imaging study"},
		EventCodes:          []Code{{"CT", "1.2.840.10008.2.16.4", "Computed Tomography"}},
	}
}

func peerFixture(t *testing.T) (*Client, *syntheticPeer, *httptest.Server, *int) {
	t.Helper()
	p := &syntheticPeer{
		manifests: map[string][]byte{}, images: map[string][]byte{testImage: imageBytes(t, testImage), testImage2: imageBytes(t, testImage2)},
		missing: map[string]bool{}, expectedAuth: "Bearer synthetic-local",
	}
	server := httptest.NewTLSServer(http.HandlerFunc(p.serve))
	t.Cleanup(server.Close)
	httpClient := server.Client()
	httpClient.Transport = bearerTransport{httpClient.Transport}
	leases := 0
	client, err := NewClient(ClientConfig{
		HTTPClient: httpClient, RepositoryEndpoint: server.URL + "/repository", ImagingEndpoint: server.URL + "/imaging", ImagingSourceUID: testSource,
		Authorize: func(context.Context, Operation, []InstanceRef) error { return nil },
		Mapping: func(_ context.Context, local PatientID) (PatientID, error) {
			if local != testMetadata().SourcePatient {
				return PatientID{}, ErrMapping
			}
			return testMetadata().Patient, nil
		},
		HoldInstances: func(_ context.Context, refs []InstanceRef) (func(), error) {
			if len(refs) != 2 {
				return nil, ErrAvailability
			}
			leases++
			return func() { leases-- }, nil
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	return client, p, server, &leases
}

func TestControlledPeerPublicationThenRetrieve(t *testing.T) {
	client, peer, _, leases := peerFixture(t)
	manifest := mustManifest(t)
	result, err := client.Publish(context.Background(), manifest, testMetadata())
	if err != nil || !result.Successful() {
		t.Fatalf("publication %#v %v (peer %v)", result, err, peer.fault)
	}
	if !validEntryUUID(result.EntryUUID) || !validEntryUUID(result.SubmissionEntryUUID) || *leases != 0 {
		t.Fatal("publication lifecycle")
	}
	stored := peer.manifests[testManifestUID]
	if !bytes.Equal(stored, manifest.Bytes()) {
		t.Fatal("manifest changed during publish")
	}
	reloaded, err := ParseManifest(context.Background(), stored)
	if err != nil {
		t.Fatal(err)
	}
	got, err := client.Retrieve(context.Background(), reloaded, []string{transfer.ExplicitVRLittleEndian.UID})
	if err != nil || len(got.Instances) != 2 || got.Status != statusSuccess {
		t.Fatalf("retrieve %#v %v (peer %v)", got, err, peer.fault)
	}
	for i, out := range got.Instances {
		want := peer.images[manifest.refs[i].SOPInstanceUID]
		if out.Err != nil || !bytes.Equal(out.Data, want) || out.SHA256 != sha256.Sum256(want) {
			t.Fatalf("instance %d identity/hash mismatch: %v", i, out.Err)
		}
	}
	if peer.published != 1 || peer.retrieved != 1 || peer.fault != nil {
		t.Fatalf("peer state %#v", peer)
	}
}

func TestControlledPeerPartialAndRejectedAndWrongIdentity(t *testing.T) {
	t.Run("partial", func(t *testing.T) {
		client, peer, _, _ := peerFixture(t)
		peer.missing[testImage2] = true
		got, err := client.Retrieve(context.Background(), mustManifest(t), []string{transfer.ExplicitVRLittleEndian.UID})
		if err != nil || got.Status != statusPartial || got.Instances[0].Err != nil || got.Instances[1].Err == nil ||
			got.Instances[1].Data != nil || len(got.Instances[1].Errors) != 1 {
			t.Fatalf("partial %+v %v", got, err)
		}
	})
	t.Run("failure", func(t *testing.T) {
		client, peer, _, _ := peerFixture(t)
		peer.missing[testImage] = true
		peer.missing[testImage2] = true
		got, err := client.Retrieve(context.Background(), mustManifest(t), []string{transfer.ExplicitVRLittleEndian.UID})
		if err != nil || got.Status != statusFailure || got.Instances[0].Data != nil || got.Instances[1].Data != nil {
			t.Fatalf("failure %+v %v", got, err)
		}
	})
	t.Run("reject publication", func(t *testing.T) {
		client, peer, _, leases := peerFixture(t)
		peer.reject = true
		got, err := client.Publish(context.Background(), mustManifest(t), testMetadata())
		if err != nil || got.Successful() || len(got.Errors) != 1 || len(peer.manifests) != 0 || *leases != 0 {
			t.Fatalf("reject %+v %v", got, err)
		}
	})
	t.Run("wrong SOP", func(t *testing.T) {
		client, peer, _, _ := peerFixture(t)
		peer.mismatch = true
		got, err := client.Retrieve(context.Background(), mustManifest(t), []string{transfer.ExplicitVRLittleEndian.UID})
		if err != nil || !errors.Is(got.Instances[0].Err, ErrIdentity) || got.Instances[0].Data != nil || got.Instances[1].Err != nil {
			t.Fatalf("identity %+v %v", got, err)
		}
	})
	t.Run("wrong patient issuer", func(t *testing.T) {
		client, peer, _, _ := peerFixture(t)
		file, err := object.ReadFile(bytes.NewReader(peer.images[testImage]))
		if err != nil {
			t.Fatal(err)
		}
		file.Dataset.SetSequence(tagPatientIssuerQualifiers, derivedio.Object(
			derivedio.Str(tagUniversalEntityID, core.VRUT, "1.2.999"),
			derivedio.Str(tagUniversalEntityIDType, core.VRCS, "ISO")))
		var out bytes.Buffer
		if err := object.WriteFile(&out, file); err != nil {
			t.Fatal(err)
		}
		file.Close()
		peer.images[testImage] = out.Bytes()
		got, err := client.Retrieve(context.Background(), mustManifest(t), []string{transfer.ExplicitVRLittleEndian.UID})
		if err != nil || !errors.Is(got.Instances[0].Err, ErrIdentity) || got.Instances[0].Data != nil {
			t.Fatalf("patient identity %+v %v", got, err)
		}
	})
	t.Run("wrong reply", func(t *testing.T) {
		client, peer, _, _ := peerFixture(t)
		peer.badCorrelation = true
		_, err := client.Retrieve(context.Background(), mustManifest(t), []string{transfer.ExplicitVRLittleEndian.UID})
		if !errors.Is(err, ErrProtocol) {
			t.Fatalf("reply %v", err)
		}
	})
}

func TestMappingAuthorizationAndAvailabilityBeforeIO(t *testing.T) {
	for _, name := range []string{"mapping", "authorization", "availability", "revoked"} {
		t.Run(name, func(t *testing.T) {
			client, peer, _, leases := peerFixture(t)
			want := ErrMapping
			switch name {
			case "mapping":
				client.mapping = nil
			case "authorization":
				want = ErrAuthorization
				client.authorize = func(context.Context, Operation, []InstanceRef) error { return errors.New("denied") }
			case "availability":
				want = ErrAvailability
				client.hold = nil
			case "revoked":
				want = ErrAuthorization
				calls := 0
				client.authorize = func(context.Context, Operation, []InstanceRef) error {
					calls++
					if calls == 2 {
						return errors.New("revoked")
					}
					return nil
				}
			}
			_, err := client.Publish(context.Background(), mustManifest(t), testMetadata())
			if !errors.Is(err, want) || *leases != 0 {
				t.Fatalf("error %v / lease %d", err, *leases)
			}
			expected := 0
			if name == "revoked" {
				expected = 1
			}
			if peer.published != expected {
				t.Fatalf("unexpected I/O %d", peer.published)
			}
		})
	}
}

func TestRetrievalCancellationAndLimits(t *testing.T) {
	client, peer, _, _ := peerFixture(t)
	client.limits.DocumentBytes = 128
	_, err := client.Retrieve(context.Background(), mustManifest(t), []string{transfer.ExplicitVRLittleEndian.UID})
	if !errors.Is(err, ErrLimit) {
		t.Fatalf("document limit %v", err)
	}
	client.limits.DocumentBytes = 64 << 20
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, err = client.Retrieve(ctx, mustManifest(t), []string{transfer.ExplicitVRLittleEndian.UID})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("cancel %v", err)
	}
	if peer.retrieved != 1 {
		t.Fatalf("canceled request reached peer %d", peer.retrieved)
	}
}

func TestReplacementMetadata(t *testing.T) {
	m := testMetadata()
	m.ReplacesEntryUUID = "urn:uuid:11111111-2222-4333-8444-555555555555"
	body := publicationBody(mustManifest(t), m, "doc", "submission")
	submit := body.Children[0].Children[0]
	var replacement *wireElement
	for _, node := range submit.Children {
		if node.Name.Local == "Association" && node.attr("associationType") == "urn:ihe:iti:2007:AssociationType:RPLC" {
			replacement = node
		}
	}
	if replacement == nil || replacement.attr("targetObject") != m.ReplacesEntryUUID || replacement.attr("sourceObject") != "doc" {
		t.Fatal("replacement association absent")
	}
	m.ReplacesEntryUUID = "not-a-registry-entry"
	if m.validate(mustManifest(t)) == nil {
		t.Fatal("malformed replacement accepted")
	}
}

func TestReturnedReferenceAndSyntaxIsolation(t *testing.T) {
	ref := mustManifest(t).refs[0]
	limits, _ := normalizeLimits(Limits{})
	bytes := imageBytes(t, testImage)
	if _, err := validateInstance(context.Background(), bytes, ref, testMetadata().SourcePatient, []string{transfer.ImplicitVRLittleEndian.UID}, limits); !errors.Is(err, ErrIdentity) {
		t.Fatal(err)
	}
	ref.SeriesUID = "1.2.999"
	if _, err := validateInstance(context.Background(), bytes, ref, testMetadata().SourcePatient, []string{transfer.ExplicitVRLittleEndian.UID}, limits); !errors.Is(err, ErrIdentity) {
		t.Fatal(err)
	}
}
