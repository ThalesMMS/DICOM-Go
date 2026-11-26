package dicomweb

import (
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"io"
	"mime"
	"net/http"
	"strconv"
	"testing"

	"github.com/ThalesMMS/dicom-go/core"
	"github.com/ThalesMMS/dicom-go/object"
	"github.com/ThalesMMS/dicom-go/pixeldata"
	"github.com/ThalesMMS/dicom-go/transfer"
)

// boundTestObject is a 16-bit monochrome object in syntax whose rows each
// hold one value, so lossless compression shrinks it several times.
func boundTestObject(t *testing.T, syntax transfer.Syntax, sop string, rows, columns, frames int) []byte {
	t.Helper()
	return boundTestObjectOf(t, syntax, sop, rows, columns, frames, func(frame, row int) uint16 { return uint16((frame*rows + row) % 4096) })
}

// boundTestObjectOf is boundTestObject whose rows hold value(frame, row).
func boundTestObjectOf(t *testing.T, syntax transfer.Syntax, sop string, rows, columns, frames int, value func(frame, row int) uint16) []byte {
	t.Helper()
	var elements []core.Element
	text := func(tag core.Tag, vr core.VR, v string) {
		elements = append(elements, core.Element{Header: core.ElementHeader{Tag: tag, VR: vr}, Value: core.StringValue{v}})
	}
	us := func(tag core.Tag, v int) {
		elements = append(elements, core.NewRawElement(tag, core.VRUS, binary.LittleEndian.AppendUint16(nil, uint16(v))))
	}
	text(core.NewTag(0x0008, 0x0016), core.VRUI, "1.2.840.10008.5.1.4.1.1.7")
	text(core.NewTag(0x0008, 0x0018), core.VRUI, sop)
	text(core.NewTag(0x0020, 0x000D), core.VRUI, "1.2")
	text(core.NewTag(0x0020, 0x000E), core.VRUI, "1.3")
	us(core.NewTag(0x0028, 0x0010), rows)
	us(core.NewTag(0x0028, 0x0011), columns)
	us(core.NewTag(0x0028, 0x0002), 1)
	us(core.NewTag(0x0028, 0x0100), 16)
	us(core.NewTag(0x0028, 0x0101), 12)
	us(core.NewTag(0x0028, 0x0102), 11)
	us(core.NewTag(0x0028, 0x0103), 0)
	text(core.NewTag(0x0028, 0x0004), core.VRCS, "MONOCHROME2")
	text(core.NewTag(0x0028, 0x0008), core.VRIS, strconv.Itoa(frames))
	pixels := make([]byte, 0, rows*columns*frames*2)
	for frame := range frames {
		for row := range rows {
			for range columns {
				pixels = binary.LittleEndian.AppendUint16(pixels, value(frame, row))
			}
		}
	}
	elements = append(elements, core.NewRawElement(core.TagPixelData, core.VROW, pixels))
	file := &object.File{Dataset: object.FromElements(elements, nil), TransferSyntax: transfer.ExplicitVRLittleEndian}
	if syntax.UID != file.TransferSyntax.UID {
		var err error
		file, _, err = pixeldata.TranscodeFile(context.Background(), file, syntax, preparedTestOptions(t))
		if err != nil {
			t.Fatal(err)
		}
	}
	var buffer bytes.Buffer
	if err := object.WriteFile(&buffer, file); err != nil {
		t.Fatal(err)
	}
	return buffer.Bytes()
}

type boundTestSource struct{ *bytes.Reader }

func (boundTestSource) Close() error { return nil }

// boundTestEntry is one stored object of a SourceOutputBound test.
type boundTestEntry struct {
	sop    string
	syntax transfer.Syntax
	data   []byte
}

