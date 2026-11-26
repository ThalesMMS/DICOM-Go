package rle

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"path/filepath"
	"testing"

	"github.com/ThalesMMS/dicom-go/core"
	"github.com/ThalesMMS/dicom-go/internal/dcmtkoracle"
	"github.com/ThalesMMS/dicom-go/internal/dicomtest"
	"github.com/ThalesMMS/dicom-go/object"
	"github.com/ThalesMMS/dicom-go/pixeldata"
	"github.com/ThalesMMS/dicom-go/transfer"
)

// oracleCase is a native image the RLE oracles encode and decode.
type oracleCase struct {
	name                   string
	samples, allocated     uint16
	stored, signed, frames uint16
	photometric            string
	rows, columns          uint16
}

func oracleCases() []oracleCase {
	return []oracleCase{
		{name: "mono8", samples: 1, allocated: 8, stored: 8, frames: 1, photometric: "MONOCHROME2", rows: 37, columns: 53},
		{name: "mono12", samples: 1, allocated: 16, stored: 12, frames: 2, photometric: "MONOCHROME2", rows: 31, columns: 40},
		{name: "mono16-signed", samples: 1, allocated: 16, stored: 16, signed: 1, frames: 1, photometric: "MONOCHROME1", rows: 29, columns: 64},
		{name: "rgb8", samples: 3, allocated: 8, stored: 8, frames: 2, photometric: "RGB", rows: 19, columns: 33},
		{name: "mono32", samples: 1, allocated: 32, stored: 32, frames: 1, photometric: "MONOCHROME2", rows: 17, columns: 21},
	}
}

// oraclePixels mixes long runs, short runs and noise so both RLE packet
// kinds and the 128-byte limits occur.
func oraclePixels(c oracleCase) []byte {
	sampleBytes := int(c.allocated / 8)
	size := int(c.rows) * int(c.columns) * int(c.samples) * sampleBytes * int(c.frames)
	out := make([]byte, size)
	state := uint32(0x9E3779B9)
	for i := 0; i < size; i++ {
		switch region := (i / 97) % 4; region {
		case 0:
			out[i] = byte(i / 300)
		case 1:
			state = state*1664525 + 1013904223
			out[i] = byte(state >> 24)
		case 2:
			out[i] = byte(i/2) & 0xF0
		default:
			out[i] = 0x80
		}
	}
	if c.stored < c.allocated {
		// Keep the bits above Bits Stored clear, as DCMTK does on decode.
		for i := 0; i+1 < size; i += 2 {
			out[i+1] &= byte(1<<(c.stored-8) - 1)
		}
	}
	return out
}

func (c oracleCase) object(pixels []byte) *object.Object {
	vr := core.VROB
	if c.allocated > 8 {
		vr = core.VROW
	}
	elements := []core.Element{
		dicomtest.NewStringElement(core.NewTag(0x0008, 0x0016), core.VRUI, "1.2.840.10008.5.1.4.1.1.7"),
		dicomtest.NewStringElement(core.NewTag(0x0008, 0x0018), core.VRUI, "1.2.826.0.1.3680043.10.1.953.1"),
		dicomtest.NewStringElement(core.NewTag(0x0028, 0x0008), core.VRIS, fmt.Sprint(c.frames)),
		dicomtest.Uint16Element(core.NewTag(0x0028, 0x0010), core.VRUS, nil, c.rows),
		dicomtest.Uint16Element(core.NewTag(0x0028, 0x0011), core.VRUS, nil, c.columns),
		dicomtest.Uint16Element(core.NewTag(0x0028, 0x0002), core.VRUS, nil, c.samples),
		dicomtest.NewStringElement(core.NewTag(0x0028, 0x0004), core.VRCS, c.photometric),
		dicomtest.Uint16Element(core.NewTag(0x0028, 0x0100), core.VRUS, nil, c.allocated),
		dicomtest.Uint16Element(core.NewTag(0x0028, 0x0101), core.VRUS, nil, c.stored),
		dicomtest.Uint16Element(core.NewTag(0x0028, 0x0102), core.VRUS, nil, c.stored-1),
		dicomtest.Uint16Element(core.NewTag(0x0028, 0x0103), core.VRUS, nil, c.signed),
		core.NewRawElement(core.TagPixelData, vr, pixels),
	}
	if c.samples == 3 {
		elements = append(elements, dicomtest.Uint16Element(core.NewTag(0x0028, 0x0006), core.VRUS, nil, 0))
	}
	return object.FromElements(elements, nil)
}

func writeOracleFile(t *testing.T, path string, dataset *object.Object, syntax transfer.Syntax) {
	t.Helper()
	var encoded bytes.Buffer
	if err := object.WriteFile(&encoded, &object.File{Dataset: dataset, TransferSyntax: syntax}); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, encoded.Bytes(), 0o600); err != nil {
		t.Fatal(err)
	}
}

