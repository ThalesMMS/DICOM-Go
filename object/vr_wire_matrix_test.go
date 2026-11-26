package object_test

import (
	"bytes"
	"compress/flate"
	"encoding/binary"
	"io"
	"math"
	"slices"
	"testing"

	"github.com/ThalesMMS/dicom-go/core"
	"github.com/ThalesMMS/dicom-go/dictionary"
	"github.com/ThalesMMS/dicom-go/dictionary/std"
	"github.com/ThalesMMS/dicom-go/object"
	"github.com/ThalesMMS/dicom-go/transfer"
)

type vrWireSample struct {
	vr         core.VR
	value      core.Value
	wireValue  []byte
	longHeader bool
}

type matrixByteOrder interface {
	binary.ByteOrder
	binary.AppendByteOrder
}

// DicomVRRoundTripMatrixTests exercises populated and empty forms of every
// standard VR. Here each form also has an independent header and payload oracle.
func TestDatasetWireMatrixAllVRs(t *testing.T) {
	const (
		privateGroup = 0x0099
		creator      = "SYNTHETIC MATRIX"
	)
	characterSetTag := core.NewTag(0x0008, 0x0005)
	creatorTag := core.NewTag(privateGroup, 0x0010)
	codeMeaningTag := core.NewTag(0x0008, 0x0104)
	for _, syntax := range []struct {
		name              string
		value             transfer.Syntax
		order             matrixByteOrder
		implicit, deflate bool
	}{
		{"implicit-le", transfer.ImplicitVRLittleEndian, binary.LittleEndian, true, false},
		{"explicit-le", transfer.ExplicitVRLittleEndian, binary.LittleEndian, false, false},
		{"explicit-be", transfer.ExplicitVRBigEndian, binary.BigEndian, false, false},
		{"deflated-le", transfer.DeflatedExplicitVRLittleEndian, binary.LittleEndian, false, true},
	} {
		t.Run(syntax.name, func(t *testing.T) {
			item := vrMatrixWireField(syntax.order, syntax.implicit, false, codeMeaningTag, core.VRLO, []byte("étude"))
			sequence := core.SequenceValue{Items: []core.DataSet{{Elements: []core.Element{
				{Header: core.ElementHeader{Tag: codeMeaningTag, VR: core.VRLO}, Value: core.StringValue{"étude"}},
			}}}}
			fields := vrMatrixSamples(syntax.order, sequence, vrMatrixWireField(syntax.order, true, false, core.TagItem, "", item))
			definitions := make([]dictionary.PrivateEntry, 0, len(fields)-1)
			for i, field := range fields {
				if field.vr != core.VRUN {
					definitions = append(definitions, dictionary.PrivateEntry{Creator: creator, Group: privateGroup, Offset: uint8(i), VR: field.vr})
				}
			}
			privateDictionary, err := dictionary.NewPrivateCatalog(definitions)
			if err != nil {
				t.Fatal(err)
			}
			dict := dictionary.Chain{privateDictionary, std.Dictionary}
			for _, populated := range []bool{true, false} {
				form := "empty"
				if populated {
					form = "populated"
				}
				t.Run(form, func(t *testing.T) {
					elements := []core.Element{
						core.NewRawElement(characterSetTag, core.VRCS, []byte("ISO_IR 192")),
						core.NewRawElement(creatorTag, core.VRLO, []byte(creator)),
					}
					for i, field := range fields {
						value := field.value
						if !populated {
							value = core.RawValue{}
							if field.vr == core.VRSQ {
								value = core.SequenceValue{}
							}
						}
						tag := core.NewTag(privateGroup, 0x1000+uint16(i))
						elements = append(elements, core.Element{Header: core.ElementHeader{Tag: tag, VR: field.vr}, Value: value})
					}
					var encoded bytes.Buffer
					if err := object.WriteDataSetWithOptions(&encoded, object.FromElements(elements, dict), syntax.value, object.WriteDataSetOptions{
						SequenceLength: object.SequenceLengthDefined,
					}); err != nil {
						t.Fatal(err)
					}
					actual := encoded.Bytes()
					if syntax.deflate {
						inflater := flate.NewReader(bytes.NewReader(actual))
						defer inflater.Close()
						actual, err = io.ReadAll(inflater)
						if err != nil {
							t.Fatal(err)
						}
					}
					decoded, err := object.ReadDataSetWithOptions(bytes.NewReader(encoded.Bytes()), syntax.value, object.ReadFileOptions{Dictionary: dict})
					if err != nil {
						t.Fatal(err)
					}
					if count := len(decoded.ToDataSet().Elements); count != len(elements) {
						t.Fatalf("decoded %d elements, want %d", count, len(elements))
					}
					for _, control := range []struct {
						tag  core.Tag
						vr   core.VR
						wire string
					}{{characterSetTag, core.VRCS, "ISO_IR 192"}, {creatorTag, core.VRLO, creator}} {
						want := vrMatrixWireField(syntax.order, syntax.implicit, false, control.tag, control.vr, []byte(control.wire))
						if !bytes.HasPrefix(actual, want) {
							t.Fatalf("wire does not start with control element %s: want %x", control.tag, want)
						}
						actual = actual[len(want):]
					}
					for i, field := range fields {
						tag := core.NewTag(privateGroup, 0x1000+uint16(i))
						payload := field.wireValue
						if !populated {
							payload = nil
						}
						want := vrMatrixWireField(syntax.order, syntax.implicit, field.longHeader, tag, field.vr, payload)
						matches := bytes.HasPrefix(actual, want)
						t.Run(field.vr.String(), func(t *testing.T) {
							if !matches {
								t.Fatalf("%s wire = %x, want %x", tag, actual[:min(len(actual), len(want))], want)
							}
							assertVRMatrixValue(t, decoded, tag, field, payload, populated, codeMeaningTag)
						})
						if !matches {
							// A wire mismatch makes the offsets of later fields untrustworthy.
							return
						}
						actual = actual[len(want):]
					}
					if len(actual) != 0 {
						t.Fatalf("%d unaccounted logical dataset bytes", len(actual))
					}
				})
			}
		})
	}
}

