package render

import (
	"context"
	"fmt"
	"math"
	"testing"
)

// itkImageRegionTest covers continuous bounds and non-finite indices.
// Here the half-open ImageFunction::IsInsideBuffer convention is checked
// after mapping through a reflected, anisotropic, sheared patient grid.
func TestITKImageRegionPatientSpaceBoundaries(t *testing.T) {
	size := [3]int{5, 3, 1}
	grid, err := NewGrid(size, Vec3{X: 12, Y: -8, Z: 20}, [3]float64{2, 4, 8},
		[3]Vec3{{Y: 1}, {Z: 1}, {X: -1, Y: .25, Z: -.5}})
	if err != nil {
		t.Fatal(err)
	}
	patient := func(index [3]float64) Vec3 {
		x, y, z := index[0], index[1], index[2]
		return Vec3{X: 12 - 8*z, Y: -8 + 2*x + 2*z, Z: 20 + 4*y - 4*z}
	}
	for axis, n := range size {
		for _, tc := range []struct {
			name       string
			coordinate float64
			index      int
			inside     bool
		}{
			{"past-lower-edge", -.5 - 1.0/64, -1, false},
			{"lower-edge", -.5, 0, true},
			{"before-upper-edge", float64(n) - .5 - 1.0/64, n - 1, true},
			{"upper-edge", float64(n) - .5, n, false},
			{"past-upper-edge", float64(n) - .5 + 1.0/64, n, false},
		} {
			t.Run(fmt.Sprintf("axis%d/%s", axis, tc.name), func(t *testing.T) {
				ci, wantIndex := [3]float64{1.5, 1.5, 0}, [3]int{2, 2, 0}
				ci[axis], wantIndex[axis] = tc.coordinate, tc.index
				point := patient(ci)
				if got := grid.IndexToPoint(Vec3{X: ci[0], Y: ci[1], Z: ci[2]}); got != point {
					t.Fatalf("IndexToPoint(%v) = %v, want %v", ci, got, point)
				}
				if got, inside := grid.Index(point); got != wantIndex || inside != tc.inside {
					t.Fatalf("Index(%v) = %v, %v, want %v, %v", point, got, inside, wantIndex, tc.inside)
				}
				if inside := grid.IsInside(point); inside != tc.inside {
					t.Fatalf("IsInside(%v) = %v, want %v", point, inside, tc.inside)
				}
			})
		}
		for _, tc := range []struct {
			name  string
			value float64
		}{
			{"NaN", math.NaN()}, {"positive-inf", math.Inf(1)}, {"negative-inf", math.Inf(-1)},
		} {
			t.Run(fmt.Sprintf("axis%d/%s", axis, tc.name), func(t *testing.T) {
				ci := [3]float64{}
				ci[axis] = tc.value
				if grid.IsInsideContinuousIndex(Vec3{X: ci[0], Y: ci[1], Z: ci[2]}) {
					t.Fatalf("non-finite index %v reported inside", ci)
				}
			})
		}
	}

	t.Run("empty-axis", func(t *testing.T) {
		empty := grid
		empty.Size[1] = 0
		if err := empty.Validate(); err != nil {
			t.Fatal(err)
		}
		if empty.IsInside(empty.Origin) {
			t.Fatal("the origin lies inside an empty grid")
		}
	})
}

