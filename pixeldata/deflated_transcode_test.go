package pixeldata_test

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"

	"github.com/ThalesMMS/dicom-go/core"
	"github.com/ThalesMMS/dicom-go/object"
	"github.com/ThalesMMS/dicom-go/pixeldata"
	"github.com/ThalesMMS/dicom-go/pixeldata/builtin"
	"github.com/ThalesMMS/dicom-go/pixeldata/rle"
	"github.com/ThalesMMS/dicom-go/transfer"
)

// spyEncoders records every lookup. Deflated Explicit VR Little Endian
// compresses the data set, never the pixels, so no pixel encoder may be used.
type spyEncoders struct {
	pixeldata.EncoderRegistry
	lookups atomic.Int32
}

func (s *spyEncoders) GetEncoder(uid string) (pixeldata.FrameEncoder, bool) {
	s.lookups.Add(1)
	return s.EncoderRegistry.GetEncoder(uid)
}

func (s *spyEncoders) EncodeFrame(ctx context.Context, uid string, frame []byte, metadata pixeldata.Metadata) (pixeldata.EncodedFrame, error) {
	s.lookups.Add(1)
	return s.EncoderRegistry.EncodeFrame(ctx, uid, frame, metadata)
}

func openCorpusFile(t *testing.T, name string) *object.File {
	t.Helper()
	file, err := object.OpenFile(filepath.Join("codecfixture", "testdata", "codecfull", "pydicom", name))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = file.Close() })
	return file
}

func nativeFrames(t *testing.T, obj *object.Object) [][]byte {
	t.Helper()
	frames, err := pixeldata.ExtractNativeFrames(obj)
	if err != nil {
		t.Fatal(err)
	}
	return frames.Data
}

