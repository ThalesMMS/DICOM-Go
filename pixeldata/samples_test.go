package pixeldata_test

import (
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"math"
	"testing"

	"github.com/ThalesMMS/dicom-go/core"
	"github.com/ThalesMMS/dicom-go/object"
	"github.com/ThalesMMS/dicom-go/pixeldata"
)

// signed13 is BitsAllocated 16, BitsStored 13, HighBit 12, signed: raw
// 0x1830 is -2000.
var signed13 = pixeldata.SampleFormat{BitsAllocated: 16, BitsStored: 13, HighBit: 12, Signed: true}

func le16(values ...uint16) []byte {
	out := make([]byte, 0, 2*len(values))
	for _, v := range values {
		out = binary.LittleEndian.AppendUint16(out, v)
	}
	return out
}

func TestStoredSamplesInterpretSignednessAndBitField(t *testing.T) {
	ctx := context.Background()
	// Extremes, the issue's -2000 and junk above HighBit (0xF830, 0x2FFF).
	src := le16(0x1000, 0x1830, 0xF830, 0x0FFF, 0x2FFF, 0x0000, 0x1FFF)
	want := []int64{-4096, -2000, -2000, 4095, 4095, 0, -1}
	got, err := pixeldata.StoredSamples[int16](ctx, src, signed13, 0)
	if err != nil {
		t.Fatal(err)
	}
	for i := range want {
		if int64(got[i]) != want[i] {
			t.Fatalf("sample %d = %d, want %d", i, got[i], want[i])
		}
	}
	wide, err := pixeldata.StoredSamples[int64](ctx, src, signed13, 0)
	if err != nil || len(wide) != len(want) || wide[1] != -2000 {
		t.Fatalf("int64: %v %v", wide, err)
	}

	for _, tc := range []struct {
		name   string
		format pixeldata.SampleFormat
		src    []byte
		want   []int64
	}{
		{"12-in-16 signed", pixeldata.SampleFormat{BitsAllocated: 16, BitsStored: 12, HighBit: 11, Signed: true}, le16(0x0800, 0x07FF), []int64{-2048, 2047}},
		{"16 unsigned big-endian", pixeldata.SampleFormat{BitsAllocated: 16, BitsStored: 16, HighBit: 15, ByteOrder: binary.BigEndian}, []byte{0xFF, 0xFE, 0x00, 0x01}, []int64{65534, 1}},
		{"8 signed", pixeldata.SampleFormat{BitsAllocated: 8, BitsStored: 8, HighBit: 7, Signed: true}, []byte{0x80, 0x7F, 0xFF}, []int64{-128, 127, -1}},
		{"32 signed", pixeldata.SampleFormat{BitsAllocated: 32, BitsStored: 32, HighBit: 31, Signed: true}, []byte{0, 0, 0, 0x80, 0xFF, 0xFF, 0xFF, 0x7F}, []int64{math.MinInt32, math.MaxInt32}},
		{"32 unsigned", pixeldata.SampleFormat{BitsAllocated: 32, BitsStored: 32, HighBit: 31}, []byte{0xFF, 0xFF, 0xFF, 0xFF}, []int64{math.MaxUint32}},
	} {
		got, err := pixeldata.StoredSamples[int64](ctx, tc.src, tc.format, 0)
		if err != nil {
			t.Fatalf("%s: %v", tc.name, err)
		}
		for i := range tc.want {
			if got[i] != tc.want[i] {
				t.Fatalf("%s: sample %d = %d, want %d", tc.name, i, got[i], tc.want[i])
			}
		}
	}
}

