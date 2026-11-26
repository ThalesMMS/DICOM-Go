package hologic

import (
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"io"
	"path/filepath"
	"testing"

	"github.com/ThalesMMS/dicom-go/core"
	"github.com/ThalesMMS/dicom-go/dictionary/tags"
	"github.com/ThalesMMS/dicom-go/object"
	"github.com/ThalesMMS/dicom-go/parser"
	"github.com/ThalesMMS/dicom-go/pixeldata"
	"github.com/ThalesMMS/dicom-go/pixeldata/encapsulated"
	"github.com/ThalesMMS/dicom-go/pixeldata/jpegls"
	"github.com/ThalesMMS/dicom-go/transfer"
)

func fixture(t *testing.T) (blocks [][]byte, frames [][]byte) {
	t.Helper()
	metadata := pixeldata.Metadata{
		Rows: 3, Columns: 2, NumberOfFrames: 1, SamplesPerPixel: 1,
		BitsAllocated: 16, BitsStored: 16, HighBit: 15, PhotometricInterpretation: "MONOCHROME2",
	}
	frames = [][]byte{make([]byte, 12), make([]byte, 12)}
	var streams [][]byte
	for i, frame := range frames {
		for sample := 0; sample < len(frame)/2; sample++ {
			binary.LittleEndian.PutUint16(frame[sample*2:], uint16(sample*2177+i*2791+16))
		}
		encoded, err := jpegls.NewEncoder().EncodeFrame(context.Background(), frame, metadata)
		if err != nil {
			t.Fatal(err)
		}
		streams = append(streams, fixtureEntropy(t, encoded.Data))
	}
	return makeFixtureBlocks(metadata, streams), frames
}

func fixtureEntropy(t *testing.T, stream []byte) []byte {
	t.Helper()
	if _, padded := encapsulated.TrailingPad(stream); padded {
		stream = stream[:len(stream)-1]
	}
	sos := bytes.Index(stream, []byte{0xff, 0xda})
	if sos < 0 || sos+4 >= len(stream) || !bytes.HasSuffix(stream, []byte{0xff, 0xd9}) {
		t.Fatal("fixture entropy not found")
	}
	start := sos + 2 + int(binary.BigEndian.Uint16(stream[sos+2:sos+4]))
	if start >= len(stream)-4 {
		t.Fatal("fixture needs entropy across the second block")
	}
	return stream[start : len(stream)-2]
}

func makeFixtureBlocks(metadata pixeldata.Metadata, streams [][]byte) [][]byte {
	payload := make([]byte, 40+24*len(streams))
	binary.LittleEndian.PutUint16(payload[20:], uint16(len(streams)))
	binary.LittleEndian.PutUint16(payload[24:], metadata.Columns)
	binary.LittleEndian.PutUint16(payload[28:], metadata.Rows)
	payload[32] = byte(metadata.BitsStored)
	payload[36] = 0
	var tail [trailerSize]byte
	binary.LittleEndian.PutUint32(tail[:], uint32(len(payload)))
	split := len(payload) + 2
	for i, stream := range streams {
		payload = append(payload, stream...)
		binary.LittleEndian.PutUint32(tail[4*(i+1):], uint32(len(payload)))
	}
	if len(payload)%2 != 0 {
		payload = append(payload, 0) // Storage pad outside the entropy extents.
	}
	payload = append(payload, tail[:]...)
	return [][]byte{payload[:split], payload[split:]}
}

func fileWithBlocks(t *testing.T, blocks [][]byte) *object.File {
	t.Helper()
	dataset := object.New(nil)
	for _, text := range []struct {
		tag   core.Tag
		vr    core.VR
		value string
	}{
		{creatorTag, core.VRLO, creator},
		{tags.SOPClassUID, core.VRUI, "1.2.840.10008.5.1.4.1.1.7"},
		{tags.SOPInstanceUID, core.VRUI, "2.25.1736"},
	} {
		if err := dataset.SetStrings(text.tag, text.vr, text.value); err != nil {
			t.Fatal(err)
		}
	}
	setFileBlocks(dataset, blocks)
	return &object.File{Dataset: dataset, TransferSyntax: transfer.ExplicitVRLittleEndian}
}

func setFileBlocks(dataset *object.Object, blocks [][]byte) {
	items := make([]*object.Object, len(blocks))
	for i, data := range blocks {
		items[i] = object.FromElements([]core.Element{core.NewRawElement(blockTag, core.VROB, data)}, nil)
	}
	dataset.SetSequence(sequenceTag, items...)
}

