package mesh

import (
	"context"
	"errors"
	"math"
	"reflect"
	"sync"
	"sync/atomic"
	"testing"
	"unsafe"
)

// Independent fixtures are projected from an icosahedron and a parametric
// cylinder. Expected curvature comes from their analytic equations.
func curvatureSphere(radius float64, level int) *Mesh {
	p := (1 + math.Sqrt(5)) / 2
	points := [][3]float64{{-1, p, 0}, {1, p, 0}, {-1, -p, 0}, {1, -p, 0}, {0, -1, p}, {0, 1, p}, {0, -1, -p}, {0, 1, -p}, {p, 0, -1}, {p, 0, 1}, {-p, 0, -1}, {-p, 0, 1}}
	m := &Mesh{Triangles: [][3]uint32{{0, 11, 5}, {0, 5, 1}, {0, 1, 7}, {0, 7, 10}, {0, 10, 11}, {1, 5, 9}, {5, 11, 4}, {11, 10, 2}, {10, 7, 6}, {7, 1, 8}, {3, 9, 4}, {3, 4, 2}, {3, 2, 6}, {3, 6, 8}, {3, 8, 9}, {4, 9, 5}, {2, 4, 11}, {6, 2, 10}, {8, 6, 7}, {9, 8, 1}}}
	project := func(v [3]float64) [3]float32 {
		n := math.Sqrt(v[0]*v[0] + v[1]*v[1] + v[2]*v[2])
		return [3]float32{float32(radius * v[0] / n), float32(radius * v[1] / n), float32(radius * v[2] / n)}
	}
	for _, v := range points {
		m.Vertices = append(m.Vertices, project(v))
	}
	for subdivision := 0; subdivision < level; subdivision++ {
		midpoints := make(map[[2]uint32]uint32)
		midpoint := func(a, b uint32) uint32 {
			key := [2]uint32{min(a, b), max(a, b)}
			if i, ok := midpoints[key]; ok {
				return i
			}
			av, bv := m.Vertices[a], m.Vertices[b]
			v := project([3]float64{float64(av[0]) + float64(bv[0]), float64(av[1]) + float64(bv[1]), float64(av[2]) + float64(bv[2])})
			i := uint32(len(m.Vertices))
			m.Vertices = append(m.Vertices, v)
			midpoints[key] = i
			return i
		}
		faces := make([][3]uint32, 0, 4*len(m.Triangles))
		for _, f := range m.Triangles {
			a, b, c := midpoint(f[0], f[1]), midpoint(f[1], f[2]), midpoint(f[2], f[0])
			faces = append(faces, [3]uint32{f[0], a, c}, [3]uint32{f[1], b, a}, [3]uint32{f[2], c, b}, [3]uint32{a, b, c})
		}
		m.Triangles = faces
	}
	return m
}

func curvatureCylinder(radius float64, segments, rings int) *Mesh {
	m := &Mesh{}
	for ring := 0; ring < rings; ring++ {
		for i := 0; i < segments; i++ {
			a := 2 * math.Pi * float64(i) / float64(segments)
			m.Vertices = append(m.Vertices, [3]float32{float32(radius * math.Cos(a)), float32(radius * math.Sin(a)), float32(ring) * 2})
		}
	}
	for ring := 0; ring < rings-1; ring++ {
		for i := 0; i < segments; i++ {
			a, b := uint32(ring*segments+i), uint32(ring*segments+(i+1)%segments)
			c, d := b+uint32(segments), a+uint32(segments)
			m.Triangles = append(m.Triangles, [3]uint32{a, b, c}, [3]uint32{a, c, d})
		}
	}
	return m
}

