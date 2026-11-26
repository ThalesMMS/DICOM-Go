package mesh

import (
	"context"
	"errors"
	"math"
	"math/rand"
	"reflect"
	"testing"

	"github.com/ThalesMMS/dicom-go/render"
	"github.com/ThalesMMS/dicom-go/roi"
	"github.com/ThalesMMS/dicom-go/seg"
)

func labelField(size [3]int, spacing [3]float64, label func(x, y, z int) uint32) *DenseLabelField {
	f := &DenseLabelField{Size: size, Affine: GridAffine(render.Vec3{X: 10, Y: -5, Z: 3}, IdentityAxes, spacing)}
	f.Labels = make([]uint32, size[0]*size[1]*size[2])
	for z := 0; z < size[2]; z++ {
		for y := 0; y < size[1]; y++ {
			for x := 0; x < size[0]; x++ {
				f.Labels[(z*size[1]+y)*size[0]+x] = label(x, y, z)
			}
		}
	}
	return f
}

func voxelCounts(f *DenseLabelField) map[uint32]int {
	counts := map[uint32]int{}
	for _, label := range f.Labels {
		if label != 0 {
			counts[label]++
		}
	}
	return counts
}

// signedVolume is the divergence-theorem volume of a closed mesh: positive
// when its triangles wind counter-clockwise seen from outside.
func signedVolume(m *Mesh) float64 {
	var volume float64
	for _, t := range m.Triangles {
		a, b, c := m.Vertex(int(t[0])), m.Vertex(int(t[1])), m.Vertex(int(t[2]))
		volume += a.Dot(b.Cross(c)) / 6
	}
	return volume
}

// requireClosed checks that every directed edge of m is matched by the same
// number of reversed uses: a closed, consistently oriented surface.
func requireClosed(t *testing.T, name string, m *Mesh) {
	t.Helper()
	if err := m.Validate(); err != nil {
		t.Fatalf("%s: %v", name, err)
	}
	balance := map[[2]uint32]int{}
	for _, tri := range m.Triangles {
		for i := 0; i < 3; i++ {
			a, b := tri[i], tri[(i+1)%3]
			if a < b {
				balance[[2]uint32{a, b}]++
			} else {
				balance[[2]uint32{b, a}]--
			}
		}
	}
	for edge, count := range balance {
		if count != 0 {
			t.Fatalf("%s: edge %v is used %+d times more in one direction", name, edge, count)
		}
	}
}

func TestSurfaceNetsUnsmoothedEnclosesTheVoxelsExactly(t *testing.T) {
	spacing := [3]float64{0.5, 1, 2}
	voxel := spacing[0] * spacing[1] * spacing[2]
	// Two labels sharing a face, a third touching the grid boundary and a
	// single voxel.
	f := labelField([3]int{9, 8, 7}, spacing, func(x, y, z int) uint32 {
		switch {
		case x >= 1 && x <= 3 && y >= 1 && y <= 4 && z >= 1 && z <= 4:
			return 1
		case x >= 4 && x <= 6 && y >= 1 && y <= 4 && z >= 1 && z <= 4:
			return 2
		case x == 8 && z >= 5:
			return 7
		case x == 1 && y == 7 && z == 6:
			return 3
		}
		return 0
	})
	surface, err := ExtractLabels(context.Background(), f, SurfaceNetsOptions{Iterations: -1})
	if err != nil {
		t.Fatal(err)
	}
	if got := surface.Labels; len(got) != 4 || got[0] != 1 || got[3] != 7 {
		t.Fatalf("labels %v", got)
	}
	shared := 0
	for i, pair := range surface.Pairs {
		if pair[0] >= pair[1] {
			t.Fatalf("pair %d = %v, want ascending", i, pair)
		}
		if pair == [2]uint32{1, 2} {
			shared++
		}
	}
	// The 1|2 boundary is one 4x4 face of quads, built once.
	if shared != 2*4*4 {
		t.Fatalf("%d triangles between labels 1 and 2, want %d", shared, 2*4*4)
	}
	for label, count := range voxelCounts(f) {
		m := surface.LabelMesh(label)
		requireClosed(t, "label", m)
		if got, want := signedVolume(m), float64(count)*voxel; math.Abs(got-want) > 1e-3 {
			t.Errorf("label %d volume %.4f, want %d voxels = %.4f", label, got, count, want)
		}
	}
	if surface.LabelMesh(99) != nil || surface.LabelMesh(0) != nil {
		t.Fatal("a missing label or the background has a mesh")
	}
}

