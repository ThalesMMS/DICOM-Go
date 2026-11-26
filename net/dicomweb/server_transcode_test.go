package dicomweb

import (
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"io"
	"mime"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/ThalesMMS/dicom-go/core"
	"github.com/ThalesMMS/dicom-go/object"
	"github.com/ThalesMMS/dicom-go/pixeldata"
	"github.com/ThalesMMS/dicom-go/pixeldata/builtin"
	"github.com/ThalesMMS/dicom-go/pixeldata/deflateframe"
	"github.com/ThalesMMS/dicom-go/pixeldata/jpeg"
	"github.com/ThalesMMS/dicom-go/pixeldata/jpeglossless"
	"github.com/ThalesMMS/dicom-go/pixeldata/jpegls"
	"github.com/ThalesMMS/dicom-go/pixeldata/rle"
	"github.com/ThalesMMS/dicom-go/transfer"
)

const preparedInstancePath = "/studies/1.2/series/1.3/instances/1.4"

var preparedTestRef = InstanceRef{StudyInstanceUID: "1.2", SeriesInstanceUID: "1.3", SOPInstanceUID: "1.4"}

func preparedTestOptions(t *testing.T) pixeldata.TranscodeOptions {
	t.Helper()
	decoders, err := builtin.NewRegistry()
	if err != nil {
		t.Fatal(err)
	}
	encoders := pixeldata.NewMemoryEncoderRegistry()
	for _, register := range []func(pixeldata.EncoderRegistry) error{rle.RegisterEncoder, jpegls.RegisterEncoder, jpeglossless.RegisterEncoder, deflateframe.RegisterEncoder, func(r pixeldata.EncoderRegistry) error { return jpeg.RegisterEncoder(r, 90) }} {
		if err := register(encoders); err != nil {
			t.Fatal(err)
		}
	}
	return pixeldata.TranscodeOptions{DecoderRegistry: decoders, EncoderRegistry: encoders}
}
func preparedTestFixture(t *testing.T, syntax transfer.Syntax) ([]byte, []byte) {
	t.Helper()
	var elements []core.Element
	text := func(tag core.Tag, vr core.VR, v string) {
		elements = append(elements, core.Element{Header: core.ElementHeader{Tag: tag, VR: vr}, Value: core.StringValue{v}})
	}
	us := func(tag core.Tag, v uint16) {
		elements = append(elements, core.NewRawElement(tag, core.VRUS, binary.LittleEndian.AppendUint16(nil, v)))
	}
	text(core.NewTag(0x0008, 0x0016), core.VRUI, "1.2.840.10008.5.1.4.1.1.7")
	text(core.NewTag(0x0008, 0x0018), core.VRUI, "1.4")
	text(core.NewTag(0x0020, 0x000D), core.VRUI, "1.2")
	text(core.NewTag(0x0020, 0x000E), core.VRUI, "1.3")
	us(core.NewTag(0x0028, 0x0010), 4)
	us(core.NewTag(0x0028, 0x0011), 8)
	us(core.NewTag(0x0028, 0x0002), 1)
	us(core.NewTag(0x0028, 0x0100), 8)
	us(core.NewTag(0x0028, 0x0101), 8)
	us(core.NewTag(0x0028, 0x0102), 7)
	us(core.NewTag(0x0028, 0x0103), 0)
	text(core.NewTag(0x0028, 0x0004), core.VRCS, "MONOCHROME2")
	text(core.NewTag(0x0028, 0x0008), core.VRIS, "2")
	pixels := make([]byte, 64)
	for i := range pixels {
		pixels[i] = byte(i*7 + 11)
	}
	elements = append(elements, core.NewRawElement(core.TagPixelData, core.VROB, pixels))
	file := &object.File{Dataset: object.FromElements(elements, nil), TransferSyntax: transfer.ExplicitVRLittleEndian}
	if syntax.UID != file.TransferSyntax.UID {
		opts := preparedTestOptions(t)
		if syntax.UID == transfer.JPEGBaseline.UID {
			opts.AllowLossy = true
		}
		var err error
		file, _, err = pixeldata.TranscodeFile(context.Background(), file, syntax, opts)
		if err != nil {
			t.Fatal(err)
		}
		// Controlled fixture construction fixes its identity after generating stored
		// lossy bytes. The HTTP implementation never rewrites a derived identity.
		file.Dataset.Put(core.Element{Header: core.ElementHeader{Tag: core.NewTag(0x0008, 0x0018), VR: core.VRUI}, Value: core.StringValue{"1.4"}})
	}
	var buffer bytes.Buffer
	if err := object.WriteFile(&buffer, file); err != nil {
		t.Fatal(err)
	}
	return buffer.Bytes(), pixels
}
func preparedTestBackend(data []byte, syntax string) *serverTestBackend {
	b := completeServerTestBackend()
	b.retrieve = func(_ context.Context, req RetrieveRequest, yield func(RetrievePart) error) error {
		if len(req.Accept) != 1 || req.Accept[0].TransferSyntaxUID != "*" {
			return ErrBackend
		}
		return yield(RetrievePart{Ref: preparedTestRef, ContentType: "application/dicom", TransferSyntaxUID: syntax, Size: int64(len(data)), Reader: io.NopCloser(bytes.NewReader(data))})
	}
	return b
}
func preparedTestPolicy(t *testing.T, targets ...string) *RetrieveTranscodePolicy {
	return &RetrieveTranscodePolicy{Options: preparedTestOptions(t), TargetTransferSyntaxUIDs: targets}
}
func assertPreparedPixels(t *testing.T, data, pixels []byte, syntax string) {
	t.Helper()
	f, err := object.ReadFile(bytes.NewReader(data))
	if err != nil {
		t.Fatal(err)
	}
	if f.TransferSyntax.UID != syntax {
		t.Fatal("response syntax differs from negotiated syntax")
	}
	uid, _ := f.Dataset.GetString(core.NewTag(0x0008, 0x0018))
	if strings.TrimSpace(uid) != "1.4" {
		t.Fatal("response changed bound SOP identity")
	}
	native, _, err := pixeldata.TranscodeFile(context.Background(), f, transfer.ExplicitVRLittleEndian, preparedTestOptions(t))
	if err != nil {
		t.Fatal(err)
	}
	frames, err := pixeldata.ExtractNativeFrames(native.Dataset)
	if err != nil {
		t.Fatal(err)
	}
	var actual []byte
	for _, frame := range frames.Data {
		actual = append(actual, frame...)
	}
	if !bytes.Equal(actual, pixels) {
		t.Fatal("lossless response changed samples")
	}
}
func TestPreparedRetrieveLosslessMediaMatrixRealHTTP(t *testing.T) {
	for _, syntax := range []transfer.Syntax{transfer.ExplicitVRLittleEndian, transfer.RLELossless, transfer.JPEGLSLossless, transfer.JPEGLosslessSV1, transfer.DeflatedImageFrameCompression, transfer.EncapsulatedUncompressedExplicitVRLittleEndian, transfer.DeflatedExplicitVRLittleEndian} {
		t.Run(syntax.Name, func(t *testing.T) {
			input, pixels := preparedTestFixture(t, transfer.ExplicitVRLittleEndian)
			before := append([]byte(nil), input...)
			_, hs := newOpenTestServer(t, preparedTestBackend(input, transfer.ExplicitVRLittleEndian.UID), func(o *ServerOptions) { o.RetrieveTranscode = preparedTestPolicy(t, syntax.UID) })
			req, _ := http.NewRequest(http.MethodGet, hs.URL+preparedInstancePath, nil)
			req.Header.Set("Accept", mediaTypeWithTransferSyntax("application/dicom", syntax.UID))
			response, err := hs.Client().Do(req)
			if err != nil {
				t.Fatal(err)
			}
			defer response.Body.Close()
			data, err := io.ReadAll(response.Body)
			if err != nil || response.StatusCode != 200 {
				t.Fatalf("HTTP status=%d err=%v", response.StatusCode, err)
			}
			media, params, err := mime.ParseMediaType(response.Header.Get("Content-Type"))
			if err != nil || media != "application/dicom" || params["transfer-syntax"] != syntax.UID {
				t.Fatal("MIME does not declare actual object syntax")
			}
			assertPreparedPixels(t, data, pixels, syntax.UID)
			expectedFile, err := object.ReadFile(bytes.NewReader(data))
			if err != nil {
				t.Fatal(err)
			}
			if !bytes.Equal(input, before) {
				t.Fatal("backend source mutated")
			}
			if syntax.UID == transfer.DeflatedExplicitVRLittleEndian.UID {
				return
			}
			expectedFrames, err := pixeldata.NewFrameReader(context.Background(), expectedFile, bytes.NewReader(data), pixeldata.FrameReaderOptions{})
			if err != nil {
				t.Fatal(err)
			}
			defer expectedFrames.Close()
			fr, _ := http.NewRequest(http.MethodGet, hs.URL+preparedInstancePath+"/frames/2,1", nil)
			fr.Header.Set("Accept", `multipart/related; type="`+frameMediaForSyntax(syntax.UID)+`"; transfer-syntax=`+syntax.UID)
			result, err := hs.Client().Do(fr)
			if err != nil {
				t.Fatal(err)
			}
			defer result.Body.Close()
			if result.StatusCode != 200 {
				t.Fatalf("frame HTTP=%d", result.StatusCode)
			}
			_, outer, _ := mime.ParseMediaType(result.Header.Get("Content-Type"))
			reader := multipart.NewReader(result.Body, outer["boundary"])
			for _, index := range []int{2, 1} {
				part, err := reader.NextPart()
				if err != nil {
					t.Fatal(err)
				}
				data, err := io.ReadAll(part)
				if err != nil || len(data) == 0 {
					t.Fatal("frame empty")
				}
				m, p, _ := mime.ParseMediaType(part.Header.Get("Content-Type"))
				if m != frameMediaForSyntax(syntax.UID) || p["transfer-syntax"] != syntax.UID || !strings.HasSuffix(part.Header.Get("Content-Location"), "/frames/"+string(rune('0'+index))) {
					t.Fatal("frame negotiation/order/binding differs")
				}
				if syntax.UID == transfer.ExplicitVRLittleEndian.UID || syntax.UID == transfer.EncapsulatedUncompressedExplicitVRLittleEndian.UID {
					if !bytes.Equal(data, pixels[(index-1)*32:index*32]) {
						t.Fatal("native frame bytes changed")
					}
				}
				encoded, err := expectedFrames.RawFrame(context.Background(), index-1)
				if err != nil || !bytes.Equal(data, encoded) {
					t.Fatal("HTTP frame differs from the negotiated raw encoded/native representation")
				}
			}
			if _, err := reader.NextPart(); !errors.Is(err, io.EOF) {
				t.Fatal("unexpected extra frame")
			}
		})
	}
}
func TestPreparedRetrieveCompressedDefaultWildcardAndLossyPassThrough(t *testing.T) {
	input, pixels := preparedTestFixture(t, transfer.RLELossless)
	_, hs := newOpenTestServer(t, preparedTestBackend(input, transfer.RLELossless.UID), func(o *ServerOptions) { o.RetrieveTranscode = preparedTestPolicy(t) })
	for _, accept := range []string{"application/dicom", `application/dicom; transfer-syntax=*`, `application/dicom; transfer-syntax=*; q=1, application/dicom; transfer-syntax=1.2.840.10008.1.2.1; q=0`} {
		req, _ := http.NewRequest("GET", hs.URL+preparedInstancePath, nil)
		req.Header.Set("Accept", accept)
		r, err := hs.Client().Do(req)
		if err != nil {
			t.Fatal(err)
		}
		data, _ := io.ReadAll(r.Body)
		r.Body.Close()
		if r.StatusCode != 200 {
			t.Fatal(r.StatusCode)
		}
		syntax := transfer.ExplicitVRLittleEndian.UID
		if strings.Contains(accept, "q=0") {
			syntax = transfer.RLELossless.UID
		}
		assertPreparedPixels(t, data, pixels, syntax)
	}
	lossy, _ := preparedTestFixture(t, transfer.JPEGBaseline)
	_, lh := newOpenTestServer(t, preparedTestBackend(lossy, transfer.JPEGBaseline.UID), func(o *ServerOptions) { o.RetrieveTranscode = &RetrieveTranscodePolicy{} })
	req, _ := http.NewRequest("GET", lh.URL+preparedInstancePath, nil)
	req.Header.Set("Accept", "application/dicom")
	r, err := lh.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	data, _ := io.ReadAll(r.Body)
	r.Body.Close()
	if r.StatusCode != 200 || !bytes.Equal(lossy, data) {
		t.Fatal("stored lossy representation did not pass through")
	}
}