// itkImageToImageToleranceTest separates direction and coordinate agreement.
// Grid.Congruent additionally bounds spatial error over the reference grid.
func TestITKImageGeometryToleranceAtPatientCorners(t *testing.T) {
	base, err := NewGrid([3]int{9, 9, 3}, Vec3{X: 3, Y: -5, Z: 19}, [3]float64{.5, 2, 1},
		[3]Vec3{{Y: 1}, {Z: 1}, {X: 1}})
	if err != nil {
		t.Fatal(err)
	}
	const coordinateTol = .125 // 1/16 mm on this grid
	for _, tc := range []struct {
		name      string
		change    func(*Grid)
		congruent bool
	}{
		{"inclusive-origin-bound", func(g *Grid) { g.Origin.Y += 1.0 / 16 }, true},
		{"origin-beyond-bound", func(g *Grid) { g.Origin.Y += 5.0 / 64 }, false},
		{"diagonal-origin-error", func(g *Grid) { g.Origin.Y += 3.0 / 64; g.Origin.Z += 3.0 / 64 }, false},
		{"inclusive-far-edge-bound", func(g *Grid) { g.Spacing[0] += 1.0 / 128 }, true},
		{"diagonal-far-corner-error", func(g *Grid) { g.Spacing[0] += 1.0 / 128; g.Spacing[1] += 1.0 / 128 }, false},
		{"zero-error-at-last-index-but-not-first", func(g *Grid) { g.Origin.Y -= .125; g.Spacing[0] += 1.0 / 64 }, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			other := base
			tc.change(&other)
			if got := base.SameGeometry(other, coordinateTol, DefaultGridDirectionTolerance); got != tc.congruent {
				t.Fatalf("SameGeometry = %v, want %v", got, tc.congruent)
			}
		})
	}

	t.Run("direction-bound-even-when-centres-coincide", func(t *testing.T) {
		other := base
		other.Direction[2].X = 17.0 / 16
		other.Spacing[2] = 16.0 / 17
		if base.SameGeometry(other, coordinateTol, 1.0/32) || !base.SameGeometry(other, coordinateTol, 1.0/16) {
			t.Fatal("independent direction tolerance was not applied inclusively")
		}
	})

	t.Run("reference-extent-bounds-drift", func(t *testing.T) {
		cropped := base
		cropped.Size[0] = 2
		cropped.Spacing[0] += 1.0 / 64
		if !cropped.Congruent(base, coordinateTol, DefaultGridDirectionTolerance) || base.Congruent(cropped, coordinateTol, DefaultGridDirectionTolerance) {
			t.Fatal("congruence must bound drift over the receiver's size")
		}
		if cropped.SameGeometry(base, coordinateTol, DefaultGridDirectionTolerance) {
			t.Fatal("different reference extents cannot share a same-geometry fast path")
		}
	})
}

