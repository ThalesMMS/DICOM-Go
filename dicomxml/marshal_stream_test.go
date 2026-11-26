package dicomxml

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/binary"
	"encoding/xml"
	"errors"
	"io"
	"math"
	"strings"
	"testing"

	"github.com/ThalesMMS/dicom-go/core"
	"github.com/ThalesMMS/dicom-go/dictionary/std"
	"github.com/ThalesMMS/dicom-go/object"
	"github.com/ThalesMMS/dicom-go/transfer"
)

func TestEncodeStreamingNumericLexicalValues(t *testing.T) {
	tests := []struct {
		vr    core.VR
		value core.Value
		want  []string
	}{
		{core.VRSV, core.Int64Value{math.MinInt64, -9007199254740993, math.MaxInt64}, []string{"-9223372036854775808", "-9007199254740993", "9223372036854775807"}},
		{core.VRUV, core.Uint64Value{0, 9007199254740993, math.MaxUint64}, []string{"0", "9007199254740993", "18446744073709551615"}},
		{core.VRFD, core.Float64Value{math.Copysign(0, -1), 1e-7, 1e-6, 1e20, 1e21, math.NaN(), math.Inf(1), math.Inf(-1)}, []string{"-0", "1e-7", "0.000001", "100000000000000000000", "1e+21", "NaN", "Infinity", "-Infinity"}},
		{core.VRFL, core.Float32Value{0.1, -1.25}, []string{"0.10000000149011612", "-1.25"}},
		{core.VRAT, core.TagValue{core.TagPixelData, core.NewTag(0x0008, 0x0005)}, []string{"7FE00010", "00080005"}},
	}
	for _, tc := range tests {
		for _, order := range []binary.ByteOrder{binary.LittleEndian, binary.BigEndian} {
			obj := object.FromElements([]core.Element{{Header: core.ElementHeader{Tag: core.NewTag(0x0072, 0x0082), VR: tc.vr}, Value: tc.value}}, std.Dictionary)
			obj.SetValueByteOrder(order)
			output, err := Marshal(obj, Options{})
			if err != nil {
				t.Fatal(err)
			}
			var model struct {
				Attribute struct {
					Values []string `xml:"Value"`
				} `xml:"DicomAttribute"`
			}
			if err := xml.Unmarshal(output, &model); err != nil {
				t.Fatal(err)
			}
			if got := strings.Join(model.Attribute.Values, "|"); got != strings.Join(tc.want, "|") {
				t.Fatalf("%s %T got %q want %q", tc.vr, order, got, tc.want)
			}
		}
	}
}

type xmlEncodingSource struct {
	*bytes.Reader
	readBytes int64
}

func (r *xmlEncodingSource) Read(p []byte) (int, error) {
	n, err := r.Reader.Read(p)
	r.readBytes += int64(n)
	return n, err
}
func xmlDeferredPixelObject(t *testing.T) (*object.Object, *xmlEncodingSource, []byte) {
	t.Helper()
	payload := bytes.Repeat([]byte{1, 2, 3, 4}, 1<<18)
	wire := append([]byte{0xe0, 0x7f, 0x10, 0x00, 'O', 'B', 0, 0, 0, 0, 0x10, 0}, payload...)
	source := &xmlEncodingSource{Reader: bytes.NewReader(wire)}
	obj, err := object.ReadDataSetWithOptions(source, transfer.ExplicitVRLittleEndian, object.ReadFileOptions{DeferPixelData: true})
	if err != nil {
		t.Fatal(err)
	}
	source.readBytes = 0
	return obj, source, payload
}

type xmlFailWriter struct {
	count, after int
	err          error
}

func (w *xmlFailWriter) Write(data []byte) (int, error) {
	if w.count+len(data) > w.after {
		return 0, w.err
	}
	w.count += len(data)
	return len(data), nil
}

func TestEncodeStreamingDeferredStopsOnWriterFailureAndAdmission(t *testing.T) {
	obj, source, _ := xmlDeferredPixelObject(t)
	marker := errors.New("writer stopped")
	w := &xmlFailWriter{after: 8192, err: marker}
	if err := Encode(w, obj, Options{}); !errors.Is(err, marker) {
		t.Fatalf("writer error %v", err)
	}
	if source.readBytes == 0 || source.readBytes >= 1<<18 {
		t.Fatalf("deferred encoder read %d bytes before noticing writer failure", source.readBytes)
	}
	pos, _ := source.Seek(0, io.SeekCurrent)
	if pos != source.Size() {
		t.Fatalf("replay changed caller cursor: %d", pos)
	}
	source.readBytes = 0
	if err := Encode(io.Discard, obj, Options{Limits: Limits{MaxInlineBinaryBytes: 1024}}); !errors.Is(err, ErrMaxInlineBytesExceeded) {
		t.Fatalf("admission error %v", err)
	}
	if source.readBytes != 0 {
		t.Fatalf("limit read %d deferred bytes", source.readBytes)
	}
}

type xmlCancelWriter struct {
	cancel context.CancelFunc
	bytes  int
}