func TestPreparedFramesMIMEDefaultsAndEncodedBytesRealHTTP(t *testing.T) {
	input, _ := preparedTestFixture(t, transfer.ExplicitVRLittleEndian)
	for _, syntax := range []transfer.Syntax{transfer.ExplicitVRLittleEndian, transfer.RLELossless, transfer.JPEGLSLossless, transfer.JPEGLosslessSV1, transfer.DeflatedImageFrameCompression} {
		t.Run(syntax.Name, func(t *testing.T) {
			expected, _ := preparedTestFixture(t, syntax)
			file, err := object.ReadFile(bytes.NewReader(expected))
			if err != nil {
				t.Fatal(err)
			}
			frames, err := pixeldata.NewFrameReader(context.Background(), file, bytes.NewReader(expected), pixeldata.FrameReaderOptions{})
			if err != nil {
				t.Fatal(err)
			}
			defer frames.Close()
			encoded, err := frames.RawFrame(context.Background(), 0)
			if err != nil {
				t.Fatal(err)
			}
			_, hs := newOpenTestServer(t, preparedTestBackend(input, transfer.ExplicitVRLittleEndian.UID), func(o *ServerOptions) { o.RetrieveTranscode = preparedTestPolicy(t, syntax.UID) })
			r, _ := http.NewRequest("GET", hs.URL+preparedInstancePath+"/frames/1", nil)
			r.Header.Set("Accept", `multipart/related; type="`+frameMediaForSyntax(syntax.UID)+`"`)
			response, err := hs.Client().Do(r)
			if err != nil {
				t.Fatal(err)
			}
			defer response.Body.Close()
			if response.StatusCode != 200 {
				t.Fatal(response.StatusCode)
			}
			_, params, _ := mime.ParseMediaType(response.Header.Get("Content-Type"))
			part, err := multipart.NewReader(response.Body, params["boundary"]).NextPart()
			if err != nil {
				t.Fatal(err)
			}
			_, frameParams, _ := mime.ParseMediaType(part.Header.Get("Content-Type"))
			actual, err := io.ReadAll(part)
			if err != nil || frameParams["transfer-syntax"] != syntax.UID || !bytes.Equal(actual, encoded) {
				t.Fatal("MIME default did not return the matching raw encoded representation")
			}
		})
	}
}

