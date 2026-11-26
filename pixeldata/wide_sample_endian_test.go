package pixeldata_test

import (
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"math"
	"os"
	"path/filepath"
	"strconv"
	"testing"

	"github.com/ThalesMMS/dicom-go/core"
	"github.com/ThalesMMS/dicom-go/dicomjson"
	"github.com/ThalesMMS/dicom-go/object"
	"github.com/ThalesMMS/dicom-go/pixeldata"
	"github.com/ThalesMMS/dicom-go/transfer"
)

// Synthetic wide-sample fixtures for #937. Expected byte layouts are computed
// here with encoding/binary at the sample width, independently of the library
// swap helpers, so a word-swapped (half-transposed) sample cannot pass.

var (
	wideTagIconImageSequence = core.NewTag(0x0088, 0x0200)
	wideTagFloatPixelData    = core.NewTag(0x7FE0, 0x0008)
	wideTagDoubleFloatPixel  = core.NewTag(0x7FE0, 0x0009)
	wideTagBitsAllocated     = core.NewTag(0x0028, 0x0100)
	wideRTDoseSOPClassUID    = "1.2.840.10008.5.1.4.1.1.481.2"
	wideUIDRoot              = "1.2.826.0.1.3680043.10.543.937"
)

type wideEndianCase struct {
	name                string
	bitsAllocated       uint16
	bitsStored          uint16
	pixelRepresentation uint16
	values              []uint64 // stored bit patterns, 2 frames of 2x3
	iconBits            uint16   // 0 means no icon
}

func wideEndianCases() []wideEndianCase {
	signed32 := func(v int32) uint64 { return uint64(uint32(v)) }
	return []wideEndianCase{
		{
			name: "rtdose-32-unsigned-icon16", bitsAllocated: 32, bitsStored: 32, iconBits: 16,
			values: []uint64{1249000, 0x01020304, 0xA1B2C3D4, 0xFFFFFFFF, 0, 7, 0x80000000, 0x7FFFFFFF, 0x00010000, 0x0000FFFF, 0xDEADBEEF, 1},
		},
		{
			name: "rtdose-32-signed-icon8", bitsAllocated: 32, bitsStored: 32, pixelRepresentation: 1, iconBits: 8,
			values: []uint64{signed32(-2000), signed32(math.MinInt32), signed32(math.MaxInt32), signed32(-1), 0, signed32(1249000), signed32(-65536), 65535, signed32(-2), 3, signed32(-1249000), 0x01020304},
		},
		{
			name: "rtdose-32-bits-stored-24", bitsAllocated: 32, bitsStored: 24,
			values: []uint64{0x00FFFFFF, 0x00123456, 1249000, 0, 0x00800000, 0x0000FF00, 0x00ABCDEF, 1, 2, 3, 0x00654321, 0x00100000},
		},
		{
			name: "samples-64-unsigned", bitsAllocated: 64, bitsStored: 64, iconBits: 16,
			values: []uint64{1249000, 0x0102030405060708, math.MaxUint64, 0, 1 << 63, 1<<53 + 1, 0xA1B2C3D4E5F60718, 7, 1 << 32, 0xFFFFFFFF, 0x8000000000000001, 42},
		},
	}
}

func (c wideEndianCase) width() int { return int(c.bitsAllocated / 8) }

func (c wideEndianCase) pixelBytes(order binary.ByteOrder) []byte {
	out := make([]byte, len(c.values)*c.width())
	for i, v := range c.values {
		switch c.width() {
		case 4:
			order.PutUint32(out[i*4:], uint32(v))
		case 8:
			order.PutUint64(out[i*8:], v)
		}
	}
	return out
}

var wideIconValues16 = []uint16{0x0102, 0xA0B0, 0x00FF, 0xFF00}

func (c wideEndianCase) iconBytes(order binary.ByteOrder) []byte {
	switch c.iconBits {
	case 8:
		return []byte{0x01, 0x80, 0xFE, 0x7F}
	case 16:
		out := make([]byte, 2*len(wideIconValues16))
		for i, v := range wideIconValues16 {
			order.PutUint16(out[i*2:], v)
		}
		return out
	default:
		return nil
	}
}

func wideU16(tag core.Tag, v uint16) core.Element {
	return core.Element{Header: core.ElementHeader{Tag: tag, VR: core.VRUS}, Value: core.Uint16Value{v}}
}

