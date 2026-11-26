package pixeldata_test

import (
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"github.com/ThalesMMS/dicom-go/pixeldata/jpeg"
	"github.com/ThalesMMS/dicom-go/pixeldata/jpegls"
	"image"
	"image/color"
	stdjpeg "image/jpeg"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"testing"

	"github.com/ThalesMMS/dicom-go/core"
	"github.com/ThalesMMS/dicom-go/object"
	"github.com/ThalesMMS/dicom-go/pixeldata"
	"github.com/ThalesMMS/dicom-go/pixeldata/display"
	"github.com/ThalesMMS/dicom-go/render"
	"github.com/ThalesMMS/dicom-go/transfer"
)

func incompleteNativeFile(t *testing.T, syntax transfer.Syntax, present int) []byte {
	t.Helper()
	obj := object.New(nil)
	obj.Put(core.Element{Header: core.ElementHeader{Tag: core.NewTag(0x0008, 0x0016), VR: core.VRUI}, Value: core.StringValue{"1.2.840.10008.5.1.4.1.1.7"}})
	obj.Put(core.Element{Header: core.ElementHeader{Tag: core.NewTag(0x0008, 0x0018), VR: core.VRUI}, Value: core.StringValue{"1.2.826.0.1.3680043.10.1312.1"}})
	putUS := func(tag core.Tag, n uint16) {
		b := make([]byte, 2)
		syntax.ByteOrder.PutUint16(b, n)
		obj.Put(core.NewRawElement(tag, core.VRUS, b))
	}
	putUS(core.NewTag(0x0028, 0x0010), 1)
	putUS(core.NewTag(0x0028, 0x0011), 2)
	putUS(core.NewTag(0x0028, 0x0002), 1)
	putUS(core.NewTag(0x0028, 0x0100), 16)
	putUS(core.NewTag(0x0028, 0x0101), 16)
	putUS(core.NewTag(0x0028, 0x0102), 15)
	putUS(core.NewTag(0x0028, 0x0103), 0)
	obj.Put(core.Element{Header: core.ElementHeader{Tag: core.NewTag(0x0028, 0x0004), VR: core.VRCS}, Value: core.StringValue{"MONOCHROME2"}})
	obj.Put(core.Element{Header: core.ElementHeader{Tag: core.NewTag(0x0028, 0x0008), VR: core.VRIS}, Value: core.StringValue{"3"}})
	raw := make([]byte, 12)
	for i, n := range []uint16{1, 0x1234, 0x4567, 0xfedc, 0x8888, 0x9999} {
		syntax.ByteOrder.PutUint16(raw[2*i:], n)
	}
	obj.Put(core.NewRawElement(core.TagPixelData, core.VROW, raw))
	obj.SetValueByteOrder(syntax.ByteOrder)
	var b bytes.Buffer
	if err := object.WriteFile(&b, &object.File{Dataset: obj, TransferSyntax: syntax}); err != nil {
		t.Fatal(err)
	}
	return bytes.Clone(b.Bytes()[:b.Len()-(12-present)])
}

func TestIncompletePixelDataNativePrefix(t *testing.T) {
	for _, syntax := range []transfer.Syntax{transfer.ExplicitVRLittleEndian, transfer.ImplicitVRLittleEndian, transfer.ExplicitVRBigEndian} {
		t.Run(syntax.UID, func(t *testing.T) {
			data := incompleteNativeFile(t, syntax, 9)
			if f, err := object.ReadFile(bytes.NewReader(data)); f != nil || !errors.Is(err, io.ErrUnexpectedEOF) {
				t.Fatalf("strict f=%v err=%v", f, err)
			}
			f, err := object.ReadFileWithOptions(bytes.NewReader(data), object.ReadFileOptions{AllowIncompletePixelData: true})
			if f == nil || !errors.Is(err, object.ErrPartialRead) || !errors.Is(err, io.ErrUnexpectedEOF) {
				t.Fatalf("read f=%v err=%v", f, err)
			}
			raw, _ := f.Dataset.Get(core.TagPixelData)
			source, _ := raw.RawBytes()
			if len(source) != 9 {
				t.Fatalf("present=%d", len(source))
			}
			info, ok := f.Dataset.IncompletePixelData()
			if !ok || info.AvailableBytes != 9 {
				t.Fatalf("report=%+v %v", info, ok)
			}
			if _, err := pixeldata.DecompressFile(f, pixeldata.DecompressOptions{}); !errors.Is(err, object.ErrPartialFile) {
				t.Fatalf("strict decode=%v", err)
			}
			var report pixeldata.DecompressReport
			out, err := pixeldata.DecompressFileContext(context.Background(), f, pixeldata.DecompressOptions{Recovery: pixeldata.DecodeRecovery{IncompleteData: true}, Report: &report})
			if err != nil {
				t.Fatal(err)
			}
			n, _ := out.Dataset.GetString(core.NewTag(0x0028, 0x0008))
			if n != strconv.Itoa(2) {
				t.Fatalf("frames=%s", n)
			}
			pixel, err := pixeldata.Extract(out.Dataset)
			if err != nil {
				t.Fatal(err)
			}
			if len(pixel.Raw) != 8 || report.Incomplete == nil || report.Incomplete.RetainedFrames != 2 || report.Incomplete.DeclaredFrames != 3 {
				t.Fatalf("raw=%d report=%+v", len(pixel.Raw), report.Incomplete)
			}
			got := make([]uint16, 4)
			for i := range got {
				got[i] = binary.LittleEndian.Uint16(pixel.Raw[2*i:])
			}
			want := []uint16{1, 0x1234, 0x4567, 0xfedc}
			for i := range want {
				if got[i] != want[i] {
					t.Fatalf("sample%d=%x want%x", i, got[i], want[i])
				}
			}
			again, _ := f.Dataset.Get(core.TagPixelData)
			unchanged, _ := again.RawBytes()
			if !bytes.Equal(source, unchanged) {
				t.Fatal("source changed")
			}
		})
	}
}

