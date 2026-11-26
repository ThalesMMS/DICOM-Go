package dicomweb

import (
	"bytes"
	"context"
	"encoding/binary"
	"io"
	"mime"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"testing"
	"time"

	"github.com/ThalesMMS/dicom-go/core"
	"github.com/ThalesMMS/dicom-go/object"
	"github.com/ThalesMMS/dicom-go/transfer"
)

// boundTestObjectWith is boundTestObject in syntax with element added.
func boundTestObjectWith(t *testing.T, syntax transfer.Syntax, sop string, element core.Element) []byte {
	t.Helper()
	file, err := object.ReadFile(bytes.NewReader(boundTestObject(t, transfer.ExplicitVRLittleEndian, sop, 4, 4, 1)))
	if err != nil {
		t.Fatal(err)
	}
	file.Dataset.Put(element)
	file.TransferSyntax = syntax
	var buffer bytes.Buffer
	if err := object.WriteFile(&buffer, file); err != nil {
		t.Fatal(err)
	}
	return buffer.Bytes()
}

// boundTestGetSeries retrieves the test series and reports whether its
// multipart body ends with its closing boundary, with its parts.
func boundTestGetSeries(t *testing.T, base, accept string) (int, bool, []string) {
	t.Helper()
	status, contentType, body := boundTestGet(t, base+"/studies/1.2/series/1.3", accept)
	_, params, err := mime.ParseMediaType(contentType)
	closed := err == nil && bytes.HasSuffix(body, []byte("--"+params["boundary"]+"--\r\n"))
	if !closed {
		t.Logf("series with %s: HTTP %d, %d bytes without a closing boundary", accept, status, len(body))
		return status, false, nil
	}
	return status, true, readMultipartStringsFromBody(t, params["boundary"], body)
}

// TestPreparedRetrieveDeliversLargeStoredElements retrieves a series whose
// second object holds a 65 MiB value outside its Pixel Data, above
// MaxMetadataBytes, with the response limit at the request limit as Twin's
// origin sets it. Prepared retrieval parses every object it delivers, also
// one it delivers as stored, but defers such values and does not hold them,
// so the parse bounds them by the object's size only: the series is
// delivered whole, the stored bytes unchanged, as a server without the
// policy delivers it, and so is the object alone. An Implicit VR object
// with such a value is converted whole as well.
func TestPreparedRetrieveDeliversLargeStoredElements(t *testing.T) {
	big := make([]byte, 65<<20)
	for _, tc := range []struct {
		name    string
		syntax  transfer.Syntax
		element core.Element
	}{
		{"encapsulated document", transfer.ExplicitVRLittleEndian, core.NewRawElement(core.NewTag(0x0042, 0x0011), core.VROB, big)},
		{"private OB", transfer.ExplicitVRLittleEndian, core.NewRawElement(core.NewTag(0x0009, 0x1010), core.VROB, big)},
		{"Implicit VR private OB", transfer.ImplicitVRLittleEndian, core.NewRawElement(core.NewTag(0x0009, 0x1010), core.VROB, big)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			first := boundTestObject(t, transfer.ExplicitVRLittleEndian, "1.4", 4, 4, 1)
			large := boundTestObjectWith(t, tc.syntax, "1.5", tc.element)
			objects := []boundTestEntry{{"1.4", transfer.ExplicitVRLittleEndian, first}, {"1.5", tc.syntax, large}}
			limits := DefaultServerLimits()
			limits.MaxResponseBytes = limits.MaxRequestBytes
			bounds, opened := map[string]int64{}, 0
			_, hs := newOpenTestServer(t, budgetingBackend(objects, limits, bounds, &opened), func(o *ServerOptions) {
				o.Limits = limits
				o.RetrieveTranscode = boundTestPolicy(t)
			})
			status, closed, parts := boundTestGetSeries(t, hs.URL, `multipart/related; type="application/dicom"`)
			if status != http.StatusOK || !closed || len(parts) != 2 {
				t.Fatalf("series: HTTP %d, closing boundary %t, %d parts; want the whole series", status, closed, len(parts))
			}
			if parts[0] != string(first) {
				t.Error("series: the first object is not the stored object")
			}
			if tc.syntax.UID == transfer.ExplicitVRLittleEndian.UID {
				if parts[1] != string(large) {
					t.Error("series: the large object is not the stored object")
				}
				status, _, body := boundTestGet(t, hs.URL+"/studies/1.2/series/1.3/instances/1.5", "application/dicom")
				if status != http.StatusOK || !bytes.Equal(body, large) {
					t.Errorf("instance: HTTP %d, %d bytes; want the stored %d bytes", status, len(body), len(large))
				}
				return
			}
			converted, err := object.ReadFile(bytes.NewReader([]byte(parts[1])))
			if err != nil {
				t.Fatal(err)
			}
			if converted.TransferSyntax.UID != transfer.ExplicitVRLittleEndian.UID {
				t.Fatalf("the Implicit VR object was delivered in %s", converted.TransferSyntax.UID)
			}
			if value, ok := converted.Dataset.Get(tc.element.Tag()); !ok {
				t.Fatal("the converted object lost its large value")
			} else if raw, _ := value.RawBytes(); len(raw) != len(big) {
				t.Fatalf("the converted object holds %d bytes of its large value, want %d", len(raw), len(big))
			}
		})
	}
}