func deferredFile(t *testing.T, in *object.File) (*object.File, *bytes.Reader, []byte) {
	t.Helper()
	var wire bytes.Buffer
	if err := object.WriteFile(&wire, in); err != nil {
		t.Fatal(err)
	}
	reader := bytes.NewReader(wire.Bytes())
	out, err := object.ReadFileWithOptions(reader, object.ReadFileOptions{
		InlineValueBytesThreshold: 1, DeferNestedValues: true, DeferValueTags: []core.Tag{BlockValueTag()},
	})
	if err != nil {
		t.Fatal(err)
	}
	items, _ := out.Dataset.GetSequence(sequenceTag)
	for _, item := range items {
		elem, _ := item.Get(blockTag)
		if elem.Value != nil {
			t.Fatal("test profile block materialized")
		}
	}
	return out, reader, wire.Bytes()
}

func TestOpenAndDecodeRangesAcrossPrivateBlocks(t *testing.T) {
	blocks, expected := fixture(t)
	for _, deferred := range []bool{false, true} {
		file := fileWithBlocks(t, blocks)
		if deferred {
			file, _, _ = deferredFile(t, file)
		}
		source, err := Open(context.Background(), file)
		if err != nil {
			t.Fatal(err)
		}
		meta := source.Metadata()
		if meta.Rows != 3 || meta.Columns != 2 || meta.SamplesPerPixel != 1 || meta.NumberOfFrames != 2 ||
			meta.BitsAllocated != 16 || meta.BitsStored != 16 || meta.HighBit != 15 || meta.PixelRepresentation != 0 ||
			meta.PhotometricInterpretation != "MONOCHROME2" {
			t.Fatalf("wrong projection: %+v", meta)
		}
		for _, index := range []int{1, 0, 1, 0} {
			pixels, err := source.Frame(context.Background(), index)
			if err != nil || !bytes.Equal(pixels, expected[index]) {
				t.Fatalf("deferred=%t frame=%d: pixels=%x err=%v", deferred, index, pixels, err)
			}
			pixels[0] ^= 0xff
		}
	}
}

func TestPrivateWrapperDecodesIndependentJPEGLSEntropy(t *testing.T) {
	root := filepath.Join("..", "codecfixture", "testdata", "codecfull", "pydicom")
	ctx := context.Background()
	input, err := pixeldata.OpenFrameReader(ctx, filepath.Join(root, "MR_small_jpeg_ls_lossless.dcm"), pixeldata.FrameReaderOptions{})
	if err != nil {
		t.Fatal(err)
	}
	defer input.Close()
	raw, err := input.RawFrame(ctx, 0)
	if err != nil {
		t.Fatal(err)
	}
	frame := fixtureEntropy(t, raw)
	native, err := object.OpenFile(filepath.Join(root, "MR_small.dcm"))
	if err != nil {
		t.Fatal(err)
	}
	defer native.Close()
	expected, err := pixeldata.ExtractNativeFramesView(native.Dataset)
	if err != nil {
		t.Fatal(err)
	}
	blocks := makeFixtureBlocks(input.Metadata(), [][]byte{frame})
	private := fileWithBlocks(t, blocks)
	source, err := Open(ctx, private)
	if err != nil {
		t.Fatal(err)
	}
	result, err := source.Frame(ctx, 0)
	if err != nil || !bytes.Equal(result, expected.Data[0]) {
		t.Fatalf("independently encoded entropy differs: %v", err)
	}
}

