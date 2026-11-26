package object

import (
	"bytes"
	"encoding/binary"
	"errors"
	"math"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/ThalesMMS/dicom-go/core"
)

func TestFormatDS(t *testing.T) {
	for _, tc := range []struct {
		value float64
		want  string
	}{
		{0, "0"},
		{math.Copysign(0, -1), "-0"},
		{1, "1"},
		{-2.5, "-2.5"},
		{0.1, "0.1"},
		{1.0 / 3, "0.33333333333333"},
		{-1.0 / 3, "-0.3333333333333"},
		{123456789012345678, "1.23456789012e17"},
		{1e20, "1e20"},
		{1e-7, "1e-7"},
		{1.2345678901234567e-4, "1.23456789012e-4"},
		{-1.7976931348623157e308, "-1.79769313e308"},
		{math.MaxFloat64, "1.79769313e308"},
		{5e-324, "5e-324"},
		{0.000123, "0.000123"},
		{12345.678, "12345.678"},
		{1234567890123456, "1234567890123456"},
		{12345678901234567, "1.23456789012e16"},
	} {
		got, err := FormatDS(tc.value)
		if err != nil || got != tc.want {
			t.Errorf("FormatDS(%v) = %q, %v; want %q", tc.value, got, err, tc.want)
		}
		if len(got) > 16 {
			t.Errorf("FormatDS(%v) = %q is longer than 16 bytes", tc.value, got)
		}
	}
	for _, value := range []float64{math.NaN(), math.Inf(1), math.Inf(-1)} {
		if _, err := FormatDS(value); !errors.Is(err, ErrInvalidElementValue) {
			t.Errorf("FormatDS(%v) error %v", value, err)
		}
	}
}

// TestFormatDSKeepsMostDigits checks that FormatDS never loses digits a
// 16-byte value could hold, and round-trips every value that fits.
func TestFormatDSKeepsMostDigits(t *testing.T) {
	values := []float64{math.Pi, -math.E * 1e-9, 6.02214076e23, -1.602176634e-19, 299792458, 1 << 53, 0.1 + 0.2}
	for i := 0; i < 2000; i++ {
		// A deterministic spread over signs, mantissas and exponents.
		mantissa := 1 + float64((i*7919)%10007)/10007
		exponent := (i*37)%600 - 300
		value := mantissa * math.Pow(10, float64(exponent))
		if i%2 == 1 {
			value = -value
		}
		values = append(values, value)
	}
	for _, value := range values {
		got, err := FormatDS(value)
		if err != nil || len(got) > 16 {
			t.Fatalf("FormatDS(%v) = %q, %v", value, got, err)
		}
		parsed, err := strconv.ParseFloat(got, 64)
		if err != nil {
			t.Fatalf("FormatDS(%v) = %q does not parse: %v", value, got, err)
		}
		if shortest := strconv.FormatFloat(value, 'g', -1, 64); len(shortest) <= 12 && parsed != value {
			t.Fatalf("FormatDS(%v) = %q loses a short value", value, got)
		}
		if relative := math.Abs(parsed-value) / math.Abs(value); relative > 5e-9 {
			t.Fatalf("FormatDS(%v) = %q: relative error %g", value, got, relative)
		}
	}
}

func TestFormatIS(t *testing.T) {
	for value, want := range map[int64]string{0: "0", -5: "-5", math.MaxInt32: "2147483647", math.MinInt32: "-2147483648"} {
		if got, err := FormatIS(value); err != nil || got != want {
			t.Errorf("FormatIS(%d) = %q, %v", value, got, err)
		}
	}
	for _, value := range []int64{math.MaxInt32 + 1, math.MinInt32 - 1} {
		if _, err := FormatIS(value); !errors.Is(err, ErrInvalidElementValue) {
			t.Errorf("FormatIS(%d) error %v", value, err)
		}
	}
}

