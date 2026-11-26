package dicomweb

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"errors"
	"io"
	"mime"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/ThalesMMS/dicom-go/core"
	"github.com/ThalesMMS/dicom-go/dicomjson"
	"github.com/ThalesMMS/dicom-go/internal/dicomtest"
	"github.com/ThalesMMS/dicom-go/object"
	"github.com/ThalesMMS/dicom-go/parser"
	"github.com/ThalesMMS/dicom-go/pixeldata"
	"github.com/ThalesMMS/dicom-go/transfer"
)

const (
	part10MetaStudy  = "1.2.826.0.1.3680043.10.543.1968"
	part10MetaSeries = "1.2.826.0.1.3680043.10.543.1968.1"
	part10MetaSOP    = "1.2.826.0.1.3680043.10.543.1968.1.1"
)

var (
	part10MetaSequence = core.NewTag(0x0054, 0x0016)
	part10MetaBlob     = core.NewTag(0x0042, 0x0011)
	part10MetaSmall    = core.NewTag(0x0400, 0x0520)
)

// part10MetadataFixture encodes, in syntax, an object with 16-bit native Pixel
// Data, a large OW value nested in a sequence item, a small OB value and text.
// It returns the file, the Pixel Data and the nested value in Little Endian.
func part10MetadataFixture(t *testing.T, syntax transfer.Syntax) ([]byte, []byte, []byte) {
	t.Helper()
	order := binary.ByteOrder(binary.LittleEndian)
	if syntax.UID == transfer.ExplicitVRBigEndian.UID {
		order = binary.BigEndian
	}
	pixels := make([]uint16, 32*32)
	nested := make([]uint16, 1500)
	for i := range pixels {
		pixels[i] = uint16(i*4099 + 7)
	}
	for i := range nested {
		nested[i] = uint16(i*257 + 3)
	}
	littleEndian := func(values []uint16) []byte {
		out := make([]byte, 2*len(values))
		for i, v := range values {
			binary.LittleEndian.PutUint16(out[2*i:], v)
		}
		return out
	}
	us := func(group, element uint16, value uint16) core.Element {
		return dicomtest.Uint16Element(core.NewTag(group, element), core.VRUS, order, value)
	}
	elements := []core.Element{
		dicomtest.StringElement(core.NewTag(0x0008, 0x0016), core.VRUI, "1.2.840.10008.5.1.4.1.1.7"),
		dicomtest.StringElement(core.NewTag(0x0008, 0x0018), core.VRUI, part10MetaSOP),
		dicomtest.StringElement(core.NewTag(0x0010, 0x0010), core.VRPN, "METADATA^BULK"),
		dicomtest.StringElement(core.NewTag(0x0020, 0x000D), core.VRUI, part10MetaStudy),
		dicomtest.StringElement(core.NewTag(0x0020, 0x000E), core.VRUI, part10MetaSeries),
		us(0x0028, 0x0002, 1),
		dicomtest.StringElement(core.NewTag(0x0028, 0x0004), core.VRCS, "MONOCHROME2"),
		us(0x0028, 0x0010, 32),
		us(0x0028, 0x0011, 32),
		us(0x0028, 0x0100, 16),
		us(0x0028, 0x0101, 16),
		us(0x0028, 0x0102, 15),
		us(0x0028, 0x0103, 0),
		dicomtest.BytesElement(part10MetaBlob, core.VROB, bytes.Repeat([]byte{0xA5}, 2048)),
		dicomtest.BytesElement(part10MetaSmall, core.VROB, []byte{1, 2, 3, 4}),
		{Header: core.ElementHeader{Tag: part10MetaSequence, VR: core.VRSQ}, Value: core.SequenceValue{Items: []core.DataSet{{Elements: []core.Element{
			dicomtest.Uint16Element(core.NewTag(0x0028, 0x1201), core.VROW, order, nested...),
		}}}}},
		dicomtest.Uint16Element(core.TagPixelData, core.VROW, order, pixels...),
	}
	data, err := dicomtest.Part10File(syntax, elements...)
	if err != nil {
		t.Fatal(err)
	}
	return data, littleEndian(pixels), littleEndian(nested)
}

type part10MetaFile struct {
	*bytes.Reader
	closed bool
}

func (f *part10MetaFile) Close() error { f.closed = true; return nil }

