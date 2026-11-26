package dicomjson

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"testing"

	"github.com/ThalesMMS/dicom-go/core"
	"github.com/ThalesMMS/dicom-go/dictionary/std"
	"github.com/ThalesMMS/dicom-go/internal/dicomtest"
	"github.com/ThalesMMS/dicom-go/internal/valueencode"
	"github.com/ThalesMMS/dicom-go/object"
	"github.com/ThalesMMS/dicom-go/transfer"
)

// bigEndianInlineValues are small OW, OF, OD, OL and OV values (a palette
// among them) in their Little Endian form, keyed by tag.
var bigEndianInlineValues = []struct {
	tag core.Tag
	vr  core.VR
}{
	{core.NewTag(0x0028, 0x1201), core.VROW}, // Red Palette Color Lookup Table Data
	{core.NewTag(0x0066, 0x0016), core.VROF}, // Point Coordinates Data
	{core.NewTag(0x0066, 0x0022), core.VROD}, // Double Point Coordinates Data
	{core.NewTag(0x0066, 0x0040), core.VROL}, // Long Primitive Point Index List
	{core.NewTag(0x0072, 0x0081), core.VROV}, // Selector OV Value
}

func bigEndianInlineLittle(index int) []byte {
	data := make([]byte, 16)
	for i := range data {
		data[i] = byte(index*16 + i + 1)
	}
	return data
}

// bigEndianInlineFile reads a Part 10 object in syntax holding the values of
// bigEndianInlineValues and 32-bit OW Pixel Data, each encoded in the byte
// order of syntax.
func bigEndianInlineFile(t *testing.T, syntax transfer.Syntax) *object.File {
	t.Helper()
	order := binary.ByteOrder(binary.LittleEndian)
	if syntax.ByteOrder == binary.BigEndian {
		order = binary.BigEndian
	}
	encode := func(little []byte, width int) []byte {
		if order == binary.BigEndian {
			return valueencode.ReverseFixedWidth(little, width)
		}
		return bytes.Clone(little)
	}
	elements := append(dicomtest.MinimalDataset(),
		dicomtest.Uint16Element(core.NewTag(0x0028, 0x0002), core.VRUS, order, 1),
		dicomtest.StringElement(core.NewTag(0x0028, 0x0004), core.VRCS, "MONOCHROME2"),
		dicomtest.Uint16Element(core.NewTag(0x0028, 0x0010), core.VRUS, order, 2),
		dicomtest.Uint16Element(core.NewTag(0x0028, 0x0011), core.VRUS, order, 2),
		dicomtest.Uint16Element(core.NewTag(0x0028, 0x0100), core.VRUS, order, 32),
		dicomtest.Uint16Element(core.NewTag(0x0028, 0x0101), core.VRUS, order, 32),
		dicomtest.Uint16Element(core.NewTag(0x0028, 0x0102), core.VRUS, order, 31),
		dicomtest.Uint16Element(core.NewTag(0x0028, 0x0103), core.VRUS, order, 0),
		core.NewRawElement(core.TagPixelData, core.VROW, encode(bigEndianInlineLittle(9), 4)),
	)
	for i, value := range bigEndianInlineValues {
		elements = append(elements, core.NewRawElement(value.tag, value.vr, encode(bigEndianInlineLittle(i), valueencode.ByteOrderWidth(value.vr))))
	}
	data, err := dicomtest.Part10File(syntax, elements...)
	if err != nil {
		t.Fatal(err)
	}
	file, err := object.ReadFile(bytes.NewReader(data))
	if err != nil {
		t.Fatal(err)
	}
	return file
}

func bigEndianInlineTags() []core.Tag {
	tags := []core.Tag{core.TagPixelData}
	for _, value := range bigEndianInlineValues {
		tags = append(tags, value.tag)
	}
	return tags
}

// inlineBinaries decodes the InlineBinary of tags from a DICOM JSON object.
func inlineBinaries(t *testing.T, encoded []byte, tags []core.Tag) map[core.Tag][]byte {
	t.Helper()
	var dataset Dataset
	if err := json.Unmarshal(encoded, &dataset); err != nil {
		t.Fatal(err)
	}
	out := map[core.Tag][]byte{}
	for _, tag := range tags {
		element, ok := dataset[tag.HexString()]
		if !ok || element.InlineBinary == "" {
			t.Fatalf("%s has no InlineBinary: %+v", tag, element)
		}
		data, err := base64.StdEncoding.DecodeString(element.InlineBinary)
		if err != nil {
			t.Fatal(err)
		}
		out[tag] = data
	}
	return out
}