// itkBinShrinkImageFilterTest2 independently varies the shrink factor on each
// axis and checks the physical location of every output sample. A calibrated
// scalar ramp through an oblique 3D stack gives the same independent oracle.
func TestITKBinShrinkMixedFactorsPreservePatientCentres(t *testing.T) {
	geometry := mprCorpusGeometry{
		origin: Vec3{X: 17, Y: -28, Z: 36},
		right:  Vec3{X: .6, Y: .8},
		down:   Vec3{X: -.64, Y: .48, Z: .6},
		normal: Vec3{X: .48, Y: -.36, Z: .8},
	}
	source, err := BuildVolume(mprCorpusStack(geometry, true, true))
	if err != nil {
		t.Fatal(err)
	}
	defer source.Close()
	check := func(t *testing.T, factors, effective [3]int) {
		t.Helper()
		shrunk, err := BinShrinkVolumeContext(context.Background(), source, factors)
		if err != nil {
			t.Fatal(err)
		}
		defer shrunk.Close()
		lease, err := shrunk.AcquireSnapshot()
		if err != nil {
			t.Fatal(err)
		}
		defer lease.Release()
		snapshot, err := lease.Snapshot()
		if err != nil {
			t.Fatal(err)
		}
		descriptor, err := snapshot.Descriptor()
		if err != nil {
			t.Fatal(err)
		}
		x, y, z := effective[0], effective[1], effective[2]
		wantSize := [3]uint32{uint32(mprCorpusCols / x), uint32(mprCorpusRows / y), uint32(mprCorpusDepth / z)}
		if descriptor.Dimensions != wantSize {
			t.Fatalf("dimensions = %v, want %v", descriptor.Dimensions, wantSize)
		}
		wantSpacing := [3]float64{mprCorpusColSpacing * float64(x), mprCorpusRowSpacing * float64(y), mprCorpusSliceSpacing * float64(z)}
		for axis := range wantSpacing {
			assertITKGridNumber(t, "spacing", descriptor.SpacingMM[axis], wantSpacing[axis], 1e-9)
		}
		for k := range wantSize[2] {
			for j := range wantSize[1] {
				for i := range wantSize[0] {
					out := Vec3{X: float64(i), Y: float64(j), Z: float64(k)}
					centre := Vec3{
						X: float64(int(i)*x) + float64(x-1)/2,
						Y: float64(int(j)*y) + float64(y-1)/2,
						Z: float64(int(k)*z) + float64(z-1)/2,
					}
					wantPatient := geometry.patient(centre)
					assertITKGridPoint(t, "block centre", descriptor.IndexToPatientLPS.TransformPoint(out), wantPatient)
					assertITKGridPoint(t, "block inverse", descriptor.PatientLPSToIndex.TransformPoint(wantPatient), out)
					got, ok := snapshot.ModalityAt(i, j, k)
					if !ok {
						t.Fatalf("missing output sample at %v", out)
					}
					assertITKGridNumber(t, fmt.Sprintf("block average at %v", out), got, mprCorpusValue(centre), 1e-6)
				}
			}
		}
	}
	for x := 1; x <= 4; x++ {
		for y := 1; y <= 4; y++ {
			for z := 1; z <= 4; z++ {
				factors := [3]int{x, y, z}
				t.Run(fmt.Sprintf("%dx%dx%d", x, y, z), func(t *testing.T) {
					check(t, factors, factors)
				})
			}
		}
	}

	// The DICOM volume adapter clamps oversized factors to the axis length
	// and nonpositive factors to one.
	for _, tc := range []struct {
		name               string
		factors, effective [3]int
	}{
		{"keep-columns-and-rows", [3]int{0, -2, 2}, [3]int{1, 1, 2}},
		{"collapse-columns", [3]int{20, 2, 3}, [3]int{mprCorpusCols, 2, 3}},
		{"collapse-rows", [3]int{2, 20, 3}, [3]int{2, mprCorpusRows, 3}},
		{"collapse-depth", [3]int{2, 2, 20}, [3]int{2, 2, mprCorpusDepth}},
		{"collapse-volume", [3]int{20, 20, 20}, [3]int{mprCorpusCols, mprCorpusRows, mprCorpusDepth}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			check(t, tc.factors, tc.effective)
		})
	}
}

