package spectroscopy

import (
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"io"
	"math"
	"strconv"
	"testing"
	"time"

	"github.com/ThalesMMS/dicom-go/core"
	"github.com/ThalesMMS/dicom-go/dictionary/std"
	"github.com/ThalesMMS/dicom-go/object"
	"github.com/ThalesMMS/dicom-go/transfer"
)

func testObject(order binary.ByteOrder, dims Dimensions, repr Representation, values []float32) *object.Object {
	raw := make([]byte, len(values)*4)
	for i, value := range values {
		order.PutUint32(raw[i*4:], math.Float32bits(value))
	}
	obj := object.FromElements([]core.Element{
		core.NewRawElement(tagFrames, core.VRIS, []byte(strconv.Itoa(dims.Frames))),
		object.USElement(tagVoxelRows, order, uint16(dims.VoxelRows)),
		object.USElement(tagVoxelColumns, order, uint16(dims.VoxelColumns)),
		object.ULElement(tagPointRows, order, uint32(dims.PointRows)),
		object.ULElement(tagPointColumns, order, uint32(dims.PointColumns)),
		core.NewRawElement(tagRepresentation, core.VRCS, []byte(repr)),
		core.NewRawElement(tagDomainColumns, core.VRCS, []byte(Frequency)),
		core.NewRawElement(tagData, core.VROF, raw),
	}, std.Dictionary)
	if dims.PointRows > 1 {
		obj.Put(core.NewRawElement(tagDomainRows, core.VRCS, []byte(Time)))
	}
	obj.SetValueByteOrder(order)
	return obj
}

func smallObject() *object.Object {
	return testObject(binary.LittleEndian, Dimensions{Frames: 1, VoxelRows: 1, VoxelColumns: 1, PointRows: 1, PointColumns: 2}, Complex, []float32{1, -2, 3, -4})
}

func TestReadAsymmetricGridAndSpectralOrdering(t *testing.T) {
	shape := Dimensions{Frames: 2, VoxelRows: 2, VoxelColumns: 3, PointRows: 2, PointColumns: 4}
	var values []float32
	for f := 0; f < 2; f++ {
		for vr := 0; vr < 2; vr++ {
			for vc := 0; vc < 3; vc++ {
				for pr := 0; pr < 2; pr++ {
					for pc := 0; pc < 4; pc++ {
						v := float32(f*10000 + vr*1000 + vc*100 + pr*10 + pc)
						values = append(values, v, -v-0.5)
					}
				}
			}
		}
	}
	for _, order := range []binary.ByteOrder{binary.LittleEndian, binary.BigEndian} {
		t.Run(order.String(), func(t *testing.T) {
			data, err := Read(testObject(order, shape, Complex, values), Options{})
			if err != nil || data.Dimensions != shape || len(data.Values) != len(values) {
				t.Fatalf("shape/read %v/%v", data, err)
			}
			for f := 0; f < 2; f++ {
				for vr := 0; vr < 2; vr++ {
					for vc := 0; vc < 3; vc++ {
						for pr := 0; pr < 2; pr++ {
							for pc := 0; pc < 4; pc++ {
								point, err := data.Point(f, vr, vc, pr, pc)
								v := float32(f*10000 + vr*1000 + vc*100 + pr*10 + pc)
								if err != nil || len(point) != 2 || point[0] != v || point[1] != -v-0.5 || cap(point) != 2 {
									t.Fatalf("point %v/%v", point, err)
								}
							}
						}
					}
				}
			}
			spectrum, err := data.Spectrum(1, 1, 2)
			if err != nil || len(spectrum) != 16 || cap(spectrum) != 16 || spectrum[0] != 11200 || spectrum[8] != 11210 {
				t.Fatalf("spectrum %v/%v", spectrum, err)
			}
		})
	}
}