func TestPreparedRetrieveNegotiatesAvailableVerifiedCodecAndFrameExclusion(t *testing.T) {
	input, pixels := preparedTestFixture(t, transfer.ExplicitVRLittleEndian)
	policy := preparedTestPolicy(t, transfer.RLELossless.UID, transfer.ExplicitVRLittleEndian.UID)
	policy.Options.DecoderRegistry = nil // RLE encoder exists, but output verification cannot decode it.
	_, hs := newOpenTestServer(t, preparedTestBackend(input, transfer.ExplicitVRLittleEndian.UID), func(o *ServerOptions) { o.RetrieveTranscode = policy })
	r, _ := http.NewRequest("GET", hs.URL+preparedInstancePath, nil)
	r.Header.Set("Accept", mediaTypeWithTransferSyntax("application/dicom", transfer.RLELossless.UID)+`; q=1, `+mediaTypeWithTransferSyntax("application/dicom", transfer.ExplicitVRLittleEndian.UID)+`; q=0.5`)
	response, err := hs.Client().Do(r)
	if err != nil {
		t.Fatal(err)
	}
	data, err := io.ReadAll(response.Body)
	response.Body.Close()
	if err != nil || response.StatusCode != 200 {
		t.Fatal("available lower-quality representation was not negotiated")
	}
	assertPreparedPixels(t, data, pixels, transfer.ExplicitVRLittleEndian.UID)
	s, _ := newOpenTestServer(t, preparedTestBackend(input, transfer.ExplicitVRLittleEndian.UID), func(o *ServerOptions) {
		o.RetrieveTranscode = preparedTestPolicy(t, transfer.JPEGLosslessSV1.UID, transfer.RLELossless.UID)
	})
	q := httptest.NewRequest("GET", preparedInstancePath+"/frames/1", nil)
	q.Header.Set("Accept", `multipart/related; type="image/*"; q=1, multipart/related; type="image/jpeg"; q=0`)
	w := httptest.NewRecorder()
	s.ServeHTTP(w, q)
	_, params, err := mime.ParseMediaType(w.Header().Get("Content-Type"))
	if err != nil || w.Code != 200 || params["type"] != "image/dicom-rle" {
		t.Fatal("frame MIME q=0 exclusion was ignored")
	}
}

