package mesh

import (
	"context"
	"errors"
	"fmt"
	"math"
	"testing"
	"time"

	"github.com/ThalesMMS/dicom-go/render"
)

// legacySmoothWindowedSinc is the sequential filter as it stood before
// #1253 (neighbours in map order), kept as the reference the cancellable,
// parallel filter must reproduce with its default options.
func legacySmoothWindowedSinc(m *Mesh, opts SmoothOptions) *Mesh {
	degree, passBand := opts.Iterations, opts.PassBand
	if degree == 0 {
		degree = 20
	}
	if passBand == 0 {
		passBand = 0.1
	}
	neighbours, start := legacySmoothingNeighbours(m, opts.FixBoundary)
	coefficients := sincCoefficients(degree, passBand, opts.Window)
	n := len(m.Vertices)
	previous := make([][3]float64, n)
	current := make([][3]float64, n)
	next := make([][3]float64, n)
	sum := make([][3]float64, n)
	for i, v := range m.Vertices {
		previous[i] = [3]float64{float64(v[0]), float64(v[1]), float64(v[2])}
		for axis := 0; axis < 3; axis++ {
			sum[i][axis] = coefficients[0] * previous[i][axis]
		}
	}
	umbrella := func(x, out [][3]float64) {
		for i := range x {
			list := neighbours[start[i]:start[i+1]]
			if len(list) == 0 {
				out[i] = x[i]
				continue
			}
			var mean [3]float64
			for _, j := range list {
				mean[0] += x[j][0]
				mean[1] += x[j][1]
				mean[2] += x[j][2]
			}
			scale := 1 / float64(len(list))
			out[i] = [3]float64{mean[0] * scale, mean[1] * scale, mean[2] * scale}
		}
	}
	umbrella(previous, current)
	for i := range sum {
		for axis := 0; axis < 3; axis++ {
			sum[i][axis] += coefficients[1] * current[i][axis]
		}
	}
	for k := 2; k <= degree; k++ {
		umbrella(current, next)
		for i := range next {
			for axis := 0; axis < 3; axis++ {
				next[i][axis] = 2*next[i][axis] - previous[i][axis]
				sum[i][axis] += coefficients[k] * next[i][axis]
			}
		}
		previous, current, next = current, next, previous
	}
	out := &Mesh{Vertices: make([][3]float32, n), Triangles: m.Triangles}
	for i, p := range sum {
		out.Vertices[i] = [3]float32{float32(p[0]), float32(p[1]), float32(p[2])}
	}
	return out
}

func legacySmoothingNeighbours(m *Mesh, fixBoundary bool) (neighbours []uint32, start []int32) {
	edges := edgeUses(m)
	n := len(m.Vertices)
	boundary := make([]bool, n)
	fixed := make([]bool, n)
	for e, uses := range edges {
		switch {
		case uses == 1:
			boundary[e[0]], boundary[e[1]] = true, true
		case uses > 2:
			fixed[e[0]], fixed[e[1]] = true, true
		}
	}
	lists := make([][]uint32, n)
	for e, uses := range edges {
		for side := 0; side < 2; side++ {
			a, b := e[side], e[1-side]
			if fixed[a] {
				continue
			}
			if boundary[a] && uses != 1 {
				continue
			}
			lists[a] = append(lists[a], b)
		}
	}
	start = make([]int32, n+1)
	for i, list := range lists {
		if boundary[i] && (fixBoundary || len(list) != 2) {
			lists[i] = nil
		}
		start[i+1] = start[i] + int32(len(lists[i]))
	}
	neighbours = make([]uint32, 0, start[n])
	for _, list := range lists {
		neighbours = append(neighbours, list...)
	}
	return neighbours, start
}

// openHemisphere is the upper half of a sphere: an open surface whose rim
// is a simple boundary curve.
func openHemisphere(t *testing.T, sphere *Mesh) *Mesh {
	t.Helper()
	var centre [3]float64
	for _, v := range sphere.Vertices {
		centre[2] += float64(v[2]) / float64(len(sphere.Vertices))
	}
	out := &Mesh{Vertices: sphere.Vertices, Normals: sphere.Normals}
	for _, tri := range sphere.Triangles {
		if float64(sphere.Vertices[tri[0]][2]) > centre[2] && float64(sphere.Vertices[tri[1]][2]) > centre[2] && float64(sphere.Vertices[tri[2]][2]) > centre[2] {
			out.Triangles = append(out.Triangles, tri)
		}
	}
	return out
}