func TestRepresentationsTypedOFAndNoAliasing(t *testing.T) {
	for _, repr := range []Representation{Real, Imaginary, Magnitude, Complex} {
		t.Run(string(repr), func(t *testing.T) {
			components, _ := repr.components()
			values := []float32{1, 2}
			if components == 2 {
				values = []float32{1, -1, 2, -2}
			}
			obj := testObject(binary.BigEndian, Dimensions{1, 1, 1, 1, 2}, repr, values)
			for _, typed := range []bool{false, true} {
				if typed {
					obj.Put(core.Element{Header: core.ElementHeader{Tag: tagData, VR: core.VROF}, Value: core.Float32Value(append([]float32(nil), values...))})
				}
				data, err := Read(obj, Options{})
				if err != nil || data.Representation != repr || len(data.Values) != len(values) {
					t.Fatalf("typed%v %v", typed, err)
				}
				point, err := data.Point(0, 0, 0, 0, 1)
				if err != nil || len(point) != components || point[0] != 2 {
					t.Fatalf("point %v/%v", point, err)
				}
				point[0] = 99
				if data.Values[components] != 99 {
					t.Fatal("point must view owned output")
				}
				element, _ := obj.Get(tagData)
				if typed {
					if element.Value.(core.Float32Value)[components] != 2 {
						t.Fatal("output aliases typed OF")
					}
				} else {
					raw := element.Value.(core.RawValue)
					if math.Float32frombits(binary.BigEndian.Uint32(raw[components*4:])) != 2 {
						t.Fatal("output aliases raw OF")
					}
				}
			}
		})
	}
}

func TestIEEE754SampleBitsPreserved(t *testing.T) {
	bits := []uint32{0x80000000, 0x7fc12345, 0x7f800000, 0xff800000, 0x00000001, 0x3f800000}
	values := make([]float32, len(bits))
	for i, b := range bits {
		values[i] = math.Float32frombits(b)
	}
	for _, order := range []binary.ByteOrder{binary.LittleEndian, binary.BigEndian} {
		for _, typed := range []bool{false, true} {
			obj := testObject(order, Dimensions{1, 1, 1, 1, len(values)}, Real, values)
			if typed {
				obj.Put(core.Element{Header: core.ElementHeader{Tag: tagData, VR: core.VROF}, Value: core.Float32Value(values)})
			}
			data, err := Read(obj, Options{})
			if err != nil {
				t.Fatal(err)
			}
			for i, bit := range bits {
				if math.Float32bits(data.Values[i]) != bit {
					t.Fatalf("typed%v word%d bits%08x want%08x", typed, i, math.Float32bits(data.Values[i]), bit)
				}
			}
		}
	}
}

