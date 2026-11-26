package pixeldata_test

import (
	"context"
	"encoding/binary"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"testing"
	"time"

	"github.com/ThalesMMS/dicom-go/core"
	"github.com/ThalesMMS/dicom-go/object"
	"github.com/ThalesMMS/dicom-go/pixeldata"
	"github.com/ThalesMMS/dicom-go/transfer"
)

// Shared with the existing broad campaign: no retuning of pixels or codec set.
func transcode1294Native(t testing.TB, dir string) string {
	t.Helper()
	dataset := streamTestSource(512, 512, 48, 1, 16)
	dataset.Put(core.Element{Header: core.ElementHeader{Tag: core.NewTag(0x0008, 0x0016), VR: core.VRUI}, Value: core.StringValue{"1.2.840.10008.5.1.4.1.1.7.3"}})
	dataset.Put(core.NewRawElement(core.NewTag(0x0028, 0x0101), core.VRUS, binary.LittleEndian.AppendUint16(nil, 12)))
	dataset.Put(core.NewRawElement(core.NewTag(0x0028, 0x0102), core.VRUS, binary.LittleEndian.AppendUint16(nil, 11)))
	return writeStreamTestFile(t, dir, "native.dcm", &object.File{Dataset: dataset, TransferSyntax: transfer.ExplicitVRLittleEndian})
}

func transcode1294AcceptanceOptions(t testing.TB, workers int) pixeldata.TranscodeOptions {
	options := transcode1294Options(t)
	options.Pipeline = pixeldata.TranscodePipelineOptions{Workers: workers, MaxInFlightFrames: workers, MaxInFlightBytes: 256 << 20}
	return options // Pixel-exact verification and durable publication stay enabled.
}

func transcode1294Codecs() []transfer.Syntax {
	return []transfer.Syntax{transfer.RLELossless, transfer.JPEGLSLossless, transfer.JPEGLosslessSV1, transfer.DeflatedImageFrameCompression}
}

type transcode1294AcceptancePlan struct {
	Schema           int       `json:"schema"`
	GOOS             string    `json:"goos"`
	GOARCH           string    `json:"goarch"`
	GoVersion        string    `json:"go_version"`
	GOMAXPROCS       int       `json:"gomaxprocs"`
	InputSHA256      string    `json:"input_sha256"`
	InputBytes       int64     `json:"input_bytes"`
	Fixture          string    `json:"fixture"`
	Codecs           []string  `json:"codecs"`
	WorkerOrder      [3][2]int `json:"worker_order_per_repetition"`
	Verification     string    `json:"verification"`
	Durability       string    `json:"durability"`
	MaxInFlightBytes int64     `json:"max_in_flight_bytes"`
}

func transcode1294Plan(hash string, size int64) transcode1294AcceptancePlan {
	return transcode1294AcceptancePlan{1, runtime.GOOS, runtime.GOARCH, runtime.Version(), runtime.GOMAXPROCS(0), hash, size,
		"512x512x48 MONOCHROME2 unsigned12 in16; sample=(i*7+frame*131+row*3)%4096; synthetic",
		[]string{transfer.RLELossless.UID, transfer.JPEGLSLossless.UID, transfer.JPEGLosslessSV1.UID, transfer.DeflatedImageFrameCompression.UID},
		[3][2]int{{1, 12}, {12, 1}, {1, 12}}, "pixel-exact", "durable", 256 << 20}
}

func writeTranscode1294JSON(t testing.TB, path string, value any) {
	t.Helper()
	data, err := json.MarshalIndent(value, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, append(data, '\n'), 0600); err != nil {
		t.Fatal(err)
	}
}