func TestPreparedFramesStoredLossyMIMEExclusionAndExplicitOverride(t *testing.T) {
	input, _ := preparedTestFixture(t, transfer.JPEGBaseline)
	_, hs := newOpenTestServer(t, preparedTestBackend(input, transfer.JPEGBaseline.UID), func(o *ServerOptions) { o.RetrieveTranscode = &RetrieveTranscodePolicy{} })
	for _, tc := range []struct {
		name, accept string
		want         int
	}{
		{"generic exclusion", `multipart/related; type="image/*"; q=1, multipart/related; type="image/jpeg"; q=0`, 406},
		{"explicit override", `multipart/related; type="image/*"; q=1, multipart/related; type="image/jpeg"; q=0, multipart/related; type="image/jpeg"; transfer-syntax=1.2.840.10008.1.2.4.50; q=1`, 200},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r, _ := http.NewRequest("GET", hs.URL+preparedInstancePath+"/frames/1", nil)
			r.Header.Set("Accept", tc.accept)
			response, err := hs.Client().Do(r)
			if err != nil {
				t.Fatal(err)
			}
			defer response.Body.Close()
			_, err = io.Copy(io.Discard, response.Body)
			if err != nil || response.StatusCode != tc.want {
				t.Fatalf("stored lossy MIME exclusion status=%d want=%d err=%v", response.StatusCode, tc.want, err)
			}
		})
	}
}

func TestPreparedRetrieveBackendLateFailureAndActualUIDBinding(t *testing.T) {
	input, _ := preparedTestFixture(t, transfer.ExplicitVRLittleEndian)
	for _, mode := range []string{"late error", "duplicate", "wrong file UID"} {
		t.Run(mode, func(t *testing.T) {
			data := input
			if mode == "wrong file UID" {
				file, err := object.ReadFile(bytes.NewReader(input))
				if err != nil {
					t.Fatal(err)
				}
				file.Dataset.Put(core.Element{Header: core.ElementHeader{Tag: core.NewTag(0x0008, 0x0018), VR: core.VRUI}, Value: core.StringValue{"1.5"}})
				var encoded bytes.Buffer
				if err := object.WriteFile(&encoded, file); err != nil {
					t.Fatal(err)
				}
				data = encoded.Bytes()
			}
			b := completeServerTestBackend()
			var closes atomic.Int32
			b.retrieve = func(_ context.Context, _ RetrieveRequest, yield func(RetrievePart) error) error {
				part := func() RetrievePart {
					return RetrievePart{Reader: &bulkOwnedReader{Reader: bytes.NewReader(data), closes: &closes}, Ref: preparedTestRef, Size: int64(len(data))}
				}
				if err := yield(part()); err != nil {
					return err
				}
				if mode == "duplicate" {
					_ = yield(part())
					return nil
				}
				return ErrBackend
			}
			dir := t.TempDir()
			s, _ := newOpenTestServer(t, b, func(o *ServerOptions) { o.SpoolDirectory = dir; o.RetrieveTranscode = preparedTestPolicy(t) })
			r := httptest.NewRequest("GET", preparedInstancePath, nil)
			r.Header.Set("Accept", "application/dicom")
			w := httptest.NewRecorder()
			s.ServeHTTP(w, r)
			if w.Code != 500 || bytes.Contains(w.Body.Bytes(), []byte("DICM")) {
				t.Fatal("failed instance response was published")
			}
			want := int32(1)
			if mode == "duplicate" {
				want = 2
			}
			if closes.Load() != want {
				t.Fatal("failed reader ownership")
			}
			entries, _ := os.ReadDir(dir)
			if len(entries) != 0 {
				t.Fatal("failed job leaked staging")
			}
		})
	}
}

