package jpeg_test

import (
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"slices"
	"testing"

	"github.com/ThalesMMS/dicom-go/core"
	"github.com/ThalesMMS/dicom-go/internal/dcmtkoracle"
	"github.com/ThalesMMS/dicom-go/internal/dicomtest"
	"github.com/ThalesMMS/dicom-go/object"
	"github.com/ThalesMMS/dicom-go/pixeldata"
	"github.com/ThalesMMS/dicom-go/pixeldata/builtin"
	"github.com/ThalesMMS/dicom-go/pixeldata/jpeglossless"
	"github.com/ThalesMMS/dicom-go/transfer"
)

// dcmcjpegImage is a native image dcmcjpeg compresses.
type dcmcjpegImage struct {
	samples, allocated, stored uint16
	photometric                string
}

const dcmcjpegRows, dcmcjpegColumns = 45, 61

// pixels is a smooth gradient with an edge and a little texture, which
// lossy coding keeps close.
func (m dcmcjpegImage) pixels() []byte {
	sampleBytes := int(m.allocated / 8)
	top := float64(int(1)<<m.stored - 1)
	out := make([]byte, dcmcjpegRows*dcmcjpegColumns*int(m.samples)*sampleBytes)
	i := 0
	for y := 0; y < dcmcjpegRows; y++ {
		for x := 0; x < dcmcjpegColumns; x++ {
			for s := 0; s < int(m.samples); s++ {
				v := (float64(x)/dcmcjpegColumns*0.6 + float64(y)/dcmcjpegRows*0.3 + float64(s)*0.05) * top
				if x > 30 && y > 20 {
					v = top * 0.85
				}
				v += 0.01 * top * math.Sin(float64(x*y+s))
				value := uint16(math.Max(0, math.Min(top, math.Round(v))))
				if sampleBytes == 2 {
					binary.LittleEndian.PutUint16(out[i:], value)
				} else {
					out[i] = byte(value)
				}
				i += sampleBytes
			}
		}
	}
	return out
}

func (m dcmcjpegImage) object(pixels []byte) *object.Object {
	vr := core.VROB
	if m.allocated > 8 {
		vr = core.VROW
	}
	elements := []core.Element{
		dicomtest.NewStringElement(core.NewTag(0x0008, 0x0016), core.VRUI, "1.2.840.10008.5.1.4.1.1.7"),
		dicomtest.NewStringElement(core.NewTag(0x0008, 0x0018), core.VRUI, "1.2.826.0.1.3680043.10.1.953.2"),
		dicomtest.Uint16Element(core.NewTag(0x0028, 0x0010), core.VRUS, nil, dcmcjpegRows),
		dicomtest.Uint16Element(core.NewTag(0x0028, 0x0011), core.VRUS, nil, dcmcjpegColumns),
		dicomtest.Uint16Element(core.NewTag(0x0028, 0x0002), core.VRUS, nil, m.samples),
		dicomtest.NewStringElement(core.NewTag(0x0028, 0x0004), core.VRCS, m.photometric),
		dicomtest.Uint16Element(core.NewTag(0x0028, 0x0100), core.VRUS, nil, m.allocated),
		dicomtest.Uint16Element(core.NewTag(0x0028, 0x0101), core.VRUS, nil, m.stored),
		dicomtest.Uint16Element(core.NewTag(0x0028, 0x0102), core.VRUS, nil, m.stored-1),
		dicomtest.Uint16Element(core.NewTag(0x0028, 0x0103), core.VRUS, nil, 0),
		core.NewRawElement(core.TagPixelData, vr, pixels),
	}
	if m.samples == 3 {
		elements = append(elements, dicomtest.Uint16Element(core.NewTag(0x0028, 0x0006), core.VRUS, nil, 0))
	}
	return object.FromElements(elements, nil)
}

// samples returns the sample values of a native frame, color-by-pixel.
func samples(t *testing.T, dataset *object.Object, m dcmcjpegImage) []int {
	t.Helper()
	raw, ok := dataset.GetRaw(core.TagPixelData)
	if !ok {
		t.Fatal("no Pixel Data")
	}
	count := dcmcjpegRows * dcmcjpegColumns * int(m.samples)
	out := make([]int, count)
	for i := range out {
		if m.allocated == 16 {
			out[i] = int(binary.LittleEndian.Uint16(raw[2*i:]))
		} else {
			out[i] = int(raw[i])
		}
	}
	if planar, err := dataset.GetInt(core.NewTag(0x0028, 0x0006)); err == nil && planar == 1 && m.samples == 3 {
		pixels := dcmcjpegRows * dcmcjpegColumns
		interleaved := make([]int, count)
		for p := 0; p < pixels; p++ {
			for s := 0; s < 3; s++ {
				interleaved[p*3+s] = out[s*pixels+p]
			}
		}
		out = interleaved
	}
	return out
}

