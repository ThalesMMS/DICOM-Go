package dicomweb

import (
	"bytes"
	"context"
	"encoding/binary"
	"io"
	"mime"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/ThalesMMS/dicom-go/core"
	"github.com/ThalesMMS/dicom-go/object"
	"github.com/ThalesMMS/dicom-go/pixeldata"
	"github.com/ThalesMMS/dicom-go/transfer"
)

const uriTestQuery = "requestType=WADO&studyUID=1.2&seriesUID=1.3&objectUID=1.4"

func TestWADOURISigned16StoredByteOrderRealHTTP(t *testing.T) {
	for _, source := range []transfer.Syntax{transfer.ExplicitVRBigEndian, transfer.ImplicitVRLittleEndian, transfer.RLELossless} {
		t.Run(source.Name, func(t *testing.T) {
			input, _ := preparedTestFixture(t, transfer.ExplicitVRLittleEndian)
			file, err := object.ReadFile(bytes.NewReader(input))
			if err != nil {
				t.Fatal(err)
			}
			for _, field := range []struct {
				tag   core.Tag
				value uint16
			}{{core.NewTag(0x0028, 0x0100), 16}, {core.NewTag(0x0028, 0x0101), 12}, {core.NewTag(0x0028, 0x0102), 11}, {core.NewTag(0x0028, 0x0103), 1}} {
				file.Dataset.Put(core.NewRawElement(field.tag, core.VRUS, binary.LittleEndian.AppendUint16(nil, field.value)))
			}
			pixels := make([]byte, 128)
			for i := 0; i < 64; i++ {
				binary.LittleEndian.PutUint16(pixels[i*2:], uint16(int16(i*59-1900)))
			}
			file.Dataset.Put(core.NewRawElement(core.TagPixelData, core.VROW, pixels))
			stored, _, err := pixeldata.TranscodeFile(context.Background(), file, source, preparedTestOptions(t))
			if err != nil {
				t.Fatal(err)
			}
			var encoded bytes.Buffer
			if err := object.WriteFile(&encoded, stored); err != nil {
				t.Fatal(err)
			}
			data := encoded.Bytes()
			before := append([]byte(nil), data...)
			_, hs := newOpenTestServer(t, preparedTestBackend(data, source.UID), func(o *ServerOptions) { o.EnableWADOURI = true; o.RetrieveTranscode = preparedTestPolicy(t) })
			r, _ := http.NewRequest("GET", hs.URL+"/wado?"+uriTestQuery+"&contentType=application%2Fdicom", nil)
			r.Header.Set("Accept", "application/dicom")
			response, err := hs.Client().Do(r)
			if err != nil {
				t.Fatal(err)
			}
			body, err := io.ReadAll(response.Body)
			response.Body.Close()
			if err != nil || response.StatusCode != 200 {
				t.Fatal("signed16 URI retrieval failed")
			}
			assertPreparedPixels(t, body, pixels, transfer.ExplicitVRLittleEndian.UID)
			if !bytes.Equal(data, before) {
				t.Fatal("stored signed16 source changed")
			}
		})
	}
}