func TestStoredSamplesRefuseLossyDestinationTypes(t *testing.T) {
	ctx := context.Background()
	u16 := pixeldata.SampleFormat{BitsAllocated: 16, BitsStored: 16, HighBit: 15}
	u12 := pixeldata.SampleFormat{BitsAllocated: 16, BitsStored: 12, HighBit: 11}
	s8 := pixeldata.SampleFormat{BitsAllocated: 8, BitsStored: 8, HighBit: 7, Signed: true}
	u32 := pixeldata.SampleFormat{BitsAllocated: 32, BitsStored: 32, HighBit: 31}
	two := le16(1, 2)
	narrow := func(err error) bool { return errors.Is(err, pixeldata.ErrSampleTypeTooNarrow) }
	checks := []struct {
		name string
		err  error
		ok   bool
	}{
		{"signed into uint16", func() error { _, err := pixeldata.StoredSamples[uint16](ctx, two, signed13, 0); return err }(), false},
		{"signed into uint64", func() error { _, err := pixeldata.StoredSamples[uint64](ctx, two, signed13, 0); return err }(), false},
		{"signed 13 into int8", func() error { _, err := pixeldata.StoredSamples[int8](ctx, two, signed13, 0); return err }(), false},
		{"unsigned 16 into int16", func() error { _, err := pixeldata.StoredSamples[int16](ctx, two, u16, 0); return err }(), false},
		{"unsigned 16 into uint8", func() error { _, err := pixeldata.StoredSamples[uint8](ctx, two, u16, 0); return err }(), false},
		{"unsigned 32 into int32", func() error { _, err := pixeldata.StoredSamples[int32](ctx, two[:4], u32, 0); return err }(), false},
		{"unsigned 12 into int16", func() error { _, err := pixeldata.StoredSamples[int16](ctx, two, u12, 0); return err }(), true},
		{"unsigned 16 into uint16", func() error { _, err := pixeldata.StoredSamples[uint16](ctx, two, u16, 0); return err }(), true},
		{"unsigned 32 into uint32", func() error { _, err := pixeldata.StoredSamples[uint32](ctx, two[:4], u32, 0); return err }(), true},
		{"unsigned 32 into int64", func() error { _, err := pixeldata.StoredSamples[int64](ctx, two[:4], u32, 0); return err }(), true},
		{"signed 8 into int8", func() error { _, err := pixeldata.StoredSamples[int8](ctx, two[:2], s8, 0); return err }(), true},
	}
	for _, c := range checks {
		if c.ok && c.err != nil || !c.ok && !narrow(c.err) {
			t.Errorf("%s: %v", c.name, c.err)
		}
	}
}

func TestStoredSamplesFailTyped(t *testing.T) {
	ctx := context.Background()
	var formatErr *pixeldata.SampleFormatError
	for _, tc := range []struct {
		format pixeldata.SampleFormat
		field  string
	}{
		{pixeldata.SampleFormat{BitsAllocated: 1, BitsStored: 1, HighBit: 0}, "BitsAllocated"},
		{pixeldata.SampleFormat{BitsAllocated: 64, BitsStored: 64, HighBit: 63}, "BitsAllocated"},
		{pixeldata.SampleFormat{BitsAllocated: 16, BitsStored: 17, HighBit: 16}, "BitsStored"},
		{pixeldata.SampleFormat{BitsAllocated: 16, BitsStored: 12, HighBit: 10}, "HighBit"},
	} {
		_, err := pixeldata.StoredSamples[int64](ctx, le16(1), tc.format, 0)
		if !errors.Is(err, pixeldata.ErrInvalidSampleFormat) || !errors.As(err, &formatErr) || formatErr.Field != tc.field {
			t.Errorf("%+v: %v", tc.format, err)
		}
		if tc.format.Validate() == nil {
			t.Errorf("%+v validates", tc.format)
		}
	}
	if _, err := pixeldata.StoredSamples[int16](ctx, []byte{1, 2, 3}, signed13, 0); !errors.Is(err, pixeldata.ErrSampleDataLength) {
		t.Errorf("odd length: %v", err)
	}
	if _, err := pixeldata.ReadStoredSamples(ctx, make([]int16, 1), le16(1, 2), signed13); !errors.Is(err, pixeldata.ErrSampleBufferTooShort) {
		t.Errorf("short destination: %v", err)
	}
	if _, err := pixeldata.StoredSamples[int16](ctx, le16(1, 2, 3), signed13, 2); !errors.Is(err, pixeldata.ErrSampleBudgetExceeded) {
		t.Errorf("budget: %v", err)
	}
}