// TestTypedSettersRoundTrip writes every typed setter in both byte orders
// and reads the values back through the getters.
func TestTypedSettersRoundTrip(t *testing.T) {
	var (
		tagDS   = core.NewTag(0x0028, 0x0030)
		tagIS   = core.NewTag(0x0020, 0x0013)
		tagUS   = core.NewTag(0x0028, 0x0010)
		tagSS   = core.NewTag(0x0028, 0x0106)
		tagUL   = core.NewTag(0x0042, 0x0015)
		tagSL   = core.NewTag(0x0018, 0x6020)
		tagFL   = core.NewTag(0x0018, 0x9087)
		tagFD   = core.NewTag(0x0018, 0x9089)
		tagDA   = core.NewTag(0x0008, 0x0020)
		tagTM   = core.NewTag(0x0008, 0x0030)
		tagDT   = core.NewTag(0x0008, 0x002A)
		tagPN   = core.NewTag(0x0010, 0x0010)
		tagLO   = core.NewTag(0x0008, 0x1030)
		tagSeq  = core.NewTag(0x0008, 0x1140)
		tagItem = core.NewTag(0x0008, 0x1155)
	)
	zone := time.FixedZone("", -3*3600)
	moment := time.Date(2026, 9, 26, 14, 5, 9, 123456000, zone)
	for _, order := range []binary.ByteOrder{binary.LittleEndian, binary.BigEndian} {
		obj := New(nil)
		obj.SetValueByteOrder(order)
		must := func(err error) {
			t.Helper()
			if err != nil {
				t.Fatal(err)
			}
		}
		must(obj.SetDS(tagDS, 0.5, 1.0/3))
		must(obj.SetIS(tagIS, 7, -2))
		obj.SetUS(tagUS, 512, 1)
		obj.SetSS(tagSS, -1024)
		obj.SetUL(tagUL, 1<<31)
		obj.SetSL(tagSL, -70000)
		obj.SetFL(tagFL, 1.5)
		obj.SetFD(tagFD, -2.25, 1e300)
		obj.SetDate(tagDA, moment)
		obj.SetTime(tagTM, moment)
		obj.SetDateTime(tagDT, moment)
		must(obj.SetPN(tagPN, core.PersonName{FamilyName: "Synthetic", GivenName: "Patient"}))
		must(obj.SetStrings(tagLO, core.VRLO, "Study"))
		item := New(nil)
		must(item.SetStrings(tagItem, core.VRUI, "2.25.1052"))
		obj.SetSequence(tagSeq, item, nil)
		obj.SetAT(core.NewTag(0x0028, 0x0009), tagFD)
		if raw, _ := obj.GetRaw(core.NewTag(0x0028, 0x0009)); len(raw) != 4 || order.Uint16(raw) != 0x0018 || order.Uint16(raw[2:]) != 0x9089 {
			t.Fatalf("AT not in %v: % x", order, raw)
		}

		if values, err := obj.GetFloats(tagDS); err != nil || len(values) != 2 || values[0] != 0.5 || math.Abs(values[1]-1.0/3) > 1e-14 {
			t.Fatalf("DS %v %v", values, err)
		}
		if values, err := obj.GetInts(tagIS); err != nil || len(values) != 2 || values[0] != 7 || values[1] != -2 {
			t.Fatalf("IS %v %v", values, err)
		}
		for _, tc := range []struct {
			tag  core.Tag
			want []int64
		}{{tagUS, []int64{512, 1}}, {tagSS, []int64{-1024}}, {tagUL, []int64{1 << 31}}, {tagSL, []int64{-70000}}} {
			if values, err := obj.GetInts(tc.tag); err != nil || len(values) != len(tc.want) || values[0] != tc.want[0] {
				t.Fatalf("%s: %v %v, want %v", tc.tag, values, err, tc.want)
			}
		}
		if raw, _ := obj.GetRaw(tagUS); order.Uint16(raw) != 512 {
			t.Fatalf("US not in %v: % x", order, raw)
		}
		if values, err := obj.GetFloats(tagFL); err != nil || len(values) != 1 || values[0] != 1.5 {
			t.Fatalf("FL %v %v", values, err)
		}
		if values, err := obj.GetFloats(tagFD); err != nil || len(values) != 2 || values[0] != -2.25 || values[1] != 1e300 {
			t.Fatalf("FD %v %v", values, err)
		}
		for tag, want := range map[core.Tag]string{
			tagDA: "20260926",
			tagTM: "140509.123456",
			tagDT: "20260926140509.123456-0300",
			tagPN: "Synthetic^Patient",
		} {
			if got, _ := obj.GetString(tag); got != want {
				t.Fatalf("%s = %q, want %q", tag, got, want)
			}
		}
		if got, err := obj.GetTime(tagDT); err != nil || !got.Equal(moment) {
			t.Fatalf("DT reads back as %v %v", got, err)
		}
		items, ok := obj.GetSequence(tagSeq)
		if !ok || len(items) != 2 || len(items[1].Elements()) != 0 {
			t.Fatalf("sequence %v", items)
		}
		if uid, _ := items[0].GetUID(tagItem); uid != "2.25.1052" {
			t.Fatalf("item UID %q", uid)
		}
	}
	whole := New(nil)
	whole.SetTime(core.NewTag(0x0008, 0x0030), time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC))
	if got, _ := whole.GetString(core.NewTag(0x0008, 0x0030)); got != "030405" {
		t.Fatalf("whole-second TM %q", got)
	}
}

