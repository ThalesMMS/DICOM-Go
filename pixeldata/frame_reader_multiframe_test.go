package pixeldata_test

import (
	"bytes"
	"context"
	"encoding/binary"
	"reflect"
	"testing"

	"github.com/ThalesMMS/dicom-go/core"
	"github.com/ThalesMMS/dicom-go/object"
	"github.com/ThalesMMS/dicom-go/pixeldata"
	"github.com/ThalesMMS/dicom-go/transfer"
)

// frameReaderObjectBaseline preserves the pre-#1308 implementation: index
// once, build a full one-frame proxy, decompress a dataset, clone Pixel Data,
// and extract the decoded metadata on every Frame call. Setup is not timed.
type frameReaderObjectBaseline struct {
	reader   *pixeldata.FrameReader
	proxy    []core.Element
	pixelVR  core.VR
	registry pixeldata.Registry
}

func newFrameReaderObjectBaseline(reader *pixeldata.FrameReader, registry pixeldata.Registry) *frameReaderObjectBaseline {
	b := &frameReaderObjectBaseline{reader: reader, registry: registry}
	for _, element := range reader.File().Dataset.Elements() {
		switch element.Tag() {
		case core.TagPixelData:
			b.pixelVR = element.VR()
			continue
		case decompressTagExtendedOffsetTable, decompressTagExtendedOffsetTableLength, decompressTagEncapsulatedValueLength:
			continue
		case decompressTagNumberOfFrames:
			element = core.Element{Header: core.ElementHeader{Tag: decompressTagNumberOfFrames, VR: core.VRIS}, Value: core.StringValue{"1"}}
		}
		b.proxy = append(b.proxy, element)
	}
	return b
}

func (b *frameReaderObjectBaseline) Frame(ctx context.Context, index int) ([]byte, pixeldata.Metadata, error) {
	frame, err := b.reader.RawFrame(ctx, index)
	if err != nil {
		return nil, pixeldata.Metadata{}, err
	}
	pixel := core.NewRawElement(core.TagPixelData, b.pixelVR, frame)
	syntax := b.reader.TransferSyntax()
	if syntax.Encapsulated {
		pixel = core.Element{Header: core.ElementHeader{Tag: core.TagPixelData, VR: core.VROB, Length: core.UndefinedLength, LengthSet: true},
			Value: core.FragmentSequence{Fragments: [][]byte{frame}}}
	}
	proxy := object.FromElements(append(append(make([]core.Element, 0, len(b.proxy)+1), b.proxy...), pixel), nil)
	order := binary.ByteOrder(binary.LittleEndian)
	if syntax.ByteOrder == binary.BigEndian {
		order = binary.BigEndian
	}
	proxy.SetValueByteOrder(order)
	decoded, _, err := pixeldata.DecompressDataSetContext(ctx, proxy, syntax, pixeldata.DecompressOptions{
		Registry: b.registry, TargetTransferSyntax: transfer.ExplicitVRLittleEndian,
		Limits: pixeldata.DecompressLimits{MaxFrames: 1},
	})
	if err != nil {
		return nil, pixeldata.Metadata{}, err
	}
	raw, ok := decoded.GetRaw(core.TagPixelData)
	if !ok {
		return nil, pixeldata.Metadata{}, pixeldata.ErrPixelDataNotFound
	}
	metadata, err := pixeldata.ExtractMetadata(decoded)
	return raw, metadata, err
}

func frameReaderMultiframeFixture(t testing.TB, target transfer.Syntax, frames, samples int) (*pixeldata.FrameReader, *frameReaderObjectBaseline) {
	t.Helper()
	options := streamTestRegistries(t)
	source := streamTestSource(64, 64, frames, samples, 16)
	// Many non-pixel attributes make rebuilding the full facade measurable.
	for i := 0; i < 256; i++ {
		source.Put(core.NewRawElement(core.NewTag(0x7776, uint16(i)), core.VRLO, []byte("synthetic metadata")))
	}
	file, _, err := pixeldata.TranscodeFile(context.Background(), &object.File{Dataset: source, TransferSyntax: transfer.ExplicitVRLittleEndian}, target, options)
	if err != nil {
		t.Fatal(err)
	}
	reader, err := pixeldata.NewFrameReader(context.Background(), file, nil, pixeldata.FrameReaderOptions{Registry: options.DecoderRegistry})
	if err != nil {
		t.Fatal(err)
	}
	return reader, newFrameReaderObjectBaseline(reader, options.DecoderRegistry)
}

func TestFrameReaderDirectMultiframeByteExact(t *testing.T) {
	for _, tc := range []struct {
		name    string
		syntax  transfer.Syntax
		samples int
	}{
		{"rle16", transfer.RLELossless, 1},
		{"rle_rgb16", transfer.RLELossless, 3},
		{"jls16", transfer.JPEGLSLossless, 1},
		{"bigendian16", transfer.ExplicitVRBigEndian, 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			reader, baseline := frameReaderMultiframeFixture(t, tc.syntax, 128, tc.samples)
			for i := 0; i < reader.Metadata().NumberOfFrames; i++ {
				want, wm, err := baseline.Frame(context.Background(), i)
				if err != nil {
					t.Fatal(err)
				}
				got, gm, err := reader.Frame(context.Background(), i)
				if err != nil || !bytes.Equal(got, want) || !reflect.DeepEqual(gm, wm) {
					t.Fatalf("frame %d differs: bytes=%t metadata=%#v want=%#v err=%v", i, bytes.Equal(got, want), gm, wm, err)
				}
			}
		})
	}
}

// BenchmarkFrameReaderDirectMultiframe times a decoded frame in a 128-frame
// series. Both paths use the same indexed reader, registry, codec and input;
// only the old full-object pipeline versus Frame's direct pipeline differs.
// This measures a warmed reader, not indexing, file I/O or end-to-end viewing.
func BenchmarkFrameReaderDirectMultiframe(b *testing.B) {
	for _, tc := range []struct {
		name   string
		syntax transfer.Syntax
	}{
		{"rle16_128frames", transfer.RLELossless},
		{"jls16_128frames", transfer.JPEGLSLossless},
		{"bigendian16_128frames", transfer.ExplicitVRBigEndian},
	} {
		reader, baseline := frameReaderMultiframeFixture(b, tc.syntax, 128, 1)
		for _, path := range []struct {
			name  string
			frame func(context.Context, int) ([]byte, pixeldata.Metadata, error)
		}{
			{"object_baseline", baseline.Frame},
			{"direct", reader.Frame},
		} {
			b.Run(tc.name+"/"+path.name, func(b *testing.B) {
				if _, _, err := path.frame(context.Background(), 0); err != nil {
					b.Fatal(err)
				}
				b.ReportAllocs()
				b.SetBytes(reader.Metadata().FrameSize())
				b.ResetTimer()
				for i := 0; i < b.N; i++ {
					if _, _, err := path.frame(context.Background(), i%reader.Metadata().NumberOfFrames); err != nil {
						b.Fatal(err)
					}
				}
			})
		}
	}
}
