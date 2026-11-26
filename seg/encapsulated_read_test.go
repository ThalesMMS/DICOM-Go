package seg

import (
	"bytes"
	"compress/flate"
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"reflect"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/ThalesMMS/dicom-go/core"
	"github.com/ThalesMMS/dicom-go/internal/derivedio"
	"github.com/ThalesMMS/dicom-go/object"
	"github.com/ThalesMMS/dicom-go/pixeldata"
	"github.com/ThalesMMS/dicom-go/pixeldata/deflateframe"
	"github.com/ThalesMMS/dicom-go/pixeldata/rle"
	"github.com/ThalesMMS/dicom-go/render"
	"github.com/ThalesMMS/dicom-go/roi"
	"github.com/ThalesMMS/dicom-go/transfer"
)

func compressedSEGRegistry(t *testing.T) pixeldata.Registry {
	t.Helper()
	r := pixeldata.NewMemoryRegistry()
	if err := deflateframe.Register(r); err != nil {
		t.Fatal(err)
	}
	if err := rle.Register(r); err != nil {
		t.Fatal(err)
	}
	return r
}

// Independent stream fixtures: RFC1951 through the Go standard library and
// literal PackBits planes from PS3.5 G.3. No dicom-go encoder is the oracle.
func fixtureSEGStream(t *testing.T, native []byte, bits int, syntax transfer.Syntax) []byte {
	t.Helper()
	if syntax.UID == transfer.DeflatedImageFrameCompression.UID {
		var b bytes.Buffer
		w, err := flate.NewWriter(&b, flate.DefaultCompression)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := w.Write(native); err != nil {
			t.Fatal(err)
		}
		if err := w.Close(); err != nil {
			t.Fatal(err)
		}
		out := b.Bytes()
		if len(out)&1 != 0 {
			out = append(out, 0)
		}
		return out
	}
	planes := bits / 8
	out := make([]byte, 64)
	binary.LittleEndian.PutUint32(out, uint32(planes))
	for plane := 0; plane < planes; plane++ {
		binary.LittleEndian.PutUint32(out[4+4*plane:], uint32(len(out)))
		var values []byte
		for sample := 0; sample < len(native)/planes; sample++ {
			values = append(values, native[sample*planes+planes-1-plane])
		}
		for len(values) > 0 {
			n := min(128, len(values))
			out = append(out, byte(n-1))
			out = append(out, values[:n]...)
			values = values[n:]
		}
	}
	if len(out)&1 != 0 {
		out = append(out, 0)
	}
	return out
}

func compressedSEGFixture(t *testing.T, kind string, columns, rows, frames, bits int) (*object.File, *Document) {
	t.Helper()
	doc := patternDocument(SegmentationStorage, kind)
	doc.Columns, doc.Rows = columns, rows
	doc.Segments = []Segment{{Number: 1, Label: "Synthetic", AlgorithmType: AlgorithmManual}}
	syntax := transfer.DeflatedImageFrameCompression
	if kind != SegmentationTypeBinary {
		syntax = transfer.RLELossless
	}
	if kind == SegmentationTypeFractional {
		doc.FractionalType, doc.MaxFractionalValue = FractionalTypeProbability, 200
	}
	if kind == SegmentationTypeLabelMap {
		doc.SOPClassUID = LabelMapSegmentationStorage
		doc.LabelMapBitsAllocated = bits
		doc.Segments = []Segment{{Number: 0, Label: "Background", AlgorithmType: AlgorithmManual},
			{Number: 1, Label: "One", AlgorithmType: AlgorithmManual}}
		if bits == 16 {
			doc.Segments = append(doc.Segments, Segment{Number: 513, Label: "Wide", AlgorithmType: AlgorithmManual})
		}
	}
	var fragments [][]byte
	for frame := 0; frame < frames; frame++ {
		f := Frame{SegmentNumber: 1, SliceIndex: frame,
			ReferencedSOPInstanceUID: fmt.Sprintf("1.2.3.1421.%d", frame+1), ReferencedFrameNumber: frame + 1,
			Geometry: render.SliceGeometry{Origin: render.Vec3{Z: float64(frame) * 2.5},
				RowDir: render.Vec3{X: 1}, ColDir: render.Vec3{Y: 1}, Normal: render.Vec3{Z: 1},
				RowSpacing: 0.8, ColSpacing: 0.6, Rows: rows, Columns: columns},
		}
		doc.ReferencedImages = append(doc.ReferencedImages, ReferencedImage{SeriesInstanceUID: "1.2.3.1421", SOPClassUID: "1.2.840.10008.5.1.4.1.1.2", SOPInstanceUID: f.ReferencedSOPInstanceUID, Frames: []int{frame + 1}})
		pixels := columns * rows
		native := make([]byte, (pixels*bits+7)/8)
		switch kind {
		case SegmentationTypeBinary:
			f.Mask = roi.NewRasterMask(columns, rows)
			for i := 0; i < pixels; i++ {
				if (i+frame)%3 == 0 {
					native[i/8] |= 1 << uint(i&7)
					f.Mask.Set(i%columns, i/columns, true)
				}
			}
			// Unused frame-local bits are not voxels in the next frame.
			if pixels&7 != 0 {
				native[len(native)-1] |= ^byte((1 << uint(pixels&7)) - 1)
			}
		case SegmentationTypeFractional:
			f.Fractional = make([]uint8, pixels)
			for i := range f.Fractional {
				f.Fractional[i] = uint8((i*17 + frame*29) % 201)
				native[i] = f.Fractional[i]
			}
		case SegmentationTypeLabelMap:
			f.LabelMap = make([]uint16, pixels)
			for i := range f.LabelMap {
				value := uint16((i + frame) % 2)
				if bits == 16 && (i+frame)%3 == 0 {
					value = 513
				}
				f.LabelMap[i] = value
				if bits == 8 {
					native[i] = byte(value)
				} else {
					binary.LittleEndian.PutUint16(native[2*i:], value)
				}
			}
		}
		doc.Frames = append(doc.Frames, f)
		fragments = append(fragments, fixtureSEGStream(t, native, bits, syntax))
	}
	file, err := Write(doc)
	if err != nil {
		t.Fatal(err)
	}
	want, err := Read(file.Dataset)
	if err != nil {
		t.Fatal(err)
	}
	file.TransferSyntax = syntax
	file.Dataset.Put(core.Element{Header: core.ElementHeader{Tag: core.TagPixelData, VR: core.VROB, Length: core.UndefinedLength, LengthSet: true},
		Value: core.FragmentSequence{Fragments: fragments}})
	return file, want
}

