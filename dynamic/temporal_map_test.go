package dynamic

import (
	"context"
	"errors"
	"fmt"
	"math"
	"reflect"
	"strings"
	"testing"
	"time"
	"unsafe"
)

// This tolerance is fixed before comparing against the independently evaluated
// two-pass weighted oracle. It applies to the synthetic numerical domain below.
const temporalOracleTolerance = 1e-11

type temporalTestSource struct {
	info    []TemporalFrameInfo
	values  [][]float64
	reads   []int
	buffers [][]float64
	onRead  func(int, []float64) error
	onInfo  func(int) error
	count   int
}

func (source *temporalTestSource) FrameCount() int {
	if source.count != 0 {
		return source.count
	}
	return len(source.info)
}

func (source *temporalTestSource) FrameInfo(ctx context.Context, frame int) (TemporalFrameInfo, error) {
	if err := ctx.Err(); err != nil {
		return TemporalFrameInfo{}, err
	}
	if source.onInfo != nil {
		if err := source.onInfo(frame); err != nil {
			return TemporalFrameInfo{}, err
		}
	}
	return source.info[frame], nil
}

func (source *temporalTestSource) ReadFrame(ctx context.Context, frame int, buffer []float64) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	source.reads = append(source.reads, frame)
	source.buffers = append(source.buffers, buffer)
	copy(buffer, source.values[frame])
	if source.onRead != nil {
		return source.onRead(frame, buffer)
	}
	return nil
}

func temporalTestGrid(voxels int) TemporalGrid {
	return TemporalGrid{Size: [3]int{voxels, 1, 1}, Spacing: [3]float64{.7, 1.2, 2.5}, Origin: [3]float64{-11, 22, 3}, Direction: [9]float64{1, 0, 0, 0, 1, 0, 0, 0, 1}, FrameOfReferenceUID: "1.2.826.0.1.3680043.10.543.1545"}
}

func temporalNewTestSource(values [][]float64, durations []time.Duration) *temporalTestSource {
	source := &temporalTestSource{values: values, info: make([]TemporalFrameInfo, len(values))}
	var offset time.Duration
	for frame := range values {
		duration := time.Second
		if len(durations) != 0 {
			duration = durations[frame]
		}
		source.info[frame] = TemporalFrameInfo{Grid: temporalTestGrid(len(values[frame])), Units: "HU", SourceID: fmt.Sprintf("frame-%d", frame), Registration: "identity; verified common grid", Offset: offset, HasOffset: true, Duration: duration, HasDuration: true}
		offset += duration
	}
	return source
}

func temporalDirectOracle(values [][]float64, weights []float64, statistic TemporalStatistic, variance TemporalVariance) (float64, bool, uint32, float64) {
	var observations []float64
	var acceptedWeights []float64
	for frame, item := range values {
		if math.IsNaN(item[0]) || math.IsInf(item[0], 0) {
			continue
		}
		observations = append(observations, item[0])
		acceptedWeights = append(acceptedWeights, weights[frame])
	}
	if len(observations) == 0 {
		return math.NaN(), false, 0, 0
	}
	weightedSum, sumWeights, sumSquares := 0.0, 0.0, 0.0
	minimum, maximum := observations[0], observations[0]
	for index, value := range observations {
		weight := acceptedWeights[index]
		weightedSum += value * weight
		sumWeights += weight
		sumSquares += weight * weight
		minimum = math.Min(minimum, value)
		maximum = math.Max(maximum, value)
	}
	mean := weightedSum / sumWeights
	result := mean
	switch statistic {
	case TemporalMinimum:
		result = minimum
	case TemporalMaximum:
		result = maximum
	case TemporalStandardDeviation:
		denominator := sumWeights
		if variance == SampleVariance {
			if len(observations) < 2 {
				return math.NaN(), false, uint32(len(observations)), sumWeights
			}
			denominator -= sumSquares / sumWeights
		}
		central := 0.0
		for index, value := range observations {
			central += acceptedWeights[index] * math.Pow(value-mean, 2)
		}
		result = math.Sqrt(central / denominator)
	}
	return result, true, uint32(len(observations)), sumWeights
}