// TestSmoothWindowedSincMatchesTheSequentialFilter (#1253): with the default
// options the parallel filter gives the previous filter's vertices (the
// neighbour sums only change order, within float32 rounding), and the same
// bits for every worker count and run.
func TestSmoothWindowedSincMatchesTheSequentialFilter(t *testing.T) {
	sphere := analyticSphere(t, 20)
	if len(sphere.Vertices) < smoothParallelMinimum {
		t.Fatalf("%d vertices do not exercise the parallel passes", len(sphere.Vertices))
	}
	hemisphere := openHemisphere(t, sphere)
	for name, c := range map[string]struct {
		m    *Mesh
		opts SmoothOptions
	}{
		"closed default":       {sphere, SmoothOptions{}},
		"closed hamming":       {sphere, SmoothOptions{Iterations: 35, PassBand: 0.05, Window: WindowHamming}},
		"open rim smoothed":    {hemisphere, SmoothOptions{}},
		"open rim fixed":       {hemisphere, SmoothOptions{FixBoundary: true, Window: WindowBlackman}},
		"open rim hanning, 10": {hemisphere, SmoothOptions{Iterations: 10, Window: WindowHanning}},
	} {
		want := legacySmoothWindowedSinc(c.m, c.opts)
		var reference *Mesh
		for _, workers := range []int{1, 3, 8, 0} {
			opts := c.opts
			opts.Workers = workers
			got, err := SmoothWindowedSincContext(context.Background(), c.m, opts)
			if err != nil {
				t.Fatal(err)
			}
			if got.Normals == nil {
				t.Fatalf("%s: normals not recomputed", name)
			}
			worst := 0.0
			for i, v := range got.Vertices {
				for axis := 0; axis < 3; axis++ {
					worst = math.Max(worst, math.Abs(float64(v[axis]-want.Vertices[i][axis])))
				}
			}
			if worst > 1e-4 {
				t.Fatalf("%s, %d workers: a vertex moved %.2g mm from the sequential filter", name, workers, worst)
			}
			if reference == nil {
				reference = got
				continue
			}
			for i, v := range got.Vertices {
				if v != reference.Vertices[i] || got.Normals[i] != reference.Normals[i] {
					t.Fatalf("%s: vertex %d differs with %d workers", name, i, workers)
				}
			}
		}
	}
	plain, err := SmoothWindowedSinc(sphere, SmoothOptions{})
	if err != nil {
		t.Fatal(err)
	}
	again, _ := SmoothWindowedSinc(sphere, SmoothOptions{Workers: 2})
	for i := range plain.Vertices {
		if plain.Vertices[i] != again.Vertices[i] {
			t.Fatalf("SmoothWindowedSinc differs from its context form at vertex %d", i)
		}
	}
}

// voxelBlock is the voxel-face surface of a w×h×d block of 1 mm voxels.
func voxelBlock(t *testing.T, w, h, d int) *Mesh {
	t.Helper()
	dims := [3]int{w + 2, h + 2, d + 2}
	data := make([]float32, dims[0]*dims[1]*dims[2])
	for z := 1; z <= d; z++ {
		for y := 1; y <= h; y++ {
			for x := 1; x <= w; x++ {
				data[(z*dims[1]+y)*dims[0]+x] = 1
			}
		}
	}
	m, err := Extract(context.Background(), &DenseField{Size: dims, Affine: GridAffine(render.Vec3{}, IdentityAxes, [3]float64{1, 1, 1}), Data: data}, Options{Method: VoxelFaces, Iso: 0.5})
	if err != nil {
		t.Fatal(err)
	}
	return m
}