func part10MetaURI(path []parser.BulkDataPathStep) (string, error) {
	text, err := FormatBulkDataPath(path)
	if err != nil {
		return "", err
	}
	return "/dicomweb/bulkdata/" + text, nil
}

func part10MetaPath(t *testing.T, dataset Dataset, tag string) []parser.BulkDataPathStep {
	t.Helper()
	uri := dataset[tag].BulkDataURI
	if !strings.HasPrefix(uri, "/dicomweb/bulkdata/") {
		t.Fatalf("%s has no BulkDataURI: %+v", tag, dataset[tag])
	}
	path, err := ParseBulkDataPath(strings.TrimPrefix(uri, "/dicomweb/bulkdata/"))
	if err != nil {
		t.Fatalf("%s: %v", uri, err)
	}
	return path
}

func part10MetaBulk(t *testing.T, data []byte, path []parser.BulkDataPathStep) []byte {
	t.Helper()
	source := &part10MetaFile{Reader: bytes.NewReader(data)}
	part, err := Part10BulkData(context.Background(), source, path, Part10MetadataOptions{})
	if err != nil {
		t.Fatalf("bulk data %v: %v", path, err)
	}
	got, err := io.ReadAll(part.Reader)
	if err != nil || int64(len(got)) != part.Size || part.ContentType != "application/octet-stream" {
		t.Fatalf("read %d of %d bytes as %q: %v", len(got), part.Size, part.ContentType, err)
	}
	if err := part.Reader.Close(); err != nil || !source.closed {
		t.Fatal("the part did not close its source")
	}
	return got
}

func TestPart10MetadataDescribesEveryNonDeflatedSyntax(t *testing.T) {
	for _, syntax := range []transfer.Syntax{transfer.ExplicitVRLittleEndian, transfer.ImplicitVRLittleEndian, transfer.ExplicitVRBigEndian} {
		t.Run(syntax.Name, func(t *testing.T) {
			data, pixels, nested := part10MetadataFixture(t, syntax)
			dataset, err := Part10Metadata(context.Background(), bytes.NewReader(data), Part10MetadataOptions{BulkDataURI: part10MetaURI})
			if err != nil {
				t.Fatal(err)
			}
			for tag := range dataset {
				if strings.HasPrefix(tag, "0002") {
					t.Fatalf("File Meta Information %s in the metadata", tag)
				}
			}
			if dicomjson.ElementString(dataset, "00100010") != "METADATA^BULK" || dicomjson.ElementString(dataset, "0020000D") != part10MetaStudy ||
				dicomjson.ElementString(dataset, "00280010") != "32" {
				t.Fatalf("attributes lost: %+v", dataset)
			}
			if small := dataset["04000520"]; small.BulkDataURI != "" || small.InlineBinary != "AQIDBA==" {
				t.Fatalf("a value below the threshold left the metadata: %+v", small)
			}
			if got := part10MetaBulk(t, data, part10MetaPath(t, dataset, "7FE00010")); !bytes.Equal(got, pixels) {
				t.Fatal("the Pixel Data bulk data is not the Little Endian pixel values")
			}
			if got := part10MetaBulk(t, data, part10MetaPath(t, dataset, "00420011")); !bytes.Equal(got, bytes.Repeat([]byte{0xA5}, 2048)) {
				t.Fatal("the OB bulk data differs")
			}
			items := dataset["00540016"].Value
			if len(items) != 1 {
				t.Fatalf("sequence %+v", dataset["00540016"])
			}
			var item Dataset
			switch typed := items[0].(type) {
			case map[string]any:
				encoded, err := json.Marshal(typed)
				if err != nil {
					t.Fatal(err)
				}
				if err := decodeJSON(encoded, &item); err != nil {
					t.Fatal(err)
				}
			default:
				t.Fatalf("item %T", typed)
			}
			path := part10MetaPath(t, item, "00281201")
			if !reflect.DeepEqual(path, []parser.BulkDataPathStep{{Tag: part10MetaSequence, ItemIndex: 0}, {Tag: core.NewTag(0x0028, 0x1201), ItemIndex: -1}}) {
				t.Fatalf("nested path %v", path)
			}
			if got := part10MetaBulk(t, data, path); !bytes.Equal(got, nested) {
				t.Fatal("the nested bulk data is not the Little Endian values")
			}
			// A value kept inline is not bulk data.
			source := &part10MetaFile{Reader: bytes.NewReader(data)}
			if _, err := Part10BulkData(context.Background(), source, []parser.BulkDataPathStep{{Tag: part10MetaSmall, ItemIndex: -1}}, Part10MetadataOptions{}); !errors.Is(err, ErrNotFound) || source.closed {
				t.Fatalf("inline value as bulk data: %v, source closed %t", err, source.closed)
			}
		})
	}
}