func temporalAssertOracle(t *testing.T, result *TemporalMap, values [][]float64, weights []float64, statistic TemporalStatistic, variance TemporalVariance) {
	t.Helper()
	maxError := 0.0
	for voxel := range result.Values {
		column := make([][]float64, len(values))
		for frame := range values {
			column[frame] = []float64{values[frame][voxel]}
		}
		want, valid, count, weightSum := temporalDirectOracle(column, weights, statistic, variance)
		if result.Valid[voxel] != valid || result.Counts[voxel] != count || result.WeightSums[voxel] != weightSum {
			t.Fatalf("voxel %d: valid/count/weights %v/%d/%g, want %v/%d/%g", voxel, result.Valid[voxel], result.Counts[voxel], result.WeightSums[voxel], valid, count, weightSum)
		}
		if !valid {
			if !math.IsNaN(result.Values[voxel]) {
				t.Fatalf("invalid voxel %d has value %g", voxel, result.Values[voxel])
			}
			continue
		}
		if (statistic == TemporalMinimum || statistic == TemporalMaximum) && result.Values[voxel] != want {
			t.Fatalf("voxel %d: exact extreme %g, want %g", voxel, result.Values[voxel], want)
		}
		error := math.Abs(result.Values[voxel] - want)
		maxError = math.Max(maxError, error)
		if error > temporalOracleTolerance*math.Max(1, math.Abs(want)) {
			t.Fatalf("voxel %d: %.17g, want %.17g (tolerance %g)", voxel, result.Values[voxel], want, temporalOracleTolerance)
		}
	}
	t.Logf("independent oracle maximum absolute error: %.17g", maxError)
}

func TestTemporalMapDirectOracle(t *testing.T) {
	values := [][]float64{{8, 0, 0, -3}, {8, 1, 0, -2}, {8, 2, 0, -1}, {8, 3, 12, 0}, {8, 4, 0, 1}, {8, 5, 0, 2}, {8, 6, 0, 3}}
	durations := []time.Duration{time.Second, 2 * time.Second, 4 * time.Second, 3 * time.Second, 8 * time.Second, time.Second, 5 * time.Second}
	for statistic := TemporalMean; statistic <= TemporalStandardDeviation; statistic++ {
		for weighting := SampleWeighting; weighting <= DurationWeighting; weighting++ {
			for variance := PopulationVariance; variance <= SampleVariance; variance++ {
				t.Run(fmt.Sprintf("%s/%s/%s", statistic, weighting, variance), func(t *testing.T) {
					source := temporalNewTestSource(values, durations)
					options := DefaultTemporalMapOptions()
					options.Statistic, options.Weighting, options.Variance = statistic, weighting, variance
					result, err := ComputeTemporalMap(context.Background(), source, options)
					if err != nil {
						t.Fatal(err)
					}
					weights := make([]float64, len(values))
					for frame := range values {
						weights[frame] = 1
						if weighting == DurationWeighting {
							weights[frame] = durations[frame].Seconds()
						}
					}
					temporalAssertOracle(t, result, values, weights, statistic, variance)
					if !reflect.DeepEqual(result.Provenance.Weights, weights) || result.Provenance.Options != options || result.Grid != source.info[0].Grid || result.Units != "HU" {
						t.Fatal("derived result lost effective weights/options/grid/units")
					}
				})
			}
		}
	}
}

