package xdsi

import (
	"bytes"
	"context"
	"errors"
	"mime"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"net/textproto"
	"strings"
	"testing"

	"github.com/ThalesMMS/dicom-go/transfer"
)

func TestXMLStructureLimitsAndMustUnderstand(t *testing.T) {
	for name, text := range map[string]string{
		"doctype":                   `<!DOCTYPE x [<!ENTITY p SYSTEM "file:///secret">]><x>&p;</x>`,
		"duplicate attribute":       `<x id="one" id="two"/>`,
		"duplicate namespace alias": `<x xmlns:a="urn:x" xmlns:b="urn:x" a:id="one" b:id="two"/>`,
		"multiple roots":            `<x/><y/>`,
		"trailing text":             `<x/>ignored`,
		"depth":                     strings.Repeat("<x>", 25) + strings.Repeat("</x>", 25),
		"element count":             "<x>" + strings.Repeat("<y/>", 20000) + "</x>",
		"attribute count":           `<x ` + strings.Repeat("a=", 25) + `/>`,
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := parseXML([]byte(text)); err == nil {
				t.Fatal("malformed or over-budget XML accepted")
			}
		})
	}
	doc := peerEnvelope(actionRetrieveResponse, "urn:request", `<xds:RetrieveDocumentSetResponse/>`)
	doc = strings.Replace(doc, "<s:Header>", `<s:Header><unknown xmlns="urn:private" s:mustUnderstand="1"/>`, 1)
	root, err := parseXML([]byte(doc))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := responseBody(root, actionRetrieveResponse, "urn:request"); !errors.Is(err, ErrProtocol) {
		t.Fatal(err)
	}
}

func rawMultipart(t *testing.T, parts []attachment) (string, []byte) {
	t.Helper()
	var out bytes.Buffer
	writer := multipart.NewWriter(&out)
	for _, a := range parts {
		h := textproto.MIMEHeader{}
		h.Set("Content-ID", "<"+a.id+">")
		h.Set("Content-Type", a.mediaType)
		h.Set("Content-Transfer-Encoding", "binary")
		part, err := writer.CreatePart(h)
		if err != nil {
			t.Fatal(err)
		}
		part.Write(a.data)
	}
	writer.Close()
	return mime.FormatMediaType("multipart/related", map[string]string{
		"boundary": writer.Boundary(), "type": "application/xop+xml", "start": "<root>",
		"start-info": "application/soap+xml",
	}), out.Bytes()
}

func TestMTOMRejectsDuplicateUnrelatedAndBoundedParts(t *testing.T) {
	root := attachment{id: "root", mediaType: `application/xop+xml; type="application/soap+xml"`, data: []byte("<x/>")}
	limits, _ := normalizeLimits(Limits{})
	for name, parts := range map[string][]attachment{
		"duplicate root":  {root, root},
		"missing root":    {{id: "data", mediaType: "application/dicom", data: []byte{1}}},
		"wrong root type": {{id: "root", mediaType: "text/xml", data: []byte("<x/>")}},
		"too many":        {root, {id: "one", mediaType: "application/dicom", data: []byte{1}}, {id: "two", mediaType: "application/dicom", data: []byte{2}}},
	} {
		t.Run(name, func(t *testing.T) {
			ct, body := rawMultipart(t, parts)
			if _, _, err := readMTOM(ct, bytes.NewReader(body), limits, 2); err == nil {
				t.Fatal("invalid multipart accepted")
			}
		})
	}
	ct, body := rawMultipart(t, []attachment{root, {id: "huge", mediaType: "application/dicom", data: make([]byte, 33)}})
	limits.DocumentBytes = 32
	if _, _, err := readMTOM(ct, bytes.NewReader(body), limits, 2); !errors.Is(err, ErrLimit) {
		t.Fatal(err)
	}
}

func TestRegistryErrorStatusConsistency(t *testing.T) {
	for _, text := range []string{
		`<rs:RegistryResponse xmlns:rs="` + nsRS + `" status="` + statusFailure + `"/>`,
		`<rs:RegistryResponse xmlns:rs="` + nsRS + `" status="` + statusFailure + `"><rs:RegistryErrorList><rs:RegistryError errorCode="x" severity="urn:oasis:names:tc:ebxml-regrep:ErrorSeverityType:Warning"/></rs:RegistryErrorList></rs:RegistryResponse>`,
		`<rs:RegistryResponse xmlns:rs="` + nsRS + `" status="` + statusSuccess + `"><rs:RegistryErrorList><rs:RegistryError errorCode="x" severity="urn:oasis:names:tc:ebxml-regrep:ErrorSeverityType:Error"/></rs:RegistryErrorList></rs:RegistryResponse>`,
	} {
		root, err := parseXML([]byte(text))
		if err != nil {
			t.Fatal(err)
		}
		if _, _, err := registryResponse(root); !errors.Is(err, ErrProtocol) {
			t.Fatal(err)
		}
	}
}