// iconTestObject is a 2-frame 8x8 object stored in syntax with a 4x4 icon.
func iconTestObject(t *testing.T, syntax transfer.Syntax, sop string) []byte {
	t.Helper()
	us := func(tag core.Tag, v int) core.Element {
		return core.NewRawElement(tag, core.VRUS, binary.LittleEndian.AppendUint16(nil, uint16(v)))
	}
	icon := object.FromElements([]core.Element{
		us(core.NewTag(0x0028, 0x0002), 1),
		{Header: core.ElementHeader{Tag: core.NewTag(0x0028, 0x0004), VR: core.VRCS}, Value: core.StringValue{"MONOCHROME2"}},
		us(core.NewTag(0x0028, 0x0010), 4), us(core.NewTag(0x0028, 0x0011), 4),
		us(core.NewTag(0x0028, 0x0100), 8), us(core.NewTag(0x0028, 0x0101), 8), us(core.NewTag(0x0028, 0x0102), 7), us(core.NewTag(0x0028, 0x0103), 0),
		core.NewRawElement(core.TagPixelData, core.VROB, make([]byte, 16)),
	}, nil)
	file, err := object.ReadFile(bytes.NewReader(boundTestObject(t, transfer.ExplicitVRLittleEndian, sop, 8, 8, 2)))
	if err != nil {
		t.Fatal(err)
	}
	file.Dataset.Put(core.Element{Header: core.ElementHeader{Tag: core.NewTag(0x0088, 0x0200), VR: core.VRSQ}, Value: core.SequenceValue{Items: []core.DataSet{icon.ToDataSet()}}})
	file.TransferSyntax = syntax
	var buffer bytes.Buffer
	if err := object.WriteFile(&buffer, file); err != nil {
		t.Fatal(err)
	}
	return buffer.Bytes()
}

// TestPreparedRetrieveCountsIconsBeforeFirstPart retrieves a series whose
// Implicit VR object of two frames carries a one-frame icon. The conversion
// bounds the frames of the image and its icons together, so with MaxFrames
// 2 it refuses the object: the series answers 413 before its first part, not
// a 200 multipart body cut after the first object. With MaxFrames 3 the
// series is delivered whole.
func TestPreparedRetrieveCountsIconsBeforeFirstPart(t *testing.T) {
	objects := []boundTestEntry{
		{"1.4", transfer.ExplicitVRLittleEndian, boundTestObject(t, transfer.ExplicitVRLittleEndian, "1.4", 4, 4, 1)},
		{"1.5", transfer.ImplicitVRLittleEndian, iconTestObject(t, transfer.ImplicitVRLittleEndian, "1.5")},
	}
	for _, tc := range []struct {
		maxFrames int
		status    int
	}{{2, http.StatusRequestEntityTooLarge}, {3, http.StatusOK}} {
		limits := DefaultServerLimits()
		limits.MaxFrames = tc.maxFrames // clips TranscodeLimits.MaxFrames
		bounds, opened := map[string]int64{}, 0
		_, hs := newOpenTestServer(t, budgetingBackend(objects, limits, bounds, &opened), func(o *ServerOptions) {
			o.Limits = limits
			o.RetrieveTranscode = boundTestPolicy(t)
		})
		status, closed, parts := boundTestGetSeries(t, hs.URL, `multipart/related; type="application/dicom"`)
		if status != tc.status || (status == http.StatusOK && (!closed || len(parts) != 2)) {
			t.Errorf("MaxFrames %d: HTTP %d, closing boundary %t, %d parts; want HTTP %d with every part", tc.maxFrames, status, closed, len(parts), tc.status)
		}
		if status, _, body := boundTestGet(t, hs.URL+"/studies/1.2/series/1.3/instances/1.5", "application/dicom"); status != tc.status {
			t.Errorf("MaxFrames %d: instance HTTP %d with %d bytes, want %d", tc.maxFrames, status, len(body), tc.status)
		}
	}
}

