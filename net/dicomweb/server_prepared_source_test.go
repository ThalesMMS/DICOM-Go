package dicomweb

import (
	"bytes"
	"context"
	"io"
	"mime"
	"net/http"
	"strconv"
	"strings"
	"testing"

	"github.com/ThalesMMS/dicom-go/core"
	"github.com/ThalesMMS/dicom-go/object"
	"github.com/ThalesMMS/dicom-go/pixeldata"
	"github.com/ThalesMMS/dicom-go/pixeldata/builtin"
	"github.com/ThalesMMS/dicom-go/transfer"
)

// preparedSourceObject is one stored object of a prepared retrieval test.
type preparedSourceObject struct {
	sop    string
	syntax transfer.Syntax
	lossy  bool
	data   []byte
}

// preparedSourceFixture is preparedTestFixture with its own SOP Instance UID
// in series 1.3, optionally marked Lossy Image Compression 01.
func preparedSourceFixture(t *testing.T, syntax transfer.Syntax, sop string, lossy bool) (preparedSourceObject, []byte) {
	t.Helper()
	data, pixels := preparedTestFixture(t, syntax)
	file, err := object.ReadFile(bytes.NewReader(data))
	if err != nil {
		t.Fatal(err)
	}
	file.Dataset.Put(core.Element{Header: core.ElementHeader{Tag: core.NewTag(0x0008, 0x0018), VR: core.VRUI}, Value: core.StringValue{sop}})
	if lossy {
		file.Dataset.Put(core.Element{Header: core.ElementHeader{Tag: core.NewTag(0x0028, 0x2110), VR: core.VRCS}, Value: core.StringValue{"01"}})
	}
	var buffer bytes.Buffer
	if err := object.WriteFile(&buffer, file); err != nil {
		t.Fatal(err)
	}
	return preparedSourceObject{sop: sop, syntax: syntax, lossy: lossy, data: buffer.Bytes()}, pixels
}

// checkingSourceBackend answers like an archive that checks every object of
// the selection with AcceptsSource before yielding the first part; with
// check false it yields every object and records what AcceptsSource said.
func checkingSourceBackend(objects []preparedSourceObject, check bool, answers *[]bool) *serverTestBackend {
	backend := completeServerTestBackend()
	backend.retrieve = func(_ context.Context, req RetrieveRequest, yield func(RetrievePart) error) error {
		selected := objects
		if req.SOPInstanceUID != "" {
			selected = nil
			for _, o := range objects {
				if o.sop == req.SOPInstanceUID {
					selected = append(selected, o)
				}
			}
		}
		for _, o := range selected {
			ok := req.AcceptsSource(o.syntax.UID)
			*answers = append(*answers, ok)
			if check && !ok {
				return ErrUnsupported
			}
		}
		for _, o := range selected {
			ref := InstanceRef{StudyInstanceUID: "1.2", SeriesInstanceUID: "1.3", SOPInstanceUID: o.sop}
			if err := yield(RetrievePart{Ref: ref, ContentType: "application/dicom", TransferSyntaxUID: o.syntax.UID, Size: int64(len(o.data)), Reader: io.NopCloser(bytes.NewReader(o.data))}); err != nil {
				return err
			}
		}
		return nil
	}
	return backend
}

func preparedDecoderPolicy(t *testing.T) *RetrieveTranscodePolicy {
	t.Helper()
	decoders, err := builtin.NewRegistry()
	if err != nil {
		t.Fatal(err)
	}
	return &RetrieveTranscodePolicy{Options: pixeldata.TranscodeOptions{DecoderRegistry: decoders}}
}

