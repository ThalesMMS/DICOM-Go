package imagefilter

import (
	"context"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"errors"
	"math"
	"math/rand"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"testing"
	"time"
)

type phase1548Case struct {
	Angle    float64 `json:"angle_degrees"`
	Width    float64 `json:"width_pixels"`
	Contrast float64 `json:"contrast"`
	Noise    float64 `json:"noise_sd"`
	Seed     int64   `json:"seed"`
}

const phase1548PlanSHA256 = "ae35b18f5661a0383d6f2a1f8548c222f314e13b7f45fb7f493371adc7de43ab"

type phase1548Plan struct {
	Dims    [3]int     `json:"dims"`
	Spacing [3]float64 `json:"spacing_mm"`
	Scales  []float64  `json:"scales_mm"`
	Phase   struct {
		Threshold float64 `json:"threshold"`
		Noise     float64 `json:"noise_threshold"`
		Floor     float64 `json:"amplitude_floor"`
	} `json:"phase"`
	Baseline struct {
		Threshold float64 `json:"threshold"`
		Beta      float64 `json:"beta"`
		Workers   int     `json:"workers"`
	} `json:"baseline"`
	Corpus struct {
		Development []phase1548Case `json:"development"`
		HeldOut     []phase1548Case `json:"held_out"`
	} `json:"corpus"`
	Gate struct {
		Recall   float64 `json:"recall_improvement_absolute"`
		FP       float64 `json:"false_positive_rate_increase_max"`
		Location float64 `json:"median_localization_pixels_max"`
		Bytes    int64   `json:"working_bytes_max"`
		Seconds  float64 `json:"elapsed_per_case_seconds_max"`
	} `json:"promotion_gate"`
}

type phase1548Metric struct {
	TP, Positive, FP, Negative                    int
	Recall, FalsePositiveRate, LocalizationPixels float64
	ElapsedNS                                     int64
	AllocatedBytes                                uint64
	WorkingBytes                                  int64
	ResponseSHA256                                string
}

type phase1548CaseResult struct {
	Parameters    phase1548Case
	HeldOut       bool
	SourceSHA256  string
	Phase, Frangi phase1548Metric
	Metadata      Wavelet2DMetadata
}