// After TestSurfaceNets3DNormalsConsistency: the smoothed surfaces of two
// touching spheres stay closed, outward and near their voxel volume, and
// every vertex normal points away from its sphere's centre.
func TestSurfaceNetsSmoothedNormalsAreConsistent(t *testing.T) {
	centres := [2][3]float64{{10, 12, 11}, {19, 12, 11}}
	f := labelField([3]int{30, 24, 22}, [3]float64{1, 1, 1}, func(x, y, z int) uint32 {
		for i, c := range centres {
			dx, dy, dz := float64(x)-c[0], float64(y)-c[1], float64(z)-c[2]
			if dx*dx+dy*dy+dz*dz <= 5.5*5.5 {
				return uint32(i + 1)
			}
		}
		return 0
	})
	surface, err := ExtractLabels(context.Background(), f, SurfaceNetsOptions{})
	if err != nil {
		t.Fatal(err)
	}
	counts := voxelCounts(f)
	origin := render.Vec3{X: 10, Y: -5, Z: 3}
	for i, c := range centres {
		label := uint32(i + 1)
		m := surface.LabelMesh(label)
		requireClosed(t, "sphere", m)
		volume := signedVolume(m)
		t.Logf("sphere %d: smoothed volume %.1f for %d voxels", label, volume, counts[label])
		if want := float64(counts[label]); math.Abs(volume-want)/want > 0.03 {
			t.Errorf("sphere %d volume %.1f, %d voxels", label, volume, counts[label])
		}
		centre := origin.Add(render.Vec3{X: c[0], Y: c[1], Z: c[2]})
		for v, normal := range m.Normals {
			n := render.Vec3{X: float64(normal[0]), Y: float64(normal[1]), Z: float64(normal[2])}
			if n.Dot(m.Vertex(v).Sub(centre)) <= 0 {
				t.Fatalf("sphere %d vertex %d normal %v points inward", label, v, normal)
			}
		}
	}
}

// Even and odd dimensions, single-voxel axes and random labels all yield
// closed label surfaces with exact unsmoothed volumes.
func TestSurfaceNetsOddEvenAndDegenerateDimensions(t *testing.T) {
	random := rand.New(rand.NewSource(4))
	for _, size := range [][3]int{{1, 1, 1}, {2, 3, 4}, {5, 4, 3}, {1, 7, 2}, {6, 6, 1}, {7, 5, 6}} {
		f := labelField(size, [3]float64{1, 1, 1}, func(x, y, z int) uint32 { return uint32(random.Intn(4)) })
		for _, iterations := range []int{-1, 0} {
			surface, err := ExtractLabels(context.Background(), f, SurfaceNetsOptions{Iterations: iterations})
			if err != nil {
				t.Fatalf("%v: %v", size, err)
			}
			for label, count := range voxelCounts(f) {
				m := surface.LabelMesh(label)
				requireClosed(t, "random", m)
				if iterations < 0 {
					if got := signedVolume(m); math.Abs(got-float64(count)) > 1e-3 {
						t.Fatalf("%v label %d volume %.3f, want %d", size, label, got, count)
					}
				}
			}
		}
	}
}

