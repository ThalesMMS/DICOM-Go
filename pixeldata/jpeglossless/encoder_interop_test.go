package jpeglossless

import (
	"bytes"
	"context"
	"encoding/binary"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/ThalesMMS/dicom-go/core"
	"github.com/ThalesMMS/dicom-go/internal/dcmtkoracle"
	"github.com/ThalesMMS/dicom-go/internal/dicomtest"
	"github.com/ThalesMMS/dicom-go/object"
	"github.com/ThalesMMS/dicom-go/pixeldata"
	"github.com/ThalesMMS/dicom-go/transfer"
)

func nativeObject(metadata pixeldata.Metadata, frame []byte) *object.Object {
	vr := core.VROB
	if metadata.BitsAllocated == 16 {
		vr = core.VROW
	}
	elements := []core.Element{
		dicomtest.NewStringElement(core.NewTag(0x0008, 0x0016), core.VRUI, "1.2.840.10008.5.1.4.1.1.7"),
		dicomtest.NewStringElement(core.NewTag(0x0008, 0x0018), core.VRUI, "1.2.826.0.1.3680043.10.1.978.1"),
		dicomtest.Uint16Element(core.NewTag(0x0028, 0x0010), core.VRUS, nil, metadata.Rows),
		dicomtest.Uint16Element(core.NewTag(0x0028, 0x0011), core.VRUS, nil, metadata.Columns),
		dicomtest.Uint16Element(core.NewTag(0x0028, 0x0002), core.VRUS, nil, metadata.SamplesPerPixel),
		dicomtest.NewStringElement(core.NewTag(0x0028, 0x0004), core.VRCS, metadata.PhotometricInterpretation),
		dicomtest.Uint16Element(core.NewTag(0x0028, 0x0100), core.VRUS, nil, metadata.BitsAllocated),
		dicomtest.Uint16Element(core.NewTag(0x0028, 0x0101), core.VRUS, nil, metadata.BitsStored),
		dicomtest.Uint16Element(core.NewTag(0x0028, 0x0102), core.VRUS, nil, metadata.HighBit),
		dicomtest.Uint16Element(core.NewTag(0x0028, 0x0103), core.VRUS, nil, metadata.PixelRepresentation),
		core.NewRawElement(core.TagPixelData, vr, frame),
	}
	if metadata.SamplesPerPixel == 3 {
		elements = append(elements, dicomtest.Uint16Element(core.NewTag(0x0028, 0x0006), core.VRUS, nil, 0))
	}
	return object.FromElements(elements, nil)
}

func transcodeRegistries(t testing.TB) (pixeldata.Registry, pixeldata.EncoderRegistry) {
	t.Helper()
	decoders := pixeldata.NewMemoryRegistry()
	if err := Register(decoders); err != nil {
		t.Fatal(err)
	}
	encoders := pixeldata.NewMemoryEncoderRegistry()
	if err := RegisterEncoder(encoders); err != nil {
		t.Fatal(err)
	}
	return decoders, encoders
}

// TestTranscodeNativeToJPEGLosslessAndBack goes through the dataset
// transcoder for .70 and .57 and back to native.
func TestTranscodeNativeToJPEGLosslessAndBack(t *testing.T) {
	decoders, encoders := transcodeRegistries(t)
	for _, target := range []transfer.Syntax{transfer.JPEGLosslessSV1, transfer.JPEGLosslessNonHierarchical} {
		metadata := encoderTestMetadata(24, 31, 3, 8, 8, 0, "RGB")
		frame := encoderTestFrame(metadata, 7)
		opts := pixeldata.TranscodeOptions{DecoderRegistry: decoders, EncoderRegistry: encoders}
		encoded, report, err := pixeldata.TranscodeDataSet(context.Background(), nativeObject(metadata, frame), transfer.ExplicitVRLittleEndian, target, opts)
		if err != nil {
			t.Fatalf("%s: %v", target.UID, err)
		}
		if report.Lossy {
			t.Fatalf("%s reported lossy", target.UID)
		}
		back, _, err := pixeldata.TranscodeDataSet(context.Background(), encoded, target, transfer.ExplicitVRLittleEndian, opts)
		if err != nil {
			t.Fatal(err)
		}
		raw, _ := back.GetRaw(core.TagPixelData)
		if !bytes.Equal(raw, frame) {
			t.Fatalf("%s: transcode round trip differs", target.UID)
		}
	}
}

