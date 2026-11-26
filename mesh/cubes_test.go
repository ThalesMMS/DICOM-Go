package mesh

import (
	"context"
	"errors"
	"math"
	"math/rand"
	"reflect"
	"testing"

	"github.com/ThalesMMS/dicom-go/render"
)

// TestCaseTableProperties checks the generated table: empty and full cases
// have no triangles, a lone corner (inside or outside) is one triangle,
// every triangle uses three distinct crossed edges, every crossed edge of a
// case is used, and the triangles of a case form closed loops: each crossed
// edge pair of the fan is matched by the loop.
func TestCaseTableProperties(t *testing.T) {
	if n := len(mcTable.triangles(0)) + len(mcTable.triangles(255)); n != 0 {
		t.Fatalf("empty/full cases have %d edge entries", n)
	}
	for corner := 0; corner < 8; corner++ {
		if n := mcTriangleCount[1<<corner]; n != 1 {
			t.Fatalf("lone inside corner %d: %d triangles", corner, n)
		}
		if n := mcTriangleCount[255^(1<<corner)]; n != 1 {
			t.Fatalf("lone outside corner %d: %d triangles", corner, n)
		}
	}
	total := 0
	for c := 0; c < 256; c++ {
		crossed := map[uint8]bool{}
		for e, edge := range mcTable.edges {
			a, b := int(edge.base), int(edge.base)|1<<edge.axis
			if c>>a&1 != c>>b&1 {
				crossed[uint8(e)] = true
			}
		}
		used := map[uint8]bool{}
		tris := mcTable.triangles(c)
		for i := 0; i < len(tris); i += 3 {
			if tris[i] == tris[i+1] || tris[i+1] == tris[i+2] || tris[i] == tris[i+2] {
				t.Fatalf("case %d: degenerate triangle %v", c, tris[i:i+3])
			}
			for _, e := range tris[i : i+3] {
				if !crossed[e] {
					t.Fatalf("case %d uses uncrossed edge %d", c, e)
				}
				used[e] = true
			}
		}
		if len(used) != len(crossed) {
			t.Fatalf("case %d uses %d of %d crossed edges", c, len(used), len(crossed))
		}
		// A loop of n crossed edges gives n-2 triangles.
		if loops := (len(crossed) - len(tris)/3) / 2; (len(crossed)-len(tris)/3)%2 != 0 || (len(crossed) > 0 && loops == 0) || loops > 4 {
			t.Fatalf("case %d: %d crossed edges, %d triangles", c, len(crossed), len(tris)/3)
		}
		total += len(tris) / 3
	}
	if total == 0 {
		t.Fatal("empty table")
	}
}

// randomField is a seeded field of uniform samples in [-1, 1]: at iso 0 it
// exercises every case, ambiguous faces included.
func randomField(seed int64, dims [3]int, affine render.GeometryAffine) *DenseField {
	rng := rand.New(rand.NewSource(seed))
	data := make([]float32, dims[0]*dims[1]*dims[2])
	for i := range data {
		data[i] = float32(rng.Float64()*2 - 1)
	}
	return &DenseField{Size: dims, Affine: affine, Data: data}
}