func TestTemporalMovingMeanDirectOracle(t *testing.T) {
	values := [][]float64{{4, 0, 0}, {4, 1, 0}, {4, 2, 10}, {4, 3, 0}, {4, 4, 0}}
	durations := []time.Duration{time.Second, 4 * time.Second, 2 * time.Second, 8 * time.Second, 3 * time.Second}
	for alignment := CausalWindow; alignment <= CenteredWindow; alignment++ {
		for weighting := SampleWeighting; weighting <= DurationWeighting; weighting++ {
			for target := range values {
				t.Run(fmt.Sprintf("%s/%s/%d", alignment, weighting, target), func(t *testing.T) {
					source := temporalNewTestSource(values, durations)
					options := DefaultMovingMeanOptions()
					options.Alignment, options.Weighting = alignment, weighting
					result, err := MovingTemporalMean(context.Background(), source, target, options)
					if err != nil {
						t.Fatal(err)
					}
					start, end := target-2, target+1
					if alignment == CenteredWindow {
						start, end = target-1, target+2
					}
					start, end = max(0, start), min(len(values), end)
					weights := make([]float64, end-start)
					indices := make([]int, end-start)
					for index := range weights {
						weights[index], indices[index] = 1, start+index
						if weighting == DurationWeighting {
							weights[index] = durations[start+index].Seconds()
						}
					}
					temporalAssertOracle(t, result, values[start:end], weights, TemporalMean, PopulationVariance)
					if !reflect.DeepEqual(source.reads, indices) || !reflect.DeepEqual(result.Provenance.FrameIndices, indices) || result.Provenance.Start != start || result.Provenance.End != end || result.Provenance.Target != target || !result.Provenance.Moving {
						t.Fatal("moving window reads or provenance diverge")
					}
					options.Border = RequireFullWindow
					_, fullErr := MovingTemporalMean(context.Background(), source, target, options)
					full := (alignment == CausalWindow && target >= 2) || (alignment == CenteredWindow && target >= 1 && target < len(values)-1)
					if full && fullErr != nil || !full && !errors.Is(fullErr, ErrTemporalWindow) {
						t.Fatalf("full border result %v, interior=%v", fullErr, full)
					}
				})
			}
		}
	}
}

func TestTemporalMapMissingAndNonFinitePolicies(t *testing.T) {
	values := [][]float64{{1, math.NaN(), math.Inf(1), 5}, {999, 999, 999, 999}, {3, 7, math.Inf(-1), math.NaN()}}
	source := temporalNewTestSource(values, nil)
	source.info[1] = TemporalFrameInfo{Missing: true}
	options := DefaultTemporalMapOptions()
	if result, err := ComputeTemporalMap(context.Background(), source, options); !errors.Is(err, ErrTemporalData) || result != nil || len(source.reads) != 0 {
		t.Fatalf("missing reject: result=%v err=%v reads=%v", result, err, source.reads)
	}
	options.Missing = SkipMissing
	if result, err := ComputeTemporalMap(context.Background(), source, options); !errors.Is(err, ErrTemporalData) || result != nil {
		t.Fatalf("nonfinite reject: result=%v err=%v", result, err)
	}
	options.NonFinite = SkipNonFinite
	options.Statistic = TemporalStandardDeviation
	for variance := PopulationVariance; variance <= SampleVariance; variance++ {
		options.Variance = variance
		source.reads = nil
		result, err := ComputeTemporalMap(context.Background(), source, options)
		if err != nil {
			t.Fatal(err)
		}
		temporalAssertOracle(t, result, [][]float64{values[0], values[2]}, []float64{1, 1}, options.Statistic, variance)
		if !reflect.DeepEqual(source.reads, []int{0, 2}) || !reflect.DeepEqual(result.Provenance.MissingIndices, []int{1}) {
			t.Fatal("missing position was read or lost")
		}
	}
	source.info[0].Missing, source.info[2].Missing = true, true
	if result, err := ComputeTemporalMap(context.Background(), source, options); !errors.Is(err, ErrTemporalNoSamples) || result != nil {
		t.Fatalf("all missing: result=%v err=%v", result, err)
	}
}