// part10InlineValue is a value below the bulk data threshold: its Little
// Endian bytes and the unit reversed to encode it in Big Endian.
type part10InlineValue struct {
	tag    core.Tag
	vr     core.VR
	little []byte
	width  int
}

func part10InlineValues() []part10InlineValue {
	sequence := func(n int) []byte {
		out := make([]byte, n)
		for i := range out {
			out[i] = byte(i*7 + 1)
		}
		return out
	}
	return []part10InlineValue{
		// Red Palette Color LUT Data: 256 entries, 512 bytes.
		{tag: core.NewTag(0x0028, 0x1201), vr: core.VROW, little: sequence(512), width: 2},
		{tag: core.NewTag(0x0066, 0x0016), vr: core.VROF, little: sequence(48), width: 4},
		{tag: core.NewTag(0x0066, 0x0022), vr: core.VROD, little: sequence(48), width: 8},
		{tag: core.NewTag(0x0066, 0x0040), vr: core.VROL, little: sequence(48), width: 4},
		{tag: core.NewTag(0x7FE0, 0x0001), vr: core.VROV, little: sequence(16), width: 8},
		{tag: part10MetaSmall, vr: core.VROB, little: sequence(6), width: 1},
	}
}

func part10Reverse(data []byte, width int) []byte {
	out := bytes.Clone(data)
	for unit := 0; unit+width <= len(out); unit += width {
		for i, j := unit, unit+width-1; i < j; i, j = i+1, j-1 {
			out[i], out[j] = out[j], out[i]
		}
	}
	return out
}

// TestPart10MetadataInlineBinaryIsLittleEndianInEverySyntax checks that the
// InlineBinary of OW, OF, OD, OL and OV values below the bulk data threshold
// is the Little Endian value in every syntax, like the bulk data of the same
// VRs above it, including OW Pixel Data with Bits Allocated 32 in an item.
func TestPart10MetadataInlineBinaryIsLittleEndianInEverySyntax(t *testing.T) {
	icon := []byte{1, 2, 3, 4, 5, 6, 7, 8, 9, 10, 11, 12, 13, 14, 15, 16}
	iconSequence := core.NewTag(0x0088, 0x0200)
	for _, syntax := range []transfer.Syntax{transfer.ExplicitVRLittleEndian, transfer.ImplicitVRLittleEndian, transfer.ExplicitVRBigEndian} {
		t.Run(syntax.Name, func(t *testing.T) {
			order := binary.ByteOrder(binary.LittleEndian)
			encode := func(little []byte, _ int) []byte { return little }
			if syntax.UID == transfer.ExplicitVRBigEndian.UID {
				order = binary.BigEndian
				encode = part10Reverse
			}
			us := func(element, value uint16) core.Element {
				return dicomtest.Uint16Element(core.NewTag(0x0028, element), core.VRUS, order, value)
			}
			elements := []core.Element{
				dicomtest.StringElement(core.NewTag(0x0008, 0x0016), core.VRUI, "1.2.840.10008.5.1.4.1.1.7"),
				dicomtest.StringElement(core.NewTag(0x0008, 0x0018), core.VRUI, part10MetaSOP),
				dicomtest.StringElement(core.NewTag(0x0020, 0x000D), core.VRUI, part10MetaStudy),
				{Header: core.ElementHeader{Tag: iconSequence, VR: core.VRSQ}, Value: core.SequenceValue{Items: []core.DataSet{{Elements: []core.Element{
					us(0x0002, 1), us(0x0010, 2), us(0x0011, 2), us(0x0100, 32), us(0x0101, 32), us(0x0102, 31), us(0x0103, 0),
					dicomtest.BytesElement(core.TagPixelData, core.VROW, encode(icon, 4)),
				}}}}},
			}
			for _, value := range part10InlineValues() {
				elements = append(elements, dicomtest.BytesElement(value.tag, value.vr, encode(value.little, value.width)))
			}
			data, err := dicomtest.Part10File(syntax, elements...)
			if err != nil {
				t.Fatal(err)
			}
			dataset, err := Part10Metadata(context.Background(), bytes.NewReader(data), Part10MetadataOptions{BulkDataURI: part10MetaURI})
			if err != nil {
				t.Fatal(err)
			}
			inline := func(element Element, name string) []byte {
				t.Helper()
				if element.BulkDataURI != "" || element.InlineBinary == "" {
					t.Fatalf("%s is not inline: %+v", name, element)
				}
				got, err := base64.StdEncoding.DecodeString(element.InlineBinary)
				if err != nil {
					t.Fatalf("%s: %v", name, err)
				}
				return got
			}
			for _, value := range part10InlineValues() {
				tag := value.tag.HexString()
				if got := inline(dataset[tag], tag); !bytes.Equal(got, value.little) {
					t.Errorf("%s %s InlineBinary % X..., want Little Endian % X...", tag, value.vr, got[:6], value.little[:6])
				}
			}
			items := dataset["00880200"].Value
			if len(items) != 1 {
				t.Fatalf("Icon Image Sequence %+v", dataset["00880200"])
			}
			encoded, err := json.Marshal(items[0])
			if err != nil {
				t.Fatal(err)
			}
			var item Dataset
			if err := decodeJSON(encoded, &item); err != nil {
				t.Fatal(err)
			}
			if got := inline(item["7FE00010"], "icon Pixel Data"); !bytes.Equal(got, icon) {
				t.Errorf("icon Pixel Data InlineBinary % X, want Little Endian 32-bit samples % X", got, icon)
			}
		})
	}
}

