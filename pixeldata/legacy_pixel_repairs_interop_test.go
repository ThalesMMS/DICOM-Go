package pixeldata_test

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"image"
	"image/color"
	_ "image/png"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/ThalesMMS/dicom-go/core"
	"github.com/ThalesMMS/dicom-go/object"
	"github.com/ThalesMMS/dicom-go/pixeldata"
	"github.com/ThalesMMS/dicom-go/pixeldata/rle"
	"github.com/ThalesMMS/dicom-go/transfer"
)

func legacyWriteFile(t *testing.T, path string, src *object.Object, syntax transfer.Syntax) {
	t.Helper()
	obj := object.FromElements(src.Elements(), nil)
	obj.SetValueByteOrder(src.ValueByteOrder())
	obj.Put(core.Element{Header: core.ElementHeader{Tag: core.NewTag(0x0008, 0x0016), VR: core.VRUI}, Value: core.StringValue{"1.2.840.10008.5.1.4.1.1.7"}})
	obj.Put(core.Element{Header: core.ElementHeader{Tag: core.NewTag(0x0008, 0x0018), VR: core.VRUI}, Value: core.StringValue{"1.2.840.1315.1"}})
	obj.Put(core.Element{Header: core.ElementHeader{Tag: core.NewTag(0x0008, 0x0060), VR: core.VRCS}, Value: core.StringValue{"OT"}})
	f, err := os.Create(path)
	if err != nil {
		t.Fatal(err)
	}
	err = object.WriteFile(f, &object.File{Dataset: obj, TransferSyntax: syntax})
	closeErr := f.Close()
	if err != nil {
		t.Fatal(err)
	}
	if closeErr != nil {
		t.Fatal(closeErr)
	}
}

func TestLegacyPixelRepairsPart10SameSyntaxPath(t *testing.T) {
	for _, tc := range legacyCases() {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			source := filepath.Join(dir, "source.dcm")
			destination := filepath.Join(dir, "normalized.dcm")
			legacyWriteFile(t, source, tc.source, transfer.ExplicitVRLittleEndian)
			report, err := pixeldata.TranscodePath(context.Background(), source, destination, transfer.ExplicitVRLittleEndian, pixeldata.TranscodeOptions{Recovery: tc.recovery})
			if err != nil {
				t.Fatal(err)
			}
			if report.PixelDataPreserved || report.Verification != pixeldata.TranscodeVerificationPixelExact || !slices.Equal(report.DecodeDiagnostics, []string{tc.diagnostic}) {
				t.Fatalf("path recovery bypassed: %+v", report)
			}
			file, err := object.OpenFile(destination)
			if err != nil {
				t.Fatal(err)
			}
			defer file.Close()
			got, _, err := pixeldata.DecompressDataSet(file.Dataset, file.TransferSyntax, pixeldata.DecompressOptions{})
			if err != nil {
				t.Fatal(err)
			}
			wantRaw, _ := tc.want.GetRaw(core.TagPixelData)
			gotRaw, _ := got.GetRaw(core.TagPixelData)
			if !bytes.Equal(gotRaw, wantRaw) {
				t.Fatalf("roundtrip %v, want %v", gotRaw, wantRaw)
			}
			gc, ge := legacyDisplay(got)
			wc, we := legacyDisplay(tc.want)
			if ge != nil || we != nil || !slices.Equal(gc, wc) {
				t.Fatalf("roundtrip display %v / %v, want %v / %v", gc, ge, wc, we)
			}
		})
	}
}

func legacyDigest(data []byte) string { sum := sha256.Sum256(data); return hex.EncodeToString(sum[:]) }
func legacyFileDigest(t *testing.T, path string) string {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return legacyDigest(data)
}
func legacyColorBytes(colors []color.RGBA) []byte {
	out := make([]byte, 0, len(colors)*3)
	for _, c := range colors {
		out = append(out, c.R, c.G, c.B)
	}
	return out
}