// TestMarchingCubesRandomFieldIsClosedManifold extracts noise, which hits
// all 256 cases and ambiguous faces in both orientations: the closed
// surface must use every edge exactly twice in opposite directions, the
// enclosed volume must be positive, and the output must not depend on the
// worker count.
func TestMarchingCubesRandomFieldIsClosedManifold(t *testing.T) {
	seen := [256]bool{}
	for seed := int64(1); seed <= 12; seed++ {
		field := randomField(seed, [3]int{13, 11, 9}, identityAffine)
		for z := 0; z+1 < 9; z++ {
			for y := 0; y+1 < 11; y++ {
				for x := 0; x+1 < 13; x++ {
					c := 0
					for corner := 0; corner < 8; corner++ {
						i := ((z+corner>>2&1)*11+y+corner>>1&1)*13 + x + corner&1
						if field.Data[i] >= 0 {
							c |= 1 << corner
						}
					}
					seen[c] = true
				}
			}
		}
		m, err := Extract(context.Background(), field, Options{Workers: 1})
		if err != nil {
			t.Fatal(err)
		}
		requireClosedManifold(t, m)
		if _, volume := massProperties(m); volume <= 0 {
			t.Fatalf("seed %d: volume %g", seed, volume)
		}
		for _, workers := range []int{2, 3, 8} {
			other, err := Extract(context.Background(), field, Options{Workers: workers})
			if err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(m, other) {
				t.Fatalf("seed %d: %d workers change the mesh", seed, workers)
			}
		}
		open, err := Extract(context.Background(), field, Options{OpenBoundary: true, SkipNormals: true})
		if err != nil {
			t.Fatal(err)
		}
		if top := meshTopology(open); top.unpaired == 0 {
			t.Fatalf("seed %d: open-boundary noise surface has no boundary", seed)
		}
	}
	missing := 0
	for _, ok := range seen {
		if !ok {
			missing++
		}
	}
	if missing > 0 {
		t.Fatalf("noise exercised only %d of 256 cases", 256-missing)
	}
}

// sphereField samples R - |p - c| on a grid in patient space.
func sphereField(dims [3]int, affine render.GeometryAffine, center render.Vec3, radius float64) *FuncField {
	return &FuncField{Size: dims, Affine: affine, Value: func(x, y, z int) float64 {
		p := affine.TransformPoint(render.Vec3{X: float64(x), Y: float64(y), Z: float64(z)})
		return radius - p.Sub(center).Length()
	}}
}

// obliqueAxes is a rotated right-handed frame.
func obliqueAxes() [3]render.Vec3 {
	a, b := 0.4, 0.3
	x := render.Vec3{X: math.Cos(a), Y: math.Sin(a)}
	y := render.Vec3{X: -math.Sin(a) * math.Cos(b), Y: math.Cos(a) * math.Cos(b), Z: math.Sin(b)}
	return [3]render.Vec3{x, y, x.Cross(y)}
}

// TestMarchingCubesSphereAnalytic samples a 20 mm sphere on an oblique grid
// with anisotropic spacing, in a right-handed and a mirrored (negative
// determinant) frame: the surface is a closed genus-0 manifold, its area and
// volume are within 1% of 4πR² and 4πR³/3, every vertex lies within 0.05 mm
// of the sphere, and vertex normals point outward and agree with the
// triangle winding.
func TestMarchingCubesSphereAnalytic(t *testing.T) {
	const radius = 20.0
	center := render.Vec3{X: -12, Y: 30, Z: 105}
	spacing := [3]float64{0.8, 1.1, 1.5}
	dims := [3]int{64, 48, 36}
	axes := obliqueAxes()
	mirrored := [3]render.Vec3{axes[0], axes[1], axes[2].Scale(-1)}
	for name, axes := range map[string][3]render.Vec3{"right-handed": axes, "mirrored": mirrored} {
		t.Run(name, func(t *testing.T) {
			// Put the grid centre on the sphere centre.
			half := axes[0].Scale(spacing[0] * float64(dims[0]-1) / 2).
				Add(axes[1].Scale(spacing[1] * float64(dims[1]-1) / 2)).
				Add(axes[2].Scale(spacing[2] * float64(dims[2]-1) / 2))
			affine := GridAffine(center.Sub(half), axes, spacing)
			m, err := Extract(context.Background(), sphereField(dims, affine, center, radius), Options{})
			if err != nil {
				t.Fatal(err)
			}
			if top := requireClosedManifold(t, m); top.euler() != 2 {
				t.Fatalf("Euler characteristic %d, want 2 (%+v)", top.euler(), top)
			}
			area, volume := massProperties(m)
			if !relClose(area, 4*math.Pi*radius*radius, 0.01) || !relClose(volume, 4*math.Pi*radius*radius*radius/3, 0.01) {
				t.Fatalf("area %g, volume %g; analytic %g, %g", area, volume, 4*math.Pi*radius*radius, 4*math.Pi*radius*radius*radius/3)
			}
			for i := range m.Vertices {
				p := m.Vertex(i)
				if d := math.Abs(p.Sub(center).Length() - radius); d > 0.05 {
					t.Fatalf("vertex %d is %g mm off the sphere", i, d)
				}
				radial := p.Sub(center).Normalize()
				if n := vertexNormal(m, i); math.Abs(n.Length()-1) > 1e-5 || n.Dot(radial) < 0.99 {
					t.Fatalf("vertex %d normal %v, radial %v", i, n, radial)
				}
			}
			for i := range m.Triangles {
				tri := m.Triangles[i]
				n := triangleNormal(m, i)
				if n.Length() == 0 {
					continue
				}
				if n.Dot(vertexNormal(m, int(tri[0]))) <= 0 {
					t.Fatalf("triangle %d winds against its vertex normal", i)
				}
			}
		})
	}
}