// TestPreparedRetrieveRefusesUndeliverableCollectionBeforeFirstPart retrieves
// a series holding an Explicit VR Little Endian object and then a JPEG-LS
// Lossless one through a backend that checks AcceptsSource first. Every
// answer is either a refusal before any byte or a complete multipart body
// whose objects carry the negotiated syntax: never a 200 that ends early.
func TestPreparedRetrieveRefusesUndeliverableCollectionBeforeFirstPart(t *testing.T) {
	first, pixels := preparedSourceFixture(t, transfer.ExplicitVRLittleEndian, "1.4", false)
	second, _ := preparedSourceFixture(t, transfer.JPEGLSLossless, "1.5", false)
	objects := []preparedSourceObject{first, second}
	explicit, jls := transfer.ExplicitVRLittleEndian.UID, transfer.JPEGLSLossless.UID
	multipartDICOM := `multipart/related; type="application/dicom"`
	for _, policy := range []struct {
		name   string
		policy func() *RetrieveTranscodePolicy
		want   map[string][]string // Accept -> syntax of each part; nil is 406
	}{
		{"without codecs", func() *RetrieveTranscodePolicy { return &RetrieveTranscodePolicy{} }, map[string][]string{
			multipartDICOM:                                   nil,
			multipartDICOM + "; transfer-syntax=*":           {explicit, jls},
			multipartDICOM + "; transfer-syntax=" + explicit: nil,
			multipartDICOM + "; transfer-syntax=" + jls:      nil,
		}},
		{"with decoders", func() *RetrieveTranscodePolicy { return preparedDecoderPolicy(t) }, map[string][]string{
			multipartDICOM:                                   {explicit, explicit},
			multipartDICOM + "; transfer-syntax=*":           {explicit, explicit},
			multipartDICOM + "; transfer-syntax=" + explicit: {explicit, explicit},
			multipartDICOM + "; transfer-syntax=" + jls:      nil,
		}},
	} {
		t.Run(policy.name, func(t *testing.T) {
			var answers []bool
			_, hs := newOpenTestServer(t, checkingSourceBackend(objects, true, &answers), func(o *ServerOptions) { o.RetrieveTranscode = policy.policy() })
			for accept, want := range policy.want {
				request, _ := http.NewRequest(http.MethodGet, hs.URL+"/studies/1.2/series/1.3", nil)
				request.Header.Set("Accept", accept)
				response, err := hs.Client().Do(request)
				if err != nil {
					t.Fatal(err)
				}
				body, err := io.ReadAll(response.Body)
				response.Body.Close()
				if err != nil {
					t.Fatalf("%s: %v", accept, err)
				}
				if want == nil {
					if response.StatusCode != http.StatusNotAcceptable {
						t.Errorf("%s: HTTP %d, want 406", accept, response.StatusCode)
					}
					continue
				}
				if response.StatusCode != http.StatusOK {
					t.Fatalf("%s: HTTP %d, want 200", accept, response.StatusCode)
				}
				_, params, err := mime.ParseMediaType(response.Header.Get("Content-Type"))
				if err != nil || !strings.HasSuffix(string(body), "--"+params["boundary"]+"--\r\n") {
					t.Fatalf("%s: the multipart body has no closing boundary", accept)
				}
				parts := readMultipartStringsFromBody(t, params["boundary"], body)
				if len(parts) != len(want) {
					t.Fatalf("%s: %d parts, want %d", accept, len(parts), len(want))
				}
				for i, part := range parts {
					assertPreparedPixelsOf(t, []byte(part), pixels, want[i], objects[i].sop)
				}
			}
		})
	}
}

// TestPreparedRetrieveAcceptsSourceNeverOverpromises delivers each stored
// syntax alone, with every kind of Accept, and requires each object that
// AcceptsSource accepted to be delivered. AcceptsSource may refuse an object
// the server could deliver only because of attributes it has not read, as a
// lossless syntax marked lossy, never the reverse.
func TestPreparedRetrieveAcceptsSourceNeverOverpromises(t *testing.T) {
	type stored struct {
		syntax transfer.Syntax
		lossy  bool
	}
	var objects []preparedSourceObject
	for i, s := range []stored{
		{transfer.ExplicitVRLittleEndian, false}, {transfer.ImplicitVRLittleEndian, false}, {transfer.ExplicitVRBigEndian, false},
		{transfer.DeflatedExplicitVRLittleEndian, false}, {transfer.EncapsulatedUncompressedExplicitVRLittleEndian, false},
		{transfer.RLELossless, false}, {transfer.JPEGLSLossless, false}, {transfer.JPEGLosslessSV1, false},
		{transfer.DeflatedImageFrameCompression, false}, {transfer.JPEGBaseline, false}, {transfer.JPEGLSLossless, true},
	} {
		o, _ := preparedSourceFixture(t, s.syntax, "1.4."+strconv.Itoa(i+1), s.lossy)
		objects = append(objects, o)
	}
	for _, policy := range []struct {
		name   string
		policy func() *RetrieveTranscodePolicy
	}{
		{"without codecs", func() *RetrieveTranscodePolicy { return &RetrieveTranscodePolicy{} }},
		{"with decoders", func() *RetrieveTranscodePolicy { return preparedDecoderPolicy(t) }},
	} {
		t.Run(policy.name, func(t *testing.T) {
			var answers []bool
			_, hs := newOpenTestServer(t, checkingSourceBackend(objects, false, &answers), func(o *ServerOptions) { o.RetrieveTranscode = policy.policy() })
			for _, o := range objects {
				for _, accept := range []string{
					"application/dicom", "application/dicom; transfer-syntax=*",
					"application/dicom; transfer-syntax=" + transfer.ExplicitVRLittleEndian.UID,
					"application/dicom; transfer-syntax=" + o.syntax.UID,
					`multipart/related; type="application/dicom"`,
				} {
					answers = nil
					request, _ := http.NewRequest(http.MethodGet, hs.URL+"/studies/1.2/series/1.3/instances/"+o.sop, nil)
					request.Header.Set("Accept", accept)
					response, err := hs.Client().Do(request)
					if err != nil {
						t.Fatal(err)
					}
					_, _ = io.Copy(io.Discard, response.Body)
					response.Body.Close()
					if len(answers) == 1 && answers[0] && response.StatusCode != http.StatusOK {
						t.Errorf("%s (lossy marked %t) with %q: AcceptsSource accepted it, delivery answered HTTP %d", o.syntax.Name, o.lossy, accept, response.StatusCode)
					}
				}
			}
		})
	}
}