func wideString(tag core.Tag, vr core.VR, v string) core.Element {
	return core.Element{Header: core.ElementHeader{Tag: tag, VR: vr}, Value: core.StringValue{v}}
}

func wideImagePixelElements(rows, columns, bitsAllocated, bitsStored, pixelRepresentation uint16) []core.Element {
	return []core.Element{
		wideU16(core.NewTag(0x0028, 0x0002), 1),
		wideString(core.NewTag(0x0028, 0x0004), core.VRCS, "MONOCHROME2"),
		wideU16(core.NewTag(0x0028, 0x0010), rows),
		wideU16(core.NewTag(0x0028, 0x0011), columns),
		wideU16(wideTagBitsAllocated, bitsAllocated),
		wideU16(core.NewTag(0x0028, 0x0101), bitsStored),
		wideU16(core.NewTag(0x0028, 0x0102), bitsStored-1),
		wideU16(core.NewTag(0x0028, 0x0103), pixelRepresentation),
	}
}

// object builds the Little Endian in-memory source for a case.
func (c wideEndianCase) object(suffix int) *object.Object {
	elements := []core.Element{
		wideString(core.NewTag(0x0008, 0x0016), core.VRUI, wideRTDoseSOPClassUID),
		wideString(core.NewTag(0x0008, 0x0018), core.VRUI, wideUIDRoot+"."+strconv.Itoa(suffix)),
		wideString(core.NewTag(0x0008, 0x0060), core.VRCS, "RTDOSE"),
		wideString(core.NewTag(0x0010, 0x0010), core.VRPN, "SYNTHETIC^ENDIAN"),
		wideString(core.NewTag(0x0028, 0x0008), core.VRIS, "2"),
	}
	elements = append(elements, wideImagePixelElements(2, 3, c.bitsAllocated, c.bitsStored, c.pixelRepresentation)...)
	if c.iconBits != 0 {
		vr := core.VROB
		if c.iconBits > 8 {
			vr = core.VROW
		}
		item := append(wideImagePixelElements(2, 2, c.iconBits, c.iconBits, 0), core.NewRawElement(core.TagPixelData, vr, c.iconBytes(binary.LittleEndian)))
		elements = append(elements, core.Element{
			Header: core.ElementHeader{Tag: wideTagIconImageSequence, VR: core.VRSQ},
			Value:  core.SequenceValue{Items: []core.DataSet{{Elements: item}}},
		})
	}
	elements = append(elements, core.NewRawElement(core.TagPixelData, core.VROW, c.pixelBytes(binary.LittleEndian)))
	return object.FromElements(elements, nil)
}

func wideRawPixel(t *testing.T, obj *object.Object) []byte {
	t.Helper()
	var buf bytes.Buffer
	if _, err := obj.CopyValueTo(core.TagPixelData, &buf); err != nil {
		t.Fatalf("Pixel Data: %v", err)
	}
	return buf.Bytes()
}

func wideIconPixel(t *testing.T, obj *object.Object) []byte {
	t.Helper()
	items, ok := obj.GetSequence(wideTagIconImageSequence)
	if !ok || len(items) != 1 {
		t.Fatalf("Icon Image Sequence items = %d ok=%v", len(items), ok)
	}
	raw, ok := items[0].GetRaw(core.TagPixelData)
	if !ok {
		t.Fatal("icon Pixel Data missing")
	}
	return raw
}

// wordSwapped is the defective layout: 16-bit word reversal of a wide sample.
func wordSwapped(data []byte) []byte {
	out := append([]byte(nil), data...)
	for i := 0; i+1 < len(out); i += 2 {
		out[i], out[i+1] = out[i+1], out[i]
	}
	return out
}

func assertWideLayout(t *testing.T, label string, c wideEndianCase, obj *object.Object, order binary.ByteOrder) {
	t.Helper()
	if got, want := wideRawPixel(t, obj), c.pixelBytes(order); !bytes.Equal(got, want) {
		t.Fatalf("%s Pixel Data = % x, want % x", label, got, want)
	}
	if c.iconBits != 0 {
		if got, want := wideIconPixel(t, obj), c.iconBytes(order); !bytes.Equal(got, want) {
			t.Fatalf("%s icon Pixel Data = % x, want % x (icon must use its own Bits Allocated)", label, got, want)
		}
	}
	metadata, err := pixeldata.ExtractMetadata(obj)
	if err != nil {
		t.Fatalf("%s metadata: %v", label, err)
	}
	if metadata.BitsAllocated != c.bitsAllocated || metadata.PixelRepresentation != c.pixelRepresentation || metadata.NumberOfFrames != 2 {
		t.Fatalf("%s metadata = %+v", label, metadata)
	}
}