func TestWADOURILegacyHTTPClientPart10AndBinding(t *testing.T) {
	for _, source := range []transfer.Syntax{transfer.ExplicitVRLittleEndian, transfer.ImplicitVRLittleEndian, transfer.ExplicitVRBigEndian, transfer.RLELossless, transfer.DeflatedExplicitVRLittleEndian} {
		t.Run(source.Name, func(t *testing.T) {
			input, pixels := preparedTestFixture(t, source)
			backend := preparedTestBackend(input, source.UID)
			var authorizations atomic.Int32
			s, err := NewServer(ServerOptions{Backend: backend, EnableWADOURI: true, RetrieveTranscode: preparedTestPolicy(t, transfer.ExplicitVRLittleEndian.UID, transfer.RLELossless.UID), ServiceRoot: "/dicomweb", Authorize: func(_ context.Context, r *http.Request, op Operation) error {
				if op != OperationRetrieveWADOURI || r.URL.Query().Get("objectUID") != "1.4" {
					return ErrForbidden
				}
				authorizations.Add(1)
				return nil
			}})
			if err != nil {
				t.Fatal(err)
			}
			hs := httptest.NewServer(http.StripPrefix("/dicomweb", s))
			defer hs.Close()
			// Independent legacy-style client: query identifies the instance; MIME TS
			// parameters are absent; the result is parsed as a plain Part10 object.
			client := &http.Client{}
			for _, suffix := range []string{"&contentType=application%2Fdicom", "&contentType=application%2Fdicom&transferSyntax=" + transfer.RLELossless.UID} {
				r, _ := http.NewRequest("GET", hs.URL+"/dicomweb/wado?"+uriTestQuery+suffix, nil)
				r.Header.Set("Accept", "application/dicom")
				response, err := client.Do(r)
				if err != nil {
					t.Fatal(err)
				}
				data, err := io.ReadAll(response.Body)
				response.Body.Close()
				if response.StatusCode != 200 || err != nil {
					t.Fatalf("URI HTTP=%d err=%v", response.StatusCode, err)
				}
				media, params, err := mime.ParseMediaType(response.Header.Get("Content-Type"))
				if err != nil || media != "application/dicom" || len(params) != 0 {
					t.Fatal("URI response included forbidden MIME parameters")
				}
				syntax := transfer.ExplicitVRLittleEndian.UID
				if strings.Contains(suffix, "transferSyntax=") {
					syntax = transfer.RLELossless.UID
				}
				assertPreparedPixels(t, data, pixels, syntax)
				if !strings.HasPrefix(response.Header.Get("Content-Location"), "/dicomweb/studies/") {
					t.Fatal("service mount/source ownership changed")
				}
			}
			if authorizations.Load() != 2 {
				t.Fatal("URI did not use its own authorization operation")
			}
		})
	}
}
func TestWADOURIRenderedTranslationAndHeaderExclusions(t *testing.T) {
	var calls atomic.Int32
	renderer := renderTestRenderer830(func(_ context.Context, r RenderRequest, yield func(RenderedPart) error) error {
		calls.Add(1)
		if r.Level != RenderLevelFrames || r.Ref != preparedTestRef || len(r.Frames) != 1 || r.Frames[0] != 2 || r.Viewport == nil || r.Viewport.Width != 20 || r.Viewport.Height != 10 || r.Window == nil || r.Window.Center != 4 || r.Window.Width != 8 || r.Quality != 91 {
			t.Fatal("URI render parameters were not translated")
		}
		return yield(RenderedPart{Ref: preparedTestRef, FrameNumber: 2, ContentType: "image/jpeg", Width: 20, Height: 10, Reader: strings.NewReader("controlled rendered bytes")})
	})
	s, hs := newOpenTestServer(t, completeServerTestBackend(), func(o *ServerOptions) { o.EnableWADOURI = true; o.Renderer = renderer })
	raw := uriTestQuery + "&contentType=image%2Fjpeg&frameNumber=2&rows=10&columns=20&windowCenter=4&windowWidth=8&imageQuality=91"
	req, _ := http.NewRequest("GET", hs.URL+"/wado?"+raw, nil)
	req.Header.Set("Accept", "image/jpeg")
	response, err := hs.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	data, _ := io.ReadAll(response.Body)
	response.Body.Close()
	if response.StatusCode != 200 || string(data) != "controlled rendered bytes" || response.Header.Get("Content-Type") != "image/jpeg" {
		t.Fatal("URI renderer result missing")
	}
	excluded := httptest.NewRequest("GET", "/wado?"+raw, nil)
	excluded.Header.Set("Accept", "image/jpeg;q=0, image/*;q=1")
	w := httptest.NewRecorder()
	s.ServeHTTP(w, excluded)
	if w.Code != 406 || calls.Load() != 1 {
		t.Fatal("contentType overrode header q=0 or called renderer")
	}
}
func TestWADOURIAuthGrammarAndUnsupportedOptionsBeforeBackend(t *testing.T) {
	input, _ := preparedTestFixture(t, transfer.ExplicitVRLittleEndian)
	b := preparedTestBackend(input, transfer.ExplicitVRLittleEndian.UID)
	var calls atomic.Int32
	retrieve := b.retrieve
	b.retrieve = func(c context.Context, r RetrieveRequest, y func(RetrievePart) error) error {
		calls.Add(1)
		return retrieve(c, r, y)
	}
	s, _ := newOpenTestServer(t, b, func(o *ServerOptions) { o.EnableWADOURI = true })
	for _, tc := range []struct {
		query, accept string
		want          int
	}{
		{uriTestQuery, "", 406},
		{"requestType=wado&studyUID=1.2&seriesUID=1.3&objectUID=1.4", "application/dicom", 400},
		{"requestType=WADO&studyUID=1.2&seriesUID=1.3", "application/dicom", 400},
		{uriTestQuery + "&studyUID=1.2", "application/dicom", 400},
		{uriTestQuery + "&transferSyntax=*", "application/dicom", 400},
		{uriTestQuery + "&transferSyntax=1.2.840.10008.999", "application/dicom", 406},
		{uriTestQuery + "&windowCenter=1&windowWidth=2", "application/dicom", 400},
		{uriTestQuery + "&rows=1", "image/jpeg", 400},
		{uriTestQuery + "&windowCenter=1", "image/jpeg", 400},
		{uriTestQuery + "&transferSyntax=1.2.840.10008.1.2.1", "image/jpeg", 400},
		{uriTestQuery + "&anonymize=yes", "application/dicom", 501},
		{uriTestQuery + "&region=0,0,1,1", "image/jpeg", 501},
		{uriTestQuery + "&charset=utf-8", "application/dicom", 501},
		{uriTestQuery + "&presentationSeriesUID=1.5&presentationUID=1.6", "image/jpeg", 501},
		{uriTestQuery + "&unknown=ignored", "application/dicom", 400},
		{uriTestQuery + "&frameNumber=%2B1", "image/jpeg", 400},
		{uriTestQuery + "&rows=%2B20&columns=10", "image/jpeg", 400},
		{uriTestQuery + "&imageQuality=%2B90", "image/jpeg", 400},
		{uriTestQuery, "application/dicom; transfer-syntax=1.2.840.10008.1.2.1", 400},
		{uriTestQuery + "&contentType=" + url.QueryEscape("application/dicom;charset=utf-8"), "application/dicom", 400},
		{uriTestQuery + "&contentType=" + url.QueryEscape("application/dicom,image/jpeg"), "*/*", 400},
	} {
		r := httptest.NewRequest("GET", "/wado?"+tc.query, nil)
		if tc.accept != "" {
			r.Header.Set("Accept", tc.accept)
		}
		w := httptest.NewRecorder()
		s.ServeHTTP(w, r)
		if w.Code != tc.want {
			t.Fatalf("URI refusal status=%d want=%d", w.Code, tc.want)
		}
	}
	if calls.Load() != 0 {
		t.Fatal("invalid URI called backend")
	}
	denied, err := NewServer(ServerOptions{Backend: b, EnableWADOURI: true})
	if err != nil {
		t.Fatal(err)
	}
	r := httptest.NewRequest("GET", "/wado?"+uriTestQuery, nil)
	r.Header.Set("Accept", "application/dicom")
	w := httptest.NewRecorder()
	denied.ServeHTTP(w, r)
	if w.Code != 401 || calls.Load() != 0 {
		t.Fatal("URI auth did not precede backend")
	}
}