func TestReadContextLosslessSEGExactAndSourceUnchanged(t *testing.T) {
	for _, tc := range []struct {
		kind string
		bits int
	}{
		{SegmentationTypeBinary, 1}, {SegmentationTypeFractional, 8}, {SegmentationTypeLabelMap, 8}, {SegmentationTypeLabelMap, 16},
	} {
		t.Run(fmt.Sprintf("%s-%d", tc.kind, tc.bits), func(t *testing.T) {
			file, want := compressedSEGFixture(t, tc.kind, 7, 5, 3, tc.bits)
			var encoded bytes.Buffer
			if err := object.WriteFile(&encoded, file); err != nil {
				t.Fatal(err)
			}
			parsed, err := object.ReadFile(bytes.NewReader(encoded.Bytes()))
			if err != nil {
				t.Fatal(err)
			}
			before := encoded.Bytes()
			got, err := ReadContext(context.Background(), parsed.Dataset, PixelDataReadOptions{TransferSyntax: parsed.TransferSyntax, Registry: compressedSEGRegistry(t)})
			if err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(got, want) {
				t.Fatalf("decoded document differs: got=%+v want=%+v", got, want)
			}
			var after bytes.Buffer
			if err := object.WriteFile(&after, parsed); err != nil {
				t.Fatal(err)
			}
			if !bytes.Equal(before, after.Bytes()) {
				t.Fatal("reading changed source file")
			}
			if _, err := Read(parsed.Dataset); !errors.Is(err, ErrUnsupportedPixelData) {
				t.Fatalf("legacy reader error=%v", err)
			}
			if tc.kind == SegmentationTypeFractional {
				got.Frames[0].Fractional[0] = 255
			}
			if tc.kind == SegmentationTypeLabelMap {
				got.Frames[0].LabelMap[0] = 65535
			}
		})
	}
}

