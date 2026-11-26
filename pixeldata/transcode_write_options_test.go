package pixeldata_test

import (
	"bytes"
	"compress/flate"
	"context"
	"encoding/binary"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/ThalesMMS/dicom-go/core"
	"github.com/ThalesMMS/dicom-go/object"
	"github.com/ThalesMMS/dicom-go/pixeldata"
	"github.com/ThalesMMS/dicom-go/transfer"
)

func TestTranscodePathWriteOptionsAllRoutes(t *testing.T) {
	input := outputOptionsFile(t, 3, 1, 16)
	input.Dataset.Put(core.NewRawElement(core.NewTag(0xfffc, 0xfffc), core.VROB, []byte{0, 0}))
	input.Dataset.Put(core.Element{Header: core.ElementHeader{Tag: core.NewTag(8, 0x1111), VR: core.VRSQ}, Value: core.SequenceValue{Items: []core.DataSet{{Elements: []core.Element{core.NewRawElement(core.NewTag(0x0011, 0x1010), core.VROB, []byte{7, 8})}}, {}}}})
	before := outputOptionsBytes(t, input)
	dir := t.TempDir()
	source := writeStreamTestFile(t, dir, "source.dcm", input)
	registry := streamTestRegistries(t)
	encoded, _, err := pixeldata.TranscodeFile(context.Background(), input, transfer.JPEGLSLossless, registry)
	if err != nil {
		t.Fatal(err)
	}
	encodedSource := writeStreamTestFile(t, dir, "encoded.dcm", encoded)
	for _, c := range []struct {
		name, path string
		target     transfer.Syntax
		workers    int
		streaming  bool
	}{{"clone", source, transfer.ExplicitVRLittleEndian, 1, true}, {"encode", source, transfer.JPEGLSLossless, 1, true}, {"encode-parallel", source, transfer.JPEGLSLossless, 3, true}, {"decode", encodedSource, transfer.ExplicitVRLittleEndian, 1, true}, {"decode-padding-only", encodedSource, transfer.ExplicitVRLittleEndian, 1, true}, {"decode-encapsulated-uncompressed", encodedSource, transfer.EncapsulatedUncompressedExplicitVRLittleEndian, 1, true}, {"decode-parallel", encodedSource, transfer.ExplicitVRLittleEndian, 3, true}, {"encoded-clone", encodedSource, transfer.JPEGLSLossless, 1, true}, {"deflate", source, transfer.DeflatedExplicitVRLittleEndian, 1, false}, {"big-endian", source, transfer.ExplicitVRBigEndian, 1, false}} {
		t.Run(c.name, func(t *testing.T) {
			opts := streamTestRegistries(t)
			opts.Pipeline.Workers = c.workers
			opts.WriteOptions = object.WriteFileOptions{Padding: object.PaddingOptions{Policy: object.PaddingCreate, Alignment: 512, ItemAlignment: 32}, GroupLength: object.GroupLengthCreate}
			if c.name == "decode-padding-only" {
				opts.WriteOptions.GroupLength = object.GroupLengthPreserve
			}
			if c.target.Deflated {
				level := 0
				opts.WriteOptions.DeflateLevel = &level
			}
			destination := filepath.Join(dir, c.name+".dcm")
			report, err := pixeldata.TranscodePath(context.Background(), c.path, destination, c.target, opts)
			if err != nil {
				t.Fatal(err)
			}
			if report.Streaming != c.streaming {
				t.Fatalf("streaming=%v", report.Streaming)
			}
			b, err := os.ReadFile(destination)
			if err != nil {
				t.Fatal(err)
			}
			offset := 144 + int(binary.LittleEndian.Uint32(b[140:144]))
			plain := b[offset:]
			if c.target.Deflated {
				r := flate.NewReader(bytes.NewReader(plain))
				plain, err = io.ReadAll(r)
				_ = r.Close()
				if err != nil {
					t.Fatal(err)
				}
			}
			if (offset+len(plain))%512 != 0 {
				t.Fatal("WriteOptions absent from path")
			}
			result, err := object.OpenFile(destination)
			if err != nil {
				t.Fatal(err)
			}
			defer result.Close()
			frames, err := pixeldata.NewFrameReader(context.Background(), result, nil, pixeldata.FrameReaderOptions{Registry: opts.DecoderRegistry})
			if err != nil {
				t.Fatal(err)
			}
			defer frames.Close()
			original, _ := pixeldata.ExtractNativeFramesView(input.Dataset)
			for i := 0; i < 3; i++ {
				got, _, err := frames.Frame(context.Background(), i)
				if err != nil {
					t.Fatal(err)
				}
				if !bytes.Equal(got, original.Data[i]) {
					t.Fatalf("frame %d changed", i)
				}
			}
		})
	}
	after, _ := os.ReadFile(source)
	if !bytes.Equal(before, after) || !bytes.Equal(before, outputOptionsBytes(t, input)) {
		t.Fatal("source changed")
	}
}

