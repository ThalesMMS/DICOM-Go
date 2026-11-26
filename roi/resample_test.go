package roi

import (
	"context"
	"errors"
	"math"
	"testing"

	"github.com/ThalesMMS/dicom-go/render"
)

func axisGrid(columns, rows, slices int, spacing [3]float64, origin render.Vec3) Grid {
	return Grid{
		Columns: columns, Rows: rows, Slices: slices, Origin: origin,
		RowDir: render.Vec3{X: 1}, ColDir: render.Vec3{Y: 1}, SliceDir: render.Vec3{Z: 1},
		ColSpacing: spacing[0], RowSpacing: spacing[1], SliceSpacing: spacing[2],
	}
}

// rotatedGrid is an oblique grid: the axes rotated about (1, 1, 1) by angle
// radians, centred on centre.
func rotatedGrid(columns, rows, slices int, spacing [3]float64, centre render.Vec3, angle float64) Grid {
	axis := render.Vec3{X: 1, Y: 1, Z: 1}.Normalize()
	g := Grid{
		Columns: columns, Rows: rows, Slices: slices,
		RowDir:     render.Vec3{X: 1}.Rotate(axis, angle),
		ColDir:     render.Vec3{Y: 1}.Rotate(axis, angle),
		SliceDir:   render.Vec3{Z: 1}.Rotate(axis, angle),
		ColSpacing: spacing[0], RowSpacing: spacing[1], SliceSpacing: spacing[2],
	}
	half := g.RowDir.Scale(float64(columns-1) * spacing[0] / 2).
		Add(g.ColDir.Scale(float64(rows-1) * spacing[1] / 2)).
		Add(g.SliceDir.Scale(float64(slices-1) * spacing[2] / 2))
	g.Origin = centre.Sub(half)
	return g
}

func sphereVolume(t *testing.T, grid Grid, centre render.Vec3, radius float64, label uint16) *LabelVolume {
	t.Helper()
	v, err := NewLabelVolume(grid)
	if err != nil {
		t.Fatal(err)
	}
	for z := 0; z < grid.Slices; z++ {
		for y := 0; y < grid.Rows; y++ {
			for x := 0; x < grid.Columns; x++ {
				if grid.VoxelCenter(x, y, z).Sub(centre).Length() <= radius {
					v.Set(x, y, z, label)
				}
			}
		}
	}
	return v
}

func voxelVolume(g Grid) float64 { return g.ColSpacing * g.RowSpacing * g.SliceSpacing }

// TestResampleLabelsSphereAcrossGrids resamples an analytic sphere between
// grids of different spacing, anisotropy and obliquity: every target voxel
// whose label disagrees with the analytic sphere lies within one source voxel
// diagonal of the sphere surface, and the volume is preserved within 6 %.
func TestResampleLabelsSphereAcrossGrids(t *testing.T) {
	centre := render.Vec3{X: 20.3, Y: 19.7, Z: 21.1}
	const radius = 12.0
	source := axisGrid(60, 60, 40, [3]float64{1, 1, 1.5}, render.Vec3{X: -9.8, Y: -10.4, Z: -9.5})
	src := sphereVolume(t, source, centre, radius, 7)
	diagonal := math.Sqrt(1 + 1 + 1.5*1.5)
	sourceVolume := float64(src.Count(7)) * voxelVolume(source)

	targets := map[string]Grid{
		"finer isotropic":     axisGrid(80, 80, 84, [3]float64{0.5, 0.5, 0.5}, render.Vec3{}),
		"coarser anisotropic": axisGrid(14, 20, 16, [3]float64{3, 2, 2.5}, render.Vec3{X: 0.7, Y: 0.1, Z: 1.3}),
		"oblique 30 degrees":  rotatedGrid(36, 34, 30, [3]float64{0.8, 0.9, 1.1}, centre, math.Pi/6),
		"oblique anisotropic": rotatedGrid(16, 26, 10, [3]float64{2, 1.25, 3}, centre.Add(render.Vec3{X: 1}), 1.1),
	}
	for name, target := range targets {
		for _, mode := range []LabelResampleMode{NearestLabels(), LabelGaussian(0.6)} {
			t.Run(name+"/"+mode.String(), func(t *testing.T) {
				result, err := ResampleLabels(context.Background(), src, target, mode)
				if err != nil {
					t.Fatal(err)
				}
				if !result.Coverage.Full() {
					t.Fatalf("coverage = %d of %d voxels, want the sphere target fully covered", result.Coverage.CoveredVoxels, result.Coverage.TotalVoxels())
				}
				mismatches := 0
				for z := 0; z < target.Slices; z++ {
					for y := 0; y < target.Rows; y++ {
						for x := 0; x < target.Columns; x++ {
							distance := target.VoxelCenter(x, y, z).Sub(centre).Length()
							inside := result.Volume.LabelAt(x, y, z) == 7
							if inside != (distance <= radius) {
								mismatches++
								if math.Abs(distance-radius) > diagonal {
									t.Fatalf("voxel (%d,%d,%d) %.2f mm from the surface is mislabelled", x, y, z, math.Abs(distance-radius))
								}
							}
						}
					}
				}
				got := float64(result.Counts[7]) * voxelVolume(target)
				if math.Abs(got-sourceVolume)/sourceVolume > 0.06 {
					t.Fatalf("volume %.0f mm³, source %.0f mm³ (%d mismatches)", got, sourceVolume, mismatches)
				}
				if len(result.Vanished) != 0 {
					t.Fatalf("Vanished = %v", result.Vanished)
				}
			})
		}
	}
}

