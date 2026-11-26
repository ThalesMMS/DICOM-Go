package compare_test

import (
	"bytes"
	"context"
	"errors"
	"math"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"testing"
	"time"

	"github.com/ThalesMMS/dicom-go/core"
	"github.com/ThalesMMS/dicom-go/imagebuild"
	"github.com/ThalesMMS/dicom-go/internal/dcmtkoracle"
	"github.com/ThalesMMS/dicom-go/internal/dicomtest"
	"github.com/ThalesMMS/dicom-go/object"
	"github.com/ThalesMMS/dicom-go/pixeldata"
	"github.com/ThalesMMS/dicom-go/pixeldata/builtin"
	"github.com/ThalesMMS/dicom-go/pixeldata/compare"
	"github.com/ThalesMMS/dicom-go/pixeldata/jpegls"
	"github.com/ThalesMMS/dicom-go/transfer"
)

func near(a, b float64) bool {
	return math.Abs(a-b) <= 1e-9*math.Max(1, math.Abs(b))
}

func TestAccumulator(t *testing.T) {
	var acc compare.Accumulator
	for i, r := range []float64{0, 10, 20, 30} {
		acc.Add(r, []float64{1, 10, 18, 30}[i])
	}
	m := acc.Metrics()
	want := compare.Metrics{Samples: 4, MaxAbsoluteError: 2, MeanAbsoluteError: 0.75, RootMeanSquareError: math.Sqrt(1.25),
		PeakSignalToNoiseRatio: 10 * math.Log10(900/1.25), SignalToNoiseRatio: 10 * math.Log10(1400.0/5)}
	if m.Samples != want.Samples || !near(m.MaxAbsoluteError, want.MaxAbsoluteError) || !near(m.MeanAbsoluteError, want.MeanAbsoluteError) ||
		!near(m.RootMeanSquareError, want.RootMeanSquareError) || !near(m.PeakSignalToNoiseRatio, want.PeakSignalToNoiseRatio) ||
		!near(m.SignalToNoiseRatio, want.SignalToNoiseRatio) {
		t.Fatalf("metrics %+v, want %+v", m, want)
	}
	var same compare.Accumulator
	same.Add(5, 5)
	if m := same.Metrics(); !m.Identical() || !math.IsInf(m.PeakSignalToNoiseRatio, 1) || !math.IsInf(m.SignalToNoiseRatio, 1) {
		t.Fatalf("identical %+v", m)
	}
}

// ctFile builds a synthetic 16x16 CT image with a -1024 intercept, stored
// unsigned.
func ctFile(t *testing.T, offset int) *object.File {
	t.Helper()
	geometry := imagebuild.Geometry{Rows: 16, Columns: 16, PixelSpacing: [2]float64{1, 1},
		RowDirection: [3]float64{1, 0, 0}, ColumnDirection: [3]float64{0, 1, 0}, SliceThickness: 1}
	values := make([]float64, 256)
	for p := range values {
		values[p] = float64(-1000 + (p*997+offset)%3000)
	}
	files, err := imagebuild.Build(geometry, []imagebuild.Slice{{Values: values}}, imagebuild.Options{IOD: imagebuild.CT, RescaleType: "HU",
		Rescale: &imagebuild.Rescale{Slope: 1, Intercept: -1024}, ContentTime: time.Date(2026, 10, 7, 12, 0, 0, 0, time.UTC)})
	if err != nil {
		t.Fatal(err)
	}
	return files[0]
}

func writeFile(t *testing.T, path string, file *object.File) string {
	t.Helper()
	var buf bytes.Buffer
	if err := object.WriteFile(&buf, file); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, buf.Bytes(), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

func readFile(t *testing.T, path string) *object.File {
	t.Helper()
	source, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = source.Close() }()
	file, err := object.ReadFile(source)
	if err != nil {
		t.Fatal(err)
	}
	return file
}