func TestStoredSamplesOwnershipAndCancellation(t *testing.T) {
	src := le16(0x1830, 0x0001)
	before := bytes.Clone(src)
	got, err := pixeldata.StoredSamples[int32](context.Background(), src, signed13, 0)
	if err != nil {
		t.Fatal(err)
	}
	got[0] = 7
	if !bytes.Equal(src, before) {
		t.Fatal("source modified")
	}
	// ReadStoredSamples writes into the caller's buffer without allocating.
	dst := make([]int32, 2)
	if allocs := testing.AllocsPerRun(10, func() {
		if _, err := pixeldata.ReadStoredSamples(context.Background(), dst, src, signed13); err != nil {
			t.Fatal(err)
		}
	}); allocs != 0 {
		t.Fatalf("ReadStoredSamples allocates %.0f times", allocs)
	}
	canceled, cancel := context.WithCancel(context.Background())
	cancel()
	if out, err := pixeldata.StoredSamples[int32](canceled, src, signed13, 0); !errors.Is(err, context.Canceled) || out != nil {
		t.Fatalf("canceled: %v %v", out, err)
	}
	if _, err := pixeldata.ComputeStoredSampleStatistics(canceled, src, signed13, nil); !errors.Is(err, context.Canceled) {
		t.Fatalf("canceled statistics: %v", err)
	}
	// Cancellation between blocks of a large frame.
	large := make([]byte, 2*(1<<17))
	ctx := &cancelAfter{Context: context.Background(), after: 1}
	if _, err := pixeldata.ReadStoredSamples(ctx, make([]int16, 1<<17), large, signed13); !errors.Is(err, context.Canceled) {
		t.Fatalf("mid-frame cancellation: %v", err)
	}
}

type cancelAfter struct {
	context.Context
	after, calls int
}

func (c *cancelAfter) Err() error {
	c.calls++
	if c.calls > c.after {
		return context.Canceled
	}
	return nil
}

func TestStoredSampleStatisticsExcludePadding(t *testing.T) {
	ctx := context.Background()
	src := le16(0x1000, 0x1830, 0x0FFF, 0x1FFF, 0x1000)
	stats, err := pixeldata.ComputeStoredSampleStatistics(ctx, src, signed13, nil)
	if err != nil {
		t.Fatal(err)
	}
	if stats.Samples != 5 || stats.Padding != 0 || stats.Minimum != -4096 || stats.Maximum != 4095 || stats.Mean != float64(-4096-2000+4095-1-4096)/5 {
		t.Fatalf("no padding: %+v", stats)
	}
	stats, err = pixeldata.ComputeStoredSampleStatistics(ctx, src, signed13, &pixeldata.PixelPadding{Value: -4096})
	if err != nil || stats.Samples != 3 || stats.Padding != 2 || stats.Minimum != -2000 || stats.Maximum != 4095 {
		t.Fatalf("padding value: %+v %v", stats, err)
	}
	// A range in either order; everything padding leaves zeroed extremes.
	stats, err = pixeldata.ComputeStoredSampleStatistics(ctx, src, signed13, &pixeldata.PixelPadding{Value: 4095, RangeLimit: -4096, HasRange: true})
	if err != nil || stats.Samples != 0 || stats.Padding != 5 || stats.Minimum != 0 || stats.Maximum != 0 || stats.Mean != 0 {
		t.Fatalf("all padding: %+v %v", stats, err)
	}
	// The mean is exact for values whose sum exceeds 2^53.
	u32 := pixeldata.SampleFormat{BitsAllocated: 32, BitsStored: 32, HighBit: 31}
	big := bytes.Repeat([]byte{0xFF, 0xFF, 0xFF, 0xFF}, 1<<22)
	stats, err = pixeldata.ComputeStoredSampleStatistics(ctx, big, u32, nil)
	if err != nil || stats.Mean != math.MaxUint32 || stats.Maximum != math.MaxUint32 {
		t.Fatalf("large mean: %+v %v", stats, err)
	}
	s32 := pixeldata.SampleFormat{BitsAllocated: 32, BitsStored: 32, HighBit: 31, Signed: true}
	negative := bytes.Repeat([]byte{0, 0, 0, 0x80}, 1<<22)
	stats, err = pixeldata.ComputeStoredSampleStatistics(ctx, negative, s32, nil)
	if err != nil || stats.Mean != math.MinInt32 || stats.Minimum != math.MinInt32 {
		t.Fatalf("large negative mean: %+v %v", stats, err)
	}
}