// TestResampleLabelsIdentityAndMasks checks that resampling onto the source
// grid is exact in both modes and that the mask output matches the dense one.
func TestResampleLabelsIdentityAndMasks(t *testing.T) {
	grid := rotatedGrid(17, 13, 9, [3]float64{0.7, 1.1, 2}, render.Vec3{X: 5, Y: -3, Z: 40}, 0.4)
	src := sphereVolume(t, grid, grid.VoxelCenter(8, 6, 4), 4, 3)
	for i := range src.Labels {
		if i%11 == 0 && src.Labels[i] == 0 {
			src.Labels[i] = 9 // isolated single-voxel labels
		}
	}
	for _, mode := range []LabelResampleMode{NearestLabels(), LabelGaussian(0.05)} {
		dense, err := ResampleLabels(context.Background(), src, grid, mode)
		if err != nil {
			t.Fatal(err)
		}
		for i := range src.Labels {
			if dense.Volume.Labels[i] != src.Labels[i] {
				t.Fatalf("%s: voxel %d = %d, want %d", mode, i, dense.Volume.Labels[i], src.Labels[i])
			}
		}
		masks, err := ResampleLabelMasks(context.Background(), src, grid, mode)
		if err != nil {
			t.Fatal(err)
		}
		for _, label := range []uint16{3, 9} {
			for z := 0; z < grid.Slices; z++ {
				want := dense.Volume.SliceMask(label, z)
				got := masks.Masks[label][z]
				if got == nil {
					got = NewRasterMask(grid.Columns, grid.Rows)
				}
				for y := 0; y < grid.Rows; y++ {
					for x := 0; x < grid.Columns; x++ {
						if got.Get(x, y) != want.Get(x, y) {
							t.Fatalf("%s: label %d mask differs at (%d,%d,%d)", mode, label, x, y, z)
						}
					}
				}
			}
		}
	}
}

// TestResampleLabelsITKGaussianCase ports the case of ITK's
// itkLabelImageGaussianInterpolateImageFunctionTest (Apache-2.0): a 3x3
// label image over a 10 mm field, interpolated with sigma 1 mm and alpha 1
// onto a 6x5 grid of 2 mm; the column beyond the source extent is outside.
func TestResampleLabelsITKGaussianCase(t *testing.T) {
	const fov = 10.0
	source := axisGrid(3, 3, 1, [3]float64{fov / 3, fov / 3, 1}, render.Vec3{X: fov / 6, Y: fov / 6})
	src, err := NewLabelVolume(source)
	if err != nil {
		t.Fatal(err)
	}
	// ITK indexes its table [x][y].
	values := [3][3]uint16{{255, 255, 255}, {255, 171, 7}, {7, 7, 7}}
	for x := 0; x < 3; x++ {
		for y := 0; y < 3; y++ {
			src.Set(x, y, 0, values[x][y])
		}
	}
	target := axisGrid(6, 5, 1, [3]float64{fov / 5, fov / 5, 1}, render.Vec3{X: fov / 10, Y: fov / 10})
	mode := LabelGaussian(1)
	mode.CutoffSigmas = 1
	result, err := ResampleLabels(context.Background(), src, target, mode)
	if err != nil {
		t.Fatal(err)
	}
	const outside = 0
	want := [6][5]uint16{
		{255, 255, 255, 255, 255},
		{255, 255, 255, 255, 255},
		{255, 255, 171, 7, 7},
		{7, 7, 7, 7, 7},
		{7, 7, 7, 7, 7},
		{outside, outside, outside, outside, outside},
	}
	for x := 0; x < 6; x++ {
		for y := 0; y < 5; y++ {
			if got := result.Volume.LabelAt(x, y, 0); got != want[x][y] {
				t.Errorf("(%d,%d) = %d, want %d", x, y, got, want[x][y])
			}
			if covered := result.Coverage.Covered(x, y, 0); covered != (x < 5) {
				t.Errorf("(%d,%d) covered = %v, want %v", x, y, covered, x < 5)
			}
		}
	}
	if !result.Coverage.Partial() || result.Coverage.SlicePixels[0] != 25 {
		t.Fatalf("coverage = %+v, want 25 of 30 pixels", result.Coverage)
	}
}