// nearLossless transcodes source with JPEG-LS NEAR n.
func nearLossless(t *testing.T, source string, n int) string {
	t.Helper()
	decoders, err := builtin.NewRegistry()
	if err != nil {
		t.Fatal(err)
	}
	encoders := pixeldata.NewMemoryEncoderRegistry()
	if err := jpegls.RegisterNearLosslessEncoder(encoders, jpegls.EncoderOptions{Near: n, AllowLossy: true}); err != nil {
		t.Fatal(err)
	}
	out := filepath.Join(filepath.Dir(source), "near.dcm")
	if _, err := pixeldata.TranscodePath(context.Background(), source, out, transfer.JPEGLSNearLossless,
		pixeldata.TranscodeOptions{DecoderRegistry: decoders, EncoderRegistry: encoders, AllowLossy: true}); err != nil {
		t.Fatal(err)
	}
	return out
}

func TestFiles(t *testing.T) {
	dir := t.TempDir()
	reference := writeFile(t, filepath.Join(dir, "ref.dcm"), ctFile(t, 0))
	test := nearLossless(t, reference, 3)
	ctx := context.Background()
	modality, err := compare.Files(ctx, readFile(t, reference), readFile(t, test), compare.Options{})
	if err != nil {
		t.Fatal(err)
	}
	if modality.Frames != 1 || modality.Samples != 256 || modality.MaxAbsoluteError == 0 || modality.MaxAbsoluteError > 3 || math.IsInf(modality.PeakSignalToNoiseRatio, 0) {
		t.Fatalf("NEAR 3: %+v", modality)
	}
	// Stored values differ by the same amounts; only the signal changes.
	stored, err := compare.Files(ctx, readFile(t, reference), readFile(t, test), compare.Options{Domain: compare.DomainStored})
	if err != nil {
		t.Fatal(err)
	}
	if stored.MaxAbsoluteError != modality.MaxAbsoluteError || !near(stored.RootMeanSquareError, modality.RootMeanSquareError) || near(stored.PeakSignalToNoiseRatio, modality.PeakSignalToNoiseRatio) {
		t.Fatalf("stored %+v against modality %+v", stored, modality)
	}
	if same, err := compare.Files(ctx, readFile(t, reference), readFile(t, reference), compare.Options{}); err != nil || !same.Identical() {
		t.Fatalf("identical: %+v %v", same, err)
	}

	// The same samples as 8 rows of 32 columns do not compare.
	reshaped := ctFile(t, 0)
	reshaped.Dataset.Put(dicomtest.NewUShortElement(core.NewTag(0x0028, 0x0010), 8))
	reshaped.Dataset.Put(dicomtest.NewUShortElement(core.NewTag(0x0028, 0x0011), 32))
	if _, err := compare.Files(ctx, readFile(t, reference), reshaped, compare.Options{}); !errors.Is(err, compare.ErrMismatch) {
		t.Fatalf("different rows: %v", err)
	}
}

