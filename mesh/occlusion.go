package mesh

import (
	"context"
	"fmt"
	"math"

	"github.com/ThalesMMS/dicom-go/internal/parallel"
)

// Per-vertex ambient occlusion (#1237): how much of the hemisphere above
// each vertex the surface itself hides, for depth cues in surface
// rendering (creases and cavities darken, convex parts stay lit). Rays are
// cast in object space against a bounding-volume hierarchy of the
// triangles, in the spirit of Zhukov, Iones and Kronin's ambient
// obscurances (1998): a hit at distance t within the radius R obscures by
// 1 - t/R, so distant occluders fade out smoothly instead of switching
// off. Written from the published method; no third-party code is copied.

// DefaultOcclusionRays is the ray count per vertex when
// OcclusionOptions.Rays is zero.
const DefaultOcclusionRays = 32

// OcclusionOptions configures AmbientOcclusion.
type OcclusionOptions struct {
	// Rays per vertex, cosine-distributed over the hemisphere around its
	// normal; zero uses DefaultOcclusionRays.
	Rays int
	// Radius in millimetres beyond which a hit does not obscure; zero uses
	// a tenth of the bounding-box diagonal.
	Radius float64
	// Workers bounds the goroutines; zero uses min(shared budget,
	// DefaultMaxWorkers). The result does not depend on it.
	Workers int
}

// AmbientOcclusion returns, per vertex of m, its ambient accessibility in
// [0, 1]: 1 where nothing of the mesh lies above it within the radius, and
// lower the more of its hemisphere nearby triangles hide. The hemisphere is
// that of the vertex normal (m.Normals, or the area-weighted triangle
// normals without them); a vertex without a normal reads 1. Triangles
// touching the vertex are ignored. The directions are a fixed spiral,
// turned per vertex by a hash of its index, so the result is deterministic
// and the same for any worker count. The context is checked between slabs
// of vertices.
func AmbientOcclusion(ctx context.Context, m *Mesh, opts OcclusionOptions) ([]float32, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	if err := m.Validate(); err != nil {
		return nil, err
	}
	rays := opts.Rays
	if rays == 0 {
		rays = DefaultOcclusionRays
	}
	if rays < 1 || rays > 1024 || !(opts.Radius >= 0) || math.IsInf(opts.Radius, 1) {
		return nil, fmt.Errorf("%w: occlusion rays %d, radius %v", ErrInvalidField, opts.Rays, opts.Radius)
	}
	n := len(m.Vertices)
	out := make([]float32, n)
	for i := range out {
		out[i] = 1
	}
	if n == 0 || len(m.Triangles) == 0 {
		return out, nil
	}
	normals := m.Normals
	if normals == nil {
		normals = make([][3]float32, n)
		faceNormals(&Mesh{Vertices: m.Vertices, Normals: normals, Triangles: m.Triangles}, nil)
	}
	tree := newTriangleBVH(m)
	radius := opts.Radius
	if radius == 0 {
		radius = 0.1 * math.Sqrt(dot3(sub3(tree.nodes[0].hi, tree.nodes[0].lo), sub3(tree.nodes[0].hi, tree.nodes[0].lo)))
	}
	if !(radius > 0) {
		return out, nil
	}
	directions := hemisphereSpiral(rays)
	workers := opts.Workers
	if workers <= 0 {
		workers = min(parallel.DefaultWorkers(), DefaultMaxWorkers)
	}
	shade := func(lo, hi int) {
		stack := make([]int32, 0, 64)
		for v := lo; v < hi; v++ {
			normal := [3]float64{float64(normals[v][0]), float64(normals[v][1]), float64(normals[v][2])}
			if dot3(normal, normal) < 0.5 {
				continue
			}
			p := m.point(uint32(v))
			// An orthonormal frame around the normal, turned by the vertex's
			// hash so neighbouring vertices sample different directions.
			tangent, bitangent := orthonormalFrame(normal)
			angle := 2 * math.Pi * float64(hashIndex(uint32(v))) / (1 << 32)
			c, s := math.Cos(angle), math.Sin(angle)
			tangent, bitangent = add3(scale3(tangent, c), scale3(bitangent, s)), sub3(scale3(bitangent, c), scale3(tangent, s))
			var obscured float64
			for _, d := range directions {
				dir := add3(add3(scale3(tangent, d[0]), scale3(bitangent, d[1])), scale3(normal, d[2]))
				if t, _, hit := tree.closestHit(m, p, dir, radius, uint32(v), 0, &stack); hit {
					obscured += 1 - t/radius
				}
			}
			out[v] = float32(1 - obscured/float64(len(directions)))
		}
	}
	if err := forEachChunk(ctx, n, occlusionChunk, workers, shade); err != nil {
		return nil, err
	}
	return out, nil
}