// TestJPEGLosslessEncoderAgainstDCMTK has dcmdjpeg decode the encoder's
// streams (every predictor, 8/12/16 bits, gray and color, signed) and
// compares the pixels bit for bit. Enable with DICOMGO_DCMTK_INTEGRATION=1.
func TestJPEGLosslessEncoderAgainstDCMTK(t *testing.T) {
	dcmdjpeg := dcmtkoracle.Tool(t, "dcmdjpeg")
	_, encoders := transcodeRegistries(t)
	dir := t.TempDir()
	type combination struct {
		predictor               int
		samples, stored, signed uint16
	}
	var combinations []combination
	for predictor := 1; predictor <= 7; predictor++ {
		combinations = append(combinations, combination{predictor, 1, 12, 0}, combination{predictor, 1, 16, 1}, combination{predictor, 3, 8, 0})
	}
	combinations = append(combinations, combination{1, 1, 8, 1}, combination{1, 1, 2, 0}, combination{1, 1, 15, 0}, combination{1, 3, 16, 0})
	for _, c := range combinations {
		name := fmt.Sprintf("sv%d-%dspp-%dbit-signed%d", c.predictor, c.samples, c.stored, c.signed)
		allocated := uint16(8)
		if c.stored > 8 {
			allocated = 16
		}
		photometric := "MONOCHROME2"
		if c.samples == 3 {
			photometric = "RGB"
		}
		metadata := encoderTestMetadata(37, 53, c.samples, allocated, c.stored, c.signed, photometric)
		frame := encoderTestFrame(metadata, c.predictor*31+int(c.stored))
		var target transfer.Syntax
		var encoder pixeldata.FrameEncoder
		if c.predictor == 1 {
			target = transfer.JPEGLosslessSV1
			encoder, _ = encoders.GetEncoder(target.UID)
		} else {
			target = transfer.JPEGLosslessNonHierarchical
			encoder, _ = NewProcess14Encoder(c.predictor)
		}
		registry := pixeldata.NewMemoryEncoderRegistry()
		if err := registry.RegisterEncoder(target.UID, encoder); err != nil {
			t.Fatal(err)
		}
		encoded, _, err := pixeldata.TranscodeDataSet(context.Background(), nativeObject(metadata, frame), transfer.ExplicitVRLittleEndian, target, pixeldata.TranscodeOptions{EncoderRegistry: registry})
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		input := filepath.Join(dir, name+".dcm")
		var buf bytes.Buffer
		if err := object.WriteFile(&buf, &object.File{Dataset: encoded, TransferSyntax: target}); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(input, buf.Bytes(), 0o600); err != nil {
			t.Fatal(err)
		}
		output := filepath.Join(dir, name+"-native.dcm")
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		out, err := dcmtkoracle.Command(ctx, t, dcmtkoracle.Exact, dcmdjpeg, input, output).CombinedOutput()
		cancel()
		if err != nil {
			t.Fatalf("%s: dcmdjpeg: %v: %s", name, err, out)
		}
		decoded, err := object.OpenFile(output)
		if err != nil {
			t.Fatal(err)
		}
		raw, _ := decoded.Dataset.GetRaw(core.TagPixelData)
		want := canonical(frame, metadata)
		if len(raw) == len(want)+1 {
			raw = raw[:len(want)]
		}
		if !bytes.Equal(maskedSamples(raw, metadata), want) {
			t.Errorf("%s: dcmdjpeg decodes different pixels", name)
		}
	}
}

// maskedSamples keeps the Bits Stored bits of each sample.
func maskedSamples(frame []byte, metadata pixeldata.Metadata) []byte {
	out := append([]byte(nil), frame...)
	mask := uint16(1)<<metadata.BitsStored - 1
	for i := 0; i+int(metadata.BitsAllocated/8) <= len(out); i += int(metadata.BitsAllocated / 8) {
		if metadata.BitsAllocated == 16 {
			binary.LittleEndian.PutUint16(out[i:], binary.LittleEndian.Uint16(out[i:])&mask)
		} else {
			out[i] &= byte(mask)
		}
	}
	return out
}