// torusField samples r - dist(p, circle of radius R about the z axis).
func torusField(dims [3]int, affine render.GeometryAffine, big, small float64) *FuncField {
	return &FuncField{Size: dims, Affine: affine, Value: func(x, y, z int) float64 {
		p := affine.TransformPoint(render.Vec3{X: float64(x), Y: float64(y), Z: float64(z)})
		q := math.Hypot(p.X, p.Y) - big
		return small - math.Hypot(q, p.Z)
	}}
}

// TestMarchingCubesTorusAnalytic checks a genus-1 surface: Euler
// characteristic 0, area 4π²Rr and volume 2π²Rr² within 1%.
func TestMarchingCubesTorusAnalytic(t *testing.T) {
	const big, small = 18.0, 6.0
	spacing := [3]float64{0.7, 0.7, 0.9}
	dims := [3]int{72, 72, 20}
	origin := render.Vec3{X: -0.7 * 35.5, Y: -0.7 * 35.5, Z: -0.9 * 9.5}
	m, err := Extract(context.Background(), torusField(dims, GridAffine(origin, IdentityAxes, spacing), big, small), Options{})
	if err != nil {
		t.Fatal(err)
	}
	if top := requireClosedManifold(t, m); top.euler() != 0 {
		t.Fatalf("Euler characteristic %d, want 0 (%+v)", top.euler(), top)
	}
	area, volume := massProperties(m)
	wantArea, wantVolume := 4*math.Pi*math.Pi*big*small, 2*math.Pi*math.Pi*big*small*small
	if !relClose(area, wantArea, 0.01) || !relClose(volume, wantVolume, 0.01) {
		t.Fatalf("area %g, volume %g; analytic %g, %g", area, volume, wantArea, wantVolume)
	}
}

