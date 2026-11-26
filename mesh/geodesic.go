package mesh

import (
	"context"
	"errors"
	"fmt"
	"math"
)

// Geodesic paths on surfaces (#1254): the shortest path along a mesh
// between two surface points and geodesic distance fields. VTK's
// vtkDijkstraGraphGeodesicPath runs Dijkstra on the mesh edges, whose paths
// zig-zag and overestimate the distance by up to 8% on a regular grid
// whatever its resolution. Here Dijkstra (A* towards a target) runs on the
// edges plus SteinerPoints points evenly spaced on every edge, every pair
// of points on the boundary of a triangle joined by the straight segment
// across it, after M. Lanthier, A. Maheshwari and J.-R. Sack,
// "Approximating Weighted Shortest Paths on Polyhedral Surfaces" (SoCG
// 1997): the result is a true path on the surface, never shorter than the
// exact geodesic and within about 1% of it with the default three points.
// Written from the papers; no third-party code is copied.

// ErrNoPath reports surface points that no path on the mesh joins (they
// lie on different connected components).
var ErrNoPath = errors.New("mesh: no path on the surface between the points")

const (
	// DefaultGeodesicSteinerPoints is the number of points added on every
	// edge when GeodesicOptions.SteinerPoints is zero.
	DefaultGeodesicSteinerPoints = 3
	// MaxGeodesicSteinerPoints bounds GeodesicOptions.SteinerPoints.
	MaxGeodesicSteinerPoints = 16
)

// GeodesicOptions configures a GeodesicGraph.
type GeodesicOptions struct {
	// SteinerPoints is the number of points evenly spaced on every edge;
	// zero uses DefaultGeodesicSteinerPoints. More points shorten the
	// paths towards the exact geodesic at a cost linear in the count.
	SteinerPoints int
	// EdgesOnly restricts the paths to the mesh edges, Dijkstra on the
	// edge graph as vtkDijkstraGraphGeodesicPath; SteinerPoints is ignored.
	EdgesOnly bool
}

// GeodesicGraph is the search graph of a mesh for geodesic queries. It
// takes about 40 bytes per triangle, and every query 12 bytes per graph
// node (one per vertex plus SteinerPoints per edge, about 1 + 3·SteinerPoints
// per vertex on a closed surface). It is safe for concurrent queries; the
// mesh must not change while it is in use.
type GeodesicGraph struct {
	m        *Mesh
	steiner  uint32
	vertices uint32
	nodes    uint32
	edges    [][2]uint32
	triEdges [][3]uint32
	// Triangles around each vertex and each edge, in compressed rows.
	vertexStart, edgeStart []uint32
	vertexTris, edgeTris   []uint32
}

// NewGeodesicGraph builds the graph of m.
func NewGeodesicGraph(m *Mesh, opts GeodesicOptions) (*GeodesicGraph, error) {
	if err := m.Validate(); err != nil {
		return nil, err
	}
	steiner := opts.SteinerPoints
	if steiner == 0 {
		steiner = DefaultGeodesicSteinerPoints
	}
	if opts.EdgesOnly {
		steiner = 0
	}
	if steiner < 0 || steiner > MaxGeodesicSteinerPoints {
		return nil, fmt.Errorf("%w: %d Steiner points per edge (0..%d)", ErrInvalidField, opts.SteinerPoints, MaxGeodesicSteinerPoints)
	}
	g := &GeodesicGraph{m: m, steiner: uint32(steiner), vertices: uint32(len(m.Vertices)), triEdges: make([][3]uint32, len(m.Triangles))}
	ids := make(map[uint64]uint32, 3*len(m.Triangles)/2)
	vertexCount := make([]uint32, len(m.Vertices)+1)
	for ti, t := range m.Triangles {
		for k := 0; k < 3; k++ {
			key := edgeID(t[k], t[(k+1)%3])
			e, ok := ids[key]
			if !ok {
				e = uint32(len(g.edges))
				ids[key] = e
				g.edges = append(g.edges, [2]uint32{uint32(key >> 32), uint32(key)})
			}
			g.triEdges[ti][k] = e
			vertexCount[t[k]+1]++
		}
	}
	nodes := uint64(len(m.Vertices)) + uint64(len(g.edges))*uint64(steiner)
	if nodes >= math.MaxInt32 {
		return nil, fmt.Errorf("%w: geodesic graph of %d nodes", ErrBudgetExceeded, nodes)
	}
	g.nodes = uint32(nodes)
	// Compressed rows of the triangles around vertices and edges.
	for v := 1; v < len(vertexCount); v++ {
		vertexCount[v] += vertexCount[v-1]
	}
	g.vertexStart = vertexCount
	g.vertexTris = make([]uint32, vertexCount[len(vertexCount)-1])
	fill := append([]uint32(nil), vertexCount[:len(vertexCount)-1]...)
	edgeCount := make([]uint32, len(g.edges)+1)
	for ti, t := range m.Triangles {
		for k := 0; k < 3; k++ {
			g.vertexTris[fill[t[k]]] = uint32(ti)
			fill[t[k]]++
			edgeCount[g.triEdges[ti][k]+1]++
		}
	}
	for e := 1; e < len(edgeCount); e++ {
		edgeCount[e] += edgeCount[e-1]
	}
	g.edgeStart = edgeCount
	g.edgeTris = make([]uint32, edgeCount[len(edgeCount)-1])
	fill = append(fill[:0], edgeCount[:len(edgeCount)-1]...)
	for ti := range m.Triangles {
		for k := 0; k < 3; k++ {
			e := g.triEdges[ti][k]
			g.edgeTris[fill[e]] = uint32(ti)
			fill[e]++
		}
	}
	return g, nil
}

