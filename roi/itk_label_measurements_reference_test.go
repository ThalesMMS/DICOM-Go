package roi_test

import (
	"context"
	"math"
	"slices"
	"testing"

	"github.com/ThalesMMS/dicom-go/render"
	"github.com/ThalesMMS/dicom-go/roi"
)

// Like ITK's label-statistics cases with signed features and outliers,
// this computes each label independently. Expected order statistics use
// exact finite samples and linear percentiles.
func TestITKReferenceLabelStatisticsUseOwnedSamples(t *testing.T) {
	grid := roi.Grid{
		Columns: 4, Rows: 2, Slices: 2,
		RowDir: render.Vec3{X: 1}, ColDir: render.Vec3{Y: 1}, SliceDir: render.Vec3{Z: 1},
		ColSpacing: 1, RowSpacing: 1, SliceSpacing: 2,
	}
	labels := &roi.LabelVolume{Grid: grid, Labels: []uint16{
		900, 7, 0, 65535, 7, 900, 0, 900,
		900, 7, 0, 900, 7, 900, 0, 0,
	}}
	values := []float64{
		-2, 1, -5000, -6, -7, math.NaN(), math.Inf(1), 0,
		math.Inf(1), 5, 9999, math.Inf(-1), -3, 2, 1, 1,
	}
	got, err := roi.LabelIntensityStatistics(context.Background(), labels, values)
	if err != nil {
		t.Fatal(err)
	}
	want := []struct {
		name        string
		moments     roi.IntensityMoments
		percentiles []float64
	}{
		{
			name: "signed samples in two slices",
			moments: roi.IntensityMoments{
				Label: 7, Count: 4, Min: -7, Max: 5, Sum: -4, Mean: -1,
				Variance: 80.0 / 3, StandardDeviation: math.Sqrt(80.0 / 3),
			},
			percentiles: []float64{-7, -4, -1, 2, 5},
		},
		{
			name: "finite subset of a disjoint label",
			moments: roi.IntensityMoments{
				Label: 900, Count: 3, NonFinite: 3, Min: -2, Max: 2,
				Variance: 4, StandardDeviation: 2,
			},
			percentiles: []float64{-2, -1, 0, 1, 2},
		},
		{
			name:        "singleton with maximum label id",
			moments:     roi.IntensityMoments{Label: 65535, Count: 1, Min: -6, Max: -6, Sum: -6, Mean: -6},
			percentiles: []float64{-6, -6, -6, -6, -6},
		},
	}
	if len(got) != len(want) {
		t.Fatalf("label statistics = %v, want exactly three foreground labels", got)
	}
	clear(values)
	clear(labels.Labels)
	for i, tc := range want {
		t.Run(tc.name, func(t *testing.T) {
			stats := got[i]
			moments := stats.Moments()
			if moments.Label != tc.moments.Label || moments.Count != tc.moments.Count || moments.NonFinite != tc.moments.NonFinite ||
				moments.Min != tc.moments.Min || moments.Max != tc.moments.Max || moments.Sum != tc.moments.Sum || moments.Mean != tc.moments.Mean {
				t.Errorf("moments = %+v, want %+v", moments, tc.moments)
			}
			if !(math.Abs(moments.Variance-tc.moments.Variance) < 1e-12 &&
				math.Abs(moments.StandardDeviation-tc.moments.StandardDeviation) < 1e-12) {
				t.Errorf("variance and SD = %g, %g, want %g, %g", moments.Variance, moments.StandardDeviation, tc.moments.Variance, tc.moments.StandardDeviation)
			}
			if stats.Median != tc.percentiles[2] {
				t.Errorf("median = %g, want %g", stats.Median, tc.percentiles[2])
			}
			if p := stats.Percentiles(0, 25, 50, 75, 100); !slices.Equal(p, tc.percentiles) {
				t.Errorf("percentiles = %v, want %v after input changed", p, tc.percentiles)
			}
		})
	}
}

// Direction and flip cases from ITK's ShapeLabelMap fixtures motivate this
// independent planar example with two sparse label IDs on a shared grid.
func TestITKReferencePlanarLabelMeasurementDirections(t *testing.T) {
	for _, tc := range []struct {
		name                          string
		row, column, slice            render.Vec3
		lineCentroid, islandsCentroid render.Vec3
	}{
		{
			name: "axial",
			row:  render.Vec3{X: 1}, column: render.Vec3{Y: 1}, slice: render.Vec3{Z: 1},
			lineCentroid: render.Vec3{X: 83, Y: -46, Z: 13}, islandsCentroid: render.Vec3{X: 81.5, Y: -46, Z: 13},
		},
		{
			name: "mirrored columns",
			row:  render.Vec3{X: -1}, column: render.Vec3{Y: 1}, slice: render.Vec3{Z: -1},
			lineCentroid: render.Vec3{X: 77, Y: -46, Z: 13}, islandsCentroid: render.Vec3{X: 78.5, Y: -46, Z: 13},
		},
		{
			name: "permuted axes",
			row:  render.Vec3{Z: 1}, column: render.Vec3{X: 1}, slice: render.Vec3{Y: 1},
			lineCentroid: render.Vec3{X: 84, Y: -50, Z: 16}, islandsCentroid: render.Vec3{X: 84, Y: -50, Z: 14.5},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			grid := roi.Grid{
				Columns: 4, Rows: 3, Slices: 1, Origin: render.Vec3{X: 80, Y: -50, Z: 13},
				RowDir: tc.row, ColDir: tc.column, SliceDir: tc.slice,
				ColSpacing: 1.5, RowSpacing: 4, SliceSpacing: 7,
			}
			vol := &roi.LabelVolume{Grid: grid, Labels: []uint16{
				5032, 0, 0, 0,
				0, 7, 7, 7,
				0, 0, 5032, 0,
			}}
			got, err := roi.LabelShapeStatistics(context.Background(), vol, roi.ShapeOptions{Planar: true})
			if err != nil || len(got) != 2 {
				t.Fatalf("LabelShapeStatistics = %v, %v, want two foreground labels", got, err)
			}
			line, islands := got[0], got[1]
			if line.Label != 7 || line.Dimension != 2 || line.VoxelCount != 3 || line.PhysicalSize != 18 ||
				line.BoundingBoxMin != [3]int{1, 1, 0} || line.BoundingBoxMax != [3]int{3, 1, 0} ||
				line.Centroid != tc.lineCentroid || line.FeretDiameter != 3 || line.VoxelsOnBorder != 1 {
				t.Errorf("line statistics = %+v; want three contiguous 6 mm² cells at %v", line, tc.lineCentroid)
			}
			if islands.Label != 5032 || islands.Dimension != 2 || islands.VoxelCount != 2 || islands.PhysicalSize != 12 ||
				islands.BoundingBoxMin != [3]int{0, 0, 0} || islands.BoundingBoxMax != [3]int{2, 2, 0} ||
				islands.Centroid != tc.islandsCentroid || !(math.Abs(islands.FeretDiameter-math.Sqrt(73)) < 1e-12) ||
				islands.VoxelsOnBorder != 2 {
				t.Errorf("islands statistics = %+v; want two disjoint 6 mm² cells at %v", islands, tc.islandsCentroid)
			}
		})
	}
}