func incompleteDecodedSource(t *testing.T, present int) *object.File {
	t.Helper()
	f, err := object.ReadFileWithOptions(bytes.NewReader(incompleteNativeFile(t, transfer.ExplicitVRLittleEndian, present)), object.ReadFileOptions{AllowIncompletePixelData: true})
	if f == nil || !errors.Is(err, object.ErrPartialRead) {
		t.Fatalf("fixture %v", err)
	}
	return f
}
func TestIncompletePixelDataNativeEdgesAndQuota(t *testing.T) {
	for _, present := range []int{0, 1, 3, 4, 7, 8, 9, 10, 11} {
		t.Run(strconv.Itoa(present), func(t *testing.T) {
			f := incompleteDecodedSource(t, present)
			var report pixeldata.DecompressReport
			out, err := pixeldata.DecompressFile(f, pixeldata.DecompressOptions{Recovery: pixeldata.DecodeRecovery{IncompleteData: true}, Report: &report})
			if present < 4 {
				if out != nil || !errors.Is(err, pixeldata.ErrNoCompleteFrames) || report.Incomplete != nil {
					t.Fatalf("zero %v", err)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if report.Incomplete.RetainedFrames != present/4 || report.Incomplete.DiscardedBytes != int64(present%4) {
				t.Fatalf("%+v", report.Incomplete)
			}
			p, _ := pixeldata.Extract(out.Dataset)
			if len(p.Raw) != 4*(present/4) {
				t.Fatal("partial frame exposed")
			}
		})
	}
	for _, limits := range []pixeldata.DecompressLimits{{MaxFrames: 2}, {MaxPixels: 4}, {MaxNativeBytes: 8}, {MaxInputBytes: 8}} {
		f := incompleteDecodedSource(t, 9)
		var report pixeldata.DecompressReport
		out, err := pixeldata.DecompressFile(f, pixeldata.DecompressOptions{Recovery: pixeldata.DecodeRecovery{IncompleteData: true}, Limits: limits, Report: &report})
		if out != nil || !errors.Is(err, pixeldata.ErrDecompressResourceLimit) || report.Incomplete != nil {
			t.Fatalf("original quota %+v %v", limits, err)
		}
	}
	f := incompleteDecodedSource(t, 9)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if out, err := pixeldata.DecompressFileContext(ctx, f, pixeldata.DecompressOptions{Recovery: pixeldata.DecodeRecovery{IncompleteData: true}}); out != nil || !errors.Is(err, context.Canceled) {
		t.Fatalf("cancel %v", err)
	}
	if codec := pixeldata.DetectableDecodeRecovery(); codec.IncompleteData {
		t.Fatal("subset recovery became automatic")
	}
	// A missing final even-VL pad may leave every declared odd-sized frame whole.
	complete, err := object.ReadFile(bytes.NewReader(incompleteNativeFile(t, transfer.ExplicitVRLittleEndian, 12)))
	if err != nil {
		t.Fatal(err)
	}
	o := complete.Dataset
	o.Put(core.NewRawElement(core.NewTag(0x0028, 0x0100), core.VRUS, []byte{8, 0}))
	o.Put(core.NewRawElement(core.NewTag(0x0028, 0x0101), core.VRUS, []byte{8, 0}))
	o.Put(core.NewRawElement(core.NewTag(0x0028, 0x0102), core.VRUS, []byte{7, 0}))
	o.Put(core.NewRawElement(core.NewTag(0x0028, 0x0011), core.VRUS, []byte{1, 0}))
	o.Put(core.NewRawElement(core.TagPixelData, core.VROB, []byte{17, 35, 99}))
	var b bytes.Buffer
	if err := object.WriteFile(&b, complete); err != nil {
		t.Fatal(err)
	}
	f, err = object.ReadFileWithOptions(bytes.NewReader(b.Bytes()[:b.Len()-1]), object.ReadFileOptions{AllowIncompletePixelData: true})
	if f == nil || !errors.Is(err, object.ErrPartialRead) {
		t.Fatal(err)
	}
	var report pixeldata.DecompressReport
	out, err := pixeldata.DecompressFile(f, pixeldata.DecompressOptions{Recovery: pixeldata.DecodeRecovery{IncompleteData: true}, Report: &report})
	if err != nil || report.Incomplete.RetainedFrames != 3 || report.Incomplete.DiscardedBytes != 0 {
		t.Fatalf("pad %+v %v", report.Incomplete, err)
	}
	p, _ := pixeldata.Extract(out.Dataset)
	if !bytes.Equal(p.Raw[:3], []byte{17, 35, 99}) {
		t.Fatal("missing pad samples changed")
	}
}
func TestIncompletePixelDataStrictConsumersAndMetadataRefusals(t *testing.T) {
	f := incompleteDecodedSource(t, 9)
	p, _ := pixeldata.ExtractView(f.Dataset)
	if _, err := pixeldata.DecodeFrames(transfer.ExplicitVRLittleEndian.UID, p, f.Dataset); !errors.Is(err, object.ErrPartialFile) {
		t.Fatalf("decode %v", err)
	}
	if _, err := pixeldata.ExtractNativeFrames(f.Dataset); !errors.Is(err, object.ErrPartialFile) {
		t.Fatalf("extract %v", err)
	}
	if _, err := pixeldata.ExtractNativeFramesView(f.Dataset); !errors.Is(err, object.ErrPartialFile) {
		t.Fatalf("view %v", err)
	}
	if _, err := pixeldata.NewFrameReader(context.Background(), f, nil, pixeldata.FrameReaderOptions{}); !errors.Is(err, object.ErrPartialFile) {
		t.Fatalf("frame reader %v", err)
	}
	if out, _, err := pixeldata.TranscodeDataSet(context.Background(), f.Dataset, transfer.ExplicitVRLittleEndian, transfer.ImplicitVRLittleEndian, pixeldata.TranscodeOptions{}); out != nil || !errors.Is(err, object.ErrPartialFile) {
		t.Fatalf("transcode %v", err)
	}
	// Neither arbitrary short core values nor a generic partial file supply the private certificate.
	forged := object.FromElements(f.Dataset.Elements(), nil)
	forged.SetValueByteOrder(f.Dataset.ValueByteOrder())
	if _, _, err := pixeldata.DecompressDataSet(forged, transfer.ExplicitVRLittleEndian, pixeldata.DecompressOptions{Recovery: pixeldata.DecodeRecovery{IncompleteData: true}}); !errors.Is(err, pixeldata.ErrPixelDataSizeMismatch) {
		t.Fatalf("forged %v", err)
	}
	generic, err := object.ReadFileWithOptions(bytes.NewReader(incompleteNativeFile(t, transfer.ExplicitVRLittleEndian, 9)), object.ReadFileOptions{AllowPartialRead: true})
	if generic == nil || !errors.Is(err, object.ErrPartialRead) {
		t.Fatal(err)
	}
	if _, err := pixeldata.DecompressFile(generic, pixeldata.DecompressOptions{Recovery: pixeldata.DecodeRecovery{IncompleteData: true}}); !errors.Is(err, object.ErrPartialFile) {
		t.Fatalf("generic partial %v", err)
	}
	for _, tag := range []core.Tag{core.NewTag(0x5200, 0x9230), core.NewTag(0x0020, 0x9161), core.NewTag(0x0020, 0x9222), core.NewTag(0x0028, 0x0009), core.NewTag(0x0018, 0x1065), core.NewTag(0x6000, 0x0015)} {
		f := incompleteDecodedSource(t, 9)
		f.Dataset.Put(core.NewRawElement(tag, core.VRUN, nil))
		out, err := pixeldata.DecompressFile(f, pixeldata.DecompressOptions{Recovery: pixeldata.DecodeRecovery{IncompleteData: true}})
		if out != nil || !errors.Is(err, pixeldata.ErrIncompatiblePixelData) {
			t.Fatalf("dependent%s %v", tag, err)
		}
	}
	for _, allocated := range []byte{1, 12} {
		f := incompleteDecodedSource(t, 9)
		f.Dataset.Put(core.NewRawElement(core.NewTag(0x0028, 0x0100), core.VRUS, []byte{allocated, 0}))
		out, err := pixeldata.DecompressFile(f, pixeldata.DecompressOptions{Recovery: pixeldata.DecodeRecovery{IncompleteData: true}})
		if out != nil || err == nil {
			t.Fatalf("packing%d %v", allocated, err)
		}
	}
	f = incompleteDecodedSource(t, 9)
	e, _ := f.Dataset.Get(core.TagPixelData)
	e.Value = core.RawValue([]byte{1, 2})
	f.Dataset.Put(e)
	if _, err := pixeldata.DecompressFile(f, pixeldata.DecompressOptions{Recovery: pixeldata.DecodeRecovery{IncompleteData: true}}); !errors.Is(err, pixeldata.ErrIncompatiblePixelData) {
		t.Fatalf("stale %v", err)
	}
	for _, r := range []pixeldata.DecodeRecovery{{IncompleteData: true, ReconcileWithCodestream: true}, {IncompleteData: true, IgnoreInconsistentBasicOffsetTable: true}, {IncompleteData: true, DefaultImagePixelModule: true}} {
		if _, err := pixeldata.DecompressFile(incompleteDecodedSource(t, 9), pixeldata.DecompressOptions{Recovery: r}); !errors.Is(err, object.ErrPartialFile) {
			t.Fatalf("combination %v", err)
		}
	}
}

func incompleteEncodedFixture(t *testing.T, syntax transfer.Syntax, declared int, fragments [][]byte, tail []byte) []byte {
	t.Helper()
	f, err := object.ReadFile(bytes.NewReader(incompleteNativeFile(t, transfer.ExplicitVRLittleEndian, 12)))
	if err != nil {
		t.Fatal(err)
	}
	f.TransferSyntax = syntax
	f.Meta = nil
	for _, pair := range []struct {
		tag core.Tag
		v   byte
	}{{core.NewTag(0x0028, 0x0100), 8}, {core.NewTag(0x0028, 0x0101), 8}, {core.NewTag(0x0028, 0x0102), 7}} {
		f.Dataset.Put(core.NewRawElement(pair.tag, core.VRUS, []byte{pair.v, 0}))
	}
	f.Dataset.Put(core.Element{Header: core.ElementHeader{Tag: core.NewTag(0x0028, 0x0008), VR: core.VRIS}, Value: core.StringValue{strconv.Itoa(declared)}})
	padded := make([][]byte, len(fragments))
	for i, v := range fragments {
		padded[i] = bytes.Clone(v)
		if len(v)%2 != 0 {
			padded[i] = append(padded[i], 0)
		}
	}
	f.Dataset.Put(core.Element{Header: core.ElementHeader{Tag: core.TagPixelData, VR: core.VROB, Length: core.UndefinedLength, LengthSet: true}, Value: core.FragmentSequence{Fragments: padded}})
	var b bytes.Buffer
	if err := object.WriteFile(&b, f); err != nil {
		t.Fatal(err)
	}
	return append(bytes.Clone(b.Bytes()[:b.Len()-8]), tail...)
}
func TestIncompletePixelDataEncodedCompletePrefix(t *testing.T) {
	for _, syntax := range []transfer.Syntax{transfer.JPEGBaseline, transfer.JPEGLSLossless} {
		t.Run(syntax.UID, func(t *testing.T) {
			registry := pixeldata.NewMemoryRegistry()
			var stream []byte
			var want []byte
			if syntax.UID == transfer.JPEGBaseline.UID {
				if err := jpeg.Register(registry); err != nil {
					t.Fatal(err)
				}
				im := image.NewGray(image.Rect(0, 0, 2, 1))
				im.SetGray(0, 0, color.Gray{Y: 24})
				im.SetGray(1, 0, color.Gray{Y: 200})
				var b bytes.Buffer
				if err := stdjpeg.Encode(&b, im, &stdjpeg.Options{Quality: 100}); err != nil {
					t.Fatal(err)
				}
				stream = b.Bytes()
				decoded, err := stdjpeg.Decode(bytes.NewReader(stream))
				if err != nil {
					t.Fatal(err)
				}
				want = []byte{color.GrayModel.Convert(decoded.At(0, 0)).(color.Gray).Y, color.GrayModel.Convert(decoded.At(1, 0)).(color.Gray).Y}
			} else {
				if err := jpegls.Register(registry); err != nil {
					t.Fatal(err)
				}
				want = []byte{24, 200}
				encoded, err := jpegls.NewEncoder().EncodeFrame(context.Background(), want, pixeldata.Metadata{Rows: 1, Columns: 2, SamplesPerPixel: 1, BitsAllocated: 8, BitsStored: 8, HighBit: 7, NumberOfFrames: 1, PhotometricInterpretation: "MONOCHROME2"})
				if err != nil {
					t.Fatal(err)
				}
				stream = encoded.Data
			}
			cut := len(stream) / 2
			cut -= cut % 2
			if cut < 2 {
				t.Fatal("small stream")
			}
			pieces := [][]byte{stream[:cut], stream[cut:], stream, []byte{0xff, 0xd8, 0xff, 0xda, 0, 2, 0x11, 0x22}}
			for _, tail := range [][]byte{nil, {0xfe, 0xff, 0, 0xe0, 16, 0, 0, 0, 1, 2, 3}, {0xfe, 0xff, 0xdd, 0xe0}} {
				wire := incompleteEncodedFixture(t, syntax, 3, pieces, tail)
				if f, err := object.ReadFile(bytes.NewReader(wire)); f != nil || err == nil {
					t.Fatal("strict admitted malformed input")
				}
				f, err := object.ReadFileWithOptions(bytes.NewReader(wire), object.ReadFileOptions{AllowIncompletePixelData: true})
				if f == nil || !errors.Is(err, object.ErrPartialRead) {
					t.Fatal(err)
				}
				var report pixeldata.DecompressReport
				out, err := pixeldata.DecompressFile(f, pixeldata.DecompressOptions{Registry: registry, Recovery: pixeldata.DecodeRecovery{IncompleteData: true}, Report: &report})
				if err != nil {
					t.Fatal(err)
				}
				pixel, _ := pixeldata.Extract(out.Dataset)
				if !bytes.Equal(pixel.Raw, append(bytes.Clone(want), want...)) || report.Incomplete.RetainedFrames != 2 || report.Incomplete.DeclaredFrames != 3 || report.Incomplete.DiscardedFragments < 1 {
					t.Fatalf("raw%x want%x report%+v", pixel.Raw, want, report.Incomplete)
				}
				if len(report.Diagnostics) != 1 || report.Diagnostics[0] != pixeldata.DiagnosticIncompletePixelData {
					t.Fatalf("diagnostics%v", report.Diagnostics)
				}
				if _, err := pixeldata.NewFrameReader(context.Background(), out, nil, pixeldata.FrameReaderOptions{}); err != nil {
					t.Fatalf("normalized frame consumer %v", err)
				}
				uid, _ := out.Dataset.GetString(core.NewTag(0x0008, 0x0018))
				sourceUID, _ := f.Dataset.GetString(core.NewTag(0x0008, 0x0018))
				if uid != sourceUID {
					t.Fatal("invented UID")
				}
			}
			// Complete-looking framing without decodable entropy is never an image.
			wire := incompleteEncodedFixture(t, syntax, 1, [][]byte{{0xff, 0xd8, 0xff, 0xd9}}, nil)
			f, err := object.ReadFileWithOptions(bytes.NewReader(wire), object.ReadFileOptions{AllowIncompletePixelData: true})
			if f == nil || !errors.Is(err, object.ErrPartialRead) {
				t.Fatal(err)
			}
			var report pixeldata.DecompressReport
			if out, err := pixeldata.DecompressFile(f, pixeldata.DecompressOptions{Registry: registry, Recovery: pixeldata.DecodeRecovery{IncompleteData: true}, Report: &report}); out != nil || err == nil || report.Incomplete != nil {
				t.Fatalf("corrupt entropy %v", err)
			}
		})
	}
}

func TestIncompletePixelDataSignedDisplayAndShortRefusal(t *testing.T) {
	f := incompleteDecodedSource(t, 9)
	f.Dataset.Put(core.NewRawElement(core.NewTag(0x0028, 0x0103), core.VRUS, []byte{1, 0}))
	out, err := pixeldata.DecompressFile(f, pixeldata.DecompressOptions{Recovery: pixeldata.DecodeRecovery{IncompleteData: true}})
	if err != nil {
		t.Fatal(err)
	}
	native, err := pixeldata.ExtractNativeFramesView(out.Dataset)
	if err != nil {
		t.Fatal(err)
	}
	for i, data := range native.Data {
		frame := display.Frame{Rows: 1, Columns: 2, Pixels: data, Format: display.PixelFormat{BitsAllocated: 16, BitsStored: 16, HighBit: 15, Signed: true, ByteOrder: binary.LittleEndian}}
		values := make([]int16, 2)
		if err := display.DecodeStoredInto(values, frame); err != nil {
			t.Fatal(err)
		}
		expected := [][]int16{{1, 0x1234}, {0x4567, -292}}
		for j, v := range values {
			if v != expected[i][j] {
				t.Fatalf("signed frame%d sample%d=%d", i, j, v)
			}
		}
		if img, err := display.RenderGray(frame); err != nil || img.Bounds().Dx() != 2 {
			t.Fatalf("display %v", err)
		}
		frame.Pixels = frame.Pixels[:3]
		if _, err := display.RenderGray(frame); !errors.Is(err, display.ErrPixelDataTooShort) {
			t.Fatalf("short refusal %v", err)
		}
	}
}
func TestIncompletePixelDataUnsupportedEncapsulatedAndCount(t *testing.T) {
	for _, syntax := range []transfer.Syntax{transfer.RLELossless, transfer.JPEG2000LosslessOnly, transfer.EncapsulatedUncompressedExplicitVRLittleEndian, transfer.MPEG2MPML} {
		wire := incompleteEncodedFixture(t, syntax, 1, [][]byte{{0xff, 0xd8, 0xff, 0xd9}}, nil)
		f, err := object.ReadFileWithOptions(bytes.NewReader(wire), object.ReadFileOptions{AllowIncompletePixelData: true})
		if f == nil || !errors.Is(err, object.ErrPartialRead) {
			t.Fatalf("read %s %v", syntax.UID, err)
		}
		out, err := pixeldata.DecompressFile(f, pixeldata.DecompressOptions{Recovery: pixeldata.DecodeRecovery{IncompleteData: true}})
		if out != nil || !errors.Is(err, pixeldata.ErrUnsupportedDecompression) {
			t.Fatalf("profile%s %v", syntax.UID, err)
		}
	}
	f := incompleteDecodedSource(t, 9)
	f.Dataset.Put(core.Element{Header: core.ElementHeader{Tag: core.NewTag(0x0028, 0x0008), VR: core.VRIS}, Value: core.StringValue{"1"}})
	if _, err := pixeldata.DecompressFile(f, pixeldata.DecompressOptions{Recovery: pixeldata.DecodeRecovery{IncompleteData: true}}); !errors.Is(err, pixeldata.ErrIncompatiblePixelData) {
		t.Fatalf("extra native frame %v", err)
	}
}

func TestIncompletePixelDataRewriteCannotCompleteSampleWithPad(t *testing.T) {
	f := incompleteDecodedSource(t, 11)
	var b bytes.Buffer
	if err := object.WriteFileWithOptions(&b, f, object.WriteFileOptions{AllowPartial: true}); !errors.Is(err, object.ErrPartialFile) || b.Len() != 0 {
		t.Fatalf("incomplete rewrite bytes%d err%v", b.Len(), err)
	}
	out, err := pixeldata.DecompressFile(f, pixeldata.DecompressOptions{Recovery: pixeldata.DecodeRecovery{IncompleteData: true}})
	if err != nil {
		t.Fatal(err)
	}
	if err := object.WriteFile(&b, out); err != nil {
		t.Fatal(err)
	}
	reread, err := object.ReadFile(bytes.NewReader(b.Bytes()))
	if err != nil {
		t.Fatal(err)
	}
	native, err := pixeldata.ExtractNativeFrames(reread.Dataset)
	if err != nil || len(native.Data) != 2 {
		t.Fatalf("rewrite %v", err)
	}
}

type incompleteCancellingCodec struct{ cancel context.CancelFunc }

func (c incompleteCancellingCodec) Decode(pixeldata.PixelData, *object.Object) (pixeldata.Frames, error) {
	c.cancel()
	return pixeldata.Frames{Rows: 1, Columns: 2, Data: [][]byte{{17, 25}}}, nil
}
func TestIncompletePixelDataCancelBeforePublicationAndHeadless2D(t *testing.T) {
	wire := incompleteEncodedFixture(t, transfer.JPEGBaseline, 1, [][]byte{{0xff, 0xd8, 0xff, 0xd9}}, nil)
	f, err := object.ReadFileWithOptions(bytes.NewReader(wire), object.ReadFileOptions{AllowIncompletePixelData: true})
	if f == nil || !errors.Is(err, object.ErrPartialRead) {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	registry := pixeldata.NewMemoryRegistry()
	if err := registry.RegisterCodec(transfer.JPEGBaseline.UID, incompleteCancellingCodec{cancel}); err != nil {
		t.Fatal(err)
	}
	var report pixeldata.DecompressReport
	out, err := pixeldata.DecompressFileContext(ctx, f, pixeldata.DecompressOptions{Registry: registry, Recovery: pixeldata.DecodeRecovery{IncompleteData: true}, Report: &report})
	if out != nil || !errors.Is(err, context.Canceled) || report.Incomplete != nil || len(report.Diagnostics) > 0 {
		t.Fatalf("cancel publish %v %+v", err, report)
	}
	f = incompleteDecodedSource(t, 11)
	out, err = pixeldata.DecompressFile(f, pixeldata.DecompressOptions{Recovery: pixeldata.DecodeRecovery{IncompleteData: true}})
	if err != nil {
		t.Fatal(err)
	}
	frames, err := pixeldata.ExtractNativeFramesView(out.Dataset)
	if err != nil {
		t.Fatal(err)
	}
	for _, raw := range frames.Data {
		slice := &render.Frame{Metadata: frames.Metadata, ByteOrder: binary.LittleEndian, PixelBytes: raw, TransferSyntaxUID: transfer.ExplicitVRLittleEndian.UID}
		img, err := render.RenderFrame(slice, render.WindowLevel{Center: 32768, Width: 65536})
		if err != nil || img.Bounds().Dx() != 2 {
			t.Fatalf("headless2D %v", err)
		}
		slice.PixelBytes = raw[:3]
		if _, err := render.RenderFrame(slice, render.WindowLevel{Center: 32768, Width: 65536}); err == nil {
			t.Fatal("renderer short check weakened")
		}
	}
	var b bytes.Buffer
	if err := object.WriteFile(&b, out); err != nil {
		t.Fatal(err)
	}
	back, err := object.ReadFile(bytes.NewReader(b.Bytes()))
	if err != nil {
		t.Fatal(err)
	}
	before, _ := pixeldata.ExtractView(f.Dataset)
	after, _ := pixeldata.ExtractView(back.Dataset)
	if !bytes.Equal(after.Raw, before.Raw[:8]) {
		t.Fatal("complete-prefix rewrite bytes differ")
	}
}

func TestIncompletePixelDataContradictoryNativeVLRefusal(t *testing.T) {
	wire := incompleteNativeFile(t, transfer.ExplicitVRLittleEndian, 7)
	binary.LittleEndian.PutUint32(wire[len(wire)-7-4:], 8)
	f, err := object.ReadFileWithOptions(bytes.NewReader(wire), object.ReadFileOptions{AllowIncompletePixelData: true})
	if f == nil || !errors.Is(err, object.ErrPartialRead) {
		t.Fatal(err)
	}
	out, err := pixeldata.DecompressFile(f, pixeldata.DecompressOptions{Recovery: pixeldata.DecodeRecovery{IncompleteData: true}})
	if out != nil || !errors.Is(err, pixeldata.ErrIncompatiblePixelData) {
		t.Fatalf("contradictory native VL %v", err)
	}
}

func TestIncompletePixelDataDeferredMetadataRefusesNormalization(t *testing.T) {
	for _, nested := range []bool{false, true} {
		t.Run(strconv.FormatBool(nested), func(t *testing.T) {
			complete, err := object.ReadFile(bytes.NewReader(incompleteNativeFile(t, transfer.ExplicitVRLittleEndian, 12)))
			if err != nil {
				t.Fatal(err)
			}
			waveformTag := core.NewTag(0x5400, 0x1010)
			sequenceTag := core.NewTag(0x5400, 0x0100)
			want := []byte{11, 22, 33, 44}
			element := core.NewRawElement(waveformTag, core.VROB, want)
			if nested {
				complete.Dataset.Put(core.Element{Header: core.ElementHeader{Tag: sequenceTag, VR: core.VRSQ}, Value: core.SequenceValue{Items: []core.DataSet{{Elements: []core.Element{element}}}}})
			} else {
				complete.Dataset.Put(element)
			}
			var wire bytes.Buffer
			if err := object.WriteFile(&wire, complete); err != nil {
				t.Fatal(err)
			}
			path := filepath.Join(t.TempDir(), "deferred-truncated.dcm")
			if err := os.WriteFile(path, wire.Bytes()[:wire.Len()-3], 0600); err != nil {
				t.Fatal(err)
			}
			source, err := object.OpenFileWithOptions(path, object.ReadFileOptions{AllowIncompletePixelData: true, DeferWaveformData: true})
			if source == nil || !errors.Is(err, object.ErrPartialRead) {
				t.Fatalf("inspection %v", err)
			}
			t.Cleanup(func() { _ = source.Close() })
			var item *object.Object
			if nested {
				items, ok := source.Dataset.GetSequence(sequenceTag)
				if !ok || len(items) != 1 {
					t.Fatal("missing waveform sequence")
				}
				item = items[0]
			}
			replay := func() {
				t.Helper()
				var b bytes.Buffer
				var n int64
				var err error
				if nested {
					n, err = source.Dataset.CopyNestedValueTo(item, waveformTag, &b)
				} else {
					n, err = source.Dataset.CopyValueTo(waveformTag, &b)
				}
				if err != nil || n != int64(len(want)) || !bytes.Equal(b.Bytes(), want) {
					t.Fatalf("source replay %d/%v/%v", n, b.Bytes(), err)
				}
			}
			replay()
			for _, fileAPI := range []bool{false, true} {
				var report pixeldata.DecompressReport
				opts := pixeldata.DecompressOptions{Recovery: pixeldata.DecodeRecovery{IncompleteData: true}, Report: &report}
				if fileAPI {
					out, err := pixeldata.DecompressFile(source, opts)
					if out != nil || !errors.Is(err, pixeldata.ErrIncompatiblePixelData) || report.Incomplete != nil {
						t.Fatalf("file normalization output=%v report=%+v err=%v", out, report.Incomplete, err)
					}
				} else {
					out, _, err := pixeldata.DecompressDataSet(source.Dataset, source.TransferSyntax, opts)
					if out != nil || !errors.Is(err, pixeldata.ErrIncompatiblePixelData) || report.Incomplete != nil {
						t.Fatalf("dataset normalization output=%v report=%+v err=%v", out, report.Incomplete, err)
					}
				}
				replay()
			}
			// Materializing these same metadata values makes independent output possible.
			materialized, err := object.OpenFileWithOptions(path, object.ReadFileOptions{AllowIncompletePixelData: true})
			if materialized == nil || !errors.Is(err, object.ErrPartialRead) {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = materialized.Close() })
			out, err := pixeldata.DecompressFile(materialized, pixeldata.DecompressOptions{Recovery: pixeldata.DecodeRecovery{IncompleteData: true}})
			if err != nil {
				t.Fatal(err)
			}
			var b bytes.Buffer
			if nested {
				items, ok := out.Dataset.GetSequence(sequenceTag)
				if !ok || len(items) != 1 {
					t.Fatal("lost materialized waveform sequence")
				}
				_, err = out.Dataset.CopyNestedValueTo(items[0], waveformTag, &b)
			} else {
				_, err = out.Dataset.CopyValueTo(waveformTag, &b)
			}
			if err != nil || !bytes.Equal(b.Bytes(), want) {
				t.Fatalf("materialized output %v/%v", b.Bytes(), err)
			}
		})
	}
}

func TestIncompletePixelDataGenericPartialHistoryCannotBeReplaced(t *testing.T) {
	generic, err := object.ReadFileWithOptions(bytes.NewReader(incompleteNativeFile(t, transfer.ExplicitVRLittleEndian, 9)), object.ReadFileOptions{AllowPartialRead: true})
	if generic == nil || !errors.Is(err, object.ErrPartialRead) {
		t.Fatal(err)
	}
	inspected := incompleteDecodedSource(t, 9)
	generic.Dataset = inspected.Dataset
	var report pixeldata.DecompressReport
	out, err := pixeldata.DecompressFile(generic, pixeldata.DecompressOptions{Recovery: pixeldata.DecodeRecovery{IncompleteData: true}, Report: &report})
	if out != nil || !errors.Is(err, object.ErrPartialFile) || report.Incomplete != nil {
		t.Fatalf("replaced partial history output%v report%+v err%v", out, report.Incomplete, err)
	}
}

type incompleteGenericSentinelReader struct{ reader *bytes.Reader }

func (r incompleteGenericSentinelReader) Read(p []byte) (int, error) {
	n, err := r.reader.Read(p)
	if err == io.EOF {
		return n, errors.Join(object.ErrIncompletePixelData, errors.New("generic source failure"))
	}
	return n, err
}
func (r incompleteGenericSentinelReader) Seek(offset int64, whence int) (int64, error) {
	return r.reader.Seek(offset, whence)
}
func TestIncompletePixelDataGenericSentinelAndOffsetDoNotCertifyFile(t *testing.T) {
	inspected := incompleteDecodedSource(t, 8)
	complete, err := object.ReadFile(bytes.NewReader(incompleteNativeFile(t, transfer.ExplicitVRLittleEndian, 12)))
	if err != nil {
		t.Fatal(err)
	}
	// One empty short-VR header shifts the generic failure by exactly eight bytes.
	complete.Dataset.Put(core.NewRawElement(core.NewTag(0x0010, 0x0020), core.VRLO, []byte{}))
	var b bytes.Buffer
	if err := object.WriteFile(&b, complete); err != nil {
		t.Fatal(err)
	}
	generic, err := object.ReadFileWithOptions(incompleteGenericSentinelReader{bytes.NewReader(b.Bytes()[:b.Len()-12])}, object.ReadFileOptions{AllowPartialRead: true})
	if generic == nil || !errors.Is(err, object.ErrPartialRead) {
		t.Fatal(err)
	}
	partial, _ := generic.PartialRead()
	info, _ := inspected.Dataset.IncompletePixelData()
	if partial.Tag != core.TagPixelData || partial.Offset != info.FailureOffset || !errors.Is(partial.Err, object.ErrIncompletePixelData) {
		t.Fatalf("control does not match public predicates: %+v info%+v", partial, info)
	}
	generic.Dataset = inspected.Dataset
	var report pixeldata.DecompressReport
	out, err := pixeldata.DecompressFile(generic, pixeldata.DecompressOptions{Recovery: pixeldata.DecodeRecovery{IncompleteData: true}, Report: &report})
	if out != nil || !errors.Is(err, object.ErrPartialFile) || report.Incomplete != nil {
		t.Fatalf("generic sentinel output%v report%+v err%v", out, report.Incomplete, err)
	}
	// File-level provenance is a copy; replacing Dataset cannot import it.
	if _, certified := generic.IncompletePixelData(); certified {
		t.Fatal("imported File certificate")
	}
	if _, certified := inspected.IncompletePixelData(); !certified {
		t.Fatal("legitimate File certificate missing")
	}
}

type incompleteWriteFrameSource struct {
	data  []byte
	count int
	calls int
}

func (s *incompleteWriteFrameSource) Frames() int                      { s.calls++; return s.count }
func (s *incompleteWriteFrameSource) FrameLength(i int) (int64, error) { s.calls++; return 4, nil }
func (s *incompleteWriteFrameSource) WriteFrameTo(i int, w io.Writer) (int64, error) {
	s.calls++
	n, err := w.Write(s.data[4*i : 4*(i+1)])
	return int64(n), err
}
func TestIncompletePixelDataFrameSourceCannotBypassWriteRefusal(t *testing.T) {
	raw := []byte{1, 0, 0x34, 0x12, 0x67, 0x45, 0xdc, 0xfe, 0x88, 0x88, 0x99, 0x99}
	for _, variant := range []string{"historical", "replaced", "object-only"} {
		t.Run(variant, func(t *testing.T) {
			file := incompleteDecodedSource(t, 11)
			switch variant {
			case "replaced":
				file.Dataset = object.FromElements(file.Dataset.Elements(), nil)
			case "object-only":
				file = &object.File{Dataset: file.Dataset, Meta: file.Meta, TransferSyntax: file.TransferSyntax}
			}
			frames := &incompleteWriteFrameSource{data: raw, count: 3}
			var b bytes.Buffer
			err := object.WriteFileFrames(&b, file, frames, object.FrameWriteOptions{WriteFileOptions: object.WriteFileOptions{AllowPartial: true}})
			if !errors.Is(err, object.ErrPartialFile) || b.Len() != 0 || frames.calls != 0 {
				t.Fatalf("frame-source bypass calls%d bytes%d err%v", frames.calls, b.Len(), err)
			}
		})
	}
	t.Run("normalized", func(t *testing.T) {
		file, err := pixeldata.DecompressFile(incompleteDecodedSource(t, 11), pixeldata.DecompressOptions{Recovery: pixeldata.DecodeRecovery{IncompleteData: true}})
		if err != nil {
			t.Fatal(err)
		}
		frames := &incompleteWriteFrameSource{data: raw[:8], count: 2}
		var b bytes.Buffer
		if err := object.WriteFileFrames(&b, file, frames, object.FrameWriteOptions{}); err != nil {
			t.Fatal(err)
		}
		reread, err := object.ReadFile(bytes.NewReader(b.Bytes()))
		if err != nil {
			t.Fatal(err)
		}
		pixel, err := pixeldata.Extract(reread.Dataset)
		if err != nil || !bytes.Equal(pixel.Raw, raw[:8]) || frames.calls != 5 {
			t.Fatalf("normalized frames calls%d raw%v err%v", frames.calls, pixel.Raw, err)
		}
	})
	t.Run("generic-partial-new-frames", func(t *testing.T) {
		file, err := object.ReadFileWithOptions(bytes.NewReader(incompleteNativeFile(t, transfer.ExplicitVRLittleEndian, 11)), object.ReadFileOptions{AllowPartialRead: true})
		if file == nil || !errors.Is(err, object.ErrPartialRead) {
			t.Fatal(err)
		}
		frames := &incompleteWriteFrameSource{data: raw, count: 3}
		var b bytes.Buffer
		if err := object.WriteFileFrames(&b, file, frames, object.FrameWriteOptions{WriteFileOptions: object.WriteFileOptions{AllowPartial: true}}); err != nil {
			t.Fatal(err)
		}
		reread, err := object.ReadFile(bytes.NewReader(b.Bytes()))
		if err != nil {
			t.Fatal(err)
		}
		pixel, err := pixeldata.Extract(reread.Dataset)
		if err != nil || !bytes.Equal(pixel.Raw, raw) || frames.calls != 7 {
			t.Fatalf("generic explicit frames calls%d raw%v err%v", frames.calls, pixel.Raw, err)
		}
	})
}