// decodeFile decodes a compressed file with the built-in decoders.
func decodeFile(path string, recovery pixeldata.DecodeRecovery) (*object.Object, pixeldata.TranscodeReport, error) {
	file, err := object.OpenFile(path)
	if err != nil {
		return nil, pixeldata.TranscodeReport{}, err
	}
	decoders, err := builtin.NewRegistry()
	if err != nil {
		return nil, pixeldata.TranscodeReport{}, err
	}
	return pixeldata.TranscodeDataSet(context.Background(), file.Dataset, file.TransferSyntax, transfer.ExplicitVRLittleEndian,
		pixeldata.TranscodeOptions{DecoderRegistry: decoders, AllowLossy: true, Recovery: recovery})
}

// decodeWithRegistry decodes a compressed file with the built-in decoders
// and returns its samples and photometric interpretation.
func decodeWithRegistry(t *testing.T, path string, m dcmcjpegImage) ([]int, string) {
	t.Helper()
	native, _, err := decodeFile(path, pixeldata.DecodeRecovery{})
	if err != nil {
		t.Fatalf("decoding %s: %v", filepath.Base(path), err)
	}
	photometric, _ := native.GetString(core.NewTag(0x0028, 0x0004))
	return samples(t, native, m), photometric
}

func writeNative(t *testing.T, path string, dataset *object.Object) {
	t.Helper()
	var encoded bytes.Buffer
	if err := object.WriteFile(&encoded, &object.File{Dataset: dataset, TransferSyntax: transfer.ExplicitVRLittleEndian}); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, encoded.Bytes(), 0o600); err != nil {
		t.Fatal(err)
	}
}

// TestDecodersAgainstDCMCJPEGLossless decodes the JPEG Lossless streams
// DCMTK dcmcjpeg writes (process 14 SV1 and every selection value of
// process 14, 8 to 16 bits, gray and RGB) and requires the original
// samples. dcmcjpeg's default "true lossless" codec compresses the whole
// Bits Allocated word, so a 12-bit image gets a 16-bit SOF3: strict decoding
// refuses the disagreement and DecodeRecovery.ReconcileWithCodestream
// (opt-in, reported) decodes it exactly. Enable with
// DICOMGO_DCMTK_INTEGRATION=1.
func TestDecodersAgainstDCMCJPEGLossless(t *testing.T) {
	dcmcjpeg := dcmtkoracle.Tool(t, "dcmcjpeg")
	dir := t.TempDir()
	images := map[string]dcmcjpegImage{
		"mono8":  {1, 8, 8, "MONOCHROME2"},
		"mono12": {1, 16, 12, "MONOCHROME2"},
		"mono16": {1, 16, 16, "MONOCHROME2"},
		"rgb8":   {3, 8, 8, "RGB"},
	}
	for name, m := range images {
		pixels := m.pixels()
		input := filepath.Join(dir, name+".dcm")
		writeNative(t, input, m.object(pixels))
		want := samples(t, m.object(pixels), m)
		options := [][]string{{"+e1"}}
		for sv := 1; sv <= 7; sv++ {
			options = append(options, []string{"+el", "+sv", fmt.Sprint(sv)})
		}
		for _, opts := range options {
			label := fmt.Sprintf("%s%v", name, opts)
			output := filepath.Join(dir, fmt.Sprintf("%s-%s.dcm", name, opts[len(opts)-1]))
			dcmtkoracle.Run(t, dcmtkoracle.Exact, dcmcjpeg, append(append([]string{}, opts...), input, output)...)
			var got []int
			if m.stored < m.allocated {
				if _, _, err := decodeFile(output, pixeldata.DecodeRecovery{}); !errors.Is(err, jpeglossless.ErrImageSizeMismatch) {
					t.Fatalf("%s: strict decoding of a 16-bit SOF3 for Bits Stored %d: %v", label, m.stored, err)
				}
				native, report, err := decodeFile(output, pixeldata.DecodeRecovery{ReconcileWithCodestream: true})
				if err != nil {
					t.Fatalf("%s: %v", label, err)
				}
				if !slices.Contains(report.DecodeDiagnostics, pixeldata.DiagnosticReconciledWithCodestream) {
					t.Fatalf("%s: reconciliation not reported: %v", label, report.DecodeDiagnostics)
				}
				got = samples(t, native, m)
			} else {
				got, _ = decodeWithRegistry(t, output, m)
			}
			for i := range want {
				if got[i] != want[i] {
					t.Fatalf("%s: sample %d is %d, want %d", label, i, got[i], want[i])
				}
			}
		}
	}
}

