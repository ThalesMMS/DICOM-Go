package mesh

import (
	"context"
	"errors"
	"fmt"
	"math"
	"testing"
)

// pittedBlock is the Marching Cubes surface of a 32×32×24 mm block (1 mm
// voxels) with a cylindrical pit of radius 5 mm and depth 14 mm bored into
// its top face around (20, 20).
func pittedBlock(t testing.TB) *Mesh {
	t.Helper()
	dims := [3]int{41, 41, 33}
	data := make([]float32, dims[0]*dims[1]*dims[2])
	for z := 0; z < dims[2]; z++ {
		for y := 0; y < dims[1]; y++ {
			for x := 0; x < dims[0]; x++ {
				inside := x >= 4 && x <= 36 && y >= 4 && y <= 36 && z >= 4 && z <= 28
				dx, dy := float64(x-20), float64(y-20)
				if z > 14 && dx*dx+dy*dy <= 25 {
					inside = false
				}
				if inside {
					data[(z*dims[1]+y)*dims[0]+x] = 1
				}
			}
		}
	}
	m, err := Extract(context.Background(), &DenseField{Size: dims, Affine: identityAffine, Data: data}, Options{Iso: 0.5})
	if err != nil {
		t.Fatal(err)
	}
	return m
}

// TestAmbientOcclusionDarkensACavity (#1237): the floor of a pit is darker
// than the open top face around it, the block's flat outer faces are fully
// lit, and the values are the same for any worker count and run.
func TestAmbientOcclusionDarkensACavity(t *testing.T) {
	block := pittedBlock(t)
	ao, err := AmbientOcclusion(context.Background(), block, OcclusionOptions{Radius: 12})
	if err != nil {
		t.Fatal(err)
	}
	var floor, top, side []float64
	for i, v := range block.Vertices {
		dx, dy := float64(v[0])-20, float64(v[1])-20
		r := math.Sqrt(dx*dx + dy*dy)
		switch {
		case math.Abs(float64(v[2])-14.5) < 0.6 && r < 3:
			floor = append(floor, float64(ao[i]))
		case math.Abs(float64(v[2])-28.5) < 0.1 && r > 10 && v[0] > 7 && v[0] < 33 && v[1] > 7 && v[1] < 33:
			top = append(top, float64(ao[i]))
		case math.Abs(float64(v[0])-3.5) < 0.1 && v[1] > 8 && v[1] < 32 && v[2] > 8 && v[2] < 24:
			side = append(side, float64(ao[i]))
		}
	}
	mean := func(values []float64) float64 {
		var sum float64
		for _, v := range values {
			sum += v
		}
		return sum / float64(len(values))
	}
	if len(floor) < 10 || len(top) < 50 || len(side) < 50 {
		t.Fatalf("%d floor, %d top, %d side vertices", len(floor), len(top), len(side))
	}
	t.Logf("accessibility: pit floor %.3f, top face %.3f, side face %.3f", mean(floor), mean(top), mean(side))
	if mean(floor) > 0.75 || mean(top) < 0.99 || mean(side) < 0.99 || mean(floor) > mean(top)-0.25 {
		t.Fatalf("pit floor %.3f is not darker than the top %.3f and side %.3f", mean(floor), mean(top), mean(side))
	}
	for i, value := range ao {
		if !(value >= 0 && value <= 1) {
			t.Fatalf("vertex %d accessibility %v", i, value)
		}
	}
	for _, workers := range []int{1, 3, 8} {
		again, err := AmbientOcclusion(context.Background(), block, OcclusionOptions{Radius: 12, Workers: workers})
		if err != nil {
			t.Fatal(err)
		}
		for i := range ao {
			if again[i] != ao[i] {
				t.Fatalf("vertex %d: %v with %d workers, %v before", i, again[i], workers, ao[i])
			}
		}
	}
	// Without normals the area-weighted normals give the same hemisphere.
	bare := &Mesh{Vertices: block.Vertices, Triangles: block.Triangles}
	if ao2, err := AmbientOcclusion(context.Background(), bare, OcclusionOptions{Radius: 12}); err != nil || math.Abs(meanFloat32(ao2)-meanFloat32(ao)) > 0.05 {
		t.Fatalf("without normals: mean %.3f against %.3f (%v)", meanFloat32(ao2), meanFloat32(ao), err)
	}
}

func (m *Mesh) meanOcclusion(t *testing.T, opts OcclusionOptions) (mean, lowest float64) {
	t.Helper()
	ao, err := AmbientOcclusion(context.Background(), m, opts)
	if err != nil {
		t.Fatal(err)
	}
	lowest = 1
	for _, v := range ao {
		mean += float64(v) / float64(len(ao))
		lowest = math.Min(lowest, float64(v))
	}
	return mean, lowest
}

func meanFloat32(values []float32) float64 {
	var sum float64
	for _, v := range values {
		sum += float64(v)
	}
	return sum / float64(len(values))
}

// TestAmbientOcclusionLeavesAConvexSurfaceLit: nothing of a sphere lies
// above its own surface.
func TestAmbientOcclusionLeavesAConvexSurfaceLit(t *testing.T) {
	sphere := analyticSphere(t, 8)
	average, lowest := sphere.meanOcclusion(t, OcclusionOptions{})
	if average < 0.995 || lowest < 0.9 {
		t.Fatalf("sphere accessibility mean %.4f, lowest %.4f", average, lowest)
	}
}

func TestAmbientOcclusionOptionsAndCancellation(t *testing.T) {
	sphere := analyticSphere(t, 4)
	for _, opts := range []OcclusionOptions{{Rays: -1}, {Rays: 5000}, {Radius: -1}, {Radius: math.Inf(1)}, {Radius: math.NaN()}} {
		if _, err := AmbientOcclusion(context.Background(), sphere, opts); !errors.Is(err, ErrInvalidField) {
			t.Errorf("%+v: %v", opts, err)
		}
	}
	if _, err := AmbientOcclusion(context.Background(), nil, OcclusionOptions{}); err == nil {
		t.Error("nil mesh accepted")
	}
	if ao, err := AmbientOcclusion(context.Background(), &Mesh{Vertices: [][3]float32{{1, 2, 3}}}, OcclusionOptions{}); err != nil || len(ao) != 1 || ao[0] != 1 {
		t.Errorf("mesh without triangles: %v, %v", ao, err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if ao, err := AmbientOcclusion(ctx, sphere, OcclusionOptions{}); ao != nil || !errors.Is(err, context.Canceled) {
		t.Fatalf("cancelled occlusion: %v, %v", ao != nil, err)
	}
}

func BenchmarkAmbientOcclusion(b *testing.B) {
	block := pittedBlock(b)
	for _, rays := range []int{16, 32} {
		b.Run(fmt.Sprintf("vertices=%d/rays=%d", len(block.Vertices), rays), func(b *testing.B) {
			b.ReportAllocs()
			for i := 0; i < b.N; i++ {
				if _, err := AmbientOcclusion(context.Background(), block, OcclusionOptions{Rays: rays, Radius: 12}); err != nil {
					b.Fatal(err)
				}
			}
		})
	}
}