// manyItemsTestObject is boundTestObject with a sequence of items, each
// holding one short value.
func manyItemsTestObject(t *testing.T, sop string, items int) []byte {
	t.Helper()
	list := make([]core.DataSet, items)
	for i := range list {
		list[i] = core.DataSet{Elements: []core.Element{core.NewRawElement(core.NewTag(0x0040, 0xA30A), core.VRDS, []byte("1 "))}}
	}
	return boundTestObjectWith(t, transfer.ExplicitVRLittleEndian, sop, core.Element{Header: core.ElementHeader{Tag: core.NewTag(0x0040, 0xA730), VR: core.VRSQ}, Value: core.SequenceValue{Items: list}})
}

// manyFragmentsTestObject is a JPEG Baseline object of frames 8x8 frames
// with one fragment per frame, like a level of a tiled whole-slide image.
func manyFragmentsTestObject(t *testing.T, sop string, frames int) []byte {
	t.Helper()
	us := func(tag core.Tag, v int) core.Element {
		return core.NewRawElement(tag, core.VRUS, binary.LittleEndian.AppendUint16(nil, uint16(v)))
	}
	text := func(tag core.Tag, vr core.VR, v string) core.Element {
		return core.Element{Header: core.ElementHeader{Tag: tag, VR: vr}, Value: core.StringValue{v}}
	}
	fragments := make([][]byte, frames)
	for i := range fragments {
		fragments[i] = []byte{0xFF, 0xD8, 0xFF, 0xD9}
	}
	dataset := object.FromElements([]core.Element{
		text(core.NewTag(0x0008, 0x0016), core.VRUI, "1.2.840.10008.5.1.4.1.1.77.1.6"), text(core.NewTag(0x0008, 0x0018), core.VRUI, sop),
		text(core.NewTag(0x0020, 0x000D), core.VRUI, "1.2"), text(core.NewTag(0x0020, 0x000E), core.VRUI, "1.3"),
		us(core.NewTag(0x0028, 0x0002), 1), text(core.NewTag(0x0028, 0x0004), core.VRCS, "MONOCHROME2"),
		text(core.NewTag(0x0028, 0x0008), core.VRIS, strconv.Itoa(frames)),
		us(core.NewTag(0x0028, 0x0010), 8), us(core.NewTag(0x0028, 0x0011), 8),
		us(core.NewTag(0x0028, 0x0100), 8), us(core.NewTag(0x0028, 0x0101), 8), us(core.NewTag(0x0028, 0x0102), 7), us(core.NewTag(0x0028, 0x0103), 0),
		{Header: core.ElementHeader{Tag: core.TagPixelData, VR: core.VROB, Length: core.UndefinedLength}, Value: core.FragmentSequence{Fragments: fragments}},
	}, nil)
	var buffer bytes.Buffer
	if err := object.WriteFile(&buffer, &object.File{Dataset: dataset, TransferSyntax: transfer.JPEGBaseline}); err != nil {
		t.Fatal(err)
	}
	return buffer.Bytes()
}