func TestReadContextNativeBigEndianLabelMap16(t *testing.T) {
	_, want := compressedSEGFixture(t, SegmentationTypeLabelMap, 7, 5, 3, 16)
	file, err := Write(want)
	if err != nil {
		t.Fatal(err)
	}
	file.TransferSyntax = transfer.ExplicitVRBigEndian
	// Raw pixel bytes are already encoded values; WriteFile does not transcode
	// them when the syntax changes. Supply typed words for this BE fixture.
	var labels core.Uint16Value
	for _, frame := range want.Frames {
		labels = append(labels, frame.LabelMap...)
	}
	file.Dataset.Put(core.Element{Header: core.ElementHeader{Tag: core.TagPixelData, VR: core.VROW}, Value: labels})
	file.Dataset = object.FromElements(fixtureSEGElementsBigEndian(t, file.Dataset.Elements()), nil)
	file.Dataset.SetValueByteOrder(binary.BigEndian)
	var encoded bytes.Buffer
	if err := object.WriteFile(&encoded, file); err != nil {
		t.Fatal(err)
	}
	parsed, err := object.ReadFile(bytes.NewReader(encoded.Bytes()))
	if err != nil {
		t.Fatal(err)
	}
	pixel, ok := parsed.Dataset.Get(core.TagPixelData)
	raw, native := pixel.RawBytes()
	if !ok || !native || pixel.VR() != core.VROW || parsed.Dataset.ValueByteOrder() != binary.BigEndian {
		t.Fatal("fixture does not contain native Big Endian OW pixels")
	}
	// Build the byte oracle independently of the object writer and reader.
	var expected []byte
	for _, frame := range want.Frames {
		for _, label := range frame.LabelMap {
			expected = binary.BigEndian.AppendUint16(expected, label)
		}
	}
	if !bytes.Equal(raw, expected) {
		t.Fatal("Part 10 fixture does not encode the known labels in Big Endian")
	}
	before := append([]byte(nil), raw...)
	got, err := ReadContext(context.Background(), parsed.Dataset, PixelDataReadOptions{TransferSyntax: parsed.TransferSyntax})
	if err != nil || !reflect.DeepEqual(got, want) {
		t.Fatalf("Big Endian LABELMAP16 values/references differ: %v", err)
	}
	if !bytes.Equal(raw, before) || parsed.Dataset.ValueByteOrder() != binary.BigEndian {
		t.Fatal("reading mutated borrowed Big Endian pixels or their byte order")
	}
}

// The SEG writer authors raw Little Endian numeric attributes. Convert these
// fixture attributes explicitly; changing TransferSyntax alone is not a
// transcode operation. Strings, byte streams and typed words remain unchanged.
func fixtureSEGElementsBigEndian(t *testing.T, elements []core.Element) []core.Element {
	t.Helper()
	out := append([]core.Element(nil), elements...)
	for i, element := range out {
		if sequence, ok := element.Value.(core.SequenceValue); ok {
			items := make([]core.DataSet, len(sequence.Items))
			for j, item := range sequence.Items {
				items[j] = core.DataSet{Elements: fixtureSEGElementsBigEndian(t, item.Elements)}
			}
			element.Value = core.SequenceValue{Items: items}
			out[i] = element
			continue
		}
		width := 0
		switch element.VR() {
		case core.VRUS, core.VRSS, core.VRAT, core.VROW:
			width = 2
		case core.VRUL, core.VRSL, core.VRFL:
			width = 4
		case core.VRFD:
			width = 8
		}
		if raw, ok := element.RawBytes(); ok && width != 0 {
			if len(raw)%width != 0 {
				t.Fatalf("unaligned numeric fixture attribute %s", element.Tag())
			}
			converted := append([]byte(nil), raw...)
			for offset := 0; offset < len(converted); offset += width {
				for low, high := offset, offset+width-1; low < high; low, high = low+1, high-1 {
					converted[low], converted[high] = converted[high], converted[low]
				}
			}
			element.Value = core.RawValue(converted)
			out[i] = element
		}
	}
	return out
}

func TestReadContextRLEOccupancyAndPaletteLabels(t *testing.T) {
	t.Run("Occupancy", func(t *testing.T) {
		file, want := compressedSEGFixture(t, SegmentationTypeFractional, 7, 5, 3, 8)
		file.Dataset.Put(derivedio.CS(tagSegmentationFractionalType, FractionalTypeOccupancy))
		want.FractionalType = FractionalTypeOccupancy
		got, err := ReadContext(context.Background(), file.Dataset, PixelDataReadOptions{TransferSyntax: file.TransferSyntax, Registry: compressedSEGRegistry(t)})
		if err != nil || !reflect.DeepEqual(got, want) {
			t.Fatalf("OCCUPANCY values differ: %v", err)
		}
	})
	t.Run("Palette labels", func(t *testing.T) {
		file, want := compressedSEGFixture(t, SegmentationTypeLabelMap, 7, 5, 3, 16)
		file.Dataset.Put(derivedio.CS(core.NewTag(0x0028, 4), "PALETTE COLOR"))
		for _, element := range []uint16{0x1101, 0x1102, 0x1103} {
			file.Dataset.Put(core.Element{Header: core.ElementHeader{Tag: core.NewTag(0x0028, element), VR: core.VRUS}, Value: core.Uint16Value{514, 0, 16}})
		}
		for _, element := range []uint16{0x1201, 0x1202, 0x1203} {
			file.Dataset.Put(core.NewRawElement(core.NewTag(0x0028, element), core.VROW, make([]byte, 514*2)))
		}
		got, err := ReadContext(context.Background(), file.Dataset, PixelDataReadOptions{TransferSyntax: file.TransferSyntax, Registry: compressedSEGRegistry(t)})
		if err != nil || !reflect.DeepEqual(got, want) {
			t.Fatalf("palette color changed label values: %v", err)
		}
	})
}

