package dicomjson

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"errors"
	"io"
	"reflect"
	"testing"

	"github.com/ThalesMMS/dicom-go/core"
	"github.com/ThalesMMS/dicom-go/dictionary/std"
	"github.com/ThalesMMS/dicom-go/internal/dicomtest"
	"github.com/ThalesMMS/dicom-go/object"
	"github.com/ThalesMMS/dicom-go/parser"
	"github.com/ThalesMMS/dicom-go/transfer"
)

// After parsing, payload reads are forbidden. Successful URI export proves
// that metadata lookup/export does not read or materialize the deferred bytes.
type bulkReferenceReadGuard struct {
	*bytes.Reader
	forbid bool
	reads  int
}

func (r *bulkReferenceReadGuard) Read(p []byte) (int, error) {
	if r.forbid {
		r.reads++
		return 0, errors.New("payload read forbidden")
	}
	return r.Reader.Read(p)
}

func TestBulkReferenceURIExportNestedWithoutPayloadRead(t *testing.T) {
	seq := core.NewTag(0x0008, 0x1115)
	tag := core.NewTag(0x0042, 0x0011)
	value := core.NewRawElement(tag, core.VROB, []byte{1, 2, 3, 4})
	wire := dicomtest.EncodeElements(transfer.ExplicitVRLittleEndian, value,
		dicomtest.NewSequenceElement(seq, core.DataSet{Elements: []core.Element{value}}, core.DataSet{Elements: []core.Element{value}}))
	source := &bulkReferenceReadGuard{Reader: bytes.NewReader(wire)}
	obj, err := object.ReadDataSetWithOptions(source, transfer.ExplicitVRLittleEndian, object.ReadFileOptions{BulkDataDescriptor: &parser.BulkDataDescriptor{Rules: []parser.BulkDataRule{{VR: core.VROB}}}})
	if err != nil {
		t.Fatal(err)
	}
	source.forbid = true
	var refs []object.BulkDataReference
	options := Options{MaxMaterializedValueBytes: 1, Limits: Limits{MaxInlineBinaryBytes: 1}, BulkDataReferenceURIFunc: func(ctx context.Context, ref object.BulkDataReference) (string, error) {
		if ctx.Err() != nil {
			t.Fatal(ctx.Err())
		}
		refs = append(refs, ref)
		return "https://example.invalid/bulk", nil
	}}
	encoded, err := MarshalContext(context.Background(), obj, options)
	if err != nil {
		t.Fatal(err)
	}
	if len(refs) != 3 || source.reads != 0 {
		t.Fatalf("refs=%d forbidden reads=%d", len(refs), source.reads)
	}
	if !reflect.DeepEqual(refs[2].Path, []parser.BulkDataPathStep{{Tag: tag, ItemIndex: -1}}) {
		t.Fatalf("root path=%v", refs[0].Path)
	}
	for i := 0; i < 2; i++ {
		ref := refs[i]
		want := []parser.BulkDataPathStep{{Tag: seq, ItemIndex: i}, {Tag: tag, ItemIndex: -1}}
		if !reflect.DeepEqual(ref.Path, want) || !bytes.Equal(wire[ref.Offset:ref.Offset+ref.Length], []byte{1, 2, 3, 4}) {
			t.Fatalf("nested reference=%+v", ref)
		}
	}
	if bytes.Count(encoded, []byte("BulkDataURI")) != 3 {
		t.Fatal(string(encoded))
	}
	refs = nil
	var out bytes.Buffer
	if err := EncodeContext(context.Background(), &out, obj, options); err != nil || !bytes.Equal(out.Bytes(), encoded) {
		t.Fatalf("stream error=%v output=%s", err, out.String())
	}
	if source.reads != 0 {
		t.Fatal("stream export replayed")
	}
}