// occlusionChunk is the number of vertices shaded between context checks.
const occlusionChunk = 256

// forEachChunk runs work on consecutive chunks [lo, hi) of n items on up
// to workers workers from the module's shared concurrency budget claiming
// chunks in order, checking the context before each chunk.
func forEachChunk(ctx context.Context, n, chunk, workers int, work func(lo, hi int)) error {
	chunks := (n + chunk - 1) / chunk
	return parallel.Do(ctx, chunks, workers, func(c int) {
		work(c*chunk, min(n, (c+1)*chunk))
	})
}

func scale3(a [3]float64, s float64) [3]float64 { return [3]float64{a[0] * s, a[1] * s, a[2] * s} }

// hashIndex scrambles a vertex index (a 32-bit integer finaliser).
func hashIndex(x uint32) uint32 {
	x ^= x >> 16
	x *= 0x7feb352d
	x ^= x >> 15
	x *= 0x846ca68b
	x ^= x >> 16
	return x
}

// hemisphereSpiral is n cosine-distributed unit directions around +Z: a
// Fibonacci spiral over the disk projected up onto the hemisphere.
func hemisphereSpiral(n int) [][3]float64 {
	golden := (math.Sqrt(5) - 1) / 2
	out := make([][3]float64, n)
	for i := range out {
		u := (float64(i) + 0.5) / float64(n)
		phi := 2 * math.Pi * math.Mod(float64(i)*golden, 1)
		r := math.Sqrt(u)
		out[i] = [3]float64{r * math.Cos(phi), r * math.Sin(phi), math.Sqrt(1 - u)}
	}
	return out
}

// orthonormalFrame returns two unit vectors orthogonal to the unit vector n
// and to each other (Duff et al., "Building an Orthonormal Basis,
// Revisited", 2017).
func orthonormalFrame(n [3]float64) (t, b [3]float64) {
	sign := math.Copysign(1, n[2])
	a := -1 / (sign + n[2])
	c := n[0] * n[1] * a
	return [3]float64{1 + sign*n[0]*n[0]*a, sign * c, -sign * n[0]}, [3]float64{c, sign + n[1]*n[1]*a, -n[1]}
}

// triangleBVH is a bounding-volume hierarchy of a mesh's triangles: nodes
// split at the middle of their centroid bounds along the longest axis,
// leaves of at most bvhLeafSize triangles.
type triangleBVH struct {
	nodes []bvhNode
	order []uint32 // triangle indices, leaves own contiguous runs
}

type bvhNode struct {
	lo, hi [3]float64
	// Leaves hold order[first:first+count]; inner nodes have count 0,
	// children first (lower along axis) and first+1.
	first, count int32
	axis         int8
}

const bvhLeafSize = 4

func newTriangleBVH(m *Mesh) *triangleBVH {
	tree := &triangleBVH{order: make([]uint32, len(m.Triangles))}
	centroids := make([][3]float64, len(m.Triangles))
	for i, t := range m.Triangles {
		tree.order[i] = uint32(i)
		a, b, c := m.point(t[0]), m.point(t[1]), m.point(t[2])
		centroids[i] = [3]float64{(a[0] + b[0] + c[0]) / 3, (a[1] + b[1] + c[1]) / 3, (a[2] + b[2] + c[2]) / 3}
	}
	bounds := func(first, count int) (lo, hi, clo, chi [3]float64) {
		lo, hi = [3]float64{math.Inf(1), math.Inf(1), math.Inf(1)}, [3]float64{math.Inf(-1), math.Inf(-1), math.Inf(-1)}
		clo, chi = lo, hi
		for _, i := range tree.order[first : first+count] {
			for _, v := range m.Triangles[i] {
				p := m.point(v)
				for k := 0; k < 3; k++ {
					lo[k], hi[k] = math.Min(lo[k], p[k]), math.Max(hi[k], p[k])
				}
			}
			for k := 0; k < 3; k++ {
				clo[k], chi[k] = math.Min(clo[k], centroids[i][k]), math.Max(chi[k], centroids[i][k])
			}
		}
		return lo, hi, clo, chi
	}
	type task struct{ node, first, count int }
	tree.nodes = append(tree.nodes, bvhNode{})
	stack := []task{{0, 0, len(m.Triangles)}}
	for len(stack) > 0 {
		job := stack[len(stack)-1]
		stack = stack[:len(stack)-1]
		lo, hi, clo, chi := bounds(job.first, job.count)
		node := bvhNode{lo: lo, hi: hi, first: int32(job.first), count: int32(job.count)}
		if job.count > bvhLeafSize {
			axis := 0
			for k := 1; k < 3; k++ {
				if chi[k]-clo[k] > chi[axis]-clo[axis] {
					axis = k
				}
			}
			mid := (clo[axis] + chi[axis]) / 2
			run := tree.order[job.first : job.first+job.count]
			left := 0
			for i := range run {
				if centroids[run[i]][axis] < mid {
					run[i], run[left] = run[left], run[i]
					left++
				}
			}
			if left == 0 || left == len(run) {
				// Coincident centroids: split the run in half.
				left = len(run) / 2
			}
			child := len(tree.nodes)
			tree.nodes = append(tree.nodes, bvhNode{}, bvhNode{})
			node.first, node.count, node.axis = int32(child), 0, int8(axis)
			stack = append(stack, task{child + 1, job.first + left, job.count - left}, task{child, job.first, left})
		}
		tree.nodes[job.node] = node
	}
	return tree
}

