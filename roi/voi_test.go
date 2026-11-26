package roi

import (
	"context"
	"encoding/binary"
	"errors"
	"math"
	"testing"

	"github.com/ThalesMMS/dicom-go/pixeldata"
	"github.com/ThalesMMS/dicom-go/render"
)

func isoGrid(n int, spacing float64) VoxelGrid {
	return VoxelGrid{
		Columns: n, Rows: n, Slices: n,
		ColumnStep: render.Vec3{X: spacing}, RowStep: render.Vec3{Y: spacing}, SliceStep: render.Vec3{Z: spacing},
	}
}

// obliqueGrid is anisotropic (0.8, 0.5, 2.5 mm) and rotated about two axes.
func obliqueGrid(cols, rows, slices int) VoxelGrid {
	x := render.Vec3{X: 1}.Rotate(render.Vec3{Z: 1}, 0.4).Rotate(render.Vec3{X: 1}, 0.3)
	y := render.Vec3{Y: 1}.Rotate(render.Vec3{Z: 1}, 0.4).Rotate(render.Vec3{X: 1}, 0.3)
	z := x.Cross(y)
	return VoxelGrid{
		Columns: cols, Rows: rows, Slices: slices,
		Origin:     render.Vec3{X: -12, Y: 7, Z: 30},
		ColumnStep: x.Scale(0.8), RowStep: y.Scale(0.5), SliceStep: z.Scale(2.5),
	}
}

func rotatedAxes(angle float64) [3]render.Vec3 {
	axis := render.Vec3{X: 1, Y: 2, Z: 3}
	return [3]render.Vec3{
		render.Vec3{X: 1}.Rotate(axis, angle),
		render.Vec3{Y: 1}.Rotate(axis, angle),
		render.Vec3{Z: 1}.Rotate(axis, angle),
	}
}

func closeTo(a, b, tol float64) bool { return math.Abs(a-b) <= tol }

func TestVoxelGridIndexRoundTrip(t *testing.T) {
	g := obliqueGrid(10, 12, 7)
	if err := g.Validate(); err != nil {
		t.Fatal(err)
	}
	if v := g.VoxelVolumeMM3(); !closeTo(v, 0.8*0.5*2.5, 1e-12) {
		t.Fatalf("voxel volume = %v, want 1", v)
	}
	for _, idx := range []render.Vec3{{}, {X: 3, Y: 4, Z: 5}, {X: -0.5, Y: 11.5, Z: 6.25}} {
		back, ok := g.PointToIndex(g.IndexToPoint(idx.X, idx.Y, idx.Z))
		if !ok || !closeTo(back.X, idx.X, 1e-9) || !closeTo(back.Y, idx.Y, 1e-9) || !closeTo(back.Z, idx.Z, 1e-9) {
			t.Fatalf("round trip of %v = %v", idx, back)
		}
	}
	for name, bad := range map[string]VoxelGrid{
		"zero dimension": {Columns: 0, Rows: 1, Slices: 1, ColumnStep: render.Vec3{X: 1}, RowStep: render.Vec3{Y: 1}, SliceStep: render.Vec3{Z: 1}},
		"coplanar steps": {Columns: 1, Rows: 1, Slices: 1, ColumnStep: render.Vec3{X: 1}, RowStep: render.Vec3{Y: 1}, SliceStep: render.Vec3{X: 1, Y: 1}},
		"zero step":      {Columns: 1, Rows: 1, Slices: 1, ColumnStep: render.Vec3{X: 1}, RowStep: render.Vec3{Y: 1}},
		"non-finite":     {Columns: 1, Rows: 1, Slices: 1, Origin: render.Vec3{X: math.NaN()}, ColumnStep: render.Vec3{X: 1}, RowStep: render.Vec3{Y: 1}, SliceStep: render.Vec3{Z: 1}},
	} {
		if err := bad.Validate(); !errors.Is(err, ErrInvalidVOI) {
			t.Errorf("%s: Validate = %v, want ErrInvalidVOI", name, err)
		}
		if _, ok := bad.PointToIndex(render.Vec3{}); ok {
			t.Errorf("%s: PointToIndex succeeded", name)
		}
	}
}