func TestPart10BulkDataRefusesEncapsulatedPixelData(t *testing.T) {
	syntax := transfer.JPEGBaseline
	data, err := dicomtest.Part10File(syntax,
		dicomtest.StringElement(core.NewTag(0x0008, 0x0016), core.VRUI, "1.2.840.10008.5.1.4.1.1.7"),
		dicomtest.StringElement(core.NewTag(0x0008, 0x0018), core.VRUI, part10MetaSOP),
		dicomtest.StringElement(core.NewTag(0x0020, 0x000D), core.VRUI, part10MetaStudy),
		dicomtest.NewFragmentSequenceElement(core.TagPixelData, nil, []byte{0xFF, 0xD8, 0xFF, 0xD9}),
	)
	if err != nil {
		t.Fatal(err)
	}
	dataset, err := Part10Metadata(context.Background(), bytes.NewReader(data), Part10MetadataOptions{BulkDataURI: part10MetaURI})
	if err != nil {
		t.Fatal(err)
	}
	path := part10MetaPath(t, dataset, "7FE00010")
	source := &part10MetaFile{Reader: bytes.NewReader(data)}
	if _, err := Part10BulkData(context.Background(), source, path, Part10MetadataOptions{}); !errors.Is(err, ErrUnsupported) {
		t.Fatalf("encapsulated Pixel Data as octet-stream: %v", err)
	}
}

func TestPart10MetadataRefusesDeflatedAndBadOptions(t *testing.T) {
	data, _, _ := part10MetadataFixture(t, transfer.ExplicitVRLittleEndian)
	file, err := object.ReadFile(bytes.NewReader(data))
	if err != nil {
		t.Fatal(err)
	}
	file.TransferSyntax = transfer.DeflatedExplicitVRLittleEndian
	if err := file.RebuildFileMeta(); err != nil {
		t.Fatal(err)
	}
	var deflated bytes.Buffer
	if err := object.WriteFile(&deflated, file); err != nil {
		t.Fatal(err)
	}
	if _, err := Part10Metadata(context.Background(), bytes.NewReader(deflated.Bytes()), Part10MetadataOptions{BulkDataURI: part10MetaURI}); !errors.Is(err, ErrUnsupported) {
		t.Fatalf("deflated: %v", err)
	}
	if _, err := Part10Metadata(context.Background(), bytes.NewReader(data), Part10MetadataOptions{}); !errors.Is(err, ErrInvalidServerOptions) {
		t.Fatalf("no BulkDataURI: %v", err)
	}
	if _, err := Part10Metadata(context.Background(), bytes.NewReader(data), Part10MetadataOptions{BulkDataURI: part10MetaURI, BulkDataThreshold: -1}); !errors.Is(err, ErrInvalidServerOptions) {
		t.Fatalf("negative threshold: %v", err)
	}
	if _, err := Part10Metadata(context.Background(), bytes.NewReader(data), Part10MetadataOptions{BulkDataURI: part10MetaURI, MaxElements: 3}); !errors.Is(err, ErrResourceLimit) {
		t.Fatalf("element limit: %v", err)
	}
	if _, err := Part10Metadata(context.Background(), bytes.NewReader([]byte("not DICOM")), Part10MetadataOptions{BulkDataURI: part10MetaURI}); !errors.Is(err, ErrBackend) {
		t.Fatalf("not Part 10: %v", err)
	}
}