func TestVertexCurvaturesAnalyticSphere(t *testing.T) {
	const radius = 20.
	// Float32 positions have relative machine epsilon 2^-23. The first
	// symmetric refinement has analytically aligned normals and falls below
	// that floor; require refinement improvement only from estimates above it.
	const relativeNoiseFloor = 1.0 / (1 << 23)
	previousH, previousK := math.Inf(1), math.Inf(1)
	for _, level := range []int{1, 2, 3, 4} {
		m := curvatureSphere(radius, level)
		got, err := VertexCurvatures(nil, m, CurvatureOptions{})
		if err != nil {
			t.Fatal(err)
		}
		var area, gaussBonnet, hError, kError, maxH, maxK, maxPrincipal float64
		for i := range m.Vertices {
			if got.Flags[i]&(CurvatureBoundary|CurvatureUnused) != 0 {
				t.Fatalf("sphere vertex %d masked", i)
			}
			h, k := got.Mean[i]*radius-1, got.Gaussian[i]*radius*radius-1
			maxH = max(maxH, math.Abs(h))
			maxK = max(maxK, math.Abs(k))
			maxPrincipal = max(maxPrincipal, math.Abs(got.PrincipalMin[i]*radius-1), math.Abs(got.PrincipalMax[i]*radius-1))
			area += got.MixedArea[i]
			gaussBonnet += got.Gaussian[i] * got.MixedArea[i]
			hError += h * h * got.MixedArea[i]
			kError += k * k * got.MixedArea[i]
		}
		hError, kError = math.Sqrt(hError/area), math.Sqrt(kError/area)
		t.Logf("sphere level=%d vertices=%d H_rms_relative=%.9g K_rms_relative=%.9g H_max_relative=%.9g K_max_relative=%.9g principal_max_relative=%.9g", level, len(m.Vertices), hError, kError, maxH, maxK, maxPrincipal)
		if math.Abs(area-m.Area()) > 1e-11*m.Area() {
			t.Fatal("mixed areas do not tile surface")
		}
		if math.Abs(gaussBonnet-4*math.Pi) > 1e-10 {
			t.Fatalf("Gauss-Bonnet %.16g", gaussBonnet)
		}
		if previousH > relativeNoiseFloor && hError >= previousH && hError > relativeNoiseFloor {
			t.Fatalf("mean refinement %.9g >= %.9g", hError, previousH)
		}
		if previousK > relativeNoiseFloor && kError >= previousK && kError > relativeNoiseFloor {
			t.Fatalf("Gaussian refinement %.9g >= %.9g", kError, previousK)
		}
		previousH, previousK = hError, kError
		if level >= 3 && (maxH > 0.05 || maxK > 0.05 || maxPrincipal > 0.15) {
			t.Fatalf("sphere prospective bounds exceeded: H=%g K=%g principal=%g", maxH, maxK, maxPrincipal)
		}
	}
}

func TestVertexCurvaturesAnalyticCylinder(t *testing.T) {
	const radius = 10.
	for _, segments := range []int{32, 64, 128} {
		m := curvatureCylinder(radius, segments, 6)
		got, err := VertexCurvatures(nil, m, CurvatureOptions{})
		if err != nil {
			t.Fatal(err)
		}
		var maxH, maxK, maxP, maxZero float64
		for i := range m.Vertices {
			boundary := i < segments || i >= 5*segments
			if boundary {
				if got.Flags[i] != CurvatureBoundary || got.Mean[i] != 0 || got.Gaussian[i] != 0 || got.PrincipalMin[i] != 0 || got.PrincipalMax[i] != 0 {
					t.Fatalf("boundary %d: %+v", i, got.Flags[i])
				}
				continue
			}
			if got.Flags[i]&(CurvatureBoundary|CurvatureUnused) != 0 {
				t.Fatalf("interior %d masked", i)
			}
			maxH = max(maxH, math.Abs(got.Mean[i]*2*radius-1))
			maxK = max(maxK, math.Abs(got.Gaussian[i]*radius*radius))
			maxP = max(maxP, math.Abs(got.PrincipalMax[i]*radius-1))
			maxZero = max(maxZero, math.Abs(got.PrincipalMin[i]*radius))
		}
		t.Logf("cylinder segments=%d H_max_relative=%.9g K_max_scaled=%.9g principal_max_relative=%.9g principal_zero_scaled=%.9g", segments, maxH, maxK, maxP, maxZero)
		if maxH > 0.02 || maxP > 0.02 || maxK > 1e-4 || maxZero > 1e-4 {
			t.Fatalf("cylinder prospective bounds exceeded")
		}
	}
}