func TestSurfaceNetsLabelSelectionMirrorAndBudget(t *testing.T) {
	f := labelField([3]int{6, 6, 6}, [3]float64{1, 1, 1}, func(x, y, z int) uint32 {
		if x >= 1 && x <= 2 && y >= 1 && y <= 4 && z >= 1 && z <= 4 {
			return 4
		}
		if x >= 3 && x <= 4 && y >= 1 && y <= 4 && z >= 1 && z <= 4 {
			return 5
		}
		return 0
	})
	only, err := ExtractLabels(context.Background(), f, SurfaceNetsOptions{Labels: []uint32{5}, Iterations: -1})
	if err != nil {
		t.Fatal(err)
	}
	if len(only.Labels) != 1 || only.Labels[0] != 5 || only.LabelMesh(4) != nil {
		t.Fatalf("selection kept %v", only.Labels)
	}
	// A left-handed frame (x mirrored) keeps the surfaces outward.
	f.Affine = GridAffine(render.Vec3{}, [3]render.Vec3{{X: -1}, {Y: 1}, {Z: 1}}, [3]float64{1, 1, 1})
	mirrored, err := ExtractLabels(context.Background(), f, SurfaceNetsOptions{Iterations: -1})
	if err != nil {
		t.Fatal(err)
	}
	if got := signedVolume(mirrored.LabelMesh(4)); math.Abs(got-32) > 1e-6 {
		t.Fatalf("mirrored label volume %.3f, want 32", got)
	}
	_, err = ExtractLabels(context.Background(), f, SurfaceNetsOptions{MaxTriangles: 10})
	var budgetErr *BudgetError
	if !errors.Is(err, ErrBudgetExceeded) || !errors.As(err, &budgetErr) {
		t.Fatalf("budget error %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := ExtractLabels(ctx, f, SurfaceNetsOptions{}); !errors.Is(err, context.Canceled) {
		t.Fatalf("cancelled extraction error %v", err)
	}
	if _, err := ExtractLabels(context.Background(), f, SurfaceNetsOptions{Relaxation: 2}); !errors.Is(err, ErrInvalidField) {
		t.Fatalf("relaxation 2 error %v", err)
	}
}

func TestSurfaceNetsBudgetsCompleteLabelOutput(t *testing.T) {
	field := labelField([3]int{1, 1, 1}, [3]float64{1, 1, 1}, func(_, _, _ int) uint32 { return 1 })
	// A unit voxel has eight vertices, twelve triangles, twelve pairs and
	// one label. Each vertex and triangle is three 32-bit values.
	const outputBytes = 8*12 + 12*12 + 12*8 + 4
	if _, err := ExtractLabels(context.Background(), field, SurfaceNetsOptions{MaxBytes: outputBytes}); err != nil {
		t.Fatalf("exact output budget: %v", err)
	}
	surface, err := ExtractLabels(context.Background(), field, SurfaceNetsOptions{MaxBytes: outputBytes - 1})
	var budgetErr *BudgetError
	if surface != nil || !errors.As(err, &budgetErr) || budgetErr.Bytes != outputBytes || budgetErr.MaxBytes != outputBytes-1 {
		t.Fatalf("one byte below required output: surface returned %t, error %v", surface != nil, err)
	}
}

func TestSurfaceNetsRejectsUnsafeGridDimensions(t *testing.T) {
	for _, dims := range [][3]int{{math.MaxInt, 1, 1}, {1, 1, math.MaxInt}, {65536, 65536, 1}} {
		field := &DenseLabelField{
			Size:   dims,
			Affine: GridAffine(render.Vec3{}, IdentityAxes, [3]float64{1, 1, 1}),
		}
		if _, err := ExtractLabels(context.Background(), field, SurfaceNetsOptions{}); !errors.Is(err, ErrInvalidField) {
			t.Fatalf("unrepresentable dimensions %v: %v", dims, err)
		}
	}
}

type cancelAfterSurfaceNetsChecks struct {
	context.Context
	checks int
}

func (c *cancelAfterSurfaceNetsChecks) Err() error {
	if c.checks == 0 {
		return context.Canceled
	}
	c.checks--
	return nil
}

func TestSurfaceNetsCancelsBeforeSmoothing(t *testing.T) {
	field := labelField([3]int{4, 5, 3}, [3]float64{1, 1, 1}, func(_, _, _ int) uint32 { return 1 })
	// Permit the admission check only: the planes are built under the slab
	// scheduler's derived context (cancelled through Done, which this one
	// never closes), so the next check is the smoothing's. Cancellation after
	// building the mesh must prevent even partial output.
	ctx := &cancelAfterSurfaceNetsChecks{Context: context.Background(), checks: 1}
	surface, err := ExtractLabels(ctx, field, SurfaceNetsOptions{})
	if surface != nil || !errors.Is(err, context.Canceled) {
		t.Fatalf("surface returned %t, error %v", surface != nil, err)
	}
}

func TestLabelFieldsFromMasksAndSEG(t *testing.T) {
	grid := roi.Grid{
		Columns: 6, Rows: 5, Slices: 4,
		Origin: render.Vec3{X: -20, Y: 4, Z: 100},
		RowDir: render.Vec3{X: 1}, ColDir: render.Vec3{Y: 1}, SliceDir: render.Vec3{Z: 1},
		ColSpacing: 0.8, RowSpacing: 0.8, SliceSpacing: 2.5,
	}
	square := func(x0, x1, y0, y1 int) *roi.RasterMask {
		m := roi.NewRasterMask(6, 5)
		for y := y0; y < y1; y++ {
			m.SetRun(y, x0, x1)
		}
		return m
	}
	masks := map[uint32][]*roi.RasterMask{
		1: {nil, square(1, 4, 1, 4), square(1, 4, 1, 4)},
		2: {nil, square(3, 5, 1, 3), nil, square(0, 2, 0, 2)},
	}
	field, overlaps, err := LabelFieldFromMasks(grid, masks)
	if err != nil {
		t.Fatal(err)
	}
	if overlaps != 2 {
		t.Fatalf("overlaps = %d, want 2 (label 2 over label 1 on slice 1)", overlaps)
	}
	if got := field.Labels[(1*5+1)*6+3]; got != 2 {
		t.Fatalf("overlap voxel label %d, want the higher label 2", got)
	}
	surface, err := ExtractLabels(context.Background(), field, SurfaceNetsOptions{Iterations: -1})
	if err != nil {
		t.Fatal(err)
	}
	voxel := 0.8 * 0.8 * 2.5
	counts := voxelCounts(field)
	for label, count := range counts {
		if got := signedVolume(surface.LabelMesh(label)); math.Abs(got-float64(count)*voxel) > 1e-3 {
			t.Fatalf("label %d volume %.3f, want %.3f", label, got, float64(count)*voxel)
		}
	}
	// The lowest corner of label 2 on slice 3 sits half a voxel before the
	// centre of voxel (0, 0, 3).
	m := surface.LabelMesh(2)
	corner := grid.VoxelCenter(0, 0, 3).Sub(render.Vec3{X: 0.4, Y: 0.4, Z: 1.25})
	found := false
	for i := range m.Vertices {
		if m.Vertex(i).Sub(corner).Length() < 1e-4 {
			found = true
		}
	}
	if !found {
		t.Fatalf("no vertex at the patient corner %v", corner)
	}
	resampled := &seg.Resampled{Grid: grid, Segments: []seg.ResampledSegment{
		{Number: 1, Masks: masks[1]}, {Number: 2, Masks: masks[2]},
	}}
	fromSEG, segOverlaps, err := LabelFieldFromSEG(resampled)
	if err != nil || segOverlaps != overlaps || len(fromSEG.Labels) != len(field.Labels) {
		t.Fatalf("LabelFieldFromSEG = %v, %d, %v", fromSEG != nil, segOverlaps, err)
	}
	for i := range field.Labels {
		if fromSEG.Labels[i] != field.Labels[i] {
			t.Fatalf("SEG label %d differs at %d", fromSEG.Labels[i], i)
		}
	}
	irregular := grid
	irregular.SliceOffsets = []float64{0, 2.5, 5, 9}
	if _, _, err := LabelFieldFromMasks(irregular, masks); !errors.Is(err, ErrInvalidField) {
		t.Fatalf("irregular grid error %v", err)
	}
	regular := grid
	regular.SliceOffsets = []float64{5, 7.5, 10, 12.5}
	shifted, _, err := LabelFieldFromMasks(regular, masks)
	if err != nil {
		t.Fatal(err)
	}
	if got := shifted.Affine[11]; math.Abs(got-105) > 1e-9 {
		t.Fatalf("offset origin z %.3f, want 105", got)
	}
}

func twoSpheresLabelField128() *DenseLabelField {
	return labelField([3]int{128, 128, 128}, [3]float64{1, 1, 1}, func(x, y, z int) uint32 {
		for i, c := range [2][3]float64{{44, 64, 64}, {84, 64, 64}} {
			dx, dy, dz := float64(x)-c[0], float64(y)-c[1], float64(z)-c[2]
			if dx*dx+dy*dy+dz*dz <= 30*30 {
				return uint32(i + 1)
			}
		}
		return 0
	})
}

func BenchmarkSurfaceNetsTwoSpheres128(b *testing.B) {
	f := twoSpheresLabelField128()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		surface, err := ExtractLabels(context.Background(), f, SurfaceNetsOptions{})
		if err != nil {
			b.Fatal(err)
		}
		b.ReportMetric(float64(len(surface.Mesh.Triangles)), "triangles")
	}
}