func TestVOIPrimitiveValidation(t *testing.T) {
	skew := [3]render.Vec3{{X: 1}, {X: 1}, {Z: 1}}
	short := [3]render.Vec3{{X: 0.5}, {Y: 1}, {Z: 1}}
	for name, tc := range map[string]struct {
		primitive VOIPrimitive
		valid     bool
	}{
		"sphere":                    {Sphere{RadiusMM: 1}, true},
		"sphere zero radius":        {Sphere{}, false},
		"sphere infinite radius":    {Sphere{RadiusMM: math.Inf(1)}, false},
		"sphere NaN centre":         {Sphere{Center: render.Vec3{Z: math.NaN()}, RadiusMM: 1}, false},
		"ellipsoid":                 {Ellipsoid{RadiiMM: [3]float64{1, 2, 3}, Axes: rotatedAxes(0.7)}, true},
		"ellipsoid negative radius": {Ellipsoid{RadiiMM: [3]float64{1, -2, 3}}, false},
		"ellipsoid skew axes":       {Ellipsoid{RadiiMM: [3]float64{1, 2, 3}, Axes: skew}, false},
		"ellipsoid short axis":      {Ellipsoid{RadiiMM: [3]float64{1, 2, 3}, Axes: short}, false},
		"box":                       {Box{SizeMM: [3]float64{1, 2, 3}, Axes: rotatedAxes(-0.2)}, true},
		"box zero edge":             {Box{SizeMM: [3]float64{1, 0, 3}}, false},
		"box skew axes":             {Box{SizeMM: [3]float64{1, 2, 3}, Axes: skew}, false},
	} {
		err := tc.primitive.Validate()
		if tc.valid && err != nil {
			t.Errorf("%s: %v", name, err)
		}
		if !tc.valid && !errors.Is(err, ErrInvalidVOI) {
			t.Errorf("%s: Validate = %v, want ErrInvalidVOI", name, err)
		}
		if !tc.valid {
			if _, err := RasterizeVOI(context.Background(), isoGrid(4, 1), tc.primitive, VOIRasterOptions{}); !errors.Is(err, ErrInvalidVOI) {
				t.Errorf("%s: RasterizeVOI = %v, want ErrInvalidVOI", name, err)
			}
		}
	}
	if _, err := RasterizeVOI(context.Background(), isoGrid(4, 1), nil, VOIRasterOptions{}); !errors.Is(err, ErrInvalidVOI) {
		t.Errorf("nil primitive: %v", err)
	}
	for _, opts := range []VOIRasterOptions{{Weighting: 7}, {Weighting: VOIPartialVolume, Supersampling: -1}, {Weighting: VOIPartialVolume, Supersampling: MaxVOISupersampling + 1}} {
		if _, err := RasterizeVOI(context.Background(), isoGrid(4, 1), Sphere{RadiusMM: 1}, opts); !errors.Is(err, ErrInvalidVOI) {
			t.Errorf("options %+v: %v", opts, err)
		}
	}
}

// ITK semantics: the ellipse surface is outside (r < 1), the box faces are
// inside (bounding-box IsInside).
func TestVOIPrimitiveBoundarySemantics(t *testing.T) {
	sphere := Sphere{Center: render.Vec3{X: 1, Y: 2, Z: 3}, RadiusMM: 2}
	if sphere.Contains(render.Vec3{X: 3, Y: 2, Z: 3}) || !sphere.Contains(render.Vec3{X: 2.999, Y: 2, Z: 3}) {
		t.Fatal("sphere surface must be outside and interior points inside")
	}
	ellipsoid := Ellipsoid{RadiiMM: [3]float64{4, 2, 1}}
	if ellipsoid.Contains(render.Vec3{X: 4}) || ellipsoid.Contains(render.Vec3{Z: -1}) || !ellipsoid.Contains(render.Vec3{Y: 1.999}) {
		t.Fatal("ellipsoid surface must be outside")
	}
	box := Box{SizeMM: [3]float64{4, 2, 6}}
	if !box.Contains(render.Vec3{X: 2, Y: -1, Z: 3}) || box.Contains(render.Vec3{X: 2.001}) {
		t.Fatal("box faces must be inside and points past them outside")
	}
	// The distance bound agrees in sign with Contains away from the surface.
	for _, primitive := range []VOIPrimitive{
		sphere,
		Ellipsoid{Center: render.Vec3{X: 3}, RadiiMM: [3]float64{5, 2, 3}, Axes: rotatedAxes(0.9)},
		Box{Center: render.Vec3{Y: -2}, SizeMM: [3]float64{3, 7, 2}, Axes: rotatedAxes(1.3)},
	} {
		for i := -8; i <= 8; i++ {
			for j := -8; j <= 8; j++ {
				p := render.Vec3{X: float64(i) * 0.9, Y: float64(j) * 0.7, Z: float64(i*j%5) * 0.6}
				bound := primitive.DistanceBound(p)
				if bound < -1e-9 && !primitive.Contains(p) || bound > 1e-9 && primitive.Contains(p) {
					t.Fatalf("%T: bound %v disagrees with Contains at %v", primitive, bound, p)
				}
			}
		}
	}
}