// assertPreparedPixelsOf is assertPreparedPixels for an object whose SOP
// Instance UID is sop.
func assertPreparedPixelsOf(t *testing.T, data, pixels []byte, syntax, sop string) {
	t.Helper()
	f, err := object.ReadFile(bytes.NewReader(data))
	if err != nil {
		t.Fatal(err)
	}
	if uid, _ := f.Dataset.GetString(core.NewTag(0x0008, 0x0018)); strings.TrimSpace(uid) != sop || f.TransferSyntax.UID != syntax {
		t.Fatalf("part is %s in %s, want %s in %s", strings.TrimSpace(uid), f.TransferSyntax.UID, sop, syntax)
	}
	native, _, err := pixeldata.TranscodeFile(context.Background(), f, transfer.ExplicitVRLittleEndian, preparedTestOptions(t))
	if err != nil {
		t.Fatal(err)
	}
	frames, err := pixeldata.ExtractNativeFrames(native.Dataset)
	if err != nil {
		t.Fatal(err)
	}
	if got := bytes.Join(frames.Data, nil); !bytes.Equal(got, pixels) {
		t.Fatal("a lossless part changed samples")
	}
}

// TestPreparedRetrieveServesImplicitAndBigEndianSourcesAsExplicitLittleEndian
// checks that the zero prepared retrieval policy, which has no codec, turns
// objects stored in the two syntaxes PS3.18 excludes from the web services
// into Explicit VR Little Endian with identical samples, at every level, and
// that a backend can tell so with AcceptsSource before its first part.
func TestPreparedRetrieveServesImplicitAndBigEndianSourcesAsExplicitLittleEndian(t *testing.T) {
	for _, syntax := range []transfer.Syntax{transfer.ImplicitVRLittleEndian, transfer.ExplicitVRBigEndian} {
		t.Run(syntax.Name, func(t *testing.T) {
			input, pixels := preparedTestFixture(t, syntax)
			backend := completeServerTestBackend()
			var accepted []bool
			backend.retrieve = func(_ context.Context, req RetrieveRequest, yield func(RetrievePart) error) error {
				ok := req.AcceptsSource(syntax.UID)
				accepted = append(accepted, ok)
				if !ok {
					return ErrUnsupported
				}
				if req.AcceptsOriginal(syntax.UID) {
					t.Error("AcceptsOriginal accepted a syntax PS3.18 excludes")
				}
				return yield(RetrievePart{Ref: preparedTestRef, ContentType: "application/dicom", TransferSyntaxUID: syntax.UID, Size: int64(len(input)), Reader: io.NopCloser(bytes.NewReader(input))})
			}
			_, hs := newOpenTestServer(t, backend, func(o *ServerOptions) { o.RetrieveTranscode = &RetrieveTranscodePolicy{} })
			explicit := transfer.ExplicitVRLittleEndian.UID
			for _, tc := range []struct {
				name, path, accept string
				status             int
			}{
				{"instance without syntax", preparedInstancePath, "application/dicom", http.StatusOK},
				{"instance in Explicit VR Little Endian", preparedInstancePath, "application/dicom; transfer-syntax=" + explicit, http.StatusOK},
				{"instance in any syntax", preparedInstancePath, "application/dicom; transfer-syntax=*", http.StatusOK},
				{"series", "/studies/1.2/series/1.3", `multipart/related; type="application/dicom"`, http.StatusOK},
				{"study", "/studies/1.2", `multipart/related; type="application/dicom"; transfer-syntax=` + explicit, http.StatusOK},
				{"the stored syntax", preparedInstancePath, "application/dicom; transfer-syntax=" + syntax.UID, http.StatusNotAcceptable},
				{"only a compressed syntax", preparedInstancePath, "application/dicom; transfer-syntax=" + transfer.RLELossless.UID, http.StatusNotAcceptable},
			} {
				accepted = nil
				request, _ := http.NewRequest(http.MethodGet, hs.URL+tc.path, nil)
				request.Header.Set("Accept", tc.accept)
				response, err := hs.Client().Do(request)
				if err != nil {
					t.Fatal(err)
				}
				data, err := io.ReadAll(response.Body)
				response.Body.Close()
				if err != nil || response.StatusCode != tc.status {
					t.Fatalf("%s: HTTP %d (%v), want %d", tc.name, response.StatusCode, err, tc.status)
				}
				if tc.status != http.StatusOK {
					// The server refuses an excluded syntax before asking the
					// backend; otherwise the backend learns it before yielding.
					if len(accepted) > 1 || (len(accepted) == 1 && accepted[0]) {
						t.Errorf("%s: AcceptsSource answered %v, want false before the first part", tc.name, accepted)
					}
					continue
				}
				if len(accepted) != 1 || !accepted[0] {
					t.Errorf("%s: AcceptsSource answered %v, want true", tc.name, accepted)
				}
				media, params, err := mime.ParseMediaType(response.Header.Get("Content-Type"))
				if err != nil {
					t.Fatal(err)
				}
				if media == "multipart/related" {
					parts := readMultipartStringsFromBody(t, params["boundary"], data)
					if len(parts) != 1 {
						t.Fatalf("%s: %d parts, want 1", tc.name, len(parts))
					}
					data = []byte(parts[0])
				} else if media != "application/dicom" || params["transfer-syntax"] != explicit {
					t.Fatalf("%s: Content-Type %q", tc.name, response.Header.Get("Content-Type"))
				}
				assertPreparedPixels(t, data, pixels, explicit)
			}
		})
	}
}