func paddingObject(elements ...core.Element) *object.Object {
	return object.FromElements(elements, nil)
}

func TestPixelPaddingFromObjectFollowsPixelRepresentation(t *testing.T) {
	value := core.NewTag(0x0028, 0x0120)
	limit := core.NewTag(0x0028, 0x0121)
	ss := func(tag core.Tag, v int16) core.Element {
		return core.Element{Header: core.ElementHeader{Tag: tag, VR: core.VRSS}, Value: core.Int16Value{v}}
	}
	us := func(tag core.Tag, v uint16) core.Element {
		return core.Element{Header: core.ElementHeader{Tag: tag, VR: core.VRUS}, Value: core.Uint16Value{v}}
	}
	u12 := pixeldata.SampleFormat{BitsAllocated: 16, BitsStored: 12, HighBit: 11}

	padding, err := pixeldata.PixelPaddingFromObject(paddingObject(ss(value, -2000), ss(limit, -4096)), signed13)
	if err != nil || padding == nil || padding.Value != -2000 || !padding.HasRange || padding.RangeLimit != -4096 || !padding.Contains(-3000) || padding.Contains(-1999) {
		t.Fatalf("signed range: %+v %v", padding, err)
	}
	padding, err = pixeldata.PixelPaddingFromObject(paddingObject(us(value, 0)), u12)
	if err != nil || padding == nil || padding.Value != 0 || padding.HasRange {
		t.Fatalf("unsigned value: %+v %v", padding, err)
	}
	// Raw little-endian SS, as read from a file.
	raw := core.NewRawElement(value, core.VRSS, le16(0xF830))
	padding, err = pixeldata.PixelPaddingFromObject(paddingObject(raw), signed13)
	if err != nil || padding.Value != -2000 {
		t.Fatalf("raw SS: %+v %v", padding, err)
	}
	if padding, err := pixeldata.PixelPaddingFromObject(paddingObject(), signed13); padding != nil || err != nil {
		t.Fatalf("absent: %+v %v", padding, err)
	}
	for name, tc := range map[string]struct {
		obj    *object.Object
		format pixeldata.SampleFormat
	}{
		"US for signed":        {paddingObject(us(value, 1)), signed13},
		"SS for unsigned":      {paddingObject(ss(value, 1)), u12},
		"outside 12-bit range": {paddingObject(us(value, 5000)), u12},
		"outside 13-bit range": {paddingObject(ss(value, -5000)), signed13},
		"two values":           {paddingObject(core.Element{Header: core.ElementHeader{Tag: value, VR: core.VRUS}, Value: core.Uint16Value{1, 2}}), u12},
		"wrong VR":             {paddingObject(core.Element{Header: core.ElementHeader{Tag: value, VR: core.VRUL}, Value: core.Uint32Value{1}}), u12},
		"limit mismatched":     {paddingObject(ss(value, 1), us(limit, 2)), signed13},
	} {
		if _, err := pixeldata.PixelPaddingFromObject(tc.obj, tc.format); !errors.Is(err, pixeldata.ErrInvalidPixelPadding) {
			t.Errorf("%s: %v", name, err)
		}
	}
}

func TestStoredSampleReaderRandomAccess(t *testing.T) {
	reader, err := signed13.Reader()
	if err != nil {
		t.Fatal(err)
	}
	src := le16(0x0001, 0x1830)
	if reader.Width() != 2 || reader.Value(src[2:]) != -2000 || reader.Value(src) != 1 {
		t.Fatalf("width %d values %d %d", reader.Width(), reader.Value(src), reader.Value(src[2:]))
	}
	if _, err := (pixeldata.SampleFormat{BitsAllocated: 16, BitsStored: 20, HighBit: 19}).Reader(); !errors.Is(err, pixeldata.ErrInvalidSampleFormat) {
		t.Fatalf("invalid format: %v", err)
	}
}