// bruteForceWeights supersamples every voxel of the grid with no shortcut.
func bruteForceWeights(g VoxelGrid, primitive VOIPrimitive, n int) map[[3]int]float64 {
	out := map[[3]int]float64{}
	for k := 0; k < g.Slices; k++ {
		for j := 0; j < g.Rows; j++ {
			for i := 0; i < g.Columns; i++ {
				inside := 0
				for c := 0; c < n; c++ {
					for b := 0; b < n; b++ {
						for a := 0; a < n; a++ {
							u := func(m int) float64 { return (float64(m)+0.5)/float64(n) - 0.5 }
							if primitive.Contains(g.IndexToPoint(float64(i)+u(a), float64(j)+u(b), float64(k)+u(c))) {
								inside++
							}
						}
					}
				}
				if inside > 0 {
					out[[3]int{i, j, k}] = float64(inside) / float64(n*n*n)
				}
			}
		}
	}
	return out
}

// The whole-voxel shortcuts (distance bound beyond the half diagonal) must
// not change any partial-volume weight.
func TestRasterizeVOIPartialVolumeMatchesBruteForce(t *testing.T) {
	g := obliqueGrid(18, 26, 8)
	centre := g.IndexToPoint(8.3, 12.6, 3.4)
	for _, primitive := range []VOIPrimitive{
		Sphere{Center: centre, RadiusMM: 4.3},
		Ellipsoid{Center: centre, RadiiMM: [3]float64{5.5, 3, 4}, Axes: rotatedAxes(0.6)},
		Box{Center: centre, SizeMM: [3]float64{6, 4.5, 7}, Axes: rotatedAxes(-1.1)},
	} {
		mask, err := RasterizeVOI(context.Background(), g, primitive, VOIRasterOptions{Weighting: VOIPartialVolume, Supersampling: 3})
		if err != nil {
			t.Fatal(err)
		}
		want := bruteForceWeights(g, primitive, 3)
		if len(mask.Voxels) != len(want) {
			t.Fatalf("%T: %d voxels, brute force %d", primitive, len(mask.Voxels), len(want))
		}
		for _, v := range mask.Voxels {
			if w := want[[3]int{v.Column, v.Row, v.Slice}]; w != v.Weight {
				t.Fatalf("%T: voxel %v weight %v, brute force %v", primitive, v, v.Weight, w)
			}
			if v.CentreInside != primitive.Contains(g.IndexToPoint(float64(v.Column), float64(v.Row), float64(v.Slice))) {
				t.Fatalf("%T: voxel %v centre flag wrong", primitive, v)
			}
		}
		centreMask, err := RasterizeVOI(context.Background(), g, primitive, VOIRasterOptions{})
		if err != nil {
			t.Fatal(err)
		}
		inside := 0
		for _, v := range mask.Voxels {
			if v.CentreInside {
				inside++
			}
		}
		if len(centreMask.Voxels) != inside {
			t.Fatalf("%T: centre mask has %d voxels, partial mask %d centres inside", primitive, len(centreMask.Voxels), inside)
		}
	}
}

// Rasterized volumes converge on the analytic ones; partial-volume weights
// are several times closer than voxel-centre membership.
func TestRasterizeVOIAnalyticVolumes(t *testing.T) {
	for _, tc := range []struct {
		name               string
		grid               VoxelGrid
		primitive          VOIPrimitive
		centreTol, partTol float64 // relative
	}{
		{"sphere 1 mm", isoGrid(40, 1), Sphere{Center: render.Vec3{X: 19.3, Y: 20.1, Z: 19.6}, RadiusMM: 12}, 0.01, 0.001},
		{"sphere 4 mm PET", isoGrid(24, 4), Sphere{Center: render.Vec3{X: 45.1, Y: 47.7, Z: 46.2}, RadiusMM: 15}, 0.05, 0.005},
		{"oblique ellipsoid", obliqueGrid(70, 110, 20), Ellipsoid{Center: obliqueGrid(70, 110, 20).IndexToPoint(35, 55, 10), RadiiMM: [3]float64{14, 9, 11}, Axes: rotatedAxes(0.5)}, 0.02, 0.003},
		{"oblique box", obliqueGrid(70, 110, 20), Box{Center: obliqueGrid(70, 110, 20).IndexToPoint(35.2, 54.9, 10.3), SizeMM: [3]float64{20, 16, 18}, Axes: rotatedAxes(2.1)}, 0.03, 0.003},
	} {
		want := tc.primitive.VolumeMM3()
		for _, weighting := range []VOIWeighting{VOICentreInside, VOIPartialVolume} {
			mask, err := RasterizeVOI(context.Background(), tc.grid, tc.primitive, VOIRasterOptions{Weighting: weighting})
			if err != nil {
				t.Fatal(err)
			}
			tol := tc.centreTol
			if weighting == VOIPartialVolume {
				tol = tc.partTol
			}
			if got := mask.VolumeMM3(); math.Abs(got-want) > tol*want {
				t.Errorf("%s (%s): volume %.2f mm³, analytic %.2f (tolerance %.2g)", tc.name, weighting, got, want, tol)
			}
			if mask.Clipped || mask.AnalyticVolumeMM3 != want {
				t.Errorf("%s: clipped %v analytic %v", tc.name, mask.Clipped, mask.AnalyticVolumeMM3)
			}
		}
	}
}