func TestPrivateProfileAdmissionAndOffsets(t *testing.T) {
	for _, tc := range []struct {
		name  string
		alter func(*testing.T, [][]byte, *object.File)
		want  error
	}{
		{"other creator", func(t *testing.T, _ [][]byte, f *object.File) {
			if err := f.Dataset.SetStrings(creatorTag, core.VRLO, "OTHER"); err != nil {
				t.Fatal(err)
			}
		}, ErrNotProfile},
		{"ordinary DICOM takes precedence", func(_ *testing.T, _ [][]byte, f *object.File) {
			f.Dataset.Put(core.NewRawElement(core.TagPixelData, core.VROW, []byte{1, 0}))
		}, ErrNotProfile},
		{"standard floating pixel data takes precedence", func(_ *testing.T, _ [][]byte, f *object.File) {
			f.Dataset.Put(core.NewRawElement(floatPixelTag, core.VROF, []byte{0, 0, 0, 0}))
		}, ErrNotProfile},
		{"other syntax", func(_ *testing.T, _ [][]byte, f *object.File) {
			f.TransferSyntax = transfer.ExplicitVRBigEndian
		}, ErrInvalidProfile},
		{"item reserves a different creator", func(t *testing.T, chunks [][]byte, f *object.File) {
			b := object.FromElements([]core.Element{core.NewRawElement(blockTag, core.VROB, chunks[1])}, nil)
			if err := b.SetStrings(creatorTag, core.VRLO, "OTHER"); err != nil {
				t.Fatal(err)
			}
			f.Dataset.SetSequence(sequenceTag, object.FromElements([]core.Element{core.NewRawElement(blockTag, core.VROB, chunks[0])}, nil), b)
		}, ErrInvalidProfile},
		{"conflicting dimensions", func(_ *testing.T, _ [][]byte, f *object.File) {
			f.Dataset.SetUS(tags.Rows, 4)
		}, ErrInvalidProfile},
		{"conflicting precision", func(_ *testing.T, _ [][]byte, f *object.File) {
			f.Dataset.SetUS(tags.BitsStored, 12)
		}, ErrInvalidProfile},
		{"unqualified NEAR", func(_ *testing.T, chunks [][]byte, f *object.File) {
			chunks[0][36] = 1
			setFileBlocks(f.Dataset, chunks)
		}, ErrInvalidProfile},
		{"frame count overflow", func(_ *testing.T, chunks [][]byte, f *object.File) {
			binary.LittleEndian.PutUint16(chunks[0][20:], 256)
			setFileBlocks(f.Dataset, chunks)
		}, ErrInvalidProfile},
		{"truncated blocks", func(_ *testing.T, chunks [][]byte, f *object.File) {
			f.Dataset.SetSequence(sequenceTag, object.FromElements([]core.Element{core.NewRawElement(blockTag, core.VROB, chunks[0])}, nil))
		}, ErrInvalidProfile},
		{"offset into header", func(_ *testing.T, chunks [][]byte, f *object.File) {
			tail := chunks[1][len(chunks[1])-trailerSize:]
			binary.LittleEndian.PutUint32(tail, 36)
			setFileBlocks(f.Dataset, chunks)
		}, ErrInvalidProfile},
		{"descending offsets", func(_ *testing.T, chunks [][]byte, f *object.File) {
			tail := chunks[1][len(chunks[1])-trailerSize:]
			copy(tail[4:], tail[:4])
			setFileBlocks(f.Dataset, chunks)
		}, ErrInvalidProfile},
		{"frame overlaps offset table", func(_ *testing.T, chunks [][]byte, f *object.File) {
			tail := chunks[1][len(chunks[1])-trailerSize:]
			binary.LittleEndian.PutUint32(tail[8:], uint32(len(chunks[0])+len(chunks[1])))
			setFileBlocks(f.Dataset, chunks)
		}, ErrInvalidProfile},
	} {
		t.Run(tc.name, func(t *testing.T) {
			chunks, _ := fixture(t)
			file := fileWithBlocks(t, chunks)
			tc.alter(t, chunks, file)
			got, err := Open(context.Background(), file)
			if got != nil || !errors.Is(err, tc.want) {
				t.Fatalf("open returned source=%t, error=%v, want %v", got != nil, err, tc.want)
			}
		})
	}
}

func TestCandidateNeedsThePrivateSequence(t *testing.T) {
	blocks, _ := fixture(t)
	file := fileWithBlocks(t, blocks)
	if !IsCandidate(file) {
		t.Fatal("valid reservation and sequence not selected")
	}
	file.Dataset.Remove(sequenceTag)
	if IsCandidate(file) {
		t.Fatal("an unrelated object reserved the same private creator")
	}
	if _, err := Open(context.Background(), file); !errors.Is(err, ErrInvalidProfile) {
		t.Fatalf("direct admission of missing private sequence: %v", err)
	}
}

func TestPrivateFrameRefusesMalformedTruncatedAndCanceledReads(t *testing.T) {
	blocks, _ := fixture(t)
	blocks[0][len(blocks[0])-2], blocks[0][len(blocks[0])-1] = 0xff, 0xd8
	bad, err := Open(context.Background(), fileWithBlocks(t, blocks))
	if err != nil {
		t.Fatal(err)
	}
	if pixels, err := bad.Frame(context.Background(), 0); pixels != nil || !errors.Is(err, ErrInvalidProfile) {
		t.Fatalf("JPEG marker in raw entropy returned %x, %v", pixels, err)
	}
	blocks, _ = fixture(t)
	file, reader, raw := deferredFile(t, fileWithBlocks(t, blocks))
	opened, err := Open(context.Background(), file)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := Open(ctx, file); !errors.Is(err, context.Canceled) {
		t.Fatalf("canceled index: %v", err)
	}
	if pixels, err := opened.Frame(ctx, 0); pixels != nil || !errors.Is(err, context.Canceled) {
		t.Fatalf("canceled frame returned %x, %v", pixels, err)
	}
	ref, ok, err := file.Dataset.DeferredBulkDataReference([]parser.BulkDataPathStep{{Tag: sequenceTag, ItemIndex: 1}, {Tag: blockTag, ItemIndex: -1}})
	if err != nil || !ok {
		t.Fatalf("private block extent: %v, found=%t", err, ok)
	}
	reader.Reset(raw[:ref.Offset+1])
	if pixels, err := opened.Frame(context.Background(), 0); pixels != nil || !errors.Is(err, io.EOF) {
		t.Fatalf("truncated source returned %x, %v", pixels, err)
	}
}
