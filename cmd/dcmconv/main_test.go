package main

import (
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/ThalesMMS/dicom-go/core"
	"github.com/ThalesMMS/dicom-go/internal/dicomtest"
	"github.com/ThalesMMS/dicom-go/object"
	"github.com/ThalesMMS/dicom-go/pixeldata"
	"github.com/ThalesMMS/dicom-go/pixeldata/codecfixture"
	"github.com/ThalesMMS/dicom-go/transfer"
)

func writeImage(t *testing.T) (string, []byte) {
	t.Helper()
	data, err := dicomtest.Part10File(transfer.ExplicitVRLittleEndian, dicomtest.DatasetWithPixelData()...)
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "in.dcm")
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatal(err)
	}
	file, err := object.OpenFile(path)
	if err != nil {
		t.Fatal(err)
	}
	defer file.Close()
	pixels, _ := file.GetRaw(core.TagPixelData)
	return path, pixels
}

func runConv(t *testing.T, args ...string) (int, string, string) {
	t.Helper()
	var stdout, stderr bytes.Buffer
	code := run(args, &stdout, &stderr)
	return code, stdout.String(), stderr.String()
}

func syntaxOf(t *testing.T, path string) string {
	t.Helper()
	file, err := object.OpenFile(path)
	if err != nil {
		t.Fatal(err)
	}
	defer file.Close()
	value, _ := file.Meta.GetString(core.NewTag(0x0002, 0x0010))
	return transfer.NormalizeUID(value)
}

// TestConvRoundTripsThroughRLE: native to RLE and back restores the pixels,
// with the target accepted as an alias, a keyword or a UID.
func TestConvRoundTripsThroughRLE(t *testing.T) {
	input, pixels := writeImage(t)
	dir := t.TempDir()
	for _, ts := range []string{"rle", "RLELossless", transfer.RLELossless.UID} {
		encoded := filepath.Join(dir, "rle.dcm")
		code, stdout, stderr := runConv(t, "-ts", ts, "-o", encoded, input)
		if code != 0 || !strings.Contains(stdout, "1 frame(s) to RLE Lossless (lossless, verified") {
			t.Fatalf("-ts %s: code %d stdout %q stderr %q", ts, code, stdout, stderr)
		}
		if got := syntaxOf(t, encoded); got != transfer.RLELossless.UID {
			t.Fatalf("output syntax %s", got)
		}
	}
	decoded := filepath.Join(dir, "native.dcm")
	if code, _, stderr := runConv(t, "-ts", "explicit-le", "-o", decoded, filepath.Join(dir, "rle.dcm")); code != 0 {
		t.Fatalf("decode: %s", stderr)
	}
	file, err := object.OpenFile(decoded)
	if err != nil {
		t.Fatal(err)
	}
	defer file.Close()
	if got, _ := file.GetRaw(core.TagPixelData); !bytes.Equal(got[:len(pixels)], pixels) {
		t.Fatal("pixels changed through RLE")
	}
}

// TestConvRoundTripsThroughDeflatedFrames: the deflated-frames alias reaches
// Deflated Image Frame Compression and the pixels come back unchanged.
func TestConvRoundTripsThroughDeflatedFrames(t *testing.T) {
	input, pixels := writeImage(t)
	dir := t.TempDir()
	encoded := filepath.Join(dir, "deflated-frames.dcm")
	code, stdout, stderr := runConv(t, "-ts", "deflated-frames", "-o", encoded, input)
	if code != 0 || !strings.Contains(stdout, "to Deflated Image Frame Compression (lossless, verified") {
		t.Fatalf("code %d stdout %q stderr %q", code, stdout, stderr)
	}
	if got := syntaxOf(t, encoded); got != transfer.DeflatedImageFrameCompression.UID {
		t.Fatalf("output syntax %s", got)
	}
	decoded := filepath.Join(dir, "native.dcm")
	if code, _, stderr := runConv(t, "-ts", "explicit-le", "-o", decoded, encoded); code != 0 {
		t.Fatalf("decode: %s", stderr)
	}
	file, err := object.OpenFile(decoded)
	if err != nil {
		t.Fatal(err)
	}
	defer file.Close()
	if got, _ := file.GetRaw(core.TagPixelData); !bytes.Equal(got[:len(pixels)], pixels) {
		t.Fatal("pixels changed through Deflated Image Frame Compression")
	}
}