func TestBulkDataPathTextIsCanonical(t *testing.T) {
	path := []parser.BulkDataPathStep{{Tag: core.NewTag(0x0054, 0x0016), ItemIndex: 12}, {Tag: core.NewTag(0x0028, 0x1201), ItemIndex: -1}}
	text, err := FormatBulkDataPath(path)
	if err != nil || text != "00540016.12/00281201" {
		t.Fatalf("format %q %v", text, err)
	}
	if parsed, err := ParseBulkDataPath(text); err != nil || !reflect.DeepEqual(parsed, path) {
		t.Fatalf("parse %v %v", parsed, err)
	}
	for _, bad := range []string{"", "7fe00010", "7FE00010.0", "00540016/00281201", "00540016.01/00281201", "00540016.-1/00281201",
		"7FE0001", "7FE000100", "FFFEE000", "00020010", "00540016.0//00281201", "0054001G"} {
		if _, err := ParseBulkDataPath(bad); !errors.Is(err, ErrInvalidRequest) {
			t.Errorf("%q parsed: %v", bad, err)
		}
	}
	for _, bad := range [][]parser.BulkDataPathStep{nil, {{Tag: core.TagPixelData, ItemIndex: 0}}, {{Tag: part10MetaSequence, ItemIndex: -1}, {Tag: core.TagPixelData, ItemIndex: -1}}} {
		if _, err := FormatBulkDataPath(bad); !errors.Is(err, ErrInvalidRequest) {
			t.Errorf("%v formatted: %v", bad, err)
		}
	}
}

// part10MetaBackend serves one Part 10 file through the server's metadata
// and bulk data routes.
type part10MetaBackend struct{ path string }

func (b part10MetaBackend) Metadata(ctx context.Context, _ MetadataRequest, yield func(Dataset) error) error {
	f, err := os.Open(b.path)
	if err != nil {
		return err
	}
	defer f.Close()
	dataset, err := Part10Metadata(ctx, f, Part10MetadataOptions{BulkDataURI: part10MetaURI})
	if err != nil {
		return err
	}
	return yield(dataset)
}

func (b part10MetaBackend) RetrieveBulkData(ctx context.Context, req BulkDataRequest) (BulkDataPart, error) {
	path, err := ParseBulkDataPath(req.Token)
	if err != nil {
		return BulkDataPart{}, ErrNotFound
	}
	f, err := os.Open(b.path)
	if err != nil {
		return BulkDataPart{}, err
	}
	part, err := Part10BulkData(ctx, f, path, Part10MetadataOptions{})
	if err != nil {
		_ = f.Close()
	}
	return part, err
}