// The opt-in campaign consumes a committed preregistered corpus/gate. A failed
// promotion gate is a recorded research result, not a test to tune until green.
func TestPhase2DResearchCampaign(t *testing.T) {
	reportPath := os.Getenv("DICOM_PHASE1548_REPORT")
	if reportPath == "" {
		t.Skip("opt-in; set DICOM_PHASE1548_REPORT to a temporary JSON destination")
	}
	planBytes, err := os.ReadFile(filepath.Join("testdata", "phase1548", "plan.json"))
	if err != nil {
		t.Fatal(err)
	}
	var plan phase1548Plan
	if err := json.Unmarshal(planBytes, &plan); err != nil {
		t.Fatal(err)
	}
	planSum := sha256.Sum256(planBytes)
	if hex.EncodeToString(planSum[:]) != phase1548PlanSHA256 {
		t.Fatal("campaign plan changed; preregister and qualify a new campaign")
	}
	now, frequency, clock := phase2DCampaignClock(t)
	report := struct {
		PlanSHA256, GOOS, GOARCH, GoVersion, Clock                              string
		ClockFrequency                                                          int64
		Cases                                                                   []phase1548CaseResult
		PhaseRecall, FrangiRecall, PhaseFPR, FrangiFPR, MedianPhaseLocalization float64
		PromotionGatePassed                                                     bool
		Decision                                                                string
		CancelTotalNS, CancelRequestAfterNS, CancelToReturnNS                   int64
	}{GOOS: runtime.GOOS, GOARCH: runtime.GOARCH, GoVersion: runtime.Version(), Clock: clock, ClockFrequency: frequency, Decision: "experimental; no clinical preset"}
	report.PlanSHA256 = hex.EncodeToString(planSum[:])
	all := append(append([]phase1548Case(nil), plan.Corpus.Development...), plan.Corpus.HeldOut...)
	var phaseTP, frangiTP, positive, phaseFP, frangiFP, negative int
	var locations []float64
	resourcesOK := true
	for index, c := range all {
		src := NewVolume(plan.Dims, plan.Spacing)
		rng := rand.New(rand.NewSource(c.Seed))
		for y := 0; y < src.Dims[1]; y++ {
			for x := 0; x < src.Dims[0]; x++ {
				d := phase1548Distance(x, y, src.Dims, c.Angle)
				src.Data[y*src.Dims[0]+x] = float32(c.Contrast*math.Exp(-d*d/(2*c.Width*c.Width)) + c.Noise*rng.NormFloat64())
			}
		}
		before := phase1548Hash(src.Data)
		wavelet := Wavelet2DOptions{ScalesMM: plan.Scales, Boundary: WaveletReflect, Limits: Phase2DLimits{MaxWorkingBytes: plan.Gate.Bytes}}
		runtime.GC()
		var memBefore, memAfter runtime.MemStats
		runtime.ReadMemStats(&memBefore)
		start := now()
		phase, err := PhaseSymmetry2D(context.Background(), src, PhaseSymmetry2DOptions{Wavelet: wavelet, NoiseThreshold: plan.Phase.Noise, AmplitudeFloor: plan.Phase.Floor, Polarity: Phase2DBright})
		end := now()
		if err != nil {
			t.Fatal(err)
		}
		runtime.ReadMemStats(&memAfter)
		pm := phase1548Metrics(phase.Symmetry, c, plan.Phase.Threshold)
		pm.ElapsedNS = (end - start) * 1e9 / frequency
		pm.AllocatedBytes = memAfter.TotalAlloc - memBefore.TotalAlloc
		pm.WorkingBytes = phase.Metadata.WorkingBytes
		runtime.GC()
		runtime.ReadMemStats(&memBefore)
		start = now()
		frangi, err := Vesselness(context.Background(), src, VesselnessOptions{SigmasMM: plan.Scales, Beta: plan.Baseline.Beta, Workers: plan.Baseline.Workers})
		end = now()
		if err != nil {
			t.Fatal(err)
		}
		runtime.ReadMemStats(&memAfter)
		fm := phase1548Metrics(frangi.Response, c, plan.Baseline.Threshold)
		fm.ElapsedNS = (end - start) * 1e9 / frequency
		fm.AllocatedBytes = memAfter.TotalAlloc - memBefore.TotalAlloc
		fm.WorkingBytes = frangi.WorkingBytes
		if before != phase1548Hash(src.Data) {
			t.Fatal("campaign mutated the source")
		}
		held := index >= len(plan.Corpus.Development)
		report.Cases = append(report.Cases, phase1548CaseResult{Parameters: c, HeldOut: held, SourceSHA256: before, Phase: pm, Frangi: fm, Metadata: phase.Metadata})
		if held {
			phaseTP += pm.TP
			frangiTP += fm.TP
			positive += pm.Positive
			phaseFP += pm.FP
			frangiFP += fm.FP
			negative += pm.Negative
			locations = append(locations, pm.LocalizationPixels)
		}
		resourcesOK = resourcesOK && pm.WorkingBytes <= plan.Gate.Bytes && float64(pm.ElapsedNS)/1e9 <= plan.Gate.Seconds
		// Preserve the numeric outputs and source independently of PNG visualization.
		for suffix, values := range map[string][]float32{"source": src.Data, "phase": phase.Symmetry.Data, "frangi": frangi.Response.Data} {
			name := filepath.Join(filepath.Dir(reportPath), "case-"+string(rune('0'+index))+"-"+suffix+".f32")
			if err := phase1548Write(values, name); err != nil {
				t.Fatal(err)
			}
		}
	}
	report.PhaseRecall = float64(phaseTP) / float64(positive)
	report.FrangiRecall = float64(frangiTP) / float64(positive)
	report.PhaseFPR = float64(phaseFP) / float64(negative)
	report.FrangiFPR = float64(frangiFP) / float64(negative)
	sort.Float64s(locations)
	report.MedianPhaseLocalization = (locations[1] + locations[2]) / 2
	report.PromotionGatePassed = resourcesOK && report.PhaseRecall-report.FrangiRecall >= plan.Gate.Recall && report.PhaseFPR-report.FrangiFPR <= plan.Gate.FP && report.MedianPhaseLocalization <= plan.Gate.Location
	// Request cancellation during a larger public call, using QPC on Windows.
	cancelSrc := NewVolume([3]int{512, 512, 1}, [3]float64{1, 1, 0})
	for i := range cancelSrc.Data {
		cancelSrc.Data[i] = float32(i % 17)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	requested := make(chan int64, 1)
	start := now()
	timer := time.AfterFunc(5*time.Millisecond, func() { at := now(); cancel(); requested <- at })
	cancelled, err := PhaseSymmetry2D(ctx, cancelSrc, PhaseSymmetry2DOptions{Wavelet: Wavelet2DOptions{ScalesMM: plan.Scales}})
	end := now()
	timer.Stop()
	if cancelled != nil || !errors.Is(err, context.Canceled) {
		t.Fatalf("public cancellation published a result: %v %v", cancelled, err)
	}
	at := <-requested
	report.CancelTotalNS = (end - start) * 1e9 / frequency
	report.CancelRequestAfterNS = (at - start) * 1e9 / frequency
	report.CancelToReturnNS = (end - at) * 1e9 / frequency
	data, err := json.MarshalIndent(report, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(reportPath, append(data, '\n'), 0600); err != nil {
		t.Fatal(err)
	}
	t.Logf("held-out recall phase=%.4f Frangi=%.4f; FPR phase=%.4f Frangi=%.4f; gate=%v; report=%s", report.PhaseRecall, report.FrangiRecall, report.PhaseFPR, report.FrangiFPR, report.PromotionGatePassed, reportPath)
}

func phase1548Distance(x, y int, dims [3]int, angle float64) float64 {
	theta := angle * math.Pi / 180
	return (float64(x)-float64(dims[0]-1)/2)*math.Cos(theta) + (float64(y)-float64(dims[1]-1)/2)*math.Sin(theta)
}

func phase1548Metrics(v *Volume, c phase1548Case, threshold float64) phase1548Metric {
	m := phase1548Metric{ResponseSHA256: phase1548Hash(v.Data)}
	maximum := -1.0
	for y := 0; y < v.Dims[1]; y++ {
		for x := 0; x < v.Dims[0]; x++ {
			xp, yp := float64(x)-float64(v.Dims[0]-1)/2, float64(y)-float64(v.Dims[1]-1)/2
			if xp*xp+yp*yp > 24*24 {
				continue
			}
			d := math.Abs(phase1548Distance(x, y, v.Dims, c.Angle))
			value := float64(v.Data[y*v.Dims[0]+x])
			hit := value >= threshold
			if d <= .75 {
				m.Positive++
				if hit {
					m.TP++
				}
			}
			if d >= 3*c.Width {
				m.Negative++
				if hit {
					m.FP++
				}
			}
			if value > maximum {
				maximum = value
				m.LocalizationPixels = d
			}
		}
	}
	m.Recall = float64(m.TP) / float64(m.Positive)
	m.FalsePositiveRate = float64(m.FP) / float64(m.Negative)
	return m
}

func phase1548Hash(values []float32) string {
	h := sha256.New()
	var word [4]byte
	for _, v := range values {
		binary.LittleEndian.PutUint32(word[:], math.Float32bits(v))
		_, _ = h.Write(word[:])
	}
	return hex.EncodeToString(h.Sum(nil))
}
func phase1548Write(values []float32, name string) error {
	data := make([]byte, 4*len(values))
	for i, v := range values {
		binary.LittleEndian.PutUint32(data[4*i:], math.Float32bits(v))
	}
	return os.WriteFile(name, data, 0600)
}

func BenchmarkPhaseSymmetry2DResearch(b *testing.B) {
	src := NewVolume([3]int{96, 96, 1}, [3]float64{1, 1, 0})
	for i := range src.Data {
		src.Data[i] = float32(math.Sin(float64(i%96) * .1))
	}
	opts := PhaseSymmetry2DOptions{Wavelet: Wavelet2DOptions{ScalesMM: []float64{1, 2, 4}}, NoiseThreshold: .03, AmplitudeFloor: 1e-6, Polarity: Phase2DBright}
	b.ReportAllocs()
	b.SetBytes(int64(4 * len(src.Data)))
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		out, err := PhaseSymmetry2D(context.Background(), src, opts)
		if err != nil {
			b.Fatal(err)
		}
		b.ReportMetric(float64(out.Metadata.WorkingBytes), "logical-working-B")
	}
}