// rgbFile writes a 4x4 interleaved 8-bit RGB image whose samples depend on
// seed.
func rgbFile(t *testing.T, path string, seed int) string {
	t.Helper()
	pixels := make([]byte, 48)
	for i := range pixels {
		pixels[i] = byte(i*13 + (i%3)*seed*7)
	}
	data, err := dicomtest.Part10File(transfer.ExplicitVRLittleEndian,
		dicomtest.NewUIElement(core.NewTag(0x0008, 0x0016), "1.2.840.10008.5.1.4.1.1.7"),
		dicomtest.NewUIElement(core.NewTag(0x0008, 0x0018), "1.2.826.0.1.3680043.10.1.1196.1"),
		dicomtest.NewUShortElement(core.NewTag(0x0028, 0x0002), 3),
		dicomtest.NewStringElement(core.NewTag(0x0028, 0x0004), core.VRCS, "RGB"),
		dicomtest.NewUShortElement(core.NewTag(0x0028, 0x0006), 0),
		dicomtest.NewUShortElement(core.NewTag(0x0028, 0x0010), 4),
		dicomtest.NewUShortElement(core.NewTag(0x0028, 0x0011), 4),
		dicomtest.NewUShortElement(core.NewTag(0x0028, 0x0100), 8),
		dicomtest.NewUShortElement(core.NewTag(0x0028, 0x0101), 8),
		dicomtest.NewUShortElement(core.NewTag(0x0028, 0x0102), 7),
		dicomtest.NewUShortElement(core.NewTag(0x0028, 0x0103), 0),
		dicomtest.NewOBElement(core.TagPixelData, pixels),
	)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

var dcmicmpLine = regexp.MustCompile(`(?m)^(Max Absolute Error|Mean Absolute Error \(MAE\)|Root Mean Square Error \(RMSE\)|Peak Signal to Noise Ratio \(PSNR\) \[dB\]|Signal to Noise Ratio \(SNR\) \[dB\])\s*= (\S+)$`)

// TestDCMTKMetrics (opt-in) compares the metrics with DCMTK dcmicmp on a
// near-lossless transcode and on a modified image.
func TestDCMTKMetrics(t *testing.T) {
	tool := dcmtkoracle.Tool(t, "dcmicmp")
	t.Log("tolerance: 1e-5 relative (dcmicmp prints 6 significant digits)")
	dir := t.TempDir()
	reference := writeFile(t, filepath.Join(dir, "ref.dcm"), ctFile(t, 0))
	// dcmicmp has no JPEG-LS decoder: compare the decoded transcode.
	decoders, err := builtin.NewRegistry()
	if err != nil {
		t.Fatal(err)
	}
	decoded, err := pixeldata.DecompressFileContext(context.Background(), readFile(t, nearLossless(t, reference, 3)), pixeldata.DecompressOptions{Registry: decoders})
	if err != nil {
		t.Fatal(err)
	}
	pairs := map[string]string{
		"near-lossless": writeFile(t, filepath.Join(dir, "decoded.dcm"), decoded),
		"other image":   writeFile(t, filepath.Join(dir, "other.dcm"), ctFile(t, 7)),
		"rgb":           rgbFile(t, filepath.Join(dir, "rgb-test.dcm"), 5),
	}
	references := map[string]string{"rgb": rgbFile(t, filepath.Join(dir, "rgb-ref.dcm"), 0)}
	for name, test := range pairs {
		reference := reference
		if other, ok := references[name]; ok {
			reference = other
		}
		ours, err := compare.Files(context.Background(), readFile(t, reference), readFile(t, test), compare.Options{})
		if err != nil {
			t.Fatal(err)
		}
		output, err := exec.Command(tool, reference, test).CombinedOutput()
		if err != nil {
			t.Fatalf("%s: dcmicmp: %v\n%s", name, err, output)
		}
		theirs := map[string]float64{}
		for _, match := range dcmicmpLine.FindAllStringSubmatch(string(output), -1) {
			value, err := strconv.ParseFloat(match[2], 64)
			if err != nil {
				t.Fatalf("%s: %q: %v", name, match[0], err)
			}
			theirs[match[1]] = value
		}
		for label, value := range map[string]float64{
			"Max Absolute Error":                     ours.MaxAbsoluteError,
			"Mean Absolute Error (MAE)":              ours.MeanAbsoluteError,
			"Root Mean Square Error (RMSE)":          ours.RootMeanSquareError,
			"Peak Signal to Noise Ratio (PSNR) [dB]": ours.PeakSignalToNoiseRatio,
			"Signal to Noise Ratio (SNR) [dB]":       ours.SignalToNoiseRatio,
		} {
			want, ok := theirs[label]
			if !ok {
				t.Fatalf("%s: dcmicmp printed no %q:\n%s", name, label, output)
			}
			if math.Abs(value-want) > 1e-5*math.Max(1, math.Abs(want)) {
				t.Errorf("%s: %s = %v, dcmicmp %v", name, label, value, want)
			}
		}
	}
}