// Embedding only Codec deliberately excludes its concrete DecodeContext method.
type legacySEGCodec struct{ pixeldata.Codec }

func TestReadContextRequiresContextCodecAndQualifiedCombinations(t *testing.T) {
	file, _ := compressedSEGFixture(t, SegmentationTypeBinary, 7, 5, 3, 1)
	r := pixeldata.NewMemoryRegistry()
	if err := r.RegisterCodec(file.TransferSyntax.UID, legacySEGCodec{Codec: deflateframe.New()}); err != nil {
		t.Fatal(err)
	}
	if got, err := ReadContext(context.Background(), file.Dataset, PixelDataReadOptions{TransferSyntax: file.TransferSyntax, Registry: r}); got != nil || !errors.Is(err, ErrUnsupportedPixelData) {
		t.Fatalf("non-context codec was accepted: %v", err)
	}
	for _, tc := range []struct {
		kind string
		bits int
		ts   transfer.Syntax
	}{
		{SegmentationTypeBinary, 1, transfer.RLELossless},
		{SegmentationTypeFractional, 8, transfer.DeflatedImageFrameCompression},
	} {
		file, _ := compressedSEGFixture(t, tc.kind, 7, 5, 3, tc.bits)
		if got, err := ReadContext(context.Background(), file.Dataset, PixelDataReadOptions{TransferSyntax: tc.ts, Registry: compressedSEGRegistry(t)}); got != nil || !errors.Is(err, ErrUnsupportedPixelData) {
			t.Fatalf("unqualified %s/%s was accepted: %v", tc.kind, tc.ts.UID, err)
		}
	}
}

func TestReadContextBinaryFramePackingAcrossByteBoundaries(t *testing.T) {
	for _, pixels := range []int{1, 3, 7, 8, 9, 15, 16, 35} {
		for _, frames := range []int{2, 3, 9} {
			t.Run(fmt.Sprintf("pixels%d-frames%d", pixels, frames), func(t *testing.T) {
				file, want := compressedSEGFixture(t, SegmentationTypeBinary, pixels, 1, frames, 1)
				got, err := ReadContext(context.Background(), file.Dataset, PixelDataReadOptions{TransferSyntax: file.TransferSyntax, Registry: compressedSEGRegistry(t)})
				if err != nil {
					t.Fatal(err)
				}
				if !reflect.DeepEqual(got, want) {
					t.Fatal("single-bit frames were misaligned")
				}
				// Native frames are contiguous bits, with only the entire OB
				// value padded to even length. For 9 pixels x 2 frames that
				// final pad coincides with a per-frame byte-aligned size.
				native, err := Write(want)
				if err != nil {
					t.Fatal(err)
				}
				var part10 bytes.Buffer
				if err := object.WriteFile(&part10, native); err != nil {
					t.Fatal(err)
				}
				parsed, err := object.ReadFile(bytes.NewReader(part10.Bytes()))
				if err != nil {
					t.Fatal(err)
				}
				got, err = ReadContext(context.Background(), parsed.Dataset, PixelDataReadOptions{TransferSyntax: parsed.TransferSyntax})
				if err != nil || !reflect.DeepEqual(got, want) {
					t.Fatalf("native continuous bits/padding differ: %v", err)
				}
			})
		}
	}
}

func setSEGOffsetTables(file *object.File, extended bool) {
	e, _ := file.Dataset.Get(core.TagPixelData)
	sequence := e.Value.(core.FragmentSequence)
	bot := make([]byte, 4*len(sequence.Fragments))
	offsets := make(core.Uint64Value, len(sequence.Fragments))
	lengths := make(core.Uint64Value, len(sequence.Fragments))
	var position uint64
	for i, f := range sequence.Fragments {
		binary.LittleEndian.PutUint32(bot[4*i:], uint32(position))
		offsets[i] = position
		lengths[i] = uint64(len(f))
		position += 8 + uint64(len(f))
	}
	if extended {
		file.Dataset.Put(core.Element{Header: core.ElementHeader{Tag: core.NewTag(0x7FE0, 1), VR: core.VROV}, Value: offsets})
		file.Dataset.Put(core.Element{Header: core.ElementHeader{Tag: core.NewTag(0x7FE0, 2), VR: core.VROV}, Value: lengths})
	} else {
		sequence.OffsetTable = bot
		e.Value = sequence
		file.Dataset.Put(e)
	}
}

func TestReadContextSEGOffsetTables(t *testing.T) {
	for _, kind := range []string{SegmentationTypeBinary, SegmentationTypeFractional, SegmentationTypeLabelMap} {
		for _, extended := range []bool{false, true} {
			file, want := compressedSEGFixture(t, kind, 7, 5, 3, map[bool]int{true: 1, false: 8}[kind == SegmentationTypeBinary])
			setSEGOffsetTables(file, extended)
			got, err := ReadContext(context.Background(), file.Dataset, PixelDataReadOptions{TransferSyntax: file.TransferSyntax, Registry: compressedSEGRegistry(t)})
			if err != nil || !reflect.DeepEqual(got, want) {
				t.Fatalf("kind%s EOT%t err%v", kind, extended, err)
			}
		}
	}
}

