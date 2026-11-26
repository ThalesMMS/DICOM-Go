package deflateframe_test

import (
	"bytes"
	"compress/flate"
	"context"
	"encoding/binary"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/ThalesMMS/dicom-go/core"
	"github.com/ThalesMMS/dicom-go/object"
	"github.com/ThalesMMS/dicom-go/pixeldata"
	"github.com/ThalesMMS/dicom-go/pixeldata/deflateframe"
	"github.com/ThalesMMS/dicom-go/pixeldata/jpegls"
	"github.com/ThalesMMS/dicom-go/pixeldata/rle"
	"github.com/ThalesMMS/dicom-go/transfer"
)

func transcodeOptions(t *testing.T) pixeldata.TranscodeOptions {
	t.Helper()
	encoders := pixeldata.NewMemoryEncoderRegistry()
	for _, register := range []func(pixeldata.EncoderRegistry) error{deflateframe.RegisterEncoder, rle.RegisterEncoder, jpegls.RegisterEncoder} {
		if err := register(encoders); err != nil {
			t.Fatal(err)
		}
	}
	return pixeldata.TranscodeOptions{DecoderRegistry: decoders(t), EncoderRegistry: encoders}
}

func transcode(t *testing.T, obj *object.Object, source, target transfer.Syntax) *object.Object {
	t.Helper()
	out, report, err := pixeldata.TranscodeDataSet(context.Background(), obj, source, target, transcodeOptions(t))
	if err != nil {
		t.Fatalf("%s -> %s: %v", source.Name, target.Name, err)
	}
	if report.Lossy {
		t.Fatalf("%s -> %s reported lossy", source.Name, target.Name)
	}
	return out
}

func nativeFrames(t *testing.T, obj *object.Object) [][]byte {
	t.Helper()
	frames, err := pixeldata.ExtractNativeFramesView(obj)
	if err != nil {
		t.Fatal(err)
	}
	return frames.Data
}

func assertFrames(t *testing.T, got, want [][]byte) {
	t.Helper()
	if len(got) != len(want) {
		t.Fatalf("frames = %d, want %d", len(got), len(want))
	}
	for i := range want {
		if !bytes.Equal(got[i], want[i]) {
			t.Fatalf("frame %d = %x, want %x", i, got[i], want[i])
		}
	}
}

// Native -> Deflated Image Frame Compression -> native is bit-exact, writes
// one fragment per frame with a Basic Offset Table, and keeps the Image Pixel
// attributes (including Planar Configuration) unchanged.
func TestTranscodeNativeToDeflatedFramesAndBackIsBitExact(t *testing.T) {
	for _, img := range syntheticImages() {
		if img.bitsAllocated < 8 {
			continue // per-frame transcode of single-bit data is refused upstream
		}
		t.Run(img.name, func(t *testing.T) {
			deflated := transcode(t, img.native(), transfer.ExplicitVRLittleEndian, transfer.DeflatedImageFrameCompression)
			pixel, err := pixeldata.ExtractView(deflated)
			if err != nil {
				t.Fatal(err)
			}
			if !pixel.Encapsulated || len(pixel.Sequence.Fragments) != len(img.frames) || len(pixel.Sequence.OffsetTable) != 4*len(img.frames) {
				t.Fatalf("Pixel Data fragments/BOT = %d/%d", len(pixel.Sequence.Fragments), len(pixel.Sequence.OffsetTable))
			}
			var offset uint32
			for i, fragment := range pixel.Sequence.Fragments {
				if got := binary.LittleEndian.Uint32(pixel.Sequence.OffsetTable[4*i:]); got != offset {
					t.Fatalf("BOT[%d] = %d, want %d", i, got, offset)
				}
				offset += uint32(8 + len(fragment))
				if got := rawInflate(t, fragment); !bytes.Equal(got, img.frames[i]) {
					t.Fatalf("fragment %d does not inflate to the native frame", i)
				}
			}
			metadata, err := pixeldata.ExtractMetadata(deflated)
			if err != nil {
				t.Fatal(err)
			}
			if metadata != img.metadata() {
				t.Fatalf("metadata = %+v, want %+v", metadata, img.metadata())
			}

			for _, target := range []transfer.Syntax{transfer.ExplicitVRLittleEndian, transfer.ImplicitVRLittleEndian} {
				native := transcode(t, deflated, transfer.DeflatedImageFrameCompression, target)
				assertFrames(t, nativeFrames(t, native), img.frames)
				back, err := pixeldata.ExtractMetadata(native)
				if err != nil {
					t.Fatal(err)
				}
				if back != img.metadata() {
					t.Fatalf("%s metadata = %+v, want %+v", target.Name, back, img.metadata())
				}
			}
		})
	}
}

