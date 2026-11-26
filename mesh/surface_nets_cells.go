package mesh

import "math/bits"

// Non-manifold dual cells of the SurfaceNets surface (#2026). A dual cell
// holds eight voxels, numbered x | y<<1 | z<<2; each of the twelve cube
// edges between two of them carries a quad when their labels differ, and
// the quads of a cell meet at its vertex. Each of the six cube faces is the
// square around one mesh edge from the vertex to the next cell's: the quads
// on its four cube edges are the faces around that mesh edge. A square
// whose four voxels alternate (two voxels of one label touching only along
// the edge, like a checkerboard) puts four quads on one mesh edge, and
// voxels touching only at a corner put two separate fans of quads around
// one vertex; either way a label's surface is not a manifold there.
//
// Following the duplication of non-manifold points in VTK's
// vtkSurfaceNets3D (after Owen, Staten and Sorensen, "Parallel hexahedral
// meshing from volume fractions", 2014), but without its case table, each
// cell gives one vertex to every fan of quads around its centre instead of
// one vertex to the cell. On each square, the quads that must share the
// mesh edge for every label's surface to stay closed are kept together:
// two or three quads always, and four quads in two pairs, each around one
// of two opposite voxels of a repeated label. Both cells of a square decide
// its pairing from the same twelve voxels, so the pairs meet the same
// neighbouring vertices from either side. The cell's fans are the classes
// of its quads joined through these pairings.

// netEdge holds the two corners of each cube edge: x edges 0..3, y edges
// 4..7 and z edges 8..11.
var netEdge [12][2]uint8

// netSquare is one square of a dual cell: the corners with bit axis equal
// to side, in cyclic order, and the cube edge from ring[r] to ring[r+1].
type netSquare struct {
	axis, side uint8
	ring       [4]uint8
	edges      [4]uint8
}

var netSquares [6]netSquare

func init() {
	edgeOf := map[[2]uint8]uint8{}
	for e := uint8(0); e < 4; e++ {
		x := e << 1
		y := e&1 | e>>1<<2
		netEdge[e], netEdge[4+e], netEdge[8+e] = [2]uint8{x, x | 1}, [2]uint8{y, y | 2}, [2]uint8{e, e | 4}
	}
	for e, c := range netEdge {
		edgeOf[c], edgeOf[[2]uint8{c[1], c[0]}] = uint8(e), uint8(e)
	}
	for a, axis := range [3]uint8{1, 2, 4} {
		u, v := [3]uint8{2, 4, 1}[a], [3]uint8{4, 1, 2}[a]
		for side := uint8(0); side < 2; side++ {
			base := side * axis
			f := netSquare{axis: axis, side: side, ring: [4]uint8{base, base | u, base | u | v, base | v}}
			for r := 0; r < 4; r++ {
				f.edges[r] = edgeOf[[2]uint8{f.ring[r], f.ring[(r+1)%4]}]
			}
			netSquares[2*a+int(side)] = f
		}
	}
}

// cellFans is the split of one dual cell's quads into fans.
type cellFans struct {
	// count fans, numbered by their lowest cube edge; fan holds the fan of
	// each cube edge with a quad, four bits per edge.
	count int
	fan   uint64
	// kind is the number of labels around each fan.
	kind [6]uint8
	// pinched marks the squares (bit f of netSquares) whose two pairs of
	// quads, around voxels of a non-zero label, end in one fan here: the
	// mesh edge is non-manifold if the neighbouring cell joins them too.
	pinched uint8
	// tangled is set when the quads of some non-zero label form more than
	// one fan around one of the vertices: two of its parts meet only at
	// the vertex, joined through the boundaries of other labels.
	tangled bool
}

// fanOf returns the fan of cube edge e.
func (c *cellFans) fanOf(e int) uint32 { return uint32(c.fan>>(4*e)) & 15 }