func TestReadContextSEGRejectsInvalidEncapsulation(t *testing.T) {
	tagEOT, tagLengths := core.NewTag(0x7FE0, 1), core.NewTag(0x7FE0, 2)
	tests := map[string]func(*object.File){
		"extra fragment": func(f *object.File) {
			e, _ := f.Dataset.Get(core.TagPixelData)
			s := e.Value.(core.FragmentSequence)
			s.Fragments = append(s.Fragments, s.Fragments[0])
			e.Value = s
			f.Dataset.Put(e)
		},
		"missing fragment": func(f *object.File) {
			e, _ := f.Dataset.Get(core.TagPixelData)
			s := e.Value.(core.FragmentSequence)
			s.Fragments = s.Fragments[:1]
			e.Value = s
			f.Dataset.Put(e)
		},
		"odd fragment": func(f *object.File) {
			e, _ := f.Dataset.Get(core.TagPixelData)
			s := e.Value.(core.FragmentSequence)
			s.Fragments[0] = s.Fragments[0][:len(s.Fragments[0])-1]
			e.Value = s
			f.Dataset.Put(e)
		},
		"empty fragment": func(f *object.File) {
			e, _ := f.Dataset.Get(core.TagPixelData)
			s := e.Value.(core.FragmentSequence)
			s.Fragments[0] = nil
			e.Value = s
			f.Dataset.Put(e)
		},
		"short BOT": func(f *object.File) {
			setSEGOffsetTables(f, false)
			e, _ := f.Dataset.Get(core.TagPixelData)
			s := e.Value.(core.FragmentSequence)
			s.OffsetTable = s.OffsetTable[:4]
			e.Value = s
			f.Dataset.Put(e)
		},
		"wrong BOT offset": func(f *object.File) {
			setSEGOffsetTables(f, false)
			e, _ := f.Dataset.Get(core.TagPixelData)
			s := e.Value.(core.FragmentSequence)
			s.OffsetTable[4]++
			e.Value = s
			f.Dataset.Put(e)
		},
		"BOT with EOT":        func(f *object.File) { setSEGOffsetTables(f, true); setSEGOffsetTables(f, false) },
		"missing EOT lengths": func(f *object.File) { setSEGOffsetTables(f, true); f.Dataset.Remove(tagLengths) },
		"lengths without EOT": func(f *object.File) { setSEGOffsetTables(f, true); f.Dataset.Remove(tagEOT) },
		"EOT wrong offset": func(f *object.File) {
			setSEGOffsetTables(f, true)
			f.Dataset.Put(core.Element{Header: core.ElementHeader{Tag: tagEOT, VR: core.VROV}, Value: core.Uint64Value{0, 1, 2}})
		},
		"EOT wrong length": func(f *object.File) {
			setSEGOffsetTables(f, true)
			f.Dataset.Put(core.Element{Header: core.ElementHeader{Tag: tagLengths, VR: core.VROV}, Value: core.Uint64Value{1, 1, 1}})
		},
		"EOT wrong VR": func(f *object.File) {
			setSEGOffsetTables(f, true)
			f.Dataset.Put(core.NewRawElement(tagEOT, core.VROB, make([]byte, 24)))
		},
	}
	for name, mutate := range tests {
		t.Run(name, func(t *testing.T) {
			file, _ := compressedSEGFixture(t, SegmentationTypeBinary, 7, 5, 3, 1)
			mutate(file)
			doc, err := ReadContext(context.Background(), file.Dataset, PixelDataReadOptions{TransferSyntax: file.TransferSyntax, Registry: compressedSEGRegistry(t)})
			if doc != nil || !errors.Is(err, ErrInvalidEncapsulation) {
				t.Fatalf("doc%v err%v", doc, err)
			}
		})
	}
}

type countedSEGCodec struct {
	codec pixeldata.ContextCodec
	calls atomic.Int64
}

func (c *countedSEGCodec) Decode(p pixeldata.PixelData, o *object.Object) (pixeldata.Frames, error) {
	return c.DecodeContext(context.Background(), p, o)
}
func (c *countedSEGCodec) DecodeContext(ctx context.Context, p pixeldata.PixelData, o *object.Object) (pixeldata.Frames, error) {
	c.calls.Add(1)
	return c.codec.DecodeContext(ctx, p, o)
}