// Compressed sources reach this syntax through their decoder and leave it
// through another encoder, frame by frame and bit-exact.
func TestTranscodeBetweenDeflatedFramesAndOtherLosslessSyntaxes(t *testing.T) {
	img := syntheticImages()[4] // multi-frame mono16
	deflated := transcode(t, img.native(), transfer.ExplicitVRLittleEndian, transfer.DeflatedImageFrameCompression)
	original, err := pixeldata.ExtractView(deflated)
	if err != nil {
		t.Fatal(err)
	}
	for _, via := range []transfer.Syntax{transfer.RLELossless, transfer.JPEGLSLossless} {
		t.Run(via.Name, func(t *testing.T) {
			other := transcode(t, deflated, transfer.DeflatedImageFrameCompression, via)
			assertFrames(t, nativeFrames(t, transcode(t, other, via, transfer.ExplicitVRLittleEndian)), img.frames)
			again := transcode(t, other, via, transfer.DeflatedImageFrameCompression)
			pixel, err := pixeldata.ExtractView(again)
			if err != nil {
				t.Fatal(err)
			}
			// The encoder is deterministic, so the deflated frames are
			// reproduced byte for byte.
			assertFrames(t, pixel.Sequence.Fragments, original.Sequence.Fragments)
		})
	}
}

func TestTranscodeRefusesDeflatedFramesBomb(t *testing.T) {
	img := mono8(16, 16, 1)
	bomb := deflateAll(t, make([]byte, 1<<20), flate.BestCompression)
	if len(bomb)%2 != 0 {
		bomb = append(bomb, 0)
	}
	_, _, err := pixeldata.TranscodeDataSet(context.Background(), img.encapsulated(nil, bomb),
		transfer.DeflatedImageFrameCompression, transfer.ExplicitVRLittleEndian, transcodeOptions(t))
	if !errors.Is(err, deflateframe.ErrExpansionLimit) {
		t.Fatalf("error = %v, want ErrExpansionLimit", err)
	}
}

// A Part 10 file in this syntax is written and read as Explicit VR Little
// Endian with encapsulated Pixel Data, never as a deflated data set, and
// TranscodePath publishes a verified file in both directions.
func TestPart10FilesInDeflatedFrames(t *testing.T) {
	img := syntheticImages()[5] // multi-frame rgb8
	deflated := transcode(t, img.native(), transfer.ExplicitVRLittleEndian, transfer.DeflatedImageFrameCompression)
	var buf bytes.Buffer
	if err := object.WriteFile(&buf, &object.File{Dataset: deflated, TransferSyntax: transfer.DeflatedImageFrameCompression}); err != nil {
		t.Fatal(err)
	}
	file, err := object.ReadFile(bytes.NewReader(buf.Bytes()))
	if err != nil {
		t.Fatal(err)
	}
	if file.TransferSyntax.UID != deflateframe.UID {
		t.Fatalf("transfer syntax = %q", file.TransferSyntax.UID)
	}
	frames, err := decode(t, file.Dataset)
	if err != nil {
		t.Fatal(err)
	}
	assertFrames(t, frames.Data, img.frames)

	dir := t.TempDir()
	source := filepath.Join(dir, "native.dcm")
	var native bytes.Buffer
	if err := object.WriteFile(&native, &object.File{Dataset: img.native(), TransferSyntax: transfer.ExplicitVRLittleEndian}); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(source, native.Bytes(), 0o600); err != nil {
		t.Fatal(err)
	}
	encoded := filepath.Join(dir, "deflated.dcm")
	report, err := pixeldata.TranscodePath(context.Background(), source, encoded, transfer.DeflatedImageFrameCompression, transcodeOptions(t))
	if errors.Is(err, pixeldata.ErrTranscodeUnsupported) {
		t.Skip("TranscodePath is unsupported on this platform")
	}
	if err != nil {
		t.Fatal(err)
	}
	if report.Lossy || report.EncodedBytes == 0 || report.Frames != len(img.frames) {
		t.Fatalf("report = %+v", report)
	}
	decoded := filepath.Join(dir, "decoded.dcm")
	if _, err := pixeldata.TranscodePath(context.Background(), encoded, decoded, transfer.ExplicitVRLittleEndian, transcodeOptions(t)); err != nil {
		t.Fatal(err)
	}
	opened, err := object.OpenFile(decoded)
	if err != nil {
		t.Fatal(err)
	}
	defer opened.Close()
	raw, ok := opened.GetRaw(core.TagPixelData)
	if !ok {
		t.Fatal("decoded file has no native Pixel Data")
	}
	var want []byte
	for _, frame := range img.frames {
		want = append(want, frame...)
	}
	if !bytes.Equal(raw[:len(want)], want) {
		t.Fatal("pixels changed through TranscodePath")
	}
}