func TestVertexCurvaturesIndependentFixtures(t *testing.T) {
	d := float32(1 / math.Sqrt(3))
	m := &Mesh{Vertices: [][3]float32{{d, d, d}, {d, -d, -d}, {-d, d, -d}, {-d, -d, d}}, Triangles: [][3]uint32{{0, 1, 2}, {0, 3, 1}, {0, 2, 3}, {1, 3, 2}}}
	r := math.Sqrt(3 * float64(d) * float64(d))
	got, err := VertexCurvatures(nil, m, CurvatureOptions{})
	if err != nil {
		t.Fatal(err)
	}
	for i := range got.Mean {
		if math.Abs(got.Mean[i]-1/r) > 1e-12 || math.Abs(got.Gaussian[i]-math.Pi*math.Sqrt(3)/(2*r*r)) > 1e-12 || got.Flags[i] != CurvaturePrincipalClamped || got.PrincipalMin[i] != got.Mean[i] || got.PrincipalMax[i] != got.Mean[i] {
			t.Fatalf("tetrahedron vertex %d", i)
		}
	}
	obtuse := &Mesh{Vertices: [][3]float32{{0, 0, 0}, {-2, 1, 0}, {2, 1, 0}}, Triangles: [][3]uint32{{0, 2, 1}}}
	got, err = VertexCurvatures(nil, obtuse, CurvatureOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(got.MixedArea, []float64{1, .5, .5}) {
		t.Fatalf("obtuse areas %v", got.MixedArea)
	}
	for _, saddle := range []bool{false, true} {
		grid := &Mesh{}
		for y := -2; y <= 2; y++ {
			for x := -2; x <= 2; x++ {
				z := float32(0)
				if saddle {
					z = .1 * float32(x*x-y*y)
				}
				grid.Vertices = append(grid.Vertices, [3]float32{float32(x), float32(y), z})
			}
		}
		for y := 0; y < 4; y++ {
			for x := 0; x < 4; x++ {
				a := uint32(y*5 + x)
				grid.Triangles = append(grid.Triangles, [3]uint32{a, a + 1, a + 6}, [3]uint32{a, a + 6, a + 5})
			}
		}
		got, err = VertexCurvatures(nil, grid, CurvatureOptions{})
		if err != nil {
			t.Fatal(err)
		}
		if saddle {
			if got.Gaussian[12] >= 0 || got.PrincipalMin[12] >= 0 || got.PrincipalMax[12] <= 0 {
				t.Fatal("saddle has no negative Gaussian curvature")
			}
		} else if math.Abs(got.Mean[12])+math.Abs(got.Gaussian[12])+math.Abs(got.PrincipalMin[12])+math.Abs(got.PrincipalMax[12]) > 1e-12 {
			t.Fatal("flat interior is curved")
		}
	}
}

func TestVertexCurvaturesTransformOwnershipConcurrent(t *testing.T) {
	m := curvatureSphere(20, 2)
	m.Normals = make([][3]float32, len(m.Vertices)) // unusable supplied normals intentionally ignored
	original := &Mesh{Vertices: append([][3]float32(nil), m.Vertices...), Triangles: append([][3]uint32(nil), m.Triangles...), Normals: append([][3]float32(nil), m.Normals...)}
	base, err := VertexCurvatures(nil, m, CurvatureOptions{})
	if err != nil {
		t.Fatal(err)
	}
	var wg sync.WaitGroup
	for task := 0; task < 4; task++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			got, e := VertexCurvatures(context.Background(), m, CurvatureOptions{})
			if e != nil || !reflect.DeepEqual(base, got) {
				t.Errorf("concurrent result: %v", e)
			}
		}()
	}
	wg.Wait()
	for _, mode := range []string{"reverse", "scale", "rigid"} {
		copyMesh := &Mesh{Vertices: append([][3]float32(nil), m.Vertices...), Triangles: append([][3]uint32(nil), m.Triangles...)}
		for i, v := range copyMesh.Vertices {
			if mode == "scale" {
				for k := range v {
					v[k] *= 2
				}
			} else if mode == "rigid" {
				v = [3]float32{-v[1] + 32, v[0] - 16, v[2] + 8}
			}
			copyMesh.Vertices[i] = v
		}
		if mode == "reverse" {
			for i, f := range copyMesh.Triangles {
				copyMesh.Triangles[i] = [3]uint32{f[0], f[2], f[1]}
			}
		}
		got, e := VertexCurvatures(nil, copyMesh, CurvatureOptions{})
		if e != nil {
			t.Fatal(e)
		}
		for i := range base.Mean {
			h, k, lo, hi, a := base.Mean[i], base.Gaussian[i], base.PrincipalMin[i], base.PrincipalMax[i], base.MixedArea[i]
			if mode == "reverse" {
				h, lo, hi = -h, -hi, -lo
			}
			if mode == "scale" {
				h, k, lo, hi, a = h/2, k/4, lo/2, hi/2, a*4
			}
			if math.Abs(got.Mean[i]-h) > 1e-6 || math.Abs(got.Gaussian[i]-k) > 1e-7 || math.Abs(got.PrincipalMin[i]-lo) > 1e-5 || math.Abs(got.PrincipalMax[i]-hi) > 1e-5 || math.Abs(got.MixedArea[i]-a) > 1e-4 {
				t.Fatalf("%s vertex %d", mode, i)
			}
		}
	}
	independent, err := VertexCurvatures(nil, m, CurvatureOptions{})
	if err != nil {
		t.Fatal(err)
	}
	wantFirst := independent.Mean[0]
	base.Mean[0] = 123
	if independent.Mean[0] != wantFirst {
		t.Fatal("result arrays shared between calls")
	}
	if !reflect.DeepEqual(m, original) {
		t.Fatal("source mutated/aliased")
	}
	unused := &Mesh{Vertices: [][3]float32{{0, 0, 0}}}
	got, e := VertexCurvatures(nil, unused, CurvatureOptions{})
	if e != nil || got.Flags[0] != CurvatureUnused || got.MixedArea[0] != 0 {
		t.Fatalf("unused %v", e)
	}
	empty, e := VertexCurvatures(nil, &Mesh{}, CurvatureOptions{})
	if e != nil || len(empty.Mean) != 0 {
		t.Fatalf("empty %v", e)
	}
}