// Patch a controlled Explicit VR fixture after serialization so the fixture
// writer's File Meta reconciliation cannot hide malformed identity fields.
func preparedPatchUID(t *testing.T, input []byte, tag core.Tag, extra, vr string) []byte {
	t.Helper()
	key := binary.LittleEndian.AppendUint16(nil, tag.Group)
	key = binary.LittleEndian.AppendUint16(key, tag.Element)
	key = append(key, 'U', 'I')
	index := bytes.Index(input, key)
	if index < 0 {
		t.Fatal("fixture UID tag absent")
	}
	length := int(binary.LittleEndian.Uint16(input[index+6:]))
	if extra == "duplicate" {
		duplicate := append([]byte(nil), input[index:index+8]...)
		binary.LittleEndian.PutUint16(duplicate[6:], 4)
		duplicate = append(duplicate, '9', '.', '9', 0)
		result := append([]byte(nil), input[:index]...)
		result = append(result, duplicate...)
		result = append(result, input[index:]...)
		if tag.Group == 2 {
			old := binary.LittleEndian.Uint32(result[140:144])
			binary.LittleEndian.PutUint32(result[140:144], old+uint32(len(duplicate)))
		}
		return result
	}
	value := append([]byte(nil), bytes.TrimRight(input[index+8:index+8+length], " \x00")...)
	if extra == "leading" {
		value = append([]byte(" "), value...)
	} else {
		value = append(value, extra...)
	}
	if len(value)%2 != 0 {
		value = append(value, 0)
	}
	result := append([]byte(nil), input[:index+8]...)
	binary.LittleEndian.PutUint16(result[index+6:], uint16(len(value)))
	if vr != "" {
		copy(result[index+4:], vr)
	}
	result = append(result, value...)
	result = append(result, input[index+8+length:]...)
	if tag.Group == 2 {
		old := binary.LittleEndian.Uint32(result[140:144])
		binary.LittleEndian.PutUint32(result[140:144], old+uint32(len(value)-length))
	}
	return result
}

func TestPreparedRetrieveRequiresWholeSingleUIIdentity(t *testing.T) {
	input, _ := preparedTestFixture(t, transfer.ExplicitVRLittleEndian)
	for _, tc := range []struct {
		name      string
		tag       core.Tag
		extra, vr string
	}{
		{"dataset study VM", core.NewTag(0x0020, 0x000d), "\\9.9", ""},
		{"dataset series VM", core.NewTag(0x0020, 0x000e), "\\9.9", ""},
		{"dataset SOP VM", core.NewTag(0x0008, 0x0018), "\\9.9", ""},
		{"dataset class VM", core.NewTag(0x0008, 0x0016), "\\9.9", ""},
		{"meta SOP VM", core.NewTag(0x0002, 0x0003), "\\9.9", ""},
		{"meta class VM", core.NewTag(0x0002, 0x0002), "\\9.9", ""},
		{"meta syntax VM", core.NewTag(0x0002, 0x0010), "\\9.9", ""},
		{"wrong UID VR", core.NewTag(0x0008, 0x0018), "", "LO"},
		{"leading UID whitespace", core.NewTag(0x0008, 0x0018), "leading", ""},
		{"dataset SOP duplicate", core.NewTag(0x0008, 0x0018), "duplicate", ""},
		{"meta SOP duplicate", core.NewTag(0x0002, 0x0003), "duplicate", ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			data := preparedPatchUID(t, input, tc.tag, tc.extra, tc.vr)
			s, _ := newOpenTestServer(t, preparedTestBackend(data, transfer.ExplicitVRLittleEndian.UID), func(o *ServerOptions) { o.RetrieveTranscode = &RetrieveTranscodePolicy{} })
			r := httptest.NewRequest("GET", preparedInstancePath, nil)
			r.Header.Set("Accept", "application/dicom")
			w := httptest.NewRecorder()
			s.ServeHTTP(w, r)
			if w.Code != 500 || bytes.Contains(w.Body.Bytes(), []byte("DICM")) {
				t.Fatalf("malformed UID identity published status=%d", w.Code)
			}
		})
	}
}