func TestReadContextSEGLimitsBeforeDecode(t *testing.T) {
	for name, limits := range map[string]pixeldata.DecompressLimits{
		"frames": {MaxFrames: 2}, "pixels": {MaxPixels: 100}, "decoded": {MaxNativeBytes: 14}, "input": {MaxInputBytes: 1},
	} {
		t.Run(name, func(t *testing.T) {
			file, _ := compressedSEGFixture(t, SegmentationTypeBinary, 7, 5, 3, 1)
			codec := &countedSEGCodec{codec: deflateframe.New()}
			r := pixeldata.NewMemoryRegistry()
			if err := r.RegisterCodec(file.TransferSyntax.UID, codec); err != nil {
				t.Fatal(err)
			}
			doc, err := ReadContext(context.Background(), file.Dataset, PixelDataReadOptions{TransferSyntax: file.TransferSyntax, Registry: r, Limits: limits})
			if doc != nil || !errors.Is(err, ErrResourceLimitExceeded) || codec.calls.Load() != 0 {
				t.Fatalf("doc%v err%v calls%d", doc, err, codec.calls.Load())
			}
		})
	}
	file, _ := compressedSEGFixture(t, SegmentationTypeBinary, 7, 5, 3, 1)
	if _, err := ReadContext(context.Background(), file.Dataset, PixelDataReadOptions{TransferSyntax: file.TransferSyntax, Limits: pixeldata.DecompressLimits{MaxPixels: -1}}); !errors.Is(err, pixeldata.ErrInvalidDecompressLimits) {
		t.Fatal(err)
	}
}

func TestReadContextSEGProfileAndMetadataFailures(t *testing.T) {
	for name, mutate := range map[string]func(*object.File){
		"missing syntax":       func(f *object.File) { f.TransferSyntax = transfer.Syntax{} },
		"lossy syntax":         func(f *object.File) { f.TransferSyntax = transfer.JPEGBaseline },
		"unqualified lossless": func(f *object.File) { f.TransferSyntax = transfer.JPEGLSLossless },
		"wrong native syntax":  func(f *object.File) { f.TransferSyntax = transfer.ExplicitVRLittleEndian },
		"Bits Stored":          func(f *object.File) { f.Dataset.Put(derivedio.US(derivedio.TagBitsStored, 8)) },
		"High Bit":             func(f *object.File) { f.Dataset.Put(derivedio.US(derivedio.TagHighBit, 1)) },
		"Pixel Representation": func(f *object.File) { f.Dataset.Put(derivedio.US(derivedio.TagPixelRepresentation, 1)) },
		"Samples Per Pixel":    func(f *object.File) { f.Dataset.Put(derivedio.US(derivedio.TagSamplesPerPixel, 3)) },
		"photometric":          func(f *object.File) { f.Dataset.Put(derivedio.CS(core.NewTag(0x0028, 4), "MONOCHROME1")) },
	} {
		t.Run(name, func(t *testing.T) {
			file, _ := compressedSEGFixture(t, SegmentationTypeBinary, 7, 5, 3, 1)
			mutate(file)
			doc, err := ReadContext(context.Background(), file.Dataset, PixelDataReadOptions{TransferSyntax: file.TransferSyntax, Registry: compressedSEGRegistry(t)})
			if doc != nil || !errors.Is(err, ErrUnsupportedPixelData) {
				t.Fatalf("doc%v err%v", doc, err)
			}
		})
	}
	file, _ := compressedSEGFixture(t, SegmentationTypeBinary, 7, 5, 3, 1)
	for _, r := range []pixeldata.Registry{nil, pixeldata.NewMemoryRegistry()} {
		doc, err := ReadContext(context.Background(), file.Dataset, PixelDataReadOptions{TransferSyntax: file.TransferSyntax, Registry: r})
		if doc != nil || !errors.Is(err, ErrUnsupportedPixelData) {
			t.Fatalf("doc%v err%v", doc, err)
		}
	}
	file.Dataset.Put(derivedio.IS(derivedio.TagNumberOfFrames, 2))
	if doc, err := ReadContext(context.Background(), file.Dataset, PixelDataReadOptions{TransferSyntax: file.TransferSyntax, Registry: compressedSEGRegistry(t)}); doc != nil || !errors.Is(err, ErrInvalidObject) {
		t.Fatalf("doc%v err%v", doc, err)
	}
}

func TestReadContextSEGCancellationAndConcurrentDecode(t *testing.T) {
	file, want := compressedSEGFixture(t, SegmentationTypeBinary, 7, 5, 3, 1)
	r := compressedSEGRegistry(t)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if doc, err := ReadContext(ctx, file.Dataset, PixelDataReadOptions{TransferSyntax: file.TransferSyntax, Registry: r}); doc != nil || !errors.Is(err, context.Canceled) {
		t.Fatalf("doc%v err%v", doc, err)
	}
	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			doc, err := ReadContext(context.Background(), file.Dataset, PixelDataReadOptions{TransferSyntax: file.TransferSyntax, Registry: r})
			if err != nil || !reflect.DeepEqual(doc, want) {
				t.Errorf("concurrent doc differs: %v", err)
			}
		}()
	}
	wg.Wait()
}