func TestVertexCurvaturesErrorsAndBudgets(t *testing.T) {
	base := curvatureSphere(1, 0)
	cases := map[string]*Mesh{
		"missing":            {Vertices: [][3]float32{{0, 0, 0}}, Triangles: [][3]uint32{{0, 1, 2}}},
		"repeat":             {Vertices: [][3]float32{{0, 0, 0}, {1, 0, 0}}, Triangles: [][3]uint32{{0, 1, 1}}},
		"nonfinite":          {Vertices: [][3]float32{{float32(math.NaN()), 0, 0}}},
		"collinear":          {Vertices: [][3]float32{{0, 0, 0}, {1, 0, 0}, {2, 0, 0}}, Triangles: [][3]uint32{{0, 1, 2}}},
		"near-collinear":     {Vertices: [][3]float32{{0, 0, 0}, {1, 0, 0}, {2, 1e-16, 0}}, Triangles: [][3]uint32{{0, 1, 2}}},
		"duplicate":          {Vertices: base.Vertices, Triangles: [][3]uint32{base.Triangles[0], base.Triangles[0]}},
		"duplicate-reversed": {Vertices: base.Vertices, Triangles: [][3]uint32{base.Triangles[0], {base.Triangles[0][0], base.Triangles[0][2], base.Triangles[0][1]}}},
		"bowtie":             {Vertices: [][3]float32{{0, 0, 0}, {1, 0, 0}, {0, 1, 0}, {-1, 0, 0}, {0, -1, 0}}, Triangles: [][3]uint32{{0, 1, 2}, {0, 3, 4}}},
		"three-face-edge":    {Vertices: [][3]float32{{0, 0, 0}, {1, 0, 0}, {0, 1, 0}, {0, -1, 0}, {0, 0, 1}}, Triangles: [][3]uint32{{0, 1, 2}, {1, 0, 3}, {0, 1, 4}}},
		"singular-normal":    {Vertices: [][3]float32{{0, 0, 0}, {2, 1, 0}, {-1, 1, 0}, {2, -1, 0}, {-1, -1, 0}}, Triangles: [][3]uint32{{0, 1, 2}, {0, 2, 3}, {0, 3, 4}, {0, 4, 1}}},
	}
	wrong := &Mesh{Vertices: base.Vertices, Triangles: append([][3]uint32(nil), base.Triangles...)}
	wrong.Triangles[0][1], wrong.Triangles[0][2] = wrong.Triangles[0][2], wrong.Triangles[0][1]
	cases["winding"] = wrong
	for name, m := range cases {
		t.Run(name, func(t *testing.T) {
			r, e := VertexCurvatures(nil, m, CurvatureOptions{})
			if r != nil || !errors.Is(e, ErrInvalidField) {
				t.Fatalf("result/error %v %v", r, e)
			}
		})
	}
	for _, opts := range []CurvatureOptions{{MaxVertices: -1}, {MaxTriangles: -1}, {MaxBytes: -1}} {
		if _, e := VertexCurvatures(nil, base, opts); !errors.Is(e, ErrInvalidField) {
			t.Fatalf("negative budget %v", e)
		}
	}
	required := 128*int64(len(base.Vertices)+len(base.Triangles)) + 8192
	for _, opts := range []CurvatureOptions{{MaxVertices: 1}, {MaxTriangles: 1}, {MaxBytes: required - 1}} {
		_, e := VertexCurvatures(nil, base, opts)
		var b *CurvatureBudgetError
		if !errors.As(e, &b) || !errors.Is(e, ErrBudgetExceeded) || b.Bytes != required {
			t.Fatalf("budget %v", e)
		}
	}
	for _, counts := range [][2]int64{{math.MaxInt64, 1}, {1, math.MaxInt64}, {math.MaxUint32, 0}, {0, math.MaxUint32/3 + 1}} {
		if !errors.Is(curvatureBudget(counts[0], counts[1], CurvatureOptions{MaxVertices: math.MaxInt64, MaxTriangles: math.MaxInt64, MaxBytes: math.MaxInt64}), ErrBudgetExceeded) {
			t.Fatal("hard count overflow accepted")
		}
	}
	// Inventory every array type at its real size. The formula is a
	// conservative managed-capacity bound, not an allocator/RSS measurement.
	perVertex := 5*unsafe.Sizeof(float64(0)) + unsafe.Sizeof(CurvatureFlags(0)) + 2*unsafe.Sizeof([3]float64{}) + 2*unsafe.Sizeof(float64(0)) + unsafe.Sizeof(uint32(0))
	perFace := 6*unsafe.Sizeof(curvatureEdge{}) + 3*unsafe.Sizeof(uint32(0)) + 3*unsafe.Sizeof(uint8(0))
	// uint64 alignment can make the edge record smaller on 32-bit targets;
	// enforce the conservative capacities, not a host-specific padding size.
	if perVertex != 109 || perFace > 111 || perVertex > 128 || perFace > 128 {
		t.Fatalf("capacity inventory %dN+%dT", perVertex, perFace)
	}
}