// TestConvRefusals: lossy targets need -allow-lossy, the output may not be
// the input, and unknown names are usage errors.
func TestConvRefusals(t *testing.T) {
	input, _ := writeImage(t)
	out := filepath.Join(t.TempDir(), "out.dcm")
	if code, _, stderr := runConv(t, "-ts", "jpeg-baseline", "-o", out, input); code != 1 || stderr == "" {
		t.Fatalf("lossy without -allow-lossy: code %d stderr %q", code, stderr)
	}
	if _, err := os.Stat(out); !os.IsNotExist(err) {
		t.Fatal("a refused conversion left an output")
	}
	if code, _, _ := runConv(t, "-ts", "rle", "-o", input, input); code != 1 {
		t.Fatalf("output = input: code %d", code)
	}
	for _, args := range [][]string{
		{"-ts", "no-such-syntax", "-o", out, input},
		{"-ts", "rle", "-recover", "no-such-recovery", "-o", out, input},
		{"-ts", "rle", "-rewrite", "no-such-rewrite", "-o", out, input},
		{"-ts", "rle", input},
	} {
		if code, _, _ := runConv(t, args...); code != 2 {
			t.Fatalf("%v: code %d, want 2", args, code)
		}
	}
	if code, _, stderr := runConv(t, "-h"); code != 0 || !strings.Contains(stderr, "Usage: dcmconv") {
		t.Fatalf("-h: code %d stderr %q", code, stderr)
	}
}

// TestConvLossyAndRecoveryOptions: -allow-lossy reaches JPEG Baseline, and
// opt-in recoveries and rewrites are accepted and reported when applied.
func TestConvLossyAndRecoveryOptions(t *testing.T) {
	input, _ := writeImage(t)
	out := filepath.Join(t.TempDir(), "out.dcm")
	code, stdout, stderr := runConv(t, "-ts", "jpeg-baseline", "-allow-lossy", "-jpeg-quality", "80", "-o", out, input)
	if code != 0 || !strings.Contains(stdout, "lossy") {
		t.Fatalf("lossy: code %d stdout %q stderr %q", code, stdout, stderr)
	}
	if got := syntaxOf(t, out); got != transfer.JPEGBaseline.UID {
		t.Fatalf("output syntax %s", got)
	}
	code, stdout, stderr = runConv(t, "-ts", "jpeg-ls-lossless", "-recover", "ignore-bot, rle-segment-overrun", "-recover-un", "-o", out, input)
	if code != 0 || !strings.Contains(stdout, "UN recovery:") {
		t.Fatalf("recoveries: code %d stdout %q stderr %q", code, stdout, stderr)
	}
}