func oracleRegistries(t *testing.T) pixeldata.TranscodeOptions {
	t.Helper()
	decoders := pixeldata.NewMemoryRegistry()
	if err := Register(decoders); err != nil {
		t.Fatal(err)
	}
	encoders := pixeldata.NewMemoryEncoderRegistry()
	if err := RegisterEncoder(encoders); err != nil {
		t.Fatal(err)
	}
	return pixeldata.TranscodeOptions{DecoderRegistry: decoders, EncoderRegistry: encoders}
}

// nativePixels returns the Pixel Data of a native file, color-by-pixel.
func nativePixels(t *testing.T, path string, c oracleCase) []byte {
	t.Helper()
	file, err := object.OpenFile(path)
	if err != nil {
		t.Fatal(err)
	}
	raw, ok := file.Dataset.GetRaw(core.TagPixelData)
	if !ok {
		t.Fatalf("%s has no Pixel Data", path)
	}
	want := len(oraclePixels(c))
	if len(raw) == want+1 {
		raw = raw[:want]
	}
	if planar, err := file.Dataset.GetInt(core.NewTag(0x0028, 0x0006)); err == nil && planar == 1 && c.samples == 3 {
		raw = interleave(raw, c)
	}
	return raw
}

// interleave converts color-by-plane frames to color-by-pixel.
func interleave(planar []byte, c oracleCase) []byte {
	pixels := int(c.rows) * int(c.columns)
	out := make([]byte, len(planar))
	for frame := 0; frame < int(c.frames); frame++ {
		base := frame * pixels * 3
		for p := 0; p < pixels; p++ {
			for s := 0; s < 3; s++ {
				out[base+p*3+s] = planar[base+s*pixels+p]
			}
		}
	}
	return out
}

// TestRLEEncoderAgainstDCMTK has DCMTK dcmdrle decode the RLE Lossless
// streams this package encodes (8, 16 and 32 bits, signed, RGB,
// multi-frame) and compares the samples bit for bit. Enable with
// DICOMGO_DCMTK_INTEGRATION=1.
func TestRLEEncoderAgainstDCMTK(t *testing.T) {
	dcmdrle := dcmtkoracle.Tool(t, "dcmdrle")
	opts := oracleRegistries(t)
	dir := t.TempDir()
	for _, c := range oracleCases() {
		t.Run(c.name, func(t *testing.T) {
			pixels := oraclePixels(c)
			encoded, _, err := pixeldata.TranscodeDataSet(context.Background(), c.object(pixels), transfer.ExplicitVRLittleEndian, transfer.RLELossless, opts)
			if err != nil {
				t.Fatal(err)
			}
			input, output := filepath.Join(dir, c.name+"-rle.dcm"), filepath.Join(dir, c.name+"-native.dcm")
			writeOracleFile(t, input, encoded, transfer.RLELossless)
			dcmtkoracle.Run(t, dcmtkoracle.Exact, dcmdrle, input, output)
			if !bytes.Equal(nativePixels(t, output, c), pixels) {
				t.Fatal("dcmdrle decodes different samples")
			}
		})
	}
}

// TestRLEDecoderAgainstDCMTK decodes the RLE Lossless streams DCMTK
// dcmcrle writes and compares the samples with the originals bit for bit.
// Enable with DICOMGO_DCMTK_INTEGRATION=1.
func TestRLEDecoderAgainstDCMTK(t *testing.T) {
	dcmcrle := dcmtkoracle.Tool(t, "dcmcrle")
	opts := oracleRegistries(t)
	dir := t.TempDir()
	for _, c := range oracleCases() {
		t.Run(c.name, func(t *testing.T) {
			pixels := oraclePixels(c)
			input, output := filepath.Join(dir, c.name+"-native.dcm"), filepath.Join(dir, c.name+"-rle.dcm")
			writeOracleFile(t, input, c.object(pixels), transfer.ExplicitVRLittleEndian)
			dcmtkoracle.Run(t, dcmtkoracle.Exact, dcmcrle, input, output)
			file, err := object.OpenFile(output)
			if err != nil {
				t.Fatal(err)
			}
			if file.TransferSyntax.UID != transfer.RLELossless.UID {
				t.Fatalf("dcmcrle wrote %s", file.TransferSyntax.UID)
			}
			native, _, err := pixeldata.TranscodeDataSet(context.Background(), file.Dataset, transfer.RLELossless, transfer.ExplicitVRLittleEndian, opts)
			if err != nil {
				t.Fatal(err)
			}
			raw, _ := native.GetRaw(core.TagPixelData)
			if len(raw) == len(pixels)+1 {
				raw = raw[:len(pixels)]
			}
			if planar, err := native.GetInt(core.NewTag(0x0028, 0x0006)); err == nil && planar == 1 && c.samples == 3 {
				raw = interleave(raw, c)
			}
			if !bytes.Equal(raw, pixels) {
				t.Fatal("decoding dcmcrle's stream gives different samples")
			}
		})
	}
}