func TestClientDisallowsRedirectAndUnsafeEndpoints(t *testing.T) {
	calls := 0
	redirectTarget := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { calls++ }))
	defer redirectTarget.Close()
	redirector := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, redirectTarget.URL, http.StatusTemporaryRedirect)
	}))
	defer redirector.Close()
	config := ClientConfig{HTTPClient: redirector.Client(), RepositoryEndpoint: redirector.URL, ImagingEndpoint: redirector.URL,
		ImagingSourceUID: testSource, Authorize: func(context.Context, Operation, []InstanceRef) error { return nil }}
	client, err := NewClient(config)
	if err != nil {
		t.Fatal(err)
	}
	_, err = client.Retrieve(context.Background(), mustManifest(t), []string{transfer.ExplicitVRLittleEndian.UID})
	if err == nil || calls != 0 {
		t.Fatalf("redirect followed %d %v", calls, err)
	}
	for _, endpoint := range []string{"http://example.test", "https://user:secret@example.test", "https://example.test/path?secret=x", "https://example.test/#fragment", "file:///local"} {
		config.RepositoryEndpoint = endpoint
		if _, err := NewClient(config); err == nil {
			t.Fatalf("unsafe endpoint %s", endpoint)
		}
	}
}

func TestMTOMCorrelationsAndRevocationHideAllData(t *testing.T) {
	type mutation struct {
		name  string
		apply func(string, map[string][]byte) (string, map[string][]byte)
	}
	changes := []mutation{
		{"unknown reference", func(s string, p map[string][]byte) (string, map[string][]byte) {
			return strings.Replace(s, testImage, "1.2.3.999", 1), p
		}},
		{"wrong repository", func(s string, p map[string][]byte) (string, map[string][]byte) {
			return strings.Replace(s, testSource, "1.2.3.999", 1), p
		}},
		{"external include", func(s string, p map[string][]byte) (string, map[string][]byte) {
			return strings.Replace(s, "cid:document-0", "https://external.test/data", 1), p
		}},
		{"unused part", func(s string, p map[string][]byte) (string, map[string][]byte) {
			p["extra"] = []byte("extra")
			return s, p
		}},
		{"missing part", func(s string, p map[string][]byte) (string, map[string][]byte) { delete(p, "document-0"); return s, p }},
		{"success with missing instance", func(s string, p map[string][]byte) (string, map[string][]byte) {
			start := strings.Index(s, "<xds:DocumentResponse>")
			end := strings.Index(s, "</xds:DocumentResponse>") + len("</xds:DocumentResponse>")
			delete(p, "document-0")
			return s[:start] + s[end:], p
		}},
		{"duplicate response", func(s string, p map[string][]byte) (string, map[string][]byte) {
			start := strings.Index(s, "<xds:DocumentResponse>")
			end := strings.Index(s, "</xds:DocumentResponse>") + len("</xds:DocumentResponse>")
			return strings.Replace(s, "</xds:RetrieveDocumentSetResponse>", s[start:end]+"</xds:RetrieveDocumentSetResponse>", 1), p
		}},
	}
	for _, change := range changes {
		t.Run(change.name, func(t *testing.T) {
			client, peer, _, _ := peerFixture(t)
			server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				root, _, err := peerRead(r)
				if err != nil {
					t.Error(err)
					return
				}
				header, _ := peerOne(root, nsSOAP, "Header")
				message, _ := peerText(header, nsWSA, "MessageID")
				body, _ := peerOne(root, nsSOAP, "Body")
				payload, parts, err := peer.retrieve(body)
				if err != nil {
					t.Error(err)
					return
				}
				payload, parts = change.apply(payload, parts)
				peerWrite(w, peerEnvelope(actionRetrieveResponse, message, payload), parts)
			}))
			defer server.Close()
			client.http = server.Client()
			client.imaging = server.URL
			got, err := client.Retrieve(context.Background(), mustManifest(t), []string{transfer.ExplicitVRLittleEndian.UID})
			if err == nil || len(got.Instances) != 0 {
				t.Fatalf("returned malformed transaction %+v %v", got, err)
			}
		})
	}
	t.Run("post-receive revoke", func(t *testing.T) {
		client, _, _, _ := peerFixture(t)
		count := 0
		client.authorize = func(context.Context, Operation, []InstanceRef) error {
			count++
			if count > 1 {
				return errors.New("revoked")
			}
			return nil
		}
		got, err := client.Retrieve(context.Background(), mustManifest(t), []string{transfer.ExplicitVRLittleEndian.UID})
		if !errors.Is(err, ErrAuthorization) || len(got.Instances) != 0 {
			t.Fatalf("revoked returned data %+v %v", got, err)
		}
	})
}