func writeWidePart10(t *testing.T, path string, dataset *object.Object, syntax transfer.Syntax) {
	t.Helper()
	var buf bytes.Buffer
	if err := object.WriteFile(&buf, &object.File{Dataset: dataset, TransferSyntax: syntax}); err != nil {
		t.Fatalf("write %s: %v", syntax.Name, err)
	}
	if err := os.WriteFile(path, buf.Bytes(), 0o600); err != nil {
		t.Fatal(err)
	}
}

func TestWideSampleBigEndianUsesSampleWidthOnEveryPath(t *testing.T) {
	ctx := context.Background()
	for index, c := range wideEndianCases() {
		t.Run(c.name, func(t *testing.T) {
			// The fixture must distinguish sample-width from word swapping.
			if bytes.Equal(c.pixelBytes(binary.BigEndian), wordSwapped(c.pixelBytes(binary.LittleEndian))) {
				t.Fatal("fixture cannot detect half-swapped samples")
			}
			source := c.object(index + 1)
			assertWideLayout(t, "in-memory LE", c, source, binary.LittleEndian)

			bigEndian, _, err := pixeldata.TranscodeDataSet(ctx, source, transfer.ExplicitVRLittleEndian, transfer.ExplicitVRBigEndian, pixeldata.TranscodeOptions{})
			if err != nil {
				t.Fatal(err)
			}
			if bigEndian.ValueByteOrder() != binary.BigEndian {
				t.Fatal("transcoded object does not record Big Endian values")
			}
			assertWideLayout(t, "transcoded BE", c, bigEndian, binary.BigEndian)
			assertWideLayout(t, "source after transcode", c, source, binary.LittleEndian)

			path := filepath.Join(t.TempDir(), "be.dcm")
			writeWidePart10(t, path, bigEndian, transfer.ExplicitVRBigEndian)
			for _, deferred := range []bool{false, true} {
				file, err := object.OpenFileWithOptions(path, object.ReadFileOptions{DeferPixelData: deferred})
				if err != nil {
					t.Fatal(err)
				}
				t.Cleanup(func() { _ = file.Close() })
				label := "reopened BE deferred=" + strconv.FormatBool(deferred)
				if file.TransferSyntax.UID != transfer.ExplicitVRBigEndian.UID {
					t.Fatalf("%s syntax = %s", label, file.TransferSyntax.UID)
				}
				// Deferred replay returns the same stored bytes; the nested icon
				// is materialized and never confused with the top-level value.
				assertWideLayout(t, label, c, file.Dataset, binary.BigEndian)
				if deferred {
					// Conversion never runs on unavailable values: it is refused
					// with a typed error instead of emitting a partial object.
					got, _, err := pixeldata.TranscodeDataSet(ctx, file.Dataset, file.TransferSyntax, transfer.ExplicitVRLittleEndian, pixeldata.TranscodeOptions{})
					if !errors.Is(err, pixeldata.ErrTranscodeUnsupported) || got != nil {
						t.Fatalf("%s: deferred conversion err=%v output=%v", label, err, got != nil)
					}
					continue
				}

				// Equivalent native clone (BE -> LE).
				little, _, err := pixeldata.TranscodeDataSet(ctx, file.Dataset, file.TransferSyntax, transfer.ExplicitVRLittleEndian, pixeldata.TranscodeOptions{})
				if err != nil {
					t.Fatalf("%s: %v", label, err)
				}
				assertWideLayout(t, label+" -> LE transcode", c, little, binary.LittleEndian)

				// Decode path: top-level frames at sample width, nested items by rule.
				decoded, target, err := pixeldata.DecompressDataSet(file.Dataset, file.TransferSyntax, pixeldata.DecompressOptions{})
				if err != nil {
					t.Fatalf("%s decompress: %v", label, err)
				}
				if target.UID != transfer.ExplicitVRLittleEndian.UID {
					t.Fatalf("%s decompress target = %s", label, target.UID)
				}
				assertWideLayout(t, label+" -> LE decompress", c, decoded, binary.LittleEndian)
				if err := file.Close(); err != nil {
					t.Fatal(err)
				}
			}

			// Writing the round-tripped LE object and reading it again keeps samples.
			little, _, err := pixeldata.TranscodeDataSet(ctx, bigEndian, transfer.ExplicitVRBigEndian, transfer.ExplicitVRLittleEndian, pixeldata.TranscodeOptions{})
			if err != nil {
				t.Fatal(err)
			}
			lePath := filepath.Join(t.TempDir(), "le.dcm")
			writeWidePart10(t, lePath, little, transfer.ExplicitVRLittleEndian)
			reread, err := object.OpenFile(lePath)
			if err != nil {
				t.Fatal(err)
			}
			defer reread.Close()
			assertWideLayout(t, "LE rewrite", c, reread.Dataset, binary.LittleEndian)
		})
	}
}