func readMultipartStringsFromBody(t *testing.T, boundary string, body []byte) []string {
	t.Helper()
	header := http.Header{}
	header.Set("Content-Type", `multipart/related; type="application/dicom"; boundary=`+boundary)
	return readMultipartStrings(t, &http.Response{Header: header, Body: io.NopCloser(bytes.NewReader(body))})
}

// TestPreparedRetrievePreferStoredSyntax checks the tie between a stored
// syntax and a conversion the client accepts as much: by default the first
// target wins, with PreferStoredSyntax the stored bytes pass through. An
// explicit or default preference is unaffected.
func TestPreparedRetrievePreferStoredSyntax(t *testing.T) {
	stored := transfer.EncapsulatedUncompressedExplicitVRLittleEndian
	input, pixels := preparedTestFixture(t, stored)
	for _, prefer := range []bool{false, true} {
		_, hs := newOpenTestServer(t, preparedTestBackend(input, stored.UID), func(o *ServerOptions) {
			o.RetrieveTranscode = &RetrieveTranscodePolicy{PreferStoredSyntax: prefer}
		})
		for _, tc := range []struct {
			accept string
			want   string
		}{
			{"application/dicom; transfer-syntax=*", map[bool]string{false: transfer.ExplicitVRLittleEndian.UID, true: stored.UID}[prefer]},
			{"application/dicom", transfer.ExplicitVRLittleEndian.UID},
			{"application/dicom; transfer-syntax=" + stored.UID, stored.UID},
		} {
			request, _ := http.NewRequest(http.MethodGet, hs.URL+preparedInstancePath, nil)
			request.Header.Set("Accept", tc.accept)
			response, err := hs.Client().Do(request)
			if err != nil {
				t.Fatal(err)
			}
			data, err := io.ReadAll(response.Body)
			response.Body.Close()
			if err != nil || response.StatusCode != http.StatusOK {
				t.Fatalf("prefer=%t %s: HTTP %d (%v)", prefer, tc.accept, response.StatusCode, err)
			}
			if tc.want == stored.UID && !bytes.Equal(data, input) {
				t.Fatalf("prefer=%t %s: the stored bytes did not pass through", prefer, tc.accept)
			}
			assertPreparedPixels(t, data, pixels, tc.want)
		}
	}
}

// TestRetrieveRequestAcceptsSourceWithoutPreparation keeps AcceptsSource
// equal to AcceptsOriginal on a request the server did not prepare.
func TestRetrieveRequestAcceptsSourceWithoutPreparation(t *testing.T) {
	request := RetrieveRequest{Level: RetrieveLevelInstance, Accept: parseMediaPreferences("application/dicom; transfer-syntax=*", "application/dicom")}
	for _, uid := range []string{transfer.ImplicitVRLittleEndian.UID, transfer.ExplicitVRBigEndian.UID, transfer.ExplicitVRLittleEndian.UID, transfer.JPEGLSLossless.UID, "1.2.3.4"} {
		if request.AcceptsSource(uid) != request.AcceptsOriginal(uid) {
			t.Errorf("AcceptsSource(%s) differs from AcceptsOriginal", uid)
		}
	}
}