func TestBulkReferenceEmptyURIFallbackBinaryNumericAndCharset(t *testing.T) {
	seq := core.NewTag(0x0008, 0x1115)
	blob := core.NewTag(0x0042, 0x0011)
	numeric := core.NewTag(0x0028, 0x0010)
	text := core.NewTag(0x0040, 0xa160)
	charset := core.NewRawElement(core.NewTag(0x0008, 0x0005), core.VRCS, []byte("ISO_IR 100"))
	item := core.DataSet{Elements: []core.Element{
		core.NewRawElement(blob, core.VROL, []byte{0, 0, 0, 42}),
		core.NewRawElement(numeric, core.VRUS, []byte{0, 42}),
		core.NewRawElement(text, core.VRUT, []byte{'c', 'a', 'f', 0xe9}),
	}}
	wire := dicomtest.EncodeElements(transfer.ExplicitVRBigEndian, charset, dicomtest.NewSequenceElement(seq, item))
	resident, err := object.ReadDataSetWithOptions(bytes.NewReader(wire), transfer.ExplicitVRBigEndian, object.ReadFileOptions{})
	if err != nil {
		t.Fatal(err)
	}
	deferred, err := object.ReadDataSetWithOptions(bytes.NewReader(wire), transfer.ExplicitVRBigEndian, object.ReadFileOptions{BulkDataDescriptor: &parser.BulkDataDescriptor{Rules: []parser.BulkDataRule{{}}}})
	if err != nil {
		t.Fatal(err)
	}
	want, err := Marshal(resident, Options{})
	if err != nil {
		t.Fatal(err)
	}
	calls := 0
	got, err := Marshal(deferred, Options{MaxMaterializedValueBytes: 16, BulkDataReferenceURIFunc: func(_ context.Context, ref object.BulkDataReference) (string, error) {
		calls++
		if ref.ByteOrder != binary.BigEndian {
			t.Fatal("wrong source order")
		}
		return "", nil
	}})
	if err != nil || !bytes.Equal(got, want) || calls != 3 {
		t.Fatalf("fallback=%s want=%s calls=%d err=%v", got, want, calls, err)
	}
	if !bytes.Contains(got, []byte("café")) || !bytes.Contains(got, []byte("42")) {
		t.Fatal(string(got))
	}
	// The legacy callback receives the bytes InlineBinary would hold: the
	// Little Endian form of the Big Endian OL by default (#1982), the source
	// bytes with SourceByteOrderInlineBinary.
	for _, source := range []bool{false, true} {
		want := []byte{42, 0, 0, 0}
		if source {
			want = []byte{0, 0, 0, 42}
		}
		oldCalls := 0
		_, err = Marshal(deferred, Options{SourceByteOrderInlineBinary: source, MaxMaterializedValueBytes: 16, BulkDataReferenceURIFunc: func(context.Context, object.BulkDataReference) (string, error) { return "", nil }, BulkDataURIFunc: func(tag core.Tag, vr core.VR, raw []byte) string {
			oldCalls++
			if tag != blob || !bytes.Equal(raw, want) {
				t.Fatalf("legacy callback representation % X, want % X", raw, want)
			}
			return "legacy"
		}})
		if err != nil || oldCalls != 1 {
			t.Fatalf("legacy calls=%d err=%v", oldCalls, err)
		}
	}
}

func TestBulkReferenceCP246SourceOrderAndFallback(t *testing.T) {
	seq := core.NewTag(0x0008, 0x1115)
	tag := core.NewTag(0x0028, 0x0011)
	wire := dicomtest.ExplicitLongHeaderBytes(binary.BigEndian, seq, core.VRUN, uint32(core.UndefinedLength))
	wire = append(wire, dicomtest.SequenceControlBytes(binary.LittleEndian, core.TagItem, 10)...)
	wire = append(wire, dicomtest.EncodeElements(transfer.ImplicitVRLittleEndian, core.NewRawElement(tag, core.VRUS, []byte{0, 1}))...)
	wire = append(wire, dicomtest.SequenceControlBytes(binary.LittleEndian, core.TagSequenceDelimitationItem, 0)...)
	resident, err := object.ReadDataSetWithOptions(bytes.NewReader(wire), transfer.ExplicitVRBigEndian, object.ReadFileOptions{Dictionary: std.Dictionary})
	if err != nil {
		t.Fatal(err)
	}
	deferred, err := object.ReadDataSetWithOptions(bytes.NewReader(wire), transfer.ExplicitVRBigEndian, object.ReadFileOptions{BulkDataDescriptor: &parser.BulkDataDescriptor{Rules: []parser.BulkDataRule{{VR: core.VRUS}}}})
	if err != nil {
		t.Fatal(err)
	}
	want, err := Marshal(resident, Options{})
	if err != nil {
		t.Fatal(err)
	}
	got, err := Marshal(deferred, Options{MaxMaterializedValueBytes: 16, BulkDataReferenceURIFunc: func(_ context.Context, ref object.BulkDataReference) (string, error) {
		if ref.ByteOrder != binary.LittleEndian || !bytes.Equal(wire[ref.Offset:ref.Offset+ref.Length], []byte{0, 1}) {
			t.Fatal("lost CP-246 source representation")
		}
		return "", nil
	}})
	if err != nil || !bytes.Equal(got, want) || !bytes.Contains(got, []byte("256")) {
		t.Fatalf("CP-246 fallback=%s want=%s err=%v", got, want, err)
	}
}