// TestMarchingCubesPlaneIsExact: a linear field is reconstructed exactly by
// linear edge interpolation. With an open boundary the surface is the plane
// z = 3.3 over the grid, with area (nx-1)(ny-1) spacing², normals +z... for
// a field increasing downward (inside below).
func TestMarchingCubesPlaneIsExact(t *testing.T) {
	spacing := [3]float64{0.5, 0.75, 2}
	dims := [3]int{9, 7, 6}
	affine := GridAffine(render.Vec3{X: 10, Y: -5, Z: 1}, IdentityAxes, spacing)
	const level = 7.6 // mm, between slices 3 and 4
	field := &FuncField{Size: dims, Affine: affine, Value: func(x, y, z int) float64 {
		return level - affine.TransformPoint(render.Vec3{Z: float64(z)}).Z // inside below the plane
	}}
	m, err := Extract(context.Background(), field, Options{OpenBoundary: true})
	if err != nil {
		t.Fatal(err)
	}
	area, _ := massProperties(m)
	if want := float64(dims[0]-1) * spacing[0] * float64(dims[1]-1) * spacing[1]; !relClose(area, want, 1e-6) {
		t.Fatalf("area %g, want %g", area, want)
	}
	for i := range m.Vertices {
		if math.Abs(m.Vertex(i).Z-level) > 1e-5 {
			t.Fatalf("vertex %v off the plane", m.Vertex(i))
		}
		if n := vertexNormal(m, i); math.Abs(n.Z-1) > 1e-6 {
			t.Fatalf("vertex normal %v, want +z", n)
		}
	}
	for i := range m.Triangles {
		if triangleNormal(m, i).Z <= 0 {
			t.Fatalf("triangle %d faces down", i)
		}
	}
	// Closed, the plane becomes the top of a box over the grid extended by
	// half a sample (x, y, and the bottom).
	closed, err := Extract(context.Background(), field, Options{})
	if err != nil {
		t.Fatal(err)
	}
	requireClosedManifold(t, closed)
	_, volume := massProperties(closed)
	lo := affine.TransformPoint(render.Vec3{X: -0.5, Y: -0.5, Z: -0.5})
	hi := affine.TransformPoint(render.Vec3{X: float64(dims[0]) - 0.5, Y: float64(dims[1]) - 0.5})
	box := (hi.X - lo.X) * (hi.Y - lo.Y) * (level - lo.Z)
	// Marching Cubes bevels the box edges and corners at the boundary.
	if volume >= box || volume < 0.9*box {
		t.Fatalf("closed plane volume %g, box %g", volume, box)
	}
}

// TestThinVolumes covers the vtkFlyingEdges3D thin-volume regressions: a
// single cell (2x2x2 samples) and volumes one sample thick along each axis
// still produce their surface.
func TestThinVolumes(t *testing.T) {
	// One cell, bottom samples -1, top +1, iso 0 (VTK's
	// TestFlyingEdges3DThinSlice): the plane z = 0.5, two triangles of
	// area 1 facing -z (inside above).
	cell := &DenseField{Size: [3]int{2, 2, 2}, Affine: identityAffine, Data: []float32{-1, -1, -1, -1, 1, 1, 1, 1}}
	m, err := Extract(context.Background(), cell, Options{OpenBoundary: true})
	if err != nil {
		t.Fatal(err)
	}
	if area, _ := massProperties(m); len(m.Triangles) != 2 || !relClose(area, 1, 1e-9) {
		t.Fatalf("%d triangles, area %g", len(m.Triangles), area)
	}
	for i := range m.Vertices {
		if m.Vertex(i).Z != 0.5 || vertexNormal(m, i).Z != -1 {
			t.Fatalf("vertex %v normal %v", m.Vertex(i), vertexNormal(m, i))
		}
	}
	for _, dims := range [][3]int{{1, 6, 5}, {6, 1, 5}, {6, 5, 1}, {1, 1, 1}} {
		n := dims[0] * dims[1] * dims[2]
		data := make([]float32, n)
		for i := range data {
			data[i] = 100
		}
		field := &DenseField{Size: dims, Affine: GridAffine(render.Vec3{}, IdentityAxes, [3]float64{0.5, 0.6, 3}), Data: data}
		m, err := Extract(context.Background(), field, Options{Iso: 50})
		if err != nil {
			t.Fatal(err)
		}
		if len(m.Triangles) == 0 {
			t.Fatalf("%v: no surface", dims)
		}
		requireClosedManifold(t, m)
		lo, hi := boundingBox(m)
		for axis, d := range dims {
			s := []float64{0.5, 0.6, 3}[axis]
			if math.Abs(lo[axis]+0.5*s) > 1e-5 || math.Abs(hi[axis]-(float64(d)-0.5)*s) > 1e-5 {
				t.Fatalf("%v: axis %d spans [%g, %g]", dims, axis, lo[axis], hi[axis])
			}
		}
		// VTK's behaviour: an open boundary on a one-sample axis has no cells.
		open, err := Extract(context.Background(), field, Options{Iso: 50, OpenBoundary: true})
		if err != nil || len(open.Triangles) != 0 {
			t.Fatalf("%v open: %d triangles, %v", dims, len(open.Triangles), err)
		}
		voxels, err := Extract(context.Background(), field, Options{Iso: 50, Method: VoxelFaces})
		if err != nil {
			t.Fatal(err)
		}
		requireClosedManifold(t, voxels)
		if _, volume := massProperties(voxels); !relClose(volume, float64(n)*0.5*0.6*3, 1e-6) {
			t.Fatalf("%v: voxel volume %g", dims, volume)
		}
	}
}