// TestResampleLabelsKeepsSmallLabelsWithGaussian downsamples a 1 mm grid
// with single-voxel labels to 3 mm: nearest misses the labels between target
// centres, the plain Gaussian argmax loses them to the background, and the
// preserving Gaussian mode keeps every one on one target voxel.
func TestResampleLabelsKeepsSmallLabelsWithGaussian(t *testing.T) {
	source := axisGrid(30, 30, 30, [3]float64{1, 1, 1}, render.Vec3{})
	src, err := NewLabelVolume(source)
	if err != nil {
		t.Fatal(err)
	}
	big := sphereVolume(t, source, render.Vec3{X: 15, Y: 15, Z: 15}, 6, 1)
	copy(src.Labels, big.Labels)
	small := map[uint16][3]int{2: {5, 5, 5}, 3: {24, 6, 21}, 4: {5, 26, 26}, 5: {14, 15, 15}}
	for label, p := range small {
		src.Set(p[0], p[1], p[2], label)
	}
	// Target centres at 1, 4, 7, ... mm: none falls on the small voxels.
	target := axisGrid(10, 10, 10, [3]float64{3, 3, 3}, render.Vec3{X: 1, Y: 1, Z: 1})

	nearest, err := ResampleLabels(context.Background(), src, target, NearestLabels())
	if err != nil {
		t.Fatal(err)
	}
	if len(nearest.Vanished) != 4 || nearest.Counts[1] == 0 {
		t.Fatalf("nearest Vanished = %v counts = %v, want the four small labels lost", nearest.Vanished, nearest.Counts)
	}

	plain := LabelGaussian(1)
	plain.DisablePreservation = true
	argmax, err := ResampleLabels(context.Background(), src, target, plain)
	if err != nil {
		t.Fatal(err)
	}
	if len(argmax.Vanished) == 0 {
		t.Fatalf("plain Gaussian argmax kept every small label: %v", argmax.Counts)
	}

	kept, err := ResampleLabels(context.Background(), src, target, LabelGaussian(1))
	if err != nil {
		t.Fatal(err)
	}
	if len(kept.Vanished) != 0 {
		t.Fatalf("Gaussian Vanished = %v", kept.Vanished)
	}
	for label, p := range small {
		if kept.Counts[label] != 1 || kept.Volume.Count(label) != 1 {
			t.Fatalf("label %d count = %d, want 1", label, kept.Counts[label])
		}
		// The kept voxel is the target voxel nearest the source voxel.
		for i, v := range kept.Volume.Labels {
			if v != label {
				continue
			}
			x, y, z := i%10, i/10%10, i/100
			if target.VoxelCenter(x, y, z).Sub(source.VoxelCenter(p[0], p[1], p[2])).Length() > math.Sqrt(3)*1.5+1e-9 {
				t.Fatalf("label %d kept at (%d,%d,%d), far from its source voxel", label, x, y, z)
			}
		}
	}
	// The large label keeps its volume (4/3·π·6³ mm³ in 27 mm³ voxels).
	if want := 4.0 / 3 * math.Pi * 216 / 27; math.Abs(float64(kept.Counts[1])-want)/want > 0.1 {
		t.Fatalf("large label has %d voxels, want about %.1f", kept.Counts[1], want)
	}
	masks, err := ResampleLabelMasks(context.Background(), src, target, LabelGaussian(1))
	if err != nil {
		t.Fatal(err)
	}
	for label := range small {
		n := 0
		for _, mask := range masks.Masks[label] {
			n += mask.Count()
		}
		if n != 1 {
			t.Fatalf("mask output label %d has %d voxels, want 1", label, n)
		}
	}
}