func runTranscode1294Acceptance(t *testing.T, artifactDir string) {
	input := transcode1294Native(t, t.TempDir())
	hash, size := transcode1294Hash(t, input)
	plan := transcode1294Plan(hash, size)
	// Persist corpus, policies and pairing BEFORE any timing sample.
	writeTranscode1294JSON(t, filepath.Join(artifactDir, "plan.json"), plan)
	var cells []transcode1294Cell
	type comparison struct {
		Codec              string
		ParallelOverSerial float64
	}
	var comparisons []comparison
	anyGain, noRegression := false, true
	for _, target := range transcode1294Codecs() {
		uid := target.UID
		pair := [2]transcode1294Cell{{Codec: uid, Operation: "encode-path", Workers: 1, Verify: true, Durability: "durable"}, {Codec: uid, Operation: "encode-path", Workers: 12, Verify: true, Durability: "durable"}}
		output := filepath.Join(t.TempDir(), "output.dcm")
		var expected string
		// One matched-policy warmup per mode. Hashing is outside the API timer.
		for _, workers := range []int{1, 12} {
			if _, err := pixeldata.TranscodePath(context.Background(), input, output, target, transcode1294AcceptanceOptions(t, workers)); err != nil {
				t.Fatal(err)
			}
			got, _ := transcode1294Hash(t, output)
			if expected != "" && got != expected {
				t.Fatal("warmup output differs")
			}
			expected = got
			if err := os.Remove(output); err != nil {
				t.Fatal(err)
			}
		}
		for _, order := range plan.WorkerOrder {
			for _, workers := range order {
				runtime.GC()
				options := transcode1294AcceptanceOptions(t, workers)
				start := time.Now()
				report, err := pixeldata.TranscodePath(context.Background(), input, output, target, options)
				elapsed := time.Since(start)
				if err != nil {
					t.Fatal(err)
				}
				got, bytes := transcode1294Hash(t, output)
				if got != expected {
					t.Fatal("serial/parallel output differs", uid)
				}
				if report.Verification != pixeldata.TranscodeVerificationPixelExact || report.Durability != pixeldata.TranscodeDurable || !report.FileSynced || report.PeakInFlightFrames > workers || report.PeakInFlightBytes > plan.MaxInFlightBytes {
					t.Fatalf("policy/window mismatch: %+v", report)
				}
				slot := 0
				if workers == 12 {
					slot = 1
				}
				pair[slot].Samples = append(pair[slot].Samples, transcode1294Sample{ElapsedNS: elapsed.Nanoseconds(), OutputBytes: bytes, OutputSHA256: got, Report: report})
				if err := os.Remove(output); err != nil {
					t.Fatal(err)
				}
			}
		}
		pair[0], pair[1] = finishTranscode1294Cell(pair[0]), finishTranscode1294Cell(pair[1])
		ratio := float64(pair[1].MedianNS) / float64(pair[0].MedianNS)
		anyGain = anyGain || ratio <= .8
		noRegression = noRegression && ratio <= 1.1
		comparisons = append(comparisons, comparison{uid, ratio})
		cells = append(cells, pair[:]...)
	}
	writeTranscode1294JSON(t, filepath.Join(artifactDir, "campaign.json"), struct {
		Plan                      transcode1294AcceptancePlan `json:"plan"`
		Cells                     []transcode1294Cell         `json:"cells"`
		Comparisons               []comparison                `json:"comparisons"`
		AnyGain20Percent          bool                        `json:"any_gain_at_least_20_percent"`
		NoRegressionOver10Percent bool                        `json:"no_regression_over_10_percent"`
	}{plan, cells, comparisons, anyGain, noRegression})
	if !anyGain || !noRegression {
		t.Error("campaign failed finite performance acceptance; all four results retained")
	}
}

type transcode1294Memory struct {
	WorkingSetBytes     uint64 `json:"working_set_bytes"`
	PeakWorkingSetBytes uint64 `json:"peak_working_set_bytes"`
}

type transcode1294MemoryResult struct {
	PID                             int
	GOOS, GOARCH, GoVersion, Metric string
	GOMAXPROCS                      int
	Before, After                   transcode1294Memory
	Report                          pixeldata.TranscodeReport
	OutputSHA256                    string
	OutputBytes                     int64
}