func boundingBox(m *Mesh) (lo, hi [3]float64) {
	for i := range lo {
		lo[i], hi[i] = math.Inf(1), math.Inf(-1)
	}
	for _, v := range m.Vertices {
		for i, c := range v {
			lo[i], hi[i] = math.Min(lo[i], float64(c)), math.Max(hi[i], float64(c))
		}
	}
	return lo, hi
}

// TestBudgetAndCancellation: an oversized surface fails with a
// *BudgetError before allocating; cancellation (before and during the
// extraction) returns the context error; invalid inputs are rejected.
func TestBudgetAndCancellation(t *testing.T) {
	field := sphereField([3]int{40, 40, 40}, identityAffine, render.Vec3{X: 19.5, Y: 19.5, Z: 19.5}, 15)
	full, err := Extract(context.Background(), field, Options{})
	if err != nil {
		t.Fatal(err)
	}
	for _, opts := range []Options{{MaxTriangles: int64(len(full.Triangles)) - 1}, {MaxBytes: full.Bytes() - 1}} {
		_, err := Extract(context.Background(), field, opts)
		var budgetErr *BudgetError
		if !errors.Is(err, ErrBudgetExceeded) || !errors.As(err, &budgetErr) ||
			budgetErr.Triangles != int64(len(full.Triangles)) || budgetErr.Vertices != int64(len(full.Vertices)) || budgetErr.Bytes != full.Bytes() {
			t.Fatalf("%+v: %v", opts, err)
		}
	}
	if _, err := Extract(context.Background(), field, Options{MaxTriangles: int64(len(full.Triangles)), MaxBytes: full.Bytes()}); err != nil {
		t.Fatalf("exact budget: %v", err)
	}
	if _, err := Extract(context.Background(), field, Options{MaxTriangles: -1, MaxBytes: -1}); err != nil {
		t.Fatalf("unbounded: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := Extract(ctx, field, Options{}); !errors.Is(err, context.Canceled) {
		t.Fatalf("cancelled before: %v", err)
	}
	for _, method := range []Method{MarchingCubes, VoxelFaces} {
		ctx, cancel = context.WithCancel(context.Background())
		reads := 0
		cancelling := &FuncField{Size: field.Size, Affine: field.Affine, Value: func(x, y, z int) float64 {
			if x == 0 && y == 0 {
				reads++
				if reads == 10 {
					cancel()
				}
			}
			return field.Value(x, y, z)
		}}
		if _, err := Extract(ctx, cancelling, Options{Method: method, Workers: 1}); !errors.Is(err, context.Canceled) {
			t.Fatalf("%v cancelled during: %v", method, err)
		}
		cancel()
	}
	for _, bad := range []struct {
		field Field
		opts  Options
	}{
		{nil, Options{}},
		{field, Options{Iso: math.NaN()}},
		{field, Options{Method: Method(9)}},
		{&DenseField{Size: [3]int{2, 2, 0}, Affine: identityAffine}, Options{}},
		{&DenseField{Size: [3]int{2, 2, 2}, Affine: render.GeometryAffine{}}, Options{}},
	} {
		if _, err := Extract(context.Background(), bad.field, bad.opts); !errors.Is(err, ErrInvalidField) {
			t.Fatalf("%+v: %v", bad, err)
		}
	}
	short := &DenseField{Size: [3]int{4, 4, 4}, Affine: identityAffine, Data: make([]float32, 10)}
	if _, err := Extract(context.Background(), short, Options{}); !errors.Is(err, ErrInvalidField) {
		t.Fatalf("short data: %v", err)
	}
}