func TestWideSampleRawDataSetAndJSONKeepDeclaredOrder(t *testing.T) {
	ctx := context.Background()
	c := wideEndianCases()[0]
	bigEndian, _, err := pixeldata.TranscodeDataSet(ctx, c.object(90), transfer.ExplicitVRLittleEndian, transfer.ExplicitVRBigEndian, pixeldata.TranscodeOptions{})
	if err != nil {
		t.Fatal(err)
	}

	// Raw dataset (no Part 10 header) under an explicit caller-supplied syntax.
	var raw bytes.Buffer
	if err := object.WriteDataSet(&raw, bigEndian, transfer.ExplicitVRBigEndian); err != nil {
		t.Fatal(err)
	}
	parsed, err := object.ReadDataSet(bytes.NewReader(raw.Bytes()), transfer.ExplicitVRBigEndian)
	if err != nil {
		t.Fatal(err)
	}
	assertWideLayout(t, "raw BE dataset", c, parsed, binary.BigEndian)

	// DICOM JSON: InlineBinary is the Value Field in the object's value byte
	// order; the reader must declare that order, which is not self-described.
	document, err := dicomjson.Marshal(parsed, dicomjson.Options{})
	if err != nil {
		t.Fatal(err)
	}
	fromJSON, err := dicomjson.UnmarshalWithOptions(document, nil, dicomjson.UnmarshalOptions{TransferSyntax: transfer.ExplicitVRBigEndian})
	if err != nil {
		t.Fatal(err)
	}
	if fromJSON.ValueByteOrder() != binary.BigEndian {
		t.Fatal("JSON reader ignored the declared Big Endian syntax")
	}
	assertWideLayout(t, "BE JSON round trip", c, fromJSON, binary.BigEndian)
	little, _, err := pixeldata.TranscodeDataSet(ctx, fromJSON, transfer.ExplicitVRBigEndian, transfer.ExplicitVRLittleEndian, pixeldata.TranscodeOptions{})
	if err != nil {
		t.Fatal(err)
	}
	assertWideLayout(t, "BE JSON -> LE", c, little, binary.LittleEndian)

	// Little Endian JSON of the converted object round-trips with the default reader.
	document, err = dicomjson.Marshal(little, dicomjson.Options{})
	if err != nil {
		t.Fatal(err)
	}
	fromJSON, err = dicomjson.UnmarshalWithOptions(document, nil, dicomjson.DefaultUnmarshalOptions())
	if err != nil {
		t.Fatal(err)
	}
	assertWideLayout(t, "LE JSON round trip", c, fromJSON, binary.LittleEndian)
}