func TestPreparedRetrieveUIDScanKeepsNestedIdentitiesSeparate(t *testing.T) {
	input, _ := preparedTestFixture(t, transfer.ExplicitVRLittleEndian)
	file, err := object.ReadFile(bytes.NewReader(input))
	if err != nil {
		t.Fatal(err)
	}
	file.Dataset.Put(core.Element{Header: core.ElementHeader{Tag: core.NewTag(0x0008, 0x1110), VR: core.VRSQ}, Value: core.SequenceValue{Items: []core.DataSet{{Elements: []core.Element{core.NewRawElement(core.NewTag(0x0008, 0x0018), core.VRUI, []byte("9.9\\9.8"))}}}}})
	var encoded bytes.Buffer
	if err := object.WriteFile(&encoded, file); err != nil {
		t.Fatal(err)
	}
	data := encoded.Bytes()
	s, _ := newOpenTestServer(t, preparedTestBackend(data, transfer.ExplicitVRLittleEndian.UID), func(o *ServerOptions) { o.RetrieveTranscode = &RetrieveTranscodePolicy{} })
	r := httptest.NewRequest("GET", preparedInstancePath, nil)
	r.Header.Set("Accept", "application/dicom")
	w := httptest.NewRecorder()
	s.ServeHTTP(w, r)
	if w.Code != 200 || !bytes.Equal(w.Body.Bytes(), data) {
		t.Fatal("nested item UID was confused with routed instance identity")
	}
}