type functionSEGCodec func(context.Context, pixeldata.PixelData, *object.Object) (pixeldata.Frames, error)

func (c functionSEGCodec) Decode(p pixeldata.PixelData, o *object.Object) (pixeldata.Frames, error) {
	return c(context.Background(), p, o)
}
func (c functionSEGCodec) DecodeContext(ctx context.Context, p pixeldata.PixelData, o *object.Object) (pixeldata.Frames, error) {
	return c(ctx, p, o)
}

func TestReadContextSEGCancelDuringDecoder(t *testing.T) {
	file, _ := compressedSEGFixture(t, SegmentationTypeBinary, 7, 5, 3, 1)
	ctx, cancel := context.WithCancel(context.Background())
	entered := make(chan struct{})
	r := pixeldata.NewMemoryRegistry()
	if err := r.RegisterCodec(file.TransferSyntax.UID, functionSEGCodec(func(ctx context.Context, _ pixeldata.PixelData, _ *object.Object) (pixeldata.Frames, error) {
		close(entered)
		<-ctx.Done()
		return pixeldata.Frames{}, ctx.Err()
	})); err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() {
		doc, err := ReadContext(ctx, file.Dataset, PixelDataReadOptions{TransferSyntax: file.TransferSyntax, Registry: r})
		if doc != nil {
			done <- errors.New("published partial document")
			return
		}
		done <- err
	}()
	<-entered
	cancel()
	if err := <-done; !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
}

func TestReadContextSEGBadDecoderOutputAndTruncatedStream(t *testing.T) {
	for name, output := range map[string]pixeldata.Frames{
		"dimensions": {Rows: 4, Columns: 7, Data: make([][]byte, 3)},
		"count":      {Rows: 5, Columns: 7, Data: make([][]byte, 2)},
		"short":      {Rows: 5, Columns: 7, Data: [][]byte{{1}, {1}, {1}}},
		"long":       {Rows: 5, Columns: 7, Data: [][]byte{make([]byte, 6), make([]byte, 6), make([]byte, 6)}},
	} {
		t.Run(name, func(t *testing.T) {
			file, _ := compressedSEGFixture(t, SegmentationTypeBinary, 7, 5, 3, 1)
			r := pixeldata.NewMemoryRegistry()
			if err := r.RegisterCodec(file.TransferSyntax.UID, functionSEGCodec(func(context.Context, pixeldata.PixelData, *object.Object) (pixeldata.Frames, error) {
				return output, nil
			})); err != nil {
				t.Fatal(err)
			}
			doc, err := ReadContext(context.Background(), file.Dataset, PixelDataReadOptions{TransferSyntax: file.TransferSyntax, Registry: r})
			if doc != nil || !errors.Is(err, pixeldata.ErrPixelDataSizeMismatch) {
				t.Fatalf("doc%v err%v", doc, err)
			}
		})
	}
	for _, kind := range []string{SegmentationTypeBinary, SegmentationTypeFractional} {
		bits := 8
		if kind == SegmentationTypeBinary {
			bits = 1
		}
		file, _ := compressedSEGFixture(t, kind, 7, 5, 3, bits)
		e, _ := file.Dataset.Get(core.TagPixelData)
		s := e.Value.(core.FragmentSequence)
		s.Fragments[0] = s.Fragments[0][:2]
		e.Value = s
		file.Dataset.Put(e)
		doc, err := ReadContext(context.Background(), file.Dataset, PixelDataReadOptions{TransferSyntax: file.TransferSyntax, Registry: compressedSEGRegistry(t)})
		if doc != nil || err == nil {
			t.Fatalf("%s returned partial segmentation", kind)
		}
	}
}

func TestReadContextSEGExpansionAndHostileDimensions(t *testing.T) {
	file, _ := compressedSEGFixture(t, SegmentationTypeBinary, 4096, 8, 1, 1)
	codec := &countedSEGCodec{codec: deflateframe.New()}
	r := pixeldata.NewMemoryRegistry()
	if err := r.RegisterCodec(file.TransferSyntax.UID, codec); err != nil {
		t.Fatal(err)
	}
	if doc, err := ReadContext(context.Background(), file.Dataset, PixelDataReadOptions{TransferSyntax: file.TransferSyntax, Registry: r, Limits: pixeldata.DecompressLimits{MaxExpansionRatio: 1}}); doc != nil || !errors.Is(err, ErrResourceLimitExceeded) || codec.calls.Load() != 0 {
		t.Fatalf("doc%v err%v calls%d", doc, err, codec.calls.Load())
	}
	file.Dataset.Put(derivedio.US(derivedio.TagRows, 65535))
	file.Dataset.Put(derivedio.US(derivedio.TagColumns, 65535))
	if doc, err := ReadContext(context.Background(), file.Dataset, PixelDataReadOptions{TransferSyntax: file.TransferSyntax, Registry: r}); doc != nil || !errors.Is(err, ErrResourceLimitExceeded) || codec.calls.Load() != 0 {
		t.Fatalf("hostile doc%v err%v calls%d", doc, err, codec.calls.Load())
	}
}