func TestFloatPixelDataSwapsByVRWidth(t *testing.T) {
	ctx := context.Background()
	floats := []float32{1.5, -2.25, 1249000.125, float32(math.Inf(-1)), 0x1p-126, -0.0}
	doubles := []float64{1.5, -2.25, 1249000.125, math.MaxFloat64, 0x1p-1074, math.Inf(1)}
	for _, tc := range []struct {
		name   string
		tag    core.Tag
		vr     core.VR
		bits   uint16
		encode func(binary.ByteOrder) []byte
	}{
		{"float-OF", wideTagFloatPixelData, core.VROF, 32, func(order binary.ByteOrder) []byte {
			out := make([]byte, 4*len(floats))
			for i, v := range floats {
				order.PutUint32(out[i*4:], math.Float32bits(v))
			}
			return out
		}},
		{"double-OD", wideTagDoubleFloatPixel, core.VROD, 64, func(order binary.ByteOrder) []byte {
			out := make([]byte, 8*len(doubles))
			for i, v := range doubles {
				order.PutUint64(out[i*8:], math.Float64bits(v))
			}
			return out
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			elements := []core.Element{
				wideString(core.NewTag(0x0008, 0x0016), core.VRUI, "1.2.840.10008.5.1.4.1.1.30"),
				wideString(core.NewTag(0x0008, 0x0018), core.VRUI, wideUIDRoot+".7"),
				wideString(core.NewTag(0x0028, 0x0008), core.VRIS, "1"),
				wideU16(core.NewTag(0x0028, 0x0002), 1),
				wideString(core.NewTag(0x0028, 0x0004), core.VRCS, "MONOCHROME2"),
				wideU16(core.NewTag(0x0028, 0x0010), 2),
				wideU16(core.NewTag(0x0028, 0x0011), 3),
				wideU16(wideTagBitsAllocated, tc.bits),
				core.NewRawElement(tc.tag, tc.vr, tc.encode(binary.LittleEndian)),
			}
			source := object.FromElements(elements, nil)
			bigEndian, _, err := pixeldata.TranscodeDataSet(ctx, source, transfer.ExplicitVRLittleEndian, transfer.ExplicitVRBigEndian, pixeldata.TranscodeOptions{})
			if err != nil {
				t.Fatal(err)
			}
			if got, _ := bigEndian.GetRaw(tc.tag); !bytes.Equal(got, tc.encode(binary.BigEndian)) {
				t.Fatalf("BE %s = % x, want % x", tc.vr, got, tc.encode(binary.BigEndian))
			}
			path := filepath.Join(t.TempDir(), "float.dcm")
			writeWidePart10(t, path, bigEndian, transfer.ExplicitVRBigEndian)
			file, err := object.OpenFile(path)
			if err != nil {
				t.Fatal(err)
			}
			defer file.Close()
			little, _, err := pixeldata.TranscodeDataSet(ctx, file.Dataset, file.TransferSyntax, transfer.ExplicitVRLittleEndian, pixeldata.TranscodeOptions{})
			if err != nil {
				t.Fatal(err)
			}
			if got, _ := little.GetRaw(tc.tag); !bytes.Equal(got, tc.encode(binary.LittleEndian)) {
				t.Fatalf("LE %s = % x, want % x", tc.vr, got, tc.encode(binary.LittleEndian))
			}
		})
	}
}

func TestWideSampleEndianRefusesInvalidLengthsWithoutOutput(t *testing.T) {
	ctx := context.Background()
	c := wideEndianCases()[0]
	for _, tc := range []struct {
		name   string
		mutate func(*object.Object)
	}{
		{"pixel data not a multiple of the sample width", func(obj *object.Object) {
			obj.Put(core.NewRawElement(core.TagPixelData, core.VROW, c.pixelBytes(binary.LittleEndian)[:len(c.values)*4-2]))
		}},
		{"nested US with odd length", func(obj *object.Object) {
			item := append(wideImagePixelElements(2, 2, 16, 16, 0), core.NewRawElement(core.NewTag(0x0028, 0x0106), core.VRUS, []byte{1, 2, 3}))
			obj.Put(core.Element{
				Header: core.ElementHeader{Tag: wideTagIconImageSequence, VR: core.VRSQ},
				Value:  core.SequenceValue{Items: []core.DataSet{{Elements: item}}},
			})
		}},
		{"raw Bits Allocated with wrong length", func(obj *object.Object) {
			item := []core.Element{
				core.NewRawElement(wideTagBitsAllocated, core.VRUS, []byte{32, 0, 0, 0}),
				core.NewRawElement(core.TagPixelData, core.VROW, []byte{1, 2, 3, 4}),
			}
			obj.Put(core.Element{
				Header: core.ElementHeader{Tag: wideTagIconImageSequence, VR: core.VRSQ},
				Value:  core.SequenceValue{Items: []core.DataSet{{Elements: item}}},
			})
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			source := c.object(95)
			tc.mutate(source)
			got, _, err := pixeldata.TranscodeDataSet(ctx, source, transfer.ExplicitVRLittleEndian, transfer.ExplicitVRBigEndian, pixeldata.TranscodeOptions{})
			if err == nil || got != nil {
				t.Fatalf("invalid input converted: err=%v output=%v", err, got != nil)
			}
			if !errors.Is(err, pixeldata.ErrPixelDataSizeMismatch) && !errors.Is(err, pixeldata.ErrInvalidMetadata) && !errors.Is(err, pixeldata.ErrTranscodeUnsupported) {
				t.Fatalf("error = %v, want typed size/metadata error", err)
			}
		})
	}
}