// BenchmarkSurfaceNetsBuildTwoSpheres128 times the surface construction
// alone, without smoothing.
func BenchmarkSurfaceNetsBuildTwoSpheres128(b *testing.B) {
	f := twoSpheresLabelField128()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		surface, err := ExtractLabels(context.Background(), f, SurfaceNetsOptions{Iterations: -1})
		if err != nil {
			b.Fatal(err)
		}
		b.ReportMetric(float64(len(surface.Mesh.Triangles)), "triangles")
	}
}

// TestSurfaceNetsMatchesVTKSurfaceNets3D: without smoothing, each label's
// surface equals vtkSurfaceNets3D's on the fixture phantoms (triangle count,
// area and volume), the multi-label phantom's labels extracted together from
// one label volume.
func TestSurfaceNetsMatchesVTKSurfaceNets3D(t *testing.T) {
	fixture := loadVTKMeshFixture(t)
	compared := 0
	for _, p := range fixture.Phantoms {
		label := uint32(p.Label)
		if label == 0 {
			label = 1
		}
		f := labelField(p.Dims, [3]float64{1, 1, 1}, func(x, y, z int) uint32 {
			if p.Label == 0 {
				if vtkInside(p.Name, p.Dims, p.Radius, p.SemiAxes, p.Half, 0, x, y, z) {
					return 1
				}
				return 0
			}
			for l := 1; l <= 3; l++ {
				if vtkInside(p.Name, p.Dims, p.Radius, p.SemiAxes, p.Half, l, x, y, z) {
					return uint32(l)
				}
			}
			return 0
		})
		f.Affine = identityAffine
		surface, err := ExtractLabels(context.Background(), f, SurfaceNetsOptions{Iterations: -1})
		if err != nil {
			t.Fatal(err)
		}
		m := surface.LabelMesh(label)
		area, volume := massProperties(m)
		want := p.SurfaceNets
		if len(m.Triangles) != want.Triangles || !relClose(area, want.Area, 1e-9) || !relClose(volume, want.Volume, 1e-9) {
			t.Fatalf("%s label %d: %d triangles, area %g, volume %g; vtkSurfaceNets3D %+v", p.Name, label, len(m.Triangles), area, volume, want)
		}
		compared++
	}
	t.Logf("compared %d phantoms", compared)
}