func legacyPPM16(t *testing.T, path string) []byte {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var text strings.Builder
	for _, line := range strings.Split(string(data), "\n") {
		if at := strings.IndexByte(line, '#'); at >= 0 {
			line = line[:at]
		}
		text.WriteString(line)
		text.WriteByte(' ')
	}
	fields := strings.Fields(text.String())
	if len(fields) < 4 || fields[0] != "P3" {
		t.Fatal("unexpected independent 16-bit PPM header")
	}
	width, e1 := strconv.Atoi(fields[1])
	height, e2 := strconv.Atoi(fields[2])
	maximum, e3 := strconv.Atoi(fields[3])
	if e1 != nil || e2 != nil || e3 != nil || maximum != 65535 || len(fields) != 4+width*height*3 {
		t.Fatal("unexpected independent PPM dimensions/depth")
	}
	raw := make([]byte, (len(fields)-4)*2)
	for i, field := range fields[4:] {
		value, err := strconv.ParseUint(field, 10, 16)
		if err != nil {
			t.Fatal(err)
		}
		binary.LittleEndian.PutUint16(raw[2*i:], uint16(value))
	}
	return raw
}

// Opt-in independent tool execution; fixtures and receipts can be retained in
// DICOMGO_LEGACY_PIXEL_EVIDENCE_DIR. No vendor implementation is imported.
func TestLegacyPixelRepairsDCMTK(t *testing.T) {
	if os.Getenv("DICOMGO_LEGACY_PIXEL_INTEROP") != "1" {
		t.Skip("set DICOMGO_LEGACY_PIXEL_INTEROP=1 for installed DCMTK qualification")
	}
	dcm2img, err := exec.LookPath("dcm2img")
	if err != nil {
		t.Fatal(err)
	}
	dcmdrle, err := exec.LookPath("dcmdrle")
	if err != nil {
		t.Fatal(err)
	}
	dir := os.Getenv("DICOMGO_LEGACY_PIXEL_EVIDENCE_DIR")
	if dir == "" {
		dir = t.TempDir()
	} else if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	type receipt struct {
		Name, Input, Normalized, Oracle, InputSHA256, NormalizedSHA256, OracleSHA256, ExpectedRGBSHA256, OracleRGBSHA256 string
		Oracle16, Oracle16SHA256, ExpectedStoredRGBSHA256, OracleStoredRGBSHA256                                         string
		NormalizedOracle, NormalizedOracleSHA256, Comparison                                                             string
		Arguments                                                                                                        []string
		Oracle16Arguments, NormalizedOracleArguments                                                                     []string
		Difference                                                                                                       string `json:",omitempty"`
	}
	type sourceReceipt struct{ Path, SHA256 string }
	evidence := struct {
		SchemaVersion, Platform, GoVersion     string
		Dcm2img, Dcm2imgSHA256, Dcm2imgVersion string
		Dcmdrle, DcmdrleSHA256, DcmdrleVersion string
		FixtureSources                         []sourceReceipt
		Profiles, KnownDifferences             []receipt
		Successful                             bool
	}{SchemaVersion: "legacy-pixel-repairs-v1", Platform: runtime.GOOS + "/" + runtime.GOARCH, GoVersion: runtime.Version(), Dcm2img: dcm2img, Dcmdrle: dcmdrle}
	evidence.Dcm2imgSHA256 = legacyFileDigest(t, dcm2img)
	evidence.DcmdrleSHA256 = legacyFileDigest(t, dcmdrle)
	_, sourceFile, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("fixture source location unavailable")
	}
	for _, path := range []string{sourceFile, filepath.Join(filepath.Dir(sourceFile), "legacy_pixel_repairs_test.go")} {
		evidence.FixtureSources = append(evidence.FixtureSources, sourceReceipt{Path: path, SHA256: legacyFileDigest(t, path)})
	}
	version, err := exec.Command(dcm2img, "--version").CombinedOutput()
	if err != nil {
		t.Fatal(err)
	}
	evidence.Dcm2imgVersion = string(version)
	version, err = exec.Command(dcmdrle, "--version").CombinedOutput()
	if err != nil {
		t.Fatal(err)
	}
	evidence.DcmdrleVersion = string(version)
	run := func(tool string, args ...string) {
		t.Helper()
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		output, err := exec.CommandContext(ctx, tool, args...).CombinedOutput()
		if err != nil {
			t.Fatalf("%s %v: %v %s", tool, args, err, output)
		}
	}
	readColors := func(path string) []color.RGBA {
		t.Helper()
		f, err := os.Open(path)
		if err != nil {
			t.Fatal(err)
		}
		img, _, err := image.Decode(f)
		_ = f.Close()
		if err != nil {
			t.Fatal(err)
		}
		var colors []color.RGBA
		for y := img.Bounds().Min.Y; y < img.Bounds().Max.Y; y++ {
			for x := img.Bounds().Min.X; x < img.Bounds().Max.X; x++ {
				colors = append(colors, color.RGBAModel.Convert(img.At(x, y)).(color.RGBA))
			}
		}
		return colors
	}
	cases := legacyCases()
	// Qualify binary hue bins across the whole turn, including partial
	// saturation/value. Expected pixels here come only from the independent
	// executable comparison, rather than a second copy of the conversion math.
	for _, bits := range []uint16{8, 16} {
		scale := uint16(1)
		if bits == 16 {
			scale = 257
		}
		var samples []uint16
		for hue := uint16(0); hue < 256; hue++ {
			for _, saturation := range []uint16{0, 127, 255} {
				for _, value := range []uint16{73, 255} {
					samples = append(samples, hue*scale, saturation*scale, value*scale)
				}
			}
		}
		src := legacyImage(bits, 3, 0, "HSV", samples)
		recovery := pixeldata.DecodeRecovery{ConvertRetiredColorModels: true}
		out, _, err := pixeldata.DecompressDataSet(src, transfer.ExplicitVRLittleEndian, pixeldata.DecompressOptions{Recovery: recovery})
		if err != nil {
			t.Fatal(err)
		}
		cases = append(cases, legacyPixelCase{name: fmt.Sprintf("HSV-grid-%d", bits), source: src, recovery: recovery, want: out})
	}
	for _, tc := range cases {
		if !tc.recovery.ConvertRetiredColorModels && !tc.recovery.WrongPaletteAttributeTags {
			continue
		}
		t.Run(tc.name, func(t *testing.T) {
			input := filepath.Join(dir, tc.name+".dcm")
			normalized := filepath.Join(dir, tc.name+".normalized.dcm")
			oracle := filepath.Join(dir, tc.name+".oracle.png")
			legacyWriteFile(t, input, tc.source, transfer.ExplicitVRLittleEndian)
			out, _, err := pixeldata.DecompressDataSet(tc.source, transfer.ExplicitVRLittleEndian, pixeldata.DecompressOptions{Recovery: tc.recovery})
			if err != nil {
				t.Fatal(err)
			}
			legacyWriteFile(t, normalized, out, transfer.ExplicitVRLittleEndian)
			args := []string{"-q", "+on"}
			if tc.recovery.WrongPaletteAttributeTags {
				args = append(args, "+Mp")
			}
			args = append(args, input, oracle)
			run(dcm2img, args...)
			colors := readColors(oracle)
			want, err := legacyDisplay(tc.want)
			if err != nil {
				t.Fatal(err)
			}
			record := receipt{Name: tc.name, Input: input, Normalized: normalized, Oracle: oracle, InputSHA256: legacyFileDigest(t, input), NormalizedSHA256: legacyFileDigest(t, normalized), OracleSHA256: legacyFileDigest(t, oracle), ExpectedRGBSHA256: legacyDigest(legacyColorBytes(want)), OracleRGBSHA256: legacyDigest(legacyColorBytes(colors)), Comparison: "display-rgb8", Arguments: args}
			m, err := pixeldata.ExtractMetadata(out)
			if err != nil {
				t.Fatal(err)
			}
			if m.BitsAllocated == 16 {
				record.Comparison = "stored-rgb16-and-display-rgb8"
				record.Oracle16 = filepath.Join(dir, tc.name+".oracle16.ppm")
				record.Oracle16Arguments = []string{"-q", "+opw", input, record.Oracle16}
				run(dcm2img, record.Oracle16Arguments...)
				oracle16 := legacyPPM16(t, record.Oracle16)
				stored, _ := out.GetRaw(core.TagPixelData)
				if !bytes.Equal(oracle16, stored) {
					for i := 0; i < min(len(oracle16), len(stored)); i += 2 {
						if !bytes.Equal(oracle16[i:i+2], stored[i:i+2]) {
							t.Fatalf("independent full16 RGB sample %d: %d, want %d", i/2, binary.LittleEndian.Uint16(oracle16[i:]), binary.LittleEndian.Uint16(stored[i:]))
						}
					}
					t.Fatal("independent full16 RGB length differs")
				}
				record.Oracle16SHA256 = legacyFileDigest(t, record.Oracle16)
				record.ExpectedStoredRGBSHA256 = legacyDigest(stored)
				record.OracleStoredRGBSHA256 = legacyDigest(oracle16)
			}
			if !slices.Equal(colors, want) {
				if m.BitsAllocated == 16 {
					// Reduction from 16-bit RGB to PNG may round differently.
					// Require exact 16-bit samples (above) and independently
					// identical display of source and strictly normalized RGB.
					record.NormalizedOracle = filepath.Join(dir, tc.name+".normalized.oracle.png")
					record.NormalizedOracleArguments = []string{"-q", "+on", normalized, record.NormalizedOracle}
					run(dcm2img, record.NormalizedOracleArguments...)
					if !slices.Equal(readColors(record.NormalizedOracle), colors) {
						t.Fatal("independent source and normalized RGB presentation differ")
					}
					record.NormalizedOracleSHA256 = legacyFileDigest(t, record.NormalizedOracle)
					record.Comparison = "stored-rgb16-and-oracle-normalized-display"
					difference := record
					difference.Difference = "Stored RGB16 and independent source/normalized display are exact; reduction to RGB8 differs because the library rounds normalized RGB while this oracle truncates."
					evidence.KnownDifferences = append(evidence.KnownDifferences, difference)
					evidence.Profiles = append(evidence.Profiles, record)
					return
				}
				for i := range min(len(colors), len(want)) {
					if colors[i] != want[i] {
						t.Fatalf("independent RGB pixel %d: %v, want %v", i, colors[i], want[i])
					}
				}
				t.Fatal("independent RGB dimensions differ")
			}
			evidence.Profiles = append(evidence.Profiles, record)
		})
	}
	// Mixed-depth ARGB already has defined library semantics. Record independent
	// disagreement separately; it must never be counted as exact qualification.
	for _, planar := range []uint16{0, 1} {
		name := fmt.Sprintf("ARGB-mixed-depth-16-palette8-%d", planar)
		t.Run(name, func(t *testing.T) {
			src := mixedDepthARGB16(planar)
			input := filepath.Join(dir, name+".dcm")
			normalized := filepath.Join(dir, name+".normalized.dcm")
			oracle := filepath.Join(dir, name+".oracle.png")
			oracle16 := filepath.Join(dir, name+".oracle16.ppm")
			legacyWriteFile(t, input, src, transfer.ExplicitVRLittleEndian)
			out, _, err := pixeldata.DecompressDataSet(src, transfer.ExplicitVRLittleEndian, pixeldata.DecompressOptions{Recovery: pixeldata.DecodeRecovery{ConvertRetiredColorModels: true}})
			if err != nil {
				t.Fatal(err)
			}
			legacyWriteFile(t, normalized, out, transfer.ExplicitVRLittleEndian)
			want, err := legacyDisplay(src)
			if err != nil {
				t.Fatal(err)
			}
			got, err := legacyDisplay(out)
			if err != nil || !slices.Equal(got, want) {
				t.Fatalf("existing ARGB semantics changed: %v %v", got, err)
			}
			args := []string{"-q", "+on", input, oracle}
			run(dcm2img, args...)
			colors := readColors(oracle)
			run(dcm2img, "-q", "+opw", input, oracle16)
			stored, _ := out.GetRaw(core.TagPixelData)
			oracleRaw := legacyPPM16(t, oracle16)
			record := receipt{Name: name, Input: input, Normalized: normalized, Oracle: oracle, Oracle16: oracle16,
				InputSHA256: legacyFileDigest(t, input), NormalizedSHA256: legacyFileDigest(t, normalized), OracleSHA256: legacyFileDigest(t, oracle), Oracle16SHA256: legacyFileDigest(t, oracle16),
				ExpectedRGBSHA256: legacyDigest(legacyColorBytes(want)), OracleRGBSHA256: legacyDigest(legacyColorBytes(colors)),
				ExpectedStoredRGBSHA256: legacyDigest(stored), OracleStoredRGBSHA256: legacyDigest(oracleRaw), Arguments: args}
			record.Oracle16Arguments = []string{"-q", "+opw", input, oracle16}
			record.Comparison = "stored-rgb16-and-display-rgb8"
			if slices.Equal(colors, want) && bytes.Equal(oracleRaw, stored) {
				evidence.Profiles = append(evidence.Profiles, record)
			} else {
				record.Difference = "16-bit ARGB with positive alpha and 8-bit palettes: library preserves independently depth-scaled palette semantics; this oracle disagrees. This case is not exact qualification."
				evidence.KnownDifferences = append(evidence.KnownDifferences, record)
				t.Logf("recorded independent difference: RGB %v vs library %v; full16 %v vs library %v", colors, want, oracleRaw, stored)
			}
		})
	}
	registry := pixeldata.NewMemoryRegistry()
	if err := rle.Register(registry); err != nil {
		t.Fatal(err)
	}
	for _, samples := range []uint16{1, 3} {
		t.Run(fmt.Sprintf("RLE16-%d", samples), func(t *testing.T) {
			src, want := reversedRLEFixture(t, 16, samples)
			name := fmt.Sprintf("RLE16-%d", samples)
			input := filepath.Join(dir, name+".dcm")
			normalized := filepath.Join(dir, name+".normalized.dcm")
			oracle := filepath.Join(dir, name+".oracle-native.dcm")
			legacyWriteFile(t, input, src, transfer.RLELossless)
			out, _, err := pixeldata.DecompressDataSet(src, transfer.RLELossless, pixeldata.DecompressOptions{Registry: registry, Recovery: pixeldata.DecodeRecovery{RLEReversedByteSegments: true}})
			if err != nil {
				t.Fatal(err)
			}
			legacyWriteFile(t, normalized, out, transfer.ExplicitVRLittleEndian)
			args := []string{"-q", "+br", input, oracle}
			run(dcmdrle, args...)
			file, err := object.OpenFile(oracle)
			if err != nil {
				t.Fatal(err)
			}
			defer file.Close()
			interleaved := uint16(0)
			native, _, err := pixeldata.RewriteNative(context.Background(), file.Dataset, pixeldata.NativeRewrite{PlanarConfiguration: &interleaved})
			if err != nil {
				t.Fatal(err)
			}
			raw, _ := native.GetRaw(core.TagPixelData)
			if !bytes.Equal(raw, want) {
				t.Fatalf("independent RLE %v, want %v", raw, want)
			}
			evidence.Profiles = append(evidence.Profiles, receipt{Name: name, Input: input, Normalized: normalized, Oracle: oracle, InputSHA256: legacyFileDigest(t, input), NormalizedSHA256: legacyFileDigest(t, normalized), OracleSHA256: legacyFileDigest(t, oracle), ExpectedRGBSHA256: legacyDigest(want), OracleRGBSHA256: legacyDigest(raw), Comparison: "stored-native-bytes", Arguments: args})
		})
	}
	evidence.Successful = !t.Failed()
	encoded, err := json.MarshalIndent(evidence, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "qualification.json"), append(encoded, '\n'), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Logf("%d exact independent profiles; %d recorded differences; receipt %s; %s", len(evidence.Profiles), len(evidence.KnownDifferences), filepath.Join(dir, "qualification.json"), strings.Split(evidence.Dcm2imgVersion, "\n")[0])
}
