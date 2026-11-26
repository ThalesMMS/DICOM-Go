package jpeg

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/ThalesMMS/dicom-go/core"
	"github.com/ThalesMMS/dicom-go/object"
	"github.com/ThalesMMS/dicom-go/pixeldata"
	"github.com/ThalesMMS/dicom-go/transfer"
)

type extendedOracleTool struct{ Path, Version, SHA256 string }
type extendedOracleDecode struct {
	Tool                     string
	Scope                    string
	Commands                 [][]string
	Headers                  []map[string]uint16
	Messages                 []string
	Args                     []string
	Path, SHA256             string
	Metadata                 pixeldata.Metadata
	PSNR                     []float64
	MaximumSourceError       []int
	MaximumDecoderDifference int
}
type extendedOracleCase struct {
	Profile                                              string
	Quality                                              int
	CompatibilityProfile                                 pixeldata.EncoderCompatibilityProfile
	NativePath, NativeSHA256, EncodedPath, EncodedSHA256 string
	Metadata                                             pixeldata.Metadata
	EncodedBytes                                         int64
	LossyMethod, LossyCompression, CompressionRatio      string
	Decodes                                              []extendedOracleDecode
}
type extendedOracleReceipt struct {
	Schema              string
	Successful          bool
	OS, Arch, GoVersion string
	SourceBase          string
	Provenance          string
	RuntimeFiles        map[string]string
	Limitations         []string
	Sources             map[string]string
	Tools               map[string]extendedOracleTool
	Cases               []extendedOracleCase
}

func extendedOracleHash(t *testing.T, path string) string {
	t.Helper()
	f, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	hash := sha256.New()
	if _, err := io.Copy(hash, f); err != nil {
		t.Fatal(err)
	}
	return hex.EncodeToString(hash.Sum(nil))
}

func extendedOracleRun(t *testing.T, tool string, args ...string) []byte {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	out, err := exec.CommandContext(ctx, tool, args...).CombinedOutput()
	if err != nil {
		t.Fatalf("oracle %s %v: %v\n%s", tool, args, err, out)
	}
	return out
}

func extendedOracleSamples(t *testing.T, path string, want pixeldata.Metadata) ([]int, pixeldata.Metadata) {
	t.Helper()
	file, err := object.OpenFile(path)
	if err != nil {
		t.Fatal(err)
	}
	defer file.Close()
	if file.TransferSyntax.Encapsulated {
		t.Fatal("oracle did not decompress", path)
	}
	frames, err := pixeldata.ExtractNativeFrames(file.Dataset)
	if err != nil {
		t.Fatal(err)
	}
	m := frames.Metadata
	if m != want {
		t.Fatalf("oracle metadata mismatch: got %+v, want %+v", m, want)
	}
	var samples []int
	for _, raw := range frames.Data {
		samples = append(samples, extendedProfileSamples(raw, m)...)
	}
	return samples, m
}

func extendedOracleMetrics(got, want []int, m pixeldata.Metadata) ([]float64, []int) {
	channels := int(m.SamplesPerPixel)
	psnr, worst := make([]float64, channels), make([]int, channels)
	for c := 0; c < channels; c++ {
		a, b := make([]int, 0, len(got)/channels), make([]int, 0, len(want)/channels)
		for i := c; i < len(got); i += channels {
			a = append(a, got[i])
			b = append(b, want[i])
		}
		psnr[c], worst[c] = extendedErrors(a, b, m.BitsStored)
	}
	return psnr, worst
}