func TestReadContextSEGDeflatedEOTExcludesItemPadding(t *testing.T) {
	file, want := compressedSEGFixture(t, SegmentationTypeBinary, 7, 5, 3, 1)
	setSEGOffsetTables(file, true)
	e, _ := file.Dataset.Get(core.TagPixelData)
	s := e.Value.(core.FragmentSequence)
	lengths := make(core.Uint64Value, len(s.Fragments))
	for i, fragment := range s.Fragments {
		b := bytes.NewReader(fragment)
		r := flate.NewReader(b)
		if _, err := io.Copy(io.Discard, r); err != nil {
			t.Fatal(err)
		}
		if err := r.Close(); err != nil {
			t.Fatal(err)
		}
		lengths[i] = uint64(len(fragment) - b.Len())
	}
	file.Dataset.Put(core.Element{Header: core.ElementHeader{Tag: core.NewTag(0x7FE0, 2), VR: core.VROV}, Value: lengths})
	doc, err := ReadContext(context.Background(), file.Dataset, PixelDataReadOptions{TransferSyntax: file.TransferSyntax, Registry: compressedSEGRegistry(t)})
	if err != nil || !reflect.DeepEqual(doc, want) {
		t.Fatalf("doc differs err%v", err)
	}
}

type checkpointSEGContext struct {
	context.Context
	checks atomic.Int64
}

func (c *checkpointSEGContext) Err() error {
	if c.checks.Add(1) > 4 {
		return context.Canceled
	}
	return nil
}

func TestSEGCancellationDuringNativeMaterialization(t *testing.T) {
	file, _ := compressedSEGFixture(t, SegmentationTypeFractional, 4096, 2, 3, 8)
	// Restore known native samples to exercise checks within a frame, after
	// decoding rather than relying only on the initial canceled-context gate.
	file.Dataset.Put(core.NewRawElement(core.TagPixelData, core.VROB, make([]byte, 4096*2*3)))
	header, err := readDocumentHeader(file.Dataset)
	if err != nil {
		t.Fatal(err)
	}
	ctx := &checkpointSEGContext{Context: context.Background()}
	frames, _, err := readFramesContext(ctx, file.Dataset, header)
	if frames != nil || !errors.Is(err, context.Canceled) {
		t.Fatalf("frames%v err%v", frames, err)
	}
}

func TestReadContextSEGDoesNotInheritRLERecovery(t *testing.T) {
	file, _ := compressedSEGFixture(t, SegmentationTypeFractional, 7, 5, 3, 8)
	e, _ := file.Dataset.Get(core.TagPixelData)
	s := e.Value.(core.FragmentSequence)
	s.Fragments[0] = s.Fragments[0][:len(s.Fragments[0])-2]
	e.Value = s
	file.Dataset.Put(e)
	ctx := pixeldata.DecodeContextWithRecovery(context.Background(), pixeldata.DecodeRecovery{RLESegmentOverrun: true})
	doc, err := ReadContext(ctx, file.Dataset, PixelDataReadOptions{TransferSyntax: file.TransferSyntax, Registry: compressedSEGRegistry(t)})
	if doc != nil || !errors.Is(err, rle.ErrSegmentDecodeFailed) {
		t.Fatalf("doc%v err%v", doc, err)
	}
}

func TestReadContextSEGFractionalAndLabelValuesRemainStrict(t *testing.T) {
	for _, kind := range []string{SegmentationTypeFractional, SegmentationTypeLabelMap} {
		file, _ := compressedSEGFixture(t, kind, 7, 5, 3, 8)
		m, err := pixeldata.ExtractMetadata(file.Dataset)
		if err != nil {
			t.Fatal(err)
		}
		native := make([]byte, int(m.FrameSize()))
		native[0] = 201
		e, _ := file.Dataset.Get(core.TagPixelData)
		s := e.Value.(core.FragmentSequence)
		s.Fragments[0] = fixtureSEGStream(t, native, 8, file.TransferSyntax)
		e.Value = s
		file.Dataset.Put(e)
		doc, err := ReadContext(context.Background(), file.Dataset, PixelDataReadOptions{TransferSyntax: file.TransferSyntax, Registry: compressedSEGRegistry(t)})
		if doc != nil || !errors.Is(err, ErrInvalidObject) {
			t.Fatalf("%s doc%v err%v", kind, doc, err)
		}
	}
}