// budgetingBackend answers like an archive that checks every object of the
// selection against MaxPartBytes and the MaxResponseBytes total with
// SourceOutputBound before yielding the first part, and records each bound
// and how many objects it opened to compute them.
func budgetingBackend(objects []boundTestEntry, limits ServerLimits, bounds map[string]int64, opened *int) *serverTestBackend {
	backend := completeServerTestBackend()
	backend.retrieve = func(ctx context.Context, req RetrieveRequest, yield func(RetrievePart) error) error {
		selected := objects
		if req.SOPInstanceUID != "" {
			selected = nil
			for _, o := range objects {
				if o.sop == req.SOPInstanceUID {
					selected = append(selected, o)
				}
			}
		}
		remaining := limits.MaxResponseBytes - int64(len(selected))*1024
		for _, o := range selected {
			data := o.data
			bound, err := req.SourceOutputBound(ctx, o.syntax.UID, int64(len(data)), func() (io.ReadSeekCloser, error) {
				*opened++
				return boundTestSource{bytes.NewReader(data)}, nil
			})
			if err != nil {
				return err
			}
			bounds[o.sop] = bound
			if bound > limits.MaxPartBytes || bound > remaining {
				return ErrResourceLimit
			}
			remaining -= bound
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

func boundTestPolicy(t *testing.T) *RetrieveTranscodePolicy {
	t.Helper()
	policy := preparedDecoderPolicy(t)
	policy.PreferStoredSyntax = true
	return policy
}

func boundTestGet(t *testing.T, url, accept string) (int, string, []byte) {
	t.Helper()
	request, _ := http.NewRequest(http.MethodGet, url, nil)
	request.Header.Set("Accept", accept)
	response, err := http.DefaultClient.Do(request)
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	body, err := io.ReadAll(response.Body)
	if err != nil {
		t.Fatalf("%s with %s: %v", url, accept, err)
	}
	return response.StatusCode, response.Header.Get("Content-Type"), body
}

// TestSourceOutputBoundCoversDeliveredObject retrieves objects stored in
// the syntaxes prepared retrieval converts and checks that the bound the
// backend computed before yielding covers the delivered object, and equals
// the stored size, without opening the object, when the stored bytes pass.
func TestSourceOutputBoundCoversDeliveredObject(t *testing.T) {
	for _, syntax := range []transfer.Syntax{transfer.ImplicitVRLittleEndian, transfer.ExplicitVRBigEndian, transfer.ExplicitVRLittleEndian, transfer.DeflatedExplicitVRLittleEndian,
		transfer.RLELossless, transfer.JPEGLSLossless, transfer.JPEGLosslessSV1, transfer.EncapsulatedUncompressedExplicitVRLittleEndian} {
		t.Run(syntax.Name, func(t *testing.T) {
			stored := boundTestObject(t, syntax, "1.4", 64, 96, 3)
			objects := []boundTestEntry{{sop: "1.4", syntax: syntax, data: stored}}
			limits := DefaultServerLimits()
			bounds, opened := map[string]int64{}, 0
			_, hs := newOpenTestServer(t, budgetingBackend(objects, limits, bounds, &opened), func(o *ServerOptions) { o.RetrieveTranscode = boundTestPolicy(t) })
			for _, accept := range []string{"application/dicom", "application/dicom; transfer-syntax=*", "application/dicom; transfer-syntax=" + transfer.ExplicitVRLittleEndian.UID} {
				opened = 0
				status, _, body := boundTestGet(t, hs.URL+preparedInstancePath, accept)
				if status != http.StatusOK {
					t.Fatalf("%s: HTTP %d", accept, status)
				}
				bound := bounds["1.4"]
				if bytes.Equal(body, stored) {
					if bound != int64(len(stored)) || opened != 0 {
						t.Errorf("%s delivered the stored %d bytes; bound %d after opening %d objects, want the stored size unopened", accept, len(stored), bound, opened)
					}
					continue
				}
				t.Logf("%s: stored %d bytes, delivered %d, bound %d", accept, len(stored), len(body), bound)
				if int64(len(body)) > bound {
					t.Errorf("%s delivered %d bytes above the bound %d", accept, len(body), bound)
				}
				if opened != 1 {
					t.Errorf("%s converted the object after opening it %d times to bound it", accept, opened)
				}
			}
		})
	}
}

// TestSourceOutputBoundCoversHeaderGrowth converts an Implicit VR object
// whose headers are mostly empty items of defined length: each grows from
// eight bytes to an item and its delimiter, sixteen bytes, in Explicit VR
// with undefined-length sequences.
func TestSourceOutputBoundCoversHeaderGrowth(t *testing.T) {
	file, err := object.ReadFile(bytes.NewReader(boundTestObject(t, transfer.ImplicitVRLittleEndian, "1.4", 2, 2, 1)))
	if err != nil {
		t.Fatal(err)
	}
	items := make([]core.DataSet, 2000)
	file.Dataset.Put(core.Element{Header: core.ElementHeader{Tag: core.NewTag(0x0008, 0x1140), VR: core.VRSQ}, Value: core.SequenceValue{Items: items}})
	var buffer bytes.Buffer
	if err := object.WriteFileWithOptions(&buffer, file, object.WriteFileOptions{SequenceLength: object.SequenceLengthDefined}); err != nil {
		t.Fatal(err)
	}
	stored := buffer.Bytes()
	bounds, opened := map[string]int64{}, 0
	_, hs := newOpenTestServer(t, budgetingBackend([]boundTestEntry{{"1.4", transfer.ImplicitVRLittleEndian, stored}}, DefaultServerLimits(), bounds, &opened), func(o *ServerOptions) { o.RetrieveTranscode = boundTestPolicy(t) })
	status, _, body := boundTestGet(t, hs.URL+preparedInstancePath, "application/dicom")
	if status != http.StatusOK || bytes.Equal(body, stored) {
		t.Fatalf("HTTP %d, stored bytes %t; want the converted object", status, bytes.Equal(body, stored))
	}
	t.Logf("stored %d bytes, delivered %d, bound %d", len(stored), len(body), bounds["1.4"])
	if int64(len(body)) > bounds["1.4"] {
		t.Fatalf("delivered %d bytes above the bound %d", len(body), bounds["1.4"])
	}
}

// TestPreparedRetrieveBudgetsConvertedSizeBeforeFirstPart retrieves a series
// whose RLE object decompresses beyond the part limit, and one whose objects
// decompress beyond the response limit together, through a backend that
// budgets SourceOutputBound before yielding. A client that omits the syntax
// receives 413 before any byte instead of a 200 multipart body cut short;
// a client that accepts any syntax receives the stored objects whole.
func TestPreparedRetrieveBudgetsConvertedSizeBeforeFirstPart(t *testing.T) {
	plain := boundTestObject(t, transfer.ExplicitVRLittleEndian, "1.4", 4, 4, 1)
	for _, tc := range []struct {
		name    string
		limits  ServerLimits
		objects []boundTestEntry
	}{
		{"part limit", ServerLimits{MaxPartBytes: 32 << 10, MaxResponseBytes: 1 << 20}, []boundTestEntry{
			{"1.4", transfer.ExplicitVRLittleEndian, plain},
			{"1.5", transfer.RLELossless, boundTestObject(t, transfer.RLELossless, "1.5", 128, 128, 2)},
		}},
		{"response limit", ServerLimits{MaxPartBytes: 32 << 10, MaxResponseBytes: 40 << 10}, []boundTestEntry{
			{"1.4", transfer.RLELossless, boundTestObject(t, transfer.RLELossless, "1.4", 96, 128, 1)},
			{"1.5", transfer.RLELossless, boundTestObject(t, transfer.RLELossless, "1.5", 96, 128, 1)},
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			stored := 0
			for _, o := range tc.objects {
				stored += len(o.data)
			}
			if stored > 8<<10 {
				t.Fatalf("the stored series has %d bytes; the test needs it well within the limits", stored)
			}
			bounds, opened := map[string]int64{}, 0
			normalized, err := normalizeServerLimits(tc.limits)
			if err != nil {
				t.Fatal(err)
			}
			_, hs := newOpenTestServer(t, budgetingBackend(tc.objects, normalized, bounds, &opened), func(o *ServerOptions) {
				o.Limits = tc.limits
				o.RetrieveTranscode = boundTestPolicy(t)
			})
			multipartDICOM := `multipart/related; type="application/dicom"`
			for _, accept := range []string{multipartDICOM, multipartDICOM + "; transfer-syntax=" + transfer.ExplicitVRLittleEndian.UID} {
				status, _, body := boundTestGet(t, hs.URL+"/studies/1.2/series/1.3", accept)
				if status != http.StatusRequestEntityTooLarge {
					t.Errorf("%s: HTTP %d with %d bytes, want 413 before the first part", accept, status, len(body))
				}
			}
			status, contentType, body := boundTestGet(t, hs.URL+"/studies/1.2/series/1.3", multipartDICOM+"; transfer-syntax=*")
			_, params, err := mime.ParseMediaType(contentType)
			if status != http.StatusOK || err != nil || !bytes.HasSuffix(body, []byte("--"+params["boundary"]+"--\r\n")) {
				t.Fatalf("series in any syntax: HTTP %d, %d bytes, closing boundary %t", status, len(body), err == nil && bytes.HasSuffix(body, []byte("--"+params["boundary"]+"--\r\n")))
			}
			parts := readMultipartStringsFromBody(t, params["boundary"], body)
			if len(parts) != len(tc.objects) {
				t.Fatalf("series in any syntax delivered %d of %d objects", len(parts), len(tc.objects))
			}
			for i, part := range parts {
				if !bytes.Equal([]byte(part), tc.objects[i].data) {
					t.Errorf("series in any syntax: part %d is not the stored object", i)
				}
			}
		})
	}
}

// TestPreparedRetrieveAppliesConversionLimitsBeforeFirstPart retrieves the
// series of a small Explicit VR Little Endian object and a constant 512x512
// JPEG-LS Lossless object whose few hundred stored bytes decode to more than
// MaxExpansionRatio times as many, so the conversion refuses it, through a
// backend that budgets SourceOutputBound before yielding. A client that does
// not accept JPEG-LS receives 413 before any byte, not a 200 multipart body
// that ends after the first part, and so does a single-instance request; a
// client that accepts the stored syntax as well receives the stored object
// whole, as it would if no decoder were configured.
func TestPreparedRetrieveAppliesConversionLimitsBeforeFirstPart(t *testing.T) {
	explicit, jls := transfer.ExplicitVRLittleEndian.UID, transfer.JPEGLSLossless.UID
	plain := boundTestObject(t, transfer.ExplicitVRLittleEndian, "1.4", 4, 4, 1)
	constant := boundTestObjectOf(t, transfer.JPEGLSLossless, "1.5", 512, 512, 1, func(int, int) uint16 { return 1000 })
	file, err := object.ReadFile(bytes.NewReader(constant))
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := pixeldata.TranscodeFile(context.Background(), file, transfer.ExplicitVRLittleEndian, preparedDecoderPolicy(t).Options); !errors.Is(err, pixeldata.ErrDecompressResourceLimit) {
		t.Fatalf("the %d-byte JPEG-LS object must exceed the conversion's expansion ratio: %v", len(constant), err)
	}
	objects := []boundTestEntry{{"1.4", transfer.ExplicitVRLittleEndian, plain}, {"1.5", transfer.JPEGLSLossless, constant}}
	multipartDICOM := `multipart/related; type="application/dicom"`
	for _, prefer := range []bool{false, true} {
		t.Run("PreferStoredSyntax="+strconv.FormatBool(prefer), func(t *testing.T) {
			bounds, opened := map[string]int64{}, 0
			_, hs := newOpenTestServer(t, budgetingBackend(objects, DefaultServerLimits(), bounds, &opened), func(o *ServerOptions) {
				policy := preparedDecoderPolicy(t)
				policy.PreferStoredSyntax = prefer
				o.RetrieveTranscode = policy
			})
			series := hs.URL + "/studies/1.2/series/1.3"
			for _, accept := range []string{multipartDICOM, multipartDICOM + "; transfer-syntax=" + explicit} {
				if status, _, body := boundTestGet(t, series, accept); status != http.StatusRequestEntityTooLarge {
					t.Errorf("series with %s: HTTP %d with %d bytes, want 413 before the first part", accept, status, len(body))
				}
			}
			if status, _, body := boundTestGet(t, series+"/instances/1.5", "application/dicom"); status != http.StatusRequestEntityTooLarge {
				t.Errorf("instance in Explicit VR Little Endian: HTTP %d with %d bytes, want 413", status, len(body))
			}
			for _, accept := range []string{
				multipartDICOM + "; transfer-syntax=*",
				multipartDICOM + "; transfer-syntax=" + explicit + ", " + multipartDICOM + "; transfer-syntax=" + jls + "; q=0.5",
			} {
				status, contentType, body := boundTestGet(t, series, accept)
				_, params, err := mime.ParseMediaType(contentType)
				closed := err == nil && bytes.HasSuffix(body, []byte("--"+params["boundary"]+"--\r\n"))
				if status != http.StatusOK || !closed {
					t.Fatalf("series with %s: HTTP %d, %d bytes, closing boundary %t", accept, status, len(body), closed)
				}
				parts := readMultipartStringsFromBody(t, params["boundary"], body)
				if len(parts) != len(objects) {
					t.Fatalf("series with %s delivered %d of %d objects", accept, len(parts), len(objects))
				}
				for i, part := range parts {
					if !bytes.Equal([]byte(part), objects[i].data) {
						t.Errorf("series with %s: part %d is not the stored object", accept, i)
					}
				}
				if bounds["1.5"] != int64(len(constant)) {
					t.Errorf("series with %s budgeted %d bytes for the stored %d-byte object", accept, bounds["1.5"], len(constant))
				}
			}
		})
	}
}

// TestSourceOutputBoundRefusals checks the answers that need no response:
// an object above the conversion input, a syntax no conversion delivers, a
// request without prepared retrieval and an open that fails.
func TestSourceOutputBoundRefusals(t *testing.T) {
	data := boundTestObject(t, transfer.RLELossless, "1.4", 16, 16, 1)
	failure := errors.New("open failed")
	var answers []error
	backend := completeServerTestBackend()
	backend.retrieve = func(ctx context.Context, req RetrieveRequest, _ func(RetrievePart) error) error {
		answers = answers[:0]
		for _, tc := range []struct {
			syntax string
			size   int64
			open   func() (io.ReadSeekCloser, error)
		}{
			{transfer.RLELossless.UID, 2 << 30, nil},
			{transfer.JPEG2000LosslessOnly.UID, int64(len(data)), nil},
			{transfer.RLELossless.UID, int64(len(data)), func() (io.ReadSeekCloser, error) { return nil, failure }},
			{"1.2.3.4.5.6", int64(len(data)), nil},
		} {
			_, err := req.SourceOutputBound(ctx, tc.syntax, tc.size, tc.open)
			answers = append(answers, err)
		}
		return ErrNotFound
	}
	_, hs := newOpenTestServer(t, backend, func(o *ServerOptions) { o.RetrieveTranscode = boundTestPolicy(t) })
	boundTestGet(t, hs.URL+preparedInstancePath, "application/dicom")
	want := []error{ErrResourceLimit, ErrUnsupported, failure, ErrUnsupported}
	if len(answers) != len(want) {
		t.Fatalf("answers %v", answers)
	}
	for i := range want {
		if !errors.Is(answers[i], want[i]) {
			t.Errorf("case %d: %v, want %v", i, answers[i], want[i])
		}
	}

	// Without prepared retrieval the stored bytes are delivered as they are.
	plain := RetrieveRequest{Level: RetrieveLevelInstance, Accept: []MediaPreference{{MediaType: "application/dicom", TransferSyntaxUID: "*", Quality: 1}}}
	if bound, err := plain.SourceOutputBound(context.Background(), transfer.RLELossless.UID, 1234, nil); err != nil || bound != 1234 {
		t.Errorf("without prepared retrieval: %d, %v", bound, err)
	}
	explicit := RetrieveRequest{Level: RetrieveLevelInstance, Accept: []MediaPreference{{MediaType: "application/dicom", TransferSyntaxUID: transfer.ExplicitVRLittleEndian.UID, Quality: 1}}}
	if _, err := explicit.SourceOutputBound(context.Background(), transfer.RLELossless.UID, 1234, nil); !errors.Is(err, ErrUnsupported) {
		t.Errorf("without prepared retrieval, a syntax the client refuses: %v", err)
	}
}