func (w *xmlCancelWriter) Write(data []byte) (int, error) {
	w.bytes += len(data)
	if w.bytes > 8192 {
		w.cancel()
	}
	return len(data), nil
}
func TestEncodeStreamingCancellationInterruptsDeferredPayload(t *testing.T) {
	obj, source, _ := xmlDeferredPixelObject(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	w := &xmlCancelWriter{cancel: cancel}
	if err := EncodeContext(ctx, w, obj, Options{}); !errors.Is(err, context.Canceled) {
		t.Fatalf("cancel error %v", err)
	}
	if source.readBytes >= 1<<18 {
		t.Fatalf("cancel read %d bytes", source.readBytes)
	}
	var output bytes.Buffer
	if err := EncodeContext(ctx, &output, obj, Options{}); !errors.Is(err, context.Canceled) || output.Len() != 0 {
		t.Fatalf("pre-cancel wrote %d bytes: %v", output.Len(), err)
	}
}

func TestEncodeStreamingDeferredAndFragmentBinaryMatchWire(t *testing.T) {
	obj, _, payload := xmlDeferredPixelObject(t)
	check := func(obj *object.Object, want []byte) {
		t.Helper()
		for _, pretty := range []bool{false, true} {
			out, err := Marshal(obj, Options{Pretty: pretty})
			if err != nil {
				t.Fatal(err)
			}
			var model struct {
				Attribute struct {
					Inline string `xml:"InlineBinary"`
				} `xml:"DicomAttribute"`
			}
			if err := xml.Unmarshal(out, &model); err != nil {
				t.Fatal(err)
			}
			got, err := base64.StdEncoding.DecodeString(model.Attribute.Inline)
			if err != nil || !bytes.Equal(got, want) {
				t.Fatalf("binary mismatch got %d want %d, err %v", len(got), len(want), err)
			}
		}
	}
	check(obj, payload)
	seq := core.FragmentSequence{OffsetTable: []byte{0, 0, 0, 0}, Fragments: [][]byte{{1, 2, 3}, {4, 5, 6, 7}}}
	obj = object.FromElements([]core.Element{{Header: core.ElementHeader{Tag: core.TagPixelData, VR: core.VROB}, Value: seq}}, std.Dictionary)
	wire := []byte{0xfe, 0xff, 0x00, 0xe0, 4, 0, 0, 0, 0, 0, 0, 0, 0xfe, 0xff, 0x00, 0xe0, 4, 0, 0, 0, 1, 2, 3, 0, 0xfe, 0xff, 0x00, 0xe0, 4, 0, 0, 0, 4, 5, 6, 7, 0xfe, 0xff, 0xdd, 0xe0, 0, 0, 0, 0}
	check(obj, wire)
}

func TestEncodeStreamingBinaryBudgetsAndLegacyBulkCallback(t *testing.T) {
	obj := object.FromElements([]core.Element{
		core.NewRawElement(core.NewTag(0x0028, 0x2000), core.VROB, []byte{1, 2, 3, 4}),
		core.NewRawElement(core.TagPixelData, core.VROB, []byte{5, 6, 7, 8}),
	}, std.Dictionary)
	if err := Encode(io.Discard, obj, Options{Limits: Limits{MaxTotalBinaryBytes: 7}}); !errors.Is(err, ErrMaxBinaryBytesExceeded) {
		t.Fatalf("aggregate %v", err)
	}
	called := 0
	opts := Options{Limits: Limits{MaxInlineBinaryBytes: 1, MaxTotalBinaryBytes: 1}, BulkDataURIFunc: func(tag core.Tag, vr core.VR, raw []byte) string {
		called++
		if len(raw) != 4 {
			t.Fatalf("callback bytes %d", len(raw))
		}
		return "urn:uuid:1234"
	}}
	out, err := Marshal(obj, opts)
	if err != nil || called != 2 || strings.Count(string(out), `<BulkData uuid="1234">`) != 2 {
		t.Fatalf("bulk calls=%d err=%v output=%s", called, err, out)
	}
	called = 0
	marker := errors.New("write rejected")
	if err := Encode(&xmlFailWriter{after: 0, err: marker}, obj, opts); !errors.Is(err, marker) || called != 0 {
		t.Fatalf("eager callback calls=%d err=%v", called, err)
	}
}

func TestEncodeStreamingPersonNameLimitAndSequenceAdmission(t *testing.T) {
	obj := object.FromElements([]core.Element{{Header: core.ElementHeader{Tag: core.NewTag(0x0010, 0x0010), VR: core.VRPN}, Value: core.StringValue{"Four^Five"}}}, std.Dictionary)
	if err := Encode(io.Discard, obj, Options{Limits: Limits{MaxValueBytes: 4}}); !errors.Is(err, ErrMaxValueBytesExceeded) {
		t.Fatalf("PN limit: %v", err)
	}
	seq := core.SequenceValue{Items: []core.DataSet{{}, {}}}
	obj = object.FromElements([]core.Element{{Header: core.ElementHeader{Tag: core.NewTag(0x0008, 0x1115), VR: core.VRSQ}, Value: seq}}, std.Dictionary)
	if err := Encode(io.Discard, obj, Options{Limits: Limits{MaxSequenceItems: 1}}); !errors.Is(err, ErrMaxItemsExceeded) {
		t.Fatalf("item limit: %v", err)
	}
}