func TestDeflatedTargetsNeverUsePixelEncoders(t *testing.T) {
	ctx := context.Background()
	decoders, err := builtin.NewRegistry()
	if err != nil {
		t.Fatal(err)
	}
	reference := nativeFrames(t, openCorpusFile(t, "MR_small.dcm").Dataset)
	encapsulatedUncompressed, _, err := pixeldata.TranscodeDataSet(ctx, openCorpusFile(t, "MR_small.dcm").Dataset, transfer.ExplicitVRLittleEndian, transfer.EncapsulatedUncompressedExplicitVRLittleEndian, pixeldata.TranscodeOptions{})
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name   string
		source *object.Object
		syntax transfer.Syntax
	}{
		{"explicit native", openCorpusFile(t, "MR_small.dcm").Dataset, transfer.ExplicitVRLittleEndian},
		{"rle encapsulated", openCorpusFile(t, "MR_small_RLE.dcm").Dataset, transfer.RLELossless},
		{"jpeg-ls encapsulated", openCorpusFile(t, "MR_small_jpeg_ls_lossless.dcm").Dataset, transfer.JPEGLSLossless},
		{"encapsulated uncompressed .98", encapsulatedUncompressed, transfer.EncapsulatedUncompressedExplicitVRLittleEndian},
	} {
		t.Run(tc.name, func(t *testing.T) {
			encoders := &spyEncoders{EncoderRegistry: pixeldata.NewMemoryEncoderRegistry()}
			deflated, _, err := pixeldata.TranscodeDataSet(ctx, tc.source, tc.syntax, transfer.DeflatedExplicitVRLittleEndian, pixeldata.TranscodeOptions{DecoderRegistry: decoders, EncoderRegistry: encoders})
			if err != nil {
				t.Fatal(err)
			}
			if n := encoders.lookups.Load(); n != 0 {
				t.Fatalf("Deflated target consulted pixel encoders %d times", n)
			}
			element, _ := deflated.Get(core.TagPixelData)
			if _, ok := element.Value.(core.RawValue); !ok {
				t.Fatalf("Deflated Pixel Data is %T, want native bytes", element.Value)
			}
			var part10 bytes.Buffer
			if err := object.WriteFile(&part10, &object.File{Dataset: deflated, TransferSyntax: transfer.DeflatedExplicitVRLittleEndian}); err != nil {
				t.Fatal(err)
			}
			read, err := object.ReadFile(bytes.NewReader(part10.Bytes()))
			if err != nil {
				t.Fatal(err)
			}
			if read.TransferSyntax.UID != transfer.DeflatedExplicitVRLittleEndian.UID {
				t.Fatalf("syntax %s", read.TransferSyntax.UID)
			}
			got := nativeFrames(t, read.Dataset)
			if len(got) != 1 || !bytes.Equal(got[0], reference[0]) {
				t.Fatal("Deflated round trip changed samples")
			}
			// Back to a native syntax: still no encoder.
			little, _, err := pixeldata.TranscodeDataSet(ctx, read.Dataset, read.TransferSyntax, transfer.ExplicitVRLittleEndian, pixeldata.TranscodeOptions{EncoderRegistry: encoders})
			if err != nil {
				t.Fatal(err)
			}
			if n := encoders.lookups.Load(); n != 0 {
				t.Fatalf("Deflated source consulted pixel encoders %d times", n)
			}
			if got := nativeFrames(t, little); !bytes.Equal(got[0], reference[0]) {
				t.Fatal("Deflated -> native changed samples")
			}
		})
	}

	// Compressing pixels from a Deflated source is an explicit encode request:
	// only then is an encoder consulted, and the output is encapsulated.
	encoders := &spyEncoders{EncoderRegistry: pixeldata.NewMemoryEncoderRegistry()}
	if err := rle.RegisterEncoder(encoders.EncoderRegistry); err != nil {
		t.Fatal(err)
	}
	deflated, _, err := pixeldata.TranscodeDataSet(ctx, openCorpusFile(t, "MR_small.dcm").Dataset, transfer.ExplicitVRLittleEndian, transfer.DeflatedExplicitVRLittleEndian, pixeldata.TranscodeOptions{})
	if err != nil {
		t.Fatal(err)
	}
	encoded, _, err := pixeldata.TranscodeDataSet(ctx, deflated, transfer.DeflatedExplicitVRLittleEndian, transfer.RLELossless, pixeldata.TranscodeOptions{EncoderRegistry: encoders, DecoderRegistry: decoders})
	if err != nil {
		t.Fatal(err)
	}
	if encoders.lookups.Load() == 0 {
		t.Fatal("explicit RLE target did not use its encoder")
	}
	if element, _ := encoded.Get(core.TagPixelData); element.Value == nil {
		t.Fatal("missing Pixel Data")
	} else if _, ok := element.Value.(core.FragmentSequence); !ok {
		t.Fatalf("RLE Pixel Data is %T", element.Value)
	}
}

func TestTranscodePathToDeflatedCanceledPublishesNothing(t *testing.T) {
	dir := t.TempDir()
	source := filepath.Join("codecfixture", "testdata", "codecfull", "pydicom", "MR_small.dcm")
	destination := filepath.Join(dir, "deflated.dcm")
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := pixeldata.TranscodePath(ctx, source, destination, transfer.DeflatedExplicitVRLittleEndian, pixeldata.TranscodeOptions{}); !errors.Is(err, context.Canceled) {
		t.Fatalf("canceled transcode error = %v", err)
	}
	if _, err := os.Stat(destination); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("canceled transcode published %s: %v", destination, err)
	}
	if _, err := pixeldata.TranscodePath(context.Background(), source, destination, transfer.DeflatedExplicitVRLittleEndian, pixeldata.TranscodeOptions{}); err != nil {
		t.Fatal(err)
	}
	file, err := object.OpenFile(destination)
	if err != nil {
		t.Fatal(err)
	}
	defer file.Close()
	if file.TransferSyntax.UID != transfer.DeflatedExplicitVRLittleEndian.UID {
		t.Fatalf("published syntax %s", file.TransferSyntax.UID)
	}
	want := nativeFrames(t, openCorpusFile(t, "MR_small.dcm").Dataset)
	if got := nativeFrames(t, file.Dataset); !bytes.Equal(got[0], want[0]) {
		t.Fatal("published Deflated file changed samples")
	}
}