// Independent native decodes consume only own generated Part10 fixtures. The
// library reads the oracle's uncompressed bytes; its JPEG decoder is never
// used to obtain the oracle output. No vendor sources or image assets are used.
func TestExtendedEncoderNewProfilesDCMTKAndGDCM(t *testing.T) {
	if os.Getenv("DICOMGO_JPEG_EXTENDED_INTEROP") != "1" {
		t.Skip("set DICOMGO_JPEG_EXTENDED_INTEROP=1 with DCMTK/GDCM tools")
	}
	dir := os.Getenv("DICOMGO_JPEG_EXTENDED_EVIDENCE_DIR")
	if dir == "" {
		dir = t.TempDir()
	}
	if err := os.MkdirAll(dir, 0700); err != nil {
		t.Fatal(err)
	}
	receipt := extendedOracleReceipt{Schema: "jpeg-extended-profiles-v1", OS: runtime.GOOS, Arch: runtime.GOARCH, GoVersion: runtime.Version(), SourceBase: "212dd7fba6f1138924642ffe4307276d95e2732d", Provenance: "Own deterministic unsigned synthetic raster and Part10 fixtures; no patient data, vendor sources or vendor assets.", Sources: make(map[string]string), Tools: make(map[string]extendedOracleTool), RuntimeFiles: make(map[string]string), Limitations: []string{"GDCM gdcmconv 3.2.5 refuses RGB12/YBR_FULL12 (Unsupported JPEG data precision 12); public gdcm::JPEGCodec succeeds. The API oracle checks JPEG dimensions/precision/components and raw stored samples; DCMTK checks full Part10 metadata."}}

	// Always replace the receipt, including failed/incomplete opt-in runs.
	// A previous successful report must not survive a missing tool or case error.
	t.Cleanup(func() {
		receipt.Successful = !t.Failed() && len(receipt.Cases) == 30
		data, err := json.MarshalIndent(receipt, "", "  ")
		if err != nil {
			t.Errorf("receipt: %v", err)
			return
		}
		if err := os.WriteFile(filepath.Join(dir, "qualification.json"), append(data, '\n'), 0600); err != nil {
			t.Errorf("receipt: %v", err)
		}
	})
	for _, source := range []string{"extended_encoder_profiles_test.go", "extended_encoder_profiles_interop_test.go", "extended_encoder.go", "testdata/gdcm_jpeg_oracle.cpp"} {
		receipt.Sources[source] = extendedOracleHash(t, source)
	}
	for _, tool := range []string{"dcmdjpeg", "gdcmconv", "gdcm-jpeg-oracle"} {
		name := tool
		if tool == "gdcm-jpeg-oracle" {
			name = os.Getenv("DICOMGO_GDCM_JPEG_ORACLE")
		}
		path, err := exec.LookPath(name)
		if err != nil {
			t.Fatalf("required independent oracle missing: %s: %v", tool, err)
		}
		receipt.Tools[tool] = extendedOracleTool{Path: path, Version: strings.TrimSpace(string(extendedOracleRun(t, path, "--version"))), SHA256: extendedOracleHash(t, path)}
	}

	if runtime.GOOS == "windows" {
		for tool, names := range map[string][]string{
			"gdcmconv": {"libgdcmMSFF.dll", "libgdcmjpeg8.dll", "libgdcmjpeg12.dll", "libgdcmjpeg16.dll"},
			"dcmdjpeg": {"libdcmjpeg.dll", "libijg8.dll", "libijg12.dll", "libijg16.dll"},
		} {
			for _, name := range names {
				path := filepath.Join(filepath.Dir(receipt.Tools[tool].Path), name)
				if _, err := os.Stat(path); err != nil {
					t.Fatal("required MSYS2 qualification runtime missing", path, err)
				}
				receipt.RuntimeFiles[path] = extendedOracleHash(t, path)
			}
		}
	} else {
		receipt.Limitations = append(receipt.Limitations, "This host records executable hashes/versions; automatic dynamic-library hashes cover the qualified Windows MSYS2 runtime only.")
	}
	ctx := pixeldata.WithJPEGColorConversion(context.Background(), pixeldata.JPEGColorNever)
	decoders := pixeldata.NewMemoryRegistry()
	if err := Register(decoders); err != nil {
		t.Fatal(err)
	}
	for _, profile := range []struct {
		bits uint16
		pi   string
	}{{8, "MONOCHROME2"}, {12, "MONOCHROME2"}, {8, "RGB"}, {12, "RGB"}, {12, "YBR_FULL"}} {
		m := extendedProfileMetadata(profile.bits, profile.pi)
		m.NumberOfFrames = 2
		raw := append(extendedProfileFrame(m, 0), extendedProfileFrame(m, 1)...)
		source := extendedProfileObject(m, raw)
		nativeName := fmt.Sprintf("%s%d", profile.pi, profile.bits)
		nativePath := filepath.Join(dir, nativeName+"-native.dcm")
		writeExtendedTestFile(t, nativePath, source, transfer.ExplicitVRLittleEndian)
		for _, quality := range []int{25, 50, 75, 90, 95, 100} {
			name := fmt.Sprintf("%s-q%d", nativeName, quality)
			t.Run(name, func(t *testing.T) {
				encoders := pixeldata.NewMemoryEncoderRegistry()
				if err := RegisterExtendedEncoderWithOptions(encoders, ExtendedEncoderOptions{Quality: quality, AllowColorCompatibility: m.SamplesPerPixel == 3}); err != nil {
					t.Fatal(err)
				}
				compressed, report, err := pixeldata.TranscodeDataSet(ctx, source, transfer.ExplicitVRLittleEndian, transfer.JPEGExtended, pixeldata.TranscodeOptions{EncoderRegistry: encoders, DecoderRegistry: decoders, AllowLossy: true})
				if err != nil {
					t.Fatal(err)
				}
				encodedPath := filepath.Join(dir, name+".dcm")
				writeExtendedTestFile(t, encodedPath, compressed, transfer.JPEGExtended)
				decoded, _, err := pixeldata.DecompressDataSetContext(ctx, compressed, transfer.JPEGExtended, pixeldata.DecompressOptions{Registry: decoders})
				if err != nil {
					t.Fatal(err)
				}
				goFrames, err := pixeldata.ExtractNativeFrames(decoded)
				if err != nil {
					t.Fatal(err)
				}
				var goSamples []int
				for _, frame := range goFrames.Data {
					goSamples = append(goSamples, extendedProfileSamples(frame, m)...)
				}
				item := extendedOracleCase{Profile: nativeName, Quality: quality, NativePath: nativePath, NativeSHA256: extendedOracleHash(t, nativePath), EncodedPath: encodedPath, EncodedSHA256: extendedOracleHash(t, encodedPath), Metadata: m, EncodedBytes: report.EncodedBytes}
				if m.SamplesPerPixel == 3 {
					item.CompatibilityProfile = pixeldata.EncoderCompatibilityJPEGExtendedColor
				}
				if !slices.Equal(report.EncodeCompatibilityProfiles, encoderExpectedProfile(item.CompatibilityProfile)) {
					t.Fatal("profile lost", report)
				}
				item.LossyMethod, _ = compressed.GetString(core.NewTag(0x0028, 0x2114))
				item.LossyCompression, _ = compressed.GetString(core.NewTag(0x0028, 0x2110))
				item.CompressionRatio, _ = compressed.GetString(core.NewTag(0x0028, 0x2112))
				if !report.Lossy || item.LossyCompression != "01" || item.LossyMethod != jpegLossyMethod || item.CompressionRatio == "" {
					t.Fatal("lossy metadata", item)
				}
				for _, requestedTool := range []string{"dcmdjpeg", "gdcmconv"} {
					tool := requestedTool
					path := filepath.Join(dir, name+"-"+tool+".dcm")
					args := []string{"+cn", "+px", "-bs", "+te", encodedPath, path}
					if tool == "gdcmconv" {
						args = []string{"-w", "--planar-configuration", "0", encodedPath, path}
					}
					var got []int
					meta := m
					scope := "Part10 native output metadata and stored samples"
					var commands [][]string
					var headers []map[string]uint16
					var messages []string
					if tool == "gdcmconv" && m.BitsStored == 12 && m.SamplesPerPixel == 3 {
						tool = "gdcm-jpeg-oracle"
						scope = "JPEG codestream header and stored samples; PI/planar are explicit input hints, Part10 metadata is checked by DCMTK"
						var all []byte
						for frameIndex := 0; frameIndex < int(m.NumberOfFrames); frameIndex++ {
							pixel, err := pixeldata.Extract(compressed)
							if err != nil || !pixel.Encapsulated || len(pixel.Sequence.Fragments) != int(m.NumberOfFrames) {
								t.Fatal("own output fragments", err)
							}
							frame := pixel.Sequence.Fragments[frameIndex]
							stem := filepath.Join(dir, fmt.Sprintf("%s-gdcm-frame%d", name, frameIndex))
							jpegPath, rawPath, headerPath := stem+".jpg", stem+".raw", stem+".json"
							if err := os.WriteFile(jpegPath, frame, 0600); err != nil {
								t.Fatal(err)
							}
							command := []string{jpegPath, rawPath, headerPath, fmt.Sprint(m.BitsStored), fmt.Sprint(m.SamplesPerPixel), m.PhotometricInterpretation, fmt.Sprint(m.Rows), fmt.Sprint(m.Columns)}
							message := extendedOracleRun(t, receipt.Tools[tool].Path, command...)
							if len(message) > 0 {
								messages = append(messages, strings.TrimSpace(string(message)))
							}
							commands = append(commands, command)
							data, err := os.ReadFile(rawPath)
							if err != nil {
								t.Fatal(err)
							}
							if len(data) != int(m.Rows)*int(m.Columns)*int(m.SamplesPerPixel)*2 {
								t.Fatal("GDCM raw length", len(data))
							}
							all = append(all, data...)
							data, err = os.ReadFile(headerPath)
							if err != nil {
								t.Fatal(err)
							}
							var header map[string]uint16
							if err := json.Unmarshal(data, &header); err != nil {
								t.Fatal(err)
							}
							if header["Rows"] != m.Rows || header["Columns"] != m.Columns || header["BitsStored"] != m.BitsStored || header["SamplesPerPixel"] != m.SamplesPerPixel {
								t.Fatal("GDCM header mismatch", header, m)
							}
							headers = append(headers, header)
						}
						path = filepath.Join(dir, name+"-gdcm-jpeg-oracle.raw")
						if err := os.WriteFile(path, all, 0600); err != nil {
							t.Fatal(err)
						}
						got = extendedProfileSamples(all, m)
						args = nil
					} else {
						extendedOracleRun(t, receipt.Tools[tool].Path, args...)
						got, meta = extendedOracleSamples(t, path, m)
					}
					psnr, worst := extendedOracleMetrics(got, extendedProfileSamples(raw, m), m)
					_, difference := extendedErrors(got, goSamples, m.BitsStored)
					floor := map[int]float64{25: 24, 50: 27, 75: 30, 90: 36, 95: 42, 100: 56}[quality]
					if profile.bits == 12 {
						floor = map[int]float64{25: 38, 50: 44, 75: 49, 90: 56, 95: 60, 100: 78}[quality]
					}
					for _, value := range psnr {
						if value < floor {
							t.Fatalf("%s PSNR %.3f below %.1f", tool, value, floor)
						}
					}
					if difference > 2 {
						t.Fatalf("%s vs own decode difference %d exceeds2", tool, difference)
					}
					item.Decodes = append(item.Decodes, extendedOracleDecode{Tool: tool, Scope: scope, Commands: commands, Headers: headers, Messages: messages, Args: args, Path: path, SHA256: extendedOracleHash(t, path), Metadata: meta, PSNR: psnr, MaximumSourceError: worst, MaximumDecoderDifference: difference})
					t.Logf("%s PSNR=%v max=%v decoderDelta=%d", tool, psnr, worst, difference)
				}
				receipt.Cases = append(receipt.Cases, item)
			})
		}
	}
	if t.Failed() || len(receipt.Cases) != 30 {
		t.Fatal("independent qualification incomplete")
	}
}

func encoderExpectedProfile(profile pixeldata.EncoderCompatibilityProfile) []pixeldata.EncoderCompatibilityProfile {
	if profile == "" {
		return nil
	}
	return []pixeldata.EncoderCompatibilityProfile{profile}
}