// TestPreparedRetrieveDeliversManyFragmentsAsStored retrieves a series whose
// JPEG Baseline object has 120,000 frames of one fragment each, above the
// conversion's MaxFragments (100,000). A client that omits the syntax
// receives a stored lossy object as stored, so it is never decoded and its
// fragments are not bounded: the series is delivered whole, as by a server
// without the policy, and so is the object alone.
func TestPreparedRetrieveDeliversManyFragmentsAsStored(t *testing.T) {
	first := boundTestObject(t, transfer.ExplicitVRLittleEndian, "1.4", 4, 4, 1)
	tiled := manyFragmentsTestObject(t, "1.5", 120_000)
	objects := []boundTestEntry{{"1.4", transfer.ExplicitVRLittleEndian, first}, {"1.5", transfer.JPEGBaseline, tiled}}
	limits := DefaultServerLimits()
	limits.MaxResponseBytes = limits.MaxRequestBytes
	bounds, opened := map[string]int64{}, 0
	_, hs := newOpenTestServer(t, budgetingBackend(objects, limits, bounds, &opened), func(o *ServerOptions) {
		o.Limits = limits
		o.RetrieveTranscode = boundTestPolicy(t)
	})
	status, closed, parts := boundTestGetSeries(t, hs.URL, `multipart/related; type="application/dicom"`)
	if status != http.StatusOK || !closed || len(parts) != 2 || parts[0] != string(first) || parts[1] != string(tiled) {
		t.Fatalf("series: HTTP %d, closing boundary %t, %d parts; want both stored objects", status, closed, len(parts))
	}
	if status, _, body := boundTestGet(t, hs.URL+"/studies/1.2/series/1.3/instances/1.5", "application/dicom"); status != http.StatusOK || !bytes.Equal(body, tiled) {
		t.Fatalf("instance: HTTP %d, %d bytes; want the stored %d bytes", status, len(body), len(tiled))
	}
}

// TestPreparedReadsAreBuffered reads an object of 100,000 items as prepared
// retrieval does. The parsers read a few bytes per element and item header;
// each read of the staged file is a system call and a lease check, so the
// reads are buffered: their number follows the object's size, not its
// headers.
func TestPreparedReadsAreBuffered(t *testing.T) {
	data := manyItemsTestObject(t, "1.4", 100_000)
	path := filepath.Join(t.TempDir(), "source.dcm")
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatal(err)
	}
	s, _ := newOpenTestServer(t, completeServerTestBackend(), func(o *ServerOptions) { o.RetrieveTranscode = boundTestPolicy(t) })
	prepared, err := s.readPreparedFile(context.Background(), path, s.retrievePolicy.Options.Limits.MaxInputBytes, InstanceRef{StudyInstanceUID: "1.2", SeriesInstanceUID: "1.3", SOPInstanceUID: "1.4"})
	if err != nil {
		t.Fatal(err)
	}
	defer prepared.file.Close()
	reads, limit := prepared.file.reads.Load(), int64(len(data)/(16<<10)+64)
	t.Logf("%d bytes read with %d reads", len(data), reads)
	if reads > limit {
		t.Fatalf("%d reads for a %d-byte object, want at most %d", reads, len(data), limit)
	}
}

// TestPreparedRetrieveSeriesWithManyItemsWithinDeadline retrieves a series
// whose second object holds 200,000 items with a MaxDuration of 8 s. An
// unbuffered prepared parse took longer than that, so the deadline cut the
// series after its first object; buffered, it takes a fraction of a second.
func TestPreparedRetrieveSeriesWithManyItemsWithinDeadline(t *testing.T) {
	if testing.Short() {
		t.Skip("parses a 200,000-item object")
	}
	first := boundTestObject(t, transfer.ExplicitVRLittleEndian, "1.4", 4, 4, 1)
	items := manyItemsTestObject(t, "1.5", 200_000)
	objects := []boundTestEntry{{"1.4", transfer.ExplicitVRLittleEndian, first}, {"1.5", transfer.ExplicitVRLittleEndian, items}}
	limits := DefaultServerLimits()
	limits.MaxResponseBytes, limits.MaxDuration = limits.MaxRequestBytes, 8*time.Second
	bounds, opened := map[string]int64{}, 0
	_, hs := newOpenTestServer(t, budgetingBackend(objects, limits, bounds, &opened), func(o *ServerOptions) {
		o.Limits = limits
		o.RetrieveTranscode = boundTestPolicy(t)
	})
	started := time.Now()
	status, closed, parts := boundTestGetSeries(t, hs.URL, `multipart/related; type="application/dicom"`)
	t.Logf("series of %d bytes: HTTP %d in %v", len(first)+len(items), status, time.Since(started))
	if status != http.StatusOK || !closed || len(parts) != 2 || parts[1] != string(items) {
		t.Fatalf("series: HTTP %d, closing boundary %t, %d parts; want both stored objects", status, closed, len(parts))
	}
}