func TestAxesRankMultiplicityAndExplicitReferences(t *testing.T) {
	obj := testObject(binary.BigEndian, Dimensions{1, 1, 1, 2, 4}, Real, make([]float32, 8))
	obj.Put(object.FDElement(tagSpectralWidth, binary.BigEndian, 8000, 4000))
	obj.Put(object.FDElement(tagTransmitter, binary.BigEndian, 100, 200))
	obj.Put(object.FDElement(tagChemicalShift, binary.BigEndian, 4.7, 1.2))
	data, err := Read(obj, Options{})
	if err != nil {
		t.Fatal(err)
	}
	if data.ColumnAxis.Points != 4 || data.ColumnAxis.Domain != Frequency || data.RowAxis == nil || data.RowAxis.Points != 2 || data.RowAxis.Domain != Time {
		t.Fatal("spectral axes/rank swapped")
	}
	offset, err := data.ColumnAxis.FrequencyOffsetHz(0, 1.5)
	if err != nil || offset != 3000 {
		t.Fatalf("frequency %g/%v", offset, err)
	}
	ppm, err := data.ColumnAxis.PPM(2, 1.5)
	if err != nil || math.Abs(ppm-(-5.3)) > 1e-12 {
		t.Fatalf("ppm %g/%v", ppm, err)
	}
	timeOffset, err := data.RowAxis.TimeOffsetSeconds(1, 0.5)
	if err != nil || timeOffset != 0.000125 {
		t.Fatalf("time %g/%v", timeOffset, err)
	}
	if *data.ColumnAxis.TransmitterFrequencyMHz != 100 || *data.RowAxis.TransmitterFrequencyMHz != 200 || *data.ColumnAxis.ChemicalShiftReferencePPM != 4.7 || *data.RowAxis.ChemicalShiftReferencePPM != 1.2 {
		t.Fatal("VM sampling/evolution mapping wrong")
	}
	obj.Put(object.FDElement(tagSpectralWidth, binary.BigEndian, 9000))
	one, err := Read(obj, Options{})
	if err != nil {
		t.Fatal(err)
	}
	if *one.ColumnAxis.SpectralWidthHz != 9000 || one.RowAxis.SpectralWidthHz != nil {
		t.Fatal("VM1 was broadcast to rows")
	}
	if _, err := one.RowAxis.TimeOffsetSeconds(0, 0); !errors.Is(err, ErrAxisUnavailable) {
		t.Fatal("missing row calibration accepted")
	}
	// Returned calibration pointers are copies, and axes may be uncalibrated.
	*data.ColumnAxis.SpectralWidthHz = 1
	if *one.ColumnAxis.SpectralWidthHz != 9000 {
		t.Fatal("axis metadata aliased across reads")
	}
	obj.Remove(tagSpectralWidth)
	obj.Remove(tagTransmitter)
	obj.Remove(tagChemicalShift)
	uncalibrated, err := Read(obj, Options{})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := uncalibrated.ColumnAxis.PPM(0, 0); !errors.Is(err, ErrAxisUnavailable) {
		t.Fatal("missing calibration must be explicit")
	}
	width := math.MaxFloat64 / 2
	axis := Axis{Points: 1000, Domain: Frequency, SpectralWidthHz: &width}
	if offset, err := axis.FrequencyOffsetHz(0, 4); err != nil || !finite(offset) || offset <= 0 {
		t.Fatalf("avoidable axis overflow %g/%v", offset, err)
	}
	for _, reference := range []float64{math.NaN(), math.Inf(1)} {
		if _, err := axis.FrequencyOffsetHz(0, reference); !errors.Is(err, ErrAxisUnavailable) {
			t.Fatal("invalid reference accepted")
		}
	}
	if _, err := axis.FrequencyOffsetHz(-1, 0); !errors.Is(err, ErrIndex) {
		t.Fatal("axis index accepted")
	}
	if _, err := axis.TimeOffsetSeconds(0, 0); !errors.Is(err, ErrAxisUnavailable) {
		t.Fatal("domain mismatch accepted")
	}
}