// TestResampleMaskStacksOverlap resamples two overlapping binary segments
// separately: both keep the shared voxels, which a single label map cannot.
func TestResampleMaskStacksOverlap(t *testing.T) {
	source := axisGrid(20, 20, 10, [3]float64{1, 1, 2}, render.Vec3{})
	a := make([]*RasterMask, source.Slices)
	b := make([]*RasterMask, source.Slices)
	for z := 2; z < 8; z++ {
		a[z], b[z] = NewRasterMask(20, 20), NewRasterMask(20, 20)
		for y := 4; y < 14; y++ {
			a[z].SetRun(y, 2, 12)
			b[z].SetRun(y, 8, 18)
		}
	}
	target := rotatedGrid(30, 30, 25, [3]float64{0.75, 0.75, 0.8}, source.VoxelCenter(10, 9, 5), 0.2)
	var results [2]*LabelMaskResult
	for i, masks := range [][]*RasterMask{a, b} {
		stack, err := NewMaskStack(source, masks, 0)
		if err != nil {
			t.Fatal(err)
		}
		results[i], err = ResampleLabelMasks(context.Background(), stack, target, NearestLabels())
		if err != nil {
			t.Fatal(err)
		}
		if results[i].Counts[1] == 0 {
			t.Fatalf("segment %d vanished", i)
		}
	}
	shared := 0
	for z := 0; z < target.Slices; z++ {
		ma, mb := results[0].Masks[1][z], results[1].Masks[1][z]
		if ma == nil || mb == nil {
			continue
		}
		both := ma.Clone()
		both.Intersect(mb)
		shared += both.Count()
	}
	if shared == 0 {
		t.Fatal("overlapping segments share no resampled voxel")
	}
	// Every shared voxel lies in the analytic overlap (x in [8,12) mm voxels,
	// widened by one source voxel diagonal).
	for z := 0; z < target.Slices; z++ {
		ma, mb := results[0].Masks[1][z], results[1].Masks[1][z]
		if ma == nil || mb == nil {
			continue
		}
		both := ma.Clone()
		both.Intersect(mb)
		both.ForEachPixel(func(x, y int) {
			p := target.VoxelCenter(x, y, z)
			if p.X < 7.5-math.Sqrt(6) || p.X > 11.5+math.Sqrt(6) {
				t.Fatalf("shared voxel at x=%.2f mm is outside the overlap", p.X)
			}
		})
	}
}

// TestResampleLabelsCoverage checks partial coverage by slice and pixel.
func TestResampleLabelsCoverage(t *testing.T) {
	source := axisGrid(10, 10, 5, [3]float64{1, 1, 2}, render.Vec3{})
	src, err := NewLabelVolume(source)
	if err != nil {
		t.Fatal(err)
	}
	for i := range src.Labels {
		src.Labels[i] = 1
	}
	// The source covers x, y in [-0.5, 9.5) and z in [-1, 9).
	target := axisGrid(8, 6, 6, [3]float64{2, 1, 2}, render.Vec3{X: 4, Y: 6, Z: -3})
	result, err := ResampleLabels(context.Background(), src, target, NearestLabels())
	if err != nil {
		t.Fatal(err)
	}
	c := result.Coverage
	// Target z centres: -3, -1, 1, 3, 5, 7 mm; -3 is outside, -1 is inside.
	if got := c.CoveredSlices(); len(got) != 5 || got[0] != 1 || got[4] != 5 {
		t.Fatalf("covered slices = %v, want 1..5", got)
	}
	// x centres 4, 6, 8, 10, ... -> 3 columns; y centres 6..9 -> 4 rows.
	for z := 1; z < 6; z++ {
		if c.SlicePixels[z] != 12 {
			t.Fatalf("slice %d covered pixels = %d, want 12", z, c.SlicePixels[z])
		}
	}
	if !c.Partial() || c.Full() || c.CoveredVoxels != 60 || math.Abs(c.SliceFraction(2)-12.0/48) > 1e-12 {
		t.Fatalf("coverage summary = %d voxels partial=%v", c.CoveredVoxels, c.Partial())
	}
	for z := 0; z < target.Slices; z++ {
		for y := 0; y < target.Rows; y++ {
			for x := 0; x < target.Columns; x++ {
				if (result.Volume.LabelAt(x, y, z) == 1) != c.Covered(x, y, z) {
					t.Fatalf("(%d,%d,%d) label/coverage disagree", x, y, z)
				}
			}
		}
	}
	direct, err := GridCoverage(context.Background(), source, target)
	if err != nil || direct.CoveredVoxels != c.CoveredVoxels {
		t.Fatalf("GridCoverage = %d, %v", direct.CoveredVoxels, err)
	}
}