// Mesh returns the graph's mesh.
func (g *GeodesicGraph) Mesh() *Mesh { return g.m }

// position is the location of graph node n: a vertex, or the j-th of the
// Steiner points of an edge.
func (g *GeodesicGraph) position(n uint32) [3]float64 {
	if n < g.vertices {
		return g.m.point(n)
	}
	e, j := (n-g.vertices)/g.steiner, (n-g.vertices)%g.steiner
	a, b := g.m.point(g.edges[e][0]), g.m.point(g.edges[e][1])
	s := float64(j+1) / float64(g.steiner+1)
	return add3(a, scale3(sub3(b, a), s))
}

// triangles are the triangles holding node n.
func (g *GeodesicGraph) triangles(n uint32) []uint32 {
	if n < g.vertices {
		return g.vertexTris[g.vertexStart[n]:g.vertexStart[n+1]]
	}
	e := (n - g.vertices) / g.steiner
	return g.edgeTris[g.edgeStart[e]:g.edgeStart[e+1]]
}

// triangleNodes appends the nodes on the boundary of triangle ti.
func (g *GeodesicGraph) triangleNodes(ti uint32, buf []uint32) []uint32 {
	t := g.m.Triangles[ti]
	buf = append(buf[:0], t[0], t[1], t[2])
	for _, e := range g.triEdges[ti] {
		base := g.vertices + e*g.steiner
		for j := uint32(0); j < g.steiner; j++ {
			buf = append(buf, base+j)
		}
	}
	return buf
}

// GeodesicPath is a path along a surface.
type GeodesicPath struct {
	// Points is the polyline from the start to the end point; every
	// segment lies in one triangle of the mesh.
	Points [][3]float64
	// Length is the polyline's length in mm.
	Length float64
}

// ShortestPath is the shortest path between vertices from and to.
func (g *GeodesicGraph) ShortestPath(ctx context.Context, from, to uint32) (*GeodesicPath, error) {
	a, okA := VertexPoint(g.m, from)
	b, okB := VertexPoint(g.m, to)
	if !okA || !okB {
		return nil, fmt.Errorf("%w: geodesic between vertices %d and %d of a mesh of %d vertices (both must be used by a triangle)", ErrInvalidField, from, to, len(g.m.Vertices))
	}
	return g.ShortestPathBetween(ctx, a, b)
}