func TestMalformedMetadataAndPayloadAdmission(t *testing.T) {
	tests := []struct {
		name   string
		mutate func(*object.Object)
		want   error
	}{
		{"missing frames", func(o *object.Object) { o.Remove(tagFrames) }, ErrInvalidMetadata},
		{"zero points", func(o *object.Object) { o.Put(object.ULElement(tagPointColumns, binary.LittleEndian, 0)) }, ErrInvalidMetadata},
		{"negative frames", func(o *object.Object) { o.Put(core.NewRawElement(tagFrames, core.VRIS, []byte("-1"))) }, ErrInvalidMetadata},
		{"dimension VM", func(o *object.Object) { o.Put(object.ULElement(tagPointRows, binary.LittleEndian, 1, 1)) }, ErrInvalidMetadata},
		{"dimension VR", func(o *object.Object) { o.Put(object.USElement(tagPointRows, binary.LittleEndian, 1)) }, ErrInvalidMetadata},
		{"dimension typed width", func(o *object.Object) {
			o.Put(core.Element{Header: core.ElementHeader{Tag: tagPointRows, VR: core.VRUL}, Value: core.Uint16Value{1}})
		}, ErrInvalidMetadata},
		{"dimension signed typed", func(o *object.Object) {
			o.Put(core.Element{Header: core.ElementHeader{Tag: tagVoxelRows, VR: core.VRUS}, Value: core.Int16Value{1}})
		}, ErrInvalidMetadata},
		{"dimension partial word", func(o *object.Object) { o.Put(core.NewRawElement(tagPointRows, core.VRUL, []byte{1, 0, 0})) }, ErrInvalidMetadata},
		{"string VM", func(o *object.Object) {
			o.Put(core.Element{Header: core.ElementHeader{Tag: tagFrames, VR: core.VRIS}, Value: core.StringValue{"1", "1"}})
		}, ErrInvalidMetadata},
		{"string oversize", func(o *object.Object) {
			o.Put(core.Element{Header: core.ElementHeader{Tag: tagFrames, VR: core.VRIS}, Value: core.StringValue{"1234567890123"}})
		}, ErrInvalidMetadata},
		{"representation", func(o *object.Object) { o.Put(core.NewRawElement(tagRepresentation, core.VRCS, []byte("PHASE"))) }, ErrInvalidMetadata},
		{"domain", func(o *object.Object) { o.Put(core.NewRawElement(tagDomainColumns, core.VRCS, []byte("UNKNOWN"))) }, ErrInvalidMetadata},
		{"domain VM", func(o *object.Object) { o.Put(core.NewRawElement(tagDomainColumns, core.VRCS, []byte("TIME\\TIME"))) }, ErrInvalidMetadata},
		{"missing rows domain", func(o *object.Object) { o.Put(object.ULElement(tagPointRows, binary.LittleEndian, 2)) }, ErrInvalidMetadata},
		{"FD typed width", func(o *object.Object) {
			o.Put(core.Element{Header: core.ElementHeader{Tag: tagSpectralWidth, VR: core.VRFD}, Value: core.Float32Value{1000}})
		}, ErrInvalidMetadata},
		{"FD wrong typed", func(o *object.Object) {
			o.Put(core.Element{Header: core.ElementHeader{Tag: tagSpectralWidth, VR: core.VRFD}, Value: core.Uint64Value{1000}})
		}, ErrInvalidMetadata},
		{"FD partial word", func(o *object.Object) {
			o.Put(core.NewRawElement(tagSpectralWidth, core.VRFD, []byte{1, 2, 3, 4, 5, 6, 7}))
		}, ErrInvalidMetadata},
		{"FD wrong VM", func(o *object.Object) { o.Put(object.FDElement(tagSpectralWidth, binary.LittleEndian, 1, 2)) }, ErrInvalidMetadata},
		{"FD nonfinite", func(o *object.Object) { o.Put(object.FDElement(tagChemicalShift, binary.LittleEndian, math.NaN())) }, ErrInvalidMetadata},
		{"FD negative", func(o *object.Object) { o.Put(object.FDElement(tagTransmitter, binary.LittleEndian, -1)) }, ErrInvalidMetadata},
		{"OF missing", func(o *object.Object) { o.Remove(tagData) }, ErrInvalidData},
		{"OF VR", func(o *object.Object) { o.Put(core.NewRawElement(tagData, core.VROB, make([]byte, 16))) }, ErrInvalidData},
		{"OF short", func(o *object.Object) { o.Put(core.NewRawElement(tagData, core.VROF, make([]byte, 12))) }, ErrDataLength},
		{"OF extra", func(o *object.Object) { o.Put(core.NewRawElement(tagData, core.VROF, make([]byte, 20))) }, ErrDataLength},
		{"OF undefined", func(o *object.Object) { e, _ := o.Get(tagData); e.Header.Length = core.UndefinedLength; o.Put(e) }, ErrDataLength},
		{"OF header stale", func(o *object.Object) { e, _ := o.Get(tagData); e.Value = core.RawValue(make([]byte, 20)); o.Put(e) }, ErrDataLength},
		{"OF typed mismatch", func(o *object.Object) {
			o.Put(core.Element{Header: core.ElementHeader{Tag: tagData, VR: core.VROF}, Value: core.Float64Value{1, 2}})
		}, ErrInvalidData},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			obj := smallObject()
			tt.mutate(obj)
			data, err := Read(obj, Options{})
			if data != nil || !errors.Is(err, tt.want) {
				t.Fatalf("data%v err%v want%v", data, err, tt.want)
			}
		})
	}
	if data, err := Read(nil, Options{}); data != nil || !errors.Is(err, ErrInvalidMetadata) {
		t.Fatal("nil object accepted")
	}
}

