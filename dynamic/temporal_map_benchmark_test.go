package dynamic

import (
	"context"
	"testing"
	"time"
)

// A procedural source keeps the benchmark free of an F*V input cache. The
// processing operation owns only one decoded frame and its output/accumulators.
type temporalBenchmarkSource struct {
	frames int
	grid   TemporalGrid
}

func (source temporalBenchmarkSource) FrameCount() int { return source.frames }

func (source temporalBenchmarkSource) FrameInfo(ctx context.Context, frame int) (TemporalFrameInfo, error) {
	if err := ctx.Err(); err != nil {
		return TemporalFrameInfo{}, err
	}
	return TemporalFrameInfo{Grid: source.grid, Units: "HU", SourceID: "procedural synthetic grid", Registration: "identity", Duration: time.Duration(frame%5+1) * time.Millisecond, HasDuration: true}, nil
}

func (source temporalBenchmarkSource) ReadFrame(ctx context.Context, frame int, buffer []float64) error {
	for voxel := range buffer {
		if voxel%4096 == 0 {
			if err := ctx.Err(); err != nil {
				return err
			}
		}
		buffer[voxel] = float64((voxel+frame*17)%1024) - 512
	}
	return nil
}

var temporalBenchmarkResult *TemporalMap

func BenchmarkTemporalMap(b *testing.B) {
	source := temporalBenchmarkSource{frames: 32, grid: temporalTestGrid(128 * 128)}
	source.grid.Size = [3]int{128, 128, 1}
	for _, profile := range []struct {
		name      string
		statistic TemporalStatistic
		weighting TemporalWeighting
		variance  TemporalVariance
	}{
		{"mean_sample", TemporalMean, SampleWeighting, PopulationVariance},
		{"stddev_duration_sample", TemporalStandardDeviation, DurationWeighting, SampleVariance},
	} {
		b.Run(profile.name, func(b *testing.B) {
			options := DefaultTemporalMapOptions()
			options.Statistic, options.Weighting, options.Variance = profile.statistic, profile.weighting, profile.variance
			b.ReportAllocs()
			b.SetBytes(int64(source.frames * 128 * 128 * 8))
			for iteration := 0; iteration < b.N; iteration++ {
				result, err := ComputeTemporalMap(context.Background(), source, options)
				if err != nil {
					b.Fatal(err)
				}
				temporalBenchmarkResult = result
			}
			b.ReportMetric(float64(temporalBenchmarkResult.PeakWorkingBytes), "working-B")
		})
	}
}

func BenchmarkTemporalMovingMean(b *testing.B) {
	source := temporalBenchmarkSource{frames: 32, grid: temporalTestGrid(128 * 128)}
	source.grid.Size = [3]int{128, 128, 1}
	options := DefaultMovingMeanOptions()
	options.Window, options.Alignment, options.Weighting = 5, CenteredWindow, DurationWeighting
	b.ReportAllocs()
	b.SetBytes(int64(options.Window * 128 * 128 * 8))
	for iteration := 0; iteration < b.N; iteration++ {
		result, err := MovingTemporalMean(context.Background(), source, 16, options)
		if err != nil {
			b.Fatal(err)
		}
		temporalBenchmarkResult = result
	}
	b.ReportMetric(float64(temporalBenchmarkResult.PeakWorkingBytes), "working-B")
}
