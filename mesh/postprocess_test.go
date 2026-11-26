package mesh

import (
	"context"
	"encoding/json"
	"math"
	"os"
	"testing"
)

type vtkMeshPostFixture struct {
	Sphere struct {
		Dims   [3]int `json:"dims"`
		Radius int    `json:"radius"`
	} `json:"sphere"`
	Smoothing []struct {
		Window     string  `json:"window"`
		PassBand   float64 `json:"pass_band"`
		Iterations int     `json:"iterations"`
		Area       float64 `json:"area"`
		Volume     float64 `json:"volume"`
	} `json:"smoothing"`
	Decimation struct {
		TargetReduction float64 `json:"target_reduction"`
		Triangles       int     `json:"triangles"`
		Area            float64 `json:"area"`
		Volume          float64 `json:"volume"`
		MaxDistance     float64 `json:"max_distance"`
	} `json:"decimation"`
	MaximumError []struct {
		MaximumError float64 `json:"maximum_error"`
		CellsBefore  int     `json:"cells_before"`
		CellsAfter   int     `json:"cells_after"`
	} `json:"maximum_error"`
}

func loadVTKMeshPostFixture(t *testing.T) vtkMeshPostFixture {
	t.Helper()
	data, err := os.ReadFile("../testdata/upstream/vtk/meshpost.json")
	if err != nil {
		t.Fatal(err)
	}
	var f vtkMeshPostFixture
	if err := json.Unmarshal(data, &f); err != nil {
		t.Fatal(err)
	}
	return f
}

// fixtureSphere returns the voxel surface of the fixture sphere (equal to
// vtkSurfaceNets3D without smoothing) and its Marching Cubes surface (as
// vtkFlyingEdges3D).
func fixtureSphere(t *testing.T, f vtkMeshPostFixture) (voxel, smooth *Mesh) {
	t.Helper()
	dims, radius := f.Sphere.Dims, float64(f.Sphere.Radius)
	mask := make([]float32, dims[0]*dims[1]*dims[2])
	field := make([]float32, len(mask))
	for z := 0; z < dims[2]; z++ {
		for y := 0; y < dims[1]; y++ {
			for x := 0; x < dims[0]; x++ {
				i := (z*dims[1]+y)*dims[0] + x
				dx, dy, dz := doubled(x, dims[0]), doubled(y, dims[1]), doubled(z, dims[2])
				field[i] = float32(radius*radius - (dx*dx+dy*dy+dz*dz)/4)
				if vtkInside("sphere", dims, f.Sphere.Radius, [3]int{}, 0, 0, x, y, z) {
					mask[i] = 1
				}
			}
		}
	}
	voxel, err := Extract(context.Background(), &DenseField{Size: dims, Affine: identityAffine, Data: mask}, Options{Method: VoxelFaces, Iso: 0.5})
	if err != nil {
		t.Fatal(err)
	}
	smooth, err = Extract(context.Background(), &DenseField{Size: dims, Affine: identityAffine, Data: field}, Options{Iso: 0, OpenBoundary: true})
	if err != nil {
		t.Fatal(err)
	}
	return voxel, smooth
}

// TestWindowedSincMatchesVTK: the windowed-sinc filter of the voxel sphere
// keeps its volume (no shrinkage) and matches vtkWindowedSincPolyDataFilter's
// area and volume for the Nuttall and Hamming windows.
func TestWindowedSincMatchesVTK(t *testing.T) {
	f := loadVTKMeshPostFixture(t)
	voxel, _ := fixtureSphere(t, f)
	_, voxelVolume := massProperties(voxel)
	windows := map[string]WindowFunction{"nuttall": WindowNuttall, "hamming": WindowHamming}
	for _, want := range f.Smoothing {
		smoothed, err := SmoothWindowedSinc(voxel, SmoothOptions{Iterations: want.Iterations, PassBand: want.PassBand, Window: windows[want.Window]})
		if err != nil {
			t.Fatal(err)
		}
		area, volume := massProperties(smoothed)
		t.Logf("%s %.2f: area %.1f (VTK %.1f), volume %.1f (VTK %.1f, voxels %.0f)", want.Window, want.PassBand, area, want.Area, volume, want.Volume, voxelVolume)
		if !relClose(volume, want.Volume, 0.003) || !relClose(area, want.Area, 0.015) {
			t.Errorf("%s pass band %v: area %.2f volume %.2f, VTK %.2f %.2f", want.Window, want.PassBand, area, volume, want.Area, want.Volume)
		}
		if !relClose(volume, voxelVolume, 0.01) {
			t.Errorf("%s pass band %v shrank the volume to %.1f from %.1f", want.Window, want.PassBand, volume, voxelVolume)
		}
	}
	// Plain Laplacian relaxation of the same strength shrinks the sphere.
	laplacian := voxel
	for i := 0; i < 20; i++ {
		neighbours, start := smoothingNeighbours(laplacian, false)
		next := &Mesh{Vertices: append([][3]float32(nil), laplacian.Vertices...), Triangles: laplacian.Triangles}
		for v := range next.Vertices {
			list := neighbours[start[v]:start[v+1]]
			if len(list) == 0 {
				continue
			}
			var mean [3]float32
			for _, u := range list {
				for axis := 0; axis < 3; axis++ {
					mean[axis] += laplacian.Vertices[u][axis] / float32(len(list))
				}
			}
			next.Vertices[v] = mean
		}
		laplacian = next
	}
	if _, v := massProperties(laplacian); v > 0.97*voxelVolume {
		t.Fatalf("Laplacian relaxation kept %.1f of %.1f; the comparison needs its shrinkage", v, voxelVolume)
	}
}