// TestGridFromSlicesIrregularAndTilted builds grids from irregular and
// gantry-tilted stacks and checks voxel centres and nearest reads.
func TestGridFromSlicesIrregularAndTilted(t *testing.T) {
	base := render.SliceGeometry{
		RowDir: render.Vec3{X: 1}, ColDir: render.Vec3{Y: 1}, Normal: render.Vec3{Z: 1},
		RowSpacing: 1, ColSpacing: 1, Rows: 4, Columns: 4,
	}
	var irregular []render.SliceGeometry
	for _, z := range []float64{0, 1, 2, 5, 6} {
		s := base
		s.Origin = render.Vec3{Z: z}
		irregular = append(irregular, s)
	}
	grid, err := GridFromSlices(irregular)
	if err != nil {
		t.Fatal(err)
	}
	if len(grid.SliceOffsets) != 5 || grid.VoxelCenter(0, 0, 3).Z != 5 {
		t.Fatalf("irregular grid = %+v", grid)
	}
	labels := make([][]uint16, 5)
	for z := range labels {
		labels[z] = make([]uint16, 16)
		for i := range labels[z] {
			labels[z][i] = uint16(z + 1)
		}
	}
	stack, err := NewLabelStack(grid, labels)
	if err != nil {
		t.Fatal(err)
	}
	// Target slices at z = 0.4, 3.4, 3.6, 6.4, 6.6 mm.
	target := Grid{
		Columns: 4, Rows: 4, Slices: 5, RowDir: render.Vec3{X: 1}, ColDir: render.Vec3{Y: 1}, SliceDir: render.Vec3{Z: 1},
		ColSpacing: 1, RowSpacing: 1, SliceSpacing: 1, SliceOffsets: []float64{0.4, 3.4, 3.6, 6.4, 6.6},
	}
	result, err := ResampleLabels(context.Background(), stack, target, NearestLabels())
	if err != nil {
		t.Fatal(err)
	}
	// 3.4 is nearer 2 (slice 3), 3.6 nearer 5 (slice 4); the last slice's
	// box ends at 6.5.
	want := []uint16{1, 3, 4, 5, 0}
	for z, label := range want {
		if got := result.Volume.LabelAt(1, 1, z); got != label {
			t.Fatalf("target slice %d = %d, want %d", z, got, label)
		}
	}

	var tilted []render.SliceGeometry
	for k := 0; k < 4; k++ {
		s := base
		s.Origin = render.Vec3{Y: 0.3 * float64(k), Z: 2 * float64(k)}
		tilted = append(tilted, s)
	}
	tiltGrid, err := GridFromSlices(tilted)
	if err != nil {
		t.Fatal(err)
	}
	for k := range tilted {
		if tiltGrid.VoxelCenter(2, 1, k).Sub(tilted[k].Origin.Add(render.Vec3{X: 2, Y: 1})).Length() > 1e-9 {
			t.Fatalf("tilted voxel centre %d mismatch", k)
		}
		x, y, z, inside := tiltGrid.ContinuousIndex(tilted[k].Origin.Add(render.Vec3{X: 3, Y: 2}))
		if !inside || math.Abs(x-3) > 1e-9 || math.Abs(y-2) > 1e-9 || math.Abs(z-float64(k)) > 1e-9 {
			t.Fatalf("tilted continuous index = %v %v %v %v", x, y, z, inside)
		}
	}

	bent := append([]render.SliceGeometry(nil), irregular...)
	bent[2].Origin.X = 3
	if _, err := GridFromSlices(bent); !errors.Is(err, ErrInvalidGrid) {
		t.Fatalf("non-collinear stack err = %v", err)
	}
	if _, err := GridFromSlices(irregular[:1]); !errors.Is(err, ErrInvalidGrid) {
		t.Fatalf("single-slice stack err = %v", err)
	}
}