func TestTemporalMapAdmissionAndSpatialIdentity(t *testing.T) {
	tests := []struct {
		name   string
		change func(*temporalTestSource, *TemporalMapOptions)
		want   error
	}{
		{"no-frames", func(s *temporalTestSource, _ *TemporalMapOptions) { s.info = nil }, ErrTemporalAdmission},
		{"frame-budget", func(_ *temporalTestSource, o *TemporalMapOptions) { o.Limits.MaxFrames = 1 }, ErrTemporalAdmission},
		{"voxel-budget", func(_ *temporalTestSource, o *TemporalMapOptions) { o.Limits.MaxVoxels = 1 }, ErrTemporalAdmission},
		{"working-budget", func(_ *temporalTestSource, o *TemporalMapOptions) { o.Limits.MaxWorkingBytes = 150 }, ErrTemporalAdmission},
		{"metadata-budget", func(_ *temporalTestSource, o *TemporalMapOptions) { o.Limits.MaxWorkingBytes = 1 }, ErrTemporalAdmission},
		{"zero-budget", func(_ *temporalTestSource, o *TemporalMapOptions) { o.Limits = TemporalLimits{} }, ErrTemporalAdmission},
		{"invalid-policy", func(_ *temporalTestSource, o *TemporalMapOptions) { o.NonFinite = 2 }, ErrTemporalAdmission},
		{"incompatible-units", func(s *temporalTestSource, _ *TemporalMapOptions) { s.info[1].Units = "SUVbw" }, ErrTemporalData},
		{"missing-units", func(s *temporalTestSource, _ *TemporalMapOptions) { s.info[0].Units = "" }, ErrTemporalData},
		{"different-origin", func(s *temporalTestSource, _ *TemporalMapOptions) { s.info[1].Grid.Origin[0] += .01 }, ErrTemporalData},
		{"different-spacing", func(s *temporalTestSource, _ *TemporalMapOptions) { s.info[1].Grid.Spacing[1] += .01 }, ErrTemporalData},
		{"different-direction", func(s *temporalTestSource, _ *TemporalMapOptions) {
			s.info[1].Grid.Direction = [9]float64{-1, 0, 0, 0, -1, 0, 0, 0, 1}
		}, ErrTemporalData},
		{"different-reference", func(s *temporalTestSource, _ *TemporalMapOptions) { s.info[1].Grid.FrameOfReferenceUID = "other" }, ErrTemporalData},
		{"missing-reference", func(s *temporalTestSource, _ *TemporalMapOptions) { s.info[0].Grid.FrameOfReferenceUID = "" }, ErrTemporalData},
		{"invalid-direction", func(s *temporalTestSource, _ *TemporalMapOptions) { s.info[0].Grid.Direction[0] = 2 }, ErrTemporalData},
		{"nonfinite-origin", func(s *temporalTestSource, _ *TemporalMapOptions) { s.info[0].Grid.Origin[0] = math.NaN() }, ErrTemporalData},
		{"zero-spacing", func(s *temporalTestSource, _ *TemporalMapOptions) { s.info[0].Grid.Spacing[0] = 0 }, ErrTemporalData},
		{"negative-offset", func(s *temporalTestSource, _ *TemporalMapOptions) { s.info[0].Offset = -1 }, ErrTemporalData},
		{"negative-duration", func(s *temporalTestSource, _ *TemporalMapOptions) { s.info[0].Duration = -1 }, ErrTemporalData},
		{"unknown-duration", func(s *temporalTestSource, o *TemporalMapOptions) {
			o.Weighting = DurationWeighting
			s.info[1].HasDuration = false
		}, ErrTemporalData},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			source := temporalNewTestSource([][]float64{{1, 2}, {3, 4}}, nil)
			options := DefaultTemporalMapOptions()
			test.change(source, &options)
			result, err := ComputeTemporalMap(context.Background(), source, options)
			if !errors.Is(err, test.want) || result != nil || len(source.reads) != 0 {
				t.Fatalf("result=%v err=%v want=%v reads=%v", result, err, test.want, source.reads)
			}
		})
	}
	// Offsets alone, even regular and complete, must never create durations.
	source := temporalNewTestSource([][]float64{{1}, {3}}, nil)
	for index := range source.info {
		source.info[index].HasDuration = false
	}
	options := DefaultTemporalMapOptions()
	if _, err := ComputeTemporalMap(context.Background(), source, options); err != nil {
		t.Fatalf("sample weighting unexpectedly needs durations: %v", err)
	}
	options.Weighting = DurationWeighting
	if result, err := ComputeTemporalMap(context.Background(), source, options); !errors.Is(err, ErrTemporalData) || result != nil {
		t.Fatal("offset-only source invented duration weights")
	}
}