func TestPreparedRetrieveAdmissionCancellationBeforeSourceRead(t *testing.T) {
	input, _ := preparedTestFixture(t, transfer.ExplicitVRLittleEndian)
	dir := t.TempDir()
	s, _ := newOpenTestServer(t, preparedTestBackend(input, transfer.ExplicitVRLittleEndian.UID), func(o *ServerOptions) {
		o.SpoolDirectory = dir
		o.RetrieveTranscode = preparedTestPolicy(t)
		o.RetrieveTranscode.MaxConcurrentJobs = 1
	})
	req := RetrieveRequest{Level: RetrieveLevelInstance, StudyInstanceUID: "1.2", SeriesInstanceUID: "1.3", SOPInstanceUID: "1.4", Accept: []MediaPreference{{MediaType: "application/dicom", Quality: 1}}}
	entered, release, done := make(chan struct{}), make(chan struct{}), make(chan error, 1)
	first := &preparedCountReader{Reader: bytes.NewReader(input)}
	go func() {
		done <- s.prepareRetrievePart(context.Background(), req, RetrievePart{Reader: first, Ref: preparedTestRef, Size: int64(len(input))}, false, false, func(*preparedRetrieve) error { close(entered); <-release; return nil })
	}()
	select {
	case <-entered:
	case <-time.After(5 * time.Second):
		t.Fatal("first preparation did not enter")
	}
	second := &preparedCountReader{Reader: bytes.NewReader(input)}
	ctx, cancel := context.WithCancel(context.Background())
	secondDone := make(chan error, 1)
	go func() {
		secondDone <- s.prepareRetrievePart(ctx, req, RetrievePart{Reader: second, Ref: preparedTestRef, Size: int64(len(input))}, false, false, func(*preparedRetrieve) error { t.Error("canceled queued preparation entered"); return nil })
	}()
	cancel()
	select {
	case err := <-secondDone:
		if !errors.Is(err, context.Canceled) {
			t.Fatal(err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("admission cancellation blocked")
	}
	close(release)
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	if first.closes.Load() != 1 || second.closes.Load() != 1 || second.reads.Load() != 0 {
		t.Fatal("queued source read or ownership leak")
	}
	entries, _ := os.ReadDir(dir)
	if len(entries) != 0 {
		t.Fatal("preparation cancellation leaked staging")
	}
}

type preparedCancelReader struct {
	ctx    context.Context
	cancel context.CancelFunc
	closes atomic.Int32
}

func (r *preparedCancelReader) Read([]byte) (int, error) { r.cancel(); return 0, r.ctx.Err() }
func (r *preparedCancelReader) Close() error             { r.closes.Add(1); return nil }
func TestPreparedRetrieveSourceCancellationCleansStaging(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	spy := &preparedCancelReader{ctx: ctx, cancel: cancel}
	b := completeServerTestBackend()
	b.retrieve = func(_ context.Context, _ RetrieveRequest, y func(RetrievePart) error) error {
		return y(RetrievePart{Reader: spy, Ref: preparedTestRef, Size: -1})
	}
	dir := t.TempDir()
	s, _ := newOpenTestServer(t, b, func(o *ServerOptions) { o.SpoolDirectory = dir; o.RetrieveTranscode = &RetrieveTranscodePolicy{} })
	r := httptest.NewRequest("GET", preparedInstancePath, nil).WithContext(ctx)
	r.Header.Set("Accept", "application/dicom")
	w := httptest.NewRecorder()
	s.ServeHTTP(w, r)
	if w.Code != 499 || spy.closes.Load() != 1 {
		t.Fatal("source cancellation not preserved")
	}
	entries, _ := os.ReadDir(dir)
	if len(entries) != 0 {
		t.Fatal("canceled source staging leaked")
	}
}

type preparedCountReader struct {
	io.Reader
	reads, closes atomic.Int32
}

func (r *preparedCountReader) Read(p []byte) (int, error) { r.reads.Add(1); return r.Reader.Read(p) }
func (r *preparedCountReader) Close() error               { r.closes.Add(1); return nil }
func TestPreparedRetrieveRefusalsLimitsAndIgnoredYield(t *testing.T) {
	input, _ := preparedTestFixture(t, transfer.ExplicitVRLittleEndian)
	for _, tc := range []struct {
		name   string
		size   int64
		ref    InstanceRef
		syntax string
		accept string
		limit  int64
		want   int
	}{
		{"oversized", int64(len(input)), preparedTestRef, transfer.ExplicitVRLittleEndian.UID, "application/dicom", 16, 413},
		{"wrong ref", int64(len(input)), InstanceRef{StudyInstanceUID: "1.9"}, transfer.ExplicitVRLittleEndian.UID, "application/dicom", 0, 500},
		{"short declaration", int64(len(input) - 2), preparedTestRef, transfer.ExplicitVRLittleEndian.UID, "application/dicom", 0, 500},
		{"lying syntax", int64(len(input)), preparedTestRef, transfer.RLELossless.UID, "application/dicom", 0, 500},
		{"new lossy", int64(len(input)), preparedTestRef, transfer.ExplicitVRLittleEndian.UID, mediaTypeWithTransferSyntax("application/dicom", transfer.JPEGBaseline.UID), 0, 406},
		{"q zero", int64(len(input)), preparedTestRef, transfer.ExplicitVRLittleEndian.UID, "application/dicom;q=0", 0, 406},
	} {
		t.Run(tc.name, func(t *testing.T) {
			b := completeServerTestBackend()
			spy := &preparedCountReader{Reader: bytes.NewReader(input)}
			b.retrieve = func(_ context.Context, _ RetrieveRequest, y func(RetrievePart) error) error {
				_ = y(RetrievePart{Reader: spy, Size: tc.size, Ref: tc.ref, TransferSyntaxUID: tc.syntax})
				return nil
			}
			dir := t.TempDir()
			s, _ := newOpenTestServer(t, b, func(o *ServerOptions) {
				o.SpoolDirectory = dir
				o.RetrieveTranscode = preparedTestPolicy(t, transfer.JPEGBaseline.UID)
				o.Limits.MaxPartBytes = tc.limit
			})
			r := httptest.NewRequest("GET", preparedInstancePath, nil)
			r.Header.Set("Accept", tc.accept)
			w := httptest.NewRecorder()
			s.ServeHTTP(w, r)
			if w.Code != tc.want {
				t.Fatalf("status=%d want=%d", w.Code, tc.want)
			}
			if tc.want == 413 || tc.name == "wrong ref" {
				if spy.reads.Load() != 0 {
					t.Fatal("refused source read")
				}
			}
			entries, _ := os.ReadDir(dir)
			if len(entries) != 0 {
				t.Fatal("spool leaked")
			}
			if tc.name != "q zero" && spy.closes.Load() != 1 {
				t.Fatal("source ownership not closed")
			}
		})
	}
}
func TestPreparedRetrieveUnsupportedFrameMediaMatrix(t *testing.T) {
	input, _ := preparedTestFixture(t, transfer.ExplicitVRLittleEndian)
	for _, tc := range []struct {
		media  string
		syntax transfer.Syntax
	}{
		{"image/jpeg", transfer.JPEGBaseline}, {"image/jp2", transfer.JPEG2000LosslessOnly}, {"image/jpx", transfer.JPEG2000Part2Lossless}, {"image/jphc", transfer.HTJ2KLossless}, {"image/jxl", transfer.JPEGXLLossless}, {"video/mpeg", transfer.MPEG2MPML}, {"video/mp4", transfer.MPEG4HP41}, {"video/h265", transfer.HEVCMP51},
	} {
		t.Run(tc.media, func(t *testing.T) {
			s, _ := newOpenTestServer(t, preparedTestBackend(input, transfer.ExplicitVRLittleEndian.UID), func(o *ServerOptions) { o.RetrieveTranscode = preparedTestPolicy(t, tc.syntax.UID) })
			r := httptest.NewRequest("GET", preparedInstancePath+"/frames/1", nil)
			r.Header.Set("Accept", `multipart/related; type="`+tc.media+`"; transfer-syntax=`+tc.syntax.UID)
			w := httptest.NewRecorder()
			s.ServeHTTP(w, r)
			if w.Code != 406 {
				t.Fatalf("unsupported/new lossy codec media status=%d", w.Code)
			}
		})
	}
}
func TestPreparedPolicyOwnsOptionsAndRejectsUnsafePolicies(t *testing.T) {
	for _, mutate := range []func(*pixeldata.TranscodeOptions){func(o *pixeldata.TranscodeOptions) { o.AllowLossy = true }, func(o *pixeldata.TranscodeOptions) { o.SkipVerification = true }, func(o *pixeldata.TranscodeOptions) { o.UIDPolicy = pixeldata.TranscodeUIDAlways }, func(o *pixeldata.TranscodeOptions) { o.ForceReencode = true }, func(o *pixeldata.TranscodeOptions) { o.SecondaryCapture = true }, func(o *pixeldata.TranscodeOptions) { o.Recovery.RLESegmentOverrun = true }} {
		p := &RetrieveTranscodePolicy{}
		mutate(&p.Options)
		if _, err := NewServer(ServerOptions{Backend: completeServerTestBackend(), RetrieveTranscode: p}); !errors.Is(err, ErrInvalidServerOptions) {
			t.Fatal("unsafe retrieval options accepted")
		}
	}
	p := preparedTestPolicy(t, transfer.RLELossless.UID)
	p.Options.WriteOptions.Preamble = make([]byte, 128)
	s, err := NewServer(ServerOptions{Backend: completeServerTestBackend(), RetrieveTranscode: p})
	if err != nil {
		t.Fatal(err)
	}
	p.TargetTransferSyntaxUIDs[0] = transfer.JPEGBaseline.UID
	p.Options.WriteOptions.Preamble[0] = 9
	if s.retrievePolicy.TargetTransferSyntaxUIDs[0] != transfer.RLELossless.UID || s.retrievePolicy.Options.WriteOptions.Preamble[0] != 0 {
		t.Fatal("policy aliases mutable caller options")
	}
}
func TestPreparedRetrieveJobScavengeBoundsAndActiveOwnership(t *testing.T) {
	root := t.TempDir()
	now := time.Now()
	old := now.Add(-48 * time.Hour)
	for _, kind := range []string{"complete", "unknown", "active", "oversize"} {
		dir := filepath.Join(root, ".dicomweb-retrieve-"+kind)
		os.Mkdir(dir, 0700)
		os.WriteFile(filepath.Join(dir, "source.dcm"), []byte("owned"), 0600)
		if kind == "unknown" {
			os.WriteFile(filepath.Join(dir, "keep"), []byte("other"), 0600)
		}
		if kind == "oversize" {
			for i := 0; i < 17; i++ {
				os.WriteFile(filepath.Join(dir, strings.Repeat("a", i+1)), nil, 0600)
			}
		}
		os.Chtimes(dir, old, old)
		if kind == "active" {
			activeRetrieveDirectories.Store(retrieveDirectoryKey(dir), struct{}{})
			defer activeRetrieveDirectories.Delete(retrieveDirectoryKey(dir))
		}
	}
	opts := ServerOptions{SpoolDirectory: root, SpoolRetentionAge: 24 * time.Hour, SpoolAggregateQuotaBytes: 1}
	if err := scavengeDICOMwebSpool(opts, DefaultServerLimits(), now); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(root, ".dicomweb-retrieve-complete")); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("stale owned job retained")
	}
	for _, kind := range []string{"unknown", "active", "oversize"} {
		if _, err := os.Stat(filepath.Join(root, ".dicomweb-retrieve-"+kind)); err != nil {
			t.Fatal("unsafe/active recovery deletion")
		}
	}
}