func TestBulkReferenceImplicitSignedContextPreserved(t *testing.T) {
	pr := core.NewTag(0x0028, 0x0103)
	leaf := core.NewTag(0x0028, 0x0106)
	seq := core.NewTag(0x0008, 0x1115)
	for _, descriptor := range []*parser.BulkDataDescriptor{
		{Rules: []parser.BulkDataRule{{}}},
		{Rules: []parser.BulkDataRule{{Path: []parser.BulkDataPathStep{{Tag: leaf, ItemIndex: -1}}}, {Path: []parser.BulkDataPathStep{{Tag: seq, ItemIndex: -1}, {Tag: leaf, ItemIndex: -1}}}}},
	} {
		// Source order deliberately places XS before its discriminant. The item
		// inherits signedness from the root; the later root PR must stay inline.
		wire := dicomtest.EncodeElements(transfer.ImplicitVRLittleEndian,
			core.NewRawElement(leaf, core.VRUS, []byte{0xff, 0xff}),
			dicomtest.NewSequenceElement(seq, core.DataSet{Elements: []core.Element{core.NewRawElement(leaf, core.VRUS, []byte{0xfe, 0xff})}}),
			core.NewRawElement(pr, core.VRUS, []byte{1, 0}))
		resident, err := object.ReadDataSetWithOptions(bytes.NewReader(wire), transfer.ImplicitVRLittleEndian, object.ReadFileOptions{})
		if err != nil {
			t.Fatal(err)
		}
		deferred, err := object.ReadDataSetWithOptions(bytes.NewReader(wire), transfer.ImplicitVRLittleEndian, object.ReadFileOptions{BulkDataDescriptor: descriptor})
		if err != nil {
			t.Fatal(err)
		}
		if elem, _ := deferred.Get(pr); elem.Value == nil {
			t.Fatal("deferred contextual discriminant")
		}
		calls := 0
		got, err := Marshal(deferred, Options{MaxMaterializedValueBytes: 16, BulkDataReferenceURIFunc: func(_ context.Context, ref object.BulkDataReference) (string, error) {
			calls++
			if ref.VR != core.VRSS || ref.ExplicitVR {
				t.Fatalf("lost resolved signed VR %+v", ref)
			}
			return "", nil
		}})
		want, e := Marshal(resident, Options{})
		if err != nil || e != nil || !bytes.Equal(got, want) || calls != 2 {
			t.Fatalf("signed fallback=%s want=%s calls=%d error=%v/%v", got, want, calls, err, e)
		}
	}
}

func TestBulkReferenceImplicitDiscriminantsAndSignedURI(t *testing.T) {
	seq := core.NewTag(0x0008, 0x1115)
	leaf := core.NewTag(0x0028, 0x0106)
	discriminants := []core.Element{
		core.NewRawElement(core.NewTag(0x0028, 0x0103), core.VRUS, []byte{1, 0}),
		core.NewRawElement(core.NewTag(0x0028, 0x0100), core.VRUS, []byte{16, 0}),
		core.NewRawElement(core.NewTag(0x0028, 0x3002), core.VRUS, []byte{1, 0, 0, 0, 16, 0}),
	}
	itemElements := append(append([]core.Element(nil), discriminants...), core.NewRawElement(leaf, core.VRUS, []byte{0xff, 0xff}))
	wire := dicomtest.EncodeElements(transfer.ImplicitVRLittleEndian, append(discriminants, dicomtest.NewSequenceElement(seq, core.DataSet{Elements: itemElements}))...)
	source := &bulkReferenceReadGuard{Reader: bytes.NewReader(wire)}
	obj, err := object.ReadDataSetWithOptions(source, transfer.ImplicitVRLittleEndian, object.ReadFileOptions{BulkDataDescriptor: &parser.BulkDataDescriptor{Rules: []parser.BulkDataRule{{}}}})
	if err != nil {
		t.Fatal(err)
	}
	items, _ := obj.GetSequence(seq)
	for _, scope := range []*object.Object{obj, items[0]} {
		for _, elem := range discriminants {
			if got, _ := scope.Get(elem.Tag()); got.Value == nil {
				t.Fatalf("deferred discriminant %s", elem.Tag())
			}
		}
	}
	source.forbid = true
	calls := 0
	encoded, err := Marshal(obj, Options{MaxMaterializedValueBytes: 1, BulkDataReferenceURIFunc: func(_ context.Context, ref object.BulkDataReference) (string, error) {
		calls++
		if ref.VR != core.VRSS || ref.ExplicitVR || len(ref.Path) != 2 {
			t.Fatalf("signed URI reference=%+v", ref)
		}
		return "signed-value", nil
	}})
	if err != nil || calls != 1 || source.reads != 0 || !bytes.Contains(encoded, []byte("signed-value")) {
		t.Fatalf("signed URI err=%v calls=%d reads=%d", err, calls, source.reads)
	}
}

