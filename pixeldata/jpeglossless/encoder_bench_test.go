package jpeglossless

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strconv"
	"testing"
	"time"

	"github.com/ThalesMMS/dicom-go/core"
	"github.com/ThalesMMS/dicom-go/internal/dcmtkoracle"
	"github.com/ThalesMMS/dicom-go/internal/dicomtest"
	"github.com/ThalesMMS/dicom-go/object"
	"github.com/ThalesMMS/dicom-go/pixeldata"
	"github.com/ThalesMMS/dicom-go/transfer"
)

// TestJPEGLosslessEncoderBenchmarkAgainstDCMCJPEG times the encoder and
// DCMTK dcmcjpeg +e1 (SV1) on the same multi-frame objects and logs the
// throughput and compressed sizes for docs/PIXEL_TRANSCODING_BENCHMARKS.md.
// Enable with DICOMGO_DCMTK_INTEGRATION=1 and DICOMGO_JPEGLOSSLESS_BENCH=1.
func TestJPEGLosslessEncoderBenchmarkAgainstDCMCJPEG(t *testing.T) {
	if os.Getenv("DICOMGO_JPEGLOSSLESS_BENCH") != "1" {
		t.Skip("opt-in: DICOMGO_DCMTK_INTEGRATION=1 DICOMGO_JPEGLOSSLESS_BENCH=1 with dcmcjpeg on PATH")
	}
	dcmcjpeg := dcmtkoracle.Tool(t, "dcmcjpeg")
	_, encoders := transcodeRegistries(t)
	dir := t.TempDir()
	for _, profile := range []struct {
		name     string
		metadata pixeldata.Metadata
		frames   int
	}{
		{"mr-512x512x12bit-40frames", encoderTestMetadata(512, 512, 1, 16, 12, 0, "MONOCHROME2"), 40},
		{"rgb-640x480x8bit-20frames", encoderTestMetadata(480, 640, 3, 8, 8, 0, "RGB"), 20},
	} {
		frame := smoothTestFrame(profile.metadata)
		native := bytes.Repeat(frame, profile.frames)
		obj := nativeObject(profile.metadata, native)
		obj.Put(dicomtest.NewStringElement(core.NewTag(0x0028, 0x0008), core.VRIS, strconv.Itoa(profile.frames)))
		started := time.Now()
		encoded, _, err := pixeldata.TranscodeDataSet(context.Background(), obj, transfer.ExplicitVRLittleEndian, transfer.JPEGLosslessSV1, pixeldata.TranscodeOptions{EncoderRegistry: encoders})
		goElapsed := time.Since(started)
		if err != nil {
			t.Fatal(err)
		}
		var goOut bytes.Buffer
		if err := object.WriteFile(&goOut, &object.File{Dataset: encoded, TransferSyntax: transfer.JPEGLosslessSV1}); err != nil {
			t.Fatal(err)
		}
		input := filepath.Join(dir, profile.name+".dcm")
		var nativeOut bytes.Buffer
		if err := object.WriteFile(&nativeOut, &object.File{Dataset: obj, TransferSyntax: transfer.ExplicitVRLittleEndian}); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(input, nativeOut.Bytes(), 0o600); err != nil {
			t.Fatal(err)
		}
		output := filepath.Join(dir, profile.name+"-dcmtk.dcm")
		started = time.Now()
		dcmtkoracle.Run(t, "throughput and size logged, not compared", dcmcjpeg, "+e1", input, output)
		dcmtkElapsed := time.Since(started)
		dcmtkInfo, _ := os.Stat(output)
		megabytes := float64(len(native)) / (1 << 20)
		t.Logf("%s: native %.1f MiB; dicom-go %.1f MiB/s, %d bytes; dcmcjpeg %.1f MiB/s (process), %d bytes",
			profile.name, megabytes, megabytes/goElapsed.Seconds(), goOut.Len(), megabytes/dcmtkElapsed.Seconds(), dcmtkInfo.Size())
	}
}
