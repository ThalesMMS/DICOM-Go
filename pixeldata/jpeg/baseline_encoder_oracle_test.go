package jpeg

import (
	"bytes"
	"context"
	"fmt"
	"image"
	"image/color"
	stdjpeg "image/jpeg"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"

	"github.com/ThalesMMS/dicom-go/core"
	"github.com/ThalesMMS/dicom-go/internal/dcmtkoracle"
	"github.com/ThalesMMS/dicom-go/object"
	"github.com/ThalesMMS/dicom-go/pixeldata"
	"github.com/ThalesMMS/dicom-go/transfer"
)

// Both independent decoders must reconstruct the written synthetic DICOM,
// including its actual SOF sampling, dimensions and color attributes. These
// tests are opt-in, require no network/PACS and never download fixtures.
func TestBaselineEncoderAgainstDCMTK(t *testing.T) {
	tool := dcmtkoracle.Tool(t, "dcmdjpeg")
	testBaselineOracle(t, "DCMTK", func(t *testing.T, input, output string) {
		dcmtkoracle.Run(t, "dimensions/metadata, mean pixel difference <= 1.5 mono or 4 RGB, quality-dependent MAE against source", tool, "+cn", input, output)
	})
}

// Set DICOMGO_GDCM_INTEGRATION=1 and optionally DICOMGO_GDCM_BIN to the
// directory containing gdcmconv. Versions and comparison metrics are logged.
func TestBaselineEncoderAgainstGDCM(t *testing.T) {
	if os.Getenv("DICOMGO_GDCM_INTEGRATION") != "1" {
		t.Skip("opt-in: DICOMGO_GDCM_INTEGRATION=1 with gdcmconv on PATH or in DICOMGO_GDCM_BIN")
	}
	tool := ""
	if dir := os.Getenv("DICOMGO_GDCM_BIN"); dir != "" {
		candidate := filepath.Join(dir, "gdcmconv")
		if info, err := os.Stat(candidate); err == nil && !info.IsDir() {
			tool = candidate
		}
	}
	if tool == "" {
		var err error
		tool, err = exec.LookPath("gdcmconv")
		if err != nil {
			t.Skipf("gdcmconv unavailable: %v", err)
		}
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	version, err := exec.CommandContext(ctx, tool, "--version").CombinedOutput()
	if err != nil {
		t.Fatalf("gdcmconv version: %v: %s", err, version)
	}
	t.Logf("GDCM oracle %s at %s", version, tool)
	testBaselineOracle(t, "GDCM", func(t *testing.T, input, output string) {
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		out, err := exec.CommandContext(ctx, tool, "--raw", input, output).CombinedOutput()
		if err != nil {
			t.Fatalf("gdcmconv --raw: %v: %s", err, out)
		}
	})
}

func testBaselineOracle(t *testing.T, oracle string, decode func(*testing.T, string, string)) {
	for _, samples := range []uint16{1, 3} {
		for _, sampling := range []Subsampling{Subsampling420, Subsampling422, Subsampling444} {
			if samples == 1 && sampling != Subsampling420 {
				continue
			}
			for _, quality := range []int{25, 90, 100} {
				t.Run(fmt.Sprintf("samples%d/sampling%d/q%d", samples, sampling, quality), func(t *testing.T) {
					const rows, columns = 45, 61 // Both edges need extension.
					frame := baselinePhantom(columns, rows, int(samples), "smooth")
					var planar *uint16
					photometric := "MONOCHROME2"
					if samples == 3 {
						zero := uint16(0)
						planar, photometric = &zero, "RGB"
					}
					source := nativeJPEGObject(rows, columns, samples, photometric, planar, frame)
					for tag, value := range map[core.Tag]string{
						core.NewTag(0x0008, 0x0016): "1.2.840.10008.5.1.4.1.1.7",
						core.NewTag(0x0008, 0x0018): "1.2.826.0.1.3680043.10.1.1280.1",
					} {
						if err := source.SetStrings(tag, core.VRUI, value); err != nil {
							t.Fatal(err)
						}
					}
					registry := pixeldata.NewMemoryEncoderRegistry()
					if err := RegisterEncoderWithOptions(registry, EncoderOptions{Quality: quality, Subsampling: sampling, OptimizeHuffman: true}); err != nil {
						t.Fatal(err)
					}
					compressed, _, err := pixeldata.TranscodeDataSet(context.Background(), source, transfer.ExplicitVRLittleEndian, transfer.JPEGBaseline, pixeldata.TranscodeOptions{EncoderRegistry: registry, AllowLossy: true})
					if err != nil {
						t.Fatal(err)
					}
					pixel, err := pixeldata.ExtractView(compressed)
					if err != nil || !pixel.Encapsulated || len(pixel.Sequence.Fragments) != 1 {
						t.Fatalf("compressed frame: %v", err)
					}
					stream := pixel.Sequence.Fragments[0]
					sof := baselineSOF(t, stream)
					wantSampling := byte(0x11)
					if samples == 3 {
						wantSampling = []byte{0x22, 0x21, 0x11}[sampling]
					}
					if sof[7] != wantSampling {
						t.Fatalf("SOF sampling %x, want %x", sof[7], wantSampling)
					}
					dir := t.TempDir()
					input, output := filepath.Join(dir, "baseline.dcm"), filepath.Join(dir, "native.dcm")
					writeExtendedTestFile(t, input, compressed, transfer.JPEGBaseline)
					decode(t, input, output)
					decoded, err := object.OpenFile(output)
					if err != nil {
						t.Fatal(err)
					}
					m, err := pixeldata.ExtractMetadata(decoded.Dataset)
					if err != nil || m.Rows != rows || m.Columns != columns || m.SamplesPerPixel != samples || m.BitsStored != 8 {
						t.Fatalf("decoded metadata %#v: %v", m, err)
					}
					if samples == 3 && m.PhotometricInterpretation != "RGB" && m.PhotometricInterpretation != "YBR_FULL" {
						t.Fatalf("decoded photometric %q, want RGB or native full-resolution YBR_FULL", m.PhotometricInterpretation)
					}
					raw, ok := decoded.Dataset.GetRaw(core.TagPixelData)
					if !ok || len(raw) < len(frame) {
						t.Fatal("missing/short decoded Pixel Data")
					}
					got := append([]byte(nil), raw[:len(frame)]...)
					if samples == 3 && m.PlanarConfiguration == 1 {
						for p := 0; p < rows*columns; p++ {
							for c := 0; c < 3; c++ {
								got[3*p+c] = raw[c*rows*columns+p]
							}
						}
					}
					if samples == 3 && m.PhotometricInterpretation == "YBR_FULL" {
						goImage, err := stdjpeg.Decode(bytes.NewReader(stream))
						if err != nil {
							t.Fatal(err)
						}
						// Luminance has no chroma interpolation ambiguity: verify
						// every pixel independently, in addition to mean RGB error.
						luma := goImage.(*image.YCbCr)
						for y := 0; y < rows; y++ {
							for x := 0; x < columns; x++ {
								delta := int(got[(y*columns+x)*3]) - int(luma.Y[luma.YOffset(x, y)])
								if delta < -2 || delta > 2 {
									t.Fatalf("%s luma(%d,%d) differs from Go by %d", oracle, x, y, delta)
								}
							}
						}
					}
					if samples == 3 && m.PhotometricInterpretation == "YBR_FULL" {
						// +cn and gdcmconv --raw preserve the oracle's full-range
						// color space after independent entropy/IDCT/upsampling.
						// Normalize it for comparison to Go's default RGB output.
						for p := 0; p < len(got); p += 3 {
							got[p], got[p+1], got[p+2] = color.YCbCrToRGB(got[p], got[p+1], got[p+2])
						}
					}
					goSamples := baselineImageSamples(t, stream, int(samples))
					mae, worst := meanAbsoluteError(got, goSamples), maxAbsoluteError(got, goSamples)
					limit := 1.5
					if samples == 3 {
						// Go uses nearest-neighbor chroma expansion; IJG may use
						// interpolation, so errors concentrate around chroma edges.
						limit = 4
					}
					if mae > limit {
						t.Fatalf("%s vs Go: MAE %.4f > %.1f, worst %d", oracle, mae, limit, worst)
					}
					if (samples == 1 && worst > 2) || (samples == 3 && sampling == Subsampling444 && worst > 3) {
						t.Fatalf("%s vs Go without subsampling: worst difference %d", oracle, worst)
					}
					floor := map[int]float64{25: 12, 90: 8, 100: 7}[quality]
					if errorFromSource := meanAbsoluteError(got, frame); errorFromSource > floor {
						t.Fatalf("%s source MAE %.4f > %.1f", oracle, errorFromSource, floor)
					}
					t.Logf("%s: SOF=%x bytes=%d vs Go MAE=%.4f worst=%d, source MAE=%.4f", oracle, sof[7], len(stream), mae, worst, meanAbsoluteError(got, frame))
				})
			}
		}
	}
}