func TestBulkReferenceLegacyPixelStreamKeepsCanonicalBytes(t *testing.T) {
	for _, bits := range []uint16{8, 16} {
		integer := dicomtest.Uint16Element(core.NewTag(0x0028, 0x0100), core.VRUS, binary.LittleEndian, bits)
		dataset := dicomtest.EncodeElements(transfer.ImplicitVRLittleEndian, integer, core.NewRawElement(core.TagPixelData, core.VROW, []byte{1, 2, 3, 4}))
		obj, err := object.ReadDataSetWithOptions(bytes.NewReader(dataset), transfer.PrivateGEImplicitVRBigEndianPixelData, object.ReadFileOptions{DeferPixelData: true})
		if err != nil {
			t.Fatal(err)
		}
		var canonical bytes.Buffer
		if _, err := obj.CopyValueTo(core.TagPixelData, &canonical); err != nil {
			t.Fatal(err)
		}
		_, err = Marshal(obj, Options{MaxMaterializedValueBytes: 1, PixelDataBulkDataURIFunc: func(_ core.Tag, _ core.VR, open func() (io.ReadCloser, error)) (string, error) {
			r, err := open()
			if err != nil {
				return "", err
			}
			defer r.Close()
			got, err := io.ReadAll(r)
			if err != nil {
				return "", err
			}
			if !bytes.Equal(got, canonical.Bytes()) {
				t.Fatalf("legacy %d stream=%x canonical=%x", bits, got, canonical.Bytes())
			}
			return "canonical", nil
		}})
		if err != nil {
			t.Fatal(err)
		}
		_, err = Marshal(obj, Options{MaxMaterializedValueBytes: 1, BulkDataReferenceURIFunc: func(_ context.Context, ref object.BulkDataReference) (string, error) {
			if ref.ByteOrder != binary.BigEndian || ref.TransferSyntaxUID != transfer.PrivateGEImplicitVRBigEndianPixelData.UID {
				t.Fatal("lost original legacy encoding")
			}
			return "original-source", nil
		}})
		if err != nil {
			t.Fatal(err)
		}
	}
}

func TestReferenceNormalizingWriterBoundsAndPartialUnits(t *testing.T) {
	var out bytes.Buffer
	writer := &referenceNormalizingWriter{writer: &out, width: 2}
	if _, err := writer.Write([]byte{1}); err != nil {
		t.Fatal(err)
	}
	if _, err := writer.Write([]byte{2, 3, 4}); err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(out.Bytes(), []byte{2, 1, 4, 3}) || len(writer.pending) != 0 {
		t.Fatal("partial unit normalization")
	}
}