func TestTemporalMapBoundedBuffersAndCancellation(t *testing.T) {
	values := [][]float64{{1, 2, 3}, {2, 3, 4}, {3, 4, 5}, {4, 5, 6}}
	before := make([][]float64, len(values))
	for index := range values {
		before[index] = append([]float64(nil), values[index]...)
	}
	source := temporalNewTestSource(values, nil)
	options := DefaultTemporalMapOptions()
	options.Statistic, options.Variance = TemporalStandardDeviation, SampleVariance
	result, err := ComputeTemporalMap(context.Background(), source, options)
	if err != nil {
		t.Fatal(err)
	}
	textBytes := len(source.info[0].Units) + len(source.info[0].Grid.FrameOfReferenceUID)
	for _, info := range source.info {
		textBytes += len(info.SourceID) + len(info.Registration)
	}
	if result.PeakWorkingBytes != 4*74+3*45+uint64(textBytes) || !reflect.DeepEqual(values, before) {
		t.Fatalf("wrong resident-array budget %d or mutated source", result.PeakWorkingBytes)
	}
	for _, buffer := range source.buffers {
		if len(buffer) != 3 || &buffer[0] != &source.buffers[0][0] || &buffer[0] == &result.Values[0] || &buffer[0] == &values[0][0] {
			t.Fatal("source read buffer not reused or aliased source/output")
		}
	}
	options.Limits.MaxWorkingBytes = result.PeakWorkingBytes
	if _, err := ComputeTemporalMap(context.Background(), temporalNewTestSource(values, nil), options); err != nil {
		t.Fatalf("exact memory budget refused: %v", err)
	}
	options.Limits.MaxWorkingBytes--
	if mapResult, err := ComputeTemporalMap(context.Background(), temporalNewTestSource(values, nil), options); !errors.Is(err, ErrTemporalAdmission) || mapResult != nil {
		t.Fatal("below-budget operation was admitted")
	}
	for _, at := range []int{-1, 0, 2} {
		t.Run(fmt.Sprintf("cancel-%d", at), func(t *testing.T) {
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			source := temporalNewTestSource(values, nil)
			source.onRead = func(frame int, _ []float64) error {
				if frame == at {
					cancel()
				}
				return nil
			}
			if at < 0 {
				cancel()
			}
			mapResult, err := ComputeTemporalMap(ctx, source, DefaultTemporalMapOptions())
			if mapResult != nil || !errors.Is(err, context.Canceled) || !reflect.DeepEqual(values, before) {
				t.Fatal("cancelled operation returned partial map or mutated source")
			}
			if at < 0 && len(source.reads) != 0 || at >= 0 && len(source.reads) != at+1 {
				t.Fatalf("unbounded read after cancellation: %v", source.reads)
			}
		})
	}
}