// ShortestPathBetween is the shortest path between two surface points (as
// returned by Locator.ClosestPoint or RayHit). It is an A* search guided by
// the straight-line distance to the end point, so it visits little more
// than the band around the path. The context is checked every 4096 steps.
func (g *GeodesicGraph) ShortestPathBetween(ctx context.Context, from, to SurfacePoint) (*GeodesicPath, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	if int(from.Triangle) >= len(g.m.Triangles) || int(to.Triangle) >= len(g.m.Triangles) {
		return nil, fmt.Errorf("%w: surface point on a missing triangle", ErrInvalidField)
	}
	p, q := g.surfacePoint(from), g.surfacePoint(to)
	// Nodes of the target triangle reach the end point directly.
	targetNodes := map[uint32]bool{}
	for _, n := range g.triangleNodes(to.Triangle, nil) {
		targetNodes[n] = true
	}
	const start, unset = -2, -1
	best, bestPrev := math.Inf(1), int32(unset)
	if from.Triangle == to.Triangle {
		best, bestPrev = distance3(p, q), start
	}
	s := newGeodesicSearch(g.nodes)
	buf := make([]uint32, 0, 3+3*g.steiner)
	for _, n := range g.triangleNodes(from.Triangle, buf) {
		s.push(n, distance3(p, g.position(n)), start, distance3(g.position(n), q))
	}
	steps := 0
	for len(s.queue) > 0 {
		item := s.queue.pop()
		if item.f >= best {
			break
		}
		if item.g > s.dist[item.node] {
			continue
		}
		if steps++; steps%4096 == 0 {
			if err := ctx.Err(); err != nil {
				return nil, err
			}
		}
		n, at := item.node, g.position(item.node)
		if targetNodes[n] {
			if d := item.g + distance3(at, q); d < best {
				best, bestPrev = d, int32(n)
			}
		}
		for _, ti := range g.triangles(n) {
			buf = g.triangleNodes(ti, buf)
			for _, next := range buf {
				if next == n {
					continue
				}
				there := g.position(next)
				s.push(next, item.g+distance3(at, there), int32(n), distance3(there, q))
			}
		}
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if bestPrev == unset {
		return nil, ErrNoPath
	}
	points := [][3]float64{q}
	for n := bestPrev; n != start; n = s.prev[n] {
		points = append(points, g.position(uint32(n)))
	}
	points = append(points, p)
	path := &GeodesicPath{Points: make([][3]float64, 0, len(points))}
	for i := len(points) - 1; i >= 0; i-- {
		if k := len(path.Points); k > 0 && points[i] == path.Points[k-1] {
			continue
		}
		if k := len(path.Points); k > 0 {
			path.Length += distance3(path.Points[k-1], points[i])
		}
		path.Points = append(path.Points, points[i])
	}
	return path, nil
}

// Distances returns the geodesic distance from the nearest of the source
// vertices to every vertex, +Inf where none reaches it or beyond
// maxDistance (no limit when zero). The context is checked every 4096
// steps.
func (g *GeodesicGraph) Distances(ctx context.Context, sources []uint32, maxDistance float64) ([]float64, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	if len(sources) == 0 {
		return nil, fmt.Errorf("%w: geodesic distances without a source", ErrInvalidField)
	}
	if !(maxDistance > 0) {
		maxDistance = math.Inf(1)
	}
	s := newGeodesicSearch(g.nodes)
	for _, v := range sources {
		if v >= g.vertices {
			return nil, fmt.Errorf("%w: source vertex %d of a mesh of %d vertices", ErrInvalidField, v, g.vertices)
		}
		s.push(v, 0, -2, 0)
	}
	buf := make([]uint32, 0, 3+3*g.steiner)
	steps := 0
	for len(s.queue) > 0 {
		item := s.queue.pop()
		if item.g > s.dist[item.node] {
			continue
		}
		if item.g > maxDistance {
			break
		}
		if steps++; steps%4096 == 0 {
			if err := ctx.Err(); err != nil {
				return nil, err
			}
		}
		n, at := item.node, g.position(item.node)
		for _, ti := range g.triangles(n) {
			buf = g.triangleNodes(ti, buf)
			for _, next := range buf {
				if next != n {
					s.push(next, item.g+distance3(at, g.position(next)), int32(n), 0)
				}
			}
		}
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	out := make([]float64, g.vertices)
	for v := range out {
		out[v] = s.dist[v]
		if out[v] > maxDistance {
			out[v] = math.Inf(1)
		}
	}
	return out, nil
}

// surfacePoint is the position of s from its barycentric weights.
func (g *GeodesicGraph) surfacePoint(s SurfacePoint) [3]float64 {
	t := g.m.Triangles[s.Triangle]
	var p [3]float64
	for k := 0; k < 3; k++ {
		p = add3(p, scale3(g.m.point(t[k]), s.Barycentric[k]))
	}
	return p
}

func distance3(a, b [3]float64) float64 {
	d := sub3(a, b)
	return math.Sqrt(dot3(d, d))
}

// geodesicSearch is the state of one Dijkstra/A* search.
type geodesicSearch struct {
	dist  []float64
	prev  []int32
	queue geodesicQueue
}

func newGeodesicSearch(nodes uint32) *geodesicSearch {
	s := &geodesicSearch{dist: make([]float64, nodes), prev: make([]int32, nodes)}
	for i := range s.dist {
		s.dist[i] = math.Inf(1)
	}
	return s
}

// push records a path of length g to node n through prev and queues it
// with priority g + h when it is shorter than the known one.
func (s *geodesicSearch) push(n uint32, g float64, prev int32, h float64) {
	if g >= s.dist[n] {
		return
	}
	s.dist[n], s.prev[n] = g, prev
	s.queue.push(geodesicItem{f: g + h, g: g, node: n})
}

type geodesicItem struct {
	f, g float64
	node uint32
}

// geodesicQueue is a binary min-heap on f.
type geodesicQueue []geodesicItem

func (q *geodesicQueue) push(item geodesicItem) {
	h := append(*q, item)
	i := len(h) - 1
	for i > 0 {
		parent := (i - 1) / 2
		if h[parent].f <= item.f {
			break
		}
		h[i] = h[parent]
		i = parent
	}
	h[i] = item
	*q = h
}

func (q *geodesicQueue) pop() geodesicItem {
	h := *q
	top, last := h[0], h[len(h)-1]
	h = h[:len(h)-1]
	if n := len(h); n > 0 {
		i := 0
		for {
			child := 2*i + 1
			if child >= n {
				break
			}
			if child+1 < n && h[child+1].f < h[child].f {
				child++
			}
			if last.f <= h[child].f {
				break
			}
			h[i] = h[child]
			i = child
		}
		h[i] = last
	}
	*q = h
	return top
}