// TestResampleFractionsLinear checks that linear interpolation reproduces a
// linear ramp and that the thresholded mask is the analytic half-space.
func TestResampleFractionsLinear(t *testing.T) {
	source := axisGrid(21, 5, 3, [3]float64{1, 1, 1}, render.Vec3{})
	slices := make([][]uint8, 3)
	for z := range slices {
		slices[z] = make([]uint8, 21*5)
		for i := range slices[z] {
			slices[z][i] = uint8(10 * (i % 21)) // 0..200 along x
		}
	}
	stack, err := NewFractionStack(source, slices)
	if err != nil {
		t.Fatal(err)
	}
	target := axisGrid(40, 3, 2, [3]float64{0.5, 1.5, 0.7}, render.Vec3{X: 0.25, Y: 0.5, Z: 0.3})
	values, err := ResampleFractions(context.Background(), stack, target, FractionResampleOptions{})
	if err != nil {
		t.Fatal(err)
	}
	for x := 0; x < target.Columns; x++ {
		want := 10 * (0.25 + 0.5*float64(x))
		if want > 200 {
			want = 200
		}
		if got := float64(values.Values[x]); math.Abs(got-want) > 1e-4 {
			t.Fatalf("x=%d value %.4f, want %.4f", x, got, want)
		}
	}
	mask, err := ResampleFractionMask(context.Background(), stack, target, 100, FractionResampleOptions{})
	if err != nil {
		t.Fatal(err)
	}
	for z := 0; z < target.Slices; z++ {
		for x := 0; x < target.Columns; x++ {
			if got, want := mask.Masks[z].Get(x, 1), 0.25+0.5*float64(x) >= 10; got != want {
				t.Fatalf("mask x=%d = %v, want %v", x, got, want)
			}
		}
	}
	if mask.Count != target.Rows*target.Slices*(target.Columns-20) {
		t.Fatalf("mask count = %d", mask.Count)
	}
}

func TestResampleLabelsCancellationAndLimits(t *testing.T) {
	grid := axisGrid(64, 64, 64, [3]float64{1, 1, 1}, render.Vec3{})
	src, err := NewLabelVolume(grid)
	if err != nil {
		t.Fatal(err)
	}
	src.Labels[1000] = 1
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := ResampleLabels(ctx, src, grid, LabelGaussian(2)); !errors.Is(err, context.Canceled) {
		t.Fatalf("cancelled err = %v", err)
	}
	mode := NearestLabels()
	mode.MaxVoxels = 1000
	if _, err := ResampleLabels(context.Background(), src, grid, mode); !errors.Is(err, ErrResampleTooLarge) {
		t.Fatalf("budget err = %v", err)
	}
	if _, err := ResampleLabels(context.Background(), src, grid, LabelGaussian(0)); !errors.Is(err, ErrInvalidResample) {
		t.Fatalf("sigma err = %v", err)
	}
	flat := grid
	flat.SliceDir = render.Vec3{X: 1}
	if _, err := ResampleLabels(context.Background(), src, flat, NearestLabels()); !errors.Is(err, ErrInvalidGrid) {
		t.Fatalf("coplanar grid err = %v", err)
	}
	if _, err := NewMaskStack(grid, make([]*RasterMask, 3), 1); !errors.Is(err, ErrInvalidResample) {
		t.Fatalf("mask stack err = %v", err)
	}
}

func BenchmarkResampleLabelMasksSlice(b *testing.B) {
	source := axisGrid(256, 256, 120, [3]float64{1.2, 1.2, 2.5}, render.Vec3{})
	masks := make([]*RasterMask, source.Slices)
	for z := 20; z < 100; z++ {
		masks[z] = NewRasterMask(256, 256)
		for y := 60; y < 200; y++ {
			masks[z].SetRun(y, 50, 210)
		}
	}
	stack, err := NewMaskStack(source, masks, 1)
	if err != nil {
		b.Fatal(err)
	}
	target := GridFromSlice(render.SliceGeometry{
		Origin: render.Vec3{X: -10, Y: -10, Z: 150}, RowDir: render.Vec3{X: 1}, ColDir: render.Vec3{Y: 1},
		Normal: render.Vec3{Z: 1}, RowSpacing: 0.7, ColSpacing: 0.7, Rows: 512, Columns: 512,
	}, 1)
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		if _, err := ResampleLabelMasks(context.Background(), stack, target, NearestLabels()); err != nil {
			b.Fatal(err)
		}
	}
}