func TestLimitsOverflowAndCancellation(t *testing.T) {
	for _, opts := range []Options{{MaxBytes: 15}, {MaxValues: 3}, {MaxBytes: -1}, {MaxValues: -1}} {
		_, err := Read(smallObject(), opts)
		if !errors.Is(err, ErrLimitExceeded) && !errors.Is(err, ErrInvalidOptions) {
			t.Fatalf("limits %v/%v", opts, err)
		}
	}
	if _, err := Read(smallObject(), Options{MaxBytes: 16, MaxValues: 4}); err != nil {
		t.Fatal("exact limit", err)
	}
	obj := smallObject()
	obj.Put(object.ULElement(tagPointColumns, binary.LittleEndian, uint32(DefaultMaxValues)))
	if _, err := Read(obj, Options{}); !errors.Is(err, ErrLimitExceeded) {
		t.Fatal("default finite limit ignored", err)
	}
	obj = smallObject()
	obj.Put(core.NewRawElement(tagFrames, core.VRIS, []byte("2147483647")))
	obj.Put(object.USElement(tagVoxelRows, binary.LittleEndian, 65535))
	obj.Put(object.USElement(tagVoxelColumns, binary.LittleEndian, 65535))
	obj.Put(object.ULElement(tagPointRows, binary.LittleEndian, math.MaxUint32))
	obj.Put(object.ULElement(tagPointColumns, binary.LittleEndian, math.MaxUint32))
	obj.Put(core.NewRawElement(tagDomainRows, core.VRCS, []byte(Time)))
	if _, err := Read(obj, Options{MaxBytes: math.MaxInt64, MaxValues: math.MaxInt64}); !errors.Is(err, ErrLimitExceeded) && !errors.Is(err, ErrInvalidMetadata) {
		t.Fatal("overflow admitted", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if data, err := ReadContext(ctx, smallObject(), Options{}); data != nil || !errors.Is(err, context.Canceled) {
		t.Fatal("cancel", err)
	}
	ctx, cancel = context.WithDeadline(context.Background(), time.Unix(0, 0))
	defer cancel()
	if data, err := ReadContext(ctx, smallObject(), Options{}); data != nil || !errors.Is(err, context.DeadlineExceeded) {
		t.Fatal("deadline", err)
	}
	if _, err := ReadContext(nil, smallObject(), Options{}); err != nil {
		t.Fatal("nil context", err)
	}
}

func TestTypedCountsBeforeSerializedLength(t *testing.T) {
	for _, test := range []struct {
		count, width, maximum int
		want                  bool
	}{
		{1, 2, 2, true}, {2, 2, 2, false}, {1, 4, 4, true}, {2, 4, 4, false},
		{2, 8, 16, true}, {3, 8, 16, false}, {maxInt(), 8, 16, false},
		{maxInt(), 4, 4, false}, {0, 4, 4, false}, {-1, 4, 4, false}, {1, 0, 4, false},
	} {
		if got := countWithinBytes(test.count, test.width, test.maximum); got != test.want {
			t.Fatalf("count/width preflight %+v got%v", test, got)
		}
	}
	// Headers claim a scalar/small value; actual typed counts still govern.
	for _, element := range []core.Element{
		{Header: core.ElementHeader{Tag: tagVoxelRows, VR: core.VRUS, Length: 2, LengthSet: true}, Value: core.Uint16Value{1, 1}},
		{Header: core.ElementHeader{Tag: tagPointRows, VR: core.VRUL, Length: 4, LengthSet: true}, Value: core.Uint32Value{1, 1}},
		{Header: core.ElementHeader{Tag: tagSpectralWidth, VR: core.VRFD, Length: 8, LengthSet: true}, Value: core.Float64Value{1, 2, 3}},
	} {
		obj := smallObject()
		obj.Put(element)
		if data, err := Read(obj, Options{}); data != nil || !errors.Is(err, ErrInvalidMetadata) {
			t.Fatalf("typed metadata %s %v", element.Tag(), err)
		}
	}
	for _, values := range []core.Float32Value{{1, 2, 3}, {1, 2, 3, 4, 5}} {
		obj := smallObject()
		obj.Put(core.Element{Header: core.ElementHeader{Tag: tagData, VR: core.VROF, Length: 16, LengthSet: true}, Value: values})
		if data, err := Read(obj, Options{}); data != nil || !errors.Is(err, ErrDataLength) {
			t.Fatal("typed OF count", err)
		}
	}
}

type replaySource struct {
	reader                  *bytes.Reader
	active                  bool
	chunk, reads, failAfter int
	failure                 error
	cancel                  context.CancelFunc
}

func (r *replaySource) Seek(offset int64, whence int) (int64, error) {
	return r.reader.Seek(offset, whence)
}
func (r *replaySource) Read(data []byte) (int, error) {
	if r.active {
		if r.failure != nil && r.reads >= r.failAfter {
			return 0, r.failure
		}
		if r.chunk > 0 && len(data) > r.chunk {
			data = data[:r.chunk]
		}
	}
	n, err := r.reader.Read(data)
	if r.active {
		r.reads += n
		if r.cancel != nil {
			cancel := r.cancel
			r.cancel = nil
			cancel()
		}
	}
	return n, err
}

func deferredTestObject(t *testing.T, order binary.ByteOrder) (*object.Object, *replaySource, []byte) {
	t.Helper()
	syntax := transfer.ExplicitVRLittleEndian
	if order == binary.BigEndian {
		syntax = transfer.ExplicitVRBigEndian
	}
	obj := testObject(order, Dimensions{1, 1, 1, 1, 4}, Complex, []float32{1, -2, 3, -4, 5, -6, 7, -8})
	var wire bytes.Buffer
	if err := object.WriteDataSet(&wire, obj, syntax); err != nil {
		t.Fatal(err)
	}
	source := &replaySource{reader: bytes.NewReader(wire.Bytes())}
	lazy, err := object.ReadDataSetWithOptions(source, syntax, object.ReadFileOptions{InlineValueBytesThreshold: 1, DeferValueTags: []core.Tag{tagData}, MaxElementBytes: 1024, MaxTotalBytes: 4096})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = lazy.Close() })
	if element, _ := lazy.Get(tagData); element.Value != nil {
		t.Fatal("test must use deferred OF")
	}
	source.active = true
	return lazy, source, wire.Bytes()
}