// pointMeshDistance is the distance of p to the closest triangle of m.
func pointMeshDistance(p [3]float64, m *Mesh) float64 {
	best := math.Inf(1)
	for _, t := range m.Triangles {
		var tri [3][3]float64
		for k := 0; k < 3; k++ {
			v := m.Vertices[t[k]]
			tri[k] = [3]float64{float64(v[0]), float64(v[1]), float64(v[2])}
		}
		best = math.Min(best, pointTriangleDistance(p, tri))
	}
	return best
}

func pointTriangleDistance(p [3]float64, t [3][3]float64) float64 {
	a, b, c := t[0], t[1], t[2]
	ab, ac, ap := sub3(b, a), sub3(c, a), sub3(p, a)
	d1, d2 := dot3(ab, ap), dot3(ac, ap)
	closest := func(q [3]float64) float64 { d := sub3(p, q); return math.Sqrt(dot3(d, d)) }
	lerp := func(x, y [3]float64, s float64) [3]float64 {
		return [3]float64{x[0] + s*(y[0]-x[0]), x[1] + s*(y[1]-x[1]), x[2] + s*(y[2]-x[2])}
	}
	if d1 <= 0 && d2 <= 0 {
		return closest(a)
	}
	bp := sub3(p, b)
	d3, d4 := dot3(ab, bp), dot3(ac, bp)
	if d3 >= 0 && d4 <= d3 {
		return closest(b)
	}
	vc := d1*d4 - d3*d2
	if vc <= 0 && d1 >= 0 && d3 <= 0 {
		return closest(lerp(a, b, d1/(d1-d3)))
	}
	cp := sub3(p, c)
	d5, d6 := dot3(ab, cp), dot3(ac, cp)
	if d6 >= 0 && d5 <= d6 {
		return closest(c)
	}
	vb := d5*d2 - d1*d6
	if vb <= 0 && d2 >= 0 && d6 <= 0 {
		return closest(lerp(a, c, d2/(d2-d6)))
	}
	va := d3*d6 - d5*d4
	if va <= 0 && d4-d3 >= 0 && d5-d6 >= 0 {
		return closest(lerp(b, c, (d4-d3)/((d4-d3)+(d5-d6))))
	}
	denominator := 1 / (va + vb + vc)
	v, w := vb*denominator, vc*denominator
	return closest([3]float64{a[0] + ab[0]*v + ac[0]*w, a[1] + ab[1]*v + ac[1]*w, a[2] + ab[2]*v + ac[2]*w})
}

func maxDistance(from, to *Mesh) float64 {
	farthest := 0.0
	for _, v := range from.Vertices {
		farthest = math.Max(farthest, pointMeshDistance([3]float64{float64(v[0]), float64(v[1]), float64(v[2])}, to))
	}
	return farthest
}