func TestTemporalMovingWindowPolicies(t *testing.T) {
	source := temporalNewTestSource([][]float64{{1}, {2}, {3}, {4}, {5}}, nil)
	options := DefaultMovingMeanOptions()
	options.Alignment = CenteredWindow
	options.Window = 2
	if result, err := MovingTemporalMean(context.Background(), source, 2, options); !errors.Is(err, ErrTemporalAdmission) || result != nil {
		t.Fatal("even centered window admitted")
	}
	options.Window = 3
	source.info[1].Missing = true
	options.Missing = SkipMissing
	result, err := MovingTemporalMean(context.Background(), source, 2, options)
	if err != nil || !reflect.DeepEqual(source.reads, []int{2, 3}) || result.Values[0] != 3.5 || !reflect.DeepEqual(result.Provenance.MissingIndices, []int{1}) {
		t.Fatalf("missing window position replacement: %v, %v", result, err)
	}
	options.Alignment, options.Border = CausalWindow, RequireFullWindow
	if mapResult, err := MovingTemporalMean(context.Background(), source, 0, options); !errors.Is(err, ErrTemporalWindow) || mapResult != nil {
		t.Fatal("causal leading border invented past frames")
	}
}

func TestTemporalMapExtremeWeightsAndOverflow(t *testing.T) {
	source := temporalNewTestSource([][]float64{{1}, {3}}, []time.Duration{time.Nanosecond, time.Duration(math.MaxInt64)})
	options := DefaultTemporalMapOptions()
	options.Statistic, options.Weighting, options.Variance = TemporalStandardDeviation, DurationWeighting, SampleVariance
	result, err := ComputeTemporalMap(context.Background(), source, options)
	if err != nil || !result.Valid[0] || math.Abs(result.Values[0]-math.Sqrt(2)) > temporalOracleTolerance {
		t.Fatalf("weighted sample denominator lost precision: result=%v err=%v", result, err)
	}
	source = temporalNewTestSource([][]float64{{-math.MaxFloat64}, {math.MaxFloat64}}, nil)
	if mapResult, err := ComputeTemporalMap(context.Background(), source, DefaultTemporalMapOptions()); !errors.Is(err, ErrTemporalData) || mapResult != nil {
		t.Fatal("recurrence overflow published a map")
	}
}

func TestTemporalMapSourceErrorsAndMetadataCancellation(t *testing.T) {
	sentinel := errors.New("source unavailable")
	for _, operation := range []struct {
		name string
		run  func(context.Context, TemporalSource) (*TemporalMap, error)
	}{
		{"statistics", func(ctx context.Context, source TemporalSource) (*TemporalMap, error) {
			return ComputeTemporalMap(ctx, source, DefaultTemporalMapOptions())
		}},
		{"moving", func(ctx context.Context, source TemporalSource) (*TemporalMap, error) {
			options := DefaultMovingMeanOptions()
			options.Alignment = CenteredWindow
			return MovingTemporalMean(ctx, source, 1, options)
		}},
	} {
		for _, failure := range []string{"metadata", "pixels", "metadata-cancel", "pixels-cancel"} {
			t.Run(operation.name+"/"+failure, func(t *testing.T) {
				ctx, cancel := context.WithCancel(context.Background())
				defer cancel()
				source := temporalNewTestSource([][]float64{{1, 2}, {2, 3}, {3, 4}}, nil)
				want := sentinel
				switch failure {
				case "metadata":
					source.onInfo = func(frame int) error {
						if frame == 1 {
							return sentinel
						}
						return nil
					}
				case "pixels":
					source.onRead = func(frame int, _ []float64) error {
						if frame == 1 {
							return sentinel
						}
						return nil
					}
				case "metadata-cancel":
					want = context.Canceled
					source.onInfo = func(frame int) error {
						if frame == 2 {
							cancel()
						}
						return nil
					}
				case "pixels-cancel":
					want = context.Canceled
					source.onRead = func(frame int, _ []float64) error {
						if frame == 1 {
							cancel()
						}
						return nil
					}
				}
				result, err := operation.run(ctx, source)
				if result != nil || !errors.Is(err, want) {
					t.Fatalf("partial map=%v error=%v want=%v", result, err, want)
				}
				if (failure == "metadata" || failure == "metadata-cancel") && len(source.reads) != 0 {
					t.Fatalf("pixels read before metadata admission finished: %v", source.reads)
				}
				if (failure == "pixels" || failure == "pixels-cancel") && !reflect.DeepEqual(source.reads, []int{0, 1}) {
					t.Fatalf("read continued after source failure: %v", source.reads)
				}
			})
		}
	}
}