func TestBulkReferenceLimitsCancellationAndExistingURI(t *testing.T) {
	tag := core.NewTag(0x0042, 0x0011)
	wire := dicomtest.EncodeElements(transfer.ExplicitVRLittleEndian, core.NewRawElement(tag, core.VROB, []byte{1, 2, 3, 4}))
	source := &bulkReferenceReadGuard{Reader: bytes.NewReader(wire)}
	obj, err := object.ReadDataSetWithOptions(source, transfer.ExplicitVRLittleEndian, object.ReadFileOptions{BulkDataDescriptor: &parser.BulkDataDescriptor{Rules: []parser.BulkDataRule{{}}}})
	if err != nil {
		t.Fatal(err)
	}
	source.forbid = true
	for _, opts := range []Options{
		{MaxMaterializedValueBytes: 3},
		{Limits: Limits{MaxInlineBinaryBytes: 3}},
		{Limits: Limits{MaxTotalBinaryBytes: 3}},
	} {
		if _, err := Marshal(obj, opts); err == nil {
			t.Fatal("missing preallocation limit")
		}
	}
	if source.reads != 0 {
		t.Fatal("limit replayed payload")
	}
	ctx, cancel := context.WithCancel(context.Background())
	_, err = MarshalContext(ctx, obj, Options{MaxMaterializedValueBytes: 16, BulkDataReferenceURIFunc: func(context.Context, object.BulkDataReference) (string, error) { cancel(); return "accepted", nil }})
	if !errors.Is(err, context.Canceled) || source.reads != 0 {
		t.Fatalf("cancel err=%v reads=%d", err, source.reads)
	}
	sentinel := errors.New("callback rejected")
	_, err = Marshal(obj, Options{BulkDataReferenceURIFunc: func(context.Context, object.BulkDataReference) (string, error) { return "", sentinel }})
	if !errors.Is(err, sentinel) {
		t.Fatal(err)
	}
	external := object.FromElements([]core.Element{{Header: core.ElementHeader{Tag: tag, VR: core.VROB}, Value: core.BulkDataValue{URI: "external"}}}, std.Dictionary)
	encoded, err := Marshal(external, Options{BulkDataReferenceURIFunc: func(context.Context, object.BulkDataReference) (string, error) {
		t.Fatal("offered external URI")
		return "", nil
	}})
	if err != nil || !bytes.Contains(encoded, []byte("external")) {
		t.Fatal(err)
	}
	if _, err := Marshal(obj, Options{MaxMaterializedValueBytes: -1}); !errors.Is(err, ErrInvalidLimits) {
		t.Fatal(err)
	}
}

func TestBulkReferenceHistoricalNestedFallbackAndEncapsulatedPixel(t *testing.T) {
	seq := core.NewTag(0x0008, 0x1115)
	tag := core.NewTag(0x0042, 0x0011)
	wire := dicomtest.EncodeElements(transfer.ExplicitVRLittleEndian, dicomtest.NewSequenceElement(seq, core.DataSet{Elements: []core.Element{core.NewRawElement(tag, core.VROB, []byte{1, 2, 3, 4})}}))
	obj, err := object.ReadDataSetWithOptions(bytes.NewReader(wire), transfer.ExplicitVRLittleEndian, object.ReadFileOptions{InlineValueBytesThreshold: 1, DeferNestedValues: true})
	if err != nil {
		t.Fatal(err)
	}
	encoded, err := Marshal(obj, Options{})
	if err != nil || !bytes.Contains(encoded, []byte(base64.StdEncoding.EncodeToString([]byte{1, 2, 3, 4}))) {
		t.Fatalf("legacy nested %s %v", encoded, err)
	}
	pixel := core.Element{Header: core.ElementHeader{Tag: core.TagPixelData, VR: core.VROB}, Value: core.FragmentSequence{Fragments: [][]byte{{1, 2}}}}
	wire = dicomtest.EncodeElements(transfer.JPEGBaseline, pixel)
	obj, err = object.ReadDataSetWithOptions(bytes.NewReader(wire), transfer.JPEGBaseline, object.ReadFileOptions{DeferPixelData: true})
	if err != nil {
		t.Fatal(err)
	}
	var ref object.BulkDataReference
	encoded, err = Marshal(obj, Options{BulkDataReferenceURIFunc: func(_ context.Context, r object.BulkDataReference) (string, error) { ref = r; return "pixel", nil }})
	if err != nil || ref.Length != int64(len(wire)-12) {
		t.Fatalf("encapsulated reference=%+v err=%v", ref, err)
	}
	var decoded map[string]Element
	if err := json.Unmarshal(encoded, &decoded); err != nil || decoded[core.TagPixelData.HexString()].BulkDataURI != "pixel" {
		t.Fatal(err)
	}
	var copied bytes.Buffer
	if _, err := obj.CopyBulkDataValueTo(context.Background(), ref.Path, &copied); err != nil || !bytes.Equal(copied.Bytes(), wire[12:]) {
		t.Fatal("encapsulated field missing item headers/delimiter")
	}
	_, err = Marshal(obj, Options{MaxMaterializedValueBytes: 100, PixelDataBulkDataURIFunc: func(_ core.Tag, _ core.VR, open func() (io.ReadCloser, error)) (string, error) {
		r, err := open()
		if err != nil {
			return "", err
		}
		defer r.Close()
		_, err = io.Copy(io.Discard, r)
		return "stream", err
	}})
	if err != nil {
		t.Fatal(err)
	}
}