// A separate opt-in test keeps fixture construction, warmups, and other codecs
// out of the measured child process. Its peak includes runtime/startup, not just
// the API. It is one representative observation, not a universal memory bound.
func TestTranscode1294RSSCampaign(t *testing.T) {
	dir := os.Getenv("DICOMGO_TRANSCODE_RSS_DIR")
	if dir == "" {
		t.Skip("explicit isolated Windows memory scenario only")
	}
	if runtime.GOOS != "windows" || runtime.GOARCH != "amd64" || runtime.GOMAXPROCS(0) != 12 || !filepath.IsAbs(dir) {
		t.Fatal("requires Windows/amd64, GOMAXPROCS12 and absolute artifact directory")
	}
	if err := os.MkdirAll(dir, 0700); err != nil {
		t.Fatal(err)
	}
	input := transcode1294Native(t, t.TempDir())
	hash, size := transcode1294Hash(t, input)
	plan := transcode1294Plan(hash, size)
	oracle := filepath.Join(t.TempDir(), "serial.dcm")
	if _, err := pixeldata.TranscodePath(context.Background(), input, oracle, transfer.JPEGLSLossless, transcode1294AcceptanceOptions(t, 1)); err != nil {
		t.Fatal(err)
	}
	expectedHash, expectedSize := transcode1294Hash(t, oracle)
	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	executableHash, _ := transcode1294Hash(t, executable)
	writeTranscode1294JSON(t, filepath.Join(dir, "memory-plan.json"), struct {
		Plan                 transcode1294AcceptancePlan
		Codec                string
		Workers              int
		ExecutableSHA256     string
		ExpectedOutputSHA256 string
		Note                 string
	}{plan, transfer.JPEGLSLossless.UID, 12, executableHash, expectedHash, "one fresh child; one call; no warmup; fixture preparation and hashing outside child; PeakWorkingSetSize includes runtime"})
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	command := exec.CommandContext(ctx, executable, "-test.run=^TestTranscode1294RSSWorker$", "-test.timeout=90s", "-test.v")
	command.Env = append(os.Environ(), "DICOMGO_TRANSCODE_RSS_INPUT="+input, "DICOMGO_TRANSCODE_RSS_RESULT="+filepath.Join(dir, "memory.json"))
	log, err := command.CombinedOutput()
	if writeErr := os.WriteFile(filepath.Join(dir, "memory-worker.log"), log, 0600); writeErr != nil {
		t.Fatal(writeErr)
	}
	if err != nil {
		t.Fatalf("memory child: %v\n%s", err, log)
	}
	data, err := os.ReadFile(filepath.Join(dir, "memory.json"))
	if err != nil {
		t.Fatal(err)
	}
	var result transcode1294MemoryResult
	if err := json.Unmarshal(data, &result); err != nil {
		t.Fatal(err)
	}
	if result.PID == os.Getpid() || result.PID <= 0 || result.OutputSHA256 != expectedHash || result.OutputBytes != expectedSize || result.GOMAXPROCS != 12 || result.Report.Verification != pixeldata.TranscodeVerificationPixelExact || result.Report.Durability != pixeldata.TranscodeDurable || !result.Report.FileSynced {
		t.Fatalf("memory child identity/output/policy mismatch: %+v", result)
	}
	if afterHash, afterSize := transcode1294Hash(t, input); afterHash != hash || afterSize != size {
		t.Fatal("memory input changed")
	}
}

func TestTranscode1294RSSWorker(t *testing.T) {
	input, result := os.Getenv("DICOMGO_TRANSCODE_RSS_INPUT"), os.Getenv("DICOMGO_TRANSCODE_RSS_RESULT")
	if input == "" || result == "" {
		t.Skip("isolated memory child only")
	}
	if runtime.GOOS != "windows" || runtime.GOARCH != "amd64" || runtime.GOMAXPROCS(0) != 12 {
		t.Fatal("unexpected memory child environment")
	}
	options := transcode1294AcceptanceOptions(t, 12)
	output := filepath.Join(t.TempDir(), "output.dcm")
	before, err := transcode1294ProcessMemory()
	if err != nil {
		t.Fatal(err)
	}
	report, err := pixeldata.TranscodePath(context.Background(), input, output, transfer.JPEGLSLossless, options)
	if err != nil {
		t.Fatal(err)
	}
	after, err := transcode1294ProcessMemory()
	if err != nil {
		t.Fatal(err)
	}
	if after.PeakWorkingSetBytes == 0 || after.PeakWorkingSetBytes < after.WorkingSetBytes {
		t.Fatal("invalid process memory counters")
	}
	hash, size := transcode1294Hash(t, output) // After capturing the high-water mark.
	writeTranscode1294JSON(t, result, transcode1294MemoryResult{os.Getpid(), runtime.GOOS, runtime.GOARCH, runtime.Version(), "Windows GetProcessMemoryInfo: PeakWorkingSetSize; process lifetime high-water bytes", runtime.GOMAXPROCS(0), before, after, report, hash, size})
}

func TestTranscode1294AcceptancePlanPreservesFinitePolicies(t *testing.T) {
	plan := transcode1294Plan("fixture-hash", 123)
	if len(plan.Codecs) != 4 || plan.WorkerOrder != [3][2]int{{1, 12}, {12, 1}, {1, 12}} || plan.Verification != "pixel-exact" || plan.Durability != "durable" || plan.InputSHA256 != "fixture-hash" || plan.InputBytes != 123 {
		t.Fatal(plan)
	}
	for _, workers := range []int{1, 12} {
		o := transcode1294AcceptanceOptions(t, workers)
		if o.SkipVerification || o.Durability != pixeldata.TranscodeDurable || o.Pipeline.Workers != workers || o.Pipeline.MaxInFlightFrames != workers || o.Pipeline.MaxInFlightBytes != 256<<20 {
			t.Fatal(o)
		}
	}
}
