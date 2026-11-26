package jpeg

import (
	"bytes"
	"context"
	"encoding/binary"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"testing"

	"github.com/ThalesMMS/dicom-go/core"
	"github.com/ThalesMMS/dicom-go/internal/dcmtkoracle"
	"github.com/ThalesMMS/dicom-go/object"
	"github.com/ThalesMMS/dicom-go/pixeldata"
	"github.com/ThalesMMS/dicom-go/transfer"
)

// TestExtendedEncoderAgainstDCMTK transcodes the synthetic phantom to JPEG
// Extended Process 4 at each quality, decodes the written file with DCMTK
// dcmdjpeg and with the pure-Go decoder, and requires the two decodings to
// agree within two levels (IDCT rounding differs between implementations)
// and the PSNR against the original to stay above the quality's floor.
// dcmdjpeg leaves lossy overshoot above 2^BitsStored-1 unclamped where the
// pure-Go decoder clamps it; those samples are counted and clamped before
// the comparison. The PSNR, codestream size and recorded ratio per quality
// are logged for docs/JPEG_EXTENDED_ENCODER.md. When dcmcjpeg is present,
// its own +ee coding of the 12-bit source at the same quality (with +sr, as
// its default min/max scaling would remap the samples) is logged beside them
// for reference. Enable with DICOMGO_DCMTK_INTEGRATION=1.
func TestExtendedEncoderAgainstDCMTK(t *testing.T) {
	dcmdjpeg := dcmtkoracle.Tool(t, "dcmdjpeg")
	dcmcjpeg, haveDCMCJPEG := dcmtkoracle.Lookup("dcmcjpeg")
	dir := t.TempDir()
	const rows, columns, tolerance = 157, 191, 2
	floors := map[int]float64{25: 38, 50: 44, 75: 49, 90: 56, 95: 60, 100: 78}
	var table bytes.Buffer
	table.WriteString("| Bits Stored | Quality | Ratio | PSNR dcmdjpeg (dB) | PSNR pure Go (dB) | max \\|Go - dcmdjpeg\\| | dcmdjpeg samples above 2^BS-1 | Bytes | dcmcjpeg +ee bytes / PSNR (dB) |\n|---|---|---|---|---|---|---|---|---|\n")
	for _, bitsStored := range []uint16{12, 10} {
		// No bits above High Bit, so that dcmcjpeg codes the same samples.
		frame := extendedPhantomFrame(rows, columns, bitsStored, 1032)
		for i := 0; i < len(frame); i += 2 {
			binary.LittleEndian.PutUint16(frame[i:], binary.LittleEndian.Uint16(frame[i:])&(uint16(1)<<bitsStored-1))
		}
		original := extendedSamples(frame, bitsStored)
		source := nativeExtendedObject(rows, columns, bitsStored, 1, frame)
		nativePath := filepath.Join(dir, fmt.Sprintf("native%d.dcm", bitsStored))
		writeExtendedTestFile(t, nativePath, source, transfer.ExplicitVRLittleEndian)
		for _, quality := range []int{25, 50, 75, 90, 95, 100} {
			name := fmt.Sprintf("bits%d-q%d", bitsStored, quality)
			floor := floors[quality] - 6.1*float64(12-bitsStored)
			t.Run(name, func(t *testing.T) {
				encoders := pixeldata.NewMemoryEncoderRegistry()
				if err := RegisterExtendedEncoder(encoders, quality); err != nil {
					t.Fatal(err)
				}
				compressed, _, err := pixeldata.TranscodeDataSet(context.Background(), source, transfer.ExplicitVRLittleEndian, transfer.JPEGExtended, pixeldata.TranscodeOptions{EncoderRegistry: encoders, AllowLossy: true})
				if err != nil {
					t.Fatal(err)
				}
				ratio, _ := compressed.GetString(core.NewTag(0x0028, 0x2112))
				input, reference := filepath.Join(dir, name+".dcm"), filepath.Join(dir, name+"-dcmtk.dcm")
				writeExtendedTestFile(t, input, compressed, transfer.JPEGExtended)
				dcmtkoracle.Run(t, fmt.Sprintf("within %d levels of the pure-Go decoder after clamping to Bits Stored, PSNR >= %.1f dB", tolerance, floor), dcmdjpeg, input, reference)
				want, overshoot := extendedFileSamples(t, reference, bitsStored)

				file, err := object.OpenFile(input)
				if err != nil {
					t.Fatal(err)
				}
				decoders := pixeldata.NewMemoryRegistry()
				if err := Register(decoders); err != nil {
					t.Fatal(err)
				}
				decoded, err := pixeldata.DecompressFile(file, pixeldata.DecompressOptions{Registry: decoders})
				if err != nil {
					t.Fatal(err)
				}
				raw, ok := decoded.Dataset.GetRaw(core.TagPixelData)
				if !ok {
					t.Fatal("no native Pixel Data")
				}
				got := extendedSamples(raw, 16)
				_, worst := extendedErrors(got, want, bitsStored)
				dcmtkPSNR, _ := extendedErrors(want, original, bitsStored)
				goPSNR, _ := extendedErrors(got, original, bitsStored)
				if worst > tolerance || dcmtkPSNR < floor || goPSNR < floor {
					t.Fatalf("max difference from dcmdjpeg %d, PSNR dcmdjpeg %.1f / Go %.1f dB (floor %.1f)", worst, dcmtkPSNR, goPSNR, floor)
				}
				comparison := "-"
				if haveDCMCJPEG && bitsStored == 12 {
					encoded, redecoded := filepath.Join(dir, name+"-dcmcjpeg.dcm"), filepath.Join(dir, name+"-dcmcjpeg-dcmdjpeg.dcm")
					dcmtkoracle.Run(t, "reference coding, logged only", dcmcjpeg, "+ee", "+sr", "+q", strconv.Itoa(quality), nativePath, encoded)
					dcmtkoracle.Run(t, "reference coding, logged only", dcmdjpeg, encoded, redecoded)
					referenceFile, err := object.OpenFile(encoded)
					if err != nil {
						t.Fatal(err)
					}
					referenceSamples, _ := extendedFileSamples(t, redecoded, bitsStored)
					referencePSNR, _ := extendedErrors(referenceSamples, original, bitsStored)
					comparison = fmt.Sprintf("%d / %.1f", extendedCodestreamBytes(t, referenceFile.Dataset), referencePSNR)
				}
				fmt.Fprintf(&table, "| %d | %d | %s | %.1f | %.1f | %d | %d | %d | %s |\n", bitsStored, quality, ratio, dcmtkPSNR, goPSNR, worst, overshoot, extendedCodestreamBytes(t, compressed), comparison)
			})
		}
	}
	t.Logf("%dx%d synthetic phantom:\n%s", columns, rows, table.String())
}

