package deflateframe_test

import (
	"bytes"
	"compress/flate"
	"context"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/ThalesMMS/dicom-go/object"
	"github.com/ThalesMMS/dicom-go/pixeldata"
	"github.com/ThalesMMS/dicom-go/pixeldata/deflateframe"
	"github.com/ThalesMMS/dicom-go/transfer"
)

func TestDeflatedLateFrameFailureDoesNotPublishPartialOutput(t *testing.T) {
	const frameBytes = 12
	ctx := context.Background()
	syntax := transfer.DeflatedImageFrameCompression
	pixels := []byte{37, 2, 8, 11, 1, 102, 4, 19, 86, 255, 125, 201}
	img := mono8(3, 4, 3)
	encode := func(data []byte) []byte {
		t.Helper()
		out, err := deflateframe.DeflateFrame(data, flate.NoCompression)
		if err != nil {
			t.Fatal(err)
		}
		return out
	}
	valid := encode(pixels)
	lastPixels := bytes.Clone(pixels)
	lastPixels[0] = 41
	last := encode(lastPixels)
	badFrames := []struct {
		name     string
		fragment []byte
		cause    error
	}{
		{"truncated", valid[:8], deflateframe.ErrInvalidStream},
		{"reserved-deflate-block-type", []byte{7, 0}, deflateframe.ErrInvalidStream},
		{"short-inflation", encode(pixels[:frameBytes-1]), deflateframe.ErrShortFrame},
		{"long-inflation", encode(append(bytes.Clone(pixels), 0x90)), deflateframe.ErrExpansionLimit},
		{"concatenated-streams", append(bytes.Clone(valid), valid...), deflateframe.ErrTrailingData},
	}
	for _, tc := range badFrames {
		t.Run(tc.name, func(t *testing.T) {
			ds := img.encapsulated(nil, valid, tc.fragment, last)
			pixel, err := pixeldata.ExtractView(ds)
			if err != nil {
				t.Fatal(err)
			}
			frames, err := deflateframe.New().Decode(pixel, ds)
			if err == nil || !errors.Is(err, tc.cause) ||
				!strings.Contains(err.Error(), "frame 1") || len(frames.Data) != 0 || frames.Rows != 0 || frames.Columns != 0 {
				t.Fatalf("whole image decode produced %+v, %v, want empty frames and frame 1 %v", frames, err, tc.cause)
			}
			if tc.cause == deflateframe.ErrExpansionLimit {
				var overflow *deflateframe.ExpansionLimitError
				if !errors.As(err, &overflow) || overflow.Frame != 1 || overflow.Limit != frameBytes {
					t.Fatalf("expansion error detail = %+v", overflow)
				}
			}
			file := &object.File{Dataset: ds, TransferSyntax: syntax}
			reader, err := pixeldata.NewFrameReader(ctx, file, nil, pixeldata.FrameReaderOptions{Registry: decoders(t)})
			if err != nil {
				t.Fatal(err)
			}
			defer reader.Close()
			bad, _, err := reader.Frame(ctx, 1)
			if !errors.Is(err, tc.cause) || len(bad) != 0 {
				t.Fatalf("requested broken frame returned %x, %v", bad, err)
			}
			for _, good := range []struct {
				index  int
				pixels []byte
			}{{0, pixels}, {2, lastPixels}} {
				got, _, err := reader.Frame(ctx, good.index)
				if err != nil || !bytes.Equal(got, good.pixels) {
					t.Fatalf("intact frame %d returned %x, %v", good.index, got, err)
				}
			}
			opts := transcodeOptions(t)
			if ds, _, err := pixeldata.TranscodeDataSet(ctx, ds, syntax, transfer.ExplicitVRLittleEndian, opts); ds != nil || !errors.Is(err, tc.cause) {
				t.Fatalf("TranscodeDataSet returned %v, %v, want no dataset and %v", ds, err, tc.cause)
			}
			var wire bytes.Buffer
			if err := object.WriteFile(&wire, file); err != nil {
				t.Fatal(err)
			}
			dir := t.TempDir()
			source, output := filepath.Join(dir, "source.dcm"), filepath.Join(dir, "output.dcm")
			if err := os.WriteFile(source, wire.Bytes(), 0o600); err != nil {
				t.Fatal(err)
			}
			_, err = pixeldata.TranscodePath(ctx, source, output, transfer.ExplicitVRLittleEndian, opts)
			switch runtime.GOOS {
			case "android", "darwin", "ios", "linux", "windows":
				var transaction *pixeldata.TranscodeError
				if !errors.Is(err, pixeldata.ErrTranscodeTransaction) || !errors.As(err, &transaction) || transaction.Stage != "write" {
					t.Fatalf("TranscodePath error = %v, want a failed write transaction", err)
				}
			default:
				if err != pixeldata.ErrTranscodeUnsupported {
					t.Fatalf("TranscodePath error on %s = %v, want unsupported", runtime.GOOS, err)
				}
			}
			entries, err := os.ReadDir(dir)
			if err != nil {
				t.Fatal(err)
			}
			if len(entries) != 1 || entries[0].Name() != "source.dcm" {
				t.Fatalf("published an output or left staging entries: %v", entries)
			}
			if got, err := os.ReadFile(source); err != nil || !bytes.Equal(got, wire.Bytes()) {
				t.Fatal("source changed during failed transcode")
			}
		})
	}
}