func TestPart10MetadataAndBulkDataThroughTheServer(t *testing.T) {
	data, pixels, _ := part10MetadataFixture(t, transfer.ExplicitVRBigEndian)
	path := filepath.Join(t.TempDir(), "object.dcm")
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatal(err)
	}
	server, err := NewServer(ServerOptions{Backend: part10MetaBackend{path: path}, AllowUnauthenticated: true, ServiceRoot: "/dicomweb", SpoolDirectory: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	httpServer := httptest.NewServer(http.StripPrefix("/dicomweb", server))
	defer httpServer.Close()
	get := func(resource, accept string) (*http.Response, []byte) {
		request, _ := http.NewRequest(http.MethodGet, httpServer.URL+resource, nil)
		request.Header.Set("Accept", accept)
		response, err := httpServer.Client().Do(request)
		if err != nil {
			t.Fatal(err)
		}
		defer response.Body.Close()
		body, err := io.ReadAll(response.Body)
		if err != nil {
			t.Fatal(err)
		}
		return response, body
	}
	response, body := get("/dicomweb/studies/"+part10MetaStudy+"/metadata", "application/dicom+json")
	if response.StatusCode != http.StatusOK {
		t.Fatalf("metadata HTTP %d: %s", response.StatusCode, body)
	}
	datasets, err := datasetsFromDICOMJSON(body)
	if err != nil || len(datasets) != 1 {
		t.Fatalf("metadata %d datasets: %v", len(datasets), err)
	}
	uri := datasets[0]["7FE00010"].BulkDataURI
	response, body = get(uri, "application/octet-stream")
	if response.StatusCode != http.StatusOK || !bytes.Equal(body, pixels) {
		t.Fatalf("bulk data HTTP %d, %d bytes, equal %t", response.StatusCode, len(body), bytes.Equal(body, pixels))
	}
	response, body = get(uri, `multipart/related; type="application/octet-stream"`)
	_, params, _ := mime.ParseMediaType(response.Header.Get("Content-Type"))
	part, err := multipart.NewReader(bytes.NewReader(body), params["boundary"]).NextPart()
	if err != nil {
		t.Fatalf("multipart bulk data HTTP %d: %v", response.StatusCode, err)
	}
	if got, _ := io.ReadAll(part); !bytes.Equal(got, pixels) {
		t.Fatal("multipart bulk data differs")
	}
	response, body = get(`/dicomweb/studies/`+part10MetaStudy+`/metadata`, `multipart/related; type="application/dicom+xml"`)
	if response.StatusCode != http.StatusOK || !bytes.Contains(body, []byte("BulkData")) {
		t.Fatalf("XML metadata HTTP %d: %.300s", response.StatusCode, body)
	}
}

// TestPart10BulkDataMatchesTheLibraryByteOrderConversion checks that the
// octet-stream value of Big Endian Pixel Data is what
// pixeldata.TranscodeDataSet gives in Explicit VR Little Endian, including
// OW Pixel Data with Bits Allocated 32, which is reversed at the sample width.
func TestPart10BulkDataMatchesTheLibraryByteOrderConversion(t *testing.T) {
	for _, bits := range []uint16{16, 32} {
		order := binary.BigEndian
		us := func(element, value uint16) core.Element {
			return dicomtest.Uint16Element(core.NewTag(0x0028, element), core.VRUS, order, value)
		}
		pixels := make([]uint32, 64)
		for i := range pixels {
			pixels[i] = uint32(i)*0x01020304 + 0x0A0B
		}
		pixel := dicomtest.Uint32Element(core.TagPixelData, core.VROW, order, pixels...)
		columns := uint16(8)
		if bits == 16 {
			words := make([]uint16, 2*len(pixels))
			for i, value := range pixels {
				words[2*i], words[2*i+1] = uint16(value>>16), uint16(value)
			}
			pixel = dicomtest.Uint16Element(core.TagPixelData, core.VROW, order, words...)
			columns = 16
		}
		data, err := dicomtest.Part10File(transfer.ExplicitVRBigEndian,
			dicomtest.StringElement(core.NewTag(0x0008, 0x0016), core.VRUI, "1.2.840.10008.5.1.4.1.1.7"),
			dicomtest.StringElement(core.NewTag(0x0008, 0x0018), core.VRUI, part10MetaSOP),
			us(0x0002, 1), dicomtest.StringElement(core.NewTag(0x0028, 0x0004), core.VRCS, "MONOCHROME2"),
			us(0x0010, 8), us(0x0011, columns), us(0x0100, bits), us(0x0101, bits), us(0x0102, bits-1), us(0x0103, 0),
			pixel)
		if err != nil {
			t.Fatal(err)
		}
		file, err := object.ReadFile(bytes.NewReader(data))
		if err != nil {
			t.Fatal(err)
		}
		little, _, err := pixeldata.TranscodeDataSet(context.Background(), file.Dataset, transfer.ExplicitVRBigEndian, transfer.ExplicitVRLittleEndian, pixeldata.TranscodeOptions{})
		if err != nil {
			t.Fatal(err)
		}
		want, _ := little.Get(core.TagPixelData)
		got := part10MetaBulk(t, data, []parser.BulkDataPathStep{{Tag: core.TagPixelData, ItemIndex: -1}})
		if raw, ok := want.Value.(core.RawValue); !ok || !bytes.Equal(got, raw.Bytes()) {
			t.Errorf("Bits Allocated %d: the bulk data is not the library's Little Endian Pixel Data", bits)
		}
	}
}