// TestQuadricDecimationMatchesVTK: a 90% reduction of the Marching Cubes
// sphere reaches the target with vtkQuadricDecimation's area and volume, the
// input points no farther from the result than VTK's, and a closed,
// consistently wound surface.
func TestQuadricDecimationMatchesVTK(t *testing.T) {
	f := loadVTKMeshPostFixture(t)
	_, sphere := fixtureSphere(t, f)
	want := f.Decimation
	decimated, report, err := DecimateQuadric(sphere, DecimateOptions{TargetReduction: want.TargetReduction})
	if err != nil {
		t.Fatal(err)
	}
	area, volume := massProperties(decimated)
	distance := maxDistance(sphere, decimated)
	t.Logf("%d -> %d triangles (VTK %d), area %.1f (VTK %.1f), volume %.1f (VTK %.1f), max distance %.3f (VTK %.3f)",
		report.TrianglesBefore, report.TrianglesAfter, want.Triangles, area, want.Area, volume, want.Volume, distance, want.MaxDistance)
	target := int(math.Ceil(float64(len(sphere.Triangles)) * (1 - want.TargetReduction)))
	if report.TrianglesAfter > target+2 || report.TrianglesAfter < target-2 {
		t.Errorf("%d triangles, target %d", report.TrianglesAfter, target)
	}
	if !relClose(area, want.Area, 0.01) || !relClose(volume, want.Volume, 0.01) {
		t.Errorf("area %.2f volume %.2f, VTK %.2f %.2f", area, volume, want.Area, want.Volume)
	}
	if distance > 1.5*want.MaxDistance {
		t.Errorf("max distance %.3f, VTK %.3f", distance, want.MaxDistance)
	}
	requireClosedManifold(t, decimated)
}

// uvSphere is a vtkSphereSource-style sphere: two poles and phi-2 rings of
// theta points, fans at the poles and two triangles per quad.
func uvSphere(radius float64, theta, phi int) *Mesh {
	m := &Mesh{Vertices: [][3]float32{{0, 0, float32(radius)}, {0, 0, float32(-radius)}}}
	ring := func(i, j int) uint32 { return uint32(2 + i*(phi-2) + j) }
	for i := 0; i < theta; i++ {
		t := 2 * math.Pi * float64(i) / float64(theta)
		for j := 1; j < phi-1; j++ {
			p := math.Pi * float64(j) / float64(phi-1)
			m.Vertices = append(m.Vertices, [3]float32{float32(radius * math.Sin(p) * math.Cos(t)), float32(radius * math.Sin(p) * math.Sin(t)), float32(radius * math.Cos(p))})
		}
	}
	for i := 0; i < theta; i++ {
		next := (i + 1) % theta
		m.Triangles = append(m.Triangles, [3]uint32{0, ring(i, 0), ring(next, 0)})
		m.Triangles = append(m.Triangles, [3]uint32{1, ring(next, phi-3), ring(i, phi-3)})
		for j := 0; j < phi-3; j++ {
			a, b, c, d := ring(i, j), ring(next, j), ring(next, j+1), ring(i, j+1)
			m.Triangles = append(m.Triangles, [3]uint32{a, d, c}, [3]uint32{a, c, b})
		}
	}
	return m
}

// After TestQuadricDecimationMaximumError: a maximum error of zero allows no
// collapse, and a tiny one only collapses within it.
func TestQuadricDecimationMaximumError(t *testing.T) {
	f := loadVTKMeshPostFixture(t)
	sphere := uvSphere(1, 70, 70)
	for _, want := range f.MaximumError {
		if len(sphere.Triangles) != want.CellsBefore {
			t.Fatalf("sphere has %d triangles, vtkSphereSource %d", len(sphere.Triangles), want.CellsBefore)
		}
		decimated, report, err := DecimateQuadric(sphere, DecimateOptions{TargetReduction: 0.9, MaxError: want.MaximumError, LimitError: true})
		if err != nil {
			t.Fatal(err)
		}
		t.Logf("maximum error %g: %d triangles (VTK %d), largest collapse error %g", want.MaximumError, report.TrianglesAfter, want.CellsAfter, report.MaxQuadricError)
		if want.MaximumError == 0 {
			if report.TrianglesAfter != want.CellsBefore {
				t.Fatalf("maximum error 0 decimated to %d triangles", report.TrianglesAfter)
			}
			continue
		}
		if report.TrianglesAfter >= want.CellsBefore || report.MaxQuadricError > want.MaximumError {
			t.Fatalf("maximum error %g: %d triangles, largest error %g", want.MaximumError, report.TrianglesAfter, report.MaxQuadricError)
		}
		if d := maxDistance(sphere, decimated); d > 1e-3 {
			t.Fatalf("maximum error %g moved the surface %.2g", want.MaximumError, d)
		}
	}
	unbounded, report, err := DecimateQuadric(sphere, DecimateOptions{TargetReduction: 0.9})
	if err != nil || report.TrianglesAfter > 953 {
		t.Fatalf("unbounded decimation to %d triangles, %v", report.TrianglesAfter, err)
	}
	requireClosedManifold(t, unbounded)
}