func TestRasterizeVOIClipsToGrid(t *testing.T) {
	g := isoGrid(10, 2)
	inside, err := RasterizeVOI(context.Background(), g, Sphere{Center: render.Vec3{X: 9, Y: 9, Z: 9}, RadiusMM: 8}, VOIRasterOptions{})
	if err != nil || inside.Clipped {
		t.Fatalf("centred sphere: clipped %v err %v", inside != nil && inside.Clipped, err)
	}
	edge, err := RasterizeVOI(context.Background(), g, Sphere{Center: render.Vec3{X: 0, Y: 9, Z: 9}, RadiusMM: 8}, VOIRasterOptions{Weighting: VOIPartialVolume})
	if err != nil {
		t.Fatal(err)
	}
	if !edge.Clipped {
		t.Fatal("a sphere past the grid border is not reported as clipped")
	}
	// Half the sphere lies beyond x = -1 mm (the grid border): about half the
	// volume remains, never a voxel outside the grid.
	if got, half := edge.VolumeMM3(), edge.AnalyticVolumeMM3/2; got > half*1.2 || got < half*0.9 {
		t.Fatalf("clipped volume %v, expected about %v", got, half)
	}
	for _, v := range edge.Voxels {
		if v.Column < 0 || v.Row < 0 || v.Slice < 0 || v.Column >= 10 || v.Row >= 10 || v.Slice >= 10 {
			t.Fatalf("voxel %v outside the grid", v)
		}
	}
	outside, err := RasterizeVOI(context.Background(), g, Sphere{Center: render.Vec3{X: -50}, RadiusMM: 3}, VOIRasterOptions{})
	if err != nil || len(outside.Voxels) != 0 || !outside.Clipped {
		t.Fatalf("sphere outside the grid: %v voxels, err %v", len(outside.Voxels), err)
	}
	huge := isoGrid(300, 1)
	if _, err := RasterizeVOI(context.Background(), huge, Sphere{Center: render.Vec3{X: 150, Y: 150, Z: 150}, RadiusMM: 140}, VOIRasterOptions{}); !errors.Is(err, ErrInvalidVOI) {
		t.Fatalf("VOI over %d voxels: %v", MaxVOIVoxels, err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := RasterizeVOI(ctx, g, Sphere{Center: render.Vec3{X: 9, Y: 9, Z: 9}, RadiusMM: 8}, VOIRasterOptions{}); !errors.Is(err, context.Canceled) {
		t.Fatalf("cancelled rasterization: %v", err)
	}
}

func linearField(g VoxelGrid, a, b, c, d float64) VoxelValueFunc {
	return func(i, j, k int) (float64, bool) {
		if i < 0 || j < 0 || k < 0 || i >= g.Columns || j >= g.Rows || k >= g.Slices {
			return 0, false
		}
		p := g.IndexToPoint(float64(i), float64(j), float64(k))
		return a*p.X + b*p.Y + c*p.Z + d, true
	}
}

// A sphere centred on a voxel centre has a point-symmetric mask, so the
// weighted mean of a linear field is its value at the centre and the median
// equals the mean.
func TestVOIStatisticsLinearAndConstantFields(t *testing.T) {
	g := obliqueGrid(40, 60, 16)
	centre := g.IndexToPoint(20, 30, 8)
	field := linearField(g, 3, -2, 5, 100)
	want := 3*centre.X - 2*centre.Y + 5*centre.Z + 100
	for _, weighting := range []VOIWeighting{VOICentreInside, VOIPartialVolume} {
		mask, err := RasterizeVOI(context.Background(), g, Sphere{Center: centre, RadiusMM: 7}, VOIRasterOptions{Weighting: weighting})
		if err != nil {
			t.Fatal(err)
		}
		stats, err := mask.Statistics(field)
		if err != nil {
			t.Fatal(err)
		}
		if !closeTo(stats.Mean, want, 1e-9) || !closeTo(stats.Median, want, 1e-9) {
			t.Fatalf("%s: mean %v median %v, want %v", weighting, stats.Mean, stats.Median, want)
		}
		if stats.MissingVoxels != 0 || stats.VoxelCount != len(mask.Voxels) || !closeTo(stats.VolumeMM3, mask.VolumeMM3(), 1e-9) {
			t.Fatalf("%s: counts %+v", weighting, stats)
		}
		if !closeTo(stats.Sum, stats.Mean*stats.WeightSum, 1e-6) || !(stats.StdDev > 0) {
			t.Fatalf("%s: sum %v std %v", weighting, stats.Sum, stats.StdDev)
		}
		if v, _ := field(stats.MaxVoxel[0], stats.MaxVoxel[1], stats.MaxVoxel[2]); v != stats.Max || !(stats.Max > want && stats.Min < want) {
			t.Fatalf("%s: extremes %v..%v at %v", weighting, stats.Min, stats.Max, stats.MaxVoxel)
		}
		constant, err := mask.Statistics(func(int, int, int) (float64, bool) { return 4.5, true })
		if err != nil || constant.Mean != 4.5 || constant.StdDev != 0 || constant.Min != 4.5 || constant.Max != 4.5 || constant.Median != 4.5 {
			t.Fatalf("%s: constant field %+v, %v", weighting, constant, err)
		}
	}
}

func TestVOIStatisticsWeightsMissingAndMedian(t *testing.T) {
	mask := &VOIMask{
		Grid: isoGrid(4, 2),
		Voxels: []VOIVoxel{
			{Column: 0, Weight: 1, CentreInside: true},
			{Column: 1, Weight: 1, CentreInside: true},
			{Column: 2, Weight: 0.5, CentreInside: false},
			{Column: 3, Weight: 1, CentreInside: true},
			{Row: 1, Weight: 1, CentreInside: true},
		},
	}
	values := map[int]float64{0: 1, 1: 2, 2: 100, 3: 4}
	stats, err := mask.Statistics(func(i, j, _ int) (float64, bool) {
		if j == 1 {
			return math.NaN(), true
		}
		v, ok := values[i]
		return v, ok
	})
	if err != nil {
		t.Fatal(err)
	}
	// Weighted: (1 + 2 + 50 + 4) / 3.5; the partial voxel's 100 is not the
	// maximum because its centre is outside.
	if stats.VoxelCount != 4 || stats.MissingVoxels != 1 || stats.WeightSum != 3.5 || !closeTo(stats.Mean, 57/3.5, 1e-12) {
		t.Fatalf("stats %+v", stats)
	}
	if stats.Max != 4 || stats.MaxVoxel != [3]int{3, 0, 0} || stats.Min != 1 || stats.VolumeMM3 != 3.5*8 {
		t.Fatalf("extremes %+v", stats)
	}
	// Cumulative weights 1, 2, 3, 3.5 over sorted values 1, 2, 4, 100: half
	// of 3.5 is reached at 2.
	if stats.Median != 2 {
		t.Fatalf("weighted median %v, want 2", stats.Median)
	}
	even := &VOIMask{Grid: isoGrid(4, 1), Voxels: []VOIVoxel{{Column: 0, Weight: 1}, {Column: 1, Weight: 1}, {Column: 2, Weight: 1}, {Column: 3, Weight: 1}}}
	evenStats, _ := even.Statistics(func(i, _, _ int) (float64, bool) { return float64(10 * (i + 1)), true })
	if evenStats.Median != 25 || evenStats.Max != 40 {
		t.Fatalf("even median %v max %v", evenStats.Median, evenStats.Max)
	}
	if _, err := even.Statistics(func(int, int, int) (float64, bool) { return 0, false }); !errors.Is(err, ErrEmptyVOI) {
		t.Fatalf("no values: %v", err)
	}
	if _, err := even.Statistics(nil); !errors.Is(err, ErrInvalidVOI) {
		t.Fatalf("nil accessor: %v", err)
	}
}

// hotSpherePhantom is a 4 mm PET-like grid: background 1, a hot sphere of
// radius 15 mm (value 10) at hot, and one 50-valued voxel at spike.
func hotSpherePhantom(g VoxelGrid, hot render.Vec3, spike [3]int) VoxelValueFunc {
	return func(i, j, k int) (float64, bool) {
		if i < 0 || j < 0 || k < 0 || i >= g.Columns || j >= g.Rows || k >= g.Slices {
			return 0, false
		}
		if [3]int{i, j, k} == spike {
			return 50, true
		}
		if g.IndexToPoint(float64(i), float64(j), float64(k)).Sub(hot).Length() < 15 {
			return 10, true
		}
		return 1, true
	}
}

func TestVOIPeakSyntheticPhantom(t *testing.T) {
	g := isoGrid(30, 4)
	hot := g.IndexToPoint(15, 15, 15)
	spike := [3]int{23, 15, 15} // 32 mm from the hot centre
	field := hotSpherePhantom(g, hot, spike)
	mask, err := RasterizeVOI(context.Background(), g, Sphere{Center: hot, RadiusMM: 40}, VOIRasterOptions{})
	if err != nil {
		t.Fatal(err)
	}
	stats, err := mask.Statistics(field)
	if err != nil || stats.Max != 50 || stats.MaxVoxel != spike {
		t.Fatalf("max %v at %v, err %v", stats.Max, stats.MaxVoxel, err)
	}

	// PERCIST: the 1 cm³ sphere of highest mean sits inside the hot sphere,
	// not on the isolated maximum.
	peak, err := mask.Peak(context.Background(), field, PeakOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if !closeTo(peak.Mean, 10, 1e-9) || peak.Center.Sub(hot).Length() >= 15 || peak.Incomplete {
		t.Fatalf("peak %+v", peak)
	}
	if !closeTo(peak.RadiusMM, 6.2035, 1e-4) || math.Abs(peak.KernelVolumeMM3-1000) > 30 || peak.Weighting != VOIPartialVolume {
		t.Fatalf("kernel radius %v volume %v", peak.RadiusMM, peak.KernelVolumeMM3)
	}
	if peak.Maximum != 50 || peak.MaximumVoxel != spike || peak.Candidates != len(mask.Voxels) || peak.SkippedIncomplete != 0 {
		t.Fatalf("search bookkeeping %+v", peak)
	}

	// Centred on the maximum, the sphere averages the spike (weight 1 at the
	// centre) with background.
	kernelWeight := peak.KernelVolumeMM3 / g.VoxelVolumeMM3()
	spikeMean := (50 + (kernelWeight - 1)) / kernelWeight
	at, err := mask.Peak(context.Background(), field, PeakOptions{Search: PeakSearchAtMaximum})
	if err != nil {
		t.Fatal(err)
	}
	if !closeTo(at.Mean, spikeMean, 1e-9) || at.CenterVoxel != spike || at.Candidates != 1 {
		t.Fatalf("at maximum: mean %v at %v, want %v at %v", at.Mean, at.CenterVoxel, spikeMean, spike)
	}
	// Around the maximum no sphere reaches the hot sphere, so the best keeps
	// the spike at full weight: the spike's own sphere or, at 5³
	// supersampling, a face neighbour whose sphere still covers the spike
	// voxel completely (ties keep the first centre in x-fastest order).
	around, err := mask.Peak(context.Background(), field, PeakOptions{Search: PeakSearchAroundMaximum, SearchRadiusMM: 8})
	if err != nil {
		t.Fatal(err)
	}
	if !closeTo(around.Mean, spikeMean, 1e-9) || around.Candidates <= 1 {
		t.Fatalf("around maximum: mean %v (want %v) over %d centres", around.Mean, spikeMean, around.Candidates)
	}
	if d := around.Center.Sub(g.IndexToPoint(23, 15, 15)).Length(); d > 4+1e-9 {
		t.Fatalf("around maximum: centre %v is %v mm from the spike", around.CenterVoxel, d)
	}
}

// With voxel-centre membership the 1 cm³ sphere on a 4 mm grid holds the 19
// voxels with i²+j²+k² ≤ 2 (the centre, 6 faces, 12 edges): 1216 mm³.
func TestVOIPeakCentreInsideKernel(t *testing.T) {
	g := isoGrid(12, 4)
	mask, err := RasterizeVOI(context.Background(), g, Sphere{Center: g.IndexToPoint(6, 6, 6), RadiusMM: 3}, VOIRasterOptions{})
	if err != nil {
		t.Fatal(err)
	}
	peak, err := mask.Peak(context.Background(), linearField(g, 0, 0, 0, 2), PeakOptions{CentreInsideKernel: true})
	if err != nil {
		t.Fatal(err)
	}
	if peak.KernelVoxels != 19 || peak.KernelVolumeMM3 != 19*64 || !closeTo(peak.Mean, 2, 1e-12) || peak.Weighting != VOICentreInside {
		t.Fatalf("peak %+v", peak)
	}
}

// For a linear field the symmetric sphere's mean is the value at its centre,
// so the peak is the candidate farthest along the gradient.
func TestVOIPeakLinearFieldIsCentreValue(t *testing.T) {
	g := obliqueGrid(60, 90, 24)
	centre := g.IndexToPoint(30, 45, 12)
	field := linearField(g, 1, 0.5, -2, 0)
	mask, err := RasterizeVOI(context.Background(), g, Sphere{Center: centre, RadiusMM: 12}, VOIRasterOptions{})
	if err != nil {
		t.Fatal(err)
	}
	peak, err := mask.Peak(context.Background(), field, PeakOptions{})
	if err != nil {
		t.Fatal(err)
	}
	stats, _ := mask.Statistics(field)
	if want, _ := field(peak.CenterVoxel[0], peak.CenterVoxel[1], peak.CenterVoxel[2]); !closeTo(peak.Mean, want, 1e-9) {
		t.Fatalf("peak mean %v, centre value %v", peak.Mean, want)
	}
	if !closeTo(peak.Mean, stats.Max, 1e-9) || peak.CenterVoxel != stats.MaxVoxel {
		t.Fatalf("peak %v at %v, max %v at %v", peak.Mean, peak.CenterVoxel, stats.Max, stats.MaxVoxel)
	}
}

func TestVOIPeakIncompleteSphereIsOptIn(t *testing.T) {
	g := isoGrid(8, 4)
	// A VOI hugging the grid corner: every 6.2 mm sphere leaves the grid.
	mask, err := RasterizeVOI(context.Background(), g, Sphere{Center: render.Vec3{}, RadiusMM: 5}, VOIRasterOptions{})
	if err != nil {
		t.Fatal(err)
	}
	field := linearField(g, 0, 0, 0, 3)
	peak, err := mask.Peak(context.Background(), field, PeakOptions{})
	if !errors.Is(err, ErrNoPeak) || peak.SkippedIncomplete != peak.Candidates || peak.Candidates == 0 {
		t.Fatalf("incomplete spheres accepted by default: %+v, %v", peak, err)
	}
	peak, err = mask.Peak(context.Background(), field, PeakOptions{AllowIncompleteSphere: true})
	if err != nil {
		t.Fatal(err)
	}
	if !peak.Incomplete || !closeTo(peak.Mean, 3, 1e-12) || !(peak.CoveredVolumeMM3 < peak.KernelVolumeMM3) {
		t.Fatalf("opt-in incomplete peak %+v", peak)
	}
	// A voxel without a value inside the sphere also makes it incomplete.
	inner := isoGrid(12, 4)
	innerMask, _ := RasterizeVOI(context.Background(), inner, Sphere{Center: inner.IndexToPoint(6, 6, 6), RadiusMM: 1}, VOIRasterOptions{})
	holey := func(i, j, k int) (float64, bool) { return 1, [3]int{i, j, k} != [3]int{7, 6, 6} }
	if _, err := innerMask.Peak(context.Background(), holey, PeakOptions{}); !errors.Is(err, ErrNoPeak) {
		t.Fatalf("sphere with a missing voxel: %v", err)
	}
}

func TestVOIPeakRejectsInvalidOptions(t *testing.T) {
	g := isoGrid(12, 4)
	mask, _ := RasterizeVOI(context.Background(), g, Sphere{Center: g.IndexToPoint(6, 6, 6), RadiusMM: 10}, VOIRasterOptions{})
	field := linearField(g, 0, 0, 0, 1)
	for _, opts := range []PeakOptions{
		{VolumeMM3: -1},
		{VolumeMM3: math.Inf(1)},
		{Search: PeakSearchAroundMaximum},
		{Search: 9},
		{Supersampling: 99},
	} {
		if _, err := mask.Peak(context.Background(), field, opts); !errors.Is(err, ErrInvalidVOI) {
			t.Errorf("%+v: %v", opts, err)
		}
	}
	// A sphere smaller than every voxel has no centre-inside kernel.
	if _, err := mask.Peak(context.Background(), field, PeakOptions{VolumeMM3: 1, CentreInsideKernel: true}); err != nil {
		t.Errorf("tiny centre kernel still holds the centre voxel: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := mask.Peak(ctx, field, PeakOptions{}); !errors.Is(err, context.Canceled) {
		t.Errorf("cancelled peak: %v", err)
	}
}

// petPhantomVolume is an n³ PT render.Volume with the given column, row and
// slice spacing whose 16-bit values (Bq/ml, slope 1) come from value at each
// voxel centre.
func petPhantomVolume(t *testing.T, n int, spacing [3]float64, value func(p render.Vec3) float64) *render.Volume {
	t.Helper()
	stack := &render.Stack{Modality: "PT", PixelSpacing: []float64{spacing[1], spacing[0]}, SliceThickness: spacing[2]}
	for z := 0; z < n; z++ {
		data := make([]byte, n*n*2)
		for y := 0; y < n; y++ {
			for x := 0; x < n; x++ {
				p := render.Vec3{X: float64(x) * spacing[0], Y: float64(y) * spacing[1], Z: float64(z) * spacing[2]}
				binary.LittleEndian.PutUint16(data[2*(y*n+x):], uint16(math.Round(value(p))))
			}
		}
		stack.Frames = append(stack.Frames, &render.Frame{
			Metadata: pixeldata.Metadata{
				Rows: uint16(n), Columns: uint16(n), SamplesPerPixel: 1,
				BitsAllocated: 16, BitsStored: 16, HighBit: 15,
				PhotometricInterpretation: "MONOCHROME2",
			},
			ByteOrder:        binary.LittleEndian,
			PixelBytes:       data,
			Rescale:          render.Rescale{Slope: 1},
			ImageOrientation: []float64{1, 0, 0, 0, 1, 0},
			ImagePosition:    []float64{0, 0, float64(z) * spacing[2]},
			PixelSpacing:     []float64{spacing[1], spacing[0]},
			SliceThickness:   spacing[2],
		})
	}
	vol, err := render.BuildVolume(stack)
	if err != nil {
		t.Fatal(err)
	}
	return vol
}

func TestVoxelGridFromDescriptorMatchesVolume(t *testing.T) {
	vol := rampVolume(t, 6, 5, 4)
	lease, err := vol.AcquireSnapshot()
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = lease.Release() }()
	snapshot, _ := lease.Snapshot()
	descriptor, _ := snapshot.Descriptor()
	g, err := VoxelGridFromDescriptor(descriptor)
	if err != nil {
		t.Fatal(err)
	}
	if g.Columns != 6 || g.Rows != 5 || g.Slices != 4 || !closeTo(g.VoxelVolumeMM3(), 0.8*0.5*2.5, 1e-12) {
		t.Fatalf("grid %+v", g)
	}
	for _, idx := range []render.Vec3{{}, {X: 5, Y: 4, Z: 3}, {X: 2, Y: 1, Z: 2}} {
		want := vol.VoxelToPatient(idx)
		if got := g.IndexToPoint(idx.X, idx.Y, idx.Z); got.Sub(want).Length() > 1e-9 {
			t.Fatalf("index %v: grid %v, volume %v", idx, got, want)
		}
	}
	if _, err := VoxelGridFromDescriptor(render.VolumeDescriptor{}); !errors.Is(err, ErrInvalidVOI) {
		t.Fatalf("empty descriptor: %v", err)
	}
}

// End to end on a render.Volume: statistics of a linear CT ramp and the
// SUVpeak-style search on a hot-sphere PET phantom.
func TestVolumeVOI(t *testing.T) {
	ramp := rampVolume(t, 30, 40, 12)
	centre := ramp.VoxelToPatient(render.Vec3{X: 15, Y: 20, Z: 6})
	result, err := VolumeVOI(context.Background(), ramp, Sphere{Center: centre, RadiusMM: 6}, VolumeVOIOptions{Raster: VOIRasterOptions{Weighting: VOIPartialVolume}})
	if err != nil {
		t.Fatal(err)
	}
	if !closeTo(result.Statistics.Mean, rampVolumeValue(centre), 1e-6) || result.Peak != nil || result.Mask.Clipped {
		t.Fatalf("ramp mean %v, want %v", result.Statistics.Mean, rampVolumeValue(centre))
	}
	if v := result.Statistics.VolumeMM3; math.Abs(v-4.0/3.0*math.Pi*216) > 0.01*v {
		t.Fatalf("ramp VOI volume %v", v)
	}

	const n = 32
	spacing := [3]float64{4, 4, 3}
	hot := render.Vec3{X: 64, Y: 64, Z: 48}
	pet := petPhantomVolume(t, n, spacing, func(p render.Vec3) float64 {
		if p.Sub(hot).Length() < 15 {
			return 1000
		}
		return 100
	})
	result, err = VolumeVOI(context.Background(), pet, Sphere{Center: hot, RadiusMM: 30}, VolumeVOIOptions{Peak: &PeakOptions{}})
	if err != nil {
		t.Fatal(err)
	}
	if result.PeakErr != nil || result.Peak == nil || !closeTo(result.Peak.Mean, 1000, 1e-9) || result.Statistics.Max != 1000 {
		t.Fatalf("PET peak %+v err %v, max %v", result.Peak, result.PeakErr, result.Statistics.Max)
	}
	if v := result.Peak.KernelVolumeMM3; math.Abs(v-1000) > 30 {
		t.Fatalf("PET kernel volume %v", v)
	}
	// A VOI at the image corner keeps its statistics and reports the peak
	// as unavailable.
	corner, err := VolumeVOI(context.Background(), pet, Sphere{Center: render.Vec3{}, RadiusMM: 5}, VolumeVOIOptions{Peak: &PeakOptions{}})
	if err != nil || corner.Peak != nil || !errors.Is(corner.PeakErr, ErrNoPeak) || corner.Statistics.Mean != 100 || !corner.Mask.Clipped {
		t.Fatalf("corner VOI %+v, err %v", corner, err)
	}
	if _, err := VolumeVOI(context.Background(), pet, Sphere{Center: render.Vec3{X: -500}, RadiusMM: 5}, VolumeVOIOptions{}); !errors.Is(err, ErrEmptyVOI) {
		t.Fatalf("VOI outside the image: %v", err)
	}
	if _, err := VolumeVOI(context.Background(), nil, Sphere{RadiusMM: 1}, VolumeVOIOptions{}); err == nil {
		t.Fatal("nil volume accepted")
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := VolumeVOI(ctx, pet, Sphere{Center: hot, RadiusMM: 30}, VolumeVOIOptions{Peak: &PeakOptions{}}); !errors.Is(err, context.Canceled) {
		t.Fatalf("cancelled volume VOI: %v", err)
	}
}