func TestPreparedPolicyRejectsInvalidConfigurationBeforeCloneOrBackend(t *testing.T) {
	for _, o := range []pixeldata.TranscodeOptions{
		{WriteOptions: object.WriteFileOptions{Preamble: make([]byte, 129)}},
		{WriteOptions: object.WriteFileOptions{SequenceLength: 99}},
		{WriteOptions: object.WriteFileOptions{GroupLength: 99}},
		{WriteOptions: object.WriteFileOptions{MetaMode: -1}},
		{WriteOptions: object.WriteFileOptions{AETitles: object.FileMetaAETitles{Source: "bad\\title"}}},
		{UIDPolicy: 99}, {PlanarOutput: 99}, {OffsetTable: 99}, {Durability: 99},
		{MaxFragmentBytes: 3}, {MaxFragmentBytes: 1 << 32},
		{Pipeline: pixeldata.TranscodePipelineOptions{Workers: -1}},
		{Pipeline: pixeldata.TranscodePipelineOptions{MaxInFlightBytes: -1}},
	} {
		_, err := NewServer(ServerOptions{Backend: completeServerTestBackend(), RetrieveTranscode: &RetrieveTranscodePolicy{Options: o}})
		if !errors.Is(err, ErrInvalidServerOptions) {
			t.Fatal("invalid policy accepted")
		}
	}
	level := 10
	_, err := NewServer(ServerOptions{Backend: completeServerTestBackend(), RetrieveTranscode: &RetrieveTranscodePolicy{Options: pixeldata.TranscodeOptions{WriteOptions: object.WriteFileOptions{DeflateLevel: &level}}}})
	if !errors.Is(err, ErrInvalidServerOptions) {
		t.Fatal("invalid Deflate level accepted")
	}
}