type curvatureCancelContext struct {
	context.Context
	cancel context.CancelFunc
	calls  atomic.Int64
	after  int64
}

func (c *curvatureCancelContext) Err() error {
	if c.calls.Add(1) >= c.after {
		c.cancel()
	}
	return c.Context.Err()
}

func TestVertexCurvaturesCancellation(t *testing.T) {
	m := curvatureSphere(20, 3)
	for _, after := range []int64{1, 2, 10, 200} {
		ctx, cancel := context.WithCancel(context.Background())
		wrapped := &curvatureCancelContext{Context: ctx, cancel: cancel, after: after}
		r, e := VertexCurvatures(wrapped, m, CurvatureOptions{})
		cancel()
		if r != nil || !errors.Is(e, context.Canceled) {
			t.Fatalf("cancel after %d: %v", after, e)
		}
	}
	// Cancel at geometry/finalization checks after the same topology succeeds.
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	counter := &curvatureCancelContext{Context: ctx, cancel: cancel, after: math.MaxInt64}
	if e := validateCurvatureInput(counter, m); e != nil {
		t.Fatal(e)
	}
	if e := curvatureTopology(counter, m, make([]CurvatureFlags, len(m.Vertices))); e != nil {
		t.Fatal(e)
	}
	ctx2, cancel2 := context.WithCancel(context.Background())
	defer cancel2()
	wrapped := &curvatureCancelContext{Context: ctx2, cancel: cancel2, after: counter.calls.Load() + 2}
	if r, e := VertexCurvatures(wrapped, m, CurvatureOptions{}); r != nil || !errors.Is(e, context.Canceled) {
		t.Fatalf("late cancellation %v", e)
	}
}