func TestTemporalMovingMeanAnalyticDistortion(t *testing.T) {
	// Predeclared analytic acceptance, separate from the direct arithmetic oracle:
	// unit ramp: causal W3 offset -1, centered W3 interior offset 0;
	// unit impulse at t=3: causal support [3,5], centered support [2,4],
	// amplitude 1/3, respective centroid delays +1 and 0. Numerical deviation
	// from every analytic value and metric must be <=1e-11. These quantify
	// deliberate smoothing distortion, not a clinical acceptance threshold.
	ramp, impulse := make([][]float64, 9), make([][]float64, 9)
	for frame := range ramp {
		ramp[frame] = []float64{float64(frame)}
		impulse[frame] = []float64{0}
	}
	impulse[3][0] = 1
	for alignment := CausalWindow; alignment <= CenteredWindow; alignment++ {
		t.Run(alignment.String(), func(t *testing.T) {
			options := DefaultMovingMeanOptions()
			options.Alignment = alignment
			options.Border = RequireFullWindow
			start, end, wantOffset := 2, len(ramp), -1.0
			wantSupport, wantDelay := []int{3, 4, 5}, 1.0
			if alignment == CenteredWindow {
				start, end, wantOffset = 1, len(ramp)-1, 0
				wantSupport, wantDelay = []int{2, 3, 4}, 0
			}
			rampSource := temporalNewTestSource(ramp, nil)
			maxRampError, maxRampDistortion := 0.0, 0.0
			for target := start; target < end; target++ {
				result, err := MovingTemporalMean(context.Background(), rampSource, target, options)
				if err != nil {
					t.Fatal(err)
				}
				maxRampError = math.Max(maxRampError, math.Abs(result.Values[0]-(float64(target)+wantOffset)))
				maxRampDistortion = math.Max(maxRampDistortion, math.Abs(result.Values[0]-ramp[target][0]))
			}
			if maxRampError > temporalOracleTolerance {
				t.Fatalf("ramp analytic error %g", maxRampError)
			}
			options.Border = ClipWindow
			impulseSource := temporalNewTestSource(impulse, nil)
			var support []int
			mass, firstMoment, peak, maxImpulseError, maxImpulseDistortion := 0.0, 0.0, 0.0, 0.0, 0.0
			for target := range impulse {
				result, err := MovingTemporalMean(context.Background(), impulseSource, target, options)
				if err != nil {
					t.Fatal(err)
				}
				value := result.Values[0]
				want := 0.0
				if target >= wantSupport[0] && target <= wantSupport[2] {
					want = 1.0 / 3
				}
				if value != 0 {
					support = append(support, target)
				}
				mass += value
				firstMoment += float64(target) * value
				peak = math.Max(peak, value)
				maxImpulseError = math.Max(maxImpulseError, math.Abs(value-want))
				maxImpulseDistortion = math.Max(maxImpulseDistortion, math.Abs(value-impulse[target][0]))
			}
			centroidDelay := firstMoment/mass - 3
			if !reflect.DeepEqual(support, wantSupport) || math.Abs(peak-1.0/3) > temporalOracleTolerance || math.Abs(mass-1) > temporalOracleTolerance || math.Abs(centroidDelay-wantDelay) > temporalOracleTolerance || maxImpulseError > temporalOracleTolerance {
				t.Fatalf("impulse support=%v amplitude=%.17g mass=%.17g centroid-delay=%.17g analytic-error=%g", support, peak, mass, centroidDelay, maxImpulseError)
			}
			t.Logf("distortion ramp-offset=%g ramp-max-deviation=%g ramp-analytic-error=%g; impulse-support=%v impulse-amplitude=%.17g impulse-centroid-delay=%g impulse-max-deviation=%.17g impulse-analytic-error=%.17g", wantOffset, maxRampDistortion, maxRampError, support, peak, centroidDelay, maxImpulseDistortion, maxImpulseError)
		})
	}
}

