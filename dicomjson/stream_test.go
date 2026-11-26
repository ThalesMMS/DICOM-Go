package dicomjson

import (
	"bytes"
	"context"
	"encoding/binary"
	"encoding/json"
	"errors"
	"io"
	"math"
	"runtime"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/ThalesMMS/dicom-go/core"
	"github.com/ThalesMMS/dicom-go/dictionary/std"
	"github.com/ThalesMMS/dicom-go/internal/dicomtest"
	"github.com/ThalesMMS/dicom-go/object"
	"github.com/ThalesMMS/dicom-go/transfer"
)

func streamFixture() *object.Object {
	obj := object.FromElements([]core.Element{
		dicomtest.NewStringElement(core.NewTag(0x0008, 0x0005), core.VRCS, "ISO_IR 192"),
		dicomtest.NewStringElement(core.NewTag(0x0010, 0x0010), core.VRPN, "Name^<é>&=表意=ふりがな"),
		dicomtest.NewStringElement(core.NewTag(0x0010, 0x0020), core.VRLO, "ID \" slash /"),
		dicomtest.NewStringElement(core.NewTag(0x0018, 0x0050), core.VRDS, "1.25"),
		dicomtest.NewOBElement(core.NewTag(0x0011, 0x1010), []byte{0, 1, 2, 3}),
		dicomtest.NewSequenceElement(core.NewTag(0x0008, 0x1111), core.DataSet{Elements: []core.Element{dicomtest.NewStringElement(core.NewTag(0x0008, 0x1155), core.VRUI, "1.2.3")}}, core.DataSet{}),
	}, std.Dictionary)
	return obj
}
func TestStreamEncoderPreservesCompactPrettyAndArrayBytes(t *testing.T) {
	for _, pretty := range []bool{false, true} {
		opts := Options{Pretty: pretty, OmitGroupLength: true}
		obj := streamFixture()
		expected, err := Marshal(obj, opts)
		if err != nil {
			t.Fatal(err)
		}
		var out bytes.Buffer
		if err := NewEncoder(&out, opts).Encode(obj); err != nil {
			t.Fatal(err)
		}
		if !bytes.Equal(out.Bytes(), expected) {
			t.Fatalf("pretty=%t\ngot %s\nwant %s", pretty, out.Bytes(), expected)
		}
		compact, _ := Marshal(obj, Options{OmitGroupLength: true})
		var arrays bytes.Buffer
		n := 0
		err = NewEncoder(&arrays, opts).EncodeArrayContext(context.Background(), func() (*object.Object, error) {
			if n == 2 {
				return nil, io.EOF
			}
			n++
			return obj, nil
		})
		if err != nil {
			t.Fatal(err)
		}
		arrayExpected := append(append(append([]byte{'['}, compact...), ','), compact...)
		arrayExpected = append(arrayExpected, ']')
		if pretty {
			var indented bytes.Buffer
			if err := json.Indent(&indented, arrayExpected, "", "  "); err != nil {
				t.Fatal(err)
			}
			arrayExpected = indented.Bytes()
		}
		if !bytes.Equal(arrays.Bytes(), arrayExpected) {
			t.Fatalf("array pretty=%t: %s != %s", pretty, arrays.Bytes(), arrayExpected)
		}
	}
}
func TestStreamEncoderPreservesPrivateUNSequenceBigEndianAndBulk(t *testing.T) {
	obj := object.FromElements([]core.Element{
		{Header: core.ElementHeader{Tag: core.NewTag(0x0011, 0x1001), VR: core.VRUN}, Value: core.SequenceValue{Items: []core.DataSet{{Elements: []core.Element{dicomtest.NewStringElement(core.NewTag(0x0010, 0x0020), core.VRLO, "PRIVATE")}}}}},
		{Header: core.ElementHeader{Tag: core.NewTag(0x0028, 0x0010), VR: core.VRUS}, Value: core.RawValue{0x12, 0x34}},
	}, std.Dictionary)
	obj.SetValueByteOrder(binary.BigEndian)
	for _, opts := range []Options{{}, {Pretty: true, BulkDataURIFunc: func(core.Tag, core.VR, []byte) string { return "/bulk/1" }}} {
		expected, err := Marshal(obj, opts)
		if err != nil {
			t.Fatal(err)
		}
		var out bytes.Buffer
		if err := EncodeContext(context.Background(), &out, obj, opts); err != nil {
			t.Fatal(err)
		}
		if !bytes.Equal(out.Bytes(), expected) {
			t.Fatalf("bytes differ: %s vs %s", out.Bytes(), expected)
		}
	}
}
func TestStreamEncoderPreservesEncapsulatedPixels(t *testing.T) {
	obj := object.FromElements([]core.Element{{Header: core.ElementHeader{Tag: core.TagPixelData, VR: core.VROB}, Value: core.FragmentSequence{Fragments: [][]byte{{1, 2, 3, 4}}}}}, std.Dictionary)
	expected, err := Marshal(obj, Options{})
	if err != nil {
		t.Fatal(err)
	}
	var out bytes.Buffer
	if err := EncodeContext(context.Background(), &out, obj, Options{}); err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(out.Bytes(), expected) {
		t.Fatal("encapsulated bytes differ")
	}
	decoded, err := NewDecoder(bytes.NewReader(out.Bytes()), std.Dictionary, UnmarshalOptions{TransferSyntax: transfer.JPEGLSLossless}).Decode()
	if err != nil {
		t.Fatal(err)
	}
	pixel, _ := decoded.Get(core.TagPixelData)
	if _, ok := pixel.Value.(core.FragmentSequence); !ok {
		t.Fatalf("pixel value %T", pixel.Value)
	}
}
func TestStreamLimitsAreCumulativeAndWriterFailuresPropagate(t *testing.T) {
	obj := streamFixture()
	raw, err := Marshal(obj, Options{})
	if err != nil {
		t.Fatal(err)
	}
	for _, limit := range []int64{int64(len(raw)), int64(len(raw)*2 + 2)} {
		var out bytes.Buffer
		n := 0
		err := NewEncoder(&out, Options{Limits: Limits{MaxJSONBytes: limit}}).EncodeArrayContext(context.Background(), func() (*object.Object, error) {
			if n == 2 {
				return nil, io.EOF
			}
			n++
			return obj, nil
		})
		if !errors.Is(err, ErrMaxJSONBytesExceeded) {
			t.Fatalf("limit %d got %v", limit, err)
		}
		if int64(out.Len()) > limit {
			t.Fatal("wrote beyond cap")
		}
	}
	sentinel := errors.New("writer failed")
	if err := EncodeContext(context.Background(), streamFailureWriter{sentinel}, obj, Options{}); !errors.Is(err, sentinel) {
		t.Fatalf("writer error %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	var out bytes.Buffer
	if err := EncodeContext(ctx, &out, obj, Options{}); !errors.Is(err, context.Canceled) || out.Len() != 0 {
		t.Fatalf("canceled writer %v/%d", err, out.Len())
	}
}

type streamFailureWriter struct{ err error }

func (w streamFailureWriter) Write([]byte) (int, error) { return 0, w.err }
func TestStreamDecoderCallbacksBeforeEOFAndPartialConsumption(t *testing.T) {
	first := `{"00280010":{"vr":"US","Value":[42]},"00100020":{"vr":"LO","Value":["ONE"]}}`
	reader, writer := io.Pipe()
	callback := make(chan struct{})
	done := make(chan error, 1)
	go func() {
		done <- NewDecoder(reader, std.Dictionary, UnmarshalOptions{}).DecodeDatasetsContext(context.Background(), func(ds Dataset) error {
			if _, ok := ds["00280010"].Value[0].(json.Number); !ok {
				t.Errorf("numeric token %T", ds["00280010"].Value[0])
			}
			close(callback)
			return nil
		})
	}()
	if _, err := io.WriteString(writer, "["+first+","); err != nil {
		t.Fatal(err)
	}
	<-callback // The producer has not emitted the remainder or EOF.
	if _, err := io.WriteString(writer, `{"00280010":{"vr":"US","Value":[-1]}}]`); err != nil {
		t.Fatal(err)
	}
	_ = writer.Close()
	if err := <-done; err == nil {
		t.Fatal("invalid later dataset admitted")
	}
	_ = reader.Close()
	for _, suffix := range []string{",broken]", "]{}", "] trailing"} {
		count := 0
		err := NewDecoder(strings.NewReader("["+first+suffix), std.Dictionary, UnmarshalOptions{}).DecodeArrayContext(context.Background(), func(*object.Object) error { count++; return nil })
		if err == nil || count != 1 {
			t.Fatalf("suffix %q count=%d err=%v", suffix, count, err)
		}
	}
}
func TestStreamDecoderLimitsAndCallbackCancellation(t *testing.T) {
	data := `[{"00110010":{"vr":"OB","InlineBinary":"AQI="}},{"00110010":{"vr":"OB","InlineBinary":"AQI="}}]`
	for _, test := range []struct {
		limits Limits
		want   error
	}{{Limits{MaxTotalBinaryBytes: 3}, ErrMaxTotalBinaryBytesExceeded}, {Limits{MaxElements: 1}, ErrMaxElementsExceeded}, {Limits{MaxJSONBytes: int64(len(data) - 1)}, ErrMaxJSONBytesExceeded}} {
		count := 0
		err := NewDecoder(strings.NewReader(data), std.Dictionary, UnmarshalOptions{Limits: test.limits}).DecodeArrayContext(context.Background(), func(*object.Object) error { count++; return nil })
		if !errors.Is(err, test.want) {
			t.Fatalf("limit err %v expected %v", err, test.want)
		}
		if !errors.Is(test.want, ErrMaxJSONBytesExceeded) && count > 1 {
			t.Fatal("callback beyond traversal bound")
		}
	}
	ctx, cancel := context.WithCancel(context.Background())
	count := 0
	err := NewDecoder(strings.NewReader(data), std.Dictionary, UnmarshalOptions{}).DecodeDatasetsContext(ctx, func(Dataset) error { count++; cancel(); return nil })
	if !errors.Is(err, context.Canceled) || count != 1 {
		t.Fatalf("cancel count%d error%v", count, err)
	}
	sentinel := errors.New("stop callback")
	err = NewDecoder(strings.NewReader(data), std.Dictionary, UnmarshalOptions{}).DecodeDatasetsContext(context.Background(), func(Dataset) error { return sentinel })
	if !errors.Is(err, sentinel) {
		t.Fatalf("callback error %v", err)
	}
}
func TestRepresentedDatasetByteParityAndValidation(t *testing.T) {
	nested := map[string]any{"00080018": map[string]any{"Value": []any{"1.2.3"}, "vr": "UI"}, "00100010": map[string]any{"vr": "PN", "Value": []any{map[string]any{"Phonetic": "P", "Alphabetic": "<A>"}}}}
	ds := Dataset{"00081111": {VR: "SQ", Value: []any{nested, Dataset{}, map[string]Element{"00100020": {VR: "LO", Value: []any{"é"}}}}}, "00280010": {VR: "US", Value: []any{json.Number("42")}}, "00110010": {VR: "OB", InlineBinary: "AQI="}}
	want, err := json.Marshal(ds)
	if err != nil {
		t.Fatal(err)
	}
	var out bytes.Buffer
	if err := EncodeDatasetContext(context.Background(), &out, ds, DatasetOptions{}); err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(out.Bytes(), want) {
		t.Fatalf("got %s\nwant %s", out.Bytes(), want)
	}
	for _, bad := range []Dataset{
		{"00100010": {VR: "PN", Value: []any{"string"}}}, {"00280010": {VR: "US", Value: []any{65536}}}, {"00100020": {VR: "LO", Value: []any{"A\\B"}}}, {"00181020": {VR: "DS", Value: []any{"bad"}}},
		{"00081111": {VR: "SQ", Value: []any{map[string]any{"00100020": map[string]any{"vr": "LO", "Unknown": true}}}}},
	} {
		if err := ValidateDatasetContext(context.Background(), bad, DatasetOptions{}); err == nil {
			t.Fatalf("invalid dataset admitted: %v", bad)
		}
	}
}

func TestRepresentedTypedSliceAdmissionAndCycles(t *testing.T) {
	nested := map[string]any{"00100020": map[string]any{"vr": "LO", "Value": []string{"ONE", "TWO"}}}
	ds := Dataset{"00081111": {VR: "SQ", Value: []any{nested}}}
	var out bytes.Buffer
	want, _ := json.Marshal(ds)
	if err := EncodeDatasetContext(context.Background(), &out, ds, DatasetOptions{}); err != nil || !bytes.Equal(out.Bytes(), want) {
		t.Fatalf("typed slice parity %s/%s %v", out.Bytes(), want, err)
	}
	if err := ValidateDatasetContext(context.Background(), ds, DatasetOptions{MaxValues: 3}); !errors.Is(err, ErrMaxValuesExceeded) {
		t.Fatalf("typed slice cap %v", err)
	}
	cycle := Dataset{}
	cycle["00081111"] = Element{VR: "SQ", Value: []any{cycle}}
	if err := ValidateDatasetContext(context.Background(), cycle, DatasetOptions{Limits: Limits{MaxSequenceDepth: 2}}); !errors.Is(err, ErrMaxSequenceDepthExceeded) {
		t.Fatalf("cycle cap %v", err)
	}
}

func TestStreamDecoderNeverDeliversDatasetPastByteBoundary(t *testing.T) {
	first := `{"00100020":{"vr":"LO","Value":["ONE"]}}`
	input := "[" + first + "]"
	for _, raw := range []bool{false, true} {
		count := 0
		decoder := NewDecoder(strings.NewReader(input), std.Dictionary, UnmarshalOptions{Limits: Limits{MaxJSONBytes: int64(1 + len(first) - 1)}})
		var err error
		if raw {
			err = decoder.DecodeDatasetsContext(context.Background(), func(Dataset) error { count++; return nil })
		} else {
			err = decoder.DecodeArrayContext(context.Background(), func(*object.Object) error { count++; return nil })
		}
		if !errors.Is(err, ErrMaxJSONBytesExceeded) || count != 0 {
			t.Fatalf("raw%t count%d err%v", raw, count, err)
		}
	}
}

func TestStreamDecoderAdmitsMaxInt64ByteCeiling(t *testing.T) {
	options := UnmarshalOptions{Limits: Limits{MaxJSONBytes: math.MaxInt64}}
	obj, err := NewDecoder(strings.NewReader(`{"00100020":{"vr":"LO","Value":["ONE"]}}`), std.Dictionary, options).Decode()
	if err != nil || obj == nil {
		t.Fatalf("single object %v %v", obj, err)
	}
	for _, raw := range []bool{false, true} {
		count := 0
		decoder := NewDecoder(strings.NewReader(`[{}]`), std.Dictionary, options)
		var err error
		if raw {
			err = decoder.DecodeDatasetsContext(context.Background(), func(Dataset) error { count++; return nil })
		} else {
			err = decoder.DecodeArrayContext(context.Background(), func(*object.Object) error { count++; return nil })
		}
		if err != nil || count != 1 {
			t.Fatalf("array raw%t count%d error%v", raw, count, err)
		}
	}
}

type streamBytesAndErrorReader struct {
	data []byte
	err  error
	sent bool
}

func (r *streamBytesAndErrorReader) Read(p []byte) (int, error) {
	if r.sent {
		return 0, io.EOF
	}
	n := copy(p, r.data)
	r.sent = true
	return n, r.err
}
func TestStreamDecoderPreservesErrorReturnedWithCompleteJSON(t *testing.T) {
	sentinel := errors.New("read failed with bytes")
	input := `{"00100020":{"vr":"LO","Value":["ONE"]}}`
	_, err := NewDecoder(&streamBytesAndErrorReader{data: []byte(input), err: sentinel}, std.Dictionary, UnmarshalOptions{}).Decode()
	if !errors.Is(err, sentinel) {
		t.Fatalf("single terminal reader error %v", err)
	}
	for _, raw := range []bool{false, true} {
		count := 0
		decoder := NewDecoder(&streamBytesAndErrorReader{data: []byte("[" + input + "]"), err: sentinel}, std.Dictionary, UnmarshalOptions{})
		var err error
		if raw {
			err = decoder.DecodeDatasetsContext(context.Background(), func(Dataset) error { count++; return nil })
		} else {
			err = decoder.DecodeArrayContext(context.Background(), func(*object.Object) error { count++; return nil })
		}
		if !errors.Is(err, sentinel) || count != 1 {
			t.Fatalf("array raw%t count%d terminal err%v", raw, count, err)
		}
	}
}

// This context trips only from streamJSONReader, so the failure occurs during
// base64 validation rather than during the earlier object/element traversal.
type streamCancelAtReadContext struct {
	context.Context
	err    error
	failAt int32
	reads  *atomic.Int32
}

func (c streamCancelAtReadContext) Err() error {
	if c.reads.Load() >= c.failAt {
		return c.err
	}
	pcs := make([]uintptr, 24)
	n := runtime.Callers(1, pcs)
	frames := runtime.CallersFrames(pcs[:n])
	reader, base64Reader := false, false
	for {
		frame, more := frames.Next()
		reader = reader || strings.Contains(frame.Function, "streamJSONReader).Read")
		base64Reader = base64Reader || strings.Contains(frame.Function, "encoding/base64.")
		if !more {
			break
		}
	}
	if reader && base64Reader && c.reads.Add(1) >= c.failAt {
		return c.err
	}
	return nil
}
func TestRepresentedInlineBinaryCancellationRemainsTyped(t *testing.T) {
	for _, expected := range []error{context.Canceled, context.DeadlineExceeded} {
		for _, failAt := range []int32{1, 3} {
			reads := &atomic.Int32{}
			ctx := streamCancelAtReadContext{Context: context.Background(), err: expected, failAt: failAt, reads: reads}
			ds := Dataset{"00110010": {VR: "OB", InlineBinary: strings.Repeat("AQID", 64)}}
			err := EncodeDatasetContext(ctx, io.Discard, ds, DatasetOptions{})
			if !errors.Is(err, expected) || reads.Load() < failAt {
				t.Fatalf("inline cancellation lost: %v expected%v base64checks%d", err, expected, reads.Load())
			}
			reads.Store(0)
			encoded, _ := json.Marshal(ds)
			err = NewDecoder(strings.NewReader("["+string(encoded)+"]"), std.Dictionary, UnmarshalOptions{}).DecodeDatasetsContext(ctx, func(Dataset) error { t.Fatal("canceled dataset delivered"); return nil })
			if !errors.Is(err, expected) || reads.Load() < failAt {
				t.Fatalf("decode inline cancellation lost%v base64checks%d", err, reads.Load())
			}
		}
	}
}