func assertVRMatrixValue(t *testing.T, obj *object.Object, tag core.Tag, field vrWireSample, payload []byte, populated bool, codeMeaningTag core.Tag) {
	t.Helper()
	got, ok := obj.Get(tag)
	if !ok || got.VR() != field.vr || got.Header.Length != core.Length(len(payload)) {
		t.Fatalf("parsed header = %+v (present %v), want %s %s VL %d", got.Header, ok, tag, field.vr, len(payload))
	}
	if field.vr == core.VRSQ {
		items, ok := obj.GetSequence(tag)
		wantItems := 0
		if populated {
			wantItems = 1
		}
		if !ok || len(items) != wantItems {
			t.Fatalf("parsed SQ = %d items, present %v, populated %v", len(items), ok, populated)
		}
		if populated {
			if text, err := items[0].LookupString(codeMeaningTag); err != nil || text != "étude" {
				t.Fatalf("sequence item content = %q, %v", text, err)
			}
		}
		return
	}
	if raw, ok := got.RawBytes(); !ok || !bytes.Equal(raw, payload) {
		t.Fatalf("parsed raw = %x (present %v), want %x", raw, ok, payload)
	}
	if values, textual := field.value.(core.StringValue); textual {
		if !populated {
			values = nil
		}
		if text, err := obj.LookupStrings(tag); err != nil || !slices.Equal(text, values) {
			t.Fatalf("parsed text = %q, %v, want %q", text, err, values)
		}
	}
}

// Item headers use the same 32-bit length shape as Implicit VR headers, but
// always keep the byte order of the enclosing sequence's transfer syntax.
func vrMatrixWireField(order matrixByteOrder, implicit, long bool, tag core.Tag, vr core.VR, payload []byte) []byte {
	header := order.AppendUint16(nil, tag.Group)
	header = order.AppendUint16(header, tag.Element)
	switch {
	case implicit:
		header = order.AppendUint32(header, uint32(len(payload)))
	case long:
		header = append(header, string(vr)...)
		header = append(header, 0, 0)
		header = order.AppendUint32(header, uint32(len(payload)))
	default:
		header = append(header, string(vr)...)
		header = order.AppendUint16(header, uint16(len(payload)))
	}
	return append(header, payload...)
}

