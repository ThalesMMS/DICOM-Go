package jpeglossless

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"testing"
	"time"

	"github.com/ThalesMMS/dicom-go/core"
	"github.com/ThalesMMS/dicom-go/internal/dcmtkoracle"
	"github.com/ThalesMMS/dicom-go/internal/dicomtest"
	"github.com/ThalesMMS/dicom-go/object"
	"github.com/ThalesMMS/dicom-go/pixeldata"
	"github.com/ThalesMMS/dicom-go/transfer"
)

type losslessTiming struct {
	Profile      string `json:"profile"`
	Operation    string `json:"operation"`
	Iteration    int    `json:"iteration"`
	ElapsedNS    int64  `json:"elapsedNS"`
	NativeBytes  int    `json:"nativeBytes"`
	OutputBytes  int    `json:"outputBytes"`
	OutputSHA256 string `json:"outputSHA256"`
}

// This opt-in campaign times warm-process Go and fresh-process DCMTK Part10
// operations, including reading and writing files. It qualifies both encoders
// with both decoders before collecting five alternating timing samples.
func TestLosslessPerformanceCampaign(t *testing.T) {
	if os.Getenv("DICOMGO_JPEGLOSSLESS_CAMPAIGN") != "1" {
		t.Skip("set DICOMGO_JPEGLOSSLESS_CAMPAIGN=1 and DICOMGO_DCMTK_INTEGRATION=1")
	}
	encoderTool := dcmtkoracle.Tool(t, "dcmcjpeg")
	decoderTool := dcmtkoracle.Tool(t, "dcmdjpeg")
	dir := os.Getenv("DICOMGO_JPEGLOSSLESS_CAMPAIGN_DIR")
	if dir == "" {
		dir = t.TempDir()
	}
	if err := os.MkdirAll(dir, 0700); err != nil {
		t.Fatal(err)
	}
	decoders, encoders := transcodeRegistries(t)
	var timings []losslessTiming
	writeObject := func(path string, dataset *object.Object, syntax transfer.Syntax) {
		file, err := os.Create(path)
		if err != nil {
			t.Fatal(err)
		}
		writeErr := object.WriteFile(file, &object.File{Dataset: dataset, TransferSyntax: syntax})
		closeErr := file.Close()
		if writeErr != nil {
			t.Fatal(writeErr)
		}
		if closeErr != nil {
			t.Fatal(closeErr)
		}
	}
	goTranscode := func(input, output string, target transfer.Syntax, reconcile bool) {
		file, err := object.OpenFile(input)
		if err != nil {
			t.Fatal(err)
		}
		defer file.Close()
		converted, _, err := pixeldata.TranscodeDataSet(context.Background(), file.Dataset, file.TransferSyntax, target,
			pixeldata.TranscodeOptions{DecoderRegistry: decoders, EncoderRegistry: encoders, Recovery: pixeldata.DecodeRecovery{ReconcileWithCodestream: reconcile}})
		if err != nil {
			t.Fatal(err)
		}
		writeObject(output, converted, target)
	}
	for _, profile := range []struct {
		name     string
		metadata pixeldata.Metadata
		frames   int
	}{
		{"mr-512x512x12bit-40frames", encoderTestMetadata(512, 512, 1, 16, 12, 0, "MONOCHROME2"), 40},
		{"rgb-640x480x8bit-20frames", encoderTestMetadata(480, 640, 3, 8, 8, 0, "RGB"), 20},
	} {
		native := bytes.Repeat(smoothTestFrame(profile.metadata), profile.frames)
		input := filepath.Join(dir, profile.name+"-input.dcm")
		dataset := nativeObject(profile.metadata, native)
		dataset.Put(dicomtest.NewStringElement(core.NewTag(0x0028, 0x0008), core.VRIS, fmt.Sprint(profile.frames)))
		writeObject(input, dataset, transfer.ExplicitVRLittleEndian)
		verify := func(path string) {
			file, err := object.OpenFile(path)
			if err != nil {
				t.Fatal(err)
			}
			defer file.Close()
			raw, ok := file.Dataset.GetRaw(core.TagPixelData)
			if !ok || !bytes.Equal(raw, native) {
				t.Fatalf("%s: samples differ in %s", profile.name, path)
			}
		}
		goEncoded := filepath.Join(dir, profile.name+"-go.dcm")
		oracleEncoded := filepath.Join(dir, profile.name+"-dcmtk.dcm")
		goDecoded := filepath.Join(dir, profile.name+"-go-native.dcm")
		oracleDecoded := filepath.Join(dir, profile.name+"-dcmtk-native.dcm")
		goTranscode(input, goEncoded, transfer.JPEGLosslessSV1, false)
		dcmtkoracle.Run(t, dcmtkoracle.Exact, encoderTool, "+e1", input, oracleEncoded)
		for _, encoded := range []string{goEncoded, oracleEncoded} {
			goTranscode(encoded, goDecoded, transfer.ExplicitVRLittleEndian, encoded == oracleEncoded)
			verify(goDecoded)
			dcmtkoracle.Run(t, dcmtkoracle.Exact, decoderTool, encoded, oracleDecoded)
			verify(oracleDecoded)
		}
		for iteration := 0; iteration < 5; iteration++ {
			operations := []struct {
				name, output string
				run          func()
			}{
				{"go-encode-verified", goEncoded, func() { goTranscode(input, goEncoded, transfer.JPEGLosslessSV1, false) }},
				{"dcmtk-encode", oracleEncoded, func() { dcmtkoracle.Run(t, dcmtkoracle.Exact, encoderTool, "+e1", input, oracleEncoded) }},
				{"go-decode", goDecoded, func() { goTranscode(oracleEncoded, goDecoded, transfer.ExplicitVRLittleEndian, true) }},
				{"dcmtk-decode", oracleDecoded, func() { dcmtkoracle.Run(t, dcmtkoracle.Exact, decoderTool, oracleEncoded, oracleDecoded) }},
			}
			if iteration%2 != 0 {
				operations[0], operations[1] = operations[1], operations[0]
				operations[2], operations[3] = operations[3], operations[2]
			}
			for _, operation := range operations {
				started := time.Now()
				operation.run()
				elapsed := time.Since(started)
				output, err := os.ReadFile(operation.output)
				if err != nil {
					t.Fatal(err)
				}
				if operation.name == "go-decode" || operation.name == "dcmtk-decode" {
					verify(operation.output)
				} else {
					goTranscode(operation.output, goDecoded, transfer.ExplicitVRLittleEndian, operation.output == oracleEncoded)
					verify(goDecoded)
				}
				timings = append(timings, losslessTiming{profile.name, operation.name, iteration, elapsed.Nanoseconds(), len(native), len(output), fmt.Sprintf("%x", sha256.Sum256(output))})
			}
		}
	}
	receipt := struct {
		OS, Arch, GoVersion, EncoderVersion, DecoderVersion string
		Policy                                              string
		Samples                                             []losslessTiming
	}{runtime.GOOS, runtime.GOARCH, runtime.Version(), dcmtkoracle.Version(encoderTool), dcmtkoracle.Version(decoderTool),
		"warm process Go / fresh process DCMTK; Part10 parse+transcode+write+close, no fsync; Go default lossless verification retained; Go decoding of DCMTK MR explicitly reconciles SOF precision16 vs dataset BitsStored12 using existing ReconcileWithCodestream, while strict Go output uses precision12; cross-decoder exact samples qualified before timing; output readback verification and SHA256 excluded from timings", timings}
	encoded, err := json.MarshalIndent(receipt, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "campaign.json"), encoded, 0600); err != nil {
		t.Fatal(err)
	}
	t.Logf("qualified %d timed outputs; receipt %s", len(timings), filepath.Join(dir, "campaign.json"))
}
