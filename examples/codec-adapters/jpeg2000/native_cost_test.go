//go:build codecfull

package jpeg2000

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"sort"
	"testing"
	"time"

	cost "github.com/ThalesMMS/dicom-go/examples/codec-cost"
	"github.com/ThalesMMS/dicom-go/pixeldata"
	"github.com/ThalesMMS/dicom-go/pixeldata/codecfixture"
)

type nativeCostFixture struct {
	name    string
	b, want []byte
	m       pixeldata.Metadata
}
type nativeCostReport struct {
	Schema      int                  `json:"schema"`
	Platform    string               `json:"platform"`
	Go          string               `json:"go"`
	Clock       string               `json:"clock"`
	Repetitions int                  `json:"repetitions"`
	Artifacts   map[string]string    `json:"artifactSHA256"`
	Versions    map[string]string    `json:"versions"`
	Notes       []string             `json:"notes"`
	Frames      []cost.FrameResult   `json:"frames"`
	Cohorts     []cost.CohortSummary `json:"cohorts"`
}

// This opt-in campaign shares codec-cost's Clock, StageRecord, FrameResult,
// exclusive-accounting validation and pixel oracle. Entire adapter calls are
// disclosed as combined spans; it never assigns cmd.Run to algorithm time.
// Run without -race or other simultaneous builds for performance evidence.
func TestNativeHelperFrameCost(t *testing.T) {
	reportPath := os.Getenv("DICOM_GO_J2K_COST_REPORT")
	if reportPath == "" {
		t.Skip("DICOM_GO_J2K_COST_REPORT not set")
	}
	helper := os.Getenv("DICOM_GO_J2K_NATIVE_HELPER")
	if helper == "" {
		t.Fatal("DICOM_GO_J2K_NATIVE_HELPER required")
	}
	report := nativeCostReport{Schema: 1, Platform: runtime.GOOS + "/" + runtime.GOARCH, Go: runtime.Version(), Clock: nativeCostClockName, Repetitions: 100, Artifacts: map[string]string{}, Versions: map[string]string{"openjpeg": QualifiedOpenJPEGVersion, "openjph": QualifiedOpenJPHVersion}, Notes: []string{
		"Non-PHI committed synthetic fixtures; no viewer, cache, PACS or clinical throughput claim.",
		"Exclusive launch span is the complete opaque adapter call (preflight, files/IPC, process launch where needed, native decode and sample conversion). These costs are not separately attributable.",
		"Cold helper span includes OpenNativeSession and version handshake; warm/switch reuse one process. Close and memory/hash sampling occur after the timed delivery.",
		"CLI creates two files per frame; helper creates none. I/O bytes count the known input/output payload once each; helper also counts 44/36-byte IPC headers.",
		"The helper uses the 128 MiB process memory limit from Twin's session option. RSS is not sampled here; AllocBytes/HeapInuseBytes refer to Go.",
		"100 paired alternating-order repetitions follow 5 untimed warmups. p95 uses nearest rank; cold means a fresh process, not a cleared OS file cache.",
	}}
	hashFile := func(path string) {
		b, e := os.ReadFile(path)
		if e != nil {
			t.Fatal(e)
		}
		h := sha256.Sum256(b)
		report.Artifacts[path] = hex.EncodeToString(h[:])
	}
	hashFile(helper)
	cases := codecfixture.JPEG2000LosslessEncoderCases()
	for _, engine := range []NativeBackend{NativeOpenJPEG, NativeOpenJPH} {
		cliPath := os.Getenv("DICOM_GO_OPENJPEG_DECOMPRESS")
		if engine == NativeOpenJPH {
			cliPath = os.Getenv("DICOM_GO_OPENJPH_EXPAND")
		}
		if cliPath == "" {
			t.Fatal("explicit CLI path required for ", engine)
		}
		resolved, e := exec.LookPath(cliPath)
		if e != nil {
			t.Fatal(e)
		}
		hashFile(resolved)
		var cli ContextDecoder
		if engine == NativeOpenJPEG {
			cli = newOpenJPEGDecoder(OpenJPEGExecutable(cliPath))
		} else {
			cli = newOpenJPHDecoder(OpenJPHExecutable(cliPath))
		}
		var fixtures []nativeCostFixture
		load := func(name, dir, ext string, id int) {
			path := filepath.Join(dir, name+ext)
			b, e := os.ReadFile(path)
			if e != nil {
				t.Fatal(e)
			}
			hashFile(path)
			fixtures = append(fixtures, nativeCostFixture{name: name, b: b, want: cases[id].ExpectedFrames[0], m: cases[id].Metadata})
		}
		if engine == NativeOpenJPEG {
			for _, i := range []int{0, 1, 3} {
				f := openJPEGFixtures[i]
				load(f.name, openJPEGFixtureDir, ".j2k", f.caseID)
			}
		} else {
			for _, i := range []int{0, 1, 5} {
				f := openJPHFixtures[i]
				load(f.name, openJPHFixtureDir, ".j2c", f.caseID)
			}
		}
		open := func() *NativeSession {
			s, e := OpenNativeSession(context.Background(), NativeSessionConfig{Executable: helper, Backend: engine, MaxNativeBytes: 128 << 20})
			if e != nil {
				t.Fatal(e)
			}
			return s
		}
		session := open()
		defer session.Close()
		for warmup := 0; warmup < 5; warmup++ {
			for _, f := range fixtures {
				for _, d := range []ContextDecoder{cli, session} {
					p, e := d.DecodeFrameContext(context.Background(), f.b, f.m)
					if e != nil || !bytes.Equal(p, f.want) {
						t.Fatalf("warmup %s: %v", f.name, e)
					}
				}
			}
		}
		now := nativeCostNow()
		measure := func(f nativeCostFixture, mode string, isHelper bool) cost.FrameResult {
			var before, after runtime.MemStats
			runtime.ReadMemStats(&before)
			clock := cost.NewClock(now)
			clock.Start()
			var d ContextDecoder = cli
			var s *NativeSession
			var starts uint64
			if isHelper {
				s = session
				if mode == cost.ModeCold {
					s = open()
					defer s.Close()
					starts = 0
				} else {
					starts = s.Stats().Starts
				}
				d = s
			}
			p, e := d.DecodeFrameContext(context.Background(), f.b, f.m)
			clock.Step(cost.StageLaunch)
			clock.Annotate("Complete adapter call, including any launch, execution, preparation and conversion; " + cost.CombinedLaunchExecuteNote())
			clock.Step(cost.StageDeliver)
			record := clock.Record()
			record.CombinedLaunchExecuteDisclosed = true
			if e != nil {
				t.Fatal(e)
			}
			if e = cost.ValidateExclusive(record.Spans); e != nil || record.ExclusiveTotal != record.FirstFrame {
				t.Fatalf("accounting: %v", e)
			}
			if e = cost.ComparePixels(p, f.want, int(f.m.BitsAllocated), false, 0); e != nil {
				t.Fatalf("%s/%s %v", engine, f.name, e)
			}
			runtime.ReadMemStats(&after)
			r := cost.FrameResult{Codec: string(engine), Backend: string(engine) + "-cli", Cohort: string(engine) + "-" + f.name, Mode: mode, StageRecord: record, Elapsed: record.FirstFrame, Equivalence: "exact-source", Launches: 1, TempFilesCreated: 2, IOBytes: int64(len(f.b) + len(p)), IOOps: 2, AllocBytes: after.TotalAlloc - before.TotalAlloc, HeapInuseBytes: after.HeapInuse}
			h := sha256.Sum256(p)
			r.PixelSHA256 = hex.EncodeToString(h[:])
			if isHelper {
				stats := s.Stats()
				r.Backend = string(engine) + "-helper"
				r.HelperPID = stats.PID
				r.Launches = int(stats.Starts - starts)
				r.TempFilesCreated = 0
				r.IOBytes += 80
				r.IOOps = 4
				if mode == cost.ModeCold {
					_ = s.Close()
				}
			}
			return r
		}
		for _, mode := range []string{cost.ModeWarm, cost.ModeSeriesSwitch, cost.ModeCold} {
			for repetition := 0; repetition < report.Repetitions; repetition++ {
				selected := fixtures
				if mode == cost.ModeSeriesSwitch {
					selected = []nativeCostFixture{fixtures[repetition%len(fixtures)]}
					selected[0].name = "series-switch"
				}
				if mode == cost.ModeCold {
					selected = fixtures[:1]
				}
				for _, f := range selected {
					order := []bool{false, true}
					if repetition%2 != 0 {
						order = []bool{true, false}
					}
					for _, isHelper := range order {
						report.Frames = append(report.Frames, measure(f, mode, isHelper))
					}
				}
			}
		}
		_ = session.Close()
	}
	groups := map[string][]cost.FrameResult{}
	for _, r := range report.Frames {
		key := r.Cohort + "/" + r.Backend + "/" + r.Mode
		groups[key] = append(groups[key], r)
	}
	keys := make([]string, 0, len(groups))
	for key := range groups {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	for _, key := range keys {
		frames := groups[key]
		values := make([]time.Duration, len(frames))
		for i, r := range frames {
			values[i] = r.StageRecord.FirstFrame
		}
		sort.Slice(values, func(i, j int) bool { return values[i] < values[j] })
		median := (values[(len(values)-1)/2] + values[len(values)/2]) / 2
		p95 := values[(95*len(values)+99)/100-1]
		f := frames[0]
		report.Cohorts = append(report.Cohorts, cost.CohortSummary{Name: f.Cohort, Backend: f.Backend, Mode: f.Mode, Median: cost.StageMedians{FirstFrame: median}, P95: cost.StageMedians{FirstFrame: p95}})
		t.Logf("%s median=%s p95=%s", key, median, p95)
	}
	if e := os.MkdirAll(filepath.Dir(reportPath), 0o755); e != nil {
		t.Fatal(e)
	}
	b, e := json.MarshalIndent(report, "", "  ")
	if e != nil {
		t.Fatal(e)
	}
	if e = os.WriteFile(reportPath, append(b, '\n'), 0o600); e != nil {
		t.Fatal(e)
	}
	// Performance is evidence, never a timing assertion on shared runners.
	t.Log(fmt.Sprintf("recorded %d exact-equivalent measured frames", len(report.Frames)))
}