// analyzeCell splits the quads of a cell with corner labels v into fans;
// outer(f) returns the four voxels behind square f's ring in the
// neighbouring cell, and is called only for squares alternating between
// two labels.
func analyzeCell(v *[8]uint32, outer func(f int) [4]uint32) cellFans {
	var out cellFans
	// units are the sets of cube edges whose quads share a mesh edge.
	var units [12]uint16
	nunits := 0
	var pairs [6][2]uint16
	paired := 0
	var pairedFace [6]uint8
	for f := range netSquares {
		face := &netSquares[f]
		var present uint16
		count := 0
		for _, e := range face.edges {
			c := netEdge[e]
			if v[c[0]] != v[c[1]] {
				present |= 1 << e
				count++
			}
		}
		if count == 0 {
			continue
		}
		if count < 4 {
			units[nunits] = present
			nunits++
			continue
		}
		x := [4]uint32{v[face.ring[0]], v[face.ring[1]], v[face.ring[2]], v[face.ring[3]]}
		around := -1 // the ring position of the first voxel paired around
		switch even, odd := x[1] == x[3], x[0] == x[2]; {
		case even && odd:
			// Two labels alternate: separate the higher label's voxels,
			// unless both cells connect them behind the square.
			around = 0
			if x[1] > x[0] {
				around = 1
			}
			if face.connects(v, x[around], around) && connectsLayer(outer(f), x[around], around) {
				around ^= 1
			}
		case even:
			around = 0
		case odd:
			around = 1
		}
		if around < 0 {
			// Four labels: each sees two quads, so all share the edge.
			units[nunits] = present
			nunits++
			continue
		}
		e := face.edges
		// The pair around ring[r] is the edges before and after it.
		a := uint16(1)<<e[(around+3)%4] | uint16(1)<<e[around]
		b := uint16(1)<<e[around+1] | uint16(1)<<e[(around+2)%4]
		units[nunits], units[nunits+1] = a, b
		nunits += 2
		// The repeated label is the one between the paired voxels; it sees
		// four quads on the edge when the pairs meet again.
		if x[(around+1)%4] != 0 || (x[around] == x[around+2] && x[around] != 0) {
			pairs[paired] = [2]uint16{a, b}
			pairedFace[paired] = uint8(f)
			paired++
		}
	}
	var fans [12]uint16
	out.count = mergeUnits(units[:nunits], fans[:])
	for i, fan := range fans[:out.count] {
		for m := fan; m != 0; m &= m - 1 {
			out.fan |= uint64(i) << (4 * bits.TrailingZeros16(m))
		}
		out.kind[i] = uint8(edgeLabels(v, fan, nil))
	}
	for p := 0; p < paired; p++ {
		if out.fanOf(bits.TrailingZeros16(pairs[p][0])) == out.fanOf(bits.TrailingZeros16(pairs[p][1])) {
			out.pinched |= 1 << pairedFace[p]
		}
	}
	// With two labels every quad borders both, so each fan is one fan of
	// either label; with more, check each non-zero label's own fans.
	var labels [8]uint32
	if edgeLabels(v, 1<<12-1, &labels) > 2 {
		var own [12]uint16
		for _, label := range labels {
			if label == 0 {
				continue
			}
			var mask uint16
			for e, c := range netEdge {
				if v[c[0]] != v[c[1]] && (v[c[0]] == label || v[c[1]] == label) {
					mask |= 1 << e
				}
			}
			var restricted [12]uint16
			n := 0
			for _, u := range units[:nunits] {
				if u&mask != 0 {
					restricted[n] = u & mask
					n++
				}
			}
			if mergeUnits(restricted[:n], own[:]) > bits.OnesCount64(fanMask(&out, mask)) {
				out.tangled = true
			}
		}
	}
	return out
}

// fanMask is the set of fans holding any of the edges in mask.
func fanMask(c *cellFans, mask uint16) uint64 {
	var fans uint64
	for m := mask; m != 0; m &= m - 1 {
		fans |= 1 << c.fanOf(bits.TrailingZeros16(m))
	}
	return fans
}

// connects reports whether the voxels of label at ring positions r and
// r+2 of the square are joined through the cell's other layer.
func (f *netSquare) connects(v *[8]uint32, label uint32, r int) bool {
	var layer [4]uint32
	for i, c := range f.ring {
		layer[i] = v[c^f.axis]
	}
	return connectsLayer(layer, label, r)
}

// connectsLayer reports whether the layer behind a square joins its ring
// positions r and r+2: both voxels behind them and one between hold label.
func connectsLayer(layer [4]uint32, label uint32, r int) bool {
	return layer[r] == label && layer[r+2] == label && (layer[r+1] == label || layer[(r+3)%4] == label)
}

// mergeUnits joins the overlapping edge sets of units into fans, ordered
// by their lowest edge, and returns their count.
func mergeUnits(units []uint16, fans []uint16) int {
	n := 0
	for _, u := range units {
		merged := u
		kept := 0
		for _, f := range fans[:n] {
			if f&merged != 0 {
				merged |= f
			} else {
				fans[kept] = f
				kept++
			}
		}
		fans[kept] = merged
		n = kept + 1
	}
	// Insertion sort by lowest edge: at most six fans.
	for i := 1; i < n; i++ {
		for j := i; j > 0 && bits.TrailingZeros16(fans[j]) < bits.TrailingZeros16(fans[j-1]); j-- {
			fans[j], fans[j-1] = fans[j-1], fans[j]
		}
	}
	return n
}

// edgeLabels counts the distinct labels at the ends of the edges in mask,
// storing them in labels when it is not nil.
func edgeLabels(v *[8]uint32, mask uint16, labels *[8]uint32) int {
	var seen [8]uint32
	n := 0
	for m := mask; m != 0; m &= m - 1 {
		c := netEdge[bits.TrailingZeros16(m)]
		for _, corner := range c {
			label := v[corner]
			found := false
			for _, s := range seen[:n] {
				if s == label {
					found = true
					break
				}
			}
			if !found {
				seen[n] = label
				n++
			}
		}
	}
	if labels != nil {
		*labels = seen
		for i := n; i < 8; i++ {
			labels[i] = 0
		}
	}
	return n
}