func writeExtendedTestFile(t *testing.T, path string, dataset *object.Object, syntax transfer.Syntax) {
	t.Helper()
	var encoded bytes.Buffer
	if err := object.WriteFile(&encoded, &object.File{Dataset: dataset, TransferSyntax: syntax}); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, encoded.Bytes(), 0o600); err != nil {
		t.Fatal(err)
	}
}

// extendedCodestreamBytes is the size of the only frame of an encapsulated
// data set, without Item padding.
func extendedCodestreamBytes(t *testing.T, dataset *object.Object) int {
	t.Helper()
	pixel, err := pixeldata.ExtractView(dataset)
	if err != nil || !pixel.Encapsulated || len(pixel.Sequence.Fragments) != 1 {
		t.Fatalf("encapsulated Pixel Data: %v", err)
	}
	return bytes.LastIndex(pixel.Sequence.Fragments[0], []byte{0xff, 0xd9}) + 2
}

// extendedFileSamples reads a DCMTK-decoded native file, counting the
// samples above 2^bitsStored-1 and clamping them.
func extendedFileSamples(t *testing.T, path string, bitsStored uint16) ([]int, int) {
	t.Helper()
	file, err := object.OpenFile(path)
	if err != nil {
		t.Fatal(err)
	}
	raw, ok := file.Dataset.GetRaw(core.TagPixelData)
	if !ok {
		t.Fatal("no native Pixel Data")
	}
	samples, overshoot := extendedSamples(raw, 16), 0
	top := int(1)<<bitsStored - 1
	for i, v := range samples {
		if v > top {
			samples[i], overshoot = top, overshoot+1
		}
	}
	return samples, overshoot
}