func TestTypedSettersRejectInvalidValues(t *testing.T) {
	obj := New(nil)
	for name, err := range map[string]error{
		"DS NaN":       obj.SetDS(core.NewTag(0x0028, 0x0030), math.NaN()),
		"IS range":     obj.SetIS(core.NewTag(0x0020, 0x0013), 1<<40),
		"CS lowercase": obj.SetStrings(core.NewTag(0x0008, 0x0060), core.VRCS, "ct"),
		"UI letters":   obj.SetStrings(core.NewTag(0x0008, 0x0018), core.VRUI, "1.2.abc"),
		"SH too long":  obj.SetStrings(core.NewTag(0x0008, 0x0050), core.VRSH, "ABCDEFGHIJKLMNOPQ"),
		"not a string": obj.SetStrings(core.NewTag(0x0028, 0x0010), core.VRUS, "1"),
		"LO backslash": obj.SetStrings(core.NewTag(0x0008, 0x1030), core.VRLT, "a\x00b"),
		"PN too long":  obj.SetPN(core.NewTag(0x0010, 0x0010), core.PersonName{FamilyName: strings.Repeat("A", 65)}),
	} {
		if !errors.Is(err, ErrInvalidElementValue) {
			t.Errorf("%s: %v", name, err)
		}
	}
	if len(obj.Elements()) != 0 {
		t.Fatalf("rejected values were stored: %v", obj.Elements())
	}
	// Non-ASCII text is valid once the object declares a character set.
	latin := New(nil)
	if err := latin.SetStrings(core.NewTag(0x0008, 0x0005), core.VRCS, "ISO_IR 100"); err != nil {
		t.Fatal(err)
	}
	if err := latin.SetPN(core.NewTag(0x0010, 0x0010), core.PersonName{FamilyName: "Müller"}); err != nil {
		t.Fatalf("Latin-1 name: %v", err)
	}
	if raw := USElement(core.NewTag(0x0028, 0x0010), nil, 0x0102).Value; !bytes.Equal(mustRaw(t, raw), []byte{0x02, 0x01}) {
		t.Fatalf("nil order is not Little Endian")
	}
}

func mustRaw(t *testing.T, value core.Value) []byte {
	t.Helper()
	element := core.Element{Value: value}
	raw, ok := element.RawBytes()
	if !ok {
		t.Fatalf("value %T has no raw bytes", value)
	}
	return raw
}
