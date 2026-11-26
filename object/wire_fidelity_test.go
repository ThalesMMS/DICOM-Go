package object_test

import (
	"bytes"
	"encoding/binary"
	"io"
	"math"
	"slices"
	"strings"
	"testing"

	"github.com/ThalesMMS/dicom-go/core"
	"github.com/ThalesMMS/dicom-go/dictionary/std"
	"github.com/ThalesMMS/dicom-go/internal/dicomtest"
	"github.com/ThalesMMS/dicom-go/object"
	"github.com/ThalesMMS/dicom-go/transfer"
)

// In Implicit VR the four bytes after the tag encode Value Length, even
// when the first two could spell a known VR (PS3.5 7.1.3).
func TestPart10ImplicitValueLengthCanResembleExplicitVR(t *testing.T) {
	syntax := transfer.ImplicitVRLittleEndian
	privateValue := core.NewTag(0x0019, 0x1014)
	rows := core.NewTag(0x0028, 0x0010)
	columns := core.NewTag(0x0028, 0x0011)
	pixels := []byte{27, 28, 29, 30, 31, 32}
	for _, apparentVR := range []string{"DS", "FD", "FL", "LO", "LT", "PN", "TM"} {
		t.Run(apparentVR, func(t *testing.T) {
			// All cases have even lengths. The upper half of the implicit VL
			// looks like a valid zero-length explicit short-form header.
			length := int(binary.LittleEndian.Uint16([]byte(apparentVR)))
			value := make([]byte, length)
			copy(value[len(value)-4:], []byte{0xca, 0xfe, 0x10, 0x20})
			data := append(dicomtest.MinimalDataset(),
				core.NewRawElement(privateValue, core.VRUN, value),
				dicomtest.NewUShortElement(rows, 2),
				dicomtest.NewUShortElement(columns, 3),
				core.NewRawElement(core.TagPixelData, core.VROW, pixels),
			)
			wire, err := dicomtest.Part10File(syntax, data...)
			if err != nil {
				t.Fatal(err)
			}
			header := []byte{0x19, 0, 0x14, 0x10, apparentVR[0], apparentVR[1], 0, 0}
			headerAt := bytes.Index(wire, header)
			if headerAt < 0 || len(wire)-headerAt < 8+length {
				t.Fatal("fixture lacks the intended implicit value length")
			}
			file, err := object.ReadFileWithOptions(bytes.NewReader(wire), object.ReadFileOptions{
				MaxTotalBytes: int64(len(wire)) + 1, MaxElementBytes: int64(length),
			})
			if err != nil {
				t.Fatal(err)
			}
			defer file.Close()
			unknown, ok := file.Dataset.Get(privateValue)
			if !ok || unknown.VR() != core.VRUN || unknown.EncodedLength() != core.Length(length) {
				t.Fatalf("private value header = %+v, found %v", unknown.Header, ok)
			}
			if raw, ok := unknown.RawBytes(); !ok || !bytes.Equal(raw, value) {
				t.Fatal("private payload shifted or changed")
			}
			for _, dimension := range []struct {
				tag  core.Tag
				want int64
			}{{rows, 2}, {columns, 3}} {
				if v, err := file.Dataset.GetInt(dimension.tag); err != nil || v != dimension.want {
					t.Fatalf("dimension %s = %d, %v, want %d", dimension.tag, v, err, dimension.want)
				}
			}
			if raw, ok := file.GetRaw(core.TagPixelData); !ok || !bytes.Equal(raw, pixels) {
				t.Fatalf("root Pixel Data = %x, found %v", raw, ok)
			}
		})
	}
}