// The planes are built in parallel and joined in order: the surface is the
// same, vertex for vertex and triangle for triangle, for any worker count,
// including sparse rows trimmed to their non-zero voxels.
func TestSurfaceNetsOutputDoesNotDependOnWorkers(t *testing.T) {
	random := rand.New(rand.NewSource(77))
	sparse := labelField([3]int{37, 29, 41}, [3]float64{0.7, 1, 1.3}, func(x, y, z int) uint32 {
		if random.Float64() < 0.04 {
			return uint32(1 + random.Intn(3))
		}
		return 0
	})
	dense := labelField([3]int{23, 19, 31}, [3]float64{1, 1, 1}, func(x, y, z int) uint32 { return uint32(random.Intn(4)) })
	spheres := labelField([3]int{40, 33, 35}, [3]float64{1, 1, 1}, func(x, y, z int) uint32 {
		for i, c := range [2][3]float64{{14, 16, 17}, {25, 16, 17}} {
			dx, dy, dz := float64(x)-c[0], float64(y)-c[1], float64(z)-c[2]
			if dx*dx+dy*dy+dz*dz <= 6*6 {
				return uint32(i + 1)
			}
		}
		return 0
	})
	for name, f := range map[string]*DenseLabelField{"sparse": sparse, "dense": dense, "spheres": spheres} {
		for _, iterations := range []int{-1, 0} {
			want, err := ExtractLabels(context.Background(), f, SurfaceNetsOptions{Iterations: iterations, Workers: 1})
			if err != nil {
				t.Fatal(err)
			}
			for _, workers := range []int{2, 3, 8, 16} {
				got, err := ExtractLabels(context.Background(), f, SurfaceNetsOptions{Iterations: iterations, Workers: workers})
				if err != nil {
					t.Fatal(err)
				}
				if !reflect.DeepEqual(got, want) {
					t.Fatalf("%s, %d iterations: %d workers built %d vertices, %d triangles, %d unresolved; one worker %d, %d, %d",
						name, iterations, workers, len(got.Mesh.Vertices), len(got.Mesh.Triangles), got.UnresolvedCells,
						len(want.Mesh.Vertices), len(want.Mesh.Triangles), want.UnresolvedCells)
				}
			}
			for label, count := range voxelCounts(f) {
				m := want.LabelMesh(label)
				requireClosed(t, name, m)
				if iterations < 0 {
					voxel := f.Affine[0] * f.Affine[5] * f.Affine[10]
					if got := signedVolume(m); math.Abs(got-float64(count)*voxel) > 1e-6*float64(count) {
						t.Fatalf("%s label %d volume %g, want %g", name, label, got, float64(count)*voxel)
					}
				}
			}
		}
	}
}