// TestDecodersAgainstDCMCJPEGLossy decodes the baseline (8-bit), extended
// (12-bit) and retired 8-bit spectral selection (.53) and full progression
// (.55) streams DCMTK dcmcjpeg writes and compares the samples
// with dcmdjpeg's decoding of the same stream, within one level for 8-bit
// gray and two for RGB (the YCbCr conversion rounds differently) and for
// 12 bits (IDCT rounding differs between implementations); the
// PSNR against the original must stay above 35 dB. Lossy output is never
// compared byte for byte. Enable with DICOMGO_DCMTK_INTEGRATION=1.
func TestDecodersAgainstDCMCJPEGLossy(t *testing.T) {
	dcmcjpeg := dcmtkoracle.Tool(t, "dcmcjpeg")
	dcmdjpeg := dcmtkoracle.Tool(t, "dcmdjpeg")
	dir := t.TempDir()
	for _, tc := range []struct {
		name    string
		image   dcmcjpegImage
		process string
		limit   int
	}{
		{"mono8-baseline", dcmcjpegImage{1, 8, 8, "MONOCHROME2"}, "+eb", 1},
		{"rgb8-baseline", dcmcjpegImage{3, 8, 8, "RGB"}, "+eb", 2},
		{"mono12-extended", dcmcjpegImage{1, 16, 12, "MONOCHROME2"}, "+ee", 2},
		// Retired 8-bit progressive: spectral selection (.53) and full
		// progression (.55), both Huffman SOF2.
		{"mono8-spectral", dcmcjpegImage{1, 8, 8, "MONOCHROME2"}, "+es", 1},
		{"rgb8-spectral", dcmcjpegImage{3, 8, 8, "RGB"}, "+es", 2},
		{"mono8-progressive", dcmcjpegImage{1, 8, 8, "MONOCHROME2"}, "+ep", 1},
		{"rgb8-progressive", dcmcjpegImage{3, 8, 8, "RGB"}, "+ep", 2},
	} {
		t.Run(tc.name, func(t *testing.T) {
			m := tc.image
			pixels := m.pixels()
			input, compressed, reference := filepath.Join(dir, tc.name+".dcm"), filepath.Join(dir, tc.name+"-jpeg.dcm"), filepath.Join(dir, tc.name+"-dcmtk.dcm")
			writeNative(t, input, m.object(pixels))
			tolerance := fmt.Sprintf("within %d levels of dcmdjpeg, PSNR >= 35 dB", tc.limit)
			dcmtkoracle.Run(t, tolerance, dcmcjpeg, tc.process, "+q", "90", input, compressed)
			dcmtkoracle.Run(t, tolerance, dcmdjpeg, compressed, reference)
			referenceFile, err := object.OpenFile(reference)
			if err != nil {
				t.Fatal(err)
			}
			want := samples(t, referenceFile.Dataset, m)
			got, photometric := decodeWithRegistry(t, compressed, m)
			if m.samples == 3 && photometric != "RGB" {
				t.Skipf("the decoder keeps %s; dcmdjpeg converts to RGB", photometric)
			}
			worst := 0
			for i := range want {
				worst = max(worst, abs(got[i]-want[i]))
			}
			if worst > tc.limit {
				t.Fatalf("samples differ from dcmdjpeg by up to %d", worst)
			}
			original := samples(t, m.object(pixels), m)
			var squared float64
			for i := range original {
				d := float64(got[i] - original[i])
				squared += d * d
			}
			peak := float64(int(1)<<m.stored - 1)
			if psnr := 10 * math.Log10(peak*peak/(squared/float64(len(original)))); psnr < 35 {
				t.Fatalf("PSNR %.1f dB", psnr)
			} else {
				t.Logf("worst difference from dcmdjpeg %d, PSNR %.1f dB", worst, psnr)
			}
		})
	}
}

// TestDecodersAgainstDCMCJPEG12BitProgressive checks both retired 12-bit
// monochrome processes against the independently executed DCMTK IJG12 decoder.
// Enable with DICOMGO_DCMTK_INTEGRATION=1.
func TestDecodersAgainstDCMCJPEG12BitProgressive(t *testing.T) {
	dcmcjpeg := dcmtkoracle.Tool(t, "dcmcjpeg")
	dcmdjpeg := dcmtkoracle.Tool(t, "dcmdjpeg")
	dir := t.TempDir()
	m := dcmcjpegImage{1, 16, 12, "MONOCHROME2"}
	input := filepath.Join(dir, "mono12.dcm")
	writeNative(t, input, m.object(m.pixels()))
	for _, process := range []string{"+es", "+ep"} {
		output := filepath.Join(dir, "mono12"+process+".dcm")
		reference := filepath.Join(dir, "mono12"+process+"-reference.dcm")
		dcmtkoracle.Run(t, dcmtkoracle.Accepted, dcmcjpeg, process, "+bt", "+sr", "+q", "90", input, output)
		dcmtkoracle.Run(t, dcmtkoracle.Accepted, dcmdjpeg, "+px", "-bs", output, reference)
		got, _ := decodeWithRegistry(t, output, m)
		file, err := object.OpenFile(reference)
		if err != nil {
			t.Fatal(err)
		}
		want := samples(t, file.Dataset, m)
		_ = file.Close()
		for i := range want {
			if abs(got[i]-want[i]) > 2 {
				t.Fatalf("%s sample%d got%d want%d", process, i, got[i], want[i])
			}
		}
	}
}

func abs(v int) int {
	if v < 0 {
		return -v
	}
	return v
}
