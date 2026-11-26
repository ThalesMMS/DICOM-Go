package mesh

import (
	"context"
	"fmt"
	"math"
)

// The at-most-16-byte record names an edge and its directed face corner.
// No position, source array or map is retained by the topology preflight.
type curvatureEdge struct {
	key    uint64
	corner uint32
}

func curvatureTopology(ctx context.Context, m *Mesh, flags []CurvatureFlags) error {
	count := 3 * len(m.Triangles)
	edges := make([]curvatureEdge, count)
	buffer := make([]curvatureEdge, count)
	parent := make([]uint32, count)
	rank := make([]uint8, count)
	first := make([]uint32, len(m.Vertices))
	for i := range first {
		if i&4095 == 0 {
			if err := ctx.Err(); err != nil {
				return err
			}
		}
		first[i] = math.MaxUint32
	}
	for f, face := range m.Triangles {
		if f&4095 == 0 {
			if err := ctx.Err(); err != nil {
				return err
			}
		}
		for i := 0; i < 3; i++ {
			corner := uint32(3*f + i)
			edges[corner] = curvatureEdge{key: edgeID(face[i], face[(i+1)%3]), corner: corner}
			parent[corner] = corner
		}
	}
	ordered, err := sortCurvatureEdges(ctx, edges, buffer)
	if err != nil {
		return err
	}
	find := func(v uint32) uint32 {
		for parent[v] != v {
			parent[v] = parent[parent[v]]
			v = parent[v]
		}
		return v
	}
	union := func(a, b uint32) {
		a, b = find(a), find(b)
		if a == b {
			return
		}
		if rank[a] < rank[b] {
			a, b = b, a
		}
		parent[b] = a
		if rank[a] == rank[b] {
			rank[a]++
		}
	}
	for start := 0; start < count; {
		if err := ctx.Err(); err != nil {
			return err
		}
		end := start + 1
		for end < count && ordered[end].key == ordered[start].key {
			// Multiplicity beyond two is already an error; do not scan an
			// unbounded run before returning or checking cancellation.
			end++
			if end-start > 2 {
				break
			}
		}
		lo, hi := uint32(ordered[start].key>>32), uint32(ordered[start].key)
		switch end - start {
		case 1:
			flags[lo] |= CurvatureBoundary
			flags[hi] |= CurvatureBoundary
		case 2:
			a, b := ordered[start], ordered[start+1]
			af, ai := int(a.corner/3), int(a.corner%3)
			bf, bi := int(b.corner/3), int(b.corner%3)
			at, bt := m.Triangles[af], m.Triangles[bf]
			if at[(ai+2)%3] == bt[(bi+2)%3] {
				return fmt.Errorf("%w: duplicate curvature faces %d/%d", ErrInvalidField, af, bf)
			}
			if (at[ai] == lo) == (bt[bi] == lo) {
				return fmt.Errorf("%w: inconsistent curvature edge (%d,%d)", ErrInvalidField, lo, hi)
			}
			for _, vertex := range [2]uint32{lo, hi} {
				ac, bc := a.corner, b.corner
				if at[ai] != vertex {
					ac = uint32(3*af + (ai+1)%3)
				}
				if bt[bi] != vertex {
					bc = uint32(3*bf + (bi+1)%3)
				}
				union(ac, bc)
			}
		default:
			return fmt.Errorf("%w: non-manifold curvature edge (%d,%d)", ErrInvalidField, lo, hi)
		}
		start = end
	}
	// Each face corner has at most two links after the edge checks. A single
	// connected component is therefore a cycle (interior) or path (boundary).
	// Checking connectivity catches bow-ties that edge multiplicities miss.
	for f, face := range m.Triangles {
		if f&4095 == 0 {
			if err := ctx.Err(); err != nil {
				return err
			}
		}
		for i, vertex := range face {
			root := find(uint32(3*f + i))
			if first[vertex] == math.MaxUint32 {
				first[vertex] = root
			} else if first[vertex] != root {
				return fmt.Errorf("%w: disconnected curvature face fan at vertex %d", ErrInvalidField, vertex)
			}
		}
	}
	for i, corner := range first {
		if i&4095 == 0 {
			if err := ctx.Err(); err != nil {
				return err
			}
		}
		if corner == math.MaxUint32 {
			flags[i] |= CurvatureUnused
		}
	}
	return ctx.Err()
}

// Bottom-up stable merge with checks during copying, not only between long
// comparison-sort calls. Both buffers are included in the byte reservation.
func sortCurvatureEdges(ctx context.Context, values, scratch []curvatureEdge) ([]curvatureEdge, error) {
	n := len(values)
	for width := 1; width < n; {
		for start := 0; start < n; {
			mid := start + min(width, n-start)
			end := mid + min(width, n-mid)
			a, b := start, mid
			for dst := start; dst < end; dst++ {
				if dst&4095 == 0 {
					if err := ctx.Err(); err != nil {
						return nil, err
					}
				}
				if b == end || (a < mid && (values[a].key < values[b].key || (values[a].key == values[b].key && values[a].corner <= values[b].corner))) {
					scratch[dst] = values[a]
					a++
				} else {
					scratch[dst] = values[b]
					b++
				}
			}
			start = end
		}
		values, scratch = scratch, values
		if width > n/2 {
			break
		}
		width *= 2
	}
	return values, ctx.Err()
}