func TestTemporalMapRetainedMetadataAdmission(t *testing.T) {
	for _, field := range []struct {
		name string
		set  func(*TemporalFrameInfo)
	}{
		{"units", func(info *TemporalFrameInfo) { info.Units = strings.Repeat("U", MaxTemporalUnitBytes+1) }},
		{"frame-of-reference", func(info *TemporalFrameInfo) {
			info.Grid.FrameOfReferenceUID = strings.Repeat("1", MaxTemporalFrameOfReferenceUIDBytes+1)
		}},
		{"source", func(info *TemporalFrameInfo) { info.SourceID = strings.Repeat("S", MaxTemporalSourceIDBytes+1) }},
		{"registration", func(info *TemporalFrameInfo) { info.Registration = strings.Repeat("R", MaxTemporalRegistrationBytes+1) }},
	} {
		t.Run(field.name, func(t *testing.T) {
			source := temporalNewTestSource([][]float64{{1}}, nil)
			field.set(&source.info[0])
			result, err := ComputeTemporalMap(context.Background(), source, DefaultTemporalMapOptions())
			if result != nil || !errors.Is(err, ErrTemporalAdmission) || len(source.reads) != 0 {
				t.Fatalf("unbounded metadata admitted: result=%v err=%v reads=%v", result, err, source.reads)
			}
		})
	}
	// Copies must detach even a tiny substring from a much larger source-owned
	// string. Comparing StringData here observes aliasing without mutating strings.
	backing := strings.Repeat("x", 8<<20)
	source := temporalNewTestSource([][]float64{{1}, {2}}, nil)
	for frame := range source.info {
		source.info[frame].Units = backing[0:2]
		source.info[frame].Grid.FrameOfReferenceUID = backing[100:110]
		source.info[frame].SourceID = backing[200:212]
		source.info[frame].Registration = backing[300:304]
	}
	options := DefaultTemporalMapOptions()
	// Two provenance capacities, one mean voxel, common text once, and per-frame
	// text twice. A budget one byte below this must fail before reading pixels.
	wantBytes := uint64(2*74 + 29 + 2 + 10 + 2*(12+4))
	options.Limits.MaxWorkingBytes = wantBytes - 1
	if result, err := ComputeTemporalMap(context.Background(), source, options); result != nil || !errors.Is(err, ErrTemporalAdmission) || len(source.reads) != 0 {
		t.Fatalf("retained text bypassed budget: result=%v err=%v reads=%v", result, err, source.reads)
	}
	options.Limits.MaxWorkingBytes = wantBytes
	result, err := ComputeTemporalMap(context.Background(), source, options)
	if err != nil || result.PeakWorkingBytes != wantBytes {
		t.Fatalf("exact retained-text budget: result=%v err=%v", result, err)
	}
	for _, pair := range [][2]string{{result.Units, source.info[0].Units}, {result.Grid.FrameOfReferenceUID, source.info[0].Grid.FrameOfReferenceUID}, {result.Provenance.Sources[0], source.info[0].SourceID}, {result.Provenance.Registrations[0], source.info[0].Registration}} {
		if pair[0] != pair[1] || unsafe.StringData(pair[0]) == unsafe.StringData(pair[1]) {
			t.Fatal("retained metadata was changed or still aliases source backing")
		}
	}
}