// closestHit returns the distance to, and the index of, the nearest
// triangle not using vertex skip along the unit ray o + t d with
// 0 < t <= maxT. Barycentric coordinates down to -tolerance count as
// inside, so a ray through a shared edge or corner cannot slip between
// the triangles by rounding.
func (tree *triangleBVH) closestHit(m *Mesh, o, d [3]float64, maxT float64, skip uint32, tolerance float64, stack *[]int32) (float64, uint32, bool) {
	var inverse [3]float64
	for k := 0; k < 3; k++ {
		inverse[k] = 1 / d[k]
	}
	best, bestTriangle, hit := maxT, uint32(0), false
	*stack = append((*stack)[:0], 0)
	for len(*stack) > 0 {
		index := (*stack)[len(*stack)-1]
		*stack = (*stack)[:len(*stack)-1]
		node := &tree.nodes[index]
		if !rayHitsBox(o, inverse, node.lo, node.hi, best) {
			continue
		}
		if node.count == 0 {
			// Visit the child nearer the ray origin first (pushed last), so
			// the closest hit soon prunes the other.
			if d[node.axis] < 0 {
				*stack = append(*stack, node.first, node.first+1)
			} else {
				*stack = append(*stack, node.first+1, node.first)
			}
			continue
		}
		for _, ti := range tree.order[node.first : node.first+node.count] {
			t := m.Triangles[ti]
			if t[0] == skip || t[1] == skip || t[2] == skip {
				continue
			}
			if distance, ok := rayTriangle(o, d, m.point(t[0]), m.point(t[1]), m.point(t[2]), tolerance); ok && distance > 1e-9 && distance < best {
				best, bestTriangle, hit = distance, ti, true
			}
		}
	}
	return best, bestTriangle, hit
}

// rayHitsBox is the slab test of the ray against [lo, hi] up to maxT.
func rayHitsBox(o, inverse, lo, hi [3]float64, maxT float64) bool {
	near, far := 0.0, maxT
	for k := 0; k < 3; k++ {
		if math.IsInf(inverse[k], 0) {
			// Parallel to the slab: inside it or never.
			if o[k] < lo[k] || o[k] > hi[k] {
				return false
			}
			continue
		}
		t0, t1 := (lo[k]-o[k])*inverse[k], (hi[k]-o[k])*inverse[k]
		if t0 > t1 {
			t0, t1 = t1, t0
		}
		if t0 > near {
			near = t0
		}
		if t1 < far {
			far = t1
		}
		if near > far {
			return false
		}
	}
	return true
}

// rayTriangle is the Möller–Trumbore intersection of the ray o + t d with
// triangle (a, b, c), from either side, with barycentric coordinates down
// to -tolerance accepted.
func rayTriangle(o, d, a, b, c [3]float64, tolerance float64) (float64, bool) {
	e1, e2 := sub3(b, a), sub3(c, a)
	p := cross3(d, e2)
	det := dot3(e1, p)
	if math.Abs(det) < 1e-18 {
		return 0, false
	}
	inv := 1 / det
	s := sub3(o, a)
	u := dot3(s, p) * inv
	if u < -tolerance || u > 1+tolerance {
		return 0, false
	}
	q := cross3(s, e1)
	v := dot3(d, q) * inv
	if v < -tolerance || u+v > 1+tolerance {
		return 0, false
	}
	return dot3(e2, q) * inv, true
}