func TestDeferredOFChunkingErrorsLimitsAndCancellation(t *testing.T) {
	for _, order := range []binary.ByteOrder{binary.LittleEndian, binary.BigEndian} {
		for _, chunk := range []int{1, 2, 3, 5, 7} {
			t.Run(order.String()+strconv.Itoa(chunk), func(t *testing.T) {
				obj, source, _ := deferredTestObject(t, order)
				source.chunk = chunk
				data, err := Read(obj, Options{})
				if err != nil || data.Values[0] != 1 || data.Values[7] != -8 || source.reads != 32 {
					t.Fatalf("deferred %v bytes%d", err, source.reads)
				}
			})
		}
	}
	t.Run("no replay past limit", func(t *testing.T) {
		obj, source, _ := deferredTestObject(t, binary.LittleEndian)
		data, err := Read(obj, Options{MaxBytes: 31})
		if data != nil || !errors.Is(err, ErrLimitExceeded) || source.reads != 0 {
			t.Fatalf("limits/replay %v bytes%d", err, source.reads)
		}
	})
	t.Run("copy failure discards prefix", func(t *testing.T) {
		obj, source, _ := deferredTestObject(t, binary.LittleEndian)
		sentinel := errors.New("source failure")
		source.chunk = 5
		source.failAfter = 5
		source.failure = sentinel
		data, err := Read(obj, Options{})
		if data != nil || !errors.Is(err, sentinel) {
			t.Fatalf("prefix %v/%v", data, err)
		}
	})
	t.Run("truncated source", func(t *testing.T) {
		obj, source, wire := deferredTestObject(t, binary.BigEndian)
		source.reader = bytes.NewReader(wire[:len(wire)-2])
		data, err := Read(obj, Options{})
		if data != nil || err == nil {
			t.Fatal("truncated source accepted")
		}
	})
	t.Run("cancel in replay", func(t *testing.T) {
		obj, source, _ := deferredTestObject(t, binary.BigEndian)
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		source.chunk = 3
		source.cancel = cancel
		data, err := ReadContext(ctx, obj, Options{})
		if data != nil || !errors.Is(err, context.Canceled) {
			t.Fatalf("cancel %v/%v", data, err)
		}
	})
	t.Run("closed source", func(t *testing.T) {
		obj, _, _ := deferredTestObject(t, binary.LittleEndian)
		_ = obj.Close()
		if data, err := Read(obj, Options{}); data != nil || err == nil {
			t.Fatal("closed source accepted")
		}
	})
}