// TestSmoothWindowedSincPreservesFeatureEdges (#1253): a block's corners
// stay in place and its edges straight with feature edges kept, so it
// keeps its volume exactly; without them the corners round off.
func TestSmoothWindowedSincPreservesFeatureEdges(t *testing.T) {
	block := voxelBlock(t, 12, 8, 6)
	_, volume := massProperties(block)
	var lo, hi [3]float32
	lo, hi = block.Vertices[0], block.Vertices[0]
	for _, v := range block.Vertices {
		for k := 0; k < 3; k++ {
			lo[k], hi[k] = min(lo[k], v[k]), max(hi[k], v[k])
		}
	}
	onBox := func(v [3]float32) int {
		count := 0
		for k := 0; k < 3; k++ {
			if v[k] == lo[k] || v[k] == hi[k] {
				count++
			}
		}
		return count
	}
	rounded, err := SmoothWindowedSinc(block, SmoothOptions{Iterations: 30})
	if err != nil {
		t.Fatal(err)
	}
	moved := 0.0
	for i, v := range block.Vertices {
		if onBox(v) == 3 {
			d := sub3([3]float64{float64(v[0]), float64(v[1]), float64(v[2])}, [3]float64{float64(rounded.Vertices[i][0]), float64(rounded.Vertices[i][1]), float64(rounded.Vertices[i][2])})
			moved = math.Max(moved, math.Sqrt(dot3(d, d)))
		}
	}
	if moved < 0.3 {
		t.Fatalf("plain smoothing moved the corners %.2f mm; the comparison needs rounded corners", moved)
	}
	for _, angle := range []float64{0, 30, 80} {
		sharp, err := SmoothWindowedSincContext(context.Background(), block, SmoothOptions{Iterations: 30, PreserveFeatureEdges: true, FeatureAngle: angle})
		if err != nil {
			t.Fatal(err)
		}
		for i, v := range block.Vertices {
			got := sharp.Vertices[i]
			switch onBox(v) {
			case 3: // corner: fixed
				if got != v {
					t.Fatalf("angle %v: corner %v moved to %v", angle, v, got)
				}
			case 2: // on a block edge: slides along it
				if onBox(got) != 2 {
					t.Fatalf("angle %v: edge vertex %v left its edge for %v", angle, v, got)
				}
			}
		}
		if _, v := massProperties(sharp); math.Abs(v-volume) > 1e-3*volume {
			t.Fatalf("angle %v: block volume %.3f mm³, want %.3f", angle, v, volume)
		}
	}
	// A feature angle above the block's 90° creases treats it as smooth.
	loose, err := SmoothWindowedSinc(block, SmoothOptions{Iterations: 30, PreserveFeatureEdges: true, FeatureAngle: 120})
	if err != nil {
		t.Fatal(err)
	}
	for i := range loose.Vertices {
		if loose.Vertices[i] != rounded.Vertices[i] {
			t.Fatalf("a 120° feature angle changed vertex %d", i)
		}
	}
	for _, angle := range []float64{-1, 180, math.NaN()} {
		if _, err := SmoothWindowedSinc(block, SmoothOptions{PreserveFeatureEdges: true, FeatureAngle: angle}); !errors.Is(err, ErrInvalidField) {
			t.Errorf("feature angle %v: %v", angle, err)
		}
	}
}

// TestSmoothWindowedSincCancellation (#1253): a cancelled context stops the
// filter within a pass and returns no mesh.
func TestSmoothWindowedSincCancellation(t *testing.T) {
	sheet := gridSheet(700, 700)
	cancelled, cancel := context.WithCancel(context.Background())
	cancel()
	if m, err := SmoothWindowedSincContext(cancelled, sheet, SmoothOptions{}); m != nil || !errors.Is(err, context.Canceled) {
		t.Fatalf("pre-cancelled smoothing: %v, %v", m != nil, err)
	}
	for _, workers := range []int{1, 4} {
		// 2000 passes over 490,000 vertices take seconds; the cancellation
		// lands within one of them.
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Millisecond)
		began := time.Now()
		m, err := SmoothWindowedSincContext(ctx, sheet, SmoothOptions{Iterations: 2000, Workers: workers})
		elapsed := time.Since(began)
		cancel()
		if m != nil || !errors.Is(err, context.DeadlineExceeded) {
			t.Fatalf("%d workers: %v, %v after %v", workers, m != nil, err, elapsed)
		}
		if elapsed > 2*time.Second {
			t.Fatalf("%d workers: cancellation took %v", workers, elapsed)
		}
	}
}

func BenchmarkSmoothWindowedSinc(b *testing.B) {
	n := 161
	field := &FuncField{Size: [3]int{n, n, n}, Affine: identityAffine, Value: func(x, y, z int) float64 {
		dx, dy, dz := float64(x-n/2), float64(y-n/2), float64(z-n/2)
		return 70 - math.Sqrt(dx*dx+dy*dy+dz*dz)
	}}
	sphere, err := Extract(context.Background(), field, Options{})
	if err != nil {
		b.Fatal(err)
	}
	for _, workers := range []int{1, 0} {
		b.Run(fmt.Sprintf("vertices=%d/workers=%d", len(sphere.Vertices), workers), func(b *testing.B) {
			b.ReportAllocs()
			for i := 0; i < b.N; i++ {
				if _, err := SmoothWindowedSincContext(context.Background(), sphere, SmoothOptions{Workers: workers}); err != nil {
					b.Fatal(err)
				}
			}
		})
	}
}