func TestPart10SingleValueTextKeepsLiteralBackslashAndWhitespace(t *testing.T) {
	for _, syntax := range []transfer.Syntax{
		transfer.ExplicitVRLittleEndian,
		transfer.ImplicitVRLittleEndian,
		transfer.ExplicitVRBigEndian,
		transfer.DeflatedExplicitVRLittleEndian,
	} {
		t.Run(syntax.Name, func(t *testing.T) {
			for _, field := range []struct {
				tag  core.Tag
				vr   core.VR
				text string
			}{
				{core.NewTag(0x0032, 0x4000), core.VRLT, "  Revisão\\controle\r\n\t終"},
				{core.NewTag(0x0008, 0x2111), core.VRST, "  Étape\\A\r\n\fβ"},
				{core.NewTag(0x0040, 0xA160), core.VRUT, "  " + strings.Repeat("amostra\\δ\r\n", 6000) + "fim"},
			} {
				t.Run(field.vr.String(), func(t *testing.T) {
					dataset := object.FromElements(dicomtest.MinimalDataset(), std.Dictionary)
					dataset.Put(dicomtest.NewStringElement(core.NewTag(0x0008, 0x0005), core.VRCS, "ISO_IR 192"))
					if err := dataset.SetStrings(field.tag, field.vr, field.text); err != nil {
						t.Fatal(err)
					}
					var wire bytes.Buffer
					if err := object.WriteFile(&wire, &object.File{TransferSyntax: syntax, Dataset: dataset}); err != nil {
						t.Fatal(err)
					}
					decoded, err := object.ReadFile(bytes.NewReader(wire.Bytes()))
					if err != nil {
						t.Fatal(err)
					}
					defer decoded.Close()
					if got, err := decoded.Dataset.LookupStrings(field.tag); err != nil || !slices.Equal(got, []string{field.text}) {
						t.Fatalf("decoded %s as %d values with error %v", field.vr, len(got), err)
					}
					want := []byte(field.text)
					if len(want)%2 == 1 {
						want = append(want, ' ')
					}
					el, ok := decoded.Dataset.Get(field.tag)
					raw, haveRaw := el.RawBytes()
					if !ok || !haveRaw || el.VR() != field.vr || el.EncodedLength() != core.Length(len(want)) || !bytes.Equal(raw, want) {
						t.Fatalf("%s wire value differs: header %+v, %d raw bytes, want %d", field.vr, el.Header, len(raw), len(want))
					}
				})
			}
		})
	}
}

func TestPart10VeryLongIntegersPreserveAllWords(t *testing.T) {
	for _, syntax := range []transfer.Syntax{
		transfer.ExplicitVRLittleEndian,
		transfer.ImplicitVRLittleEndian,
		transfer.ExplicitVRBigEndian,
		transfer.DeflatedExplicitVRLittleEndian,
	} {
		t.Run(syntax.Name, func(t *testing.T) {
			for _, sample := range []struct {
				vr    core.VR
				tag   core.Tag
				value core.Value
				words []uint64
			}{
				{core.VRSV, core.NewTag(0x0072, 0x0082), core.Int64Value{math.MinInt64, -2, 0, math.MaxInt64},
					[]uint64{0x8000000000000000, 0xfffffffffffffffe, 0, 0x7fffffffffffffff}},
				{core.VRUV, core.NewTag(0x0072, 0x0083), core.Uint64Value{0, 1 << 53, 1<<53 + 1, math.MaxUint64},
					[]uint64{0, 0x0020000000000000, 0x0020000000000001, 0xffffffffffffffff}},
				{core.VROV, core.NewTag(0x0072, 0x0081), core.Uint64Value{0x235781a4ce2f960b, math.MaxUint64},
					[]uint64{0x235781a4ce2f960b, 0xffffffffffffffff}},
			} {
				t.Run(sample.vr.String(), func(t *testing.T) {
					dataset := object.FromElements(dicomtest.MinimalDataset(), std.Dictionary)
					dataset.Put(core.Element{Header: core.ElementHeader{Tag: sample.tag, VR: sample.vr}, Value: sample.value})
					var wire bytes.Buffer
					if err := object.WriteFile(&wire, &object.File{TransferSyntax: syntax, Dataset: dataset}); err != nil {
						t.Fatal(err)
					}
					decoded, err := object.ReadFile(bytes.NewReader(wire.Bytes()))
					if err != nil {
						t.Fatal(err)
					}
					defer decoded.Close()
					el, found := decoded.Dataset.Get(sample.tag)
					raw, haveRaw := el.RawBytes()
					if !found || !haveRaw || el.VR() != sample.vr || len(raw) != 8*len(sample.words) {
						t.Fatalf("header %+v, %d raw bytes for %d words", el.Header, len(raw), len(sample.words))
					}
					for i, want := range sample.words {
						if word := syntax.ByteOrder.Uint64(raw[i*8:]); word != want {
							t.Errorf("word %d = %#016x, want %#016x", i, word, want)
						}
					}
					if sample.vr == core.VRUV {
						if got, err := decoded.Dataset.GetInts(sample.tag); err == nil || got != nil {
							t.Errorf("a UV above MaxInt64 should return an error and no truncated values, got %v, %v", got, err)
						}
					}
				})
			}
		})
	}
}