// An Explicit VR Big Endian object is exported, by every Marshal and Encoder
// entry point and without options, with the Little Endian InlineBinary of the
// Explicit VR Little Endian object holding the same values, and reading that
// JSON with a Big Endian ByteOrder restores the original values (#1982).
func TestBigEndianInlineBinaryIsLittleEndianAndRoundTrips(t *testing.T) {
	tags := bigEndianInlineTags()
	little := bigEndianInlineFile(t, transfer.ExplicitVRLittleEndian)
	big := bigEndianInlineFile(t, transfer.ExplicitVRBigEndian)
	if big.Dataset.ValueByteOrder() != binary.BigEndian {
		t.Fatalf("Big Endian object value byte order = %v", big.Dataset.ValueByteOrder())
	}
	reference, err := MarshalCompact(little.Dataset)
	if err != nil {
		t.Fatal(err)
	}
	want := inlineBinaries(t, reference, tags)
	exports := map[string]func(*object.Object) ([]byte, error){
		"Marshal":       func(obj *object.Object) ([]byte, error) { return Marshal(obj, Options{}) },
		"MarshalObject": MarshalObject,
		"MarshalCompact": func(obj *object.Object) ([]byte, error) {
			return MarshalCompact(obj)
		},
		"MarshalPretty": MarshalPretty,
		"MarshalContextByteOrder": func(obj *object.Object) ([]byte, error) {
			return MarshalContext(context.Background(), obj, Options{ByteOrder: binary.BigEndian})
		},
		"EncodeContext": func(obj *object.Object) ([]byte, error) {
			var out bytes.Buffer
			err := NewEncoder(&out, Options{}).EncodeContext(context.Background(), obj)
			return out.Bytes(), err
		},
	}
	for name, export := range exports {
		t.Run(name, func(t *testing.T) {
			encoded, err := export(big.Dataset)
			if err != nil {
				t.Fatal(err)
			}
			got := inlineBinaries(t, encoded, tags)
			for _, tag := range tags {
				if !bytes.Equal(got[tag], want[tag]) {
					t.Errorf("%s InlineBinary % X, want the Little Endian % X", tag, got[tag], want[tag])
				}
			}
			decoders := map[string]func() (*object.Object, error){
				"UnmarshalWithOptions": func() (*object.Object, error) {
					return UnmarshalWithOptions(encoded, std.Dictionary, UnmarshalOptions{ByteOrder: binary.BigEndian})
				},
				"UnmarshalContextSyntax": func() (*object.Object, error) {
					return UnmarshalContext(context.Background(), encoded, std.Dictionary, UnmarshalOptions{TransferSyntax: transfer.ExplicitVRBigEndian})
				},
				"Decoder": func() (*object.Object, error) {
					return NewDecoder(bytes.NewReader(encoded), std.Dictionary, UnmarshalOptions{ByteOrder: binary.BigEndian}).DecodeContext(context.Background())
				},
			}
			for decoderName, decode := range decoders {
				decoded, err := decode()
				if err != nil {
					t.Fatalf("%s: %v", decoderName, err)
				}
				for _, tag := range tags {
					original, _ := big.Dataset.Get(tag)
					restored, ok := decoded.Get(tag)
					originalRaw, _ := original.RawBytes()
					restoredRaw, isRaw := restored.RawBytes()
					if !ok || !isRaw || !bytes.Equal(restoredRaw, originalRaw) {
						t.Errorf("%s: %s restored % X, want the Big Endian % X", decoderName, tag, restoredRaw, originalRaw)
					}
				}
			}
		})
	}
}

// SourceByteOrderInlineBinary keeps the historical form on both sides: the
// object's own bytes, restored unchanged.
func TestSourceByteOrderInlineBinaryRoundTrips(t *testing.T) {
	tags := bigEndianInlineTags()
	big := bigEndianInlineFile(t, transfer.ExplicitVRBigEndian)
	encoded, err := Marshal(big.Dataset, Options{SourceByteOrderInlineBinary: true})
	if err != nil {
		t.Fatal(err)
	}
	got := inlineBinaries(t, encoded, tags)
	decoded, err := UnmarshalWithOptions(encoded, std.Dictionary, UnmarshalOptions{ByteOrder: binary.BigEndian, SourceByteOrderInlineBinary: true})
	if err != nil {
		t.Fatal(err)
	}
	for _, tag := range tags {
		original, _ := big.Dataset.Get(tag)
		originalRaw, _ := original.RawBytes()
		restored, _ := decoded.Get(tag)
		restoredRaw, _ := restored.RawBytes()
		if !bytes.Equal(got[tag], originalRaw) || !bytes.Equal(restoredRaw, originalRaw) {
			t.Errorf("%s InlineBinary % X restored % X, want % X", tag, got[tag], restoredRaw, originalRaw)
		}
	}
}