// acrNemaFile writes an ACR-NEMA style MR image: Implicit VR Little Endian,
// no preamble, File Meta or SOP UIDs.
func acrNemaFile(t *testing.T) string {
	t.Helper()
	data := dicomtest.EncodeElements(transfer.ImplicitVRLittleEndian,
		dicomtest.NewStringElement(core.NewTag(0x0008, 0x0010), core.VRLO, "ACR-NEMA 2.0"),
		dicomtest.NewStringElement(core.NewTag(0x0008, 0x0060), core.VRCS, "MR"),
		dicomtest.NewUShortElement(core.NewTag(0x0028, 0x0010), 1),
		dicomtest.NewUShortElement(core.NewTag(0x0028, 0x0011), 2),
		dicomtest.NewUShortElement(core.NewTag(0x0028, 0x0100), 16),
		dicomtest.NewUShortElement(core.NewTag(0x0028, 0x0101), 16),
		dicomtest.NewUShortElement(core.NewTag(0x0028, 0x0102), 15),
		dicomtest.NewUShortElement(core.NewTag(0x0028, 0x0103), 0),
		core.NewRawElement(core.TagPixelData, core.VROW, []byte{1, 2, 3, 4}),
	)
	path := filepath.Join(t.TempDir(), "image.acr")
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestConvUpgradesLegacyInput(t *testing.T) {
	input := acrNemaFile(t)
	out := filepath.Join(t.TempDir(), "out.dcm")
	code, stdout, stderr := runConv(t, "-ts", "explicit-le", "-read-recover", "-upgrade-legacy", "-o", out, input)
	if code != 0 {
		t.Fatalf("exit %d, stderr %s", code, stderr)
	}
	if !strings.Contains(stdout, "legacy upgrade: SOP Class 1.2.840.10008.5.1.4.1.1.4 (modality MR)") || !strings.Contains(stdout, "legacy upgrade generated: (0008,0018)") {
		t.Fatalf("stdout does not report the upgrade:\n%s", stdout)
	}
	file, err := object.OpenFile(out)
	if err != nil {
		t.Fatalf("strict read of the upgraded output: %v", err)
	}
	defer file.Close()
	if raw, _ := file.GetRaw(core.TagPixelData); !bytes.Equal(raw, []byte{1, 2, 3, 4}) {
		t.Fatalf("pixels = % X", raw)
	}
	entries, _ := os.ReadDir(filepath.Dir(out))
	if len(entries) != 1 {
		t.Fatalf("temporary files left beside the output: %d entries", len(entries))
	}

	if code, _, stderr := runConv(t, "-ts", "explicit-le", "-read-recover", "-o", filepath.Join(t.TempDir(), "x.dcm"), input); code != 1 || !strings.Contains(stderr, "-upgrade-legacy") {
		t.Fatalf("-read-recover without -upgrade-legacy: exit %d, stderr %s", code, stderr)
	}
	if code, _, _ := runConv(t, "-ts", "explicit-le", "-upgrade-legacy", "-o", filepath.Join(t.TempDir(), "x.dcm"), input); code != 2 {
		t.Fatalf("-upgrade-legacy without -read-recover: exit %d, want 2", code)
	}
}

func TestConvIconPolicyAndReport(t *testing.T) {
	for _, compress := range []bool{false, true} {
		main, icon := codecfixture.NativeSmall().Object(), codecfixture.NativeSmall().Object()
		iconTag := core.NewTag(0x0088, 0x0200)
		main.Put(core.Element{Header: core.ElementHeader{Tag: iconTag, VR: core.VRSQ}, Value: core.SequenceValue{Items: []core.DataSet{icon.ToDataSet()}}})
		dir := t.TempDir()
		input, output, restored := filepath.Join(dir, "in.dcm"), filepath.Join(dir, "out.dcm"), filepath.Join(dir, "back.dcm")
		f, err := os.Create(input)
		if err != nil {
			t.Fatal(err)
		}
		err = object.WriteFile(f, &object.File{Dataset: main, TransferSyntax: transfer.ExplicitVRLittleEndian})
		closeErr := f.Close()
		if err != nil || closeErr != nil {
			t.Fatalf("fixture=%v/%v", err, closeErr)
		}
		args := []string{"-ts", "rle", "-o", output}
		if compress {
			args = append(args, "-compress-icons")
		}
		args = append(args, input)
		code, stdout, stderr := runConv(t, args...)
		action := "preserved-native"
		if compress {
			action = "compressed"
		}
		if code != 0 || !strings.Contains(stdout, "icon (0088,0200)[0]: "+action) {
			t.Fatalf("code=%d stdout=%q stderr=%q", code, stdout, stderr)
		}
		encoded, err := object.OpenFile(output)
		if err != nil {
			t.Fatal(err)
		}
		defer encoded.Close()
		items, ok := encoded.Dataset.GetSequence(iconTag)
		if !ok || len(items) != 1 {
			t.Fatal("icon missing")
		}
		pixels, err := pixeldata.ExtractView(items[0])
		if err != nil || pixels.Encapsulated != compress {
			t.Fatalf("icon encoded=%v err=%v", pixels.Encapsulated, err)
		}
		code, stdout, stderr = runConv(t, "-ts", "explicit-le", "-o", restored, output)
		backAction := "preserved-native"
		if compress {
			backAction = "decompressed"
		}
		if code != 0 || !strings.Contains(stdout, "icon (0088,0200)[0]: "+backAction) {
			t.Fatalf("back code=%d stdout=%q stderr=%q", code, stdout, stderr)
		}
		back, err := object.OpenFile(restored)
		if err != nil {
			t.Fatal(err)
		}
		defer back.Close()
		items, ok = back.Dataset.GetSequence(iconTag)
		if !ok || len(items) != 1 {
			t.Fatal("restored icon missing")
		}
		actual, _ := items[0].GetRaw(core.TagPixelData)
		want, _ := icon.GetRaw(core.TagPixelData)
		if !bytes.Equal(actual, want) {
			t.Fatal("CLI icon round-trip changed pixels")
		}
	}
}

func TestConvIconIncompleteMetadataReportContainsOnlyStructuralPath(t *testing.T) {
	lines := reportLines(pixeldata.TranscodeReport{Icons: []pixeldata.IconTranscodeReport{{Path: pixeldata.PixelDataPath{{Tag: core.NewTag(0x0088, 0x0200), ItemIndex: 0}}, Action: pixeldata.IconPixelsPreservedNative, MetadataIncomplete: true}}})
	want := "icon (0088,0200)[0]: preserved-native; metadata incomplete, pixels preserved without inference"
	if len(lines) != 1 || lines[0] != want {
		t.Fatalf("report=%q", lines)
	}
}

func TestConvExplicitVerificationDurabilityAndPipelinePolicy(t *testing.T) {
	input, _ := writeImage(t)
	out := filepath.Join(t.TempDir(), "atomic.dcm")
	code, stdout, stderr := runConv(t, "-ts", "rle", "-verify=false", "-durability", "atomic", "-workers", "2", "-in-flight-frames", "2", "-in-flight-bytes", "1048576", "-o", out, input)
	if code != 0 || !strings.Contains(stdout, "verification skipped") || !strings.Contains(stdout, "verification=skipped durability=atomic file-synced=false directory-synced=false") || !strings.Contains(stdout, "pipeline:") {
		t.Fatalf("policy: code=%d stdout=%q stderr=%q", code, stdout, stderr)
	}
	for _, flags := range [][]string{{"-workers", "-1"}, {"-in-flight-frames", "-1"}, {"-in-flight-bytes", "-1"}, {"-max-encoded-frame-bytes", "-1"}, {"-durability", "unknown"}} {
		args := append([]string{"-ts", "rle", "-o", out}, flags...)
		args = append(args, input)
		if code, _, stderr := runConv(t, args...); code != 2 || !strings.Contains(stderr, "invalid pipeline or durability policy") {
			t.Fatalf("%v: code=%d stderr=%q", flags, code, stderr)
		}
	}
	// Defaults preserve both safeguards; the stdout report describes the
	// executed policy rather than inferring it from a target codec name.
	code, stdout, stderr = runConv(t, "-ts", "rle", "-workers", "1", "-o", filepath.Join(t.TempDir(), "durable.dcm"), input)
	if code != 0 || !strings.Contains(stdout, "verification=pixel-exact durability=durable file-synced=true") {
		t.Fatalf("default policy: code=%d stdout=%q stderr=%q", code, stdout, stderr)
	}
}

func TestConvLossyVerificationFlags(t *testing.T) {
	base := []string{"-ts", "jpeg-ls-near", "-o", "out.dcm"}
	for _, tc := range []struct {
		name  string
		flags []string
		want  *float64
		block int
	}{
		{name: "disabled default"},
		{name: "disabled explicitly", flags: []string{"-max-pixel-error", "-1", "-verify=false"}},
		{name: "zero limit", flags: []string{"-max-pixel-error", "0"}, want: floatPointer(0)},
		{name: "fractional block limit", flags: []string{"-max-pixel-error", "0.25", "-average-block-size", "3"}, want: floatPointer(0.25), block: 3},
	} {
		t.Run(tc.name, func(t *testing.T) {
			args := append(append(append([]string(nil), base...), tc.flags...), "in.dcm")
			opts, err := parseArgs(args, &bytes.Buffer{})
			if err != nil {
				t.Fatal(err)
			}
			_, policy, err := transcodeOptions(opts)
			if err != nil {
				t.Fatal(err)
			}
			if (policy.MaxPixelError == nil) != (tc.want == nil) || policy.AverageBlockSize != tc.block ||
				(tc.want != nil && *policy.MaxPixelError != *tc.want) {
				t.Fatalf("verification flags not wired: max=%v block=%d", policy.MaxPixelError, policy.AverageBlockSize)
			}
			if policy.AllowLossy {
				t.Fatal("verification flags authorized lossy encoding")
			}
		})
	}
	for _, flags := range [][]string{
		{"-max-pixel-error", "NaN"}, {"-max-pixel-error", "+Inf"}, {"-max-pixel-error", "-Inf"},
		{"-max-pixel-error", "-0.5"}, {"-max-pixel-error", "-2"},
		{"-average-block-size", "-1"}, {"-average-block-size", "1"},
		{"-max-pixel-error", "-1", "-average-block-size", "2"},
		{"-max-pixel-error", "0", "-verify=false"},
	} {
		args := append(append(append([]string(nil), base...), flags...), "in.dcm")
		var stderr bytes.Buffer
		if _, err := parseArgs(args, &stderr); !errors.Is(err, errUsage) || !strings.Contains(stderr.String(), "invalid lossy verification policy") {
			t.Fatalf("%v: err=%v stderr=%q", flags, err, stderr.String())
		}
	}
}

func floatPointer(value float64) *float64 { return &value }

func TestConvLossyVerificationAcceptanceAndPublicationFailure(t *testing.T) {
	input, pixels := writeImage(t)
	dir := t.TempDir()
	output := filepath.Join(dir, "near.dcm")
	common := []string{"-ts", "jpeg-ls-near", "-allow-lossy", "-near", "1", "-o", output}
	code, stdout, stderr := runConv(t, append(append([]string(nil), common...), "-max-pixel-error", "1", input)...)
	if code != 0 || !strings.Contains(stdout, "lossy verification: maximum-error=1 maximum-sample-error=1 limit=1 average-block-size=1 frames=1 blocks=64") {
		t.Fatalf("accepted NEAR=1: code=%d stdout=%q stderr=%q", code, stdout, stderr)
	}
	decoded := filepath.Join(dir, "decoded.dcm")
	if code, _, stderr := runConv(t, "-ts", "explicit-le", "-o", decoded, output); code != 0 {
		t.Fatalf("decode accepted output: %s", stderr)
	}
	file, err := object.OpenFile(decoded)
	if err != nil {
		t.Fatal(err)
	}
	actual, _ := file.GetRaw(core.TagPixelData)
	if len(actual) < len(pixels) {
		_ = file.Close()
		t.Fatalf("decoded sample bytes=%d, want at least %d", len(actual), len(pixels))
	}
	maximum := 0
	for i, sample := range pixels {
		difference := int(sample) - int(actual[i])
		if difference < 0 {
			difference = -difference
		}
		if difference > maximum {
			maximum = difference
		}
	}
	if err := file.Close(); err != nil {
		t.Fatal(err)
	}
	if maximum != 1 {
		t.Fatalf("independent decoded sample maximum=%d, want 1", maximum)
	}
	code, stdout, stderr = runConv(t, append(append([]string(nil), common...), "-max-pixel-error", "1", "-average-block-size", "3", input)...)
	if code != 0 || !strings.Contains(stdout, "maximum-sample-error=1 limit=1 average-block-size=3 frames=1 blocks=9") {
		t.Fatalf("partial edge blocks: code=%d stdout=%q stderr=%q", code, stdout, stderr)
	}
	prior := []byte("previous output must remain")
	if err := os.WriteFile(output, prior, 0o600); err != nil {
		t.Fatal(err)
	}
	code, stdout, stderr = runConv(t, append(append([]string(nil), common...), "-max-pixel-error", "0", input)...)
	if code != 1 || stdout != "" || !strings.Contains(stderr, "lossy verification failed: frame=") ||
		!strings.Contains(stderr, "maximum-error=1 limit=0 average-block-size=1") {
		t.Fatalf("refused NEAR=1: code=%d stdout=%q stderr=%q", code, stdout, stderr)
	}
	got, err := os.ReadFile(output)
	if err != nil || !bytes.Equal(got, prior) {
		t.Fatalf("rejected conversion changed prior output: %q %v", got, err)
	}
	entries, err := os.ReadDir(dir)
	if err != nil || len(entries) != 2 {
		t.Fatalf("rejected conversion left temporaries: entries=%v err=%v", entries, err)
	}
	for _, sensitive := range []string{input, output, dicomtest.TestSOPInstanceUID, "TESTID001"} {
		if strings.Contains(stderr, sensitive) {
			t.Fatalf("failure diagnostic disclosed source details: %q", stderr)
		}
	}
}