func vrMatrixSamples(order matrixByteOrder, sequence core.SequenceValue, item []byte) []vrWireSample {
	return []vrWireSample{
		{core.VRAE, core.StringValue{"SOURCE", "SINK"}, []byte("SOURCE\\SINK "), false},
		{core.VRAS, core.StringValue{"031Y"}, []byte("031Y"), false},
		{core.VRAT, core.TagValue{core.NewTag(0x1234, 0x5678)}, order.AppendUint16(order.AppendUint16(nil, 0x1234), 0x5678), false},
		{core.VRCS, core.StringValue{"PRIMARY", "DERIVED"}, []byte("PRIMARY\\DERIVED "), false},
		{core.VRDA, core.StringValue{"20240229"}, []byte("20240229"), false},
		{core.VRDS, core.StringValue{"-.125", "6.02E23"}, []byte("-.125\\6.02E23 "), false},
		{core.VRDT, core.StringValue{"20240229123456.125+0530"}, []byte("20240229123456.125+0530 "), false},
		{core.VRFD, core.Float64Value{-0.125}, order.AppendUint64(nil, 0xbfc0000000000000), false},
		{core.VRFL, core.Float32Value{-0.125}, order.AppendUint32(nil, 0xbe000000), false},
		{core.VRIS, core.StringValue{"-2147483648", "+7"}, []byte("-2147483648\\+7"), false},
		{core.VRLO, core.StringValue{"été", "", "画像"}, []byte("été\\\\画像 "), false},
		{core.VRLT, core.StringValue{"  étude\\a\n終"}, []byte("  étude\\a\n終"), false},
		{core.VROB, core.RawValue{0x91, 0x24, 0xf3}, []byte{0x91, 0x24, 0xf3, 0}, true},
		{core.VROD, core.Float64Value{1.625}, order.AppendUint64(nil, 0x3ffa000000000000), true},
		{core.VROF, core.Float32Value{1.625}, order.AppendUint32(nil, 0x3fd00000), true},
		{core.VROL, core.Uint32Value{0x01020a0b}, order.AppendUint32(nil, 0x01020a0b), true},
		{core.VROV, core.Uint64Value{0x0102030405060708}, order.AppendUint64(nil, 0x0102030405060708), true},
		{core.VROW, core.Uint16Value{0x1234, 0xfedc}, order.AppendUint16(order.AppendUint16(nil, 0x1234), 0xfedc), true},
		{core.VRPN, core.StringValue{"Léa^Qa=山本^葵", "Case^Two"}, []byte("Léa^Qa=山本^葵\\Case^Two "), false},
		{core.VRSH, core.StringValue{"hélice"}, []byte("hélice "), false},
		{core.VRSL, core.Int32Value{-83517}, order.AppendUint32(nil, 0xfffeb9c3), false},
		{core.VRSQ, sequence, item, true},
		{core.VRSS, core.Int16Value{-257}, order.AppendUint16(nil, 0xfeff), false},
		{core.VRSV, core.Int64Value{math.MinInt64 + 1}, order.AppendUint64(nil, 0x8000000000000001), true},
		{core.VRST, core.StringValue{" rayon\\b"}, []byte(" rayon\\b"), false},
		{core.VRTM, core.StringValue{"235959.25"}, []byte("235959.25 "), false},
		{core.VRUC, core.StringValue{"画像", "", "été"}, []byte("画像\\\\été "), true},
		{core.VRUI, core.StringValue{"2.25.7", "2.25.8"}, []byte("2.25.7\\2.25.8\x00"), false},
		{core.VRUL, core.Uint32Value{0xfe123456}, order.AppendUint32(nil, 0xfe123456), false},
		{core.VRUN, core.RawValue{0xfb, 0x01, 0x80, 0x12}, []byte{0xfb, 0x01, 0x80, 0x12}, true},
		{core.VRUR, core.StringValue{"https://example.invalid/a%20b"}, []byte("https://example.invalid/a%20b "), true},
		{core.VRUS, core.Uint16Value{0xf012}, order.AppendUint16(nil, 0xf012), false},
		{core.VRUT, core.StringValue{"été\\画像"}, []byte("été\\画像"), true},
		{core.VRUV, core.Uint64Value{0x8020040803020100}, order.AppendUint64(nil, 0x8020040803020100), true},
	}
}