func TestDecimatePreservesBoundary(t *testing.T) {
	// An open hemisphere: its rim stays in place.
	sphere := uvSphere(10, 32, 32)
	hemisphere := &Mesh{Vertices: sphere.Vertices}
	for _, tri := range sphere.Triangles {
		keep := true
		for _, v := range tri {
			if sphere.Vertices[v][2] < -0.01 {
				keep = false
			}
		}
		if keep {
			hemisphere.Triangles = append(hemisphere.Triangles, tri)
		}
	}
	hemisphere, _ = Clean(hemisphere, 0)
	rim := map[[3]float32]bool{}
	for e, uses := range edgeUses(hemisphere) {
		if uses == 1 {
			rim[hemisphere.Vertices[e[0]]], rim[hemisphere.Vertices[e[1]]] = true, true
		}
	}
	decimated, report, err := DecimateQuadric(hemisphere, DecimateOptions{TargetReduction: 0.7, PreserveBoundary: true})
	if err != nil || report.TrianglesAfter >= report.TrianglesBefore {
		t.Fatalf("decimation %+v, %v", report, err)
	}
	kept := 0
	for e, uses := range edgeUses(decimated) {
		if uses == 1 {
			for _, v := range e {
				if !rim[decimated.Vertices[v]] {
					t.Fatalf("boundary vertex %v moved", decimated.Vertices[v])
				}
				kept++
			}
		}
	}
	if kept == 0 {
		t.Fatal("the boundary vanished")
	}
}

func TestCleanComponentsAndFillHoles(t *testing.T) {
	// A triangle soup of a cube and a box of two cubes: every triangle has
	// its own corners.
	cube := func(offset float32, size float32, voxels int) *Mesh {
		data := make([]float32, voxels)
		for i := range data {
			data[i] = 1
		}
		field := &DenseField{Size: [3]int{voxels, 1, 1}, Affine: identityAffine, Data: data}
		m, err := Extract(context.Background(), field, Options{Method: VoxelFaces, Iso: 0.5, SkipNormals: true})
		if err != nil {
			t.Fatal(err)
		}
		for i := range m.Vertices {
			for axis := 0; axis < 3; axis++ {
				m.Vertices[i][axis] = m.Vertices[i][axis]*size + offset
			}
		}
		return m
	}
	soup := &Mesh{}
	for _, m := range []*Mesh{cube(0, 1, 1), cube(5, 2, 2)} {
		for _, tri := range m.Triangles {
			base := uint32(len(soup.Vertices))
			for _, v := range tri {
				soup.Vertices = append(soup.Vertices, m.Vertices[v])
			}
			soup.Triangles = append(soup.Triangles, [3]uint32{base, base + 1, base + 2})
		}
	}
	soup.Triangles = append(soup.Triangles, soup.Triangles[0], [3]uint32{0, 0, 1})
	cleaned, report := Clean(soup, 1e-6)
	if len(cleaned.Vertices) != 20 || report.DuplicateTriangles != 1 || report.DegenerateTriangles != 1 || report.MergedVertices != len(soup.Vertices)-20 {
		t.Fatalf("clean: %d vertices, %+v", len(cleaned.Vertices), report)
	}
	requireClosedManifold(t, cleaned)
	component, count := ConnectedComponents(cleaned)
	if count != 2 || len(component) != len(cleaned.Triangles) {
		t.Fatalf("%d components", count)
	}
	largest := LargestComponent(cleaned)
	if _, v := massProperties(largest); math.Abs(v-16) > 1e-5 || len(largest.Vertices) != 12 {
		t.Fatalf("largest component volume %.3f with %d vertices", v, len(largest.Vertices))
	}
	// Open the sphere at a pole and close it again.
	sphere := uvSphere(10, 24, 24)
	open := &Mesh{Vertices: sphere.Vertices}
	for _, tri := range sphere.Triangles {
		if tri[0] != 0 {
			open.Triangles = append(open.Triangles, tri)
		}
	}
	if filled, holes := FillHoles(open, 1); holes != 0 || len(filled.Triangles) != len(open.Triangles) {
		t.Fatalf("a hole of radius %.2f was filled under 1 mm", 10*math.Sin(math.Pi/23))
	}
	filled, holes := FillHoles(open, 5)
	if holes != 1 {
		t.Fatalf("%d holes filled", holes)
	}
	filled, _ = Clean(filled, 0)
	requireClosedManifold(t, filled)
	_, before := massProperties(sphere)
	if _, after := massProperties(filled); after <= 0 || !relClose(after, before, 0.01) {
		t.Fatalf("filled volume %.2f, closed sphere %.2f", after, before)
	}
}