// The itkResampleImageTest family checks mapping through output physical
// coordinates and assigning the outside value. Here the reference output grid
// is derived from a DICOM acquisition with oblique axes, tilt and a missing
// slice. Both the device-facing mapping and the headless resample must use it.
func TestITKRegularizeObliqueMissingSliceGrid(t *testing.T) {
	for _, tc := range []struct {
		name             string
		xAxis, yAxis, n  Vec3
		driftX, driftY   float64 // millimetres of lateral shift per mm along n
		targetX, targetY float64 // lower lateral bounds in acquired coordinates
	}{
		{"row-positive-column-negative", Vec3{Y: .6, Z: .8}, Vec3{X: -1}, Vec3{Y: -.8, Z: .6}, .3, -.2, 0, -1.6},
		{"row-negative-column-positive", Vec3{X: .6, Y: .8}, Vec3{X: -.64, Y: .48, Z: .6}, Vec3{X: .48, Y: -.36, Z: .8}, -.3, .2, -2.4, 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			const cols, rows = 6, 4
			origin := Vec3{X: -37, Y: 23, Z: 11}
			patient := func(x, y, z float64) Vec3 {
				return origin.Add(tc.xAxis.Scale(x)).Add(tc.yAxis.Scale(y)).Add(tc.n.Scale(z))
			}
			stack := &Stack{PixelSpacing: []float64{1, 2}}
			// Spatial sorting must put the pixels and geometry in agreement.
			for _, distance := range []int{8, 2, 6, 0} {
				pixels := make([]byte, cols*rows)
				for y := range rows {
					for x := range cols {
						pixels[y*cols+x] = byte(40 + 10*x + 15*y + 4*distance)
					}
				}
				frame := volumeTestFrame(rows, cols, pixels, distance)
				z := float64(distance)
				p := patient(tc.driftX*z, tc.driftY*z, z)
				frame.ImagePosition = []float64{p.X, p.Y, p.Z}
				frame.ImageOrientation = []float64{tc.xAxis.X, tc.xAxis.Y, tc.xAxis.Z, tc.yAxis.X, tc.yAxis.Y, tc.yAxis.Z}
				stack.Frames = append(stack.Frames, frame)
			}
			volume, err := BuildVolume(stack)
			if err != nil {
				t.Fatal(err)
			}
			defer volume.Close()
			geometry := volume.Geometry()
			if !geometry.RequiresResampling || !geometry.GantryTilted || !geometry.MissingSlices {
				t.Fatalf("expected a tilted acquisition with a gap, got %v", geometry.Issues)
			}
			plan, err := PlanVolumeRegularization(geometry)
			if err != nil {
				t.Fatal(err)
			}
			values, grid, err := volume.ResampleRegularGridContext(context.Background(), RegularGridLinear)
			if err != nil {
				t.Fatal(err)
			}
			wantSize := [3]int{8, 6, 5}
			if plan.SourceDimensions != [3]uint32{cols, rows, 4} || plan.Target != grid || grid.Dimensions != wantSize {
				t.Fatalf("source %v, planned %v, resampled %v", plan.SourceDimensions, plan.Target.Dimensions, grid.Dimensions)
			}
			if len(plan.SliceOrigins) != wantSize[2] || len(values) != wantSize[0]*wantSize[1]*wantSize[2] {
				t.Fatalf("regularization produced %d slice origins and %d samples", len(plan.SliceOrigins), len(values))
			}
			assertITKGridPoint(t, "column step", plan.ColumnStep, Vec3{X: 1})
			assertITKGridPoint(t, "row step", plan.RowStep, Vec3{Y: 1})
			for z, sourceZ := range []float64{0, 1, 1.5, 2, 3} {
				distance := 2 * float64(z)
				sx := (tc.targetX - tc.driftX*distance) / 2
				sy := tc.targetY - tc.driftY*distance
				assertITKGridPoint(t, "acquired-plane origin", plan.SliceOrigins[z], Vec3{X: sx, Y: sy, Z: sourceZ})
				for y := range wantSize[1] {
					for x := range wantSize[0] {
						output := Vec3{X: float64(x), Y: float64(y), Z: float64(z)}
						wantPoint := patient(tc.targetX+2*output.X, tc.targetY+output.Y, distance)
						assertITKGridPoint(t, "regular point", grid.VoxelToPatientAffine.TransformPoint(output), wantPoint)
						assertITKGridPoint(t, "regular inverse", grid.PatientToVoxelAffine.TransformPoint(wantPoint), output)
						ix, iy := sx+output.X, sy+output.Y
						// Grid centres outside the source's half-voxel footprint get zero.
						// Those in the outer half voxel repeat the edge sample.
						want := 0.0
						if ix >= -.5 && ix < cols-.5 && iy >= -.5 && iy < rows-.5 {
							want = 40 + 10*max(0, min(cols-1, ix)) + 15*max(0, min(rows-1, iy)) + 4*distance
						}
						got := values[(z*wantSize[1]+y)*wantSize[0]+x]
						assertITKGridNumber(t, fmt.Sprintf("regular sample at %v", output), float64(got), want, 1e-5)
					}
				}
			}
		})
	}
}

func assertITKGridPoint(t *testing.T, label string, got, want Vec3) {
	t.Helper()
	if distance := got.Sub(want).Length(); !(distance <= 1e-9) {
		t.Fatalf("%s = %v, want %v", label, got, want)
	}
}

func assertITKGridNumber(t *testing.T, label string, got, want, tolerance float64) {
	t.Helper()
	if !(math.Abs(got-want) <= tolerance) {
		t.Fatalf("%s = %g, want %g ± %g", label, got, want, tolerance)
	}
}