// A defined-length private value can contain Item tags and even byte
// sequences spelling Pixel Data. They are data inside the declared value.
func TestPart10OpaqueItemHeadersDoNotMoveEncapsulatedPixelData(t *testing.T) {
	syntax := transfer.JPEGBaseline
	// An undefined-length item containing an explicit Pixel Data header and its
	// zero-length value, followed by item and sequence delimiters.
	opaque := []byte{
		0xfe, 0xff, 0x00, 0xe0, 0xff, 0xff, 0xff, 0xff,
		0xe0, 0x7f, 0x10, 0x00, 'O', 'B', 0, 0, 0, 0, 0, 0,
		0xfe, 0xff, 0x0d, 0xe0, 0, 0, 0, 0,
		0xfe, 0xff, 0xdd, 0xe0, 0, 0, 0, 0,
	}
	tag := core.NewTag(0x0051, 0x1033)
	first, second := []byte{0xff, 0xd8, 11, 16}, []byte{35, 71, 0xff, 0xd9}
	for _, vr := range []core.VR{core.VROB, core.VRUN} {
		t.Run(vr.String(), func(t *testing.T) {
			data := append(dicomtest.MinimalDataset(), core.NewRawElement(tag, vr, opaque),
				dicomtest.NewFragmentSequenceElement(core.TagPixelData, nil, first, second))
			wire, err := dicomtest.Part10File(syntax, data...)
			if err != nil {
				t.Fatal(err)
			}
			file, err := object.ReadFile(bytes.NewReader(wire))
			if err != nil {
				t.Fatal(err)
			}
			defer file.Close()
			var rewritten bytes.Buffer
			if err := object.WriteFile(&rewritten, file); err != nil {
				t.Fatal(err)
			}
			for _, input := range []struct {
				name string
				r    io.Reader
			}{{"original", bytes.NewReader(wire)}, {"rewritten", bytes.NewReader(rewritten.Bytes())}} {
				t.Run(input.name, func(t *testing.T) {
					decoded, err := object.ReadFile(input.r)
					if err != nil {
						t.Fatal(err)
					}
					defer decoded.Close()
					private, ok := decoded.Dataset.Get(tag)
					raw, haveRaw := private.RawBytes()
					if !ok || !haveRaw || private.VR() != vr || !bytes.Equal(raw, opaque) {
						t.Fatalf("private value changed: header %+v, %x", private.Header, raw)
					}
					pixel, _ := decoded.Dataset.Get(core.TagPixelData)
					value, ok := pixel.Value.(core.FragmentSequence)
					if !ok || len(value.OffsetTable) != 0 || len(value.Fragments) != 2 ||
						!bytes.Equal(value.Fragments[0], first) || !bytes.Equal(value.Fragments[1], second) {
						t.Fatalf("unexpected fragment sequence: %#v", pixel.Value)
					}
				})
			}
		})
	}
}