func TestFloatSinkRejectsExcessAndIncompleteWords(t *testing.T) {
	sink := floatSink{ctx: context.Background(), order: binary.LittleEndian, values: make([]float32, 1)}
	if n, err := sink.Write(make([]byte, 5)); n != 0 || !errors.Is(err, ErrDataLength) {
		t.Fatal("excess admitted")
	}
	if n, err := sink.Write([]byte{0, 0, 128}); n != 3 || err != nil || sink.buffered != 3 || sink.index != 0 {
		t.Fatal("partial word state")
	}
	if n, err := sink.Write([]byte{63}); n != 1 || err != nil || sink.buffered != 0 || sink.index != 1 || sink.values[0] != 1 {
		t.Fatal("split word completion")
	}
	if n, err := sink.Write([]byte{0}); n != 0 || !errors.Is(err, ErrDataLength) {
		t.Fatal("extra point admitted")
	}
}

func TestAccessorBoundsAndMutatedLayout(t *testing.T) {
	data, err := Read(smallObject(), Options{})
	if err != nil {
		t.Fatal(err)
	}
	for _, idx := range [][5]int{{-1, 0, 0, 0, 0}, {1, 0, 0, 0, 0}, {0, 1, 0, 0, 0}, {0, 0, 1, 0, 0}, {0, 0, 0, 1, 0}, {0, 0, 0, 0, 2}, {0, 0, 0, 0, -1}} {
		if _, err := data.Point(idx[0], idx[1], idx[2], idx[3], idx[4]); !errors.Is(err, ErrIndex) {
			t.Fatal("index", idx, err)
		}
	}
	data.Dimensions.PointColumns = maxInt()
	if _, err := data.Spectrum(0, 0, 0); !errors.Is(err, ErrLimitExceeded) {
		t.Fatal("mutated overflow", err)
	}
	data.Dimensions.PointColumns = 2
	data.Values = nil
	if _, err := data.Spectrum(0, 0, 0); !errors.Is(err, ErrDataLength) {
		t.Fatal("mutated payload", err)
	}
	var nilData *Data
	if _, err := nilData.Spectrum(0, 0, 0); !errors.Is(err, ErrInvalidMetadata) {
		t.Fatal("nil data", err)
	}
}

var _ io.ReadSeeker = (*replaySource)(nil)