// TestPreparedRetrieveStagesSmallObjectsInMemory prepares a small object
// delivered as stored and one that is converted. Creating, opening and
// removing a job directory cost more than parsing a small object, so a
// series of 300 small objects took seconds longer than without the policy:
// the stored object is staged in memory and leaves the spool directory
// empty, and only the conversion writes files there, removed afterwards.
func TestPreparedRetrieveStagesSmallObjectsInMemory(t *testing.T) {
	for _, tc := range []struct {
		syntax transfer.Syntax
		files  bool
	}{{transfer.ExplicitVRLittleEndian, false}, {transfer.ImplicitVRLittleEndian, true}} {
		t.Run(tc.syntax.Name, func(t *testing.T) {
			input, _ := preparedTestFixture(t, tc.syntax)
			spool := t.TempDir()
			s, _ := newOpenTestServer(t, completeServerTestBackend(), func(o *ServerOptions) {
				o.SpoolDirectory = spool
				o.RetrieveTranscode = preparedTestPolicy(t)
			})
			request := RetrieveRequest{Level: RetrieveLevelInstance, StudyInstanceUID: "1.2", SeriesInstanceUID: "1.3", SOPInstanceUID: "1.4", Accept: []MediaPreference{{MediaType: "application/dicom", Quality: 1}}}
			part := RetrievePart{Reader: io.NopCloser(bytes.NewReader(input)), Ref: preparedTestRef, Size: int64(len(input))}
			delivered := ""
			err := s.prepareRetrievePart(context.Background(), request, part, false, false, func(p *preparedRetrieve) error {
				entries, err := os.ReadDir(spool)
				if err != nil {
					return err
				}
				if (len(entries) > 0) != tc.files {
					t.Errorf("%d spool entries while the object is delivered, want files %t", len(entries), tc.files)
				}
				delivered = p.parsed.TransferSyntax.UID
				return nil
			})
			if err != nil || delivered != transfer.ExplicitVRLittleEndian.UID {
				t.Fatalf("prepared %q: %v", delivered, err)
			}
			if entries, _ := os.ReadDir(spool); len(entries) != 0 {
				t.Fatalf("%d spool entries left", len(entries))
			}
		})
	}
}

// TestPreparedRetrieveFallsBackToStoredAboveMaxFragments retrieves a series
// whose JPEG Baseline object has 120,000 one-fragment frames, above the
// conversion's MaxFragments, for a client that prefers Explicit VR Little
// Endian. Its conversion is refused on that limit before the first part, as
// on its frames: a client that also accepts JPEG Baseline receives the
// stored object, the series whole; one that accepts only Explicit VR Little
// Endian receives 413 before the first part.
func TestPreparedRetrieveFallsBackToStoredAboveMaxFragments(t *testing.T) {
	first := boundTestObject(t, transfer.ExplicitVRLittleEndian, "1.4", 4, 4, 1)
	tiled := manyFragmentsTestObject(t, "1.5", 120_000)
	objects := []boundTestEntry{{"1.4", transfer.ExplicitVRLittleEndian, first}, {"1.5", transfer.JPEGBaseline, tiled}}
	limits := DefaultServerLimits()
	limits.MaxResponseBytes = limits.MaxRequestBytes
	bounds, opened := map[string]int64{}, 0
	_, hs := newOpenTestServer(t, budgetingBackend(objects, limits, bounds, &opened), func(o *ServerOptions) {
		o.Limits = limits
		o.RetrieveTranscode = boundTestPolicy(t)
	})
	multipartDICOM := `multipart/related; type="application/dicom"`
	explicit := multipartDICOM + "; transfer-syntax=" + transfer.ExplicitVRLittleEndian.UID
	status, closed, parts := boundTestGetSeries(t, hs.URL, explicit+", "+multipartDICOM+"; transfer-syntax="+transfer.JPEGBaseline.UID+"; q=0.5")
	if status != http.StatusOK || !closed || len(parts) != 2 || parts[0] != string(first) || parts[1] != string(tiled) {
		t.Fatalf("series accepting the stored syntax: HTTP %d, closing boundary %t, %d parts; want both stored objects", status, closed, len(parts))
	}
	if bounds["1.5"] != int64(len(tiled)) {
		t.Errorf("budgeted %d bytes for the stored %d-byte object", bounds["1.5"], len(tiled))
	}
	if status, _, body := boundTestGet(t, hs.URL+"/studies/1.2/series/1.3", explicit); status != http.StatusRequestEntityTooLarge {
		t.Fatalf("series in Explicit VR Little Endian only: HTTP %d with %d bytes, want 413 before the first part", status, len(body))
	}
}