func TestTranscodePathWriteOptionsFailurePreservesDestination(t *testing.T) {
	dir := t.TempDir()
	input := outputOptionsFile(t, 1, 1, 8)
	source := writeStreamTestFile(t, dir, "source.dcm", input)
	destination := filepath.Join(dir, "destination.dcm")
	old := []byte("existing destination")
	for _, mode := range []string{"padding-budget", "output-budget", "cancelled", "invalid-level"} {
		if err := os.WriteFile(destination, old, 0600); err != nil {
			t.Fatal(err)
		}
		opts := streamTestRegistries(t)
		opts.WriteOptions.Padding = object.PaddingOptions{Policy: object.PaddingCreate, Alignment: 4096}
		ctx := context.Background()
		switch mode {
		case "padding-budget":
			opts.WriteOptions.Padding.MaxAddedBytes = 1
		case "output-budget":
			opts.Limits.MaxOutputBytes = 1024
		case "cancelled":
			var cancel context.CancelFunc
			ctx, cancel = context.WithCancel(ctx)
			cancel()
		case "invalid-level":
			level := 10
			opts.WriteOptions.DeflateLevel = &level
		}
		if _, err := pixeldata.TranscodePath(ctx, source, destination, transfer.ExplicitVRLittleEndian, opts); err == nil {
			t.Fatalf("%s accepted", mode)
		} else if mode == "cancelled" && !errors.Is(err, context.Canceled) {
			t.Fatal(err)
		}
		got, _ := os.ReadFile(destination)
		if !bytes.Equal(got, old) {
			t.Fatalf("%s replaced destination", mode)
		}
		matches, _ := filepath.Glob(filepath.Join(dir, ".dicom-go-*"))
		if len(matches) != 0 {
			t.Fatal("temporary file leaked")
		}
	}
	level := 10
	_, err := pixeldata.TranscodePath(context.Background(), "missing-source", destination, transfer.ExplicitVRLittleEndian, pixeldata.TranscodeOptions{WriteOptions: object.WriteFileOptions{DeflateLevel: &level}})
	if err == nil || !strings.Contains(err.Error(), "DeflateLevel") {
		t.Fatal("invalid serialization options not checked before source")
	}
}

func TestTranscodeMaterializedWriteOptionsAreSerializationOnly(t *testing.T) {
	input := outputOptionsFile(t, 1, 1, 8)
	level := 10
	opts := pixeldata.TranscodeOptions{WriteOptions: object.WriteFileOptions{DeflateLevel: &level, Padding: object.PaddingOptions{Policy: 99}}}
	output, _, err := pixeldata.TranscodeFile(context.Background(), input, transfer.ExplicitVRLittleEndian, opts)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(outputOptionsBytes(t, input), outputOptionsBytes(t, output)) {
		t.Fatal("materialized API applied serialization options")
	}
}
